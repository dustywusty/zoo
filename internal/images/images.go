// Package images builds cage images from the user's own image (zoo.yaml
// image:) or Dockerfile (zoo.yaml dockerfile:). zoo puts an overlay on top
// (assets/images/cage-overlay) with what every cage needs: the agent user
// with the host uid/gid, zoo-init/zoo-session/zoo-doctor, a few tools and
// the cage environment. Agent CLIs are the user's to install.
//
// A Dockerfile lives in the worktree, which the agent can write, and its RUN
// steps run with network access and no keeper. So zoo builds it only with
// contents a human approved (approve.go), and with a build context that
// holds nothing but the rendered Dockerfile and zoo's own overlay files:
// never the worktree, whose masked secrets (.env) a COPY would bake in.
package images

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dusty/zoo/assets"
	"github.com/dusty/zoo/internal/config"
)

// Repo is the repository derived cage images are tagged in.
const Repo = "zoo-cage-custom"

// maxDockerfile caps what zoo reads, prints and hashes.
const maxDockerfile = 1 << 20

// Source is what a derived cage image is built from: a base image ref, or
// the contents of a Dockerfile.
type Source struct {
	Base       string // image: ref, used when Dockerfile is nil
	Dockerfile []byte // approved Dockerfile contents
	Path       string // the Dockerfile's path in the worktree, for messages
}

// Describe names the source for humans.
func (s Source) Describe() string {
	if s.Dockerfile != nil {
		if s.Path != "" {
			return s.Path
		}
		return "your Dockerfile"
	}
	return s.Base
}

// Pick returns the effective image: and dockerfile: settings. zoo.yaml
// wins over the global config as a pair: a repo that names either one
// doesn't inherit the other from the global defaults.
func Pick(g config.Global, p config.Project) (image, dockerfile string) {
	image, dockerfile = strings.TrimSpace(p.Image), strings.TrimSpace(p.Dockerfile)
	if image == "" && dockerfile == "" {
		image, dockerfile = strings.TrimSpace(g.Image), strings.TrimSpace(g.Dockerfile)
	}
	return image, dockerfile
}

var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// ValidRef checks an image: ref is a plain reference (it goes on a FROM line).
func ValidRef(ref string) error {
	if len(ref) > 255 || !refRe.MatchString(ref) {
		return fmt.Errorf("image: %q isn't an image reference (like python:3.12-slim or ghcr.io/org/img@sha256:…)", ref)
	}
	return nil
}

// ReadDockerfile reads dockerfile: from a worktree. The path must stay in
// the worktree (symlinks included) and name a regular file.
func ReadDockerfile(root, rel string) ([]byte, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("dockerfile: %q must be a path relative to the worktree", rel)
	}
	p := filepath.Join(root, filepath.Clean(rel))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	if r, err := filepath.Rel(realRoot, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("dockerfile: %s is outside the worktree", rel)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("dockerfile: %s isn't a file", rel)
	}
	if st.Size() > maxDockerfile {
		return nil, fmt.Errorf("dockerfile: %s is over 1 MiB", rel)
	}
	return os.ReadFile(real)
}

// overlayFile is one file zoo adds to the build context.
type overlayFile struct {
	name string
	data []byte
}

// overlay returns the overlay Dockerfile fragment and the files it copies.
func overlay() (fragment []byte, files []overlayFile, err error) {
	fragment, err = assets.File("cage-overlay/overlay.Dockerfile")
	if err != nil {
		return nil, nil, err
	}
	for _, src := range []string{"cage-overlay/zoo-setup", "cage/zoo-init", "cage/zoo-doctor", "cage/zoo-session"} {
		b, err := assets.File(src)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, overlayFile{filepath.Base(src), b})
	}
	return fragment, files, nil
}

// Render returns the Dockerfile zoo builds: the base (FROM <image>, or the
// user's Dockerfile, whose final stage the overlay continues) and the overlay.
func Render(s Source) (string, error) {
	fragment, _, err := overlay()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if s.Dockerfile != nil {
		b.Write(s.Dockerfile)
		if len(s.Dockerfile) > 0 && s.Dockerfile[len(s.Dockerfile)-1] != '\n' {
			b.WriteByte('\n')
		}
	} else {
		if err := ValidRef(s.Base); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "FROM %s\n", s.Base)
	}
	b.Write(fragment)
	return b.String(), nil
}

// Tag is the derived image's tag: a hash of everything that goes into it,
// so a changed base ref, Dockerfile, overlay, zoo cage version or uid gets a
// new tag (and a stopped cage on the old one is recreated).
func Tag(s Source, cageImage string, uid, gid int) (string, error) {
	fragment, files, err := overlay()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	field := func(name string, data []byte) {
		fmt.Fprintf(h, "%s %d\n", name, len(data))
		h.Write(data)
	}
	if s.Dockerfile != nil {
		field("dockerfile", s.Dockerfile)
	} else {
		field("base", []byte(s.Base))
	}
	field("overlay", fragment)
	for _, f := range files {
		field(f.name, f.data)
	}
	field("cage", []byte(cageImage))
	field("ids", []byte(fmt.Sprintf("%d:%d", uid, gid)))
	return Repo + ":" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// WriteContext makes a fresh build context under dir holding only the
// rendered Dockerfile and zoo's overlay files. The caller removes it.
func WriteContext(dir string, s Source) (string, error) {
	df, err := Render(s)
	if err != nil {
		return "", err
	}
	_, files, err := overlay()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ctxDir, err := os.MkdirTemp(dir, "custom-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(df), 0o644); err != nil {
		os.RemoveAll(ctxDir)
		return "", err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(ctxDir, f.name), f.data, 0o755); err != nil {
			os.RemoveAll(ctxDir)
			return "", err
		}
	}
	return ctxDir, nil
}
