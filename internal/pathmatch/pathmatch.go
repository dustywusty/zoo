// Package pathmatch compares host folder paths the way zoo's folder
// settings need: a folder matches itself and everything inside it, letter
// case is ignored where the file system ignores it (Windows, macOS), and
// "~" is the home directory. It compares paths only and never follows
// links that a repository could plant.
package pathmatch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// In reports whether dir is folder or inside it. A relative or empty
// folder matches nothing.
func In(dir, folder string) bool {
	folder = ExpandHome(strings.TrimSpace(folder))
	if dir == "" || folder == "" || !filepath.IsAbs(folder) {
		return false
	}
	dir, folder = fold(filepath.Clean(dir)), fold(filepath.Clean(folder))
	if dir == folder {
		return true
	}
	if !strings.HasSuffix(folder, string(filepath.Separator)) {
		folder += string(filepath.Separator)
	}
	return strings.HasPrefix(dir, folder)
}

// Same reports whether a and b name the same folder.
func Same(a, b string) bool {
	return fold(filepath.Clean(ExpandHome(a))) == fold(filepath.Clean(ExpandHome(b)))
}

func fold(p string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

// ExpandHome turns "~" and "~/x" into paths in the home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
