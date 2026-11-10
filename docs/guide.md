# Guide

Everything you can do with zoo, roughly in the order you'll need it. New here? Start with [Getting started](getting-started.md).

- [Commands](#commands)
- [Cages: sizes, sharing, resetting](#cages)
- [Disposable tasks](#disposable-tasks)
- [The allowlist](#the-allowlist)
- [Per-repo settings: zoo.yaml](#per-repo-settings-zooyaml)
- [Services: databases, host ports, SSH](#services)
- [Credentials that never enter a cage](#credentials-that-never-enter-a-cage)
- [Agents and harnesses](#agents-and-harnesses)
- [Previewing dev servers](#previewing-dev-servers)
- [SSH and editors](#ssh-and-editors)
- [Branches the agent moves](#branches-the-agent-moves)
- [Package caches](#package-caches)
- [Your own image](#your-own-image)
- [The dashboard](#the-dashboard)
- [Windows](#windows)
- [Config reference](#config-reference)
- [Files](#files)
- [Troubleshooting](#troubleshooting)

## Commands

| Command | Does |
|---|---|
| `zoo up` / `zoo down [--all]` | Start / stop the keeper and dashboard (`--all` also stops cages) |
| `zoo run [harness] [--size S] [-- args]` | Run an agent in this worktree's cage (default harness: `claude`) |
| `zoo task new/run/list/diff/apply/destroy` | Work in a separate copy, then review and apply edits |
| `zoo shell [cage]` | Bash in a cage |
| `zoo ls` / `zoo status` | List cages / backend, keeper, dashboard and shims at a glance |
| `zoo stop <cage\|all>` / `zoo reset <cage>` | Stop a cage / delete it so the next run starts clean |
| `zoo allow [--for 4h] <host…>` / `zoo deny <host…>` / `zoo allowlist` | Edit the allowlist |
| `zoo blocked [cage]` | Hosts refused that aren't allowlisted: your approval queue |
| `zoo service ls` | Services and credentials, and which cages hold them |
| `zoo grant [--for 4h] <cage> <name>` / `zoo revoke <cage> <name>` | Give a cage a service or credential, for a while or for good |
| `zoo trust [path] [--show]` | Approve this worktree's `zoo.yaml` |
| `zoo refs [ok] [cage]` | Branches and tags the cage moved besides its own, with undo commands; `ok` accepts them |
| `zoo keys set\|rm <name>` / `zoo keys ls` | Secrets in your Keychain |
| `zoo ports [cage]` / `zoo open [cage] [port]` | Dev servers in a cage, and their preview URLs |
| `zoo ssh setup` / `zoo ssh [cage] [cmd…]` | SSH into cages, for editors |
| `zoo cache ls` / `zoo cache clear <cage\|shared\|all>` | Package caches |
| `zoo doctor [--deep] [--json] [cage]` | Check every wall from inside a cage |
| `zoo dash` | Open the dashboard |
| `zoo shims install [--path]\|uninstall` | Put `claude`/`codex`/… shims on PATH so other tools land in cages (`--path`: Windows user PATH) |
| `zoo build [cage\|keeper]` / `zoo build --project` | Rebuild images / approve and build this repo's Dockerfile |
| `zoo backend [list\|use <name>]` | Show runtimes and their isolation; pick one |

Global flags, on any command: `--backend apple|docker|podman|auto`, `--isolation-floor vm|microvm|sandboxed-container|container` (both remembered by `zoo up`), and `--allow-weaker-isolation` (start this one cage below the floor; it's marked on the dashboard).

`<cage>` is the name from `zoo ls` (e.g. `api-3fa2c1`), a unique prefix of it, or a worktree path.

## Cages

One cage per git worktree, named after the directory plus a short hash. It persists: stop it and the next `zoo run` starts it again with the same files. Several agents in the same worktree share the cage.

**Sizes.** `zoo run --size medium`, or `size: medium` in `zoo.yaml`, or one-off `--size 3:6G`. Changing size restarts the cage; the worktree is untouched.

| Size | CPUs | Memory | Processes |
|---|---|---|---|
| `tiny` | 1 | 1 GiB | 512 |
| `small` (default) | 2 | 2 GiB | 1024 |
| `medium` | 4 | 4 GiB | 2048 |
| `large` | 6 | 8 GiB | 4096 |

Define your own under `sizes:` in `~/.zoo/config.yaml`.

**Reset.** `zoo reset <cage>` deletes it; the next run starts from a clean image. Do this after changing masks, mounts, `git:` or the image, or if you think the agent tampered with its own environment. Your worktree and the cage's package cache are kept (`zoo cache clear` for the cache).

**What's inside.** Your worktree at the same path as on your machine (on Windows, `C:\Users\you\repo` appears as `/c/Users/you/repo`), read-write. Secret files read as empty (see [masks](#masks)). `.git/hooks`, `.git/config` and other worktrees' git data are read-only. Nothing else from your machine: no home directory, SSH keys, cloud credentials or Docker socket.

### Masks

zoo hides files matching these names anywhere in the worktree (to a depth of 12, build output included; `node_modules`, `vendor`, virtualenvs and similar dependency trees skipped):

- env files and tool secrets: `.env`, `.env.*`, `.envrc`, `.dev.vars`, `.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`, `.pgpass`, `.htpasswd`
- keys and keystores: `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `id_rsa*`, `id_ed25519*`, `id_ecdsa*`, `id_dsa*`
- cloud and cluster credentials: `credentials.json`, `client_secret*.json`, `service-account*.json`, `application_default_credentials.json`, `kubeconfig`, `*.kubeconfig`, `.kube/config`, `.aws/credentials`, `.docker/config.json`, `.dockercfg`
- infrastructure: `*.tfvars`, `*.tfstate`, `*.tfstate.*`, `secrets.yaml`, `secrets.yml`, `secrets.json`

Not hidden: `*.example`, `*.sample`, `*.template`, `*.pub`. Add more per repo with `mask:` in `zoo.yaml`, or show one with `unmask:`. `masks:` in `~/.zoo/config.yaml` *replaces* the built-in list.

Masks are set when a cage starts. A secret file added while it runs is readable inside until it restarts; `zoo run` warns, and the next start hides it. If a worktree has more than 512 matches, zoo refuses to start the cage rather than hide only some: `unmask:` the ones that aren't secret (test fixtures, sample keys).

## Disposable tasks

A task gives the agent a separate project copy. The original files stay unchanged until you apply the edits.

```sh
zoo task new fix-parser .
zoo task run fix-parser claude
zoo task diff fix-parser --stat
zoo task diff fix-parser
zoo task apply fix-parser --dry-run
zoo task apply fix-parser
zoo task destroy fix-parser
```

`new` copies the selected directory, including tracked and non-ignored untracked files.
It excludes secret masks, Git metadata, and dependency directories such as `node_modules`.
Each task starts with a fresh Git baseline. Source history and submodules are not supported.
On Windows, projects with symlinks require permission to create symlinks. Zoo preserves symlinks instead of converting them to text files.
The task inherits the approved source configuration. Unapproved `zoo.yaml` edits remain proposals.
Extra host mounts are refused. Each task has a private package cache.

`run` stops the task cage on normal exit. The task copy persists for another session.
`diff` includes uncommitted edits and non-ignored new files. Host review uses a separate Git database outside the cage.
`apply` writes unstaged edits to the original directory. It does not preserve agent commits.
If a changed host file differs from the baseline, apply refuses the entire patch.
Unrelated host edits remain unchanged. Do not edit the same host files during apply.

Review and apply require a stopped cage and an available task backend.
`destroy` removes the task copy, runtime cage, and private cache.
If the task has unapplied changes, destruction requires `--discard`.

**After a disconnect:** Run `zoo task list`, then `zoo task run NAME` to continue.
If a previous command left a lock, first make sure that its process has exited.
The error gives the lock path and process ID is stored inside that file.
Remove that lock only after the process exits. Stop a surviving cage with `zoo stop CAGE --backend BACKEND` before review.
An interrupted apply saves `last-apply.patch` and a recovery record outside the cage.
Retry `apply` to recover a fully applied patch. Mixed host changes require manual recovery from the saved patch.

## The allowlist

Cages can only reach hosts on the allowlist, through the keeper. A fresh install allows the model APIs (Anthropic, OpenAI), GitHub, npm, PyPI and the Go proxy. Everything else is refused and logged until you approve it.

```sh
zoo blocked                         # what's been refused, most-requested first
zoo allow registry.yarnpkg.com      # for good
zoo allow --for 4h docs.example.com # lapses after 4 hours (30m, 4h, 2d…)
zoo allow .example.com              # the domain and all its subdomains
zoo deny docs.example.com
zoo allowlist                       # the list, with time left on temporary entries
```

The dashboard's **Allow** button does the same and asks how long. Changes apply to running cages immediately.

Whatever the allowlist says, the keeper refuses private, loopback, link-local and CGNAT addresses (your LAN, your Mac, a tailnet, cloud metadata) and names ending `.localhost`, `.internal` or `.local`. To give a cage something on your machine or network, use a [service](#services).

### Project network policy

An approved `zoo.yaml` can replace the global allowlist for that project:

```yaml
network:
  allow:
    - api.anthropic.com
    - registry.npmjs.org
    - .pythonhosted.org
```

Run `zoo trust` after you review the change. A leading dot permits a domain and its subdomains.
Without `network.allow`, the cage inherits the global allowlist. `allow: []` denies all web destinations.
An explicit list replaces the global list. Include the model endpoints and package registries that the task requires.
`zoo allow` and harness `allow:` entries do not widen an explicit project policy.
Private addresses and unsafe ports remain blocked. Service grants are separate from this web policy.
The dashboard shows **Project policy** instead of a global **Allow** button for project restrictions.

Zoo reloads policies before an agent starts. The daemon also refreshes approved changes.
Existing proxy connections can survive a reload. To revoke those connections, stop and restart the cage.
If Docker cannot create a private cage network, startup fails without using a shared network.

## Per-repo settings: zoo.yaml

Optional, at the worktree root:

```yaml
size: medium
mask: ["config/*.secret.json"]   # more files to hide
unmask: [".env.test"]            # files the defaults would hide but this repo needs
services: [pg, github]           # services and credentials this repo's cage gets
mounts: [../infra, ~/src/web]    # more repos in the same cage (masked and git-guarded too)
git: rw                          # rw (default) | ro | none
cache: project                   # project (default) | shared | off
image: python:3.12-slim          # your own base image
# dockerfile: .zoo/Dockerfile    # or build one
```

**zoo only uses a `zoo.yaml` you approved.** The file sits in the worktree, which the agent can write, and it can grant services or unmask secrets. So what's on disk is a proposal:

- `zoo run` from a terminal shows what changed and asks.
- Anywhere else (a shim launched by an app) zoo keeps the last approved copy and warns.
- `zoo trust` approves it; `zoo trust --show` prints what's in effect.

Grants apply as soon as they're approved. Masks, mounts, `git:`, cache and image apply when the cage is created (`zoo reset <cage>`).

**`git:`** what the cage may do with the repository:

- `rw` (default): commit, branch, rebase. Hooks and config stay read-only.
- `ro`: read history; can't commit or move branches.
- `none`: no `.git` in the cage at all. For repos you don't trust, or if you commit outside the sandbox anyway.

**`mounts:`** more repos in the same cage, e.g. a frontend and a backend, or a set of gitops repos. Each gets the same masks and git guard. Paths are relative to this worktree or start with `~/`. They can't be `/`, your home, a parent of this worktree, or overlap `~/.ssh`, `~/.aws`, `~/.config`, `~/Library` and similar.

## Services

Agents often need more than web hosts: a database, a port on your machine, SSH to GitHub. A **service** is one of those, and a **grant** gives one cage access to it. Cages reach every service through the keeper's gate, which only answers cages holding a grant and logs every connection.

Define services in `~/.zoo/config.yaml`:

```yaml
services:
  pg:     { host: 127.0.0.1:5432 }          # something on your machine (it can stay on 127.0.0.1)
  redis:  { image: redis:7, port: 6379 }    # a sidecar container zoo runs
  gh-ssh: { remote: github.com:22 }         # a non-HTTP destination
  keys:   { ssh-agent: bitwarden, keys: [github], hosts: [github.com] }
```

Then grant:

```sh
zoo grant api-3fa2c1 pg               # for good
zoo grant --for 4h api-3fa2c1 keys    # lapses
# or in zoo.yaml: services: [pg]
```

Inside the cage, a granted service is `<name>.zoo` on its port (`pg.zoo:5432`), with the usual variables set where there's a convention (`PGHOST`/`PGPORT`, `REDIS_URL`; set your own with `cage_env:`). Ports below 1024 move up by 20000 (`gh-ssh.zoo:20022`); for SSH on port 22 zoo writes an `ssh_config` entry, so `git@github.com:` just works.

| Kind | Key | Notes |
|---|---|---|
| Host port | `host: 127.0.0.1:5432` | Reached through zood's host bridge, so the service doesn't need to listen on `0.0.0.0` |
| Sidecar | `image: postgres:16`, `port:`, `env:`, `volume:` | Runs on a network only the keeper joins; data in `~/.zoo/services/<name>/` when `volume:` is set; `env:` values may be secret refs |
| Remote | `remote: host:port` | Any TCP destination |
| SSH agent | `ssh-agent: bitwarden \| 1password \| system \| <socket>`, `keys:`, `hosts:`, `approve: true` | The cage gets `SSH_AUTH_SOCK`; zoo lists only the granted keys, signs only for the allowed hosts, optionally asks on the dashboard first, and logs every signature. Private keys never leave your vault |

## Credentials that never enter a cage

Give agents GitHub (or any HTTPS API) without giving them the token.

```yaml
# ~/.zoo/config.yaml
credentials:
  github:
    hosts: [github.com, api.github.com]
    secret: github-pat          # zoo keys set github-pat  (or bw:item, op://vault/item/field)
    env: [GH_TOKEN, GITHUB_TOKEN]
    git: true                   # git over https uses it; ssh remotes are fetched over https
    readonly: true              # only reads: GET, GraphQL queries, git fetch, LFS download…
    push: ["agent/*"]           # …plus git push to these branches
    repos: [you/app]            # …in these repositories
```

```sh
zoo keys set github-pat                  # a fine-grained token, scoped to the repos you mean
zoo grant --for 4h api-3fa2c1 github     # or services: [github] in zoo.yaml, or the dashboard
```

The cage gets a placeholder like `zoocred.github.api-3fa2c1.9c1e…` in `GH_TOKEN`, and from git's credential helper. It's tied to that cage and credential, so it's useless anywhere else. On the way to the credential's hosts, and only those, the keeper swaps it for the real token and checks the policy:

- `gh run view --log`, `git fetch`, reading issues: work.
- `git push origin agent/fix-tests`: works.
- `git push origin main`: `! [remote rejected] main (zoo keeper: push to refs/heads/main isn't allowed …)`.
- `gh gist create`, `gh issue create`: 403 from the keeper. That holds even for a token the agent got from somewhere else, because the policy covers every request to those hosts.

Leave out `readonly`/`push` for full access with the token; set only `readonly` for a read-only GitHub with no token at all.

`repos:` narrows REST and git paths, but GraphQL can't be narrowed by path, so scope the token itself too. `zoo service ls` lists credentials and who holds them; `~/.zoo/keeper/log/inject.log` has a line per request (never the value). How the keeper does this safely is in [How it works](how-it-works.md#credentials).

**Model credentials.** The default Claude and Codex harnesses use keeper placeholders.
Store the Claude token with `zoo keys set claude`, or a Codex API key with `zoo keys set codex`.
The real values stay on the host and in the keeper.
For Codex API keys, Zoo writes a placeholder to `.codex/auth.json` inside the cage.
A stored API key takes precedence over a stored subscription login.

Codex subscription login remains a direct-file fallback from `codex-auth`. Zoo prints a warning when it uses this fallback.
Explicit harness overrides remain unchanged. Plain key references and `files:` still deliver real credentials into the cage.
To remove credentials from an older cage, reset it before you start a new session.
Credential delivery checks current runtime state. A temporary denial is retried on the next request.

## Agents and harnesses

A harness is how zoo starts an agent CLI: `zoo run claude`, `zoo run codex`, `zoo run shell`. Add others in `~/.zoo/config.yaml`, then `zoo shims install` again:

```yaml
harnesses:
  pi:
    command: pi
    env: { ANTHROPIC_API_KEY: anthropic }    # ENV_VAR: keychain item (zoo keys set anthropic)
    allow: [api.anthropic.com]               # hosts it needs, allowlisted on first run
  opencode:
    command: opencode
```

`env:` values are Keychain items, vault references (`bw:item/field`, `op://vault/item/field`, fetched on your machine per session; the vault session never enters the cage) or `cred:<name>` placeholders. `files:` puts a secret in a file under the agent's home instead.

The CLI has to exist in the cage image: use [your own image](#your-own-image), or add it to `assets/images/cage/Dockerfile` and rebuild.

| Harness | Credential | Notes |
|---|---|---|
| Claude Code | `claude setup-token`, then `zoo keys set claude` | Non-essential traffic and auto-update are off inside cages |
| Codex | `zoo keys set codex` (API key), or `zoo keys set codex-auth < ~/.codex/auth.json` (ChatGPT login) | |

**Launching from other tools.** `zoo shims install` puts `claude`, `codex` and your other harnesses in `~/.zoo/shims`; put that first on PATH (last line of `~/.zshrc` and `~/.zprofile`) and Conductor, herdr, Orca's local tabs and your terminal all land in the right cage. GUI apps that don't read your shell's PATH can point their agent path at `~/.zoo/shims/claude`. `ZOO=off claude` runs the real one.

## Previewing dev servers

Each cage's dev servers are reachable from your browser, even when your machine can't route to the cage:

```
http://<cage>.localhost:7779          the cage's lowest listening port (e.g. Vite on 5173)
http://<port>.<cage>.localhost:7779   a specific port
```

`zoo ports` lists what's listening with URLs; `zoo open [cage] [port]` opens one. The dashboard shows the same links on each cage card, so two agents' versions of your app sit side by side.

Only `*.localhost` names of running cages are answered, on 127.0.0.1. `preview_port:` changes the port; `0` turns previews off. Chrome and Firefox resolve `*.localhost` themselves; Safari on recent macOS does too (older releases: add `127.0.0.1 <cage>.localhost` to `/etc/hosts`). If a dev server rejects the host name, allow `.localhost` (Vite already does; Next.js: `allowedDevOrigins`).

## SSH and editors

Editors that work over SSH (VS Code and Cursor Remote-SSH, JetBrains Gateway, Orca's remote mode) can open a cage directly:

```sh
zoo ssh setup          # writes ~/.zoo/ssh/config and a key in ~/.zoo/ssh/
```

Add the line it prints at the **top** of `~/.ssh/config` (`Include ~/.zoo/ssh/config`), then `ssh zoo-<cage>`, or pick `zoo-<cage>` in your editor. `zoo ssh [cage] [cmd…]` works without touching `~/.ssh/config`. Run `zoo ssh setup` again after creating cages to list them by name.

You're the `agent` user, with the same keeper, allowlist and services as `zoo run`, but not the model credentials. Local port forwarding works (editors use it); agent forwarding, X11 and remote forwarding are refused.

**Editor settings you need.** Your editor trusts the server it connects to, and in a cage the agent controls that server. Don't give it a way back to your credentials:

- **VS Code / Cursor:** `"remote.SSH.enableAgentForwarding": false`, `"github.gitAuthentication": false`, `"git.terminalAuthentication": false`, `"git.useIntegratedAskPass": false`. Otherwise git in the cage is answered from your desktop's credentials. `"remote.SSH.localServerDownload": "always"` saves allowlisting Microsoft's download hosts.
- **JetBrains Gateway** downloads its backend from inside the cage: allowlist its hosts (`zoo blocked` shows them).

## Branches the agent moves

In `git: rw` mode a cage has to write the repo's refs (its commits must be visible outside), and mounts can't separate one branch from another. So an agent *can* move `main`, point `origin/main` somewhere so it looks pushed, or move a tag. zoo can't stop that, so it reports it: when a session starts it snapshots the refs, and at the end of the session, on the dashboard (within 30 s) and in `zoo doctor` it lists anything moved besides the cage's own branch, with the command to undo it:

```sh
zoo refs              # what moved, with undo commands
zoo refs ok           # accept it (e.g. you fetched or committed on the host yourself)
```

zoo can't tell who moved a ref, so your own `git fetch` on the host while a cage runs shows up too; `zoo refs ok` accepts it. Repos using reftable ref storage aren't watched. For no risk at all, use `git: ro`; for pushes, a [credential](#credentials-that-never-enter-a-cage) with `push:` branches.

## Package caches

Each cage keeps its package downloads (npm, pnpm, yarn, pip, uv, Go, cargo, bun) in `~/.zoo/cache/project/<cage>`, so `zoo reset` doesn't mean downloading everything again. No other cage sees it.

`cache:` in `~/.zoo/config.yaml` or `zoo.yaml`:

- `project` (default): one cache per cage.
- `shared`: one cache, `~/.zoo/cache/shared`, for every cage that picks it. Less disk, but **any cage using it can plant packages the others then install and run**. Keep `project` for repos whose agents read untrusted input.
- `off`: no cache.

`zoo cache ls` shows sizes; `zoo cache clear <cage|shared|all>` deletes one. A project cache outlives `zoo reset`, so clear it too if you think an agent was compromised. pnpm can't hard-link from the cache into `node_modules` (they're separate mounts), so it copies; still much faster than downloading.

## Your own image

The default cage has Node 22, Python 3, uv, git, ripgrep and the Claude and Codex CLIs. For other tools, give a repo its own image in `zoo.yaml`:

```yaml
image: python:3.12-slim          # any image with apt-get or apk
# or
dockerfile: .zoo/Dockerfile      # relative to the worktree
```

zoo adds what a cage needs on top: an `agent` user with your uid, zoo's init, bash, git, socat, curl and CA certificates. Agent CLIs are yours to install (`RUN npm install -g @anthropic-ai/claude-code`); `zoo run` tells you if one is missing.

**Dockerfiles need your approval.** The agent can edit the Dockerfile, and its `RUN` steps run with network access outside the keeper, so zoo never builds a version you haven't seen. `zoo build --project` shows it, asks, and builds. Until you approve a change, cages keep the image from the version you approved (or zoo's default), and `zoo run` says so. The build context is empty, so `COPY` from the worktree doesn't work and secrets like `.env` can't be baked in. Running cages switch after `zoo reset`.

## The dashboard

`zoo dash` opens `http://127.0.0.1:7777`.

- **Cages:** status, isolation level, size, live CPU, memory and network against limits, agents attached, request and deny counts, granted services (with time left on temporary ones), preview links. Stop button.
- **Network:** every request from every cage, live, plus service connections and SSH signatures. Search (`/`) matches host, URL, cage, method and status; `-term` excludes. Filter by cage and by All / Allowed / Denied / New hosts. Pause freezes the feed. **Allow** changes the global list for cages that inherit it, for as long as you say.
- **Services:** what's defined, its state, and who holds it; `+ grant` on a cage card.
- **Approvals:** SSH signatures waiting for you (`approve: true`).
- **Alerts:** refused requests, a repository planted in a worktree, a cage uploading more than 5 MB to one host (`bytes_out_alert`), temporary allows and grants lapsing. Set `ntfy_url` to get them on your phone.

It listens on 127.0.0.1 only; changes need a per-install token, so other web pages can't drive it.

Recent web events, alerts, counters, and session records persist in `history.json` across daemon restarts.
The checkpoint keeps up to 5,000 web events and 500 alerts. It is saved every five seconds and on normal shutdown.
Saved events retain their cage names even after an address is reused.
Service connections, SSH signature events, and resource samples are not included in this checkpoint.
If the checkpoint is corrupt, startup reports its path. Preserve that file before moving it aside to start with fresh history.

## Windows

zoo runs on Windows with Docker Desktop (WSL 2 backend) and `--isolation-floor container`. Cages share Docker Desktop's Linux VM kernel, not Windows'.

- **Paths:** your worktree appears inside the cage as `/c/Users/you/repo`. zoo shows the cage rewritten copies of git's pointer files (yours aren't touched) and passes your `core.autocrlf` through, so git in the cage sees the same clean tree you do.
- **Secrets:** `zoo keys set` stores them in Windows Credential Manager as generic credentials named `zoo:<name>`.
- **Shims:** `.exe` copies of zoo (`claude.exe`, `codex.exe`) in `%USERPROFILE%\.zoo\shims`, because Node-based tools like Orca and Conductor won't launch `.cmd` files. `zoo shims install --path` puts that folder first on your user PATH; restart Orca and your terminals. `set ZOO=off` runs the real CLI.
- **SSH agent services** use the Windows agent pipe (`\\.\pipe\openssh-ssh-agent`), served by the OpenSSH agent service, 1Password and Bitwarden: `ssh-agent: system`, `1password` or `bitwarden` all work.
- **The dashboard daemon** runs without a window and watches Docker through its API.

Repos on network shares (`\\server\…`) can't be caged. Large `node_modules` trees are slow over Docker Desktop's file sharing; a [package cache](#package-caches) helps with downloads but not with the share itself.

## Config reference

`~/.zoo/config.yaml`. Everything is optional; `zoo up` writes a commented starter.

| Key | Default | What |
|---|---|---|
| `backend` | `auto` | `auto`, `apple`, `docker`, `podman` |
| `isolation_floor` | `microvm` | Refuse cages weaker than `vm`, `microvm`, `sandboxed-container` or `container` |
| `docker` | | `{ bin: docker, runtime: "" }`; `runtime: kata` or `runsc` for stronger isolation on Linux |
| `port` | `7777` | Dashboard (127.0.0.1). The host bridge uses `port`+1 |
| `preview_port` | `port`+2 | Dev-server previews; `0` turns them off |
| `default_size`, `sizes` | `small` | See [Cages](#cages) |
| `masks`, `unmask` | see [Masks](#masks) | File patterns hidden / not hidden in every cage. `masks:` *replaces* the built-in list; to add, use `mask:` in `zoo.yaml` |
| `git` | `rw` | Default for `zoo.yaml`'s `git:` |
| `cache` | `project` | Default for `zoo.yaml`'s `cache:` |
| `image`, `dockerfile` | | Defaults for `zoo.yaml`'s `image:` / `dockerfile:` |
| `services` | | [Services](#services) |
| `credentials` | | [Credentials](#credentials-that-never-enter-a-cage) |
| `harnesses` | `claude`, `codex`, `shell` | [Agents and harnesses](#agents-and-harnesses) |
| `bytes_out_alert` | `5242880` | Alert when one cage sends this many bytes to one host |
| `ntfy_url` | | Send alerts to an [ntfy](https://ntfy.sh) topic |
| `cage_image`, `keeper_image`, `network` | | Advanced: image tags and the cage network name |

`zoo.yaml` keys: `size`, `mask`, `unmask`, `services`, `mounts`, `git`, `cache`, `image`, `dockerfile`, `network.allow` (see [Per-repo settings](#per-repo-settings-zooyaml)).

Environment: `ZOO_HOME` (instead of `~/.zoo`), `ZOO_BACKEND`, `ZOO_ISOLATION_FLOOR`, `ZOO=off` (shims run the real CLI), `ZOO_KEYS_FILE=1` (files instead of the Keychain or Credential Manager).

## Files

Everything zoo keeps is under `~/.zoo`:

| Path | What |
|---|---|
| `config.yaml` | Global config |
| `tasks/NAME/` | Task copy, review baseline, metadata, and saved apply patch |
| `history.json` | Recent web activity, alerts, counters, sessions, and log cursor |
| `keeper/etc/project-access.json` | Generated address-to-policy mapping |
| `keeper/etc/allowlist.txt` | The allowlist, one host per line |
| `allow-expiry.json` | When temporary allowlist entries lapse |
| `keeper/log/access.log` | Every web request through the keeper |
| `keeper/log/gate.log` | Every service connection |
| `keeper/log/inject.log` | Every request to a credential host (never the value) |
| `grants.json`, `grant-expiry.json` | `zoo grant`s and when temporary ones lapse |
| `trust/`, `dockerfiles/` | Approved copies of each worktree's `zoo.yaml` and Dockerfile |
| `cred-key` | The key placeholders are signed with (never leaves your machine) |
| `cache/` | Package caches |
| `refs/` | Each cage's branch and tag snapshot, for `zoo refs` |
| `services/<name>/` | Sidecar data |
| `ssh/` | The SSH door's key and config |
| `daemon.log` | Dashboard daemon log |

## Troubleshooting

- **The agent can't reach something:** look for the red row on the dashboard and click **Allow**, or `zoo blocked` then `zoo allow <host>`.
- **"below your isolation floor":** you're on Docker or Podman; see [Pick a backend](getting-started.md#2-pick-a-backend).
- **`zoo up` fails on `network create --internal`:** Apple's backend needs macOS 26 and a recent `container` release.
- **After upgrading zoo:** `zoo up` builds the new images and replaces the keeper. Stopped cages are recreated on their next run; a running cage from an older zoo prints a warning: stop its agents and `zoo reset` it.
- **"zoo.yaml isn't approved" / "changed since you approved it":** review it, then `zoo trust`.
- **Files in the worktree owned by the wrong user:** the cage image uses your uid at build time; `zoo build cage` then `zoo reset <cage>`.
- **TLS errors to a credential host:** the cage predates the credential; `zoo reset <cage>`. `zoo doctor` checks this.
- **"more than 512 secret-looking files":** zoo won't start a cage it can only partly mask. Add the ones that aren't secret (fixtures, sample keys) to `unmask:` in `zoo.yaml`.
- **"refs changed besides the cage's own branch":** see [Branches the agent moves](#branches-the-agent-moves); `zoo refs ok` if it was you.
- **"secret files changed since its cage started":** a new secret is readable in the running cage; stop its agents, then `zoo stop <cage>` (the next run hides it).
- **husky (or another hook installer) fails in the cage:** expected. Its hooks directory is read-only inside, because hooks there would run on your machine at your next commit. Run the install on your machine.
- **Dashboard empty:** `cat ~/.zoo/daemon.log`, then `zoo down && zoo up`.
- **Your machine's open ports are visible to Apple cages:** see [Known gaps](security.md#known-gaps).
