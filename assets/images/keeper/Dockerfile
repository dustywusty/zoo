# zoo keeper image: squid (web egress) + zoo-gate (granted services), the only
# ways out of a cage. Attached to the cage network(s) and one with a route out.
FROM golang:1.24-bookworm AS gate
WORKDIR /src
COPY gate/main.go .
RUN go mod init zoo-gate && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /zoo-gate .

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends squid ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=gate /zoo-gate /usr/local/bin/zoo-gate
COPY keeper-entrypoint /usr/local/bin/keeper-entrypoint
RUN chmod 0755 /usr/local/bin/keeper-entrypoint /usr/local/bin/zoo-gate

EXPOSE 3128
ENTRYPOINT ["/usr/local/bin/keeper-entrypoint"]
