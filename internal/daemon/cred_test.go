package daemon

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"

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
	s := &Server{cfg: config.Global{Credentials: map[string]config.Credential{"github": cred}}, svc: newSvcState()}
	s.cages = []cage.Info{{Name: "api-1", Running: true}, {Name: "web-2", Running: true}, {Name: "old-3"}}
	s.svc.grants = map[string][]string{"api-1": {"github"}, "old-3": {"github"}}

	ask := func(ph string) string {
		a, b := net.Pipe()
		go func() { s.credAnswer(a, ph); a.Close() }()
		line, _ := bufio.NewReader(b).ReadString('\n')
		return strings.TrimSpace(line)
	}
	ph := func(cg string) string { p, _ := inject.Placeholder(cred, "github", cg); return p }

	if got := ask(ph("api-1")); got != "OK "+base64.StdEncoding.EncodeToString([]byte("ghp_REAL")) {
		t.Fatalf("granted cage: %q", got)
	}
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
