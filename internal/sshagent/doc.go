// Package sshagent is zoo's filtering SSH-agent proxy. It runs on the host,
// inside zood, between a cage and the user's real agent (Bitwarden,
// 1Password, or the system ssh-agent), so a cage can authenticate with a key
// that never leaves the host agent:
//
//   - list returns only the keys the policy allows;
//   - sign is refused for any other key, and (when the policy names hosts)
//     for any destination other than the allowed ones, checked through the
//     OpenSSH session-bind@openssh.com extension: the server host key's
//     signature over the SSH session id is verified and the host key is
//     mapped to hostnames via known host keys;
//   - agent-forwarding hops, key add/remove, lock/unlock and unknown
//     extensions are refused;
//   - optional per-sign approval;
//   - every list and sign is reported as an Event (never key material or
//     signatures).
//
// Dependency: golang.org/x/crypto (ssh, ssh/agent). It is the Go team's
// maintained SSH implementation; we need its public-key parsing, signature
// verification for every host key type OpenSSH uses (ed25519, ecdsa,
// rsa-sha2-*, certificates), SHA256 fingerprints, known_hosts parsing, and
// the agent client used to talk to the upstream agent. Reimplementing any of
// that by hand on a security boundary would be worse than taking the
// dependency. The agent wire protocol on the cage side is implemented here
// directly so every message type is under our control.
package sshagent
