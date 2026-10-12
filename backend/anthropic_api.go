package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Anthropic API (pay-as-you-go) usage via the Usage & Cost Admin API. Needs an
// organization Admin API key (sk-ant-admin...); a regular API key and Claude
// Pro/Max subscriptions are not accepted. Both reports are month-to-date with
// daily buckets. Report reads consume no model tokens.

const anthropicAPIMaxPages = 5

type anthropicUsageResult struct {
	UncachedInputTokens  float64 `json:"uncached_input_tokens"`
	CacheReadInputTokens float64 `json:"cache_read_input_tokens"`
	CacheCreation        struct {
		Ephemeral5m float64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h float64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokens float64 `json:"output_tokens"`
}

type anthropicUsagePage struct {
	Data []struct {
		Results []anthropicUsageResult `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

type anthropicCostPage struct {
	Data []struct {
		Results []struct {
			Amount   string `json:"amount"` // decimal string, in cents
			Currency string `json:"currency"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

type anthropicTokenTotals struct {
	input, cacheRead, cacheWrite, output float64
}

func (s *anthropicTokenTotals) add(p anthropicUsagePage) {
	for _, b := range p.Data {
		for _, x := range b.Results {
			s.input += x.UncachedInputTokens
			s.cacheRead += x.CacheReadInputTokens
			s.cacheWrite += x.CacheCreation.Ephemeral5m + x.CacheCreation.Ephemeral1h
			s.output += x.OutputTokens
		}
	}
}

func anthropicCostCents(p anthropicCostPage) (float64, string) {
	cents, currency := 0.0, ""
	for _, b := range p.Data {
		for _, x := range b.Results {
			if n, err := strconv.ParseFloat(x.Amount, 64); err == nil {
				cents += n
			}
			if x.Currency != "" {
				currency = strings.ToUpper(x.Currency)
			}
		}
	}
	return cents, currency
}

// anthropicReportQuery covers the current month in daily buckets.
func anthropicReportQuery(now time.Time) url.Values {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	q := url.Values{}
	q.Set("starting_at", start.Format(time.RFC3339))
	q.Set("ending_at", now.Add(time.Minute).Format(time.RFC3339))
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	return q
}

// anthropicGetPages follows next_page up to anthropicAPIMaxPages times.
func anthropicGetPages[T any](client *http.Client, base string, q url.Values, headers map[string]string, next func(*T) (bool, string), each func(T)) error {
	for i := 0; i < anthropicAPIMaxPages; i++ {
		var page T
		if err := doJSON(client, http.MethodGet, base+"?"+q.Encode(), headers, &page); err != nil {
			return err
		}
		each(page)
		more, token := next(&page)
		if !more || token == "" {
			return nil
		}
		q.Set("page", token)
	}
	return nil
}

func fetchAnthropicAPI(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "Anthropic Usage & Cost Admin API", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + " (an organization Admin API key, sk-ant-admin...). Claude Pro/Max subscriptions are shown by the claude tile type."
		return r
	}
	headers := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	now := time.Now().UTC()

	var tokens anthropicTokenTotals
	usageErr := anthropicGetPages(client, t.GetOr("USAGE_URL", "https://api.anthropic.com/v1/organizations/usage_report/messages"),
		anthropicReportQuery(now), headers,
		func(p *anthropicUsagePage) (bool, string) { return p.HasMore, p.NextPage },
		func(p anthropicUsagePage) { tokens.add(p) })

	cents, currency := 0.0, ""
	costErr := anthropicGetPages(client, t.GetOr("COST_URL", "https://api.anthropic.com/v1/organizations/cost_report"),
		anthropicReportQuery(now), headers,
		func(p *anthropicCostPage) (bool, string) { return p.HasMore, p.NextPage },
		func(p anthropicCostPage) {
			c, cur := anthropicCostCents(p)
			cents += c
			if cur != "" {
				currency = cur
			}
		})

	if usageErr != nil && costErr != nil {
		r.Status = "error"
		r.Message = safeProviderError(usageErr) + " The Admin API requires an organization Admin key and is not available for individual accounts."
		return r
	}
	if usageErr == nil {
		total := tokens.input + tokens.cacheRead + tokens.cacheWrite + tokens.output
		r.Metrics = append(r.Metrics, Metric{Label: "Tokens this month", Value: formatCount(total), Rows: []MetricRow{
			{Label: "Input", Value: formatCount(tokens.input)},
			{Label: "Cache read", Value: formatCount(tokens.cacheRead)},
			{Label: "Cache write", Value: formatCount(tokens.cacheWrite)},
			{Label: "Output", Value: formatCount(tokens.output)},
		}})
	}
	if costErr == nil {
		if currency == "" {
			currency = "USD"
		}
		total := cents / 100.0
		r.Metrics = append(r.Metrics, Metric{Label: "Cost this month", Value: fmt.Sprintf("%.2f %s", total, currency)})
		if limit, ok := t.Float("MONTHLY_LIMIT_USD"); ok && limit > 0 {
			p := clamp(total/limit*100, 0, 100)
			r.Metrics = append(r.Metrics, Metric{Label: "Custom monthly limit", Value: fmt.Sprintf("%.2f / %.2f USD", total, limit), Percent: &p})
		}
	}
	switch {
	case usageErr != nil:
		r.Message = "Usage report unavailable: " + safeProviderError(usageErr)
	case costErr != nil:
		r.Message = "Cost report unavailable: " + safeProviderError(costErr)
	}
	r.Status = "ok"
	r.Summary = "API usage (not the Claude Pro/Max quota)"
	return r
}
