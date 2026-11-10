// Package daemon is the zoo host daemon: it tails the keeper's access log,
// polls cage resources, raises alerts and serves the dashboard on 127.0.0.1.
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/gitrefs"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/proc"
	"github.com/dusty/zoo/internal/services"
)

//go:embed web/index.html
var indexHTML string

const maxEvents = 5000

// NetEvent is a keeper event attributed to a cage.
type NetEvent struct {
	ID int64 `json:"id"`
	keeper.Event
	Cage        string `json:"cage"`
	FirstSeen   bool   `json:"first_seen"`  // first time this cage reached this host
	Allowlisted bool   `json:"allowlisted"` // host is on the allowlist right now
}

type Alert struct {
	ID    int64     `json:"id"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // warn | crit
	Cage  string    `json:"cage"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
}

type Counts struct {
	Requests int64 `json:"requests"`
	Denied   int64 `json:"denied"`
	BytesOut int64 `json:"bytes_out"`
	BytesIn  int64 `json:"bytes_in"`
	Hosts    int   `json:"hosts"`
}

type Session struct {
	ID      string    `json:"id"`
	Cage    string    `json:"cage"`
	Harness string    `json:"harness"`
	Started time.Time `json:"started"`
	HostPID int       `json:"host_pid"`
}

type Server struct {
	cfg     config.Global
	token   string
	backend backend.Backend
	info    backend.Info
	floor   backend.Isolation

	mu        sync.Mutex
	historyMu sync.Mutex
	cursor    logCursor
	cages     []cage.Info
	keeperUp  bool
	keeperIP  string
	stats     map[string]Stat // by container id
	events    []NetEvent
	alerts    []Alert
	counts    map[string]*Counts
	seen      map[string]map[string]bool
	sentBytes map[string]int64 // cage|host -> bytes out
	alerted   map[string]time.Time
	sessions  map[string]Session
	allowlist []string
	nextID    int64
	subs      map[chan []byte]struct{}
	lastErr   string
	svc       *svcState
	prev      *previewState
}

// Token returns the dashboard token, creating it on first use.
func Token() (string, error) {
	if b, err := os.ReadFile(paths.Token()); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	return t, os.WriteFile(paths.Token(), []byte(t+"\n"), 0o600)
}

func New(cfg config.Global, b backend.Backend, floor backend.Isolation) (*Server, error) {
	tok, err := Token()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, token: tok, backend: b, floor: floor,
		info:  b.Detect(context.Background()),
		stats: map[string]Stat{}, counts: map[string]*Counts{},
		seen: map[string]map[string]bool{}, sentBytes: map[string]int64{},
		alerted: map[string]time.Time{}, sessions: map[string]Session{},
		subs: map[chan []byte]struct{}{},
		svc:  newSvcState(),
		prev: newPreviewState(),
	}, nil
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	loaded, err := s.loadHistory()
	if err != nil {
		return err
	}
	s.refreshAllowlist()
	s.pollCages()
	// Backfill recent history so the feed isn't empty after a restart, then
	// tail from exactly where the backfill stopped (0 if the log is new).
	start := s.cursor.Offset
	if !loaded {
		s.cursor = initialCursor()
		start = s.cursor.Offset
		for _, e := range keeper.ReadTail(1000, start) {
			s.ingest(e, false)
		}
	} else if !cursorMatches(s.cursor) {
		start = 0
		s.cursor = logCursor{}
	}
	go s.watchCages(ctx)
	go s.loop(ctx, 2*time.Second, s.pollStats)
	go s.loop(ctx, 5*time.Second, s.refreshAllowlist)
	go s.loop(ctx, 2*time.Second, s.broadcastState)
	go s.loop(ctx, 30*time.Second, s.scanGitTraps)
	tailDone := make(chan struct{})
	go func() { defer close(tailDone); s.tail(ctx, start) }()
	go s.loop(ctx, 5*time.Second, func() {
		if err := s.saveHistory(); err != nil {
			log.Printf("save history: %v", err)
		}
	})
	defer func() {
		cancel()
		<-tailDone
		if err := s.saveHistory(); err != nil {
			log.Printf("save history: %v", err)
		}
	}()
	go s.tailGate(ctx)
	go s.runBridge(ctx)
	go s.runPreview(ctx)

	addr := fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Printf("zoo dashboard on http://%s", addr)
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// watchCages keeps the cage list current. Backends that report changes
// (backend.Watcher: docker through its API) are refreshed when something
// happens, plus a slow safety poll; others are polled every 2 s. Before
// D25 docker was polled too, which meant several docker processes every
// 2 s: enough to freeze a Windows desktop.
func (s *Server) watchCages(ctx context.Context) {
	s.pollCages()
	w, ok := s.backend.(backend.Watcher)
	if !ok {
		s.loop(ctx, 2*time.Second, s.pollCages)
		return
	}
	safety := time.NewTicker(30 * time.Second)
	defer safety.Stop()
	for ctx.Err() == nil {
		changes, err := w.Watch(ctx)
		if err != nil {
			// No event stream (API unreachable): poll until it's back.
			log.Printf("cage events: %v; polling", err)
			t := time.NewTimer(30 * time.Second)
			for done := false; !done; {
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
					done = true
				case <-time.After(2 * time.Second):
					s.pollCages()
				}
			}
			continue
		}
		s.pollCages() // catch up on anything missed while (re)connecting
		for open := true; open; {
			select {
			case <-ctx.Done():
				return
			case _, open = <-changes:
				if open {
					// Let a burst (create, connect, start) settle first.
					time.Sleep(250 * time.Millisecond)
					drain(changes)
					s.pollCages()
				}
			case <-safety.C:
				s.pollCages()
			}
		}
	}
}

