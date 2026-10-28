package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dusty/zoo/test/fakes/fakestate"
)

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// appleCmd emulates Apple `container` 1.6.1.
func (f *fake) appleCmd(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "--version":
		fmt.Fprintln(f.out, "container CLI version 1.6.1 (build: release, commit: aa8d76b)")
		return nil
	case "system":
		return f.appleSystem(rest)
	case "list", "ls":
		return f.appleList(rest)
	case "inspect":
		return f.appleInspect(rest)
	case "stats":
		return f.appleStats(rest)
	case "network":
		return f.appleNetwork(rest)
	case "image", "images", "i":
		return f.appleImage(rest)
	case "build":
		return f.build(rest)
	case "run", "create":
		return f.appleRun(cmd, rest)
	case "start":
		return f.appleStartStop(cmd, rest)
	case "stop":
		return f.appleStartStop(cmd, rest)
	case "delete", "rm":
		return f.appleDelete(rest)
	case "exec":
		return f.prepareExec(rest,
			func(id string) error { return fmt.Errorf("Error: container %s not found", id) },
			func(id string) error { return fmt.Errorf("Error: container %s is not running", id) })
	}
	return usagef("unknown command %q", cmd)
}

func (f *fake) appleDown() error {
	if down() {
		return fmt.Errorf("Error: XPC connection error: Connection invalid (is `container system start` running?)")
	}
	return nil
}

func (f *fake) appleSystem(args []string) error {
	if len(args) == 0 {
		return usagef("system: missing subcommand")
	}
	switch args[0] {
	case "status":
		if _, _, err := parse("system status", defs(), args[1:], false); err != nil {
			return err
		}
		if down() {
			return codeError{1, "apiserver is not running and not registered with launchd"}
		}
		fmt.Fprintln(f.out, "apiserver is running")
		return nil
	case "start":
		if down() {
			return fmt.Errorf("Error: failed to start apiserver")
		}
		return nil
	}
	return usagef("unknown command \"system %s\"", args[0])
}

func formatFlag(p parsed) (bool, error) {
	switch p.last("format") {
	case "json":
		return true, nil
	case "", "table":
		return false, nil
	}
	return false, usagef("unsupported --format %q (json, table)", p.last("format"))
}

func (f *fake) appleList(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	p, pos, err := parse("list", defs("-a,--all", "--format=", "-q,--quiet"), args, false)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("list takes no arguments")
	}
	asJSON, err := formatFlag(p)
	if err != nil {
		return err
	}
	var arr []any
	for _, c := range f.st.Containers {
		if !c.Running && !p.has("all") {
			continue
		}
		if p.has("quiet") {
			fmt.Fprintln(f.out, c.ID)
			continue
		}
		arr = append(arr, f.appleJSON(c))
	}
	if p.has("quiet") {
		return nil
	}
	if asJSON {
		if arr == nil {
			arr = []any{}
		}
		return f.printJSON(arr)
	}
	fmt.Fprintln(f.out, "ID  IMAGE  OS  ARCH  STATE  ADDR")
	for _, c := range f.st.Containers {
		if c.Running || p.has("all") {
			fmt.Fprintf(f.out, "%s  %s  linux  arm64  %s  %s\n", c.ID, c.Image, appleState(c), c.IPs[firstNet(c)])
		}
	}
	return nil
}

func firstNet(c *fakestate.Container) string {
	if len(c.Networks) > 0 {
		return c.Networks[0]
	}
	return ""
}

func appleState(c *fakestate.Container) string {
	if c.Running {
		return "running"
	}
	return "stopped"
}

