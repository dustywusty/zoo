
# --- zoo cage overlay -------------------------------------------------------
# Appended by zoo after `FROM <image:>` or after your zoo.yaml `dockerfile:`,
# so it continues the final stage. It adds what every cage needs: the
# unprivileged agent user with your uid/gid, zoo's init and session
# wrappers, bash/git/socat/ca-certificates/curl, and the cage environment.
# No line continuations, so a `# escape=` directive above can't break it.
USER root
ARG AGENT_UID=501
ARG AGENT_GID=20
COPY zoo-setup zoo-init zoo-doctor zoo-session /usr/local/bin/
RUN ["/bin/sh", "/usr/local/bin/zoo-setup"]
ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_AUTOUPDATER=1 NO_PROXY=localhost,127.0.0.1 no_proxy=localhost,127.0.0.1 ZOO_INSIDE=1
WORKDIR /
ENTRYPOINT ["/usr/local/bin/zoo-init"]
CMD []
