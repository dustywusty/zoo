// zoo: run AI coding agents in small VMs ("cages") whose only way out is a
// logging, allowlisting proxy ("the keeper"), with a live dashboard.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dusty/zoo/assets"
	"github.com/dusty/zoo/internal/backend"
	_ "github.com/dusty/zoo/internal/backend/apple"
	_ "github.com/dusty/zoo/internal/backend/docker"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/daemon"
	"github.com/dusty/zoo/internal/images"
	"github.com/dusty/zoo/internal/inject"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/keys"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/services"
	"github.com/dusty/zoo/internal/shims"
)

var version = "0.1.0-dev"

const usage = `zoo: agents in cages, one way out.

Usage:
  zoo up                         start the keeper, the daemon and the dashboard
  zoo down [--all]               stop the daemon and keeper (--all: and every cage)
  zoo run [harness] [flags] [-- args]
                                 run an agent (default: claude) in this worktree's cage
      --size S                   tiny | small | medium | large | cpus:memory (e.g. 3:6G)
  zoo shell [cage]               bash in a cage (default: this worktree's)
  zoo ssh setup                  write ~/.zoo/ssh/config (Host zoo-<cage>) for editors; prints the Include line
  zoo ssh [cage] [cmd...]        ssh into a cage as the agent (no listener: runs over the backend's exec)
  zoo ls                         list cages
  zoo stop <cage|all>            stop a cage (the worktree is untouched)
  zoo reset <cage>               delete a cage; next run starts from a clean image
  zoo allow [--for 4h] <host...> allow hosts for every cage (".example.com" = subdomains too);
                                 --for makes it lapse (30m, 4h, 2d)
  zoo deny <host...>             remove hosts from the allowlist
  zoo allowlist                  print the allowlist
  zoo blocked [cage]             hosts the keeper refused that aren't allowlisted (approve with zoo allow)
  zoo keys set|rm <name>         store a credential in the Keychain (e.g. "claude", "codex")
  zoo keys ls                    which harness credentials are stored
  zoo shims install|uninstall    put claude/codex/... shims on PATH so other tools land in cages
  zoo doctor [--deep] [--json] [cage]
                                 prove the walls hold from inside a cage; --deep also probes
                                 for reachable private addresses (other cages, LAN, metadata);
                                 --json prints one {verdict,check,detail} object per line
  zoo dash                       open the dashboard
  zoo ports [cage]               dev servers listening in a cage, with their preview URLs
  zoo open [cage] [port]         open a cage's dev server in the browser (http://<cage>.localhost:7779)
  zoo build [cage|keeper]        (re)build images
  zoo build --project [path]     review and approve this worktree's dockerfile: (zoo.yaml), then
                                 build its cage image; or rebuild the image: one
  zoo backend [list|use <name>]  show runtimes and their isolation; pick one (apple, docker, podman, auto)
  zoo status                     backend, keeper, daemon, isolation, shims
  zoo service ls                 services cages can be granted (config: services:)
  zoo grant [--for 4h] <cage> <service>
                                 let a cage reach a service (as <service>.zoo), for a while or for good
  zoo revoke <cage> <service>    take it back
  zoo trust [path] [--show]      approve this worktree's zoo.yaml (agents can edit it, so zoo
                                 only uses the copy you approved)
  zoo cache ls                   package caches kept across zoo reset, and their sizes
  zoo cache clear <cage|shared|all>
                                 delete a cache (not while a cage using it runs)
  zoo version

Global flags (any command):
  --backend NAME                 apple | docker | podman | auto (zoo up remembers it)
  --isolation-floor LEVEL        vm | microvm | sandboxed-container | container (zoo up remembers it)
  --allow-weaker-isolation       start this cage even if the backend is below the floor

Escape hatch: ZOO=off claude ...  runs the real CLI outside any cage.
`

