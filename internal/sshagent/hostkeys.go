package sshagent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

type staticHosts map[string][]string

func (m staticHosts) Hosts(fp string) []string { return m[fp] }

// Static returns HostKeys from a fingerprint -> hostnames map.
func Static(m map[string][]string) HostKeys {
	c := make(staticHosts, len(m))
	for fp, hs := range m {
		c[fp] = append([]string(nil), hs...)
	}
	return c
}

type multiHosts []HostKeys

func (m multiHosts) Hosts(fp string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range m {
		if k == nil {
			continue
		}
		for _, h := range k.Hosts(fp) {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// Multi combines several HostKeys; a fingerprint maps to the union of hosts.
func Multi(ks ...HostKeys) HostKeys { return multiHosts(ks) }

// KnownHostsFile parses OpenSSH known_hosts files. With no paths it reads
// ~/.ssh/known_hosts and /etc/ssh/ssh_known_hosts. Missing files are
// skipped. Hashed hostnames (|1|...) can't be reverse-mapped and are skipped,
// as are wildcard/negated patterns, @cert-authority lines and unparsable
// lines. A key listed under @revoked maps to no hosts, whatever other lines
// say. "[host]:port" entries map to "host".
func KnownHostsFile(paths ...string) (HostKeys, error) {
	if len(paths) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, ".ssh", "known_hosts"))
		}
		paths = append(paths, "/etc/ssh/ssh_known_hosts")
	}
	m := map[string][]string{}
	revoked := map[string]bool{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sshagent: known hosts: %w", err)
		}
		parseKnownHosts(data, m, revoked)
	}
	for fp := range revoked {
		delete(m, fp)
	}
	return staticHosts(m), nil
}

func parseKnownHosts(data []byte, m map[string][]string, revoked map[string]bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		marker, hosts, pk, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil || pk == nil {
			continue
		}
		fp := ssh.FingerprintSHA256(pk)
		switch marker {
		case "revoked":
			revoked[fp] = true
			continue
		case "":
		default: // cert-authority: not a host key
			continue
		}
		for _, h := range hosts {
			h = normalizeHost(h)
			if h == "" || contains(m[fp], h) {
				continue
			}
			m[fp] = append(m[fp], h)
		}
	}
}

func normalizeHost(h string) string {
	if strings.HasPrefix(h, "|") || strings.HasPrefix(h, "!") || strings.ContainsAny(h, "*?") {
		return ""
	}
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			h = h[1:i]
		}
	}
	return strings.ToLower(h)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// githubKnownHosts are GitHub's published SSH host keys, from
// https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
// (and https://api.github.com/meta "ssh_keys"). Published fingerprints:
//
//	SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s (RSA)
//	SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM (ECDSA)
//	SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU (Ed25519)
//
// TestGitHubHostKeys checks these keys hash to those fingerprints.
const githubKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// GitHubHostKeys maps GitHub's published host keys to "github.com".
func GitHubHostKeys() HostKeys {
	m := map[string][]string{}
	parseKnownHosts([]byte(githubKnownHosts), m, map[string]bool{})
	return staticHosts(m)
}
