package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/shims"
)

// Global flags, accepted anywhere before `--`.
var (
	flagBackend     string
	flagFloor       string
	flagAllowWeaker bool
)

// parseGlobal pulls the global flags out of argv and returns the rest.
func parseGlobal(argv []string) ([]string, error) {
	var out []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return append(out, argv[i:]...), nil
		}
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--backend", "--isolation-floor":
			if !hasVal {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = argv[i]
			}
			if name == "--backend" {
				flagBackend = val
			} else {
				if _, err := backend.ParseIsolation(val); err != nil {
					return nil, err
				}
				flagFloor = val
			}
		case "--allow-weaker-isolation":
			flagAllowWeaker = true
		default:
			out = append(out, a)
		}
	}
	return out, nil
}

// env is the resolved backend for this invocation.
type env struct {
	g     config.Global
	b     backend.Backend
	info  backend.Info
	floor backend.Isolation
}

func backendOptions(g config.Global) backend.Options {
	return backend.Options{
		Network:  g.Network,
		Bin:      g.Docker.Bin,
		Runtime:  g.Docker.Runtime,
		MaskFile: paths.MaskFile(),
		StageDir: paths.Stage(),
	}
}

// backendChoice returns the configured backend name and where it came from.
// Precedence: --backend, $ZOO_BACKEND, config, `zoo up --backend`/`zoo backend use`, auto.
func backendChoice(g config.Global) (string, string) {
	switch {
	case flagBackend != "" && flagBackend != "auto":
		return flagBackend, "--backend"
	case flagBackend == "auto":
		return "auto", "--backend"
	case os.Getenv("ZOO_BACKEND") != "":
		return os.Getenv("ZOO_BACKEND"), "$ZOO_BACKEND"
	case g.Backend != "" && g.Backend != "auto":
		return g.Backend, paths.Config()
	case keeper.LoadState().Backend != "":
		return keeper.LoadState().Backend, "saved by zoo up --backend / zoo backend use"
	}
	return "auto", "auto"
}

// floorChoice: --isolation-floor, $ZOO_ISOLATION_FLOOR, `zoo up --isolation-floor`, config.
func floorChoice(g config.Global) (backend.Isolation, error) {
	v := g.IsolationFloor
	if st := keeper.LoadState(); st.IsolationFloor != "" {
		v = st.IsolationFloor
	}
	if e := os.Getenv("ZOO_ISOLATION_FLOOR"); e != "" {
		v = e
	}
	if flagFloor != "" {
		v = flagFloor
	}
	if v == "" {
		v = string(backend.IsoMicroVM)
	}
	return backend.ParseIsolation(v)
}

func optionsFor(name string, g config.Global) backend.Options {
	o := backendOptions(g)
	if name != "docker" {
		o.Bin = "" // docker.bin is for the docker backend only
	}
	if name == "apple" {
		o.Runtime = ""
	}
	return o
}

func resolve(ctx context.Context, g config.Global) (env, error) {
	floor, err := floorChoice(g)
	if err != nil {
		return env{}, fmt.Errorf("isolation floor: %w", err)
	}
	name, _ := backendChoice(g)
	var b backend.Backend
	var info backend.Info
	if name == "auto" {
		b, info, err = autoBackend(ctx, g, floor)
		if err != nil {
			return env{}, err
		}
	} else {
		b, err = backend.New(name, optionsFor(name, g))
		if err != nil {
			return env{}, err
		}
		info = b.Detect(ctx)
	}
	return env{g: g, b: b, info: info, floor: floor}, nil
}

// autoBackend picks the strongest ready backend that meets the floor, else
// the strongest ready one (prepare() then explains the floor), else the
// strongest installed one (ensureSystem then says how to start it).
func autoBackend(ctx context.Context, g config.Global, floor backend.Isolation) (backend.Backend, backend.Info, error) {
	type cand struct {
		b    backend.Backend
		info backend.Info
	}
	var ready, installed []cand
	for _, n := range backend.Names() {
		b, _ := backend.New(n, optionsFor(n, g))
		info := b.Detect(ctx)
		switch {
		case info.Ready():
			ready = append(ready, cand{b, info})
		case info.Installed:
			installed = append(installed, cand{b, info})
		}
	}
	for _, c := range ready {
		if c.info.Caps.Isolation.AtLeast(floor) {
			return c.b, c.info, nil
		}
	}
	if len(ready) > 0 {
		return ready[0].b, ready[0].info, nil
	}
	if len(installed) > 0 {
		return installed[0].b, installed[0].info, nil
	}
	return nil, backend.Info{}, errors.New("no container runtime found: install Apple's `container` (macOS 26+, https://github.com/apple/container/releases), OrbStack, Docker, or Podman")
}

