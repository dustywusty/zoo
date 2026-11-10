package cage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
)

func TestNetworkUsesApprovedProjectOnly(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := keeper.EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	p := filepath.Join(root, "zoo.yaml")
	if err := os.WriteFile(p, []byte("network:\n  allow: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Trust(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("network:\n  allow: [evil.example]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncNetwork([]Info{{Name: "test", Worktree: root, Running: true, IP: "172.20.0.2"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(paths.SquidConf())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "evil.example") || strings.Contains(string(b), "http_access allow zoo_project_0") {
		t.Fatal("agent edit widened approved deny-all policy")
	}
}
