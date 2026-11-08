package cage

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/proc"
)

// A repository can arrive with hooks and config that run commands the next
// time *you* use git in it: a "take-home assignment" zip whose .git/hooks
// runs a remote payload on commit, or a .git/config with core.fsmonitor set.
// Cages can't write those (they're read-only mounts), but zoo can't stop
// what was there before the first `zoo run`, so it looks and says so.
//
// Separately, some config points git at files *inside the worktree*, which
// the agent can write: core.hooksPath=.husky is the common one. A hook the
// agent drops there runs on your machine at your next commit. Those paths
// are made read-only in the cage (Plan.ReadOnly) and reported here.

// GitRisk is something in a repository's git setup that runs commands.
type GitRisk struct {
	Path   string `json:"path"`   // file or config key
	Reason string `json:"reason"` // what it does
	// Writable is set when the command lives in the worktree, where the
	// agent can change it. Plan guards these; they're listed so the user
	// knows why a path is read-only in the cage.
	Writable string `json:"writable,omitempty"`
}

// commandKeys are config keys whose values git runs (or loads config from).
// Matched on the key with any middle section removed: filter.<name>.clean
// is checked as "filter.clean".
var commandKeys = map[string]string{
	"core.fsmonitor":             "runs on every git status",
	"core.hookspath":             "moves hooks somewhere else",
	"core.sshcommand":            "runs for every fetch and push over ssh",
	"core.pager":                 "runs for git log, diff and show",
	"core.editor":                "runs when git opens an editor",
	"core.askpass":               "runs when git asks for a password",
	"core.gitproxy":              "runs for git:// fetches",
	"sequence.editor":            "runs for interactive rebases",
	"diff.external":              "runs for every diff",
	"diff.textconv":              "runs when diffing matching files",
	"diff.command":               "runs when diffing matching files",
	"merge.driver":               "runs when merging matching files",
	"filter.clean":               "runs when staging matching files",
	"filter.smudge":              "runs on checkout of matching files",
	"filter.process":             "runs on checkout and staging of matching files",
	"include.path":               "pulls in more config",
	"includeif.path":             "pulls in more config",
	"gpg.program":                "runs when signing or verifying",
	"gpg.openpgp.program":        "runs when signing or verifying",
	"gpg.x509.program":           "runs when signing or verifying",
	"gpg.ssh.program":            "runs when signing or verifying",
	"uploadpack.packobjectshook": "runs when someone fetches from this repo",
	"remote.uploadpack":          "runs on fetch",
	"remote.receivepack":         "runs on push",
	"credential.helper":          "runs when git needs credentials",
}

// benignValues are common values not worth a warning.
var benignValues = map[string]bool{
	"git-lfs filter-process": true, "git-lfs clean -- %f": true, "git-lfs smudge -- %f": true,
	"osxkeychain": true, "cache": true, "store": true, "manager": true, "manager-core": true,
	"less": true, "cat": true, "vim": true, "vi": true, "nano": true,
}

func normKey(k string) string {
	k = strings.ToLower(k)
	parts := strings.Split(k, ".")
	if len(parts) > 2 {
		// section.<subsection>.key -> section.key (subsections may contain dots)
		return parts[0] + "." + parts[len(parts)-1]
	}
	return k
}

// gitConfigEntries lists key/value pairs in one config file, without
// following includes (git config --file doesn't by default).
func gitConfigEntries(file string) [][2]string {
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	cmd := exec.Command("git", "config", "--file", file, "--null", "--list")
	proc.Quiet(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var res [][2]string
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		k, v, _ := strings.Cut(string(rec), "\n")
		res = append(res, [2]string{k, v})
	}
	return res
}

// inWorktree returns the paths a config value names inside root: the value
// itself for path keys, or any word of a command that is an existing file
// under root (commands run from the worktree root).
func inWorktree(root, key, value string) []string {
	var out []string
	consider := func(p string) {
		if p == "" {
			return
		}
		if strings.HasPrefix(p, "~") {
			return // outside the worktree
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if within(p, root) && p != root && !within(p, filepath.Join(root, ".git")) {
			out = append(out, p)
		}
	}
	switch normKey(key) {
	case "core.hookspath":
		// Created if missing, so the agent can't create it either.
		consider(value)
		return out
	case "include.path", "includeif.path":
		consider(value)
		return out
	}
	for _, w := range strings.Fields(strings.TrimPrefix(value, "!")) {
		w = strings.Trim(w, `"'`)
		if strings.HasPrefix(w, "-") || strings.Contains(w, "%") {
			continue
		}
		abs := w
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, w)
		}
		if st, err := os.Stat(abs); err == nil && !st.IsDir() {
			consider(w)
		}
	}
	return out
}

