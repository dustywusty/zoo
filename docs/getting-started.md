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
- **Windows:** a zip with a signed `zoo.exe`. See [Install on Windows](#install-on-windows).

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

On Windows, `Get-AuthenticodeSignature zoo.exe` in PowerShell shows the signature on `zoo.exe`. The status must be `Valid`, and the signer must be SignPath Foundation.
</details>

### Install on Windows

zoo runs on Windows 10 and 11 with Docker Desktop. Run these steps in PowerShell.

1. Install Docker Desktop:

   ```powershell
   winget install Docker.DockerDesktop
   ```

2. Start Docker Desktop. In its Settings, under General, make sure that "Use the WSL 2 based engine" is on.
3. From the [releases page](https://github.com/dustywusty/zoo/releases/latest), download `zoo_<version>_windows_amd64.zip`. On a Windows computer with an ARM processor, download `zoo_<version>_windows_arm64.zip`. Releases after v0.1.1 include these files.
4. Copy `zoo.exe` from the zip to a folder in your user PATH. These commands use `%USERPROFILE%\.local\bin` and add it to your user PATH if it is not there. Replace the zip name with the name of the file that you downloaded:

   ```powershell
   $bin = "$env:USERPROFILE\.local\bin"
   Expand-Archive "$env:USERPROFILE\Downloads\zoo_<version>_windows_amd64.zip" "$env:TEMP\zoo" -Force
   New-Item -ItemType Directory -Force $bin | Out-Null
   Copy-Item "$env:TEMP\zoo\zoo.exe" $bin -Force
   $p = [Environment]::GetEnvironmentVariable('Path', 'User')
   if (($p -split ';') -notcontains $bin) { [Environment]::SetEnvironmentVariable('Path', "$p;$bin", 'User') }
   ```

5. Open a new PowerShell window. Run `zoo version`, then `zoo backend`. The `docker` backend must show as ready.

Then continue with step 3 below. On Windows, start zoo with `zoo up --backend docker --isolation-floor container`. Step 2 tells why.

The `zoo.exe` in a release is signed through SignPath Foundation (see the [code signing policy](code-signing-policy.md)). A signed file can run when Smart App Control is on. Smart App Control is a Windows feature that blocks programs that have no code signature.

<details>
<summary>Build zoo.exe from source</summary>

1. Install Git for Windows and Go: `winget install Git.Git GoLang.Go`. Then open a new PowerShell window.
2. Build zoo:

   ```powershell
   git clone https://github.com/dustywusty/zoo
   cd zoo
   go build -o zoo.exe ./cmd/zoo
   ```

3. Copy `zoo.exe` to a folder in your PATH, as in step 4 above.

A `zoo.exe` that you build yourself has no signature. If Smart App Control is on, `zoo` stops with "An Application Control policy has blocked this file". The shims from step 8 are copies of `zoo.exe`, so Windows blocks them too. To see the setting, open Windows Security, then App & browser control, then Smart App Control settings. On some Windows versions, you cannot turn Smart App Control back on after you turn it off, unless you reset Windows.
</details>

<details>
<summary>Uninstall zoo on Windows</summary>

1. Stop zoo and all cages: `zoo down --all`.
2. Delete each cage: run `zoo ls`, then `zoo reset <cage>` for each cage in the list.
3. Remove the shims and take them out of your PATH: `zoo shims uninstall`.
4. Remove the secrets that zoo stored: run `zoo keys ls`, then `zoo keys rm <name>` for each name.
5. Remove the images that zoo built. List them with `docker image ls "zoo-*"`, then remove each one with `docker image rm <image>`.
6. Delete the zoo folder and the program:

   ```powershell
   Remove-Item -Recurse -Force "$env:USERPROFILE\.zoo"
   Remove-Item "$env:USERPROFILE\.local\bin\zoo.exe"
   ```

The `Remove-Item` commands delete your zoo configuration, logs, and allowlist. They cannot be undone.
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

On Windows, run `zoo shims install --path`. This command puts `%USERPROFILE%\.zoo\shims` (with `claude.exe` and `codex.exe`) first in your user PATH. Then close Orca and all terminals, and open them again. After that, `claude` in an Orca local tab runs in a cage. To run the real CLI outside a cage, set `ZOO=off`: `$env:ZOO = 'off'` in PowerShell, or `set ZOO=off` in cmd.

Editors (VS Code, Cursor, JetBrains Gateway, Orca remote mode) can open a cage over SSH: see [SSH and editors](guide.md#ssh-and-editors).

## Next

- Give an agent a database or GitHub without giving it the token: [Services](guide.md#services) and [Credentials](guide.md#credentials-that-never-enter-a-cage).
- Open each agent's dev server side by side: [Previews](guide.md#previewing-dev-servers).
- Tune a repo: [`zoo.yaml`](guide.md#per-repo-settings-zooyaml).
- Understand what you're relying on: [Security](security.md).
