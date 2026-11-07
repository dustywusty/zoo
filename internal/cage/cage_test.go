package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindMasks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".env"), "SECRET=1")
	write(t, filepath.Join(root, ".env.local"), "SECRET=1")
	write(t, filepath.Join(root, ".env.example"), "SECRET=")
	write(t, filepath.Join(root, "apps/api/.env"), "SECRET=1")
	write(t, filepath.Join(root, "certs/server.key"), "k")
	write(t, filepath.Join(root, "node_modules/x/.env"), "nope")
	write(t, filepath.Join(root, "README.md"), "hi")
	g := config.Defaults()
	got := FindMasks(root, g.Masks, g.Unmask)
	rel := []string{}
	for _, p := range got {
		r, _ := filepath.Rel(root, p)
		rel = append(rel, r)
	}
	joined := strings.Join(rel, ",")
	for _, want := range []string{".env", ".env.local", "apps/api/.env", "certs/server.key"} {
		if !strings.Contains(","+joined+",", ","+want+",") {
			t.Errorf("missing %s in %s", want, joined)
		}
	}
	if strings.Contains(joined, "example") || strings.Contains(joined, "node_modules") {
		t.Errorf("over-masked: %s", joined)
	}
}

func TestPlanWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base := t.TempDir()
	main := filepath.Join(base, "repo")
	wt := filepath.Join(base, "repo-feature")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_ = os.MkdirAll(main, 0o755)
	run(main, "init", "-q")
	write(t, filepath.Join(main, "a.txt"), "a")
	run(main, "add", ".")
	run(main, "commit", "-qm", "init")
	run(main, "worktree", "add", "-q", wt)
	write(t, filepath.Join(wt, ".env"), "SECRET=1")

	root, _ := Root(filepath.Join(wt))
	g := config.Defaults()
	pl, err := NewPlan(root, g, config.Project{Size: "medium"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if pl.SizeName != "medium" || pl.Size.CPUs != 4 {
		t.Fatalf("size: %+v", pl)
	}
	if len(pl.ExtraRW) != 1 || !strings.HasSuffix(pl.ExtraRW[0], filepath.Join("repo", ".git")) {
		t.Fatalf("expected main .git mounted rw: %v", pl.ExtraRW)
	}
	ro := strings.Join(pl.ReadOnly, ",")
	if !strings.Contains(ro, filepath.Join("repo", ".git", "hooks")) || !strings.Contains(ro, filepath.Join("repo", ".git", "config")) {
		t.Fatalf("git guard paths: %v", pl.ReadOnly)
	}
	if len(pl.Masks) != 1 {
		t.Fatalf("masks: %v", pl.Masks)
	}
	common := pl.ExtraRW[0]
	gitDir := filepath.Join(common, "worktrees", filepath.Base(wt))
	for _, want := range []string{filepath.Join(root, ".git"), filepath.Join(gitDir, "commondir")} {
		if !strings.Contains(ro, want) {
			t.Errorf("linked worktree pointer %s must be read-only: %v", want, pl.ReadOnly)
		}
	}
	// Every other worktree's gitdir and every submodule's git dir is
	// read-only: rewriting another worktree's commondir points the host's
	// git at a config the agent wrote.
	roSet := map[string]bool{}
	for _, p := range pl.ReadOnly {
		roSet[p] = true
	}
	for _, want := range []string{
		filepath.Join(common, "worktrees"), filepath.Join(common, "modules"),
		filepath.Join(gitDir, "modules"), filepath.Join(gitDir, "config.worktree"),
	} {
		if !roSet[want] {
			t.Errorf("%s must be read-only: %v", want, pl.ReadOnly)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("plan must create %s so it can be mounted read-only (else the agent creates it): %v", want, err)
		}
	}
	// ... except this worktree's own gitdir (HEAD, index), reopened rw.
	if len(pl.ReopenRW) != 1 || pl.ReopenRW[0] != gitDir {
		t.Errorf("own gitdir must be reopened rw: %v", pl.ReopenRW)
	}
	// Those are mount points already, so they can't be renamed; pinning them
	// rw as well would undo the read-only mount.
	for _, p := range pl.Pin {
		if roSet[p] || p == gitDir {
			t.Errorf("%s is both pinned rw and read-only/reopened", p)
		}
	}
	if spec := pl.Spec(g, "", nil); len(spec.ReopenRW) != 1 {
		t.Errorf("spec drops ReopenRW: %v", spec.ReopenRW)
	}
	spec := pl.Spec(g, "192.168.64.2", map[string]string{"zoo.backend": "fake"})
	if spec.CPUs != 4 || spec.Memory != "4G" || spec.Pids != 2048 || spec.Labels["zoo.backend"] != "fake" || spec.Labels[LabelWorktree] != root {
		t.Errorf("spec: %+v", spec)
	}
	// worktree, main .git, package cache
	if len(spec.Mounts) != 3 || spec.Mounts[2].Target != CacheTarget || spec.Mounts[0].Source != root || spec.Mounts[0].Target != root || spec.Mounts[0].ReadOnly {
		t.Errorf("mounts: %+v", spec.Mounts)
	}
	if !strings.Contains(strings.Join(spec.Env, " "), "HTTPS_PROXY=http://192.168.64.2:3128") {
		t.Errorf("env: %v", spec.Env)
	}
	if NameFor(root) != NameFor(root) || !strings.HasPrefix(NameFor(root), "repo-feature-") {
		t.Errorf("name: %s", NameFor(root))
	}
}

func TestPlanMainCheckoutPinsGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	_ = os.RemoveAll(filepath.Join(root, ".git", "hooks"))
	root, _ = Root(root)
	pl, err := NewPlan(root, config.Defaults(), config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.ExtraRW) != 0 {
		t.Errorf("main checkout needs no extra mounts: %v", pl.ExtraRW)
	}
	if len(pl.Pin) != 1 || pl.Pin[0] != filepath.Join(root, ".git") {
		t.Errorf("want .git pinned (else `mv .git .git2` escapes the read-only mounts): %v", pl.Pin)
	}
	for _, d := range []string{"hooks", "worktrees", "modules"} {
		p := filepath.Join(root, ".git", d)
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			t.Errorf("plan must create a missing .git/%s so the agent can't", d)
		}
		if !strings.Contains(","+strings.Join(pl.ReadOnly, ",")+",", ","+p+",") {
			t.Errorf(".git/%s must be read-only: %v", d, pl.ReadOnly)
		}
	}
}
