package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/backend"
)

// TestRealEngineBackend drives the podman backend's API path (or docker's,
// with ZOO_TEST_ENGINE=docker) against a real engine. Opt in with its
// socket, a running zoo-* container to look at (ZOO_TEST_BUSY=1 if it pegs
// a core: CPU must then read over 50%), and a command that stops it (to see
// the event). For podman, inside a podman container:
//
//	podman system service --time=0 unix:///tmp/podman.sock &
//	podman run --cgroups=enabled -d --name zoo-podtest alpine sh -c 'while :; do :; done'
//	ZOO_TEST_ENGINE_HOST=unix:///tmp/podman.sock ZOO_TEST_ENGINE_CAGE=zoo-podtest \
//	  ZOO_TEST_BUSY=1 ZOO_TEST_ENGINE_STOP="podman stop -t 1 zoo-podtest" ./docker.test -test.run TestRealEngineBackend -test.v
//
// For Docker Desktop on Windows: ZOO_TEST_ENGINE=docker
// ZOO_TEST_ENGINE_HOST=npipe:////./pipe/dockerDesktopLinuxEngine, same otherwise.
func TestRealEngineBackend(t *testing.T) {
	host, id := os.Getenv("ZOO_TEST_ENGINE_HOST"), os.Getenv("ZOO_TEST_ENGINE_CAGE")
	if host == "" || id == "" {
		t.Skip("set ZOO_TEST_ENGINE_HOST and ZOO_TEST_ENGINE_CAGE to test against a real engine")
	}
	engine := "podman"
	if os.Getenv("ZOO_TEST_ENGINE") == "docker" {
		engine = "docker"
		t.Setenv("DOCKER_HOST", host)
	} else {
		t.Setenv("CONTAINER_HOST", host)
	}
	t.Setenv("ZOO_DOCKER_BIN", "")
	t.Setenv("ZOO_DOCKER_API", "")
	d := New(engine, backend.Options{Bin: "zoo-test-no-such-cli"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info := d.Detect(ctx)
	t.Logf("detect: installed=%v running=%v version=%q detail=%q isolation=%s", info.Installed, info.Running, info.Version, info.Detail, info.Caps.Isolation)
	if !info.Running || info.Version == "" {
		t.Fatalf("detect via the engine api: %+v", info)
	}
	all, err := d.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range all {
		t.Logf("list: %s running=%v ips=%v", s.ID, s.Running, s.IPs)
		found = found || s.ID == id
	}
	if !found {
		t.Fatalf("%s not listed", id)
	}
	if st, ok, err := d.Get(ctx, id); !ok || err != nil || !st.Running {
		t.Fatalf("get %s: %+v %v %v", id, st, ok, err)
	}
	if _, ok, err := d.Get(ctx, "zoo-no-such-cage"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}

	var cpu bool
	for i := 0; i < 60 && !cpu; i++ {
		ss, err := d.Sample(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			if s.ID == id && s.Have["cpu"] {
				cpu = true
				t.Logf("stats: cpu=%.2f%% mem=%.0f/%.0f pids=%.0f net=%.0f/%.0f", s.CPUPercent, s.MemUsed, s.MemLimit, s.Pids, s.NetRx, s.NetTx)
				if os.Getenv("ZOO_TEST_BUSY") != "" && s.CPUPercent < 50 {
					t.Errorf("container is pegging a core but cpu = %.2f%%", s.CPUPercent)
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !cpu {
		t.Error("no stats sample with a CPU rate from the stream")
	}

	stop := os.Getenv("ZOO_TEST_ENGINE_STOP")
	if stop == "" {
		return
	}
	ch, err := d.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	f := strings.Fields(stop)
	if out, err := exec.Command(f[0], f[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v %s", stop, err, out)
	}
	select {
	case <-ch:
		t.Logf("event for the stop after %s", time.Since(start).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("no event from podman for the stop")
	}
	if st, _, _ := d.Get(ctx, id); st.Running {
		t.Error("still running after stop")
	}
}
