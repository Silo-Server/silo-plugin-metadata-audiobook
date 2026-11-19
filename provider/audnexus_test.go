package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// The Audnexus fixtures are captures of api.audnex.us answers, taken
// 2026-11-18 with region=us. Each is the full answer minus "rating" and
// "ratings", with "summary" cut to its opening. Keep them real: a fixture
// written by hand to fit this client once hid that the client read keys the
// API does not send.
//
//	audnexus_book.json         GET /books/B08G9PRS1K, a book in no series
//	audnexus_book_series.json  GET /books/B002V5D5ZE, a book with seriesPrimary
//	audnexus_search.json       GET /books/search?q=Mistborn, first two results

func newAudnexusStub(t *testing.T, fixture string) *AudnexusClient {
	t.Helper()
	body, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	client := NewAudnexusClient()
	client.baseURL = srv.URL
	return client
}

func TestAudnexusFetch(t *testing.T) {
	client := newAudnexusStub(t, "audnexus_book.json")

	m, err := client.Fetch(context.Background(), "B08G9PRS1K")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil match")
	}

	if m.Provider != "audnexus" {
		t.Errorf("Provider = %q, want %q", m.Provider, "audnexus")
	}
	if m.ASIN != "B08G9PRS1K" {
		t.Errorf("ASIN = %q, want %q", m.ASIN, "B08G9PRS1K")
	}
	if m.Title != "Project Hail Mary" {
		t.Errorf("Title = %q", m.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0] != "Andy Weir" {
		t.Errorf("Authors = %v", m.Authors)
	}
	if len(m.Narrators) != 1 || m.Narrators[0] != "Ray Porter" {
		t.Errorf("Narrators = %v", m.Narrators)
	}
	if m.Publisher != "Audible Studios" {
		t.Errorf("Publisher = %q, want %q", m.Publisher, "Audible Studios")
	}
	if m.DurationMin != 970 {
		t.Errorf("DurationMin = %d, want 970", m.DurationMin)
	}
	if m.PublishYear != 2021 {
		t.Errorf("PublishYear = %d, want 2021", m.PublishYear)
	}
	if m.Language != "english" {
		t.Errorf("Language = %q, want %q", m.Language, "english")
	}
	// The answer has neither series key.
	if m.SeriesName != "" || m.SeriesPosition != "" {
		t.Errorf("series = %q #%q, want none", m.SeriesName, m.SeriesPosition)
	}
	// Only "genre"-type entries should appear, not the six "tag" ones.
	wantGenres := []string{"Mystery, Thriller & Suspense", "Science Fiction & Fantasy"}
	if !slices.Equal(m.Genres, wantGenres) {
		t.Errorf("Genres = %q, want %q", m.Genres, wantGenres)
	}
	if !strings.HasPrefix(m.Description, "When the Sun is threatened by an alien microbe") {
		t.Errorf("Description = %q", m.Description)
	}
}

// Audnexus names a book's series in seriesPrimary, an object, and its
// publisher in publisherName.
func TestAudnexusFetchSeries(t *testing.T) {
	client := newAudnexusStub(t, "audnexus_book_series.json")

	m, err := client.Fetch(context.Background(), "B002V5D5ZE")
	if err != nil || m == nil {
		t.Fatalf("Fetch = (%+v, %v), want a match", m, err)
	}
	if m.Title != "The Restaurant at the End of the Universe" {
		t.Errorf("Title = %q", m.Title)
	}
	if m.Subtitle != "The Hitchhiker's Guide to the Galaxy, Book 2" {
		t.Errorf("Subtitle = %q", m.Subtitle)
	}
	if m.Publisher != "Random House Audio" {
		t.Errorf("Publisher = %q, want %q", m.Publisher, "Random House Audio")
	}
	if m.SeriesName != "Hitchhiker's Guide to the Galaxy" {
		t.Errorf("SeriesName = %q, want %q", m.SeriesName, "Hitchhiker's Guide to the Galaxy")
	}
	if m.SeriesPosition != "2" {
		t.Errorf("SeriesPosition = %q, want %q", m.SeriesPosition, "2")
	}
}

func TestAudnexusSearchByASIN(t *testing.T) {
	client := newAudnexusStub(t, "audnexus_book.json")

	q := metadata.SearchQuery{
		ProviderIDs: map[string]string{"asin": "B08G9PRS1K"},
	}
	results, err := client.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ASIN != "B08G9PRS1K" {
		t.Errorf("ASIN = %q", results[0].ASIN)
	}
}

