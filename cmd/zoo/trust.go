package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

// cmdTrust approves a worktree's zoo.yaml as it is on disk now.
//
//	zoo trust [path]          show what changed, then approve it
//	zoo trust --show [path]   show the approved copy and any pending change
func cmdTrust(args []string) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	show := false
	var rest []string
	for _, a := range args {
		if a == "--show" {
			show = true
		} else {
			rest = append(rest, a)
		}
	}
	dir := "."
	if len(rest) > 0 {
		dir = rest[0]
	}
	root, err := cage.Root(dir)
	if err != nil {
		return err
	}
	st, err := config.CheckTrust(root)
	if err != nil {
		return err
	}
	if show {
		if st.Approved == "" {
			fmt.Println("# no zoo.yaml approved for " + root)
		} else {
			fmt.Print(st.Approved)
		}
		if st.Changed {
			fmt.Fprintln(os.Stderr)
			say("zoo.yaml on disk differs from the approved copy:")
			printTrustDiff(st)
		}
		return nil
	}
	if !st.Changed {
		say("zoo.yaml in %s is already approved", root)
		return nil
	}
	printTrustDiff(st)
	if _, err := config.Trust(root); err != nil {
		return err
	}
	say("approved %s; it applies now to grants and on the next cage creation to mounts, masks and image (zoo reset <cage>)", config.ProjectFile(root))
	return nil
}

// ensureTrust is called before a run: if zoo.yaml changed since it was
// approved, ask on a terminal and keep the approved copy otherwise.
func ensureTrust(root string) {
	st, err := config.CheckTrust(root)
	if err != nil {
		say("zoo.yaml: %v", err)
		return
	}
	if !st.Changed {
		return
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		say("WARNING: %s changed since you approved it (an agent can write it). Using the approved copy; review and apply with `zoo trust %s`", config.ProjectFile(root), root)
		return
	}
	what := "changed since you approved it"
	if !st.EverTrust {
		what = "hasn't been approved yet"
	}
	say("%s %s. Agents can write this file, so zoo only uses what you approve:", config.ProjectFile(root), what)
	printTrustDiff(st)
	fmt.Fprint(os.Stderr, "Apply it? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		if _, err := config.Trust(root); err != nil {
			say("not applied: %v", err)
			return
		}
		say("applied")
		return
	}
	say("not applied; using the approved copy (zoo trust to apply later)")
}

func printTrustDiff(st config.TrustState) {
	if !st.Present {
		fmt.Fprintln(os.Stderr, "    (zoo.yaml was deleted)")
		return
	}
	if d, err := unifiedDiff(st.Approved, st.Proposed); err == nil && d != "" {
		for _, l := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
			fmt.Fprintln(os.Stderr, "    "+l)
		}
		return
	}
	for _, l := range strings.Split(strings.TrimRight(st.Proposed, "\n"), "\n") {
		fmt.Fprintln(os.Stderr, "    + "+l)
	}
}

func unifiedDiff(a, b string) (string, error) {
	dir, err := os.MkdirTemp(paths.Tmp(), "trust-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	pa, pb := filepath.Join(dir, "approved"), filepath.Join(dir, "zoo.yaml")
	if err := os.WriteFile(pa, []byte(a), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(pb, []byte(b), 0o600); err != nil {
		return "", err
	}
	out, err := exec.Command("diff", "-u", "--label", "approved", "--label", "zoo.yaml", pa, pb).Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		return "", err
	}
	return string(out), nil
}
