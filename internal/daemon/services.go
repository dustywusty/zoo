package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/services"
	"github.com/dusty/zoo/internal/sshagent"
)

// ConnEvent is one connection through the keeper's gate.
type ConnEvent struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	Cage     string    `json:"cage"`
	Service  string    `json:"service"`
	Target   string    `json:"target"`
	Verdict  string    `json:"verdict"`
	Reason   string    `json:"reason,omitempty"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
	MS       int64     `json:"ms"`
}

// SignEvent is one SSH agent operation from a cage.
type SignEvent struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"`
	Cage        string    `json:"cage"`
	Service     string    `json:"service"`
	Op          string    `json:"op"`
	Key         string    `json:"key"`
	Fingerprint string    `json:"fingerprint"`
	DestHost    string    `json:"dest_host"`
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason,omitempty"`
}

// Pending is a sign waiting for the human.
type Pending struct {
	ID          string    `json:"id"`
	Time        time.Time `json:"time"`
	Cage        string    `json:"cage"`
	Service     string    `json:"service"`
	Kind        string    `json:"kind"`
	Key         string    `json:"key"`
	Fingerprint string    `json:"fingerprint"`
	DestHost    string    `json:"dest_host"`
	Expires     time.Time `json:"expires"`
	Resolved    bool      `json:"resolved,omitempty"`

	answer chan bool
}