// appleJSON is one element of `list --format json` / `inspect`.
func (f *fake) appleJSON(c *fakestate.Container) map[string]any {
	var cfgNets, stNets []any
	for _, n := range c.Networks {
		cfgNets = append(cfgNets, map[string]any{"network": n, "options": map[string]any{"hostname": c.Name}})
		if c.Running {
			net := f.st.Net(n)
			gw := ""
			if net != nil {
				gw = fmt.Sprintf("192.168.%d.1", net.Subnet)
			}
			stNets = append(stNets, map[string]any{
				"network":     n,
				"hostname":    c.Name,
				"ipv4Address": c.IPs[n] + "/24",
				"ipv4Gateway": gw,
			})
		}
	}
	if stNets == nil {
		stNets = []any{}
	}
	if cfgNets == nil {
		cfgNets = []any{}
	}
	mounts := []any{}
	for _, m := range c.Mounts {
		opts := []any{}
		if m.ReadOnly {
			opts = append(opts, "ro")
		}
		mounts = append(mounts, map[string]any{
			"type":        map[string]any{"virtiofs": map[string]any{}},
			"source":      m.Source,
			"destination": m.Target,
			"options":     opts,
		})
	}
	labels := c.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	env := c.Env
	if env == nil {
		env = []string{}
	}
	return map[string]any{
		"id": c.ID,
		"configuration": map[string]any{
			"id":        c.ID,
			"image":     map[string]any{"reference": normalizeAppleRef(c.Image)},
			"labels":    labels,
			"resources": map[string]any{"cpus": int(c.CPUs), "memoryInBytes": c.Memory},
			"networks":  cfgNets,
			"mounts":    mounts,
			"initProcess": map[string]any{
				"environment": env,
				"arguments":   c.Cmd,
			},
		},
		"status": map[string]any{
			"state":    appleState(c),
			"networks": stNets,
		},
	}
}

func (f *fake) appleInspect(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	_, pos, err := parse("inspect", defs(), args, false)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("inspect needs at least one container")
	}
	arr := []any{}
	for _, id := range pos {
		c := f.st.Find(id)
		if c == nil {
			return fmt.Errorf("Error: container not found: %s", id)
		}
		arr = append(arr, f.appleJSON(c))
	}
	return f.printJSON(arr)
}

func (f *fake) appleStats(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	p, pos, err := parse("stats", defs("--no-stream", "--format="), args, false)
	if err != nil {
		return err
	}
	if !p.has("no-stream") {
		return usagef("fake only supports stats --no-stream")
	}
	asJSON, err := formatFlag(p)
	if err != nil {
		return err
	}
	if !asJSON {
		return usagef("fake only supports stats --format json")
	}
	arr := []any{}
	for _, c := range f.st.Containers {
		if !c.Running || (len(pos) > 0 && !containsStr(pos, c.ID)) {
			continue
		}
		arr = append(arr, map[string]any{
			"id":               c.ID,
			"cpuUsageUsec":     123456,
			"memoryUsageBytes": 52428800,
			"memoryLimitBytes": c.Memory,
			"networkRxBytes":   16700,
			"networkTxBytes":   4280,
			"blockReadBytes":   221000,
			"blockWriteBytes":  4096,
			"numProcesses":     3,
		})
	}
	return f.printJSON(arr)
}

func (f *fake) appleNetwork(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	if len(args) == 0 {
		return usagef("network: missing subcommand")
	}
	switch args[0] {
	case "list", "ls":
		p, pos, err := parse("network list", defs("--format=", "-q,--quiet"), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("network list takes no arguments")
		}
		asJSON, err := formatFlag(p)
		if err != nil {
			return err
		}
		if !asJSON {
			for _, n := range f.st.Networks {
				fmt.Fprintln(f.out, n.Name)
			}
			return nil
		}
		arr := []any{}
		for _, n := range f.st.Networks {
			mode := "nat"
			if n.Internal {
				mode = "hostOnly"
			}
			labels := n.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			arr = append(arr, map[string]any{
				"id":     n.Name,
				"state":  "running",
				"config": map[string]any{"id": n.Name, "mode": mode, "labels": labels},
				"status": map[string]any{
					"ipv4Subnet":  fmt.Sprintf("192.168.%d.0/24", n.Subnet),
					"ipv4Gateway": fmt.Sprintf("192.168.%d.1", n.Subnet),
				},
			})
		}
		return f.printJSON(arr)
	case "create":
		p, pos, err := parse("network create", defs("--internal", "--label=", "--subnet="), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("network create needs exactly one name")
		}
		if f.st.Net(pos[0]) != nil {
			return fmt.Errorf("Error: network %s already exists", pos[0])
		}
		labels, err := parseLabels(p["label"])
		if err != nil {
			return err
		}
		f.newNetwork(pos[0], p.has("internal"), labels)
		fmt.Fprintln(f.out, pos[0])
		return nil
	case "delete", "rm":
		_, pos, err := parse("network delete", defs(), args[1:], false)
		if err != nil {
			return err
		}
		for _, name := range pos {
			if name == "default" || f.st.Net(name) == nil {
				return fmt.Errorf("Error: cannot delete network %s", name)
			}
			out := f.st.Networks[:0]
			for _, n := range f.st.Networks {
				if n.Name != name {
					out = append(out, n)
				}
			}
			f.st.Networks = out
		}
		return nil
	}
	return usagef("unknown command \"network %s\"", args[0])
}

