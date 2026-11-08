//go:build !windows

package dockerapi

import (
	"context"
	"fmt"
	"net"
)

func dialPipe(_ context.Context, path string) (net.Conn, error) {
	return nil, fmt.Errorf("named pipe %s: only on Windows", path)
}
