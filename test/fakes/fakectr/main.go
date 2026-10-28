// Command fakectr emulates the parts of Apple's `container` CLI and the
// docker/podman CLIs that zoo calls, keeping state in a JSON file at
// $ZOO_FAKE_STATE. The dialect comes from the name it's invoked as:
// "container" speaks Apple, "docker" and "podman" speak docker. Install it
// under those names (symlink or copy) and point zoo at it with
// ZOO_CONTAINER_BIN / ZOO_DOCKER_BIN.
//
// Output shapes follow the real CLIs (container 1.6.1, docker 29 on
// OrbStack). Anything the fake doesn't know exits 2 so tests notice a
// backend calling something the real CLI might reject.
//
// Env:
//
//	ZOO_FAKE_STATE           state file (required)
//	ZOO_FAKE_DIALECT         apple|docker|podman, overrides argv[0]
//	ZOO_FAKE_DOWN=1          the runtime isn't running (status/info fail)
//	ZOO_FAKE_DOCKER_RUNTIME  docker DefaultRuntime / podman ociRuntime (default runc / crun)
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dusty/zoo/test/fakes/fakestate"
)

type fake struct {
	dialect string // "apple", "docker", "podman"
	st      *fakestate.State
	out     io.Writer
	errw    io.Writer
	// execRun is set by exec: the command to run after the state is saved
	// and unlocked.
	execRun *exec.Cmd
}

func (f *fake) apple() bool { return f.dialect == "apple" }

func dialectFor(argv0 string) string {
	if d := os.Getenv("ZOO_FAKE_DIALECT"); d != "" {
		return d
	}
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	switch {
	case base == "container" || strings.HasPrefix(base, "container-"):
		return "apple"
	case base == "podman" || strings.HasPrefix(base, "podman-"):
		return "podman"
	case base == "docker" || strings.HasPrefix(base, "docker-"):
		return "docker"
	}
	return ""
}

func main() { os.Exit(run(os.Args)) }

func run(argv []string) int {
	dialect := dialectFor(argv[0])
	if dialect == "" {
		fmt.Fprintf(os.Stderr, "fakectr: invoke as `container`, `docker` or `podman` (got %q) or set ZOO_FAKE_DIALECT\n", argv[0])
		return 2
	}
	path := os.Getenv("ZOO_FAKE_STATE")
	if path == "" {
		fmt.Fprintln(os.Stderr, "fakectr: ZOO_FAKE_STATE is not set (path to a JSON state file)")
		return 2
	}
	unlock, err := lock(path + ".lock")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakectr: lock:", err)
		return 1
	}
	st, err := fakestate.Load(path)
	if err != nil {
		unlock()
		fmt.Fprintln(os.Stderr, "fakectr: load state:", err)
		return 1
	}
	f := &fake{dialect: dialect, st: st, out: os.Stdout, errw: os.Stderr}
	f.builtins()
	st.Calls = append(st.Calls, append([]string{dialect}, argv[1:]...))

	var code int
	if f.apple() {
		err = f.appleCmd(argv[1:])
	} else {
		err = f.dockerCmd(argv[1:])
	}
	var ue usageError
	var ce codeError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintf(os.Stderr, "fakectr (%s): %s\n", dialect, ue.msg)
		code = 2
	case errors.As(err, &ce):
		if ce.msg != "" {
			fmt.Fprintln(os.Stderr, ce.msg)
		}
		code = ce.code
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if serr := st.Save(path); serr != nil {
		fmt.Fprintln(os.Stderr, "fakectr: save state:", serr)
		code = 1
	}
	unlock()
	if f.execRun != nil && code == 0 {
		return runExec(f.execRun)
	}
	return code
}

// codeError exits with a given code after printing msg.
type codeError struct {
	code int
	msg  string
}

func (e codeError) Error() string { return e.msg }

func (f *fake) builtins() {
	if f.apple() {
		if f.st.Net("default") == nil {
			f.st.Networks = append(f.st.Networks, &fakestate.Network{Name: "default", Subnet: 64, Next: 2, Labels: map[string]string{}})
		}
		return
	}
	if f.st.Net("bridge") == nil {
		f.st.Networks = append(f.st.Networks, &fakestate.Network{Name: "bridge", Subnet: 215, Next: 2, Labels: map[string]string{}})
	}
}

func down() bool { return os.Getenv("ZOO_FAKE_DOWN") == "1" }

func (f *fake) printJSON(v any) error {
	return writeJSON(f.out, v)
}

// newNetwork adds a network with the next free subnet.
func (f *fake) newNetwork(name string, internal bool, labels map[string]string) *fakestate.Network {
	sub := 128
	if !f.apple() {
		sub = 97
	}
	for {
		used := false
		for _, n := range f.st.Networks {
			if n.Subnet == sub {
				used = true
			}
		}
		if !used {
			break
		}
		sub++
	}
	n := &fakestate.Network{Name: name, Internal: internal, Labels: labels, Subnet: sub, Next: 2}
	f.st.Networks = append(f.st.Networks, n)
	return n
}

