// zoo-inject (part of zoo-gate): terminates TLS for credential hosts only,
// swaps placeholders for real credentials fetched from zood over the host
// bridge, enforces each credential's policy, and forwards to the real host
// with normal certificate checks. squid routes only those hosts' CONNECTs
// here (cache_peer on 127.0.0.1:3129); every other host stays a tunnel.
//
// Config: /etc/zoo/inject/inject.json (no secrets). Log: inject.log, one
// JSON line per request.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type injectRule struct {
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts"`
	ReadOnly bool     `json:"readonly"`
	Push     []string `json:"push"`
	Repos    []string `json:"repos"`
}

type injectConfig struct {
	Listen string       `json:"listen"`
	CACert string       `json:"ca_cert"`
	CAKey  string       `json:"ca_key"`
	Rules  []injectRule `json:"rules"`
}

type injectEntry struct {
	TS      string `json:"ts"`
	Host    string `json:"host"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Cred    string `json:"cred,omitempty"`
	Cage    string `json:"cage,omitempty"`
	Verdict string `json:"verdict"` // allowed | denied | error
	Reason  string `json:"reason,omitempty"`
	Status  int    `json:"status,omitempty"`
	MS      int64  `json:"ms"`
}

// secretSource fetches a credential's value for a placeholder; ok=false
// with a reason when zood refuses.
type secretSource func(placeholder string) (value, reason string, ok bool)

type injector struct {
	cfg    atomic.Pointer[injectConfig]
	ca     atomic.Pointer[tls.Certificate]
	caPEM  []byte
	leafMu sync.Mutex
	leaves map[string]*tls.Certificate
	secret secretSource
	logMu  sync.Mutex
	logw   io.Writer
	// upstream is the transport to real hosts; tests replace it.
	upstream http.RoundTripper
}

var placeholderRe = regexp.MustCompile(`[A-Za-z0-9_-]*zoocred\.([a-z0-9-]+)\.([a-z0-9-]+)\.([0-9a-f]{24})`)

func newInjector(secret secretSource, logw io.Writer) *injector {
	in := &injector{leaves: map[string]*tls.Certificate{}, secret: secret, logw: logw}
	in.cfg.Store(&injectConfig{})
	in.upstream = &http.Transport{
		Proxy:                 nil,
		DialContext:           publicDialer,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return in
}

// load reads inject.json and the CA. Leaves are re-minted after a CA change.
func (in *injector) load(confPath string) error {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return err
	}
	var c injectConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("parse %s: %w", confPath, err)
	}
	if len(c.Rules) > 0 {
		certPEM, err := os.ReadFile(c.CACert)
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(c.CAKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(certPEM, in.caPEM) {
			ca, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return fmt.Errorf("CA: %w", err)
			}
			ca.Leaf, _ = x509.ParseCertificate(ca.Certificate[0])
			in.ca.Store(&ca)
			in.caPEM = certPEM
			in.leafMu.Lock()
			in.leaves = map[string]*tls.Certificate{}
			in.leafMu.Unlock()
		}
	}
	in.cfg.Store(&c)
	return nil
}

func (in *injector) ruleFor(host string) (injectRule, bool) {
	for _, r := range in.cfg.Load().Rules {
		for _, h := range r.Hosts {
			if h == host {
				return r, true
			}
		}
	}
	return injectRule{}, false
}

func (in *injector) ruleNamed(name string) (injectRule, bool) {
	for _, r := range in.cfg.Load().Rules {
		if r.Name == name {
			return r, true
		}
	}
	return injectRule{}, false
}

// leaf mints (and caches) a certificate for host signed by the zoo CA.
func (in *injector) leaf(host string) (*tls.Certificate, error) {
	in.leafMu.Lock()
	defer in.leafMu.Unlock()
	if c, ok := in.leaves[host]; ok && time.Until(c.Leaf.NotAfter) > time.Hour {
		return c, nil
	}
	ca := in.ca.Load()
	if ca == nil {
		return nil, errors.New("no CA loaded")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Leaf, &k.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, ca.Certificate[0]}, PrivateKey: k}
	c.Leaf, _ = x509.ParseCertificate(der)
	in.leaves[host] = c
	return c, nil
}

// serve accepts squid's CONNECTs.
func (in *injector) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go in.handleConnect(c)
	}
}

func (in *injector) handleConnect(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if req.Method != http.MethodConnect || err != nil || port != "443" {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	host = strings.ToLower(host)
	if _, ok := in.ruleFor(host); !ok {
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	cert, err := in.leaf(host)
	if err != nil {
		log.Printf("zoo-inject: cert for %s: %v", host, err)
		return
	}
	tc := tls.Server(&bufConn{Conn: c, r: br}, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	srv := &http.Server{
		Handler:           in.handler(host),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_ = srv.Serve(&oneConn{c: tc, done: make(chan struct{})})
}

// bufConn reads what the CONNECT reader already buffered first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// oneConn is a listener that yields one connection, then blocks until it
// closes (so http.Server keeps serving it).
type oneConn struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
	used bool
}

func (l *oneConn) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return &closeNotify{Conn: l.c, done: l.done, once: &l.once}, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

type closeNotify struct {
	net.Conn
	done chan struct{}
	once *sync.Once
}

func (c *closeNotify) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// handler serves decrypted requests for one CONNECTed host.
func (in *injector) handler(host string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = host
			pr.Out.Host = host
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("X-Forwarded-For")
		},
		Transport:     in.upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "zoo keeper: upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		e := injectEntry{TS: start.UTC().Format(time.RFC3339Nano), Host: host, Method: r.Method, Path: r.URL.Path}
		deny := func(code int, why string) {
			e.Verdict, e.Reason, e.Status, e.MS = "denied", why, code, time.Since(start).Milliseconds()
			in.write(e)
			http.Error(w, "zoo keeper: "+why, code)
		}
		// The Host header must be the host squid allowed (no fronting).
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			r.Host = h
		}
		if !strings.EqualFold(r.Host, host) {
			deny(http.StatusMisdirectedRequest, "Host header doesn't match the connection")
			return
		}
		rule, _ := in.ruleFor(host)
		if why := checkPolicy(rule, r); why != "" {
			deny(http.StatusForbidden, why)
			return
		}
		cred, cage, why := in.substitute(r, host)
		e.Cred, e.Cage = cred, cage
		if why != "" {
			deny(http.StatusForbidden, why)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		rp.ServeHTTP(sw, r)
		e.Verdict, e.Status, e.MS = "allowed", sw.code, time.Since(start).Milliseconds()
		in.write(e)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = 200
	}
	return s.ResponseWriter.Write(b)
}
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// substitute replaces placeholders in request headers with real values. A
// placeholder for a credential whose hosts don't include this host is left
// alone (it's meaningless upstream). Basic auth is decoded to look inside.
func (in *injector) substitute(r *http.Request, host string) (cred, cage, why string) {
	for name, vals := range r.Header {
		for i, v := range vals {
			out, c, g, err := in.replaceIn(v, host)
			if err != "" {
				return c, g, err
			}
			if c != "" {
				cred, cage = c, g
			}
			if strings.EqualFold(name, "Authorization") {
				if enc, ok := strings.CutPrefix(out, "Basic "); ok {
					if dec, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(enc)); derr == nil {
						o2, c2, g2, err := in.replaceIn(string(dec), host)
						if err != "" {
							return c2, g2, err
						}
						if c2 != "" {
							cred, cage = c2, g2
							out = "Basic " + base64.StdEncoding.EncodeToString([]byte(o2))
						}
					}
				}
			}
			vals[i] = out
		}
	}
	return cred, cage, ""
}

func (in *injector) replaceIn(v, host string) (out, cred, cage, why string) {
	out = v
	for _, m := range placeholderRe.FindAllStringSubmatch(v, -1) {
		full, name, cg := m[0], m[1], m[2]
		rule, ok := in.ruleNamed(name)
		if !ok || !contains(rule.Hosts, host) {
			continue
		}
		val, reason, ok := in.secret(full)
		if !ok {
			return v, name, cg, "credential " + name + " for " + cg + ": " + reason
		}
		out = strings.ReplaceAll(out, full, val)
		cred, cage = name, cg
	}
	return out, cred, cage, ""
}

// --- policy -----------------------------------------------------------------------

const maxInspect = 1 << 20

// checkPolicy returns why a request breaks the rule's policy, or "". It may
// replace r.Body with an equivalent reader after peeking at it.
func checkPolicy(rule injectRule, r *http.Request) string {
	readonly := rule.ReadOnly || len(rule.Push) > 0
	if why := checkRepos(rule, r); why != "" {
		return why
	}
	if !readonly {
		return ""
	}
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git-upload-pack"):
		return "" // fetch
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/info/lfs/objects/batch"):
		return checkLFS(r)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/git-receive-pack"):
		if len(rule.Push) == 0 {
			return "read-only: git push isn't allowed"
		}
		return checkPush(rule.Push, r)
	case r.Method == http.MethodPost && (p == "/graphql" || p == "/api/graphql"):
		return checkGraphQL(r)
	}
	return "read-only: " + r.Method + " " + p + " isn't allowed"
}

// checkRepos limits GitHub-style paths to the listed owner/repo.
func checkRepos(rule injectRule, r *http.Request) string {
	if len(rule.Repos) == 0 {
		return ""
	}
	p := path.Clean("/" + r.URL.Path)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	var owner, repo string
	switch {
	case len(parts) >= 3 && parts[0] == "repos":
		owner, repo = parts[1], parts[2]
	case len(parts) >= 2 && (strings.HasSuffix(parts[1], ".git") || len(parts) >= 3 && (parts[2] == "info" || parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack")):
		owner, repo = parts[0], parts[1]
	default:
		// Not a repository path (user, rate_limit, graphql, web pages).
		// GraphQL can't be narrowed by path; a fine-grained token should.
		return ""
	}
	repo = strings.TrimSuffix(repo, ".git")
	for _, want := range rule.Repos {
		if strings.EqualFold(want, owner+"/"+repo) {
			return ""
		}
	}
	return "repository " + owner + "/" + repo + " isn't in this credential's repos"
}

// peekBody reads up to maxInspect bytes of the body (gunzipped if needed)
// and restores r.Body to the original stream.
func peekBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInspect+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxInspect {
		return nil, errors.New("request body too large to check")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(zr, maxInspect))
	}
	return raw, nil
}

// checkGraphQL allows queries, not mutations. Conservative: any
// "mutation" keyword outside a string literal is refused.
func checkGraphQL(r *http.Request) string {
	body, err := peekBody(r)
	if err != nil {
		return "read-only: " + err.Error()
	}
	var q struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(body, &q) != nil {
		return "read-only: GraphQL body isn't a single JSON query"
	}
	if hasMutation(q.Query) {
		return "read-only: GraphQL mutations aren't allowed"
	}
	return ""
}

func hasMutation(q string) bool {
	var b strings.Builder
	inStr, comment := false, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case comment:
			if c == '\n' {
				comment = false
			}
			continue
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		case c == '#':
			comment = true
			continue
		case c == '"':
			inStr = true
			continue
		}
		b.WriteByte(c)
	}
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])mutation([^A-Za-z0-9_]|$)`).MatchString(b.String())
}

