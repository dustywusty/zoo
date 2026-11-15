package shims

import (
	"os"
	"strings"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/pathmatch"
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
	for _, o := range off {
		if pathmatch.In(dir, o) {
			return true
		}
	}
	return false
}
