package dockerapi_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/backend/dockerapi"
)

// TestRealEngine reads (never writes) from a real engine. Opt in with the
// engine's address, e.g. on Windows:
//
//	ZOO_TEST_DOCKER_HOST=npipe:////./pipe/docker_engine go test -run TestRealEngine ./internal/backend/dockerapi/
func TestRealEngine(t *testing.T) {
	host := os.Getenv("ZOO_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set ZOO_TEST_DOCKER_HOST to read from a real engine")
	}
	c, err := dockerapi.New(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping %s: %v", host, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("engine: %v %v", info["ServerVersion"], info["OperatingSystem"])
	cs, err := c.Containers(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d containers", len(cs))
	for _, x := range cs {
		if x.State != "running" {
			continue
		}
		sctx, stop := context.WithCancel(ctx)
		n := 0
		_ = c.Stats(sctx, x.ID, func(s dockerapi.StatsJSON) {
			n++
			cpu, ok := s.CPUPercent()
			t.Logf("%s: cpu=%.2f%% (%v) mem=%.0f pids=%d", x.Name(), cpu, ok, s.MemUsed(), s.PidsStats.Current)
			if n == 2 {
				stop()
			}
		})
		if n < 2 {
			t.Errorf("stats stream for %s gave %d samples", x.Name(), n)
		}
		break
	}
}