func checkLFS(r *http.Request) string {
	body, err := peekBody(r)
	if err != nil {
		return "read-only: " + err.Error()
	}
	var b struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(body, &b) != nil || b.Operation != "download" {
		return "read-only: LFS uploads aren't allowed"
	}
	return ""
}

// checkPush reads git's receive-pack commands (pkt-lines before the pack)
// and allows only refs matching the patterns.
func checkPush(patterns []string, r *http.Request) string {
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		return "push: compressed push requests aren't supported"
	}
	br := bufio.NewReaderSize(r.Body, 64*1024)
	var head bytes.Buffer
	var refs []string
	for {
		var lenHex [4]byte
		if _, err := io.ReadFull(br, lenHex[:]); err != nil {
			return "push: can't read the ref updates"
		}
		head.Write(lenHex[:])
		var n int
		if _, err := fmt.Sscanf(string(lenHex[:]), "%04x", &n); err != nil {
			return "push: malformed request"
		}
		if n == 0 {
			break // flush: commands done, pack follows
		}
		if n < 4 || n > 65520 {
			return "push: malformed request"
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return "push: can't read the ref updates"
		}
		head.Write(line)
		s := string(line)
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i] // capabilities follow the first command
		}
		s = strings.TrimRight(s, "\n")
		if strings.HasPrefix(s, "shallow ") || strings.HasPrefix(s, "push-cert") {
			if strings.HasPrefix(s, "push-cert") {
				return "push: signed pushes aren't supported"
			}
			continue
		}
		f := strings.Fields(s)
		if len(f) != 3 {
			return "push: malformed ref update"
		}
		refs = append(refs, f[2])
	}
	if len(refs) == 0 {
		return "push: no ref updates"
	}
	for _, ref := range refs {
		if !refAllowed(patterns, ref) {
			return "push to " + ref + " isn't allowed (allowed: " + strings.Join(patterns, ", ") + ")"
		}
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head.Bytes()), br), r.Body}
	return ""
}

