package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// asinSources stubs the sources an ASIN can be resolved through and counts
// the requests each receives.
type asinSources struct {
	p                           *Provider
	audnexus, audimeta, audible atomic.Int32
}

func newASINSources(t *testing.T, audnexusKnows bool) *asinSources {
	t.Helper()
	s := &asinSources{p: NewProvider()}

	book, err := os.ReadFile("testdata/audnexus_book.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	audnexus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.audnexus.Add(1)
		if !audnexusKnows {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(book)
	}))
	t.Cleanup(audnexus.Close)
	s.p.Audnexus.baseURL = audnexus.URL

	// AudiMeta answers every request with 403, as the retired service does.
	audimeta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.audimeta.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(audimeta.Close)
	s.p.AudiMeta.baseURL = audimeta.URL

	// No source may reach the network: a record without a cover would
	// otherwise ask the real cover service.
	covers := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(covers.Close)
	s.p.AudiobookCovers.baseURL = covers.URL

	page, err := os.ReadFile("testdata/audible_product.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	s.p.Audible.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		s.audible.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(bytes.NewReader(page)),
		}, nil
	})}
	return s
}

// A host that declares nothing but the ASIN from a book's tags gets the
// Audnexus record, and an ASIN Audnexus does not know is "no record": the
// retired AudiMeta service is asked only when it is switched on.
func TestFetchByASINAlone(t *testing.T) {
	ids := map[string]string{"asin": "B002V0QHBU"}

	t.Run("known to audnexus", func(t *testing.T) {
		s := newASINSources(t, true)
		m, err := s.p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: ids})
		if err != nil || m == nil || m.Provider != "audnexus" {
			t.Fatalf("Fetch = (%+v, %v), want the audnexus record", m, err)
		}
		if s.audimeta.Load() != 0 || s.audible.Load() != 0 {
			t.Fatalf("other sources asked: audimeta=%d audible=%d", s.audimeta.Load(), s.audible.Load())
		}
	})

	t.Run("unknown asin is no record", func(t *testing.T) {
		s := newASINSources(t, false)
		m, err := s.p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: ids})
		if err != nil || m != nil {
			t.Fatalf("Fetch = (%+v, %v), want (nil, nil)", m, err)
		}
		if s.audimeta.Load() != 0 {
			t.Fatalf("audimeta asked %d times while switched off", s.audimeta.Load())
		}
	})

	t.Run("audimeta is asked when switched on", func(t *testing.T) {
		s := newASINSources(t, false)
		cfg := DefaultSourceConfig()
		cfg.AudiMeta = true
		s.p.SetSources(cfg)
		if _, err := s.p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: ids}); err == nil {
			t.Fatal("Fetch error = nil, want the audimeta failure")
		}
		if s.audimeta.Load() == 0 {
			t.Fatal("audimeta was not asked")
		}
	})
}

// An Audible ID is an ASIN: Audnexus answers it, and the rate-limited scraper
// is asked only for an ASIN Audnexus has no record of.
func TestFetchAudibleIDPrefersAudnexus(t *testing.T) {
	ids := map[string]string{"audible": "B002V0QHBU", "asin": "B002V0QHBU"}

	t.Run("audnexus has the record", func(t *testing.T) {
		s := newASINSources(t, true)
		m, err := s.p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: ids})
		if err != nil || m == nil || m.Provider != "audnexus" {
			t.Fatalf("Fetch = (%+v, %v), want the audnexus record", m, err)
		}
		if s.audible.Load() != 0 {
			t.Fatalf("audible scraped %d pages", s.audible.Load())
		}
	})

	t.Run("audnexus has no record", func(t *testing.T) {
		s := newASINSources(t, false)
		m, err := s.p.Fetch(context.Background(), metadata.SearchQuery{ProviderIDs: ids})
		if err != nil || m == nil || m.Provider != "audible" {
			t.Fatalf("Fetch = (%+v, %v), want the audible record", m, err)
		}
		if s.audible.Load() != 1 {
			t.Fatalf("audible scraped %d pages, want 1", s.audible.Load())
		}
	})
}

type delayedSearch struct {
	delay   time.Duration
	matches []metadata.Match
}

func (d delayedSearch) Search(ctx context.Context, _ metadata.SearchQuery) ([]metadata.Match, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.matches, nil
}

// Results come back in fan-out order even when a later source answers first.
func TestSearchResultsKeepFanOutOrder(t *testing.T) {
	tasks := []searchTask{
		// The 20 ms head start is the point of the test: the first source
		// must answer after the second.
		{name: "first", fn: delayedSearch{delay: 20 * time.Millisecond, matches: []metadata.Match{{Provider: "first"}}}},
		{name: "second", fn: delayedSearch{matches: []metadata.Match{{Provider: "second"}}}},
	}
	got, err := searchAll(context.Background(), tasks, metadata.SearchQuery{Title: "Book"})
	if err != nil {
		t.Fatalf("searchAll error: %v", err)
	}
	if len(got) != 2 || got[0].Provider != "first" || got[1].Provider != "second" {
		t.Fatalf("order = %+v, want first then second", got)
	}
}
