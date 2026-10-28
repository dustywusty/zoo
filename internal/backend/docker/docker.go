// Package docker drives the docker CLI (Docker Engine, Docker Desktop,
// OrbStack, colima) and podman, which takes the same flags. Cages are
// containers: with runc they share the runtime's kernel (isolation
// "container"); with gVisor or Kata configured they get stronger isolation.
//
// Masks and the git guard are applied host-side with read-only bind mounts
// (decision D10), so cages run with every capability dropped.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/jsonx"
)

// EgressNetwork is the keeper's network with a route out. It's zoo's own
// bridge rather than the default one so unrelated containers can't reach the
// keeper.
const EgressNetwork = "zoo-egress"

// DefaultPids caps processes per cage when the spec doesn't say.
const DefaultPids = 1024

func init() {
	backend.Register("docker", func(o backend.Options) backend.Backend { return New("docker", o) })
	backend.Register("podman", func(o backend.Options) backend.Backend { return New("podman", o) })
}

// Docker is the docker/podman backend.
type Docker struct {
	name     string
	cli      backend.CLI
	network  string
	runtime  string
	maskFile string
}

// New builds the backend. Bin defaults to $ZOO_DOCKER_BIN, then the backend name.
func New(name string, o backend.Options) *Docker {
	bin := o.Bin
	if bin == "" {
		bin = os.Getenv("ZOO_DOCKER_BIN")
	}
	if bin == "" {
		bin = name
	}
	return &Docker{name: name, cli: backend.CLI{Bin: bin}, network: o.Network, runtime: o.Runtime, maskFile: o.MaskFile}
}

func (d *Docker) Name() string { return d.name }

// isolationFor maps an OCI runtime name to the isolation it gives.
func isolationFor(runtime string) backend.Isolation {
	r := strings.ToLower(runtime)
	switch {
	case strings.Contains(r, "kata"):
		return backend.IsoMicroVM
	case strings.Contains(r, "runsc") || strings.Contains(r, "gvisor"):
		return backend.IsoSandboxed
	}
	return backend.IsoContainer
}

func (d *Docker) Detect(ctx context.Context) backend.Info {
	info := backend.Info{
		Name: d.name,
		Caps: backend.Capabilities{
			Isolation:          backend.IsoContainer,
			InternalNetworks:   true,
			MultiNetworkAttach: true,
			HostFileMounts:     true,
			ReadOnlyMounts:     true,
			PidsLimit:          true,
			PerCageNetworks:    true,
		},
	}
	if d.name == "podman" {
		info.Hint = "install Podman (https://podman.io)"
	} else {
		info.Hint = "install OrbStack, Docker Desktop, colima, or Docker Engine"
	}
	if !d.cli.Installed() {
		return info
	}
	info.Installed = true
	out, err := d.cli.Run(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		if d.name == "podman" {
			info.Hint = "start it: `podman machine start` (macOS) or check `podman info`"
		} else {
			info.Hint = "start your Docker runtime (OrbStack, Docker Desktop, `colima start`, or the docker service)"
		}
		return info
	}
	info.Running, info.Hint = true, ""
	var v any
	_ = json.Unmarshal([]byte(out), &v)
	// docker: ServerVersion, OperatingSystem, DefaultRuntime. podman: version.Version, host.ociRuntime.name.
	info.Version = jsonx.String(v, "ServerVersion")
	if info.Version == "" {
		if m := jsonx.Map(v, "version"); m != nil {
			info.Version = jsonx.String(m, "Version")
		}
	}
	info.Detail = jsonx.String(v, "OperatingSystem")
	if d.name == "podman" {
		info.Detail = "podman"
	}
	runtime := d.runtime
	if runtime == "" {
		runtime = jsonx.String(v, "DefaultRuntime")
		if m := jsonx.Map(v, "ociRuntime"); m != nil && runtime == "" {
			runtime = jsonx.String(m, "name")
		}
	}
	info.Caps.Isolation = isolationFor(runtime)
	info.Caps.PerCageKernel = info.Caps.Isolation == backend.IsoMicroVM
	if runtime != "" && runtime != "runc" && runtime != "crun" {
		info.Detail = strings.TrimSpace(info.Detail + " · " + runtime)
	}
	return info
}

func (d *Docker) Prepare(ctx context.Context) error {
	if d.cli.OK(ctx, "info") {
		return nil
	}
	return fmt.Errorf("%s isn't answering: %s", d.cli.Bin, d.Detect(ctx).Hint)
}

