package inject

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

func setup(t *testing.T) {
	t.Helper()
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceholderBinding(t *testing.T) {
	setup(t)
	c := config.Credential{Prefix: "ghp_"}
	ph, err := Placeholder(c, "github", "api-1a2b3c")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ph, "ghp_zoocred.github.api-1a2b3c.") {
		t.Fatalf("placeholder %q", ph)
	}
	cred, cg, ok := Parse("Bearer " + ph)
	if !ok || cred != "github" || cg != "api-1a2b3c" {
		t.Fatalf("parse = %q %q %v", cred, cg, ok)
	}
	// A cage can't relabel its placeholder for another cage or credential.
	for _, forged := range []string{
		strings.Replace(ph, "api-1a2b3c", "web-9f8e7d", 1),
		strings.Replace(ph, ".github.", ".prod-db.", 1),
		ph[:len(ph)-1] + "0",
	} {
		if _, _, ok := Parse(forged); ok && forged != ph {
			t.Errorf("forged placeholder accepted: %s", forged)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := map[string]config.Credential{"github": {Hosts: []string{"github.com", "api.github.com"}, Push: []string{"agent/*"}, Repos: []string{"you/app"}}}
	if err := Validate(ok, nil); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]map[string]config.Credential{
		"wildcard": {"a": {Hosts: []string{".github.com"}}},
		"port":     {"a": {Hosts: []string{"github.com:443"}}},
		"no hosts": {"a": {}},
		"overlap":  {"a": {Hosts: []string{"x.com"}}, "b": {Hosts: []string{"x.com"}}},
		"repo":     {"a": {Hosts: []string{"x.com"}, Repos: []string{"justname"}}},
		"Name":     {"Bad": {Hosts: []string{"x.com"}}},
	} {
		if Validate(bad, nil) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if Validate(ok, map[string]config.Service{"github": {Remote: "github.com:22"}}) == nil {
		t.Error("credential and service with one name accepted")
	}
}

func TestActiveAndCA(t *testing.T) {
	setup(t)
	g := config.Defaults()
	// The built-in model-key definitions stay dormant until referenced.
	if a := Active(g, nil); len(a) != 0 {
		t.Fatalf("dormant credentials active: %v", a)
	}
	g.Harnesses["claude"] = config.Harness{Command: "claude", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "cred:claude"}}
	g.Credentials["github"] = config.Credential{Hosts: []string{"github.com", "api.github.com"}, Secret: "gh", ReadOnly: true}
	a := Active(g, nil)
	if _, ok := a["claude"]; !ok {
		t.Fatal("referenced credential not active")
	}
	if _, ok := a["github"]; !ok {
		t.Fatal("policy credential not active")
	}
	if _, err := Sync(a); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(paths.InjectDir(), "ca.pem"))
	blk, _ := pem.Decode(b)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.PermittedDNSDomainsCritical || strings.Join(ca.PermittedDNSDomains, ",") != "api.anthropic.com,api.github.com,github.com" {
		t.Fatalf("CA constraints: %v", ca.PermittedDNSDomains)
	}
	hosts, _ := os.ReadFile(paths.InjectHosts())
	if strings.TrimSpace(string(hosts)) != "api.anthropic.com\napi.github.com\ngithub.com" {
		t.Fatalf("inject hosts: %q", hosts)
	}
	// Unchanged hosts: no new CA. Changed hosts: a new, re-constrained CA.
	if changed, _ := Sync(a); changed {
		t.Fatal("re-sync with same hosts changed files")
	}
	delete(a, "github")
	if changed, _ := Sync(a); !changed {
		t.Fatal("host change didn't regenerate")
	}
}

func TestEnv(t *testing.T) {
	setup(t)
	g := config.Defaults()
	g.Credentials["github"] = config.Credential{Hosts: []string{"github.com"}, Secret: "gh", Env: []string{"GH_TOKEN"}}
	active := map[string]config.Credential{"github": g.Credentials["github"], "claude": g.Credentials["claude"]}
	env, err := Env(g, active, "api-1", []string{"github"}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "cred:claude"})
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(env, "\n")
	if !strings.Contains(j, "GH_TOKEN=zoocred.github.api-1.") || !strings.Contains(j, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-zoocred.claude.api-1.") || !strings.Contains(j, "NODE_EXTRA_CA_CERTS=") {
		t.Fatalf("env:\n%s", j)
	}
	// Not granted: no placeholder, and a harness reference is an error.
	env, _ = Env(g, active, "web-2", nil, nil)
	if strings.Contains(strings.Join(env, "\n"), "GH_TOKEN") {
		t.Fatal("ungranted cage got a placeholder")
	}
	g.Credentials["github"] = config.Credential{Hosts: []string{"github.com"}, Secret: "gh"}
	if _, err := Env(g, map[string]config.Credential{"github": g.Credentials["github"]}, "web-2", nil, map[string]string{"X": "cred:github"}); err == nil {
		t.Fatal("harness reference to an ungranted credential accepted")
	}
}
