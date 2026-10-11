package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ChatGPT / Codex subscription limits via the (unofficial) ChatGPT backend that
// Codex CLI and various clients use. Read-only: only the usage endpoint is
// called, no prompts are sent.
const (
	defaultChatGPTUsageURL = "https://chatgpt.com/backend-api/wham/usage"
	defaultChatGPTTokenURL = "https://auth.openai.com/oauth/token"
	// Public PKCE client id of the Codex CLI (not a secret).
	chatGPTClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatGPTUserAgent = "codex_cli_rs/0.142.5"
)

type chatGPTWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	LimitWindowSecs   *float64 `json:"limit_window_seconds"`
	ResetAfterSeconds *float64 `json:"reset_after_seconds"`
	ResetAt           *float64 `json:"reset_at"`
}

type chatGPTAuth struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

func fetchChatGPT(client *http.Client) ProviderResult {
	r := baseResult("chatgpt", "ChatGPT (subscription)", "ChatGPT backend usage API (unofficial)", false)
	path := strings.TrimSpace(os.Getenv("CHATGPT_AUTH_FILE"))
	if path == "" {
		r.Message = "Set CHATGPT_AUTH_FILE (path to auth.json from Codex CLI)."
		return r
	}
	auth, err := readChatGPTAuth(path)
	if err != nil {
		r.Message = "Cannot read the ChatGPT auth file: " + err.Error()
		return r
	}
	if auth.Tokens.AccessToken == "" {
		r.Message = "The auth file does not contain tokens.access_token."
		return r
	}
	r.Configured = true

	usageURL := envOr("CHATGPT_USAGE_URL", defaultChatGPTUsageURL)
	body, status, err := chatGPTGet(client, usageURL, auth.Tokens.AccessToken, auth.Tokens.AccountID)
	if err != nil {
		r.Status = "error"
		r.Message = safeProviderError(err)
		return r
	}
	if status == http.StatusUnauthorized {
		if auth.Tokens.RefreshToken == "" {
			r.Status = "error"
			r.Message = "Token expired and the file has no tokens.refresh_token. Update CHATGPT_AUTH_FILE."
			return r
		}
		if err := refreshChatGPT(client, path, &auth); err != nil {
			r.Status = "error"
			r.Message = "Token expired and refresh failed: " + safeProviderError(err)
			return r
		}
		body, status, err = chatGPTGet(client, usageURL, auth.Tokens.AccessToken, auth.Tokens.AccountID)
		if err != nil {
			r.Status = "error"
			r.Message = safeProviderError(err)
			return r
		}
	}
	if status < 200 || status >= 300 {
		r.Status = "error"
		r.Message = fmt.Sprintf("HTTP %d: %s", status, compactError(body))
		return r
	}

	var payload struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			PrimaryWindow   *chatGPTWindow `json:"primary_window"`
			SecondaryWindow *chatGPTWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		r.Message = "The endpoint responded but did not return valid JSON."
		return r
	}

	addWindow := func(w *chatGPTWindow, fallbackLabel string) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		p := round1(clamp(*w.UsedPercent, 0, 100))
		label := fallbackLabel
		if w.LimitWindowSecs != nil {
			switch {
			case *w.LimitWindowSecs <= 21600: // <= 6 h
				label = "5 h"
			case *w.LimitWindowSecs <= 691200: // <= 8 days
				label = "Week"
			default:
				label = "Month"
			}
		}
		reset := ""
		if w.ResetAt != nil {
			reset = time.Unix(int64(*w.ResetAt), 0).UTC().Format(time.RFC3339)
		} else if w.ResetAfterSeconds != nil {
			reset = time.Now().UTC().Add(time.Duration(*w.ResetAfterSeconds) * time.Second).Format(time.RFC3339)
		}
		r.Metrics = append(r.Metrics, Metric{Label: label, Value: usedPercentLabel(p), Percent: &p, ResetAt: reset})
	}
	addWindow(payload.RateLimit.PrimaryWindow, "5 h")
	addWindow(payload.RateLimit.SecondaryWindow, "Week")
	if payload.PlanType != "" {
		r.Metrics = append(r.Metrics, Metric{Label: "Plan", Value: strings.ToUpper(payload.PlanType)})
	}
	if len(r.Metrics) == 0 {
		r.Message = "The endpoint responded but returned no supported limit windows."
		return r
	}
	r.Status = "ok"
	r.Summary = "Subscription limits"
	return r
}

// chatGPTGet performs the request without failing on non-2xx, so the caller can
// distinguish an expired token (401) from other errors.
func chatGPTGet(client *http.Client, endpoint, token, accountID string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", chatGPTUserAgent)
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
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

func readChatGPTAuth(path string) (chatGPTAuth, error) {
	var auth chatGPTAuth
	b, err := os.ReadFile(path)
	if err != nil {
		return auth, err
	}
	if err := json.Unmarshal(b, &auth); err != nil {
		return auth, err
	}
	return auth, nil
}

// refreshChatGPT exchanges the refresh token for a new access token and persists
// the rotated tokens back into the auth file (Codex rotates refresh tokens).
func refreshChatGPT(client *http.Client, path string, auth *chatGPTAuth) error {
	reqBody, err := json.Marshal(map[string]string{
		"client_id":     chatGPTClientID,
		"grant_type":    "refresh_token",
		"refresh_token": auth.Tokens.RefreshToken,
	})
	if err != nil {
		return err
	}
	tokenURL := envOr("CHATGPT_TOKEN_URL", defaultChatGPTTokenURL)
	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", chatGPTUserAgent)
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
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, compactError(body))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if out.AccessToken == "" {
		return fmt.Errorf("refresh response has no access_token")
	}
	auth.Tokens.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		auth.Tokens.RefreshToken = out.RefreshToken
	}
	if out.IDToken != "" {
		auth.Tokens.IDToken = out.IDToken
	}
	return persistChatGPTAuth(path, auth)
}

// persistChatGPTAuth updates the tokens in the auth file while preserving any
// other fields the file may contain.
func persistChatGPTAuth(path string, auth *chatGPTAuth) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	tokens, _ := root["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = auth.Tokens.AccessToken
	if auth.Tokens.RefreshToken != "" {
		tokens["refresh_token"] = auth.Tokens.RefreshToken
	}
	if auth.Tokens.IDToken != "" {
		tokens["id_token"] = auth.Tokens.IDToken
	}
	if auth.Tokens.AccountID != "" {
		tokens["account_id"] = auth.Tokens.AccountID
	}
	root["tokens"] = tokens
	root["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	// Write in place: the /app directory is root-owned, so temp+rename will not work,
	// while auth.json itself belongs to the app user.
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
