package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/dusty/zoo/test/fakes/fakestate"
)

const (
	jsonFormat    = "{{json .}}"
	gatewayFormat = "{{range .IPAM.Config}}{{.Gateway}} {{end}}"
)

func (f *fake) dockerNetJSON(n *fakestate.Network) map[string]any {
	containers := map[string]any{}
	for _, c := range f.st.Containers {
		if c.Running && containsStr(c.Networks, n.Name) {
			containers[c.ID] = map[string]any{"Name": c.Name, "IPv4Address": c.IPs[n.Name] + "/24"}
		}
	}
	labels := n.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return map[string]any{
		"Name": n.Name, "Id": hashID("net/" + n.Name), "Driver": "bridge", "Internal": n.Internal, "Labels": labels,
		"IPAM": map[string]any{"Config": []any{map[string]any{
			"Subnet": fmt.Sprintf("192.168.%d.0/24", n.Subnet), "Gateway": fmt.Sprintf("192.168.%d.1", n.Subnet)}}},
		"Containers": containers,
	}
}

// dockerCmd emulates the docker 29 CLI (as on OrbStack); podman shares it
// except for `info`.
func (f *fake) dockerCmd(args []string) error {
	if len(args) == 0 {
		return usagef("missing subcommand")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "container" && len(rest) > 0 {
		switch rest[0] {
		case "inspect", "start", "stop", "rm", "create", "run", "exec", "ls", "list", "ps", "stats":
			cmd, rest = rest[0], rest[1:]
			if cmd == "ls" || cmd == "list" {
				cmd = "ps"
			}
		default:
			return usagef("unknown command \"container %s\"", rest[0])
		}
	}
	switch cmd {
	case "--version", "-v":
		if f.dialect == "podman" {
			fmt.Fprintln(f.out, "podman version 5.2.0")
		} else {
			fmt.Fprintln(f.out, "Docker version 29.8.2, build 1a2b3c4")
		}
		return nil
	case "info":
		return f.dockerInfo(rest)
	case "image":
		return f.dockerImage(rest)
	case "build":
		return f.build(rest)
	case "network":
		return f.dockerNetwork(rest)
	case "create", "run":
		return f.dockerRun(cmd, rest)
	case "start", "stop":
		return f.dockerStartStop(cmd, rest)
	case "rm":
		return f.dockerRm(rest)
	case "ps":
		return f.dockerPs(rest)
	case "inspect":
		return f.dockerInspect(rest)
	case "stats":
		return f.dockerStats(rest)
	case "exec":
		if err := f.dockerDown(); err != nil {
			return err
		}
		return f.prepareExec(rest,
			func(id string) error { return fmt.Errorf("Error response from daemon: No such container: %s", id) },
			func(id string) error {
				return fmt.Errorf("Error response from daemon: container %s is not running", id)
			})
	}
	return usagef("unknown command %q", cmd)
}

func (f *fake) dockerDown() error {
	if down() {
		if f.dialect == "podman" {
			return fmt.Errorf("Cannot connect to Podman. Please verify your connection to the Linux system using `podman system connection list`, or try `podman machine init` and `podman machine start` to manage a new Linux VM")
		}
		return fmt.Errorf("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	}
	return nil
}

// dockerOS is `info`'s OperatingSystem: OrbStack unless ZOO_FAKE_DOCKER_OS
// says otherwise (e.g. "Ubuntu 24.04.1 LTS" for Docker Engine on Linux).
func dockerOS() string {
	if v := os.Getenv("ZOO_FAKE_DOCKER_OS"); v != "" {
		return v
	}
	return "OrbStack"
}

func dockerRuntime(def string) string {
	if r := os.Getenv("ZOO_FAKE_DOCKER_RUNTIME"); r != "" {
		return r
	}
	return def
}

func (f *fake) dockerInfo(args []string) error {
	p, pos, err := parse("info", defs("-f,--format="), args, false)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("info takes no arguments")
	}
	if err := f.dockerDown(); err != nil {
		return err
	}
	format := p.last("format")
	osName := dockerOS()
	switch format {
	case "", jsonFormat:
	case "{{.OperatingSystem}}":
		if f.dialect == "podman" {
			return usagef("podman info has no .OperatingSystem (template: can't evaluate field OperatingSystem)")
		}
		fmt.Fprintln(f.out, osName)
		return nil
	default:
		return usagef("fake only supports info --format '{{json .}}' and '{{.OperatingSystem}}'")
	}
	var v any
	if f.dialect == "podman" {
		v = map[string]any{
			"host":    map[string]any{"arch": "arm64", "os": "linux", "ociRuntime": map[string]any{"name": dockerRuntime("crun")}},
			"version": map[string]any{"Version": "5.2.0", "APIVersion": "5.2.0"},
		}
	} else {
		v = map[string]any{"ServerVersion": "29.8.2", "OperatingSystem": osName, "DefaultRuntime": dockerRuntime("runc"),
			"Runtimes": map[string]any{"runc": map[string]any{"path": "runc"}}}
	}
	if format == "" {
		fmt.Fprintf(f.out, "Server:\n Server Version: 29.8.2\n Default Runtime: %s\n Operating System: %s\n", dockerRuntime("runc"), osName)
		return nil
	}
	return writeJSON(f.out, v)
}

func (f *fake) dockerImage(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	if len(args) == 0 || args[0] != "inspect" {
		return usagef("fake only supports `image inspect`")
	}
	p, pos, err := parse("image inspect", defs("-f,--format="), args[1:], false)
	if err != nil {
		return err
	}
	if p.has("format") {
		return usagef("fake doesn't support image inspect --format")
	}
	if len(pos) == 0 {
		return usagef("image inspect needs a reference")
	}
	arr := []any{}
	var missing []string
	for _, ref := range pos {
		if !containsStr(f.st.Images, f.normRef(ref)) {
			missing = append(missing, ref)
			continue
		}
		arr = append(arr, map[string]any{"Id": "sha256:" + hashID(f.normRef(ref)), "RepoTags": []string{f.normRef(ref)}})
	}
	if err := dockerJSON(f, arr); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("Error response from daemon: No such image: %s", missing[0])
	}
	return nil
}

func dockerJSON(f *fake, v any) error {
	b, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f.out, "%s\n", b)
	return err
}

