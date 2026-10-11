package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Claude subscription limits via the (undocumented) OAuth usage endpoint that
// powers Claude Code's /usage command. Read-only: only the usage endpoint is
// called, no prompts are sent.
const (
	defaultClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	defaultClaudeTokenURL = "https://console.anthropic.com/v1/oauth/token"
	// Public Claude Code OAuth client id (not a secret).
	claudeClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeUserAgent  = "claude-code/2.1.0"
	claudeBetaHeader = "oauth-2025-04-20"
)

// Token refresh is protected by a lock (single-flight) and a cooldown so that
// parallel requests and too-frequent attempts do not deepen the 429 limit on the token
// endpointu Anthropic.
const (
	claudeRefreshCooldown   = 5 * time.Minute
	claudeRateLimitCooldown = time.Hour
	claudeInvalidCooldown   = time.Hour
)

// claudeRefreshState is kept per credentials file: the cooldown belongs to the
// refresh token, not to the tile.
type claudeRefreshState struct {
	mu      sync.Mutex
	nextTry time.Time
	lastErr error
}

type claudeCacheEntry struct {
	metrics []Metric
	at      time.Time
}

var (
	claudeStatesMu sync.Mutex
	claudeStates   = map[string]*claudeRefreshState{}

	// Last successful limit metrics per tile — shown when refresh/usage fails
	// (429, revoked token) so the tile is not empty.
	claudeCacheMu sync.Mutex
	claudeCache   = map[string]claudeCacheEntry{}
)

func claudeStateFor(path string) *claudeRefreshState {
	claudeStatesMu.Lock()
	defer claudeStatesMu.Unlock()
	st, ok := claudeStates[path]
	if !ok {
		st = &claudeRefreshState{}
		claudeStates[path] = st
	}
	return st
}

type claudeCreds struct {
	ClaudeAiOauth struct {
		AccessToken      string  `json:"accessToken"`
		RefreshToken     string  `json:"refreshToken"`
		ExpiresAt        float64 `json:"expiresAt"`
		SubscriptionType string  `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// claudeHTTPError carries the status and any Retry-After from the token endpoint.
type claudeHTTPError struct {
	Status     int
	RetryAfter time.Duration
	Body       string
}

func (e *claudeHTTPError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" (retry po %s)", e.RetryAfter.Round(time.Second))
	}
	return msg
}

// claudeCooldownError means we ourselves paused further refresh attempts.
// Reason carries the real cause of the last error (e.g. HTTP 429, revoked
// refresh token) so the message does not blame everything on the "Anthropic limit".
type claudeCooldownError struct {
	Until  time.Time
	Reason string
}

func (e *claudeCooldownError) Error() string {
	msg := "Claude token refresh paused until " + e.Until.UTC().Format("15:04:05Z")
	if e.Reason != "" {
		msg += " (last refresh error: " + e.Reason + ")"
	}
	return msg
}

func fetchAnthropic(client *http.Client, t TileConfig) ProviderResult {
	r := baseResult(t, "Claude OAuth usage API (unofficial)", false)
	path := t.Get("AUTH_FILE")
	if path == "" {
		r.Message = "Set " + t.EnvKey("AUTH_FILE") + " (path to .credentials.json from Claude Code)."
		return r
	}
	creds, err := readClaudeCreds(path)
	if err != nil {
		return claudeFail(r, "no_data", "Cannot read the Claude credentials file: "+err.Error())
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return claudeFail(r, "no_data", "The credentials file does not contain claudeAiOauth.accessToken.")
	}
	r.Configured = true

	usageURL := t.GetOr("URL", defaultClaudeUsageURL)

	// Refresh only on 401 — no proactive refresh. The Anthropic token endpoint
	// is heavily rate-limited (429), so we do not hammer it on
	// every dashboard visit while the current token still works.
	body, status, err := claudeGet(client, usageURL, creds.ClaudeAiOauth.AccessToken)
	if err != nil {
		return claudeFail(r, "error", safeProviderError(err))
	}
	// 401 despite a seemingly valid token (e.g. revoked by another Claude Code session).
	if status == http.StatusUnauthorized {
		if creds.ClaudeAiOauth.RefreshToken == "" {
			return claudeFail(r, "error", "Token expired or was revoked and the file has no claudeAiOauth.refreshToken. Update "+t.EnvKey("AUTH_FILE")+".")
		}
		if err := claudeTryRefresh(client, t.GetOr("TOKEN_URL", defaultClaudeTokenURL), path, &creds); err != nil {
			return claudeFail(r, "error", "Token expired and refresh failed: "+safeProviderError(err))
		}
		body, status, err = claudeGet(client, usageURL, creds.ClaudeAiOauth.AccessToken)
		if err != nil {
			return claudeFail(r, "error", safeProviderError(err))
		}
	}
	if status == http.StatusTooManyRequests {
		return claudeFail(r, "error", "Anthropic is rate-limiting the usage endpoint (HTTP 429). Try again later.")
	}
	if status < 200 || status >= 300 {
		return claudeFail(r, "error", fmt.Sprintf("HTTP %d: %s", status, compactError(body)))
	}

	var payload struct {
		FiveHour       *claudeWindow `json:"five_hour"`
		SevenDay       *claudeWindow `json:"seven_day"`
		SevenDaySonnet *claudeWindow `json:"seven_day_sonnet"`
		SevenDayOpus   *claudeWindow `json:"seven_day_opus"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return claudeFail(r, r.Status, "The endpoint responded but did not return valid JSON.")
	}

	add := func(w *claudeWindow, label string) {
		if w == nil || w.Utilization == nil {
			return
		}
		p := round1(clamp(*w.Utilization, 0, 100))
		r.Metrics = append(r.Metrics, Metric{Label: label, Value: usedPercentLabel(p), Percent: &p, ResetAt: w.ResetsAt})
	}
	add(payload.FiveHour, "5 h")
	add(payload.SevenDay, "Week")
	add(payload.SevenDaySonnet, "Week (Sonnet)")
	add(payload.SevenDayOpus, "Week (Opus)")
	if sub := creds.ClaudeAiOauth.SubscriptionType; sub != "" {
		r.Metrics = append(r.Metrics, Metric{Label: "Plan", Value: strings.ToUpper(sub)})
	}
	if len(r.Metrics) == 0 {
		return claudeFail(r, r.Status, "The endpoint responded but returned no limit windows (account without an active Pro/Max subscription?).")
	}
	r.Status = "ok"
	r.Summary = "Subscription limits"
	claudeSetCachedMetrics(r.ID, r.Metrics)
	return r
}

