package gitrefs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReadMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, repo, "tag", "-a", "v1", "-m", "v1")
	git(t, repo, "branch", "old")
	git(t, repo, "pack-refs", "--all") // main, old, v1 now packed
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "two")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")

	refs, err := Read(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(git(t, repo, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
		name, id, _ := strings.Cut(line, " ")
		if refs[name] != id {
			t.Errorf("%s: read %q, git says %q", name, refs[name], id)
		}
	}
	if refs["HEAD"] != "ref: refs/heads/main" {
		t.Errorf("HEAD = %q", refs["HEAD"])
	}
	if Own(filepath.Join(repo, ".git")) != "refs/heads/main" {
		t.Errorf("own = %q", Own(filepath.Join(repo, ".git")))
	}
}

func TestDiff(t *testing.T) {
	before := Refs{
		"HEAD":                     "ref: refs/heads/main",
		"refs/heads/main":          "aaa",
		"refs/heads/feature":       "bbb", // the cage's own branch
		"refs/heads/release":       "ccc",
		"refs/remotes/origin/main": "aaa",
		"refs/tags/v1":             "ddd",
		"refs/stash":               "eee",
	}
	after := Refs{
		"HEAD":                     "ref: refs/heads/release", // main checkout switched
		"refs/heads/main":          "fff",                     // moved
		"refs/heads/feature":       "999",                     // own: fine
		"refs/remotes/origin/main": "fff",                     // forged "pushed"
		"refs/tags/v1":             "ddd",
		"refs/tags/v2":             "fff", // new tag
		"refs/heads/scratch":       "999", // new local branch: a note
		"refs/stash":               "123", // git's own bookkeeping
		// refs/heads/release deleted
	}
	got := map[string]Change{}
	for _, c := range Diff(before, after, "refs/heads/feature", false) {
		got[c.Ref] = c
	}
	want := map[string]bool{ // ref -> alarm
		"HEAD": true, "refs/heads/main": true, "refs/remotes/origin/main": true,
		"refs/tags/v2": true, "refs/heads/release": true, "refs/heads/scratch": false,
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	for ref, alarm := range want {
		c, ok := got[ref]
		if !ok || c.Alarm != alarm {
			t.Errorf("%s: %+v (present %v), want alarm=%v", ref, c, ok, alarm)
		}
	}
	if u := got["refs/heads/main"].Undo(); u != "git update-ref refs/heads/main aaa" {
		t.Errorf("undo: %s", u)
	}
	if u := got["refs/heads/release"].Undo(); u != "git update-ref refs/heads/release ccc" {
		t.Errorf("undo delete: %s", u)
	}
	if u := got["refs/tags/v2"].Undo(); u != "git update-ref -d refs/tags/v2" {
		t.Errorf("undo create: %s", u)
	}
	if u := got["HEAD"].Undo(); u != "git symbolic-ref HEAD refs/heads/main" {
		t.Errorf("undo head: %s", u)
	}
	// In the main checkout, HEAD is the cage's own.
	for _, c := range Diff(before, after, "refs/heads/feature", true) {
		if c.Ref == "HEAD" {
			t.Error("main checkout's own HEAD reported")
		}
	}
}

func TestSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x", "snap.json")
	if _, ok := Load(p); ok {
		t.Fatal("loaded nothing")
	}
	s := Snapshot{Common: "/r/.git", Own: "refs/heads/f", Refs: Refs{"refs/heads/f": "abc"}}
	if err := Save(p, s); err != nil {
		t.Fatal(err)
	}
	if got, ok := Load(p); !ok || got.Own != s.Own || got.Refs["refs/heads/f"] != "abc" {
		t.Fatalf("%+v %v", got, ok)
	}
}

// A session's unreviewed alarms survive the next session start instead of
// being absorbed into its baseline; accepting them (Rebaseline) clears them.
func TestBeginKeepsUnreviewedAlarms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, repo, "branch", "release")
	gd := filepath.Join(repo, ".git")
	snap := filepath.Join(t.TempDir(), "s.json")

	if pending, err := Begin(snap, gd, gd); err != nil || len(pending) != 0 {
		t.Fatalf("first begin: %v %v", pending, err)
	}
	// The "agent" commits on its own branch (fine) and moves release (not).
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "two")
	git(t, repo, "update-ref", "refs/heads/release", "HEAD")
	s, _ := Load(snap)
	if a := Alarms(Check(s, gd, gd)); len(a) != 1 || a[0].Ref != "refs/heads/release" {
		t.Fatalf("alarms: %+v", a)
	}
	// Next session: still reported, baseline kept.
	if pending, _ := Begin(snap, gd, gd); len(pending) != 1 {
		t.Fatalf("pending after next begin: %+v", pending)
	}
	if err := Rebaseline(snap, gd, gd); err != nil {
		t.Fatal(err)
	}
	if pending, _ := Begin(snap, gd, gd); len(pending) != 0 {
		t.Fatalf("after accepting: %+v", pending)
	}
}
