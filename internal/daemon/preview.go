package daemon

// Preview URLs: open a cage's dev servers from the host browser as
// http://<cage>.localhost:<preview port> (the cage's lowest listening port)
// or http://<port>.<cage>.localhost:<preview port>.
//
// The host often can't route to a cage (Docker Desktop, Linux internal
// networks without a host address, per-cage networks), so nothing here uses
// the network: each connection is a `<runtime> exec` in the cage running
// socat (or bash's /dev/tcp) to the dev server's loopback address, and the
// exec's stdin/stdout is the connection. Connections are kept alive and
// reused because an exec costs ~100 ms on Apple `container`.
//
// The listener is loopback only and answers only for Host headers naming a
// running cage, so a DNS-rebinding page can't use it, and it never proxies
// anywhere but a port inside the named cage.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
)

// PreviewPort is where zood serves preview URLs; 0 means off.
func PreviewPort(g config.Global) int {
	if g.PreviewPort == nil {
		return g.Port + 2 // +1 is the host bridge
	}
	return max(*g.PreviewPort, 0)
}

// PreviewURL is the URL for a cage's dev server; port 0 means its default.
func PreviewURL(cageName string, port, previewPort int) string {
	if port > 0 {
		return fmt.Sprintf("http://%d.%s.localhost:%d", port, cageName, previewPort)
	}
	return fmt.Sprintf("http://%s.localhost:%d", cageName, previewPort)
}

// --- listening ports ------------------------------------------------------------

// Listeners maps a listening TCP port to the address to dial it at from
// inside the cage (127.0.0.1 for 0.0.0.0, ::1 for ::, else the bound address).
type Listeners map[int]string

