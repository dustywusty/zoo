package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// SidecarPrefix names sidecar instances: zoo-svc-<service>.
const SidecarPrefix = "zoo-svc-"

// Token authenticates the keeper's gate to zood's host bridge.
func Token() (string, error) {
	p := paths.GateToken()
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	return t, os.WriteFile(p, []byte(t+"\n"), 0o600)
}

// BridgePort is zood's host bridge port: the dashboard port + 1.
func BridgePort(g config.Global) int { return g.Port + 1 }

// Grants returns each running cage's effective grants (repo zoo.yaml ∪ zoo grant).
func GrantsFor(g config.Global, cages []cage.Info) map[string][]string {
	explicit := LoadGrants()
	out := map[string][]string{}
	for _, c := range cages {
		var repo []string
		if c.Worktree != "" {
			if p, err := config.LoadProject(c.Worktree); err == nil {
				repo = p.Services
			}
		}
		out[c.Name] = Effective(g.Services, explicit, c.Name, repo)
	}
	return out
}

// Sync starts sidecars that running cages are granted and rewrites the
// gate's config. Returns each cage's grants.
func Sync(ctx context.Context, b backend.Backend, g config.Global, cages []cage.Info) (map[string][]string, error) {
	if err := Validate(g.Services); err != nil {
		return nil, err
	}
	grants := GrantsFor(g, cages)
	wanted := map[string]bool{}
	var refs []CageRef
	for _, c := range cages {
		if !c.Running {
			continue
		}
		refs = append(refs, CageRef{Name: c.Name, IP: c.IP, Grants: grants[c.Name]})
		for _, s := range grants[c.Name] {
			wanted[s] = true
		}
	}
	var errs []string
	sidecarIPs := map[string]string{}
	for _, name := range Names(g.Services) {
		s := g.Services[name]
		if Kind(s) != Sidecar {
			continue
		}
		id := SidecarPrefix + name
		if wanted[name] {
			ip, err := b.EnsureSidecar(ctx, sidecarSpec(name, s))
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			sidecarIPs[name] = ip
		} else if st, ok, _ := b.Get(ctx, id); ok && st.Running {
			sidecarIPs[name] = st.IPs[backend.ServicesNetwork]
		}
	}
	tok, err := Token()
	if err != nil {
		return grants, err
	}
	bridge := ""
	if _, dial, err := b.HostBridge(ctx); err == nil {
		bridge = net.JoinHostPort(dial, strconv.Itoa(BridgePort(g)))
	}
	gc := BuildGate(g.Services, refs, tok, bridge, func(n string) string { return sidecarIPs[n] })
	if _, err := WriteGate(gc); err != nil {
		return grants, err
	}
	if len(errs) > 0 {
		return grants, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return grants, nil
}

func sidecarSpec(name string, s config.Service) backend.SidecarSpec {
	spec := backend.SidecarSpec{
		ID:     SidecarPrefix + name,
		Image:  s.Image,
		Labels: map[string]string{"zoo.role": "service", "zoo.service": name},
	}
	for _, k := range sortedKeys(s.Env) {
		spec.Env = append(spec.Env, k+"="+s.Env[k])
	}
	if s.Volume != "" {
		dir := paths.ServiceData(name)
		_ = os.MkdirAll(dir, 0o700)
		spec.Mounts = append(spec.Mounts, backend.Mount{Source: dir, Target: s.Volume})
	}
	return spec
}

// Apply writes service names into a running cage's /etc/hosts (and an SSH
// config for remote port-22 services) as guest root, which owns those files;
// the agent user can't change them.
func Apply(ctx context.Context, b backend.Backend, g config.Global, cageID, keeperIP string, granted []string) error {
	var hosts, sshConf strings.Builder
	for _, name := range granted {
		s, ok := g.Services[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&hosts, "%s %s # zoo-service\n", keeperIP, HostName(name))
		if Kind(s) == Remote {
			if h, p, err := net.SplitHostPort(s.Remote); err == nil && p == "22" {
				fmt.Fprintf(&sshConf, "Host %s\n  HostName %s\n  Port %d\n  HostKeyAlias %s\n\n", h, HostName(name), ListenPort(s), h)
			}
		}
	}
	script := `set -e
umask 022
tmp=$(mktemp)
grep -v ' # zoo-service$' /etc/hosts > "$tmp" || true
printf '%s' "$1" >> "$tmp"
cat "$tmp" > /etc/hosts   # in place: /etc/hosts may be a bind mount
rm -f "$tmp"
if [ -d /etc/ssh/ssh_config.d ]; then printf '%s' "$2" > /etc/ssh/ssh_config.d/zoo.conf; chmod 0644 /etc/ssh/ssh_config.d/zoo.conf; fi`
	var errb bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: cageID, User: "root", NoStdin: true,
		Cmd:    []string{"sh", "-c", script, "sh", hosts.String(), sshConf.String()},
		Stdout: &errb, Stderr: &errb,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("update service names in %s: %s", cageID, strings.TrimSpace(errb.String()))
	}
	return nil
}

// SessionEnv is the env a session in a cage gets for its grants.
func SessionEnv(g config.Global, granted []string) []string {
	env := CageEnv(g.Services, granted)
	for _, name := range granted {
		if s, ok := g.Services[name]; ok && Kind(s) == SSHAgent {
			// zoo-session relays a local socket to this.
			env = append(env, "ZOO_SSH_AGENT="+HostName(name)+":"+strconv.Itoa(ListenPort(s)))
			break
		}
	}
	return env
}
