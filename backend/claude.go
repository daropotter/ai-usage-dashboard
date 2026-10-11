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

// Odświeżanie tokenu chronimy blokadą (single-flight) i cooldownem, żeby
// równoległe żądania oraz zbyt częste próby nie pogłębiały limitu 429 z token
// endpointu Anthropic.
const (
	claudeRefreshSkew       = 5 * time.Minute
	claudeRefreshCooldown   = 5 * time.Minute
	claudeRateLimitCooldown = 15 * time.Minute
	claudeInvalidCooldown   = time.Hour
)

var (
	claudeMu      sync.Mutex
	claudeNextTry time.Time
	claudeLastErr error
)

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

// claudeHTTPError niesie status i ewentualne Retry-After z token endpointu.
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

// claudeCooldownError oznacza, że sami wstrzymaliśmy kolejne próby odświeżenia.
// Reason niesie prawdziwą przyczynę ostatniego błędu (np. HTTP 429, unieważniony
// refresh token), żeby komunikat nie zrzucał wszystkiego na "limit Anthropic".
type claudeCooldownError struct {
	Until  time.Time
	Reason string
}

func (e *claudeCooldownError) Error() string {
	msg := "odświeżanie tokenu Claude wstrzymane do " + e.Until.UTC().Format("15:04:05Z")
	if e.Reason != "" {
		msg += " (ostatni błąd refreshu: " + e.Reason + ")"
	}
	return msg
}

