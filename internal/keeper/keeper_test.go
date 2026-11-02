package keeper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/paths"
)

func TestParseLine(t *testing.T) {
	e, ok := ParseLine("1761636000.123 192.168.64.5 CONNECT example.com:443 403 TCP_DENIED 3812 214 0")
	if !ok || !e.Denied || e.Host != "example.com" || e.Client != "192.168.64.5" || e.Status != 403 {
		t.Fatalf("bad parse: %+v", e)
	}
	e, ok = ParseLine("1761636000.5 192.168.64.5 GET http://deb.debian.org/debian/x.deb 200 TCP_MISS 1000 300 12")
	if !ok || e.Denied || e.Host != "deb.debian.org" || e.BytesIn != 1000 || e.BytesOut != 300 {
		t.Fatalf("bad parse: %+v", e)
	}
	if _, ok := ParseLine("garbage"); ok {
		t.Fatal("parsed garbage")
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"https://Api.GitHub.com/x": "api.github.com",
		"example.com:443":          "example.com",
		"*.example.com":            ".example.com",
		".example.com":             ".example.com",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestAllowDenyPrune(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	added, err := Allow("api.stripe.com", "https://foo.dev/path")
	if err != nil || len(added) != 2 {
		t.Fatalf("allow: %v %v", added, err)
	}
	if again, _ := Allow("api.stripe.com"); len(again) != 0 {
		t.Fatalf("duplicate added: %v", again)
	}
	// wildcard makes the specific entry redundant
	if _, err := Allow(".stripe.com"); err != nil {
		t.Fatal(err)
	}
	list, _ := ReadAllowlist()
	joined := strings.Join(list, ",")
	if strings.Contains(joined, "api.stripe.com") || !strings.Contains(joined, ".stripe.com") {
		t.Fatalf("prune failed: %s", joined)
	}
	if !Allowed(list, "files.stripe.com") || !Allowed(list, "stripe.com") || Allowed(list, "evilstripe.com") {
		t.Fatal("wildcard matching wrong")
	}
	if removed, _ := Deny("foo.dev"); len(removed) != 1 {
		t.Fatalf("deny: %v", removed)
	}
	b, _ := os.ReadFile(paths.Allowlist())
	if strings.Contains(string(b), "foo.dev") || !strings.Contains(string(b), "# Model APIs") {
		t.Fatalf("deny broke file: %s", b)
	}
}

func TestSquidConfNoReverseLookup(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(paths.SquidConf())
	if !strings.Contains(string(b), `dstdomain -n "/etc/zoo/allowlist.txt"`) {
		t.Fatal("allowlist acl must use -n so IP literals aren't matched by their PTR name")
	}
}

// Private destinations are refused before anything is allowed, and nothing
// allows by source (the old learn mode): every new host is approved by hand.
func TestSquidConfDeniesPrivateFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOO_HOME", home)
	// Files from an install that had learn mode.
	_ = os.MkdirAll(paths.KeeperEtc(), 0o755)
	for _, f := range []string{"learn.txt", "learn-cages.txt"} {
		_ = os.WriteFile(filepath.Join(paths.KeeperEtc(), f), []byte("10.0.0.5/32\n"), 0o644)
	}
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"learn.txt", "learn-cages.txt"} {
		if _, err := os.Stat(filepath.Join(paths.KeeperEtc(), f)); err == nil {
			t.Errorf("%s from learn mode left behind", f)
		}
	}
	b, _ := os.ReadFile(paths.SquidConf())
	conf := string(b)
	var access []string
	for _, l := range strings.Split(conf, "\n") {
		if strings.HasPrefix(l, "http_access ") {
			access = append(access, strings.TrimPrefix(l, "http_access "))
		}
		if strings.HasPrefix(l, "acl ") && strings.Contains(l, " src ") {
			t.Errorf("source-based acl (allow-all for a cage?): %s", l)
		}
	}
	firstAllow := -1
	for i, a := range access {
		if strings.HasPrefix(a, "allow ") && firstAllow < 0 {
			firstAllow = i
		}
	}
	if firstAllow < 0 || access[firstAllow] != "allow zoo_allowed" || access[len(access)-1] != "deny all" {
		t.Fatalf("want a single allow (zoo_allowed) and deny all last: %v", access)
	}
	for _, want := range []string{"deny zoo_private_name", "deny zoo_private"} {
		i := indexOf(access, want)
		if i < 0 || i > firstAllow {
			t.Errorf("%q must come before the allow: %v", want, access)
		}
	}
	for _, cidr := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "::1/128", "fc00::/7", "fe80::/10"} {
		if !strings.Contains(conf, " "+cidr) {
			t.Errorf("private range %s not denied", cidr)
		}
	}
	// Squid stores IPv4 as v4-mapped IPv6, so these read as 0.0.0.0/0: in a
	// deny they'd refuse everything (squid currently drops them with a
	// warning; don't rely on it).
	for _, all := range []string{"::/128", "::/0", "::ffff:0:0/96", "0.0.0.0/0"} {
		if strings.Contains(conf, " "+all) {
			t.Errorf("%s in the config means every address to squid", all)
		}
	}
	if !strings.Contains(conf, ".internal") {
		t.Error("host.docker.internal & co. not denied by name")
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}
