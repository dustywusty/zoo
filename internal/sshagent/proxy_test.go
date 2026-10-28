package sshagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// --- fixtures ---------------------------------------------------------------

// shortDir returns a temp dir with a short path (macOS limits unix socket
// paths to 104 bytes and t.TempDir() is long).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "zsa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type testKey struct {
	comment string
	priv    ed25519.PrivateKey
	pub     ssh.PublicKey
	fp      string
}

type upstream struct {
	sock string
	kr   agent.Agent
	keys map[string]testKey // by comment
}

// newUpstream serves an in-memory keyring with keys "github" and "prod" on a
// unix socket, standing in for Bitwarden/1Password/ssh-agent.
func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{kr: agent.NewKeyring(), keys: map[string]testKey{}}
	for _, c := range []string{"github", "prod"} {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.kr.Add(agent.AddedKey{PrivateKey: priv, Comment: c}); err != nil {
			t.Fatal(err)
		}
		sp, _ := ssh.NewPublicKey(pub)
		u.keys[c] = testKey{c, priv, sp, ssh.FingerprintSHA256(sp)}
	}
	u.sock = filepath.Join(shortDir(t), "up.sock")
	ln, err := net.Listen("unix", u.sock)
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
			go func() { defer c.Close(); _ = agent.ServeAgent(u.kr, c) }()
		}
	}()
	return u
}

type eventLog struct {
	mu sync.Mutex
	ev []Event
}

func (l *eventLog) add(e Event) { l.mu.Lock(); l.ev = append(l.ev, e); l.mu.Unlock() }
func (l *eventLog) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.ev...)
}
func (l *eventLog) last(op string) Event {
	ev := l.all()
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].Op == op {
			return ev[i]
		}
	}
	return Event{}
}

// startProxy serves p for cage "c1" with pol on a unix socket and returns
// the socket path.
func startProxy(t *testing.T, p *Proxy, pol Policy) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "px.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.ServeListener(ctx, ln, "c1", pol) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func dialAgent(t *testing.T, sock string) agent.ExtendedAgent {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return agent.NewClient(c)
}

func setup(t *testing.T, pol Policy, known HostKeys) (*upstream, *Proxy, *eventLog, agent.ExtendedAgent, string) {
	u := newUpstream(t)
	log := &eventLog{}
	p := &Proxy{Upstream: u.sock, Known: known, Log: log.add}
	sock := startProxy(t, p, pol)
	return u, p, log, dialAgent(t, sock), sock
}

func comments(keys []*agent.Key) []string {
	var out []string
	for _, k := range keys {
		out = append(out, k.Comment)
	}
	return out
}

// --- session-bind helpers -----------------------------------------------------

type hostKey struct {
	signer ssh.Signer
	fp     string
}

func newHostKey(t *testing.T) hostKey {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return hostKey{s, ssh.FingerprintSHA256(s.PublicKey())}
}

func bindMsg(t *testing.T, hk ssh.PublicKey, signer ssh.Signer, sid []byte, fwd bool) []byte {
	t.Helper()
	sig, err := signer.Sign(rand.Reader, sid)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.Marshal(struct {
		HostKey, SessionID, Sig []byte
		Fwd                     bool
	}{hk.Marshal(), sid, ssh.Marshal(sig), fwd})
}

func userAuthData(sid []byte, key ssh.PublicKey, method string, hostKey []byte) []byte {
	b := ssh.Marshal(struct {
		SID                   []byte
		Type                  byte
		User, Service, Method string
		HasSig                bool
		Alg                   string
		PK                    []byte
	}{sid, 50, "git", "ssh-connection", method, true, key.Type(), key.Marshal()})
	if hostKey != nil {
		b = append(b, ssh.Marshal(struct{ HK []byte }{hostKey})...)
	}
	return b
}

func randSID() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

// --- tests --------------------------------------------------------------------

func TestListFiltering(t *testing.T) {
	u := newUpstream(t)
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"all", nil, "github,prod"},
		{"by comment", []string{"github"}, "github"},
		{"by fingerprint", []string{u.keys["prod"].fp}, "prod"},
		{"none match", []string{"nope"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			c := dialAgent(t, startProxy(t, &Proxy{Upstream: u.sock, Log: log.add}, Policy{Keys: tc.keys}))
			keys, err := c.List()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(comments(keys), ","); got != tc.want {
				t.Fatalf("list = %q, want %q", got, tc.want)
			}
			e := log.last("list")
			if e.Cage != "c1" || e.Verdict != "allowed" || e.Key != tc.want || e.Time.IsZero() {
				t.Fatalf("event = %+v", e)
			}
		})
	}
}

