#!/usr/bin/env bash
# Builds the release binaries and their checksums into dist/.
#
# usage: scripts/build.sh <version>
set -euo pipefail

version=${1:?usage: scripts/build.sh <version>}
targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)

rm -rf dist
mkdir dist
for target in "${targets[@]}"; do
  os=${target%/*} arch=${target#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "dist/selfhostctl_${version}_${os}_${arch}" ./cmd/selfhostctl
done
(cd dist && sha256sum selfhostctl_* >checksums.txt)
