// Package inject keeps credentials out of cages. A cage gets a placeholder
// (in an env var, or from git's credential helper); the keeper terminates
// TLS for the credential's hosts only, swaps the placeholder for the real
// value fetched from zood over the host bridge, enforces the credential's
// policy (read-only, push branches, repositories) and re-encrypts to the
// real host with normal certificate checks.
//
// The keeper's CA is generated per install and name-constrained to exactly
// the hosts that have credentials or policies, so even its key can't mint a
// certificate for anything else. Every other host is still a plain CONNECT
// tunnel squid never looks inside.
package inject

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// ListenAddr is where the injector listens inside the keeper (loopback
// only: squid forwards the credential hosts' CONNECTs to it).
const ListenAddr = "127.0.0.1:3129"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// placeholderRe finds a placeholder with any token-shaped prefix in front.
// The prefix is part of what gets replaced ("sk-ant-oat01-zoocred....").
var placeholderRe = regexp.MustCompile(`[A-Za-z0-9_-]*zoocred\.([a-z0-9-]+)\.([a-z0-9-]+)\.([0-9a-f]{24})`)

// Validate checks credential definitions.
func Validate(creds map[string]config.Credential, svcs map[string]config.Service) error {
	owner := map[string]string{}
	for _, name := range Names(creds) {
		c := creds[name]
		if !nameRe.MatchString(name) {
			return fmt.Errorf("credential %q: names are lowercase letters, digits and dashes", name)
		}
		if _, clash := svcs[name]; clash {
			return fmt.Errorf("credential %q: a service has the same name", name)
		}
		if len(c.Hosts) == 0 {
			return fmt.Errorf("credential %q: needs hosts:", name)
		}
		for _, h := range c.Hosts {
			if h != strings.ToLower(h) || strings.HasPrefix(h, ".") || strings.ContainsAny(h, ":/* ") || !strings.Contains(h, ".") {
				return fmt.Errorf("credential %q: host %q must be an exact name like api.github.com", name, h)
			}
			if o, ok := owner[h]; ok {
				return fmt.Errorf("credential %q: %s already belongs to credential %q", name, h, o)
			}
			owner[h] = name
		}
		for _, p := range c.Push {
			if strings.TrimSpace(p) == "" || strings.HasPrefix(p, "refs/") && !strings.HasPrefix(p, "refs/heads/") && !strings.HasPrefix(p, "refs/tags/") {
				return fmt.Errorf("credential %q: push pattern %q", name, p)
			}
		}
		for _, r := range c.Repos {
			if strings.Count(r, "/") != 1 {
				return fmt.Errorf("credential %q: repos entries are owner/name, got %q", name, r)
			}
		}
	}
	return nil
}

// Names returns credential names in order.
func Names(m map[string]config.Credential) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Active returns the credentials the keeper must handle: those referenced
// by a harness (cred:<name>), granted somewhere, or carrying a policy. The
// built-in claude/codex definitions stay dormant until something uses them.
func Active(g config.Global, granted map[string]bool) map[string]config.Credential {
	used := map[string]bool{}
	for _, h := range g.Harnesses {
		for _, v := range h.Env {
			if n, ok := strings.CutPrefix(v, "cred:"); ok {
				used[n] = true
			}
		}
	}
	out := map[string]config.Credential{}
	for n, c := range g.Credentials {
		policy := c.ReadOnly || len(c.Push) > 0 || len(c.Repos) > 0
		if used[n] || granted[n] || policy {
			out[n] = c
		}
	}
	return out
}

// --- placeholders -------------------------------------------------------------

// key is the HMAC key placeholders are made with (host-only, 0600).
func key() ([]byte, error) {
	p := paths.CredKey()
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) >= 32 {
		return bytes.TrimSpace(b), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	k := []byte(hex.EncodeToString(buf))
	return k, os.WriteFile(p, append(k, '\n'), 0o600)
}

func mac(k []byte, cred, cage string) string {
	m := hmac.New(sha256.New, k)
	fmt.Fprintf(m, "%s\x00%s", cred, cage)
	return hex.EncodeToString(m.Sum(nil))[:24]
}

// Placeholder is what a cage holds instead of credential cred. It names the
// cage and credential and is bound to them by an HMAC only the host knows,
// so a cage can't mint one for another cage or credential.
func Placeholder(c config.Credential, cred, cage string) (string, error) {
	k, err := key()
	if err != nil {
		return "", err
	}
	return c.Prefix + "zoocred." + cred + "." + cage + "." + mac(k, cred, cage), nil
}

// Parse checks a placeholder (with or without prefix) and returns its
// credential and cage.
func Parse(ph string) (cred, cage string, ok bool) {
	m := placeholderRe.FindStringSubmatch(ph)
	if m == nil {
		return "", "", false
	}
	k, err := key()
	if err != nil {
		return "", "", false
	}
	if !hmac.Equal([]byte(mac(k, m[1], m[2])), []byte(m[3])) {
		return "", "", false
	}
	return m[1], m[2], true
}

// --- the keeper's files -------------------------------------------------------------

// Rule is one credential as the keeper's injector sees it: no secret.
type Rule struct {
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts"`
	ReadOnly bool     `json:"readonly,omitempty"`
	Push     []string `json:"push,omitempty"`
	Repos    []string `json:"repos,omitempty"`
}

