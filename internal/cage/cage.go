// Package cage plans and runs cages: one small VM per worktree.
package cage

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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
	"github.com/dusty/zoo/internal/paths"
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
	LabelMounts   = "zoo.mounts"           // JSON list of further worktrees mounted (zoo.yaml mounts:)
	LabelLayout   = "zoo.layout"           // hash of mounts and guards; a change needs a new cage
)

// Plan is everything needed to create a cage for one worktree.
type Plan struct {
	Name     string   // short name, e.g. api-3fa2c1
	ID       string   // container id, e.g. zoo-api-3fa2c1
	Root     string   // worktree root on the host (and in the guest)
	Extra    []string // further worktrees mounted rw at the same path (zoo.yaml mounts:)
	ExtraRW  []string // other host paths mounted rw (a worktree's main .git)
	Masks    []string // secret files hidden inside the cage
	ReadOnly []string // paths made read-only inside the cage
	Pin      []string // directories mounted onto themselves so they can't be renamed
	ReopenRW []string // dirs inside a read-only dir made writable again (this worktree's gitdir)
	// ExtraMounts are further mounts: a git dir outside the worktree shown
	// read-only (git: ro), or an empty directory over one (git: none).
	ExtraMounts []backend.Mount
	Git         string // rw | ro | none
	SizeName    string
	Size        config.Size
}

// Roots is every worktree in the plan: the main one, then zoo.yaml mounts.
func (pl Plan) Roots() []string { return append([]string{pl.Root}, pl.Extra...) }

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

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Git modes: what a cage can do with the repository's .git.
const (
	GitRW   = "rw"   // commit, branch, rebase (default); hooks and config stay read-only
	GitRO   = "ro"   // read history, can't change anything in .git
	GitNone = "none" // no .git in the cage at all
)

// GitMode resolves zoo.yaml's git: over the global default.
func GitMode(g config.Global, p config.Project) (string, error) {
	m := strings.ToLower(strings.TrimSpace(p.Git))
	if m == "" {
		m = strings.ToLower(strings.TrimSpace(g.Git))
	}
	switch m {
	case "", GitRW:
		return GitRW, nil
	case GitRO, GitNone:
		return m, nil
	}
	return "", fmt.Errorf("git: %q isn't a mode (rw, ro, none)", m)
}

// guardGit adds the mounts that keep one repository's .git from running
// anything on the host. root is a worktree mounted rw in the cage.
func (pl *Plan) guardGit(root, mode string) {
	common, gitDir := gitPaths(root)
	if common == "" {
		return
	}
	outside := !within(common, root)
	switch mode {
	case GitNone:
		// An empty read-only directory over the git dir; a linked
		// worktree's .git file reads as empty. A git dir outside the
		// worktree isn't mounted at all.
		if !outside {
			pl.ExtraMounts = append(pl.ExtraMounts, backend.Mount{Source: paths.EmptyDir(), Target: common, ReadOnly: true})
		}
		if gitDir != common {
			pl.Masks = append(pl.Masks, filepath.Join(root, ".git"))
		}
		return
	case GitRO:
		if outside {
			pl.ExtraMounts = append(pl.ExtraMounts, backend.Mount{Source: common, Target: common, ReadOnly: true})
		} else {
			pl.ReadOnly = append(pl.ReadOnly, common)
		}
		if gitDir != common {
			pl.ReadOnly = append(pl.ReadOnly, filepath.Join(root, ".git"))
		}
		pl.ReadOnly = append(pl.ReadOnly, WritableGitPaths(root)...)
		return
	}
	if outside {
		pl.ExtraRW = append(pl.ExtraRW, common)
	}
	// Everything under the common dir that decides what the host's git
	// runs, for this worktree or any other:
	//   hooks/, config      this repo
	//   worktrees/          every linked worktree's commondir and
	//                       config.worktree: rewriting another worktree's
	//                       commondir points the host's git at a config
	//                       the agent wrote
	//   modules/            submodules' own config and hooks
	// Make sure the dirs exist so the agent can't create them.
	for _, d := range []string{"hooks", "worktrees", "modules"} {
		_ = os.MkdirAll(filepath.Join(common, d), 0o755)
	}
	pl.ReadOnly = append(pl.ReadOnly,
		filepath.Join(common, "hooks"), filepath.Join(common, "config"),
		filepath.Join(common, "worktrees"), filepath.Join(common, "modules"))
	if gitDir != "" && gitDir != common {
		// A linked worktree: its own gitdir (HEAD, index) lives under
		// worktrees/ and must stay writable, but its .git file and gitdir
		// pointers decide which config the host's git reads.
		if within(gitDir, filepath.Join(common, "worktrees")) {
			pl.ReopenRW = append(pl.ReopenRW, gitDir)
		}
		// A linked worktree keeps its submodules' git dirs in its own
		// gitdir, not the common one.
		_ = os.MkdirAll(filepath.Join(gitDir, "modules"), 0o755)
		pl.ReadOnly = append(pl.ReadOnly,
			filepath.Join(gitDir, "modules"),
			filepath.Join(root, ".git"),
			filepath.Join(gitDir, "config.worktree"),
			filepath.Join(gitDir, "commondir"))
		// Absent, the agent could create it in its writable gitdir and
		// the host's git would read it once extensions.worktreeConfig is
		// on. An empty one changes nothing and can be mounted read-only.
		if cw := filepath.Join(gitDir, "config.worktree"); !exists(cw) {
			_ = os.WriteFile(cw, nil, 0o644)
		}
	}
	// Hooks dirs, included config and filter scripts that git config points
	// at inside the worktree would run on the host after the agent edits
	// them (core.hooksPath=.husky is the common case).
	pl.ReadOnly = append(pl.ReadOnly, WritableGitPaths(root)...)
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

	mode, err := GitMode(g, p)
	if err != nil {
		return Plan{}, err
	}
	pl.Git = mode
	pl.guardGit(root, mode)
	extra, err := ResolveMounts(root, p.Mounts)
	if err != nil {
		return Plan{}, err
	}
	for _, m := range extra {
		pl.Extra = append(pl.Extra, m)
		if top, err := Root(m); err == nil && top == m {
			pl.guardGit(m, mode)
		}
	}
	// A directory already mounted (read-only or reopened) can't be renamed
	// either, and mounting it again rw as a pin would undo the read-only one.
	mounted := map[string]bool{}
	for _, p := range append(append([]string{}, pl.ReadOnly...), pl.ReopenRW...) {
		mounted[p] = true
	}
	for _, d := range pinsFor(pl.ReadOnly, append(append([]string{root}, pl.Extra...), pl.ExtraRW...)) {
		if !mounted[d] {
			pl.Pin = append(pl.Pin, d)
		}
	}

	masks := append(append([]string{}, g.Masks...), p.Mask...)
	unmask := append(append([]string{}, g.Unmask...), p.Unmask...)
	for _, r := range append([]string{root}, pl.Extra...) {
		pl.Masks = append(pl.Masks, FindMasks(r, masks, unmask)...)
	}
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
	if len(pl.Extra) > 0 {
		b, _ := json.Marshal(pl.Extra)
		l[LabelMounts] = string(b)
	}
	for k, v := range labels {
		l[k] = v
	}
	mounts := []backend.Mount{{Source: pl.Root, Target: pl.Root}}
	for _, p := range pl.Extra {
		mounts = append(mounts, backend.Mount{Source: p, Target: p})
	}
	for _, p := range pl.ExtraRW {
		mounts = append(mounts, backend.Mount{Source: p, Target: p})
	}
	mounts = append(mounts, pl.ExtraMounts...)
	spec := backend.CageSpec{
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
		ReopenRW: pl.ReopenRW,
	}
	spec.Labels[LabelLayout] = Layout(spec)
	return spec
}