func main() {
	argv, err := parseGlobal(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zoo:", err)
		os.Exit(2)
	}
	if len(argv) < 1 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := argv[0], argv[1:]
	switch cmd {
	case "up":
		err = cmdUp()
	case "down":
		err = cmdDown(args)
	case "run":
		err = cmdRun(args)
	case "shell":
		err = cmdShell(args)
	case "ssh":
		err = cmdSSH(args)
	case "ssh-proxy":
		err = cmdSSHProxy(args)
	case "ls", "list":
		err = cmdLs()
	case "stop":
		err = cmdStop(args)
	case "reset":
		err = cmdReset(args)
	case "allow":
		err = cmdAllow(args)
	case "deny":
		err = cmdDeny(args)
	case "allowlist":
		err = cmdAllowlist()
	case "blocked":
		err = cmdBlocked(args)
	case "keys":
		err = cmdKeys(args)
	case "shims":
		err = cmdShims(args)
	case "doctor":
		err = cmdDoctor(args)
	case "dash", "dashboard":
		err = cmdDash()
	case "ports":
		err = cmdPorts(args)
	case "open":
		err = cmdOpen(args)
	case "build":
		err = cmdBuild(args)
	case "daemon":
		err = cmdDaemon()
	case "backend", "backends":
		err = cmdBackend(args)
	case "status":
		err = cmdStatus()
	case "service", "services":
		err = cmdService(args)
	case "grant", "revoke":
		err = cmdGrant(cmd, args)
	case "trust":
		err = cmdTrust(args)
	case "cache", "caches":
		err = cmdCache(args)
	case "version", "--version", "-v":
		fmt.Println("zoo", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "zoo: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		fmt.Fprintln(os.Stderr, "zoo:", err)
		os.Exit(1)
	}
}

// --- setup -----------------------------------------------------------------------

func loadCfg() (config.Global, error) {
	if err := paths.Ensure(); err != nil {
		return config.Global{}, err
	}
	_ = config.WriteDefaultIfMissing()
	return config.LoadGlobal()
}

func say(format string, a ...any) { fmt.Fprintf(os.Stderr, "zoo: "+format+"\n", a...) }

// ensureSystem starts the runtime if needed and builds missing images.
func ensureSystem(ctx context.Context, b backend.Backend, g config.Global) error {
	info := b.Detect(ctx)
	if !info.Installed {
		return fmt.Errorf("%s backend: not installed: %s", b.Name(), info.Hint)
	}
	if !info.Running {
		say("starting %s…", b.Name())
		if err := b.Prepare(ctx); err != nil {
			return err
		}
	}
	for _, im := range []struct{ which, tag, note string }{
		{"keeper", g.KeeperImage, "first run only"},
		{"cage", g.CageImage, "first run only, a few minutes"},
	} {
		ok, err := b.ImageExists(ctx, im.tag)
		if err != nil {
			return err
		}
		if !ok {
			say("building the %s image on %s (%s)…", im.which, b.Name(), im.note)
			if err := buildImage(ctx, b, im.which, im.tag); err != nil {
				return err
			}
		}
	}
	return nil
}

func buildImage(ctx context.Context, b backend.Backend, which, tag string) error {
	dir, err := assets.WriteImage(which, paths.Build())
	if err != nil {
		return err
	}
	args := map[string]string{}
	if which == "cage" {
		args["AGENT_UID"] = strconv.Itoa(os.Getuid())
		args["AGENT_GID"] = strconv.Itoa(os.Getgid())
	}
	return b.BuildImage(ctx, backend.ImageSpec{Tag: tag, Context: dir, BuildArgs: args})
}

// ensureUp brings up the keeper and the daemon. Returns the keeper IP.
func ensureUp(ctx context.Context, e env) (string, error) {
	if err := ensureSystem(ctx, e.b, e.g); err != nil {
		return "", err
	}
	ip, err := keeper.Up(ctx, e.b, e.g)
	if err != nil {
		return "", err
	}
	if !daemonAlive(e.g) {
		if err := startDaemon(e.g); err != nil {
			say("dashboard didn't start: %v (see %s)", err, paths.DaemonLog())
		}
	}
	return ip, nil
}

func cmdUp() error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	// Remember an explicit backend / floor so later commands and the daemon
	// agree; a plain `zoo up` drops a remembered floor override.
	old := keeper.LoadState()
	st := old
	if flagBackend != "" {
		st.Backend = flagBackend
		if flagBackend == "auto" {
			st.Backend = ""
		}
	}
	st.IsolationFloor = flagFloor
	if st.Backend != old.Backend || st.IsolationFloor != old.IsolationFloor {
		if daemonAlive(g) {
			stopDaemon() // restart it on the new backend
			time.Sleep(300 * time.Millisecond)
		}
		if err := keeper.SaveState(st); err != nil {
			return err
		}
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	ip, err := ensureUp(ctx, e)
	if err != nil {
		return err
	}
	say("backend %s (%s isolation%s)", e.b.Name(), e.info.Caps.Isolation, detailSuffix(e.info))
	if !e.info.Caps.Isolation.AtLeast(e.floor) {
		say("warning: %s gives %s isolation, below your floor (%s); cages won't start without --allow-weaker-isolation or `isolation_floor: %s`", e.b.Name(), e.info.Caps.Isolation, e.floor, e.info.Caps.Isolation)
	}
	say("keeper on %s", keeper.ProxyURL(ip))
	say("dashboard on http://127.0.0.1:%d", g.Port)
	if !shims.OnPath() {
		say("tip: `zoo shims install` makes Conductor, herdr and your terminal use cages automatically")
	}
	return nil
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	all := fs.Bool("all", false, "also stop every cage")
	_ = fs.Parse(args)
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	if *all {
		cs, _ := cage.List(ctx, e.b)
		for _, c := range cs {
			if c.Running {
				say("stopping %s", c.Name)
				_ = e.b.Stop(ctx, c.ID)
			}
		}
	}
	stopDaemon()
	if err := keeper.Down(ctx, e.b); err != nil {
		say("keeper: %v", err)
	}
	say("down")
	return nil
}

// --- daemon process ----------------------------------------------------------------

func daemonAlive(g config.Global) bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/state", g.Port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func startDaemon(g config.Global) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(paths.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = os.WriteFile(paths.DaemonPID(), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Process.Release()
	for i := 0; i < 30; i++ {
		if daemonAlive(g) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("daemon not answering")
}

func stopDaemon() {
	b, err := os.ReadFile(paths.DaemonPID())
	if err != nil {
		return
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(paths.DaemonPID())
}

func cmdDaemon() error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	e, err := resolve(context.Background(), g)
	if err != nil {
		return err
	}
	s, err := daemon.New(g, e.b, e.floor)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func cmdDash() error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	if !daemonAlive(g) {
		if err := startDaemon(g); err != nil {
			return err
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", g.Port)
	opener := "open"
	if runtime.GOOS != "darwin" {
		opener = "xdg-open"
	}
	if err := exec.Command(opener, url).Run(); err != nil {
		fmt.Println(url)
	}
	return nil
}

// --- run / shell ------------------------------------------------------------------

type runOpts struct {
	harness string
	size    string
	rest    []string
}

func parseRun(args []string) (runOpts, error) {
	var o runOpts
	var pre []string
	for i, a := range args {
		if a == "--" {
			o.rest = args[i+1:]
			break
		}
		pre = append(pre, a)
	}
	if len(pre) > 0 && !strings.HasPrefix(pre[0], "-") {
		o.harness, pre = pre[0], pre[1:]
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&o.size, "size", "", "cage size")
	if err := fs.Parse(pre); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		if o.harness == "" {
			o.harness = fs.Arg(0)
			o.rest = append(fs.Args()[1:], o.rest...)
		} else {
			o.rest = append(fs.Args(), o.rest...)
		}
	}
	return o, nil
}

func cmdRun(args []string) error {
	o, err := parseRun(args)
	if err != nil {
		return err
	}
	if o.harness == "" {
		o.harness = "claude"
	}
	return runIn(o, "")
}

func cmdShell(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	return runIn(runOpts{harness: "shell"}, name)
}

// prepare brings everything up and returns the cage plan for the current dir
// (or the named cage) plus the keeper IP.
func prepare(ctx context.Context, e env, cageName, size string) (cage.Plan, string, error) {
	if !e.info.Caps.Isolation.AtLeast(e.floor) && !flagAllowWeaker {
		return cage.Plan{}, "", weakerErr(e)
	}
	ip, err := ensureUp(ctx, e)
	if err != nil {
		return cage.Plan{}, "", err
	}
	root := ""
	if cageName != "" {
		c, err := cage.Find(ctx, e.b, cageName)
		if err != nil {
			return cage.Plan{}, "", err
		}
		root = c.Worktree
		if size == "" {
			size = c.Size
		}
	} else {
		wd, _ := os.Getwd()
		if root, err = cage.Root(wd); err != nil {
			return cage.Plan{}, "", err
		}
		if home, _ := os.UserHomeDir(); root == home || root == "/" {
			return cage.Plan{}, "", fmt.Errorf("refusing to cage %s: run zoo from a project directory", root)
		}
	}
	ensureTrust(root)
	expireTemporary(ctx, e)
	proj, err := config.LoadProject(root)
	if err != nil {
		return cage.Plan{}, "", err
	}
	pl, err := cage.NewPlan(root, e.g, proj, size)
	if err != nil {
		return cage.Plan{}, "", err
	}
	if pl.Image, err = cageImage(ctx, e, root, proj); err != nil {
		return cage.Plan{}, "", err
	}
	if existing, ok, _ := e.b.Get(ctx, pl.ID); ok && size != "" && existing.Labels[cage.LabelSize] != pl.SizeName {
		say("resizing %s from %s to %s (restarts the cage; the worktree is untouched)", pl.Name, existing.Labels[cage.LabelSize], pl.SizeName)
		_ = e.b.Delete(ctx, pl.ID)
	}
	labels := map[string]string{
		cage.LabelBackend: e.b.Name(),
		cage.LabelIso:     string(e.info.Caps.Isolation),
	}
	if !e.info.Caps.Isolation.AtLeast(e.floor) {
		labels[cage.LabelWeaker] = "1"
		say("WARNING: %s runs with %s isolation, below your floor (%s), because of --allow-weaker-isolation", pl.Name, e.info.Caps.Isolation, e.floor)
	}
	spec := pl.Spec(e.g, ip, labels)
	st, exists, _ := e.b.Get(ctx, pl.ID)
	if exists && st.Running {
		switch cage.Changed(st, spec) {
		case "image":
			if strings.HasPrefix(spec.Image, images.Repo+":") || strings.HasPrefix(st.Labels[cage.LabelImage], images.Repo+":") {
				say("note: %s's image changed (image: or dockerfile:); it applies after `zoo reset %s`", pl.Name, pl.Name)
				break
			}
			say("WARNING: %s was created by an older zoo; its secret masks and git guard may not hold. Stop the agents in it, then `zoo reset %s`", pl.Name, pl.Name)
		case "layout":
			say("note: %s's mounts or git settings changed (zoo.yaml or git config); they apply after `zoo reset %s`", pl.Name, pl.Name)
		}
	}
	if !exists || (!st.Running && cage.Stale(st, spec)) {
		// A new cage gets its own network where the backend supports it.
		network, kip, err := e.b.CageNetwork(ctx, pl.ID)
		if err != nil {
			return pl, ip, fmt.Errorf("cage network: %w", err)
		}
		ip = kip
		spec = pl.Spec(e.g, ip, labels)
		spec.Network = network
	}
	created, err := cage.Ensure(ctx, e.b, spec)
	if err != nil {
		return pl, ip, err
	}
	if kip := keeperIPFor(ctx, e, pl.ID); kip != "" {
		ip = kip
	}
	if created {
		say("cage %s up on %s (%s: %d CPU, %s, %s isolation) for %s", pl.Name, e.b.Name(), pl.SizeName, pl.Size.CPUs, pl.Size.Memory, e.info.Caps.Isolation, pl.Root)
		for _, m := range pl.Extra {
			say("  also mounted: %s", m)
		}
		if len(pl.Masks) > 0 {
			say("hiding %d secret file(s) inside the cage", len(pl.Masks))
		}
		sayCache(pl)
		for _, r := range pl.Roots() {
			warnGitRisks(r)
		}
	}
	active := syncKeeper(ctx, e)
	if err := applyServices(ctx, e, pl.Name, pl.ID, ip); err != nil {
		say("services: %v", err)
	}
	if cs, err := cage.List(ctx, e.b); err == nil {
		grants := services.GrantsFor(e.g, cs)[pl.Name]
		if err := inject.ApplyToCage(ctx, e.b, e.g, active, pl.ID, pl.Name, grants); err != nil {
			say("credentials: %v", err)
		}
	}
	return pl, ip, nil
}

// expireTemporary drops allowlist entries and grants whose time is up, for
// when the daemon (which normally does it) isn't running.
func expireTemporary(ctx context.Context, e env) {
	if removed, err := keeper.Expire(time.Now()); err == nil && len(removed) > 0 {
		say("temporary allow expired: %s", strings.Join(removed, " "))
		reloadIfUp(ctx, e)
	}
	if gone, err := services.PruneExpired(time.Now()); err == nil {
		for _, g := range gone {
			say("temporary grant expired: %s from %s", g[1], g[0])
		}
	}
}

// keeperIPFor is the keeper's address on a network the cage is on.
func keeperIPFor(ctx context.Context, e env, cageID string) string {
	c, ok, _ := e.b.Get(ctx, cageID)
	k, kok, _ := e.b.Get(ctx, keeper.Name)
	if !ok || !kok {
		return ""
	}
	for n := range c.IPs {
		if ip := k.IPs[n]; ip != "" {
			return ip
		}
	}
	return ""
}

// applyServices writes a cage's granted service names into it.
func applyServices(ctx context.Context, e env, name, id, keeperIP string) error {
	cs, err := cage.List(ctx, e.b)
	if err != nil {
		return err
	}
	grants := services.GrantsFor(e.g, cs)
	return services.Apply(ctx, e.b, e.g, id, keeperIP, grants[name])
}

func weakerErr(e env) error {
	return fmt.Errorf(`%s gives %s isolation (%s), below your isolation floor (%s).
Cages on it share a kernel with each other%s.
To accept that: add "isolation_floor: %s" to %s,
or pass --allow-weaker-isolation for one run, or use a stronger backend (zoo backend)`,
		e.b.Name(), e.info.Caps.Isolation, strings.TrimPrefix(detailSuffix(e.info), ", "), e.floor,
		map[bool]string{true: "", false: " and the runtime's VM"}[e.info.Caps.Isolation != backend.IsoContainer],
		e.info.Caps.Isolation, paths.Config())
}

// syncKeeper re-renders the gate's grants from the cages' current IPs,
// starts granted sidecars, and updates which hosts the keeper intercepts for
// credentials. Returns the active credentials.
func syncKeeper(ctx context.Context, e env) map[string]config.Credential {
	cs, err := cage.List(ctx, e.b)
	if err != nil {
		return nil
	}
	grants, err := services.Sync(ctx, e.b, e.g, cs)
	if err != nil {
		say("services: %v", err)
	}
	active, reload, err := inject.Activate(e.g, grants)
	if err != nil {
		say("credentials: %v", err)
	}
	if reload {
		reloadIfUp(ctx, e)
	}
	return active
}

func runIn(o runOpts, cageName string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	h, ok := g.Harnesses[o.harness]
	if !ok {
		names := make([]string, 0, len(g.Harnesses))
		for k := range g.Harnesses {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown harness %q (have: %s; add more under harnesses: in %s)", o.harness, strings.Join(names, ", "), paths.Config())
	}
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	pl, ip, err := prepare(ctx, e, cageName, o.size)
	if err != nil {
		return err
	}
	if len(h.Allow) > 0 {
		if added, _ := keeper.Allow(h.Allow...); len(added) > 0 {
			_ = keeper.Reload(ctx, e.b)
		}
	}
	env := append([]string{
		"HOME=/home/agent", "USER=agent", "ZOO_CAGE=" + pl.Name, "ZOO_INSIDE=1",
		"TERM=" + envOr("TERM", "xterm-256color"), "COLORTERM=" + os.Getenv("COLORTERM"), "LANG=" + envOr("LANG", "en_US.UTF-8"),
	}, cage.ProxyEnv(ip)...)
	if cs, err := cage.List(ctx, e.b); err == nil {
		grants := services.GrantsFor(e.g, cs)
		env = append(env, services.SessionEnv(e.g, grants[pl.Name])...)
		active, _, _ := inject.Activate(e.g, grants)
		credEnv, err := inject.Env(e.g, active, pl.Name, grants[pl.Name], h.Env)
		if err != nil {
			return err
		}
		env = append(env, credEnv...)
	}
	if st, ok, _ := e.b.Get(ctx, pl.ID); ok {
		env = append(env, cage.CacheEnv(st.Labels)...)
		if img := st.Labels[cage.LabelImage]; img != e.g.CageImage {
			if err := checkHarness(ctx, e.b, pl.ID, img, o.harness, h.Command); err != nil {
				return err
			}
		}
	}
	var missing []string
	for envName, item := range h.Env {
		if strings.HasPrefix(item, "cred:") {
			continue // a placeholder; the keeper holds the value
		}
		// Keychain item, or a vault reference (bw:…, op://…) fetched on this
		// machine; the vault session itself never enters the cage.
		v, err := keys.Resolve(item)
		if err != nil {
			say("%s: %v", item, err)
		}
		if v == "" {
			missing = append(missing, item)
			continue
		}
		env = append(env, envName+"="+v)
	}
	for rel, item := range h.Files {
		v, err := keys.Resolve(item)
		if err != nil {
			say("%s: %v", item, err)
		}
		if v == "" {
			continue
		}
		if err := writeGuestFile(ctx, e.b, pl.ID, "/home/agent/"+rel, v); err != nil {
			say("couldn't place %s in the cage: %v", rel, err)
		}
	}
	if len(missing) > 0 {
		hint := ""
		if o.harness == "claude" {
			hint = " (run `claude setup-token` on your Mac, then `zoo keys set claude` and paste it)"
		}
		if o.harness == "codex" {
			hint = " (`zoo keys set codex` for an API key, or `zoo keys set codex-auth < ~/.codex/auth.json` for a ChatGPT login)"
		}
		if len(h.Files) == 0 || len(missing) == len(h.Env) && !anyKey(h.Files) {
			say("no credential stored for %s%s; you'll need to log in inside the cage", strings.Join(missing, ", "), hint)
		}
	}

	envFile, cleanup, err := writeEnvFile(env)
	if err != nil {
		return err
	}
	defer cleanup()

	wd, _ := os.Getwd()
	workdir := pl.Root
	if cageName == "" && (wd == pl.Root || strings.HasPrefix(wd, pl.Root+string(filepath.Separator))) {
		workdir = wd
	}

	spec := backend.ExecSpec{
		ID: pl.ID, Cmd: append([]string{"/usr/local/bin/zoo-session", h.Command}, o.rest...),
		User: "agent", Workdir: workdir, EnvFile: envFile,
		TTY: isTerminal(os.Stdin) && isTerminal(os.Stdout),
	}

	sess := registerSession(g, pl.Name, o.harness)
	defer endSession(g, sess)
	// Remove the env file once the CLI has read it.
	go func() { time.Sleep(3 * time.Second); cleanup() }()
	code, err := e.b.Exec(ctx, spec)
	for _, r := range pl.Roots() {
		warnGitTraps(r)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// warnGitTraps tells the user, loudly, about repositories planted in a
// worktree (see cage.GitTraps) before they run git there.
func warnGitTraps(root string) {
	traps, complete := cage.GitTraps(root)
	if !complete {
		say("couldn't finish scanning %s for planted git repositories (too many files)", root)
	}
	if len(traps) == 0 {
		return
	}
	say("WARNING: found %d planted git repository(s) in %s.", len(traps), root)
	say("Running git there (or opening it in an editor, or a shell with a git prompt) can run commands the agent chose. Inspect, then delete:")
	for _, t := range traps {
		fmt.Fprintf(os.Stderr, "    %s  (%s)\n", t.Path, t.Reason)
	}
}

// warnGitRisks lists hooks and config in the repository that run commands
// when you use git on this machine: things that were there before zoo (a
// downloaded repo can ship a hook), and paths in the worktree that zoo made
// read-only in the cage because git would run them.
func warnGitRisks(root string) {
	risks := cage.HostGitRisks(root)
	if len(risks) == 0 {
		return
	}
	say("this repository runs commands when you use git on this machine; check you trust them:")
	for _, r := range risks {
		line := fmt.Sprintf("    %s  (%s)", r.Path, r.Reason)
		if r.Writable != "" {
			line += "\n      lives in the worktree, so it's read-only inside the cage: " + r.Writable
		}
		fmt.Fprintln(os.Stderr, line)
	}
}

// exitCode passes an agent's exit status through to zoo's.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

func anyKey(files map[string]string) bool {
	for _, item := range files {
		if ok, _ := keys.Available(item); ok {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeEnvFile(env []string) (string, func(), error) {
	f, err := os.CreateTemp(paths.Tmp(), "env-*")
	if err != nil {
		return "", func() {}, err
	}
	_ = f.Chmod(0o600)
	for _, e := range env {
		if strings.ContainsAny(e, "\n\r") {
			continue
		}
		fmt.Fprintln(f, e)
	}
	f.Close()
	name := f.Name()
	return name, func() { _ = os.Remove(name) }, nil
}

func writeGuestFile(ctx context.Context, b backend.Backend, id, path, content string) error {
	var errb bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: id, User: "agent",
		Cmd:   []string{"sh", "-c", `umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path},
		Stdin: strings.NewReader(content), Stdout: io.Discard, Stderr: &errb,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func token() string {
	t, _ := daemon.Token()
	return t
}

func registerSession(g config.Global, cageName, harness string) string {
	body, _ := json.Marshal(map[string]any{"cage": cageName, "harness": harness, "host_pid": os.Getpid()})
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/sessions", g.Port), bytes.NewReader(body))
	req.Header.Set("X-Zoo-Token", token())
	req.Header.Set("Content-Type", "application/json")
	c := http.Client{Timeout: time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ID
}

func endSession(g config.Global, id string) {
	if id == "" {
		return
	}
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("http://127.0.0.1:%d/api/sessions/%s", g.Port, id), nil)
	req.Header.Set("X-Zoo-Token", token())
	c := http.Client{Timeout: time.Second}
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}

// --- cages -------------------------------------------------------------------------

func cmdLs() error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	cs, err := cage.List(ctx, e.b)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		fmt.Println("no cages yet. In a repo: zoo run claude")
		return nil
	}
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAGE\tSTATUS\tISOLATION\tSIZE\tIP\tWORKTREE")
	for _, c := range cs {
		st := c.Status
		iso := c.Isolation
		if iso == "" {
			iso = "?"
		}
		if c.Weaker {
			iso += " (below floor)"
		}
		wt := c.Worktree
		if home != "" && strings.HasPrefix(wt, home) {
			wt = "~" + strings.TrimPrefix(wt, home)
		}
		size := c.Size
		if c.CPUs != "" {
			size += fmt.Sprintf(" (%s CPU, %s)", c.CPUs, c.Memory)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, st, iso, size, c.IP, wt)
	}
	return tw.Flush()
}

func cmdStop(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: zoo stop <cage|all>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	defer syncKeeper(ctx, e)
	if args[0] == "all" {
		cs, err := cage.List(ctx, e.b)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Running {
				say("stopping %s", c.Name)
				if err := e.b.Stop(ctx, c.ID); err != nil {
					say("%v", err)
				}
			}
		}
		return nil
	}
	c, err := cage.Find(ctx, e.b, args[0])
	if err != nil {
		return err
	}
	return e.b.Stop(ctx, c.ID)
}

func cmdReset(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: zoo reset <cage>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	c, err := cage.Find(ctx, e.b, args[0])
	if err != nil {
		return err
	}
	if err := e.b.Delete(ctx, c.ID); err != nil {
		return err
	}
	syncKeeper(ctx, e)
	say("reset %s; the next `zoo run` there starts from a clean image", c.Name)
	noteCacheKept(c.Name)
	return nil
}

// --- allowlist / blocked ----------------------------------------------------------

func reloadIfUp(ctx context.Context, e env) {
	if c, ok, _ := e.b.Get(ctx, keeper.Name); ok && c.Running {
		if err := keeper.Reload(ctx, e.b); err != nil {
			say("keeper reload: %v", err)
		}
	}
}

// reloadKeeper reloads squid on the active backend, if it's up.
func reloadKeeper(g config.Global) {
	ctx := context.Background()
	if e, err := resolve(ctx, g); err == nil {
		reloadIfUp(ctx, e)
	}
}

func cmdAllow(args []string) error {
	args, forDur, err := takeFor(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: zoo allow [--for 4h] <host...>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	if err := keeper.EnsureFiles(); err != nil {
		return err
	}
	if forDur > 0 {
		until := time.Now().Add(forDur)
		touched, err := keeper.AllowFor(until, args...)
		if err != nil {
			return err
		}
		reloadKeeper(g)
		if len(touched) == 0 {
			say("already allowed for good")
			return nil
		}
		say("allowed %s until %s", strings.Join(touched, " "), config.Until(until))
		return nil
	}
	added, err := keeper.Allow(args...)
	if err != nil {
		return err
	}
	if err := keeper.MakePermanent(args...); err != nil {
		return err
	}
	if len(added) == 0 {
		say("already allowed (now for good, if it was temporary)")
		return nil
	}
	reloadKeeper(g)
	say("allowed: %s", strings.Join(added, " "))
	return nil
}

func cmdDeny(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoo deny <host...>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	removed, err := keeper.Deny(args...)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		say("none of those were on the allowlist (a wildcard entry may cover them; see zoo allowlist)")
		return nil
	}
	reloadKeeper(g)
	say("removed: %s", strings.Join(removed, " "))
	return nil
}

func cmdAllowlist() error {
	if _, err := loadCfg(); err != nil {
		return err
	}
	if err := keeper.EnsureFiles(); err != nil {
		return err
	}
	b, err := os.ReadFile(paths.Allowlist())
	if err != nil {
		return err
	}
	os.Stdout.Write(b)
	if exp := keeper.LoadExpiries(); len(exp) > 0 {
		hosts := make([]string, 0, len(exp))
		for h := range exp {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		fmt.Println("\n# temporary:")
		for _, h := range hosts {
			fmt.Printf("#   %s expires %s\n", h, config.Until(exp[h]))
		}
	}
	return nil
}

func currentOrNamed(ctx context.Context, e env, args []string) (cage.Info, error) {
	if len(args) > 0 {
		return cage.Find(ctx, e.b, args[0])
	}
	wd, _ := os.Getwd()
	return cage.Find(ctx, e.b, wd)
}

// cmdBlocked lists hosts the keeper refused that still aren't allowlisted:
// the approval queue for a new repo's first run.
func cmdBlocked(args []string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	var ip string
	if len(args) > 0 {
		ctx := context.Background()
		e, err := resolve(ctx, g)
		if err != nil {
			return err
		}
		c, err := cage.Find(ctx, e.b, args[0])
		if err != nil {
			return err
		}
		ip = c.IP
	}
	list, _ := keeper.ReadAllowlist()
	counts := map[string]int{}
	f, err := os.Open(paths.AccessLog())
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		e, ok := keeper.ParseLine(sc.Text())
		if !ok || !e.Denied || e.Host == "" {
			continue
		}
		if ip != "" && e.Client != ip {
			continue
		}
		if !keeper.Allowed(list, e.Host) {
			counts[e.Host]++
		}
	}
	if len(counts) == 0 {
		fmt.Println("nothing blocked that isn't allowlisted now")
		return nil
	}
	hosts := make([]string, 0, len(counts))
	for h := range counts {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return counts[hosts[i]] > counts[hosts[j]] })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REQUESTS\tHOST")
	for _, h := range hosts {
		fmt.Fprintf(tw, "%d\t%s\n", counts[h], h)
	}
	tw.Flush()
	fmt.Printf("\nkeep the ones you trust: zoo allow %s\n", strings.Join(hosts[:min(3, len(hosts))], " "))
	return nil
}

// --- keys / shims ----------------------------------------------------------------

func cmdKeys(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoo keys set|rm <name> | zoo keys ls")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		seen := map[string]bool{}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ITEM\tSOURCE\tAVAILABLE\tUSED BY")
		row := func(item, usedBy string) {
			src, name := keys.Describe(item)
			ok, hint := keys.Available(item)
			avail := "yes"
			if !ok {
				avail = "no: " + hint
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, src, avail, usedBy)
		}
		names := make([]string, 0, len(g.Harnesses))
		for n := range g.Harnesses {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			h := g.Harnesses[n]
			for envName, item := range h.Env {
				if !seen[item] {
					seen[item] = true
					row(item, fmt.Sprintf("%s ($%s)", n, envName))
				}
			}
			for rel, item := range h.Files {
				if !seen[item] {
					seen[item] = true
					row(item, fmt.Sprintf("%s (~/%s)", n, rel))
				}
			}
		}
		return tw.Flush()
	case "set":
		if len(args) != 2 {
			return errors.New("usage: zoo keys set <name>   (value from stdin if piped, otherwise prompted)")
		}
		if src, _ := keys.Describe(args[1]); src != "keychain" {
			return fmt.Errorf("%s lives in %s; zoo reads it from there and never stores it", args[1], src)
		}
		if !isTerminal(os.Stdin) {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			if err != nil {
				return err
			}
			v := strings.TrimRight(string(b), "\n")
			if v == "" {
				return errors.New("empty value on stdin")
			}
			if err := keys.Set(args[1], v); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "Value for %s (input hidden):\n", args[1])
			if err := keys.Set(args[1], ""); err != nil {
				return err
			}
		}
		say("stored %s", args[1])
		return nil
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: zoo keys rm <name>")
		}
		return keys.Remove(args[1])
	}
	return fmt.Errorf("unknown keys command %q", args[0])
}

func cmdShims(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoo shims install|uninstall")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		var hs []string
		for n := range g.Harnesses {
			hs = append(hs, n)
		}
		written, err := shims.Install(hs, self)
		if err != nil {
			return err
		}
		for _, w := range written {
			fmt.Println("  " + w)
		}
		if !shims.OnPath() {
			fmt.Printf("\nAdd this as the LAST line of ~/.zshrc (and ~/.zprofile, so GUI apps that read your login shell see it):\n\n  export PATH=\"%s:$PATH\"\n\nEscape hatch: ZOO=off claude …\n", paths.Shims())
		}
		return nil
	case "uninstall":
		removed, err := shims.Uninstall()
		for _, r := range removed {
			fmt.Println("removed " + r)
		}
		return err
	}
	return fmt.Errorf("unknown shims command %q", args[0])
}

// --- doctor -----------------------------------------------------------------------

func cmdDoctor(args []string) error {
	opts, err := parseDoctorArgs(args)
	if err != nil {
		return err
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	pl, ip, err := prepare(ctx, e, opts.cage, "")
	if err != nil {
		return err
	}
	env := []string{
		"ZOO_MASKS=" + strings.Join(pl.Masks, "\n"),
		"ZOO_READONLY=" + strings.Join(pl.ReadOnly, "\n"),
		"ZOO_PIN=" + strings.Join(pl.Pin, "\n"),
		"ZOO_GIT=" + pl.Git,
		"ZOO_INJECT_HOSTS=" + injectHosts(),
		"ZOO_KEEPER=" + ip,
		"ZOO_DASH_PORT=" + strconv.Itoa(g.Port),
		"ZOO_HOST_GATEWAY_EXPECTED=" + strconv.FormatBool(e.info.Caps.HostGatewayReachable),
		"ZOO_PER_CAGE_NETWORK=" + strconv.FormatBool(e.info.Caps.PerCageNetworks),
		"HOME=/home/agent",
	}
	if opts.deep {
		env = append(env, "ZOO_DEEP=1")
	}
	var outb bytes.Buffer
	code, runErr := e.b.Exec(ctx, backend.ExecSpec{
		ID: pl.ID, User: "agent", Workdir: pl.Root, NoStdin: true,
		Cmd:    []string{"/usr/local/bin/zoo-doctor"},
		Env:    append(env, cage.ProxyEnv(ip)...),
		Stdout: &outb, Stderr: &outb,
	})
	out := outb.String()
	if runErr == nil && code != 0 && out == "" {
		runErr = fmt.Errorf("zoo-doctor exited %d", code)
	}
	if runErr != nil && out == "" {
		return runErr
	}

	checks := parseDoctorOutput(out)
	// Host-side checks.
	if !e.info.Caps.Isolation.AtLeast(e.floor) {
		checks = append(checks, doctorCheck{"WARN", "isolation", fmt.Sprintf("%s is below your floor (%s)", e.info.Caps.Isolation, e.floor)})
	} else {
		checks = append(checks, doctorCheck{"PASS", "isolation", fmt.Sprintf("%s (floor %s)", e.info.Caps.Isolation, e.floor)})
	}
	if g.Port > 0 {
		checks = append(checks, doctorCheck{"PASS", "dashboard", fmt.Sprintf("bound to 127.0.0.1:%d only", g.Port)})
	}
	for _, root := range pl.Roots() {
		if traps, complete := cage.GitTraps(root); len(traps) > 0 {
			for _, t := range traps {
				checks = append(checks, doctorCheck{"FAIL", "git-traps", fmt.Sprintf("%s (%s); delete it before running git there", t.Path, t.Reason)})
			}
		} else if !complete {
			checks = append(checks, doctorCheck{"WARN", "git-traps", "scan stopped early in " + root + " (too many files)"})
		} else {
			checks = append(checks, doctorCheck{"PASS", "git-traps", "no planted repositories in " + root})
		}
	}
	var risks []cage.GitRisk
	for _, root := range pl.Roots() {
		risks = append(risks, cage.HostGitRisks(root)...)
	}
	if len(risks) > 0 {
		for _, r := range risks {
			msg := fmt.Sprintf("%s (%s)", r.Path, r.Reason)
			if r.Writable != "" {
				msg += "; in the worktree, read-only in the cage"
			}
			checks = append(checks, doctorCheck{"WARN", "git-runs", msg})
		}
	} else {
		checks = append(checks, doctorCheck{"PASS", "git-runs", "no hooks or command-running git config"})
	}
	if !shims.OnPath() {
		checks = append(checks, doctorCheck{"WARN", "shims", "not on PATH, so Conductor/herdr launch agents outside cages (zoo shims install)"})
	}

	fails := 0
	for _, c := range checks {
		if c.Verdict == "FAIL" {
			fails++
		}
	}
	if opts.json {
		if err := writeDoctorJSON(os.Stdout, checks); err != nil {
			return err
		}
	} else {
		fmt.Printf("zoo doctor · cage %s · %s\n", pl.Name, pl.Root)
		fmt.Printf("backend %s · isolation %s%s%s\n\n", e.b.Name(), e.info.Caps.Isolation, detailSuffix(e.info), map[bool]string{true: " · deep", false: ""}[opts.deep])
		writeDoctorText(os.Stdout, checks, isTerminal(os.Stdout))
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	if !opts.json {
		fmt.Println("\nall walls hold.")
	}
	return nil
}

// --- build ------------------------------------------------------------------------

func cmdBuild(args []string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	if !e.info.Ready() {
		return fmt.Errorf("%s backend isn't ready: %s", e.b.Name(), e.info.Hint)
	}
	if len(args) > 0 && args[0] == "--project" {
		return buildProject(ctx, e, args[1:])
	}
	which := []string{"keeper", "cage"}
	if len(args) > 0 {
		which = args
	}
	for _, w := range which {
		switch w {
		case "keeper":
			if err := buildImage(ctx, e.b, "keeper", g.KeeperImage); err != nil {
				return err
			}
		case "cage":
			if err := buildImage(ctx, e.b, "cage", g.CageImage); err != nil {
				return err
			}
			say("rebuilt the cage image; existing cages keep the old one until `zoo reset <cage>`")
		default:
			return fmt.Errorf("unknown image %q (cage or keeper)", w)
		}
	}
	return nil
}

// injectHosts lists the hosts the keeper intercepts for credentials.
func injectHosts() string {
	b, err := os.ReadFile(paths.InjectHosts())
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
