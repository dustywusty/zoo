# zoo cage image: the VM every agent runs in.
# Built by `zoo build`. AGENT_UID/AGENT_GID default to your Mac user so files
# the agent writes in your worktree stay owned by you.
FROM node:22-bookworm-slim

ARG AGENT_UID=501
ARG AGENT_GID=20

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash ca-certificates curl git git-lfs jq less openssh-client procps \
      python3 python3-pip python3-venv ripgrep socat unzip util-linux xz-utils \
 && rm -rf /var/lib/apt/lists/*

# Python tooling and agent CLIs, installed at build time (with network),
# never at agent runtime.
RUN pip3 install --no-cache-dir --break-system-packages uv \
 && npm install -g --no-fund --no-audit @anthropic-ai/claude-code @openai/codex \
 && npm cache clean --force

# The unprivileged agent user. Uses your Mac uid; if that gid is taken in the
# image (20 is "dialout" in Debian), reuse the existing group.
RUN set -eux; \
    userdel -r node 2>/dev/null || true; \
    if ! getent group "${AGENT_GID}" >/dev/null; then groupadd -g "${AGENT_GID}" agent; fi; \
    useradd -m -u "${AGENT_UID}" -g "${AGENT_GID}" -s /bin/bash agent; \
    mkdir -p /etc/zoo /run/zoo; \
    : > /etc/zoo/masked; chmod 0444 /etc/zoo/masked

# Skip Claude Code's first-run onboarding in fresh cages.
RUN printf '{"hasCompletedOnboarding":true,"autoUpdates":false}\n' > /home/agent/.claude.json \
 && chown "${AGENT_UID}:${AGENT_GID}" /home/agent/.claude.json

COPY zoo-init /usr/local/bin/zoo-init
COPY zoo-doctor /usr/local/bin/zoo-doctor
COPY zoo-session /usr/local/bin/zoo-session
RUN chmod 0755 /usr/local/bin/zoo-init /usr/local/bin/zoo-doctor /usr/local/bin/zoo-session

ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_AUTOUPDATER=1 \
    NO_PROXY=localhost,127.0.0.1 \
    no_proxy=localhost,127.0.0.1 \
    ZOO_INSIDE=1

# Runs as root only to set up masks and read-only paths, then idles.
# Agents are started with `container exec -u agent`.
ENTRYPOINT ["/usr/local/bin/zoo-init"]
