#!/usr/bin/env bash
# Builds zoo.exe for Windows (amd64 and arm64), with version metadata in
# each exe. SignPath requires the metadata: it signs only a file whose
# product name is "zoo" and whose product version matches the release.
#
# usage: scripts/build-windows.sh <version> <outdir>
# writes <outdir>/amd64/zoo.exe and <outdir>/arm64/zoo.exe
set -euo pipefail

version=${1:?usage: build-windows.sh <version> <outdir>}
out=${2:?usage: build-windows.sh <version> <outdir>}

# Windows version fields are four numbers: 0.1.2-rc1 becomes 0.1.2.0.
numeric=${version%%[-+]*}
case $numeric in
  *[!0-9.]* | "" ) echo "build-windows: version $version does not start with numbers" >&2; exit 1 ;;
esac
while [ "$(echo "$numeric" | tr -cd . | wc -c)" -lt 3 ]; do numeric=$numeric.0; done

commit=$(git rev-parse HEAD)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
winres=github.com/tc-hib/go-winres@v0.3.3

cd "$(git rev-parse --show-toplevel)"
trap 'rm -f cmd/zoo/rsrc_windows_*.syso' EXIT

# go build links the .syso files in cmd/zoo into the matching GOARCH.
go run "$winres" simply \
  --arch amd64,arm64 \
  --out cmd/zoo/rsrc \
  --manifest cli \
  --product-name zoo \
  --product-version "$numeric" \
  --file-version "$numeric" \
  --file-description "zoo: run AI coding agents in sandboxes" \
  --original-filename zoo.exe \
  --copyright "Copyright the zoo authors. Apache License 2.0."

for arch in amd64 arm64; do
  mkdir -p "$out/$arch"
  GOOS=windows GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date" \
    -o "$out/$arch/zoo.exe" ./cmd/zoo
done
echo "build-windows: $out/amd64/zoo.exe and $out/arm64/zoo.exe, version $numeric"
# The release workflow gives this value to SignPath, which compares it
# with the product version in each exe.
if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "file-version=$numeric" >> "$GITHUB_OUTPUT"; fi
