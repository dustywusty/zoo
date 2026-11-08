//go:build !windows

package fakeengine

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func listen(t *testing.T) (net.Listener, string) {
	// Unix socket paths are short (~104 bytes on macOS): not t.TempDir().
	dir, err := os.MkdirTemp("", "fe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	return ln, "unix://" + p
}
