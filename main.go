package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/metadata"
	"github.com/Silo-Server/silo-plugin-audiobook-metadata/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

const capabilityID = "audiobook-metadata"

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer

	manifest *pluginv1.PluginManifest
	provider *provider.Provider
}

type metadataServer struct {
	pluginv1.UnimplementedMetadataProviderServer
	runtime *runtimeServer
}

//go:embed manifest.json
var manifestJSON []byte

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure applies the "sources" and "limits" global config. A missing or
// partial entry leaves the defaults in place for the keys it does not name.
func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	s.provider.SetSources(sourceConfigFromEntries(req.GetConfig()))
	s.provider.Audnexus.SetRequestsPerMinute(audnexusPerMinuteFromEntries(req.GetConfig()))
	return &pluginv1.ConfigureResponse{}, nil
}

// audnexusPerMinuteFromEntries reads audnexus_per_minute from the "limits"
// config entry, or the default when it is absent or not a whole number of at
// least one. The client keeps it inside the range it supports.
func audnexusPerMinuteFromEntries(entries []*pluginv1.ConfigEntry) int {
	perMinute := provider.DefaultAudnexusPerMinute
	for _, entry := range entries {
		if entry == nil || entry.GetKey() != "limits" || entry.GetValue() == nil {
			continue
		}
		if v, ok := entry.GetValue().AsMap()["audnexus_per_minute"].(float64); ok && v >= 1 && v == math.Trunc(v) {
			perMinute = int(min(v, provider.MaxAudnexusPerMinute))
		}
	}
	return perMinute
}

// sourceConfigFromEntries reads the "sources" config entry over the defaults.
func sourceConfigFromEntries(entries []*pluginv1.ConfigEntry) provider.SourceConfig {
	cfg := provider.DefaultSourceConfig()
	for _, entry := range entries {
		if entry == nil || entry.GetKey() != "sources" || entry.GetValue() == nil {
			continue
		}
		values := entry.GetValue().AsMap()
		set := func(key string, target *bool) {
			if v, ok := values[key].(bool); ok {
				*target = v
			}
		}
		set("audnexus", &cfg.Audnexus)
		set("audimeta", &cfg.AudiMeta)
		set("itunes", &cfg.ITunes)
		set("audible", &cfg.Audible)
		set("storytel", &cfg.Storytel)
		set("bookbeat", &cfg.BookBeat)
		set("audioteka", &cfg.Audioteka)
		set("audiobookcovers", &cfg.AudiobookCovers)
	}
	return cfg
}

func (s *runtimeServer) providerForRequest() (*provider.Provider, error) {
	return s.provider, nil
}

func (s *metadataServer) Search(ctx context.Context, req *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	p, err := s.runtime.providerForRequest()
	if err != nil {
		return nil, err
	}

	results, err := p.Search(ctx, metadata.SearchQuery{
		Title:       req.GetQuery(),
		Year:        int(req.GetYear()),
		ContentType: req.GetItemType(),
		ProviderIDs: stringMapFromStruct(req.GetProviderIds()),
		Language:    req.GetLanguage(),
	})
	if err != nil {
		if done := callerDone(ctx); done != nil {
			return nil, done
		}
		return nil, searchFailureStatus(err)
	}

	response := &pluginv1.SearchMetadataResponse{
		Results: make([]*pluginv1.ProviderSearchResult, 0, len(results)),
	}
	for _, result := range results {
		searchResult, err := providerSearchResultFromMatch(result, req.GetItemType())
		if err != nil {
			return nil, err
		}
		if searchResult != nil {
			response.Results = append(response.Results, searchResult)
		}
	}
	return response, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	p, err := s.runtime.providerForRequest()
	if err != nil {
		return nil, err
	}

	result, err := p.Fetch(ctx, metadata.SearchQuery{
		ProviderIDs: providerIDsFromProto(req.GetProviderIds(), capabilityID, req.GetProviderId()),
		ContentType: req.GetItemType(),
		Language:    req.GetLanguage(),
	})
	if err != nil {
		if done := callerDone(ctx); done != nil {
			return nil, done
		}
		return nil, fetchFailureStatus(err)
	}
	if result == nil {
		return nil, nil
	}

	item, err := metadataItemFromMatch(*result, req.GetItemType())
	if err != nil {
		return nil, err
	}
	return &pluginv1.GetMetadataResponse{Item: item}, nil
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}

	rs := &runtimeServer{
		manifest: manifest,
		provider: provider.NewProvider(),
	}

	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:          rs,
			MetadataProvider: &metadataServer{runtime: rs},
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}

	if version != "" {
		manifest.Version = version
	}

	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])

	return manifest, nil
}

func stringMapFromStruct(value *structpb.Struct) map[string]string {
	result := make(map[string]string)
	if value == nil {
		return result
	}
	for key, raw := range value.AsMap() {
		text, ok := raw.(string)
		if ok && text != "" {
			result[key] = text
		}
	}
	return result
}

func providerIDsFromProto(value *structpb.Struct, capabilityID string, fallbackID string) map[string]string {
	result := stringMapFromStruct(value)
	if fallbackID != "" && result[capabilityID] == "" {
		result[capabilityID] = fallbackID
	}
	return result
}

