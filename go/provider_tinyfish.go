// provider_tinyfish.go — TinyFish Search adapter. Verified endpoint
// (2026-10-13): GET https://api.search.tinyfish.ai?query=…, header
// `X-API-Key`; supports `location` and `language`; honors `site:` directly
// in the query text. The exact result field names are NOT fully documented
// (2026-10-13 investigation doc, known gap #3), so decoding is defensive —
// it tolerates several title/url/content-style field-name variants rather
// than assuming one exact shape.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

type TinyFishProvider struct {
	Client  *http.Client
	BaseURL string
	APIKey  func() string
}

func NewTinyFishProvider() *TinyFishProvider {
	return &TinyFishProvider{
		Client:  &http.Client{Timeout: providerTimeout},
		BaseURL: "https://api.search.tinyfish.ai",
		APIKey:  func() string { return os.Getenv("TINYFISH_API_KEY") },
	}
}

func (p *TinyFishProvider) Name() string { return "tinyfish" }

func (p *TinyFishProvider) Search(ctx context.Context, query string, params url.Values) ProviderResult {
	key := p.APIKey()
	if key == "" {
		return ProviderResult{Err: "missing_api_key"}
	}
	v := url.Values{}
	v.Set("query", query) // site: filters are honored directly in the query text per the docs
	if lang := params.Get("language"); lang != "" {
		v.Set("language", lang)
	}
	if loc := params.Get("location"); loc != "" {
		v.Set("location", loc)
	}
	u := p.BaseURL + "?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ProviderResult{Err: "bad_request: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", key)

	t0 := time.Now()
	resp, err := p.Client.Do(req)
	lat := time.Since(t0).Milliseconds()
	if err != nil {
		return ProviderResult{Err: "request_failed: " + err.Error(), LatencyMS: lat}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusTooManyRequests {
		return ProviderResult{StatusCode: resp.StatusCode, Err: "rate_limited", LatencyMS: lat}
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderResult{StatusCode: resp.StatusCode, Err: fmt.Sprintf("http_%d", resp.StatusCode), LatencyMS: lat}
	}

	var raw struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ProviderResult{StatusCode: resp.StatusCode, Err: "bad_json", LatencyMS: lat}
	}

	var out []SearxResult
	for _, m := range raw.Results {
		resultURL := firstString(m, "url", "link")
		if resultURL == "" {
			continue
		}
		out = append(out, SearxResult{
			URL:     resultURL,
			Title:   firstString(m, "title", "name"),
			Content: firstString(m, "snippet", "content", "description", "summary"),
			Engines: []string{"tinyfish"},
		})
	}
	return ProviderResult{Results: out, StatusCode: resp.StatusCode, LatencyMS: lat}
}
