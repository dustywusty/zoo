// Package update finds the latest zoo release on GitHub and replaces the
// running zoo with it. It is used by `zoo update` and by the dashboard,
// which shows when a newer release exists.
//
// An update is the release archive for this OS and architecture. Its
// SHA-256 must match the line for it in the release's checksums.txt,
// which is fetched over HTTPS from the same release.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/paths"
)

// Repo is the GitHub repository that publishes zoo releases.
const Repo = "dustywusty/zoo"

// APIBase is the GitHub API. Tests point it at a local server.
var APIBase = "https://api.github.com"

// maxDownload caps each file that an update downloads.
const maxDownload = 200 << 20

// Release is the part of a GitHub release that an update needs.
type Release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

// Asset is one file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Version is the tag without its "v": "v0.1.2" -> "0.1.2".
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ArchiveName is the release file for this OS and architecture, as
// GoReleaser and the Windows release job name it.
func ArchiveName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("zoo_%s_%s_%s.%s", version, goos, goarch, ext)
}

// Latest asks GitHub for the latest release. GitHub sees only a request
// from this IP address, with a generic User-Agent.
func Latest(ctx context.Context, c *http.Client) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zoo-update")
	resp, err := c.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("latest release: GitHub answered %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("latest release: %w", err)
	}
	if r.Tag == "" {
		return Release{}, errors.New("latest release: no tag")
	}
	return r, nil
}

// Newer reports whether release version latest is newer than current.
// current can be a build from git: "0.1.1-8-ge90e31c" is 8 commits past
// v0.1.1, so v0.1.1 is not newer. A pre-release such as "0.1.2-rc1" is
// older than "0.1.2". A version that does not start with numbers, such
// as "dev", is never updated, and neither is a plain `go build`
// ("0.1.0-dev"), whose real version is unknown.
func Newer(latest, current string) bool {
	lc, lpre, ok1 := split(latest)
	cc, cpre, ok2 := split(current)
	if !ok1 || !ok2 || strings.Contains(cpre, "dev") {
		return false
	}
	for i := range lc {
		if lc[i] != cc[i] {
			return lc[i] > cc[i]
		}
	}
	if lpre != "" {
		return false // never offer a pre-release over the same version
	}
	return cpre != "" && !gitDescribe(cpre)
}

// split parses "1.2.3-rest" into [1 2 3] and "rest".
func split(v string) ([3]int, string, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	if p, _, found := strings.Cut(core, "+"); found {
		core = p
	}
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, "", false
		}
		out[i] = n
	}
	return out, pre, true
}

// gitDescribe: "8-ge90e31c" or "8-ge90e31c-dirty", the part that
// `git describe` adds after the tag.
func gitDescribe(pre string) bool {
	n, rest, ok := strings.Cut(pre, "-")
	if !ok || !strings.HasPrefix(rest, "g") {
		return false
	}
	_, err := strconv.Atoi(n)
	return err == nil
}

// Kind is how zoo was installed, which decides how it updates.
type Kind string

const (
	Self     Kind = "self"     // a file that zoo can replace (zip, tarball, source build)
	Homebrew Kind = "homebrew" // brew upgrade zoo
	Package  Kind = "package"  // a .deb or .rpm
)

// KindOf tells how the zoo at exe was installed.
func KindOf(exe string) Kind {
	p := filepath.ToSlash(exe)
	for _, s := range []string{"/Cellar/", "/homebrew/", "/linuxbrew/"} {
		if strings.Contains(p, s) {
			return Homebrew
		}
	}
	if runtime.GOOS == "linux" && p == "/usr/bin/zoo" {
		return Package
	}
	return Self
}

// Command is what to run for an install that zoo does not replace itself.
func Command(k Kind) string {
	switch k {
	case Homebrew:
		return "brew upgrade zoo"
	case Package:
		return "download the .deb or .rpm from https://github.com/" + Repo + "/releases/latest and install it"
	}
	return "zoo update"
}

// Status is what the dashboard and `zoo update --check` show.
type Status struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	URL       string    `json:"url,omitempty"`
	Available bool      `json:"available"`
	Kind      Kind      `json:"kind"`
	Command   string    `json:"command"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// Check compares the latest release with current.
func Check(ctx context.Context, c *http.Client, current, exe string) Status {
	k := KindOf(exe)
	st := Status{Current: current, Kind: k, Command: Command(k), CheckedAt: time.Now().UTC()}
	r, err := Latest(ctx, c)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Latest = r.Version()
	// The dashboard links to this page, so only a GitHub page is kept.
	if strings.HasPrefix(r.URL, "https://github.com/") {
		st.URL = r.URL
	}
	st.Available = Newer(r.Version(), current)
	return st
}

// Apply downloads release r for this OS, checks it against the release's
// checksums.txt, and puts its zoo binary at exe. On Windows, a running
// exe cannot be overwritten but can be renamed, so the old one moves aside
// as <exe>.old-<n>. RemoveOld deletes those later.
func Apply(ctx context.Context, c *http.Client, r Release, exe string) error {
	name := ArchiveName(r.Version(), runtime.GOOS, runtime.GOARCH)
	arc, ok := r.asset(name)
	if !ok {
		return fmt.Errorf("release %s has no %s", r.Tag, name)
	}
	sums, ok := r.asset("checksums.txt")
	if !ok {
		return fmt.Errorf("release %s has no checksums.txt", r.Tag)
	}
	sumFile, err := download(ctx, c, sums.URL)
	if err != nil {
		return err
	}
	want, err := checksumFor(sumFile, name)
	if err != nil {
		return err
	}
	data, err := download(ctx, c, arc.URL)
	if err != nil {
		return err
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s: checksum does not match checksums.txt; nothing was changed", name)
	}
	bin, err := extract(name, data)
	if err != nil {
		return err
	}
	return replace(exe, bin)
}

func download(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zoo-update")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, maxDownload)
	}
	return b, nil
}

// checksumFor finds the SHA-256 for name in a checksums.txt file
// ("<hex>  <name>" per line).
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no line for %s", name)
}

// extract returns the zoo binary from a release archive.
func extract(name string, data []byte) ([]byte, error) {
	want := "zoo"
	if strings.HasSuffix(name, ".zip") {
		want = "zoo.exe"
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, fmt.Errorf("%s has no %s", name, want)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s", name, want)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && h.Name == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// replace puts bin at exe. The new file is written next to exe first, so
// a failed write leaves the old zoo in place.
func replace(exe string, bin []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".zoo-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after the rename
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if err := moveAside(exe); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, exe)
}

func moveAside(exe string) error {
	for i := 0; i < 100; i++ {
		old := fmt.Sprintf("%s.old-%d", exe, i)
		if _, err := os.Lstat(old); err == nil {
			continue
		}
		return os.Rename(exe, old)
	}
	return fmt.Errorf("too many old copies of %s", exe)
}

// RemoveOld deletes the zoo binaries that Apply moved aside, once nothing
// runs them any more.
func RemoveOld(exe string) {
	old, _ := filepath.Glob(exe + ".old-*")
	for _, p := range old {
		_ = os.Remove(p)
	}
}

// cacheFile holds the last check, so the daemon checks at most once a day.
func cacheFile() string { return filepath.Join(paths.Home(), "update.json") }

// Cached returns the last saved check, if any.
func Cached() (Status, bool) {
	b, err := os.ReadFile(cacheFile())
	if err != nil {
		return Status{}, false
	}
	var st Status
	if json.Unmarshal(b, &st) != nil {
		return Status{}, false
	}
	return st, true
}

// Save stores a check for Cached.
func Save(st Status) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(cacheFile(), b, 0o600)
}
