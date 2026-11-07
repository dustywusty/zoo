package docker

import (
	"context"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/backend/dockerapi/fakeengine"
)

// Everything zood does on its refresh loop goes through the engine API: the
// CLI here is a binary that doesn't exist, so any process launch would fail.
func TestDaemonReadsUseTheAPI(t *testing.T) {
	e := fakeengine.Start(t)
	e.Gateways[EgressNetwork] = "172.18.0.1"
	e.Set(fakeengine.Container{ID: "k", Name: "zoo-keeper", State: "running", Labels: map[string]string{"zoo.role": "keeper"},
		Network: map[string]string{"zoo-cages": "172.20.0.2", "zoo-egress": "172.18.0.2"}})
	e.Set(fakeengine.Container{ID: "c", Name: "zoo-api-1", State: "running", Labels: map[string]string{"zoo.cage": "api-1"},
		Network: map[string]string{"zoo-cages-api-1": "172.30.0.3"}})
	e.Set(fakeengine.Container{ID: "s", Name: "zoo-web-2", State: "exited"})
	t.Setenv("DOCKER_HOST", e.Host)
	t.Setenv("ZOO_DOCKER_BIN", "")
	t.Setenv("ZOO_DOCKER_API", "")
	d := New("docker", backend.Options{Bin: "zoo-test-no-such-docker-cli", Network: "zoo-cages"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if info := d.Detect(ctx); !info.Installed || !info.Running || info.Version != "99.0" || info.Caps.Isolation != backend.IsoContainer {
		t.Fatalf("detect via api: %+v", info)
	}
	all, err := d.List(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("list: %v %v", all, err)
	}
	st, ok, err := d.Get(ctx, "zoo-api-1")
	if err != nil || !ok || !st.Running || st.IP != "172.30.0.3" || st.Labels["zoo.cage"] != "api-1" {
		t.Fatalf("get: %+v %v %v", st, ok, err)
	}
	if _, ok, err := d.Get(ctx, "zoo-nope"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if listen, dial, err := d.HostBridge(ctx); err != nil || listen != "172.18.0.1" || dial != "172.18.0.1" {
		t.Fatalf("host bridge: %s %s %v", listen, dial, err)
	}

	// Stats: one stream per running zoo container, read from memory. A
	// stream's first sample has no CPU rate yet (it needs two readings), so
	// wait for one that has.
	var samples []backend.Sample
	withCPU := func() bool {
		n := 0
		for _, s := range samples {
			if s.Have["cpu"] {
				n++
			}
		}
		return n == 2
	}
	for i := 0; i < 100 && !withCPU(); i++ {
		samples, err = d.Sample(ctx)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(samples) != 2 {
		t.Fatalf("want samples for the 2 running containers, got %+v", samples)
	}
	for _, s := range samples {
		if !s.Have["cpu"] || s.CPUPercent <= 0 || s.MemUsed != 200<<20 || s.Pids != 7 {
			t.Errorf("sample %+v", s)
		}
	}

	// Watch: zoo changes wake the daemon; execs and other containers don't.
	ch, err := d.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.Emit("container", "exec_start", "zoo-api-1")
	e.Emit("container", "start", "postgres")
	select {
	case <-ch:
		t.Fatal("woke for an exec or a non-zoo container")
	case <-time.After(300 * time.Millisecond):
	}
	e.Emit("container", "die", "zoo-api-1")
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no wake for a zoo cage stopping")
	}
}

func TestLocalHost(t *testing.T) {
	for in, want := range map[string]string{
		"/run/user/1000/podman/podman.sock":       "unix:///run/user/1000/podman/podman.sock",
		"unix:///run/podman/podman.sock":          "unix:///run/podman/podman.sock",
		`\\.\pipe\podman-machine-default`:         "npipe:////./pipe/podman-machine-default",
		"npipe:////./pipe/podman-machine-default": "npipe:////./pipe/podman-machine-default",
		"<no value>": "",
		"":           "",
	} {
		if got := localHost(in); got != want {
			t.Errorf("localHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// Podman's API is found through CONTAINER_HOST (or its service socket) and
// used the same way as docker's.
func TestPodmanUsesItsAPI(t *testing.T) {
	e := fakeengine.Start(t)
	t.Setenv("CONTAINER_HOST", e.Host)
	t.Setenv("ZOO_DOCKER_BIN", "")
	t.Setenv("ZOO_DOCKER_API", "")
	e.Set(fakeengine.Container{ID: "c", Name: "zoo-api-1", State: "running"})
	d := New("podman", backend.Options{Bin: "zoo-test-no-such-podman-cli"})
	if d.api(context.Background()) == nil {
		t.Fatal("podman API not used")
	}
	if all, err := d.List(context.Background()); err != nil || len(all) != 1 {
		t.Fatalf("list via podman api: %v %v", all, err)
	}
	// Podman's host bridge is fixed (host.containers.internal), not asked of the API.
	if _, dial, _ := d.HostBridge(context.Background()); dial != "host.containers.internal" {
		t.Errorf("podman host bridge = %s", dial)
	}
}

// A substituted CLI (the contract tests' fake) or ZOO_DOCKER_API=off keeps
// everything on the CLI.
func TestAPIOff(t *testing.T) {
	e := fakeengine.Start(t)
	t.Setenv("DOCKER_HOST", e.Host)
	for name, env := range map[string][2]string{
		"fake cli": {"ZOO_DOCKER_BIN", "/fake/docker"},
		"opt out":  {"ZOO_DOCKER_API", "off"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if d := New("docker", backend.Options{}); d.api(context.Background()) != nil {
				t.Error("api used")
			}
		})
	}
	// DOCKER_HOST is docker's; podman reads CONTAINER_HOST. (Bin: a missing CLI,
	// so the podman info fallback finds nothing either.)
	t.Setenv("CONTAINER_HOST", "")
	if d := New("podman", backend.Options{Bin: "zoo-test-no-such-podman-cli"}); d.api(context.Background()) != nil {
		t.Error("podman used DOCKER_HOST")
	}
}

func TestCPURate(t *testing.T) {
	var r cpuRate
	t0 := time.Unix(1000, 0)
	if _, ok := r.add(5e9, t0); ok {
		t.Fatal("rate from one reading")
	}
	// Readings microseconds apart (podman's first samples) don't make a rate.
	if _, ok := r.add(5e9+1000, t0.Add(10*time.Microsecond)); ok {
		t.Fatal("rate from a burst")
	}
	// One busy core for a second: 100%.
	if v, ok := r.add(6e9, t0.Add(time.Second)); !ok || v < 99.9 || v > 100.1 {
		t.Fatalf("one core = %v %v", v, ok)
	}
	// Two cores' worth over the next second: 200%; in-between readings keep the last value.
	if v, ok := r.add(6.5e9, t0.Add(1200*time.Millisecond)); !ok || v < 99.9 || v > 100.1 {
		t.Fatalf("held = %v %v", v, ok)
	}
	if v, _ := r.add(8e9, t0.Add(2*time.Second)); v < 199.9 || v > 200.1 {
		t.Fatalf("two cores = %v", v)
	}
	// Counter reset (container restarted): no bogus negative rate.
	if v, _ := r.add(1e6, t0.Add(3*time.Second)); v < 0 {
		t.Fatalf("after reset = %v", v)
	}
}
