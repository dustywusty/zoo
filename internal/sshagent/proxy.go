package sshagent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Policy says what one cage may do with the upstream agent.
type Policy struct {
	Keys    []string // allowed key fingerprints ("SHA256:...") or comments; empty = all keys from upstream
	Hosts   []string // allowed destination hostnames (e.g. "github.com"); empty = any destination
	Approve bool     // ask Approver before each sign
}

// Event records one agent operation from a cage. It never carries key
// material, data to be signed, or signatures.
type Event struct {
	Time        time.Time
	Cage        string
	Op          string // "list" | "sign" | "other"
	Key         string // key comment (for list: comments of the keys returned, comma-separated)
	Fingerprint string // SHA256 fingerprint of the key (sign only)
	DestHost    string // resolved from session-bind host key, "" if unknown
	HostKey     string // SHA256 fingerprint of the bound server host key, "" if none
	Verdict     string // "allowed" | "denied"
	Reason      string
}

// HostKeys maps a server host key to the hostnames it belongs to.
type HostKeys interface {
	Hosts(fingerprint string) []string
}

// DefaultApproveTimeout bounds how long a sign waits for approval.
const DefaultApproveTimeout = 60 * time.Second

// Proxy is a filtering agent proxy in front of one upstream agent socket.
// A Proxy may serve many connections concurrently.
type Proxy struct {
	Upstream       string // path of the real agent socket
	Known          HostKeys
	Log            func(Event)
	Approve        func(ctx context.Context, e Event) bool // nil = deny when Policy.Approve
	ApproveTimeout time.Duration                           // 0 = DefaultApproveTimeout

	// Dial overrides how the upstream is reached (tests); nil = unix socket at Upstream.
	Dial func(ctx context.Context) (net.Conn, error)
}

// Agent protocol message numbers (draft-miller-ssh-agent).
const (
	msgFailure            = 5
	msgSuccess            = 6
	msgRequestRSAIDs      = 1 // SSH1, obsolete
	msgRemoveAllRSAIDs    = 9 // SSH1, obsolete
	msgRequestIdentities  = 11
	msgIdentitiesAnswer   = 12
	msgSignRequest        = 13
	msgSignResponse       = 14
	msgAddIdentity        = 17
	msgRemoveIdentity     = 18
	msgRemoveAll          = 19
	msgAddSmartcard       = 20
	msgRemoveSmartcard    = 21
	msgLock               = 22
	msgUnlock             = 23
	msgAddIDConstrained   = 25
	msgAddSmartcardConstr = 26
	msgExtension          = 27

	msgUserAuthRequest = 50 // SSH_MSG_USERAUTH_REQUEST

	maxMessage = 256 << 10 // agent requests are small; refuse anything absurd
)

const sessionBindExt = "session-bind@openssh.com"

// ServeListener accepts connections on ln and serves each with Serve until
// ctx is cancelled or ln fails. Per-connection errors are not fatal.
func (p *Proxy) ServeListener(ctx context.Context, ln net.Listener, cage string, pol Policy) error {
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Serve(ctx, c, cage, pol) }()
	}
}