func drain(c <-chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

func (s *Server) loop(ctx context.Context, every time.Duration, fn func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// --- polling ---------------------------------------------------------------

func (s *Server) pollCages() {
	ctx := context.Background()
	cs, err := cage.List(ctx, s.backend)
	kc, kok, _ := s.backend.Get(ctx, keeper.Name)
	if err == nil {
		s.syncServices(cs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
		return
	}
	s.lastErr = ""
	s.cages = cs
	s.keeperUp = kok && kc.Running
	s.keeperIP = kc.IP
	// Drop sessions whose `zoo run` process is gone (killed without cleanup).
	for id, ss := range s.sessions {
		if ss.HostPID > 0 && !alive(ss.HostPID) {
			delete(s.sessions, id)
		}
	}
}

func alive(pid int) bool { return proc.Alive(pid) }

func (s *Server) pollStats() {
	samples, err := s.backend.Sample(context.Background())
	if err != nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]Stat{}
	for _, sm := range samples {
		var prev *Stat
		if p, ok := s.stats[sm.ID]; ok {
			prev = &p
		}
		next[sm.ID] = toStat(sm, prev, now)
	}
	s.stats = next
}

func (s *Server) refreshAllowlist() {
	s.expireTemporary()
	l, err := keeper.ReadAllowlist()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.allowlist = l
	s.mu.Unlock()
}

// expireTemporary removes `--for` allows and grants whose time is up.
func (s *Server) expireTemporary() {
	now := time.Now()
	if removed, err := keeper.Expire(now); err != nil {
		log.Printf("expire allows: %v", err)
	} else if len(removed) > 0 {
		if err := keeper.Reload(context.Background(), s.backend); err != nil {
			log.Printf("keeper reload: %v", err)
		}
		s.mu.Lock()
		a := s.newAlert("info", "", "Temporary allow expired", strings.Join(removed, ", ")+" removed from the allowlist.")
		s.mu.Unlock()
		s.publish("alert", a)
	}
	gone, err := services.PruneExpired(now)
	if err != nil {
		log.Printf("expire grants: %v", err)
	}
	for _, g := range gone {
		s.mu.Lock()
		a := s.newAlert("info", g[0], "Temporary grant expired", g[1]+" revoked from "+g[0]+".")
		s.mu.Unlock()
		s.publish("alert", a)
	}
	if len(gone) > 0 {
		go s.pollCages()
	}
}

// --- keeper log --------------------------------------------------------------

func (s *Server) tail(ctx context.Context, off int64) {
	var partial []byte
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f, err := os.Open(paths.AccessLog())
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			continue
		}
		s.mu.Lock()
		cursor := s.cursor
		s.mu.Unlock()
		if st.Size() < off || !cursorMatches(cursor) {
			// Truncated or replaced: start over.
			off = 0
			partial = nil
			s.mu.Lock()
			s.cursor = logCursor{}
			s.mu.Unlock()
		}
		if st.Size() > off {
			if _, err := f.Seek(off, io.SeekStart); err == nil {
				buf, _ := io.ReadAll(io.LimitReader(f, st.Size()-off))
				off += int64(len(buf))
				buf = append(partial, buf...)
				lines := bytes.Split(buf, []byte("\n"))
				partial = append([]byte(nil), lines[len(lines)-1]...)
				lineEnd := off - int64(len(buf))
				for _, l := range lines[:len(lines)-1] {
					lineEnd += int64(len(l) + 1)
					cursor := cursorAt(lineEnd, append(append([]byte(nil), l...), '\n'))
					if e, ok := keeper.ParseLine(string(l)); ok {
						s.ingestAt(e, true, &cursor)
					} else {
						s.mu.Lock()
						s.cursor = cursor
						s.mu.Unlock()
					}
				}
			}
		}
		f.Close()
	}
}

