package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProviderHintFromIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  map[string]string
		want string
	}{
		{
			name: "legacy provider hint",
			ids:  map[string]string{"provider": "audible", "audible": "B002V0QHBU"},
			want: "audible",
		},
		{
			name: "provider specific id",
			ids:  map[string]string{"itunes": "1234567890"},
			want: "itunes",
		},
		{
			name: "bookbeat id",
			ids:  map[string]string{"bookbeat": "midnight-library-98765"},
			want: "bookbeat",
		},
		{
			name: "audioteka id",
			ids:  map[string]string{"audioteka": "wiedzmin-ostatnie-zyczenie"},
			want: "audioteka",
		},
		{
			name: "audiobookcovers id",
			ids:  map[string]string{"audiobookcovers": "B08G9PRS1K"},
			want: "audiobookcovers",
		},
		{
			name: "ignores capability id",
			ids:  map[string]string{capabilityProviderID: "B002V0QHBU"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerHintFromIDs(tt.ids); got != tt.want {
				t.Fatalf("providerHintFromIDs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewProviderIncludesRegionalAndCoverSources(t *testing.T) {
	p := NewProvider()

	if p.BookBeat == nil {
		t.Fatal("BookBeat source is nil")
	}
	if p.Audioteka == nil {
		t.Fatal("Audioteka source is nil")
	}
	if p.AudiobookCovers == nil {
		t.Fatal("AudiobookCovers source is nil")
	}
}

func TestFirstProviderID(t *testing.T) {
	ids := map[string]string{
		"audible":            "  ",
		capabilityProviderID: "B002V0QHBU",
	}
	if got := firstProviderID(ids, "asin", "audible", capabilityProviderID); got != "B002V0QHBU" {
		t.Fatalf("firstProviderID() = %q, want B002V0QHBU", got)
	}
}

func TestIsLikelyASIN(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "B002V0QHBU", want: true},
		{value: "b002v0qhbu", want: true},
		{value: "B002v0Qhbu", want: true},
		{value: "1234567890", want: true},
		{value: "123456789", want: false},
		{value: "12345678901", want: false},
		{value: "B002V0QHB_", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := isLikelyASIN(tt.value); got != tt.want {
				t.Fatalf("isLikelyASIN(%q) = %t, want %t", tt.value, got, tt.want)
			}
		})
	}
}

func TestIsLikelyAudibleASIN(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "B08G9PRS1K", want: true},
		{value: "B002V0QHBU", want: true},
		{value: "b08g9prs1k", want: true},
		{value: "B08g9Prs1k", want: true},
		{value: "1234567890", want: false},
		{value: "midnight-library-98765", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := isLikelyAudibleASIN(tt.value); got != tt.want {
				t.Fatalf("isLikelyAudibleASIN(%q) = %t, want %t", tt.value, got, tt.want)
			}
		})
	}
}

// silo-server classifies our error by pattern-matching its text: a bare 401 or
// 403 anywhere in the message marks the ITEM permanent and parks it 30 days.
// audimeta answers 403 to every request, so leaking its status code condemned
// items for a reason that says nothing about them. Assert the summary names the
// providers without quoting their status codes.
func TestProvidersFailedErrorHidesStatusCodes(t *testing.T) {
	err := &ProvidersFailedError{Failures: []ProviderFailure{
		{Provider: "audimeta", Kind: FailureBlocked, Err: errors.New("audimeta: HTTP 403")},
		{Provider: "itunes", Kind: FailureUnavailable, Err: errors.New("itunes: HTTP 503")},
	}}

	msg := err.Error()
	for _, banned := range []string{"401", "403", "forbidden", "unauthorized"} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Fatalf("error text leaks %q, which the host reads as permanent: %s", banned, msg)
		}
	}
	if !strings.Contains(msg, "audimeta (blocked)") {
		t.Errorf("summary should name the provider and kind, got: %s", msg)
	}
	if !strings.Contains(msg, "itunes (unavailable)") {
		t.Errorf("summary should name every failure, got: %s", msg)
	}
	// The detail must stay reachable in-process for logging and tests.
	if len(err.Unwrap()) != 2 {
		t.Errorf("Unwrap should expose both underlying errors")
	}
}

func TestProvidersFailedErrorRateLimitedDetection(t *testing.T) {
	blocked := &ProvidersFailedError{Failures: []ProviderFailure{
		{Provider: "audimeta", Kind: FailureBlocked},
	}}
	if blocked.RateLimited() {
		t.Error("a blocked provider is not rate limiting")
	}
	throttled := &ProvidersFailedError{Failures: []ProviderFailure{
		{Provider: "audimeta", Kind: FailureBlocked},
		{Provider: "audible", Kind: FailureRateLimited},
	}}
	if !throttled.RateLimited() {
		t.Error("one throttled provider should ask for the longer backoff")
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		in   error
		want FailureKind
	}{
		{errors.New("audimeta: HTTP 403"), FailureBlocked},
		{errors.New("audnexus: HTTP 401"), FailureBlocked},
		{errors.New("itunes: HTTP 429"), FailureRateLimited},
		// Our own limiter refusing, not the provider throttling us.
		{errors.New("rate: Wait(n=1) would exceed context deadline"), FailureRateLimited},
		{context.DeadlineExceeded, FailureTimeout},
		{errors.New("storytel: HTTP 503"), FailureUnavailable},
	}
	for _, c := range cases {
		if got := classifyFailure(c.in); got != c.want {
			t.Errorf("classifyFailure(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
