// zoo-gate: the keeper's TCP gate for services (stdlib only; built into the
// keeper image). Listens on one port per service, accepts a connection only
// from a cage IP holding a grant, and forwards it to the service: directly
// for sidecars and remote destinations, through zood's host bridge for host
// ports and SSH agents. Config: /etc/zoo/gate.json, written by the host and
// re-read when it changes. Log: one JSON line per connection.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type service struct {
	Name   string            `json:"name"`
	Listen int               `json:"listen"`
	Kind   string            `json:"kind"`
	Target string            `json:"target"`
	Allow  map[string]string `json:"allow"`
}

type config struct {
	Token    string    `json:"token"`
	Bridge   string    `json:"bridge"`
	Services []service `json:"services"`
}

type entry struct {
	TS       string `json:"ts"`
	Client   string `json:"client"`
	Cage     string `json:"cage,omitempty"`
	Service  string `json:"service"`
	Target   string `json:"target"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
	BytesIn  int64  `json:"bytes_in"`  // to the cage
	BytesOut int64  `json:"bytes_out"` // from the cage
	MS       int64  `json:"ms"`
}

type gate struct {
	cfg       atomic.Pointer[config]
	mu        sync.Mutex
	listeners map[int]net.Listener
	logMu     sync.Mutex
	logw      io.Writer
}

func main() {
	confPath := flag.String("config", "/etc/zoo/gate.json", "gate config")
	logPath := flag.String("log", "/var/log/zoo/gate.log", "connection log")
	flag.Parse()

	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("zoo-gate: %v", err)
	}
	g := &gate{listeners: map[int]net.Listener{}, logw: f}
	g.cfg.Store(&config{})
	var lastMod time.Time
	var lastSize int64 = -1
	for {
		if st, err := os.Stat(*confPath); err == nil && (st.ModTime() != lastMod || st.Size() != lastSize) {
			lastMod, lastSize = st.ModTime(), st.Size()
			if err := g.load(*confPath); err != nil {
				log.Printf("zoo-gate: %v", err)
			}
		}
		time.Sleep(time.Second)
	}
}

func (g *gate) load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	g.apply(&c)
	return nil
}

// apply swaps the config in and opens/closes listeners to match. Allow
// lists are read per connection, so a revoke applies to the next one.
func (g *gate) apply(c *config) {
	g.cfg.Store(c)
	want := map[int]bool{}
	for _, s := range c.Services {
		want[s.Listen] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for port, ln := range g.listeners {
		if !want[port] {
			ln.Close()
			delete(g.listeners, port)
		}
	}
	for port := range want {
		if _, ok := g.listeners[port]; ok || port <= 0 {
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			log.Printf("zoo-gate: listen %d: %v", port, err)
			continue
		}
		g.listeners[port] = ln
		go g.serve(ln, port)
	}
}

func (g *gate) serviceFor(port int) (service, *config, bool) {
	c := g.cfg.Load()
	for _, s := range c.Services {
		if s.Listen == port {
			return s, c, true
		}
	}
	return service{}, c, false
}

func (g *gate) serve(ln net.Listener, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go g.handle(conn, port)
	}
}

func (g *gate) handle(conn net.Conn, port int) {
	defer conn.Close()
	start := time.Now()
	client, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	e := entry{TS: start.UTC().Format(time.RFC3339Nano), Client: client}
	s, c, ok := g.serviceFor(port)
	if !ok {
		e.Service, e.Verdict, e.Reason = strconv.Itoa(port), "denied", "no such service"
		g.write(e)
		return
	}
	e.Service, e.Target = s.Name, s.Target
	cage, granted := s.Allow[client]
	e.Cage = cage
	if !granted {
		e.Verdict, e.Reason = "denied", "no grant"
		g.write(e)
		return
	}
	up, err := g.dial(s, c, cage)
	if err != nil {
		e.Verdict, e.Reason, e.MS = "denied", err.Error(), time.Since(start).Milliseconds()
		g.write(e)
		return
	}
	defer up.Close()
	e.Verdict = "allowed"
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.BytesOut, _ = io.Copy(up, conn)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		e.BytesIn, _ = io.Copy(conn, up)
		closeWrite(conn)
	}()
	wg.Wait()
	e.MS = time.Since(start).Milliseconds()
	g.write(e)
}

func closeWrite(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
	}
}

// dial reaches the service. Host ports and SSH agents go through zood's
// bridge, which needs the token and is told which cage is asking.
func (g *gate) dial(s service, c *config, cage string) (net.Conn, error) {
	switch s.Kind {
	case "host", "ssh-agent":
		if c.Bridge == "" {
			return nil, fmt.Errorf("host bridge not configured (is the zoo daemon running?)")
		}
		conn, err := net.DialTimeout("tcp", c.Bridge, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("host bridge: %v", err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(conn, "ZOO1 %s %s %s\n", c.Token, s.Name, cage); err != nil {
			conn.Close()
			return nil, err
		}
		line, err := readLine(conn)
		if err != nil || line != "OK\n" {
			conn.Close()
			if err == nil {
				err = fmt.Errorf("%s", trim(line))
			}
			return nil, fmt.Errorf("host bridge refused: %v", err)
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	default:
		if s.Target == "" {
			return nil, fmt.Errorf("service %s has no address (is it running?)", s.Name)
		}
		return net.DialTimeout("tcp", s.Target, 10*time.Second)
	}
}

// readLine reads up to a newline one byte at a time: a buffered reader could
// swallow the first bytes of a server that speaks first (MySQL, SSH).
func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 512 {
		if _, err := io.ReadFull(c, one); err != nil {
			return string(b), err
		}
		b = append(b, one[0])
		if one[0] == '\n' {
			return string(b), nil
		}
	}
	return string(b), fmt.Errorf("line too long")
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func (g *gate) write(e entry) {
	b, _ := json.Marshal(e)
	g.logMu.Lock()
	defer g.logMu.Unlock()
	_, _ = g.logw.Write(append(b, '\n'))
}
