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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dusty/zoo/assets"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/ctr"
	"github.com/dusty/zoo/internal/daemon"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/keys"
	"github.com/dusty/zoo/internal/paths"
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
      --learn                    allow all egress for this cage, logged, until "zoo learn off"
  zoo shell [cage]               bash in a cage (default: this worktree's)
  zoo ls                         list cages
  zoo stop <cage|all>            stop a cage (the worktree is untouched)
  zoo reset <cage>               delete a cage; next run starts from a clean image
  zoo allow <host...>            allow hosts for every cage (".example.com" = subdomains too)
  zoo deny <host...>             remove hosts from the allowlist
  zoo allowlist                  print the allowlist
  zoo learn on|off [cage]        toggle learn mode for a cage
  zoo learned [cage]             hosts reached in learn mode that aren't on the allowlist
  zoo keys set|rm <name>         store a credential in the Keychain (e.g. "claude", "codex")
  zoo keys ls                    which harness credentials are stored
  zoo shims install|uninstall    put claude/codex/... shims on PATH so other tools land in cages
  zoo doctor [cage]              prove the walls hold from inside a cage
  zoo dash                       open the dashboard
  zoo build [cage|keeper]        (re)build images
  zoo version

Escape hatch: ZOO=off claude ...  runs the real CLI outside any cage.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "up":
		err = cmdUp()
	case "down":
		err = cmdDown(args)
	case "run":
		err = cmdRun(args)
	case "shell":
		err = cmdShell(args)
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
	case "learn":
		err = cmdLearn(args)
	case "learned":
		err = cmdLearned(args)
	case "keys":
		err = cmdKeys(args)
	case "shims":
		err = cmdShims(args)
	case "doctor":
		err = cmdDoctor(args)
	case "dash", "dashboard":
		err = cmdDash()
	case "build":
		err = cmdBuild(args)
	case "daemon":
		err = cmdDaemon()
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

// ensureSystem checks the container CLI and service, and builds missing images.
func ensureSystem(g config.Global) error {
	if !ctr.Installed() {
		return errors.New("Apple's `container` CLI isn't installed. Get it from https://github.com/apple/container/releases (macOS 26+, Apple silicon)")
	}
	if !ctr.SystemRunning() {
		say("starting the container service…")
		if err := ctr.SystemStart(); err != nil {
			return fmt.Errorf("container system start: %w", err)
		}
	}
	if !ctr.ImageExists(g.KeeperImage) {
		say("building the keeper image (first run only)…")
		if err := buildImage("keeper", g.KeeperImage); err != nil {
			return err
		}
	}
	if !ctr.ImageExists(g.CageImage) {
		say("building the cage image (first run only, a few minutes)…")
		if err := buildImage("cage", g.CageImage); err != nil {
			return err
		}
	}
	return nil
}

func buildImage(which, tag string) error {
	dir, err := assets.WriteImage(which, paths.Build())
	if err != nil {
		return err
	}
	args := map[string]string{}
	if which == "cage" {
		args["AGENT_UID"] = strconv.Itoa(os.Getuid())
		args["AGENT_GID"] = strconv.Itoa(os.Getgid())
	}
	return ctr.Build(tag, dir, args)
}

// ensureUp brings up the keeper and the daemon. Returns the keeper IP.
func ensureUp(g config.Global) (string, error) {
	if err := ensureSystem(g); err != nil {
		return "", err
	}
	ip, err := keeper.Up(g)
	if err != nil {
		return "", err
	}
	if !daemonAlive(g) {
		if err := startDaemon(g); err != nil {
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
	ip, err := ensureUp(g)
	if err != nil {
		return err
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
	if *all {
		cs, _ := cage.List(g)
		for _, c := range cs {
			if c.Running {
				say("stopping %s", c.Name)
				_ = ctr.Stop(c.ID)
			}
		}
	}
	stopDaemon()
	if err := keeper.Down(); err != nil {
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
	s, err := daemon.New(g)
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
	if err := exec.Command("open", url).Run(); err != nil {
		fmt.Println(url)
	}
	return nil
}

// --- run / shell ------------------------------------------------------------------

type runOpts struct {
	harness string
	size    string
	learn   bool
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
	fs.BoolVar(&o.learn, "learn", false, "learn mode")
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
func prepare(g config.Global, cageName, size string) (cage.Plan, string, error) {
	ip, err := ensureUp(g)
	if err != nil {
		return cage.Plan{}, "", err
	}
	root := ""
	if cageName != "" {
		c, err := cage.Find(g, cageName)
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
	proj, err := config.LoadProject(root)
	if err != nil {
		return cage.Plan{}, "", err
	}
	pl, err := cage.NewPlan(root, g, proj, size)
	if err != nil {
		return cage.Plan{}, "", err
	}
	if existing, ok := ctr.Get(pl.ID, g.Network); ok && size != "" && existing.Labels[cage.LabelSize] != pl.SizeName {
		say("resizing %s from %s to %s (restarts the cage; the worktree is untouched)", pl.Name, existing.Labels[cage.LabelSize], pl.SizeName)
		_ = cage.Reset(pl.ID)
	}
	created, err := cage.Ensure(pl, g, ip)
	if err != nil {
		return pl, ip, err
	}
	if created {
		say("cage %s up (%s: %d CPU, %s) for %s", pl.Name, pl.SizeName, pl.Size.CPUs, pl.Size.Memory, pl.Root)
		if len(pl.Masks) > 0 {
			say("hiding %d secret file(s) inside the cage", len(pl.Masks))
		}
	}
	return pl, ip, nil
}

func runIn(o runOpts, cageName string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	h, ok := g.Harnesses[o.harness]
	if !ok {
		names := make([]string, 0, len(g.Harnesses))
		for k := range g.Harnesses {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown harness %q (have: %s; add more under harnesses: in %s)", o.harness, strings.Join(names, ", "), paths.Config())
	}
	pl, ip, err := prepare(g, cageName, o.size)
	if err != nil {
		return err
	}
	if len(h.Allow) > 0 {
		if added, _ := keeper.Allow(h.Allow...); len(added) > 0 {
			_ = keeper.Reload()
		}
	}
	if o.learn {
		if c, ok := ctr.Get(pl.ID, g.Network); ok && c.IP != "" {
			if err := keeper.SetLearn(c.IP, true); err == nil && keeper.Reload() == nil {
				say("learn mode ON for %s: all egress allowed and logged. `zoo learned` to review, `zoo learn off` to end", pl.Name)
			}
		}
	}

	env := append([]string{
		"HOME=/home/agent", "USER=agent", "ZOO_CAGE=" + pl.Name, "ZOO_INSIDE=1",
		"TERM=" + envOr("TERM", "xterm-256color"), "COLORTERM=" + os.Getenv("COLORTERM"), "LANG=" + envOr("LANG", "en_US.UTF-8"),
	}, cage.ProxyEnv(ip)...)
	var missing []string
	for envName, item := range h.Env {
		v, err := keys.Get(item)
		if err != nil {
			say("keychain %s: %v", item, err)
		}
		if v == "" {
			missing = append(missing, item)
			continue
		}
		env = append(env, envName+"="+v)
	}
	for rel, item := range h.Files {
		v, _ := keys.Get(item)
		if v == "" {
			continue
		}
		if err := writeGuestFile(pl.ID, "/home/agent/"+rel, v); err != nil {
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

	execArgs := []string{"exec", "-i"}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		execArgs = append(execArgs, "-t")
	}
	execArgs = append(execArgs, "-u", "agent", "-w", workdir, "--env-file", envFile, pl.ID, h.Command)
	execArgs = append(execArgs, o.rest...)

	sess := registerSession(g, pl.Name, o.harness)
	defer endSession(g, sess)
	// Remove the env file once the CLI has read it.
	go func() { time.Sleep(3 * time.Second); cleanup() }()
	return ctr.Attach(execArgs...)
}

func anyKey(files map[string]string) bool {
	for _, item := range files {
		if keys.Has(item) {
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

func writeGuestFile(id, path, content string) error {
	cmd := exec.Command(ctr.Bin(), "exec", "-i", "-u", "agent", id, "sh", "-c",
		`umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
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
	cs, err := cage.List(g)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		fmt.Println("no cages yet. In a repo: zoo run claude")
		return nil
	}
	learn := keeper.LearnIPs()
	home, _ := os.UserHomeDir()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAGE\tSTATUS\tSIZE\tIP\tWORKTREE")
	for _, c := range cs {
		st := c.Status
		if learn[c.IP] {
			st += " (learn)"
		}
		wt := c.Worktree
		if home != "" && strings.HasPrefix(wt, home) {
			wt = "~" + strings.TrimPrefix(wt, home)
		}
		size := c.Size
		if c.CPUs != "" {
			size += fmt.Sprintf(" (%s CPU, %s)", c.CPUs, c.Memory)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, st, size, c.IP, wt)
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
	if args[0] == "all" {
		cs, err := cage.List(g)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Running {
				say("stopping %s", c.Name)
				if err := ctr.Stop(c.ID); err != nil {
					say("%v", err)
				}
			}
		}
		return nil
	}
	c, err := cage.Find(g, args[0])
	if err != nil {
		return err
	}
	if c.IP != "" {
		_ = keeper.SetLearn(c.IP, false)
	}
	return ctr.Stop(c.ID)
}

func cmdReset(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: zoo reset <cage>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	c, err := cage.Find(g, args[0])
	if err != nil {
		return err
	}
	if c.IP != "" {
		_ = keeper.SetLearn(c.IP, false)
		_ = keeper.Reload()
	}
	if err := cage.Reset(c.ID); err != nil {
		return err
	}
	say("reset %s; the next `zoo run` there starts from a clean image", c.Name)
	return nil
}

// --- allowlist / learn ------------------------------------------------------------

func reloadIfUp() {
	if c, ok := ctr.Get(keeper.Name, ""); ok && c.Running() {
		if err := keeper.Reload(); err != nil {
			say("keeper reload: %v", err)
		}
	}
}

func cmdAllow(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoo allow <host...>")
	}
	if _, err := loadCfg(); err != nil {
		return err
	}
	if err := keeper.EnsureFiles(); err != nil {
		return err
	}
	added, err := keeper.Allow(args...)
	if err != nil {
		return err
	}
	if len(added) == 0 {
		say("already allowed")
		return nil
	}
	reloadIfUp()
	say("allowed: %s", strings.Join(added, " "))
	return nil
}

func cmdDeny(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoo deny <host...>")
	}
	if _, err := loadCfg(); err != nil {
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
	reloadIfUp()
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
	return nil
}

func currentOrNamed(g config.Global, args []string) (cage.Info, error) {
	if len(args) > 0 {
		return cage.Find(g, args[0])
	}
	wd, _ := os.Getwd()
	return cage.Find(g, wd)
}

func cmdLearn(args []string) error {
	if len(args) == 0 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: zoo learn on|off [cage]")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	c, err := currentOrNamed(g, args[1:])
	if err != nil {
		return err
	}
	if c.IP == "" {
		return fmt.Errorf("%s has no address (is it running?)", c.Name)
	}
	if err := keeper.SetLearn(c.IP, args[0] == "on"); err != nil {
		return err
	}
	if err := keeper.Reload(); err != nil {
		return err
	}
	say("learn mode %s for %s", strings.ToUpper(args[0]), c.Name)
	return nil
}

func cmdLearned(args []string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	var ip string
	if len(args) > 0 {
		c, err := cage.Find(g, args[0])
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
		if !ok || e.Denied || e.Host == "" {
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
		fmt.Println("nothing learned outside the allowlist")
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
		fmt.Fprintln(tw, "ITEM\tSTORED\tUSED BY")
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
					fmt.Fprintf(tw, "%s\t%v\t%s ($%s)\n", item, keys.Has(item), n, envName)
				}
			}
			for rel, item := range h.Files {
				if !seen[item] {
					seen[item] = true
					fmt.Fprintf(tw, "%s\t%v\t%s (~/%s)\n", item, keys.Has(item), n, rel)
				}
			}
		}
		return tw.Flush()
	case "set":
		if len(args) != 2 {
			return errors.New("usage: zoo keys set <name>   (value from stdin if piped, otherwise prompted)")
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
	g, err := loadCfg()
	if err != nil {
		return err
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	pl, ip, err := prepare(g, name, "")
	if err != nil {
		return err
	}
	env := append([]string{
		"ZOO_MASKS=" + strings.Join(pl.Masks, "\n"),
		"ZOO_READONLY=" + strings.Join(pl.ReadOnly, "\n"),
		"HOME=/home/agent",
	}, cage.ProxyEnv(ip)...)
	execArgs := []string{"exec", "-u", "agent", "-w", pl.Root}
	for _, e := range env {
		execArgs = append(execArgs, "-e", e)
	}
	execArgs = append(execArgs, pl.ID, "/usr/local/bin/zoo-doctor")
	out, runErr := ctr.Run(execArgs...)

	fmt.Printf("zoo doctor · cage %s · %s\n\n", pl.Name, pl.Root)
	fails := 0
	color := isTerminal(os.Stdout)
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\033[" + code + "m" + s + "\033[0m"
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		verdict, rest, _ := strings.Cut(line, " ")
		switch verdict {
		case "PASS":
			fmt.Println(paint("32", "  ✓ ") + rest)
		case "FAIL":
			fails++
			fmt.Println(paint("31", "  ✗ ") + rest)
		case "WARN":
			fmt.Println(paint("33", "  ! ") + rest)
		default:
			fmt.Println("    " + line)
		}
	}
	// Host-side checks.
	if g.Port > 0 {
		fmt.Println(paint("32", "  ✓ ") + fmt.Sprintf("dashboard: bound to 127.0.0.1:%d only", g.Port))
	}
	if !shims.OnPath() {
		fmt.Println(paint("33", "  ! ") + "shims: not on PATH, so Conductor/herdr launch agents outside cages (zoo shims install)")
	}
	if runErr != nil && out == "" {
		return runErr
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	fmt.Println("\nall walls hold.")
	return nil
}

// --- build ------------------------------------------------------------------------

func cmdBuild(args []string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	if !ctr.Installed() {
		return errors.New("Apple's `container` CLI isn't installed")
	}
	which := []string{"keeper", "cage"}
	if len(args) > 0 {
		which = args
	}
	for _, w := range which {
		switch w {
		case "keeper":
			if err := buildImage("keeper", g.KeeperImage); err != nil {
				return err
			}
		case "cage":
			if err := buildImage("cage", g.CageImage); err != nil {
				return err
			}
			say("rebuilt the cage image; existing cages keep the old one until `zoo reset <cage>`")
		default:
			return fmt.Errorf("unknown image %q (cage or keeper)", w)
		}
	}
	return nil
}
