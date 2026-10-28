// Package config loads ~/.zoo/config.yaml (global) and zoo.yaml (per repo).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/dusty/zoo/internal/paths"
)

// Size is a resource profile for a cage.
type Size struct {
	CPUs   int    `yaml:"cpus" json:"cpus"`
	Memory string `yaml:"memory" json:"memory"` // e.g. "2G"
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
}

// Project is zoo.yaml at a worktree root. Everything is optional.
type Project struct {
	Size    string   `yaml:"size"`
	Harness string   `yaml:"harness"`
	Mask    []string `yaml:"mask"`
	Unmask  []string `yaml:"unmask"`
}

func Defaults() Global {
	return Global{
		Port:        7777,
		DefaultSize: "small",
		Sizes: map[string]Size{
			"tiny":   {CPUs: 1, Memory: "1G"},
			"small":  {CPUs: 2, Memory: "2G"},
			"medium": {CPUs: 4, Memory: "4G"},
			"large":  {CPUs: 6, Memory: "8G"},
		},
		Masks:  []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "id_rsa*", "id_ed25519*", ".npmrc", ".pypirc", ".netrc"},
		Unmask: []string{".env.example", ".env.sample", ".env.template", "*.pub"},
		Harnesses: map[string]Harness{
			"claude": {Command: "claude", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "claude"}},
			"codex":  {Command: "codex", Env: map[string]string{"OPENAI_API_KEY": "codex"}, Files: map[string]string{".codex/auth.json": "codex-auth"}},
			"shell":  {Command: "bash"},
		},
		BytesOutAlert: 5 << 20,
		CageImage:     "zoo-cage:latest",
		KeeperImage:   "zoo-keeper:latest",
		Network:       "zoo-cages",
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
}

// LoadProject reads <root>/zoo.yaml if present.
func LoadProject(root string) (Project, error) {
	var p Project
	b, err := os.ReadFile(filepath.Join(root, "zoo.yaml"))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("zoo.yaml: %w", err)
	}
	return p, nil
}

// ResolveSize picks a size by name, or parses "cpus:memory" like "3:6G".
func (g Global) ResolveSize(name string) (string, Size, error) {
	if name == "" {
		name = g.DefaultSize
	}
	if s, ok := g.Sizes[name]; ok {
		return name, s, nil
	}
	if cpu, mem, ok := strings.Cut(name, ":"); ok {
		var n int
		if _, err := fmt.Sscanf(cpu, "%d", &n); err == nil && n > 0 && mem != "" {
			return "custom", Size{CPUs: n, Memory: strings.ToUpper(mem)}, nil
		}
	}
	names := make([]string, 0, len(g.Sizes))
	for k := range g.Sizes {
		names = append(names, k)
	}
	sort.Strings(names)
	return "", Size{}, fmt.Errorf("unknown size %q (have: %s, or cpus:memory like 3:6G)", name, strings.Join(names, ", "))
}

// WriteDefaultIfMissing writes a commented starter config.
func WriteDefaultIfMissing() error {
	if _, err := os.Stat(paths.Config()); err == nil {
		return nil
	}
	return os.WriteFile(paths.Config(), []byte(starter), 0o600)
}

const starter = `# zoo global config. Everything here is optional; defaults are shown.
# port: 7777                 # dashboard, bound to 127.0.0.1 only
# default_size: small
# sizes:
#   tiny:   { cpus: 1, memory: 1G }
#   small:  { cpus: 2, memory: 2G }
#   medium: { cpus: 4, memory: 4G }
#   large:  { cpus: 6, memory: 8G }
# masks: [".env", ".env.*", "*.pem", "*.key", "*.p12", "id_rsa*", "id_ed25519*", ".npmrc", ".pypirc", ".netrc"]
# unmask: [".env.example", ".env.sample", ".env.template", "*.pub"]
# bytes_out_alert: 5242880   # alert when one cage sends this much to one host
# ntfy_url: https://ntfy.sh/your-private-topic
# harnesses:
#   pi:
#     command: pi
#     env: { ANTHROPIC_API_KEY: anthropic }   # ENV_VAR: keychain item (zoo keys set anthropic)
`