func (d *Docker) ImageExists(ctx context.Context, ref string) (bool, error) {
	return d.cli.OK(ctx, "image", "inspect", ref), nil
}

func (d *Docker) BuildImage(ctx context.Context, s backend.ImageSpec) error {
	args := []string{"build", "-t", s.Tag}
	for _, k := range backend.SortedKeys(s.BuildArgs) {
		args = append(args, "--build-arg", k+"="+s.BuildArgs[k])
	}
	return d.cli.Attach(ctx, append(args, s.Context)...)
}

// noHostAddr keeps Docker from giving the host an address on a bridge. On
// a Linux host that address is the host itself, so without this anything
// listening on all interfaces (sshd, databases) is reachable from every cage,
// internal network or not (found by the escape suite in CI). Cages and the
// keeper need no gateway on internal networks. Podman's netavark has no such
// option.
const noHostAddr = "com.docker.network.bridge.inhibit_ipv4=true"

func (d *Docker) internalNetArgs() []string {
	args := []string{"--internal"}
	if d.name == "docker" {
		args = append(args, "-o", noHostAddr)
	}
	return args
}

func (d *Docker) ensureNet(ctx context.Context, name string, internal bool) error {
	if out, err := d.cli.Run(ctx, "network", "inspect", name, "--format", "{{json .Options}}"); err == nil {
		if !internal || d.name != "docker" || strings.Contains(out, "inhibit_ipv4") {
			return nil
		}
		// Made by an older zoo with a host address: replace it if nothing uses it.
		if _, err := d.cli.Run(ctx, "network", "rm", name); err != nil {
			fmt.Fprintf(os.Stderr, "zoo: network %s gives the host an address cages can reach; stop its cages and run zoo up again to replace it\n", name)
			return nil
		}
	}
	args := []string{"network", "create", "--label", "zoo=1"}
	if internal {
		args = append(args, d.internalNetArgs()...)
	}
	_, err := d.cli.Run(ctx, append(args, name)...)
	return err
}

func (d *Docker) EnsureNetwork(ctx context.Context) error {
	if err := d.ensureNet(ctx, d.network, true); err != nil {
		return err
	}
	return d.ensureNet(ctx, backend.ServicesNetwork, true)
}

// cageNet is a cage's own network.
func (d *Docker) cageNet(cageID string) string {
	return d.network + "-" + strings.TrimPrefix(cageID, "zoo-")
}

// connectKeeper attaches the keeper to a network unless it already is.
func (d *Docker) connectKeeper(ctx context.Context, keeperID, network string) (string, error) {
	st, ok, err := d.Get(ctx, keeperID)
	if err != nil || !ok {
		return "", fmt.Errorf("keeper %s not found: %v", keeperID, err)
	}
	if ip := st.IPs[network]; ip != "" {
		return ip, nil
	}
	if _, err := d.cli.Run(ctx, "network", "connect", network, keeperID); err != nil {
		return "", err
	}
	st, _, err = d.Get(ctx, keeperID)
	if err != nil || st.IPs[network] == "" {
		return "", fmt.Errorf("keeper has no address on %s", network)
	}
	return st.IPs[network], nil
}

// CageNetwork gives each cage its own internal network with the keeper on
// it, so cages can't reach each other. If the runtime runs out of address
// space it falls back to the shared network.
func (d *Docker) CageNetwork(ctx context.Context, cageID string) (string, string, error) {
	n := d.cageNet(cageID)
	if !d.cli.OK(ctx, "network", "inspect", n) {
		args := append([]string{"network", "create", "--label", "zoo=1", "--label", "zoo.cage-net=" + cageID}, d.internalNetArgs()...)
		if _, err := d.cli.Run(ctx, append(args, n)...); err != nil {
			fmt.Fprintf(os.Stderr, "zoo: couldn't create a network for %s (%v); it shares %s with other cages\n", cageID, err, d.network)
			n = d.network
		}
	}
	ip, err := d.connectKeeper(ctx, backend.KeeperID, n)
	return n, ip, err
}

func labelArgs(labels map[string]string) []string {
	var out []string
	for _, k := range backend.SortedKeys(labels) {
		out = append(out, "--label", k+"="+labels[k])
	}
	return out
}