func TestSignAllowedAndDisallowedKey(t *testing.T) {
	u, _, log, c, _ := setup(t, Policy{Keys: []string{"github"}}, nil)
	data := []byte("hello")
	sig, err := c.Sign(u.keys["github"].pub, data)
	if err != nil {
		t.Fatalf("sign allowed key: %v", err)
	}
	if err := u.keys["github"].pub.Verify(data, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	if e := log.last("sign"); e.Verdict != "allowed" || e.Key != "github" || e.Fingerprint != u.keys["github"].fp {
		t.Fatalf("event = %+v", e)
	}
	if _, err := c.Sign(u.keys["prod"].pub, data); err == nil {
		t.Fatal("sign with disallowed key succeeded")
	}
	if e := log.last("sign"); e.Verdict != "denied" || e.Fingerprint != u.keys["prod"].fp || !strings.Contains(e.Reason, "not allowed") {
		t.Fatalf("event = %+v", e)
	}
	// A key the upstream doesn't have at all.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	op, _ := ssh.NewPublicKey(other.Public())
	if _, err := c.Sign(op, data); err == nil {
		t.Fatal("sign with unknown key succeeded")
	}
}

func TestSessionBind(t *testing.T) {
	hk := newHostKey(t)
	other := newHostKey(t)
	known := Static(map[string][]string{hk.fp: {"github.com"}, other.fp: {"evil.example"}})

	type step func(t *testing.T, u *upstream, c agent.ExtendedAgent) error
	bindAndSign := func(bindHK ssh.PublicKey, bindSigner ssh.Signer, fwd bool, signSID func(sid []byte) []byte, method string, authHK []byte) step {
		return func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			sid := randSID()
			if _, err := c.Extension(sessionBindExt, bindMsg(t, bindHK, bindSigner, sid, fwd)); err != nil {
				t.Logf("bind: %v", err)
			}
			k := u.keys["github"].pub
			_, err := c.Sign(k, userAuthData(signSID(sid), k, method, authHK))
			return err
		}
	}
	same := func(sid []byte) []byte { return sid }
	pk := hk.signer.PublicKey()

	for _, tc := range []struct {
		name   string
		hosts  []string
		step   step
		ok     bool
		reason string
	}{
		{"allowed host", []string{"github.com"}, bindAndSign(pk, hk.signer, false, same, "publickey", nil), true, ""},
		{"allowed host, no host restriction", nil, bindAndSign(pk, hk.signer, false, same, "publickey", nil), true, ""},
		{"hostbound method", []string{"github.com"}, bindAndSign(pk, hk.signer, false, same, "publickey-hostbound-v00@openssh.com", pk.Marshal()), true, ""},
		{"hostbound wrong host key", []string{"github.com"}, bindAndSign(pk, hk.signer, false, same, "publickey-hostbound-v00@openssh.com", other.signer.PublicKey().Marshal()), false, "host key does not match"},
		{"wrong host", []string{"github.com"}, bindAndSign(other.signer.PublicKey(), other.signer, false, same, "publickey", nil), false, "not an allowed host"},
		{"unknown host key", []string{"github.com"}, func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			x := newHostKey(t)
			return bindAndSign(x.signer.PublicKey(), x.signer, false, same, "publickey", nil)(t, u, c)
		}, false, "not a known host"},
		{"missing bind", []string{"github.com"}, func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			k := u.keys["github"].pub
			_, err := c.Sign(k, userAuthData(randSID(), k, "publickey", nil))
			return err
		}, false, "no session-bind"},
		{"forged bind signature", []string{"github.com"}, bindAndSign(pk, other.signer, false, same, "publickey", nil), false, "does not verify"},
		{"forged bind, no host restriction", nil, bindAndSign(pk, other.signer, false, same, "publickey", nil), false, "does not verify"},
		{"is_forwarding", nil, bindAndSign(pk, hk.signer, true, same, "publickey", nil), false, "forwarding"},
		{"session id mismatch", []string{"github.com"}, bindAndSign(pk, hk.signer, false, func([]byte) []byte { return randSID() }, "publickey", nil), false, "session id does not match"},
		{"not userauth data after bind", nil, func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			sid := randSID()
			if _, err := c.Extension(sessionBindExt, bindMsg(t, pk, hk.signer, sid, false)); err != nil {
				t.Fatal(err)
			}
			_, err := c.Sign(u.keys["github"].pub, []byte("SSHSIG arbitrary"))
			return err
		}, false, "not an SSH userauth"},
		{"sign data names other key", nil, func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			sid := randSID()
			if _, err := c.Extension(sessionBindExt, bindMsg(t, pk, hk.signer, sid, false)); err != nil {
				t.Fatal(err)
			}
			_, err := c.Sign(u.keys["github"].pub, userAuthData(sid, u.keys["prod"].pub, "publickey", nil))
			return err
		}, false, "different key"},
		{"second bind poisons", []string{"github.com", "evil.example"}, func(t *testing.T, u *upstream, c agent.ExtendedAgent) error {
			sid := randSID()
			if _, err := c.Extension(sessionBindExt, bindMsg(t, pk, hk.signer, sid, false)); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Extension(sessionBindExt, bindMsg(t, other.signer.PublicKey(), other.signer, randSID(), false)); err == nil {
				t.Fatal("second bind accepted")
			}
			k := u.keys["github"].pub
			_, err := c.Sign(k, userAuthData(sid, k, "publickey", nil))
			return err
		}, false, "already bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _, log, c, _ := setup(t, Policy{Hosts: tc.hosts}, known)
			err := tc.step(t, u, c)
			e := log.last("sign")
			if tc.ok {
				if err != nil {
					t.Fatalf("sign denied: %v (event %+v)", err, e)
				}
				if e.Verdict != "allowed" || e.DestHost != "github.com" || e.HostKey != hk.fp {
					t.Fatalf("event = %+v", e)
				}
				return
			}
			if err == nil {
				t.Fatalf("sign allowed, want denied (event %+v)", e)
			}
			if e.Verdict != "denied" || !strings.Contains(e.Reason, tc.reason) {
				t.Fatalf("event = %+v, want reason containing %q", e, tc.reason)
			}
		})
	}
}