// Ports returns the ports a browser would want, lowest first: everything
// except sshd.
func (l Listeners) Ports() []int {
	out := make([]int, 0, len(l))
	for p := range l {
		if p != 22 {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

// Default is the port <cage>.localhost goes to: the lowest one, or 0.
func (l Listeners) Default() int {
	if ps := l.Ports(); len(ps) > 0 {
		return ps[0]
	}
	return 0
}

// ParseProcNetTCP reads /proc/net/tcp and /proc/net/tcp6 text (either or
// both, concatenated) and returns sockets in the LISTEN state (0A).
func ParseProcNetTCP(text string) Listeners {
	type cand struct {
		addr string
		rank int
	}
	best := map[int]cand{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		ipHex, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		ip := procIP(ipHex)
		if ip == nil {
			continue
		}
		addr, rank := dialAddr(ip)
		if c, ok := best[int(port)]; !ok || rank < c.rank {
			best[int(port)] = cand{addr, rank}
		}
	}
	out := Listeners{}
	for p, c := range best {
		out[p] = c.addr
	}
	return out
}

// procIP decodes /proc/net's hex address: 32-bit words in host byte order
// (little-endian on every architecture cages run on).
func procIP(h string) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}

// dialAddr picks the address to reach a listener bound to ip, and how much
// to prefer it when a port has several listeners (lower is better).
func dialAddr(ip net.IP) (string, int) {
	if v4 := ip.To4(); v4 != nil {
		if v4.IsUnspecified() {
			return "127.0.0.1", 0
		}
		if v4.IsLoopback() {
			return v4.String(), 0
		}
		return v4.String(), 2
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return "::1", 1
	}
	return ip.String(), 2
}

// ListPorts reads a running cage's listening TCP ports.
func ListPorts(ctx context.Context, b backend.Backend, id string) (Listeners, error) {
	var out, errb bytes.Buffer
	code, err := b.Exec(ctx, backend.ExecSpec{
		ID: id, User: "agent", NoStdin: true,
		Cmd:    []string{"sh", "-c", "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; true"},
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("reading ports in %s: exit %d: %s", id, code, strings.TrimSpace(errb.String()))
	}
	return ParseProcNetTCP(out.String()), nil
}

// --- Host header ----------------------------------------------------------------

// ParsePreviewHost splits a Host header of the form <cage>.localhost[:port]
// or <n>.<cage>.localhost[:port]. port is 0 when the cage's default is
// meant. It only checks the shape; the caller checks the cage exists.
func ParsePreviewHost(h string) (cageName string, port int, ok bool) {
	h = strings.ToLower(h)
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		p := h[i+1:]
		if p == "" || !allDigits(p) {
			return "", 0, false
		}
		h = h[:i]
	}
	rest, found := strings.CutSuffix(h, ".localhost")
	if !found || rest == "" {
		return "", 0, false
	}
	if first, after, dot := strings.Cut(rest, "."); dot && allDigits(first) {
		n, err := strconv.Atoi(first)
		if err != nil || n < 1 || n > 65535 || first[0] == '0' {
			return "", 0, false
		}
		port, rest = n, after
	}
	if !validCageName(rest) {
		return "", 0, false
	}
	return rest, port, true
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func validCageName(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[0] == '.' || s[len(s)-1] == '.' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return !strings.Contains(s, "..")
}

// --- proxy --------------------------------------------------------------------------

type previewTarget struct {
	id   string
	port int
}

type previewKey struct{}

// previewProxy is the HTTP side, independent of how cages are found or
// dialed so it can be tested without one.
type previewProxy struct {
	// lookup resolves a cage name to the id of a running cage.
	lookup func(name string) (id string, ok bool)
	// defaultPort returns the port <cage>.localhost goes to (0: none).
	defaultPort func(ctx context.Context, id string) int
	rp          *httputil.ReverseProxy
	tr          *http.Transport
}

// newPreviewProxy builds the proxy. dial gets addr "<cage id>:<port>".
func newPreviewProxy(lookup func(string) (string, bool), defaultPort func(context.Context, string) int,
	dial func(ctx context.Context, network, addr string) (net.Conn, error)) *previewProxy {
	tr := &http.Transport{
		Proxy:               nil, // never the keeper or anything in the environment
		DialContext:         dial,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
	p := &previewProxy{lookup: lookup, defaultPort: defaultPort, tr: tr}
	p.rp = &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(previewKey{}).(previewTarget)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(t.id, strconv.Itoa(t.port))
			// Keep the browser's Host so redirects and absolute URLs the dev
			// server builds point back at the preview URL.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			t, _ := r.Context().Value(previewKey{}).(previewTarget)
			http.Error(w, fmt.Sprintf("zoo: couldn't reach port %d in %s: %v", t.port, t.id, err), http.StatusBadGateway)
		},
	}
	return p
}

func (p *previewProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, port, ok := ParsePreviewHost(r.Host)
	var id string
	if ok {
		id, ok = p.lookup(name)
	}
	if !ok {
		// Not ours (wrong host, unknown or stopped cage): DNS-rebinding defence.
		http.NotFound(w, r)
		return
	}
	if port == 0 {
		if port = p.defaultPort(r.Context(), id); port == 0 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, fmt.Sprintf("zoo: nothing is listening in cage %s yet. Start a dev server there (e.g. npm run dev), then reload.", name), http.StatusBadGateway)
			return
		}
	}
	ctx := context.WithValue(r.Context(), previewKey{}, previewTarget{id: id, port: port})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// --- dialing through exec ---------------------------------------------------------

// dialScript connects stdin/stdout to $1:$2 inside the cage: socat when the
// image has it, else bash's /dev/tcp. When the server closes, the process
// exits so the host side sees EOF. (A background job's stdin is /dev/null
// in a non-interactive shell, hence the copy of stdin on fd 4.)
const dialScript = `a=$1 p=$2
if command -v socat >/dev/null 2>&1; then
  case $a in
    *:*) exec socat - "TCP6:[$a]:$p" ;;
    *) exec socat - "TCP4:$a:$p" ;;
  esac
fi
exec 3<>"/dev/tcp/$a/$p" || exit 1
exec 4<&0
cat <&4 >&3 2>/dev/null &
exec cat <&3 4<&-`

// execConn is a net.Conn over an exec's stdin and stdout. Both are OS pipes
// so the runtime CLI gets them directly (no copy goroutines inside os/exec,
// so the exec returns as soon as the process ends) and deadlines work.
type execConn struct {
	r, w   *os.File
	cancel context.CancelFunc
	once   sync.Once
	local  net.Addr
	remote net.Addr
}

func (c *execConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *execConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *execConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.r.Close()
		c.cancel()
	})
	return nil
}
func (c *execConn) LocalAddr() net.Addr                { return c.local }
func (c *execConn) RemoteAddr() net.Addr               { return c.remote }
func (c *execConn) SetDeadline(t time.Time) error      { c.r.SetDeadline(t); return c.w.SetDeadline(t) }
func (c *execConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *execConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

type execAddr string

func (execAddr) Network() string  { return "exec" }
func (a execAddr) String() string { return string(a) }

// DialExec opens a connection to addr:port inside cage id via the backend's
// exec, as the agent user. The connection lives until closed or the process
// ends; ctx only bounds setup.
func DialExec(ctx context.Context, b backend.Backend, id, addr string, port int) (net.Conn, error) {
	if net.ParseIP(addr) == nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("bad target %s:%d", addr, port)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	ectx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := b.Exec(ectx, backend.ExecSpec{
			ID: id, User: "agent",
			Cmd:   []string{"bash", "-c", dialScript, "zoo-preview", addr, strconv.Itoa(port)},
			Stdin: inR, Stdout: outW, Stderr: io.Discard,
		})
		if err != nil && ectx.Err() == nil {
			log.Printf("preview: exec in %s: %v", id, err)
		}
		inR.Close()
		outW.Close()
		cancel()
	}()
	return &execConn{
		r: outR, w: inW, cancel: cancel,
		local:  execAddr("zood"),
		remote: execAddr(fmt.Sprintf("%s/%s", id, net.JoinHostPort(addr, strconv.Itoa(port)))),
	}, nil
}

