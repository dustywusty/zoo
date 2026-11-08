package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/inject"
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
	if len(g.Services) == 0 && len(userCredentials(g)) == 0 {
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
	for _, n := range userCredentials(g) {
		c := g.Credentials[n]
		to := strings.Join(by[n], ", ")
		if c.AllCages {
			to = "every cage"
		} else if to == "" {
			to = "-"
		}
		var pol []string
		if c.ReadOnly || len(c.Push) > 0 {
			pol = append(pol, "read-only")
		}
		if len(c.Push) > 0 {
			pol = append(pol, "push "+strings.Join(c.Push, ","))
		}
		if len(c.Repos) > 0 {
			pol = append(pol, "repos "+strings.Join(c.Repos, ","))
		}
		as := strings.Join(c.Env, ",")
		if c.Git {
			as = strings.TrimPrefix(as+",git", ",")
		}
		target := strings.Join(c.Hosts, ",")
		if len(pol) > 0 {
			target += " (" + strings.Join(pol, "; ") + ")"
		}
		fmt.Fprintf(tw, "%s\tcredential\t%s\t%s\t%s\n", n, target, or(as, "-"), to)
	}
	return tw.Flush()
}

const serviceExample = `services:
  pg:     { host: 127.0.0.1:5432 }        # on this machine
  redis:  { image: redis:7, port: 6379 }  # a sidecar zoo runs
  gh-ssh: { remote: github.com:22 }       # non-HTTP egress
  keys:   { ssh-agent: bitwarden, keys: [github], hosts: [github.com] }
`

// cmdGrant handles `zoo grant [--for D] <cage> <service>` and `zoo revoke`.
func cmdGrant(verb string, args []string) error {
	args, forDur, err := takeFor(args)
	if err != nil {
		return err
	}
	if len(args) != 2 || (verb == "revoke" && forDur > 0) {
		return fmt.Errorf("usage: zoo grant [--for 4h] <cage> <service> | zoo revoke <cage> <service>")
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	_, isSvc := g.Services[args[1]]
	cred, isCred := g.Credentials[args[1]]
	if !isSvc && !isCred {
		return fmt.Errorf("no service or credential %q (zoo service ls)", args[1])
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
	var until time.Time
	if forDur > 0 {
		until = time.Now().Add(forDur)
	}
	if err := grantChange(ctx, e, c, args[1], verb == "grant", until); err != nil {
		return err
	}
	if verb == "grant" && isCred {
		say("%s holds %s for %s (new sessions get the placeholder; the keeper keeps the value)", c.Name, args[1], strings.Join(cred.Hosts, ", "))
		if !until.IsZero() {
			say("for %s; then it's revoked", config.Until(until))
		}
	} else if verb == "grant" {
		say("%s can reach %s as %s:%d", c.Name, args[1], services.HostName(args[1]), services.ListenPort(g.Services[args[1]]))
		if !until.IsZero() {
			say("for %s; then it's revoked", config.Until(until))
		}
	} else {
		say("revoked %s from %s", args[1], c.Name)
	}
	return nil
}

// grantChange records a grant or revoke and applies it to the keeper and
// the cage right away.
func grantChange(ctx context.Context, e env, c cage.Info, service string, on bool, until time.Time) error {
	gr := services.LoadGrants()
	if on {
		gr.Add(c.Name, service)
		if err := services.SetGrantExpiry(c.Name, service, until); err != nil {
			return err
		}
	} else {
		_ = services.SetGrantExpiry(c.Name, service, time.Time{})
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

// takeFor pulls `--for D` / `--for=D` out of args.
func takeFor(args []string) ([]string, time.Duration, error) {
	var out []string
	var d time.Duration
	for i := 0; i < len(args); i++ {
		a := args[i]
		v, ok := strings.CutPrefix(a, "--for=")
		if a == "--for" {
			if i+1 >= len(args) {
				return nil, 0, errors.New("--for needs a duration (30m, 4h, 2d)")
			}
			i++
			v, ok = args[i], true
		}
		if !ok {
			out = append(out, a)
			continue
		}
		pd, err := config.ParseFor(v)
		if err != nil {
			return nil, 0, err
		}
		d = pd
	}
	return out, d, nil
}

// userCredentials are credentials worth listing: not the dormant built-in
// model-key definitions.
func userCredentials(g config.Global) []string {
	var out []string
	for _, n := range inject.Names(g.Credentials) {
		c := g.Credentials[n]
		if c.AllCages && len(c.Env) == 0 && !c.Git {
			continue
		}
		out = append(out, n)
	}
	return out
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
