// Package apple drives Apple's `container` CLI (github.com/apple/container):
// one lightweight VM per cage. Every call shells out, so zoo works with
// whatever version is installed; JSON output is read loosely through jsonx.
package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/jsonx"
)

func init() {
	backend.Register("apple", func(o backend.Options) backend.Backend { return New(o) })
}

// Apple is the `container` backend.
type Apple struct {
	cli      backend.CLI
	network  string
	maskFile string
	stageDir string
}

// New builds the backend. Bin defaults to $ZOO_CONTAINER_BIN, then `container`.
func New(o backend.Options) *Apple {
	bin := o.Bin
	if bin == "" {
		bin = os.Getenv("ZOO_CONTAINER_BIN")
	}
	if bin == "" {
		bin = "container"
	}
	return &Apple{cli: backend.CLI{Bin: bin}, network: o.Network, maskFile: o.MaskFile, stageDir: o.StageDir}
}

func (a *Apple) Name() string { return "apple" }

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func (a *Apple) Detect(ctx context.Context) backend.Info {
	info := backend.Info{
		Name: "apple",
		Caps: backend.Capabilities{
			Isolation:            backend.IsoVM,
			InternalNetworks:     true,
			HostGatewayReachable: true, // apple/container#1320
			MultiNetworkAttach:   true,
			HostFileMounts:       true, // via -v, not --mount
			ReadOnlyMounts:       true,
			PidsLimit:            true, // via --ulimit nproc
			PerCageKernel:        true,
		},
		Hint: "install Apple's `container` from https://github.com/apple/container/releases (macOS 26+, Apple silicon)",
	}
	if !a.cli.Installed() {
		return info
	}
	info.Installed = true
	if out, err := a.cli.Run(ctx, "--version"); err == nil {
		info.Version = strings.TrimSpace(out)
		if m := versionRe.FindString(out); m != "" {
			info.Version = m // "container CLI version 1.6.1 (build: …)" -> 1.6.1
		}
	}
	info.Running = a.cli.OK(ctx, "system", "status")
	if !info.Running {
		info.Hint = "run `container system start`"
	} else {
		info.Hint = ""
	}
	return info
}

func (a *Apple) Prepare(ctx context.Context) error {
	if a.cli.OK(ctx, "system", "status") {
		return nil
	}
	// Interactive: it may ask to install a kernel.
	if err := a.cli.Attach(ctx, "system", "start"); err != nil {
		return fmt.Errorf("container system start: %w", err)
	}
	return nil
}

func (a *Apple) listJSON(ctx context.Context, args ...string) ([]any, error) {
	out, err := a.cli.Run(ctx, append(args, "--format", "json")...)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parse %s output: %w", strings.Join(args, " "), err)
	}
	if arr, ok := v.([]any); ok {
		return arr, nil
	}
	return []any{v}, nil
}

func (a *Apple) ImageExists(ctx context.Context, ref string) (bool, error) {
	if a.cli.OK(ctx, "image", "inspect", ref) {
		return true, nil
	}
	// Older releases lack `image inspect`; names are stored fully qualified
	// (docker.io/library/zoo-cage:latest), so match by suffix.
	arr, err := a.listJSON(ctx, "image", "list")
	if err != nil {
		return false, err
	}
	name, tag, _ := strings.Cut(ref, ":")
	for _, img := range arr {
		found := false
		jsonx.Walk(img, func(_ string, v any) {
			s, ok := v.(string)
			if !ok {
				return
			}
			if s == ref || strings.HasSuffix(s, "/"+ref) || (tag == "latest" && (s == name || strings.HasSuffix(s, "/"+name))) {
				found = true
			}
		})
		if found {
			return true, nil
		}
	}
	return false, nil
}

func (a *Apple) BuildImage(ctx context.Context, s backend.ImageSpec) error {
	args := []string{"build", "-t", s.Tag}
	for _, k := range backend.SortedKeys(s.BuildArgs) {
		args = append(args, "--build-arg", k+"="+s.BuildArgs[k])
	}
	return a.cli.Attach(ctx, append(args, s.Context)...)
}

