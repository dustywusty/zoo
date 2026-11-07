package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
)

func TestParsePreviewHost(t *testing.T) {
	cases := []struct {
		in   string
		cage string
		port int
		ok   bool
	}{
		{"api-3fa2c1.localhost:7779", "api-3fa2c1", 0, true},
		{"api-3fa2c1.localhost", "api-3fa2c1", 0, true},
		{"API-3FA2C1.LOCALHOST:7779", "api-3fa2c1", 0, true},
		{"5173.api-3fa2c1.localhost:7779", "api-3fa2c1", 5173, true},
		{"3000.web.localhost", "web", 3000, true},
		{"65535.web.localhost", "web", 65535, true},
		{"65536.web.localhost", "", 0, false},
		{"0.web.localhost", "", 0, false},
		{"05173.web.localhost", "", 0, false},
		{"localhost:7779", "", 0, false},
		{".localhost:7779", "", 0, false},
		{"127.0.0.1:7779", "", 0, false},
		{"[::1]:7779", "", 0, false},
		{"evil.com:7779", "", 0, false},
		{"web.localhost.evil.com", "", 0, false},
		{"web.localhost.:7779", "", 0, false},
		{"web.localhost:", "", 0, false},
		{"web.localhost:abc", "", 0, false},
		{"we_b.localhost", "", 0, false},
		{"a..b.localhost", "", 0, false},
		{"5173.localhost", "5173", 0, true}, // a cage named 5173 (lookup decides)
		{"", "", 0, false},
	}
	for _, c := range cases {
		cage, port, ok := ParsePreviewHost(c.in)
		if cage != c.cage || port != c.port || ok != c.ok {
			t.Errorf("ParsePreviewHost(%q) = %q, %d, %v; want %q, %d, %v", c.in, cage, port, ok, c.cage, c.port, c.ok)
		}
	}
}

const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1002 1 0000000000000000 100 0 0 10 0
   2: 0C40A8C0:A3C2 0A40A8C0:0C38 01 00000000:00000000 00:00000000 00000000  1000        0 1003 1 0000000000000000 20 4 30 10 -1
   3: 0C40A8C0:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1004 1 0000000000000000 100 0 0 10 0
`

const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1435 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2001 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2002 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000000000000:2382 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 2003 1 0000000000000000 100 0 0 10 0
   3: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2004 1 0000000000000000 100 0 0 10 0
   4: 00000000000000000000000001000000:1F49 00000000000000000000000001000000:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 2005 1 0000000000000000 20 4 30 10 -1
`

func TestParseProcNetTCP(t *testing.T) {
	got := ParseProcNetTCP(procTCP + procTCP6)
	want := Listeners{
		22:   "127.0.0.1",     // 0.0.0.0 beats ::
		5173: "127.0.0.1",     // 127.0.0.1 beats ::1
		8080: "192.168.64.12", // bound to the cage's address only
		3000: "::1",           // ::1 only (Node resolving localhost to ::1)
		9090: "::1",           // ::
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if p := got.Ports(); !reflect.DeepEqual(p, []int{3000, 5173, 8080, 9090}) {
		t.Errorf("Ports() = %v", p)
	}
	if d := got.Default(); d != 3000 {
		t.Errorf("Default() = %d", d)
	}
	if d := ParseProcNetTCP(procTCP[:strings.Index(procTCP, "   1:")]).Default(); d != 0 {
		t.Errorf("sshd only: Default() = %d, want 0", d)
	}
}

func TestPreviewPortConfig(t *testing.T) {
	g := config.Defaults()
	if p := PreviewPort(g); p != 7779 {
		t.Errorf("default = %d", p)
	}
	g.Port = 8000
	if p := PreviewPort(g); p != 8002 {
		t.Errorf("port+2 = %d", p)
	}
	zero := 0
	g.PreviewPort = &zero
	if p := PreviewPort(g); p != 0 {
		t.Errorf("disabled = %d", p)
	}
	if u := PreviewURL("web", 5173, 7779); u != "http://5173.web.localhost:7779" {
		t.Error(u)
	}
}

// upstream is a dev server: echoes the Host it saw, and upgrades /ws to an
// echo connection.
func upstream(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				http.Error(w, "want upgrade", 400)
				return
			}
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			brw.WriteString("echo " + line)
			brw.Flush()
			return
		}
		fmt.Fprintf(w, "host=%s path=%s xfh=%s", r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Host"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func upstreamPort(t *testing.T, srv *httptest.Server) int {
	_, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(ps)
	return p
}

