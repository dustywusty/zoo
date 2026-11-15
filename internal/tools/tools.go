// Package tools keeps the lists of tools that zoo installs in cage images:
// a default list for every cage, and lists for folders (a project, or one
// worktree). zoo builds them into the image, so they survive zoo reset,
// and an agent never needs root or a package server to get them.
//
// The lists live in ~/.zoo/tools.json, which zoo writes (zoo tools add,
// and later the dashboard). Agents cannot reach ~/.zoo, so they cannot
// add tools, whose installs run with network access outside the keeper.
package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/pathmatch"
	"github.com/dusty/zoo/internal/paths"
)

// Recipe installs one tool that Debian packages badly or not at all.
type Recipe struct {
	Desc   string
	Script string   // POSIX sh, runs as root while the image builds
	Env    []string // ENV lines for the image, such as PATH additions
}

// Catalog is the tools zoo knows by name.
var Catalog = map[string]Recipe{
	"gh": {
		Desc: "GitHub CLI, from GitHub's own apt repository",
		Script: `need_apt gh
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
chmod 0644 /etc/apt/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y --no-install-recommends gh
rm -rf /var/lib/apt/lists/*`,
	},
	"go": {
		Desc: "Go, the current release from go.dev, checked against its SHA-256",
		Script: `case "$(uname -m)" in x86_64) a=amd64 ;; aarch64|arm64) a=arm64 ;; *) echo "zoo tools: go: no build for $(uname -m)" >&2; exit 1 ;; esac
v=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n 1)
f="$v.linux-$a.tar.gz"
sum=$(curl -fsSL 'https://go.dev/dl/?mode=json' | tr -d ' \n' | grep -o "\"filename\":\"$f\",\"os\":\"linux\",\"arch\":\"$a\",\"version\":\"$v\",\"sha256\":\"[0-9a-f]*\"" | sed 's/.*"sha256":"//; s/"$//')
[ -n "$sum" ] || { echo "zoo tools: go: no checksum for $f on go.dev" >&2; exit 1; }
curl -fsSL "https://go.dev/dl/$f" -o /tmp/go.tgz
echo "$sum  /tmp/go.tgz" | sha256sum -c -
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
# Linked into /usr/local/bin: Debian's /etc/profile resets PATH in login shells.
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/`,
	},
	"bun": {
		Desc: "Bun, from bun.sh",
		Script: `command -v unzip >/dev/null 2>&1 || { need_apt unzip; apt-get update; apt-get install -y --no-install-recommends unzip; rm -rf /var/lib/apt/lists/*; }
curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash`,
	},
}

// A package name for apt:, npm: or pip:, with an optional version
// (=1.2, @1.2). It goes into a shell command, so it stays plain.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._+/@-]*([=@]=?[A-Za-z0-9._+~:-]+)?$`)

// Validate checks one tool: a catalog name, or apt:NAME, npm:NAME, pip:NAME.
func Validate(t string) error {
	if _, ok := Catalog[t]; ok {
		return nil
	}
	kind, name, ok := strings.Cut(t, ":")
	if !ok {
		return fmt.Errorf("unknown tool %q: use one of %s, or apt:NAME, npm:NAME, pip:NAME", t, strings.Join(Names(), ", "))
	}
	switch kind {
	case "apt", "npm", "pip":
	default:
		return fmt.Errorf("unknown tool kind %q in %q: use apt:, npm: or pip:", kind, t)
	}
	if len(name) > 128 || !nameRe.MatchString(name) {
		return fmt.Errorf("%q is not a package name zoo accepts", name)
	}
	return nil
}

