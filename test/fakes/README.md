# Fake container CLIs

`fakectr/` is one Go program that pretends to be Apple's `container` CLI
(1.6.1) or the `docker` CLI (29, as on OrbStack); `podman` is the docker
dialect with podman's `info` shape (unverified against real podman). It keeps
its state in a JSON file and runs `exec` commands on the host, so zoo's
orchestration can be tested end to end without a VM or a daemon.

- The dialect comes from the name it runs as (`container`, `docker`, `podman`),
  or from `ZOO_FAKE_DIALECT=apple|docker|podman`.
- Output shapes (`list`/`inspect`/`stats` JSON, docker's `{{json .}}` lines and
  error messages) follow the real CLIs.
- Unknown commands or flags exit 2 with a clear message, so a backend calling
  something the real CLI wouldn't accept fails loudly.
- `exec` runs the command on the host with the container's env, `--env-file`
  and `-e` values merged, `-w` as the working directory if it exists, and
  `ZOO_FAKE_EXEC_USER` / `ZOO_FAKE_EXEC_WORKDIR` / `ZOO_FAKE_EXEC_ID` set.
- `fakestate/` is the state file format; tests read it to check what a backend
  asked for (mounts, cap-drop, DNS, pids, networks, every call's argv).

## Pointing zoo at it

```sh
go build -o /tmp/fakes/fakectr ./test/fakes/fakectr
ln -s fakectr /tmp/fakes/container
ln -s fakectr /tmp/fakes/docker

export ZOO_FAKE_STATE=/tmp/fakes/state.json    # required; created on first use
export ZOO_CONTAINER_BIN=/tmp/fakes/container  # apple backend
export ZOO_DOCKER_BIN=/tmp/fakes/docker        # docker/podman backend
```

Knobs:

| Env | Effect |
|---|---|
| `ZOO_FAKE_DOWN=1` | the runtime isn't running (`system status` / `info` fail) |
| `ZOO_FAKE_DOCKER_RUNTIME=kata` | docker `DefaultRuntime` (podman `ociRuntime.name`); default `runc` / `crun` |

Delete the state file to start over. The contract suite in
`internal/backend/contract` builds the fake and runs every backend against it.
