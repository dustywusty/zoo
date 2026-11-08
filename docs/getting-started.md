# Getting started

From nothing to an agent working in a cage, in about ten minutes (most of it the first image build).

## 1. Install

```sh
brew install dustywusty/tap/zoo                 # macOS and Linux
```

Other ways:

- **Debian/Ubuntu, Fedora/RHEL:** `.deb` and `.rpm` on the [releases page](https://github.com/dustywusty/zoo/releases/latest): `sudo dpkg -i zoo_<version>_linux_amd64.deb` or `sudo rpm -i zoo_<version>_linux_amd64.rpm`.
- **Tarball:** darwin and linux, amd64 and arm64, on the same page. Unpack and put `zoo` on your PATH.
- **From source:** Go 1.24+, then `go build -o ~/.local/bin/zoo ./cmd/zoo`.

<details>
<summary>Verifying a download</summary>

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify zoo_<version>_linux_amd64.tar.gz --repo dustywusty/zoo     # build provenance
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/dustywusty/zoo/\.github/workflows/release\.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
</details>

## 2. Pick a backend

zoo runs cages on one of these:

| Backend | Runs on | Isolation |
|---|---|---|
| `apple` | macOS 26+ on Apple silicon, with [`container`](https://github.com/apple/container/releases) installed | **vm**: every cage is its own VM |
| `docker` | OrbStack, Docker Desktop, colima or Docker Engine, macOS or Linux | **container**: cages share one kernel |
| `podman` | Podman (a machine on macOS, native on Linux) | **container** |
| `docker` on Windows | Docker Desktop with the WSL 2 backend | **container** (Docker Desktop's Linux VM kernel, not Windows') |

`zoo backend` shows what's installed and ready. zoo picks the strongest one automatically.

**Using Docker or Podman?** By default zoo refuses cages that share a kernel (the "isolation floor" is `microvm`). Accept container isolation once and zoo remembers it:

```sh
zoo up --backend docker --isolation-floor container
```

On Linux, Docker with Kata (`docker.runtime: kata` in `~/.zoo/config.yaml`) gives every cage its own kernel again. With Podman, the dashboard reads it through its API service when one is running (Linux: `systemctl --user enable --now podman.socket`).

**On Windows**, use Docker Desktop and the same `--isolation-floor container`. Your worktree shows up inside the cage as `/c/Users/you/repo`; zoo handles git's paths and line endings so the cage sees the same clean tree you do. Secrets go to Windows Credential Manager, and shims are `.exe` files (see step 8). More in the [guide](guide.md#windows).

## 3. Start zoo

```sh
zoo up
```

This builds the cage and keeper images the first time (a few minutes), starts the keeper and the dashboard daemon, and prints where they are. `zoo status` shows the same any time.

## 4. Give the agent its model token

```sh
claude setup-token        # on your machine: a long-lived token for Claude Code
zoo keys set claude       # paste it; stored in your Keychain (Credential Manager on Windows, a 0600 file on Linux)
```

For Codex: `zoo keys set codex` (an API key) or `zoo keys set codex-auth < ~/.codex/auth.json` (a ChatGPT login).

Use a dedicated token you can revoke. To keep the token out of the cage entirely, see [Credentials](guide.md#credentials-that-never-enter-a-cage).

## 5. Run an agent

```sh
cd ~/src/your-repo
zoo run claude
```

zoo creates a cage for this worktree (or reuses it), and runs Claude Code inside as an unprivileged user. What you'll see the first time:

- **Secrets hidden:** "hiding N secret file(s)": `.env` files, keys, cloud and cluster credentials, Terraform state and similar read as empty inside, including copies in build output.
- **Git warnings, if any:** zoo lists hooks and git config in the repo that run commands when *you* use git. A repo you downloaded (say, a take-home assignment) can carry a hook that runs a payload at your first commit; check the list.
- **A `zoo.yaml` prompt, if the repo has one:** agents can edit that file, so zoo only uses a version you approve. It shows the file and asks.

Run `zoo run claude` again in the same worktree (or from another terminal) and you join the same cage.

## 6. Approve what the agent needs

The keeper allows nothing by default except model APIs, GitHub and the main package registries. When the agent hits something else, the request is refused and shows up red on the dashboard:

```sh
zoo dash                 # opens http://127.0.0.1:7777
```

Click **Allow** on a red row (it asks for how long: `4h`, `2d`, or empty for good), or from the terminal:

```sh
zoo blocked              # hosts refused so far, most-requested first
zoo allow registry.yarnpkg.com
zoo allow --for 2h some-docs-site.com
```

Changes apply to running cages at once; the agent just retries.

## 7. Check the walls

```sh
zoo doctor               # every check from inside the cage
zoo doctor --deep        # also probes for your LAN, other cages, cloud metadata (~10 s)
```

All green means: no direct internet, no DNS, secrets masked, `.git` hooks and config read-only, process limits set, dashboard unreachable from the cage.

## 8. Hook up your tools

To make Conductor, herdr, Orca's local tabs or plain `claude` in any terminal land in cages:

```sh
zoo shims install
# then add as the LAST line of ~/.zshrc and ~/.zprofile:
export PATH="$HOME/.zoo/shims:$PATH"
```

Restart the app afterwards. GUI apps that don't read your shell's PATH can point their agent path setting at `~/.zoo/shims/claude`. `ZOO=off claude` runs the real CLI outside any cage.

On Windows, `zoo shims install --path` puts `%USERPROFILE%\.zoo\shims` (with `claude.exe`, `codex.exe`) first on your user PATH; restart Orca and your terminals. `set ZOO=off` runs the real CLI.

Editors (VS Code, Cursor, JetBrains Gateway, Orca remote mode) can open a cage over SSH: see [SSH and editors](guide.md#ssh-and-editors).

## Next

- Give an agent a database or GitHub without giving it the token: [Services](guide.md#services) and [Credentials](guide.md#credentials-that-never-enter-a-cage).
- Open each agent's dev server side by side: [Previews](guide.md#previewing-dev-servers).
- Tune a repo: [`zoo.yaml`](guide.md#per-repo-settings-zooyaml).
- Understand what you're relying on: [Security](security.md).
