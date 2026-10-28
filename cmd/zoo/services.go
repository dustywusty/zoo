package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/services"
)

func cmdService(args []string) error {
	if len(args) > 0 && args[0] != "ls" && args[0] != "list" {
		return errors.New("usage: zoo service ls   (define services under services: in " + paths.Config() + ")")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	if err := services.Validate(g.Services); err != nil {
		return err
	}
	if len(g.Services) == 0 {
		fmt.Printf("no services yet. Add them under services: in %s, e.g.\n\n%s", paths.Config(), serviceExample)
		return nil
	}
	ctx := context.Background()
	grants := map[string][]string{}
	if e, err := resolve(ctx, g); err == nil {
		if cs, err := cage.List(ctx, e.b); err == nil {
			grants = services.GrantsFor(g, cs)
		}
	}
	by := map[string][]string{}
	for c, gs := range grants {
		for _, s := range gs {
			by[s] = append(by[s], c)
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tKIND\tTARGET\tIN CAGES AS\tGRANTED TO")
	for _, n := range services.Names(g.Services) {
		s := g.Services[n]
		to := strings.Join(by[n], ", ")
		if to == "" {
			to = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s:%d\t%s\n", n, services.Kind(s), services.Target(s), services.HostName(n), services.ListenPort(s), to)
	}
	return tw.Flush()
}

const serviceExample = `services:
  pg:     { host: 127.0.0.1:5432 }        # on this machine
  redis:  { image: redis:7, port: 6379 }  # a sidecar zoo runs
  gh-ssh: { remote: github.com:22 }       # non-HTTP egress
  keys:   { ssh-agent: bitwarden, keys: [github], hosts: [github.com] }
`

// cmdGrant handles `zoo grant|revoke <cage> <service>`.
func cmdGrant(verb string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: zoo %s <cage> <service>", verb)
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	if _, ok := g.Services[args[1]]; !ok {
		return fmt.Errorf("no service %q (zoo service ls)", args[1])
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	c, err := cage.Find(ctx, e.b, args[0])
	if err != nil {
		return err
	}
	if err := grantChange(ctx, e, c, args[1], verb == "grant"); err != nil {
		return err
	}
	if verb == "grant" {
		say("%s can reach %s as %s:%d", c.Name, args[1], services.HostName(args[1]), services.ListenPort(g.Services[args[1]]))
	} else {
		say("revoked %s from %s", args[1], c.Name)
	}
	return nil
}

// grantChange records a grant or revoke and applies it to the keeper and
// the cage right away.
func grantChange(ctx context.Context, e env, c cage.Info, service string, on bool) error {
	gr := services.LoadGrants()
	if on {
		gr.Add(c.Name, service)
	} else {
		if !gr.Remove(c.Name, service) && repoGrants(c.Worktree, service) {
			return fmt.Errorf("%s gets %s from %s/zoo.yaml; remove it there", c.Name, service, c.Worktree)
		}
	}
	if err := gr.Save(); err != nil {
		return err
	}
	syncKeeper(ctx, e)
	if c.Running {
		if kip := keeperIPFor(ctx, e, c.ID); kip != "" {
			return applyServices(ctx, e, c.Name, c.ID, kip)
		}
	}
	return nil
}

func repoGrants(worktree, service string) bool {
	p, err := config.LoadProject(worktree)
	if err != nil {
		return false
	}
	for _, s := range p.Services {
		if s == service {
			return true
		}
	}
	return false
}
