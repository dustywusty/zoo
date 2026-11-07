package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// Approval is the Dockerfile a human last approved for one worktree. Its
// contents are kept, so zoo can rebuild the approved image (never the file
// on disk, which the agent may have changed since).
type Approval struct {
	Root     string    `json:"root"`
	Path     string    `json:"path"`
	SHA256   string    `json:"sha256"`
	Content  string    `json:"content"`
	Approved time.Time `json:"approved"`
}

// Sum is the content hash approvals compare.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func approvalFile(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Join(paths.Dockerfiles(), hex.EncodeToString(sum[:])[:24]+".json")
}

// LoadApproval returns the approved Dockerfile for a worktree.
func LoadApproval(root string) (Approval, bool) {
	var a Approval
	b, err := os.ReadFile(approvalFile(root))
	if err != nil || json.Unmarshal(b, &a) != nil || a.Root != filepath.Clean(root) || Sum([]byte(a.Content)) != a.SHA256 {
		return Approval{}, false
	}
	return a, true
}

// Approve records content as the approved Dockerfile for a worktree.
func Approve(root, path string, content []byte) error {
	if err := os.MkdirAll(paths.Dockerfiles(), 0o700); err != nil {
		return err
	}
	a := Approval{Root: filepath.Clean(root), Path: path, SHA256: Sum(content), Content: string(content), Approved: time.Now().UTC()}
	out, _ := json.MarshalIndent(a, "", "  ")
	tmp := approvalFile(root) + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, approvalFile(root))
}

// Decision is which image a worktree's cage uses right now.
type Decision struct {
	Source *Source // nil: zoo's default cage image
	// Pending is set when the Dockerfile on disk isn't the approved one;
	// Proposed holds it (nil if it couldn't be read).
	Pending  bool
	Proposed []byte
	Path     string // dockerfile: setting
	Note     string // what to tell the human, if anything
}

// Decide picks the image source for a worktree from the approved config:
// an image: ref as is (zoo.yaml is approved with `zoo trust`), a Dockerfile
// only with contents approved by `zoo build --project`. A Dockerfile that
// changed since keeps the last approved one in use (or zoo's default).
func Decide(root string, g config.Global, p config.Project) (Decision, error) {
	image, df := Pick(g, p)
	if df == "" {
		if image == "" {
			return Decision{}, nil
		}
		if err := ValidRef(image); err != nil {
			return Decision{}, err
		}
		return Decision{Source: &Source{Base: image}}, nil
	}
	d := Decision{Path: df}
	content, readErr := ReadDockerfile(root, df)
	a, ok := LoadApproval(root)
	if readErr == nil && ok && a.SHA256 == Sum(content) {
		d.Source = &Source{Dockerfile: content, Path: df}
		return d, nil
	}
	d.Pending = true
	if readErr == nil {
		d.Proposed = content
	}
	what := fmt.Sprintf("%s changed since you approved it", df)
	switch {
	case readErr != nil:
		what = readErr.Error()
	case !ok:
		what = fmt.Sprintf("%s hasn't been approved yet", df)
	}
	if ok {
		d.Source = &Source{Dockerfile: []byte(a.Content), Path: a.Path}
		d.Note = what + "; using the image built from the version you approved. Review and build it with `zoo build --project`"
	} else {
		d.Note = what + "; using zoo's default cage image. Review and build it with `zoo build --project`"
	}
	return d, nil
}