// The title search answers with an array of books in the fetch shape.
func TestAudnexusTitleSearch(t *testing.T) {
	client := newAudnexusStub(t, "audnexus_search.json")

	results, err := client.Search(context.Background(), metadata.SearchQuery{Title: "Mistborn"})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// seriesPrimary is "The Mistborn Saga" at position 1 and seriesSecondary
	// is "The Cosmere": the primary one is the book's series.
	first := results[0]
	if first.ASIN != "B002V0QCYU" || first.Title != "The Final Empire" {
		t.Errorf("first result = %q %q", first.ASIN, first.Title)
	}
	if first.Publisher != "Macmillan Audio" {
		t.Errorf("Publisher = %q, want %q", first.Publisher, "Macmillan Audio")
	}
	if first.SeriesName != "The Mistborn Saga" || first.SeriesPosition != "1" {
		t.Errorf("series = %q #%q, want The Mistborn Saga #1", first.SeriesName, first.SeriesPosition)
	}

	// A series the book has no number in comes without a position.
	second := results[1]
	if second.Publisher != "Podium Audio" {
		t.Errorf("Publisher = %q, want %q", second.Publisher, "Podium Audio")
	}
	if second.SeriesName != "The Fae & Alchemy Series" || second.SeriesPosition != "" {
		t.Errorf("series = %q #%q, want The Fae & Alchemy Series with no position", second.SeriesName, second.SeriesPosition)
	}
}

// No captured answer has a secondary series without a primary one. This one
// is constructed: the seriesSecondary object is the one Audnexus sends for
// B0189QQXRA, with the primary series it sends beside it left out.
func TestAudnexusSecondarySeriesWithoutPrimary(t *testing.T) {
	var book audnexusBook
	answer := `{"seriesSecondary":{"asin":"B09KHFX6N1","name":"Wax and Wayne","position":"3"}}`
	if err := json.Unmarshal([]byte(answer), &book); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := bookFromAudnexus(book)
	if m.SeriesName != "Wax and Wayne" || m.SeriesPosition != "3" {
		t.Errorf("series = %q #%q, want Wax and Wayne #3", m.SeriesName, m.SeriesPosition)
	}
}

// Audnexus sends a plain-text description and an HTML summary. A book with
// only the summary gets it as plain text.
func TestBookFromAudnexusStripsSummaryHTML(t *testing.T) {
	m := bookFromAudnexus(audnexusBook{Summary: "<p><b>A lone astronaut</b> must save the earth.</p>"})
	if want := "A lone astronaut must save the earth."; m.Description != want {
		t.Errorf("Description = %q, want %q", m.Description, want)
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
		// Apple Books summaries use numeric entities and paragraph markup.
		{"Halftime Walk&#xa0;A boy &#8220;and&#8221; a girl", "Halftime Walk A boy “and” a girl"},
		{"<p>rare originality.</p><p>Read by Yareli</p>", "rare originality. Read by Yareli"},
		{"<p>First.</p>\n<p>Second.</p>", "First.\nSecond."},
		{"<p>First.</p>\r\n<p>Second.</p>", "First.\r\nSecond."},
	}
	for _, tt := range tests {
		got := stripHTML(tt.in)
		if got != tt.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A 404 from the ASIN route means "no such book", which get() reports as a nil
// body. Decoding that nil body produced a bogus JSON error instead.
func TestAudnexusFetchUnknownASINReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL

	match, err := client.Fetch(context.Background(), "B0000000000")
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if match != nil {
		t.Fatalf("match = %+v, want nil", match)
	}
}

// Audnexus answers 400 for an ASIN it knows as a podcast, and a tag can hold
// a value that is no ASIN at all. Both mean "no book", not a failed source;
// only a server error is a failure.
func TestAudnexusFetchNoBook(t *testing.T) {
	tests := []struct {
		name         string
		asin         string
		status       int
		body         string
		wantErr      bool
		wantRequests int
	}{
		{"podcast asin", "B002V0QHBU", http.StatusBadRequest, `{"error":{"code":"CONTENT_TYPE_MISMATCH"}}`, false, 1},
		{"not an asin", "itunes:1234567890", http.StatusBadRequest, `{"error":{"code":"BAD_REQUEST"}}`, false, 0},
		{"server error", "B002V0QHBU", http.StatusInternalServerError, `oops`, true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				http.Error(w, tc.body, tc.status)
			}))
			defer srv.Close()

			client := NewAudnexusClient()
			client.baseURL = srv.URL
			m, err := client.Fetch(context.Background(), tc.asin)
			if m != nil || (err != nil) != tc.wantErr {
				t.Fatalf("Fetch = (%+v, %v), want no match and error=%v", m, err, tc.wantErr)
			}
			if requests != tc.wantRequests {
				t.Fatalf("requests = %d, want %d", requests, tc.wantRequests)
			}
		})
	}
}

// A search whose ASIN names no book still searches by title, so a wrong ASIN
// tag does not hide a book Audnexus lists.
func TestAudnexusSearchFallsBackToTitleWhenASINHasNoBook(t *testing.T) {
	fixture, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/books/search" {
			http.Error(w, `{"error":{"code":"CONTENT_TYPE_MISMATCH"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[" + string(fixture) + "]"))
	}))
	defer srv.Close()

	client := NewAudnexusClient()
	client.baseURL = srv.URL
	got, err := client.Search(context.Background(), metadata.SearchQuery{
		Title:       "Book",
		ProviderIDs: map[string]string{"asin": "B0PODCAST1"},
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("Search = (%d results, %v), want the title search's one result", len(got), err)
	}
}
