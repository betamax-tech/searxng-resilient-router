// provider_brave.go — Brave Search API adapter. Verified endpoint
// (2026-10-13): GET https://api.search.brave.com/res/v1/web/search, header
// `X-Subscription-Token`. This account has a CARD ON FILE (overage would be
// billed), so its cap (see caps.go) is the strictest in the router: any
// unknown/ambiguous state is a hard stop, never an assumed-OK pass-through.
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

type BraveProvider struct {
	Client  *http.Client
	BaseURL string
	APIKey  func() string
}

func NewBraveProvider() *BraveProvider {
	return &BraveProvider{
		Client:  &http.Client{Timeout: providerTimeout},
		BaseURL: "https://api.search.brave.com/res/v1/web/search",
		APIKey:  func() string { return os.Getenv("BRAVE_API_KEY") },
	}
}

func (p *BraveProvider) Name() string { return "brave" }

func (p *BraveProvider) Search(ctx context.Context, query string, params url.Values) ProviderResult {
	key := p.APIKey()
	if key == "" {
		return ProviderResult{Err: "missing_api_key"}
	}
	u := p.BaseURL + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ProviderResult{Err: "bad_request: " + err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

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

	var doc struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ProviderResult{StatusCode: resp.StatusCode, Err: "bad_json", LatencyMS: lat}
	}

	var out []SearxResult
	for _, r := range doc.Web.Results {
		out = append(out, SearxResult{URL: r.URL, Title: r.Title, Content: r.Description, Engines: []string{"brave"}})
	}
	return ProviderResult{Results: out, StatusCode: resp.StatusCode, LatencyMS: lat}
}