func (s *Server) cageForIP(ip string) string {
	for _, c := range s.cages {
		if c.IP == ip {
			return c.Name
		}
	}
	if ip == "127.0.0.1" || ip == "::1" {
		return "keeper"
	}
	return "unknown " + ip
}

func (s *Server) ingest(e keeper.Event, live bool) {
	s.ingestAt(e, live, nil)
}

func (s *Server) ingestAt(e keeper.Event, live bool, cursor *logCursor) {
	s.mu.Lock()
	if cursor != nil {
		s.cursor = *cursor
	}
	s.nextID++
	ev := NetEvent{ID: s.nextID, Event: e, Cage: s.cageForIP(e.Client)}
	list := s.allowlist
	for _, c := range s.cages {
		if c.Name == ev.Cage && c.NetworkAllow != nil {
			list = c.NetworkAllow
			break
		}
	}
	ev.Allowlisted = keeper.Allowed(list, e.Host)
	if s.seen[ev.Cage] == nil {
		s.seen[ev.Cage] = map[string]bool{}
	}
	if !s.seen[ev.Cage][e.Host] {
		s.seen[ev.Cage][e.Host] = true
		ev.FirstSeen = true
	}
	c := s.counts[ev.Cage]
	if c == nil {
		c = &Counts{}
		s.counts[ev.Cage] = c
	}
	c.Requests++
	c.BytesIn += e.BytesIn
	c.BytesOut += e.BytesOut
	c.Hosts = len(s.seen[ev.Cage])
	if e.Denied {
		c.Denied++
	}
	s.events = append(s.events, ev)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}

	var alerts []Alert
	if live {
		if e.Denied && s.once("deny|"+ev.Cage+"|"+e.Host, 10*time.Minute) {
			alerts = append(alerts, s.newAlert("warn", ev.Cage, "Blocked request to "+e.Host,
				fmt.Sprintf("%s %s was denied by the keeper.", e.Method, e.URL)))
		}
		k := ev.Cage + "|" + e.Host
		s.sentBytes[k] += e.BytesOut
		if s.cfg.BytesOutAlert > 0 && s.sentBytes[k] > s.cfg.BytesOutAlert && s.once("bytes|"+k, 24*time.Hour) {
			alerts = append(alerts, s.newAlert("crit", ev.Cage, "Large upload to "+e.Host,
				fmt.Sprintf("%s has sent %s to %s.", ev.Cage, human(s.sentBytes[k]), e.Host)))
		}
	}
	s.mu.Unlock()

	if live {
		s.publish("net", ev)
		for _, a := range alerts {
			s.publish("alert", a)
			go s.notify(a)
		}
	}
}

