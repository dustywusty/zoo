package sshagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGitHubHostKeys(t *testing.T) {
	// Published at docs.github.com ("GitHub's SSH key fingerprints").
	published := map[string]string{
		"ssh-rsa":             "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
		"ecdsa-sha2-nistp256": "SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
		"ssh-ed25519":         "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
	}
	rest := []byte(githubKnownHosts)
	seen := map[string]bool{}
	for len(strings.TrimSpace(string(rest))) > 0 {
		_, hosts, pk, _, r, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			t.Fatal(err)
		}
		rest = r
		fp := ssh.FingerprintSHA256(pk)
		if published[pk.Type()] != fp {
			t.Errorf("%s fingerprint %s, published %s", pk.Type(), fp, published[pk.Type()])
		}
		if !reflect.DeepEqual(hosts, []string{"github.com"}) {
			t.Errorf("hosts = %v", hosts)
		}
		seen[pk.Type()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("keys = %v", seen)
	}
	gh := GitHubHostKeys()
	for _, fp := range published {
		if got := gh.Hosts(fp); !reflect.DeepEqual(got, []string{"github.com"}) {
			t.Errorf("Hosts(%s) = %v", fp, got)
		}
	}
	if got := gh.Hosts("SHA256:nope"); got != nil {
		t.Errorf("unknown fp -> %v", got)
	}
}

func newPub(t *testing.T) (ssh.PublicKey, string) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	return k, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestKnownHostsFile(t *testing.T) {
	a, aLine := newPub(t)
	b, bLine := newPub(t)
	c, cLine := newPub(t)
	d, dLine := newPub(t)
	e, eLine := newPub(t)
	dir := t.TempDir()
	f1 := filepath.Join(dir, "known_hosts")
	f2 := filepath.Join(dir, "known_hosts2")
	os.WriteFile(f1, []byte(strings.Join([]string{
		"# comment",
		"",
		"Example.com,192.0.2.1 " + aLine,
		"[git.example.com]:2222 " + bLine,
		"|1|F1E1KeoE/eEWhi10WpGv4OdiO6Y=|3988QV0VE8wmZL7suNrYQLITLCg= " + cLine,
		"*.wild.example,!no.example " + cLine,
		"@cert-authority *.example.com " + dLine,
		"this line is garbage",
		"revoked.example " + eLine,
	}, "\n")), 0o600)
	os.WriteFile(f2, []byte("other.example "+aLine+"\n@revoked * "+eLine+"\n"), 0o600)

	k, err := KnownHostsFile(f1, f2, filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	fp := ssh.FingerprintSHA256
	for _, tc := range []struct {
		key  ssh.PublicKey
		want []string
	}{
		{a, []string{"example.com", "192.0.2.1", "other.example"}},
		{b, []string{"git.example.com"}},
		{c, nil}, // hashed / wildcard only
		{d, nil}, // cert authority
		{e, nil}, // revoked
	} {
		if got := k.Hosts(fp(tc.key)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Hosts = %v, want %v", got, tc.want)
		}
	}

	m := Multi(k, GitHubHostKeys(), Static(map[string][]string{fp(a): {"example.com", "extra"}}))
	if got := m.Hosts(fp(a)); !reflect.DeepEqual(got, []string{"example.com", "192.0.2.1", "other.example", "extra"}) {
		t.Errorf("Multi = %v", got)
	}
}

func TestResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-socket agent layouts (macOS, Linux); Windows: TestResolveWindowsPipe")
	}
	home := shortDir(t)
	oldHome, oldOS, oldEnv := userHomeDir, goos, getenv
	t.Cleanup(func() { userHomeDir, goos, getenv = oldHome, oldOS, oldEnv })
	userHomeDir = func() (string, error) { return home, nil }
	env := map[string]string{}
	getenv = func(k string) string { return env[k] }

	mkSock := func(p string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o700)
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
	}

	goos = "darwin"
	if _, err := Resolve("bitwarden"); err == nil || !strings.Contains(err.Error(), "SSH agent") {
		t.Fatalf("missing bitwarden: %v", err)
	}
	if _, err := Resolve("1password"); err == nil || !strings.Contains(err.Error(), "1Password") {
		t.Fatalf("missing 1password: %v", err)
	}
	// Sandboxed (App Store) Bitwarden.
	bw := filepath.Join(home, "Library/Containers/com.bitwarden.desktop/Data/.bitwarden-ssh-agent.sock")
	mkSock(bw)
	if got, err := Resolve("Bitwarden"); err != nil || got != bw {
		t.Fatalf("bitwarden = %q, %v", got, err)
	}
	// Direct-download Bitwarden takes precedence.
	bw2 := filepath.Join(home, ".bitwarden-ssh-agent.sock")
	mkSock(bw2)
	if got, _ := Resolve("bitwarden"); got != bw2 {
		t.Fatalf("bitwarden = %q", got)
	}

	// 1Password's real macOS path has a space and is longer than a unix
	// socket path allows under a temp home, so only check the error names it.
	if _, err := Resolve("1password"); err == nil || !strings.Contains(err.Error(), "Group Containers/2BUA8C4S2C.com.1password/t/agent.sock") {
		t.Fatalf("1password: %v", err)
	}
	goos = "linux"
	op := filepath.Join(home, ".1password/agent.sock")
	mkSock(op)
	if got, err := Resolve("1password"); err != nil || got != op {
		t.Fatalf("1password linux = %q, %v", got, err)
	}

	if _, err := Resolve("system"); err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("system unset: %v", err)
	}
	sys := filepath.Join(home, "sys.sock")
	mkSock(sys)
	env["SSH_AUTH_SOCK"] = sys
	if got, err := Resolve("system"); err != nil || got != sys {
		t.Fatalf("system = %q, %v", got, err)
	}

	if got, err := Resolve("~/sys.sock"); err != nil || got != sys {
		t.Fatalf("~ path = %q, %v", got, err)
	}
	if got, err := Resolve(sys); err != nil || got != sys {
		t.Fatalf("abs path = %q, %v", got, err)
	}
	regular := filepath.Join(home, "file")
	os.WriteFile(regular, nil, 0o600)
	for _, bad := range []string{regular, "relative.sock", "keepassxc", ""} {
		if _, err := Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) succeeded", bad)
		} else {
			t.Log(fmt.Sprint(err))
		}
	}
}