func (a *Apple) networkExists(ctx context.Context, name string) bool {
	arr, err := a.listJSON(ctx, "network", "list")
	if err != nil {
		return false
	}
	for _, n := range arr {
		// Match the id only: a label value equal to the name doesn't count.
		if m, ok := n.(map[string]any); ok && m["id"] == name {
			return true
		}
	}
	return false
}

func (a *Apple) EnsureNetwork(ctx context.Context) error {
	for _, n := range []string{a.network, backend.ServicesNetwork} {
		if a.networkExists(ctx, n) {
			continue
		}
		if _, err := a.cli.Run(ctx, "network", "create", "--internal", "--label", "zoo=1", n); err != nil {
			return fmt.Errorf("create internal network %s (needs macOS 26+): %w", n, err)
		}
	}
	return nil
}

func labelArgs(labels map[string]string) []string {
	var out []string
	for _, k := range backend.SortedKeys(labels) {
		out = append(out, "-l", k+"="+labels[k])
	}
	return out
}

func (a *Apple) EnsureKeeper(ctx context.Context, s backend.KeeperSpec) (string, error) {
	st, exists, _ := a.Get(ctx, s.ID)
	if exists && !st.Running {
		if err := a.Start(ctx, s.ID); err != nil {
			_ = a.Delete(ctx, s.ID)
			exists = false
		}
	}
	if !exists {
		args := []string{"run", "-d", "--name", s.ID,
			// `container` can't attach networks later, so the keeper joins
			// every network it will ever need now.
			"--network", "default", "--network", a.network, "--network", backend.ServicesNetwork,
			"-c", fmt.Sprint(s.CPUs), "-m", s.Memory}
		args = append(args, labelArgs(s.Labels)...)
		args = append(args, "-v", s.EtcDir+":/etc/zoo", "-v", s.LogDir+":/var/log/zoo", s.Image)
		if _, err := a.cli.Run(ctx, args...); err != nil {
			return "", fmt.Errorf("start keeper: %w", err)
		}
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		ip, err := a.inspectIP(ctx, s.ID)
		if err == nil {
			return ip, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("keeper has no address on %s: %w", a.network, lastErr)
}

func (a *Apple) inspectIP(ctx context.Context, id string) (string, error) {
	out, err := a.cli.Run(ctx, "inspect", id)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", err
	}
	ip := jsonx.IPOnNetwork(v, a.network)
	if ip == "" {
		return "", fmt.Errorf("no address for %s on network %s", id, a.network)
	}
	return ip, nil
}

// RunArgs is the `container run` invocation for a cage.
//
// Masks and the git guard are host-side `-v src:dst:ro` mounts layered over
// the worktree mount. Verified on container 1.6.1: `--mount` rejects single
// files but `-v` takes them, and the guest's root has no CAP_SYS_ADMIN by
// default, so in-guest bind mounts (v0's approach) fail. A file `-v` shares
// the file's parent directory with the VM (only reachable with
// CAP_SYS_ADMIN, which cages never get), so the mask file lives alone in its
// own directory.
func (a *Apple) RunArgs(s backend.CageSpec) ([]string, error) {
	args := []string{"run", "-d",
		"--name", s.ID,
		"--network", or(s.Network, a.network),
		"--no-dns",
		"-c", fmt.Sprint(s.CPUs),
		"-m", s.Memory,
		"--cap-drop", "ALL",
	}
	if s.Pids > 0 {
		// No --pids-limit in `container`; RLIMIT_NPROC caps the agent user.
		args = append(args, "--ulimit", fmt.Sprintf("nproc=%d:%d", s.Pids, s.Pids))
	}
	args = append(args, labelArgs(s.Labels)...)
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	args = append(args, "-e", "ZOO_GUARD=host")
	vol := func(src, dst string, ro bool) error {
		if strings.Contains(src, ":") || strings.Contains(dst, ":") {
			return fmt.Errorf("can't mount %q into an Apple cage: paths with ':' aren't supported by `container -v`", src)
		}
		v := src + ":" + dst
		if ro {
			v += ":ro"
		}
		args = append(args, "-v", v)
		return nil
	}
	for _, m := range s.Mounts {
		if err := vol(m.Source, m.Target, m.ReadOnly); err != nil {
			return nil, err
		}
	}
	for _, p := range backend.Existing(s.Pin) {
		if err := vol(p, p, false); err != nil {
			return nil, err
		}
	}
	// Directories mounted as directories: the worktree, extra rw dirs, pins,
	// and read-only dirs.
	dirMounted := map[string]bool{}
	for _, m := range s.Mounts {
		dirMounted[m.Source] = true
	}
	for _, p := range s.Pin {
		dirMounted[p] = true
	}
	for _, p := range s.ReadOnly {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			dirMounted[p] = true
		}
	}
	for i, p := range backend.Existing(s.ReadOnly) {
		src := p
		if st, err := os.Stat(p); err == nil && !st.IsDir() && dirMounted[filepath.Dir(p)] {
			// `container` shares a file mount's parent directory and can then
			// drop a directory mount of that same source; seen undoing a pin
			// on 1.6.1. Mount a staged read-only copy instead; host edits show
			// up in the cage after `zoo reset`.
			staged, err := a.stage(s.ID, i, p)
			if err != nil {
				return nil, fmt.Errorf("stage read-only copy of %s: %w", p, err)
			}
			src = staged
		}
		if err := vol(src, p, true); err != nil {
			return nil, err
		}
	}
	if masks := backend.Existing(s.Masks); len(masks) > 0 {
		if a.maskFile == "" {
			return nil, fmt.Errorf("apple backend needs a mask file to hide %d secret file(s)", len(masks))
		}
		for _, p := range masks {
			if err := vol(a.maskFile, p, true); err != nil {
				return nil, err
			}
		}
	}
	return append(args, s.Image), nil
}

