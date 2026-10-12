package main

import (
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tiles are configured entirely from ENV:
//
//	TILES=claude,opencode-a                # order = order on the dashboard
//	TILE_CLAUDE_TYPE=claude                # provider type (see tileTypes)
//	TILE_CLAUDE_NAME=Claude / Anthropic    # optional, defaults per type
//	TILE_CLAUDE_AUTH_FILE=/secrets/claude.json
//
// Every per-tile setting lives under TILE_<ID>_, where <ID> is the tile id
// upper-cased with "-" replaced by "_".

// TileConfig describes one dashboard tile. Settings are read lazily from ENV
// through Get/Float so fetchers only touch the keys they need.
type TileConfig struct {
	ID        string
	Type      string
	Name      string
	envPrefix string
}

// tileFetcher fetches data for one tile. It must never return secrets.
type tileFetcher func(client *http.Client, t TileConfig) ProviderResult

type tileType struct {
	defaultName string
	fetch       tileFetcher
}

var tileTypes = map[string]tileType{
	"openai-costs":   {"OpenAI", fetchOpenAI},
	"chatgpt":        {"ChatGPT (subscription)", fetchChatGPT},
	"claude":         {"Claude (subscription)", fetchAnthropic},
	"anthropic-api":  {"Anthropic API", fetchAnthropicAPI},
	"ollama-usage":   {"Ollama Cloud", fetchOllama},
	"ollama-balance": {"Ollama Cloud — limit", fetchOllamaQuota},
	"opencode-go":    {"OpenCode Go", fetchOpenCodeQuota},
}

var tileIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// EnvKey returns the full ENV variable name of a tile setting, e.g. TILE_CLAUDE_AUTH_FILE.
func (t TileConfig) EnvKey(name string) string { return t.envPrefix + name }

func (t TileConfig) Get(name string) string {
	return strings.TrimSpace(os.Getenv(t.EnvKey(name)))
}

func (t TileConfig) GetOr(name, fallback string) string {
	if v := t.Get(name); v != "" {
		return v
	}
	return fallback
}

func (t TileConfig) Float(name string) (float64, bool) {
	s := t.Get(name)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func tileEnvPrefix(id string) string {
	return "TILE_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_"
}

// loadTiles parses TILES and the per-tile TYPE/NAME. Invalid or duplicate ids
// are skipped; a missing/unknown TYPE keeps the tile (it renders as an error)
// so a typo in .env stays visible on the dashboard.
func loadTiles() []TileConfig {
	var tiles []TileConfig
	seen := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("TILES"), ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !tileIDPattern.MatchString(id) {
			log.Printf("config: skipping tile %q: id may contain only letters, digits, '-' and '_'", id)
			continue
		}
		prefix := tileEnvPrefix(id)
		if seen[prefix] {
			log.Printf("config: skipping duplicate tile %q", id)
			continue
		}
		seen[prefix] = true
		t := TileConfig{ID: id, envPrefix: prefix}
		t.Type = strings.ToLower(t.Get("TYPE"))
		t.Name = t.Get("NAME")
		if t.Name == "" {
			if tt, ok := tileTypes[t.Type]; ok {
				t.Name = tt.defaultName
			} else {
				t.Name = id
			}
		}
		if _, ok := tileTypes[t.Type]; !ok {
			log.Printf("config: tile %q has unknown %s=%q", id, t.EnvKey("TYPE"), t.Type)
		}
		tiles = append(tiles, t)
	}
	if len(tiles) == 0 {
		log.Printf("config: no tiles configured — set TILES in .env (see .env.example)")
	}
	return tiles
}

// fetchTile dispatches to the fetcher for the tile's type.
func fetchTile(client *http.Client, t TileConfig) ProviderResult {
	tt, ok := tileTypes[t.Type]
	if !ok {
		r := baseResult(t, "Dashboard config", false)
		r.Status = "error"
		r.Summary = "Configuration error"
		r.Message = "Unknown tile type " + strconv.Quote(t.Type) + ". Set " + t.EnvKey("TYPE") + " to one of: " + strings.Join(tileTypeNames(), ", ") + "."
		return r
	}
	return tt.fetch(client, t)
}

func tileTypeNames() []string {
	names := make([]string, 0, len(tileTypes))
	for k := range tileTypes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
