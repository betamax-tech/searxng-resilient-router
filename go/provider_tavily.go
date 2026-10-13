// provider_tavily.go — Tavily adapter. Verified endpoint (2026-10-13):
// POST https://api.tavily.com/search, Bearer auth. Basic search = 1 credit
// (count-mode cap; see caps.go).
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
	"time"
)

type TavilyProvider struct {
	Client  *http.Client
	BaseURL string
	APIKey  func() string
}

func NewTavilyProvider() *TavilyProvider {
	return &TavilyProvider{
		Client:  &http.Client{Timeout: providerTimeout},
		BaseURL: "https://api.tavily.com",
		APIKey:  func() string { return os.Getenv("TAVILY_API_KEY") },
	}
}

func (p *TavilyProvider) Name() string { return "tavily" }

func (p *TavilyProvider) Search(ctx context.Context, query string, params url.Values) ProviderResult {
	key := p.APIKey()
	if key == "" {
		return ProviderResult{Err: "missing_api_key"}
	}
	reqBody, _ := json.Marshal(map[string]any{
		"query":        query,
		"search_depth": "basic", // 1 credit/call — keeps the free 1,000/month pool stretched
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/search", bytes.NewReader(reqBody))
	if err != nil {
		return ProviderResult{Err: "bad_request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

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
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ProviderResult{StatusCode: resp.StatusCode, Err: "bad_json", LatencyMS: lat}
	}

	var out []SearxResult
	for _, r := range doc.Results {
		out = append(out, SearxResult{URL: r.URL, Title: r.Title, Content: r.Content, Engines: []string{"tavily"}})
	}
	return ProviderResult{Results: out, StatusCode: resp.StatusCode, LatencyMS: lat}
}
