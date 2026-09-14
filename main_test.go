package main

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
	"github.com/Silo-Server/silo-plugin-audiobook-metadata/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestRuntimeServerConfigureAppliesSources(t *testing.T) {
	server := &runtimeServer{provider: provider.NewProvider()}

	// No config: defaults stand.
	if _, err := server.Configure(context.Background(), &pluginv1.ConfigureRequest{}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}
	if got := server.provider.Sources(); got != provider.DefaultSourceConfig() {
		t.Fatalf("Sources() after empty Configure = %+v", got)
	}

	// A partial "sources" entry overrides only the keys it names.
	value, err := structpb.NewStruct(map[string]any{"audible": true, "itunes": false})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "sources", Value: value}},
	}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}
	got := server.provider.Sources()
	want := provider.DefaultSourceConfig()
	want.Audible = true
	want.ITunes = false
	if got != want {
		t.Fatalf("Sources() = %+v, want %+v", got, want)
	}

	// Entries under other keys are ignored.
	other, _ := structpb.NewStruct(map[string]any{"audnexus": false})
	if _, err := server.Configure(context.Background(), &pluginv1.ConfigureRequest{
		Config: []*pluginv1.ConfigEntry{{Key: "account", Value: other}},
	}); err != nil {
		t.Fatalf("Configure() returned error: %v", err)
	}
	if got := server.provider.Sources(); !got.Audnexus {
		t.Fatalf("unrelated config key changed sources: %+v", got)
	}
}

func TestMetadataServerGetMetadata_ReturnsNilForUnknown(t *testing.T) {
	rs := &runtimeServer{provider: provider.NewProvider()}
	ms := &metadataServer{runtime: rs}

	resp, err := ms.GetMetadata(context.Background(), &pluginv1.GetMetadataRequest{
		ProviderId: "unknown-id",
		ItemType:   "audiobook",
	})
	if err != nil {
		t.Fatalf("GetMetadata() returned error: %v", err)
	}
	if resp.GetItem() != nil {
		t.Fatalf("expected nil item, got %v", resp.GetItem())
	}
}

func TestProviderSearchResultFromMatchMapsProviderIDs(t *testing.T) {
	result, err := providerSearchResultFromMatch(metadata.Match{
		Provider:    "audible",
		ProviderID:  "B012345678",
		Title:       "The Name of the Wind",
		PublishYear: 2007,
		Description: "A book summary",
		ASIN:        "B012345678",
		CoverURL:    "https://example.test/cover.jpg",
	}, "audiobook")
	if err != nil {
		t.Fatalf("providerSearchResultFromMatch() error = %v", err)
	}
	if result.GetProviderId() != "B012345678" {
		t.Fatalf("ProviderId = %q, want B012345678", result.GetProviderId())
	}
	if result.GetTitle() != "The Name of the Wind" {
		t.Fatalf("Title = %q", result.GetTitle())
	}

	ids := result.GetProviderIds().AsMap()
	if _, ok := ids["provider"]; ok {
		t.Fatalf("provider ids should not include synthetic provider hint: %#v", ids)
	}
	if ids["audible"] != "B012345678" {
		t.Fatalf("provider ids = %#v", ids)
	}
	if ids[capabilityID] != "B012345678" || ids["asin"] != "B012345678" {
		t.Fatalf("capability/asin ids = %#v", ids)
	}
}

func TestMetadataItemFromMatchMapsAudiobookFields(t *testing.T) {
	item, err := metadataItemFromMatch(metadata.Match{
		Provider:       "audible",
		ProviderID:     "B012345678",
		Title:          "The Name of the Wind",
		Authors:        []string{"Patrick Rothfuss"},
		Narrators:      []string{"Nick Podehl"},
		Description:    "A book summary",
		Publisher:      "DAW",
		PublishYear:    2007,
		ASIN:           "B012345678",
		Genres:         []string{"Fantasy"},
		CoverURL:       "https://example.test/cover.jpg",
		DurationMin:    1650,
		SeriesName:     "The Kingkiller Chronicle",
		SeriesPosition: "1",
	}, "audiobook")
	if err != nil {
		t.Fatalf("metadataItemFromMatch() error = %v", err)
	}
	if item.GetProviderId() != "B012345678" || item.GetItemType() != "audiobook" {
		t.Fatalf("item ids/type = %q/%q", item.GetProviderId(), item.GetItemType())
	}
	if item.GetPosterPath() != "https://example.test/cover.jpg" {
		t.Fatalf("PosterPath = %q", item.GetPosterPath())
	}
	if item.GetRuntime() != 1650 {
		t.Fatalf("Runtime = %d, want 1650", item.GetRuntime())
	}
	if len(item.GetStudios()) != 1 || item.GetStudios()[0] != "DAW" {
		t.Fatalf("Studios = %#v", item.GetStudios())
	}
	if len(item.GetPeople()) != 2 {
		t.Fatalf("people = %#v", item.GetPeople())
	}
	if item.GetPeople()[0].GetKind() != "author" || item.GetPeople()[1].GetKind() != "narrator" {
		t.Fatalf("people kinds = %#v", item.GetPeople())
	}
	if item.GetMetadata().AsMap()["series_name"] != "The Kingkiller Chronicle" {
		t.Fatalf("metadata = %#v", item.GetMetadata().AsMap())
	}
}
