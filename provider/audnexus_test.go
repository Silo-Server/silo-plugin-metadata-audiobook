package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

func TestAudnexusFetch(t *testing.T) {
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	m, err := client.Fetch(context.Background(), "B002V0QHBU")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil match")
	}

	if m.Provider != "audnexus" {
		t.Errorf("Provider = %q, want %q", m.Provider, "audnexus")
	}
	if m.ASIN != "B002V0QHBU" {
		t.Errorf("ASIN = %q, want %q", m.ASIN, "B002V0QHBU")
	}
	if m.Title != "The Hitchhiker's Guide to the Galaxy" {
		t.Errorf("Title = %q", m.Title)
	}
	if len(m.Authors) == 0 || m.Authors[0] != "Douglas Adams" {
		t.Errorf("Authors = %v", m.Authors)
	}
	if len(m.Narrators) == 0 || m.Narrators[0] != "Stephen Fry" {
		t.Errorf("Narrators = %v", m.Narrators)
	}
	if m.DurationMin != 193 {
		t.Errorf("DurationMin = %d, want 193", m.DurationMin)
	}
	if m.PublishYear != 2005 {
		t.Errorf("PublishYear = %d, want 2005", m.PublishYear)
	}
	if m.SeriesName != "Hitchhiker's Guide to the Galaxy" {
		t.Errorf("SeriesName = %q", m.SeriesName)
	}
	if m.SeriesPosition != "1" {
		t.Errorf("SeriesPosition = %q, want %q", m.SeriesPosition, "1")
	}
	// Only "genre"-type entries should appear.
	if len(m.Genres) != 1 || m.Genres[0] != "Science Fiction" {
		t.Errorf("Genres = %v, want [Science Fiction]", m.Genres)
	}
	// HTML tags should be stripped.
	if m.Description == "" {
		t.Error("Description should not be empty after HTML strip")
	}
}

func TestAudnexusSearchByASIN(t *testing.T) {
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	q := metadata.SearchQuery{
		ProviderIDs: map[string]string{"asin": "B002V0QHBU"},
	}
	results, err := client.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ASIN != "B002V0QHBU" {
		t.Errorf("ASIN = %q", results[0].ASIN)
	}
}

func TestAudnexusTitleSearch(t *testing.T) {
	// The title search endpoint returns an array of books.
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	arrayFixture := "[" + string(fixture) + "]"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(arrayFixture))
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	q := metadata.SearchQuery{Title: "Hitchhiker"}
	results, err := client.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Title != "The Hitchhiker's Guide to the Galaxy" {
		t.Errorf("Title = %q", results[0].Title)
	}
}

// The title search hit /books?q= for as long as this client existed, which is
// not a route on api.audnex.us -- it answers with a 404 "Route not found". Every
// title search failed silently for exactly the audiobooks that had no ASIN yet.
//
// It survived because the other tests' stub server answers any path, so the
// wrong URL still returned the fixture. This one asserts the path itself.
func TestAudnexusSearchUsesTheBooksSearchRoute(t *testing.T) {
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[" + string(fixture) + "]"))
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	if _, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Hitchhiker"}); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if gotPath != "/books/search" {
		t.Errorf("requested path = %q, want %q -- /books is a 404 on the real API", gotPath, "/books/search")
	}
	if gotQuery != "Hitchhiker" {
		t.Errorf("q = %q, want %q", gotQuery, "Hitchhiker")
	}
}

// An ASIN must still resolve through /books/{asin}, not the search route.
func TestAudnexusSearchByASINUsesTheFetchRoute(t *testing.T) {
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	q := metadata.SearchQuery{ProviderIDs: map[string]string{"asin": "B0182NWM9I"}}
	if _, err := client.Search(context.Background(), q); err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if gotPath != "/books/B0182NWM9I" {
		t.Errorf("requested path = %q, want /books/B0182NWM9I", gotPath)
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"<p>Hello <b>World</b></p>", "Hello World"},
		{"No tags here", "No tags here"},
		{"&amp; &lt;tag&gt;", "& <tag>"},
	}
	for _, tt := range tests {
		got := stripHTML(tt.in)
		if got != tt.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