// ServiceView is a service as the dashboard shows it.
type ServiceView struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Port   int    `json:"port"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type svcState struct {
	mu       sync.Mutex
	conns    []ConnEvent
	signs    []SignEvent
	pending  map[string]*Pending
	views    []ServiceView
	grants   map[string][]string
	agents   map[string]*sshagent.Proxy // by service name
	agentErr map[string]string
}

func newSvcState() *svcState {
	return &svcState{pending: map[string]*Pending{}, grants: map[string][]string{}, agents: map[string]*sshagent.Proxy{}, agentErr: map[string]string{}}
}

// --- sync ---------------------------------------------------------------------

// syncServices reloads services config (so edits apply without a restart),
// starts granted sidecars, rewrites the gate config and refreshes the views.
func (s *Server) syncServices(cs []cage.Info) {
	if g, err := config.LoadGlobal(); err == nil {
		s.mu.Lock()
		s.cfg.Services = g.Services
		s.mu.Unlock()
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	grants, err := services.Sync(context.Background(), s.backend, cfg, cs)
	if err != nil {
		log.Printf("services: %v", err)
	}
	var views []ServiceView
	for _, n := range services.Names(cfg.Services) {
		v := ServiceView{Name: n, Kind: services.Kind(cfg.Services[n]), Target: services.Target(cfg.Services[n]), Port: services.ListenPort(cfg.Services[n])}
		v.Status, v.Note = s.serviceStatus(n, cfg.Services[n])
		views = append(views, v)
	}
	s.svc.mu.Lock()
	s.svc.views = views
	if grants != nil {
		s.svc.grants = grants
	}
	s.svc.mu.Unlock()
}

func (s *Server) serviceStatus(name string, sv config.Service) (string, string) {
	switch services.Kind(sv) {
	case services.Host:
		c, err := net.DialTimeout("tcp", sv.Host, 300*time.Millisecond)
		if err != nil {
			return "down", "nothing listening on " + sv.Host
		}
		c.Close()
		return "ready", ""
	case services.Sidecar:
		st, ok, _ := s.backend.Get(context.Background(), services.SidecarPrefix+name)
		if !ok {
			return "stopped", "starts when a cage with a grant runs"
		}
		if !st.Running {
			return "stopped", st.State
		}
		return "ready", ""
	case services.SSHAgent:
		if _, err := sshagent.Resolve(sv.SSHAgent); err != nil {
			return "down", err.Error()
		}
		return "ready", ""
	}
	return "ready", ""
}

// --- host bridge ------------------------------------------------------------------

// runBridge serves the keeper's gate: host ports and SSH agents. It listens
// where the keeper can reach and cages can't, and every connection must
// present the gate token.
func (s *Server) runBridge(ctx context.Context) {
	host, _, err := s.backend.HostBridge(ctx)
	if err != nil {
		log.Printf("host bridge: %v (host services and ssh agents unavailable)", err)
		return
	}
	tok, err := services.Token()
	if err != nil {
		log.Printf("host bridge: %v", err)
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(services.BridgePort(s.cfg)))
	var ln net.Listener
	for i := 0; i < 30; i++ { // the backend's host address may appear after start
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		log.Printf("host bridge: listen %s: %v", addr, err)
		return
	}
	log.Printf("host bridge on %s", addr)
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.bridgeConn(ctx, conn, tok)
	}
}

func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 512 {
		if _, err := io.ReadFull(c, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", fmt.Errorf("line too long")
}

func (s *Server) bridgeConn(ctx context.Context, conn net.Conn, tok string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(conn)
	if err != nil {
		return
	}
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != "ZOO1" || subtle.ConstantTimeCompare([]byte(f[1]), []byte(tok)) != 1 {
		io.WriteString(conn, "ERR bad handshake\n")
		return
	}
	name, cageName := f[2], f[3]
	s.mu.Lock()
	sv, ok := s.cfg.Services[name]
	s.mu.Unlock()
	if !ok {
		io.WriteString(conn, "ERR no such service\n")
		return
	}
	// The gate already checked the grant; check again in case it lags a revoke.
	if !s.granted(cageName, name) {
		io.WriteString(conn, "ERR no grant\n")
		return
	}
	_ = conn.SetDeadline(time.Time{})
	switch services.Kind(sv) {
	case services.Host:
		up, err := net.DialTimeout("tcp", sv.Host, 5*time.Second)
		if err != nil {
			io.WriteString(conn, "ERR "+err.Error()+"\n")
			return
		}
		defer up.Close()
		io.WriteString(conn, "OK\n")
		splice(conn, up)
	case services.SSHAgent:
		p, err := s.agentProxy(name, sv)
		if err != nil {
			io.WriteString(conn, "ERR "+err.Error()+"\n")
			return
		}
		io.WriteString(conn, "OK\n")
		_ = p.Serve(ctx, conn, cageName, sshagent.Policy{Keys: sv.Keys, Hosts: sv.Hosts, Approve: sv.Approve})
	default:
		io.WriteString(conn, "ERR not a bridge service\n")
	}
}

func (s *Server) granted(cageName, service string) bool {
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	for _, g := range s.svc.grants[cageName] {
		if g == service {
			return true
		}
	}
	return false
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if t, ok := dst.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

func (s *Server) agentProxy(name string, sv config.Service) (*sshagent.Proxy, error) {
	sock, err := sshagent.Resolve(sv.SSHAgent)
	if err != nil {
		return nil, err
	}
	s.svc.mu.Lock()
	defer s.svc.mu.Unlock()
	if p, ok := s.svc.agents[name]; ok && p.Upstream == sock {
		return p, nil
	}
	known := []sshagent.HostKeys{sshagent.GitHubHostKeys()}
	home, _ := os.UserHomeDir()
	if kh, err := sshagent.KnownHostsFile(filepath.Join(home, ".ssh", "known_hosts")); err == nil {
		known = append(known, kh)
	}
	p := &sshagent.Proxy{
		Upstream: sock,
		Known:    sshagent.Multi(known...),
		Log:      func(e sshagent.Event) { s.recordSign(name, e) },
		Approve:  func(ctx context.Context, e sshagent.Event) bool { return s.askApproval(ctx, name, e) },
	}
	s.svc.agents[name] = p
	return p, nil
}

func (s *Server) recordSign(service string, e sshagent.Event) {
	s.mu.Lock()
	s.nextID++
	ev := SignEvent{ID: s.nextID, Time: e.Time.UTC(), Cage: e.Cage, Service: service, Op: e.Op, Key: e.Key,
		Fingerprint: e.Fingerprint, DestHost: e.DestHost, Verdict: e.Verdict, Reason: e.Reason}
	var alert *Alert
	if e.Op == "sign" && e.Verdict == "denied" && s.once("sign|"+e.Cage+"|"+e.Fingerprint+"|"+e.DestHost, 10*time.Minute) {
		a := s.newAlert("warn", e.Cage, "Refused SSH signature", fmt.Sprintf("%s asked to use key %q for %s: %s.", e.Cage, e.Key, or(e.DestHost, "an unknown host"), e.Reason))
		alert = &a
	}
	s.mu.Unlock()
	if e.Op == "list" {
		return // routine; keep the feed for signs
	}
	s.svc.mu.Lock()
	s.svc.signs = append(s.svc.signs, ev)
	if len(s.svc.signs) > 1000 {
		s.svc.signs = s.svc.signs[len(s.svc.signs)-1000:]
	}
	s.svc.mu.Unlock()
	s.publish("sign", ev)
	if alert != nil {
		s.publish("alert", *alert)
		go s.notify(*alert)
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// askApproval shows a pending sign on the dashboard (and ntfy) and waits.
func (s *Server) askApproval(ctx context.Context, service string, e sshagent.Event) bool {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(sshagent.DefaultApproveTimeout)
	}
	p := &Pending{ID: hex.EncodeToString(buf), Time: time.Now().UTC(), Cage: e.Cage, Service: service, Kind: "ssh-sign",
		Key: e.Key, Fingerprint: e.Fingerprint, DestHost: e.DestHost, Expires: deadline.UTC(), answer: make(chan bool, 1)}
	s.svc.mu.Lock()
	s.svc.pending[p.ID] = p
	s.svc.mu.Unlock()
	s.publish("pending", p)
	go s.notify(Alert{Level: "warn", Cage: e.Cage, Title: "Approve SSH key use?",
		Body: fmt.Sprintf("%s wants to use key %q for %s. Open the zoo dashboard to allow.", e.Cage, e.Key, or(e.DestHost, "an unknown host"))})
	var allow bool
	select {
	case allow = <-p.answer:
	case <-ctx.Done():
	}
	s.svc.mu.Lock()
	delete(s.svc.pending, p.ID)
	s.svc.mu.Unlock()
	s.publish("pending", map[string]any{"id": p.ID, "resolved": true})
	return allow
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow bool `json:"allow"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "need {\"allow\": true|false}", http.StatusBadRequest)
		return
	}
	s.svc.mu.Lock()
	p, ok := s.svc.pending[r.PathValue("id")]
	s.svc.mu.Unlock()
	if !ok {
		http.Error(w, "no such request (it may have expired)", http.StatusNotFound)
		return
	}
	select {
	case p.answer <- body.Allow:
	default:
	}
	writeJSON(w, map[string]any{"id": p.ID, "allow": body.Allow})
}

