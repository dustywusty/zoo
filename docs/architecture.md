# Architecture

What can reach what, and how the pieces talk. For the narrative, see [How it works](how-it-works.md); for the reasons behind each choice, the decision log in [BRIEF.md](BRIEF.md#15-decision-log).

## Trust boundaries

Each box is a trust boundary: your machine (trusted), the runtime (an Apple VM per cage, or Docker/Podman containers), the internal networks cages live on (no route out, no DNS), and the keeper, the only thing a cage can reach. Every arrow is the only path between two zones.

```mermaid
flowchart LR
  subgraph mac["Your machine"]
    direction TB
    you(["You: browser on 127.0.0.1:7777<br/>Allow · Grant · Approve · Stop"])
    cli["zoo CLI + shims<br/>creates cages, writes allowlist,<br/>gate and injector config"]
    zood["zood<br/>dashboard · previews · host bridge<br/>SSH-agent proxy · credential values<br/>tails keeper logs, alerts"]
    host[("Host services<br/>127.0.0.1:5432<br/>(stay on loopback)")]
    vault[("Keychain · Credential Manager<br/>Bitwarden · 1Password")]
  end

  subgraph rt["Runtime"]
    subgraph nets["Internal networks"]
      direction TB
      cageA["Cage api-3fa2c1<br/>agent user · all caps dropped<br/>worktree rw · .env empty<br/>.git hooks/config read-only"]
      cageB["Cage web-9b1e07<br/>own network on Docker"]
    end
    subgraph keeper["Keeper"]
      direction TB
      squid["squid :3128<br/>allowlist · private addrs refused<br/>access.log"]
      inject["injector (127.0.0.1:3129)<br/>credential hosts only:<br/>placeholder → token, policy"]
      gate["zoo-gate<br/>one port per service<br/>grants · gate.log"]
    end
    sidecars[("Sidecars on zoo-services<br/>redis · postgres")]
  end

  subgraph net["Internet"]
    allowed(["Allowlisted hosts"])
    credhosts(["Credential hosts<br/>github.com · api.github.com"])
    remote(["Granted remotes<br/>github.com:22"])
    denied(["Everything else:<br/>denied, logged, alert"])
  end

  cageA -- "HTTPS_PROXY" --> squid
  cageB -- "HTTPS_PROXY" --> squid
  cageA -- "name.zoo" --> gate
  squid -- "CONNECT, tunnel" --> allowed
  squid -- "credential hosts" --> inject
  inject -- "TLS, token added" --> credhosts
  squid -. "403" .-> denied
  gate --> remote
  gate --> sidecars
  gate -- "ZOO1 token service cage" --> zood
  inject -- "placeholder → value?" --> zood
  zood --> host
  zood --> vault
  cli --> rt
  you --> zood
  zood -. "previews · ssh door (exec)" .-> cageA
```

No arrow means no path: a cage can't reach the internet directly, DNS, your machine (except Apple's gateway, see below), other cages on Docker, or the dashboard.

## Boundaries

