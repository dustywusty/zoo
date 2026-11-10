package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

func write(t *testing.T, root, path, value string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixture(t *testing.T) (Task, string) {
	t.Helper()
	t.Setenv("ZOO_HOME", t.TempDir())
	source := t.TempDir()
	write(t, source, "file.txt", "original\n")
	write(t, source, "unrelated.txt", "unchanged\n")
	x, err := Create("fix-bug", source, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	return x, source
}

func TestCopyDiffApplyAndResume(t *testing.T) {
	x, source := fixture(t)
	write(t, x.Workdir(), "file.txt", "agent\n")
	write(t, x.Workdir(), "new file.txt", "new\n")
	write(t, x.Workdir(), "binary", "\x00\xff\x01")
	write(t, source, "unrelated.txt", "host edit\n")
	if read(t, source, "file.txt") != "original\n" {
		t.Fatal("copy changed source")
	}
	patch, err := x.Diff(false)
	if err != nil || !strings.Contains(string(patch), "+agent") {
		t.Fatalf("diff: %s %v", patch, err)
	}
	if err := x.Destroy(false); err == nil {
		t.Fatal("destroy discarded unreviewed work")
	}
	x, err = Load(x.Name)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := x.Apply(true); err != nil || n != 3 {
		t.Fatalf("dry run: %d %v", n, err)
	}
	if read(t, source, "file.txt") != "original\n" {
		t.Fatal("dry run wrote files")
	}
	if n, err := x.Apply(false); err != nil || n != 3 {
		t.Fatalf("apply: %d %v", n, err)
	}
	if read(t, source, "file.txt") != "agent\n" || read(t, source, "unrelated.txt") != "host edit\n" || read(t, source, "binary") != "\x00\xff\x01" {
		t.Fatal("incorrect apply")
	}
	if p, err := x.Diff(false); err != nil || len(p) != 0 {
		t.Fatalf("baseline not advanced: %s %v", p, err)
	}
	write(t, x.Workdir(), "file.txt", "second change\n")
	if n, err := x.Apply(false); err != nil || n != 1 {
		t.Fatalf("incremental apply: %d %v", n, err)
	}
	if err := x.Destroy(false); err != nil {
		t.Fatal(err)
	}
}

func TestConflictDoesNotPartiallyApply(t *testing.T) {
	x, source := fixture(t)
	write(t, x.Workdir(), "file.txt", "agent\n")
	write(t, x.Workdir(), "new.txt", "new\n")
	write(t, source, "file.txt", "host\n")
	if _, err := x.Apply(false); err == nil || !strings.Contains(err.Error(), "host files changed") {
		t.Fatalf("conflict: %v", err)
	}
	if read(t, source, "file.txt") != "host\n" {
		t.Fatal("overwrote host edit")
	}
	if _, err := os.Stat(filepath.Join(source, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("partially applied patch")
	}
}

func TestCopyExcludesSecretsAndGitHistory(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	source := t.TempDir()
	if _, err := cleanGit(source, nil, "init", "--template=", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	write(t, source, ".gitignore", "ignored\n")
	write(t, source, "ignored", "ignored")
	write(t, source, ".env", "test-only-secret")
	write(t, source, "tracked.txt", "tracked")
	if _, err := cleanGit(source, nil, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanGit(source, nil, "commit", "-m", "source history"); err != nil {
		t.Fatal(err)
	}
	write(t, source, "untracked.txt", "untracked")
	x, err := Create("copy", source, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"ignored", ".env"} {
		if _, err := os.Stat(filepath.Join(x.Workdir(), p)); !os.IsNotExist(err) {
			t.Errorf("copied %s", p)
		}
	}
	if read(t, x.Workdir(), "untracked.txt") != "untracked" {
		t.Fatal("missing untracked file")
	}
	log, err := cleanGit(x.Workdir(), nil, "log", "--format=%s")
	if err != nil || strings.Contains(string(log), "source history") {
		t.Fatalf("history was shared: %s %v", log, err)
	}
	// Malicious agent hooks/config must never run during host review/apply.
	write(t, x.Workdir(), ".git/config", "this is not valid git config\n")
	write(t, x.Workdir(), "tracked.txt", "edited")
	if _, err := x.Apply(false); err != nil {
		t.Fatal(err)
	}
}

func TestDeletionAndExecutableMode(t *testing.T) {
	x, source := fixture(t)
	if err := os.Remove(filepath.Join(x.Workdir(), "file.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, x.Workdir(), "script", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(x.Workdir(), "script"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Apply(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "file.txt")); !os.IsNotExist(err) {
		t.Fatal("deletion missing")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(source, "script"))
		if st.Mode()&0o111 == 0 {
			t.Fatal("executable mode lost")
		}
	}
}

func TestUnsafePathsAndSymlinks(t *testing.T) {
	x, source := fixture(t)
	for _, p := range []string{"../escape", ".git/config", "nested/.Git/hooks/x", "x/.git /config", "C:/escape", "git~1/config"} {
		if !blockedPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	outside := t.TempDir()
	write(t, outside, "private", "private")
	if err := os.Symlink(outside, filepath.Join(x.Workdir(), "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := x.Diff(false); err == nil {
		t.Fatal("accepted symlink outside copy")
	}
	if err := os.Remove(filepath.Join(x.Workdir(), "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(x.Workdir(), "link")); err != nil {
		t.Fatal(err)
	}
	// Git for Windows defaults to core.symlinks=false. The host review repo
	// must not turn an agent symlink into a file containing its target name.
	if _, err := x.git("", nil, "config", "core.symlinks", "false"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Apply(false); err != nil {
		t.Fatal(err)
	}
	if read(t, source, "link") != "original\n" {
		t.Fatal("relative symlink not applied")
	}
}

func TestLockAndInvalidNames(t *testing.T) {
	x, source := fixture(t)
	unlock, err := x.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Lock(); err == nil {
		t.Fatal("concurrent task access allowed")
	}
	unlock()
	for _, name := range []string{"../other", "/tmp/x", "a/b", "-bad", ""} {
		if _, err := Create(name, source, config.Defaults()); err == nil {
			t.Errorf("accepted name %q", name)
		}
	}
	if _, err := Create(x.Name, source, config.Defaults()); err == nil {
		t.Fatal("overwrote existing task")
	}
}

func TestInterruptedApplyRecovery(t *testing.T) {
	x, source := fixture(t)
	old := x.Baseline
	write(t, x.Workdir(), "file.txt", "applied before disconnect\n")
	if _, err := x.Apply(false); err != nil {
		t.Fatal(err)
	}
	next := x.Baseline
	// Simulate a crash after applying files but before saving the baseline.
	x.Baseline = old
	if err := x.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pendingApply{Old: old, New: next})
	if err := os.WriteFile(filepath.Join(x.Dir(), "pending-apply.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := x.Apply(true); err != nil || n != 1 || x.Baseline != old {
		t.Fatalf("recovery dry run: %d %v", n, err)
	}
	if n, err := x.Apply(false); err != nil || n != 1 || x.Baseline != next {
		t.Fatalf("recovery: %d %v", n, err)
	}
	if read(t, source, "file.txt") != "applied before disconnect\n" {
		t.Fatal("recovery changed host content")
	}
}

func TestGitSubdirectoryHonorsIgnore(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	if _, err := cleanGit(root, nil, "init", "--template="); err != nil {
		t.Fatal(err)
	}
	write(t, root, ".gitignore", "private/\n")
	write(t, root, "sub/public", "public")
	write(t, root, "sub/private/token", "secret")
	x, err := Create("subdir", filepath.Join(root, "sub"), config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.Workdir(), "private/token")); !os.IsNotExist(err) {
		t.Fatal("ignored file copied from subdirectory")
	}
	if read(t, x.Workdir(), "public") != "public" {
		t.Fatal("missing subdirectory file")
	}
}

func TestTaskInheritsOnlyApprovedPolicy(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "zoo.yaml")
	if err := os.WriteFile(path, []byte("network:\n  allow: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Trust(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("network:\n  allow: [evil.example]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task, err := Create("policy", root, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	p, err := config.LoadProject(task.Workdir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Network.Allow == nil || len(p.Network.Allow) != 0 {
		t.Fatalf("lost approved deny-all policy: %+v", p.Network)
	}
	st, err := config.CheckTrust(task.Workdir())
	if err != nil || !st.Changed {
		t.Fatalf("agent proposal silently approved: %+v %v", st, err)
	}
}

func TestCopyPreservesSymlink(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	source := t.TempDir()
	write(t, source, "file.txt", "original\n")
	if err := os.Symlink("file.txt", filepath.Join(source, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	x, err := Create("links", source, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(x.Workdir(), "link")); err != nil || target != "file.txt" {
		t.Fatalf("symlink converted on copy: %q %v", target, err)
	}
	setting, err := cleanGit(x.Workdir(), nil, "config", "--local", "core.symlinks")
	if err != nil || strings.TrimSpace(string(setting)) != "true" {
		t.Fatalf("guest symlink setting: %q %v", setting, err)
	}
	if diff, err := x.Diff(false); err != nil || len(diff) != 0 {
		t.Fatalf("copy differs from baseline: %s %v", diff, err)
	}
}