func refAllowed(patterns []string, ref string) bool {
	for _, p := range patterns {
		full := p
		if !strings.HasPrefix(p, "refs/") {
			full = "refs/heads/" + p
		}
		if globMatch(full, ref) {
			return true
		}
	}
	return false
}

// globMatch is path.Match where a trailing "/*" also matches deeper names
// ("agent/*" covers agent/fix/x).
func globMatch(pattern, name string) bool {
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	if pre, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(name, pre+"/") && !strings.Contains(pre, "*")
	}
	return false
}

// --- upstream ----------------------------------------------------------------------

// publicDialer resolves and refuses private, loopback, link-local and
// CGNAT addresses, like squid's zoo_private ACL, and dials what it checked.
func publicDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	d.Timeout = 10 * time.Second
	var last error = fmt.Errorf("%s has no public address", host)
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			continue
		}
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

var privateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3", "::1/128", "64:ff9b::/96", "fc00::/7", "fe80::/10", "ff00::/8", "::/128"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func publicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// --- secrets from zood -------------------------------------------------------------

// bridgeSecrets asks zood for a placeholder's value over the host bridge
// (the same token-authenticated channel host services use) and caches
// answers briefly so a revoke takes effect within a minute.
func bridgeSecrets(g *gate) secretSource {
	type cached struct {
		val, reason string
		ok          bool
		at          time.Time
	}
	var mu sync.Mutex
	cache := map[string]cached{}
	return func(ph string) (string, string, bool) {
		mu.Lock()
		if c, hit := cache[ph]; hit && time.Since(c.at) < time.Minute {
			mu.Unlock()
			return c.val, c.reason, c.ok
		}
		mu.Unlock()
		val, reason, ok := fetchSecret(g.cfg.Load(), ph)
		if ok || reason != "" {
			mu.Lock()
			cache[ph] = cached{val, reason, ok, time.Now()}
			mu.Unlock()
		}
		return val, reason, ok
	}
}