func TestApproval(t *testing.T) {
	u := newUpstream(t)
	k := u.keys["github"].pub
	for _, tc := range []struct {
		name    string
		approve func(ctx context.Context, e Event) bool
		ok      bool
	}{
		{"approved", func(ctx context.Context, e Event) bool { return e.Key == "github" && e.Cage == "c1" }, true},
		{"rejected", func(context.Context, Event) bool { return false }, false},
		{"nil approver", nil, false},
		{"timeout honoring ctx", func(ctx context.Context, e Event) bool { <-ctx.Done(); return true }, false},
		{"timeout ignoring ctx", func(ctx context.Context, e Event) bool { time.Sleep(time.Second); return true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &eventLog{}
			p := &Proxy{Upstream: u.sock, Log: log.add, Approve: tc.approve, ApproveTimeout: 50 * time.Millisecond}
			c := dialAgent(t, startProxy(t, p, Policy{Approve: true}))
			_, err := c.Sign(k, []byte("x"))
			if (err == nil) != tc.ok {
				t.Fatalf("sign err = %v, want ok=%v", err, tc.ok)
			}
			e := log.last("sign")
			if !tc.ok && (e.Verdict != "denied" || e.Reason != "not approved") {
				t.Fatalf("event = %+v", e)
			}
		})
	}
}