func fetchAnthropic(client *http.Client) ProviderResult {
	r := baseResult("anthropic", "Claude / Anthropic", "Claude OAuth usage API (nieoficjalne)", false)
	path := strings.TrimSpace(os.Getenv("CLAUDE_AUTH_FILE"))
	if path == "" {
		r.Message = "Ustaw CLAUDE_AUTH_FILE (ścieżka do .credentials.json z Claude Code)."
		return r
	}
	creds, err := readClaudeCreds(path)
	if err != nil {
		r.Message = "Nie można odczytać pliku poświadczeń Claude: " + err.Error()
		return r
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		r.Message = "Plik poświadczeń nie zawiera claudeAiOauth.accessToken."
		return r
	}
	r.Configured = true

	usageURL := envOr("CLAUDE_USAGE_URL", defaultClaudeUsageURL)

	// Proaktywne odświeżenie: nie czekamy na 401, gdy token jest blisko wygaśnięcia.
	if claudeNeedsRefresh(creds) {
		if err := claudeTryRefresh(client, path, &creds); err != nil {
			r.Status = "error"
			r.Message = "Token wymaga odświeżenia, ale się nie udało: " + safeProviderError(err)
			return r
		}
	}

	body, status, err := claudeGet(client, usageURL, creds.ClaudeAiOauth.AccessToken)
	if err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err)
		return r
	}
	// 401 mimo z pozoru ważnego tokenu (np. unieważniony przez inną sesję Claude Code).
	if status == http.StatusUnauthorized {
		if creds.ClaudeAiOauth.RefreshToken == "" {
			r.Status = "error"
			r.Message = "Token wygasł lub został unieważniony, a plik nie zawiera claudeAiOauth.refreshToken. Zaktualizuj CLAUDE_AUTH_FILE."
			return r
		}
		if err := claudeTryRefresh(client, path, &creds); err != nil {
			r.Status = "error"
			r.Message = "Token wygasł, odświeżenie nie powiodło się: " + safeProviderError(err)
			return r
		}
		body, status, err = claudeGet(client, usageURL, creds.ClaudeAiOauth.AccessToken)
		if err != nil {
			r.Status = "error"
			r.Message = safeProviderError(err)
			return r
		}
	}
	if status == http.StatusTooManyRequests {
		r.Status = "error"
		r.Message = "Anthropic ogranicza częstotliwość tego endpointu (HTTP 429). Spróbuj ponownie później."
		return r
	}
	if status < 200 || status >= 300 {
		r.Status = "error"
		r.Message = fmt.Sprintf("HTTP %d: %s", status, compactError(body))
		return r
	}

	var payload struct {
		FiveHour       *claudeWindow `json:"five_hour"`
		SevenDay       *claudeWindow `json:"seven_day"`
		SevenDaySonnet *claudeWindow `json:"seven_day_sonnet"`
		SevenDayOpus   *claudeWindow `json:"seven_day_opus"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		r.Message = "Endpoint odpowiedział, ale nie zwrócił poprawnego JSON."
		return r
	}

	add := func(w *claudeWindow, label string) {
		if w == nil || w.Utilization == nil {
			return
		}
		p := round1(clamp(*w.Utilization, 0, 100))
		r.Metrics = append(r.Metrics, Metric{Label: label, Value: usedPercentLabel(p), Percent: &p, ResetAt: w.ResetsAt})
	}
	add(payload.FiveHour, "5 h")
	add(payload.SevenDay, "Tydzień")
	add(payload.SevenDaySonnet, "Tydzień (Sonnet)")
	add(payload.SevenDayOpus, "Tydzień (Opus)")
	if sub := creds.ClaudeAiOauth.SubscriptionType; sub != "" {
		r.Metrics = append(r.Metrics, Metric{Label: "Plan", Value: strings.ToUpper(sub)})
	}
	if len(r.Metrics) == 0 {
		r.Message = "Endpoint odpowiedział, ale nie zwrócił okien limitów (konto bez aktywnej subskrypcji Pro/Max?)."
		return r
	}
	r.Status = "ok"
	r.Summary = "Limity subskrypcji"
	return r
}

// claudeNeedsRefresh mówi, czy token wygasa w ciągu claudeRefreshSkew.
func claudeNeedsRefresh(creds claudeCreds) bool {
	if creds.ClaudeAiOauth.AccessToken == "" || creds.ClaudeAiOauth.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(claudeRefreshSkew).UnixMilli() >= int64(creds.ClaudeAiOauth.ExpiresAt)
}

// claudeTryRefresh odświeża token z blokadą i cooldownem. Blokada sprawia, że
// równoległe żądania wykonają tylko jedną wymianę tokenu.
func claudeTryRefresh(client *http.Client, path string, creds *claudeCreds) error {
	claudeMu.Lock()
	defer claudeMu.Unlock()

	if time.Now().Before(claudeNextTry) {
		reason := ""
		if claudeLastErr != nil {
			reason = safeProviderError(claudeLastErr)
		}
		return &claudeCooldownError{Until: claudeNextTry, Reason: reason}
	}
	// Inne żądanie mogło już odświeżyć plik — użyj świeższych danych.
	if fresh, err := readClaudeCreds(path); err == nil && fresh.ClaudeAiOauth.AccessToken != creds.ClaudeAiOauth.AccessToken {
		*creds = fresh
		return nil
	}

	if err := refreshClaude(client, path, creds); err != nil {
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
				// refresh_token nieważny — nie ma sensu dobijać endpointu.
				cooldown = claudeInvalidCooldown
			}
		}
		claudeNextTry = time.Now().Add(cooldown)
		claudeLastErr = err
		return err
	}
	claudeNextTry = time.Time{}
	claudeLastErr = nil
	return nil
}

// claudeGet nie przerywa na non-2xx, żeby odróżnić wygasły token (401) od reszty.
func claudeGet(client *http.Client, endpoint, token string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeBetaHeader)
	// Bez poprawnego User-Agent endpoint wpada w agresywny rate limit (429).
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
// wartości z powrotem do pliku.
func refreshClaude(client *http.Client, path string, creds *claudeCreds) error {
	reqBody, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": creds.ClaudeAiOauth.RefreshToken,
		"client_id":     claudeClientID,
	})
	if err != nil {
		return err
	}
	tokenURL := envOr("CLAUDE_TOKEN_URL", defaultClaudeTokenURL)
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
		return fmt.Errorf("odpowiedź refresh nie zawiera access_token")
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

// parseRetryAfter rozumie zarówno liczbę sekund, jak i datę HTTP.
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

// persistClaudeCreds aktualizuje claudeAiOauth w pliku, zachowując inne pola.
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
	// Zapis w miejscu: katalog /app jest root-owned, a plik należy do aplikacji.
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
