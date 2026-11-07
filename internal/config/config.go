// Package config loads ~/.zoo/config.yaml (global) and zoo.yaml (per repo).
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/dusty/zoo/internal/paths"
)

// Size is a resource profile for a cage.
type Size struct {
	CPUs   int    `yaml:"cpus" json:"cpus"`
	Memory string `yaml:"memory" json:"memory"` // e.g. "2G"
	Pids   int    `yaml:"pids" json:"pids"`     // process limit
}

// Harness says how to launch an agent CLI inside a cage and which keychain
// items to inject as environment variables.
type Harness struct {
	Command string            `yaml:"command" json:"command"`
	Env     map[string]string `yaml:"env" json:"env"`     // ENV_VAR -> keychain item name
	Files   map[string]string `yaml:"files" json:"files"` // guest path (relative to agent home) -> keychain item name
	Allow   []string          `yaml:"allow" json:"allow"` // extra hosts this harness needs
}

// Global is ~/.zoo/config.yaml.
type Global struct {
	Port          int                `yaml:"port"`
	PreviewPort   *int               `yaml:"preview_port"` // nil = Port+2; 0 disables
	DefaultSize   string             `yaml:"default_size"`
	Sizes         map[string]Size    `yaml:"sizes"`
	Masks         []string           `yaml:"masks"`
	Unmask        []string           `yaml:"unmask"`
	Harnesses     map[string]Harness `yaml:"harnesses"`
	NtfyURL       string             `yaml:"ntfy_url"`
	BytesOutAlert int64              `yaml:"bytes_out_alert"` // per host per cage, bytes
	CageImage     string             `yaml:"cage_image"`
	KeeperImage   string             `yaml:"keeper_image"`
	Network       string             `yaml:"network"`

	// Backend picks the runtime: auto | apple | docker | podman.
	Backend string `yaml:"backend"`
	// IsolationFloor refuses cages weaker than this: vm | microvm |
	// sandboxed-container | container. Docker with runc is "container".
	IsolationFloor string `yaml:"isolation_floor"`
	Docker         Docker `yaml:"docker"`

	// Services are things cages may be granted besides web hosts.
	Services map[string]Service `yaml:"services"`

	// Git is the default for what cages may do with .git: rw | ro | none.
	Git string `yaml:"git"`

	// Cache is the default package cache: project | shared | off.
	Cache string `yaml:"cache"`

	// Image and Dockerfile are the default for zoo.yaml's image: and
	// dockerfile: (a cage built from your own image; see Project).
	Image      string `yaml:"image"`
	Dockerfile string `yaml:"dockerfile"`

	// Credentials stay on the host; cages get a placeholder that the keeper
	// swaps for the real value on the way to the credential's hosts.
	Credentials map[string]Credential `yaml:"credentials"`
}

// Credential is a secret the keeper adds to requests for some hosts, plus
// what those hosts may be asked to do. Cages only ever see a placeholder.
type Credential struct {
	Hosts  []string `yaml:"hosts" json:"hosts"`   // where the real value may go (exact names)
	Secret string   `yaml:"secret" json:"secret"` // keychain item, bw:item[/field] or op://vault/item/field
	// Env names variables that hold the placeholder in granted cages
	// (GH_TOKEN, GITHUB_TOKEN, ...).
	Env []string `yaml:"env" json:"env,omitempty"`
	// Prefix goes in front of the placeholder, for tools that check a
	// token's shape (e.g. "ghp_").
	Prefix string `yaml:"prefix" json:"prefix,omitempty"`
	// Git makes git in granted cages use the credential for https remotes on
	// Hosts (user x-access-token), and fetch ssh remotes there over https.
	Git bool `yaml:"git" json:"git,omitempty"`
	// AllCages grants it to every cage (model keys for harnesses).
	AllCages bool `yaml:"all_cages" json:"all_cages,omitempty"`

	// Policy on Hosts, for every request (with or without the credential):
	ReadOnly bool     `yaml:"readonly" json:"readonly,omitempty"` // only reads: GET/HEAD, GraphQL queries, git fetch
	Push     []string `yaml:"push" json:"push,omitempty"`         // git push allowed to these branches ("agent/*"); implies nothing else
	Repos    []string `yaml:"repos" json:"repos,omitempty"`       // owner/repo the API and git may touch (GitHub-style paths)
}

