package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/sshdoor"
)

// execRecorder is a backend that only records Exec calls.
type execRecorder struct {
	backend.Backend
	calls []backend.ExecSpec
	stdin []string
	codes []int
}

func (r *execRecorder) Exec(_ context.Context, s backend.ExecSpec) (int, error) {
	r.calls = append(r.calls, s)
	in := ""
	if s.Stdin != nil {
		b, _ := io.ReadAll(s.Stdin)
		in = string(b)
		if s.Stdout != nil {
			_, _ = io.WriteString(s.Stdout, "echo:"+in)
		}
	}
	r.stdin = append(r.stdin, in)
	code := 0
	if len(r.codes) > 0 {
		code, r.codes = r.codes[0], r.codes[1:]
	}
	return code, nil
}

func TestSSHProxy(t *testing.T) {
	r := &execRecorder{}
	var out bytes.Buffer
	env := append(sshdoor.BaseEnv("web"), "HTTPS_PROXY=http://10.0.0.2:3128")
	code, err := sshProxy(context.Background(), r, "zoo-web", "web", "restrict ssh-ed25519 AAAA zoo", env, strings.NewReader("SSH-2.0-client"), &out)
	if err != nil || code != 0 {
		t.Fatalf("sshProxy = %d, %v", code, err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("want 2 execs (install as root, sshd as agent), got %d", len(r.calls))
	}
	inst, sshd := r.calls[0], r.calls[1]
	if inst.User != "root" || inst.ID != "zoo-web" || inst.TTY || inst.NoStdin {
		t.Errorf("install exec = %+v", inst)
	}
	if !strings.HasPrefix(r.stdin[0], "restrict ssh-ed25519 AAAA zoo\n") || !strings.Contains(r.stdin[0], `"HTTPS_PROXY=http://10.0.0.2:3128"`) {
		t.Errorf("install stdin = %q", r.stdin[0])
	}
	for _, a := range inst.Cmd {
		if strings.Contains(a, "ssh-ed25519") || strings.Contains(a, "HTTPS_PROXY") {
			t.Errorf("key or env on argv: %q", a)
		}
	}
	if sshd.User != "agent" || sshd.TTY || sshd.NoStdin || sshd.EnvFile != "" || len(sshd.Env) != 0 {
		t.Errorf("sshd exec = %+v (agent, no tty, stdin attached, no env)", sshd)
	}
	if strings.Join(sshd.Cmd, " ") != "sh -c "+sshdoor.SSHDScript {
		t.Errorf("sshd cmd = %q", sshd.Cmd)
	}
	if r.stdin[1] != "SSH-2.0-client" || out.String() != "echo:SSH-2.0-client" {
		t.Errorf("connection not wired through: in %q out %q", r.stdin[1], out.String())
	}
}

func TestSSHProxyOldImage(t *testing.T) {
	r := &execRecorder{codes: []int{3}}
	_, err := sshProxy(context.Background(), r, "zoo-web", "web", "k", nil, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "zoo build cage") {
		t.Fatalf("err = %v, want a rebuild hint", err)
	}
	if len(r.calls) != 1 {
		t.Error("sshd must not run when setup failed")
	}
}
