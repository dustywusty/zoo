// Package services is zoo's model of things a cage can be granted besides
// web hosts: ports on the host, sidecars, remote TCP destinations and SSH
// agents. Cages reach every service through the keeper's gate, which only
// accepts connections from cages holding a grant.
package services

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// Kinds.
const (
	Host     = "host"
	Sidecar  = "sidecar"
	Remote   = "remote"
	SSHAgent = "ssh-agent"
)

// Reserved gate ports: squid, and the low range that low ports are moved to.
const (
	squidPort    = 3128
	lowPortShift = 20000
	sshAgentPort = 22022
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Kind reports which kind a service is.
func Kind(s config.Service) string {
	switch {
	case s.Host != "":
		return Host
	case s.Image != "":
		return Sidecar
	case s.Remote != "":
		return Remote
	case s.SSHAgent != "":
		return SSHAgent
	}
	return ""
}

// Target is where the service lives, for display and for the gate.
func Target(s config.Service) string {
	switch Kind(s) {
	case Host:
		return s.Host
	case Sidecar:
		return s.Image
	case Remote:
		return s.Remote
	case SSHAgent:
		return s.SSHAgent
	}
	return ""
}

// Port is the port cages connect to (on <name>.zoo).
func Port(s config.Service) int {
	if s.Port > 0 {
		return s.Port
	}
	switch Kind(s) {
	case Host, Remote:
		_, p, err := net.SplitHostPort(Target(s))
		if err == nil {
			n, _ := strconv.Atoi(p)
			return n
		}
	case SSHAgent:
		return sshAgentPort
	}
	return 0
}

// ListenPort is the gate's port for a service. The gate runs unprivileged,
// so ports below 1024 move up by 20000 (ssh to github.com:22 -> 20022).
func ListenPort(s config.Service) int {
	p := Port(s)
	if p > 0 && p < 1024 {
		return p + lowPortShift
	}
	return p
}

// Validate checks every service and that gate ports don't collide.
func Validate(all map[string]config.Service) error {
	used := map[int]string{squidPort: "the keeper's proxy"}
	for _, name := range Names(all) {
		s := all[name]
		if !nameRe.MatchString(name) {
			return fmt.Errorf("service %q: names are lowercase letters, digits and dashes", name)
		}
		set := 0
		for _, v := range []string{s.Host, s.Image, s.Remote, s.SSHAgent} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return fmt.Errorf("service %q: set exactly one of host, image, remote, ssh-agent", name)
		}
		if k := Kind(s); k == Host || k == Remote {
			if _, _, err := net.SplitHostPort(Target(s)); err != nil {
				return fmt.Errorf("service %q: %s must be host:port", name, k)
			}
		}
		if Kind(s) == Sidecar && s.Port == 0 {
			return fmt.Errorf("service %q: sidecars need port:", name)
		}
		lp := ListenPort(s)
		if lp <= 0 || lp > 65535 {
			return fmt.Errorf("service %q: no usable port", name)
		}
		if other, ok := used[lp]; ok {
			return fmt.Errorf("service %q: port %d is already used by %s; set port:", name, lp, other)
		}
		used[lp] = "service " + name
	}
	return nil
}

