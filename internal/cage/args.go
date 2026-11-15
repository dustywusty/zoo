package cage

import (
	"path/filepath"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/pathmatch"
)

// GuestArgs rewrites host paths in an agent's arguments to the paths the
// cage sees them at. Only paths in dirs (the worktrees and host folders
// mounted in the cage) change: on Windows, C:\Users\you\x becomes
// /c/Users/you/x. "--flag=<path>" keeps its flag. Elsewhere host and
// guest paths are the same, so nothing changes.
func GuestArgs(args, dirs []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = guestArg(a, dirs)
	}
	return out
}

func guestArg(a string, dirs []string) string {
	prefix, val := "", a
	if strings.HasPrefix(a, "-") {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return a
		}
		prefix, val = k+"=", v
	}
	if !filepath.IsAbs(val) {
		return a
	}
	for _, d := range dirs {
		if pathmatch.In(val, d) {
			return prefix + backend.GuestPath(val)
		}
	}
	return a
}
