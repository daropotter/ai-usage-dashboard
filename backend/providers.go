package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func baseResult(t TileConfig, source string, configured bool) ProviderResult {
	return ProviderResult{ID: t.ID, Name: t.Name, Source: source, Configured: configured, Status: "no_data", Summary: "No data", Metrics: []Metric{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
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
		return fmt.Errorf("invalid JSON: %w", err)
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

func fetchOpenCodeQuota(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "OpenCode Go quota API", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + "."
		return r
	}

	var payload struct {
		Usage map[string]struct {
			Status   string  `json:"status"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resetsAt"`
		} `json:"usage"`
	}
	endpoint := t.GetOr("URL", "https://opencode.ai/zen/go/v1/usage")
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &payload); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err)
		return r
	}
	labels := []struct{ key, label string }{{"rolling", "5 h"}, {"weekly", "Week"}, {"monthly", "Month"}}
	for _, item := range labels {
		if w, ok := payload.Usage[item.key]; ok {
			p := round1(clamp(w.Percent, 0, 100))
			r.Metrics = append(r.Metrics, Metric{Label: item.label, Value: usedPercentLabel(p), Percent: &p, ResetAt: w.ResetsAt})
		}
	}
	if len(r.Metrics) == 0 {
		r.Message = "The endpoint responded but returned no supported usage windows."
		return r
	}
	r.Status = "ok"
	r.Summary = "Subscription limits"
	return r
}

func fetchOllama(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "Ollama Cloud API (/api/usage)", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + ". Without a key the tile stays in the No data state."
		return r
	}

	endpoint := t.GetOr("URL", "https://ollama.com/api/usage")
	endpoint = withOllamaRange(endpoint, t.Get("RANGE"))
	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &raw); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " The Ollama Cloud endpoint is not publicly guaranteed."
		return r
	}

	limit, _ := t.Float("MONTHLY_LIMIT_USD")
	haveQuota := appendOllamaQuotaMetrics(&r, raw, t.Get("RESET_ANCHOR"))
	haveUsage := appendOllamaUsageMetrics(&r, raw, limit)

	if len(r.Metrics) == 0 {
		r.Message = "The endpoint responded but the format contains no recognised usage data. Set " + t.EnvKey("URL") + " if you use a compatible endpoint/proxy."
		return r
	}
	r.Status = "ok"
	switch {
	case haveQuota && haveUsage:
		r.Summary = "Ollama Cloud limits and usage"
	case haveQuota:
		r.Summary = "Limity Ollama Cloud"
	default:
		r.Summary = "Ollama Cloud usage"
	}
	return r
}

// withOllamaRange appends ?range=24h|7d|30d if the user set
// TILE_<ID>_RANGE and the URL does not contain it yet.
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
// cached_input_tokens,output_tokens} and buckets[]. Since 2026-10 the endpoint
// returns exactly this shape (previously: limits.session/weekly/monthly).
// A positive budgetUSD (local limit) adds a percentage bar to the cost.
func appendOllamaUsageMetrics(r *ProviderResult, raw map[string]any, budgetUSD float64) bool {
	totals, ok := raw["totals"].(map[string]any)
	if !ok {
		return false
	}
	window := ollamaRangeLabel(raw)
	added := false

	if n, ok := findNumber(totals, []string{"request_count"}); ok {
		r.Metrics = append(r.Metrics, Metric{Label: "Requests (" + window + ")", Value: formatCount(n)})
		added = true
	}
	if cost, ok := findNumber(totals, []string{"usage_usd", "cost_usd"}); ok {
		r.Metrics = append(r.Metrics, Metric{Label: "Cost (" + window + ")", Value: fmt.Sprintf("%.2f USD", cost)})
		if budgetUSD > 0 {
			p := clamp(cost/budgetUSD*100, 0, 100)
			r.Metrics = append(r.Metrics, Metric{Label: "Custom monthly limit", Value: fmt.Sprintf("%.2f / %.2f USD", cost, budgetUSD), Percent: &p})
		}
		added = true
	}
	inTok, inOK := findNumber(totals, []string{"input_tokens"})
	outTok, outOK := findNumber(totals, []string{"output_tokens"})
	if inOK || outOK {
		rows := make([]MetricRow, 0, 3)
		if inOK {
			rows = append(rows, MetricRow{Label: "Input", Value: formatCount(inTok)})
		}
		if cached, ok := findNumber(totals, []string{"cached_input_tokens"}); ok {
			rows = append(rows, MetricRow{Label: "Cached", Value: formatCount(cached)})
		}
		if outOK {
			rows = append(rows, MetricRow{Label: "Output", Value: formatCount(outTok)})
		}
		r.Metrics = append(r.Metrics, Metric{Label: "Tokens (" + window + ")", Value: formatCount(inTok + outTok), Rows: rows})
		added = true
	}
	// Breakdown by time bucket (hours/days) — a stand-in for a per-model
	// breakdown, which the endpoint does not expose yet.
	if rows, sum := ollamaBucketRows(raw); len(rows) > 0 {
		r.Metrics = append(r.Metrics, Metric{Label: "Requests by day (" + window + ")", Value: formatCount(sum), Rows: rows})
		added = true
	}
	return added
}

