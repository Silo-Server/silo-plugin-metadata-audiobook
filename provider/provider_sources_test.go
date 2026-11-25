package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/time/rate"

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

// The Audible store setting reaches every source that queries Audible's
// catalog, for ASIN lookups and title searches alike.
func TestSetAudibleRegionReachesAudibleSources(t *testing.T) {
	var mu sync.Mutex
	var apiRegions, audibleHosts []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiRegions = append(apiRegions, r.URL.Query().Get("region"))
		mu.Unlock()
		switch r.URL.Path {
		case "/books/search":
			w.Write([]byte(`[]`))
		case "/search":
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	p := NewProvider()
	p.Audnexus.baseURL = api.URL
	p.AudiMeta.baseURL = api.URL
	p.Audible.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		audibleHosts = append(audibleHosts, r.URL.Host)
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: r}, nil
	})}
	for _, limiter := range []**rate.Limiter{&p.Audnexus.limiter, &p.AudiMeta.limiter, &p.Audible.limiter} {
		*limiter = rate.NewLimiter(rate.Inf, 1)
	}
	p.SetAudibleRegion("uk")

	queries := []metadata.SearchQuery{
		{ProviderIDs: map[string]string{"asin": "B076HYPQLK"}},
		{Title: "Killing Floor"},
	}
	for _, source := range []searchProvider{p.Audnexus, p.AudiMeta, p.Audible} {
		for _, q := range queries {
			if _, err := source.Search(context.Background(), q); err != nil {
				t.Fatalf("%T Search(%+v): %v", source, q, err)
			}
		}
	}

	if len(apiRegions) != 4 {
		t.Fatalf("Audnexus and AudiMeta requests = %d, want 4", len(apiRegions))
	}
	for _, region := range apiRegions {
		if region != "uk" {
			t.Errorf("API request region = %q, want uk", region)
		}
	}
	if len(audibleHosts) != 2 {
		t.Fatalf("Audible requests = %d, want 2", len(audibleHosts))
	}
	for _, host := range audibleHosts {
		if host != "www.audible.co.uk" {
			t.Errorf("Audible request host = %q, want www.audible.co.uk", host)
		}
	}
}
