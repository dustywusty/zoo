package cage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
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
		rel = append(rel, filepath.ToSlash(r))
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
	t.Setenv("ZOO_HOME", t.TempDir()) // guest copies are staged under it
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
	guarded := strings.Join(pl.Guarded(), ",")
	for _, want := range []string{filepath.Join(root, ".git"), filepath.Join(gitDir, "commondir")} {
		if !strings.Contains(guarded, want) {
			t.Errorf("linked worktree pointer %s must be read-only: %v", want, pl.Guarded())
		}
	}
	// Where the guest spells paths differently (Windows), the cage sees a
	// rewritten read-only copy of the .git pointer instead of the host file.
	dotgit := filepath.Join(root, ".git")
	host, _ := os.ReadFile(dotgit)
	_, needs := guestPointer(string(host))
	var staged *backend.Mount
	for i := range pl.Files {
		if pl.Files[i].Target == dotgit {
			staged = &pl.Files[i]
		}
	}
	switch {
	case needs && staged == nil:
		t.Errorf(".git holds a host-only path (%q) but no guest copy is mounted: %+v", host, pl.Files)
	case !needs && staged != nil:
		t.Errorf("guest copy of .git staged though paths match: %+v", staged)
	case staged != nil:
		b, _ := os.ReadFile(staged.Source)
		want := "gitdir: " + backend.GuestPath(gitDir)
		if !strings.HasPrefix(string(b), want) || !staged.ReadOnly || strings.Contains(ro, dotgit) {
			t.Errorf("guest .git = %q (ro=%v), want %q, mounted only once", b, staged.ReadOnly, want)
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
	// Where the guest sees host files as another owner (Windows), git in the
	// cage is told to trust the worktree; on Unix it needs nothing.
	env := strings.Join(spec.Env, " ")
	if trusts := strings.Contains(env, "GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=*"); trusts != (backend.GuestPath(root) != root) {
		t.Errorf("safe.directory in env = %v for root %s: %v", trusts, root, spec.Env)
	}
	if pl.AutoCRLF != "" && !strings.Contains(env, "GIT_CONFIG_KEY_1=core.autocrlf GIT_CONFIG_VALUE_1="+pl.AutoCRLF) {
		t.Errorf("host core.autocrlf=%s not mirrored: %v", pl.AutoCRLF, spec.Env)
	}
	if len(spec.Mounts) != 2 || spec.Mounts[0].Source != root || spec.Mounts[0].Target != root || spec.Mounts[0].ReadOnly {
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

func TestGuestPointer(t *testing.T) {
	for in, want := range map[string]string{
		"gitdir: C:/Users/me/repo/.git/worktrees/wt\n":   "gitdir: /c/Users/me/repo/.git/worktrees/wt\n",
		"gitdir: C:/Users/me/repo/.git/worktrees/wt\r\n": "gitdir: /c/Users/me/repo/.git/worktrees/wt\n",
		`D:\src\repo\.git` + "\n":                        "/d/src/repo/.git\n",
	} {
		got, ok := guestPointer(in)
		if !ok || got != want {
			t.Errorf("guestPointer(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, same := range []string{"gitdir: /home/me/repo/.git/worktrees/wt\n", "../..\n", "gitdir: ../.git/modules/x\n"} {
		if got, ok := guestPointer(same); ok || got != same {
			t.Errorf("guestPointer(%q) changed it to %q", same, got)
		}
	}
}

func TestMaskGaps(t *testing.T) {
	root := t.TempDir()
	secret := []string{
		"apps/web/.next/standalone/.env", // build output copying secrets
		"dist/.env.production",           // build output
		"a/b/c/d/e/f/g/h/.env",           // deeper than the old limit of 6
		"infra/prod.tfvars",              // terraform
		"infra/terraform.tfstate",        //
		"deploy/service-account-ci.json", // cloud credentials
		"ops/.kube/config",               // path pattern, any depth
		".envrc",                         // direnv
		"worker/.dev.vars",               // wrangler
		"charts/secrets.yaml",            //
		"certs/server.pfx",               //
	}
	public := []string{
		"infra/prod.tfvars.example", ".env.example", "node_modules/pkg/.env", "keys/id_ed25519.pub",
		"docs/kube/config", // not .kube
		"README.md",
	}
	for _, p := range append(append([]string{}, secret...), public...) {
		write(t, filepath.Join(root, filepath.FromSlash(p)), "x")
	}
	g := config.Defaults()
	got := map[string]bool{}
	for _, p := range FindMasks(root, g.Masks, g.Unmask) {
		r, _ := filepath.Rel(root, p)
		got[filepath.ToSlash(r)] = true
	}
	for _, p := range secret {
		if !got[p] {
			t.Errorf("not masked: %s", p)
		}
	}
	for _, p := range public {
		if got[p] {
			t.Errorf("masked but not secret: %s", p)
		}
	}
}

func TestMaskPathPatterns(t *testing.T) {
	for _, c := range []struct {
		pat, rel string
		want     bool
	}{
		{".kube/config", ".kube/config", true},
		{".kube/config", "a/b/.kube/config", true},
		{"/.kube/config", "a/.kube/config", false}, // anchored
		{"/.kube/config", ".kube/config", true},
		{"config/*.secret.json", "svc/config/db.secret.json", true},
		{"config/*.secret.json", "config/db.json", false},
	} {
		if got := matchRel([]string{c.pat}, filepath.FromSlash(c.rel)); got != c.want {
			t.Errorf("matchRel(%q, %q) = %v", c.pat, c.rel, got)
		}
	}
}

// Past MaxMasks the plan fails rather than hiding only some secrets.
func TestTooManyMasksFailsClosed(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= MaxMasks; i++ {
		write(t, filepath.Join(root, "fixtures", fmt.Sprintf("k%04d.key", i)), "k")
	}
	if _, complete := FindMasksAll(root, []string{"*.key"}, nil); complete {
		t.Fatal("scan claims complete")
	}
	if _, err := NewPlan(root, config.Defaults(), config.Project{}, ""); err == nil || !strings.Contains(err.Error(), "unmask") {
		t.Fatalf("plan with too many secrets: %v", err)
	}
	if _, err := NewPlan(root, config.Defaults(), config.Project{Unmask: []string{"k*.key"}}, ""); err != nil {
		t.Fatalf("unmasked fixtures should pass: %v", err)
	}
}

// A secret file created after the cage makes it stale, so the next start
// recreates it with the new mask.
func TestNewSecretMakesCageStale(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	g := config.Defaults()
	pl, err := NewPlan(root, g, config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	spec := pl.Spec(g, "", nil)
	running := backend.Status{Labels: spec.Labels}
	if Stale(running, spec) {
		t.Fatal("fresh cage stale")
	}
	write(t, filepath.Join(root, ".env"), "SECRET=1")
	pl2, _ := NewPlan(root, g, config.Project{}, "")
	if !Stale(running, pl2.Spec(g, "", nil)) {
		t.Fatal("new .env didn't make the cage stale")
	}
	// A cage from before fingerprints is stale only if there's something to hide.
	old := backend.Status{Labels: map[string]string{LabelImage: g.CageImage}}
	if !Stale(old, pl2.Spec(g, "", nil)) || Stale(old, spec) {
		t.Error("pre-fingerprint cages")
	}
}
