package cage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

// Plans create cache dirs under ZOO_HOME: keep every test in this package
// away from the real ~/.zoo.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zoo-cage-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ZOO_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestCacheMode(t *testing.T) {
	for _, c := range []struct{ global, project, want string }{
		{"", "", CacheProject},
		{"shared", "", CacheShared},
		{"shared", "off", CacheOff},
		{"off", "Project", CacheProject},
	} {
		got, err := CacheMode(config.Global{Cache: c.global}, config.Project{Cache: c.project})
		if err != nil || got != c.want {
			t.Errorf("global %q project %q: got %q %v, want %q", c.global, c.project, got, err, c.want)
		}
	}
	if _, err := CacheMode(config.Global{}, config.Project{Cache: "everywhere"}); err == nil {
		t.Error("unknown cache mode accepted")
	}
}

func cacheMounts(t *testing.T, mode string) (Plan, map[string]string, []string) {
	t.Helper()
	root := t.TempDir()
	pl, err := NewPlan(root, config.Defaults(), config.Project{Cache: mode}, "")
	if err != nil {
		t.Fatal(err)
	}
	spec := pl.Spec(config.Defaults(), "10.0.0.2", nil)
	var targets []string
	for _, m := range spec.Mounts {
		if m.Target == CacheTarget {
			targets = append(targets, m.Source)
			if m.ReadOnly {
				t.Errorf("cache mounted read-only")
			}
		}
	}
	return pl, spec.Labels, targets
}

func TestCachePlan(t *testing.T) {
	home := os.Getenv("ZOO_HOME")

	pl, labels, src := cacheMounts(t, "")
	want := filepath.Join(home, "cache", "project", pl.Name)
	if len(src) != 1 || src[0] != want || labels[LabelCache] != CacheProject {
		t.Fatalf("project cache: mounts %v labels %v, want %s", src, labels, want)
	}
	for _, sub := range []string{"npm", "pnpm-store", "pip", "uv", "go/mod", "go/build", "cargo", "bun", "yarn"} {
		if st, err := os.Stat(filepath.Join(want, sub)); err != nil || !st.IsDir() {
			t.Errorf("plan must create %s so the agent owns it", sub)
		}
	}
	// Windows has no Unix modes; the dir is private through its ACLs there.
	if st, _ := os.Stat(want); runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode %v, want 0700", st.Mode().Perm())
	}
	projectLayout := labels[LabelLayout]

	_, labels, src = cacheMounts(t, "shared")
	if len(src) != 1 || src[0] != filepath.Join(home, "cache", "shared") || labels[LabelCache] != CacheShared {
		t.Fatalf("shared cache: mounts %v labels %v", src, labels)
	}

	_, labels, src = cacheMounts(t, "off")
	if len(src) != 0 || labels[LabelCache] != "" {
		t.Fatalf("cache off still mounts: %v %v", src, labels)
	}
	if labels[LabelLayout] == projectLayout {
		t.Error("turning the cache on or off must change the layout, so stopped cages are recreated")
	}
}

func TestCacheEnv(t *testing.T) {
	if env := CacheEnv(map[string]string{}); env != nil {
		t.Fatalf("a cage without the mount gets no cache env: %v", env)
	}
	env := strings.Join(CacheEnv(map[string]string{LabelCache: CacheShared}), "\n")
	for _, want := range []string{
		"npm_config_cache=" + CacheTarget + "/npm",
		"npm_config_store_dir=" + CacheTarget + "/pnpm-store",
		"YARN_CACHE_FOLDER=", "PIP_CACHE_DIR=", "UV_CACHE_DIR=",
		"GOMODCACHE=" + CacheTarget + "/go/mod", "GOCACHE=" + CacheTarget + "/go/build",
		"CARGO_HOME=" + CacheTarget + "/cargo", "BUN_INSTALL_CACHE_DIR=",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
}

func TestRemoveCacheReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	mod := filepath.Join(dir, "go", "mod", "example.com", "x@v1")
	write(t, filepath.Join(mod, "x.go"), "package x")
	write(t, filepath.Join(dir, "npm", "blob"), "12345")
	if n, err := DirSize(dir); err != nil || n != int64(len("package x")+5) {
		t.Fatalf("size %d %v", n, err)
	}
	// Go makes its module cache read-only.
	for _, d := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveCache(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("cache still there")
	}
}