func fetchSecret(c *config, ph string) (string, string, bool) {
	if c == nil || c.Bridge == "" {
		return "", "", false
	}
	conn, err := net.DialTimeout("tcp", c.Bridge, 5*time.Second)
	if err != nil {
		return "", "", false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "ZOO1 %s @cred %s\n", c.Token, ph); err != nil {
		return "", "", false
	}
	line, err := bufio.NewReaderSize(conn, 64*1024).ReadString('\n')
	if err != nil {
		return "", "", false
	}
	line = strings.TrimRight(line, "\r\n")
	if v, ok := strings.CutPrefix(line, "OK "); ok {
		dec, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return "", "bad answer from zood", false
		}
		return string(dec), "", true
	}
	return "", strings.TrimPrefix(strings.TrimPrefix(line, "ERR "), "NO "), false
}

func (in *injector) write(e injectEntry) {
	b, _ := json.Marshal(e)
	in.logMu.Lock()
	defer in.logMu.Unlock()
	_, _ = in.logw.Write(append(b, '\n'))
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// runInjector starts the injector next to the gate and reloads its config
// when inject.json changes.
func runInjector(g *gate, confPath, logPath string) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("zoo-inject: %v", err)
		return
	}
	in := newInjector(bridgeSecrets(g), f)
	var ln net.Listener
	var lastMod time.Time
	var lastSize int64 = -1
	for {
		if st, err := os.Stat(confPath); err == nil && (st.ModTime() != lastMod || st.Size() != lastSize) {
			lastMod, lastSize = st.ModTime(), st.Size()
			if err := in.load(confPath); err != nil {
				log.Printf("zoo-inject: %v", err)
			} else if ln == nil {
				addr := in.cfg.Load().Listen
				if addr == "" {
					addr = "127.0.0.1:3129"
				}
				if l, err := net.Listen("tcp", addr); err != nil {
					log.Printf("zoo-inject: listen %s: %v", addr, err)
				} else {
					ln = l
					go in.serve(ln)
				}
			}
		}
		time.Sleep(time.Second)
	}
}

// pemCert is used by tests.
func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
