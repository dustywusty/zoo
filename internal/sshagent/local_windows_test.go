//go:build windows

package sshagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

var pipeSeq atomic.Int64

// listenLocal listens where an agent would on this OS: a named pipe.
func listenLocal(t *testing.T, name string) (net.Listener, string) {
	t.Helper()
	addr := fmt.Sprintf("%szoo-sshagent-test-%d-%d-%s", pipePrefix, os.Getpid(), pipeSeq.Add(1), name)
	ln, err := winio.ListenPipe(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ln, addr
}

func dialLocal(addr string) (net.Conn, error) { return dialPipe(context.Background(), addr) }

func TestWindowsAgentPipeName(t *testing.T) {
	if windowsAgentPipe != `\\.\pipe\openssh-ssh-agent` {
		t.Fatalf("agent pipe = %q", windowsAgentPipe)
	}
}

// On Windows the named agents (system, 1Password, Bitwarden) are all the
// OpenSSH agent pipe.
func TestResolveWindowsPipe(t *testing.T) {
	old, oldOS, oldEnv := windowsAgentPipe, goos, getenv
	t.Cleanup(func() { windowsAgentPipe, goos, getenv = old, oldOS, oldEnv })
	goos = "windows"
	getenv = func(string) string { return "" }
	windowsAgentPipe = fmt.Sprintf("%szoo-sshagent-test-%d-agent", pipePrefix, os.Getpid())
	for _, n := range []string{"system", "1password", "bitwarden"} {
		if _, err := Resolve(n); err == nil {
			t.Fatalf("%s resolved with no agent running", n)
		}
	}
	ln, err := winio.ListenPipe(windowsAgentPipe, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	for _, n := range []string{"system", "1password", "Bitwarden", windowsAgentPipe} {
		if got, err := Resolve(n); err != nil || got != windowsAgentPipe {
			t.Errorf("Resolve(%q) = %q, %v", n, got, err)
		}
	}
}

// ownerOnly gives p an ACL with just the current user: Windows OpenSSH
// refuses key files others can read, and file modes (0600) mean nothing
// here. Temp dirs inherit whatever the machine grants.
func ownerOnly(t *testing.T, p string) {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(u.User.Sid)},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}
