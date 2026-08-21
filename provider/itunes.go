package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// ITunesClient queries the Apple iTunes Search API for audiobook metadata.
//
// Reference implementation:
//
//	/opt/audiobookshelf/server/providers/iTunes.js
//
// Search endpoint: https://itunes.apple.com/search?media=audiobook&entity=audiobook
// Rate limit: ~20 rpm (conservative).
type ITunesClient struct {
	httpClient *http.Client
	baseURL    string
	limiter    *rate.Limiter
}

// iTunesSearchResponse wraps the iTunes JSON envelope.
type iTunesSearchResponse struct {
	ResultCount int              `json:"resultCount"`
	Results     []iTunesResult   `json:"results"`
}

// iTunesResult is a single audiobook entry from the iTunes search response.
type iTunesResult struct {
	CollectionID      int     `json:"collectionId"`
	ArtistID          int     `json:"artistId"`
	CollectionName    string  `json:"collectionName"`
	TrackName         string  `json:"trackName"`
	ArtistName        string  `json:"artistName"`
	Description       string  `json:"description"`
	ReleaseDate       string  `json:"releaseDate"`
	PrimaryGenreName  string  `json:"primaryGenreName"`
	ArtworkURL30      string  `json:"artworkUrl30"`
	ArtworkURL60      string  `json:"artworkUrl60"`
	ArtworkURL100     string  `json:"artworkUrl100"`
	ArtworkURL600     string  `json:"artworkUrl600"`
	TrackTimeMillis   int64   `json:"trackTimeMillis"`
}

// NewITunesClient creates an iTunes client with a 20 rpm rate limiter.
func NewITunesClient() *ITunesClient {
	return &ITunesClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://itunes.apple.com",
		limiter:    newLimiter(20),
	}
}

// coverArtwork picks the best available artwork URL, preferring 600px.
// If artworkUrl100 is available, it substitutes the size segment to get 600x600bb.
func (c *ITunesClient) coverArtwork(r iTunesResult) string {
	if r.ArtworkURL600 != "" {
		return r.ArtworkURL600
	}
	// Attempt size substitution on the 100px URL (which is always present).
	if r.ArtworkURL100 != "" {
		// Replace "100x100bb" → "600x600bb" (Apple CDN pattern).
		replaced := strings.Replace(r.ArtworkURL100, "100x100bb", "600x600bb", 1)
		if replaced != r.ArtworkURL100 {
			return replaced
		}
		// Fallback: just use the 100px URL.
		return r.ArtworkURL100
	}
	if r.ArtworkURL60 != "" {
		return r.ArtworkURL60
	}
	return r.ArtworkURL30
}

func (c *ITunesClient) matchFromResult(r iTunesResult) metadata.Match {
	title := r.CollectionName
	if title == "" {
		title = r.TrackName
	}

	// artistName can be "Name1 & Name2" — normalise to comma separation.
	author := strings.ReplaceAll(r.ArtistName, " & ", ", ")

	var authors []string
	if author != "" {
		authors = []string{author}
	}

	var genres []string
	if r.PrimaryGenreName != "" {
		genres = []string{r.PrimaryGenreName}
	}

	durationMin := 0
	if r.TrackTimeMillis > 0 {
		durationMin = int(r.TrackTimeMillis / 60000)
	}

	return metadata.Match{
		Provider:    "itunes",
		ProviderID:  fmt.Sprintf("%d", r.CollectionID),
		Title:       title,
		Authors:     authors,
		Description: stripHTML(r.Description),
		PublishYear: extractYear(r.ReleaseDate),
		Genres:      genres,
		CoverURL:    c.coverArtwork(r),
		DurationMin: durationMin,
	}
}

// Search queries the iTunes Search API for audiobooks matching q.Title.
func (c *ITunesClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if q.Title == "" {
		return nil, nil
	}

	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("term", q.Title)
	params.Set("media", "audiobook")
	params.Set("entity", "audiobook")
	params.Set("limit", "10")
	if q.Language != "" {
		params.Set("lang", q.Language)
	}

	reqURL := c.baseURL + "/search?" + params.Encode()
	body, err := c.get(ctx, reqURL)
	if err != nil || body == nil {
		return nil, err
	}

	var resp iTunesSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("itunes: decode response: %w", err)
	}

	results := make([]metadata.Match, 0, len(resp.Results))
	for _, r := range resp.Results {
		results = append(results, c.matchFromResult(r))
	}
	return results, nil
}

// Fetch returns full metadata for an iTunes collectionId.
//
// This used to return (nil, nil) with a comment claiming the public API has no
// single-item lookup. It does: GET /lookup?id=<collectionId> is documented and
// answers with the same result shape as /search. Verified 2026-08-21 --
// /lookup?id=1641927799&entity=audiobook returns resultCount 1 with a 961-char
// description and artwork.
//
// The stub was not a harmless gap. iTunes is by far the strongest provider here
// (12 hits in 258ms where Audible needs ~20s for 2), so Silo admitted an iTunes
// candidate in the search phase, called GetMetadata for it, got nothing back --
// without an error, so nothing looked wrong -- and recorded the item as
// outcome=no_match. That is terminal: next_attempt_at stays NULL and
// media_items.last_refreshed is stamped, so the item is never retried. Across
// 3,284 enrichment attempts on this library it produced exactly zero successes.
func (c *ITunesClient) Fetch(ctx context.Context, id string) (*metadata.Match, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}

	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("id", id)
	params.Set("entity", "audiobook")

	body, err := c.get(ctx, c.baseURL+"/lookup?"+params.Encode())
	if err != nil || body == nil {
		return nil, err
	}

	var resp iTunesSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("itunes: decode lookup response: %w", err)
	}

	// A lookup can echo back a non-collection wrapper (an artist, say) when the
	// id resolves to something else. Take the first entry that actually carries
	// a collectionId rather than trusting position.
	for _, r := range resp.Results {
		if r.CollectionID == 0 {
			continue
		}
		m := c.matchFromResult(r)
		return &m, nil
	}
	return nil, nil
}

func (c *ITunesClient) get(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("itunes: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("itunes: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("itunes: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("itunes: read response: %w", err)
	}
	return body, nil
}