// mountArg renders a --mount value. Fields are CSV, so paths with commas or
// quotes are quoted.
func mountArg(src, dst string, ro bool) string {
	q := func(s string) string {
		if strings.ContainsAny(s, ",\"") {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	v := "type=bind," + q("source="+src) + "," + q("target="+dst)
	if ro {
		v += ",readonly"
	}
	return v
}

func (d *Docker) EnsureKeeper(ctx context.Context, s backend.KeeperSpec) (string, error) {
	if err := d.ensureNet(ctx, EgressNetwork, false); err != nil {
		return "", err
	}
	st, exists, _ := d.Get(ctx, s.ID)
	if exists && !st.Running {
		if err := d.Start(ctx, s.ID); err != nil {
			_ = d.Delete(ctx, s.ID)
			exists = false
		}
	}
	if exists {
		// Rejoin cage networks created while the keeper was down.
		if out, err := d.cli.Run(ctx, "network", "ls", "-q", "--filter", "label=zoo.cage-net", "--format", "{{.Name}}"); err == nil {
			for _, n := range strings.Fields(out) {
				_, _ = d.connectKeeper(ctx, s.ID, n)
			}
		}
	}
	if !exists {
		args := []string{"create", "--name", s.ID,
			"--network", EgressNetwork,
			// Where zood's host bridge is (Linux needs this mapping; Desktop
			// and OrbStack provide it anyway).
			"--add-host", "host.docker.internal:host-gateway",
			"--cpus", fmt.Sprint(s.CPUs), "--memory", s.Memory,
			"--pids-limit", "512",
			// squid starts as root to set up its dirs, then runs as "proxy".
			"--cap-drop", "ALL",
			"--cap-add", "CHOWN", "--cap-add", "SETUID", "--cap-add", "SETGID",
			"--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
			"--security-opt", "no-new-privileges",
			"--restart", "unless-stopped",
		}
		args = append(args, labelArgs(s.Labels)...)
		args = append(args,
			"--mount", mountArg(s.EtcDir, "/etc/zoo", false),
			"--mount", mountArg(s.LogDir, "/var/log/zoo", false),
			s.Image)
		if _, err := d.cli.Run(ctx, args...); err != nil {
			return "", fmt.Errorf("create keeper: %w", err)
		}
		nets := []string{d.network, backend.ServicesNetwork}
		// A replaced keeper rejoins every cage's own network.
		if out, err := d.cli.Run(ctx, "network", "ls", "-q", "--filter", "label=zoo.cage-net", "--format", "{{.Name}}"); err == nil {
			nets = append(nets, strings.Fields(out)...)
		}
		for _, n := range nets {
			if _, err := d.cli.Run(ctx, "network", "connect", n, s.ID); err != nil {
				_ = d.Delete(ctx, s.ID)
				return "", fmt.Errorf("attach keeper to %s: %w", n, err)
			}
		}
		if err := d.Start(ctx, s.ID); err != nil {
			return "", fmt.Errorf("start keeper: %w", err)
		}
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		st, ok, err := d.Get(ctx, s.ID)
		switch {
		case err != nil:
			lastErr = err
		case !ok:
			lastErr = fmt.Errorf("keeper %s disappeared", s.ID)
		case st.IP != "":
			return st.IP, nil
		case !st.Running:
			lastErr = fmt.Errorf("keeper isn't running (%s); see `%s logs %s`", st.State, d.cli.Bin, s.ID)
		default:
			lastErr = fmt.Errorf("keeper has no address on %s", d.network)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", lastErr
}

// RunArgs is the `docker run` invocation for a cage.
func (d *Docker) RunArgs(s backend.CageSpec) []string {
	pids := s.Pids
	if pids <= 0 {
		pids = DefaultPids
	}
	args := []string{"run", "-d",
		"--name", s.ID,
		"--network", or(s.Network, d.network),
		// Docker's embedded resolver still answers on internal networks and
		// forwards upstream; point it at nothing so external names never resolve.
		"--dns", "127.0.0.1",
		"--cpus", fmt.Sprint(s.CPUs),
		"--memory", s.Memory,
		"--pids-limit", strconv.Itoa(pids),
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
	}
	if d.runtime != "" {
		args = append(args, "--runtime", d.runtime)
	}
	args = append(args, labelArgs(s.Labels)...)
	for _, e := range s.Env {
		args = append(args, "--env", e)
	}
	// Tell zoo-init (and zoo doctor) the guard is applied from outside.
	args = append(args, "--env", "ZOO_GUARD=host")
	for _, m := range s.Mounts {
		args = append(args, "--mount", mountArg(m.Source, m.Target, m.ReadOnly))
	}
	for _, p := range s.Pin {
		if exists(p) {
			args = append(args, "--mount", mountArg(p, p, false))
		}
	}
	for _, p := range s.ReadOnly {
		if exists(p) {
			args = append(args, "--mount", mountArg(p, p, true))
		}
	}
	for _, p := range s.Masks {
		if exists(p) {
			args = append(args, "--mount", mountArg(d.maskFile, p, true))
		}
	}
	return append(args, s.Image)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (d *Docker) CreateCage(ctx context.Context, s backend.CageSpec) error {
	if masks := backend.Existing(s.Masks); len(masks) > 0 && d.maskFile == "" {
		return fmt.Errorf("docker backend needs a mask file to hide %d secret file(s)", len(masks))
	}
	_, err := d.cli.Run(ctx, d.RunArgs(s)...)
	return err
}

func (d *Docker) Start(ctx context.Context, id string) error {
	_, err := d.cli.Run(ctx, "start", id)
	return err
}

func (d *Docker) Stop(ctx context.Context, id string) error {
	_, err := d.cli.Run(ctx, "stop", "-t", "3", id)
	return err
}

func (d *Docker) Delete(ctx context.Context, id string) error {
	if _, err := d.cli.Run(ctx, "rm", "-f", id); err != nil {
		return err
	}
	// Drop the cage's own network, if it had one.
	if n := d.cageNet(id); id != backend.KeeperID && d.cli.OK(ctx, "network", "inspect", n) {
		_, _ = d.cli.Run(ctx, "network", "disconnect", "-f", n, backend.KeeperID)
		_, _ = d.cli.Run(ctx, "network", "rm", n)
	}
	return nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// inspected is the part of `container inspect` zoo reads (same in podman).
type inspected struct {
	Name  string
	State struct {
		Status  string
		Running bool
	}
	Config struct {
		Labels map[string]string
	}
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string
		}
	}
}

func (d *Docker) toStatus(in inspected) backend.Status {
	st := backend.Status{
		ID:      strings.TrimPrefix(in.Name, "/"),
		State:   in.State.Status,
		Running: in.State.Running || backend.RunningState(in.State.Status),
		Labels:  in.Config.Labels,
	}
	if st.Labels == nil {
		st.Labels = map[string]string{}
	}
	st.IPs = map[string]string{}
	for name, n := range in.NetworkSettings.Networks {
		if n.IPAddress != "" {
			st.IPs[name] = n.IPAddress
		}
	}
	// A cage's address is on the shared cage network or its own one.
	if ip := st.IPs[d.network]; ip != "" {
		st.IP = ip
	} else {
		for name, ip := range st.IPs {
			if strings.HasPrefix(name, d.network+"-") {
				st.IP = ip
				break
			}
		}
	}
	return st
}

func (d *Docker) inspect(ctx context.Context, ids ...string) ([]backend.Status, error) {
	out, err := d.cli.Run(ctx, append([]string{"container", "inspect"}, ids...)...)
	var arr []inspected
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &arr); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("parse %s container inspect: %w", d.cli.Bin, jerr)
	}
	// A missing id makes inspect exit 1 but still print the rest.
	res := make([]backend.Status, 0, len(arr))
	for _, in := range arr {
		res = append(res, d.toStatus(in))
	}
	return res, nil
}

func (d *Docker) List(ctx context.Context) ([]backend.Status, error) {
	out, err := d.cli.Run(ctx, "ps", "-a", "-q", "--filter", "name=zoo-")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	all, err := d.inspect(ctx, ids...)
	if err != nil {
		return nil, err
	}
	res := all[:0]
	for _, s := range all {
		if backend.Prefixed(s.ID) {
			res = append(res, s)
		}
	}
	return res, nil
}

func (d *Docker) Get(ctx context.Context, id string) (backend.Status, bool, error) {
	res, err := d.inspect(ctx, id)
	if len(res) == 1 && res[0].ID == id {
		return res[0], true, nil
	}
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such") {
		return backend.Status{}, false, err
	}
	return backend.Status{}, false, nil
}

func (d *Docker) EnsureSidecar(ctx context.Context, s backend.SidecarSpec) (string, error) {
	st, exists, err := d.Get(ctx, s.ID)
	if err != nil {
		return "", err
	}
	if exists && !st.Running {
		if err := d.Start(ctx, s.ID); err != nil {
			return "", err
		}
	}
	if !exists {
		args := []string{"run", "-d", "--name", s.ID, "--network", backend.ServicesNetwork,
			"--dns", "127.0.0.1", "--security-opt", "no-new-privileges", "--pids-limit", "512",
			"--restart", "unless-stopped"}
		args = append(args, labelArgs(s.Labels)...)
		for _, e := range s.Env {
			args = append(args, "--env", e)
		}
		for _, m := range s.Mounts {
			args = append(args, "--mount", mountArg(m.Source, m.Target, m.ReadOnly))
		}
		if _, err := d.cli.Run(ctx, append(args, s.Image)...); err != nil && !alreadyExists(err) {
			// (The daemon and the CLI may race to create it; the loser is fine.)
			return "", fmt.Errorf("start sidecar %s: %w", s.ID, err)
		}
	}
	for i := 0; i < 40; i++ {
		if st, ok, _ := d.Get(ctx, s.ID); ok && st.IPs[backend.ServicesNetwork] != "" {
			return st.IPs[backend.ServicesNetwork], nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("sidecar %s has no address on %s", s.ID, backend.ServicesNetwork)
}

// HostBridge: Docker Desktop, OrbStack and podman machine route
// host.docker.internal / host.containers.internal to the Mac's loopback
// (verified on OrbStack; cages, on internal networks, can't resolve or reach
// it). On Linux the keeper reaches the host at its egress bridge's gateway.
func (d *Docker) HostBridge(ctx context.Context) (string, string, error) {
	if d.name == "podman" {
		return "127.0.0.1", "host.containers.internal", nil
	}
	osName, _ := d.cli.Run(ctx, "info", "--format", "{{.OperatingSystem}}")
	if strings.Contains(osName, "OrbStack") || strings.Contains(osName, "Docker Desktop") {
		return "127.0.0.1", "host.docker.internal", nil
	}
	gw, err := d.cli.Run(ctx, "network", "inspect", EgressNetwork, "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(gw)
	if len(f) == 0 {
		return "", "", fmt.Errorf("no gateway on %s", EgressNetwork)
	}
	return f[0], f[0], nil
}

func (d *Docker) Exec(ctx context.Context, s backend.ExecSpec) (int, error) {
	return d.cli.Exec(ctx, s)
}

func (d *Docker) Sample(ctx context.Context) ([]backend.Sample, error) {
	out, err := d.cli.Run(ctx, "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return parseStats(out), nil
}

// parseStats reads `stats --format '{{json .}}'` output: one JSON object per
// line with human-readable values ("0.50%", "1.2MiB / 2GiB", "1.1kB / 0B").
// Podman may print a JSON array instead; both are accepted, keys matched
// case- and underscore-insensitively.
func parseStats(out string) []backend.Sample {
	var rows []map[string]any
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &rows)
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &m) == nil {
				rows = append(rows, m)
			}
		}
	}
	var res []backend.Sample
	for _, row := range rows {
		f := map[string]string{}
		for k, v := range row {
			key := strings.ToLower(strings.ReplaceAll(k, "_", ""))
			switch t := v.(type) {
			case string:
				f[key] = t
			case float64:
				f[key] = strconv.FormatFloat(t, 'f', -1, 64)
			}
		}
		name := strings.TrimPrefix(f["name"], "/")
		if !backend.Prefixed(name) {
			continue
		}
		s := backend.Sample{ID: name, Have: map[string]bool{}}
		if v, ok := percent(first(f, "cpuperc", "cpupercent", "cpu")); ok {
			s.CPUPercent, s.Have["cpu"] = v, true
		}
		if a, b, ok := pair(first(f, "memusage")); ok {
			s.MemUsed, s.MemLimit, s.Have["mem"] = a, b, true
		}
		if a, b, ok := pair(first(f, "netio")); ok {
			s.NetRx, s.NetTx, s.Have["net"] = a, b, true
		}
		if a, b, ok := pair(first(f, "blockio")); ok {
			s.BlkRead, s.BlkWrite, s.Have["blk"] = a, b, true
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(first(f, "pids")), 64); err == nil {
			s.Pids, s.Have["pids"] = v, true
		}
		res = append(res, s)
	}
	return res
}

func first(f map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := f[k]; ok {
			return v
		}
	}
	return ""
}

func percent(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func pair(s string) (float64, float64, bool) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, false
	}
	x, ok1 := ParseBytes(a)
	y, ok2 := ParseBytes(b)
	return x, y, ok1 && ok2
}

var units = map[string]float64{
	"b": 1, "": 1,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// ParseBytes reads docker's human sizes: "648B", "1.2kB", "1.5MiB", "2GiB".
func ParseBytes(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	m, ok := units[strings.ToLower(strings.TrimSpace(s[i:]))]
	if !ok {
		return 0, false
	}
	return n * m, true
}

func alreadyExists(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already in use") || strings.Contains(m, "already exists") || strings.Contains(m, "conflict")
}