// --- grants API -------------------------------------------------------------------

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cage    string `json:"cage"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Cage == "" || body.Service == "" {
		http.Error(w, "need {\"cage\": ..., \"service\": ...}", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, known := s.cfg.Services[body.Service]
	var c *cage.Info
	for i := range s.cages {
		if s.cages[i].Name == body.Cage {
			cc := s.cages[i]
			c = &cc
		}
	}
	s.mu.Unlock()
	if !known || c == nil {
		http.Error(w, "unknown cage or service", http.StatusNotFound)
		return
	}
	gr := services.LoadGrants()
	if r.Method == http.MethodPost {
		gr.Add(c.Name, body.Service)
	} else if !gr.Remove(c.Name, body.Service) {
		http.Error(w, body.Cage+" gets "+body.Service+" from its zoo.yaml; remove it there", http.StatusConflict)
		return
	}
	if err := gr.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.pollCages() // syncs the gate
	if c.Running {
		if err := s.applyServices(r.Context(), *c); err != nil {
			http.Error(w, "saved, but updating the cage failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	go s.broadcastState()
	writeJSON(w, map[string]any{"cage": c.Name, "service": body.Service, "granted": r.Method == http.MethodPost})
}

// applyServices writes a cage's service names into it.
func (s *Server) applyServices(ctx context.Context, c cage.Info) error {
	cs, k, kok := s.backend, keeper.Name, false
	cst, ok, _ := cs.Get(ctx, c.ID)
	kst, kok, _ := cs.Get(ctx, k)
	if !ok || !kok {
		return fmt.Errorf("cage or keeper not running")
	}
	kip := ""
	for n := range cst.IPs {
		if ip := kst.IPs[n]; ip != "" {
			kip = ip
		}
	}
	s.svc.mu.Lock()
	grants := append([]string(nil), s.svc.grants[c.Name]...)
	s.svc.mu.Unlock()
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	return services.Apply(ctx, s.backend, cfg, c.ID, kip, grants)
}

// --- gate log -----------------------------------------------------------------------

type gateEntry struct {
	TS       string `json:"ts"`
	Client   string `json:"client"`
	Cage     string `json:"cage"`
	Service  string `json:"service"`
	Target   string `json:"target"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	MS       int64  `json:"ms"`
}

func (s *Server) tailGate(ctx context.Context) {
	var off int64
	if st, err := os.Stat(paths.GateLog()); err == nil {
		off = st.Size()
	}
	var partial []byte
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f, err := os.Open(paths.GateLog())
		if err != nil {
			continue
		}
		st, _ := f.Stat()
		if st != nil && st.Size() < off {
			off, partial = 0, nil
		}
		if st != nil && st.Size() > off {
			if _, err := f.Seek(off, io.SeekStart); err == nil {
				buf, _ := io.ReadAll(io.LimitReader(f, st.Size()-off))
				off += int64(len(buf))
				buf = append(partial, buf...)
				lines := bytes.Split(buf, []byte("\n"))
				partial = append([]byte(nil), lines[len(lines)-1]...)
				for _, l := range lines[:len(lines)-1] {
					var e gateEntry
					if json.Unmarshal(l, &e) == nil {
						s.ingestConn(e)
					}
				}
			}
		}
		f.Close()
	}
}

func (s *Server) ingestConn(e gateEntry) {
	ts, _ := time.Parse(time.RFC3339Nano, e.TS)
	s.mu.Lock()
	s.nextID++
	cg := e.Cage
	if cg == "" {
		cg = s.cageForIP(e.Client)
	}
	ev := ConnEvent{ID: s.nextID, Time: ts, Cage: cg, Service: e.Service, Target: e.Target, Verdict: e.Verdict,
		Reason: e.Reason, BytesIn: e.BytesIn, BytesOut: e.BytesOut, MS: e.MS}
	var alert *Alert
	if e.Verdict == "denied" && s.once("conn|"+cg+"|"+e.Service, 10*time.Minute) {
		a := s.newAlert("warn", cg, "Blocked connection to "+e.Service, fmt.Sprintf("%s tried %s: %s.", cg, e.Service, or(e.Reason, "denied")))
		alert = &a
	}
	s.mu.Unlock()
	s.svc.mu.Lock()
	s.svc.conns = append(s.svc.conns, ev)
	if len(s.svc.conns) > 2000 {
		s.svc.conns = s.svc.conns[len(s.svc.conns)-2000:]
	}
	s.svc.mu.Unlock()
	s.publish("conn", ev)
	if alert != nil {
		s.publish("alert", *alert)
		go s.notify(*alert)
	}
}
