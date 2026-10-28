package sshagent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Overridable in tests.
var (
	userHomeDir = os.UserHomeDir
	goos        = runtime.GOOS
	getenv      = os.Getenv
)

// Resolve maps an agent name ("bitwarden", "1password", "system") or a
// socket path ("~/" expanded) to an existing Unix socket path on this host.
func Resolve(name string) (string, error) {
	home, _ := userHomeDir()
	var candidates []string
	var hint string
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bitwarden":
		candidates = []string{filepath.Join(home, ".bitwarden-ssh-agent.sock")}
		if goos == "darwin" {
			candidates = append(candidates, filepath.Join(home, "Library", "Containers", "com.bitwarden.desktop", "Data", ".bitwarden-ssh-agent.sock"))
		} else {
			candidates = append(candidates,
				filepath.Join(home, "snap", "bitwarden", "current", ".bitwarden-ssh-agent.sock"),
				filepath.Join(home, ".var", "app", "com.bitwarden.desktop", "data", ".bitwarden-ssh-agent.sock"))
		}
		hint = "open the Bitwarden desktop app and turn on Settings → SSH agent"
	case "1password":
		if goos == "darwin" {
			candidates = []string{filepath.Join(home, "Library", "Group Containers", "2BUA8C4S2C.com.1password", "t", "agent.sock")}
		} else {
			candidates = []string{filepath.Join(home, ".1password", "agent.sock")}
		}
		hint = "open the 1Password app and turn on Settings → Developer → Use the SSH agent"
	case "system":
		sock := getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return "", fmt.Errorf("ssh-agent system: SSH_AUTH_SOCK is not set in zood's environment (start ssh-agent, or name the socket path)")
		}
		candidates = []string{sock}
		hint = "check that ssh-agent is running (ssh-add -l)"
	case "":
		return "", fmt.Errorf("ssh-agent: empty name (want bitwarden, 1password, system, or a socket path)")
	default:
		p := name
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[1:])
		}
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("ssh-agent %q: unknown agent (want bitwarden, 1password, system, or an absolute socket path)", name)
		}
		candidates = []string{p}
		hint = "check the path"
	}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err == nil && fi.Mode()&os.ModeSocket != 0 {
			return c, nil
		}
	}
	return "", fmt.Errorf("ssh-agent %s: no agent socket at %s; %s", name, strings.Join(candidates, " or "), hint)
}
