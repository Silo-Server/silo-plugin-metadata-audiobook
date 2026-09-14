package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/time/rate"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

// AudibleScraper scrapes Audible product pages for audiobook metadata.
//
// Reference implementation:
//
//	/opt/grimmory/backend/src/main/java/org/booklore/service/metadata/parser/AudibleParser.java
//	/opt/audiobookshelf/server/providers/Audible.js  (region map)
//
// The Java implementation extracts JSON-LD structured data embedded in the
// product page; this Go implementation follows the same strategy using
// github.com/PuerkitoBio/goquery for HTML parsing.
//
// Rate limit: 6 rpm (very conservative scraping).
type AudibleScraper struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	region     string // e.g. "us", "uk", "de"
}

// audibleRegionMap maps region codes to Audible TLD suffixes.
var audibleRegionMap = map[string]string{
	"us": ".com",
	"ca": ".ca",
	"uk": ".co.uk",
	"au": ".com.au",
	"fr": ".fr",
	"de": ".de",
	"jp": ".co.jp",
	"it": ".it",
	"in": ".in",
	"es": ".es",
}

var (
	asinPathRE        = regexp.MustCompile(`/pd/[^/]+/([A-Z0-9]{10})`)
	seriesBookRE      = regexp.MustCompile(`,?\s*Book\s*(\d+(?:\.\d+)?)$`)
	isoDurationRE     = regexp.MustCompile(`PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?`)
	nonAlphanumericRE = regexp.MustCompile(`[^\p{L}\p{M}0-9]`)
)

const audibleUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// NewAudibleScraper creates an Audible scraper for the US region with 6 rpm rate limit.
func NewAudibleScraper() *AudibleScraper {
	return &AudibleScraper{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		limiter:    newLimiter(6),
		region:     "us",
	}
}

func (s *AudibleScraper) tld() string {
	if tld, ok := audibleRegionMap[s.region]; ok {
		return tld
	}
	return ".com"
}

func (s *AudibleScraper) baseURL() string {
	return "https://www.audible" + s.tld()
}

// Search fetches one Audible search results page and returns a thin match
// per listed product: ASIN, title, cover, and whatever the card states
// (author, narrator, runtime). Product pages are not scraped here. Each page
// costs a token from a six-per-minute bucket, so the old search-plus-three-
// product-pages design took about twenty seconds; the host resolves the
// winning candidate through Fetch, which reads the product page once.
func (s *AudibleScraper) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	if asin := q.ProviderIDs["asin"]; asin != "" {
		m, err := s.Fetch(ctx, asin)
		if err != nil || m == nil {
			return nil, err
		}
		return []metadata.Match{*m}, nil
	}

	if q.Title == "" {
		return nil, nil
	}

	doc, err := s.searchDoc(ctx, q)
	if err != nil || doc == nil {
		return nil, err
	}
	return parseSearchResults(doc, maxSearchResults), nil
}

// maxSearchResults caps the thin matches returned from one results page.
const maxSearchResults = 5

// Fetch scrapes the Audible product page for the given ASIN.
func (s *AudibleScraper) Fetch(ctx context.Context, asin string) (*metadata.Match, error) {
	asin = normalizeASIN(asin)
	if err := waitForLimiter(ctx, s.limiter); err != nil {
		return nil, err
	}

	pageURL := s.baseURL() + "/pd/" + url.PathEscape(asin)
	doc, err := s.fetchDoc(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, nil
	}

	return s.parseProductPage(doc, asin), nil
}

// searchDoc fetches the search results page for the query.
func (s *AudibleScraper) searchDoc(ctx context.Context, q metadata.SearchQuery) (*goquery.Document, error) {
	if err := waitForLimiter(ctx, s.limiter); err != nil {
		return nil, err
	}

	// Build search term: clean title + optional author.
	var parts []string
	if q.Title != "" {
		parts = append(parts, cleanSearchTerm(q.Title))
	}
	for _, a := range q.Authors {
		if a != "" {
			parts = append(parts, cleanSearchTerm(a))
		}
	}
	term := strings.Join(parts, " ")
	if term == "" {
		return nil, nil
	}

	searchURL := s.baseURL() + "/search?keywords=" + url.QueryEscape(term)
	return s.fetchDoc(ctx, searchURL)
}

