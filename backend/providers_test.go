package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRaw(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid test JSON: %v", err)
	}
	return m
}

func metricByLabel(r ProviderResult, label string) *Metric {
	for i := range r.Metrics {
		if r.Metrics[i].Label == label {
			return &r.Metrics[i]
		}
	}
	return nil
}

// Nowy, udokumentowany format /api/usage (od 2026-10): totals + buckets.
func TestOllamaUsageShape(t *testing.T) {
	raw := mustRaw(t, `{
	  "range": "7d",
	  "scope": "self",
	  "granularity": "day",
	  "totals": {
	    "request_count": 15,
	    "usage_usd": 0.01718,
	    "input_tokens": 106000,
	    "cached_input_tokens": 46000,
	    "output_tokens": 13600
	  },
	  "buckets": [
	    {"from":"2026-09-30T12:00:00Z","until":"2026-09-30T13:00:00Z","request_count":4},
	    {"from":"2026-10-01T00:00:00Z","until":"2026-10-01T01:00:00Z","request_count":8},
	    {"from":"2026-10-01T02:00:00Z","until":"2026-10-01T02:30:00Z","request_count":0,"partial":true}
	  ]
	}`)

	var r ProviderResult
	if !appendOllamaUsageMetrics(&r, raw, 0) {
		t.Fatal("oczekiwano rozpoznania formatu totals/buckets")
	}
	if appendOllamaQuotaMetrics(&r, raw, "") {
		t.Fatal("the cloud-usage format should not be counted as the old limits format")
	}

	if m := metricByLabel(r, "Requests (7 days)"); m == nil || m.Value != "15" {
		t.Fatalf("Requests (7 days) = %+v", m)
	}
	if m := metricByLabel(r, "Cost (7 days)"); m == nil || m.Value != "0.02 USD" {
		t.Fatalf("Cost (7 days) = %+v", m)
	}
	if m := metricByLabel(r, "Tokens (7 days)"); m == nil || m.Value != "119600" {
		t.Fatalf("Tokens (7 days) = %+v", m)
	}
	if m := metricByLabel(r, "Requests by day (7 days)"); m == nil || m.Value != "12" || len(m.Rows) != 2 {
		t.Fatalf("Requests by day (7 days) = %+v", m)
	}
}

// An optional custom USD limit adds a percentage bar.
func TestOllamaUsageShapeWithBudget(t *testing.T) {
	raw := mustRaw(t, `{"range":"30d","totals":{"request_count":3,"usage_usd":25}}`)

	var r ProviderResult
	if !appendOllamaUsageMetrics(&r, raw, 100) {
		t.Fatal("oczekiwano rozpoznania formatu")
	}
	m := metricByLabel(r, "Custom monthly limit")
	if m == nil || m.Percent == nil || *m.Percent != 25 {
		t.Fatalf("Custom monthly limit = %+v", m)
	}
}

// Older format: limits.session + limits.weekly as 0..1 fractions.
func TestOllamaLegacySessionWeekly(t *testing.T) {
	raw := mustRaw(t, `{
	  "activity": {"cost": "0.00000", "models": [], "period": {"type": "last_4_weeks"}},
	  "limits": {
	    "session": {"usage": 0.046, "models": [{"name": "glm-5.2", "request_count": 34}]},
	    "weekly": {"usage": 0.051, "models": [{"name": "glm-5.2", "request_count": 254}]}
	  }
	}`)

	var r ProviderResult
	if appendOllamaUsageMetrics(&r, raw, 0) {
		t.Fatal("the older format has no totals and should not be counted as cloud-usage")
	}
	if !appendOllamaQuotaMetrics(&r, raw, "") {
		t.Fatal("oczekiwano rozpoznania formatu limits")
	}
	if m := metricByLabel(r, "5 h / session"); m == nil || m.Value != "4.6% used" {
		t.Fatalf("5 h / session = %+v", m)
	}
	if m := metricByLabel(r, "Week"); m == nil || m.Value != "5.1% used" {
		t.Fatalf("Week = %+v", m)
	}
}

// Starszy format: limits.monthly + rozbicie na modele + activity.cost.
func TestOllamaLegacyMonthly(t *testing.T) {
	raw := mustRaw(t, `{
	  "activity": {
	    "cost": "31.21553",
	    "period": {"type": "last_4_weeks"},
	    "models": [{"name": "glm-5.3", "request_count": 305, "cost": "28.87030"}]
	  },
	  "limits": {
	    "monthly": {"usage": 0.231, "models": [
	      {"name": "glm-5.3", "request_count": 1312},
	      {"name": "web search", "request_count": 9}
	    ]}
	  }
	}`)

	var r ProviderResult
	if !appendOllamaQuotaMetrics(&r, raw, "") {
		t.Fatal("oczekiwano rozpoznania formatu monthly")
	}
	if m := metricByLabel(r, "Month"); m == nil || m.Value != "23.1% used" || m.ResetAt == "" {
		t.Fatalf("Month = %+v", m)
	}
	if m := metricByLabel(r, "Requests by model (month)"); m == nil || m.Value != "1321" || len(m.Rows) != 2 {
		t.Fatalf("Requests by model = %+v", m)
	}
	if m := metricByLabel(r, "Cost (4 wk)"); m == nil || m.Value != "31.22 USD" {
		t.Fatalf("Cost (4 wk) = %+v", m)
	}
}

