// Package sshdoor is the SSH door into cages (M6): host editors (VS Code
// Remote-SSH, JetBrains Gateway, Orca's remote mode) reach a cage with plain
// `ssh zoo-<cage>`.
//
// There is no network listener anywhere. The host's ssh client runs
// `zoo ssh-proxy <cage>` as its ProxyCommand; that execs, through the
// backend (container/docker exec), an `sshd -i` running as the unprivileged
// agent user inside the cage, with the client's stdin/stdout as the
// connection. Non-root sshd can only log in as itself, so the door opens on
// `agent` and nothing else. The cage gains no path to the host: remote
// forwarding, agent forwarding, X11 and unix-socket forwarding are refused
// by the root-owned sshd_config and by the authorized_keys options.
//
// This package holds the pieces that don't touch a backend: host-side key
// and ssh_config generation, and the guest-side scripts and files.
package sshdoor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HostPrefix starts every SSH host alias zoo writes: `zoo-<cage name>`.
const HostPrefix = "zoo-"

// Guest paths (baked into the cage image; see assets/images/cage).
const (
	GuestConfig         = "/etc/zoo/sshd_config"
	GuestAuthorizedKeys = "/etc/zoo/authorized_keys"
	GuestEnvConf        = "/etc/zoo/sshd_env.conf"
	GuestHostKeyDir     = "/home/agent/.zoo-ssh"
	GuestSSHD           = "/usr/sbin/sshd"
)

// KeyOptions restrict what the host's key may do even if sshd_config were
// weakened: no agent or X11 forwarding, no ~/.ssh/rc; local port forwarding
// (editors reach their in-cage server and cage ports this way) and ptys
// stay on. sshd_config still refuses remote and unix-socket forwarding.
const KeyOptions = "restrict,port-forwarding,pty"

// CageFromHost turns the host as typed (`zoo-myproj-1a2b3c`) into a cage
// name. A bare name passes through.
func CageFromHost(h string) string {
	return strings.TrimPrefix(strings.TrimSpace(h), HostPrefix)
}

// ValidCageName reports whether s is safe to put in an ssh_config Host line
// and to look up as a cage (cage names are [a-z0-9-]).
func ValidCageName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// KeyPath is the host's client key under dir (~/.zoo/ssh).
func KeyPath(dir string) string { return filepath.Join(dir, "id_ed25519") }

// EnsureClientKey returns the authorized_keys line (with KeyOptions) for the
// client key in dir, generating an ed25519 key first if there is none. The
// directory is 0700, the private key 0600.
func EnsureClientKey(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	priv := KeyPath(dir)
	if _, err := os.Stat(priv); errors.Is(err, os.ErrNotExist) {
		if err := generate(priv); err != nil {
			return "", err
		}
	}
	b, err := os.ReadFile(priv)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return "", fmt.Errorf("%s: %w (delete it and zoo makes a new one)", priv, err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	// Keep the .pub next to it in sync for people who look.
	_ = os.WriteFile(priv+".pub", []byte(pub+" zoo\n"), 0o644)
	return KeyOptions + " " + pub + " zoo", nil
}

func generate(path string) error {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(key, "zoo")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".id-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(pem.EncodeToMemory(block)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link, not rename: if another zoo made one meanwhile, keep theirs.
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// ConfigOptions are written under every Host block. Agent and X11 forwarding
// would hand the cage a path into the host; host keys live in the cage (an
// agent can change them) and the exec transport already decides which cage
// answers, so host-key checking adds nothing but prompts.
var ConfigOptions = []string{
	"User agent",
	"IdentitiesOnly yes",
	"PreferredAuthentications publickey",
	"PasswordAuthentication no",
	"KbdInteractiveAuthentication no",
	"GSSAPIAuthentication no",
	"ForwardAgent no",
	"ForwardX11 no",
	"ForwardX11Trusted no",
	"Tunnel no",
	"PermitLocalCommand no",
	"ClearAllForwardings no",
	"StrictHostKeyChecking no",
	"UserKnownHostsFile /dev/null",
	"UpdateHostKeys no",
	"LogLevel ERROR",
}

// ClientConfig renders ~/.zoo/ssh/config: one block per cage (so editors
// list them) and a zoo-* catch-all for cages made later. zooBin is the
// absolute path of zoo, keyFile the client key as ssh should read it, and
// zooHome is passed through as ZOO_HOME when non-empty (editors launch ssh
// without your shell's environment).
func ClientConfig(zooBin, keyFile, zooHome string, cages []string) (string, error) {
	for _, s := range []string{zooBin, keyFile, zooHome} {
		if strings.ContainsAny(s, "\"'`$\\\n\r") {
			return "", fmt.Errorf("can't write an ssh config for path %q (quotes, $, ` or \\ in it); move zoo or ZOO_HOME to a plainer path", s)
		}
	}
	pc := quote(pct(zooBin)) + " ssh-proxy %n"
	if zooHome != "" {
		pc = "env ZOO_HOME=" + quote(pct(zooHome)) + " " + pc
	}
	block := func(host string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "Host %s\n", host)
		fmt.Fprintf(&b, "  ProxyCommand %s\n", pc)
		fmt.Fprintf(&b, "  IdentityFile %s\n", quote(pct(keyFile)))
		for _, o := range ConfigOptions {
			fmt.Fprintf(&b, "  %s\n", o)
		}
		return b.String()
	}
	var b strings.Builder
	b.WriteString("# Written by `zoo ssh setup`; rewritten each time it runs, so don't edit.\n")
	b.WriteString("# Use it by putting this line at the TOP of ~/.ssh/config:\n")
	b.WriteString("#   Include " + IncludePath(zooHome) + "\n")
	b.WriteString("# (ssh takes the first value it sees for each option, so it must come first.)\n\n")
	seen := map[string]bool{}
	for _, c := range cages {
		if !ValidCageName(c) || seen[c] {
			continue
		}
		seen[c] = true
		b.WriteString(block(HostPrefix + c))
		b.WriteString("\n")
	}
	b.WriteString(block(HostPrefix + "*"))
	return b.String(), nil
}

// IncludePath is how ~/.ssh/config should name zoo's config.
func IncludePath(zooHome string) string {
	if zooHome == "" {
		return "~/.zoo/ssh/config"
	}
	return quote(filepath.Join(zooHome, "ssh", "config"))
}

// pct escapes ssh_config's %-tokens.
func pct(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t#") {
		return s
	}
	return `"` + s + `"`
}

