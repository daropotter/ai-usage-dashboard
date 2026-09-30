package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func baseResult(id, name, source string, configured bool) ProviderResult {
	return ProviderResult{ID: id, Name: name, Source: source, Configured: configured, Status: "no_data", Summary: "Brak danych", Metrics: []Metric{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
}

func doJSON(client *http.Client, method, endpoint string, headers map[string]string, out any) error {
	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-dashboard/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactError(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("nieprawidłowy JSON: %w", err)
	}
	return nil
}

func compactError(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 180 {
		s = s[:180] + "…"
	}
	return s
}

func fetchOpenCodeGo(client *http.Client) ProviderResult {
	return fetchOpenCodeQuota(client, "opencode-go", "OpenCode Go A", "OPENCODE_GO_API_KEY", "OPENCODE_GO_USAGE_URL")
}

func fetchOpenCodeGoB(client *http.Client) ProviderResult {
	return fetchOpenCodeQuota(client, "opencode-go-b", "OpenCode Go B", "OPENCODE_GO_B_API_KEY", "OPENCODE_GO_B_USAGE_URL")
}

func fetchOpenCodeQuota(client *http.Client, id, name, envKey, envURL string) ProviderResult {
	key := strings.TrimSpace(os.Getenv(envKey))
	r := baseResult(id, name, "OpenCode Go quota API", key != "")
	if key == "" {
		r.Message = "Ustaw " + envKey + "."
		return r
	}

	var payload struct {
		Usage map[string]struct {
			Status   string  `json:"status"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resetsAt"`
		} `json:"usage"`
	}
	endpoint := envOr(envURL, "https://opencode.ai/zen/go/v1/usage")
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &payload); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err)
		return r
	}
	labels := []struct{ key, label string }{{"rolling", "5 h"}, {"weekly", "Tydzień"}, {"monthly", "Miesiąc"}}
	for _, item := range labels {
		if w, ok := payload.Usage[item.key]; ok {
			p := round1(clamp(w.Percent, 0, 100))
			r.Metrics = append(r.Metrics, Metric{Label: item.label, Value: fmt.Sprintf("%.1f%% użyte", p), Percent: &p, ResetAt: w.ResetsAt})
		}
	}
	if len(r.Metrics) == 0 {
		r.Message = "Endpoint odpowiedział, ale nie zwrócił obsługiwanych okien usage."
		return r
	}
	r.Status = "ok"
	r.Summary = "Limity subskrypcji"
	return r
}

func fetchOllama(client *http.Client) ProviderResult {
	key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	r := baseResult("ollama-cloud", "Ollama Cloud", "Nieudokumentowany account usage endpoint", key != "")
	if key == "" {
		r.Message = "Ustaw OLLAMA_API_KEY. Bez klucza kafelek pozostaje w stanie Brak danych."
		return r
	}

	endpoint := envOr("OLLAMA_USAGE_URL", "https://ollama.com/api/usage")
	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &raw); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " Endpoint Ollama Cloud nie jest publicznie gwarantowany."
		return r
	}

	candidates := []struct {
		keys  []string
		label string
	}{
		{[]string{"session", "rolling", "five_hour", "5h", "session_usage"}, "5 h / sesja"},
		{[]string{"weekly", "week", "weekly_usage"}, "Tydzień"},
		{[]string{"monthly", "month", "monthly_usage"}, "Miesiąc"},
	}
	for _, c := range candidates {
		if m, ok := findObject(raw, c.keys); ok {
			p, found := findPercent(m)
			if !found {
				// Ollama Cloud /api/usage zwraca limits.<okno>.usage jako ułamek
				// 0..1 (np. 0.932 = 93,2% wykorzystania). Wartości >1 traktujemy
				// jako już wyrażone w procentach.
				if u, ok := findNumber(m, []string{"usage", "used"}); ok {
					if u <= 1 {
						u *= 100
					}
					p, found = u, true
				}
			}
			if found {
				p = round1(clamp(p, 0, 100))
				reset := findString(m, []string{"resetsAt", "resets_at", "reset_at", "reset", "resets_in"})
				if reset == "" && c.label == "Miesiąc" {
					reset = ollamaMonthlyReset(time.Now().UTC())
				}
				r.Metrics = append(r.Metrics, Metric{Label: c.label, Value: fmt.Sprintf("%.1f%% użyte", p), Percent: &p, ResetAt: reset})
			}
		}
	}
	// Dodatkowe dane z formatu Ollama Cloud: rozbicie żądań na modele i koszt overage.
	if m, ok := findObject(raw, []string{"monthly", "month", "monthly_usage"}); ok {
		if arr, ok := m["models"].([]any); ok {
			type modelUse struct {
				name string
				n    int
			}
			var list []modelUse
			total := 0
			for _, it := range arr {
				if o, ok := it.(map[string]any); ok {
					if n, ok := findNumber(o, []string{"request_count", "requests", "count"}); ok {
						name := findString(o, []string{"name", "model"})
						if name == "" {
							name = "model"
						}
						count := int(n)
						total += count
						list = append(list, modelUse{name, count})
					}
				}
			}
			if total > 0 {
				sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
				rows := make([]MetricRow, 0, len(list))
				for _, x := range list {
					rows = append(rows, MetricRow{Label: x.name, Value: fmt.Sprintf("%d", x.n)})
				}
				r.Metrics = append(r.Metrics, Metric{Label: "Żądania wg modelu (miesiąc)", Value: fmt.Sprintf("%d", total), Rows: rows})
			}
		}
	}
	if act, ok := raw["activity"].(map[string]any); ok {
		if cost, ok := findNumber(act, []string{"cost"}); ok && cost > 0 {
			r.Metrics = append(r.Metrics, Metric{Label: "Koszt (4 tyg.)", Value: fmt.Sprintf("%.2f USD", cost)})
		}
	}
	if len(r.Metrics) == 0 {
		r.Message = "Endpoint odpowiedział, ale format nie zawiera rozpoznanych danych quota. Ustaw OLLAMA_USAGE_URL, jeśli używasz kompatybilnego endpointu/proxy."
		return r
	}
	r.Status = "ok"
	r.Summary = "Limity Ollama Cloud"
	return r
}

