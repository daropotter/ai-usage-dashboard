package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// claudeTestTile points a claude tile at a fake usage endpoint and a temp credentials file.
func claudeTestTile(t *testing.T, id string, handler http.HandlerFunc) (TileConfig, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	creds := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"tok","subscriptionType":"max"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tile := TileConfig{ID: id, Type: "claude", Name: "Claude", envPrefix: tileEnvPrefix(id)}
	t.Setenv(tile.EnvKey("AUTH_FILE"), creds)
	t.Setenv(tile.EnvKey("URL"), srv.URL)
	return tile, &calls
}

const claudeUsageOK = `{"five_hour":{"utilization":12,"resets_at":"2026-10-20T05:00:00Z"},"seven_day":{"utilization":40}}`

func TestClaudeUsageCachedAndSingleFlight(t *testing.T) {
	tile, calls := claudeTestTile(t, "claude-cache-test", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte(claudeUsageOK))
	})
	var wg sync.WaitGroup
	results := make([]ProviderResult, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = fetchAnthropic(http.DefaultClient, tile) }(i)
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("parallel fetches made %d upstream calls, want 1", n)
	}
	for _, r := range results {
		if r.Status != "ok" || metricByLabel(r, "5 h") == nil {
			t.Fatalf("unexpected result %+v", r)
		}
	}
	fetchAnthropic(http.DefaultClient, tile)
	if n := calls.Load(); n != 1 {
		t.Fatalf("fresh cache still called upstream (%d calls)", n)
	}
	t.Setenv(tile.EnvKey("CACHE_SECONDS"), "0")
	fetchAnthropic(http.DefaultClient, tile)
	if n := calls.Load(); n != 2 {
		t.Fatalf("CACHE_SECONDS=0 should refetch, got %d calls", n)
	}
}

func TestClaudeUsageBacksOffAfter429(t *testing.T) {
	tile, calls := claudeTestTile(t, "claude-429-test", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	first := fetchAnthropic(http.DefaultClient, tile)
	second := fetchAnthropic(http.DefaultClient, tile)
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls during backoff = %d, want 1", n)
	}
	for _, r := range []ProviderResult{first, second} {
		if r.Status != "error" || !strings.Contains(r.Message, "HTTP 429") || !strings.Contains(r.Message, "next try at") {
			t.Fatalf("unexpected result %+v", r)
		}
	}
	st := claudeUsageStateFor(tile.Get("AUTH_FILE"))
	if wait := time.Until(st.nextTry); wait < 119*time.Minute {
		t.Fatalf("Retry-After ignored: next try in %s", wait)
	}
}

func TestClaudeUsageBackoffGrowsAndCaps(t *testing.T) {
	st := &claudeUsageState{}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		st.rateLimited(0)
		if st.backoff != w {
			t.Fatalf("429 #%d: backoff %s, want %s", i+1, st.backoff, w)
		}
	}
}
