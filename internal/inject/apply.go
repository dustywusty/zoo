package inject

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
)

// Granted reports whether a cage holds a credential: explicitly (zoo grant,
// zoo.yaml) or because the credential is for every cage.
func Granted(c config.Credential, grants []string, name string) bool {
	if c.AllCages {
		return true
	}
	for _, g := range grants {
		if g == name {
			return true
		}
	}
	return false
}

// Activate works out which credentials the keeper handles from every cage's
// grants, writes the injector's files and squid.conf, and returns whether
// squid must reload.
func Activate(g config.Global, grants map[string][]string) (map[string]config.Credential, bool, error) {
	if err := Validate(g.Credentials, g.Services); err != nil {
		return nil, false, err
	}
	used := map[string]bool{}
	for _, gs := range grants {
		for _, n := range gs {
			used[n] = true
		}
	}
	for n, c := range g.Credentials {
		if c.AllCages && (len(c.Env) > 0 || c.Git) {
			used[n] = true
		}
	}
	active := Active(g, used)
	changed, err := Sync(active)
	if err != nil {
		return active, false, err
	}
	sq, err := keeper.WriteSquid(len(Hosts(active)) > 0)
	if err != nil {
		return active, false, err
	}
	// Hosts with credentials must be reachable at all.
	if hosts := Hosts(active); len(hosts) > 0 {
		if added, err := keeper.Allow(hosts...); err == nil && len(added) > 0 {
			changed = true
		}
	}
	return active, changed || sq, nil
}

// CageCredentials returns the active credentials a cage holds.
func CageCredentials(active map[string]config.Credential, grants []string) []string {
	var out []string
	for n, c := range active {
		if Granted(c, grants, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ApplyToCage makes a running cage trust the keeper's CA (when any host is
// intercepted) and points git at its credentials' placeholders. Runs as
// guest root; the agent can't change either.
func ApplyToCage(ctx context.Context, b backend.Backend, g config.Global, active map[string]config.Credential, cageID, cageName string, grants []string) error {
	ca := ""
	if len(Hosts(active)) > 0 {
		ca = strings.TrimSpace(CACert())
	}
	var gitHosts strings.Builder
	for _, n := range CageCredentials(active, grants) {
		c := active[n]
		if !c.Git {
			continue
		}
		ph, err := Placeholder(c, n, cageName)
		if err != nil {
			return err
		}
		for _, h := range c.Hosts {
			fmt.Fprintf(&gitHosts, "%s\t%s\n", h, ph)
		}
	}
	var out bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: cageID, User: "root", NoStdin: true,
		Cmd:    []string{"sh", "-c", TrustScript, "sh", ca, gitHosts.String()},
		Stdout: &out, Stderr: &out,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("set up credentials in %s: %s", cageID, strings.TrimSpace(out.String()))
	}
	return nil
}

// Env is what a session in a cage gets for its credentials: placeholders
// in each credential's env vars, plus CA settings when anything is
// intercepted. harnessEnv maps ENV -> "cred:<name>" references.
func Env(g config.Global, active map[string]config.Credential, cageName string, grants []string, harnessEnv map[string]string) ([]string, error) {
	var env []string
	if len(Hosts(active)) > 0 {
		env = append(env, SessionEnv()...)
	}
	for _, n := range CageCredentials(active, grants) {
		c := active[n]
		ph, err := Placeholder(c, n, cageName)
		if err != nil {
			return nil, err
		}
		for _, v := range c.Env {
			env = append(env, v+"="+ph)
		}
	}
	for k, v := range harnessEnv {
		n, ok := strings.CutPrefix(v, "cred:")
		if !ok {
			continue
		}
		c, ok := active[n]
		if !ok {
			return nil, fmt.Errorf("%s: no credential %q (define it under credentials:)", k, n)
		}
		if !Granted(c, grants, n) {
			return nil, fmt.Errorf("%s: credential %q isn't granted to this cage (zoo grant %s %s)", k, n, cageName, n)
		}
		ph, err := Placeholder(c, n, cageName)
		if err != nil {
			return nil, err
		}
		env = append(env, k+"="+ph)
	}
	return env, nil
}