// Layout fingerprints what a cage can't change while it exists: its mounts
// and guards. Masks are left out (they're fixed at creation by design).
func Layout(spec backend.CageSpec) string {
	h := sha1.New()
	for _, m := range spec.Mounts {
		fmt.Fprintf(h, "m %s %s %v\n", m.Source, m.Target, m.ReadOnly)
	}
	for _, list := range [][]string{spec.ReadOnly, spec.Pin, spec.ReopenRW} {
		for _, p := range list {
			fmt.Fprintf(h, "p %s\n", p)
		}
		fmt.Fprintln(h, "--")
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Changed reports why an existing cage no longer matches the spec, or "".
func Changed(st backend.Status, spec backend.CageSpec) string {
	switch {
	case st.Labels[LabelImage] != spec.Image:
		return "image"
	case st.Labels[LabelLayout] != "" && st.Labels[LabelLayout] != spec.Labels[LabelLayout]:
		return "layout"
	}
	return ""
}

// Stale reports whether an existing cage predates the current image (and so
// may lack the mounts this zoo relies on, e.g. v0 cages' guard).
func Stale(st backend.Status, spec backend.CageSpec) bool {
	return Changed(st, spec) != ""
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
	Name      string   `json:"name"`
	ID        string   `json:"id"`
	Worktree  string   `json:"worktree"`
	Size      string   `json:"size"`
	CPUs      string   `json:"cpus"`
	Memory    string   `json:"memory"`
	Status    string   `json:"status"`
	Running   bool     `json:"running"`
	IP        string   `json:"ip"`
	Isolation string   `json:"isolation"`
	Weaker    bool     `json:"weaker_isolation"` // started below the isolation floor
	Network   string   `json:"network"`
	Mounts    []string `json:"mounts,omitempty"` // further worktrees (zoo.yaml mounts:)
}

// Roots is every worktree mounted in the cage: the main one, then the rest.
func (c Info) Roots() []string {
	if c.Worktree == "" {
		return c.Mounts
	}
	return append([]string{c.Worktree}, c.Mounts...)
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
		var extra []string
		if m := c.Labels[LabelMounts]; m != "" {
			_ = json.Unmarshal([]byte(m), &extra)
		}
		out = append(out, Info{
			Mounts: extra,
			Name:   name, ID: c.ID, Worktree: c.Labels[LabelWorktree],
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