// Service is a host port, a sidecar, a remote TCP destination or an SSH
// agent that cages reach through the keeper's gate once granted. Exactly
// one of Host, Image, Remote, SSHAgent is set.
type Service struct {
	Host     string            `yaml:"host" json:"host,omitempty"`           // "127.0.0.1:5432" on this machine
	Image    string            `yaml:"image" json:"image,omitempty"`         // sidecar image, e.g. postgres:16
	Remote   string            `yaml:"remote" json:"remote,omitempty"`       // "github.com:22"
	SSHAgent string            `yaml:"ssh-agent" json:"ssh_agent,omitempty"` // bitwarden | 1password | system | socket path
	Port     int               `yaml:"port" json:"port,omitempty"`           // port cages use (default: the target's)
	Env      map[string]string `yaml:"env" json:"env,omitempty"`             // sidecar env (values may be secret refs)
	Volume   string            `yaml:"volume" json:"volume,omitempty"`       // sidecar data dir, kept in ~/.zoo/services/<name>
	Keys     []string          `yaml:"keys" json:"keys,omitempty"`           // ssh-agent: allowed key fingerprints or comments
	Hosts    []string          `yaml:"hosts" json:"hosts,omitempty"`         // ssh-agent: allowed destinations
	Approve  bool              `yaml:"approve" json:"approve,omitempty"`     // ssh-agent: ask on the dashboard before each use
	CageEnv  map[string]string `yaml:"cage_env" json:"cage_env,omitempty"`   // env for granted cages; {host} and {port} expand
}

// Docker configures the docker/podman backend.
type Docker struct {
	Bin     string `yaml:"bin"`     // docker (default) or a path; podman has its own backend name
	Runtime string `yaml:"runtime"` // OCI runtime: "" (daemon default) | runc | runsc | kata
}

// Project is zoo.yaml at a worktree root. Everything is optional.
type Project struct {
	Size     string   `yaml:"size"`
	Harness  string   `yaml:"harness"`
	Mask     []string `yaml:"mask"`
	Unmask   []string `yaml:"unmask"`
	Services []string `yaml:"services"` // services and credentials granted to this repo's cage
	Git      string   `yaml:"git"`      // rw (default) | ro | none
	Mounts   []string `yaml:"mounts"`   // further worktrees in the same cage (relative to this one, or ~/...)
	Cache    string   `yaml:"cache"`    // package cache: project (default) | shared | off
	// Image is a base image for this repo's cage (zoo adds its user and
	// init on top). Dockerfile is a Dockerfile in the worktree to build that
	// base from instead; it's built only once you approve its contents with
	// `zoo build --project`, with an empty build context.
	Image      string `yaml:"image"`
	Dockerfile string `yaml:"dockerfile"`
}

func Defaults() Global {
	return Global{
		Port:        7777,
		DefaultSize: "small",
		Sizes: map[string]Size{
			"tiny":   {CPUs: 1, Memory: "1G", Pids: 512},
			"small":  {CPUs: 2, Memory: "2G", Pids: 1024},
			"medium": {CPUs: 4, Memory: "4G", Pids: 2048},
			"large":  {CPUs: 6, Memory: "8G", Pids: 4096},
		},
		Masks:  []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "id_rsa*", "id_ed25519*", ".npmrc", ".pypirc", ".netrc"},
		Unmask: []string{".env.example", ".env.sample", ".env.template", "*.pub"},
		Harnesses: map[string]Harness{
			"claude": {Command: "claude", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "claude"}},
			"codex":  {Command: "codex", Env: map[string]string{"OPENAI_API_KEY": "codex"}, Files: map[string]string{".codex/auth.json": "codex-auth"}},
			"shell":  {Command: "bash"},
		},
		BytesOutAlert: 5 << 20,
		// Bump the tag when the images or the mounts they expect change:
		// zoo rebuilds missing images and replaces instances on an old tag.
		CageImage:   "zoo-cage:m4",
		KeeperImage: "zoo-keeper:m4",
		Network:     "zoo-cages",
		Credentials: map[string]Credential{
			// Used by harnesses whose env says "cred:claude" / "cred:codex".
			"claude": {Hosts: []string{"api.anthropic.com"}, Secret: "claude", AllCages: true, Prefix: "sk-ant-oat01-"},
			"codex":  {Hosts: []string{"api.openai.com"}, Secret: "codex", AllCages: true, Prefix: "sk-"},
		},
		Backend:        "auto",
		IsolationFloor: "microvm",
	}
}

