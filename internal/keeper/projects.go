package keeper

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/paths"
)

// ProjectAccess binds a running cage's address to its approved policy.
// Allow=nil inherits the global allowlist; [] denies all web destinations.
type ProjectAccess struct {
	IP    string   `json:"ip"`
	Allow []string `json:"allow"`
}

func projectAccessPath() string { return filepath.Join(paths.KeeperEtc(), "project-access.json") }

func ReadProjects() ([]ProjectAccess, error) {
	b, err := os.ReadFile(projectAccessPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []ProjectAccess
	err = json.Unmarshal(b, &rules)
	return rules, err
}

func appliedConfigPath() string    { return filepath.Join(paths.KeeperEtc(), "applied-config.sha256") }
func configDigest(b []byte) []byte { return []byte(fmt.Sprintf("%x", sha256.Sum256(b))) }

var policyHost = regexp.MustCompile(`^\.?[a-z0-9][a-z0-9.-]*[a-z0-9]$|^[a-z0-9]$`)

func projectBlock(rules []ProjectAccess) (string, error) {
	var block strings.Builder
	seen := map[string]bool{}
	for i, rule := range rules {
		ip := net.ParseIP(rule.IP)
		if ip == nil || seen[ip.String()] {
			return "", fmt.Errorf("invalid or repeated cage IP %q", rule.IP)
		}
		seen[ip.String()] = true
		fmt.Fprintf(&block, "acl zoo_project_%d src %s\n", i, ip.String())
		if rule.Allow == nil {
			fmt.Fprintf(&block, "http_access allow zoo_project_%d zoo_allowed\n", i)
		} else if len(rule.Allow) > 0 {
			var hosts []string
			for _, host := range rule.Allow {
				host = strings.ToLower(strings.TrimSpace(host))
				if !policyHost.MatchString(host) || strings.Contains(host, "..") {
					return "", fmt.Errorf("network.allow: invalid host %q; use a hostname or .example.com", host)
				}
				hosts = append(hosts, host)
			}
			fmt.Fprintf(&block, "acl zoo_project_hosts_%d dstdomain -n %s\n", i, strings.Join(prune(hosts), " "))
			fmt.Fprintf(&block, "http_access allow zoo_project_%d zoo_project_hosts_%d\n", i, i)
		}
		fmt.Fprintf(&block, "http_access deny zoo_project_%d\n", i)
	}
	// Unknown addresses never fall through to the global allowlist. A stale
	// cage list can briefly deny a new cage, but cannot widen its access.
	return block.String(), nil
}

// SyncProjects publishes all running cages, including those that inherit the
// global policy. Callers must reload the keeper before starting an agent.
func SyncProjects(rules []ProjectAccess) (bool, error) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].IP < rules[j].IP })
	if _, err := projectBlock(rules); err != nil {
		return false, err
	}
	if rules == nil {
		rules = []ProjectAccess{}
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return false, err
	}
	old, err := os.ReadFile(projectAccessPath())
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if !bytes.Equal(old, b) {
		if err := writeConfigAtomic(projectAccessPath(), b); err != nil {
			return false, err
		}
	}
	changed, err := WriteSquid(injectOn())
	if err != nil {
		return false, err
	}
	conf, err := os.ReadFile(paths.SquidConf())
	if err != nil {
		return false, err
	}
	applied, err := os.ReadFile(appliedConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return changed || !bytes.Equal(applied, configDigest(conf)), nil
}

func renderProjectSquid(inject bool) (string, error) {
	conf := RenderSquid(inject)
	b, err := os.ReadFile(projectAccessPath())
	if os.IsNotExist(err) {
		return conf, nil
	} // upgrades: initialized on first cage sync
	if err != nil {
		return "", err
	}
	var rules []ProjectAccess
	if err := json.Unmarshal(b, &rules); err != nil {
		return "", fmt.Errorf("read project network policies: %w", err)
	}
	block, err := projectBlock(rules)
	if err != nil {
		return "", err
	}
	return strings.Replace(conf, "http_access allow zoo_allowed\n", block, 1), nil
}

func writeConfigAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
