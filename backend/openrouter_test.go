package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenRouterBilling(t *testing.T) {
	for _, tc := range []struct {
		name, keyData, credits  string
		creditsStatus           int
		wantStatus              string
		wantBalance, wantBudget bool
	}{
		{"limited", `{"usage":25,"usage_daily":1,"usage_weekly":4,"usage_monthly":12,"limit":20,"limit_remaining":8,"limit_reset":"monthly","free_model_daily_requests":{"used":30,"limit":1000},"label":"secret-fragment"}`, `{"total_credits":100,"total_usage":40}`, 200, "ok", true, true},
		{"unlimited zero usage", `{"usage":0,"usage_daily":0,"usage_weekly":0,"usage_monthly":0,"limit":null,"limit_remaining":null}`, `{"total_credits":0,"total_usage":0}`, 200, "ok", true, false},
		{"credits forbidden", `{"usage_monthly":12}`, `{"error":{"message":"secret-fragment"}}`, 403, "ok", false, false},
		{"unknown shape", `{}`, `{}`, 200, "no_data", false, false},
		{"null data", `null`, `null`, 200, "no_data", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("expected an authenticated GET")
				}
				switch r.URL.Path {
				case "/key":
					w.Write([]byte(`{"data":` + tc.keyData + `}`))
				case "/credits":
					w.WriteHeader(tc.creditsStatus)
					w.Write([]byte(`{"data":` + tc.credits + `}`))
				default:
					t.Error("unexpected endpoint")
				}
			}))
			defer srv.Close()
			t.Setenv("TILE_TEST_API_KEY", "test-key")
			t.Setenv("TILE_TEST_URL", srv.URL+"/key")
			t.Setenv("TILE_TEST_CREDITS_URL", srv.URL+"/credits")
			r := fetchTile(srv.Client(), TileConfig{ID: "test", Type: "openrouter", Name: "OpenRouter", envPrefix: "TILE_TEST_"})
			if r.Status != tc.wantStatus || !r.Configured {
				t.Fatalf("unexpected result: %+v", r)
			}
			if (metricByLabel(r, "Credits remaining") != nil) != tc.wantBalance {
				t.Fatalf("unexpected balance: %+v", r.Metrics)
			}
			if (metricByLabel(r, "API key budget (monthly)") != nil) != tc.wantBudget {
				t.Fatalf("unexpected budget: %+v", r.Metrics)
			}
			if tc.name == "limited" {
				if m := metricByLabel(r, "Credits remaining"); m.Value != "60.00 USD" {
					t.Fatalf("balance = %+v", m)
				}
				if m := metricByLabel(r, "API key budget (monthly)"); m.Value != "12.00 / 20.00 USD used" || m.Percent == nil || *m.Percent != 60 {
					t.Fatalf("budget = %+v", m)
				}
				if m := metricByLabel(r, "Free model requests today"); m.Value != "30 / 1,000" || m.Percent == nil || *m.Percent != 3 {
					t.Fatalf("free requests = %+v", m)
				}
			}
			if tc.name == "unlimited zero usage" {
				if m := metricByLabel(r, "Cost this month (API key)"); m == nil || m.Value != "0.00 USD" || len(m.Rows) != 3 {
					t.Fatalf("zero usage = %+v", m)
				}
			}
			if tc.name == "credits forbidden" && r.Message == "" {
				t.Fatal("missing balance explanation")
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "secret-fragment") || strings.Contains(string(b), "test-key") {
				t.Fatal("credential data leaked")
			}
		})
	}
}

func TestOpenRouterMissingKeyAndFetchError(t *testing.T) {
	tile := TileConfig{ID: "test", Type: "openrouter", envPrefix: "TILE_TEST_"}
	t.Setenv("TILE_TEST_API_KEY", "")
	r := fetchTile(http.DefaultClient, tile)
	if r.Status != "no_data" || r.Configured || !strings.Contains(r.Message, "TILE_TEST_API_KEY") {
		t.Fatalf("missing key: %+v", r)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"test-secret"}`))
	}))
	defer srv.Close()
	t.Setenv("TILE_TEST_API_KEY", "test-secret")
	t.Setenv("TILE_TEST_URL", srv.URL)
	r = fetchTile(srv.Client(), tile)
	if r.Status != "error" || !r.Configured || strings.Contains(r.Message, "test-secret") {
		t.Fatalf("fetch error: %+v", r)
	}
}