| From → to | Allowed | Enforced by |
|---|---|---|
| cage → internet | never directly | internal network: no default route, no DNS (`--no-dns` / `--dns 127.0.0.1`) |
| cage → web host | allowlisted only | squid `CONNECT` by hostname (`dstdomain -n`, no PTR tricks), ports 80/443 only, private, loopback and metadata addresses refused even for allowlisted names |
| cage → credential host | with policy | the injector: Host must match the CONNECT, `readonly` / `push` / `repos` checked, placeholder swapped for the real token from zood, re-encrypted with normal certificate checks |
| cage → service | granted only | zoo-gate: per-port allow list of cage IPs, checked against the keeper address the connection arrived on; zood re-checks the grant on the bridge |
| cage → your machine | Apple: gateway visible | Docker: unreachable. Apple: anything bound to `0.0.0.0` is reachable ([apple/container#1320](https://github.com/apple/container/issues/1320)); `zoo doctor` reports it. Host services go through the bridge, so they can stay on loopback |
| cage → other cage | Docker: no | Docker: one internal network per cage. Apple: shared network (no live network attach); `zoo doctor --deep` warns |
| cage → host files | worktree only | bind mount at the same path (Windows: `/c/…`), plus `zoo.yaml mounts:`; secrets masked; `.git` hooks, config, worktrees/, modules/ and worktree hook paths read-only; directories pinned against rename; planted repos and moved branches detected on the host |
| cage → `zoo.yaml` / Dockerfile | writes, not effect | zoo uses only the copy you approved (`zoo trust`, `zoo build --project`); Dockerfiles build with an empty context |
| cage → your keys | signatures, not keys | SSH-agent proxy: key filter, `session-bind` host check, optional approval, log; the key never leaves the vault |
| cage → credentials | placeholders | an HMAC-bound placeholder per cage and credential; without `credentials:`/`cred:`, the model token in a 0600 env file, never on argv |
| anything → dashboard | no | loopback bind, Host header check, token on writes; `zoo doctor` checks cages can't reach it |
| you → cage | over exec | previews and the SSH door run through the runtime's exec: no port is opened into or out of a cage |
| cage → its own kernel | depends | Apple: VM per cage. Docker: shared kernel, refused unless `isolation_floor: container`; shown on every card |

## Sequences

### `zoo run claude`

Shims make any orchestrator (Conductor, herdr, Orca, a terminal) land here without knowing about zoo. The cage persists per worktree; sessions come and go.

```mermaid
sequenceDiagram
  participant You as you / orchestrator
  participant CLI as zoo CLI
  participant RT as runtime
  participant K as keeper
  participant C as cage
  You->>CLI: claude (shim) → zoo run claude
  CLI->>CLI: zoo.yaml: use the approved copy (ask if it changed)
  CLI->>RT: ensure keeper (squid, gate, injector), networks
  CLI->>CLI: plan: worktree, mounts, masks, git guard, pins, size, image
  CLI->>RT: create cage: caps dropped, internal net, read-only mounts
  RT-->>C: VM / container up
  CLI->>K: gate.json (grants), inject.json + CA (credential hosts)
  CLI->>C: as guest root: /etc/hosts ← pg.zoo, trust the keeper CA, git credential helper
  CLI->>CLI: snapshot refs, write secrets and placeholders to an env file (0600)
  CLI->>C: exec -u agent --env-file … zoo-session claude
  C-->>You: agent runs, exit code passes through
  CLI->>You: on exit: planted repos? branches moved?
```

### Web request: allowed, denied, approved

Hostname-level control via `CONNECT`; TLS is end to end for every host not under `credentials:`.

```mermaid
sequenceDiagram
  participant A as agent (cage)
  participant S as squid
  participant H as api.anthropic.com
  participant Z as zood
  participant You as you
  A->>S: CONNECT api.anthropic.com:443
  Note over S: port 443 · public address · allowlisted ✓
  S->>H: tunnel (TLS end to end)
  S-->>Z: access.log line
  A->>S: CONNECT evil.example:443
  S-->>A: 403 denied
  S-->>Z: access.log: DENIED
  Z->>You: alert + red row (+ ntfy)
  You->>Z: Allow, for 4h (token)
  Z->>S: allowlist.txt + squid -k reconfigure
```

### Credential: the token stays in the keeper

The cage holds `zoocred.github.api-3fa2c1.<hmac>`. Only for the credential's hosts does the keeper look inside the TLS session, with a CA that can't sign for anything else.

```mermaid
sequenceDiagram
  participant A as gh / git (cage)
  participant S as squid
  participant I as injector
  participant Z as zood
  participant G as api.github.com
  A->>S: CONNECT api.github.com:443
  S->>I: credential host → cache_peer 127.0.0.1:3129
  I-->>A: TLS with a leaf from the name-constrained zoo CA
  A->>I: GET /repos/you/app/actions/runs<br/>Authorization: Bearer zoocred…
  Note over I: Host matches CONNECT ✓ · policy (readonly, repos) ✓
  I->>Z: ZOO1 token @cred zoocred…
  Note over Z: HMAC ✓ · cage running ✓ · grant held ✓
  Z-->>I: value from Keychain / vault (cached 1 min)
  I->>G: TLS, normal certificate checks · Bearer ghp_…
  G-->>A: response
  A->>I: POST /gists
  I-->>A: 403 zoo keeper: read-only
  A->>I: git push main
  I-->>A: ! [remote rejected] main (not in push: agent/*)
```

### Granted service: `pg.zoo`

The host's Postgres stays on 127.0.0.1. Only the keeper can reach the bridge's address, and only with the token; a cage without the grant is refused at the gate and logged.

```mermaid
sequenceDiagram
  participant A as agent (cage)
  participant G as zoo-gate
  participant B as zood bridge
  participant P as postgres :5432
  A->>G: connect pg.zoo:5432 (→ keeper)
  Note over G: source IP holds the grant, on its own network ✓
  G->>B: ZOO1 token pg api-3fa2c1
  Note over B: token ✓ · grant ✓ (re-check)
  B->>P: dial 127.0.0.1:5432
  B-->>G: OK
  A->>P: bytes spliced both ways
  G-->>B: gate.log: allowed, bytes, ms
  Note over A,G: another cage, no grant: refused, logged, alert
```

### `git push` over SSH with your key

The private key never leaves Bitwarden or 1Password. The proxy only signs for hosts the service allows (checked against the server's host key via `session-bind`), and can ask you first.

```mermaid
sequenceDiagram
  participant A as git / ssh (cage)
  participant G as zoo-gate
  participant H as github.com:22
  participant P as agent proxy (zood)
  participant V as you · vault
  A->>G: ssh github.com → gh-ssh.zoo:20022
  G->>H: TCP (grant ✓, logged)
  A->>P: agent socket (zoo-session relay) → keys.zoo:22022 → bridge
  A->>P: list identities
  P-->>A: only the granted keys
  A->>P: session-bind(github host key) + sign request
  Note over P: host key → github.com ∈ hosts ✓
  P->>V: approve? (dashboard / ntfy)
  V-->>P: Allow · vault signs
  P-->>A: signature (key stays home)
```

### Previews and the SSH door

Neither opens a port. Both run through the runtime's exec, the same channel `zoo run` uses.

```mermaid
sequenceDiagram
  participant B as browser / editor
  participant Z as zood / zoo CLI
  participant RT as runtime exec
  participant C as cage
  B->>Z: http://5173.api-3fa2c1.localhost:7779
  Note over Z: Host names a running cage ✓ (else 404)
  Z->>RT: exec -u agent socat - TCP:127.0.0.1:5173
  RT->>C: dev server
  C-->>B: page, HMR websocket
  B->>Z: ssh zoo-api-3fa2c1 → ProxyCommand zoo ssh-proxy
  Z->>RT: exec -u agent sshd -i (root-owned config, no forwarding back)
  RT->>C: shell / VS Code server as agent
```