// stage copies a file to <stageDir>/<cage>/<i>/<name>, alone in its directory
// because the whole directory is shared with the guest.
func (a *Apple) stage(id string, i int, p string) (string, error) {
	if a.stageDir == "" {
		return "", fmt.Errorf("no stage dir configured")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.stageDir, id, strconv.Itoa(i))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(p))
	_ = os.Remove(dst)
	return dst, os.WriteFile(dst, b, 0o444)
}

func (a *Apple) CreateCage(ctx context.Context, s backend.CageSpec) error {
	if a.stageDir != "" {
		_ = os.RemoveAll(filepath.Join(a.stageDir, s.ID))
	}
	args, err := a.RunArgs(s)
	if err != nil {
		return err
	}
	_, err = a.cli.Run(ctx, args...)
	return err
}

func (a *Apple) Start(ctx context.Context, id string) error {
	_, err := a.cli.Run(ctx, "start", id)
	return err
}

func (a *Apple) Stop(ctx context.Context, id string) error {
	_, err := a.cli.Run(ctx, "stop", id)
	return err
}

func (a *Apple) Delete(ctx context.Context, id string) error {
	_, err := a.cli.Run(ctx, "delete", "--force", id)
	if err == nil && a.stageDir != "" {
		_ = os.RemoveAll(filepath.Join(a.stageDir, id))
	}
	return err
}

