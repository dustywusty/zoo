package tools

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, ok := range []string{"gh", "go", "bun", "apt:postgresql-client", "apt:libpq-dev=15.4-0", "npm:pnpm", "npm:@scope/pkg@1.2.3", "pip:ruff", "pip:ruff==0.6.0"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want ok", ok, err)
		}
	}
	// Names go into a shell command, so anything that could break out of
	// it is refused.
	for _, bad := range []string{"nope", "apt:", "apt:a;rm -rf /", "apt:$(id)", "npm:a b", "pip:`x`", "brew:jq", "apt:-y", "apt:a|b", "apt:a&&b"} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", bad)
		}
	}
}

func TestForMergesDefaultAndFolders(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "workspaces", "zoo")
	var l Lists
	if err := l.Add("", "gh", "apt:jq"); err != nil {
		t.Fatal(err)
	}
	if err := l.Add(proj, "go", "gh"); err != nil {
		t.Fatal(err)
	}
	if got, want := l.For(filepath.Join(proj, "goldfish")), []string{"apt:jq", "gh", "go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("in the project: %v, want %v", got, want)
	}
	if got, want := l.For(filepath.Join(root, "workspaces", "zoo-other")), []string{"apt:jq", "gh"}; !reflect.DeepEqual(got, want) {
		t.Errorf("next to the project: %v, want %v", got, want)
	}
	if missing := l.Remove(proj, "go", "rust"); !reflect.DeepEqual(missing, []string{"rust"}) {
		t.Errorf("Remove missing = %v", missing)
	}
	if missing := l.Remove(proj, "gh"); len(missing) != 0 || len(l.Folders) != 0 {
		t.Errorf("an empty folder list stays: %+v", l.Folders)
	}
	if err := l.Add("", "nope"); err == nil {
		t.Error("Add accepted an unknown tool")
	}
}

func TestSaveLoad(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if l, err := Load(); err != nil || len(l.Default) != 0 {
		t.Fatalf("Load with no file = %+v %v", l, err)
	}
	var l Lists
	_ = l.Add("", "gh")
	if err := Save(l); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || !reflect.DeepEqual(got.Default, []string{"gh"}) {
		t.Errorf("Load = %+v %v", got, err)
	}
}

func TestScript(t *testing.T) {
	s, _, err := Script([]string{"apt:jq", "apt:tree", "gh", "npm:pnpm", "pip:ruff"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"set -eu", "cli.github.com/packages", "apt-get install -y --no-install-recommends jq tree", "npm install -g --no-fund --no-audit pnpm", "pip3 install --no-cache-dir --break-system-packages ruff"} {
		if !strings.Contains(s, want) {
			t.Errorf("script has no %q:\n%s", want, s)
		}
	}
	if _, _, err := Script([]string{"apt:a;b"}); err == nil {
		t.Error("Script accepted a bad name")
	}
}
