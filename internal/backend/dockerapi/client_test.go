package dockerapi_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/backend/dockerapi"
	"github.com/dusty/zoo/internal/backend/dockerapi/fakeengine"
)

func TestClientOverLocalSocket(t *testing.T) {
	e := fakeengine.Start(t)
	e.Set(fakeengine.Container{ID: "abc", Name: "zoo-api-1", State: "running", Labels: map[string]string{"zoo.cage": "api-1"}, Network: map[string]string{"zoo-cages-api-1": "172.30.0.3"}})
	e.Set(fakeengine.Container{ID: "def", Name: "unrelated", State: "running"})
	c, err := dockerapi.New(e.Host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping over %s: %v", e.Host, err)
	}
	cs, err := c.Containers(ctx, "zoo-")
	if err != nil || len(cs) != 1 || cs[0].Name() != "zoo-api-1" || cs[0].NetworkSettings.Networks["zoo-cages-api-1"].IPAddress != "172.30.0.3" {
		t.Fatalf("containers: %+v %v", cs, err)
	}
	var in struct{ Name string }
	if err := c.Inspect(ctx, "zoo-api-1", &in); err != nil || in.Name != "/zoo-api-1" {
		t.Fatalf("inspect: %+v %v", in, err)
	}
	if err := c.Inspect(ctx, "zoo-gone", &in); !errors.Is(err, dockerapi.ErrNotFound) {
		t.Fatalf("missing container: %v, want ErrNotFound", err)
	}

	// Stats stream: CPU% the way `docker stats` computes it.
	sctx, stop := context.WithCancel(ctx)
	var got []dockerapi.StatsJSON
	_ = c.Stats(sctx, "zoo-api-1", func(s dockerapi.StatsJSON) {
		got = append(got, s)
		if len(got) == 3 {
			stop()
		}
	})
	if len(got) != 3 {
		t.Fatalf("stats samples: %d", len(got))
	}
	s := got[2]
	// cpu delta 2e6 / system delta 1e7 * 4 cpus * 100 = 80%
	if v, ok := s.CPUPercent(); !ok || math.Abs(v-80) > 0.001 {
		t.Errorf("cpu = %v %v, want 80", v, ok)
	}
	if s.MemUsed() != 200<<20 {
		t.Errorf("mem = %v, want usage minus inactive_file", s.MemUsed())
	}
	if rx, tx := s.Net(); rx != 1024 || tx != 512 {
		t.Errorf("net = %v/%v", rx, tx)
	}
	if r, w := s.Blk(); r != 4096 || w != 8192 {
		t.Errorf("blk = %v/%v", r, w)
	}

	// Events stream.
	evs, _ := c.Events(ctx)
	e.Emit("container", "start", "zoo-api-1")
	select {
	case ev := <-evs:
		if ev.Type != "container" || ev.Action != "start" || ev.Actor.Attributes["name"] != "zoo-api-1" {
			t.Errorf("event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}

func TestCPUPercentNeedsTwoReadings(t *testing.T) {
	var s dockerapi.StatsJSON
	s.CPUStats.SystemUsage = 100
	if _, ok := s.CPUPercent(); ok {
		t.Error("first sample has no previous reading: no rate")
	}
}

func TestOnlyLocalHosts(t *testing.T) {
	for _, h := range []string{"tcp://10.0.0.1:2375", "ssh://me@box", ""} {
		if _, err := dockerapi.New(h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
}
