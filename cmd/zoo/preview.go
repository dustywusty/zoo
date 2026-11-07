package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/daemon"
)

// cmdPorts lists dev servers listening in a cage (default: this worktree's;
// outside one, every running cage) and their preview URLs.
func cmdPorts(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: zoo ports [cage]")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	var cages []cage.Info
	if c, err := currentOrNamed(ctx, e, args); err == nil {
		cages = []cage.Info{c}
	} else if len(args) > 0 {
		return err
	} else {
		all, err := cage.List(ctx, e.b)
		if err != nil {
			return err
		}
		for _, c := range all {
			if c.Running {
				cages = append(cages, c)
			}
		}
		if len(cages) == 0 {
			fmt.Println("no running cages.")
			return nil
		}
	}
	pp := daemon.PreviewPort(g)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CAGE\tPORT\tURL")
	found := false
	for _, c := range cages {
		if !c.Running {
			fmt.Fprintf(tw, "%s\t-\t(stopped)\n", c.Name)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		l, err := daemon.ListPorts(pctx, e.b, c.ID)
		cancel()
		if err != nil {
			fmt.Fprintf(tw, "%s\t-\t(%v)\n", c.Name, err)
			continue
		}
		ports := l.Ports()
		if len(ports) == 0 {
			fmt.Fprintf(tw, "%s\t-\t(nothing listening)\n", c.Name)
			continue
		}
		found = true
		for i, p := range ports {
			url := "-"
			if pp > 0 {
				url = daemon.PreviewURL(c.Name, p, pp)
				if i == 0 {
					url += "  (also " + daemon.PreviewURL(c.Name, 0, pp) + ")"
				}
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", c.Name, p, url)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	switch {
	case !found:
	case pp == 0:
		say("preview URLs are off (preview_port: 0 in ~/.zoo/config.yaml)")
	case !daemonAlive(g):
		say("the daemon isn't running, so these URLs won't answer yet: zoo up (or zoo open)")
	}
	return nil
}

// cmdOpen opens a cage's dev server in the browser: zoo open [cage] [port].
func cmdOpen(args []string) error {
	var port int
	if n := len(args); n > 0 {
		if p, err := strconv.Atoi(args[n-1]); err == nil && n <= 2 {
			if p < 1 || p > 65535 {
				return fmt.Errorf("port %d out of range", p)
			}
			port, args = p, args[:n-1]
		}
	}
	if len(args) > 1 {
		return errors.New("usage: zoo open [cage] [port]")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	pp := daemon.PreviewPort(g)
	if pp == 0 {
		return errors.New("preview URLs are off (preview_port: 0 in ~/.zoo/config.yaml)")
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	c, err := currentOrNamed(ctx, e, args)
	if err != nil {
		return err
	}
	if !c.Running {
		return fmt.Errorf("cage %s isn't running (zoo run or zoo shell starts it)", c.Name)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	l, err := daemon.ListPorts(pctx, e.b, c.ID)
	cancel()
	if err != nil {
		return err
	}
	ports := l.Ports()
	if len(ports) == 0 {
		return fmt.Errorf("nothing is listening in %s yet; start a dev server there (e.g. npm run dev)", c.Name)
	}
	if _, ok := l[port]; port != 0 && !ok {
		return fmt.Errorf("nothing is listening on port %d in %s (listening: %s)", port, c.Name, joinInts(ports))
	}
	if !daemonAlive(g) {
		if err := startDaemon(g); err != nil {
			return err
		}
	}
	url := daemon.PreviewURL(c.Name, port, pp)
	for _, opener := range openers() {
		if path, err := exec.LookPath(opener); err == nil && exec.Command(path, url).Run() == nil {
			say("opened %s", url)
			return nil
		}
	}
	fmt.Println(url)
	return nil
}

func openers() []string {
	if runtime.GOOS == "darwin" {
		return []string{"open"}
	}
	return []string{"xdg-open"}
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}