// claudeFail returns a tile error and, when we have remembered the last successful metrics,
// attaches them with a fetch-time note — a 429/limit does not leave an empty
// tile.
func claudeFail(r ProviderResult, status, msg string) ProviderResult {
	if metrics, at, ok := claudeCachedMetrics(r.ID); ok {
		r.Status = "error"
		r.Metrics = metrics
		r.Summary = "Subscription limits (stale data)"
		r.Message = fmt.Sprintf("Last successful fetch: %s. %s", at.UTC().Format("2006-01-02 15:04 UTC"), msg)
		return r
	}
	r.Status = status
	r.Message = msg
	return r
}

func claudeSetCachedMetrics(tileID string, m []Metric) {
	claudeCacheMu.Lock()
	defer claudeCacheMu.Unlock()
	claudeCache[tileID] = claudeCacheEntry{metrics: append([]Metric(nil), m...), at: time.Now().UTC()}
}

func claudeCachedMetrics(tileID string) ([]Metric, time.Time, bool) {
	claudeCacheMu.Lock()
	defer claudeCacheMu.Unlock()
	e, ok := claudeCache[tileID]
	if !ok || len(e.metrics) == 0 {
		return nil, time.Time{}, false
	}
	return append([]Metric(nil), e.metrics...), e.at, true
}

// claudeTryRefresh refreshes the token with a lock and cooldown. The lock ensures that
// parallel requests perform only one token exchange.
func claudeTryRefresh(client *http.Client, tokenURL, path string, creds *claudeCreds) error {
	st := claudeStateFor(path)
	st.mu.Lock()
	defer st.mu.Unlock()

	if time.Now().Before(st.nextTry) {
		reason := ""
		if st.lastErr != nil {
			reason = safeProviderError(st.lastErr)
		}
		return &claudeCooldownError{Until: st.nextTry, Reason: reason}
	}
	// Another request may have already refreshed the file — use the fresher data.
	if fresh, err := readClaudeCreds(path); err == nil && fresh.ClaudeAiOauth.AccessToken != creds.ClaudeAiOauth.AccessToken {
		*creds = fresh
		return nil
	}

	if err := refreshClaude(client, tokenURL, path, creds); err != nil {
		cooldown := claudeRefreshCooldown
		var he *claudeHTTPError
		if errors.As(err, &he) {
			switch {
			case he.Status == http.StatusTooManyRequests:
				cooldown = claudeRateLimitCooldown
				if he.RetryAfter > cooldown {
					cooldown = he.RetryAfter
				}
			case he.Status == http.StatusBadRequest || he.Status == http.StatusUnauthorized:
				// refresh_token invalid — no point hammering the endpoint.
				cooldown = claudeInvalidCooldown
			}
		}
		st.nextTry = time.Now().Add(cooldown)
		st.lastErr = err
		return err
	}
	st.nextTry = time.Time{}
	st.lastErr = nil
	return nil
}

// claudeGet does not abort on non-2xx so it can tell an expired token (401) from the rest.
func claudeGet(client *http.Client, endpoint, token string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeBetaHeader)
	// Without a proper User-Agent the endpoint falls into aggressive rate limiting (429).
	req.Header.Set("User-Agent", claudeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func readClaudeCreds(path string) (claudeCreds, error) {
	var creds claudeCreds
	b, err := os.ReadFile(path)
	if err != nil {
		return creds, err
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		return creds, err
	}
	return creds, nil
}

// refreshClaude wymienia refresh_token na nowy access token i zapisuje zrotowane
// values back to the file.
func refreshClaude(client *http.Client, tokenURL, path string, creds *claudeCreds) error {
	reqBody, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": creds.ClaudeAiOauth.RefreshToken,
		"client_id":     claudeClientID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "anthropic")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &claudeHTTPError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       compactError(body),
		}
	}
	var out struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		return fmt.Errorf("refresh response has no access_token")
	}
	creds.ClaudeAiOauth.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		creds.ClaudeAiOauth.RefreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		creds.ClaudeAiOauth.ExpiresAt = float64(time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UnixMilli())
	}
	return persistClaudeCreds(path, creds)
}

// parseRetryAfter understands both a number of seconds and an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// persistClaudeCreds updates claudeAiOauth in the file, preserving other fields.
func persistClaudeCreds(path string, creds *claudeCreds) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	oauth, _ := root["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		oauth = map[string]any{}
	}
	oauth["accessToken"] = creds.ClaudeAiOauth.AccessToken
	oauth["refreshToken"] = creds.ClaudeAiOauth.RefreshToken
	if creds.ClaudeAiOauth.ExpiresAt > 0 {
		oauth["expiresAt"] = int64(creds.ClaudeAiOauth.ExpiresAt)
	}
	root["claudeAiOauth"] = oauth

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	// Write in place: the /app directory is root-owned, while the file belongs to the app.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(out, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
