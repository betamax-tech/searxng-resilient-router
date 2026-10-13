package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestExaProvider_ParsesRealResponseShape locks in a real regression found
// during live verification (2026-10-13): `costDollars` is a NESTED OBJECT
// ({"total": 0.007, "search": {"neural": 0.007}, ...}), not a bare number.
// Decoding it as *float64 silently failed json.Unmarshal() for the WHOLE
// response, which surfaced as a mysterious "bad_json" failover on every
// single live Exa call until fixed.
func TestExaProvider_ParsesRealResponseShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("expected x-api-key header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"requestId": "abc",
			"resolvedSearchType": "neural",
			"results": [
				{"id": "https://lawphil.net/x", "url": "https://lawphil.net/x", "title": "X",
				 "highlights": ["hello world"]}
			],
			"searchTime": 123.4,
			"costDollars": {"total": 0.007, "search": {"neural": 0.007}}
		}`))
	}))
	defer srv.Close()

	p := &ExaProvider{Client: srv.Client(), BaseURL: srv.URL, APIKey: func() string { return "test-key" }}
	res := p.Search(context.Background(), "G.R. No. 123456", url.Values{})

	if res.Err != "" {
		t.Fatalf("expected no error, got %q", res.Err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res.Results))
	}
	if res.Results[0].URL != "https://lawphil.net/x" || res.Results[0].Content != "hello world" {
		t.Errorf("unexpected mapped result: %+v", res.Results[0])
	}
	if !res.CostKnown {
		t.Fatalf("expected CostKnown=true from the nested costDollars.total field")
	}
	if res.CostDollars != 0.007 {
		t.Errorf("expected CostDollars=0.007, got %v", res.CostDollars)
	}
}

func TestExaProvider_MissingAPIKey(t *testing.T) {
	p := &ExaProvider{APIKey: func() string { return "" }}
	res := p.Search(context.Background(), "x", url.Values{})
	if res.Err != "missing_api_key" {
		t.Errorf("expected missing_api_key error, got %q", res.Err)
	}
}

func TestExaProvider_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := &ExaProvider{Client: srv.Client(), BaseURL: srv.URL, APIKey: func() string { return "k" }}
	res := p.Search(context.Background(), "x", url.Values{})
	if res.Err != "rate_limited" {
		t.Errorf("expected rate_limited error, got %q", res.Err)
	}
}

func TestExaProvider_MissingCostDollarsLeavesCostUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"url":"https://a","title":"A"}]}`))
	}))
	defer srv.Close()
	p := &ExaProvider{Client: srv.Client(), BaseURL: srv.URL, APIKey: func() string { return "k" }}
	res := p.Search(context.Background(), "x", url.Values{})
	if res.Err != "" {
		t.Fatalf("expected success, got err %q", res.Err)
	}
	if res.CostKnown {
		t.Errorf("expected CostKnown=false when costDollars is absent, got true")
	}
}
