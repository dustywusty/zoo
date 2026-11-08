# zoo

**Run AI coding agents in small sandboxes whose only way out is a proxy you control, and watch everything they do.**

Coding agents read web pages, issues and dependency docs, and any of that text can tell them to run commands. zoo assumes that will happen and limits the damage: each agent works in a **cage** (a small VM or container) that can see your project and nothing else from your machine, and can only reach the internet through the **keeper**, a proxy that allows the hosts you approve, logs every request and keeps your tokens out of the cage.

![zoo dashboard](docs/images/dashboard-dark.png)

- **Works with any agent:** Claude Code, Codex, pi, opencode, or anything that runs as a CLI.
- **Works with any launcher:** your terminal, Conductor, herdr, Orca, scripts. Shims catch `claude` and `codex` wherever they start; no tool needs to know zoo exists.
- **Runs on your machine:** Apple `container` (a VM per cage) on macOS 26+, Docker/OrbStack/Podman on macOS and Linux, or Docker Desktop on Windows.

## Quick start

```sh
brew install dustywusty/tap/zoo     # or .deb/.rpm, a tarball, or from source: docs/getting-started.md
zoo up                              # starts the keeper and dashboard; first run builds images (a few minutes)

claude setup-token                  # a long-lived Claude Code token
zoo keys set claude                 # paste it; stored in your Keychain

cd ~/src/your-repo
zoo run claude                      # Claude Code, caged
zoo dash                            # the dashboard: cages, resources, every request
```

The first time, the agent will hit hosts that aren't approved yet. Approve the ones you trust with **Allow** on the red row in the dashboard, or `zoo blocked` then `zoo allow <host>`. `zoo doctor` proves the walls hold from inside the cage.

Using Docker, Podman, or Windows? See [Getting started](docs/getting-started.md#2-pick-a-backend).

## What a cage stops

| An agent that's been talked into… | …gets |
|---|---|
| Reading `.env`, keys, `~/.ssh`, cloud credentials | Empty files; your home directory isn't there at all |
| Sending your code to some server | A 403 from the keeper, an alert on the dashboard |
| Using your GitHub token elsewhere | A placeholder: the real token stays in the keeper ([credentials](docs/guide.md#credentials-that-never-enter-a-cage)) |
| Pushing to `main` | Refused, when pushes go through a credential with `push: ["agent/*"]` |
| Quietly moving `main` (or `origin/*`, or a tag) in your repo | Reported when the session ends, on the dashboard and in `zoo refs`, with the command to undo it |
| Planting a git hook that runs on your Mac later | Read-only `.git`; planted repositories flagged |
| Reaching your LAN, other cages or cloud metadata | No route |
| Granting itself more via `zoo.yaml` | Ignored until you approve the change |

The full list, and what zoo doesn't stop, is in [Security](docs/security.md).

## Docs

| | |
|---|---|
| [Getting started](docs/getting-started.md) | Install, pick a backend, first run, approving hosts, hooking up your tools |
| [Guide](docs/guide.md) | Everything you can do: commands, `zoo.yaml`, services, credentials, previews, SSH and editors, caches, custom images, dashboard, config reference, troubleshooting |
| [How it works](docs/how-it-works.md) | The pieces, how a request flows, and why it's built this way |
| [Security](docs/security.md) | Threat model, what a cage can and can't do, known gaps |
| [Architecture](docs/architecture.md) | Trust boundaries, what can reach what, and sequence diagrams |
| [Contributing](CONTRIBUTING.md) | Building, testing, and how changes are made |
| [Design brief](docs/BRIEF.md) | Invariants, decisions and roadmap: the source of truth for what zoo is |

## Status

zoo is young and moving fast. It works day to day on Apple `container` and Docker; the [roadmap](docs/BRIEF.md#14-milestones-and-acceptance-gates) has what's next (per-project allowlists, history that survives restarts, agent session timelines, Kubernetes). Report security issues as described in [SECURITY.md](SECURITY.md).
