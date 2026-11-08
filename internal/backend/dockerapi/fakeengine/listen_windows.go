//go:build windows

package fakeengine

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

const pipePrefix = `\\.\pipe\`

func listen(t *testing.T) (net.Listener, string) {
	short := fmt.Sprintf("zoo-fakeengine-%d-%d", os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(pipePrefix+short, nil)
	if err != nil {
		t.Fatal(err)
	}
	// DOCKER_HOST spelling of \\.\pipe\<short>.
	return ln, "npipe:////./pipe/" + short
}
