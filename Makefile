.PHONY: build test install dist snapshot

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 0.1.0-dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o zoo ./cmd/zoo

test:
	go test ./...

install: build
	install -d $(HOME)/.local/bin
	install -m 0755 zoo $(HOME)/.local/bin/zoo

dist:
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/zoo-darwin-arm64 ./cmd/zoo

# Local release dry run: needs goreleaser; nothing is published.
snapshot:
	goreleaser release --snapshot --clean