// parseSearchResults reads the product cards on a search results page. A
// card carries the ASIN in its product link, the title as its heading, the
// cover image, and "By:", "Narrated by:" and "Length:" lines.
func parseSearchResults(doc *goquery.Document, limit int) []metadata.Match {
	var results []metadata.Match
	seen := make(map[string]bool)
	doc.Find("[data-widget='productList'] li.productListItem").EachWithBreak(func(_ int, card *goquery.Selection) bool {
		asin := ""
		card.Find("a[href*='/pd/']").EachWithBreak(func(_ int, a *goquery.Selection) bool {
			href, _ := a.Attr("href")
			if m := asinPathRE.FindStringSubmatch(href); len(m) >= 2 {
				asin = m[1]
				return false
			}
			return true
		})
		if asin == "" || seen[asin] {
			return true
		}
		seen[asin] = true

		title := strings.TrimSpace(card.Find("h3").First().Text())
		if title == "" {
			title = strings.TrimSpace(card.AttrOr("aria-label", ""))
		}
		cover := card.Find("img").First().AttrOr("src", "")

		m := metadata.Match{
			Provider:   "audible",
			ProviderID: asin,
			ASIN:       asin,
			Title:      title,
			CoverURL:   cover,
		}
		card.Find("li").Each(func(_ int, li *goquery.Selection) {
			text := strings.TrimSpace(strings.Join(strings.Fields(li.Text()), " "))
			switch {
			case strings.HasPrefix(text, "By:"):
				m.Authors = splitAudibleNames(strings.TrimPrefix(text, "By:"))
			case strings.HasPrefix(text, "Narrated by:"):
				m.Narrators = splitAudibleNames(strings.TrimPrefix(text, "Narrated by:"))
			case strings.HasPrefix(text, "Length:"):
				m.DurationMin = parseAudibleLength(strings.TrimPrefix(text, "Length:"))
			}
		})
		results = append(results, m)
		return limit <= 0 || len(results) < limit
	})
	return results
}