func TestMutationsDenied(t *testing.T) {
	u, _, log, c, _ := setup(t, Policy{}, nil)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := c.Add(agent.AddedKey{PrivateKey: priv, Comment: "planted"}); err == nil {
		t.Error("add succeeded")
	}
	if err := c.Remove(u.keys["github"].pub); err == nil {
		t.Error("remove succeeded")
	}
	if err := c.RemoveAll(); err == nil {
		t.Error("remove all succeeded")
	}
	if err := c.Lock([]byte("pw")); err == nil {
		t.Error("lock succeeded")
	}
	if err := c.Unlock([]byte("pw")); err == nil {
		t.Error("unlock succeeded")
	}
	if _, err := c.Extension("evil@example.com", []byte("x")); !errors.Is(err, agent.ErrExtensionUnsupported) {
		t.Errorf("unknown extension: %v", err)
	}
	if _, err := c.Extension("query", nil); err == nil {
		t.Error("query succeeded")
	}
	keys, err := u.kr.List()
	if err != nil || len(keys) != 2 {
		t.Fatalf("upstream changed: %v %v", comments(keys), err)
	}
	for _, k := range keys {
		if k.Comment == "planted" {
			t.Fatal("key planted upstream")
		}
	}
	// The proxy still works after the refusals (connection not wedged).
	if _, err := c.Sign(u.keys["github"].pub, []byte("x")); err != nil {
		t.Fatal(err)
	}
	var others int
	for _, e := range log.all() {
		if e.Op == "other" {
			others++
			if e.Verdict != "denied" {
				t.Errorf("event = %+v", e)
			}
		}
	}
	if others != 6 { // add, remove, remove all, lock, unlock, unknown extension
		t.Errorf("other events = %d, want 6: %+v", others, log.all())
	}
}

func TestEventsCarryNoSecrets(t *testing.T) {
	hk := newHostKey(t)
	u, _, log, c, _ := setup(t, Policy{Hosts: []string{"github.com"}}, Static(map[string][]string{hk.fp: {"github.com"}}))
	k := u.keys["github"].pub
	sid := randSID()
	bind := bindMsg(t, hk.signer.PublicKey(), hk.signer, sid, false)
	if _, err := c.Extension(sessionBindExt, bind); err != nil {
		t.Fatal(err)
	}
	data := userAuthData(sid, k, "publickey", nil)
	sig, err := c.Sign(k, data)
	if err != nil {
		t.Fatal(err)
	}
	c.Sign(u.keys["prod"].pub, data)
	c.List()

	var secrets [][]byte
	for _, tk := range u.keys {
		secrets = append(secrets, tk.priv, tk.priv.Seed(), tk.pub.Marshal())
	}
	secrets = append(secrets, sig.Blob, ssh.Marshal(sig), sid, hk.signer.PublicKey().Marshal())
	var dump bytes.Buffer
	for _, e := range log.all() {
		fmt.Fprintf(&dump, "%#v\n", e)
	}
	for _, s := range secrets {
		for _, enc := range []string{string(s), base64.StdEncoding.EncodeToString(s), base64.RawStdEncoding.EncodeToString(s), hex.EncodeToString(s), fmt.Sprintf("%v", s)} {
			if strings.Contains(dump.String(), enc) {
				t.Fatalf("event log contains secret material %q:\n%s", enc, dump.String())
			}
		}
	}
	if len(log.all()) < 3 {
		t.Fatalf("too few events: %s", dump.String())
	}
}

func TestContextCancelClosesConnection(t *testing.T) {
	u := newUpstream(t)
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- (&Proxy{Upstream: u.sock}).Serve(ctx, a, "c1", Policy{}) }()
	c := agent.NewClient(b)
	if _, err := c.List(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := c.List(); err == nil {
		t.Fatal("connection still open after cancel")
	}
}

func TestUpstreamMissing(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	err := (&Proxy{Upstream: "/nonexistent/agent.sock"}).Serve(context.Background(), a, "c1", Policy{})
	if err == nil {
		t.Fatal("Serve with missing upstream succeeded")
	}
	if _, err := b.Write([]byte{0}); err == nil {
		t.Fatal("client conn not closed")
	}
}

// --- in-process SSH server ----------------------------------------------------

