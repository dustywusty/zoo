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
	args := strings.Join(pl.RunArgs(g, "192.168.64.2"), " ")
	for _, want := range []string{"--network zoo-cages", "--no-dns", "-c 4", "-m 4G", "HTTPS_PROXY=http://192.168.64.2:3128", "-v " + root + ":" + root, "zoo-cage:latest"} {
		if !strings.Contains(args, want) {
			t.Errorf("run args missing %q:\n%s", want, args)
		}
	}
	if NameFor(root) != NameFor(root) || !strings.HasPrefix(NameFor(root), "repo-feature-") {
		t.Errorf("name: %s", NameFor(root))
	}
}