// ollamaRangeLabel translates the response `range` field into a readable label.
func ollamaRangeLabel(raw map[string]any) string {
	switch findString(raw, []string{"range"}) {
	case "24h":
		return "24 h"
	case "7d", "":
		return "7 days"
	case "30d":
		return "30 days"
	default:
		return findString(raw, []string{"range"})
	}
}

// ollamaBucketRows returns non-zero buckets from buckets[] as rows
// (label = start date/time, value = request count) and their sum.
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

// ollamaModelRows returns the per-model groups from
// GET /api/usage?group_by=model as rows, most used first. The API ranks groups
// by cost (or requests for legacy plans); the list under the monthly limit is
// explicitly by request count. Each row shows requests and, when available,
// cost or total tokens.
func ollamaModelRows(raw map[string]any) []MetricRow {
	totals, ok := raw["totals"].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := totals["groups"].([]any)
	if !ok {
		return nil
	}
	type modelUse struct {
		name        string
		requests    float64
		hasRequests bool
		cost        float64
		hasCost     bool
		tokens      float64
		hasTokens   bool
	}
	list := make([]modelUse, 0, len(arr))
	for _, it := range arr {
		g, ok := it.(map[string]any)
		if !ok {
			continue
		}
		name := findString(g, []string{"model", "name"})
		if name == "" {
			name = "model"
		}
		m := modelUse{name: name}
		m.requests, m.hasRequests = findNumber(g, []string{"request_count", "requests", "count"})
		m.cost, m.hasCost = findNumber(g, []string{"usage_usd", "cost_usd", "cost"})
		in, inOK := findNumber(g, []string{"input_tokens"})
		out, outOK := findNumber(g, []string{"output_tokens"})
		m.tokens, m.hasTokens = in+out, inOK || outOK
		if !m.hasRequests && !m.hasCost && !m.hasTokens {
			continue
		}
		list = append(list, m)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].requests != list[j].requests {
			return list[i].requests > list[j].requests
		}
		if list[i].cost != list[j].cost {
			return list[i].cost > list[j].cost
		}
		return list[i].name < list[j].name
	})

	rows := make([]MetricRow, 0, len(list))
	for _, m := range list {
		parts := make([]string, 0, 2)
		if m.hasRequests {
			parts = append(parts, formatCount(m.requests)+" req")
		}
		if m.hasCost && m.cost > 0 {
			parts = append(parts, formatUSD4(m.cost))
		} else if m.hasTokens && m.tokens > 0 {
			parts = append(parts, formatCount(m.tokens)+" tok")
		}
		if len(parts) == 0 {
			continue
		}
		rows = append(rows, MetricRow{Label: m.name, Value: strings.Join(parts, " · ")})
	}
	return rows
}

