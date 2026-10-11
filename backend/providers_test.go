package main

import (
	"encoding/json"
	"testing"
)

func mustRaw(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("nieprawidłowy JSON testowy: %v", err)
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
	if !appendOllamaUsageMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu totals/buckets")
	}
	if appendOllamaQuotaMetrics(&r, raw) {
		t.Fatal("format cloud-usage nie powinien być liczony jako stary format limits")
	}

	if m := metricByLabel(r, "Żądania (7 dni)"); m == nil || m.Value != "15" {
		t.Fatalf("Żądania (7 dni) = %+v", m)
	}
	if m := metricByLabel(r, "Koszt (7 dni)"); m == nil || m.Value != "0.02 USD" {
		t.Fatalf("Koszt (7 dni) = %+v", m)
	}
	if m := metricByLabel(r, "Tokeny (7 dni)"); m == nil || m.Value != "119600" {
		t.Fatalf("Tokeny (7 dni) = %+v", m)
	}
	if m := metricByLabel(r, "Żądania wg dnia (7 dni)"); m == nil || m.Value != "12" || len(m.Rows) != 2 {
		t.Fatalf("Żądania wg dnia (7 dni) = %+v", m)
	}
}

// Opcjonalny własny limit USD dokłada pasek procentowy.
func TestOllamaUsageShapeWithBudget(t *testing.T) {
	t.Setenv("OLLAMA_MONTHLY_LIMIT_USD", "100")
	raw := mustRaw(t, `{"range":"30d","totals":{"request_count":3,"usage_usd":25}}`)

	var r ProviderResult
	if !appendOllamaUsageMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu")
	}
	m := metricByLabel(r, "Własny limit miesięczny")
	if m == nil || m.Percent == nil || *m.Percent != 25 {
		t.Fatalf("Własny limit miesięczny = %+v", m)
	}
}

// Starszy format: limits.session + limits.weekly jako ułamki 0..1.
func TestOllamaLegacySessionWeekly(t *testing.T) {
	raw := mustRaw(t, `{
	  "activity": {"cost": "0.00000", "models": [], "period": {"type": "last_4_weeks"}},
	  "limits": {
	    "session": {"usage": 0.046, "models": [{"name": "glm-5.2", "request_count": 34}]},
	    "weekly": {"usage": 0.051, "models": [{"name": "glm-5.2", "request_count": 254}]}
	  }
	}`)

	var r ProviderResult
	if appendOllamaUsageMetrics(&r, raw) {
		t.Fatal("starszy format nie ma totals, nie powinien być liczony jako cloud-usage")
	}
	if !appendOllamaQuotaMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu limits")
	}
	if m := metricByLabel(r, "5 h / sesja"); m == nil || m.Value != "4.6% użyte" {
		t.Fatalf("5 h / sesja = %+v", m)
	}
	if m := metricByLabel(r, "Tydzień"); m == nil || m.Value != "5.1% użyte" {
		t.Fatalf("Tydzień = %+v", m)
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
	if !appendOllamaQuotaMetrics(&r, raw) {
		t.Fatal("oczekiwano rozpoznania formatu monthly")
	}
	if m := metricByLabel(r, "Miesiąc"); m == nil || m.Value != "23.1% użyte" || m.ResetAt == "" {
		t.Fatalf("Miesiąc = %+v", m)
	}
	if m := metricByLabel(r, "Żądania wg modelu (miesiąc)"); m == nil || m.Value != "1321" || len(m.Rows) != 2 {
		t.Fatalf("Żądania wg modelu = %+v", m)
	}
	if m := metricByLabel(r, "Koszt (4 tyg.)"); m == nil || m.Value != "31.22 USD" {
		t.Fatalf("Koszt (4 tyg.) = %+v", m)
	}
}

// Nierozpoznany format nie generuje metryk (kafelek pokaże komunikat).
func TestOllamaUnknownShape(t *testing.T) {
	raw := mustRaw(t, `{"hello":"world"}`)
	var r ProviderResult
	if appendOllamaUsageMetrics(&r, raw) || appendOllamaQuotaMetrics(&r, raw) {
		t.Fatal("nieznany format nie powinien generować metryk")
	}
	if len(r.Metrics) != 0 {
		t.Fatalf("oczekiwano 0 metryk, jest %d", len(r.Metrics))
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
			t.Errorf("withOllamaRange(%q, %q) = %q, chcę %q", c.in, c.rng, got, c.want)
		}
	}
}
