package cage

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mounts keep the agent out of the repo's own .git, but the worktree itself
// is writable, and git will happily treat anything in it that looks like a
// repository as one. A repository brings its own config, and config can run
// commands (core.fsmonitor, core.hooksPath, ...) the next time *you* run git
// there, or your shell prompt or editor does it for you:
//
//   - a nested .git directory (sub/.git/config)
//   - a .git file or symlink pointing at a git dir the agent wrote
//     ("gitdir: ../somewhere"), e.g. under a forged submodule entry, which a
//     plain `git status` at the top level recurses into
//   - a bare repository (HEAD, config, objects/, refs/), which git uses as-is
//     when you cd into it
//
// None of these can be stopped with mounts, so zoo looks for them on the
// host, outside the cage's reach, and tells you before you trip one.

// maxTrapScan bounds the walk; past it the scan reports itself incomplete.
const maxTrapScan = 500_000

// Trap is something in a worktree the host's git could pick up as a
// repository the agent controls.
type Trap struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// GitTraps scans a worktree for planted repositories. Legitimate submodule
// .git files (pointing into the repo's own modules dirs, which cages can't
// write) pass. complete is false when the walk hit its bound.
func GitTraps(root string) (traps []Trap, complete bool) {
	root = filepath.Clean(root)
	common, gitDir := cachedGitPaths(root)
	var trusted []string // git dirs a submodule .git file may point into
	if common != "" {
		trusted = append(trusted, filepath.Join(common, "modules"))
	}
	if gitDir != "" && gitDir != common {
		trusted = append(trusted, filepath.Join(gitDir, "modules"))
	}
	n := 0
	complete = true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > maxTrapScan {
			complete = false
			return filepath.SkipAll
		}
		if d.Name() == ".git" {
			if filepath.Dir(p) == root {
				// The repo's own: guarded by mounts.
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				traps = append(traps, Trap{p, ".git symlink"})
			case d.IsDir():
				traps = append(traps, Trap{p, "nested repository"})
				return filepath.SkipDir
			default:
				if why := checkGitFile(p, trusted); why != "" {
					traps = append(traps, Trap{p, why})
				}
			}
			return nil
		}
		if d.IsDir() && p != root && looksBare(p) {
			traps = append(traps, Trap{p, "bare repository"})
			return filepath.SkipDir
		}
		return nil
	})
	sort.Slice(traps, func(i, j int) bool { return traps[i].Path < traps[j].Path })
	return traps, complete
}

// checkGitFile returns why a non-root .git file is a trap, or "" when it's
// an ordinary submodule pointer into a trusted modules dir.
func checkGitFile(p string, trusted []string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "" // not a gitdir pointer; git ignores it
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	target = filepath.Clean(target)
	for _, t := range trusted {
		if within(target, t) && target != t {
			return ""
		}
	}
	return "points git at " + target
}

// looksBare mirrors git's test for a bare repository: HEAD and config files
// with objects/ and refs/ dirs.
func looksBare(dir string) bool {
	for _, f := range []struct {
		name string
		dir  bool
	}{{"HEAD", false}, {"config", false}, {"objects", true}, {"refs", true}} {
		st, err := os.Lstat(filepath.Join(dir, f.name))
		if err != nil || st.IsDir() != f.dir {
			return false
		}
	}
	return true
}

// The daemon scans every running cage every 30 s; asking git for the same
// worktree's git dirs each time would start a git process per cage per scan
// (on Windows, each with a console host). They only change if the worktree
// is moved or re-linked, so a few minutes' cache is plenty.
var gitPathCache = struct {
	sync.Mutex
	m map[string]gitPathEntry
}{m: map[string]gitPathEntry{}}

type gitPathEntry struct {
	common, gitDir string
	at             time.Time
}

func cachedGitPaths(root string) (string, string) {
	gitPathCache.Lock()
	defer gitPathCache.Unlock()
	if e, ok := gitPathCache.m[root]; ok && time.Since(e.at) < 5*time.Minute {
		return e.common, e.gitDir
	}
	common, gitDir := gitPaths(root)
	gitPathCache.m[root] = gitPathEntry{common, gitDir, time.Now()}
	return common, gitDir
}

// GitDirs returns a worktree's common git dir and its own git dir (the same
// for a main checkout), cached like the trap scan's.
func GitDirs(root string) (common, gitDir string) {
	return cachedGitPaths(filepath.Clean(root))
}
