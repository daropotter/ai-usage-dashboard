package main

import (
	"fmt"
	"net/http"
)

// fetchOpenRouter reads billing metadata only. Key labels (which can contain
// credential fragments) and upstream error bodies never reach the dashboard.
func fetchOpenRouter(client *http.Client, t TileConfig) ProviderResult {
	key := t.Get("API_KEY")
	r := baseResult(t, "OpenRouter billing API", key != "")
	if key == "" {
		r.Message = "Set " + t.EnvKey("API_KEY") + "."
		return r
	}
	headers := map[string]string{"Authorization": "Bearer " + key}
	var payload struct {
		Data struct {
			Usage          *float64 `json:"usage"`
			UsageDaily     *float64 `json:"usage_daily"`
			UsageWeekly    *float64 `json:"usage_weekly"`
			UsageMonthly   *float64 `json:"usage_monthly"`
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     string   `json:"limit_reset"`
			FreeRequests   *struct {
				Used      *float64 `json:"used"`
				Limit     *float64 `json:"limit"`
				Remaining *float64 `json:"remaining"`
			} `json:"free_model_daily_requests"`
		} `json:"data"`
	}
	if err := doJSON(client, http.MethodGet, t.GetOr("URL", "https://openrouter.ai/api/v1/key"), headers, &payload); err != nil {
		r.Status = "error"
		r.Message = "Could not fetch OpenRouter API key usage. Check " + t.EnvKey("API_KEY") + " and the endpoint."
		return r
	}

	var credits struct {
		Data struct {
			TotalCredits *float64 `json:"total_credits"`
			TotalUsage   *float64 `json:"total_usage"`
		} `json:"data"`
	}
	err := doJSON(client, http.MethodGet, t.GetOr("CREDITS_URL", "https://openrouter.ai/api/v1/credits"), headers, &credits)
	haveCredits := err == nil && credits.Data.TotalCredits != nil && credits.Data.TotalUsage != nil
	if haveCredits {
		purchased, spent := *credits.Data.TotalCredits, *credits.Data.TotalUsage
		r.Metrics = append(r.Metrics, Metric{
			Label: "Credits remaining", Value: fmt.Sprintf("%.2f USD", purchased-spent),
			Rows: []MetricRow{
				{Label: "Purchased (all time)", Value: fmt.Sprintf("%.2f USD", purchased)},
				{Label: "Spent (all time)", Value: fmt.Sprintf("%.2f USD", spent)},
			},
		})
	} else {
		r.Message = "Account balance unavailable. Showing usage for this API key only; account credits may require a management key."
	}

	d := payload.Data
	var rows []MetricRow
	for _, item := range []struct {
		label string
		value *float64
	}{{"Today", d.UsageDaily}, {"This week", d.UsageWeekly}, {"All time", d.Usage}} {
		if item.value != nil {
			rows = append(rows, MetricRow{Label: item.label, Value: fmt.Sprintf("%.2f USD", *item.value)})
		}
	}
	if d.UsageMonthly != nil {
		r.Metrics = append(r.Metrics, Metric{Label: "Cost this month (API key)", Value: fmt.Sprintf("%.2f USD", *d.UsageMonthly), Rows: rows})
	} else if len(rows) > 0 {
		r.Metrics = append(r.Metrics, Metric{Label: "API key usage", Value: "USD", Rows: rows})
	}
	if d.Limit != nil && d.LimitRemaining != nil {
		label := "API key budget"
		if d.LimitReset != "" {
			label += " (" + d.LimitReset + ")"
		}
		m := Metric{Label: label, Value: fmt.Sprintf("%.2f / %.2f USD used", *d.Limit-*d.LimitRemaining, *d.Limit)}
		if *d.Limit > 0 {
			p := clamp((*d.Limit-*d.LimitRemaining)/(*d.Limit)*100, 0, 100)
			m.Percent = &p
		}
		r.Metrics = append(r.Metrics, m)
	}
	if f := d.FreeRequests; f != nil && f.Used != nil && f.Limit != nil {
		m := Metric{Label: "Free model requests today", Value: formatCount(*f.Used) + " / " + formatCount(*f.Limit)}
		if *f.Limit > 0 {
			p := clamp(*f.Used/(*f.Limit)*100, 0, 100)
			m.Percent = &p
		}
		r.Metrics = append(r.Metrics, m)
	}
	if len(r.Metrics) == 0 {
		r.Message = "The OpenRouter endpoints returned no supported billing or usage data."
		return r
	}
	r.Status = "ok"
	r.Summary = "API key usage and limits"
	if haveCredits {
		r.Summary = "Account balance and API key usage"
	}
	return r
}
