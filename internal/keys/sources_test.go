package keys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeBW = `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case " $* " in *" --nointeraction "*) ;; *) echo "missing --nointeraction" >&2; exit 9;; esac
if [ "$1" = status ]; then
  if [ -z "$BW_SESSION" ]; then echo '{"status":"locked"}'; else echo '{"status":"unlocked"}'; fi
  exit 0
fi
if [ -z "$BW_SESSION" ]; then echo "Vault is locked." >&2; exit 1; fi
case "$1 $2 $3" in
"get item Stripe")
  cat <<'EOF'
{"id":"11111111-2222-3333-4444-555555555555","name":"Stripe","notes":"line1\nline2\n",
 "login":{"username":"me@example.com","password":"pw-secret\n","totp":"otpauth://x"},
 "fields":[{"name":"test-key","value":"sk_test_123","type":1},{"name":"Region","value":"eu","type":0}]}
EOF
  ;;
"get item bare")
  echo '{"id":"x","name":"bare","notes":null,"login":null,"fields":null}' ;;
"get item dup")
  printf 'More than one result was found. Try getting a specific object by ` + "`id`" + ` instead. The following objects were found:\naaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\nffffffff-bbbb-cccc-dddd-eeeeeeeeeeee\n' >&2
  exit 1 ;;
"get item garbage")
  echo 'not json' ;;
"get item sleepy")
  sleep 5 ;;
"get totp Stripe")
  echo 123456 ;;
"get totp bare")
  echo "No TOTP available for this login." >&2; exit 1 ;;
*)
  echo "Not found." >&2; exit 1 ;;
esac
`

