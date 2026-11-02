# zoo

Run AI coding agents in small VMs ("cages") whose only way out is a logging, allowlisting proxy ("the keeper"), and watch everything on a live dashboard.

- **Cage:** one sandbox per worktree: an Apple `container` VM, or a Docker/Podman container. Your worktree is shared in; nothing else from your machine is.
- **Keeper:** squid in its own small VM or container. Cages sit on an internal network with no internet, so the keeper is the only route out. Default-deny allowlist, every request logged.
- **Dashboard:** what's running, CPU and memory per cage, and a live network feed with search and filters, on `http://127.0.0.1:7777`.

Works with any CLI agent (Claude Code, Codex, pi, opencode, …) and any tool that launches them (Conductor, Orca, herdr, your terminal).

![zoo dashboard](docs/images/dashboard-dark.png)

How it fits together, with the trust boundaries and sequence diagrams: [docs/overview.html](docs/overview.html) (open it locally) or the [architecture diagram](docs/images/architecture.png).

## Requirements

One of these backends:

| Backend | Runs on | Isolation | Notes |
|---|---|---|---|
| `apple` | macOS 26+, Apple silicon, [`container`](https://github.com/apple/container/releases) | `vm`: a VM per cage | Default when installed. |
| `docker` | OrbStack, Docker Desktop, colima, Docker Engine (macOS or Linux); Docker Desktop on Windows | `container`: cages share one kernel | Set `docker.runtime: kata` (`microvm`) or `runsc` (`sandboxed-container`) on Linux for more. |
| `podman` | Podman (machine on macOS, native on Linux) | `container` | Same driver as docker. The dashboard watches it through its API service when one is running (Linux: `systemctl --user enable --now podman.socket`), else polls the CLI. |

Go 1.24+ to build from source.

### Picking a backend and the isolation floor

`zoo backend` lists what's installed, running, and how strong each is. `auto` (the default) picks the strongest ready backend that meets your **isolation floor**.

The floor defaults to `microvm`: zoo refuses to start cages that share a kernel with each other unless you say so. Plain Docker/OrbStack/Podman cages are `container` isolation, so to use them:

```sh
zoo up --backend docker --isolation-floor container   # remembered for later commands
# or in ~/.zoo/config.yaml:
#   backend: docker
#   isolation_floor: container
# or once: zoo run --allow-weaker-isolation claude   (the cage is marked "below floor")
```

The isolation level is shown in `zoo ls`, `zoo status`, `zoo doctor` and on every dashboard card. `zoo backend use <name>` switches the default; cages on the old backend stay where they are.

### Windows

Docker Desktop (WSL 2 backend), with `--isolation-floor container`. Cages share Docker Desktop's Linux VM kernel, not Windows'. Your worktree appears inside the cage as `/c/Users/you/repo`; zoo rewrites git's pointer files for the cage (yours aren't touched) and passes your `core.autocrlf` through, so git in the cage sees the same clean tree you do. The dashboard daemon runs without a window and watches Docker through its API rather than launching `docker` repeatedly.

- **Credentials** (`zoo keys set`) go to Windows Credential Manager, as generic credentials named `zoo:<name>` (Control Panel → Credential Manager → Windows Credentials).
- **Shims** are small `.exe` copies of zoo (`claude.exe`, `codex.exe`) in `%USERPROFILE%\.zoo\shims`, because Node-based tools like Orca and Conductor won't launch `.cmd` files. `zoo shims install --path` puts that folder first on your user PATH; restart Orca and your terminals afterwards. `set ZOO=off` runs the real CLI.
- **SSH agent grants** use the Windows agent pipe (`\\.\pipe\openssh-ssh-agent`), which the OpenSSH agent service, 1Password and Bitwarden all serve: a service with `ssh-agent: system`, `1password` or `bitwarden` works.

Repos on network shares (`\\server\…`) can't be caged. Large `node_modules` trees are slow over Docker Desktop's file sharing.

