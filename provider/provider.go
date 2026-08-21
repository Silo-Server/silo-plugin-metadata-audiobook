package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
)

const (
	capabilityProviderID = "audiobook-metadata"

	// providerTimeout is the per-provider deadline for Search/Fetch calls.
	//
	// Must exceed the slowest provider's rate-limit interval, or that provider can
	// never serve a second request. rate.Limiter.Wait fails IMMEDIATELY when the
	// required wait would outlast the context deadline, so with the four scrapers
	// at 6 rpm -- one request every 10s -- a 10s deadline meant every queued call
	// returned "rate: Wait(n=1) would exceed context deadline" without a single
	// HTTP request being made. That was 1,542 of the 2,645 errors seen in a
	// 30-minute window on 2026-08-21.
	//
	// The ceiling is the host's own deadline: pluginhost.DefaultMetadataTimeout
	// is 30s for a metadata RPC, so anything at or above that just moves the
	// failure from us to the caller -- 35s produced a sweep of
	// "rpc error: code = DeadlineExceeded" instead of results.
	//
	// 25s stays inside that budget with headroom for gRPC overhead while still
	// leaving room for a full 10s limiter wait plus a slow scrape, so requests
	// queue and succeed at the limited rate instead of failing instantly.
	providerTimeout = 25 * time.Second

	// searchWorkers is the maximum number of providers queried in parallel.
	searchWorkers = 3
)

// searchProvider is the minimal interface each backend must satisfy.
type searchProvider interface {
	Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error)
}

// Provider is the coordinator for all audiobook metadata sources.
type Provider struct {
	Audnexus        *AudnexusClient
	AudiMeta        *AudiMetaClient
	ITunes          *ITunesClient
	Audible         *AudibleScraper
	Storytel        *StorytelScraper
	BookBeat        *BookBeatScraper
	Audioteka       *AudiotekaScraper
	AudiobookCovers *AudiobookCoversClient
}

// NewProvider creates a Provider with all source clients initialized.
func NewProvider() *Provider {
	return &Provider{
		Audnexus:        NewAudnexusClient(),
		AudiMeta:        NewAudiMetaClient(),
		ITunes:          NewITunesClient(),
		Audible:         NewAudibleScraper(),
		Storytel:        NewStorytelScraper(),
		BookBeat:        NewBookBeatScraper(),
		Audioteka:       NewAudiotekaScraper(),
		AudiobookCovers: NewAudiobookCoversClient(),
	}
}

// Search queries all registered providers in parallel (up to searchWorkers
// concurrent goroutines) and returns the merged results.
// Errors from individual providers are logged but are not fatal.
func (p *Provider) Search(ctx context.Context, q metadata.SearchQuery) ([]metadata.Match, error) {
	type task struct {
		name string
		fn   searchProvider
	}

	tasks := []task{
		{"audnexus", p.Audnexus},
		{"audimeta", p.AudiMeta},
		{"itunes", p.ITunes},
		{"audible", p.Audible},
		{"storytel", p.Storytel},
		{"bookbeat", p.BookBeat},
		{"audioteka", p.Audioteka},
		{"audiobookcovers", p.AudiobookCovers},
	}

	type result struct {
		name    string
		matches []metadata.Match
		err     error
	}

	ch := make(chan result, len(tasks))
	sem := make(chan struct{}, searchWorkers)

	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func(name string, sp searchProvider) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			tctx, cancel := context.WithTimeout(ctx, providerTimeout)
			defer cancel()

			matches, err := sp.Search(tctx, q)
			if err != nil {
				// The full provider error stays HERE, in the plugin log. It is
				// deliberately not forwarded verbatim to the host -- see
				// ProvidersFailedError.Error() for why.
				log.Printf("audiobook-metadata: provider %s search error: %v", name, err)
				ch <- result{name: name, err: err}
				return
			}
			ch <- result{name: name, matches: matches}
		}(t.name, t.fn)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var all []metadata.Match
	var failures []ProviderFailure
	for r := range ch {
		all = append(all, r.matches...)
		if r.err != nil {
			failures = append(failures, ProviderFailure{
				Provider: r.name,
				Kind:     classifyFailure(r.err),
				Err:      r.err,
			})
		}
	}

	// Report failure only when EVERY provider failed and none returned a match.
	//
	// The caller cannot tell "searched and found nothing" from "could not reach
	// anything" unless we say so, and it acts very differently on each: Silo
	// stamps a clean empty result as outcome=no_match with next_attempt_at NULL,
	// which is terminal -- the item is never looked at again. Swallowing the
	// errors here meant a provider outage permanently wrote items off. On
	// 2026-08-21 that marked 500 audiobooks as "no match" in 25 minutes, on
	// course to write off all 242,330 in about nine days, without a single
	// provider having answered.
	//
	// Partial success stays a success: if one provider answered, the others
	// failing is normal and the matches we did get are worth returning.
	if len(all) == 0 && len(failures) > 0 {
		return nil, &ProvidersFailedError{Failures: failures}
	}

	return all, nil
}

