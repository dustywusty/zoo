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
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/proc"
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
	LabelMasks    = "zoo.masks"            // fingerprint of the masked files: secrets added later mean a stale cage
)

// Plan is everything needed to create a cage for one worktree.
type Plan struct {
	Name     string          // short name, e.g. api-3fa2c1
	ID       string          // container id, e.g. zoo-api-3fa2c1
	Root     string          // worktree root on the host (and in the guest)
	ExtraRW  []string        // other host paths mounted rw (a worktree's main .git)
	Masks    []string        // secret files hidden inside the cage
	ReadOnly []string        // paths made read-only inside the cage
	Pin      []string        // directories mounted onto themselves so they can't be renamed
	ReopenRW []string        // dirs inside a read-only dir made writable again (this worktree's gitdir)
	Files    []backend.Mount // read-only files shown with rewritten content (git pointers, Windows)
	AutoCRLF string          // host's core.autocrlf, mirrored into the cage when paths differ (Windows)
	SizeName string
	Size     config.Size
}

// Root returns the git toplevel for dir, or dir itself outside a repo.
func Root(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel")
	proc.Quiet(cmd)
	out, err := cmd.Output()
	if err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			// Git for Windows answers C:/Users/...; keep one spelling so the
			// cage name (a hash of the path) is stable.
			return filepath.Clean(top), nil
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
	cmd := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir")
	proc.Quiet(cmd)
	out, err := cmd.Output()
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

// within reports whether path is dir or inside it. Rel's ".." uses the OS
// separator (..\ on Windows), and paths on different drives don't relate.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	if err := backend.CheckGuestPath(root); err != nil {
		return Plan{}, err
	}
	name := NameFor(root)
	pl := Plan{Name: name, ID: Prefix + name, Root: root, SizeName: sn, Size: size}
	if backend.GuestPath(root) != root {
		pl.AutoCRLF = hostGitConfig(root, "core.autocrlf")
	}

	common, gitDir := gitPaths(root)
	if common != "" {
		if !within(common, root) {
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
	}
	// A directory already mounted (read-only or reopened) can't be renamed
	// either, and mounting it again rw as a pin would undo the read-only one.
	mounted := map[string]bool{}
	for _, p := range append(append([]string{}, pl.ReadOnly...), pl.ReopenRW...) {
		mounted[p] = true
	}
	for _, d := range pinsFor(pl.ReadOnly, append([]string{root}, pl.ExtraRW...)) {
		if !mounted[d] {
			pl.Pin = append(pl.Pin, d)
		}
	}
	// Git's pointer files hold absolute host paths. Where the guest spells
	// them differently (Windows: C:/Users/... -> /c/Users/...), the cage sees
	// a read-only copy rewritten for the guest; the host's file is untouched.
	if gitDir != "" && gitDir != common {
		for _, f := range []string{filepath.Join(root, ".git"), filepath.Join(gitDir, "commondir")} {
			if err := pl.translate(f); err != nil {
				return Plan{}, err
			}
		}
	}

	masks, complete := FindMasksAll(root, append(append([]string{}, g.Masks...), p.Mask...), append(append([]string{}, g.Unmask...), p.Unmask...))
	if !complete {
		return Plan{}, fmt.Errorf("more than %d secret-looking files in %s: zoo won't start a cage that hides only some of them. Add the ones that aren't secret (test fixtures, sample keys) to unmask: in zoo.yaml", MaxMasks, root)
	}
	pl.Masks = masks
	return pl, nil
}

// guestPointer rewrites a git pointer file ("gitdir: C:/x/.git/worktrees/y",
// or a commondir holding one absolute path) for the guest. ok is false when
// nothing changes.
func guestPointer(content string) (string, bool) {
	line, rest, _ := strings.Cut(content, "\n")
	line = strings.TrimRight(line, "\r")
	prefix := ""
	if v, found := strings.CutPrefix(line, "gitdir: "); found {
		prefix, line = "gitdir: ", v
	}
	g := backend.GuestPath(line)
	if g == line {
		return content, false
	}
	return prefix + g + "\n" + rest, true
}

// translate stages a guest copy of a read-only git pointer file if the
// guest needs different paths, and mounts the copy in its place.
func (pl *Plan) translate(f string) error {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil // absent: nothing to translate
	}
	out, ok := guestPointer(string(b))
	if !ok {
		return nil
	}
	dir := filepath.Join(paths.Stage(), pl.Name, "guest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	staged := filepath.Join(dir, fmt.Sprintf("%d-%s", len(pl.Files), filepath.Base(f)))
	if err := os.WriteFile(staged, []byte(out), 0o444); err != nil {
		// 0444 from an earlier run: replace it.
		_ = os.Chmod(staged, 0o644)
		if err := os.WriteFile(staged, []byte(out), 0o444); err != nil {
			return fmt.Errorf("stage guest copy of %s: %w", f, err)
		}
	}
	kept := pl.ReadOnly[:0]
	for _, p := range pl.ReadOnly {
		if p != f {
			kept = append(kept, p)
		}
	}
	pl.ReadOnly = kept
	pl.Files = append(pl.Files, backend.Mount{Source: staged, Target: f, ReadOnly: true})
	return nil
}