## Install

```sh
# from source
go build -o ~/.local/bin/zoo ./cmd/zoo

# or the prebuilt binary (darwin/arm64)
install -m 0755 zoo-darwin-arm64 ~/.local/bin/zoo
xattr -d com.apple.quarantine ~/.local/bin/zoo 2>/dev/null
```

## Quickstart

```sh
zoo up                      # first run builds the cage + keeper images (a few minutes)

claude setup-token          # on your Mac: a long-lived token for Claude Code
zoo keys set claude         # paste it; stored in your Keychain

cd ~/src/your-repo
zoo doctor                  # proves the walls hold from inside a cage
zoo run claude              # Claude Code, caged
zoo dash                    # open the dashboard
```

`zoo run` creates (or reuses) the cage for the current git worktree, then runs the agent inside it as an unprivileged user. Run it again in the same worktree and you join the same cage; that's how several agents share one.

## Commands

| Command | Does |
|---|---|
| `zoo up` / `zoo down [--all]` | Start / stop the keeper and dashboard (`--all` also stops cages) |
| `zoo run [harness] [--size S] [-- args]` | Run an agent in this worktree's cage. Default harness: `claude` |
| `zoo shell [cage]` | Bash in a cage |
| `zoo ls` | List cages |
| `zoo stop <cage\|all>` / `zoo reset <cage>` | Stop a cage / delete it so the next run starts clean |
| `zoo allow <host…>` / `zoo deny <host…>` / `zoo allowlist` | Edit the allowlist (`.example.com` = domain + subdomains) |
| `zoo blocked [cage]` | Hosts the keeper refused that aren't allowlisted: your approval queue |
| `zoo refs [ok] [cage]` | Branches and tags the cage moved besides its own, with undo commands; `ok` accepts them |
| `zoo keys set\|rm <name>` / `zoo keys ls` | Harness credentials in the Keychain |
| `zoo shims install` | PATH shims so other tools land in cages |
| `zoo doctor [cage]` | Check every wall from inside a cage |
| `zoo build [cage\|keeper]` | Rebuild images (then `zoo reset` cages to pick it up) |
| `zoo backend [list\|use <name>]` | Show backends, their isolation and readiness; pick one |
| `zoo status` | Backend, isolation, keeper, dashboard, shims at a glance |
| `--backend B`, `--isolation-floor L`, `--allow-weaker-isolation` | Global flags for any command |

### Sizes

| Size | CPUs | Memory | Processes |
|---|---|---|---|
| `tiny` | 1 | 1 GiB | 512 |
| `small` (default) | 2 | 2 GiB | 1024 |
| `medium` | 4 | 4 GiB | 2048 |
| `large` | 6 | 8 GiB | 4096 |

`--size 3:6G` for a one-off. Per repo: `size: medium` in `zoo.yaml`. Changing size restarts the cage; the worktree is untouched. Define your own under `sizes:` in `~/.zoo/config.yaml`.

### Per-repo `zoo.yaml` (optional)

```yaml
size: medium
mask: ["config/*.secret.json"]   # extra files to hide inside the cage
unmask: [".env.test"]            # files the defaults would hide but this repo needs
```

## What a cage can and can't do

