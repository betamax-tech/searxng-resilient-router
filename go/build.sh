#!/usr/bin/env bash
# Build the static Go binaries. Requires Go (see README for install).
set -euo pipefail
cd "$(dirname "$0")"

export PATH="${HOME}/.local/go/bin:${PATH}"
export GOPATH="${GOPATH:-${HOME}/go}"
export GOCACHE="${GOCACHE:-${HOME}/.cache/go-build}"
export CGO_ENABLED=0

echo "Building searxng-router…"
# NOTE: the router is split across multiple package-main files (only
# router.go has main()). ALL must be compiled together:
#   router.go           - front-door HTTP handlers, SearXNG direct/proxied tiers
#   analytics.go        - event log + /stats
#   providers.go        - Provider interface + registry
#   provider_exa.go      \
#   provider_tavily.go    } paid provider adapters
#   provider_brave.go     }
#   provider_tinyfish.go /
#   caps.go             - per-provider spend/usage cap limiter (disk-persisted)
#   tiers.go            - tier=crucial|standard|bulk classification + shadow hook
#   searchcache.go      - 24h disk cache for the tiered router
#   tiered_router.go     - the provider/tier waterfall itself
go build -ldflags="-s -w" -o searxng-router \
	router.go analytics.go \
	providers.go provider_exa.go provider_tavily.go provider_brave.go provider_tinyfish.go \
	caps.go tiers.go searchcache.go tiered_router.go

echo "Building proxypool…"
# settingsguard.go is required: proxypool.go's writeSettings() calls
# settingsHasCoreSections() from it (shared with the router build/tests).
go build -ldflags="-s -w" -o proxypool proxypool.go settingsguard.go

echo "Done:"
ls -lh searxng-router proxypool
