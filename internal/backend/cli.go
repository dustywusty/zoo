package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/dusty/zoo/internal/proc"
)

// CLI shells out to a runtime's command-line tool (decision D5: no SDKs).
type CLI struct {
	Bin string
}

// Installed reports whether the binary is on PATH.
func (c CLI) Installed() bool {
	_, err := exec.LookPath(c.Bin)
	return err == nil
}

// Run runs the CLI and returns stdout. Errors include stderr. Output is
// returned even on error (some commands print partial results and exit 1).
func (c CLI) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	proc.Quiet(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%s %s: %v: %s", c.Bin, strings.Join(Redact(args), " "), err, msg)
	}
	return out.String(), nil
}

// OK runs the CLI and reports only whether it exited 0.
func (c CLI) OK(ctx context.Context, args ...string) bool {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	proc.Quiet(cmd)
	return cmd.Run() == nil
}

// Attach runs the CLI wired to the terminal.
func (c CLI) Attach(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Exec runs `<bin> exec …` for a spec and returns the exit code. The flag
// shape is shared by Apple `container`, docker and podman.
func (c CLI) Exec(ctx context.Context, s ExecSpec) (int, error) {
	args := []string{"exec"}
	if !s.NoStdin {
		args = append(args, "-i")
	}
	if s.TTY {
		args = append(args, "-t")
	}
	if s.User != "" {
		args = append(args, "-u", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "-w", s.Workdir)
	}
	if s.EnvFile != "" {
		args = append(args, "--env-file", s.EnvFile)
	}
	for _, e := range s.Env {
		args = append(args, "-e", e)
	}
	args = append(append(args, s.ID), s.Cmd...)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = or[io.Reader](s.Stdin, os.Stdin), or[io.Writer](s.Stdout, os.Stdout), or[io.Writer](s.Stderr, os.Stderr)
	if s.NoStdin {
		cmd.Stdin = nil
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, fmt.Errorf("%s exec: %w", c.Bin, err)
	}
	return 0, nil
}

func or[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// Redact hides values of -e KEY=VALUE pairs in error messages.
func Redact(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-e" || out[i] == "--env" {
			if k, _, ok := strings.Cut(out[i+1], "="); ok {
				out[i+1] = k + "=…"
			}
		}
	}
	return out
}
