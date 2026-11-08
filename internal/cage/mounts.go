package cage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// One cage can hold several worktrees (zoo.yaml mounts:): a frontend and a
// backend repo, or a set of gitops repos. Each gets the same treatment as
// the main worktree: masks, the git guard, planted-repository scans. A
// mount can't widen the cage to things that aren't project directories.

// sensitive are home-relative paths no mount may include or sit inside.
var sensitive = []string{
	".ssh", ".aws", ".gnupg", ".kube", ".docker", ".config", ".zoo", ".netrc",
	".npmrc", ".pypirc", ".azure", ".gcloud", ".password-store", ".codex", ".claude",
	"Library", "Documents", "Desktop", "Downloads",
}

// ResolveMounts turns zoo.yaml mounts: into absolute, checked directories.
func ResolveMounts(root string, mounts []string) ([]string, error) {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var out []string
	for _, m := range mounts {
		p := strings.TrimSpace(m)
		if p == "" {
			continue
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("mounts: %s isn't a directory", m)
		}
		if err := checkMount(root, home, p); err != nil {
			return nil, fmt.Errorf("mounts: %s: %w", m, err)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	// One mount inside another would mount the same files twice.
	for _, a := range out {
		for _, b := range out {
			if a != b && within(a, b) {
				return nil, fmt.Errorf("mounts: %s is inside %s; list only one", a, b)
			}
		}
	}
	return out, nil
}

func checkMount(root, home, p string) error {
	if p == "/" || p == home || within(root, p) {
		return fmt.Errorf("would put your whole home directory or a parent of this worktree in the cage")
	}
	if within(p, root) {
		return fmt.Errorf("is inside this worktree, which is already mounted")
	}
	if home != "" {
		if within(home, p) {
			return fmt.Errorf("contains your home directory")
		}
		for _, s := range sensitive {
			sp := filepath.Join(home, s)
			if within(p, sp) || within(sp, p) {
				return fmt.Errorf("overlaps ~/%s, which zoo keeps out of cages", s)
			}
		}
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("paths with ':' can't be mounted")
	}
	return nil
}
