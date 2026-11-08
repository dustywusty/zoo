package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const ph = "ghp_zoocred.github.api-1a2b3c.0123456789abcdef01234567"

type seen struct {
	auth, path, method, body string
}

// harness: an injector whose CA covers api.example.com and git.example.com,
// an upstream that records what reached it, and a client that trusts the CA
// and goes through the injector the way squid does (CONNECT).
type harness struct {
	in     *injector
	client *http.Client
	addr   string // the injector, as squid would dial it
	caFile string // the test CA, for clients other than h.client
	mu     sync.Mutex
	got    []seen
	log    bytes.Buffer
}

func newHarness(t *testing.T, rules []injectRule, secrets map[string]string) *harness {
	return newHarnessWith(t, rules, secrets, nil)
}

// newHarnessWith uses upstream as the real server when it's non-nil.
func newHarnessWith(t *testing.T, rules []injectRule, secrets map[string]string, upstream http.Handler) *harness {
	t.Helper()
	dir := t.TempDir()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test zoo CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, PermittedDNSDomainsCritical: true,
		PermittedDNSDomains: []string{"api.example.com", "git.example.com"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	_ = os.WriteFile(filepath.Join(dir, "ca.pem"), pemCert(der), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "ca.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	conf, _ := json.Marshal(injectConfig{CACert: filepath.Join(dir, "ca.pem"), CAKey: filepath.Join(dir, "ca.key"), Rules: rules})
	_ = os.WriteFile(filepath.Join(dir, "inject.json"), conf, 0o644)

	h := &harness{caFile: filepath.Join(dir, "ca.pem")}
	h.in = newInjector(func(p string) (string, string, bool) {
		if v, ok := secrets[p]; ok {
			return v, "", true
		}
		return "", "not granted", false
	}, &h.log)
	if err := h.in.load(filepath.Join(dir, "inject.json")); err != nil {
		t.Fatal(err)
	}

	if upstream == nil {
		upstream = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			h.mu.Lock()
			h.got = append(h.got, seen{r.Header.Get("Authorization"), r.URL.Path, r.Method, string(b)})
			h.mu.Unlock()
			fmt.Fprint(w, "upstream ok")
		})
	}
	up := httptest.NewTLSServer(upstream)
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	upPool := x509.NewCertPool()
	upPool.AddCert(up.Certificate())
	// Upstream TLS is verified (against the test server's own cert).
	h.in.upstream = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", upURL.Host)
		},
		TLSClientConfig: &tls.Config{RootCAs: upPool, ServerName: "example.com"},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go h.in.serve(ln)
	h.addr = ln.Addr().String()

	caPool := x509.NewCertPool()
	caPool.AddCert(mustParse(der))
	proxy, _ := url.Parse("http://" + ln.Addr().String())
	h.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxy),
		TLSClientConfig: &tls.Config{RootCAs: caPool},
	}}
	return h
}

func mustParse(der []byte) *x509.Certificate {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return c
}

func (h *harness) do(t *testing.T, method, rawurl, auth, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, rawurl, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawurl, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) last() seen {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.got) == 0 {
		return seen{}
	}
	return h.got[len(h.got)-1]
}

func (h *harness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.got)
}

func TestInjectSwapsPlaceholder(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}}}, map[string]string{ph: "ghp_REAL"})
	code, _ := h.do(t, "GET", "https://api.example.com/user", "Bearer "+ph, "")
	if code != 200 || h.last().auth != "Bearer ghp_REAL" {
		t.Fatalf("code %d, upstream saw %q", code, h.last().auth)
	}
	// Basic auth (git's credential helper) is decoded, swapped, re-encoded.
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+ph))
	h.do(t, "GET", "https://api.example.com/o/r.git/info/refs", basic, "")
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h.last().auth, "Basic "))
	if string(dec) != "x-access-token:ghp_REAL" {
		t.Fatalf("basic auth upstream = %q", dec)
	}
	if strings.Contains(h.log.String(), "ghp_REAL") {
		t.Fatal("secret written to the log")
	}
}

func TestInjectRefusesUngranted(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}}}, nil)
	code, body := h.do(t, "GET", "https://api.example.com/user", "Bearer "+ph, "")
	if code != 403 || !strings.Contains(body, "not granted") || h.count() != 0 {
		t.Fatalf("ungranted placeholder: %d %q, upstream hits %d", code, body, h.count())
	}
}

func TestInjectOnlyForItsHosts(t *testing.T) {
	// A placeholder sent to a host its credential doesn't cover stays a
	// placeholder: the real value never goes anywhere else.
	h := newHarness(t, []injectRule{
		{Name: "github", Hosts: []string{"api.example.com"}},
		{Name: "other", Hosts: []string{"git.example.com"}},
	}, map[string]string{ph: "ghp_REAL"})
	h.do(t, "GET", "https://git.example.com/x", "Bearer "+ph, "")
	if h.last().auth != "Bearer "+ph {
		t.Fatalf("secret sent to another credential's host: %q", h.last().auth)
	}
}