// HostGitRisks lists hooks and command-running config in root's repository.
func HostGitRisks(root string) []GitRisk {
	common, gitDir := gitPaths(root)
	if common == "" {
		return nil
	}
	var risks []GitRisk
	hooksDir := filepath.Join(common, "hooks")
	files := []string{filepath.Join(common, "config")}
	if gitDir != "" && gitDir != common {
		files = append(files, filepath.Join(gitDir, "config.worktree"))
	}
	for _, f := range files {
		for _, kv := range gitConfigEntries(f) {
			k, v := kv[0], kv[1]
			nk := normKey(k)
			why, ok := commandKeys[nk]
			if !ok || benignValues[strings.TrimSpace(v)] {
				continue
			}
			if nk == "core.hookspath" {
				hp := v
				if !filepath.IsAbs(hp) && !strings.HasPrefix(hp, "~") {
					hp = filepath.Join(root, hp)
				}
				hooksDir = filepath.Clean(hp)
			}
			r := GitRisk{Path: k + " = " + v, Reason: why + " (" + f + ")"}
			if w := inWorktree(root, k, v); len(w) > 0 {
				r.Writable = strings.Join(w, ", ")
			}
			risks = append(risks, r)
		}
	}
	if ents, err := os.ReadDir(hooksDir); err == nil {
		for _, e := range ents {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".sample") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if !isHookName(e.Name()) {
				continue
			}
			r := GitRisk{Path: filepath.Join(hooksDir, e.Name()), Reason: "hook: runs on " + hookWhen(e.Name())}
			if within(hooksDir, root) && !within(hooksDir, filepath.Join(root, ".git")) {
				r.Writable = hooksDir
			}
			risks = append(risks, r)
		}
	}
	sort.SliceStable(risks, func(i, j int) bool { return risks[i].Path < risks[j].Path })
	return risks
}

// WritableGitPaths are the worktree paths git config makes executable on the
// host; the plan mounts them read-only. A hooks dir that doesn't exist yet
// is created (empty) so the agent can't create it.
func WritableGitPaths(root string) []string {
	common, gitDir := gitPaths(root)
	if common == "" {
		return nil
	}
	files := []string{filepath.Join(common, "config")}
	if gitDir != "" && gitDir != common {
		files = append(files, filepath.Join(gitDir, "config.worktree"))
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		for _, kv := range gitConfigEntries(f) {
			if _, ok := commandKeys[normKey(kv[0])]; !ok || benignValues[strings.TrimSpace(kv[1])] {
				continue
			}
			for _, p := range inWorktree(root, kv[0], kv[1]) {
				if normKey(kv[0]) == "core.hookspath" {
					_ = os.MkdirAll(p, 0o755)
				}
				if exists(p) && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

var hookNames = map[string]string{
	"applypatch-msg": "git am", "pre-applypatch": "git am", "post-applypatch": "git am",
	"pre-commit": "commit", "pre-merge-commit": "merge", "prepare-commit-msg": "commit",
	"commit-msg": "commit", "post-commit": "commit", "pre-rebase": "rebase",
	"post-checkout": "checkout and clone", "post-merge": "pull and merge", "pre-push": "push",
	"pre-receive": "push to this repo", "update": "push to this repo", "proc-receive": "push to this repo",
	"post-receive": "push to this repo", "post-update": "push to this repo",
	"reference-transaction": "every ref change", "push-to-checkout": "push to this repo",
	"pre-auto-gc": "automatic gc", "post-rewrite": "amend and rebase",
	"sendemail-validate": "git send-email", "fsmonitor-watchman": "git status (with core.fsmonitor)",
	"p4-changelist": "git p4", "p4-prepare-changelist": "git p4", "p4-post-changelist": "git p4",
	"p4-pre-submit": "git p4", "post-index-change": "every index write",
}

func isHookName(n string) bool { _, ok := hookNames[n]; return ok }
func hookWhen(n string) string { return hookNames[n] }