// Names lists the catalog.
func Names() []string {
	var out []string
	for n := range Catalog {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Folder is a list for one folder and everything in it.
type Folder struct {
	Path  string   `json:"path"`
	Tools []string `json:"tools"`
}

// Lists is the whole tools.json.
type Lists struct {
	Default []string `json:"default"`
	Folders []Folder `json:"folders,omitempty"`
}

func file() string { return filepath.Join(paths.Home(), "tools.json") }

// Load reads tools.json. A missing file means no tools.
func Load() (Lists, error) {
	b, err := os.ReadFile(file())
	if errors.Is(err, os.ErrNotExist) {
		return Lists{}, nil
	}
	if err != nil {
		return Lists{}, err
	}
	var l Lists
	if err := json.Unmarshal(b, &l); err != nil {
		return Lists{}, fmt.Errorf("%s: %w", file(), err)
	}
	return l, nil
}

// Save writes tools.json.
func Save(l Lists) error {
	if err := os.MkdirAll(paths.Home(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file(), append(b, '\n'), 0o600)
}

// For returns the tools for a cage whose worktree is root: the default
// list and the list of every folder that contains root. The result is
// sorted, so the same tools always give the same image.
func (l Lists) For(root string) []string {
	seen := map[string]bool{}
	add := func(ts []string) {
		for _, t := range ts {
			seen[t] = true
		}
	}
	add(l.Default)
	for _, f := range l.Folders {
		if pathmatch.In(root, f.Path) {
			add(f.Tools)
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Add puts tools on the default list (folder "") or a folder's list.
func (l *Lists) Add(folder string, ts ...string) error {
	for _, t := range ts {
		if err := Validate(t); err != nil {
			return err
		}
	}
	list := l.list(folder, true)
	for _, t := range ts {
		if !contains(*list, t) {
			*list = append(*list, t)
		}
	}
	return nil
}

// Remove takes tools off a list. It reports the ones that were not there.
func (l *Lists) Remove(folder string, ts ...string) (missing []string) {
	list := l.list(folder, false)
	for _, t := range ts {
		if list == nil || !contains(*list, t) {
			missing = append(missing, t)
			continue
		}
		kept := (*list)[:0]
		for _, x := range *list {
			if x != t {
				kept = append(kept, x)
			}
		}
		*list = kept
	}
	// Drop folders with nothing left.
	kept := l.Folders[:0]
	for _, f := range l.Folders {
		if len(f.Tools) > 0 {
			kept = append(kept, f)
		}
	}
	l.Folders = kept
	return missing
}

func (l *Lists) list(folder string, create bool) *[]string {
	if folder == "" {
		return &l.Default
	}
	folder = filepath.Clean(folder)
	for i := range l.Folders {
		if pathmatch.Same(l.Folders[i].Path, folder) {
			return &l.Folders[i].Tools
		}
	}
	if !create {
		return nil
	}
	l.Folders = append(l.Folders, Folder{Path: folder})
	return &l.Folders[len(l.Folders)-1].Tools
}

func contains(list []string, t string) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}

// Script is the shell script that installs ts while the image builds, as
// root, with network access. It stops at the first failure. env is the
// ENV lines the tools need.
func Script(ts []string) (script string, env []string, err error) {
	var apt, npm, pip []string
	var b strings.Builder
	b.WriteString(`#!/bin/sh
# zoo-tools: installs the tools from ~/.zoo/tools.json (zoo tools).
set -eu
export DEBIAN_FRONTEND=noninteractive
need_apt() { command -v apt-get >/dev/null 2>&1 || { echo "zoo tools: $1 needs apt-get, and this image has none" >&2; exit 1; }; }
`)
	for _, t := range ts {
		if err := Validate(t); err != nil {
			return "", nil, err
		}
		if r, ok := Catalog[t]; ok {
			fmt.Fprintf(&b, "\necho '--- zoo tools: %s'\n%s\n", t, r.Script)
			env = append(env, r.Env...)
			continue
		}
		kind, name, _ := strings.Cut(t, ":")
		switch kind {
		case "apt":
			apt = append(apt, name)
		case "npm":
			npm = append(npm, name)
		case "pip":
			pip = append(pip, name)
		}
	}
	if len(apt) > 0 {
		fmt.Fprintf(&b, "\necho '--- zoo tools: apt %s'\nneed_apt apt:\napt-get update\napt-get install -y --no-install-recommends %s\nrm -rf /var/lib/apt/lists/*\n", strings.Join(apt, " "), strings.Join(apt, " "))
	}
	if len(npm) > 0 {
		fmt.Fprintf(&b, "\necho '--- zoo tools: npm %s'\ncommand -v npm >/dev/null 2>&1 || { echo 'zoo tools: npm: tools need npm, and this image has none' >&2; exit 1; }\nnpm install -g --no-fund --no-audit %s\nnpm cache clean --force\n", strings.Join(npm, " "), strings.Join(npm, " "))
	}
	if len(pip) > 0 {
		fmt.Fprintf(&b, "\necho '--- zoo tools: pip %s'\ncommand -v pip3 >/dev/null 2>&1 || { echo 'zoo tools: pip: tools need pip3, and this image has none' >&2; exit 1; }\npip3 install --no-cache-dir --break-system-packages %s\n", strings.Join(pip, " "), strings.Join(pip, " "))
	}
	return b.String(), env, nil
}
