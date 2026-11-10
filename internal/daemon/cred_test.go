package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/inject"
	"github.com/dusty/zoo/internal/keys"
	"github.com/dusty/zoo/internal/paths"
)

func TestCredAnswer(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	t.Setenv("ZOO_KEYS_FILE", "1")
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := keys.Set("gh-pat", "ghp_REAL"); err != nil {
		t.Fatal(err)
	}
	cred := config.Credential{Hosts: []string{"api.github.com"}, Secret: "gh-pat"}
	runtime := &credentialBackend{running: map[string]bool{"zoo-api-1": true, "zoo-web-2": true, "zoo-old-3": false}}
	s := &Server{backend: runtime, cfg: config.Global{Credentials: map[string]config.Credential{"github": cred}}, svc: newSvcState()}
	// Deliberately stale: the new cage is absent and the stopped cage looks live.
	s.cages = []cage.Info{{Name: "old-3", Running: true}}
	s.svc.grants = map[string][]string{"api-1": {"github"}, "old-3": {"github"}}

	ask := func(ph string) string {
		a, b := net.Pipe()
		defer b.Close()
		go func() { s.credAnswer(a, ph); a.Close() }()
		line, _ := bufio.NewReader(b).ReadString('\n')
		return strings.TrimSpace(line)
	}
	ph := func(cg string) string { p, _ := inject.Placeholder(cred, "github", cg); return p }

	if got := ask(ph("api-1")); got != "OK "+base64.StdEncoding.EncodeToString([]byte("ghp_REAL")) {
		t.Fatalf("granted cage: %q", got)
	}
	runtime.fail = true
	if got := ask(ph("api-1")); !strings.HasPrefix(got, "NO ") {
		t.Fatalf("runtime unavailable: %q", got)
	}
	runtime.fail = false
	for name, p := range map[string]string{
		"ungranted": ph("web-2"),
		"stopped":   ph("old-3"),
		"forged":    strings.Replace(ph("web-2"), "web-2", "api-1", 1),
		"garbage":   "zoocred.github.api-1.000000000000000000000000",
	} {
		if got := ask(p); !strings.HasPrefix(got, "NO ") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

type credentialBackend struct {
	backend.Backend
	running map[string]bool
	fail    bool
}

func (b *credentialBackend) Get(_ context.Context, id string) (backend.Status, bool, error) {
	if b.fail {
		return backend.Status{}, false, fmt.Errorf("test runtime unavailable")
	}
	running, exists := b.running[id]
	return backend.Status{ID: id, Running: running}, exists, nil
}