func (f *fake) nextIP(network string) string {
	n := f.st.Net(network)
	if n == nil {
		return ""
	}
	ip := fmt.Sprintf("192.168.%d.%d", n.Subnet, n.Next)
	n.Next++
	return ip
}

// startC marks a container running and gives it addresses. Apple hands out a
// fresh address on every start; docker reuses the previous one.
func (f *fake) startC(c *fakestate.Container) {
	if c.Running {
		return
	}
	c.Running, c.Started = true, true
	if c.IPs == nil {
		c.IPs = map[string]string{}
	}
	if c.LastIPs == nil {
		c.LastIPs = map[string]string{}
	}
	for _, n := range c.Networks {
		ip := c.LastIPs[n]
		if f.apple() || ip == "" {
			ip = f.nextIP(n)
		}
		c.IPs[n], c.LastIPs[n] = ip, ip
	}
}

func (f *fake) stopC(c *fakestate.Container) {
	c.Running = false
	c.IPs = map[string]string{}
}

func (f *fake) removeC(c *fakestate.Container) {
	out := f.st.Containers[:0]
	for _, x := range f.st.Containers {
		if x != c {
			out = append(out, x)
		}
	}
	f.st.Containers = out
}

func (f *fake) newID(name string) string {
	f.st.Seq++
	return hashID(fmt.Sprintf("%s/%d", name, f.st.Seq))
}

func hashID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// parseMem reads "512M", "1G", "1g", "1024MB", "2GiB", or plain bytes.
// Units are binary (both runtimes treat them so).
func parseMem(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	i := 0
	for i < len(t) && (t[i] == '.' || (t[i] >= '0' && t[i] <= '9')) {
		i++
	}
	n, err := strconv.ParseFloat(t[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	mult := map[string]float64{"": 1, "b": 1, "k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20, "g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40}[t[i:]]
	if mult == 0 {
		return 0, fmt.Errorf("invalid memory size %q", s)
	}
	return int64(n * mult), nil
}

func parseLabels(vals []string) (map[string]string, error) {
	m := map[string]string{}
	for _, v := range vals {
		k, val, _ := strings.Cut(v, "=")
		if k == "" {
			return nil, usagef("invalid label %q", v)
		}
		m[k] = val
	}
	return m, nil
}

func checkEnv(vals []string) error {
	for _, v := range vals {
		if strings.HasPrefix(v, "=") || v == "" {
			return usagef("invalid env %q", v)
		}
	}
	return nil
}

// readEnvFile reads docker-style env files: KEY=VALUE lines, # comments,
// a bare KEY takes the value from the caller's environment.
func readEnvFile(path string) ([]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []string
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			if v, ok := os.LookupEnv(line); ok {
				out = append(out, line+"="+v)
			}
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// prepareExec handles `exec` for both dialects; the command itself runs on
// the host after the state is saved.
func (f *fake) prepareExec(args []string, notFound func(string) error, notRunning func(string) error) error {
	p, pos, err := parse("exec", defs("-i,--interactive", "-t,--tty", "-u,--user=", "-w,--workdir=", "--env-file=", "-e,--env=", "-d,--detach"), args, true)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("exec needs a container and a command")
	}
	if p.has("detach") {
		return usagef("fake doesn't support exec -d")
	}
	c := f.st.Find(pos[0])
	if c == nil {
		return notFound(pos[0])
	}
	if !c.Running {
		return notRunning(pos[0])
	}
	if err := checkEnv(p["env"]); err != nil {
		return err
	}
	env := append(os.Environ(), c.Env...)
	for _, ef := range p["env-file"] {
		vals, err := readEnvFile(ef)
		if err != nil {
			return fmt.Errorf("Error: read env file %s: %v", ef, err)
		}
		env = append(env, vals...)
	}
	env = append(env, p["env"]...)
	env = append(env,
		"ZOO_FAKE_EXEC_ID="+c.Name,
		"ZOO_FAKE_EXEC_USER="+p.last("user"),
		"ZOO_FAKE_EXEC_WORKDIR="+p.last("workdir"),
		"ZOO_FAKE_EXEC_TTY="+strconv.FormatBool(p.has("tty")))
	cmd := exec.Command(pos[1], pos[2:]...)
	cmd.Env = env
	if wd := p.last("workdir"); wd != "" {
		if st, err := os.Stat(wd); err == nil && st.IsDir() {
			cmd.Dir = wd
		}
	}
	if p.has("interactive") {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	f.execRun = cmd
	return nil
}

func runExec(cmd *exec.Cmd) int {
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode()
	case err != nil:
		fmt.Fprintf(os.Stderr, "exec failed: %v\n", err)
		return 127
	}
	return 0
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func pathExists(p string) (os.FileInfo, bool) {
	st, err := os.Stat(p)
	return st, err == nil
}
