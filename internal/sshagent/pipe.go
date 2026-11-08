package sshagent

import "strings"

// pipePrefix starts every Windows named pipe path.
const pipePrefix = `\\.\pipe\`

// windowsAgentPipe is where Windows agents listen: the OpenSSH agent
// service, and 1Password and Bitwarden when their SSH agent is on (they
// take over the same pipe). Overridable in tests.
var windowsAgentPipe = pipePrefix + "openssh-ssh-agent"

// isPipe reports whether an agent address is a Windows named pipe.
func isPipe(p string) bool {
	return strings.HasPrefix(p, pipePrefix) || strings.HasPrefix(p, "//./pipe/")
}