// LoadGlobal reads ~/.zoo/config.yaml over the defaults. A missing file is fine.
func LoadGlobal() (Global, error) {
	g := Defaults()
	b, err := os.ReadFile(paths.Config())
	if os.IsNotExist(err) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	var file Global
	if err := yaml.Unmarshal(b, &file); err != nil {
		return g, fmt.Errorf("%s: %w", paths.Config(), err)
	}
	merge(&g, file)
	return g, nil
}

func merge(g *Global, f Global) {
	if f.Port != 0 {
		g.Port = f.Port
	}
	if f.PreviewPort != nil {
		g.PreviewPort = f.PreviewPort
	}
	if f.DefaultSize != "" {
		g.DefaultSize = f.DefaultSize
	}
	for k, v := range f.Sizes {
		g.Sizes[k] = v
	}
	if f.Masks != nil {
		g.Masks = f.Masks
	}
	if f.Unmask != nil {
		g.Unmask = f.Unmask
	}
	for k, v := range f.Harnesses {
		g.Harnesses[k] = v
	}
	if f.NtfyURL != "" {
		g.NtfyURL = f.NtfyURL
	}
	if f.BytesOutAlert != 0 {
		g.BytesOutAlert = f.BytesOutAlert
	}
	if f.CageImage != "" {
		g.CageImage = f.CageImage
	}
	if f.KeeperImage != "" {
		g.KeeperImage = f.KeeperImage
	}
	if f.Network != "" {
		g.Network = f.Network
	}
	if f.Backend != "" {
		g.Backend = f.Backend
	}
	if f.IsolationFloor != "" {
		g.IsolationFloor = f.IsolationFloor
	}
	if g.Credentials == nil {
		g.Credentials = map[string]Credential{}
	}
	for k, v := range f.Credentials {
		g.Credentials[k] = v
	}
	if f.Git != "" {
		g.Git = f.Git
	}
	if f.Cache != "" {
		g.Cache = f.Cache
	}
	if f.Image != "" {
		g.Image = f.Image
	}
	if f.Dockerfile != "" {
		g.Dockerfile = f.Dockerfile
	}
	if f.Docker.Bin != "" {
		g.Docker.Bin = f.Docker.Bin
	}
	if f.Docker.Runtime != "" {
		g.Docker.Runtime = f.Docker.Runtime
	}
	if g.Services == nil {
		g.Services = map[string]Service{}
	}
	for k, v := range f.Services {
		g.Services[k] = v
	}
}

// ResolveSize picks a size by name, or parses "cpus:memory" like "3:6G".
func (g Global) ResolveSize(name string) (string, Size, error) {
	if name == "" {
		name = g.DefaultSize
	}
	if s, ok := g.Sizes[name]; ok {
		if s.Pids <= 0 {
			s.Pids = defaultPids(s.CPUs)
		}
		return name, s, nil
	}
	if cpu, mem, ok := strings.Cut(name, ":"); ok {
		var n int
		if _, err := fmt.Sscanf(cpu, "%d", &n); err == nil && n > 0 && mem != "" {
			return "custom", Size{CPUs: n, Memory: strings.ToUpper(mem), Pids: defaultPids(n)}, nil
		}
	}
	names := make([]string, 0, len(g.Sizes))
	for k := range g.Sizes {
		names = append(names, k)
	}
	sort.Strings(names)
	return "", Size{}, fmt.Errorf("unknown size %q (have: %s, or cpus:memory like 3:6G)", name, strings.Join(names, ", "))
}

