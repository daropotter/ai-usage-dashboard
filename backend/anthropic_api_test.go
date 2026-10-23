package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAPIUsageAndCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "admin-key" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage") && r.URL.Query().Get("page") == "":
			w.Write([]byte(`{"data":[{"results":[{"uncached_input_tokens":1000,"cache_read_input_tokens":500,
				"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":50},"output_tokens":200}]}],
				"has_more":true,"next_page":"p2"}`))
		case strings.HasSuffix(r.URL.Path, "/usage"):
			w.Write([]byte(`{"data":[{"results":[{"uncached_input_tokens":10,"output_tokens":5}]}],"has_more":false}`))
		case strings.HasSuffix(r.URL.Path, "/cost"):
			w.Write([]byte(`{"data":[{"results":[{"amount":"1234.5","currency":"USD"}]},{"results":[{"amount":"100","currency":"USD"}]}],"has_more":false}`))
		}
	}))
	defer srv.Close()
	t.Setenv("TILES", "api")
	t.Setenv("TILE_API_TYPE", "anthropic-api")
	t.Setenv("TILE_API_API_KEY", "admin-key")
	t.Setenv("TILE_API_USAGE_URL", srv.URL+"/usage")
	t.Setenv("TILE_API_COST_URL", srv.URL+"/cost")
	t.Setenv("TILE_API_MONTHLY_LIMIT_USD", "50")

	r := fetchTile(srv.Client(), loadTiles()[0])
	if r.Status != "ok" || r.Message != "" {
		t.Fatalf("result: %+v", r)
	}
	if m := metricByLabel(r, "Tokens this month"); m == nil || m.Value != "1,865" || len(m.Rows) != 4 || m.Rows[2].Value != "150" {
		t.Fatalf("Tokens this month = %+v", m)
	}
	if m := metricByLabel(r, "Cost this month"); m == nil || m.Value != "13.35 USD" {
		t.Fatalf("Cost this month = %+v", m)
	}
	if m := metricByLabel(r, "Custom monthly limit"); m == nil || m.Percent == nil || *m.Percent < 26.6 || *m.Percent > 26.8 {
		t.Fatalf("Custom monthly limit = %+v", m)
	}
}

// One report failing still shows the other; both failing is an error.
func TestAnthropicAPIPartialAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cost") && r.Header.Get("x-api-key") == "good" {
			w.Write([]byte(`{"data":[{"results":[{"amount":"500","currency":"USD"}]}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv("TILES", "api")
	t.Setenv("TILE_API_TYPE", "anthropic-api")
	t.Setenv("TILE_API_USAGE_URL", srv.URL+"/usage")
	t.Setenv("TILE_API_COST_URL", srv.URL+"/cost")

	t.Setenv("TILE_API_API_KEY", "good")
	r := fetchTile(srv.Client(), loadTiles()[0])
	if r.Status != "ok" || metricByLabel(r, "Tokens this month") != nil || metricByLabel(r, "Cost this month") == nil || !strings.Contains(r.Message, "Usage report unavailable") {
		t.Fatalf("partial: %+v", r)
	}

	t.Setenv("TILE_API_API_KEY", "bad")
	r = fetchTile(srv.Client(), loadTiles()[0])
	if r.Status != "error" || len(r.Metrics) != 0 || strings.Contains(r.Message, "bad") {
		t.Fatalf("failure: %+v", r)
	}
}
