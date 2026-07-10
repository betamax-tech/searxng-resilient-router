#!/usr/bin/env bash
# Build all three static Go binaries. Requires Go (see README for install).
set -euo pipefail
cd "$(dirname "$0")"

export PATH="${HOME}/.local/go/bin:${PATH}"
export GOPATH="${GOPATH:-${HOME}/go}"
export GOCACHE="${GOCACHE:-${HOME}/.cache/go-build}"
export CGO_ENABLED=0

echo "Building searxng-router…"
go build -ldflags="-s -w" -o searxng-router router.go

echo "Building proxypool…"
go build -ldflags="-s -w" -o proxypool proxypool.go

echo "Building deadletter-retry…"
go build -ldflags="-s -w" -o deadletter-retry deadletter_retry.go

echo "Done:"
ls -lh searxng-router proxypool deadletter-retry
