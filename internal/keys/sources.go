package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/proc"
)

// Secret source prefixes.
const (
	bwPrefix = "bw:"
	opPrefix = "op://"
)

// cliTimeout bounds each vault CLI call.
var cliTimeout = 15 * time.Second

const (
	bwInstallHint = "install the Bitwarden CLI (brew install bitwarden-cli), run `bw login` and `bw unlock`, then export BW_SESSION"
	bwUnlockHint  = "run `bw unlock` and export the BW_SESSION it prints"
	bwLoginHint   = "run `bw login`, then `bw unlock` and export BW_SESSION"
	opInstallHint = "install the 1Password CLI (brew install 1password-cli) and run `op signin`"
	opSigninHint  = "`op` not signed in: run `op signin` (or enable the 1Password app's CLI integration)"
)

// Resolve returns the secret a reference names. References:
//
//	"bw:<item>"                    Bitwarden CLI: password of the item (name or id)
//	"bw:<item>/<field>"            a custom field, or "username", "notes", "totp", "password"
//	"op://<vault>/<item>/<field>"  1Password CLI (`op read`)
//	anything else                  a Keychain item (Get; "" with nil error when absent)
//
// For bw refs the item/field split is at the last "/", so an item whose name
// contains "/" must be addressed as "bw:<name>/password" or by id.
// Vault CLIs never prompt: a locked vault or missing sign-in is an error
// with a hint. The secret is never put on argv or logged.
func Resolve(ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, bwPrefix):
		item, field, err := parseBW(ref)
		if err != nil {
			return "", err
		}
		return bwResolve(item, field)
	case strings.HasPrefix(ref, opPrefix):
		if err := checkOP(ref); err != nil {
			return "", err
		}
		return opResolve(ref)
	default:
		return Get(ref)
	}
}

// Describe names a reference's source and the item within it, for display:
// ("bitwarden", "Stripe/test-key"), ("1password", "Private/Stripe/key"),
// ("keychain", "claude"). Never reads the secret.
func Describe(ref string) (source, name string) {
	switch {
	case strings.HasPrefix(ref, bwPrefix):
		return "bitwarden", strings.TrimPrefix(ref, bwPrefix)
	case strings.HasPrefix(ref, opPrefix):
		return "1password", strings.TrimPrefix(ref, opPrefix)
	default:
		return "keychain", ref
	}
}

// Available is a cheap presence check: for Keychain items whether the item
// is stored; for vault refs whether the CLI is installed and unlocked/signed
// in (the item itself isn't fetched). When false, the string is a hint.
func Available(ref string) (bool, string) {
	switch {
	case strings.HasPrefix(ref, bwPrefix):
		if _, _, err := parseBW(ref); err != nil {
			return false, err.Error()
		}
		return bwAvailable()
	case strings.HasPrefix(ref, opPrefix):
		if err := checkOP(ref); err != nil {
			return false, err.Error()
		}
		return opAvailable()
	default:
		if Has(ref) {
			return true, ""
		}
		return false, fmt.Sprintf("not stored: run `zoo keys set %s`", ref)
	}
}

// ---- Bitwarden ----

func parseBW(ref string) (item, field string, err error) {
	rest := strings.TrimPrefix(ref, bwPrefix)
	item, field = rest, "password"
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		item, field = rest[:i], rest[i+1:]
	}
	if item == "" || field == "" {
		return "", "", fmt.Errorf("bad Bitwarden reference %q: want bw:<item> or bw:<item>/<field>", ref)
	}
	return item, field, nil
}

func bwBin() (string, error) {
	if b := os.Getenv("ZOO_BW_BIN"); b != "" {
		return b, nil
	}
	b, err := exec.LookPath("bw")
	if err != nil {
		return "", errors.New("bitwarden: `bw` not found: " + bwInstallHint)
	}
	return b, nil
}

type bwItem struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Notes *string `json:"notes"`
	Login *struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
		Totp     *string `json:"totp"`
	} `json:"login"`
	Fields []struct {
		Name  string  `json:"name"`
		Value *string `json:"value"`
	} `json:"fields"`
}

func bwResolve(item, field string) (string, error) {
	bin, err := bwBin()
	if err != nil {
		return "", err
	}
	if field == "totp" {
		out, err := runCLI(bin, "get", "totp", item, "--nointeraction")
		if err != nil {
			return "", bwError(item, err)
		}
		v := trimNL(out)
		if v == "" {
			return "", fmt.Errorf("bitwarden: item %q has no TOTP", item)
		}
		return v, nil
	}
	out, err := runCLI(bin, "get", "item", item, "--nointeraction")
	if err != nil {
		return "", bwError(item, err)
	}
	var it bwItem
	if err := json.Unmarshal([]byte(out), &it); err != nil {
		return "", fmt.Errorf("bitwarden: couldn't parse `bw get item` output for %q", item)
	}
	var v *string
	switch field {
	case "password":
		if it.Login != nil {
			v = it.Login.Password
		}
	case "username":
		if it.Login != nil {
			v = it.Login.Username
		}
	case "notes":
		v = it.Notes
	default:
		v = bwField(it, field)
	}
	if v == nil || *v == "" {
		if field == "password" || field == "username" || field == "notes" {
			return "", fmt.Errorf("bitwarden: item %q has no %s", item, field)
		}
		return "", fmt.Errorf("bitwarden: item %q has no field %q", item, field)
	}
	return *v, nil
}

