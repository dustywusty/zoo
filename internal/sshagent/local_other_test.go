//go:build !windows

package sshagent

import (
	"net"
	"path/filepath"
	"testing"
)

// listenLocal listens where an agent would on this OS: a Unix socket.
func listenLocal(t *testing.T, name string) (net.Listener, string) {
	t.Helper()
	addr := filepath.Join(shortDir(t), name+".sock")
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	return ln, addr
}

func dialLocal(addr string) (net.Conn, error) { return net.Dial("unix", addr) }

// ownerOnly is a no-op here: the files are written 0600 already.
func ownerOnly(*testing.T, string) {}
