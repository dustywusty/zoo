// Package fakeengine is a tiny Docker Engine API for tests: enough of
// /_ping, /info, /containers, /networks, /events and /stats to exercise
// zoo's API client, served on a Unix socket (or a named pipe on Windows).
package fakeengine

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Container is a fake container.
type Container struct {
	ID      string
	Name    string
	State   string
	Labels  map[string]string
	Network map[string]string // network -> IP
}

// Engine is the fake. Mutate with Set; push events with Emit.
type Engine struct {
	Host     string
	Info     map[string]any
	Gateways map[string]string // network -> gateway

	mu         sync.Mutex
	containers map[string]Container
	events     chan any
	Requests   atomic.Int64
}

// Start serves a new fake engine until the test ends.
func Start(t *testing.T) *Engine {
	t.Helper()
	ln, host := listen(t)
	e := &Engine{Host: host, containers: map[string]Container{}, events: make(chan any, 16),
		Info:     map[string]any{"ServerVersion": "99.0", "OperatingSystem": "Fake Linux", "DefaultRuntime": "runc"},
		Gateways: map[string]string{}}
	srv := &http.Server{Handler: http.HandlerFunc(e.serve)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return e
}

// Set adds or replaces a container.
func (e *Engine) Set(c Container) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[c.Name] = c
}

// Emit sends an event to /events subscribers.
func (e *Engine) Emit(typ, action, name string) {
	e.events <- map[string]any{"Type": typ, "Action": action, "Actor": map[string]any{"ID": "x", "Attributes": map[string]string{"name": name}}}
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	e.Requests.Add(1)
	p := r.URL.Path
	switch {
	case p == "/_ping":
		fmt.Fprint(w, "OK")
	case p == "/info":
		json.NewEncoder(w).Encode(e.Info)
	case p == "/containers/json":
		var f map[string][]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &f)
		e.mu.Lock()
		var out []any
		for _, c := range e.containers {
			if len(f["name"]) > 0 && !strings.Contains(c.Name, f["name"][0]) {
				continue
			}
			out = append(out, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "State": c.State,
				"Labels": c.Labels, "NetworkSettings": map[string]any{"Networks": nets(c)}})
		}
		e.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		e.mu.Lock()
		c, ok := e.containers[id]
		e.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Name": "/" + c.Name,
			"State":  map[string]any{"Status": c.State, "Running": c.State == "running"},
			"Config": map[string]any{"Labels": c.Labels}, "NetworkSettings": map[string]any{"Networks": nets(c)}})
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stats"):
		fl, _ := w.(http.Flusher)
		for i := uint64(1); ; i++ {
			s := map[string]any{
				"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 2_000_000 * i}, "system_cpu_usage": 10_000_000 * i, "online_cpus": 4},
				"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 2_000_000 * (i - 1)}, "system_cpu_usage": 10_000_000 * (i - 1), "online_cpus": 4},
				"memory_stats": map[string]any{"usage": 300 << 20, "limit": 2 << 30, "stats": map[string]any{"inactive_file": 100 << 20}},
				"networks":     map[string]any{"eth0": map[string]any{"rx_bytes": 1000, "tx_bytes": 500}, "eth1": map[string]any{"rx_bytes": 24, "tx_bytes": 12}},
				"blkio_stats":  map[string]any{"io_service_bytes_recursive": []any{map[string]any{"op": "read", "value": 4096}, map[string]any{"op": "Write", "value": 8192}}},
				"pids_stats":   map[string]any{"current": 7},
			}
			if json.NewEncoder(w).Encode(s) != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	case strings.HasPrefix(p, "/networks/"):
		name := strings.TrimPrefix(p, "/networks/")
		gw, ok := e.Gateways[name]
		if !ok {
			http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Name": name, "IPAM": map[string]any{"Config": []any{map[string]any{"Gateway": gw}}}})
	case p == "/events":
		fl, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-e.events:
				if json.NewEncoder(w).Encode(ev) != nil {
					return
				}
				fl.Flush()
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func nets(c Container) map[string]any {
	m := map[string]any{}
	for n, ip := range c.Network {
		m[n] = map[string]any{"IPAddress": ip}
	}
	return m
}

var _ net.Listener // listen is per-OS
