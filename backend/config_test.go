package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadTilesOrderDefaultsAndValidation(t *testing.T) {
	t.Setenv("TILES", " claude , opencode-a,bad id,opencode_a,typo,")
	t.Setenv("TILE_CLAUDE_TYPE", "claude")
	t.Setenv("TILE_OPENCODE_A_TYPE", "OpenCode-Go")
	t.Setenv("TILE_OPENCODE_A_NAME", "OpenCode Go A")
	t.Setenv("TILE_TYPO_TYPE", "cladue")

	tiles := loadTiles()
	if len(tiles) != 3 {
		t.Fatalf("expected 3 tiles (invalid id and prefix duplicate skipped), got %+v", tiles)
	}
	want := []struct{ id, typ, name string }{
		{"claude", "claude", "Claude (subscription)"},
		{"opencode-a", "opencode-go", "OpenCode Go A"},
		{"typo", "cladue", "typo"},
	}
	for i, w := range want {
		if tiles[i].ID != w.id || tiles[i].Type != w.typ || tiles[i].Name != w.name {
			t.Errorf("tile %d = %+v, want %+v", i, tiles[i], w)
		}
	}
	if got := tiles[1].EnvKey("API_KEY"); got != "TILE_OPENCODE_A_API_KEY" {
		t.Errorf("EnvKey = %q", got)
	}
}

func TestFetchTileUnknownType(t *testing.T) {
	r := fetchTile(http.DefaultClient, TileConfig{ID: "typo", Name: "typo", Type: "cladue", envPrefix: "TILE_TYPO_"})
	if r.Status != "error" || !strings.Contains(r.Message, "TILE_TYPO_TYPE") {
		t.Fatalf("unknown type result: %+v", r)
	}
}

func TestFetchTileMissingKeyNamesTileEnv(t *testing.T) {
	t.Setenv("TILES", "second")
	t.Setenv("TILE_SECOND_TYPE", "opencode-go")
	r := fetchTile(http.DefaultClient, loadTiles()[0])
	if r.Status != "no_data" || r.Configured || !strings.Contains(r.Message, "TILE_SECOND_API_KEY") {
		t.Fatalf("missing key result: %+v", r)
	}
}

// Two tiles of the same type use their own key and URL.
func TestTilesOfSameTypeAreIndependent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pct := "10"
		if r.Header.Get("Authorization") == "Bearer key-b" {
			pct = "80"
		}
		w.Write([]byte(`{"usage":{"weekly":{"percent":` + pct + `}}}`))
	}))
	defer srv.Close()

	t.Setenv("TILES", "a,b")
	for _, id := range []string{"A", "B"} {
		t.Setenv("TILE_"+id+"_TYPE", "opencode-go")
		t.Setenv("TILE_"+id+"_API_KEY", "key-"+strings.ToLower(id))
		t.Setenv("TILE_"+id+"_URL", srv.URL)
	}
	tiles := loadTiles()
	a, b := fetchTile(srv.Client(), tiles[0]), fetchTile(srv.Client(), tiles[1])
	if a.ID != "a" || a.Metrics[0].Value != "10% used" {
		t.Fatalf("tile a: %+v", a)
	}
	if b.ID != "b" || b.Metrics[0].Value != "80% used" {
		t.Fatalf("tile b: %+v", b)
	}
}
