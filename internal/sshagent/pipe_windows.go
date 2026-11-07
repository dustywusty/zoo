//go:build windows

package sshagent

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

func dialPipe(ctx context.Context, p string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, strings.ReplaceAll(p, "/", `\`))
}

// pipeExists dials the pipe briefly: Windows has no cheap stat for it.
func pipeExists(p string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c, err := dialPipe(ctx, p)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
