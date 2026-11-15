package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// AudnexusClient queries https://api.audnex.us for audiobook metadata.
//
// Reference implementations:
//
//	/opt/booklore-ng/src/lib/metadata/providers/audnexus.ts
//	/opt/audiobookshelf/server/providers/Audnexus.js
//
// Audnexus has no general book-title search — only ASIN-based lookups.
// Search() returns results only when q.ProviderIDs["asin"] is set.
// Rate limit: DefaultAudnexusPerMinute requests a minute until the host sets
// another one (SetRequestsPerMinute).
type AudnexusClient struct {
	httpClient *http.Client
	baseURL    string
	limiter    *rate.Limiter
}

// audnexusBook is the JSON shape returned by GET /books/{asin}.
type audnexusBook struct {
	ASIN             string           `json:"asin"`
	Title            string           `json:"title"`
	Subtitle         string           `json:"subtitle"`
	Authors          []audnexusPerson `json:"authors"`
	Narrators        []audnexusPerson `json:"narrators"`
	Publisher        string           `json:"publisher"`
	ReleaseDate      string           `json:"releaseDate"`
	RuntimeLengthMin int              `json:"runtimeLengthMin"`
	Description      string           `json:"description"`
	Summary          string           `json:"summary"`
	Image            string           `json:"image"`
	Genres           []audnexusGenre  `json:"genres"`
	Series           []audnexusSeries `json:"series"`
	Language         string           `json:"language"`
}

type audnexusPerson struct {
	Name string `json:"name"`
}

type audnexusGenre struct {
	Name string `json:"name"`
	Type string `json:"type"` // "genre" | "tag"
}

type audnexusSeries struct {
	Name     string `json:"name"`
	Position string `json:"position"`
}

var (
	htmlTagRE = regexp.MustCompile(`<[^>]+>`)
	// Block-level tags separate sentences; dropping them outright glued
	// "originality.</p><p>Read by" into "originality.Read by".
	htmlBlockTagRE = regexp.MustCompile(`(?i)<\s*/?\s*(p|br|div|li|ul|ol|h[1-6])\b[^>]*>`)
	spaceRunRE     = regexp.MustCompile(`[ \t]{2,}`)
	// A block tag beside a line break leaves a stray space on either side of it.
	newlineSpaceRE = regexp.MustCompile(`[ \t]*(\r?\n)[ \t]*`)
)

func stripHTML(s string) string {
	s = htmlBlockTagRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllString(s, "")
	// Decode every entity, numeric ones (&#xa0;, &#8220;) included.
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = spaceRunRE.ReplaceAllString(s, " ")
	s = newlineSpaceRE.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func extractYear(dateStr string) int {
	if len(dateStr) < 4 {
		return 0
	}
	y := 0
	for _, ch := range dateStr[:4] {
		if ch < '0' || ch > '9' {
			return 0
		}
		y = y*10 + int(ch-'0')
	}
	return y
}

func bookFromAudnexus(b audnexusBook) metadata.Match {
	authors := make([]string, 0, len(b.Authors))
	for _, a := range b.Authors {
		if a.Name != "" {
			authors = append(authors, a.Name)
		}
	}

	narrators := make([]string, 0, len(b.Narrators))
	for _, n := range b.Narrators {
		if n.Name != "" {
			narrators = append(narrators, n.Name)
		}
	}

	genres := make([]string, 0, len(b.Genres))
	for _, g := range b.Genres {
		// include entries that are type "genre" or have no type set
		if g.Type == "genre" || g.Type == "" {
			genres = append(genres, g.Name)
		}
	}

	desc := b.Description
	if desc == "" {
		desc = b.Summary
	}
	desc = stripHTML(desc)

	var seriesName, seriesPos string
	if len(b.Series) > 0 {
		seriesName = b.Series[0].Name
		seriesPos = b.Series[0].Position
	}

	return metadata.Match{
		Provider:       "audnexus",
		ProviderID:     b.ASIN,
		ASIN:           b.ASIN,
		Title:          b.Title,
		Subtitle:       b.Subtitle,
		Authors:        authors,
		Narrators:      narrators,
		Publisher:      b.Publisher,
		PublishYear:    extractYear(b.ReleaseDate),
		DurationMin:    b.RuntimeLengthMin,
		Description:    desc,
		CoverURL:       b.Image,
		Genres:         genres,
		SeriesName:     seriesName,
		SeriesPosition: seriesPos,
		Language:       b.Language,
	}
}

const (
	// DefaultAudnexusPerMinute is the Audnexus request rate until an admin
	// sets one. It leaves most of the public API's allowance unused, so a
	// server does not lean on a free community service by default.
	DefaultAudnexusPerMinute = 100
	// MaxAudnexusPerMinute is the highest rate an admin can set: the public
	// api.audnex.us advertises 300 requests a minute for each address
	// (x-ratelimit-limit) and answers 429 beyond it.
	MaxAudnexusPerMinute = 300
	// MinAudnexusPerMinute is the lowest. A request waits on the limiter
	// inside providerTimeout, and the limiter refuses at once a wait that
	// would outlast it. At 20 a minute a request is three seconds behind the
	// one before it, so a few lookups arriving together still fit; a rate of
	// one or two a minute would fail them instead of slowing them down.
	MinAudnexusPerMinute = 20
)

// NewAudnexusClient creates an Audnexus client limited to
// DefaultAudnexusPerMinute requests a minute.
func NewAudnexusClient() *AudnexusClient {
	return &AudnexusClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.audnex.us",
		limiter:    newLimiter(DefaultAudnexusPerMinute),
	}
}