| | |
|---|---|
| Read/write your worktree | Yes |
| See secret files in the worktree: `.env*`, keys and keystores, cloud/kube credentials, `*.tfvars`/`*.tfstate`, `secrets.*`, `.envrc`, `.npmrc`, `.netrc` (any depth to 12, build output included; dependency dirs skipped) | No: replaced by empty read-only files. Over 512 matches, zoo refuses to start the cage rather than hide only some |
| Move a branch other than its own (`main`, `origin/*`, tags) | Can't be blocked (refs are shared), so it's reported: at the end of each `zoo run`, on the dashboard, in `zoo doctor` and `zoo refs`, with the command to undo it |
| Write `.git/hooks` or `.git/config` (things that run on your Mac later) | No: read-only, and `.git` can't be renamed to get around that |
| Redirect another worktree's git, or a submodule's (`.git/worktrees/*`, `.git/modules`) | No: read-only; only this worktree's own gitdir is writable |
| Plant a repository in the worktree (`sub/.git`, a bare repo, a `.git` file pointing elsewhere) | Can't be blocked, so it's caught: `zoo run` warns when the session ends, the dashboard alerts within 30 s, `zoo doctor` fails. Don't run git in a flagged dir; delete it |
| Reach your Mac, LAN, tailnet or cloud metadata through the keeper | No: private, loopback, link-local and CGNAT addresses are refused even for allowlisted names |
| Gain capabilities, run setuid binaries | No: every capability dropped; agents run as an unprivileged user |
| Reach the internet directly | No: internal network, no DNS |
| Reach allowlisted hosts | Yes, through the keeper, logged |
| Reach other hosts | No: denied and logged, alert on the dashboard |
| See your home dir, SSH keys, cloud creds, Keychain | No |
| Push to GitHub | Not by default: agents commit, you push |

The agent's own model token is inside the cage by design. Use a dedicated token you can revoke.

## Using it with other tools

zoo catches agents through three doors. No tool needs to know zoo exists.

**1. Shims (anything that spawns a local CLI)**

```sh
zoo shims install
# then, as the LAST line of ~/.zshrc and ~/.zprofile:
export PATH="$HOME/.zoo/shims:$PATH"
```

Now `claude` and `codex` anywhere on your Mac run inside the right cage. `ZOO=off claude` runs the real one.

- **Conductor** and **herdr**: spawn `claude`/`codex` in their worktrees, so the shim catches them. If an app doesn't see the shim (GUI apps don't always read your shell's PATH), point its agent/CLI path setting at `~/.zoo/shims/claude`. Quit and reopen the app after changing PATH.
- **Your terminal / scripts**: just works.

**2. Direct**

`zoo run codex`, `zoo run claude -- -p "fix the tests"`, `zoo shell`.

**3. More harnesses**

Add any CLI agent in `~/.zoo/config.yaml`, then `zoo shims install` again:

```yaml
harnesses:
  pi:
    command: pi
    env: { ANTHROPIC_API_KEY: anthropic }    # ENV_VAR: keychain item (zoo keys set anthropic)
    allow: [api.anthropic.com]               # hosts it needs, added on first run
  opencode:
    command: opencode
```

The CLI must be installed in the cage image: add it to `assets/images/cage/Dockerfile`, rebuild `zoo`, then `zoo build cage` and `zoo reset` any existing cages.