func fetchOpenAI(client *http.Client) ProviderResult {
	key := strings.TrimSpace(os.Getenv("OPENAI_ADMIN_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	r := baseResult("openai", "OpenAI", "OpenAI Organization Costs API", key != "")
	if key == "" {
		r.Message = "Ustaw OPENAI_ADMIN_API_KEY (zalecane) lub OPENAI_API_KEY. Zwykła subskrypcja ChatGPT Plus/Pro nie udostępnia tego licznika przez API."
		return r
	}

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(start, 10))
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	endpoint := envOr("OPENAI_COSTS_URL", "https://api.openai.com/v1/organization/costs") + "?" + q.Encode()
	var payload struct {
		Data []struct {
			Results []struct {
				Amount struct {
					Value    float64 `json:"value"`
					Currency string  `json:"currency"`
				} `json:"amount"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &payload); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " Ten endpoint zwykle wymaga klucza administracyjnego organizacji; nie pokazuje limitu ChatGPT Plus/Pro."
		return r
	}
	total := 0.0
	currency := "USD"
	for _, b := range payload.Data {
		for _, x := range b.Results {
			total += x.Amount.Value
			if x.Amount.Currency != "" {
				currency = strings.ToUpper(x.Amount.Currency)
			}
		}
	}
	r.Metrics = append(r.Metrics, Metric{Label: "Koszt w tym miesiącu", Value: fmt.Sprintf("%.2f %s", total, currency)})
	if limit, ok := envFloat("OPENAI_MONTHLY_LIMIT_USD"); ok && limit > 0 {
		p := clamp(total/limit*100, 0, 100)
		r.Metrics = append(r.Metrics, Metric{Label: "Własny limit miesięczny", Value: fmt.Sprintf("%.2f / %.2f USD", total, limit), Percent: &p})
	}
	r.Status = "ok"
	r.Summary = "Billing API (nie ChatGPT subscription quota)"
	return r
}

// round1 zaokrągla do jednego miejsca po przecinku (czytelne wartości procentowe).
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// ollamaMonthlyReset zwraca najbliższą datę resetu limitu miesięcznego Ollamy.
// Endpoint /api/usage jej nie podaje, więc liczymy z kotwicy OLLAMA_RESET_ANCHOR
// (dowolna znana data resetu; rzutowana w przyszłość co miesiąc). Bez kotwicy
// używamy przybliżenia: 1. dzień następnego miesiąca 00:00 UTC.
func ollamaMonthlyReset(now time.Time) string {
	if s := strings.TrimSpace(os.Getenv("OLLAMA_RESET_ANCHOR")); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return nextMonthlyReset(t.UTC(), now).Format(time.RFC3339)
		}
	}
	return firstDayNextMonthUTC()
}

// nextMonthlyReset przesuwa kotwicę o pełne miesiące, aż wypadnie po `now`,
// zachowując dzień i godzinę (dzień przycinany do długości miesiąca).
func nextMonthlyReset(anchor, now time.Time) time.Time {
	if anchor.After(now) {
		return anchor
	}
	y, mo, d := anchor.Date()
	h, mi, sec := anchor.Clock()
	for i := 1; i < 1200; i++ {
		total := int(mo) - 1 + i
		yy := y + total/12
		mm := time.Month(total%12 + 1)
		dd := d
		if last := daysInMonth(yy, mm); dd > last {
			dd = last
		}
		cand := time.Date(yy, mm, dd, h, mi, sec, 0, time.UTC)
		if cand.After(now) {
			return cand
		}
	}
	return anchor
}

func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// firstDayNextMonthUTC zwraca początek następnego miesiąca o 00:00 UTC w RFC3339.
func firstDayNextMonthUTC() string {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return next.Format(time.RFC3339)
}

func envFloat(key string) (float64, bool) {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}
func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
func safeProviderError(err error) string {
	s := err.Error()
	// Never echo a secret: provider errors should not contain request headers, but strip obvious Bearer fragments defensively.
	if i := strings.Index(strings.ToLower(s), "bearer "); i >= 0 {
		s = s[:i] + "[credential redacted]"
	}
	return s
}
func findObject(root map[string]any, keys []string) (map[string]any, bool) {
	for _, k := range keys {
		if v, ok := root[k].(map[string]any); ok {
			return v, true
		}
	}
	for _, v := range root {
		if m, ok := v.(map[string]any); ok {
			if found, ok := findObject(m, keys); ok {
				return found, true
			}
		}
	}
	return nil, false
}
func findPercent(m map[string]any) (float64, bool) {
	keys := []string{"percent", "percentage", "used_percent", "usage_percent", "usedPercentage", "used_percentage"}
	return findNumber(m, keys)
}
func findNumber(m map[string]any, keys []string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return n, true
			case string:
				if x, err := strconv.ParseFloat(strings.TrimSuffix(n, "%"), 64); err == nil {
					return x, true
				}
			}
		}
	}
	return 0, false
}
func findString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}