// SetRequestsPerMinute changes the request rate, kept between
// MinAudnexusPerMinute and MaxAudnexusPerMinute. Safe to call while requests
// wait on the limiter: a request already waiting keeps the slot it was given,
// and requests after it are spaced at the new rate.
func (c *AudnexusClient) SetRequestsPerMinute(perMinute int) {
	perMinute = min(max(perMinute, MinAudnexusPerMinute), MaxAudnexusPerMinute)
	c.limiter.SetLimit(rate.Limit(float64(perMinute) / 60.0))
}

// RequestsPerMinute returns the current request rate.
func (c *AudnexusClient) RequestsPerMinute() int {
	return int(math.Round(float64(c.limiter.Limit()) * 60.0))
}

// Search resolves by ASIN when q.ProviderIDs["asin"] is set and Audnexus has
// a book for it, and otherwise by title against the /books/search endpoint.
func (c *AudnexusClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if asin := q.ProviderIDs["asin"]; asin != "" {
		m, err := c.Fetch(ctx, asin)
		if err != nil {
			return nil, err
		}
		if m != nil {
			return []metadata.Match{*m}, nil
		}
		// No book under that ASIN: the tag may be wrong, so the title still
		// gets its search.
	}

	// Fall back to title search if a title is provided.
	if q.Title == "" {
		return nil, nil
	}

	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	// The title-search route is /books/search, not /books. /books?q= is not a
	// route at all: api.audnex.us answers it with
	// {"message":"Route GET:/books?q=... not found","statusCode":404}, so every
	// title search this client issued failed, silently, for every audiobook
	// that had no ASIN yet -- which is exactly the population that needs one.
	// Verified 2026-07-27 against 20 unidentified production audiobooks: the
	// old path returned 404 on all 20, the correct path answered 18.
	params := url.Values{}
	params.Set("q", q.Title)
	params.Set("region", "us")
	reqURL := c.baseURL + "/books/search?" + params.Encode()

	body, err := c.get(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	// Response is an array of books.
	var books []audnexusBook
	if err := json.Unmarshal(body, &books); err != nil {
		// Some versions wrap in {"books": [...]}
		var wrapper struct {
			Books []audnexusBook `json:"books"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			return nil, fmt.Errorf("audnexus: decode search response: %w", err)
		}
		books = wrapper.Books
	}

	results := make([]metadata.Match, 0, len(books))
	for _, b := range books {
		results = append(results, bookFromAudnexus(b))
	}
	return results, nil
}

// errAudnexusBadRequest is an HTTP 400 answer. For a book fetch it means
// Audnexus has no book under that ID.
var errAudnexusBadRequest = errors.New("audnexus: HTTP 400")

// Fetch returns full metadata for the given ASIN, or nothing when Audnexus
// has no book for it. That covers more than an unknown ASIN: a value that is
// not an ASIN at all (a tag holding another service's ID) is not sent, and
// Audnexus answers 400 for an ASIN it knows as something other than a book,
// such as a podcast. Both describe the item, not the service. Reported as
// failures, they would count against the provider on the host, which pauses
// it after a few in a row.
func (c *AudnexusClient) Fetch(ctx context.Context, asin string) (*metadata.Match, error) {
	if !isLikelyASIN(asin) {
		return nil, nil
	}
	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/books/" + url.PathEscape(normalizeASIN(asin)) + "?region=us"
	body, err := c.get(ctx, reqURL)
	if errors.Is(err, errAudnexusBadRequest) {
		return nil, nil
	}
	if err != nil || body == nil {
		// get() reports an unknown/delisted ASIN as (nil, nil); decoding that
		// nil body would surface as a bogus JSON error instead of "no match".
		return nil, err
	}

	var book audnexusBook
	if err := json.Unmarshal(body, &book); err != nil {
		return nil, fmt.Errorf("audnexus: decode book response: %w", err)
	}

	m := bookFromAudnexus(book)
	return &m, nil
}

func (c *AudnexusClient) get(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("audnexus: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Silo/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audnexus: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return nil, errAudnexusBadRequest
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audnexus: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("audnexus: read response: %w", err)
	}
	return body, nil
}