// FailureKind is a small, stable vocabulary for why a provider could not
// answer. It exists so the aggregate error can say what happened without
// quoting the underlying provider text -- see ProvidersFailedError.Error().
type FailureKind string

const (
	FailureBlocked     FailureKind = "blocked"      // 401/403 -- refuses us entirely
	FailureRateLimited FailureKind = "rate-limited" // 429, or our own limiter starving
	FailureTimeout     FailureKind = "timeout"
	FailureUnavailable FailureKind = "unavailable" // network, 5xx, bad payload
)

// ProviderFailure records one provider's failure for a single query.
type ProviderFailure struct {
	Provider string
	Kind     FailureKind
	Err      error
}

// ProvidersFailedError reports that every provider failed and none matched.
//
// Its Error() text is a SUMMARY -- provider names and a failure kind, never the
// underlying provider message. That is not cosmetic and not an attempt to hide
// detail (the full error is logged in Search, next to the provider that raised
// it). It exists because the host classifies our error by pattern-matching its
// TEXT: silo-server's classifyProviderErrorText treats a bare 401 or 403
// anywhere in the message as a PERMANENT failure for the item, and parks it for
// 30 days.
//
// We aggregate eight providers into one error, so that heuristic misfired
// badly. audimeta answers HTTP 403 to every request, so its status code leaked
// into the joined text and condemned the item -- 67 items were parked to
// 2026-09-20 for the sole reason that one provider is blocked, which says
// nothing whatsoever about the item. A provider being unreachable is an
// availability problem and must stay retryable.
type ProvidersFailedError struct {
	Failures []ProviderFailure
}

func (e *ProvidersFailedError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s (%s)", f.Provider, f.Kind))
	}
	return fmt.Sprintf("all %d audiobook provider(s) failed: %s",
		len(e.Failures), strings.Join(parts, ", "))
}

// Unwrap keeps the underlying errors reachable for callers in this process
// (tests, logging). Only Error() crosses the RPC boundary.
func (e *ProvidersFailedError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

// RateLimited reports whether any provider failed for a throttling reason, so
// the caller can ask the host for a longer backoff than a plain retry.
func (e *ProvidersFailedError) RateLimited() bool {
	for _, f := range e.Failures {
		if f.Kind == FailureRateLimited {
			return true
		}
	}
	return false
}

// classifyFailure maps a provider error onto the vocabulary above. Text
// matching is unavoidable here -- the providers are HTTP scrapers and clients
// that report status inline -- but it happens ONCE, at the edge, instead of
// leaving raw status codes to be re-interpreted downstream.
func classifyFailure(err error) FailureKind {
	if err == nil {
		return FailureUnavailable
	}
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timeout"),
		// golang.org/x/time/rate refuses when the wait would outlast the
		// deadline. That is our own limiter throttling us, not the provider.
		strings.Contains(msg, "would exceed context deadline"):
		if strings.Contains(msg, "rate:") || strings.Contains(msg, "would exceed context deadline") {
			return FailureRateLimited
		}
		return FailureTimeout
	case strings.Contains(msg, "429"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "rate limit"):
		return FailureRateLimited
	case strings.Contains(msg, "401"),
		strings.Contains(msg, "403"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "unauthorized"):
		return FailureBlocked
	default:
		return FailureUnavailable
	}
}

