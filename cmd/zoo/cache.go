package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/paths"
)

// cmdCache lists and clears package caches (config cache:).
//
//	zoo cache ls
//	zoo cache clear <cage|shared|all>
func cmdCache(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		var running map[string]bool
		var users map[string][]string
		if e, err := resolve(ctx, g); err == nil {
			running, users, _ = cacheUse(ctx, e.b)
		}
		return cacheLs(running, users)
	case "clear", "rm":
		if len(args) != 2 {
			return errors.New("usage: zoo cache clear <cage|shared|all>")
		}
		e, err := resolve(ctx, g)
		if err != nil {
			return fmt.Errorf("can't tell whether cages are using the cache: %w", err)
		}
		running, _, err := cacheUse(ctx, e.b)
		if err != nil {
			return fmt.Errorf("can't tell whether cages are using the cache: %w", err)
		}
		return cacheClear(ctx, e.b, args[1], running)
	}
	return fmt.Errorf("unknown cache command %q (ls, clear)", args[0])
}

// cacheUse maps cache dirs to whether a running cage has them mounted, and
// to the cages created with them.
func cacheUse(ctx context.Context, b backend.Backend) (running map[string]bool, users map[string][]string, err error) {
	running, users = map[string]bool{}, map[string][]string{}
	all, err := b.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, st := range all {
		name := st.Labels[cage.LabelCage]
		if name == "" {
			continue
		}
		dir := cage.CacheDir(st.Labels[cage.LabelCache], name)
		if dir == "" {
			continue
		}
		users[dir] = append(users[dir], name)
		if st.Running {
			running[dir] = true
		}
	}
	return running, users, nil
}

type cacheEntry struct{ label, dir string }

func cacheEntries() []cacheEntry {
	var out []cacheEntry
	ents, _ := os.ReadDir(paths.CacheProjects())
	for _, d := range ents {
		if d.IsDir() {
			out = append(out, cacheEntry{d.Name(), paths.CacheProject(d.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].label < out[j].label })
	if st, err := os.Stat(paths.CacheShared()); err == nil && st.IsDir() {
		out = append(out, cacheEntry{cage.CacheShared, paths.CacheShared()})
	}
	return out
}

func cacheLs(running map[string]bool, users map[string][]string) error {
	ents := cacheEntries()
	if len(ents) == 0 {
		fmt.Println("no package caches yet (they're made when a cage starts; config cache: project | shared | off)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CACHE\tSIZE\tIN USE\tPATH")
	var total int64
	for _, c := range ents {
		n, err := cage.DirSize(c.dir)
		size := humanBytes(n)
		if err != nil {
			size += " (partial)"
		}
		total += n
		use := "-"
		switch {
		case running[c.dir]:
			use = "running"
		case c.label == cage.CacheShared && len(users[c.dir]) > 0:
			use = strings.Join(users[c.dir], ",")
		case len(users[c.dir]) > 0:
			use = "stopped"
		case running == nil:
			use = "?"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.label, size, use, c.dir)
	}
	fmt.Fprintf(tw, "total\t%s\t\t\n", humanBytes(total))
	return tw.Flush()
}

func cacheClear(ctx context.Context, b backend.Backend, what string, running map[string]bool) error {
	var targets []cacheEntry
	switch what {
	case "all":
		targets = cacheEntries()
	case cage.CacheShared:
		targets = []cacheEntry{{cage.CacheShared, paths.CacheShared()}}
	default:
		name, err := cacheName(ctx, b, what)
		if err != nil {
			return err
		}
		targets = []cacheEntry{{name, paths.CacheProject(name)}}
	}
	var busy []string
	cleared := 0
	for _, t := range targets {
		if running[t.dir] {
			busy = append(busy, t.label)
			continue
		}
		if _, err := os.Stat(t.dir); err != nil {
			continue
		}
		n, _ := cage.DirSize(t.dir)
		if err := cage.RemoveCache(t.dir); err != nil {
			return fmt.Errorf("clear %s: %w", t.dir, err)
		}
		cleared++
		say("cleared %s (%s)", t.label, humanBytes(n))
	}
	if len(busy) > 0 {
		return fmt.Errorf("not cleared while a cage using it runs: %s (zoo stop it first)", strings.Join(busy, ", "))
	}
	if cleared == 0 {
		say("nothing to clear")
	}
	return nil
}

// cacheName resolves a cage name, worktree path or prefix to a project
// cache's name. The cage itself may be gone (zoo reset keeps the cache).
func cacheName(ctx context.Context, b backend.Backend, q string) (string, error) {
	if strings.ContainsAny(q, `/\`) || q == "." || q == ".." {
		if st, err := os.Stat(q); err == nil && st.IsDir() {
			root, err := cage.Root(q)
			if err != nil {
				return "", err
			}
			return cage.NameFor(root), nil
		}
		return "", fmt.Errorf("no cache %q (see zoo cache ls)", q)
	}
	q = strings.TrimPrefix(q, cage.Prefix)
	var hits []string
	for _, c := range cacheEntries() {
		if c.label == cage.CacheShared {
			continue
		}
		if c.label == q {
			return q, nil
		}
		if strings.HasPrefix(c.label, q) {
			hits = append(hits, c.label)
		}
	}
	if c, err := cage.Find(ctx, b, q); err == nil {
		return c.Name, nil
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", fmt.Errorf("%q matches %d caches; be more specific", q, len(hits))
	}
	return "", fmt.Errorf("no cache %q (see zoo cache ls)", q)
}

// sayCache reports a new cage's cache, and warns about a shared one.
func sayCache(pl cage.Plan) {
	switch pl.Cache {
	case cage.CacheProject:
		say("package cache: %s (kept across zoo reset; zoo cache clear %s)", pl.CacheDir, pl.Name)
	case cage.CacheShared:
		say("WARNING: %s uses the shared package cache %s. Any cage using it can plant packages that other cages then install and run; use `cache: project` for repos whose agents read untrusted input", pl.Name, pl.CacheDir)
	}
}

// noteCacheKept tells the user zoo reset left the package cache alone.
func noteCacheKept(name string) {
	dir := paths.CacheProject(name)
	if _, err := os.Stat(dir); err == nil {
		say("its package cache stays in %s (zoo cache clear %s to drop it)", dir, name)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
