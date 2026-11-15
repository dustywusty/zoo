package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"0.1.2", "0.1.1", true},
		{"0.2.0", "0.1.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.1", "0.1.1", false},
		{"0.1.1", "0.1.2", false},
		{"0.1.1", "0.1.1-8-ge90e31c", false},       // a build 8 commits past the tag
		{"0.1.1", "0.1.1-8-ge90e31c-dirty", false}, // same, with local changes
		{"0.1.2", "0.1.1-8-ge90e31c", true},
		{"0.1.2", "0.1.2-rc1", true},  // the release replaces its pre-release
		{"0.1.2-rc2", "0.1.1", true},  // a newer core wins
		{"0.1.2-rc1", "0.1.2", false}, // never a pre-release over the same version
		{"0.1.1", "0.1.0-dev", false}, // a plain go build: version unknown
		{"0.1.1", "dev", false},
		{"garbage", "0.1.0", false},
		{"v0.1.2", "v0.1.1", true},
	} {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestKindOf(t *testing.T) {
	for exe, want := range map[string]Kind{
		"/opt/homebrew/Cellar/zoo/0.1.1/bin/zoo":           Homebrew,
		"/home/linuxbrew/.linuxbrew/bin/zoo":               Homebrew,
		filepath.Join("C:", "Users", "x", ".local", "bin"): Self,
		"/home/x/.local/bin/zoo":                           Self,
	} {
		if got := KindOf(exe); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", exe, got, want)
		}
	}
	if runtime.GOOS == "linux" && KindOf("/usr/bin/zoo") != Package {
		t.Error("/usr/bin/zoo on Linux is not a package install")
	}
}

// fakeGitHub serves a latest release with an archive for this OS that
// holds bin, and a checksums.txt. A non-empty badSum replaces the
// archive's checksum.
func fakeGitHub(t *testing.T, version string, bin []byte, badSum string) *httptest.Server {
	t.Helper()
	name := ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	arc := archive(t, name, bin)
	sum := sha256.Sum256(arc)
	line := hex.EncodeToString(sum[:])
	if badSum != "" {
		line = badSum
	}
	sums := fmt.Sprintf("%s  %s\n%s  zoo_%s_other_os.tar.gz\n", line, name, hex.EncodeToString(make([]byte, 32)), version)

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Release{
			Tag: "v" + version, URL: "https://example.invalid/release",
			Assets: []Asset{
				{Name: name, URL: srv.URL + "/dl/" + name},
				{Name: "checksums.txt", URL: srv.URL + "/dl/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("/dl/"+name, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(arc) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sums)) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })
	return srv
}

func archive(t *testing.T, name string, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if filepath.Ext(name) == ".zip" {
		zw := zip.NewWriter(&buf)
		for _, f := range []struct {
			name string
			body []byte
		}{{"README.md", []byte("readme")}, {"zoo.exe", bin}} {
			w, err := zw.Create(f.name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(f.body)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"README.md", []byte("readme")}, {"zoo", bin}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestCheckAndApply(t *testing.T) {
	srv := fakeGitHub(t, "0.2.0", []byte("new zoo"), "")
	exe := filepath.Join(t.TempDir(), "zoo")
	if err := os.WriteFile(exe, []byte("old zoo"), 0o755); err != nil {
		t.Fatal(err)
	}

	st := Check(context.Background(), srv.Client(), "0.1.1", exe)
	if st.Error != "" || !st.Available || st.Latest != "0.2.0" || st.Kind != Self || st.URL != "" {
		t.Fatalf("Check = %+v (the link is not a GitHub page, so it must be dropped)", st)
	}
	r, err := Latest(context.Background(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), srv.Client(), r, exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new zoo" {
		t.Errorf("zoo after update = %q", b)
	}
	RemoveOld(exe)
	if old, _ := filepath.Glob(exe + ".old-*"); len(old) != 0 {
		t.Errorf("old copies left: %v", old)
	}
}

func TestApplyRefusesABadChecksum(t *testing.T) {
	srv := fakeGitHub(t, "0.2.0", []byte("tampered zoo"), hex.EncodeToString(make([]byte, 32)))
	exe := filepath.Join(t.TempDir(), "zoo")
	if err := os.WriteFile(exe, []byte("old zoo"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Latest(context.Background(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), srv.Client(), r, exe); err == nil {
		t.Fatal("Apply accepted an archive whose checksum does not match")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old zoo" {
		t.Errorf("zoo changed after a refused update: %q", b)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if _, ok := Cached(); ok {
		t.Fatal("a cache exists before any check")
	}
	if err := Save(Status{Current: "0.1.1", Latest: "0.2.0", Available: true}); err != nil {
		t.Fatal(err)
	}
	if st, ok := Cached(); !ok || st.Latest != "0.2.0" || !st.Available {
		t.Errorf("Cached = %+v %v", st, ok)
	}
}