// withOllamaGroupBy adds group_by=model unless the endpoint already has it.
func withOllamaGroupBy(endpoint, group string) string {
	if group == "" || strings.Contains(endpoint, "group_by=") {
		return endpoint
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + "group_by=" + url.QueryEscape(group)
}

// appendOllamaRecentModels adds a list of the most used models from
// GET /api/usage?group_by=model directly below the monthly limit. Failures are
// non-fatal: the balance tile still shows the limit without the breakdown.
func appendOllamaRecentModels(client *http.Client, r *ProviderResult, t TileConfig) {
	endpoint := t.GetOr("USAGE_URL", "https://ollama.com/api/usage")
	rng := t.GetOr("RANGE", "7d")
	switch rng {
	case "24h", "7d", "30d":
	default:
		rng = "7d"
	}
	endpoint = withOllamaRange(endpoint, rng)
	endpoint = withOllamaGroupBy(endpoint, "model")

	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + t.Get("API_KEY")}, &raw); err != nil {
		r.Message = "Model breakdown unavailable: " + safeProviderError(err)
		return
	}
	rows := ollamaModelRows(raw)
	if len(rows) == 0 {
		return
	}
	total := 0.0
	if totals, ok := raw["totals"].(map[string]any); ok {
		total, _ = findNumber(totals, []string{"request_count"})
	}
	metric := Metric{Label: "Top models (" + ollamaRangeLabel(raw) + ")", Value: formatCount(total), Rows: rows}

	// Keep the breakdown right under the monthly limit (or included credits).
	idx := -1
	for i := range r.Metrics {
		if r.Metrics[i].Label == "Monthly limit" {
			idx = i
		}
	}
	if idx < 0 {
		for i := range r.Metrics {
			if r.Metrics[i].Label == "Included credits" {
				idx = i
			}
		}
	}
	if idx < 0 {
		r.Metrics = append(r.Metrics, metric)
		return
	}
	r.Metrics = append(r.Metrics, Metric{})
	copy(r.Metrics[idx+2:], r.Metrics[idx+1:])
	r.Metrics[idx+1] = metric
}

// appendOllamaQuotaMetrics handles the older, undocumented format
// (/api/usage with limits.session|weekly|monthly.usage as a 0..1 fraction and
// activity.cost). Ollama switches the response shape, so we keep both.
// resetAnchor is an optional known monthly reset date (RFC3339).
func appendOllamaQuotaMetrics(r *ProviderResult, raw map[string]any, resetAnchor string) bool {
	added := false
	candidates := []struct {
		keys  []string
		label string
	}{
		{[]string{"session", "rolling", "five_hour", "5h", "session_usage"}, "5 h / session"},
		{[]string{"weekly", "week", "weekly_usage"}, "Week"},
		{[]string{"monthly", "month", "monthly_usage"}, "Month"},
	}
	for _, c := range candidates {
		if m, ok := findObject(raw, c.keys); ok {
			p, found := findPercent(m)
			if !found {
				// limits.<window>.usage is a 0..1 fraction (e.g. 0.932 = 93.2%
				// used). Values >1 are treated as already being percentages.
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
				if reset == "" && c.label == "Month" {
					reset = ollamaMonthlyReset(resetAnchor, time.Now().UTC())
				}
				r.Metrics = append(r.Metrics, Metric{Label: c.label, Value: usedPercentLabel(p), Percent: &p, ResetAt: reset})
				added = true
			}
		}
	}
	// Per-model request breakdown (older format) and overage cost.
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
					rows = append(rows, MetricRow{Label: x.name, Value: formatCount(float64(x.n))})
				}
				r.Metrics = append(r.Metrics, Metric{Label: "Requests by model (month)", Value: formatCount(float64(total)), Rows: rows})
				added = true
			}
		}
	}
	if act, ok := raw["activity"].(map[string]any); ok {
		if cost, ok := findNumber(act, []string{"cost"}); ok && cost > 0 {
			r.Metrics = append(r.Metrics, Metric{Label: "Cost (4 wk)", Value: fmt.Sprintf("%.2f USD", cost)})
			added = true
		}
	}
	return added
}

