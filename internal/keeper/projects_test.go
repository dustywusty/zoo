package keeper

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/paths"
)

func TestProjectPolicyCannotFallThrough(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	rules := []ProjectAccess{{IP: "172.20.0.2", Allow: []string{"api.anthropic.com"}}, {IP: "172.21.0.2", Allow: nil}, {IP: "172.22.0.2", Allow: []string{}}}
	if changed, err := SyncProjects(rules); err != nil || !changed {
		t.Fatalf("sync: %v %v", changed, err)
	}
	conf, err := os.ReadFile(paths.SquidConf())
	if err != nil {
		t.Fatal(err)
	}
	s := string(conf)
	for _, want := range []string{"http_access allow zoo_project_0 zoo_project_hosts_0\nhttp_access deny zoo_project_0", "http_access allow zoo_project_1 zoo_allowed\nhttp_access deny zoo_project_1", "http_access deny zoo_project_2"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing scoped rule %q", want)
		}
	}
	if strings.Contains(s, "http_access allow zoo_allowed\n") || strings.Contains(s, "http_access allow zoo_project_2") {
		t.Fatal("restricted or unknown cage could use global access")
	}
	if strings.Index(s, "http_access deny zoo_private\n") > strings.Index(s, "http_access allow zoo_project_0") {
		t.Fatal("project rule bypasses private-address denial")
	}
	// A failed reload must keep requesting a reload even with unchanged data.
	if changed, err := SyncProjects(rules); err != nil || !changed {
		t.Fatalf("failed reload not retried: %v %v", changed, err)
	}
	if err := os.WriteFile(appliedConfigPath(), configDigest(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := SyncProjects(rules); err != nil || changed {
		t.Fatalf("unchanged applied config: %v %v", changed, err)
	}
}

func TestProjectPolicyRejectsConfigInjection(t *testing.T) {
	for _, host := range []string{"x\nhttp_access allow all", "x #", "http://x.com", "*", ".", "x..com"} {
		if _, err := projectBlock([]ProjectAccess{{IP: "172.20.0.2", Allow: []string{host}}}); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
	if _, err := projectBlock([]ProjectAccess{{IP: "172.20.0.2"}, {IP: "172.20.0.2"}}); err == nil {
		t.Fatal("ambiguous cage address accepted")
	}
}

// Opt-in validation uses a separate, networkless container, never zoo-keeper.
func TestProjectSquidSyntax(t *testing.T) {
	image := os.Getenv("ZOO_TEST_SQUID_IMAGE")
	if image == "" {
		t.Skip("set ZOO_TEST_SQUID_IMAGE to an existing keeper image")
	}
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProjects([]ProjectAccess{{IP: "172.20.0.2", Allow: []string{"api.anthropic.com"}}, {IP: "172.21.0.2"}, {IP: "172.22.0.2", Allow: []string{}}}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--mount", "type=bind,source="+paths.KeeperEtc()+",target=/etc/zoo,readonly", "--entrypoint", "squid", image, "-k", "parse", "-f", "/etc/zoo/squid.conf")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("squid parse: %v\n%s", err, out)
	}
}
