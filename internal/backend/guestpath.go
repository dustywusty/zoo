package backend

import (
	"fmt"
	"strings"
)

// GuestPath is where a host path appears inside a Linux cage. Cages mount
// the worktree at the same path it has on the host, so absolute paths (git's
// gitdir pointers, tool caches, error messages) mean the same thing on both
// sides. Unix paths are used as-is. A Windows path can't be a Linux path, so
// it maps to the Git Bash / MSYS form: C:\Users\me\repo -> /c/Users/me/repo.
func GuestPath(p string) string {
	if !isDrivePath(p) {
		return p
	}
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	if rest == "" || rest[0] != '/' {
		rest = "/" + rest
	}
	return "/" + strings.ToLower(p[:1]) + strings.TrimSuffix(rest, "/")
}

// GuestPaths maps every path with GuestPath.
func GuestPaths(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = GuestPath(p)
	}
	return out
}

// CheckGuestPath rejects host paths a cage can't mount at a stable guest
// path: UNC shares and Windows paths relative to a drive.
func CheckGuestPath(p string) error {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") && len(p) > 2 && p[2] != '/' {
		return fmt.Errorf("%s: network (UNC) paths can't be mounted into a cage; use a local drive", p)
	}
	if len(p) >= 2 && p[1] == ':' && isLetter(p[0]) && !isDrivePath(p) {
		return fmt.Errorf("%s: drive-relative path; use an absolute one", p)
	}
	return nil
}

func isDrivePath(p string) bool {
	return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