const fakeOP = `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
if [ -z "$OP_SIGNED_IN" ]; then
  case "$1" in
  account) if [ -n "$OP_ACCOUNTS" ]; then echo '[{"url":"my.1password.com"}]'; else echo '[]'; fi; exit 0 ;;
  esac
  echo '[ERROR] 2026/10/28 10:00:00 You are not currently signed in. Please run ` + "`op signin --help`" + ` for instructions' >&2
  exit 1
fi
case "$1" in
whoami) echo "me"; exit 0 ;;
read)
  [ "$2" = "--no-newline" ] || { echo "missing --no-newline" >&2; exit 9; }
  case "$3" in
  op://Private/Stripe/key) printf 'op-secret\n\n' ;;
  op://Private/Empty/key) ;;
  *) echo "[ERROR] 2026/10/28 10:00:00 could not read secret '$3': no item matched" >&2; exit 1 ;;
  esac ;;
esac
`

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"bw": fakeBW, "op": fakeOP} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "argv.log")
	t.Setenv("ZOO_BW_BIN", filepath.Join(dir, "bw"))
	t.Setenv("ZOO_OP_BIN", filepath.Join(dir, "op"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("BW_SESSION", "sess")
	t.Setenv("OP_SIGNED_IN", "1")
	t.Setenv("OP_ACCOUNTS", "")
	t.Setenv("ZOO_KEYS_FILE", "1")
	t.Setenv("ZOO_HOME", filepath.Join(dir, "home"))
	return log
}

func TestResolveBitwarden(t *testing.T) {
	setup(t)
	for ref, want := range map[string]string{
		"bw:Stripe":          "pw-secret\n", // JSON values are returned verbatim
		"bw:Stripe/password": "pw-secret\n",
		"bw:Stripe/username": "me@example.com",
		"bw:Stripe/notes":    "line1\nline2\n",
		"bw:Stripe/test-key": "sk_test_123",
		"bw:Stripe/region":   "eu", // unique case-insensitive match
		"bw:Stripe/totp":     "123456",
	} {
		got, err := Resolve(ref)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

func TestResolveBitwardenErrors(t *testing.T) {
	log := setup(t)
	cases := map[string]string{
		"bw:dup":           `more than one item named "dup"; use the item id`,
		"bw:nope":          `no item "nope"`,
		"bw:Stripe/absent": `item "Stripe" has no field "absent"`,
		"bw:bare":          `item "bare" has no password`,
		"bw:bare/notes":    `item "bare" has no notes`,
		"bw:bare/totp":     `item "bare" has no TOTP`,
		"bw:garbage":       "couldn't parse",
		"bw:":              "bad Bitwarden reference",
		"bw:Stripe/":       "bad Bitwarden reference",
	}
	for ref, want := range cases {
		_, err := Resolve(ref)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%q) err = %v; want containing %q", ref, err, want)
		}
	}
	_, err := Resolve("bw:dup")
	if !strings.Contains(err.Error(), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Errorf("ambiguous error should list ids: %v", err)
	}

	t.Setenv("BW_SESSION", "")
	_, err = Resolve("bw:Stripe")
	if err == nil || !strings.Contains(err.Error(), "locked") || !strings.Contains(err.Error(), "BW_SESSION") {
		t.Errorf("locked vault err = %v", err)
	}
	if ok, hint := Available("bw:Stripe"); ok || !strings.Contains(hint, "bw unlock") {
		t.Errorf("Available locked = %v, %q", ok, hint)
	}
	t.Setenv("BW_SESSION", "sess")
	if ok, hint := Available("bw:Stripe/test-key"); !ok {
		t.Errorf("Available unlocked = false, %q", hint)
	}

	b, _ := os.ReadFile(log)
	for _, s := range []string{"pw-secret", "sk_test_123", "sess"} {
		if strings.Contains(string(b), s) {
			t.Errorf("argv contained %q:\n%s", s, b)
		}
	}
}

func TestBitwardenMissingCLI(t *testing.T) {
	setup(t)
	t.Setenv("ZOO_BW_BIN", "")
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve("bw:Stripe")
	if err == nil || !strings.Contains(err.Error(), "brew install bitwarden-cli") {
		t.Errorf("err = %v", err)
	}
	if ok, hint := Available("bw:Stripe"); ok || !strings.Contains(hint, "brew install bitwarden-cli") {
		t.Errorf("Available = %v, %q", ok, hint)
	}
}

func TestTimeout(t *testing.T) {
	setup(t)
	old := cliTimeout
	cliTimeout = 200 * time.Millisecond
	defer func() { cliTimeout = old }()
	start := time.Now()
	_, err := Resolve("bw:sleepy")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestResolveOnePassword(t *testing.T) {
	log := setup(t)
	got, err := Resolve("op://Private/Stripe/key")
	if err != nil || got != "op-secret\n" { // only one trailing newline trimmed
		t.Errorf("Resolve = %q, %v", got, err)
	}
	if _, err := Resolve("op://Private/Empty/key"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty err = %v", err)
	}
	if _, err := Resolve("op://Private/Nope/key"); err == nil || !strings.Contains(err.Error(), "no item matched") ||
		strings.Contains(err.Error(), "[ERROR]") {
		t.Errorf("missing err = %v", err)
	}
	if _, err := Resolve("op://Private/Stripe"); err == nil || !strings.Contains(err.Error(), "bad 1Password reference") {
		t.Errorf("bad ref err = %v", err)
	}
	if ok, hint := Available("op://Private/Stripe/key"); !ok {
		t.Errorf("Available signed in = false, %q", hint)
	}

	t.Setenv("OP_SIGNED_IN", "")
	if _, err := Resolve("op://Private/Stripe/key"); err == nil || !strings.Contains(err.Error(), "op signin") {
		t.Errorf("not signed in err = %v", err)
	}
	if ok, hint := Available("op://Private/Stripe/key"); ok || !strings.Contains(hint, "op signin") {
		t.Errorf("Available signed out = %v, %q", ok, hint)
	}
	t.Setenv("OP_ACCOUNTS", "1")
	if ok, _ := Available("op://Private/Stripe/key"); !ok {
		t.Errorf("Available with app-integration account = false")
	}

	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "op-secret") {
		t.Errorf("argv contained the secret:\n%s", b)
	}
}

func TestKeychainRefsRouteToGet(t *testing.T) {
	setup(t)
	if v, err := Resolve("claude"); err != nil || v != "" {
		t.Errorf("absent = %q, %v", v, err)
	}
	if ok, hint := Available("claude"); ok || !strings.Contains(hint, "zoo keys set claude") {
		t.Errorf("Available absent = %v, %q", ok, hint)
	}
	if err := Set("claude", "tok\n"); err != nil {
		t.Fatal(err)
	}
	if v, err := Resolve("claude"); err != nil || v != "tok\n" {
		t.Errorf("Resolve = %q, %v", v, err)
	}
	if ok, _ := Available("claude"); !ok {
		t.Error("Available stored = false")
	}
}

func TestDescribe(t *testing.T) {
	for ref, want := range map[string][2]string{
		"bw:Stripe/test-key":      {"bitwarden", "Stripe/test-key"},
		"bw:Stripe":               {"bitwarden", "Stripe"},
		"op://Private/Stripe/key": {"1password", "Private/Stripe/key"},
		"claude":                  {"keychain", "claude"},
	} {
		s, n := Describe(ref)
		if s != want[0] || n != want[1] {
			t.Errorf("Describe(%q) = %q, %q; want %q", ref, s, n, want)
		}
	}
}
