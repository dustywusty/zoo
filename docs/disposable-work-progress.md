# Disposable agent work

## Scope and state

The user approved these changes after the yoloai comparison on 2026-11-08.
Work resumed on 2026-11-10 in `hafgufa`, based on `d421c0c`.
The running Zoo installation is unchanged.

Implemented locally:

- Broker Claude tokens and Codex API keys through the keeper by default.
- Refuse Docker network creation failures without a shared-network fallback.
- Add `zoo task new/run/list/diff/apply/destroy` with separate project copies.
- Keep task review data outside agent mounts. Reject host conflicts on apply.
- Recover interrupted applies with a saved patch and pending baseline record.
- Add approved `network.allow` project policies and source-address Squid rules.
- Save dashboard events, alerts, counters, sessions, and log position across restarts.

## Validation

Passed on 2026-11-10:

- Full `go test ./...`, including the Docker/Podman network-failure contract.
- Race checks for task, CLI, daemon, and keeper packages.
- CLI lifecycle with a fake runtime: create, run, stop, dry-run, apply, destroy.
- Regression checks for host conflicts, secret exclusion, unsafe paths, interrupted
  apply recovery, approved-policy inheritance, running-cage refusal, and cache cleanup.
- History restart, attribution, cursor, corrupt-checkpoint, and live log-replacement checks.
- Project policy ordering, deny-all, configuration injection, and reload retry checks.
- `go vet ./...`, shellcheck, gofmt, `git diff --check`, and dashboard JavaScript syntax.
- Generated Squid configuration parsed in an isolated networkless container using
  `ZOO_TEST_SQUID_IMAGE=zoo-keeper:m4 go test ./internal/keeper -run TestProjectSquidSyntax -v`.

The guide, README, and decision log D41–D44 describe the changes.
The local build is available as `./zoo`; it has not been installed or deployed.

## Remaining runtime verification

`needs-hw-verify`: Run the real cage escape suite and live model sessions on an
isolated runtime before release. No live credentials or user keeper were used.
The new cage image tag is m5; the keeper image remains m4.

## Resume

Read this file, `git status`, and `git diff` before continuing.
Read `CONTRIBUTING.md` for repository checks and security requirements.
Do not start a development keeper on the Docker runtime: `zoo-keeper` is in use.
Runtime names collide even with a separate `ZOO_HOME`.
Use fake runtimes for CLI lifecycle tests. Apple container service is unavailable.
The isolated Squid syntax check uses an unnamed temporary container and removes it on exit.

## Known limits

Tasks use a fresh Git baseline. Source history and submodules are not copied.
Apply produces unstaged edits. It does not preserve agent commits.
Abrupt termination can leave a lock. Remove it only after its process exits.
Review/apply refuse running cages or unavailable task backends.
Codex subscription auth remains a direct-file fallback with a warning.
History is a bounded JSON checkpoint, not the planned SQLite event store.