// Serve serves one client connection: it dials its own upstream connection,
// answers requests until the client hangs up or ctx is cancelled, and closes
// both connections. A clean hang-up returns nil.
func (p *Proxy) Serve(ctx context.Context, conn net.Conn, cage string, pol Policy) error {
	defer conn.Close()
	up, err := p.dial(ctx)
	if err != nil {
		return fmt.Errorf("sshagent: upstream: %w", err)
	}
	defer up.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
			up.Close()
		case <-done:
		}
	}()

	s := &session{
		p:    p,
		ctx:  ctx,
		cage: cage,
		pol:  pol,
		up:   agent.NewClient(up),
	}
	for {
		req, err := readMessage(conn)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if err := writeMessage(conn, s.handle(req)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

func (p *Proxy) dial(ctx context.Context) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", p.Upstream)
}

func readMessage(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxMessage {
		return nil, fmt.Errorf("sshagent: bad request length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeMessage(w io.Writer, msg []byte) error {
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out, uint32(len(msg)))
	copy(out[4:], msg)
	_, err := w.Write(out)
	return err
}

// binding is a verified session-bind@openssh.com on this connection.
type binding struct {
	hostKey   []byte // wire blob
	hostFP    string
	sessionID []byte
	hosts     []string
}

// session is the per-connection state.
type session struct {
	p    *Proxy
	ctx  context.Context
	cage string
	pol  Policy
	up   agent.ExtendedAgent

	bind    *binding
	bindErr string // a rejected bind poisons the connection for signing
}

var failure = []byte{msgFailure}

func (s *session) log(e Event) {
	if s.p.Log == nil {
		return
	}
	e.Time = time.Now()
	e.Cage = s.cage
	s.p.Log(e)
}

func (s *session) handle(req []byte) []byte {
	switch req[0] {
	case msgRequestIdentities:
		return s.list()
	case msgSignRequest:
		return s.sign(req)
	case msgExtension:
		return s.extension(req)
	case msgRequestRSAIDs, msgRemoveAllRSAIDs:
		return failure
	case msgAddIdentity, msgAddIDConstrained, msgRemoveIdentity, msgRemoveAll,
		msgAddSmartcard, msgAddSmartcardConstr, msgRemoveSmartcard, msgLock, msgUnlock:
		s.log(Event{Op: "other", Verdict: "denied", Reason: opName(req[0]) + " is not allowed from a cage"})
		return failure
	default:
		s.log(Event{Op: "other", Verdict: "denied", Reason: fmt.Sprintf("unknown request type %d", req[0])})
		return failure
	}
}

func opName(t byte) string {
	switch t {
	case msgAddIdentity, msgAddIDConstrained:
		return "add identity"
	case msgRemoveIdentity:
		return "remove identity"
	case msgRemoveAll:
		return "remove all identities"
	case msgAddSmartcard, msgAddSmartcardConstr:
		return "add smartcard key"
	case msgRemoveSmartcard:
		return "remove smartcard key"
	case msgLock:
		return "lock"
	case msgUnlock:
		return "unlock"
	}
	return fmt.Sprintf("request %d", t)
}

// allowedKeys lists upstream keys filtered by policy.
func (s *session) allowedKeys() (allowed []*agent.Key, total int, err error) {
	keys, err := s.up.List()
	if err != nil {
		return nil, 0, err
	}
	for _, k := range keys {
		if s.keyAllowed(k) {
			allowed = append(allowed, k)
		}
	}
	return allowed, len(keys), nil
}

func (s *session) keyAllowed(k *agent.Key) bool {
	if len(s.pol.Keys) == 0 {
		return true
	}
	fp := ssh.FingerprintSHA256(k)
	for _, want := range s.pol.Keys {
		if want == fp || want == k.Comment {
			return true
		}
	}
	return false
}

func (s *session) list() []byte {
	keys, total, err := s.allowedKeys()
	if err != nil {
		s.log(Event{Op: "list", Verdict: "denied", Reason: "upstream agent: " + err.Error()})
		return failure
	}
	var body []byte
	comments := make([]string, 0, len(keys))
	for _, k := range keys {
		body = append(body, ssh.Marshal(struct {
			Blob    []byte
			Comment string
		}{k.Blob, k.Comment})...)
		comments = append(comments, k.Comment)
	}
	s.log(Event{
		Op:      "list",
		Key:     strings.Join(comments, ","),
		Verdict: "allowed",
		Reason:  fmt.Sprintf("%d of %d keys", len(keys), total),
	})
	out := []byte{msgIdentitiesAnswer, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(keys)))
	return append(out, body...)
}

type signRequest struct {
	KeyBlob []byte `sshtype:"13"`
	Data    []byte
	Flags   uint32
}

func (s *session) sign(req []byte) []byte {
	var r signRequest
	if err := ssh.Unmarshal(req, &r); err != nil {
		s.log(Event{Op: "sign", Verdict: "denied", Reason: "malformed sign request"})
		return failure
	}
	ev := Event{Op: "sign", Cage: s.cage}
	if pk, err := ssh.ParsePublicKey(r.KeyBlob); err == nil {
		ev.Fingerprint = ssh.FingerprintSHA256(pk)
	}
	if s.bind != nil {
		ev.HostKey = s.bind.hostFP
		ev.DestHost = strings.Join(s.bind.hosts, ",")
	}
	deny := func(reason string) []byte {
		ev.Verdict, ev.Reason = "denied", reason
		s.log(ev)
		return failure
	}

	keys, _, err := s.allowedKeys()
	if err != nil {
		return deny("upstream agent: " + err.Error())
	}
	var key *agent.Key
	for _, k := range keys {
		if bytes.Equal(k.Blob, r.KeyBlob) {
			key = k
			break
		}
	}
	if key == nil {
		return deny("key not allowed for this cage")
	}
	ev.Key = key.Comment

	if s.bindErr != "" {
		return deny(s.bindErr)
	}
	if s.bind != nil {
		if reason := checkUserAuth(r.Data, s.bind, r.KeyBlob); reason != "" {
			return deny(reason)
		}
	}
	if len(s.pol.Hosts) > 0 {
		if s.bind == nil {
			return deny("no session-bind: destination unknown (needs OpenSSH 8.9+ client)")
		}
		host, ok := matchHost(s.bind.hosts, s.pol.Hosts)
		if !ok {
			if len(s.bind.hosts) == 0 {
				return deny("destination host key " + s.bind.hostFP + " is not a known host")
			}
			return deny("destination " + strings.Join(s.bind.hosts, ",") + " is not an allowed host")
		}
		ev.DestHost = host
	}

	if s.pol.Approve {
		if !s.approve(ev) {
			return deny("not approved")
		}
	}

	sig, err := s.up.SignWithFlags(key, r.Data, agent.SignatureFlags(r.Flags))
	if err != nil {
		return deny("upstream agent: " + err.Error())
	}
	ev.Verdict, ev.Reason = "allowed", ""
	s.log(ev)
	return ssh.Marshal(struct {
		SigBlob []byte `sshtype:"14"`
	}{ssh.Marshal(sig)})
}

func (s *session) approve(ev Event) bool {
	if s.p.Approve == nil {
		return false
	}
	d := s.p.ApproveTimeout
	if d <= 0 {
		d = DefaultApproveTimeout
	}
	ctx, cancel := context.WithTimeout(s.ctx, d)
	defer cancel()
	ev.Time = time.Now()
	res := make(chan bool, 1)
	go func() { res <- s.p.Approve(ctx, ev) }()
	select {
	case ok := <-res:
		return ok && ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func matchHost(have, allowed []string) (string, bool) {
	for _, h := range have {
		for _, a := range allowed {
			if strings.EqualFold(h, a) {
				return h, true
			}
		}
	}
	return "", false
}

// checkUserAuth verifies that data is an SSH userauth "publickey" request for
// the bound session and the requested key. It returns "" if so, else a reason.
func checkUserAuth(data []byte, b *binding, keyBlob []byte) string {
	rd := reader{b: data}
	sid := rd.str()
	typ := rd.byte()
	_ = rd.str() // user
	_ = rd.str() // service
	method := string(rd.str())
	hasSig := rd.byte()
	_ = rd.str() // algorithm
	pk := rd.str()
	if rd.bad {
		return "sign data is not an SSH userauth request for the bound session"
	}
	if !bytes.Equal(sid, b.sessionID) {
		return "sign data session id does not match session-bind"
	}
	if typ != msgUserAuthRequest || hasSig != 1 {
		return "sign data is not an SSH userauth request"
	}
	switch method {
	case "publickey":
	case "publickey-hostbound-v00@openssh.com":
		hk := rd.str()
		if rd.bad || !bytes.Equal(hk, b.hostKey) {
			return "sign data host key does not match session-bind"
		}
	default:
		return "sign data has unexpected userauth method"
	}
	if len(rd.b) != 0 {
		return "sign data has trailing bytes"
	}
	if !bytes.Equal(pk, keyBlob) {
		return "sign data names a different key"
	}
	return ""
}

type reader struct {
	b   []byte
	bad bool
}

func (r *reader) byte() byte {
	if r.bad || len(r.b) < 1 {
		r.bad = true
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) str() []byte {
	if r.bad || len(r.b) < 4 {
		r.bad = true
		return nil
	}
	n := binary.BigEndian.Uint32(r.b)
	if uint64(n) > uint64(len(r.b)-4) {
		r.bad = true
		return nil
	}
	s := r.b[4 : 4+n]
	r.b = r.b[4+n:]
	return s
}

func (s *session) extension(req []byte) []byte {
	rd := reader{b: req[1:]}
	name := string(rd.str())
	if rd.bad {
		s.log(Event{Op: "other", Verdict: "denied", Reason: "malformed extension request"})
		return failure
	}
	if name != sessionBindExt {
		if name != "query" {
			s.log(Event{Op: "other", Verdict: "denied", Reason: "extension " + quoteName(name) + " is not allowed"})
		}
		return failure
	}
	if err := s.sessionBind(rd.b); err != nil {
		if s.bindErr == "" {
			s.bindErr = err.Error()
		}
		ev := Event{Op: "other", Verdict: "denied", Reason: err.Error()}
		if s.bind != nil {
			ev.HostKey = s.bind.hostFP
		}
		s.log(ev)
		return failure
	}
	// Pass the bind on so an upstream that enforces destination
	// constraints itself (OpenSSH ssh-agent) sees it. Agents that don't
	// know the extension (Bitwarden, 1Password) answer failure; ignore.
	_, _ = s.up.Extension(sessionBindExt, rd.b)
	return []byte{msgSuccess}
}

func quoteName(s string) string {
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return fmt.Sprintf("%q", s)
}

// sessionBind parses and verifies session-bind@openssh.com contents:
// string hostkey, string session id, string signature, bool is_forwarding.
func (s *session) sessionBind(contents []byte) error {
	rd := reader{b: contents}
	hk := rd.str()
	sid := rd.str()
	sigBlob := rd.str()
	fwd := rd.byte()
	if rd.bad || len(rd.b) != 0 || len(sid) == 0 || len(sid) > 64 || fwd > 1 {
		return errors.New("malformed session-bind")
	}
	pk, err := ssh.ParsePublicKey(hk)
	if err != nil {
		return errors.New("session-bind: bad host key")
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(sigBlob, &sig); err != nil {
		return errors.New("session-bind: bad signature encoding")
	}
	fp := ssh.FingerprintSHA256(pk)
	if err := pk.Verify(sid, &sig); err != nil {
		return fmt.Errorf("session-bind: host key %s signature does not verify", fp)
	}
	if fwd == 1 {
		return errors.New("session-bind: agent forwarding is not allowed")
	}
	if s.bind != nil {
		return errors.New("session-bind: connection already bound")
	}
	var hosts []string
	if s.p.Known != nil {
		hosts = s.p.Known.Hosts(fp)
	}
	s.bind = &binding{hostKey: bytes.Clone(hk), hostFP: fp, sessionID: bytes.Clone(sid), hosts: hosts}
	return nil
}
