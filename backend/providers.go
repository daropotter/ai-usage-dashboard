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
			r.Metrics = append(r.Metrics, Metric{Label: item.label, Value: usedPercentLabel(p), Percent: &p, ResetAt: w.ResetsAt})
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
	r := baseResult("ollama-cloud", "Ollama Cloud", "Ollama Cloud API (/api/usage)", key != "")
	if key == "" {
		r.Message = "Ustaw OLLAMA_API_KEY. Bez klucza kafelek pozostaje w stanie Brak danych."
		return r
	}

	endpoint := envOr("OLLAMA_USAGE_URL", "https://ollama.com/api/usage")
	endpoint = withOllamaRange(endpoint, strings.TrimSpace(os.Getenv("OLLAMA_USAGE_RANGE")))
	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &raw); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " Endpoint Ollama Cloud nie jest publicznie gwarantowany."
		return r
	}

	haveQuota := appendOllamaQuotaMetrics(&r, raw)
	haveUsage := appendOllamaUsageMetrics(&r, raw)

	if len(r.Metrics) == 0 {
		r.Message = "Endpoint odpowiedział, ale format nie zawiera rozpoznanych danych zużycia. Ustaw OLLAMA_USAGE_URL, jeśli używasz kompatybilnego endpointu/proxy."
		return r
	}
	r.Status = "ok"
	switch {
	case haveQuota && haveUsage:
		r.Summary = "Limity i zużycie Ollama Cloud"
	case haveQuota:
		r.Summary = "Limity Ollama Cloud"
	default:
		r.Summary = "Zużycie Ollama Cloud"
	}
	return r
}

// withOllamaRange dokłada ?range=24h|7d|30d, jeśli użytkownik ustawił
// OLLAMA_USAGE_RANGE, a adres jeszcze go nie zawiera.
func withOllamaRange(endpoint, rng string) string {
	if rng == "" || strings.Contains(endpoint, "range=") {
		return endpoint
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + "range=" + url.QueryEscape(rng)
}

// appendOllamaUsageMetrics parsuje udokumentowany format Ollama Cloud
// (GET /api/usage): totals.{request_count,usage_usd,input_tokens,
// cached_input_tokens,output_tokens} oraz buckets[]. Od 2026-10 endpoint
// zwraca właśnie ten kształt (wcześniej: limits.session/weekly/monthly).
func appendOllamaUsageMetrics(r *ProviderResult, raw map[string]any) bool {
	totals, ok := raw["totals"].(map[string]any)
	if !ok {
		return false
	}
	window := ollamaRangeLabel(raw)
	added := false

	if n, ok := findNumber(totals, []string{"request_count"}); ok {
		r.Metrics = append(r.Metrics, Metric{Label: "Żądania (" + window + ")", Value: formatCount(n)})
		added = true
	}
	if cost, ok := findNumber(totals, []string{"usage_usd", "cost_usd"}); ok {
		r.Metrics = append(r.Metrics, Metric{Label: "Koszt (" + window + ")", Value: fmt.Sprintf("%.2f USD", cost)})
		if limit, ok := envFloat("OLLAMA_MONTHLY_LIMIT_USD"); ok && limit > 0 {
			p := clamp(cost/limit*100, 0, 100)
			r.Metrics = append(r.Metrics, Metric{Label: "Własny limit miesięczny", Value: fmt.Sprintf("%.2f / %.2f USD", cost, limit), Percent: &p})
		}
		added = true
	}
	inTok, inOK := findNumber(totals, []string{"input_tokens"})
	outTok, outOK := findNumber(totals, []string{"output_tokens"})
	if inOK || outOK {
		rows := make([]MetricRow, 0, 3)
		if inOK {
			rows = append(rows, MetricRow{Label: "Wejściowe", Value: formatCount(inTok)})
		}
		if cached, ok := findNumber(totals, []string{"cached_input_tokens"}); ok {
			rows = append(rows, MetricRow{Label: "Z cache", Value: formatCount(cached)})
		}
		if outOK {
			rows = append(rows, MetricRow{Label: "Wyjściowe", Value: formatCount(outTok)})
		}
		r.Metrics = append(r.Metrics, Metric{Label: "Tokeny (" + window + ")", Value: formatCount(inTok + outTok), Rows: rows})
		added = true
	}
	// Rozbicie na przedziały czasowe (godziny/dni) — namiastka rozbicia na
	// modele, którego endpoint na razie nie udostępnia.
	if rows, sum := ollamaBucketRows(raw); len(rows) > 0 {
		r.Metrics = append(r.Metrics, Metric{Label: "Żądania wg dnia (" + window + ")", Value: formatCount(sum), Rows: rows})
		added = true
	}
	return added
}

// ollamaRangeLabel tłumaczy pole `range` z odpowiedzi na czytelną etykietę.
func ollamaRangeLabel(raw map[string]any) string {
	switch findString(raw, []string{"range"}) {
	case "24h":
		return "24 h"
	case "7d", "":
		return "7 dni"
	case "30d":
		return "30 dni"
	default:
		return findString(raw, []string{"range"})
	}
}

// ollamaBucketRows zwraca niezerowe przedziały z buckets[] jako wiersze
// (etykieta = data/godzina początku, wartość = liczba żądań) i ich sumę.
func ollamaBucketRows(raw map[string]any) ([]MetricRow, float64) {
	arr, ok := raw["buckets"].([]any)
	if !ok {
		return nil, 0
	}
	rows := make([]MetricRow, 0, len(arr))
	sum := 0.0
	for _, it := range arr {
		o, ok := it.(map[string]any)
		if !ok {
			continue
		}
		n, ok := findNumber(o, []string{"request_count"})
		if !ok || n <= 0 {
			continue
		}
		label := findString(o, []string{"from"})
		if len(label) >= 10 {
			label = label[:10]
		}
		rows = append(rows, MetricRow{Label: label, Value: formatCount(n)})
		sum += n
	}
	return rows, sum
}

// appendOllamaQuotaMetrics obsługuje starszy, nieudokumentowany format
// (/api/usage z limits.session|weekly|monthly.usage jako ułamek 0..1 oraz
// activity.cost). Ollama przełącza kształt odpowiedzi, więc trzymamy oba.
func appendOllamaQuotaMetrics(r *ProviderResult, raw map[string]any) bool {
	added := false
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
				// limits.<okno>.usage to ułamek 0..1 (np. 0.932 = 93,2%
				// wykorzystania). Wartości >1 traktujemy jako już w procentach.
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
				r.Metrics = append(r.Metrics, Metric{Label: c.label, Value: usedPercentLabel(p), Percent: &p, ResetAt: reset})
				added = true
			}
		}
	}
	// Rozbicie żądań na modele (starszy format) i koszt overage.
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
				added = true
			}
		}
	}
	if act, ok := raw["activity"].(map[string]any); ok {
		if cost, ok := findNumber(act, []string{"cost"}); ok && cost > 0 {
			r.Metrics = append(r.Metrics, Metric{Label: "Koszt (4 tyg.)", Value: fmt.Sprintf("%.2f USD", cost)})
			added = true
		}
	}
	return added
}

