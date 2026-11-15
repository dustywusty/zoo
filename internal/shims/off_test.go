package shims

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dusty/zoo/internal/paths"
)

func TestOffHere(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "projects", "windows-local")
	work := filepath.Join(root, "workspaces", "windows-local")
	off := []string{host, work + string(filepath.Separator), "relative/is/ignored", ""}

	for dir, want := range map[string]bool{
		host:                                  true,
		filepath.Join(host, "sub", "dir"):     true,
		filepath.Join(work, "pelican"):        true,
		host + "-other":                       false, // a prefix of the name is not inside
		filepath.Join(root, "workspaces"):     false,
		filepath.Join(root, "random-project"): false,
		"":                                    false,
	} {
		if got := OffHere(dir, off); got != want {
			t.Errorf("OffHere(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestOffHereFoldsCaseWhereTheFileSystemDoes(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-sensitive file system")
	}
	root := t.TempDir()
	if !OffHere(filepath.Join(root, "PROJECTS", "Windows-Local"), []string{filepath.Join(root, "projects", "windows-local")}) {
		t.Error("a folder with different letter case is not off")
	}
}

func TestOffHereExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if !OffHere(filepath.Join(home, "orca", "projects", "x"), []string{"~/orca/projects"}) {
		t.Error("~/orca/projects does not match a folder in the home directory")
	}
}

// The shim reads the off folders from ~/.zoo/config.yaml and compares the
// working directory with them. A configuration that does not load sends
// the agent to a cage.
func TestRunRealReadsTheConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOO_HOME", home)
	t.Setenv("ZOO", "")
	t.Setenv("ZOO_INSIDE", "")
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	offDir := filepath.Join(home, "host-project")
	cagedDir := filepath.Join(home, "random-project")
	for _, d := range []string{offDir, cagedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "shims:\n  off:\n    - '" + offDir + "'\n"
	if err := os.WriteFile(paths.Config(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(offDir)
	if !runReal() {
		t.Error("in an off folder, the shim does not run the real CLI")
	}
	t.Chdir(cagedDir)
	if runReal() {
		t.Error("outside the off folders, the shim runs the real CLI")
	}

	if err := os.WriteFile(paths.Config(), []byte("shims: [not, a, map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(offDir)
	if runReal() {
		t.Error("with a configuration that does not load, the shim runs the real CLI")
	}
}
