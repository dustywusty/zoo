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

// Resolve maps an agent name ("bitwarden", "1password", "system") or an
// address ("~/" expanded) to an existing agent on this host: a Unix socket
// path, or on Windows a named pipe (\\.\pipe\...).
func Resolve(name string) (string, error) {
	home, _ := userHomeDir()
	var candidates []string
	var hint string
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bitwarden":
		switch goos {
		case "windows":
			candidates = []string{windowsAgentPipe}
		case "darwin":
			candidates = []string{filepath.Join(home, ".bitwarden-ssh-agent.sock"),
				filepath.Join(home, "Library", "Containers", "com.bitwarden.desktop", "Data", ".bitwarden-ssh-agent.sock")}
		default:
			candidates = []string{filepath.Join(home, ".bitwarden-ssh-agent.sock"),
				filepath.Join(home, "snap", "bitwarden", "current", ".bitwarden-ssh-agent.sock"),
				filepath.Join(home, ".var", "app", "com.bitwarden.desktop", "data", ".bitwarden-ssh-agent.sock")}
		}
		hint = "open the Bitwarden desktop app and turn on Settings → SSH agent"
	case "1password":
		switch goos {
		case "windows":
			candidates = []string{windowsAgentPipe}
		case "darwin":
			candidates = []string{filepath.Join(home, "Library", "Group Containers", "2BUA8C4S2C.com.1password", "t", "agent.sock")}
		default:
			candidates = []string{filepath.Join(home, ".1password", "agent.sock")}
		}
		hint = "open the 1Password app and turn on Settings → Developer → Use the SSH agent"
	case "system":
		sock := getenv("SSH_AUTH_SOCK")
		switch {
		case sock != "":
			candidates = []string{sock}
		case goos == "windows":
			// Windows' OpenSSH agent doesn't set SSH_AUTH_SOCK: it's a service
			// on a fixed pipe.
			candidates = []string{windowsAgentPipe}
		default:
			return "", fmt.Errorf("ssh-agent system: SSH_AUTH_SOCK is not set in zood's environment (start ssh-agent, or name the socket path)")
		}
		hint = "check that ssh-agent is running (ssh-add -l)"
		if goos == "windows" && sock == "" {
			hint = "start the OpenSSH agent service (Get-Service ssh-agent | Set-Service -StartupType Automatic -PassThru | Start-Service)"
		}
	case "":
		return "", fmt.Errorf("ssh-agent: empty name (want bitwarden, 1password, system, or a socket path)")
	default:
		p := name
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[1:])
		}
		if !filepath.IsAbs(p) && !isPipe(p) {
			return "", fmt.Errorf("ssh-agent %q: unknown agent (want bitwarden, 1password, system, an absolute socket path, or a named pipe)", name)
		}
		candidates = []string{p}
		hint = "check the path"
	}
	for _, c := range candidates {
		if agentAt(c) {
			return c, nil
		}
	}
	where := "agent socket"
	if goos == "windows" {
		where = "agent"
	}
	return "", fmt.Errorf("ssh-agent %s: no %s at %s; %s", name, where, strings.Join(candidates, " or "), hint)
}

// agentAt reports whether something that could be an agent listens at c.
func agentAt(c string) bool {
	if isPipe(c) {
		return pipeExists(c)
	}
	fi, err := os.Stat(c)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
