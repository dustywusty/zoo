// Package cage plans and runs cages: one small VM per worktree.
package cage

import (
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

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/ctr"
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
)

// Plan is everything needed to create a cage for one worktree.
type Plan struct {
	Name     string   // short name, e.g. api-3fa2c1
	ID       string   // container id, e.g. zoo-api-3fa2c1
	Root     string   // worktree root on the host (and in the guest)
	ExtraRW  []string // other host paths mounted rw (a worktree's main .git)
	Masks    []string // secret files hidden inside the cage
	ReadOnly []string // paths made read-only inside the cage
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
		pl.ReadOnly = append(pl.ReadOnly, filepath.Join(common, "hooks"), filepath.Join(common, "config"))
		if gitDir != "" && gitDir != common {
			pl.ReadOnly = append(pl.ReadOnly, filepath.Join(gitDir, "config.worktree"))
		}
	}

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

// RunArgs is the `container run` invocation for a plan.
func (pl Plan) RunArgs(g config.Global, keeperIP string) []string {
	args := []string{"run", "-d",
		"--name", pl.ID,
		"--network", g.Network,
		"--no-dns",
		"-c", fmt.Sprint(pl.Size.CPUs),
		"-m", pl.Size.Memory,
		"-l", LabelCage + "=" + pl.Name,
		"-l", LabelWorktree + "=" + pl.Root,
		"-l", LabelSize + "=" + pl.SizeName,
		"-l", LabelCPUs + "=" + fmt.Sprint(pl.Size.CPUs),
		"-l", LabelMemory + "=" + pl.Size.Memory,
		"-e", "ZOO_MASKS=" + strings.Join(pl.Masks, "\n"),
		"-e", "ZOO_READONLY=" + strings.Join(pl.ReadOnly, "\n"),
		"-e", "ZOO_CAGE=" + pl.Name,
	}
	for _, e := range ProxyEnv(keeperIP) {
		args = append(args, "-e", e)
	}
	args = append(args, "-v", pl.Root+":"+pl.Root)
	for _, p := range pl.ExtraRW {
		args = append(args, "-v", p+":"+p)
	}
	return append(args, g.CageImage)
}

// Ensure creates or starts the cage. Returns true if it was (re)created.
func Ensure(pl Plan, g config.Global, keeperIP string) (bool, error) {
	c, exists := ctr.Get(pl.ID, g.Network)
	if exists && c.Running() {
		return false, nil
	}
	if exists {
		if err := ctr.Start(pl.ID); err == nil {
			return false, waitRunning(pl.ID, g.Network)
		}
		_ = ctr.Delete(pl.ID)
	}
	if _, err := ctr.Run(pl.RunArgs(g, keeperIP)...); err != nil {
		return false, err
	}
	return true, waitRunning(pl.ID, g.Network)
}

func waitRunning(id, network string) error {
	for i := 0; i < 40; i++ {
		if c, ok := ctr.Get(id, network); ok && c.Running() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("cage %s did not reach running state", id)
}

// Reset deletes the cage so the next run starts from a clean image.
func Reset(id string) error { return ctr.Delete(id) }

// Info is a cage as listed for humans and the dashboard.
type Info struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
	Size     string `json:"size"`
	CPUs     string `json:"cpus"`
	Memory   string `json:"memory"`
	Status   string `json:"status"`
	Running  bool   `json:"running"`
	IP       string `json:"ip"`
}

// List returns all zoo cages.
func List(g config.Global) ([]Info, error) {
	all, err := ctr.List(g.Network)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, c := range all {
		name := c.Labels[LabelCage]
		if name == "" {
			if !strings.HasPrefix(c.ID, Prefix) || c.ID == keeper.Name {
				continue
			}
			name = strings.TrimPrefix(c.ID, Prefix)
		}
		out = append(out, Info{
			Name: name, ID: c.ID, Worktree: c.Labels[LabelWorktree],
			Size: c.Labels[LabelSize], CPUs: c.Labels[LabelCPUs], Memory: c.Labels[LabelMemory],
			Status: c.Status, Running: c.Running(), IP: c.IP,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Find resolves a user-supplied name (short name, id, or worktree path).
func Find(g config.Global, q string) (Info, error) {
	all, err := List(g)
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
