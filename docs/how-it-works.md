# How it works

The short version: every agent runs in a **cage** on a network with no way out except the **keeper**, and **zood**, a daemon on your machine, watches everything and holds the secrets. This page explains the pieces, how a request flows, and why it's built this way. [Architecture](architecture.md) shows the same as diagrams, with trust boundaries; [BRIEF.md](BRIEF.md) has the decisions behind each choice (numbered `D<n>`).


## Why

Coding agents act on text they read: web pages, issues, READMEs, dependency docs, tool output. Any of it can tell an agent to run a command, and agents are good at running commands. So zoo assumes an agent *will* eventually be told to read your secrets, send your code somewhere, or leave something behind that runs on your machine later, and makes each of those either impossible or loud.

The other half is visibility. When an agent works for an hour in the background, you want to know what it touched on the network, what it's using, and what got refused, without reading its transcript.

## The pieces

**Cage.** One per git worktree: an Apple `container` VM (its own kernel) or a Docker/Podman container (shared kernel; zoo refuses it unless you lower the isolation floor). Every capability dropped, no new privileges, CPU, memory and process limits. The agent runs as an unprivileged `agent` user with your uid, so files it writes in your worktree stay yours. The cage sits on an *internal* network: no default route, no DNS.

**Worktree guard.** Your worktree is bind-mounted at the same path, read-write. On top of it, host-side mounts the agent can't undo:

- secret-looking files are replaced by an empty read-only file;
- `.git/hooks`, `.git/config`, other worktrees' and submodules' git data, and any hooks folder or script git config points at inside the worktree, are read-only;
- every directory between the worktree and those paths is "pinned" (mounted onto itself) so `mv .git .git2` can't carry the read-only mounts away.

**Keeper.** Its own small VM or container, attached to the cage network(s) and to a network with a route out. It runs:

