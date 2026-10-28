# zoo keeper image: squid, the only way out of a cage.
# Attached to two networks: the internal cage network and the default (NAT) one.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends squid ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY keeper-entrypoint /usr/local/bin/keeper-entrypoint
RUN chmod 0755 /usr/local/bin/keeper-entrypoint

EXPOSE 3128
ENTRYPOINT ["/usr/local/bin/keeper-entrypoint"]
