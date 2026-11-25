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

// AudiMetaClient queries the AudiMeta API for audiobook metadata.
//
// Reference implementation:
//
//	/opt/booklore-ng/src/lib/metadata/providers/audimeta.ts
//
// Rate limit: 120 requests/minute.
// Base URL: https://api.audimeta.de
type AudiMetaClient struct {
	httpClient *http.Client
	baseURL    string
	limiter    *rate.Limiter
	region     audibleRegion
}

// audiMetaBook is the detailed book shape from GET /books/{asin}.
type audiMetaBook struct {
	ASIN             string           `json:"asin"`
	Title            string           `json:"title"`
	Subtitle         string           `json:"subtitle"`
	Authors          []audiMetaPerson `json:"authors"`
	Narrators        []audiMetaPerson `json:"narrators"`
	Publisher        string           `json:"publisher"`
	ReleaseDate      string           `json:"releaseDate"`
	RuntimeLengthMin int              `json:"runtimeLengthMin"`
	Description      string           `json:"description"`
	Summary          string           `json:"summary"`
	Image            string           `json:"image"`
	Genres           []audiMetaGenre  `json:"genres"`
	Series           *audiMetaSeries  `json:"series"`
	SeriesPrimary    *audiMetaSeries  `json:"seriesPrimary"`
	SeriesSecondary  *audiMetaSeries  `json:"seriesSecondary"`
	Language         string           `json:"language"`
	ISBN             string           `json:"isbn"`
}

type audiMetaPerson struct {
	Name string `json:"name"`
}

type audiMetaGenre struct {
	Name string `json:"name"`
	Type string `json:"type"` // "genre" | "tag"
}

type audiMetaSeries struct {
	Name     string `json:"name"`
	Position string `json:"position"`
}

// audiMetaSearchResult is the lighter shape returned by GET /search.
type audiMetaSearchResult struct {
	ASIN             string           `json:"asin"`
	Title            string           `json:"title"`
	Subtitle         string           `json:"subtitle"`
	Authors          []audiMetaPerson `json:"authors"`
	Narrators        []audiMetaPerson `json:"narrators"`
	Image            string           `json:"image"`
	ReleaseDate      string           `json:"releaseDate"`
	RuntimeLengthMin int              `json:"runtimeLengthMin"`
	Series           *audiMetaSeries  `json:"series"`
}

// audiMetaSearchResponse handles multiple possible envelope shapes.
type audiMetaSearchResponse struct {
	Results []audiMetaSearchResult `json:"results"`
	Books   []audiMetaSearchResult `json:"books"`
	Items   []audiMetaSearchResult `json:"items"`
	Data    []audiMetaSearchResult `json:"data"`
}

func (r audiMetaSearchResponse) items() []audiMetaSearchResult {
	if len(r.Results) > 0 {
		return r.Results
	}
	if len(r.Books) > 0 {
		return r.Books
	}
	if len(r.Items) > 0 {
		return r.Items
	}
	return r.Data
}

func matchFromAudiMetaBook(b audiMetaBook) metadata.Match {
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
		if g.Type == "genre" || g.Type == "" {
			genres = append(genres, g.Name)
		}
	}

	desc := b.Description
	if desc == "" {
		desc = b.Summary
	}
	desc = stripHTML(desc)

	// Primary series preference: series → seriesPrimary → seriesSecondary.
	series := b.Series
	if series == nil {
		series = b.SeriesPrimary
	}
	if series == nil {
		series = b.SeriesSecondary
	}

	var seriesName, seriesPos string
	if series != nil {
		seriesName = series.Name
		seriesPos = series.Position
	}

	return metadata.Match{
		Provider:       "audimeta",
		ProviderID:     b.ASIN,
		ASIN:           b.ASIN,
		ISBN:           b.ISBN,
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

func matchFromAudiMetaSearchResult(r audiMetaSearchResult) metadata.Match {
	authors := make([]string, 0, len(r.Authors))
	for _, a := range r.Authors {
		if a.Name != "" {
			authors = append(authors, a.Name)
		}
	}

	narrators := make([]string, 0, len(r.Narrators))
	for _, n := range r.Narrators {
		if n.Name != "" {
			narrators = append(narrators, n.Name)
		}
	}

	var seriesName, seriesPos string
	if r.Series != nil {
		seriesName = r.Series.Name
		seriesPos = r.Series.Position
	}

	return metadata.Match{
		Provider:       "audimeta",
		ProviderID:     r.ASIN,
		ASIN:           r.ASIN,
		Title:          r.Title,
		Subtitle:       r.Subtitle,
		Authors:        authors,
		Narrators:      narrators,
		PublishYear:    extractYear(r.ReleaseDate),
		DurationMin:    r.RuntimeLengthMin,
		CoverURL:       r.Image,
		SeriesName:     seriesName,
		SeriesPosition: seriesPos,
	}
}

// NewAudiMetaClient creates an AudiMeta client with a 120 rpm rate limiter.
func NewAudiMetaClient() *AudiMetaClient {
	return &AudiMetaClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.audimeta.de",
		limiter:    newLimiter(120),
	}
}

// Search returns matching audiobooks. If q.ProviderIDs["asin"] is set, a
// direct ASIN lookup is performed; otherwise a title/author query is sent.
func (c *AudiMetaClient) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if asin := q.ProviderIDs["asin"]; asin != "" {
		m, err := c.Fetch(ctx, asin)
		if err != nil || m == nil {
			return nil, err
		}
		return []metadata.Match{*m}, nil
	}

	if q.Title == "" && len(q.Authors) == 0 {
		return nil, nil
	}

	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	params := url.Values{}
	if q.Title != "" {
		params.Set("q", q.Title)
	}
	if len(q.Authors) > 0 {
		params.Set("author", strings.Join(q.Authors, " "))
	}
	params.Set("region", c.region.get())

	reqURL := c.baseURL + "/search?" + params.Encode()
	body, err := c.get(ctx, reqURL)
	if err != nil || body == nil {
		return nil, err
	}

	var resp audiMetaSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("audimeta: decode search response: %w", err)
	}

	items := resp.items()
	if len(items) > 10 {
		items = items[:10]
	}

	results := make([]metadata.Match, 0, len(items))
	for _, item := range items {
		results = append(results, matchFromAudiMetaSearchResult(item))
	}
	return results, nil
}

// Fetch returns full metadata for the given ASIN.
func (c *AudiMetaClient) Fetch(ctx context.Context, asin string) (*metadata.Match, error) {
	if err := waitForLimiter(ctx, c.limiter); err != nil {
		return nil, err
	}

	reqURL := c.baseURL + "/books/" + url.PathEscape(strings.ToUpper(asin)) + "?region=" + c.region.get()
	body, err := c.get(ctx, reqURL)
	if err != nil || body == nil {
		return nil, err
	}

	var book audiMetaBook
	if err := json.Unmarshal(body, &book); err != nil {
		return nil, fmt.Errorf("audimeta: decode book response: %w", err)
	}

	m := matchFromAudiMetaBook(book)
	return &m, nil
}

func (c *AudiMetaClient) get(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("audimeta: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Silo/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audimeta: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audimeta: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("audimeta: read response: %w", err)
	}
	return body, nil
}