**Orca:** v0 works through shims for local tabs. SSH access into cages (so Orca's remote mode can target a cage directly) is on the phase 3 list.

### Harness notes

| Harness | Credential | Notes |
|---|---|---|
| Claude Code | `claude setup-token` → `zoo keys set claude` | Non-essential traffic and auto-update are off inside cages |
| Codex | `zoo keys set codex` (API key), or `zoo keys set codex-auth < ~/.codex/auth.json` (ChatGPT login) | |
| Anything else | `env:` mapping in config | Network, resources and file safety work for every harness |

## Dashboard

`zoo dash` opens `http://127.0.0.1:7777`.

- **Cages:** status, size, live CPU / memory / network against limits, attached agents, request and deny counts. Stop button.
- **Network:** every request from every cage, live. Search box (`/` to focus) matches host, URL, cage, method and status; `-term` excludes. Filter by cage and by All / Allowed / Denied / New hosts. Pause freezes the feed while you read. **Allow** on a blocked row adds that host for every cage.
- **Alerts:** blocked requests, a repository planted in a worktree, and any cage that uploads more than 5 MB to one host (`bytes_out_alert` in config). Set `ntfy_url` to get them on your phone.

It listens on 127.0.0.1 only; changes need a per-install token, so other web pages can't drive it.

## First run in a new repo

There's no allow-everything mode: every host is approved by hand. Run the agent, let it hit walls, and approve what you trust, either with **Allow** on a red row in the dashboard or from the terminal:

```sh
zoo blocked            # hosts refused that aren't on the allowlist, most-requested first
zoo allow <the ones you trust>
```

Approvals apply to running cages immediately; the agent just retries.

## Files

| Path | What |
|---|---|
| `~/.zoo/config.yaml` | Global config (sizes, masks, harnesses, alerts) |
| `~/.zoo/keeper/etc/allowlist.txt` | The allowlist, one host per line |
| `~/.zoo/keeper/log/access.log` | Every request through the keeper |
| `~/.zoo/daemon.log` | Dashboard daemon log |

## Known gaps in v0

- **Your Mac's open ports are visible to cages.** On an internal network the host gateway is still reachable ([apple/container#1320](https://github.com/apple/container/issues/1320)), so anything on your Mac bound to `0.0.0.0` is reachable from a cage. `zoo doctor` lists them. Common culprit: AirPlay Receiver (ports 5000/7000): turn it off in System Settings → General → AirDrop & Handoff. Bind dev servers to `127.0.0.1`.
- **Allowed hosts can still carry data out** (e.g. a gist on github.com, using a token the attacker supplies). The bytes-out alert is the tripwire, and it won't notice a few KB.
- **Branch moves are reported, not prevented**, and zoo can't tell who moved a ref: a `git fetch` you run on the host while a cage is running shows up too. Accept those with `zoo refs ok`. Repos using reftable ref storage aren't watched.
- **Planted repositories are detected, not prevented.** The dashboard scans every 30 s, so a shell prompt or editor running git in the worktree *while* the agent works can beat it. Setting `safe.bareRepository=explicit` in your global git config closes the bare-repo variant for good.
- **Submodules are read-only in cages** (`.git/modules`): update them on the host.
- **Attribution is per cage**, not per agent, when several agents share a cage.
- **Masks are fixed while a cage runs.** A secret file added later is readable in a running cage (`zoo run` warns); the next start re-masks. On Apple, edits you make to `.git/config` on the host also show up in the cage only after `zoo reset` (the cage sees a read-only copy).
- **Docker/Podman cages share a kernel** (isolation `container`), which is why the floor refuses them by default. On macOS that kernel is OrbStack's or Docker Desktop's VM, not your Mac's.
- **`nproc` inside a Docker cage shows all host CPUs**; the CPU limit is a quota, not a CPU set.
- **No disk limit per cage yet**, and no Docker-inside-the-cage yet.

## Troubleshooting

- `zoo up` fails on `network create --internal`: needs macOS 26 and a recent `container` release.
- `zoo run` says the backend is below your isolation floor: see "Picking a backend" above.
- After upgrading zoo: `zoo up` builds the new images and replaces the keeper. Stopped cages are rebuilt on their next run; a running cage from an older zoo prints a warning: stop its agents and `zoo reset` it.
- Agent can't reach something it needs: look for the red row in the dashboard and click **Allow**, or `zoo allow host`.
- Cage files owned by the wrong user: the cage image uses your Mac uid/gid at build time; rebuild with `zoo build cage` and `zoo reset <cage>`.
- Dashboard empty: `cat ~/.zoo/daemon.log`, then `zoo down && zoo up`.

## Roadmap

1. Cage + keeper + basic dashboard; Apple and Docker/Podman backends ← you are here
2. Visibility: OTel collector + store, file watcher, resource history, phone alerts
3. Agent sessions: Claude Code / Codex OTel → AG-UI timelines, Tetragon in the guest, per-agent attribution, SSH into cages
4. Homelab + k8s: k3s + Kata on Proxmox, keeper as egress gateway, Helm chart
