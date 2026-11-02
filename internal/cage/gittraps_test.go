package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitTraps(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "repo")
	wt := filepath.Join(tmp, "repo-wt")
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

	for _, root := range []string{main, wt} {
		root, _ := Root(root)
		if traps, complete := GitTraps(root); len(traps) != 0 || !complete {
			t.Fatalf("clean %s: %v complete=%v", root, traps, complete)
		}
	}

	root, _ := Root(wt)
	common, gitDir := gitPaths(root)
	// Ordinary submodule pointers into the repo's (read-only) modules dirs pass.
	write(t, filepath.Join(root, "sub1", ".git"), "gitdir: "+filepath.Join(common, "modules", "sub1")+"\n")
	rel, _ := filepath.Rel(filepath.Join(root, "sub2"), filepath.Join(gitDir, "modules", "sub2"))
	write(t, filepath.Join(root, "sub2", ".git"), "gitdir: "+rel+"\n")
	// Not a gitdir pointer at all: git ignores it.
	write(t, filepath.Join(root, "notes", ".git"), "just a file\n")

	// Planted.
	_ = os.MkdirAll(filepath.Join(root, "tools", ".git"), 0o755)
	write(t, filepath.Join(root, "vendor", "x", ".git"), "gitdir: ../../.cache/gd\n")
	write(t, filepath.Join(root, "esc", ".git"), "gitdir: "+filepath.Join(common, "modules")+"\n") // the modules dir itself
	bare := filepath.Join(root, "docs", "deep", "bare")
	write(t, filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(bare, "config"), "")
	_ = os.MkdirAll(filepath.Join(bare, "objects"), 0o755)
	_ = os.MkdirAll(filepath.Join(bare, "refs"), 0o755)
	// Inside dirs the mask scan skips: nothing is skipped here.
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "pkg", ".git"), 0o755)
	link := filepath.Join(root, "ln", ".git")
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	hasLink := os.Symlink(filepath.Join(tmp, "elsewhere"), link) == nil

	traps, complete := GitTraps(root)
	if !complete {
		t.Fatal("scan incomplete")
	}
	got := map[string]string{}
	for _, tr := range traps {
		got[tr.Path] = tr.Reason
	}
	want := []string{
		filepath.Join(root, "tools", ".git"),
		filepath.Join(root, "vendor", "x", ".git"),
		filepath.Join(root, "esc", ".git"),
		bare,
		filepath.Join(root, "node_modules", "pkg", ".git"),
	}
	if hasLink {
		want = append(want, link)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missed %s; got %v", w, got)
		}
	}
	for _, ok := range []string{"sub1", "sub2", "notes"} {
		if r, bad := got[filepath.Join(root, ok, ".git")]; bad {
			t.Errorf("flagged legitimate %s: %s", ok, r)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d traps, want %d: %v", len(got), len(want), got)
	}
}
