// Package task manages disposable work copies. Only Workdir is mounted into a
// cage. Metadata, the baseline, and the Git database used for review stay on
// the host. Host Git commands never read the agent's Git configuration.
package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/proc"
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

type Task struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Created  time.Time `json:"created"`
	Baseline string    `json:"baseline"`
	Backend  string    `json:"backend,omitempty"`
	Masks    []string  `json:"masks"`
	Unmask   []string  `json:"unmask"`
}

func home() string {
	h, _ := filepath.Abs(paths.Home())
	if resolved, err := filepath.EvalSymlinks(h); err == nil {
		h = resolved
	}
	return filepath.Join(h, "tasks")
}
func (t Task) Dir() string     { return filepath.Join(home(), t.Name) }
func (t Task) Workdir() string { return filepath.Join(t.Dir(), "work") }
func (t Task) Cage() string    { return cage.NameFor(t.Workdir()) }
func (t Task) repo() string    { return filepath.Join(t.Dir(), "review.git") }

func Load(name string) (Task, error) {
	var t Task
	if !validName.MatchString(name) {
		return t, fmt.Errorf("task name must use 1–48 lowercase letters, digits, or hyphens, starting with a letter or digit")
	}
	b, err := os.ReadFile(filepath.Join(home(), name, "task.json"))
	if err != nil {
		return t, fmt.Errorf("read task %s: %w", name, err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, err
	}
	if t.Name != name || !filepath.IsAbs(t.Source) || len(t.Baseline) != 40 {
		return t, fmt.Errorf("task %s has invalid metadata", name)
	}
	return t, nil
}

func List() ([]Task, error) {
	entries, err := os.ReadDir(home())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, e := range entries {
		if !e.IsDir() || !validName.MatchString(e.Name()) {
			continue
		}
		t, err := Load(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (t Task) Save() error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(t.Dir(), "task.json")
	if err := writeDurable(p+".tmp", append(b, '\n')); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func writeDurable(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// Lock serializes run/review/apply/destroy. An interrupted operation leaves a
// visible lock rather than allowing an unsafe concurrent apply.
func (t Task) Lock() (func(), error) {
	p := filepath.Join(t.Dir(), "lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("task %s is locked; if its command has exited, remove %s: %w", t.Name, p, err)
	}
	_, err = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

func IsWorkdir(root string) bool {
	parent := filepath.Dir(root)
	return filepath.Clean(filepath.Dir(parent)) == filepath.Clean(home()) && filepath.Base(root) == "work"
}

// cleanGit disables ambient Git configuration, hooks, filters, and alternate
// object stores. In particular, no command discovers the agent's .git.
func cleanGit(dir string, input []byte, args ...string) ([]byte, error) {
	base := []string{"--no-pager", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.protectNTFS=true", "-c", "core.protectHFS=true", "-c", "user.name=Zoo", "-c", "user.email=zoo@localhost"}
	cmd := exec.CommandContext(context.Background(), "git", append(base, args...)...)
	cmd.Dir = dir
	for _, v := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(v), "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	proc.Quiet(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (t Task) git(work string, input []byte, args ...string) ([]byte, error) {
	base := []string{"--git-dir=" + t.repo()}
	dir := t.Dir()
	if work != "" {
		base = append(base, "--work-tree="+work)
		dir = work
	}
	return cleanGit(dir, input, append(base, args...)...)
}

func Create(name, source string, g config.Global) (result Task, err error) {
	if !validName.MatchString(name) {
		return result, fmt.Errorf("invalid task name %q: use lowercase letters, digits, and hyphens", name)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return result, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return result, err
	}
	userHome, _ := os.UserHomeDir()
	if source == userHome || filepath.Dir(source) == source {
		return result, fmt.Errorf("choose a project directory, not %s", source)
	}
	absHome, err := filepath.Abs(paths.Home())
	if err != nil {
		return result, err
	}
	if resolved, e := filepath.EvalSymlinks(absHome); e == nil {
		absHome = resolved
	}
	if inside(source, absHome) || inside(absHome, source) {
		return result, fmt.Errorf("task source must not overlap Zoo's state directory")
	}
	p, err := config.LoadProject(source)
	if err != nil {
		return result, err
	}
	t := Task{Name: name, Source: source, Created: time.Now().UTC(), Masks: append(append([]string{}, g.Masks...), p.Mask...), Unmask: append(append([]string{}, g.Unmask...), p.Unmask...)}
	if err := os.MkdirAll(home(), 0o700); err != nil {
		return result, err
	}
	if err := os.Mkdir(t.Dir(), 0o700); err != nil {
		return result, fmt.Errorf("create task %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(t.Dir())
		}
	}()
	if _, err = cleanGit(t.Dir(), nil, "init", "--bare", "--object-format=sha1", "--initial-branch=main", "--template=", t.repo()); err != nil {
		return result, err
	}
	names, err := sourceFiles(source)
	if err != nil {
		return result, err
	}
	tree, err := t.snapshot(source, names)
	if err != nil {
		return result, err
	}
	commit, err := t.git("", []byte("Disposable task baseline\n"), "commit-tree", tree)
	if err != nil {
		return result, err
	}
	t.Baseline = strings.TrimSpace(string(commit))
	if _, err = t.git("", nil, "update-ref", "refs/heads/main", t.Baseline); err != nil {
		return result, err
	}
	// --no-local copies objects through Git's protocol; no shared object store
	// or hardlinks connect the writable copy to the review database.
	if _, err = cleanGit(t.Dir(), nil, "clone", "--no-local", "--template=", t.repo(), t.Workdir()); err != nil {
		return result, err
	}
	if _, err = cleanGit(t.Dir(), nil, "--git-dir="+filepath.Join(t.Workdir(), ".git"), "remote", "remove", "origin"); err != nil {
		return result, err
	}
	if err = config.CopyTrust(source, t.Workdir()); err != nil {
		return result, err
	}
	if err = t.Save(); err != nil {
		return result, err
	}
	return t, nil
}

func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func sourceFiles(root string) ([]string, error) {
	if _, err := cleanGit(root, nil, "rev-parse", "--show-toplevel"); err == nil {
		b, err := cleanGit(root, nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		if err != nil {
			return nil, err
		}
		return splitPaths(b), nil
	}
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if blockedPath(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return names, err
}

func splitPaths(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// Reject Git metadata at any depth, and portable aliases for .git on HFS/NTFS.
func blockedPath(p string) bool {
	if !filepath.IsLocal(filepath.FromSlash(p)) || strings.ContainsAny(p, "\\:\x00\r\n") {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		lower := strings.ToLower(strings.TrimRight(part, " ."))
		if lower == ".git" || strings.HasPrefix(lower, ".git~") || strings.HasPrefix(lower, "git~") || strings.ContainsAny(part, "\u200c\u200d\ufeff") {
			return true
		}
		switch lower {
		case "node_modules", "__pycache__", ".build", "deriveddata":
			return true
		}
	}
	return false
}

func (t Task) snapshot(root string, names []string) (string, error) {
	if _, err := t.git("", nil, "read-tree", "--empty"); err != nil {
		return "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	sort.Strings(names)
	var entries bytes.Buffer
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] || blockedPath(name) || cage.Masked(name, t.Masks, t.Unmask) {
			continue
		}
		seen[name] = true
		st, err := r.Lstat(filepath.FromSlash(name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		var data []byte
		mode := "100644"
		switch {
		case st.Mode().IsRegular():
			f, err := r.Open(filepath.FromSlash(name))
			if err != nil {
				return "", err
			}
			data, err = io.ReadAll(f)
			closeErr := f.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
			if st.Mode()&0o111 != 0 {
				mode = "100755"
			}
		case st.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(name)))
			if err != nil {
				return "", err
			}
			resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(name)), target)))
			if filepath.IsAbs(target) || blockedPath(resolved) || cage.Masked(resolved, t.Masks, t.Unmask) {
				return "", fmt.Errorf("%s: symlink must target an unmasked path inside the project", name)
			}
			data, mode = []byte(target), "120000"
		case st.IsDir():
			return "", fmt.Errorf("%s: submodules and tracked directories are not supported in disposable tasks", name)
		default:
			return "", fmt.Errorf("%s: cannot copy a socket, device, or pipe", name)
		}
		blob, err := t.git("", data, "hash-object", "-w", "--stdin", "--no-filters")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&entries, "%s %s\t%s\x00", mode, bytes.TrimSpace(blob), name)
	}
	if _, err := t.git("", entries.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := t.git("", nil, "write-tree")
	return strings.TrimSpace(string(tree)), err
}

func (t Task) current() (string, error) {
	if _, err := t.git("", nil, "read-tree", t.Baseline); err != nil {
		return "", err
	}
	names, err := t.git(t.Workdir(), nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	return t.snapshot(t.Workdir(), splitPaths(names))
}

func (t Task) patch(tree string, stat bool) ([]byte, error) {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", t.Baseline, tree}
	if stat {
		args = append(args, "--stat")
	}
	return t.git("", nil, append(args, "--")...)
}

func (t Task) Diff(stat bool) ([]byte, error) {
	tree, err := t.current()
	if err != nil {
		return nil, err
	}
	return t.patch(tree, stat)
}

// Apply checks every changed host file against the baseline before Git applies
// the binary patch. Unrelated host changes are preserved. Git refuses symlink
// traversal and unsafe patch paths; its database and config are host-owned.
func (t *Task) Apply(dryRun bool) (int, error) {
	recovered, err := t.recoverApply(dryRun)
	if err != nil {
		return 0, err
	}
	if recovered > 0 {
		return recovered, nil
	}
	tree, err := t.current()
	if err != nil {
		return 0, err
	}
	patch, err := t.patch(tree, false)
	if err != nil || len(patch) == 0 {
		return 0, err
	}
	changed, err := t.git("", nil, "diff", "--name-only", "--no-renames", "-z", t.Baseline, tree, "--")
	if err != nil {
		return 0, err
	}
	names := splitPaths(changed)
	// Include only changed paths from the baseline, then compare to the same
	// paths on the host. This catches edits outside the patch's context too.
	base, err := t.git("", nil, append([]string{"ls-tree", "-r", "-z", t.Baseline, "--"}, names...)...)
	if err != nil {
		return 0, err
	}
	hostTree, err := t.snapshot(t.Source, names)
	if err != nil {
		return 0, err
	}
	host, err := t.git("", nil, "ls-tree", "-r", "-z", hostTree)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(base, host) {
		return 0, fmt.Errorf("host files changed since the baseline; nothing applied; review `zoo task diff %s` and merge the changes manually", t.Name)
	}
	if _, err := t.git(t.Source, patch, "apply", "--check", "--whitespace=nowarn", "-"); err != nil {
		return 0, err
	}
	if dryRun {
		return len(names), nil
	}
	// Save the exact reviewed patch before touching the host. Keep it if an
	// I/O failure interrupts apply so recovery does not depend on the agent.
	if err := writeDurable(filepath.Join(t.Dir(), "last-apply.patch"), patch); err != nil {
		return 0, err
	}
	commit, err := t.git("", []byte("Applied disposable changes\n"), "commit-tree", tree, "-p", t.Baseline)
	if err != nil {
		return len(names), err
	}
	next := strings.TrimSpace(string(commit))
	pending := pendingApply{Old: t.Baseline, New: next}
	data, _ := json.Marshal(pending)
	if _, err := t.git("", nil, "update-ref", "refs/heads/applied", next); err != nil {
		return 0, err
	}
	if err := writeDurable(filepath.Join(t.Dir(), "pending-apply.json"), data); err != nil {
		return 0, err
	}
	if _, err := t.git(t.Source, patch, "apply", "--whitespace=nowarn", "-"); err != nil {
		return 0, err
	}
	t.Baseline = next
	if err := t.Save(); err != nil {
		return len(names), fmt.Errorf("files applied but baseline was not saved: %w", err)
	}
	if err := os.Remove(filepath.Join(t.Dir(), "pending-apply.json")); err != nil {
		return len(names), err
	}
	return len(names), nil
}

type pendingApply struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// recoverApply handles a disconnect between writing host files and advancing
// the baseline. Mixed or edited host files require manual recovery, never a
// second blind apply. Dry runs do not change task metadata.
func (t *Task) recoverApply(dryRun bool) (int, error) {
	p := filepath.Join(t.Dir(), "pending-apply.json")
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var pending pendingApply
	if err := json.Unmarshal(data, &pending); err != nil {
		return 0, fmt.Errorf("read interrupted apply: %w", err)
	}
	if pending.New == t.Baseline {
		if dryRun {
			return 0, nil
		}
		return 0, os.Remove(p)
	}
	if pending.Old != t.Baseline {
		return 0, fmt.Errorf("interrupted apply has a different baseline; keep %s for recovery", p)
	}
	changed, err := t.git("", nil, "diff", "--name-only", "--no-renames", "-z", pending.Old, pending.New, "--")
	if err != nil {
		return 0, err
	}
	names := splitPaths(changed)
	tree, err := t.snapshot(t.Source, names)
	if err != nil {
		return 0, err
	}
	host, err := t.git("", nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return 0, err
	}
	for _, ref := range []string{pending.New, pending.Old} {
		want, err := t.git("", nil, append([]string{"ls-tree", "-r", "-z", ref, "--"}, names...)...)
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(host, want) {
			continue
		}
		if ref == pending.New {
			if !dryRun {
				t.Baseline = ref
				if err := t.Save(); err != nil {
					return 0, err
				}
				if err := os.Remove(p); err != nil {
					return 0, err
				}
			}
			return len(names), nil
		}
		if !dryRun {
			if err := os.Remove(p); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	return 0, fmt.Errorf("interrupted apply: host files differ from both snapshots; recover with %s before retrying", filepath.Join(t.Dir(), "last-apply.patch"))
}

func (t Task) Destroy(discard bool) error {
	if !discard {
		patch, err := t.Diff(false)
		if err != nil {
			return err
		}
		if len(patch) > 0 {
			return errors.New("task has unapplied changes; apply them first, or use --discard")
		}
	}
	if err := os.RemoveAll(paths.CacheProject(t.Cage())); err != nil {
		return err
	}
	return os.RemoveAll(t.Dir())
}
