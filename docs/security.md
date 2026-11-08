# Security

What zoo protects, what it doesn't, and how to check. How each wall works is in [How it works](how-it-works.md). To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Threat model

**The attacker is text the agent reads:** a web page, an issue, a PR comment, a README, dependency docs, tool output. It can make the agent run any command, as the agent user, inside the cage. zoo also assumes it might get root inside the guest through a kernel bug or misconfiguration, which is why the default is a VM per cage.

**zoo protects:** your machine, your secrets, your network, and other projects. It does **not** make the agent's output safe: code it writes can still be wrong or malicious, and runs on your machine when you run it.

## What a cage can and can't do

| | |
|---|---|
| Read and write your worktree | Yes |
| See secret files in the worktree: `.env*`, keys and keystores, cloud and cluster credentials, `*.tfvars`/`*.tfstate`, `secrets.*`, `.npmrc`, `.netrc` ([full list](guide.md#masks)), including copies in build output | No: empty, read-only files. Over 512 matches, zoo refuses to start the cage rather than hide only some |
| See your home directory, SSH keys, cloud credentials, Keychain, Docker socket | No: not mounted |
| See your GitHub token or model key | By default the model key is in the cage. With `credentials:` (and `cred:` for the model key), the cage holds only a placeholder |
| Write `.git/hooks`, `.git/config`, other worktrees' or submodules' git data | No: read-only, and `.git` can't be renamed to get around that |
| Write hooks where git config points into the worktree (`core.hooksPath=.husky`, `include.path`, filter scripts) | No: read-only |
| Plant a repository in the worktree (`sub/.git`, a bare repo, a `.git` file pointing elsewhere) | Can't be blocked; it's caught: `zoo run` warns when the session ends, the dashboard alerts within 30 s, `zoo doctor` fails |
| Change `zoo.yaml` or the Dockerfile to grant itself more | Ignored until you approve the change |
| Reach the internet directly | No: no route, no DNS |
| Reach allowlisted hosts | Yes, through the keeper, logged |
| Reach other hosts | No: refused, logged, alert |
| Reach your machine, LAN, tailnet or cloud metadata through the keeper | No, even for allowlisted names that resolve there |
| Reach them directly | No route; `zoo doctor --deep` probes them. Apple exception below |
| Reach other cages | Docker/Podman: no, each cage has its own network. Apple: cages share a network; `--deep` warns about anything listening |
| Reach a service | Only with a grant; logged |
| Use a granted service by forging another cage's address | No: the gate checks which keeper address it arrived on |
| Bring up a VPN to route around the keeper | No capabilities to create interfaces; `zoo doctor` fails on any extra interface or a default route |
| Gain capabilities, run setuid binaries | No: all capabilities dropped, no new privileges |
| Push to GitHub | Not by default (agents commit, you push). Through a credential: only its `push:` branches |
| Move a branch other than its own (`main`, `origin/*`, tags) | Can't be blocked in `git: rw` (refs are shared), so it's reported: at the end of each session, on the dashboard, in `zoo doctor` and `zoo refs`, with the command to undo it. `git: ro` prevents it |
| Post to an allowed host with a token from elsewhere (e.g. a gist) | Possible, unless that host has a `credentials:` policy like `readonly`. The bytes-out alert is the tripwire |
| Reach the dashboard | No: loopback only, token on writes |
| Use a lot of CPU, memory or processes | Up to the cage's size limits |

## Known gaps

- **Apple cages can see your machine's open ports.** On an internal network the host gateway is still reachable ([apple/container#1320](https://github.com/apple/container/issues/1320)), so anything on your Mac bound to `0.0.0.0` is reachable from a cage. `zoo doctor` lists them. Common culprit: AirPlay Receiver (ports 5000 and 7000): turn it off in System Settings → General → AirDrop & Handoff. Bind dev servers to `127.0.0.1`. Host services you *want* cages to use go through [services](guide.md#services) and can stay on loopback.
- **Allowed hosts can carry data out.** A `credentials:` policy closes that for its hosts; elsewhere the bytes-out alert (5 MB per host by default) is the tripwire and won't notice a few KB.
- **Branch moves are reported, not prevented**, in `git: rw` mode, and zoo can't tell who moved a ref: a `git fetch` you run on the host while a cage runs shows up too (`zoo refs ok` accepts it). Repos using reftable ref storage aren't watched. `git: ro` prevents moves outright.
- **Planted repositories are detected, not prevented.** A shell prompt or editor running git in the worktree *while* the agent works can beat the 30-second scan. `git config --global safe.bareRepository explicit` closes the bare-repo variant for good.
- **Code the agent writes runs when you run it.** `package.json` scripts, tests, Makefiles: zoo doesn't review them.
- **Docker and Podman cages share a kernel.** That's why the isolation floor refuses them by default. On macOS and Windows that kernel is Docker Desktop's (or OrbStack's) Linux VM, not your machine's.
- **Credential hosts are decrypted in the keeper**, by design, so it can add the token and enforce policy. Only those hosts, with a CA that can't sign for anything else. Clients that pin certificates won't work against them.
- **The model token is in the cage** unless you use `cred:`. Use a dedicated, revocable token either way.
- **A project's package cache survives `zoo reset`.** If you think an agent was compromised, `zoo cache clear <cage>` too. A `shared` cache can be poisoned by any cage using it.
- **Masks are fixed while a cage runs.** A secret file added later is readable in a running cage (`zoo run` warns); the next start hides it.
- **Attribution is per cage**, not per agent, when several agents share one.
- **No disk limit per cage yet.**

## Checking it yourself

```sh
zoo doctor              # the escape suite, from inside the cage, as the agent
zoo doctor --deep       # plus scans for other cages, your LAN, tailnet and metadata
zoo doctor --json       # one {verdict, check, detail} per line, for CI
```

`zoo doctor` also lists hooks and command-running git config in the repo, and checks that cages trust the keeper's CA for credential hosts. The same suite runs in CI on Linux with Docker on every push.

## Good habits

- Use dedicated, revocable tokens; prefer fine-grained GitHub tokens scoped to the repos you mean.
- Grant for a while, not for good: `zoo grant --for 4h`, `zoo allow --for 1h`.
- Keep the isolation floor at `microvm` unless you're on Docker or Podman and accept the shared kernel.
- Read the git warnings zoo prints for a repo you just downloaded, and use `git: none` for ones you don't trust.
- Use the VS Code settings in [SSH and editors](guide.md#ssh-and-editors) before opening a cage in your editor.