// searchFailureStatus gives an all-providers-failed error an explicit gRPC
// code, so the host classifies it by CODE rather than by pattern-matching the
// message.
//
// Without a code the error arrives as codes.Unknown, which is exactly the case
// where silo-server falls through to reading the text -- and a bare 401/403 in
// there is treated as permanent for the item and parks it for 30 days. Every
// failure here is an availability problem: the item was never judged, so it
// must stay retryable.
//
//	Unavailable       -> transient: the item is retried, other items go on
//	ResourceExhausted -> rate-limited: the host stops calling this plugin for
//	                     a while, so it is kept for a source that throttled us
func searchFailureStatus(err error) error {
	var failed *provider.ProvidersFailedError
	if !errors.As(err, &failed) {
		return err
	}
	code := codes.Unavailable
	if failed.RateLimited() {
		code = codes.ResourceExhausted
	}
	return status.Error(code, failed.Error())
}

// callerDone reports a request the host cancelled or let expire as Canceled
// or DeadlineExceeded. Every source fails once the request's context is done,
// and that must not read as the sources being down. A source running into the
// plugin's own per-source timeout is different: the request is still live,
// and that stays a source failure.
func callerDone(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

// fetchFailureStatus does the same for a record fetch. Without a code the
// failure reaches the host as codes.Unknown and is classified by its text.
func fetchFailureStatus(err error) error {
	var failed *provider.FetchFailedError
	if !errors.As(err, &failed) {
		return err
	}
	code := codes.Unavailable
	if failed.Kind == provider.FailureRateLimited {
		code = codes.ResourceExhausted
	}
	return status.Error(code, failed.Error())
}

func providerSearchResultFromMatch(match metadata.Match, itemType string) (*pluginv1.ProviderSearchResult, error) {
	providerID := primaryProviderID(match)
	if providerID == "" {
		return nil, nil
	}

	providerIDs, err := stringStruct(providerIDsFromMatch(match))
	if err != nil {
		return nil, err
	}

	return &pluginv1.ProviderSearchResult{
		ProviderId:    providerID,
		ItemType:      itemType,
		Title:         match.Title,
		OriginalTitle: match.Title,
		Year:          int32(match.PublishYear),
		Overview:      match.Description,
		ProviderIds:   providerIDs,
		ImageUrl:      match.CoverURL,
	}, nil
}

func metadataItemFromMatch(match metadata.Match, itemType string) (*pluginv1.MetadataItem, error) {
	providerID := primaryProviderID(match)
	providerIDs, err := stringStruct(providerIDsFromMatch(match))
	if err != nil {
		return nil, err
	}

	return &pluginv1.MetadataItem{
		ProviderId:    providerID,
		ItemType:      itemType,
		Title:         match.Title,
		OriginalTitle: match.Title,
		Year:          int32(match.PublishYear),
		Overview:      match.Description,
		Genres:        append([]string(nil), match.Genres...),
		ProviderIds:   providerIDs,
		Metadata:      metadataStruct(match),
		Runtime:       int32(match.DurationMin),
		Studios:       publisherStudio(match.Publisher),
		PosterPath:    match.CoverURL,
		People:        peopleFromMatch(match),
	}, nil
}

func providerIDsFromMatch(match metadata.Match) map[string]string {
	ids := make(map[string]string)
	providerID := primaryProviderID(match)
	provider := strings.TrimSpace(match.Provider)

	if provider != "" && providerID != "" {
		ids[provider] = providerID
	}
	if providerID != "" {
		ids[capabilityID] = providerID
	}
	if asin := strings.TrimSpace(match.ASIN); asin != "" {
		ids["asin"] = asin
	}
	if isbn := strings.TrimSpace(match.ISBN); isbn != "" {
		ids["isbn"] = isbn
	}

	return ids
}

func primaryProviderID(match metadata.Match) string {
	if id := strings.TrimSpace(match.ProviderID); id != "" {
		return id
	}
	if id := strings.TrimSpace(match.ASIN); id != "" {
		return id
	}
	return strings.TrimSpace(match.ISBN)
}

func stringStruct(value map[string]string) (*structpb.Struct, error) {
	converted := make(map[string]any, len(value))
	for key, entry := range value {
		key = strings.TrimSpace(key)
		entry = strings.TrimSpace(entry)
		if key == "" || entry == "" {
			continue
		}
		converted[key] = entry
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(converted)
}

func metadataStruct(match metadata.Match) *structpb.Struct {
	values := map[string]string{
		"subtitle":        match.Subtitle,
		"isbn":            match.ISBN,
		"asin":            match.ASIN,
		"publisher":       match.Publisher,
		"language":        match.Language,
		"series_name":     match.SeriesName,
		"series_position": match.SeriesPosition,
	}

	converted := make(map[string]any, len(values)+1)
	for key, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			converted[key] = value
		}
	}
	if match.DurationMin > 0 {
		converted["duration_min"] = match.DurationMin
	}
	if len(converted) == 0 {
		return nil
	}
	result, err := structpb.NewStruct(converted)
	if err != nil {
		return nil
	}
	return result
}

func publisherStudio(publisher string) []string {
	publisher = strings.TrimSpace(publisher)
	if publisher == "" {
		return nil
	}
	return []string{publisher}
}

func peopleFromMatch(match metadata.Match) []*pluginv1.PersonRecord {
	people := make([]*pluginv1.PersonRecord, 0, len(match.Authors)+len(match.Narrators))
	for _, name := range match.Authors {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		people = append(people, &pluginv1.PersonRecord{
			Name:      name,
			Kind:      "author",
			SortOrder: int32(len(people)),
		})
	}
	for _, name := range match.Narrators {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		people = append(people, &pluginv1.PersonRecord{
			Name:      name,
			Kind:      "narrator",
			SortOrder: int32(len(people)),
		})
	}
	if len(people) == 0 {
		return nil
	}
	return people
}