func (a *Apple) List(ctx context.Context) ([]backend.Status, error) {
	arr, err := a.listJSON(ctx, "list", "--all")
	if err != nil {
		return nil, err
	}
	out := make([]backend.Status, 0, len(arr))
	for _, el := range arr {
		st := backend.Status{Labels: map[string]string{}}
		st.ID = jsonx.String(el, "id")
		if !backend.Prefixed(st.ID) {
			continue
		}
		st.State = jsonx.String(el, "status", "state")
		st.Running = backend.RunningState(st.State)
		if m := jsonx.Map(el, "labels"); m != nil {
			for k, v := range m {
				if s, ok := v.(string); ok {
					st.Labels[k] = s
				}
			}
		}
		st.IP = jsonx.IPOnNetwork(el, a.network)
		st.IPs = map[string]string{}
		if m, ok := el.(map[string]any); ok {
			if stat, ok := m["status"].(map[string]any); ok {
				nets, _ := stat["networks"].([]any)
				for _, n := range nets {
					nm, _ := n.(map[string]any)
					name, _ := nm["network"].(string)
					addr, _ := nm["ipv4Address"].(string)
					if name != "" && addr != "" {
						ip, _, _ := strings.Cut(addr, "/")
						st.IPs[name] = ip
					}
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

func (a *Apple) Get(ctx context.Context, id string) (backend.Status, bool, error) {
	all, err := a.List(ctx)
	if err != nil {
		return backend.Status{}, false, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, true, nil
		}
	}
	return backend.Status{}, false, nil
}

// CageNetwork: Apple cages share one network, because `container` can't
// attach a network to the running keeper.
func (a *Apple) CageNetwork(ctx context.Context, cageID string) (string, string, error) {
	ip, err := a.inspectIP(ctx, backend.KeeperID)
	return a.network, ip, err
}

func (a *Apple) EnsureSidecar(ctx context.Context, s backend.SidecarSpec) (string, error) {
	st, exists, err := a.Get(ctx, s.ID)
	if err != nil {
		return "", err
	}
	if exists && !st.Running {
		if err := a.Start(ctx, s.ID); err != nil {
			return "", err
		}
	}
	if !exists {
		args := []string{"run", "-d", "--name", s.ID, "--network", backend.ServicesNetwork, "--no-dns"}
		args = append(args, labelArgs(s.Labels)...)
		for _, e := range s.Env {
			args = append(args, "-e", e)
		}
		for _, m := range s.Mounts {
			if strings.Contains(m.Source, ":") || strings.Contains(m.Target, ":") {
				return "", fmt.Errorf("can't mount %q into an Apple sidecar: paths with ':' aren't supported", m.Source)
			}
			v := m.Source + ":" + m.Target
			if m.ReadOnly {
				v += ":ro"
			}
			args = append(args, "-v", v)
		}
		if _, err := a.cli.Run(ctx, append(args, s.Image)...); err != nil && !alreadyExists(err) {
			// (The daemon and the CLI may race to create it; the loser is fine.)
			return "", fmt.Errorf("start sidecar %s: %w", s.ID, err)
		}
	}
	for i := 0; i < 40; i++ {
		if st, ok, _ := a.Get(ctx, s.ID); ok && st.IPs[backend.ServicesNetwork] != "" {
			return st.IPs[backend.ServicesNetwork], nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", fmt.Errorf("sidecar %s has no address on %s", s.ID, backend.ServicesNetwork)
}

// HostBridge listens on the host's address on the `default` network: the
// keeper reaches it there; cages, on host-only networks, can't (verified on
// 1.6.1).
func (a *Apple) HostBridge(ctx context.Context) (string, string, error) {
	arr, err := a.listJSON(ctx, "network", "list")
	if err != nil {
		return "", "", err
	}
	for _, n := range arr {
		m, _ := n.(map[string]any)
		if m["id"] != "default" {
			continue
		}
		if gw := jsonx.String(m, "ipv4Gateway"); gw != "" {
			return gw, gw, nil
		}
	}
	return "", "", fmt.Errorf("can't find the host's address on the default network")
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *Apple) Exec(ctx context.Context, s backend.ExecSpec) (int, error) {
	return a.cli.Exec(ctx, s)
}

func (a *Apple) Sample(ctx context.Context) ([]backend.Sample, error) {
	arr, err := a.listJSON(ctx, "stats", "--no-stream")
	if err != nil {
		return nil, err
	}
	out := make([]backend.Sample, 0, len(arr))
	for _, el := range arr {
		if s := parseStat(el); s.ID != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func alreadyExists(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already in use") || strings.Contains(m, "already exists") || strings.Contains(m, "conflict")
}
