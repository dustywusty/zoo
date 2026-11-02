//go:build windows

package dockerapi

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// dialPipe opens a Windows named pipe (\\.\pipe\docker_engine) with
// overlapped I/O, so a streaming read and a request can share it.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
