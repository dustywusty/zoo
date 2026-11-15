package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/pathmatch"
	"github.com/dusty/zoo/internal/tools"
)

const toolsUsage = `usage:
  zoo tools                           the tools for this worktree's cage, and all lists
  zoo tools add <tool>... [where]     install tools in cage images
  zoo tools rm <tool>... [where]      stop installing them
  zoo tools catalog                   the tools zoo knows by name

where (default: this worktree):
  --all          every cage
  --for <dir>    cages whose worktree is <dir> or inside it (a project folder)

A tool is a catalog name (gh, go, bun) or apt:NAME, npm:NAME, pip:NAME.`

// cmdTools edits ~/.zoo/tools.json. Images change at the next cage start.
func cmdTools(args []string) error {
	if len(args) == 0 {
		return toolsShow()
	}
	switch args[0] {
	case "catalog":
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, n := range tools.Names() {
			fmt.Fprintf(tw, "%s\t%s\n", n, tools.Catalog[n].Desc)
		}
		fmt.Fprintln(tw, "apt:NAME\ta Debian package (zoo's cage image is Debian)")
		fmt.Fprintln(tw, "npm:NAME\ta global npm package")
		fmt.Fprintln(tw, "pip:NAME\ta Python package, installed for every user")
		return tw.Flush()
	case "add", "rm":
		names, folder, err := toolsArgs(args[1:])
		if err != nil {
			return err
		}
		l, err := tools.Load()
		if err != nil {
			return err
		}
		if args[0] == "add" {
			if err := l.Add(folder, names...); err != nil {
				return err
			}
		} else if missing := l.Remove(folder, names...); len(missing) > 0 {
			say("not on that list: %s", strings.Join(missing, ", "))
		}
		if err := tools.Save(l); err != nil {
			return err
		}
		where := "every cage"
		if folder != "" {
			where = "cages in " + folder
		}
		say("tools for %s: %s", where, orNone(listFor(l, folder)))
		say("zoo builds the new image when a cage starts (or now: zoo build --project). A running cage keeps its image until it restarts: zoo stop <cage>, then zoo run")
		return nil
	case "help", "-h", "--help":
		fmt.Println(toolsUsage)
		return nil
	}
	return errors.New(toolsUsage)
}

// toolsArgs splits tool names from --all / --for <dir>. The default list
// is "" (--all); with neither flag, the list is this worktree's.
func toolsArgs(args []string) (names []string, folder string, err error) {
	all, forDir := false, ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--all":
			all = true
		case a == "--for":
			if i+1 >= len(args) {
				return nil, "", errors.New("--for needs a folder")
			}
			i++
			forDir = args[i]
		case strings.HasPrefix(a, "--for="):
			forDir = strings.TrimPrefix(a, "--for=")
		case strings.HasPrefix(a, "-"):
			return nil, "", fmt.Errorf("unknown flag %s\n%s", a, toolsUsage)
		default:
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return nil, "", errors.New(toolsUsage)
	}
	switch {
	case all && forDir != "":
		return nil, "", errors.New("use --all or --for, not both")
	case all:
		return names, "", nil
	case forDir != "":
		abs, err := filepath.Abs(pathmatch.ExpandHome(forDir))
		if err != nil {
			return nil, "", err
		}
		return names, abs, nil
	}
	root, err := cage.Root(".")
	if err != nil {
		return nil, "", fmt.Errorf("not in a git worktree: use --all or --for <dir> (%w)", err)
	}
	return names, root, nil
}

func listFor(l tools.Lists, folder string) []string {
	if folder == "" {
		return l.Default
	}
	for _, f := range l.Folders {
		if pathmatch.Same(f.Path, folder) {
			return f.Tools
		}
	}
	return nil
}

func orNone(ts []string) string {
	if len(ts) == 0 {
		return "none"
	}
	return strings.Join(ts, ", ")
}

func toolsShow() error {
	l, err := tools.Load()
	if err != nil {
		return err
	}
	if root, err := cage.Root("."); err == nil {
		fmt.Printf("this worktree (%s): %s\n\n", root, orNone(l.For(root)))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LIST\tTOOLS")
	fmt.Fprintf(tw, "every cage (--all)\t%s\n", orNone(l.Default))
	for _, f := range l.Folders {
		fmt.Fprintf(tw, "%s\t%s\n", f.Path, orNone(f.Tools))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nzoo tools catalog: tools zoo knows by name. zoo tools add <tool> [--all | --for <dir>].")
	return nil
}
