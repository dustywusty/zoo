.PHONY: build test install dist

build:
	go build -o zoo ./cmd/zoo

test:
	go test ./...

install: build
	install -m 0755 zoo $(HOME)/.local/bin/zoo

dist:
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/zoo-darwin-arm64 ./cmd/zoo
