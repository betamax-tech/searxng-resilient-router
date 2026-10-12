#!/usr/bin/env bash
# Build the static Go binaries. Requires Go (see README for install).
set -euo pipefail
cd "$(dirname "$0")"

export PATH="${HOME}/.local/go/bin:${PATH}"
export GOPATH="${GOPATH:-${HOME}/go}"
export GOCACHE="${GOCACHE:-${HOME}/.cache/go-build}"
export CGO_ENABLED=0

echo "Building searxng-router…"
# NOTE: the router is split across router.go + analytics.go (both package main,
# only router.go has main()). Both MUST be compiled together.
go build -ldflags="-s -w" -o searxng-router router.go analytics.go

echo "Building proxypool…"
go build -ldflags="-s -w" -o proxypool proxypool.go

echo "Done:"
ls -lh searxng-router proxypool
