package main

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInjectRealGitPush runs real `git push` through the injector to a real
// git http-backend, so the push policy is checked against git's own wire
// format rather than hand-built pkt-lines.
func TestInjectRealGitPush(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	repos := filepath.Join(root, "srv")
	_ = os.MkdirAll(filepath.Join(repos, "you"), 0o755)
	run(root, "init", "-q", "--bare", filepath.Join(repos, "you", "app.git"))
	run(filepath.Join(repos, "you", "app.git"), "config", "http.receivepack", "true")

	// Upstream: git http-backend, which also checks the token arrived.
	var sawAuth []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		if a == "" {
			// Like GitHub: ask for credentials, so git runs its helper.
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "Basic "))
		sawAuth = append(sawAuth, string(dec))
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + repos, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x"}}).ServeHTTP(w, r)
	})
	h := newHarnessWith(t, []injectRule{{Name: "github", Hosts: []string{"git.example.com"}, Push: []string{"agent/*"}, Repos: []string{"you/app"}}},
		map[string]string{ph: "ghp_REAL"}, upstream)

	work := filepath.Join(root, "work")
	run(root, "init", "-q", work)
	_ = os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	run(work, "add", "f")
	if out := run(work, "commit", "-qm", "c"); strings.Contains(out, "fatal") {
		t.Fatalf("commit: %s", out)
	}
	push := func(ref string) string {
		cmd := exec.Command(gitBin, "-c", "http.proxy=http://"+h.addr, "-c", "http.sslCAInfo="+h.caFile,
			"-c", "credential.helper=!f() { echo username=x-access-token; echo password="+ph+"; }; f",
			"push", "https://git.example.com/you/app.git", "HEAD:"+ref)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		return string(out) + fmtErr(err)
	}

	out := push("refs/heads/main")
	if !strings.Contains(out, "[remote rejected] HEAD -> main (zoo keeper: push to refs/heads/main isn't allowed") {
		t.Fatalf("push to main through the keeper:\n%s", out)
	}
	if got := run(filepath.Join(repos, "you", "app.git"), "for-each-ref"); got != "" {
		t.Fatalf("main was created upstream: %s", got)
	}
	out = push("refs/heads/agent/fix")
	if got := run(filepath.Join(repos, "you", "app.git"), "for-each-ref", "--format=%(refname)"); strings.TrimSpace(got) != "refs/heads/agent/fix" {
		t.Fatalf("push to agent/fix didn't land (refs %q):\n%s", got, out)
	}
	for _, a := range sawAuth {
		if a != "x-access-token:ghp_REAL" {
			t.Fatalf("upstream saw credentials %q, want the real token", a)
		}
	}
	if len(sawAuth) == 0 {
		t.Fatal("upstream never saw credentials")
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return "\n(" + err.Error() + ")"
}