// Fetch retrieves full metadata for a specific item by providerID and externalID.
// providerID selects which backend to use (e.g. "audnexus", "audimeta", "itunes",
// "audible", "storytel"). externalID is the backend-specific item identifier.
func (p *Provider) Fetch(ctx context.Context, q metadata.SearchQuery) (*metadata.Match, error) {
	tctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	// Dispatch by explicit legacy hint or by the real provider-specific IDs
	// stored in ProviderIDs.
	providerHint := providerHintFromIDs(q.ProviderIDs)

	switch providerHint {
	case "audnexus":
		asin := firstProviderID(q.ProviderIDs, "asin", "audnexus", capabilityProviderID)
		if asin != "" {
			return p.Audnexus.Fetch(tctx, asin)
		}
	case "audimeta":
		asin := firstProviderID(q.ProviderIDs, "asin", "audimeta", capabilityProviderID)
		if asin != "" {
			return p.AudiMeta.Fetch(tctx, asin)
		}
	case "itunes":
		if id := firstProviderID(q.ProviderIDs, "itunes", capabilityProviderID); id != "" {
			return p.ITunes.Fetch(tctx, id)
		}
	case "audible":
		asin := firstProviderID(q.ProviderIDs, "asin", "audible", capabilityProviderID)
		if asin != "" {
			return p.Audible.Fetch(tctx, asin)
		}
	case "storytel":
		if id := firstProviderID(q.ProviderIDs, "storytel", capabilityProviderID); id != "" {
			return p.Storytel.Fetch(tctx, id)
		}
	case "bookbeat":
		if id := firstProviderID(q.ProviderIDs, "bookbeat", capabilityProviderID); id != "" {
			return p.BookBeat.Fetch(tctx, id)
		}
	case "audioteka":
		if id := firstProviderID(q.ProviderIDs, "audioteka", capabilityProviderID); id != "" {
			return p.Audioteka.Fetch(tctx, id)
		}
	case "audiobookcovers":
		asin := firstProviderID(q.ProviderIDs, "asin", "audiobookcovers", capabilityProviderID)
		if asin != "" {
			return p.AudiobookCovers.Fetch(tctx, asin)
		}
	}

	// Fallback: if an ASIN is present, try Audnexus then AudiMeta. Only
	// treat the capability-level ID as an ASIN when it has ASIN shape, since
	// non-ASIN providers such as iTunes also use that field as a fallback ID.
	asin := firstProviderID(q.ProviderIDs, "asin", "audnexus", "audimeta", "audible")
	if asin == "" {
		if id := firstProviderID(q.ProviderIDs, capabilityProviderID); isLikelyASIN(id) {
			asin = id
		}
	}
	if asin != "" {
		if m, err := p.Audnexus.Fetch(tctx, asin); m != nil || err != nil {
			return m, err
		}
		return p.AudiMeta.Fetch(tctx, asin)
	}

	return nil, nil
}

func providerHintFromIDs(ids map[string]string) string {
	if hint := strings.TrimSpace(ids["provider"]); hint != "" {
		return hint
	}
	for _, provider := range []string{
		"audnexus",
		"audimeta",
		"itunes",
		"audible",
		"storytel",
		"bookbeat",
		"audioteka",
		"audiobookcovers",
	} {
		if strings.TrimSpace(ids[provider]) != "" {
			return provider
		}
	}
	return ""
}

func isLikelyASIN(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 10 {
		return false
	}
	for _, ch := range value {
		if (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		return false
	}
	return true
}

func isLikelyAudibleASIN(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 10 || !strings.HasPrefix(value, "B0") {
		return false
	}
	return isLikelyASIN(value)
}

func normalizeASIN(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func firstProviderID(ids map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(ids[key]); value != "" {
			return value
		}
	}
	return ""
}