func (f *fake) dockerNetwork(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	if len(args) == 0 {
		return usagef("network: missing subcommand")
	}
	switch args[0] {
	case "inspect":
		p, pos, err := parse("network inspect", defs("-f,--format="), args[1:], false)
		if err != nil {
			return err
		}
		format := p.last("format")
		switch format {
		case "", jsonFormat, gatewayFormat, "{{.Name}}", "{{.Internal}}":
		default:
			return usagef("unsupported network inspect --format %q", format)
		}
		if len(pos) == 0 {
			return usagef("network inspect needs a name")
		}
		arr := []any{}
		var missing []string
		for _, name := range pos {
			n := f.st.Net(name)
			if n == nil {
				missing = append(missing, name)
				continue
			}
			v := f.dockerNetJSON(n)
			switch format {
			case "":
				arr = append(arr, v)
			case jsonFormat:
				if err := writeJSON(f.out, v); err != nil {
					return err
				}
			case gatewayFormat:
				fmt.Fprintf(f.out, "192.168.%d.1 \n", n.Subnet)
			case "{{.Name}}":
				fmt.Fprintln(f.out, n.Name)
			case "{{.Internal}}":
				fmt.Fprintln(f.out, n.Internal)
			}
		}
		if format == "" {
			if err := dockerJSON(f, arr); err != nil {
				return err
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("Error response from daemon: network %s not found", missing[0])
		}
		return nil
	case "create":
		p, pos, err := parse("network create", defs("--internal", "--label=", "-d,--driver=", "--subnet="), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("network create needs exactly one name")
		}
		if d := p.last("driver"); d != "" && d != "bridge" {
			return usagef("fake only supports the bridge driver")
		}
		if f.st.Net(pos[0]) != nil {
			return fmt.Errorf("Error response from daemon: network with name %s already exists", pos[0])
		}
		labels, err := parseLabels(p["label"])
		if err != nil {
			return err
		}
		n := f.newNetwork(pos[0], p.has("internal"), labels)
		fmt.Fprintln(f.out, hashID("net/"+n.Name))
		return nil
	case "connect":
		_, pos, err := parse("network connect", defs(), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef("network connect needs NETWORK CONTAINER")
		}
		if f.st.Net(pos[0]) == nil {
			return fmt.Errorf("Error response from daemon: network %s not found", pos[0])
		}
		c := f.st.Find(pos[1])
		if c == nil {
			return fmt.Errorf("Error response from daemon: No such container: %s", pos[1])
		}
		if containsStr(c.Networks, pos[0]) {
			return fmt.Errorf("Error response from daemon: endpoint with name %s already exists in network %s", c.Name, pos[0])
		}
		c.Networks = append(c.Networks, pos[0])
		if c.Running {
			ip := f.nextIP(pos[0])
			c.IPs[pos[0]], c.LastIPs[pos[0]] = ip, ip
		}
		return nil
	case "disconnect":
		p, pos, err := parse("network disconnect", defs("-f,--force"), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef("network disconnect needs NETWORK CONTAINER")
		}
		if f.st.Net(pos[0]) == nil {
			return fmt.Errorf("Error response from daemon: network %s not found", pos[0])
		}
		c := f.st.Find(pos[1])
		if c == nil {
			if p.has("force") {
				return nil
			}
			return fmt.Errorf("Error response from daemon: No such container: %s", pos[1])
		}
		if !containsStr(c.Networks, pos[0]) {
			return fmt.Errorf("Error response from daemon: container %s is not connected to network %s", shortID(c), pos[0])
		}
		out := c.Networks[:0]
		for _, n := range c.Networks {
			if n != pos[0] {
				out = append(out, n)
			}
		}
		c.Networks = out
		delete(c.IPs, pos[0])
		delete(c.LastIPs, pos[0])
		return nil
	case "ls", "list":
		p, pos, err := parse("network ls", defs("-q,--quiet", "-f,--filter=", "--format=", "--no-trunc"), args[1:], false)
		if err != nil {
			return err
		}
		if len(pos) > 0 {
			return usagef("network ls takes no arguments")
		}
		format := p.last("format")
		switch format {
		case "", "{{.Name}}", "{{.ID}}", jsonFormat:
		default:
			return usagef("unsupported network ls --format %q", format)
		}
		if format == "" && !p.has("quiet") {
			fmt.Fprintln(f.out, "NETWORK ID     NAME      DRIVER    SCOPE")
		}
		for _, n := range f.st.Networks {
			ok := true
			for _, flt := range p["filter"] {
				k, v, _ := strings.Cut(flt, "=")
				switch k {
				case "label":
					lk, lv, hasV := strings.Cut(v, "=")
					got, has := n.Labels[lk]
					ok = ok && has && (!hasV || got == lv)
				case "name":
					ok = ok && strings.Contains(n.Name, v)
				default:
					return usagef("fake doesn't support network filter %q", k)
				}
			}
			if !ok {
				continue
			}
			id := hashID("net/" + n.Name)[:12]
			switch {
			case format == "{{.Name}}":
				fmt.Fprintln(f.out, n.Name)
			case format == jsonFormat:
				if err := writeJSON(f.out, map[string]any{"ID": id, "Name": n.Name, "Driver": "bridge", "Scope": "local", "Internal": strconv.FormatBool(n.Internal)}); err != nil {
					return err
				}
			case format == "{{.ID}}" || p.has("quiet"):
				// --format wins over -q, like docker.
				fmt.Fprintln(f.out, id)
			default:
				fmt.Fprintf(f.out, "%s   %s   bridge   local\n", id, n.Name)
			}
		}
		return nil
	case "rm":
		_, pos, err := parse("network rm", defs("-f,--force"), args[1:], false)
		if err != nil {
			return err
		}
		for _, name := range pos {
			if f.st.Net(name) == nil || name == "bridge" {
				return fmt.Errorf("Error response from daemon: network %s not found", name)
			}
			for _, c := range f.st.Containers {
				if c.Running && containsStr(c.Networks, name) {
					return fmt.Errorf("Error response from daemon: error while removing network: network %s has active endpoints (name:%q id:%q)", name, c.Name, shortID(c))
				}
			}
			out := f.st.Networks[:0]
			for _, n := range f.st.Networks {
				if n.Name != name {
					out = append(out, n)
				}
			}
			f.st.Networks = out
			fmt.Fprintln(f.out, name)
		}
		return nil
	}
	return usagef("unknown command \"network %s\"", args[0])
}

func (f *fake) dockerRun(cmd string, args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse(cmd, defs("-d,--detach", "--name=", "--net,--network=", "--dns=", "--cpus=", "-m,--memory=",
		"--pids-limit=", "--cap-drop=", "--cap-add=", "--security-opt=", "--restart=", "--runtime=", "-l,--label=",
		"-e,--env=", "--env-file=", "--mount=", "-v,--volume=", "--read-only", "--tmpfs=", "-u,--user=", "-w,--workdir=",
		"--rm", "--init", "--entrypoint=", "--hostname=", "--add-host="), args, true)
	if err != nil {
		return err
	}
	if cmd == "run" && !p.has("detach") {
		return usagef("fake only supports run -d")
	}
	if len(pos) == 0 {
		return usagef("%s needs an image", cmd)
	}
	if len(p["network"]) > 1 {
		// docker 29 accepts several --network flags only on API >= 1.44 with
		// endpoint options; zoo uses `network connect` instead. Be strict.
		return usagef("fake accepts one --network; use `network connect` for more")
	}
	name := p.last("name")
	id := f.newID(name)
	if name == "" {
		name = "fake_" + id[:6]
	}
	if f.st.Find(name) != nil {
		return fmt.Errorf("docker: Error response from daemon: Conflict. The container name \"/%s\" is already in use", name)
	}
	c := &fakestate.Container{ID: id, Name: name, Image: pos[0], Cmd: pos[1:], Args: append([]string{cmd}, args...),
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
			return fmt.Errorf("docker: %v", err)
		}
		c.Env = append(c.Env, vals...)
	}
	net := p.last("network")
	if net == "" {
		net = "bridge"
	}
	if f.st.Net(net) == nil {
		return fmt.Errorf("docker: Error response from daemon: network %s not found", net)
	}
	c.Networks = []string{net}
	for _, d := range p["dns"] {
		if strings.Count(d, ".") != 3 && !strings.Contains(d, ":") {
			return fmt.Errorf("invalid argument %q for \"--dns\" flag: %s is not an ip address", d, d)
		}
	}
	c.DNS = p["dns"]
	if v := p.last("cpus"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			return usagef("invalid argument %q for \"--cpus\" flag", v)
		}
		c.CPUs = n
	}
	if v := p.last("memory"); v != "" {
		if c.Memory, err = parseMem(v); err != nil {
			return usagef("invalid argument %q for \"-m, --memory\" flag", v)
		}
	}
	if v := p.last("pids-limit"); v != "" {
		if c.PidsLimit, err = strconv.Atoi(v); err != nil {
			return usagef("invalid argument %q for \"--pids-limit\" flag", v)
		}
	}
	c.CapDrop, c.CapAdd, c.SecurityOpt = p["cap-drop"], p["cap-add"], p["security-opt"]
	for _, so := range c.SecurityOpt {
		if so != "no-new-privileges" && !strings.Contains(so, "=") && !strings.Contains(so, ":") {
			return fmt.Errorf("docker: Error response from daemon: invalid --security-opt: %q", so)
		}
	}
	if r := p.last("restart"); r != "" {
		base, _, _ := strings.Cut(r, ":")
		if !containsStr([]string{"no", "always", "unless-stopped", "on-failure"}, base) {
			return fmt.Errorf("docker: invalid restart policy %q", r)
		}
		c.Restart = r
	}
	for _, h := range p["add-host"] {
		host, ip, ok := strings.Cut(h, ":")
		if !ok {
			host, ip, ok = strings.Cut(h, "=")
		}
		if !ok || host == "" || (ip != "host-gateway" && strings.Count(ip, ".") != 3 && !strings.Contains(ip, ":")) {
			return usagef("invalid argument %q for \"--add-host\" flag: bad format for add-host", h)
		}
		c.ExtraHosts = append(c.ExtraHosts, host+":"+ip)
	}
	c.Runtime = p.last("runtime")
	c.User, c.ReadOnlyFS, c.Tmpfs = p.last("user"), p.has("read-only"), p["tmpfs"]
	for _, v := range p["volume"] {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return usagef("invalid volume %q", v)
		}
		ro := len(parts) == 3 && parts[2] == "ro"
		c.Mounts = append(c.Mounts, fakestate.Mount{Source: parts[0], Target: parts[1], ReadOnly: ro, Via: "-v"})
	}
	for _, v := range p["mount"] {
		m, err := parseMountCSV(v)
		if err != nil {
			return err
		}
		if _, ok := pathExists(m.Source); !ok {
			return fmt.Errorf("docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist: %s", m.Source)
		}
		c.Mounts = append(c.Mounts, m)
	}
	f.st.Containers = append(f.st.Containers, c)
	if cmd == "run" {
		f.startC(c)
	}
	fmt.Fprintln(f.out, c.ID)
	return nil
}