// File is /etc/zoo/inject/inject.json.
type File struct {
	Listen string `json:"listen"`
	CACert string `json:"ca_cert"`
	CAKey  string `json:"ca_key"`
	Rules  []Rule `json:"rules"`
}

// Hosts returns every host with a rule, sorted.
func Hosts(active map[string]config.Credential) []string {
	var out []string
	for _, c := range active {
		out = append(out, c.Hosts...)
	}
	sort.Strings(out)
	return out
}

// Sync writes the injector's config, the host list squid routes to it and
// a CA constrained to those hosts (regenerated when they change). Returns
// whether anything the keeper reads changed.
func Sync(active map[string]config.Credential) (bool, error) {
	dir := paths.InjectDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	_ = os.Chmod(dir, 0o755)
	hosts := Hosts(active)
	changed := false
	if len(hosts) > 0 {
		c, err := ensureCA(hosts)
		if err != nil {
			return false, err
		}
		changed = c
	}
	f := File{Listen: ListenAddr, CACert: "/etc/zoo/inject/ca.pem", CAKey: "/etc/zoo/inject/ca.key"}
	for _, n := range Names(active) {
		c := active[n]
		f.Rules = append(f.Rules, Rule{Name: n, Hosts: c.Hosts, ReadOnly: c.ReadOnly, Push: c.Push, Repos: c.Repos})
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	c1, err := writeIfChanged(filepath.Join(dir, "inject.json"), append(b, '\n'), 0o644)
	if err != nil {
		return false, err
	}
	c2, err := writeIfChanged(paths.InjectHosts(), []byte(strings.Join(hosts, "\n")+"\n"), 0o644)
	if err != nil {
		return false, err
	}
	return changed || c1 || c2, nil
}

func writeIfChanged(p string, b []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, p)
}

// HasHosts reports whether the keeper should route anything to the injector.
func HasHosts() bool {
	b, err := os.ReadFile(paths.InjectHosts())
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

// CACert returns the CA certificate cages must trust ("" when none).
func CACert() string {
	b, err := os.ReadFile(filepath.Join(paths.InjectDir(), "ca.pem"))
	if err != nil {
		return ""
	}
	return string(b)
}

// ensureCA makes a CA whose name constraints permit exactly hosts. The key
// is readable inside the keeper (its injector runs as squid's user); ~/.zoo
// itself is 0700.
func ensureCA(hosts []string) (bool, error) {
	dir := paths.InjectDir()
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	if b, err := os.ReadFile(certPath); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil && sameSet(c.PermittedDNSDomains, hosts) && time.Until(c.NotAfter) > 30*24*time.Hour {
				if _, err := os.Stat(keyPath); err == nil {
					return false, nil
				}
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "zoo keeper (this machine only)", Organization: []string{"zoo"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().Add(825 * 24 * time.Hour),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return false, err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(keyPath+".tmp", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(keyPath+".tmp", keyPath); err != nil {
		return false, err
	}
	return true, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// --- the cage side -------------------------------------------------------------------

// CAPath is where the keeper's CA lives inside cages.
const CAPath = "/usr/local/share/ca-certificates/zoo-keeper.crt"

// TrustScript installs $1 (a PEM CA, or empty to remove it) into the cage's
// trust store as root, and sets git's credential helper and https rewrites
// from $2 (lines "host<TAB>placeholder"). Works without update-ca-certificates
// by appending to the bundle.
const TrustScript = `set -e
umask 022
ca="$1"
dst=` + CAPath + `
bundle=/etc/ssl/certs/ca-certificates.crt
mkdir -p "$(dirname "$dst")"
old=$(cat "$dst" 2>/dev/null || true)
if [ "$ca" != "$old" ]; then
  if [ -n "$ca" ]; then printf '%s\n' "$ca" > "$dst"; else rm -f "$dst"; fi
  if command -v update-ca-certificates >/dev/null 2>&1; then
    update-ca-certificates --fresh >/dev/null 2>&1 || update-ca-certificates >/dev/null 2>&1
  elif [ -n "$ca" ] && [ -f "$bundle" ]; then
    printf '%s\n' "$ca" >> "$bundle"
  fi
fi
# git: system config is root's, so the agent can't change it (it can
# override per user, which only affects its own requests).
if command -v git >/dev/null 2>&1; then
  for host in $(git config --system --get-all zoo.hosts 2>/dev/null); do
    git config --system --unset-all "credential.https://$host.helper" 2>/dev/null || true
    git config --system --unset-all "url.https://$host/.insteadof" 2>/dev/null || true
  done
  git config --system --unset-all zoo.hosts 2>/dev/null || true
  printf '%s' "$2" | while IFS="$(printf '\t')" read -r host ph; do
    [ -n "$host" ] || continue
    git config --system --add zoo.hosts "$host"
    git config --system "credential.https://$host.helper" "!f() { test \"\$1\" = get && printf 'username=x-access-token\\npassword=%s\\n' '$ph'; }; f"
    git config --system --add "url.https://$host/.insteadof" "git@$host:"
    git config --system --add "url.https://$host/.insteadof" "ssh://git@$host/"
  done
fi
`

// SessionEnv is the environment for sessions in a cage that trusts the CA.
func SessionEnv() []string {
	return []string{
		"NODE_EXTRA_CA_CERTS=" + CAPath,
		"REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
	}
}