// splitAudibleNames splits "A, B, and C" style name lists from a card line.
func splitAudibleNames(text string) []string {
	var out []string
	for _, part := range strings.Split(strings.ReplaceAll(text, " and ", ","), ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

var audibleLengthRE = regexp.MustCompile(`(?:(\d+)\s*hrs?)?\s*(?:and\s*)?(?:(\d+)\s*mins?)?`)

// parseAudibleLength reads "15 hrs and 40 mins" style runtimes into minutes.
func parseAudibleLength(text string) int {
	m := audibleLengthRE.FindStringSubmatch(strings.TrimSpace(text))
	if len(m) < 3 {
		return 0
	}
	hours, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	return hours*60 + mins
}

// jsonLdNode is a minimally-typed JSON-LD node for Audible pages.
type jsonLdNode struct {
	Type            string          `json:"@type"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Image           string          `json:"image"`
	InLanguage      string          `json:"inLanguage"`
	Publisher       string          `json:"publisher"`
	DatePublished   string          `json:"datePublished"`
	Duration        string          `json:"duration"`
	Abridged        string          `json:"abridged"`
	Author          json.RawMessage `json:"author"`
	ReadBy          json.RawMessage `json:"readBy"`
	AggregateRating *struct {
		RatingValue string `json:"ratingValue"`
		RatingCount string `json:"ratingCount"`
	} `json:"aggregateRating"`
}

type jsonLdBreadcrumb struct {
	Type            string `json:"@type"`
	ItemListElement []struct {
		Item struct {
			Name string `json:"name"`
		} `json:"item"`
	} `json:"itemListElement"`
}

// parseProductPage extracts metadata from an Audible product page document.
func (s *AudibleScraper) parseProductPage(doc *goquery.Document, asin string) *metadata.Match {
	if doc == nil {
		return nil
	}

	var audiobookNode *jsonLdNode
	var breadcrumb *jsonLdBreadcrumb

	// Parse all JSON-LD script blocks.
	doc.Find("script[type='application/ld+json']").Each(func(_ int, sel *goquery.Selection) {
		content := sel.Text()

		// Try array-wrapped JSON-LD (Audible often sends an array).
		var nodes []json.RawMessage
		if err := json.Unmarshal([]byte(content), &nodes); err == nil {
			for _, raw := range nodes {
				var node jsonLdNode
				if err2 := json.Unmarshal(raw, &node); err2 == nil && node.Type == "Audiobook" {
					n := node
					audiobookNode = &n
				}
				var bc jsonLdBreadcrumb
				if err2 := json.Unmarshal(raw, &bc); err2 == nil && bc.Type == "BreadcrumbList" {
					b := bc
					breadcrumb = &b
				}
			}
			return
		}

		// Single object.
		var node jsonLdNode
		if err := json.Unmarshal([]byte(content), &node); err == nil {
			if node.Type == "Audiobook" {
				audiobookNode = &node
			}
		}
		var bc jsonLdBreadcrumb
		if err := json.Unmarshal([]byte(content), &bc); err == nil && bc.Type == "BreadcrumbList" {
			breadcrumb = &bc
		}
	})

	if audiobookNode == nil {
		return nil
	}

	// Parse title / subtitle (split on first colon, strip series book suffix).
	fullName := audiobookNode.Name
	if m := seriesBookRE.FindStringIndex(fullName); m != nil {
		fullName = strings.TrimSpace(fullName[:m[0]])
	}
	var title, subtitle string
	if idx := strings.Index(fullName, ":"); idx > 0 && idx < len(fullName)-1 {
		title = strings.TrimSpace(fullName[:idx])
		subtitle = strings.TrimSpace(fullName[idx+1:])
	} else {
		title = fullName
	}

	// Authors and narrators.
	authors := extractJSONLDPersonNames(audiobookNode.Author)
	narrators := extractJSONLDPersonNames(audiobookNode.ReadBy)

	// Duration: ISO 8601 → minutes.
	durationMin := parseISODurationToMin(audiobookNode.Duration)

	// Genres from breadcrumb.
	var genres []string
	if breadcrumb != nil {
		for _, item := range breadcrumb.ItemListElement {
			name := item.Item.Name
			if name != "" && !strings.EqualFold(name, "Home") {
				genres = append(genres, name)
			}
		}
	}

	// Series from HTML links (reliable even when JSON-LD omits it).
	var seriesName, seriesPos string
	doc.Find("a[href*='/series/']").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		text := strings.TrimSpace(sel.Text())
		if text == "" {
			return true
		}
		m := seriesBookRE.FindStringSubmatch(text)
		if len(m) >= 2 {
			seriesPos = m[1]
			seriesName = strings.TrimSpace(seriesBookRE.ReplaceAllString(text, ""))
			return false
		}
		if seriesName == "" {
			seriesName = text
		}
		return true
	})

	return &metadata.Match{
		Provider:       "audible",
		ProviderID:     asin,
		ASIN:           asin,
		Title:          title,
		Subtitle:       subtitle,
		Authors:        authors,
		Narrators:      narrators,
		Publisher:      audiobookNode.Publisher,
		PublishYear:    extractYear(audiobookNode.DatePublished),
		DurationMin:    durationMin,
		Description:    stripHTML(audiobookNode.Description),
		CoverURL:       audiobookNode.Image,
		Language:       audiobookNode.InLanguage,
		Genres:         genres,
		SeriesName:     seriesName,
		SeriesPosition: seriesPos,
	}
}

// extractJSONLDPersonNames parses an author/readBy field that may be an object
// or an array of objects with a "name" field.
func extractJSONLDPersonNames(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}

	var single struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Name != "" {
		return []string{single.Name}
	}

	var many []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &many); err == nil {
		names := make([]string, 0, len(many))
		for _, p := range many {
			if p.Name != "" {
				names = append(names, p.Name)
			}
		}
		return names
	}

	return nil
}

// parseISODurationToMin parses an ISO 8601 duration string (e.g. "PT10H30M")
// into minutes. Returns 0 on parse failure.
func parseISODurationToMin(s string) int {
	if s == "" {
		return 0
	}
	m := isoDurationRE.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	hours := parseDigits(m[1])
	mins := parseDigits(m[2])
	secs := parseDigits(m[3])
	return hours*60 + mins + secs/60
}

func parseDigits(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// cleanSearchTerm removes non-alphanumeric characters from each word.
func cleanSearchTerm(text string) string {
	words := strings.Fields(text)
	out := make([]string, 0, len(words))
	for _, w := range words {
		cleaned := nonAlphanumericRE.ReplaceAllString(w, "")
		if cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return strings.Join(out, " ")
}

// fetchDoc downloads the URL and parses it with goquery.
func (s *AudibleScraper) fetchDoc(ctx context.Context, pageURL string) (*goquery.Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("audible: create request: %w", err)
	}
	req.Header.Set("User-Agent", audibleUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audible: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audible: HTTP %d for %s", resp.StatusCode, pageURL)
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("audible: parse HTML: %w", err)
	}
	return doc, nil
}