func (f *fake) dockerStartStop(cmd string, args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	fs := defs("-a,--attach", "-i,--interactive")
	if cmd == "stop" {
		fs = defs("-t,--time,--timeout=", "-s,--signal=")
	}
	p, pos, err := parse(cmd, fs, args, false)
	if err != nil {
		return err
	}
	if p.has("attach") || p.has("interactive") {
		return usagef("fake doesn't support start -a/-i")
	}
	if v := p.last("timeout"); v != "" {
		if _, err := strconv.Atoi(v); err != nil {
			return usagef("invalid argument %q for \"-t, --timeout\" flag", v)
		}
	}
	if len(pos) == 0 {
		return usagef("%s needs at least one container", cmd)
	}
	var missing []string
	for _, ref := range pos {
		c := f.st.Find(ref)
		if c == nil {
			missing = append(missing, ref)
			continue
		}
		if cmd == "start" {
			f.startC(c)
		} else {
			f.stopC(c)
		}
		fmt.Fprintln(f.out, ref)
	}
	if len(missing) > 0 {
		return fmt.Errorf("Error response from daemon: No such container: %s", missing[0])
	}
	return nil
}

func (f *fake) dockerRm(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse("rm", defs("-f,--force", "-v,--volumes"), args, false)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("rm needs at least one container")
	}
	for _, ref := range pos {
		c := f.st.Find(ref)
		if c == nil {
			if p.has("force") {
				continue // docker rm -f ignores missing containers
			}
			return fmt.Errorf("Error response from daemon: No such container: %s", ref)
		}
		if c.Running && !p.has("force") {
			return fmt.Errorf("Error response from daemon: cannot remove container \"/%s\": container is running: stop the container before removing or force remove", c.Name)
		}
		f.removeC(c)
		fmt.Fprintln(f.out, ref)
	}
	return nil
}

