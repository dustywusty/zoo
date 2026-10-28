#!/usr/bin/env python3
"""Dev server for the zoo dashboard. Stdlib only; no daemon, no cages.

    python3 internal/daemon/web/dev/serve.py [--port 8787] [--big]

Serves ../index.html (token replaced, same CSP as zood), GET /api/state from
../fixture.json with every timestamp shifted so the fixture looks "live",
and GET /api/stream with a trickle of state/net/conn/sign/pending/alert
events. Writes (stop, allow, grants, pending) are checked for X-Zoo-Token,
applied to the in-memory state, and printed. --big pads the feed to 5000
events to check rendering speed.
"""
import argparse
import copy
import datetime as dt
import json
import os
import queue
import random
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
WEB = os.path.dirname(HERE)
TOKEN = "dev-token"
CSP = "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"
TIME_KEYS = {"time", "started", "expires", "now"}


def parse(ts):
    return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def fmt(d):
    return d.astimezone(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def now():
    return dt.datetime.now(dt.timezone.utc)


def shift(v, delta):
    if isinstance(v, dict):
        return {k: (fmt(parse(x) + delta) if k in TIME_KEYS and isinstance(x, str) and x else shift(x, delta)) for k, x in v.items()}
    if isinstance(v, list):
        return [shift(x, delta) for x in v]
    return v


with open(os.path.join(WEB, "fixture.json")) as f:
    FIX = json.load(f)
DB = shift(FIX, now() - parse(FIX["anchor"]))
LOCK = threading.Lock()
SUBS = []
IDS = {"net": 10_000, "conn": 100, "sign": 100, "alert": 1000, "pending": 0}


def next_id(kind):
    IDS[kind] += 1
    return IDS[kind]


def publish(kind, data):
    msg = f"event: {kind}\ndata: {json.dumps(data)}\n\n".encode()
    with LOCK:
        for q in SUBS:
            q.put(msg)


def snapshot():
    with LOCK:
        s = copy.deepcopy(DB["state"])
    s["now"] = fmt(now())
    for c in s["cages"]:
        st = c.get("stat")
        if c.get("running") and st:
            cpus = float(c.get("cpus") or 1)
            st["cpu_percent"] = max(1.0, min(cpus * 100, st["cpu_percent"] * random.uniform(0.75, 1.25)))
            st["mem_used"] = max(0, min(st["mem_limit"], st["mem_used"] * random.uniform(0.97, 1.03)))
            st["net_rx_rate"] = st["net_rx_rate"] * random.uniform(0.2, 1.8)
            st["net_tx_rate"] = st["net_tx_rate"] * random.uniform(0.2, 1.8)
    return s


def pad_big():
    base = DB["events"]
    out = []
    t0 = now() - dt.timedelta(hours=3)
    for i in range(5000 - len(base)):
        e = copy.deepcopy(random.choice(base))
        e["id"] = i + 1
        e["first_seen"] = False
        e["time"] = fmt(t0 + dt.timedelta(seconds=i * 2))
        out.append(e)
    for i, e in enumerate(base):
        e["id"] = 5000 + i
    DB["events"] = out + base
    IDS["net"] = 20_000


def fake_net():
    s = DB["state"]
    running = [c for c in s["cages"] if c.get("running")]
    if not running:
        return None
    c = random.choice(running)
    host = random.choice(["api.anthropic.com", "registry.npmjs.org", "proxy.golang.org", "github.com",
                          "objects.githubusercontent.com", "sentry.io", "api.segment.io"])
    denied = host in ("sentry.io", "api.segment.io")
    first = host == "api.segment.io" and random.random() < 0.5
    return {"id": next_id("net"), "time": fmt(now()), "client": c.get("ip", ""), "method": "CONNECT",
            "url": host + ":443", "host": host, "status": 403 if denied else 200,
            "squid": "TCP_DENIED" if denied else "TCP_TUNNEL", "bytes_in": 0 if denied else random.randint(900, 900_000),
            "bytes_out": random.randint(300, 9000), "ms": 0 if denied else random.randint(20, 1600),
            "denied": denied, "cage": c["name"], "first_seen": first, "allowlisted": not denied, "learn": bool(c.get("learn"))}


def ticker():
    n = 0
    while True:
        time.sleep(1.0)
        n += 1
        if n % 2 == 0:
            publish("state", snapshot())
        if random.random() < 0.7:
            e = fake_net()
            if e:
                with LOCK:
                    DB["events"].append(e)
                publish("net", e)
        if n % 9 == 0:
            ok = random.random() < 0.8
            c = {"id": next_id("conn"), "time": fmt(now()), "cage": "discus", "service": "pg", "target": "127.0.0.1:5432",
                 "verdict": "allowed" if ok else "denied", "bytes_in": random.randint(1000, 90000) if ok else 0,
                 "bytes_out": random.randint(200, 9000) if ok else 0, "ms": random.randint(4, 4000) if ok else 0}
            publish("conn", c)
        if n % 23 == 0:
            p = {"id": f"p-{next_id('pending'):04x}", "time": fmt(now()), "cage": "web-checkout", "service": "ssh-agent",
                 "kind": "ssh-sign", "key": "deploy", "fingerprint": "SHA256:Zt7c0Qe1u9mWbN4pR2kXyL8sVdA6hJfGoE3iT5aYcB0",
                 "dest_host": "github.com", "expires": fmt(now() + dt.timedelta(seconds=30))}
            with LOCK:
                DB["pending"].append(p)
            publish("pending", p)
        if n % 41 == 0:
            publish("alert", {"id": next_id("alert"), "time": fmt(now()), "level": "warn", "cage": "web-checkout",
                              "title": "Blocked request to api.segment.io", "body": "CONNECT api.segment.io:443 was denied by the keeper."})


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send(self, code, body=b"", ctype="application/json", extra=None):
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
        elif isinstance(body, str):
            body = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/":
            with open(os.path.join(WEB, "index.html")) as f:
                html = f.read().replace("__ZOO_TOKEN__", TOKEN, 1)
            return self.send(200, html, "text/html; charset=utf-8", {"Content-Security-Policy": CSP})
        if self.path == "/api/state":
            with LOCK:
                DB["pending"] = [p for p in DB["pending"] if parse(p["expires"]) > now()]
                d = {"state": None, "events": DB["events"][-5000:], "alerts": DB["alerts"],
                     "conns": DB["conns"], "signs": DB["signs"], "pending": DB["pending"]}
                d = copy.deepcopy(d)
            d["state"] = snapshot()
            return self.send(200, d)
        if self.path == "/api/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            q = queue.Queue()
            with LOCK:
                SUBS.append(q)
            try:
                self.wfile.write(b": hello\n\n")
                self.wfile.flush()
                while True:
                    try:
                        msg = q.get(timeout=15)
                    except queue.Empty:
                        msg = b": ping\n\n"
                    self.wfile.write(msg)
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                with LOCK:
                    SUBS.remove(q)
            return
        self.send(404, "not found", "text/plain")

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            return json.loads(raw or b"{}")
        except ValueError:
            return {}

    def write(self):
        body = self.body()
        print(f"{self.command} {self.path} {json.dumps(body)}", flush=True)
        if self.headers.get("X-Zoo-Token") != TOKEN:
            return self.send(403, "missing or bad token", "text/plain")
        s = DB["state"]
        m = re.fullmatch(r"/api/cages/([^/]+)/stop", self.path)
        if self.command == "POST" and m:
            with LOCK:
                for c in s["cages"]:
                    if c["id"] == m.group(1):
                        c.update(running=False, status="stopped", stat=None, sessions=None)
            publish("state", snapshot())
            return self.send(200, {"stopped": m.group(1)})
        if self.command == "POST" and self.path == "/api/allow":
            with LOCK:
                s["allowlist"].append(body.get("host", ""))
            return self.send(200, {"added": [body.get("host")]})
        if self.path == "/api/grants" and self.command in ("POST", "DELETE"):
            with LOCK:
                for c in s["cages"]:
                    if c["name"] == body.get("cage"):
                        g = [x for x in (c.get("grants") or []) if x != body.get("service")]
                        if self.command == "POST":
                            g.append(body.get("service"))
                        c["grants"] = g
            publish("state", snapshot())
            return self.send(200, {"ok": True})
        m = re.fullmatch(r"/api/pending/([^/]+)", self.path)
        if self.command == "POST" and m:
            with LOCK:
                DB["pending"] = [p for p in DB["pending"] if p["id"] != m.group(1)]
            publish("pending", {"id": m.group(1), "resolved": True})
            return self.send(200, {"ok": True})
        self.send(404, "not found", "text/plain")

    do_POST = write
    do_DELETE = write


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8787)
    ap.add_argument("--big", action="store_true", help="pad the feed to 5000 events")
    ap.add_argument("--quiet", action="store_true", help="no live events")
    a = ap.parse_args()
    if a.big:
        pad_big()
    if not a.quiet:
        threading.Thread(target=ticker, daemon=True).start()
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), H)
    srv.handle_error = lambda req, addr: None  # browsers drop SSE/keep-alive sockets; not interesting
    srv.daemon_threads = True
    print(f"zoo dashboard dev server: http://127.0.0.1:{a.port}  (token {TOKEN})", file=sys.stderr)
    srv.serve_forever()


if __name__ == "__main__":
    main()