// sshServer runs an x/crypto SSH server on 127.0.0.1 that accepts publickey
// auth for `allowed` and answers any exec with exit status 0.
func sshServer(t *testing.T, hk ssh.Signer, allowed ssh.PublicKey) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != "session" {
						nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					ch, chReqs, err := nch.Accept()
					if err != nil {
						continue
					}
					go func() {
						for r := range chReqs {
							if r.Type == "exec" || r.Type == "shell" {
								r.Reply(true, nil)
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{0}))
								ch.Close()
								return
							}
							r.Reply(false, nil)
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestGoSSHClientThroughProxy runs a full x/crypto SSH handshake using the
// proxy as the client's agent. x/crypto's client does not send
// session-bind, so this only works without a host restriction.
func TestGoSSHClientThroughProxy(t *testing.T) {
	hk := newHostKey(t)
	u := newUpstream(t)
	addr := sshServer(t, hk.signer, u.keys["github"].pub)

	dialSSH := func(pol Policy) (*eventLog, error) {
		log := &eventLog{}
		p := &Proxy{Upstream: u.sock, Known: Static(map[string][]string{hk.fp: {"github.com"}}), Log: log.add}
		c := dialAgent(t, startProxy(t, p, pol))
		cl, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
			User:            "git",
			Auth:            []ssh.AuthMethod{ssh.PublicKeysCallback(c.Signers)},
			HostKeyCallback: ssh.FixedHostKey(hk.signer.PublicKey()),
			Timeout:         5 * time.Second,
		})
		if err == nil {
			cl.Close()
		}
		return log, err
	}

	log, err := dialSSH(Policy{Keys: []string{"github"}})
	if err != nil {
		t.Fatalf("handshake via proxy: %v", err)
	}
	e := log.last("sign")
	if e.Verdict != "allowed" || e.Key != "github" {
		t.Fatalf("event = %+v", e)
	}
	t.Logf("x/crypto client sent session-bind: %v", e.HostKey != "")

	if _, err := dialSSH(Policy{Hosts: []string{"github.com"}}); err == nil {
		t.Fatal("handshake without session-bind allowed under a host restriction")
	}
}

// TestOpenSSHClientSendsSessionBind points the real OpenSSH client at the
// proxy and an in-process server: the most realistic check that the
// destination restriction works end to end.
func TestOpenSSHClientSendsSessionBind(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh binary")
	}
	hk := newHostKey(t)
	u := newUpstream(t)
	addr := sshServer(t, hk.signer, u.keys["github"].pub)
	host, port, _ := net.SplitHostPort(addr)

	dir := shortDir(t)
	knownHosts := filepath.Join(dir, "known_hosts")
	line := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hk.signer.PublicKey()))
	if err := os.WriteFile(knownHosts, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(idFile, ssh.MarshalAuthorizedKey(u.keys["github"].pub), 0o600); err != nil {
		t.Fatal(err)
	}
	// Known hosts parsed from the same file the client trusts.
	known, err := KnownHostsFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if got := known.Hosts(hk.fp); len(got) != 1 || got[0] != host {
		t.Fatalf("known hosts = %v", got)
	}

	runSSH := func(pol Policy) (*eventLog, string, error) {
		log := &eventLog{}
		sock := startProxy(t, &Proxy{Upstream: u.sock, Known: known, Log: log.add}, pol)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, sshBin, "-F", "/dev/null",
			"-o", "BatchMode=yes",
			"-o", "IdentityAgent="+sock,
			"-o", "IdentityFile="+idFile,
			"-o", "IdentitiesOnly=yes",
			"-o", "UserKnownHostsFile="+knownHosts,
			"-o", "GlobalKnownHostsFile=/dev/null",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UpdateHostKeys=no",
			"-o", "PasswordAuthentication=no",
			"-o", "KbdInteractiveAuthentication=no",
			"-o", "ConnectTimeout=5",
			"-p", port, "git@"+host, "true")
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		return log, string(out), err
	}

	log, out, err := runSSH(Policy{Keys: []string{"github"}, Hosts: []string{host}})
	if err != nil {
		t.Fatalf("ssh via proxy failed: %v\n%s\nevents: %+v", err, out, log.all())
	}
	e := log.last("sign")
	if e.Verdict != "allowed" || e.HostKey != hk.fp || e.DestHost != host || e.Key != "github" {
		t.Fatalf("sign event = %+v (OpenSSH should have sent session-bind)", e)
	}

	log, out, err = runSSH(Policy{Keys: []string{"github"}, Hosts: []string{"github.com"}})
	if err == nil {
		t.Fatalf("ssh to a disallowed host succeeded\n%s", out)
	}
	e = log.last("sign")
	if e.Verdict != "denied" || e.HostKey != hk.fp || !strings.Contains(e.Reason, "not an allowed host") {
		t.Fatalf("sign event = %+v", e)
	}
}