// formatCount formatuje liczbę całkowitą (żądania, tokeny) bez separatorów.
func formatCount(v float64) string {
	return strconv.FormatInt(int64(math.Round(v)), 10)
}

// fetchOllamaQuota to oddzielny kafelek z limitem konta. GET /api/balance
// zwraca miesięczny allowance i okres (z datą resetu) — danych, których nie ma
// już w /api/usage.
func fetchOllamaQuota(client *http.Client) ProviderResult {
	key := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	r := baseResult("ollama-cloud-quota", "Ollama Cloud — limit", "Ollama Cloud API (/api/balance)", key != "")
	if key == "" {
		r.Message = "Ustaw OLLAMA_API_KEY. Bez klucza kafelek pozostaje w stanie Brak danych."
		return r
	}

	endpoint := envOr("OLLAMA_BALANCE_URL", "https://ollama.com/api/balance")
	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &raw); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " Endpoint Ollama Cloud nie jest publicznie gwarantowany."
		return r
	}
	if !appendOllamaBalanceMetrics(&r, raw) {
		r.Message = "Endpoint /api/balance odpowiedział, ale bez rozpoznanych danych limitu. Ustaw OLLAMA_BALANCE_URL, jeśli używasz kompatybilnego proxy."
		return r
	}
	r.Status = "ok"
	r.Summary = "Limit miesięczny Ollama Cloud"
	return r
}

// appendOllamaBalanceMetrics parsuje GET /api/balance:
// {included:{allowance_usd,balance_usd,period:{until}},purchased:{balance_usd}}.
// Wykorzystanie liczymy jako (allowance - balance)/allowance; period.until to
// data resetu (wcześniej trzeba ją było zgadywać przez OLLAMA_RESET_ANCHOR).
func appendOllamaBalanceMetrics(r *ProviderResult, raw map[string]any) bool {
	added := false
	included, _ := raw["included"].(map[string]any)
	allowance, hasAllowance := findNumber(included, []string{"allowance_usd", "allowance"})
	balance, hasBalance := findNumber(included, []string{"balance_usd", "balance"})
	reset := ""
	if period, ok := included["period"].(map[string]any); ok {
		reset = findString(period, []string{"until", "resets_at", "reset_at", "ending_at"})
	}

	if hasAllowance && allowance > 0 {
		used := clamp(allowance-balance, 0, allowance)
		p := round1(clamp(used/allowance*100, 0, 100))
		r.Metrics = append(r.Metrics, Metric{Label: "Limit miesięczny", Value: fmt.Sprintf("%.2f / %.2f USD", used, allowance), Percent: &p, ResetAt: reset})
		added = true
	} else if hasBalance {
		r.Metrics = append(r.Metrics, Metric{Label: "Środki wliczone", Value: fmt.Sprintf("%.2f USD", balance), ResetAt: reset})
		added = true
	}
	if purchased, ok := raw["purchased"].(map[string]any); ok {
		if b, ok := findNumber(purchased, []string{"balance_usd", "balance"}); ok && b > 0 {
			r.Metrics = append(r.Metrics, Metric{Label: "Dokupione środki", Value: fmt.Sprintf("%.2f USD", b)})
			added = true
		}
	}
	return added
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

// usedPercentLabel formatuje procent zaokrąglony do 0.1 bez zbędnego ".0"
// (11 -> "11% użyte", 93.2 -> "93.2% użyte").
func usedPercentLabel(p float64) string {
	return strconv.FormatFloat(round1(p), 'f', -1, 64) + "% użyte"
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
