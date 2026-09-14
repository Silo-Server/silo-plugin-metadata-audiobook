package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// A primary match without a cover must pick one up from AudiobookCovers when
// the item carries an ASIN; a match that already has a cover must be left
// alone and must not cost an extra request.
func TestBackfillCover(t *testing.T) {
	var coverHits int
	covers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coverHits++
		if r.URL.Path != "/cover/by_book/B0182NWM9I" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"cover_url":"https://covers.example/B0182NWM9I.jpg"}`))
	}))
	defer covers.Close()

	p := NewProvider()
	p.AudiobookCovers.baseURL = covers.URL

	tests := []struct {
		name      string
		match     metadata.Match
		ids       map[string]string
		wantCover string
		wantHits  int
	}{
		{
			name:      "missing cover is grafted from audiobookcovers by asin",
			match:     metadata.Match{Provider: "audimeta", Title: "Book"},
			ids:       map[string]string{"asin": "B0182NWM9I"},
			wantCover: "https://covers.example/B0182NWM9I.jpg",
			wantHits:  1,
		},
		{
			name:      "existing cover is kept without a request",
			match:     metadata.Match{Provider: "audimeta", Title: "Book", CoverURL: "https://primary.example/c.jpg"},
			ids:       map[string]string{"asin": "B0182NWM9I"},
			wantCover: "https://primary.example/c.jpg",
			wantHits:  0,
		},
		{
			name:      "no asin means no backfill",
			match:     metadata.Match{Provider: "itunes", Title: "Book"},
			ids:       map[string]string{"itunes": "1459653604"},
			wantCover: "",
			wantHits:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			coverHits = 0
			m := tc.match
			p.backfillCover(context.Background(), &m, metadata.SearchQuery{ProviderIDs: tc.ids})
			if m.CoverURL != tc.wantCover {
				t.Errorf("CoverURL = %q, want %q", m.CoverURL, tc.wantCover)
			}
			if coverHits != tc.wantHits {
				t.Errorf("audiobookcovers requests = %d, want %d", coverHits, tc.wantHits)
			}
		})
	}
}
