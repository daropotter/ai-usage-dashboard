package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Metric struct {
	Label   string   `json:"label"`
	Value   string   `json:"value"`
	Percent *float64 `json:"percent,omitempty"`
	ResetAt string   `json:"resetAt,omitempty"`
}

type ProviderResult struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"` // ok | no_data | error
	Configured bool     `json:"configured"`
	Summary    string   `json:"summary"`
	Message    string   `json:"message,omitempty"`
	Source     string   `json:"source"`
	Metrics    []Metric `json:"metrics"`
	UpdatedAt  string   `json:"updatedAt"`
}

type DashboardResponse struct {
	Providers []ProviderResult `json:"providers"`
	UpdatedAt string           `json:"updatedAt"`
}

func main() {
	client := &http.Client{Timeout: 12 * time.Second}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		type indexed struct {
			idx    int
			result ProviderResult
		}
		ch := make(chan indexed, 4)
		providers := []func() ProviderResult{
			func() ProviderResult { return fetchOpenAI(client) },
			func() ProviderResult { return fetchAnthropic(client) },
			func() ProviderResult { return fetchOpenCodeGo(client) },
			func() ProviderResult { return fetchOllama(client) },
		}
		for i, fn := range providers {
			go func(i int, fn func() ProviderResult) { ch <- indexed{i, fn()} }(i, fn)
		}
		results := make([]ProviderResult, 4)
		for range providers {
			v := <-ch
			results[v.idx] = v.result
		}
		writeJSON(w, http.StatusOK, DashboardResponse{Providers: results, UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	})

	staticDir := envOr("STATIC_DIR", "../frontend/dist")
	if _, err := os.Stat(staticDir); err == nil {
		fs := http.FileServer(http.Dir(staticDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			p := filepath.Join(staticDir, filepath.Clean(r.URL.Path))
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				fs.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(staticDir, "index.html"))
		})
	}

	addr := ":" + envOr("PORT", "8080")
	log.Printf("AI usage dashboard listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, securityHeaders(mux)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