func (f *fake) dockerPs(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse("ps", defs("-a,--all", "-q,--quiet", "--filter=", "--no-trunc"), args, false)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("ps takes no arguments")
	}
	match := func(c *fakestate.Container) (bool, error) {
		for _, flt := range p["filter"] {
			k, v, ok := strings.Cut(flt, "=")
			if !ok {
				return false, usagef("bad filter %q", flt)
			}
			switch k {
			case "name":
				if !strings.Contains(c.Name, v) {
					return false, nil
				}
			case "label":
				lk, lv, hasV := strings.Cut(v, "=")
				got, ok := c.Labels[lk]
				if !ok || (hasV && got != lv) {
					return false, nil
				}
			default:
				return false, usagef("fake doesn't support filter %q", k)
			}
		}
		return true, nil
	}
	if !p.has("quiet") {
		fmt.Fprintln(f.out, "CONTAINER ID   IMAGE   COMMAND   CREATED   STATUS   PORTS   NAMES")
	}
	// Newest first, like docker.
	for i := len(f.st.Containers) - 1; i >= 0; i-- {
		c := f.st.Containers[i]
		if !c.Running && !p.has("all") {
			continue
		}
		ok, err := match(c)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		id := shortID(c)
		if p.has("no-trunc") {
			id = c.ID
		}
		if p.has("quiet") {
			fmt.Fprintln(f.out, id)
		} else {
			fmt.Fprintf(f.out, "%s   %s   \"\"   now   %s   -   %s\n", id, c.Image, dockerStatus(c), c.Name)
		}
	}
	return nil
}

