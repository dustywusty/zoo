package shims

import (
	"os"
	"strings"

	"github.com/dusty/zoo/internal/paths"
)

// Installed reports whether zoo wrote shims, so an update knows to write
// them again for the new zoo.
func Installed() bool {
	entries, err := os.ReadDir(paths.Shims())
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == ".zoo-shims" || !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}
