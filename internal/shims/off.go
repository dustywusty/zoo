package shims

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dusty/zoo/internal/config"
)

// runReal reports whether a shim runs the real CLI instead of a cage:
// ZOO=off, already inside a cage, or the working directory is in one of
// the shims.off folders in ~/.zoo/config.yaml. If the configuration does
// not load, there are no off folders: the agent still goes to a cage,
// and zoo run reports the error.
func runReal() bool {
	if strings.EqualFold(os.Getenv("ZOO"), "off") || os.Getenv("ZOO_INSIDE") != "" {
		return true
	}
	g, err := config.LoadGlobal()
	if err != nil {
		return false
	}
	wd, err := os.Getwd()
	if err != nil {
		return false
	}
	return OffHere(wd, g.Shims.Off)
}

// OffHere reports whether dir is one of the off folders or inside one.
// It compares paths only. It does not follow a worktree's .git file to
// its main repository, because a downloaded repository could point that
// file into an off folder and run its agent outside a cage. Relative
// entries are ignored. "~" is the home directory.
func OffHere(dir string, off []string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	for _, o := range off {
		o = expandHome(strings.TrimSpace(o))
		if o == "" || !filepath.IsAbs(o) {
			continue
		}
		if within(dir, filepath.Clean(o)) {
			return true
		}
	}
	return false
}

func within(dir, root string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		dir, root = strings.ToLower(dir), strings.ToLower(root)
	}
	if dir == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(dir, root)
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
