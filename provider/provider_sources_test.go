package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// The default fan-out is the fast API sources only; the scrapers must stay
// out of a title search until an operator switches them on.
func TestDefaultSourceConfig(t *testing.T) {
	got := DefaultSourceConfig()
	want := SourceConfig{Audnexus: true, ITunes: true, AudiobookCovers: true}
	if got != want {
		t.Fatalf("DefaultSourceConfig() = %+v, want %+v", got, want)
	}
	names := make([]string, 0)
	for _, task := range NewProvider().searchTasks() {
		names = append(names, task.name)
	}
	if len(names) != 3 || names[0] != "audnexus" || names[1] != "itunes" || names[2] != "audiobookcovers" {
		t.Fatalf("default search tasks = %v", names)
	}
}

// Disabling a source removes it from Search but keeps it reachable through
// Fetch, so an item that already carries that source's ID still resolves.
func TestSearchSkipsDisabledSourcesButFetchStillResolvesThem(t *testing.T) {
	var itunesSearches, itunesLookups atomic.Int32
	itunes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			itunesSearches.Add(1)
			w.Write([]byte(`{"resultCount":1,"results":[{"wrapperType":"audiobook","collectionId":42,"collectionName":"Book","artistName":"A"}]}`))
		case "/lookup":
			itunesLookups.Add(1)
			w.Write([]byte(`{"resultCount":1,"results":[{"wrapperType":"audiobook","collectionId":42,"collectionName":"Book","artistName":"A"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer itunes.Close()
	audnexus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer audnexus.Close()

	p := NewProvider()
	p.ITunes.baseURL = itunes.URL
	p.Audnexus.baseURL = audnexus.URL
	p.SetSources(SourceConfig{Audnexus: true})

	results, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Book"})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 0 || itunesSearches.Load() != 0 {
		t.Fatalf("disabled itunes was searched: results=%d searches=%d", len(results), itunesSearches.Load())
	}

	m, err := p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: map[string]string{"itunes": "42"}})
	if err != nil || m == nil {
		t.Fatalf("Fetch via disabled source = (%v, %v), want a match", m, err)
	}
	if itunesLookups.Load() != 1 {
		t.Fatalf("itunes lookups = %d, want 1", itunesLookups.Load())
	}
}

// With every source off, a title search declines cleanly rather than
// reporting a provider failure.
func TestSearchWithNoSourcesDeclines(t *testing.T) {
	p := NewProvider()
	p.SetSources(SourceConfig{})
	results, err := p.Search(context.Background(), metadata.SearchQuery{Title: "Book"})
	if err != nil || len(results) != 0 {
		t.Fatalf("Search = (%d, %v), want (0, nil)", len(results), err)
	}
}