// An unrecognised format generates no metrics (the tile shows a message).
func TestOllamaUnknownShape(t *testing.T) {
	raw := mustRaw(t, `{"hello":"world"}`)
	var r ProviderResult
	if appendOllamaUsageMetrics(&r, raw, 0) || appendOllamaQuotaMetrics(&r, raw, "") {
		t.Fatal("an unknown format should not generate metrics")
	}
	if len(r.Metrics) != 0 {
		t.Fatalf("expected 0 metrics, got %d", len(r.Metrics))
	}
}

func TestWithOllamaRange(t *testing.T) {
	cases := []struct{ in, rng, want string }{
		{"https://ollama.com/api/usage", "", "https://ollama.com/api/usage"},
		{"https://ollama.com/api/usage", "30d", "https://ollama.com/api/usage?range=30d"},
		{"http://proxy.local/usage?token=x", "7d", "http://proxy.local/usage?token=x&range=7d"},
		{"https://ollama.com/api/usage?range=24h", "30d", "https://ollama.com/api/usage?range=24h"},
	}
	for _, c := range cases {
		if got := withOllamaRange(c.in, c.rng); got != c.want {
			t.Errorf("withOllamaRange(%q, %q) = %q, want %q", c.in, c.rng, got, c.want)
		}
	}
}

// New quota tile: GET /api/balance (allowance + reset date).
func TestOllamaBalance(t *testing.T) {
	raw := mustRaw(t, `{
	  "included": {"balance_usd": 0, "allowance_usd": 60,
	    "period": {"from": "2025-12-01T00:00:00Z", "until": "2026-01-01T00:00:00Z"}},
	  "purchased": {"balance_usd": 0}
	}`)

	var r ProviderResult
	if !appendOllamaBalanceMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu balance")
	}
	m := metricByLabel(r, "Monthly limit")
	if m == nil || m.Value != "60.00 / 60.00 USD" || m.Percent == nil || *m.Percent != 100 || m.ResetAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("Monthly limit = %+v", m)
	}
	if p := metricByLabel(r, "Purchased credits"); p != nil {
		t.Fatalf("Purchased credits should not appear when 0: %+v", p)
	}
}

// Claude tile: without cache the error is plain, with cache it shows the last data.
func TestClaudeFailWithoutCache(t *testing.T) {
	r := baseResult(TileConfig{ID: "claude-no-cache", Name: "Claude"}, "x", true)
	got := claudeFail(r, "error", "HTTP 429")
	if got.Status != "error" || got.Message != "HTTP 429" || len(got.Metrics) != 0 {
		t.Fatalf("without cache: %+v", got)
	}
}

func TestClaudeFailWithCache(t *testing.T) {
	p := 42.0
	claudeSetCachedMetrics("claude-cached", []Metric{{Label: "5 h", Value: "42% used", Percent: &p}})

	r := baseResult(TileConfig{ID: "claude-cached", Name: "Claude"}, "x", true)
	got := claudeFail(r, "error", "HTTP 429")
	if got.Status != "error" || len(got.Metrics) != 1 || got.Metrics[0].Label != "5 h" {
		t.Fatalf("with cache: %+v", got)
	}
	if !strings.Contains(got.Message, "Last successful fetch") || !strings.Contains(got.Message, "HTTP 429") {
		t.Fatalf("message: %q", got.Message)
	}
	if !strings.Contains(got.Summary, "stale data") {
		t.Fatalf("summary: %q", got.Summary)
	}
}

func TestOllamaBalancePartial(t *testing.T) {
	raw := mustRaw(t, `{
	  "included": {"balance_usd": 15, "allowance_usd": 60,
	    "period": {"until": "2026-01-01T00:00:00Z"}},
	  "purchased": {"balance_usd": 5}
	}`)

	var r ProviderResult
	if !appendOllamaBalanceMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu balance")
	}
	m := metricByLabel(r, "Monthly limit")
	if m == nil || m.Value != "45.00 / 60.00 USD" || m.Percent == nil || *m.Percent != 75 {
		t.Fatalf("Monthly limit = %+v", m)
	}
	if p := metricByLabel(r, "Purchased credits"); p == nil || p.Value != "5.00 USD" {
		t.Fatalf("Purchased credits = %+v", p)
	}
}
