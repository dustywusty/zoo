//go:build !windows

package sshagent

import (
	"context"
	"fmt"
	"net"
)

func dialPipe(_ context.Context, p string) (net.Conn, error) {
	return nil, fmt.Errorf("named pipe %s: only on Windows", p)
}

func pipeExists(string) bool { return false }