// formatCount formats an integer (requests, tokens) with "," thousands
// separators: 119600 -> "119,600".
func formatCount(v float64) string {
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	b.Grow(len(sign) + len(s) + len(s)/3)
	b.WriteString(sign)
	first := len(s) % 3
	if first > 0 {
		b.WriteString(s[:first])
	}
	for i := first; i < len(s); i += 3 {
		if b.Len() > len(sign) {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// formatUSD4 formats a small per-model cost (e.g. 0.0132 USD) without
// trailing zeros.
func formatUSD4(v float64) string {
	return strconv.FormatFloat(math.Round(v*10000)/10000, 'f', -1, 64) + " USD"
}

// fetchOllamaQuota is a separate tile with the account quota. GET /api/balance
// returns the monthly allowance and period (with reset date) — data that is no
// longer in /api/usage.
func fetchOllamaQuota(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "Ollama Cloud API (/api/balance)", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + ". Without a key the tile stays in the No data state."
		return r
	}

	endpoint := t.GetOr("URL", "https://ollama.com/api/balance")
	var raw map[string]any
	if err := doJSON(client, http.MethodGet, endpoint, map[string]string{"Authorization": "Bearer " + key}, &raw); err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err) + " The Ollama Cloud endpoint is not publicly guaranteed."
		return r
	}
	if !appendOllamaBalanceMetrics(&r, raw) {
		r.Message = "The /api/balance endpoint responded but with no recognised quota data. Set " + t.EnvKey("URL") + " if you use a compatible proxy."
		return r
	}
	appendOllamaRecentModels(client, &r, t)
	r.Status = "ok"
	r.Summary = "Ollama Cloud monthly limit"
	return r
}

// appendOllamaBalanceMetrics parsuje GET /api/balance:
// {included:{allowance_usd,balance_usd,period:{until}},purchased:{balance_usd}}.
// Usage is computed as (allowance - balance)/allowance; period.until is the
// reset date (previously it had to be guessed via a reset anchor).
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
		r.Metrics = append(r.Metrics, Metric{Label: "Monthly limit", Value: fmt.Sprintf("%.2f / %.2f USD", used, allowance), Percent: &p, ResetAt: reset})
		added = true
	} else if hasBalance {
		r.Metrics = append(r.Metrics, Metric{Label: "Included credits", Value: fmt.Sprintf("%.2f USD", balance), ResetAt: reset})
		added = true
	}
	if purchased, ok := raw["purchased"].(map[string]any); ok {
		if b, ok := findNumber(purchased, []string{"balance_usd", "balance"}); ok && b > 0 {
			r.Metrics = append(r.Metrics, Metric{Label: "Purchased credits", Value: fmt.Sprintf("%.2f USD", b)})
			added = true
		}
	}
	return added
}

func fetchOpenAI(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "OpenAI Organization Costs API", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + " (an organization admin key is recommended). A regular ChatGPT Plus/Pro subscription does not expose this counter through the API."
		return r
	}

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(start, 10))
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	endpoint := t.GetOr("URL", "https://api.openai.com/v1/organization/costs") + "?" + q.Encode()
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
		r.Message = safeProviderError(err) + " This endpoint usually requires an organization admin key; it does not show ChatGPT Plus/Pro limits."
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
	r.Metrics = append(r.Metrics, Metric{Label: "Cost this month", Value: fmt.Sprintf("%.2f %s", total, currency)})
	if limit, ok := t.Float("MONTHLY_LIMIT_USD"); ok && limit > 0 {
		p := clamp(total/limit*100, 0, 100)
		r.Metrics = append(r.Metrics, Metric{Label: "Custom monthly limit", Value: fmt.Sprintf("%.2f / %.2f USD", total, limit), Percent: &p})
	}
	r.Status = "ok"
	r.Summary = "Billing API (not the ChatGPT subscription quota)"
	return r
}

// round1 rounds to one decimal place (readable percentages).
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// usedPercentLabel formats a percentage rounded to 0.1 without a redundant ".0"
// (11 -> "11% used", 93.2 -> "93.2% used").
func usedPercentLabel(p float64) string {
	return strconv.FormatFloat(round1(p), 'f', -1, 64) + "% used"
}

// ollamaMonthlyReset returns the nearest reset date of Ollama's monthly limit.
// The /api/usage endpoint does not provide it, so we compute it from the anchor
// (any known reset date; projected into the future month by month). Without an anchor
// we use an approximation: the 1st day of next month 00:00 UTC.
func ollamaMonthlyReset(anchor string, now time.Time) string {
	if s := strings.TrimSpace(anchor); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return nextMonthlyReset(t.UTC(), now).Format(time.RFC3339)
		}
	}
	return firstDayNextMonthUTC()
}

// nextMonthlyReset shifts the anchor by whole months until it falls after `now`,
// keeping the day and time (day clamped to the month length).
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

// firstDayNextMonthUTC returns the start of next month at 00:00 UTC in RFC3339.
func firstDayNextMonthUTC() string {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	return next.Format(time.RFC3339)
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