// scanGitTraps looks for repositories planted in running cages' worktrees
// (see cage.GitTraps) and raises a critical alert for each new one, so you
// hear about it before your shell prompt or editor runs git there.
func (s *Server) scanGitTraps() {
	s.mu.Lock()
	cages := append([]cage.Info(nil), s.cages...)
	s.mu.Unlock()
	for _, c := range cages {
		if !c.Running || c.Worktree == "" {
			continue
		}
		var traps []cage.Trap
		for _, root := range c.Roots() {
			t, _ := cage.GitTraps(root)
			traps = append(traps, t...)
		}
		// Branches and tags moved besides the cage's own, since its session
		// began (snapshot taken by zoo run). Read from files: no process.
		var moved []gitrefs.Change
		if snap, ok := gitrefs.Load(paths.RefSnapshot(c.Name)); ok {
			common, gitDir := cage.GitDirs(c.Worktree)
			moved = gitrefs.Alarms(gitrefs.Check(snap, common, gitDir))
		}
		var alerts []Alert
		s.mu.Lock()
		for _, t := range traps {
			if s.once("gittrap|"+t.Path+"|"+t.Reason, 24*time.Hour) {
				alerts = append(alerts, s.newAlert("crit", c.Name, "Planted git repository",
					fmt.Sprintf("%s (%s). Running git there, or opening it in an editor or a shell with a git prompt, can run commands the agent chose. Inspect and delete it.", t.Path, t.Reason)))
			}
		}
		for _, m := range moved {
			if s.once("ref|"+c.Name+"|"+m.Ref+"|"+m.To, 24*time.Hour) {
				alerts = append(alerts, s.newAlert("warn", c.Name, "Branch moved: "+m.Ref,
					fmt.Sprintf("%s while %s was running. Check it before you push. Undo: %s. If you did it on the host, accept with: zoo refs ok %s", m, c.Name, m.Undo(), c.Name)))
			}
		}
		s.mu.Unlock()
		for _, a := range alerts {
			s.publish("alert", a)
			go s.notify(a)
		}
	}
}

func (s *Server) once(key string, every time.Duration) bool {
	if t, ok := s.alerted[key]; ok && time.Since(t) < every {
		return false
	}
	s.alerted[key] = time.Now()
	return true
}

func (s *Server) newAlert(level, cage, title, body string) Alert {
	s.nextID++
	a := Alert{ID: s.nextID, Time: time.Now().UTC(), Level: level, Cage: cage, Title: title, Body: body}
	s.alerts = append(s.alerts, a)
	if len(s.alerts) > 500 {
		s.alerts = s.alerts[len(s.alerts)-500:]
	}
	return a
}

func (s *Server) notify(a Alert) {
	if s.cfg.NtfyURL == "" {
		return
	}
	req, err := http.NewRequest("POST", s.cfg.NtfyURL, strings.NewReader(a.Body))
	if err != nil {
		return
	}
	req.Header.Set("Title", "zoo: "+a.Title)
	if a.Level == "crit" {
		req.Header.Set("Priority", "high")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resp, err := http.DefaultClient.Do(req.WithContext(ctx)); err == nil {
		resp.Body.Close()
	}
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// --- pub/sub ---------------------------------------------------------------

func (s *Server) publish(kind string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte("event: " + kind + "\ndata: " + string(b) + "\n\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default: // slow client; drop
		}
	}
}

type cageView struct {
	cage.Info
	Grants []string `json:"grants"`
	// GrantExpiry: temporary grants and when they lapse.
	GrantExpiry map[string]time.Time `json:"grant_expiry,omitempty"`
	Stat        *Stat                `json:"stat,omitempty"`
	Counts      Counts               `json:"counts"`
	Sessions    []Session            `json:"sessions"`
	Ports       []int                `json:"ports,omitempty"` // listening in the cage (preview.go)
}

type stateView struct {
	Services  []ServiceView `json:"services"`
	Pending   []*Pending    `json:"pending"`
	Backend   any           `json:"backend"`
	Cages     []cageView    `json:"cages"`
	Keeper    any           `json:"keeper"`
	Allowlist []string      `json:"allowlist"`
	// AllowExpiry: temporary allowlist entries and when they lapse.
	AllowExpiry map[string]time.Time `json:"allow_expiry"`
	Error       string               `json:"error,omitempty"`
	Now         time.Time            `json:"now"`
	PreviewPort int                  `json:"preview_port"` // 0 = preview URLs off
}

func (s *Server) snapshot() stateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := stateView{
		Backend: map[string]any{
			"name": s.info.Name, "detail": s.info.Detail, "version": s.info.Version,
			"isolation": s.info.Caps.Isolation, "floor": s.floor,
		},
		Keeper:      map[string]any{"running": s.keeperUp, "ip": s.keeperIP},
		Allowlist:   append([]string(nil), s.allowlist...),
		AllowExpiry: keeper.LoadExpiries(),
		Error:       s.lastErr,
		Now:         time.Now().UTC(),
		PreviewPort: PreviewPort(s.cfg),
	}
	s.svc.mu.Lock()
	v.Services = append([]ServiceView(nil), s.svc.views...)
	v.Pending = []*Pending{}
	for _, p := range s.svc.pending {
		v.Pending = append(v.Pending, p)
	}
	grants := s.svc.grants
	s.svc.mu.Unlock()
	for _, c := range s.cages {
		cv := cageView{Info: c, Grants: grants[c.Name], Ports: s.prev.portsFor(c.ID)}
		for _, g := range cv.Grants {
			if t := services.GrantExpiry(c.Name, g); !t.IsZero() {
				if cv.GrantExpiry == nil {
					cv.GrantExpiry = map[string]time.Time{}
				}
				cv.GrantExpiry[g] = t
			}
		}
		if st, ok := s.stats[c.ID]; ok {
			st := st
			cv.Stat = &st
		}
		if cnt := s.counts[c.Name]; cnt != nil {
			cv.Counts = *cnt
		}
		for _, ss := range s.sessions {
			if ss.Cage == c.Name {
				cv.Sessions = append(cv.Sessions, ss)
			}
		}
		v.Cages = append(v.Cages, cv)
	}
	return v
}