// Names returns service names in order.
func Names(all map[string]config.Service) []string {
	out := make([]string, 0, len(all))
	for k := range all {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- grants -------------------------------------------------------------------

// Grants maps cage name -> granted service names (explicit `zoo grant`s).
type Grants map[string][]string

// LoadGrants reads explicit grants, leaving out any whose time is up.
func LoadGrants() Grants {
	g := Grants{}
	b, err := os.ReadFile(paths.Grants())
	if err == nil {
		_ = json.Unmarshal(b, &g)
	}
	g.dropExpired(time.Now())
	return g
}

func (g Grants) Save() error {
	b, _ := json.MarshalIndent(g, "", "  ")
	return os.WriteFile(paths.Grants(), append(b, '\n'), 0o600)
}

// Add grants service to cage; reports whether it changed.
func (g Grants) Add(cage, service string) bool {
	for _, s := range g[cage] {
		if s == service {
			return false
		}
	}
	g[cage] = append(g[cage], service)
	sort.Strings(g[cage])
	return true
}

// Remove revokes an explicit grant; reports whether it changed.
func (g Grants) Remove(cage, service string) bool {
	var kept []string
	for _, s := range g[cage] {
		if s != service {
			kept = append(kept, s)
		}
	}
	changed := len(kept) != len(g[cage])
	if len(kept) == 0 {
		delete(g, cage)
	} else {
		g[cage] = kept
	}
	return changed
}

// Effective is a cage's grants: its repo's zoo.yaml services plus explicit
// grants, limited to services that exist.
func Effective(all map[string]config.Service, explicit Grants, cage string, repo []string) []string {
	set := map[string]bool{}
	for _, s := range append(append([]string{}, repo...), explicit[cage]...) {
		if _, ok := all[s]; ok {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// --- the gate's config ------------------------------------------------------------

// GateService is one listener in gate.json.
type GateService struct {
	Name   string            `json:"name"`
	Listen int               `json:"listen"`
	Kind   string            `json:"kind"`
	Target string            `json:"target"` // host:port the gate dials (bridge kinds: the service name)
	Allow  map[string]string `json:"allow"`  // cage IP -> cage name
}

// GateConfig is /etc/zoo/gate.json, read by zoo-gate in the keeper.
type GateConfig struct {
	Token    string        `json:"token"`
	Bridge   string        `json:"bridge"` // host:port of zood's host bridge, as the keeper dials it
	Services []GateService `json:"services"`
}

// CageRef is what the gate needs to know about a running cage.
type CageRef struct {
	Name   string
	IP     string
	Grants []string
}

// BuildGate renders the gate config. sidecarIP resolves a sidecar's address.
func BuildGate(all map[string]config.Service, cages []CageRef, token, bridge string, sidecarIP func(name string) string) GateConfig {
	gc := GateConfig{Token: token, Bridge: bridge}
	for _, name := range Names(all) {
		s := all[name]
		gs := GateService{Name: name, Listen: ListenPort(s), Kind: Kind(s), Allow: map[string]string{}}
		switch gs.Kind {
		case Remote:
			gs.Target = s.Remote
		case Sidecar:
			if ip := sidecarIP(name); ip != "" {
				gs.Target = net.JoinHostPort(ip, strconv.Itoa(s.Port))
			}
		case Host, SSHAgent:
			gs.Target = name // via the bridge
		}
		for _, c := range cages {
			if c.IP == "" {
				continue
			}
			for _, g := range c.Grants {
				if g == name {
					gs.Allow[c.IP] = c.Name
				}
			}
		}
		gc.Services = append(gc.Services, gs)
	}
	return gc
}

// WriteGate writes gate.json if it changed; reports whether it did.
func WriteGate(gc GateConfig) (bool, error) {
	b, _ := json.MarshalIndent(gc, "", "  ")
	b = append(b, '\n')
	old, _ := os.ReadFile(paths.GateConf())
	if string(old) == string(b) {
		return false, nil
	}
	tmp := paths.GateConf() + ".tmp"
	// Readable inside the keeper (squid's user runs the gate); the token
	// only authenticates the keeper to zood's bridge.
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, paths.GateConf())
}

// HostName is the name a cage uses for a service.
func HostName(name string) string { return name + ".zoo" }

// CageEnv returns the env hints for granted services ({host}, {port} expand).
func CageEnv(all map[string]config.Service, granted []string) []string {
	var out []string
	for _, name := range granted {
		s, ok := all[name]
		if !ok {
			continue
		}
		env := s.CageEnv
		if env == nil {
			env = defaultEnv(s)
		}
		for _, k := range sortedKeys(env) {
			v := strings.NewReplacer("{host}", HostName(name), "{port}", strconv.Itoa(ListenPort(s))).Replace(env[k])
			out = append(out, k+"="+v)
		}
	}
	return out
}

// defaultEnv guesses conventional variables from the service's port.
func defaultEnv(s config.Service) map[string]string {
	switch Port(s) {
	case 5432:
		return map[string]string{"PGHOST": "{host}", "PGPORT": "{port}"}
	case 6379:
		return map[string]string{"REDIS_URL": "redis://{host}:{port}"}
	case 3306:
		return map[string]string{"MYSQL_HOST": "{host}", "MYSQL_TCP_PORT": "{port}"}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
