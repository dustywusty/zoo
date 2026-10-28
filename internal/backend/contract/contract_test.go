// Package contract runs one table of behaviors against every backend, using
// the fake CLIs in test/fakes/fakectr. A backend passes when it drives its
// CLI the way the real one expects (the fake rejects unknown commands and
// flags with exit 2) and enforces zoo's invariants: internal cage network
// (per cage where the runtime can), no capabilities, no DNS, a pids cap,
// read-only masks and git guard, sidecars only on the services network.
//
// The same table can later run against real runtimes; only the assertions
// that read the fake's recorded state (fakestate) are fake-specific.
package contract

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
	_ "github.com/dusty/zoo/internal/backend/apple"
	_ "github.com/dusty/zoo/internal/backend/docker"
	"github.com/dusty/zoo/test/fakes/fakestate"
)

const (
	cageNet = "zoo-cages"
	svcNet  = backend.ServicesNetwork
)

// target is one backend under test.
type target struct {
	backend string // registered name
	bin     string // name the fake is invoked as (selects its dialect)
	binEnv  string // if set, point the backend at the fake through this env var instead of Options.Bin
	iso     backend.Isolation
	perCage bool      // one internal network per cage
	bridge  [2]string // HostBridge (listen, dial) on the fake's default setup
}

var targets = []target{
	{backend: "apple", bin: "container", binEnv: "ZOO_CONTAINER_BIN", iso: backend.IsoVM,
		bridge: [2]string{"192.168.64.1", "192.168.64.1"}},
	{backend: "docker", bin: "docker", iso: backend.IsoContainer, perCage: true,
		bridge: [2]string{"127.0.0.1", "host.docker.internal"}},
	{backend: "podman", bin: "podman", iso: backend.IsoContainer, perCage: true,
		bridge: [2]string{"127.0.0.1", "host.containers.internal"}},
}

// harness is the per-backend fixture shared by the behaviors, in order.
type harness struct {
	tg       target
	ctx      context.Context
	b        backend.Backend
	opts     backend.Options
	bin      string // path to the fake under tg.bin
	state    string // ZOO_FAKE_STATE
	maskFile string
	repo     string
	keeper   backend.KeeperSpec
	cage     backend.CageSpec
	cage2    backend.CageSpec // a second cage, for network isolation
	sidecar  backend.SidecarSpec
	buildCtx string
}

// perCageNets are the per-cage networks in the fake (zoo-cages-<id>).
func perCageNets(st *fakestate.State) []string {
	var out []string
	for _, n := range st.Networks {
		if strings.HasPrefix(n.Name, cageNet+"-") {
			out = append(out, n.Name)
		}
	}
	return out
}

func (h *harness) load(t *testing.T) *fakestate.State {
	t.Helper()
	st, err := fakestate.Load(h.state)
	if err != nil {
		t.Fatalf("load fake state: %v", err)
	}
	return st
}

func (h *harness) container(t *testing.T, name string) *fakestate.Container {
	t.Helper()
	c := h.load(t).Find(name)
	if c == nil {
		t.Fatalf("fake has no container %s", name)
	}
	return c
}

// newBackend builds the backend pointed at bin (the fake, or a missing path).
func (h *harness) newBackend(t *testing.T, bin string) backend.Backend {
	t.Helper()
	o := h.opts
	if h.tg.binEnv != "" {
		t.Setenv(h.tg.binEnv, bin)
		o.Bin = ""
	} else {
		o.Bin = bin
	}
	b, err := backend.New(h.tg.backend, o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) exec(t *testing.T, s backend.ExecSpec) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	s.ID, s.NoStdin, s.Stdout, s.Stderr = h.cage.ID, true, &out, &errb
	code, err := h.b.Exec(h.ctx, s)
	if err != nil {
		t.Fatalf("Exec %v: %v (stderr %q)", s.Cmd, err, errb.String())
	}
	return code, out.String()
}

func findStatus(all []backend.Status, id string) (backend.Status, bool) {
	for _, s := range all {
		if s.ID == id {
			return s, true
		}
	}
	return backend.Status{}, false
}

func mountAt(c *fakestate.Container, target string) (int, *fakestate.Mount) {
	for i := range c.Mounts {
		if c.Mounts[i].Target == target {
			return i, &c.Mounts[i]
		}
	}
	return -1, nil
}

func hasFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

type behavior struct {
	name string
	run  func(t *testing.T, h *harness)
}

var behaviors = []behavior{
	{"Detect", func(t *testing.T, h *harness) {
		info := h.b.Detect(h.ctx)
		if !info.Installed || !info.Running || !info.Ready() {
			t.Fatalf("Detect = %+v, want installed and running", info)
		}
		if info.Caps.Isolation != h.tg.iso {
			t.Errorf("isolation = %q, want %q", info.Caps.Isolation, h.tg.iso)
		}
		if info.Version == "" {
			t.Error("no version")
		}
		if !info.Caps.InternalNetworks || !info.Caps.MultiNetworkAttach || !info.Caps.ReadOnlyMounts || !info.Caps.HostFileMounts || !info.Caps.PidsLimit {
			t.Errorf("caps = %+v, want internal networks, multi-network attach, ro mounts, host file mounts, pids limit", info.Caps)
		}
		if err := h.b.Prepare(h.ctx); err != nil {
			t.Errorf("Prepare on a running runtime: %v", err)
		}
	}},
	{"DetectDown", func(t *testing.T, h *harness) {
		t.Setenv("ZOO_FAKE_DOWN", "1")
		info := h.b.Detect(h.ctx)
		if !info.Installed || info.Running || info.Ready() {
			t.Errorf("Detect with runtime down = %+v, want installed, not running", info)
		}
		if info.Hint == "" {
			t.Error("no hint when not running")
		}
	}},
	{"DetectMissing", func(t *testing.T, h *harness) {
		b := h.newBackend(t, filepath.Join(t.TempDir(), "nope", h.tg.bin))
		if info := b.Detect(h.ctx); info.Installed || info.Running || info.Hint == "" {
			t.Errorf("Detect with no CLI = %+v, want not installed, with a hint", info)
		}
	}},
	{"DetectRuntime", func(t *testing.T, h *harness) {
		if h.tg.backend == "apple" {
			t.Skip("apple has no OCI runtime choice")
		}
		for rt, want := range map[string]backend.Isolation{
			"runc":                  backend.IsoContainer,
			"kata":                  backend.IsoMicroVM,
			"io.containerd.kata.v2": backend.IsoMicroVM,
			"runsc":                 backend.IsoSandboxed,
		} {
			t.Setenv("ZOO_FAKE_DOCKER_RUNTIME", rt)
			info := h.b.Detect(h.ctx)
			if info.Caps.Isolation != want {
				t.Errorf("runtime %s: isolation = %q, want %q", rt, info.Caps.Isolation, want)
			}
			if got := info.Caps.PerCageKernel; got != (want == backend.IsoMicroVM) {
				t.Errorf("runtime %s: PerCageKernel = %v", rt, got)
			}
		}
	}},
	{"Image", func(t *testing.T, h *harness) {
		for _, ref := range []string{h.cage.Image, h.keeper.Image} {
			ok, err := h.b.ImageExists(h.ctx, ref)
			if err != nil || ok {
				t.Fatalf("ImageExists(%s) before build = %v, %v; want false, nil", ref, ok, err)
			}
			if err := h.b.BuildImage(h.ctx, backend.ImageSpec{Tag: ref, Context: h.buildCtx, BuildArgs: map[string]string{"A": "1", "B": "2"}}); err != nil {
				t.Fatalf("BuildImage(%s): %v", ref, err)
			}
			if ok, err := h.b.ImageExists(h.ctx, ref); err != nil || !ok {
				t.Fatalf("ImageExists(%s) after build = %v, %v; want true, nil", ref, ok, err)
			}
		}
	}},
	{"EnsureNetwork", func(t *testing.T, h *harness) {
		for i := 0; i < 2; i++ {
			if err := h.b.EnsureNetwork(h.ctx); err != nil {
				t.Fatalf("EnsureNetwork #%d: %v", i+1, err)
			}
		}
		st := h.load(t)
		for _, name := range []string{cageNet, svcNet} {
			n := st.Net(name)
			if n == nil {
				t.Fatalf("no %s network created", name)
			}
			if !n.Internal {
				t.Errorf("%s is not internal", name)
			}
			// On a Linux host the bridge's host address is the host itself:
			// docker networks must not have one (CI found sshd reachable).
			if h.tg.backend == "docker" && n.Options["com.docker.network.bridge.inhibit_ipv4"] != "true" {
				t.Errorf("%s gives the host an address: options %v", name, n.Options)
			}
			creates := 0
			for _, c := range st.Calls {
				if len(c) > 2 && c[1] == "network" && c[2] == "create" && c[len(c)-1] == name {
					creates++
				}
			}
			if creates != 1 {
				t.Errorf("network create %s ran %d times, want 1 (EnsureNetwork must be idempotent)", name, creates)
			}
		}
	}},
	{"EnsureKeeper", func(t *testing.T, h *harness) {
		ip, err := h.b.EnsureKeeper(h.ctx, h.keeper)
		if err != nil {
			t.Fatalf("EnsureKeeper: %v", err)
		}
		st := h.load(t)
		k := st.Find(h.keeper.ID)
		if k == nil {
			t.Fatal("keeper not created")
		}
		if want := k.IPs[cageNet]; ip == "" || ip != want {
			t.Errorf("keeper IP = %q, want its address on %s (%q)", ip, cageNet, want)
		}
		egress := ""
		for _, n := range k.Networks {
			if net := st.Net(n); net != nil && !net.Internal {
				egress = n
			}
		}
		if !containsStr(k.Networks, cageNet) || !containsStr(k.Networks, svcNet) || egress == "" {
			t.Errorf("keeper networks = %v, want %s, %s and a network with a route out", k.Networks, cageNet, svcNet)
		}
		if k.IPs[svcNet] == "" {
			t.Errorf("keeper has no address on %s: %v", svcNet, k.IPs)
		}
		s, ok, err := h.b.Get(h.ctx, h.keeper.ID)
		if err != nil || !ok || !s.Running || s.Labels["zoo.role"] != "keeper" {
			t.Errorf("Get(keeper) = %+v, %v, %v", s, ok, err)
		}
		for n, ip := range k.IPs {
			if s.IPs[n] != ip {
				t.Errorf("Status.IPs = %v, want %v", s.IPs, k.IPs)
				break
			}
		}
		// Idempotent: a second call reuses the running keeper.
		ip2, err := h.b.EnsureKeeper(h.ctx, h.keeper)
		if err != nil || ip2 != ip {
			t.Errorf("second EnsureKeeper = %q, %v; want %q", ip2, err, ip)
		}
		if n := len(h.load(t).Containers); n != 1 {
			t.Errorf("%d containers after second EnsureKeeper, want 1", n)
		}
	}},
	{"HostBridge", func(t *testing.T, h *harness) {
		listen, dial, err := h.b.HostBridge(h.ctx)
		if err != nil || listen != h.tg.bridge[0] || dial != h.tg.bridge[1] {
			t.Errorf("HostBridge = %q, %q, %v; want %q, %q", listen, dial, err, h.tg.bridge[0], h.tg.bridge[1])
		}
		if h.tg.backend != "docker" {
			return
		}
		// Docker Engine on Linux: the keeper reaches the host at its egress
		// bridge's gateway.
		t.Setenv("ZOO_FAKE_DOCKER_OS", "Ubuntu 24.04.1 LTS")
		listen, dial, err = h.b.HostBridge(h.ctx)
		if err != nil || listen != "192.168.107.1" || dial != "192.168.107.1" {
			t.Errorf("HostBridge on Linux = %q, %q, %v; want the zoo-egress gateway 192.168.107.1 for both", listen, dial, err)
		}
	}},
	{"CageNetwork", func(t *testing.T, h *harness) {
		if got := h.b.Detect(h.ctx).Caps.PerCageNetworks; got != h.tg.perCage {
			t.Errorf("Caps.PerCageNetworks = %v, want %v", got, h.tg.perCage)
		}
		net, keeperIP, err := h.b.CageNetwork(h.ctx, h.cage.ID)
		if err != nil {
			t.Fatalf("CageNetwork: %v", err)
		}
		st := h.load(t)
		n := st.Net(net)
		if n == nil || !n.Internal {
			t.Fatalf("CageNetwork = %q; want an existing internal network (got %+v)", net, n)
		}
		if h.tg.backend == "docker" && n.Options["com.docker.network.bridge.inhibit_ipv4"] != "true" {
			t.Errorf("cage network %s gives the host an address: options %v", net, n.Options)
		}
		if h.tg.perCage {
			if net == cageNet || net == svcNet {
				t.Errorf("CageNetwork = %q, want the cage's own network", net)
			}
			if n.Labels["zoo.cage-net"] != h.cage.ID {
				t.Errorf("network %s labels = %v, want zoo.cage-net=%s", net, n.Labels, h.cage.ID)
			}
		} else if net != cageNet {
			t.Errorf("CageNetwork = %q, want the shared %s", net, cageNet)
		}
		k := st.Find(h.keeper.ID)
		if keeperIP == "" || k == nil || k.IPs[net] != keeperIP {
			t.Errorf("keeper IP on %s = %q, fake has %v", net, keeperIP, k.IPs)
		}
		// Idempotent.
		net2, ip2, err := h.b.CageNetwork(h.ctx, h.cage.ID)
		if err != nil || net2 != net || ip2 != keeperIP {
			t.Errorf("second CageNetwork = %q, %q, %v; want %q, %q", net2, ip2, err, net, keeperIP)
		}
		h.cage.Network = net
	}},
	{"CreateCage", func(t *testing.T, h *harness) {
		if err := h.b.CreateCage(h.ctx, h.cage); err != nil {
			t.Fatalf("CreateCage: %v", err)
		}
		all, err := h.b.List(h.ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		s, ok := findStatus(all, h.cage.ID)
		if !ok {
			t.Fatalf("List = %+v, missing %s", all, h.cage.ID)
		}
		if _, ok := findStatus(all, h.keeper.ID); !ok {
			t.Errorf("List = %+v, missing the keeper", all)
		}
		if !s.Running || s.IP == "" || s.IPs[h.cage.Network] != s.IP {
			t.Errorf("cage status = %+v, want running with an IP on %s", s, h.cage.Network)
		}
		for k, v := range h.cage.Labels {
			if s.Labels[k] != v {
				t.Errorf("label %s = %q, want %q", k, s.Labels[k], v)
			}
		}
		g, ok, err := h.b.Get(h.ctx, h.cage.ID)
		if err != nil || !ok || g.IP != s.IP || !g.Running {
			t.Errorf("Get = %+v, %v, %v; want %+v", g, ok, err, s)
		}
	}},
	{"CageHardening", func(t *testing.T, h *harness) {
		c := h.container(t, h.cage.ID)
		if len(c.Networks) != 1 || c.Networks[0] != h.cage.Network {
			t.Errorf("cage networks = %v, want only %s", c.Networks, h.cage.Network)
		}
		if !hasFold(c.CapDrop, "ALL") {
			t.Errorf("cap-drop = %v, want ALL", c.CapDrop)
		}
		if len(c.CapAdd) > 0 {
			t.Errorf("cage adds capabilities %v", c.CapAdd)
		}
		if !c.NoDNS && !containsStr(c.DNS, "127.0.0.1") {
			t.Errorf("no DNS block (apple --no-dns / docker --dns 127.0.0.1): noDns=%v dns=%v", c.NoDNS, c.DNS)
		}
		pids := c.PidsLimit == h.cage.Pids
		for _, u := range c.Ulimits {
			if strings.HasPrefix(u, "nproc=") {
				pids = pids || u == "nproc=256:256"
			}
		}
		if !pids {
			t.Errorf("no pids limit of %d: pidsLimit=%d ulimits=%v", h.cage.Pids, c.PidsLimit, c.Ulimits)
		}
		if c.Memory != 1<<30 || c.CPUs != float64(h.cage.CPUs) {
			t.Errorf("memory=%d cpus=%v, want %d and %d", c.Memory, c.CPUs, 1<<30, h.cage.CPUs)
		}
	}},
	{"CageMounts", func(t *testing.T, h *harness) {
		c := h.container(t, h.cage.ID)
		dump := func() string {
			var b strings.Builder
			for _, m := range c.Mounts {
				b.WriteString("\n  " + m.Via + " " + m.Source + " -> " + m.Target)
				if m.ReadOnly {
					b.WriteString(" (ro)")
				}
			}
			return b.String()
		}
		wi, w := mountAt(c, h.repo)
		if w == nil || w.Source != h.repo || w.ReadOnly {
			t.Fatalf("no rw worktree mount of %s:%s", h.repo, dump())
		}
		gitDir := filepath.Join(h.repo, ".git")
		if _, p := mountAt(c, gitDir); p == nil || p.Source != gitDir || p.ReadOnly {
			t.Errorf("no rw pin mount of %s:%s", gitDir, dump())
		}
		env := filepath.Join(h.repo, ".env")
		mi, m := mountAt(c, env)
		if m == nil || m.Source != h.maskFile || !m.ReadOnly {
			t.Errorf("mask %s isn't the mask file mounted read-only:%s", env, dump())
		} else if mi < wi {
			t.Errorf("mask mounted before the worktree it covers:%s", dump())
		}
		if _, r := mountAt(c, filepath.Join(h.repo, ".gitmodules")); r == nil || !r.ReadOnly {
			t.Errorf("read-only root file not mounted ro:%s", dump())
		}
		hooks := filepath.Join(gitDir, "hooks")
		if _, r := mountAt(c, hooks); r == nil || r.Source != hooks || !r.ReadOnly {
			t.Errorf("read-only dir %s not mounted ro:%s", hooks, dump())
		}
		cfg := filepath.Join(gitDir, "config")
		ri, r := mountAt(c, cfg)
		if r == nil || !r.ReadOnly {
			t.Fatalf("read-only file %s not mounted ro:%s", cfg, dump())
		}
		if ri < wi {
			t.Errorf("read-only file mounted before the worktree:%s", dump())
		}
		if r.Source != cfg {
			// A staged copy (apple) must have the same content and must not
			// live in the pinned dir.
			want, _ := os.ReadFile(cfg)
			got, err := os.ReadFile(r.Source)
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("staged copy %s of %s: %q, %v", r.Source, cfg, got, err)
			}
			if filepath.Dir(r.Source) == gitDir {
				t.Errorf("staged copy %s is inside the pinned dir", r.Source)
			}
		}
	}},
	{"SecondCage", func(t *testing.T, h *harness) {
		net2, keeperIP, err := h.b.CageNetwork(h.ctx, h.cage2.ID)
		if err != nil {
			t.Fatalf("CageNetwork(%s): %v", h.cage2.ID, err)
		}
		h.cage2.Network = net2
		if err := h.b.CreateCage(h.ctx, h.cage2); err != nil {
			t.Fatalf("CreateCage(%s): %v", h.cage2.ID, err)
		}
		st := h.load(t)
		c1, c2, k := st.Find(h.cage.ID), st.Find(h.cage2.ID), st.Find(h.keeper.ID)
		if c1 == nil || c2 == nil || k == nil {
			t.Fatal("missing cage or keeper in the fake")
		}
		if k.IPs[net2] != keeperIP || keeperIP == "" {
			t.Errorf("keeper on %s = %q, CageNetwork said %q", net2, k.IPs[net2], keeperIP)
		}
		shared := false
		for _, n := range c2.Networks {
			shared = shared || containsStr(c1.Networks, n)
		}
		if h.tg.perCage {
			if net2 == h.cage.Network {
				t.Errorf("both cages got network %s", net2)
			}
			if shared {
				t.Errorf("cages share a network: %v and %v", c1.Networks, c2.Networks)
			}
		} else if !shared || net2 != cageNet {
			t.Errorf("without per-cage networks both cages should be on %s: %v, %v", cageNet, c1.Networks, c2.Networks)
		}
		// Only the keeper bridges cage networks.
		for _, c := range st.Containers {
			if c == c1 || c == c2 || c == k {
				continue
			}
			for _, n := range c.Networks {
				if n == h.cage.Network || n == net2 {
					t.Errorf("%s is on cage network %s", c.Name, n)
				}
			}
		}
		s, ok, err := h.b.Get(h.ctx, h.cage2.ID)
		if err != nil || !ok || !s.Running || s.IP == "" || s.IP != c2.IPs[net2] {
			t.Errorf("Get(%s) = %+v, %v, %v", h.cage2.ID, s, ok, err)
		}
	}},
	{"KeeperRejoins", func(t *testing.T, h *harness) {
		// A replaced keeper must rejoin every cage's network.
		if err := h.b.Delete(h.ctx, h.keeper.ID); err != nil {
			t.Fatalf("Delete keeper: %v", err)
		}
		if _, err := h.b.EnsureKeeper(h.ctx, h.keeper); err != nil {
			t.Fatalf("EnsureKeeper: %v", err)
		}
		st := h.load(t)
		k := st.Find(h.keeper.ID)
		if k == nil {
			t.Fatal("no keeper")
		}
		for _, id := range []string{h.cage.ID, h.cage2.ID} {
			c := st.Find(id)
			if c == nil {
				t.Fatalf("Delete(keeper) removed %s", id)
			}
			for _, n := range c.Networks {
				if k.IPs[n] == "" {
					t.Errorf("new keeper isn't on %s's network %s (keeper: %v)", id, n, k.IPs)
				}
			}
		}
		if k.IPs[svcNet] == "" {
			t.Errorf("new keeper isn't on %s", svcNet)
		}
	}},
	{"Sidecar", func(t *testing.T, h *harness) {
		ip, err := h.b.EnsureSidecar(h.ctx, h.sidecar)
		if err != nil {
			t.Fatalf("EnsureSidecar: %v", err)
		}
		st := h.load(t)
		c := st.Find(h.sidecar.ID)
		if c == nil {
			t.Fatal("sidecar not created")
		}
		if len(c.Networks) != 1 || c.Networks[0] != svcNet {
			t.Errorf("sidecar networks = %v, want only %s", c.Networks, svcNet)
		}
		if ip == "" || ip != c.IPs[svcNet] {
			t.Errorf("EnsureSidecar = %q, want its address on %s (%v)", ip, svcNet, c.IPs)
		}
		if !c.NoDNS && !containsStr(c.DNS, "127.0.0.1") {
			t.Errorf("sidecar has working DNS: noDns=%v dns=%v", c.NoDNS, c.DNS)
		}
		if _, m := mountAt(c, "/data"); m == nil || m.Source != h.sidecar.Mounts[0].Source {
			t.Errorf("sidecar mounts = %+v", c.Mounts)
		}
		if !containsStr(c.Env, h.sidecar.Env[0]) || c.Labels["zoo.role"] != "sidecar" {
			t.Errorf("sidecar env %v labels %v", c.Env, c.Labels)
		}
		ip2, err := h.b.EnsureSidecar(h.ctx, h.sidecar)
		if err != nil || ip2 != ip {
			t.Errorf("second EnsureSidecar = %q, %v; want %q", ip2, err, ip)
		}
		if n := len(h.load(t).Containers); n != len(st.Containers) {
			t.Errorf("second EnsureSidecar created a container (%d -> %d)", len(st.Containers), n)
		}
	}},
	{"Exec", func(t *testing.T, h *harness) {
		if code, _ := h.exec(t, backend.ExecSpec{Cmd: []string{"sh", "-c", "exit 7"}}); code != 7 {
			t.Errorf("exit code = %d, want 7", code)
		}
		if code, _ := h.exec(t, backend.ExecSpec{Cmd: []string{"true"}}); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		envFile := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(envFile, []byte("# secrets\nZOO_SECRET=s3cret-value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out := h.exec(t, backend.ExecSpec{
			Cmd:     []string{"sh", "-c", `echo "$FOO|$ZOO_SECRET|$ZOO_FAKE_EXEC_USER|$ZOO_FAKE_EXEC_WORKDIR"`},
			Env:     []string{"FOO=bar baz"},
			EnvFile: envFile,
			User:    "agent",
			Workdir: h.repo,
		})
		want := "bar baz|s3cret-value|agent|" + h.repo
		if code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("exec = %d, %q; want 0, %q", code, out, want)
		}
		for _, call := range h.load(t).Calls {
			if strings.Contains(strings.Join(call, " "), "s3cret-value") {
				t.Errorf("env-file secret leaked onto argv: %v", call)
			}
		}
	}},
	{"Sample", func(t *testing.T, h *harness) {
		samples, err := h.b.Sample(h.ctx)
		if err != nil {
			t.Fatalf("Sample: %v", err)
		}
		var s *backend.Sample
		for i := range samples {
			if samples[i].ID == h.cage.ID {
				s = &samples[i]
			}
		}
		if s == nil {
			t.Fatalf("Sample = %+v, missing %s", samples, h.cage.ID)
		}
		if s.MemLimit != 1<<30 {
			t.Errorf("MemLimit = %v, want %d (spec %s)", s.MemLimit, 1<<30, h.cage.Memory)
		}
		if s.MemUsed <= 0 || s.Pids <= 0 || s.NetRx <= 0 || s.BlkRead <= 0 {
			t.Errorf("sample = %+v, want non-zero mem, pids, net, blk", *s)
		}
		for _, k := range []string{"mem", "net", "blk", "pids"} {
			if !s.Have[k] {
				t.Errorf("Have[%s] = false (%v)", k, s.Have)
			}
		}
		if !s.Have["cpu"] && !s.Have["cpu_time"] {
			t.Errorf("no cpu reading (%v)", s.Have)
		}
	}},
	{"Stop", func(t *testing.T, h *harness) {
		if err := h.b.Stop(h.ctx, h.cage.ID); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		s, ok, err := h.b.Get(h.ctx, h.cage.ID)
		if err != nil || !ok || s.Running || s.IP != "" {
			t.Errorf("Get after Stop = %+v, %v, %v; want present, not running, no IP", s, ok, err)
		}
		if _, err := h.b.Sample(h.ctx); err != nil {
			t.Errorf("Sample with a stopped cage: %v", err)
		}
	}},
	{"Start", func(t *testing.T, h *harness) {
		if err := h.b.Start(h.ctx, h.cage.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		s, ok, err := h.b.Get(h.ctx, h.cage.ID)
		if err != nil || !ok || !s.Running || s.IP == "" {
			t.Errorf("Get after Start = %+v, %v, %v; want running with an IP", s, ok, err)
		}
		if s.Labels["zoo.role"] != "cage" {
			t.Errorf("labels after restart = %v", s.Labels)
		}
	}},
	{"Delete", func(t *testing.T, h *harness) {
		if err := h.b.Delete(h.ctx, h.cage.ID); err != nil {
			t.Fatalf("Delete (running cage): %v", err)
		}
		all, err := h.b.List(h.ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if _, ok := findStatus(all, h.cage.ID); ok {
			t.Errorf("List after Delete still has %s", h.cage.ID)
		}
		if k, ok := findStatus(all, h.keeper.ID); !ok || !k.Running {
			t.Errorf("Delete removed or stopped the keeper: %+v", all)
		}
		if _, ok := findStatus(all, h.cage2.ID); !ok {
			t.Errorf("Delete removed the other cage: %+v", all)
		}
		st := h.load(t)
		k := st.Find(h.keeper.ID)
		if h.tg.perCage {
			if st.Net(h.cage.Network) != nil {
				t.Errorf("cage network %s left behind", h.cage.Network)
			}
			if k != nil && containsStr(k.Networks, h.cage.Network) {
				t.Errorf("keeper still attached to %s", h.cage.Network)
			}
		}
		if k == nil || k.IPs[h.cage2.Network] == "" || k.IPs[svcNet] == "" {
			t.Errorf("keeper lost its other networks: %+v", k)
		}
	}},
	{"GetMissing", func(t *testing.T, h *harness) {
		s, ok, err := h.b.Get(h.ctx, "zoo-does-not-exist")
		if ok || err != nil {
			t.Errorf("Get(missing) = %+v, %v, %v; want ok=false, err=nil", s, ok, err)
		}
	}},
	{"EmptyList", func(t *testing.T, h *harness) {
		for _, id := range []string{h.cage2.ID, h.sidecar.ID, h.keeper.ID} {
			if err := h.b.Delete(h.ctx, id); err != nil {
				t.Fatalf("Delete %s: %v", id, err)
			}
		}
		if left := perCageNets(h.load(t)); len(left) != 0 {
			t.Errorf("per-cage networks left behind: %v", left)
		}
		all, err := h.b.List(h.ctx)
		if err != nil || len(all) != 0 {
			t.Errorf("List with nothing = %+v, %v", all, err)
		}
		if s, err := h.b.Sample(h.ctx); err != nil || len(s) != 0 {
			t.Errorf("Sample with nothing = %+v, %v", s, err)
		}
	}},
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// buildFake compiles test/fakes/fakectr and links it under each CLI name.
func buildFake(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake CLIs need a unix shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakectr")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/dusty/zoo/test/fakes/fakectr")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v\n%s", err, out)
	}
	for _, tg := range targets {
		if err := os.Symlink(bin, filepath.Join(dir, tg.bin)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContract(t *testing.T) {
	fakes := buildFake(t)
	for _, tg := range targets {
		t.Run(tg.backend, func(t *testing.T) {
			tmp := t.TempDir()
			h := &harness{tg: tg, ctx: context.Background(), bin: filepath.Join(fakes, tg.bin), state: filepath.Join(tmp, "state.json")}
			t.Setenv("ZOO_FAKE_STATE", h.state)
			t.Setenv("ZOO_FAKE_DOWN", "")
			t.Setenv("ZOO_FAKE_DOCKER_RUNTIME", "")
			t.Setenv("ZOO_FAKE_DOCKER_OS", "")
			t.Setenv("ZOO_FAKE_DIALECT", "")
			t.Setenv("ZOO_CONTAINER_BIN", "")
			t.Setenv("ZOO_DOCKER_BIN", "")

			// The mask file lives alone in its own directory (apple shares a
			// file mount's parent directory with the VM).
			h.maskFile = filepath.Join(tmp, "mask", "empty")
			mkfile(t, h.maskFile, "")
			h.repo = filepath.Join(tmp, "repo")
			mkfile(t, filepath.Join(h.repo, "main.go"), "package main\n")
			mkfile(t, filepath.Join(h.repo, ".env"), "API_KEY=hunter2\n")
			mkfile(t, filepath.Join(h.repo, ".gitmodules"), "")
			mkfile(t, filepath.Join(h.repo, ".git", "config"), "[core]\n\tbare = false\n")
			mkfile(t, filepath.Join(h.repo, ".git", "hooks", "pre-commit.sample"), "#!/bin/sh\n")
			h.buildCtx = filepath.Join(tmp, "image")
			mkfile(t, filepath.Join(h.buildCtx, "Dockerfile"), "FROM scratch\n")
			etc, logs := filepath.Join(tmp, "etc"), filepath.Join(tmp, "log")
			mkfile(t, filepath.Join(etc, "squid.conf"), "")
			mkfile(t, filepath.Join(logs, ".keep"), "")

			h.opts = backend.Options{Network: cageNet, MaskFile: h.maskFile, StageDir: filepath.Join(tmp, "stage")}
			h.b = h.newBackend(t, h.bin)
			h.keeper = backend.KeeperSpec{ID: "zoo-keeper", Image: "zoo-keeper:latest", EtcDir: etc, LogDir: logs,
				CPUs: 1, Memory: "256M", Labels: map[string]string{"zoo": "1", "zoo.role": "keeper"}}
			h.cage = backend.CageSpec{
				ID: "zoo-contract-1", Image: "zoo-cage:latest", CPUs: 2, Memory: "1G", Pids: 256,
				Labels: map[string]string{"zoo": "1", "zoo.role": "cage", "zoo.worktree": h.repo, "zoo.size": "m"},
				Env:    []string{"ZOO_CAGE=zoo-contract-1"},
				Mounts: []backend.Mount{{Source: h.repo, Target: h.repo}},
				Masks:  []string{filepath.Join(h.repo, ".env"), filepath.Join(h.repo, ".env.missing")},
				ReadOnly: []string{
					filepath.Join(h.repo, ".git", "hooks"),
					filepath.Join(h.repo, ".git", "config"),
					// A file directly in the worktree root (a linked
					// worktree's .git file): must not cost the worktree mount.
					filepath.Join(h.repo, ".gitmodules"),
				},
				Pin: []string{filepath.Join(h.repo, ".git")},
			}
			h.cage2 = backend.CageSpec{
				ID: "zoo-contract-2", Image: "zoo-cage:latest", CPUs: 1, Memory: "512M", Pids: 128,
				Labels: map[string]string{"zoo": "1", "zoo.role": "cage"},
				Mounts: []backend.Mount{{Source: h.repo, Target: h.repo}},
			}
			data := filepath.Join(tmp, "services", "redis")
			mkfile(t, filepath.Join(data, ".keep"), "")
			h.sidecar = backend.SidecarSpec{
				ID: "zoo-svc-redis", Image: "redis:7",
				Env:    []string{"REDIS_ARGS=--appendonly yes"},
				Mounts: []backend.Mount{{Source: data, Target: "/data"}},
				Labels: map[string]string{"zoo": "1", "zoo.role": "sidecar", "zoo.service": "redis"},
			}
			for _, bh := range behaviors {
				t.Run(bh.name, func(t *testing.T) { bh.run(t, h) })
			}
		})
	}
}