// --- daemon wiring -------------------------------------------------------------------

type previewState struct {
	mu      sync.Mutex
	ports   map[string]Listeners // by cage id
	scanned map[string]time.Time
}

func newPreviewState() *previewState {
	return &previewState{ports: map[string]Listeners{}, scanned: map[string]time.Time{}}
}

func (p *previewState) portsFor(id string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ports[id].Ports()
}

// runningCage returns the id of the running cage called name.
func (s *Server) runningCage(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cages {
		if c.Running && strings.EqualFold(c.Name, name) {
			return c.ID, true
		}
	}
	return "", false
}

func (s *Server) cageRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cages {
		if c.Running && c.ID == id {
			return true
		}
	}
	return false
}

// scanPorts reads a cage's ports unless that happened within maxAge.
func (s *Server) scanPorts(ctx context.Context, id string, maxAge time.Duration) Listeners {
	s.prev.mu.Lock()
	if t, ok := s.prev.scanned[id]; ok && time.Since(t) < maxAge {
		l := s.prev.ports[id]
		s.prev.mu.Unlock()
		return l
	}
	s.prev.scanned[id] = time.Now()
	s.prev.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	l, err := ListPorts(ctx, s.backend, id)
	if err != nil {
		l = nil
	}
	s.prev.mu.Lock()
	if len(l) > 0 {
		s.prev.ports[id] = l
	} else {
		delete(s.prev.ports, id)
	}
	s.prev.mu.Unlock()
	return l
}

// pollPorts refreshes listening ports for every running cage.
func (s *Server) pollPorts() {
	s.mu.Lock()
	var ids []string
	for _, c := range s.cages {
		if c.Running {
			ids = append(ids, c.ID)
		}
	}
	s.mu.Unlock()
	live := map[string]bool{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, id := range ids {
		live[id] = true
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.scanPorts(context.Background(), id, 5*time.Second)
		}(id)
	}
	wg.Wait()
	s.prev.mu.Lock()
	for id := range s.prev.ports {
		if !live[id] {
			delete(s.prev.ports, id)
		}
	}
	for id := range s.prev.scanned {
		if !live[id] {
			delete(s.prev.scanned, id)
		}
	}
	s.prev.mu.Unlock()
}

// dialPreview is the proxy's dialer: addr is "<cage id>:<port>".
func (s *Server) dialPreview(ctx context.Context, _, addr string) (net.Conn, error) {
	id, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	if !s.cageRunning(id) {
		return nil, fmt.Errorf("cage %s isn't running", id)
	}
	s.prev.mu.Lock()
	target, ok := s.prev.ports[id][port]
	s.prev.mu.Unlock()
	if !ok {
		// Started since the last poll? Look again (at most every 2 s).
		target, ok = s.scanPorts(ctx, id, 2*time.Second)[port]
	}
	if !ok {
		return nil, fmt.Errorf("nothing is listening on port %d", port)
	}
	return DialExec(ctx, s.backend, id, target, port)
}

func (s *Server) defaultPreviewPort(ctx context.Context, id string) int {
	s.prev.mu.Lock()
	l := s.prev.ports[id]
	s.prev.mu.Unlock()
	if p := l.Default(); p != 0 {
		return p
	}
	return s.scanPorts(ctx, id, 2*time.Second).Default()
}

// runPreview serves preview URLs on loopback until ctx is done. A port that
// can't be bound is logged, not fatal: the dashboard still works.
func (s *Server) runPreview(ctx context.Context) {
	port := PreviewPort(s.cfg)
	if port == 0 {
		return
	}
	go func() {
		s.pollPorts()
		s.loop(ctx, 10*time.Second, s.pollPorts)
	}()
	px := newPreviewProxy(s.runningCage, s.defaultPreviewPort, s.dialPreview)
	srv := &http.Server{Handler: px, ReadHeaderTimeout: 10 * time.Second}
	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			// ::1 is a convenience (macOS resolves *.localhost to ::1 first);
			// 127.0.0.1 is what matters.
			if host == "127.0.0.1" {
				log.Printf("preview URLs off: %v", err)
				return
			}
			continue
		}
		lns = append(lns, ln)
	}
	log.Printf("zoo previews on http://<cage>.localhost:%d", port)
	for _, ln := range lns {
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("preview: %v", err)
			}
		}(ln)
	}
	<-ctx.Done()
	_ = srv.Close()
	px.tr.CloseIdleConnections()
}
