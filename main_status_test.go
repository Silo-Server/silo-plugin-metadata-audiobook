package main

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/provider"
)

// The host classifies our failure by gRPC code; codes.Unknown is the one case
// where it falls back to reading the message, and a bare 401/403 there marks
// the item permanent for 30 days. Pin the mapping, since that is the contract.
func TestSearchFailureStatusCodes(t *testing.T) {
	tests := []struct {
		name     string
		failures []provider.ProviderFailure
		want     codes.Code
	}{
		{
			name:     "a blocked provider is an availability problem, not the item's fault",
			failures: []provider.ProviderFailure{{Provider: "audimeta", Kind: provider.FailureBlocked}},
			want:     codes.Unavailable,
		},
		{
			name: "throttling asks for the longer backoff",
			failures: []provider.ProviderFailure{
				{Provider: "audimeta", Kind: provider.FailureBlocked},
				{Provider: "audible", Kind: provider.FailureRateLimited},
			},
			want: codes.ResourceExhausted,
		},
		{
			name:     "a timeout stays retryable",
			failures: []provider.ProviderFailure{{Provider: "itunes", Kind: provider.FailureTimeout}},
			want:     codes.Unavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := searchFailureStatus(&provider.ProvidersFailedError{Failures: tc.failures})
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("error carries no gRPC status; the host would read it as Unknown and fall back to text")
			}
			if st.Code() != tc.want {
				t.Errorf("code = %v, want %v", st.Code(), tc.want)
			}
			if st.Code() == codes.Unknown {
				t.Error("Unknown is the exact case that triggers the host's text heuristic")
			}
		})
	}
}

// Anything that is not an all-providers-failed error must pass through
// untouched, so unrelated failures keep their own classification.
func TestSearchFailureStatusPassesThroughOtherErrors(t *testing.T) {
	sentinel := errors.New("some other failure")
	if got := searchFailureStatus(sentinel); !errors.Is(got, sentinel) {
		t.Fatalf("expected the original error, got %v", got)
	}
}
