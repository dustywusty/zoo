package cage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// Package caches. Without one every cage downloads every dependency again,
// and `zoo reset` throws that away. A cache is a host directory zoo owns
// under ~/.zoo/cache, mounted rw at CacheTarget, never a host tool's own
// cache (I5). Per project by default, so one cage can't poison the packages
// another installs; "shared" trades that for disk space.
const (
	CacheProject = "project" // ~/.zoo/cache/project/<cage>, only this cage
	CacheShared  = "shared"  // ~/.zoo/cache/shared, every cage that picks it
	CacheOff     = "off"

	// CacheTarget is where a cache is mounted in the cage.
	CacheTarget = "/home/agent/.zoo-cache"
	// LabelCache records the cache a cage was created with; session env
	// points tools at it only when the cage has the mount.
	LabelCache = "zoo.cache"
)

// cacheDirs are the tools pointed at the cache, and where in it.
var cacheDirs = []struct{ env, sub string }{
	{"npm_config_cache", "npm"},
	{"npm_config_store_dir", "pnpm-store"},
	{"YARN_CACHE_FOLDER", "yarn"},
	{"PIP_CACHE_DIR", "pip"},
	{"UV_CACHE_DIR", "uv"},
	{"GOMODCACHE", "go/mod"},
	{"GOCACHE", "go/build"},
	{"CARGO_HOME", "cargo"}, // cargo keeps installed binaries here too (cargo/bin)
	{"BUN_INSTALL_CACHE_DIR", "bun"},
}

// CacheMode resolves zoo.yaml's cache: over the global default.
func CacheMode(g config.Global, p config.Project) (string, error) {
	m := strings.ToLower(strings.TrimSpace(p.Cache))
	if m == "" {
		m = strings.ToLower(strings.TrimSpace(g.Cache))
	}
	switch m {
	case "", CacheProject:
		return CacheProject, nil
	case CacheShared, CacheOff:
		return m, nil
	}
	return "", fmt.Errorf("cache: %q isn't a mode (project, shared, off)", m)
}

// CacheDir is the host directory for a cage's cache, or "" for off.
func CacheDir(mode, cageName string) string {
	switch mode {
	case CacheProject:
		return paths.CacheProject(cageName)
	case CacheShared:
		return paths.CacheShared()
	}
	return ""
}

// EnsureCacheDir creates a cache directory and the tools' subdirectories.
// Zoo creates them as you, and the agent has your uid (Docker on Linux,
// Docker Desktop, OrbStack) or sees virtiofs files as writable (Apple), so
// the agent owns them; creating the subdirs up front means no tool has to
// create anything under a mount point the runtime made as root.
func EnsureCacheDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	for _, d := range cacheDirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d.sub)), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// CacheEnv is the session env for a cage with the given labels: nothing
// unless the cage was created with a cache mount.
func CacheEnv(labels map[string]string) []string {
	switch labels[LabelCache] {
	case CacheProject, CacheShared:
	default:
		return nil
	}
	out := make([]string, 0, len(cacheDirs))
	for _, d := range cacheDirs {
		out = append(out, d.env+"="+CacheTarget+"/"+d.sub)
	}
	return out
}

// cacheMount is the plan's cache mount, if any.
func (pl Plan) cacheMount() []backend.Mount {
	if pl.CacheDir == "" {
		return nil
	}
	return []backend.Mount{{Source: pl.CacheDir, Target: CacheTarget}}
}

// DirSize sums the sizes of regular files under dir.
func DirSize(dir string) (int64, error) {
	var n int64
	err := filepath.Walk(dir, func(_ string, info fs.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n, err
}

// RemoveCache deletes a cache directory. Go's module cache is read-only on
// purpose, so directories are made writable first.
func RemoveCache(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.Walk(dir, func(p string, info fs.FileInfo, err error) error {
		if err == nil && info.IsDir() && info.Mode().Perm()&0o700 != 0o700 {
			_ = os.Chmod(p, info.Mode().Perm()|0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
