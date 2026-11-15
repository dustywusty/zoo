package pathmatch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIn(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "proj")
	for dir, want := range map[string]bool{
		f:                          true,
		filepath.Join(f, "a", "b"): true,
		f + "-other":               false,
		root:                       false,
		"":                         false,
	} {
		if got := In(dir, f); got != want {
			t.Errorf("In(%q, %q) = %v, want %v", dir, f, got, want)
		}
	}
	if In(f, "relative/path") || In(f, "") {
		t.Error("a relative or empty folder matched")
	}
	if home, err := os.UserHomeDir(); err == nil && !In(filepath.Join(home, "x", "y"), "~/x") {
		t.Error("~/x does not match a folder in the home directory")
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && !In(filepath.Join(root, "PROJ"), f) {
		t.Error("letter case matters on a file system that ignores it")
	}
}