func newTestProxy(t *testing.T, defPort int, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *httptest.Server {
	px := newPreviewProxy(
		func(name string) (string, bool) {
			if name == "web" {
				return "zoo-web", true
			}
			return "", false
		},
		func(context.Context, string) int { return defPort },
		dial)
	front := httptest.NewServer(px)
	t.Cleanup(func() { front.Close(); px.tr.CloseIdleConnections() })
	return front
}

func get(t *testing.T, front *httptest.Server, host, path string) (int, string) {
	req, _ := http.NewRequest("GET", front.URL+path, nil)
	req.Host = host
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPreviewProxy(t *testing.T) {
	up := upstream(t)
	port := upstreamPort(t, up)
	var dials atomic.Int32
	var lastAddr atomic.Value
	front := newTestProxy(t, port, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		lastAddr.Store(addr)
		_, ps, _ := net.SplitHostPort(addr)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", "127.0.0.1:"+ps)
	})

	host := "web.localhost:7779"
	code, body := get(t, front, host, "/a")
	if code != 200 || body != "host=web.localhost:7779 path=/a xfh=web.localhost:7779" {
		t.Fatalf("default port: %d %q", code, body)
	}
	if a := lastAddr.Load(); a != fmt.Sprintf("zoo-web:%d", port) {
		t.Errorf("dial addr = %v", a)
	}
	host = fmt.Sprintf("%d.web.localhost:7779", port)
	for i := 0; i < 3; i++ {
		if code, body := get(t, front, host, "/b"); code != 200 || !strings.HasPrefix(body, "host="+host) {
			t.Fatalf("explicit port: %d %q", code, body)
		}
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1 (keep-alive reuse)", n)
	}

	for _, h := range []string{"other.localhost:7779", "localhost:7779", "127.0.0.1:7779", "evil.example:7779", "web.localhost.evil.example"} {
		if code, _ := get(t, front, h, "/"); code != 404 {
			t.Errorf("host %q: %d, want 404", h, code)
		}
	}
	if dials.Load() != 1 {
		t.Error("a rejected host dialed")
	}
}

func TestPreviewNoDefault(t *testing.T) {
	front := newTestProxy(t, 0, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed")
		return nil, io.EOF
	})
	if code, body := get(t, front, "web.localhost:7779", "/"); code != 502 || !strings.Contains(body, "nothing is listening") {
		t.Errorf("%d %q", code, body)
	}
}

func TestPreviewDialError(t *testing.T) {
	front := newTestProxy(t, 1234, func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("nothing is listening on port 1234")
	})
	if code, body := get(t, front, "web.localhost:7779", "/"); code != 502 || !strings.Contains(body, "port 1234") {
		t.Errorf("%d %q", code, body)
	}
}

// websocket checks an Upgrade round-trips through the proxy.
func websocket(t *testing.T, front *httptest.Server, host string) {
	t.Helper()
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	fmt.Fprintf(conn, "hello hmr\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo hello hmr\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
}

func TestPreviewWebSocket(t *testing.T) {
	up := upstream(t)
	front := newTestProxy(t, upstreamPort(t, up), func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, ps, _ := net.SplitHostPort(addr)
		return net.Dial("tcp", "127.0.0.1:"+ps)
	})
	websocket(t, front, "web.localhost:7779")
}

// execBackend fakes a runtime's exec: the "process" connects stdin/stdout
// to a local TCP port, as socat would inside the cage.
type execBackend struct {
	backend.Backend
	execs atomic.Int32
}

func (b *execBackend) Exec(ctx context.Context, s backend.ExecSpec) (int, error) {
	b.execs.Add(1)
	if s.User != "agent" || len(s.Cmd) < 2 || s.Cmd[0] != "bash" {
		return 1, nil
	}
	if _, ok := s.Stdin.(*os.File); !ok {
		return -1, fmt.Errorf("stdin is not a file")
	}
	addr, port := s.Cmd[len(s.Cmd)-2], s.Cmd[len(s.Cmd)-1]
	conn, err := net.Dial("tcp", net.JoinHostPort(addr, port))
	if err != nil {
		return 1, nil
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, s.Stdin)
		conn.(*net.TCPConn).CloseWrite()
	}()
	done := make(chan struct{})
	go func() { io.Copy(s.Stdout, conn); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return 0, nil
}

func TestDialExec(t *testing.T) {
	up := upstream(t)
	port := upstreamPort(t, up)
	b := &execBackend{}
	front := newTestProxy(t, port, func(ctx context.Context, network, addr string) (net.Conn, error) {
		id, ps, _ := net.SplitHostPort(addr)
		p, _ := strconv.Atoi(ps)
		return DialExec(ctx, b, id, "127.0.0.1", p)
	})
	for i := 0; i < 3; i++ {
		if code, body := get(t, front, "web.localhost:7779", "/x"); code != 200 || body != "host=web.localhost:7779 path=/x xfh=web.localhost:7779" {
			t.Fatalf("%d %q", code, body)
		}
	}
	if n := b.execs.Load(); n != 1 {
		t.Errorf("execs = %d, want 1 (keep-alive reuse)", n)
	}
	websocket(t, front, "web.localhost:7779")

	// The server closing the connection reaches the host as EOF.
	c, err := DialExec(context.Background(), b, "zoo-web", "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: web.localhost\r\nConnection: close\r\n\r\n")
	all, err := io.ReadAll(c)
	if err != nil || !strings.Contains(string(all), "host=web.localhost") {
		t.Fatalf("read %q, %v", all, err)
	}

	if _, err := DialExec(context.Background(), b, "zoo-web", "not-an-ip", port); err == nil {
		t.Error("bad address accepted")
	}
}