func TestInjectHostMustMatchConnect(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}}}, map[string]string{ph: "ghp_REAL"})
	req, _ := http.NewRequest("GET", "https://api.example.com/user", nil)
	req.Host = "evil.example.net"
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest || h.count() != 0 {
		t.Fatalf("fronted request: %d", resp.StatusCode)
	}
}

func TestInjectReadOnly(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}, ReadOnly: true}}, map[string]string{ph: "ghp_REAL"})
	cases := []struct {
		method, path, body string
		ok                 bool
	}{
		{"GET", "/repos/o/r/actions/runs", "", true},
		{"POST", "/graphql", `{"query":"query { viewer { login } }"}`, true},
		{"POST", "/graphql", `{"query":"# mutation in a comment\nquery { viewer { login } }"}`, true},
		{"POST", "/graphql", `{"query":"query { repo(name: \"mutation\") { id } }"}`, true},
		{"POST", "/graphql", `{"query":"mutation { addStar(input:{starrableId:\"x\"}) { clientMutationId } }"}`, false},
		{"POST", "/graphql", `{"query":"query A { a } mutation B { b }","operationName":"B"}`, false},
		{"POST", "/gists", `{"files":{}}`, false},
		{"DELETE", "/repos/o/r", "", false},
		{"POST", "/o/r.git/git-upload-pack", "0000", true},
		{"POST", "/o/r.git/git-receive-pack", "0000", false},
		{"POST", "/o/r.git/info/lfs/objects/batch", `{"operation":"download"}`, true},
		{"POST", "/o/r.git/info/lfs/objects/batch", `{"operation":"upload"}`, false},
	}
	for _, c := range cases {
		before := h.count()
		code, body := h.do(t, c.method, "https://api.example.com"+c.path, "Bearer "+ph, c.body)
		if c.ok && (code != 200 || h.count() != before+1) {
			t.Errorf("%s %s %s: want allowed, got %d %q", c.method, c.path, c.body, code, body)
		}
		if !c.ok && (code != 403 || h.count() != before) {
			t.Errorf("%s %s %s: want refused, got %d %q", c.method, c.path, c.body, code, body)
		}
	}
	// Bodies reach upstream intact after inspection.
	h.do(t, "POST", "https://api.example.com/graphql", "", `{"query":"query { x }"}`)
	if h.last().body != `{"query":"query { x }"}` {
		t.Fatalf("body changed: %q", h.last().body)
	}
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func TestInjectPushBranches(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}, Push: []string{"agent/*"}}}, map[string]string{ph: "ghp_REAL"})
	old, nw := strings.Repeat("0", 40), strings.Repeat("a", 40)
	push := func(refs ...string) (int, string) {
		var b strings.Builder
		for i, r := range refs {
			line := old + " " + nw + " " + r
			if i == 0 {
				line += "\x00report-status side-band-64k"
			}
			b.WriteString(pkt(line + "\n"))
		}
		b.WriteString("0000PACK....")
		return h.do(t, "POST", "https://api.example.com/o/r.git/git-receive-pack", "", b.String())
	}
	if code, body := push("refs/heads/agent/fix-tests"); code != 200 {
		t.Fatalf("allowed branch refused: %d %s", code, body)
	}
	if !strings.HasSuffix(h.last().body, "0000PACK....") || !strings.Contains(h.last().body, "refs/heads/agent/fix-tests") {
		t.Fatalf("push body not forwarded intact: %q", h.last().body)
	}
	before := h.count()
	if code, body := push("refs/heads/agent/x", "refs/heads/main"); code != 200 || !strings.Contains(body, "ng refs/heads/main zoo keeper: push to refs/heads/main isn't allowed") || h.count() != before {
		t.Fatalf("push to main: %d %q (upstream hits %d -> %d)", code, body, before, h.count())
	}
	if _, body := push("refs/tags/v1"); !strings.Contains(body, "ng refs/tags/v1") || h.count() != before {
		t.Fatal("tag push allowed")
	}
	// Reads still work, other writes don't.
	if code, _ := h.do(t, "GET", "https://api.example.com/repos/o/r", "", ""); code != 200 {
		t.Fatal("read refused under push policy")
	}
	if code, _ := h.do(t, "POST", "https://api.example.com/repos/o/r/issues", "", "{}"); code != 403 {
		t.Fatal("write allowed under push policy")
	}
}

func TestInjectRepos(t *testing.T) {
	h := newHarness(t, []injectRule{{Name: "github", Hosts: []string{"api.example.com"}, Repos: []string{"you/app"}}}, nil)
	for path, ok := range map[string]bool{
		"/repos/you/app/actions/runs":       true,
		"/repos/You/App/pulls":              true,
		"/repos/you/other/pulls":            false,
		"/you/app.git/info/refs":            true,
		"/someone/else.git/git-upload-pack": false,
		"/user":                             true,
		"/repos/you/app/../../x/y/pulls":    false,
	} {
		code, _ := h.do(t, "GET", "https://api.example.com"+path, "", "")
		if (code == 200) != ok {
			t.Errorf("%s: code %d, want allowed=%v", path, code, ok)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{"140.82.112.3": true, "10.1.2.3": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.100.100.100": false, "::1": false, "fd00::1": false, "2606:4700::1": true, "::ffff:10.0.0.1": false} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v", ip, got)
		}
	}
}
