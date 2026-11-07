package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/dusty/zoo/internal/paths"
)

// zoo.yaml lives in the worktree, which the agent can write. Anything it
// says (services to grant, files to unmask, extra mounts, an image) would be
// the agent's to decide if zoo read it straight from disk. So zoo only ever
// uses a copy the human approved: the file on disk is a proposal, applied
// with `zoo trust` (or a y/N prompt on `zoo run` from a terminal).

// trusted is the approved copy of one worktree's zoo.yaml.
type trusted struct {
	Root     string    `json:"root"`
	Present  bool      `json:"present"` // false: the human approved having no zoo.yaml
	Content  string    `json:"content"`
	Approved time.Time `json:"approved"`
}

func trustFile(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Join(paths.Trust(), hex.EncodeToString(sum[:])[:24]+".json")
}

func readTrusted(root string) (trusted, bool) {
	var t trusted
	b, err := os.ReadFile(trustFile(root))
	if err != nil || json.Unmarshal(b, &t) != nil || t.Root != filepath.Clean(root) {
		return trusted{}, false
	}
	return t, true
}

// ProjectFile is a worktree's zoo.yaml path.
func ProjectFile(root string) string { return filepath.Join(root, "zoo.yaml") }

// readProposal returns zoo.yaml as it is on disk now.
func readProposal(root string) (content []byte, present bool, err error) {
	b, err := os.ReadFile(ProjectFile(root))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func parseProject(b []byte) (Project, error) {
	var p Project
	if len(bytes.TrimSpace(b)) == 0 {
		return p, nil
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("zoo.yaml: %w", err)
	}
	return p, nil
}

// LoadProject returns the approved zoo.yaml for a worktree (empty if none
// was ever approved). It never reads the file on disk: see TrustState.
func LoadProject(root string) (Project, error) {
	t, ok := readTrusted(root)
	if !ok || !t.Present {
		return Project{}, nil
	}
	return parseProject([]byte(t.Content))
}

// TrustState compares a worktree's zoo.yaml on disk with the approved copy.
type TrustState struct {
	Root      string
	Changed   bool // the file on disk differs from what was approved
	Present   bool // zoo.yaml exists on disk
	Proposed  string
	Approved  string
	EverTrust bool // some version was approved before
}

// CheckTrust reports whether zoo.yaml on disk matches the approved copy.
// A worktree with no zoo.yaml and no approval is unchanged.
func CheckTrust(root string) (TrustState, error) {
	b, present, err := readProposal(root)
	if err != nil {
		return TrustState{}, err
	}
	t, ok := readTrusted(root)
	st := TrustState{Root: root, Present: present, Proposed: string(b), EverTrust: ok}
	if ok && t.Present {
		st.Approved = t.Content
	}
	switch {
	case !ok:
		st.Changed = present
	default:
		st.Changed = t.Present != present || (present && t.Content != string(b))
	}
	return st, nil
}

// Trust approves zoo.yaml as it is on disk now. A file that doesn't parse
// is refused, so a typo can't silently drop masks.
func Trust(root string) (Project, error) {
	b, present, err := readProposal(root)
	if err != nil {
		return Project{}, err
	}
	p, err := parseProject(b)
	if err != nil {
		return Project{}, err
	}
	if err := os.MkdirAll(paths.Trust(), 0o700); err != nil {
		return Project{}, err
	}
	out, _ := json.MarshalIndent(trusted{Root: filepath.Clean(root), Present: present, Content: string(b), Approved: time.Now().UTC()}, "", "  ")
	tmp := trustFile(root) + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return Project{}, err
	}
	return p, os.Rename(tmp, trustFile(root))
}
