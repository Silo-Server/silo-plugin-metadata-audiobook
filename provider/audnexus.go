package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// Rate limit: 100 requests/minute.
type AudnexusClient struct {
	httpClient *http.Client
	baseURL    string
	limiter    *rate.Limiter
}

// audnexusBook is the JSON shape returned by GET /books/{asin}.
type audnexusBook struct {
	ASIN             string              `json:"asin"`
	Title            string              `json:"title"`
	Subtitle         string              `json:"subtitle"`
	Authors          []audnexusPerson    `json:"authors"`
	Narrators        []audnexusPerson    `json:"narrators"`
	Publisher        string              `json:"publisher"`
	ReleaseDate      string              `json:"releaseDate"`
	RuntimeLengthMin int                 `json:"runtimeLengthMin"`
	Description      string              `json:"description"`
	Summary          string              `json:"summary"`
	Image            string              `json:"image"`
	Genres           []audnexusGenre     `json:"genres"`
	Series           []audnexusSeries    `json:"series"`
	Language         string              `json:"language"`
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

var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	s = htmlTagRE.ReplaceAllString(s, "")
	// Normalise common HTML entities.
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&nbsp;", " ",
	)
	return strings.TrimSpace(replacer.Replace(s))
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

// NewAudnexusClient creates an Audnexus client with a 100 rpm rate limiter.
func NewAudnexusClient() *AudnexusClient {
	return &AudnexusClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.audnex.us",
		limiter:    newLimiter(100),
	}
}

// Search resolves by ASIN when q.ProviderIDs["asin"] is set, and otherwise by
// title against the /books/search endpoint.
func (c *AudnexusClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if asin := q.ProviderIDs["asin"]; asin != "" {
		m, err := c.Fetch(ctx, asin)
		if err != nil || m == nil {
			return nil, err
		}
		return []metadata.Match{*m}, nil
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

// Fetch returns full metadata for the given ASIN.
func (c *AudnexusClient) Fetch(ctx context.Context, asin string) (*metadata.Match, error) {
	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/books/" + url.PathEscape(strings.ToUpper(asin)) + "?region=us"
	body, err := c.get(ctx, reqURL)
	if err != nil {
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audnexus: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("audnexus: read response: %w", err)
	}
	return body, nil
}
