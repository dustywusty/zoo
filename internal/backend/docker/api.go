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
	"runtime"
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
		if os.Getenv("ZOO_DOCKER_API") == "off" || os.Getenv("ZOO_DOCKER_BIN") != "" {
			return
		}
		host := d.apiHost(ctx)
		if host == "" {
			return
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

// cpuWindow is the shortest interval a CPU rate is computed over.
const cpuWindow = 500 * time.Millisecond

func (d *Docker) streamStats(ctx context.Context, c *dockerapi.Client, id string) {
	var r cpuRate
	_ = c.Stats(ctx, id, func(s dockerapi.StatsJSON) {
		smp := toSample(id, s)
		if v, ok := r.add(s.CPUStats.CPUUsage.TotalUsage, time.Now()); ok {
			smp.CPUPercent, smp.Have["cpu"] = v, true
		}
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

// cpuRate turns a cumulative CPU-time counter into percent of one core
// (100% = one busy core, as `docker stats` shows), measured against wall
// time between our own readings, over windows of at least cpuWindow.
//
// Not the engine's own precpu/system_cpu_usage: podman's compatible API
// sends its first samples microseconds apart and fills system time
// differently, which gave 0% for a container pegging a core (seen against
// podman 5.8). Wall time means the same thing on every engine.
type cpuRate struct {
	total uint64
	at    time.Time
	last  float64
	have  bool
}

func (r *cpuRate) add(total uint64, now time.Time) (float64, bool) {
	switch {
	case r.at.IsZero() || total < r.total: // first reading, or the counter reset
		r.total, r.at = total, now
		return r.last, r.have
	case now.Sub(r.at) < cpuWindow:
		return r.last, r.have // too soon for a meaningful rate
	}
	r.last = float64(total-r.total) / float64(now.Sub(r.at).Nanoseconds()) * 100
	r.total, r.at, r.have = total, now, true
	return r.last, true
}

func toSample(id string, s dockerapi.StatsJSON) backend.Sample {
	smp := backend.Sample{ID: id, Have: map[string]bool{"mem": true, "net": true, "blk": true, "pids": true}}
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

// apiHost finds the engine's local API address, or "" to stay on the CLI.
//
// docker: DOCKER_HOST, else the current context's endpoint.
// podman: CONTAINER_HOST, else the API service socket. On Linux that's the
// user's podman.socket (systemctl --user enable --now podman.socket), used
// only if it exists; on macOS and Windows, the podman machine's forwarded
// socket or pipe. Podman serves the same compatible API docker does.
func (d *Docker) apiHost(ctx context.Context) string {
	switch d.name {
	case "docker":
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			return h
		}
		out, err := d.cli.Run(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	case "podman":
		if h := os.Getenv("CONTAINER_HOST"); h != "" {
			return h
		}
		if runtime.GOOS == "linux" {
			out, err := d.cli.Run(ctx, "info", "--format", "{{.Host.RemoteSocket.Path}} {{.Host.RemoteSocket.Exists}}")
			if err != nil {
				return ""
			}
			path, exists, _ := strings.Cut(strings.TrimSpace(out), " ")
			if exists != "true" {
				return ""
			}
			return localHost(path)
		}
		field := "{{.ConnectionInfo.PodmanSocket.Path}}"
		if runtime.GOOS == "windows" {
			field = "{{.ConnectionInfo.PodmanPipe.Path}}"
		}
		out, err := d.cli.Run(ctx, "machine", "inspect", "--format", field)
		if err != nil {
			return ""
		}
		return localHost(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	}
	return ""
}

// localHost turns a socket path or pipe name into a DOCKER_HOST-style
// address (paths podman prints may or may not carry a scheme).
func localHost(p string) string {
	switch {
	case p == "" || p == "<no value>":
		return ""
	case strings.Contains(p, "://"):
		return p
	case strings.HasPrefix(p, `\\.\pipe\`) || strings.HasPrefix(p, "//./pipe/"):
		return "npipe://" + strings.ReplaceAll(p, `\`, "/")
	}
	return "unix://" + p
}