- **squid**, the web proxy. Cages get `HTTPS_PROXY` pointing at it. It allows `CONNECT` only to allowlisted hostnames, only on ports 80/443, never to private addresses (checked after DNS, so an allowlisted name that resolves to your LAN is still refused), and logs every request.
- **zoo-gate**, a small TCP forwarder for [services](guide.md#services): one port per service, answering only cages holding a grant.
- **the injector**, inside zoo-gate, for [credentials](#credentials).

The keeper's config and logs live on your machine (`~/.zoo/keeper`), mounted into the keeper and never into a cage.

**zood**, the daemon on your machine (`zoo up` starts it). It watches Docker and Podman through their APIs (events, not polling, and no console windows on Windows), tails the keeper's logs, samples resources, raises alerts, serves the dashboard on 127.0.0.1, and runs the **host bridge**: the only path from the keeper back to your machine, authenticated by a token, used for host-port services, SSH agents and credential values. Cages can't reach it.

**The CLI.** `zoo run` plans the cage (masks, git guard, size), creates or reuses it, puts session secrets in a 0600 env file the runtime reads (never on a command line), and execs the agent through `zoo-session`. Shims make `claude` or `codex` anywhere call `zoo run`.

## A request, start to finish

1. The agent runs `curl https://api.example.com`. The proxy env sends it to squid as `CONNECT api.example.com:443`.
2. squid checks: port 443, not a private name, resolves to a public address, on the allowlist. If any check fails: 403, a red row on the dashboard, an alert.
3. If allowed, squid opens a tunnel. TLS is end-to-end between the agent and the real host; squid sees only the hostname and byte counts. One line goes in `access.log`, which zood tails and attributes to the cage by its IP.
4. You click **Allow** on a refused host: zood adds it to `allowlist.txt` and tells squid to reload. The agent's next try works.

Without the proxy env, there's nothing to talk to: no route, no DNS.

## Services

A cage resolves `pg.zoo` to the keeper (zoo writes `/etc/hosts` as guest root). zoo-gate accepts the connection only if the source address belongs to a cage holding the grant *and* arrived on the keeper address for that cage's network (so a forged source address doesn't help). Then:

- **sidecar or remote:** the gate dials it directly. Sidecars sit on a network only the keeper joins.
- **host port or SSH agent:** the gate dials zood's host bridge with a token and the cage's name; zood re-checks the grant and connects to `127.0.0.1:5432`, or serves the SSH-agent protocol itself: listing only granted keys, signing only when the session's host key matches an allowed host, optionally asking you first.

Every connection and signature is logged and shown on the dashboard.

## Credentials

Most hosts stay opaque tunnels. For hosts you list under `credentials:`, the keeper looks inside, because that's the only way to add a token the cage never sees and to tell a `git fetch` from a `git push`.

1. The cage gets a placeholder: `zoocred.<credential>.<cage>.<mac>`, where the mac is an HMAC with a key only your machine has. A cage can't forge one for another cage or credential.
2. squid routes only the credential hosts' CONNECTs to the injector (a `cache_peer` on the keeper's loopback). Everything else is untouched.
3. The injector presents a certificate from a CA zoo generated for this install. The CA is **name-constrained to exactly the credential hosts**: even with its key, nobody can mint a certificate for any other site. Cages trust it; nothing else does.
4. For each request it checks that the `Host` header matches the CONNECT, applies the policy (`readonly`, `push` branches parsed from git's own push commands, `repos`), and finds placeholders in headers, including inside Basic auth.
5. It asks zood for the real value over the host bridge. zood checks the placeholder's HMAC, that the cage is running and holds the grant, reads the secret from your Keychain or vault, and answers. Values are cached for a minute in the keeper and five in zood, so a revoke takes effect within a minute.
6. It forwards to the real host over TLS with normal certificate checks, refusing private addresses.

The policy applies to every request to those hosts, with or without a placeholder. That's what stops an agent from using a token it got elsewhere to post your code to a gist.

## Trust in the worktree

The worktree is the one place the agent can write that you also act on. zoo is careful about three kinds of files there:

- **`zoo.yaml`** can grant services and unmask secrets, so zoo uses only a copy you've approved (`zoo trust`).
- **A Dockerfile** named by `zoo.yaml` runs with network access when built, so zoo builds only the version you approved, with an empty build context.
- **Git.** Hooks and config are read-only, but the agent can still create a nested `.git`, a bare repository, or a `.git` file pointing elsewhere, any of which brings config your git (or your shell prompt or editor) would honour. It can also move branches besides its own, since it has to write refs to commit. Mounts can't stop either, so zoo looks: it snapshots refs when a session starts, and checks after each session, every 30 seconds in the daemon, and in `zoo doctor`, telling you how to undo a moved branch.

On Windows, a host path like `C:\Users\you\repo` can't be mounted at the same path, so the cage sees `/c/Users/you/repo`, with rewritten read-only copies of git's pointer files and your `core.autocrlf` passed through.

## Previews and SSH without opening ports

Both need to reach into a cage from your machine, and your machine often can't route to cages (Docker Desktop, internal networks). So neither uses the network: they run over the runtime's exec, the same channel `zoo run` uses.

- **Previews:** zood answers `http://<port>.<cage>.localhost:7779` and, per connection, runs `socat` inside the cage to the dev server's loopback port.
- **SSH:** `ssh zoo-<cage>` runs `zoo ssh-proxy`, which execs `sshd -i` inside the cage as `agent`. Nothing listens anywhere; a non-root sshd can only log in as itself.

## Proving it

`zoo doctor` runs the escape suite inside a cage as the agent: direct egress, DNS, proxy bypass, a non-allowlisted host, non-web ports, masks, read-only git paths, the rename pin, process limits, extra network interfaces and routes, your machine's exposed ports, the dashboard. `--deep` scans for other cages, your LAN, tailnet and cloud metadata, directly and through the keeper. CI runs it on Linux with Docker on every push.

## Why these choices

- **A proxy, not a firewall.** Hostnames are what you want to approve, and a proxy sees them without decrypting anything. squid is mature, reloads without dropping connections, and logs well (D2).
- **A VM per cage by default.** A shared kernel means one kernel bug away from every other cage and your machine; containers are allowed, but only when you say so, and it's shown everywhere (D1, I12).
- **One cage per worktree.** That's how orchestrators work: a worktree lives for days and agents come and go (D3).
- **Host-side masks.** Masks applied from outside the guest can't be undone from inside, so cages can drop every capability (D10, D11).
- **Approve by hand, no learn mode.** An allow-everything mode lets the agent reach your machine and cloud metadata exactly when it matters most (D24).
- **Decrypt only what you name.** TLS interception everywhere would see everything and break pinned clients; decrypting only credential hosts, with a CA that can't sign for anything else, gets tokens out of cages without that (D35).
