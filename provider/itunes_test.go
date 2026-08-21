package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

func TestITunesSearch(t *testing.T) {
	fixture, err := os.ReadFile("testdata/itunes_search.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("media") != "audiobook" {
			t.Errorf("missing media=audiobook param")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	client := NewITunesClient()
	client.baseURL = srv.URL

	q := metadata.SearchQuery{Title: "Hitchhiker"}
	results, err := client.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	m := results[0]
	if m.Provider != "itunes" {
		t.Errorf("Provider = %q, want itunes", m.Provider)
	}
	if m.Title != "The Hitchhiker's Guide to the Galaxy" {
		t.Errorf("Title = %q", m.Title)
	}
	if len(m.Authors) == 0 || m.Authors[0] != "Douglas Adams" {
		t.Errorf("Authors = %v", m.Authors)
	}
	if m.PublishYear != 2005 {
		t.Errorf("PublishYear = %d", m.PublishYear)
	}
	// artworkUrl600 should be used directly.
	if m.CoverURL != "https://is1-ssl.mzstatic.com/image/thumb/Music/abc/600x600bb.jpg" {
		t.Errorf("CoverURL = %q", m.CoverURL)
	}
	// 11580000 ms / 60000 = 193 min
	if m.DurationMin != 193 {
		t.Errorf("DurationMin = %d, want 193", m.DurationMin)
	}
	if len(m.Genres) == 0 || m.Genres[0] != "Science Fiction" {
		t.Errorf("Genres = %v", m.Genres)
	}
}

func TestITunesSearchEmpty(t *testing.T) {
	client := NewITunesClient()
	results, err := client.Search(context.Background(), metadata.SearchQuery{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestITunesCoverArtworkFallback(t *testing.T) {
	client := NewITunesClient()

	// When artworkUrl600 is absent, the 100px URL should be substituted.
	r := iTunesResult{
		ArtworkURL100: "https://example.com/image/100x100bb.jpg",
	}
	got := client.coverArtwork(r)
	want := "https://example.com/image/600x600bb.jpg"
	if got != want {
		t.Errorf("coverArtwork = %q, want %q", got, want)
	}
}

// TestITunesFetchUsesLookupEndpoint guards a bug that cost this library every
// single match: Fetch was a stub returning (nil, nil), so Silo admitted an
// iTunes search candidate, asked for its metadata, got nothing back WITHOUT an
// error, and wrote the item off as a terminal no_match. Assert both that the
// lookup endpoint is called and that a match comes back, so a future
// "unsupported" stub cannot pass silently.
func TestITunesFetchUsesLookupEndpoint(t *testing.T) {
	fixture, err := os.ReadFile("testdata/itunes_search.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/lookup" {
			t.Errorf("path = %s, want /lookup", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "12345" {
			t.Errorf("id = %q, want 12345", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	client := NewITunesClient()
	client.baseURL = srv.URL

	m, err := client.Fetch(context.Background(), "12345")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !called {
		t.Fatal("Fetch made no HTTP request — the lookup endpoint was not used")
	}
	if m == nil {
		t.Fatal("Fetch returned nil; a resolvable collectionId must yield metadata")
	}
	if m.Provider != "itunes" {
		t.Errorf("Provider = %q, want itunes", m.Provider)
	}
	if m.Title == "" {
		t.Error("Fetch returned a match with no title")
	}
}

// An empty id is a caller mistake, not a provider outage: declining must stay
// error-free so it cannot be mistaken for a failure.
func TestITunesFetchEmptyIDDeclines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Fetch must not call the API for an empty id")
	}))
	defer srv.Close()

	client := NewITunesClient()
	client.baseURL = srv.URL

	m, err := client.Fetch(context.Background(), "  ")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if m != nil {
		t.Fatalf("expected nil match, got %+v", m)
	}
}
