package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/images"
	"github.com/dusty/zoo/internal/paths"
)

// cageImage picks the image for a worktree's cage (zoo.yaml image: or an
// approved dockerfile:, else zoo's own) and builds a derived one if missing.
func cageImage(ctx context.Context, e env, root string, proj config.Project) (string, error) {
	d, err := images.Decide(root, e.g, proj)
	if err != nil {
		return "", err
	}
	if d.Note != "" {
		say("%s", d.Note)
	}
	if d.Source == nil {
		return e.g.CageImage, nil
	}
	return ensureCustomImage(ctx, e.b, e.g, *d.Source, false)
}

// ensureCustomImage builds the derived image for src unless it exists.
func ensureCustomImage(ctx context.Context, b backend.Backend, g config.Global, src images.Source, force bool) (string, error) {
	uid, gid := os.Getuid(), os.Getgid()
	tag, err := images.Tag(src, g.CageImage, uid, gid)
	if err != nil {
		return "", err
	}
	if !force {
		ok, err := b.ImageExists(ctx, tag)
		if err != nil {
			return "", err
		}
		if ok {
			return tag, nil
		}
	}
	say("building cage image %s from %s on %s (zoo adds its agent user and init)…", tag, src.Describe(), b.Name())
	dir, err := images.WriteContext(paths.Build(), src)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	err = b.BuildImage(ctx, backend.ImageSpec{Tag: tag, Context: dir, BuildArgs: map[string]string{
		"AGENT_UID": strconv.Itoa(uid),
		"AGENT_GID": strconv.Itoa(gid),
	}})
	if err != nil {
		return "", fmt.Errorf("building your cage image from %s: %w (the build context is empty: COPY from the worktree won't work; install what you need with RUN)", src.Describe(), err)
	}
	return tag, nil
}

// buildProject is `zoo build --project [path]`: approve this worktree's
// Dockerfile (after showing it) and build its cage image, or rebuild the
// image: one.
func buildProject(ctx context.Context, e env, args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	root, err := cage.Root(dir)
	if err != nil {
		return err
	}
	ensureTrust(root)
	proj, err := config.LoadProject(root)
	if err != nil {
		return err
	}
	image, df := images.Pick(e.g, proj)
	var src images.Source
	switch {
	case df != "":
		content, err := images.ReadDockerfile(root, df)
		if err != nil {
			return err
		}
		prev, had := images.LoadApproval(root)
		if had && prev.SHA256 == images.Sum(content) {
			say("%s is already approved; rebuilding", df)
		} else {
			if err := askApproveDockerfile(root, df, content, prev, had); err != nil {
				return err
			}
		}
		src = images.Source{Dockerfile: content, Path: df}
		tag, err := ensureCustomImage(ctx, e.b, e.g, src, true)
		if err != nil {
			return err
		}
		if err := images.Approve(root, df, content); err != nil {
			return err
		}
		say("approved %s and built %s", df, tag)
	case image != "":
		src = images.Source{Base: image}
		tag, err := ensureCustomImage(ctx, e.b, e.g, src, true)
		if err != nil {
			return err
		}
		say("built %s from %s", tag, image)
	default:
		return fmt.Errorf("no image: or dockerfile: for %s (in its approved zoo.yaml, or %s)", root, paths.Config())
	}
	say("stopped cages pick it up on their next run; a running one after `zoo reset %s`", cage.NameFor(root))
	return nil
}

// askApproveDockerfile shows a Dockerfile and asks the human to approve it.
func askApproveDockerfile(root, df string, content []byte, prev images.Approval, had bool) error {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		return fmt.Errorf("%s isn't approved; run `zoo build --project` from a terminal to review it", df)
	}
	say("%s in %s. Agents can write this file, and its RUN steps run with network access outside the keeper, so read it:", df, root)
	fmt.Fprintln(os.Stderr)
	for _, l := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
		fmt.Fprintln(os.Stderr, "    "+l)
	}
	fmt.Fprintln(os.Stderr)
	if had {
		if d, err := unifiedDiff(prev.Content, string(content)); err == nil && d != "" {
			say("changes since you last approved it:")
			for _, l := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
				fmt.Fprintln(os.Stderr, "    "+l)
			}
		}
	}
	fmt.Fprint(os.Stderr, "Build it (with an empty build context) and use it for this worktree's cage? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New("not approved; cages keep the image they had")
}

// checkHarness fails early, with a hint, when a cage built from your own
// image doesn't have the harness's CLI.
func checkHarness(ctx context.Context, b backend.Backend, id, image, harness, command string) error {
	bin := strings.Fields(command)
	if len(bin) == 0 || bin[0] == "bash" || bin[0] == "sh" {
		return nil
	}
	var out bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: id, User: "agent", NoStdin: true,
		Cmd:    []string{"sh", "-c", `command -v "$1"`, "sh", bin[0]},
		Stdout: io.Discard, Stderr: &out,
	})
	if err != nil || code == 0 {
		return nil // couldn't check; let the session say what's wrong
	}
	hint := ""
	switch harness {
	case "claude":
		hint = "\n  e.g. RUN npm install -g @anthropic-ai/claude-code"
	case "codex":
		hint = "\n  e.g. RUN npm install -g @openai/codex"
	}
	return fmt.Errorf("%s isn't installed in this cage's image (%s), which you supply with image: or dockerfile:.\nInstall it where the agent user can run it (on PATH, not under /root), rebuild with `zoo build --project`, then `zoo reset %s`%s",
		bin[0], image, strings.TrimPrefix(id, cage.Prefix), hint)
}