// normalizeAppleRef stores names fully qualified like the real CLI:
// zoo-cage -> docker.io/library/zoo-cage:latest.
func normalizeAppleRef(ref string) string {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	parts := strings.Split(name, "/")
	if len(parts) == 1 {
		name = "docker.io/library/" + name
	} else if !strings.ContainsAny(parts[0], ".:") && parts[0] != "localhost" {
		name = "docker.io/" + name
	}
	return name + ":" + tag
}

// normalizeDockerRef adds :latest.
func normalizeDockerRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref
	}
	return ref + ":latest"
}

func (f *fake) normRef(ref string) string {
	if f.apple() {
		return normalizeAppleRef(ref)
	}
	return normalizeDockerRef(ref)
}

func (f *fake) appleImage(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	if len(args) == 0 {
		return usagef("image: missing subcommand")
	}
	switch args[0] {
	case "inspect":
		_, pos, err := parse("image inspect", defs(), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) == 0 {
			return usagef("image inspect needs a reference")
		}
		arr := []any{}
		for _, ref := range pos {
			if !containsStr(f.st.Images, f.normRef(ref)) {
				return fmt.Errorf("Error: image not found: %s", ref)
			}
			arr = append(arr, map[string]any{"name": f.normRef(ref), "index": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json"}})
		}
		return f.printJSON(arr)
	case "list", "ls":
		p, pos, err := parse("image list", defs("--format=", "-q,--quiet", "-v,--verbose"), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("image list takes no arguments")
		}
		asJSON, err := formatFlag(p)
		if err != nil {
			return err
		}
		if !asJSON {
			for _, i := range f.st.Images {
				fmt.Fprintln(f.out, i)
			}
			return nil
		}
		arr := []any{}
		for _, i := range f.st.Images {
			arr = append(arr, map[string]any{"reference": i, "descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "size": 1234}})
		}
		return f.printJSON(arr)
	}
	return usagef("unknown command \"image %s\"", args[0])
}

// build is `build -t tag [--build-arg k=v] [-f file] dir` for both dialects.
func (f *fake) build(args []string) error {
	if f.apple() {
		if err := f.appleDown(); err != nil {
			return err
		}
	} else if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse("build", defs("-t,--tag=", "--build-arg=", "-f,--file=", "-q,--quiet", "--no-cache"), args, false)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("build needs exactly one context directory")
	}
	if st, ok := pathExists(pos[0]); !ok || !st.IsDir() {
		return fmt.Errorf("Error: build context %s is not a directory", pos[0])
	}
	for _, a := range p["build-arg"] {
		if !strings.Contains(a, "=") {
			return usagef("invalid --build-arg %q", a)
		}
	}
	if !p.has("tag") {
		return usagef("fake needs build -t")
	}
	for _, t := range p["tag"] {
		ref := f.normRef(t)
		if !containsStr(f.st.Images, ref) {
			f.st.Images = append(f.st.Images, ref)
		}
		fmt.Fprintf(f.errw, "Successfully built %s\n", ref)
	}
	return nil
}

// appleRun emulates `container run -d` / `container create`.
func (f *fake) appleRun(cmd string, args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	p, pos, err := parse(cmd, defs("-d,--detach", "--name=", "--network=", "-l,--label=", "-e,--env=",
		"--env-file=", "-v,--volume=", "--mount=", "--no-dns", "--dns=", "-c,--cpus=", "-m,--memory=",
		"--cap-drop=", "--cap-add=", "--ulimit=", "-u,--user=", "-w,--workdir=", "--rm", "--read-only",
		"--tmpfs=", "--entrypoint="), args, true)
	if err != nil {
		return err
	}
	if cmd == "run" && !p.has("detach") {
		return usagef("fake only supports run -d")
	}
	if len(pos) == 0 {
		return usagef("%s needs an image", cmd)
	}
	name := p.last("name")
	if name == "" {
		name = fmt.Sprintf("fake-%d", f.st.Seq+1)
		f.st.Seq++
	}
	if f.st.Find(name) != nil {
		return fmt.Errorf("Error: container with id %s already exists", name)
	}
	c := &fakestate.Container{ID: name, Name: name, Image: pos[0], Cmd: pos[1:], Args: append([]string{cmd}, args...),
		IPs: map[string]string{}, LastIPs: map[string]string{}}
	if c.Labels, err = parseLabels(p["label"]); err != nil {
		return err
	}
	if err := checkEnv(p["env"]); err != nil {
		return err
	}
	c.Env = p["env"]
	for _, ef := range p["env-file"] {
		vals, err := readEnvFile(ef)
		if err != nil {
			return fmt.Errorf("Error: %v", err)
		}
		c.Env = append(c.Env, vals...)
	}
	nets := p["network"]
	if len(nets) == 0 {
		nets = []string{"default"}
	}
	for _, n := range nets {
		if f.st.Net(n) == nil {
			return fmt.Errorf("Error: network %s not found", n)
		}
		if containsStr(c.Networks, n) {
			return fmt.Errorf("Error: network %s given twice", n)
		}
		c.Networks = append(c.Networks, n)
	}
	c.NoDNS = p.has("no-dns")
	c.DNS = p["dns"]
	c.CPUs = 4
	if v := p.last("cpus"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return usagef("invalid --cpus %q (an integer)", v)
		}
		c.CPUs = float64(n)
	}
	c.Memory = 1 << 30
	if v := p.last("memory"); v != "" {
		if c.Memory, err = parseMem(v); err != nil {
			return usagef("%v", err)
		}
	}
	c.CapDrop, c.CapAdd = p["cap-drop"], p["cap-add"]
	for _, u := range p["ulimit"] {
		k, v, ok := strings.Cut(u, "=")
		if !ok || k == "" {
			return usagef("invalid --ulimit %q (name=soft[:hard])", u)
		}
		soft, hard, _ := strings.Cut(v, ":")
		if _, err := strconv.Atoi(soft); err != nil {
			return usagef("invalid --ulimit %q", u)
		}
		if hard != "" {
			if _, err := strconv.Atoi(hard); err != nil {
				return usagef("invalid --ulimit %q", u)
			}
		}
		c.Ulimits = append(c.Ulimits, u)
	}
	c.User, c.ReadOnlyFS, c.Tmpfs = p.last("user"), p.has("read-only"), p["tmpfs"]
	for _, v := range p["volume"] {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return usagef("invalid volume %q (src:dst[:ro])", v)
		}
		ro := false
		if len(parts) == 3 {
			switch parts[2] {
			case "ro":
				ro = true
			case "rw":
			default:
				return usagef("invalid volume option %q in %q", parts[2], v)
			}
		}
		if _, ok := pathExists(parts[0]); !ok {
			return fmt.Errorf("Error: path '%s' does not exist", parts[0])
		}
		c.Mounts = append(c.Mounts, fakestate.Mount{Source: parts[0], Target: parts[1], ReadOnly: ro, Via: "-v"})
	}
	for _, v := range p["mount"] {
		m, err := parseMountCSV(v)
		if err != nil {
			return err
		}
		st, ok := pathExists(m.Source)
		if !ok {
			return fmt.Errorf("Error: path '%s' does not exist", m.Source)
		}
		if !st.IsDir() {
			// Verified on container 1.6.1: --mount only takes directories.
			return fmt.Errorf("Error: path '%s' is not a directory", m.Source)
		}
		c.Mounts = append(c.Mounts, m)
	}
	c.Mounts = dropShadowedDirs(c.Mounts)
	f.st.Containers = append(f.st.Containers, c)
	if cmd == "run" {
		f.startC(c)
	}
	fmt.Fprintln(f.out, c.ID)
	return nil
}

// dropShadowedDirs emulates a quirk verified on container 1.6.1: a single
// file mount shares the file's parent directory with the VM, and a directory
// mount of that same source is then silently dropped.
func dropShadowedDirs(ms []fakestate.Mount) []fakestate.Mount {
	parents := map[string]bool{}
	for _, m := range ms {
		if st, ok := pathExists(m.Source); ok && !st.IsDir() {
			parents[filepath.Clean(filepath.Dir(m.Source))] = true
		}
	}
	out := ms[:0]
	for _, m := range ms {
		if st, ok := pathExists(m.Source); ok && st.IsDir() && parents[filepath.Clean(m.Source)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// parseMountCSV reads --mount type=bind,source=..,target=..[,readonly]
// (fields may be CSV-quoted).
func parseMountCSV(v string) (fakestate.Mount, error) {
	r := csv.NewReader(strings.NewReader(v))
	fields, err := r.Read()
	if err != nil {
		return fakestate.Mount{}, usagef("invalid --mount %q: %v", v, err)
	}
	m := fakestate.Mount{Via: "--mount"}
	typ := ""
	for _, fld := range fields {
		k, val, hasVal := strings.Cut(fld, "=")
		switch strings.ToLower(k) {
		case "type":
			typ = val
		case "source", "src":
			m.Source = val
		case "target", "destination", "dst":
			m.Target = val
		case "readonly", "ro":
			if !hasVal {
				m.ReadOnly = true
				break
			}
			b, err := strconv.ParseBool(val)
			if err != nil {
				return m, usagef("invalid readonly value %q in --mount %q", val, v)
			}
			m.ReadOnly = b
		case "bind-propagation", "consistency":
		default:
			return m, usagef("unknown --mount field %q in %q", k, v)
		}
	}
	if typ != "bind" {
		return m, usagef("fake only supports --mount type=bind (got %q)", typ)
	}
	if m.Source == "" || m.Target == "" {
		return m, usagef("--mount %q needs source and target", v)
	}
	return m, nil
}

func (f *fake) appleStartStop(cmd string, args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	fs := defs()
	if cmd == "stop" {
		fs = defs("-s,--signal=", "-t,--time=", "-a,--all")
	}
	_, pos, err := parse(cmd, fs, args, false)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("%s needs a container", cmd)
	}
	for _, id := range pos {
		c := f.st.Find(id)
		if c == nil {
			return fmt.Errorf("Error: container %s not found", id)
		}
		if cmd == "start" {
			f.startC(c)
		} else {
			f.stopC(c)
		}
		fmt.Fprintln(f.out, c.ID)
	}
	return nil
}

func (f *fake) appleDelete(args []string) error {
	if err := f.appleDown(); err != nil {
		return err
	}
	p, pos, err := parse("delete", defs("-f,--force", "-a,--all"), args, false)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("delete needs a container")
	}
	for _, id := range pos {
		c := f.st.Find(id)
		if c == nil {
			return fmt.Errorf("Error: container %s not found", id)
		}
		if c.Running && !p.has("force") {
			return fmt.Errorf("Error: container %s is running; stop it or use --force", id)
		}
		f.removeC(c)
		fmt.Fprintln(f.out, c.ID)
	}
	return nil
}
