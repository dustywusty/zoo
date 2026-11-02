package docker

// Reads through the Docker Engine API (decision D25). zood watches cages all
// day; doing that with the CLI meant launching docker several times every
// two seconds (ps, inspect per container, stats, info, network inspect),
// which on Windows froze the desktop. Through the API those are HTTP
// requests on one socket, cage changes arrive as events instead of being
// polled for, and stats arrive as a stream per cage.
//
// Only reads use the API. Creating, starting and exec-ing stay on the CLI
// (they're one-off, user-driven, and exec needs the terminal). Podman, or a
// docker whose socket can't be found, keep using the CLI for everything.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/backend/dockerapi"
)

// apiState is the lazily found engine client and what hangs off it.
type apiState struct {
	once   sync.Once
	client *dockerapi.Client

	statsMu sync.Mutex
	streams map[string]context.CancelFunc // running cage id -> its stats stream
	latest  map[string]backend.Sample

	bridgeMu   sync.Mutex
	bridge     [2]string
	bridgeTime time.Time
}

// api returns the engine client, or nil to use the CLI. Found once: from
// DOCKER_HOST, else the current docker context (one CLI call, at startup).
// Off for podman, for a substituted CLI (ZOO_DOCKER_BIN: the test fakes),
// and with ZOO_DOCKER_API=off.
func (d *Docker) api(ctx context.Context) *dockerapi.Client {
	d.a.once.Do(func() {
		if d.name != "docker" || os.Getenv("ZOO_DOCKER_API") == "off" || os.Getenv("ZOO_DOCKER_BIN") != "" {
			return
		}
		host := os.Getenv("DOCKER_HOST")
		if host == "" {
			out, err := d.cli.Run(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
			if err != nil {
				return
			}
			host = strings.TrimSpace(out)
		}
		c, err := dockerapi.New(host)
		if err != nil {
			return // tcp:// or ssh:// contexts: stay on the CLI
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if c.Ping(pctx) == nil {
			d.a.client = c
		}
	})
	return d.a.client
}

func fromContainer(c dockerapi.Container) inspected {
	var in inspected
	in.Name = c.Name()
	in.State.Status = c.State
	in.State.Running = c.State == "running"
	in.Config.Labels = c.Labels
	in.NetworkSettings.Networks = map[string]struct{ IPAddress string }{}
	for n, v := range c.NetworkSettings.Networks {
		in.NetworkSettings.Networks[n] = struct{ IPAddress string }{v.IPAddress}
	}
	return in
}

func (d *Docker) listAPI(ctx context.Context, c *dockerapi.Client) ([]backend.Status, error) {
	cs, err := c.Containers(ctx, "zoo-")
	if err != nil {
		return nil, err
	}
	var res []backend.Status
	for _, x := range cs {
		if st := d.toStatus(fromContainer(x)); backend.Prefixed(st.ID) {
			res = append(res, st)
		}
	}
	return res, nil
}

func (d *Docker) getAPI(ctx context.Context, c *dockerapi.Client, id string) (backend.Status, bool, error) {
	var in inspected
	err := c.Inspect(ctx, id, &in)
	if errors.Is(err, dockerapi.ErrNotFound) {
		return backend.Status{}, false, nil
	}
	if err != nil {
		return backend.Status{}, false, err
	}
	st := d.toStatus(in)
	return st, st.ID == id, nil
}

// sampleAPI keeps one stats stream per running zoo container and returns
// the latest reading of each. A new cage shows up from its second sample.
func (d *Docker) sampleAPI(ctx context.Context, c *dockerapi.Client) ([]backend.Sample, error) {
	all, err := d.listAPI(ctx, c)
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, s := range all {
		if s.Running {
			running[s.ID] = true
		}
	}
	a := &d.a
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.streams == nil {
		a.streams, a.latest = map[string]context.CancelFunc{}, map[string]backend.Sample{}
	}
	for id, stop := range a.streams {
		if !running[id] {
			stop()
			delete(a.streams, id)
			delete(a.latest, id)
		}
	}
	for id := range running {
		if _, ok := a.streams[id]; ok {
			continue
		}
		sctx, stop := context.WithCancel(context.Background())
		a.streams[id] = stop
		go d.streamStats(sctx, c, id)
	}
	out := make([]backend.Sample, 0, len(a.latest))
	for _, s := range a.latest {
		out = append(out, s)
	}
	return out, nil
}

func (d *Docker) streamStats(ctx context.Context, c *dockerapi.Client, id string) {
	_ = c.Stats(ctx, id, func(s dockerapi.StatsJSON) {
		smp := toSample(id, s)
		d.a.statsMu.Lock()
		if _, live := d.a.streams[id]; live {
			d.a.latest[id] = smp
		}
		d.a.statsMu.Unlock()
	})
	// Stream ended (container stopped, engine restarted): let the next
	// Sample start a fresh one if the container is still running.
	d.a.statsMu.Lock()
	if ctx.Err() == nil {
		delete(d.a.streams, id)
	}
	d.a.statsMu.Unlock()
}

func toSample(id string, s dockerapi.StatsJSON) backend.Sample {
	smp := backend.Sample{ID: id, Have: map[string]bool{"mem": true, "net": true, "blk": true, "pids": true}}
	if v, ok := s.CPUPercent(); ok {
		smp.CPUPercent, smp.Have["cpu"] = v, true
	}
	smp.MemUsed, smp.MemLimit = s.MemUsed(), float64(s.MemoryStats.Limit)
	smp.NetRx, smp.NetTx = s.Net()
	smp.BlkRead, smp.BlkWrite = s.Blk()
	smp.Pids = float64(s.PidsStats.Current)
	return smp
}

// hostBridgeAPI is HostBridge through the API, cached for a minute: the
// daemon asks on every refresh and the answer only changes with `zoo up`.
func (d *Docker) hostBridgeAPI(ctx context.Context, c *dockerapi.Client) (string, string, error) {
	a := &d.a
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	if time.Since(a.bridgeTime) < time.Minute {
		return a.bridge[0], a.bridge[1], nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		return "", "", err
	}
	osName, _ := info["OperatingSystem"].(string)
	listen, dial := "", ""
	if strings.Contains(osName, "OrbStack") || strings.Contains(osName, "Docker Desktop") {
		listen, dial = "127.0.0.1", "host.docker.internal"
	} else {
		n, err := c.Network(ctx, EgressNetwork)
		if err != nil {
			return "", "", err
		}
		for _, cfg := range n.IPAM.Config {
			if cfg.Gateway != "" {
				listen, dial = cfg.Gateway, cfg.Gateway
				break
			}
		}
		if listen == "" {
			return "", "", errors.New("no gateway on " + EgressNetwork)
		}
	}
	a.bridge, a.bridgeTime = [2]string{listen, dial}, time.Now()
	return listen, dial, nil
}

// Watch reports changes to zoo's containers and networks as they happen
// (backend.Watcher). The channel closes when the event stream ends; call
// Watch again to resume.
func (d *Docker) Watch(ctx context.Context) (<-chan struct{}, error) {
	c := d.api(ctx)
	if c == nil {
		return nil, errors.New("no docker api: poll instead")
	}
	evs, _ := c.Events(ctx)
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		for e := range evs {
			if relevant(e) {
				select {
				case out <- struct{}{}:
				default: // a refresh is already pending
				}
			}
		}
	}()
	return out, nil
}

// relevant: lifecycle and network changes of zoo's own objects. Execs
// (every agent session, every doctor run) change nothing zood shows.
func relevant(e dockerapi.Event) bool {
	if strings.HasPrefix(e.Action, "exec_") || e.Action == "top" || e.Action == "attach" || e.Action == "resize" {
		return false
	}
	name := e.Actor.Attributes["name"]
	switch e.Type {
	case "container":
		return strings.HasPrefix(name, "zoo-")
	case "network":
		return strings.HasPrefix(name, "zoo")
	}
	return false
}
