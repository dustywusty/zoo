// Package cage plans and runs cages: one small VM per worktree.
package cage

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
)

const Prefix = "zoo-"

// Labels set on every cage.
const (
	LabelCage     = "zoo.cage"
	LabelWorktree = "zoo.worktree"
	LabelSize     = "zoo.size"
	LabelCPUs     = "zoo.cpus"
	LabelMemory   = "zoo.memory"
	LabelBackend  = "zoo.backend"
	LabelIso      = "zoo.isolation"
	LabelWeaker   = "zoo.weaker-isolation" // set when started below the isolation floor
	LabelImage    = "zoo.image"            // image the cage was created from
)

// Plan is everything needed to create a cage for one worktree.
type Plan struct {
	Name     string   // short name, e.g. api-3fa2c1
	ID       string   // container id, e.g. zoo-api-3fa2c1
	Root     string   // worktree root on the host (and in the guest)
	ExtraRW  []string // other host paths mounted rw (a worktree's main .git)
	Masks    []string // secret files hidden inside the cage
	ReadOnly []string // paths made read-only inside the cage
	Pin      []string // directories mounted onto themselves so they can't be renamed
	SizeName string
	Size     config.Size
}

// Root returns the git toplevel for dir, or dir itself outside a repo.
func Root(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top, nil
		}
	}
	return abs, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// NameFor derives a stable cage name from a worktree path.
func NameFor(root string) string {
	base := slugRe.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-")
	base = strings.Trim(base, "-")
	if len(base) > 24 {
		base = base[:24]
	}
	if base == "" {
		base = "cage"
	}
	sum := sha1.Sum([]byte(root))
	return base + "-" + hex.EncodeToString(sum[:])[:6]
}

// gitPaths returns the common .git dir and this worktree's git dir (absolute).
func gitPaths(root string) (common, gitDir string) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir").Output()
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", ""
	}
	return filepath.Clean(lines[0]), filepath.Clean(lines[1])
}

