#!/usr/bin/env bash
set -euo pipefail

repository_root=$(git rev-parse --show-toplevel)
cd "$repository_root"
build_root=$(mktemp -d "${TMPDIR:-/tmp}/agentbridge-verify.XXXXXX")
cleanup() {
  rm -rf "$build_root"
}
trap cleanup EXIT

go test -race ./...
make lint
GOOS=linux GOARCH=amd64 go build -o "$build_root/agentbridge-linux-amd64" ./cmd/agentbridge
GOOS=linux GOARCH=arm64 go build -o "$build_root/agentbridge-linux-arm64" ./cmd/agentbridge
GOOS=darwin GOARCH=arm64 go build -o "$build_root/agentbridge-darwin-arm64" ./cmd/agentbridge
bash scripts/check-public-boundary-test.sh
bash scripts/check-public-boundary.sh --generic

if [[ -d protocol ]]; then
  (
    cd protocol
    go test -race ./...
    go vet ./...
  )
fi