// gitEnv is git configuration the cage needs from its environment. Docker
// Desktop shows Windows files to the guest as root-owned, so git's
// ownership check (safe.directory) refuses the worktree. That check guards
// against repositories owned by other users; inside a cage everything is
// the agent's anyway. Bookworm's git (2.39) has no "dir/*" form, so "*"
// covers submodule checkouts too. Unix hosts map ownership and need nothing.
//
// Line endings too: Git for Windows usually runs with core.autocrlf=true,
// so the worktree holds CRLF files. Linux git without the same setting sees
// every text file as modified (and would commit CRLF), so the cage gets the
// host's value.
func (pl Plan) gitEnv() []string {
	if backend.GuestPath(pl.Root) == pl.Root {
		return nil
	}
	kv := [][2]string{{"safe.directory", "*"}}
	if pl.AutoCRLF != "" {
		kv = append(kv, [2]string{"core.autocrlf", pl.AutoCRLF})
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(kv))}
	for i, p := range kv {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

// hostGitConfig is the host's effective value of a git setting for root
// (system, global and repo config together), or "".
func hostGitConfig(root, key string) string {
	cmd := exec.Command("git", "-C", root, "config", "--get", key)
	proc.Quiet(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Guarded is every host path the cage must see read-only: ReadOnly plus
// the targets of Files (for zoo doctor).
func (pl Plan) Guarded() []string {
	out := append([]string{}, pl.ReadOnly...)
	for _, f := range pl.Files {
		out = append(out, f.Target)
	}
	return out
}

// skipDirs are dependency trees: huge, and holding other people's code
// rather than this project's secrets. Build outputs are NOT skipped: they
// copy secrets around (Next.js standalone output carries .env files).
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"target": true, "__pycache__": true, ".gradle": true, ".terraform": true,
}

// Mask scan bounds. Past MaxMasks the cage isn't started at all (failing
// closed beats hiding only some secrets); it's also about as many mounts as
// fit on a Windows command line.
const (
	maskDepth = 12
	MaxMasks  = 512
)

// FindMasks walks the worktree for secret-looking files (see FindMasksAll).
func FindMasks(root string, masks, unmask []string) []string {
	out, _ := FindMasksAll(root, masks, unmask)
	return out
}

// FindMasksAll walks the worktree (to depth maskDepth, skipping dependency
// trees) for secret-looking files. complete is false when it found more
// than MaxMasks.
func FindMasksAll(root string, masks, unmask []string) (out []string, complete bool) {
	complete = true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.Count(rel, string(filepath.Separator)) >= maskDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if match(unmask, base) {
			return nil
		}
		if match(masks, base) || matchRel(masks, rel) {
			if len(out) >= MaxMasks {
				complete = false
				return filepath.SkipAll
			}
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, complete
}

// foldCase: macOS and Windows file systems ignore case by default, so
// .ENV is as secret as .env there.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func fold(s string) string {
	if foldCase {
		return strings.ToLower(s)
	}
	return s
}

func match(patterns []string, name string) bool {
	for _, pat := range patterns {
		if strings.Contains(pat, "/") {
			continue
		}
		if ok, _ := path.Match(fold(pat), fold(name)); ok {
			return true
		}
	}
	return false
}

// matchRel matches patterns written with "/" (config/*.secret.json,
// .kube/config) against a worktree-relative path, whatever the OS
// separator. They match at any depth (apps/api/.kube/config) unless they
// start with "/", which anchors them to the worktree root.
func matchRel(patterns []string, rel string) bool {
	rel = fold(filepath.ToSlash(rel))
	for _, pat := range patterns {
		if !strings.Contains(pat, "/") {
			continue
		}
		pat = fold(pat)
		if anchored, ok := strings.CutPrefix(pat, "/"); ok {
			if m, _ := path.Match(anchored, rel); m {
				return true
			}
			continue
		}
		for tail := rel; ; {
			if m, _ := path.Match(pat, tail); m {
				return true
			}
			i := strings.IndexByte(tail, '/')
			if i < 0 {
				break
			}
			tail = tail[i+1:]
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
		LabelMasks:    MasksFingerprint(pl.Masks),
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
		Env:      append(append([]string{"ZOO_CAGE=" + pl.Name}, ProxyEnv(keeperIP)...), pl.gitEnv()...),
		Mounts:   mounts,
		Masks:    pl.Masks,
		ReadOnly: pl.ReadOnly,
		Pin:      pl.Pin,
		ReopenRW: pl.ReopenRW,
		Files:    pl.Files,
	}
}

// Stale reports whether an existing cage predates the current image (and so
// may lack the mounts this zoo relies on, e.g. v0 cages' guard).
func Stale(st backend.Status, spec backend.CageSpec) bool {
	return st.Labels[LabelImage] != spec.Image || MasksChanged(st, spec)
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

// MasksFingerprint identifies a set of masked files, so a cage created
// before a secret file appeared can be told apart (masks are mounts, fixed
// when the cage is created).
func MasksFingerprint(masks []string) string {
	s := append([]string(nil), masks...)
	sort.Strings(s)
	sum := sha1.Sum([]byte(strings.Join(s, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// MasksChanged reports whether the secret files a cage hides differ from
// what it should hide now. A cage from before fingerprints counts as
// changed only if there's something to mask.
func MasksChanged(st backend.Status, spec backend.CageSpec) bool {
	have, ok := st.Labels[LabelMasks]
	if !ok {
		return len(spec.Masks) > 0
	}
	return have != spec.Labels[LabelMasks]
}
