# Contributing

Thanks for helping. zoo is a security tool, so the bar is: every change keeps the walls up, and proves it.

## Get going

You need Go 1.24+, git, and for anything beyond unit tests a container runtime (Docker/OrbStack/Podman, or Apple `container` on macOS 26+). `shellcheck` for the scripts.

```sh
git clone https://github.com/dustywusty/zoo && cd zoo
make build            # ./zoo
make test             # go test ./...  (no runtime needed; under a minute)
```

Before a PR, the same checks CI runs:

```sh
test -z "$(gofmt -l .)" && go vet ./... && go test ./...
shellcheck -s bash assets/images/cage/zoo-doctor
shellcheck -s sh assets/images/cage/zoo-init assets/images/cage-overlay/zoo-setup assets/images/keeper/keeper-entrypoint
```

CI uses an older shellcheck than Homebrew; if it disagrees, `docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:v0.9.0 …` matches it.

## Trying a change for real

Run a development build in its own sandbox so it doesn't touch your real `~/.zoo` or dashboard:

```sh
export ZOO_HOME=/tmp/zoo-dev ZOO_KEYS_FILE=1        # separate state; secrets in files, not your Keychain
mkdir -p $ZOO_HOME && printf 'port: 17777\n' > $ZOO_HOME/config.yaml
./zoo up --backend docker --isolation-floor container
cd /some/test/repo && /path/to/zoo doctor --deep
```

Caveat: instance names (`zoo-keeper`, `zoo-<cage>`, `zoo-cages*` networks) are per runtime, not per `ZOO_HOME`, so a dev keeper on the same runtime as your real one replaces it. Use the other runtime (Docker for development if you normally use Apple, or vice versa) or stop your real zoo first. `zoo down --all` and `docker rm -f $(docker ps -aq --filter name=zoo-)` clean up.

After changing anything under `assets/images/`, rebuild: `zoo build`, then `zoo reset <cage>`. If the change affects what cages or the keeper expect, bump the image tags in `internal/config/config.go` so existing installs pick it up.

## Where things are

| Path | What |
|---|---|
| `cmd/zoo` | The CLI: one file per area (`main.go` has run/prepare/doctor; `services.go`, `trust.go`, `ssh.go`, `preview.go`, `cache.go`, `image.go`…) |
| `internal/backend` | The runtime interface; `apple/` and `docker/` (also podman) drive the runtime CLIs. `contract/` is one behaviour table run against every backend |
| `internal/cage` | Planning a cage: worktree root, masks, git guard, pins, mounts, caches; git trap and risk scans |
| `internal/keeper` | squid config, the allowlist and its expiries, the access log |
| `internal/services` | Services, grants, the gate's config |
| `internal/inject` | Credentials: placeholders, the keeper CA, what cages get |
| `internal/daemon` | zood: log tailing, alerts, the host bridge, previews, the dashboard (`web/index.html`, one static page) |
| `internal/sshagent`, `internal/sshdoor` | The filtering SSH-agent proxy; the SSH door |
| `internal/config` | `config.yaml`, `zoo.yaml` and its trust |
| `internal/gitrefs` | Ref snapshots and the "branch moved" report |
| `internal/proc` | OS differences for zoo's own processes (detaching, no console windows on Windows) |
| `internal/backend/dockerapi` | The Docker Engine API client zood uses for Docker and Podman |
| `internal/images` | Custom cage images and Dockerfile approval |
| `assets/images` | Image sources embedded in the binary: `cage/`, `cage-overlay/`, `keeper/` (with `gate/`, the gate and credential injector, stdlib-only Go) |
| `test/fakes` | Fake `container`/`docker`/`podman` CLIs with recorded state, for testing without a runtime |
| `docs/` | User docs, the architecture page, and `BRIEF.md` |

## Tests

- **Unit tests** next to the code. Pure logic is tested directly; anything touching the filesystem uses `t.TempDir()` and `t.Setenv("ZOO_HOME", …)`.
- **Fake runtimes.** `test/fakes/fakectr` emulates the runtime CLIs (Apple 1.6.1 and docker 29 dialects), rejects anything it doesn't know, and records state. Point a backend at it with `ZOO_CONTAINER_BIN` / `ZOO_DOCKER_BIN`.
- **Contract suite** (`internal/backend/contract`). Every backend must pass the same table: internal networks, no capabilities, no DNS, a process cap, read-only masks and git guard. A new backend starts here.
- **Escape suite** (`assets/images/cage/zoo-doctor`), run by `zoo doctor` inside a real cage. CI runs it on Linux with Docker against a main checkout and a linked worktree. Add a check here when you add a wall.
- **CI** also runs the whole suite natively on Windows (`windows-unit`) and the daemon's Engine API path against real Docker and Podman (`engine-api`). The escape suite on Windows is run by hand.
- **The keeper's gate and injector** (`assets/images/keeper/gate`) have their own tests, including real TLS round trips and a real `git push` through the injector.

A security change needs a test that fails without it: a planted path that's writable, a request that gets through, a forged placeholder that's accepted.

## How changes are made

[`docs/BRIEF.md`](docs/BRIEF.md) is the source of truth for what zoo is: invariants (section 3), threat model (4), milestones (14) and the decision log (15). When a change alters a decision, add a `D<n>` entry in the same commit. Never weaken an invariant to make something work; if one would have to bend, open an issue first.

- **Commits:** one logical change each, message `<area>: <what>` with a body that says why (e.g. `Keeper: refuse pushes in git's protocol so git shows why`).
- **Dependencies:** prefer the standard library. Today there's `goccy/go-yaml` and `golang.org/x/crypto`; a new one needs a reason in the commit message.
- **Subprocesses:** wrap `exec.Command` that runs in the background with `proc.Quiet(cmd)`, or Windows flashes a console window.
- **Paths into a cage:** host paths go through `backend.GuestPath` (Windows maps `C:\x` to `/c/x`); backends translate mounts, but anything else you pass in (workdirs, env) has to do it itself.
- **Secrets:** never on a command line, in a log, in an image, or in a fixture that looks real.
- **Messages:** plain words, specific, and say what to do next (`… needs macOS 26+; on older macOS use --backend docker`).
- **Docs:** user-visible changes update `docs/guide.md` (and the README if they change the first five minutes).

**Security checklist for anything touching cages, the keeper or zood:** invariants I1–I12 still hold; `zoo doctor` passes; no new host→cage mount; no new port a cage can reach; no secret in argv or logs; anything an agent can write (the worktree, `zoo.yaml`, a Dockerfile) isn't trusted without your approval.

## Releases

Tagging `v*` runs `.github/workflows/release.yml`: tests, then GoReleaser builds tarballs, `.deb`/`.rpm` and the Homebrew formula, signs `checksums.txt` with keyless cosign, and attests build provenance. `make snapshot` builds the same locally without publishing.