// EnvConf renders the sshd_config fragment that gives SSH sessions the
// cage's session environment. sshd starts sessions with a fresh environment
// (it doesn't pass its own on), so SetEnv is the one place that reaches
// every shell, command and subsystem. Only the first SetEnv line counts, so
// it is a single line. Entries sshd's parser can't carry safely (quotes,
// backslashes, control characters) are left out and returned.
func EnvConf(env []string) (string, []string) {
	var parts, skipped []string
	seen := map[string]bool{}
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if !ok || k == "" || !validEnvName(k) || strings.ContainsAny(e, "\"\\") || hasControl(e) {
			skipped = append(skipped, k)
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		parts = append(parts, `"`+e+`"`)
	}
	if len(parts) == 0 {
		return "# no session environment\n", skipped
	}
	return "# Written by zoo ssh-proxy on every connection.\nSetEnv " + strings.Join(parts, " ") + "\n", skipped
}

func validEnvName(k string) bool {
	for i, r := range k {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// BaseEnv is what every SSH session gets besides the keeper proxy and
// service variables: the variables `zoo run` sessions get from the image
// and from zoo, which sshd's fresh session environment would otherwise drop.
// HOME, USER, SHELL and PATH come from sshd itself.
func BaseEnv(cageName string) []string {
	return []string{
		"ZOO_CAGE=" + cageName, "ZOO_INSIDE=1", "ZOO_SSH=1",
		"LANG=C.UTF-8",
		"NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
	}
}

// InstallScript runs as guest root with the authorized_keys line on the
// first line of stdin and the env fragment after it. Both files are
// root-owned, so the agent can't add keys or change what sessions get.
// Exit 3 means the cage's image predates the SSH door.
const InstallScript = `set -e
if [ ! -x ` + GuestSSHD + ` ] || [ ! -f ` + GuestConfig + ` ]; then exit 3; fi
umask 022
IFS= read -r key
t=$(mktemp /etc/zoo/.ssh.XXXXXX)
printf '%s\n' "$key" > "$t"
chmod 0644 "$t"
mv -f "$t" ` + GuestAuthorizedKeys + `
t=$(mktemp /etc/zoo/.ssh.XXXXXX)
cat > "$t"
chmod 0644 "$t"
mv -f "$t" ` + GuestEnvConf + `
`

// SSHDScript runs as the agent with the ssh connection on stdin/stdout: it
// makes the cage's host key on first use (agent-owned; non-root sshd needs
// to read it) and becomes sshd. Nothing but sshd may write to stdout.
const SSHDScript = `d=` + GuestHostKeyDir + `
if [ ! -s "$d/host_ed25519" ]; then
  umask 077
  mkdir -p "$d" || exit 1
  t=$(mktemp -d "$d/.new.XXXXXX") || exit 1
  ssh-keygen -q -t ed25519 -N "" -C zoo-cage -f "$t/k" </dev/null >/dev/null 2>&1 || { rm -rf "$t"; exit 1; }
  mv -n "$t/k" "$d/host_ed25519" 2>/dev/null || true
  rm -rf "$t"
fi
exec ` + GuestSSHD + ` -i -e -f ` + GuestConfig + `
`
