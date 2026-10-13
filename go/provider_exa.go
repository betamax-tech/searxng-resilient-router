// provider_exa.go — Exa adapter. Verified endpoint (2026-10-13, Exa's own
// `build-with-exa` agent skill): POST https://api.exa.ai/search, header
// `x-api-key`. Response includes a top-level `costDollars` field — this is
// the ONLY provider in this router whose cap is cost-tracked rather than
// request-counted, per the owner's "zero expenditure" decision.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ExaProvider struct {
	Client  *http.Client
	BaseURL string
	APIKey  func() string
}

func NewExaProvider() *ExaProvider {
	return &ExaProvider{
		Client:  &http.Client{Timeout: providerTimeout},
		BaseURL: "https://api.exa.ai",
		APIKey:  func() string { return os.Getenv("EXA_API_KEY") },
	}
}

func (p *ExaProvider) Name() string { return "exa" }

func (p *ExaProvider) Search(ctx context.Context, query string, params url.Values) ProviderResult {
	key := p.APIKey()
	if key == "" {
		return ProviderResult{Err: "missing_api_key"}
	}
	// Recommended request per the vendor skill: query + type auto + bare
	// highlights. No numResults/domain-filter boilerplate (gated per skill).
	reqBody, _ := json.Marshal(map[string]any{
		"query":    query,
		"type":     "auto",
		"contents": map[string]any{"highlights": true},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/search", bytes.NewReader(reqBody))
	if err != nil {
		return ProviderResult{Err: "bad_request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)

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
		Results []struct {
			URL        string   `json:"url"`
			Title      string   `json:"title"`
			Highlights []string `json:"highlights"`
			Text       string   `json:"text"`
		} `json:"results"`
		CostDollars *float64 `json:"costDollars"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ProviderResult{StatusCode: resp.StatusCode, Err: "bad_json", LatencyMS: lat}
	}

	var out []SearxResult
	for _, r := range doc.Results {
		content := strings.Join(r.Highlights, " ")
		if content == "" {
			content = r.Text
		}
		out = append(out, SearxResult{URL: r.URL, Title: r.Title, Content: content, Engines: []string{"exa"}})
	}

	pr := ProviderResult{Results: out, StatusCode: resp.StatusCode, LatencyMS: lat}
	if doc.CostDollars != nil {
		pr.CostKnown = true
		pr.CostDollars = *doc.CostDollars
	}
	return pr
}
