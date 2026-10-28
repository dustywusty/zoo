package keeper

import (
	"os"
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

func TestLearn(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	_ = EnsureFiles()
	_ = SetLearn("api-1a2b3c", true)
	if changed, _ := SyncLearn(map[string]string{"api-1a2b3c": "192.168.64.9", "web-4d5e6f": "192.168.64.10"}); !changed {
		t.Fatal("sync should report a change")
	}
	if ips := LearnIPs(); !ips["192.168.64.9"] || ips["192.168.64.10"] {
		t.Fatalf("learn on: %v", ips)
	}
	if changed, _ := SyncLearn(map[string]string{"api-1a2b3c": "192.168.64.9"}); changed {
		t.Fatal("no-op sync reported a change")
	}
	// The cage restarted with a new IP and another cage got the old one:
	// learn mode must follow the cage, not the address.
	_, _ = SyncLearn(map[string]string{"api-1a2b3c": "192.168.64.20", "web-4d5e6f": "192.168.64.9"})
	if ips := LearnIPs(); ips["192.168.64.9"] || !ips["192.168.64.20"] {
		t.Fatalf("learn followed the IP, not the cage: %v", ips)
	}
	_ = SetLearn("api-1a2b3c", false)
	_, _ = SyncLearn(map[string]string{"api-1a2b3c": "192.168.64.20"})
	if len(LearnIPs()) != 0 {
		t.Fatal("learn off failed")
	}
	b, _ := os.ReadFile(paths.LearnList())
	if !strings.Contains(string(b), learnPlaceholder) {
		t.Fatal("placeholder missing; squid needs a non-empty src acl")
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