// defaultPids scales the process limit with CPUs, as the built-in sizes do.
func defaultPids(cpus int) int { return max(512, cpus*512) }

// WriteDefaultIfMissing writes a commented starter config.
func WriteDefaultIfMissing() error {
	if _, err := os.Stat(paths.Config()); err == nil {
		return nil
	}
	return os.WriteFile(paths.Config(), []byte(starter), 0o600)
}

const starter = `# zoo global config. Everything here is optional; defaults are shown.
# backend: auto              # auto | apple | docker | podman
# isolation_floor: microvm   # refuse cages weaker than this. Docker with runc is
#                            # "container" (shared kernel): set "container" to allow it
# docker: { bin: docker, runtime: "" }   # runtime: runsc (gVisor) or kata for stronger isolation
# port: 7777                 # dashboard, bound to 127.0.0.1 only
# preview_port: 7779         # dev-server previews at http://<cage>.localhost:7779 (0 = off)
# default_size: small
# sizes:
#   tiny:   { cpus: 1, memory: 1G, pids: 512 }
#   small:  { cpus: 2, memory: 2G, pids: 1024 }
#   medium: { cpus: 4, memory: 4G, pids: 2048 }
#   large:  { cpus: 6, memory: 8G, pids: 4096 }
# masks: [".env", ".env.*", "*.pem", "*.key", "*.p12", "id_rsa*", "id_ed25519*", ".npmrc", ".pypirc", ".netrc"]
# unmask: [".env.example", ".env.sample", ".env.template", "*.pub"]
# git: rw                   # what cages may do with .git: rw (commit; hooks/config read-only),
#                            # ro (read history only) or none (no .git). zoo.yaml can set it per repo
# cache: project             # package caches (npm, pnpm, yarn, pip, uv, go, cargo, bun) kept
#                            # across zoo reset: project (one per cage), shared (one for every cage
#                            # that picks it: less disk, but a compromised cage can poison packages
#                            # the others install) or off. zoo.yaml can set it per repo
# image: python:3.12-slim    # build cages from your own image instead of zoo's (zoo adds its
#                            # agent user, init, bash/git/socat/curl; agent CLIs are yours to add)
# dockerfile: .zoo/Dockerfile   # or from a Dockerfile in each worktree, built with an empty
#                            # context once you approve it (zoo build --project). zoo.yaml wins
# bytes_out_alert: 5242880   # alert when one cage sends this much to one host
# ntfy_url: https://ntfy.sh/your-private-topic
# services:                  # things cages can be granted (zoo grant <cage> <service>)
#   pg:     { host: 127.0.0.1:5432 }                  # on this machine; reached as pg.zoo:5432
#   redis:  { image: redis:7, port: 6379 }            # a sidecar zoo runs
#   gh-ssh: { remote: github.com:22 }                 # non-HTTP egress
#   keys:   { ssh-agent: bitwarden, keys: [github], hosts: [github.com] }
# credentials:              # secrets that never enter a cage: cages get a placeholder and the
#                            # keeper swaps in the real value for these hosts only (zoo grant)
#   github:
#     hosts: [github.com, api.github.com]
#     secret: github-pat      # zoo keys set github-pat (a fine-grained token)
#     env: [GH_TOKEN, GITHUB_TOKEN]
#     git: true               # git over https uses it; ssh remotes are fetched over https
#     readonly: true          # reads only: GET, GraphQL queries, git fetch...
#     push: ["agent/*"]       # ...plus git push to these branches
#     repos: [you/app]        # and only these repositories
# harnesses:
#   claude:                   # keep the model token in the keeper too:
#     command: claude
#     env: { CLAUDE_CODE_OAUTH_TOKEN: "cred:claude" }
#   pi:
#     command: pi
#     env: { ANTHROPIC_API_KEY: anthropic }   # ENV_VAR: keychain item (zoo keys set anthropic)
`