func (s *Server) broadcastState() { s.publish("state", s.snapshot()) }

// --- HTTP ---------------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("POST /api/cages/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /api/allow", s.handleAllow)
	mux.HandleFunc("POST /api/sessions", s.handleSessionStart)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionEnd)
	mux.HandleFunc("POST /api/grants", s.handleGrant)
	mux.HandleFunc("DELETE /api/grants", s.handleGrant)
	mux.HandleFunc("POST /api/pending/{id}", s.handlePending)
	return s.guard(mux)
}

// guard blocks DNS rebinding (Host must be loopback) and cross-site writes
// (state changes need the token header, which a foreign page can't send).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Zoo-Token") != s.token {
			http.Error(w, "missing or bad token", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	io.WriteString(w, strings.Replace(indexHTML, "__ZOO_TOKEN__", s.token, 1))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	v := s.snapshot()
	s.mu.Lock()
	events := append([]NetEvent(nil), s.events...)
	alerts := append([]Alert(nil), s.alerts...)
	s.mu.Unlock()
	if len(events) > 1000 {
		events = events[len(events)-1000:]
	}
	s.svc.mu.Lock()
	conns := append([]ConnEvent(nil), s.svc.conns...)
	signs := append([]SignEvent(nil), s.svc.signs...)
	pending := make([]*Pending, 0, len(s.svc.pending))
	for _, p := range s.svc.pending {
		pending = append(pending, p)
	}
	s.svc.mu.Unlock()
	writeJSON(w, map[string]any{"state": v, "events": events, "alerts": alerts, "conns": conns, "signs": signs, "pending": pending})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan []byte, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	io.WriteString(w, ": hello\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			w.Write(m)
			fl.Flush()
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !strings.HasPrefix(id, cage.Prefix) || id == keeper.Name {
		http.Error(w, "not a cage", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if err := s.backend.Stop(ctx, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go s.pollCages()
	writeJSON(w, map[string]string{"stopped": id})
}

func (s *Server) handleAllow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host string `json:"host"`
		For  string `json:"for"` // optional: 30m, 4h, 2d
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Host == "" {
		http.Error(w, "need {\"host\": ...}", http.StatusBadRequest)
		return
	}
	var added []string
	var err error
	if body.For != "" {
		d, perr := config.ParseFor(body.For)
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusBadRequest)
			return
		}
		added, err = keeper.AllowFor(time.Now().Add(d), body.Host)
	} else {
		added, err = keeper.Allow(body.Host)
		if err == nil {
			err = keeper.MakePermanent(body.Host)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(added) > 0 {
		if err := keeper.Reload(r.Context(), s.backend); err != nil {
			http.Error(w, "saved, but keeper reload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.refreshAllowlist()
	writeJSON(w, map[string]any{"added": added})
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	var ss Session
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ss); err != nil || ss.Cage == "" {
		http.Error(w, "bad session", http.StatusBadRequest)
		return
	}
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	ss.ID = hex.EncodeToString(buf)
	ss.Started = time.Now().UTC()
	s.mu.Lock()
	s.sessions[ss.ID] = ss
	s.mu.Unlock()
	writeJSON(w, ss)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delete(s.sessions, r.PathValue("id"))
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
