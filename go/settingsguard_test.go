package main

import "testing"

// P0 durability fix: proxypool.go's writeSettings() must refuse to persist a
// settings*.yml that has already lost use_default_settings/engines, so the
// 2026-10-12 regression (file reduced to only the managed proxy block,
// crashing every request with KeyError: 'default_doi_resolver') can never
// silently recur.
func TestSettingsHasCoreSections(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "full restored settings.proxied.yml shape",
			text: "use_default_settings: true\n\ngeneral:\n  instance_name: x\n\nengines:\n  - name: google\n    engine: google\n",
			want: true,
		},
		{
			name: "exact 2026-10-12 corruption shape: managed block only",
			text: "\n\n# >>> MANAGED PROXIES (proxy_pool) — do not edit by hand >>>\n" +
				"# regenerated: 2026-10-12T15:31:02Z | 20 healthy proxies | SearXNG round-robins these\n" +
				"outgoing:\n  request_timeout: 6.0\n  proxies:\n    all://:\n      - http://1.2.3.4:8080\n" +
				"# <<< MANAGED PROXIES (proxy_pool) <<<\n",
			want: false,
		},
		{
			name: "missing engines section only",
			text: "use_default_settings: true\n\nsearch:\n  safe_search: 2\n",
			want: false,
		},
		{
			name: "missing use_default_settings only",
			text: "engines:\n  - name: google\n",
			want: false,
		},
		{
			name: "empty file",
			text: "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := settingsHasCoreSections(c.text); got != c.want {
				t.Errorf("settingsHasCoreSections() = %v, want %v\ntext:\n%s", got, c.want, c.text)
			}
		})
	}
}
