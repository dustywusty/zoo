package main

// The SSH door (M6): `ssh zoo-<cage>` for editors, with no listener. See
// internal/sshdoor for the design.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/services"
	"github.com/dusty/zoo/internal/sshdoor"
)

func sshDir() string        { return filepath.Join(paths.Home(), "ssh") }
func sshConfigPath() string { return filepath.Join(sshDir(), "config") }

// cmdSSH is `zoo ssh setup` and `zoo ssh [cage] [cmd...]`.
func cmdSSH(args []string) error {
	if len(args) > 0 && args[0] == "setup" {
		path, err := writeSSHConfig()
		if err != nil {
			return err
		}
		say("wrote %s", path)
		fmt.Fprintf(os.Stderr, `
Add this line at the TOP of ~/.ssh/config (ssh uses the first value it sees,
so it must come before any "Host *" block):

    Include %s

Then: ssh zoo-<cage>, or pick zoo-<cage> in VS Code Remote-SSH, JetBrains
Gateway or Orca. Run zoo ssh setup again after making new cages to list them
by name (zoo-* works for any cage either way).
`, sshdoor.IncludePath(os.Getenv("ZOO_HOME")))
		return nil
	}
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = sshdoor.CageFromHost(args[0]), args[1:]
	}
	if name == "" {
		wd, _ := os.Getwd()
		root, err := cage.Root(wd)
		if err != nil {
			return err
		}
		name = cage.NameFor(root)
	}
	if !sshdoor.ValidCageName(name) {
		return fmt.Errorf("%q isn't a cage name (see zoo ls)", name)
	}
	cfg, err := writeSSHConfig()
	if err != nil {
		return err
	}
	argv := append([]string{"-F", cfg, sshdoor.HostPrefix + name}, args...)
	cmd := exec.Command("ssh", argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// writeSSHConfig (re)writes ~/.zoo/ssh/config and makes sure the client key
// exists. It lists cages by name when the backend answers, and falls back to
// the zoo-* catch-all alone when it doesn't.
func writeSSHConfig() (string, error) {
	if err := paths.Ensure(); err != nil {
		return "", err
	}
	if _, err := sshdoor.EnsureClientKey(sshDir()); err != nil {
		return "", fmt.Errorf("ssh key: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	var names []string
	if g, err := loadCfg(); err == nil {
		ctx := context.Background()
		if e, err := resolve(ctx, g); err == nil && e.b.Detect(ctx).Ready() {
			if cs, err := cage.List(ctx, e.b); err == nil {
				for _, c := range cs {
					names = append(names, c.Name)
				}
			}
		}
	}
	key := sshdoor.KeyPath(sshDir())
	zooHome := os.Getenv("ZOO_HOME")
	if zooHome == "" {
		key = "~/.zoo/ssh/id_ed25519"
	} else if abs, err := filepath.Abs(zooHome); err == nil {
		zooHome = abs
		key = sshdoor.KeyPath(filepath.Join(abs, "ssh"))
	}
	text, err := sshdoor.ClientConfig(self, key, zooHome, names)
	if err != nil {
		return "", err
	}
	path := sshConfigPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// cmdSSHProxy is ssh's ProxyCommand: stdin/stdout are the SSH connection,
// so nothing else may touch them. Messages go to stderr (ssh shows them).
func cmdSSHProxy(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: zoo ssh-proxy <cage> (ssh runs this; see zoo ssh setup)")
	}
	// Keep the connection's ends, then point everything else (say, image
	// builds, runtime start-up, prompts) at stderr and /dev/null.
	conn := struct {
		in  *os.File
		out *os.File
	}{os.Stdin, os.Stdout}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	os.Stdout, os.Stdin = os.Stderr, devnull

	name := sshdoor.CageFromHost(args[0])
	if !sshdoor.ValidCageName(name) {
		return fmt.Errorf("%q isn't a cage name", args[0])
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	cs, err := cage.List(ctx, e.b)
	if err != nil {
		return err
	}
	var target *cage.Info
	for i, c := range cs {
		if c.Name == name || c.ID == name {
			target = &cs[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no cage %q: start it once with zoo run or zoo shell in its worktree (zoo ls lists cages)", name)
	}
	if target.Worktree == "" {
		return fmt.Errorf("cage %s has no worktree label; zoo reset %s", target.Name, target.Name)
	}
	// prepare looks a cage up by name or path; the worktree path is exact.
	pl, ip, err := prepare(ctx, e, target.Worktree, "")
	if err != nil {
		return err
	}
	if pl.Name != target.Name {
		return fmt.Errorf("cage %s resolved to %s; zoo reset %s", target.Name, pl.Name, target.Name)
	}
	authLine, err := sshdoor.EnsureClientKey(sshDir())
	if err != nil {
		return fmt.Errorf("ssh key: %w", err)
	}
	env := append(sshdoor.BaseEnv(pl.Name), cage.ProxyEnv(ip)...)
	if all, err := cage.List(ctx, e.b); err == nil {
		env = append(env, services.SessionEnv(e.g, services.GrantsFor(e.g, all)[pl.Name])...)
	}
	sess := registerSession(g, pl.Name, "ssh")
	defer endSession(g, sess)
	code, err := sshProxy(ctx, e.b, pl.ID, pl.Name, authLine, env, conn.in, conn.out)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// sshProxy installs the client key and session environment in the cage as
// guest root, then runs sshd as the agent on the connection. env must not
// hold harness credentials: SSH sessions get the cage's environment, not an
// agent session's.
func sshProxy(ctx context.Context, b backend.Backend, id, name, authLine string, env []string, in io.Reader, out io.Writer) (int, error) {
	conf, skipped := sshdoor.EnvConf(env)
	for _, k := range skipped {
		say("ssh: left %s out of the session environment (characters sshd can't carry)", k)
	}
	var errb bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: id, User: "root",
		Cmd:    []string{"sh", "-c", sshdoor.InstallScript},
		Stdin:  strings.NewReader(authLine + "\n" + conf),
		Stdout: &errb, Stderr: &errb,
	})
	if err != nil {
		return -1, err
	}
	if code == 3 {
		return -1, fmt.Errorf("cage %s was made from an image without the SSH door: `zoo build cage`, then `zoo reset %s`", name, name)
	}
	if code != 0 {
		return -1, fmt.Errorf("set up ssh in %s: exit %d: %s", name, code, strings.TrimSpace(errb.String()))
	}
	return b.Exec(ctx, backend.ExecSpec{
		ID: id, User: "agent", Workdir: "/home/agent",
		Cmd:   []string{"sh", "-c", sshdoor.SSHDScript},
		Stdin: in, Stdout: out, Stderr: os.Stderr,
	})
}