func detailSuffix(i backend.Info) string {
	var parts []string
	if i.Detail != "" {
		parts = append(parts, i.Detail)
	}
	if i.Version != "" {
		parts = append(parts, i.Version)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, " ")
}

func cmdBackend(args []string) error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if len(args) >= 1 && args[0] == "use" {
		if len(args) != 2 {
			return errors.New("usage: zoo backend use <apple|docker|podman|auto>")
		}
		name := args[1]
		if name != "auto" {
			if _, err := backend.New(name, optionsFor(name, g)); err != nil {
				return err
			}
		}
		st := keeper.LoadState()
		st.Backend = name
		if name == "auto" {
			st.Backend = ""
		}
		if err := keeper.SaveState(st); err != nil {
			return err
		}
		if g.Backend != "" && g.Backend != "auto" {
			say("note: %s sets backend: %s, which wins over this", paths.Config(), g.Backend)
		}
		say("backend: %s. Run `zoo down && zoo up` to move the keeper and dashboard; cages on the old backend stay there", name)
		return nil
	}
	if len(args) >= 1 && args[0] != "list" && args[0] != "ls" {
		return errors.New("usage: zoo backend [list|use <name>]")
	}
	floor, err := floorChoice(g)
	if err != nil {
		return err
	}
	active, from := backendChoice(g)
	if active == "auto" {
		if e, err := resolve(ctx, g); err == nil {
			active = e.b.Name()
			from = "auto"
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tBACKEND\tSTATUS\tISOLATION\tMEETS FLOOR\tDETAIL")
	for _, n := range backend.Names() {
		b, _ := backend.New(n, optionsFor(n, g))
		info := b.Detect(ctx)
		status := "ready"
		switch {
		case !info.Installed:
			status = "not installed"
		case !info.Running:
			status = "not running"
		}
		meets := "no"
		if info.Caps.Isolation.AtLeast(floor) {
			meets = "yes"
		}
		mark := ""
		if n == active {
			mark = "*"
		}
		detail := strings.TrimPrefix(detailSuffix(info), ", ")
		if info.Hint != "" {
			detail = strings.TrimSpace(detail + "  (" + info.Hint + ")")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", mark, n, status, info.Caps.Isolation, meets, detail)
	}
	tw.Flush()
	fmt.Printf("\nactive: %s (from %s) · isolation floor: %s\n", active, from, floor)
	return nil
}

func cmdStatus() error {
	g, err := loadCfg()
	if err != nil {
		return err
	}
	ctx := context.Background()
	e, err := resolve(ctx, g)
	if err != nil {
		return err
	}
	_, from := backendChoice(g)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	state := "ready"
	if !e.info.Ready() {
		state = "not ready: " + e.info.Hint
	}
	row("backend", fmt.Sprintf("%s (%s; from %s)%s", e.b.Name(), state, from, detailSuffix(e.info)))
	iso := string(e.info.Caps.Isolation)
	if !e.info.Caps.Isolation.AtLeast(e.floor) {
		iso += fmt.Sprintf("  BELOW FLOOR (%s): cages need --allow-weaker-isolation", e.floor)
	} else {
		iso += fmt.Sprintf("  (floor %s)", e.floor)
	}
	row("isolation", iso)
	if e.info.Ready() {
		if k, ok, _ := e.b.Get(ctx, keeper.Name); ok && k.Running {
			row("keeper", "running at "+keeper.ProxyURL(k.IP))
		} else if ok {
			row("keeper", k.State)
		} else {
			row("keeper", "not created (zoo up)")
		}
	}
	if daemonAlive(g) {
		row("dashboard", fmt.Sprintf("http://127.0.0.1:%d", g.Port))
	} else {
		row("dashboard", "not running (zoo up)")
	}
	if shims.OnPath() {
		row("shims", "on PATH")
	} else {
		row("shims", "not on PATH (zoo shims install)")
	}
	row("version", version)
	return tw.Flush()
}