func dockerStatus(c *fakestate.Container) string {
	switch {
	case c.Running:
		return "running"
	case c.Started:
		return "exited"
	}
	return "created"
}

func (f *fake) dockerInspectJSON(c *fakestate.Container) map[string]any {
	nets := map[string]any{}
	for _, n := range c.Networks {
		gw := ""
		if net := f.st.Net(n); net != nil && c.Running {
			gw = fmt.Sprintf("192.168.%d.1", net.Subnet)
		}
		nets[n] = map[string]any{"IPAddress": c.IPs[n], "Gateway": gw, "IPPrefixLen": 24, "NetworkID": hashID("net/" + n)}
	}
	mounts := []any{}
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": m.Source, "Destination": m.Target,
			"Mode": "", "RW": !m.ReadOnly, "Propagation": "rprivate"})
	}
	labels := c.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	runtime := c.Runtime
	if runtime == "" {
		runtime = dockerRuntime("runc")
	}
	restart := c.Restart
	if restart == "" {
		restart = "no"
	}
	exit := 0
	if c.Started && !c.Running {
		exit = 137
	}
	return map[string]any{
		"Id":   c.ID,
		"Name": "/" + c.Name,
		"State": map[string]any{"Status": dockerStatus(c), "Running": c.Running, "Paused": false,
			"Pid": map[bool]int{true: 4242, false: 0}[c.Running], "ExitCode": exit},
		"Config": map[string]any{"Image": c.Image, "Labels": labels, "Env": c.Env, "Cmd": c.Cmd, "User": c.User},
		"HostConfig": map[string]any{
			"PidsLimit": c.PidsLimit, "NanoCpus": int64(math.Round(c.CPUs * 1e9)), "Memory": c.Memory,
			"CapDrop": c.CapDrop, "CapAdd": c.CapAdd, "SecurityOpt": c.SecurityOpt, "Dns": c.DNS,
			"Runtime": runtime, "RestartPolicy": map[string]any{"Name": restart}, "ReadonlyRootfs": c.ReadOnlyFS,
			"NetworkMode": firstNet(c), "ExtraHosts": c.ExtraHosts,
		},
		"Mounts":          mounts,
		"NetworkSettings": map[string]any{"Networks": nets},
	}
}

