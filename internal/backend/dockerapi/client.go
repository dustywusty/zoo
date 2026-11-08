// Package dockerapi is a small, read-mostly client for the Docker Engine API
// over its local socket: a Unix socket on macOS/Linux, a named pipe on
// Windows. It exists so zood can watch cages without starting processes:
// polling the docker CLI meant several process launches every two seconds,
// which on Windows (where every console process also gets a console host)
// was enough to freeze a desktop. See decision D25.
//
// Stdlib HTTP, unversioned paths (the engine answers at its own default
// API version) and loosely read JSON, in the spirit of D5. Anything that
// changes state stays on the CLI.
package dockerapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned for a 404 (no such container or network).
var ErrNotFound = errors.New("not found")

// Client talks to one engine.
type Client struct {
	host string // as given: unix:///var/run/docker.sock, npipe:////./pipe/docker_engine
	http *http.Client
}

// New returns a client for a DOCKER_HOST-style address. Only local
// transports are accepted: unix:// and npipe://.
func New(host string) (*Client, error) {
	dial, err := dialerFor(host)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{host: host, http: &http.Client{Transport: tr}}, nil
}

// Host is the address the client was made for.
func (c *Client) Host() string { return c.host }

func dialerFor(host string) (func(ctx context.Context) (net.Conn, error), error) {
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		return func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}, nil
	case strings.HasPrefix(host, "npipe://"):
		// npipe:////./pipe/docker_engine -> \\.\pipe\docker_engine
		p := strings.ReplaceAll(strings.TrimPrefix(host, "npipe://"), "/", `\`)
		return func(ctx context.Context) (net.Conn, error) { return dialPipe(ctx, p) }, nil
	}
	return nil, fmt.Errorf("docker host %q: only unix:// and npipe:// are supported", host)
}

func (c *Client) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("docker api %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := c.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Ping checks the engine answers.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "/_ping", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Info is GET /info, read loosely.
func (c *Client) Info(ctx context.Context) (map[string]any, error) {
	var v map[string]any
	return v, c.getJSON(ctx, "/info", nil, &v)
}

// Container is one entry of GET /containers/json.
type Container struct {
	ID              string            `json:"Id"`
	Names           []string          `json:"Names"`
	State           string            `json:"State"`
	Labels          map[string]string `json:"Labels"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Name is the container's name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func filters(f map[string][]string) url.Values {
	b, _ := json.Marshal(f)
	return url.Values{"filters": {string(b)}}
}

// Containers lists containers (all states) whose name contains namePart.
func (c *Client) Containers(ctx context.Context, namePart string) ([]Container, error) {
	q := filters(map[string][]string{"name": {namePart}})
	q.Set("all", "1")
	var out []Container
	return out, c.getJSON(ctx, "/containers/json", q, &out)
}

// Inspect is GET /containers/{id}/json, decoded into v (the same shape
// `docker container inspect` prints). ErrNotFound if there's no such id.
func (c *Client) Inspect(ctx context.Context, id string, v any) error {
	return c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, v)
}

// Network is the part of GET /networks/{id} zoo reads.
type Network struct {
	Name string `json:"Name"`
	IPAM struct {
		Config []struct {
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// Network inspects one network by name or id.
func (c *Client) Network(ctx context.Context, name string) (Network, error) {
	var n Network
	return n, c.getJSON(ctx, "/networks/"+url.PathEscape(name), nil, &n)
}

// Event is one line of GET /events.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// Events streams container and network events until ctx ends or the
// connection drops; the error channel then gets one value (nil on ctx end).
func (c *Client) Events(ctx context.Context) (<-chan Event, <-chan error) {
	evs, errc := make(chan Event, 16), make(chan error, 1)
	go func() {
		defer close(evs)
		resp, err := c.do(ctx, "/events", filters(map[string][]string{"type": {"container", "network"}}))
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(bufio.NewReader(resp.Body))
		for {
			var e Event
			if err := dec.Decode(&e); err != nil {
				if ctx.Err() != nil {
					err = nil
				}
				errc <- err
				return
			}
			select {
			case evs <- e:
			case <-ctx.Done():
				errc <- nil
				return
			}
		}
	}()
	return evs, errc
}

// Stats streams GET /containers/{id}/stats (one sample a second) until ctx
// ends or the container stops.
func (c *Client) Stats(ctx context.Context, id string, each func(StatsJSON)) error {
	resp, err := c.do(ctx, "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"1"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var s StatsJSON
		if err := dec.Decode(&s); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		each(s)
	}
}
