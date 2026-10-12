// settingsguard.go — shared, testable guard used by proxypool.go's
// writeSettings() to refuse writing back a settings*.yml that has already lost
// its core SearXNG config. This is the durability fix for the 2026-10-12
// regression: settings.proxied.yml was found reduced to ONLY the managed proxy
// block (outgoing/proxies), missing use_default_settings and the entire
// general/search/server/engines sections, which crashes every single request
// with `KeyError: 'default_doi_resolver'` (searx/preferences.py). The file is
// git `skip-worktree`'d for routine proxy-list churn, so git never caught it.
//
// No build tag on this file: it is compiled into BOTH the router binary
// (router.go + analytics.go + settingsguard.go) and the proxypool binary
// (proxypool.go + settingsguard.go, per go/build.sh), and into `go test`.
// proxypool.go itself carries `//go:build ignore` (it defines its own main()
// and can't coexist with router.go's main() in a default `go build ./...`),
// so the guard lives here, untagged, to stay testable under `go test ./...`.
package main

import "strings"

// settingsHasCoreSections reports whether text (a SearXNG settings.yml-shaped
// file) still contains its core directives. A file reduced to only the managed
// proxy block (just `outgoing:`/`proxies:`) fails this check — exactly the
// shape of the 2026-10-12 corruption.
func settingsHasCoreSections(text string) bool {
	return strings.Contains(text, "use_default_settings") && strings.Contains(text, "engines:")
}