func (f *fake) dockerInspect(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse("inspect", defs("-f,--format=", "--type="), args, false)
	if err != nil {
		return err
	}
	if p.has("format") {
		return usagef("fake doesn't support inspect --format")
	}
	if t := p.last("type"); t != "" && t != "container" {
		return usagef("fake only inspects containers")
	}
	if len(pos) == 0 {
		return usagef("inspect needs at least one container")
	}
	arr := []any{}
	var missing []string
	for _, ref := range pos {
		c := f.st.Find(ref)
		if c == nil {
			missing = append(missing, ref)
			continue
		}
		arr = append(arr, f.dockerInspectJSON(c))
	}
	if err := dockerJSON(f, arr); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("Error response from daemon: No such container: %s", missing[0])
	}
	return nil
}

// bytesSize is docker's units.BytesSize (binary, 4 significant digits).
func bytesSize(n float64) string {
	u := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for n >= 1024 && i < len(u)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.4g%s", n, u[i])
}

// humanSize is docker's units.HumanSize (decimal, 3 significant digits).
func humanSize(n float64) string {
	u := []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for n >= 1000 && i < len(u)-1 {
		n /= 1000
		i++
	}
	return fmt.Sprintf("%.3g%s", n, u[i])
}

func (f *fake) dockerStats(args []string) error {
	if err := f.dockerDown(); err != nil {
		return err
	}
	p, pos, err := parse("stats", defs("--no-stream", "--format=", "-a,--all", "--no-trunc"), args, false)
	if err != nil {
		return err
	}
	if !p.has("no-stream") {
		return usagef("fake only supports stats --no-stream")
	}
	if p.last("format") != jsonFormat {
		return usagef("fake only supports stats --format '{{json .}}'")
	}
	for _, c := range f.st.Containers {
		if !c.Running || (len(pos) > 0 && !containsStr(pos, c.Name) && !containsStr(pos, c.ID)) {
			continue
		}
		limit := float64(c.Memory)
		if limit == 0 {
			limit = 8 << 30 // the VM's memory when unlimited
		}
		used := 2.148 * (1 << 20)
		row := map[string]string{
			"BlockIO":   humanSize(221000) + " / " + humanSize(0),
			"CPUPerc":   "0.50%",
			"Container": shortID(c),
			"ID":        shortID(c),
			"MemPerc":   fmt.Sprintf("%.2f%%", used/limit*100),
			"MemUsage":  bytesSize(used) + " / " + bytesSize(limit),
			"Name":      c.Name,
			"NetIO":     humanSize(16700) + " / " + humanSize(4280),
			"PIDs":      "2",
		}
		if err := writeJSON(f.out, row); err != nil {
			return err
		}
	}
	return nil
}

func shortID(c *fakestate.Container) string {
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}
