// Package gitrefs notices when a cage moves refs it shouldn't.
//
// A cage shares the repo's refs with the host (it has to: commits made in
// the cage must be visible outside). So an agent can move any branch, not
// just its own: point `main` at a commit of its choosing, or forge
// `origin/main` so the branch looks pushed, and the human pushes the
// agent's work believing it was reviewed. Mounts can't separate one ref
// from another, so zoo snapshots the refs when a session starts and
// reports what changed besides the cage's own branch.
//
// Refs are read straight from the files (loose refs and packed-refs), not
// with `git for-each-ref`: the daemon checks every running cage every 30 s,
// and a process per check is what D25 removed.
package gitrefs

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Refs maps a ref name (refs/heads/main, HEAD) to its value: an object id,
// or "ref: <target>" for a symbolic ref.
type Refs map[string]string

// ErrReftable means the repo stores refs in the reftable format, which
// zoo doesn't read; callers should say they can't check.
var ErrReftable = errors.New("reftable ref storage isn't supported")

// Read returns the refs in a common git dir, plus its HEAD (the main
// worktree's checkout). Loose refs override packed ones, as in git.
func Read(common string) (Refs, error) {
	if _, err := os.Stat(filepath.Join(common, "reftable")); err == nil {
		return nil, ErrReftable
	}
	out := Refs{}
	if f, err := os.Open(filepath.Join(common, "packed-refs")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if l == "" || l[0] == '#' || l[0] == '^' {
				continue // header, or the peeled value of the tag above
			}
			if id, name, ok := strings.Cut(l, " "); ok {
				out[name] = id
			}
		}
		f.Close()
	}
	root := filepath.Join(common, "refs")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil // mid-update; the next read gets it
		}
		rel, _ := filepath.Rel(common, p)
		if v := strings.TrimSpace(string(b)); v != "" {
			out[filepath.ToSlash(rel)] = v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(common, "HEAD")); err == nil {
		out["HEAD"] = strings.TrimSpace(string(b))
	}
	return out, nil
}

// Own is the branch a worktree has checked out ("refs/heads/feature"), from
// its gitdir's HEAD; "" when detached. The cage may move this one.
func Own(gitDir string) string {
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if t, ok := strings.CutPrefix(v, "ref: "); ok {
		return t
	}
	return ""
}

// Change is one ref that differs from the snapshot.
type Change struct {
	Ref  string `json:"ref"`
	From string `json:"from,omitempty"` // "" = created
	To   string `json:"to,omitempty"`   // "" = deleted
	// Alarm: worth interrupting someone for. Moving or deleting an existing
	// branch, tag or remote-tracking ref, creating tags or remote refs, or
	// re-pointing the main checkout's HEAD. New local branches aren't.
	Alarm bool `json:"alarm"`
}

func (c Change) String() string {
	short := func(v string) string {
		if strings.HasPrefix(v, "ref: ") || len(v) <= 12 {
			return v
		}
		return v[:12]
	}
	switch {
	case c.From == "":
		return fmt.Sprintf("%s created at %s", c.Ref, short(c.To))
	case c.To == "":
		return fmt.Sprintf("%s deleted (was %s)", c.Ref, short(c.From))
	}
	return fmt.Sprintf("%s moved %s -> %s", c.Ref, short(c.From), short(c.To))
}

// Undo is the git command that puts the ref back, run in the main checkout.
func (c Change) Undo() string {
	switch {
	case c.From == "":
		return "git update-ref -d " + c.Ref
	case strings.HasPrefix(c.From, "ref: "):
		return "git symbolic-ref " + c.Ref + " " + strings.TrimPrefix(c.From, "ref: ")
	}
	return "git update-ref " + c.Ref + " " + c.From
}

// Diff lists changes from before to after, ignoring the cage's own branch
// (own, which it's allowed to move), HEAD when the cage is the main
// checkout (mainCheckout: then HEAD is its own), and refs git itself
// rewrites as a side effect of normal work (stash, bisect, notes...).
func Diff(before, after Refs, own string, mainCheckout bool) []Change {
	if mainCheckout {
		before, after = without(before, "HEAD"), without(after, "HEAD")
	}
	var out []Change
	seen := map[string]bool{}
	for ref, from := range before {
		seen[ref] = true
		if to := after[ref]; to != from && !ignored(ref, own) {
			out = append(out, Change{Ref: ref, From: from, To: to, Alarm: true})
		}
	}
	for ref, to := range after {
		if seen[ref] || ignored(ref, own) {
			continue
		}
		alarm := !strings.HasPrefix(ref, "refs/heads/")
		out = append(out, Change{Ref: ref, To: to, Alarm: alarm})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func ignored(ref, own string) bool {
	if ref == own && own != "" {
		return true
	}
	for _, p := range []string{"refs/stash", "refs/bisect/", "refs/notes/", "refs/original/", "refs/rewritten/", "refs/worktree/"} {
		if ref == strings.TrimSuffix(p, "/") || strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// Snapshot is the stored baseline for one cage.
type Snapshot struct {
	Common string `json:"common"`
	Own    string `json:"own"`
	Refs   Refs   `json:"refs"`
}

// Save writes a snapshot as JSON.
func Save(path string, s Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", " ")
	return os.WriteFile(path, b, 0o600)
}

// Load reads a snapshot; ok is false if there is none.
func Load(path string) (Snapshot, bool) {
	var s Snapshot
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &s) != nil || s.Refs == nil {
		return Snapshot{}, false
	}
	return s, true
}

func without(r Refs, ref string) Refs {
	out := make(Refs, len(r))
	for k, v := range r {
		if k != ref {
			out[k] = v
		}
	}
	return out
}

// Begin starts watching a cage's refs at the start of a session. It takes a
// fresh baseline unless the old one still shows alarms nobody acknowledged
// (zoo refs ok): those stay reported rather than being absorbed into the
// new baseline. It returns the pending alarms.
func Begin(snapPath, common, gitDir string) ([]Change, error) {
	if old, ok := Load(snapPath); ok && old.Common == common {
		if pending := Alarms(Check(old, common, gitDir)); len(pending) > 0 {
			return pending, nil
		}
	}
	return nil, Rebaseline(snapPath, common, gitDir)
}

// Rebaseline accepts the current refs as normal.
func Rebaseline(snapPath, common, gitDir string) error {
	refs, err := Read(common)
	if err != nil {
		return err
	}
	return Save(snapPath, Snapshot{Common: common, Own: Own(gitDir), Refs: refs})
}

// Check compares current refs with a snapshot. Errors (unreadable refs,
// reftable) come back as no changes: callers that need to know use Read.
func Check(s Snapshot, common, gitDir string) []Change {
	now, err := Read(common)
	if err != nil {
		return nil
	}
	return Diff(s.Refs, now, s.Own, gitDir == common)
}

// Alarms keeps the changes worth interrupting someone for.
func Alarms(cs []Change) []Change {
	var out []Change
	for _, c := range cs {
		if c.Alarm {
			out = append(out, c)
		}
	}
	return out
}
