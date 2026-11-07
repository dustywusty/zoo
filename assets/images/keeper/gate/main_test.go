package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// A server that speaks first, like MySQL or SSH.
func greeter(t *testing.T, msg string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(msg))
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// A fake zood bridge that checks the handshake, then greets.
func bridge(t *testing.T, token string, seen chan string) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(io.LimitReader(c, 200)).ReadString('\n')
			seen <- line
			if !strings.HasPrefix(line, "ZOO1 "+token+" ") {
				c.Write([]byte("ERR bad\n"))
			} else {
				c.Write([]byte("OK\nHELLO-FROM-HOST"))
			}
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func dialRead(t *testing.T, port int) string {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	return string(b)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestGate(t *testing.T) {
	seen := make(chan string, 10)
	remotePort, hostPort, otherPort := freePort(t), freePort(t), freePort(t)
	var logs bytes.Buffer
	g := &gate{listeners: map[int]net.Listener{}, logw: &logs}
	g.apply(&config{
		Token:  "tok",
		Bridge: bridge(t, "tok", seen),
		Services: []service{
			{Name: "db", Listen: remotePort, Kind: "remote", Target: greeter(t, "GREETING"), Allow: map[string]string{"127.0.0.1": "api-1"}},
			{Name: "pg", Listen: hostPort, Kind: "host", Target: "pg", Allow: map[string]string{"127.0.0.1": "api-1"}},
			{Name: "other", Listen: otherPort, Kind: "remote", Target: greeter(t, "NOPE"), Allow: map[string]string{"10.0.0.9": "web-2"}},
		},
	})
	defer func() {
		for _, ln := range g.listeners {
			ln.Close()
		}
	}()
	if got := dialRead(t, remotePort); got != "GREETING" {
		t.Errorf("server-first protocol through the gate: %q", got)
	}
	if got := dialRead(t, hostPort); got != "HELLO-FROM-HOST" {
		t.Errorf("bridge: %q (bytes after OK must not be lost)", got)
	}
	if line := <-seen; line != "ZOO1 tok pg api-1\n" {
		t.Errorf("handshake %q", line)
	}
	if got := dialRead(t, otherPort); got != "" {
		t.Errorf("ungranted cage got through: %q", got)
	}
	// Revoke takes effect on the next connection.
	g.apply(&config{Token: "tok", Services: []service{{Name: "db", Listen: remotePort, Kind: "remote", Target: "x:1", Allow: map[string]string{}}}})
	if got := dialRead(t, remotePort); got != "" {
		t.Errorf("revoked grant still works: %q", got)
	}
	if _, ok := g.listeners[otherPort]; ok {
		t.Error("removed service still listening")
	}
	// Anti-spoofing: a granted source IP is only accepted on the keeper
	// address its own network uses.
	right, wrong := freePort(t), freePort(t)
	target := greeter(t, "VIA-OK")
	g.apply(&config{Token: "tok", Services: []service{
		{Name: "right", Listen: right, Kind: "remote", Target: target,
			Allow: map[string]string{"127.0.0.1": "api-1"}, Via: map[string]string{"127.0.0.1": "127.0.0.1"}},
		{Name: "wrong", Listen: wrong, Kind: "remote", Target: target,
			Allow: map[string]string{"127.0.0.1": "api-1"}, Via: map[string]string{"127.0.0.1": "10.9.9.9"}},
	}})
	if got := dialRead(t, right); got != "VIA-OK" {
		t.Errorf("connection on the expected keeper address refused: %q", got)
	}
	if got := dialRead(t, wrong); got != "" {
		t.Errorf("connection on the wrong keeper address got through: %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	log := logs.String()
	for _, want := range []string{`"verdict":"allowed"`, `"reason":"no grant"`, `"cage":"api-1"`, `"reason":"arrived on the wrong network"`, `"via":"127.0.0.1"`} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %s:\n%s", want, log)
		}
	}
}