// bwField finds a custom field by exact name, else by a unique
// case-insensitive match.
func bwField(it bwItem, name string) *string {
	var fold *string
	n := 0
	for _, f := range it.Fields {
		if f.Name == name {
			return f.Value
		}
		if strings.EqualFold(f.Name, name) {
			fold = f.Value
			n++
		}
	}
	if n == 1 {
		return fold
	}
	return nil
}

func bwError(item string, err error) error {
	var ce *cliError
	if !errors.As(err, &ce) {
		return fmt.Errorf("bitwarden: %v", err)
	}
	msg := ce.msg
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "more than one result"):
		ids := bwIDs(msg)
		s := fmt.Sprintf("bitwarden: more than one item named %q; use the item id (bw:<id>[/<field>])", item)
		if len(ids) > 0 {
			s += ": " + strings.Join(ids, ", ")
		}
		return errors.New(s)
	case strings.Contains(low, "not logged in"):
		return errors.New("bitwarden: not logged in: " + bwLoginHint)
	case strings.Contains(low, "locked"), strings.Contains(low, "master password"),
		strings.Contains(low, "session"):
		return errors.New("bitwarden: vault is locked: " + bwUnlockHint)
	case strings.Contains(low, "not found"):
		return fmt.Errorf("bitwarden: no item %q", item)
	case strings.Contains(low, "no totp") || strings.Contains(low, "totp"):
		return fmt.Errorf("bitwarden: item %q has no TOTP", item)
	}
	return fmt.Errorf("bitwarden: %s", firstLine(msg))
}

// bwIDs pulls the candidate ids bw lists after an ambiguous match.
func bwIDs(msg string) []string {
	var ids []string
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		if len(l) == 36 && strings.Count(l, "-") == 4 {
			ids = append(ids, l)
		}
	}
	return ids
}

func bwAvailable() (bool, string) {
	bin, err := bwBin()
	if err != nil {
		return false, bwInstallHint
	}
	out, err := runCLI(bin, "status", "--nointeraction")
	if err != nil {
		return false, "`bw status` failed: " + bwUnlockHint
	}
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		return false, "couldn't parse `bw status`"
	}
	switch st.Status {
	case "unlocked":
		return true, ""
	case "unauthenticated":
		return false, "Bitwarden not logged in: " + bwLoginHint
	default:
		return false, "Bitwarden vault is locked: " + bwUnlockHint
	}
}

// ---- 1Password ----

func checkOP(ref string) error {
	parts := strings.Split(strings.TrimPrefix(ref, opPrefix), "/")
	if len(parts) < 3 {
		return fmt.Errorf("bad 1Password reference %q: want op://<vault>/<item>/<field>", ref)
	}
	for _, p := range parts {
		if p == "" {
			return fmt.Errorf("bad 1Password reference %q: want op://<vault>/<item>/<field>", ref)
		}
	}
	return nil
}

func opBin() (string, error) {
	if b := os.Getenv("ZOO_OP_BIN"); b != "" {
		return b, nil
	}
	b, err := exec.LookPath("op")
	if err != nil {
		return "", errors.New("1password: `op` not found: " + opInstallHint)
	}
	return b, nil
}

func opResolve(ref string) (string, error) {
	bin, err := opBin()
	if err != nil {
		return "", err
	}
	out, err := runCLI(bin, "read", "--no-newline", ref)
	if err != nil {
		var ce *cliError
		if !errors.As(err, &ce) {
			return "", fmt.Errorf("1password: %v", err)
		}
		if opNotSignedIn(ce.msg) {
			return "", errors.New("1password: " + opSigninHint)
		}
		return "", fmt.Errorf("1password: %s", firstLine(stripOPPrefix(ce.msg)))
	}
	v := trimNL(out)
	if v == "" {
		return "", fmt.Errorf("1password: %s is empty", ref)
	}
	return v, nil
}

func opNotSignedIn(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "not signed in") || strings.Contains(low, "not currently signed in") ||
		strings.Contains(low, "no accounts configured") || strings.Contains(low, "signin")
}

// stripOPPrefix drops op's "[ERROR] 2024/01/02 15:04:05 " prefix.
func stripOPPrefix(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.HasPrefix(msg, "[ERROR] ") {
		f := strings.SplitN(msg, " ", 4)
		if len(f) == 4 {
			return f[3]
		}
	}
	return msg
}

func opAvailable() (bool, string) {
	bin, err := opBin()
	if err != nil {
		return false, opInstallHint
	}
	if _, err := runCLI(bin, "whoami"); err == nil {
		return true, ""
	}
	// With the desktop app integration `op whoami` fails until the first
	// real command unlocks; a configured account is enough to try.
	out, err := runCLI(bin, "account", "list", "--format=json")
	if err == nil {
		var accts []json.RawMessage
		if json.Unmarshal([]byte(out), &accts) == nil && len(accts) > 0 {
			return true, ""
		}
	}
	return false, opSigninHint
}

// ---- shared ----

type cliError struct {
	msg string
}

func (e *cliError) Error() string { return e.msg }

// runCLI runs a vault CLI with the caller's environment (BW_SESSION, OP_*),
// no stdin (so it can't prompt), and a timeout. It returns stdout; on failure
// the error carries stderr (falling back to stdout), never the secret.
func runCLI(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	proc.Quiet(cmd)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = os.Environ()
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("`%s %s` timed out after %s (waiting for input?)", baseName(bin), args[0], cliTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", &cliError{msg: msg}
	}
	return out.String(), nil
}

// trimNL drops a single trailing newline (\n or \r\n).
func trimNL(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
