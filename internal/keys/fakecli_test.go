package keys

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The bw and op fakes are this test binary, copied into a temp dir under
// those names: TestMain sees the name and plays the CLI. (They used to be
// shell scripts, which don't run on Windows.)
func TestMain(m *testing.M) {
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") {
	case "bw":
		os.Exit(fakeBW(os.Args[1:], os.Stdout, os.Stderr))
	case "op":
		os.Exit(fakeOP(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func logArgs(args []string) {
	if p := os.Getenv("FAKE_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			fmt.Fprintln(f, strings.Join(args, " "))
			f.Close()
		}
	}
}

func arg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

const stripeItem = `{"id":"11111111-2222-3333-4444-555555555555","name":"Stripe","notes":"line1\nline2\n",
 "login":{"username":"me@example.com","password":"pw-secret\n","totp":"otpauth://x"},
 "fields":[{"name":"test-key","value":"sk_test_123","type":1},{"name":"Region","value":"eu","type":0}]}`

func fakeBW(args []string, out, errw io.Writer) int {
	logArgs(args)
	if !strings.Contains(" "+strings.Join(args, " ")+" ", " --nointeraction ") {
		fmt.Fprintln(errw, "missing --nointeraction")
		return 9
	}
	if arg(args, 0) == "status" {
		if os.Getenv("BW_SESSION") == "" {
			fmt.Fprintln(out, `{"status":"locked"}`)
		} else {
			fmt.Fprintln(out, `{"status":"unlocked"}`)
		}
		return 0
	}
	if os.Getenv("BW_SESSION") == "" {
		fmt.Fprintln(errw, "Vault is locked.")
		return 1
	}
	switch arg(args, 0) + " " + arg(args, 1) + " " + arg(args, 2) {
	case "get item Stripe":
		fmt.Fprintln(out, stripeItem)
	case "get item bare":
		fmt.Fprintln(out, `{"id":"x","name":"bare","notes":null,"login":null,"fields":null}`)
	case "get item dup":
		fmt.Fprint(errw, "More than one result was found. Try getting a specific object by `id` instead. The following objects were found:\naaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\nffffffff-bbbb-cccc-dddd-eeeeeeeeeeee\n")
		return 1
	case "get item garbage":
		fmt.Fprintln(out, "not json")
	case "get item sleepy":
		time.Sleep(5 * time.Second)
	case "get totp Stripe":
		fmt.Fprintln(out, "123456")
	case "get totp bare":
		fmt.Fprintln(errw, "No TOTP available for this login.")
		return 1
	default:
		fmt.Fprintln(errw, "Not found.")
		return 1
	}
	return 0
}

func fakeOP(args []string, out, errw io.Writer) int {
	logArgs(args)
	if os.Getenv("OP_SIGNED_IN") == "" {
		if arg(args, 0) == "account" {
			if os.Getenv("OP_ACCOUNTS") != "" {
				fmt.Fprintln(out, `[{"url":"my.1password.com"}]`)
			} else {
				fmt.Fprintln(out, "[]")
			}
			return 0
		}
		fmt.Fprintln(errw, "[ERROR] 2026/10/28 10:00:00 You are not currently signed in. Please run `op signin --help` for instructions")
		return 1
	}
	switch arg(args, 0) {
	case "whoami":
		fmt.Fprintln(out, "me")
	case "read":
		if arg(args, 1) != "--no-newline" {
			fmt.Fprintln(errw, "missing --no-newline")
			return 9
		}
		switch arg(args, 2) {
		case "op://Private/Stripe/key":
			fmt.Fprint(out, "op-secret\n\n")
		case "op://Private/Empty/key":
		default:
			fmt.Fprintf(errw, "[ERROR] 2026/10/28 10:00:00 could not read secret '%s': no item matched\n", arg(args, 2))
			return 1
		}
	}
	return 0
}

// installFakes copies this test binary into dir as bw and op.
func installFakes(t *testing.T, dir string) (bw, op string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	bw, op = filepath.Join(dir, "bw"+ext), filepath.Join(dir, "op"+ext)
	for _, p := range []string{bw, op} {
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bw, op
}