// pinsFor returns every directory between a mount root and a protected path.
// A rename moves a directory together with the read-only mounts inside it,
// so after `mv .git .git2` the agent could write a fresh .git/config the host
// would run. A directory that is itself a mount point can't be renamed.
func pinsFor(protected, roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range protected {
		var top string
		for _, r := range roots {
			if within(p, r) && len(r) > len(top) {
				top = r
			}
		}
		if top == "" {
			continue
		}
		for d := filepath.Dir(p); d != top && within(d, top); d = filepath.Dir(d) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Strings(out)
	return out
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// NewPlan builds the plan for a worktree.
func NewPlan(root string, g config.Global, p config.Project, sizeFlag string) (Plan, error) {
	sizeName := sizeFlag
	if sizeName == "" {
		sizeName = p.Size
	}
	sn, size, err := g.ResolveSize(sizeName)
	if err != nil {
		return Plan{}, err
	}
	name := NameFor(root)
	pl := Plan{Name: name, ID: Prefix + name, Root: root, SizeName: sn, Size: size}

	common, gitDir := gitPaths(root)
	if common != "" {
		if !within(common, root) {
			pl.ExtraRW = append(pl.ExtraRW, common)
		}
		// Make sure hooks/ exists so the agent can't create it.
		_ = os.MkdirAll(filepath.Join(common, "hooks"), 0o755)
		pl.ReadOnly = append(pl.ReadOnly, filepath.Join(common, "hooks"), filepath.Join(common, "config"))
		if gitDir != "" && gitDir != common {
			// A linked worktree: its .git file and gitdir pointers decide which
			// config the host's git reads.
			pl.ReadOnly = append(pl.ReadOnly,
				filepath.Join(root, ".git"),
				filepath.Join(gitDir, "config.worktree"),
				filepath.Join(gitDir, "commondir"))
		}
	}
	pl.Pin = pinsFor(pl.ReadOnly, append([]string{root}, pl.ExtraRW...))

	pl.Masks = FindMasks(root, append(append([]string{}, g.Masks...), p.Mask...), append(append([]string{}, g.Unmask...), p.Unmask...))
	return pl, nil
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "target": true, ".next": true, ".turbo": true, ".cache": true,
}

// FindMasks walks the worktree (depth 6, at most 256 hits) for secret-looking files.
func FindMasks(root string, masks, unmask []string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.Count(rel, string(filepath.Separator)) >= 6) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= 256 {
			return filepath.SkipAll
		}
		base := d.Name()
		if match(unmask, base) {
			return nil
		}
		if match(masks, base) || matchRel(masks, rel) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func match(patterns []string, name string) bool {
	for _, pat := range patterns {
		if strings.Contains(pat, "/") {
			continue
		}
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}

func matchRel(patterns []string, rel string) bool {
	for _, pat := range patterns {
		if !strings.Contains(pat, "/") {
			continue
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// ProxyEnv is the environment that points every tool at the keeper.
func ProxyEnv(keeperIP string) []string {
	u := keeper.ProxyURL(keeperIP)
	return []string{
		"HTTPS_PROXY=" + u, "HTTP_PROXY=" + u, "https_proxy=" + u, "http_proxy=" + u,
		"npm_config_proxy=" + u, "npm_config_https_proxy=" + u,
	}
}

// Spec is the backend's view of a plan.
func (pl Plan) Spec(g config.Global, keeperIP string, labels map[string]string) backend.CageSpec {
	l := map[string]string{
		LabelCage:     pl.Name,
		LabelWorktree: pl.Root,
		LabelSize:     pl.SizeName,
		LabelCPUs:     fmt.Sprint(pl.Size.CPUs),
		LabelMemory:   pl.Size.Memory,
		LabelImage:    g.CageImage,
	}
	for k, v := range labels {
		l[k] = v
	}
	mounts := []backend.Mount{{Source: pl.Root, Target: pl.Root}}
	for _, p := range pl.ExtraRW {
		mounts = append(mounts, backend.Mount{Source: p, Target: p})
	}
	return backend.CageSpec{
		ID:       pl.ID,
		Image:    g.CageImage,
		CPUs:     pl.Size.CPUs,
		Memory:   pl.Size.Memory,
		Pids:     pl.Size.Pids,
		Labels:   l,
		Env:      append([]string{"ZOO_CAGE=" + pl.Name}, ProxyEnv(keeperIP)...),
		Mounts:   mounts,
		Masks:    pl.Masks,
		ReadOnly: pl.ReadOnly,
		Pin:      pl.Pin,
	}
}

// Stale reports whether an existing cage predates the current image (and so
// may lack the mounts this zoo relies on, e.g. v0 cages' guard).
func Stale(st backend.Status, spec backend.CageSpec) bool {
	return st.Labels[LabelImage] != spec.Image
}

// Ensure creates or starts the cage. Returns true if it was (re)created.
// A stopped cage from an older image is recreated; a running one is left
// alone (agents may be inside) and reported by Stale.
func Ensure(ctx context.Context, b backend.Backend, spec backend.CageSpec) (bool, error) {
	st, exists, err := b.Get(ctx, spec.ID)
	if err != nil {
		return false, err
	}
	if exists && st.Running {
		return false, nil
	}
	if exists && Stale(st, spec) {
		if err := b.Delete(ctx, spec.ID); err != nil {
			return false, err
		}
		exists = false
	}
	if exists {
		if err := b.Start(ctx, spec.ID); err == nil {
			return false, waitRunning(ctx, b, spec.ID)
		}
		_ = b.Delete(ctx, spec.ID)
	}
	if err := b.CreateCage(ctx, spec); err != nil {
		return false, err
	}
	return true, waitRunning(ctx, b, spec.ID)
}

func waitRunning(ctx context.Context, b backend.Backend, id string) error {
	for i := 0; i < 40; i++ {
		if st, ok, _ := b.Get(ctx, id); ok && st.Running {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("cage %s did not reach running state", id)
}

// Info is a cage as listed for humans and the dashboard.
type Info struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Worktree  string `json:"worktree"`
	Size      string `json:"size"`
	CPUs      string `json:"cpus"`
	Memory    string `json:"memory"`
	Status    string `json:"status"`
	Running   bool   `json:"running"`
	IP        string `json:"ip"`
	Isolation string `json:"isolation"`
	Weaker    bool   `json:"weaker_isolation"` // started below the isolation floor
	Network   string `json:"network"`
}

// List returns all zoo cages.
func List(ctx context.Context, b backend.Backend) ([]Info, error) {
	all, err := b.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, c := range all {
		if c.ID == keeper.Name || c.Labels["zoo.role"] == "keeper" || c.Labels["zoo.role"] == "service" || strings.HasPrefix(c.ID, "zoo-svc-") {
			continue
		}
		network := ""
		for n, ip := range c.IPs {
			if ip == c.IP {
				network = n
			}
		}
		name := c.Labels[LabelCage]
		if name == "" {
			if !strings.HasPrefix(c.ID, Prefix) {
				continue
			}
			name = strings.TrimPrefix(c.ID, Prefix)
		}
		out = append(out, Info{
			Name: name, ID: c.ID, Worktree: c.Labels[LabelWorktree],
			Size: c.Labels[LabelSize], CPUs: c.Labels[LabelCPUs], Memory: c.Labels[LabelMemory],
			Status: c.State, Running: c.Running, IP: c.IP,
			Isolation: c.Labels[LabelIso], Weaker: c.Labels[LabelWeaker] == "1",
			Network: network,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Find resolves a user-supplied name (short name, id, or worktree path).
func Find(ctx context.Context, b backend.Backend, q string) (Info, error) {
	all, err := List(ctx, b)
	if err != nil {
		return Info{}, err
	}
	if abs, err := filepath.Abs(q); err == nil {
		if _, statErr := os.Stat(abs); statErr == nil {
			if root, err := Root(abs); err == nil {
				q = NameFor(root)
			}
		}
	}
	var hits []Info
	for _, c := range all {
		if c.Name == q || c.ID == q {
			return c, nil
		}
		if strings.HasPrefix(c.Name, q) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return Info{}, fmt.Errorf("%q matches %d cages; be more specific", q, len(hits))
	}
	return Info{}, fmt.Errorf("no cage %q (see zoo ls)", q)
}
