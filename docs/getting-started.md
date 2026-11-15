# Getting started

From nothing to an agent working in a cage, in about ten minutes (most of it the first image build).

## 1. Install

```sh
brew install dustywusty/tap/zoo                 # macOS and Linux
```

Other ways, from the [releases page](https://github.com/dustywusty/zoo/releases/latest):

- **Debian/Ubuntu, Fedora/RHEL:** `sudo dpkg -i zoo_<version>_linux_amd64.deb` or `sudo rpm -i zoo_<version>_linux_amd64.rpm` (`arm64` files for ARM).
- **Tarball:** darwin and linux, amd64 and arm64. Unpack and put `zoo` on your PATH.
- **From source:** git and Go 1.24+, then `git clone https://github.com/dustywusty/zoo && cd zoo && make install` (builds into `~/.local/bin`; put that on your PATH).
- **Windows:** no release package yet. Build from source: see [Install on Windows](#install-on-windows).

If Homebrew says "Your Command Line Tools are too outdated", update them from Software Update in System Settings (or `sudo rm -rf /Library/Developer/CommandLineTools && sudo xcode-select --install`), then try again.

<details>
<summary>Verifying a download</summary>

Every release has checksums, a keyless cosign signature over them, and GitHub build provenance:

```sh
shasum -a 256 --check --ignore-missing checksums.txt          # sha256sum on Linux
gh attestation verify zoo_<version>_darwin_arm64.tar.gz --repo dustywusty/zoo
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/dustywusty/zoo/\.github/workflows/release\.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
</details>

### Install on Windows

zoo runs on Windows 10 and 11 with Docker Desktop. The releases do not include a Windows build yet, so you build `zoo.exe` from source. Run these steps in PowerShell.

1. Install Docker Desktop, Git for Windows, and Go:

   ```powershell
   winget install Docker.DockerDesktop Git.Git GoLang.Go
   ```

2. Start Docker Desktop. In its Settings, under General, make sure that "Use the WSL 2 based engine" is on.
3. Open a new PowerShell window, so that it finds `git` and `go`.
4. Build zoo:

   ```powershell
   git clone https://github.com/dustywusty/zoo
   cd zoo
   go build -o zoo.exe ./cmd/zoo
   ```

5. Copy `zoo.exe` to a folder in your user PATH. These commands use `%USERPROFILE%\.local\bin` and add it to your user PATH if it is not there:

   ```powershell
   $bin = "$env:USERPROFILE\.local\bin"
   New-Item -ItemType Directory -Force $bin | Out-Null
   Copy-Item zoo.exe $bin
   $p = [Environment]::GetEnvironmentVariable('Path', 'User')
   if (($p -split ';') -notcontains $bin) { [Environment]::SetEnvironmentVariable('Path', "$p;$bin", 'User') }
   ```

6. Open a new PowerShell window. Run `zoo version`, then `zoo backend`. The `docker` backend must show as ready.

If `zoo` stops with "An Application Control policy has blocked this file", Smart App Control is on. Smart App Control is a Windows feature that blocks programs that have no code signature. A `zoo.exe` that you build yourself has no signature. The shims from step 8 are copies of `zoo.exe`, so Windows blocks them too. To see the setting, open Windows Security, then App & browser control, then Smart App Control settings. zoo does not run while this setting is on. On some Windows versions, you cannot turn Smart App Control back on after you turn it off, unless you reset Windows.

Then continue with step 3 below. On Windows, start zoo with `zoo up --backend docker --isolation-floor container`. Step 2 tells why.

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

On Windows, run `zoo shims install --path`. This command puts `%USERPROFILE%\.zoo\shims` (with `claude.exe` and `codex.exe`) first in your user PATH. Then close Orca and all terminals, and open them again. After that, `claude` in an Orca local tab runs in a cage. To run the real CLI outside a cage, set `ZOO=off`: `$env:ZOO = 'off'` in PowerShell, or `set ZOO=off` in cmd.

Editors (VS Code, Cursor, JetBrains Gateway, Orca remote mode) can open a cage over SSH: see [SSH and editors](guide.md#ssh-and-editors).

## Next

- Give an agent a database or GitHub without giving it the token: [Services](guide.md#services) and [Credentials](guide.md#credentials-that-never-enter-a-cage).
- Open each agent's dev server side by side: [Previews](guide.md#previewing-dev-servers).
- Tune a repo: [`zoo.yaml`](guide.md#per-repo-settings-zooyaml).
- Understand what you're relying on: [Security](security.md).
