package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Silo-Server/silo-plugin-audiobook-metadata/provider"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// The manifest's "sources" schema, its form, and the plugin's defaults must
// agree: every switch names a real source key, and the form defaults match
// DefaultSourceConfig so the admin page shows what an unconfigured plugin does.
func TestManifestSourcesSchemaMatchesDefaults(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	var schema *pluginv1.ConfigSchema
	for _, s := range manifest.GetGlobalConfigSchema() {
		if s.GetKey() == "sources" {
			schema = s
		}
	}
	if schema == nil {
		t.Fatal("manifest has no \"sources\" global config schema")
	}

	var jsonSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema.GetJsonSchema()), &jsonSchema); err != nil {
		t.Fatalf("parse json_schema: %v", err)
	}

	defaults := map[string]bool{}
	for _, field := range schema.GetAdminForm().GetFields() {
		if _, ok := jsonSchema.Properties[field.GetKey()]; !ok {
			t.Errorf("form field %q is not in json_schema properties", field.GetKey())
		}
		defaults[field.GetKey()] = field.GetDefaultValue().GetBoolValue()
	}
	if len(defaults) != len(jsonSchema.Properties) {
		t.Errorf("form has %d fields, schema has %d properties", len(defaults), len(jsonSchema.Properties))
	}

	want := map[string]bool{}
	cfg := sourceConfigFromEntries(nil)
	for key, v := range map[string]bool{"audnexus": cfg.Audnexus, "audimeta": cfg.AudiMeta, "itunes": cfg.ITunes, "audible": cfg.Audible, "storytel": cfg.Storytel, "bookbeat": cfg.BookBeat, "audioteka": cfg.Audioteka, "audiobookcovers": cfg.AudiobookCovers} {
		want[key] = v
	}
	for key, wantDefault := range want {
		if got, ok := defaults[key]; !ok {
			t.Errorf("source %q has no switch in the manifest form", key)
		} else if got != wantDefault {
			t.Errorf("manifest default for %q = %v, code default = %v", key, got, wantDefault)
		}
	}
}

// The manifest's "limits" form must show the rate an unconfigured plugin uses
// and accept no more than the client does.
func TestManifestLimitsSchemaMatchesDefaults(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, schema := range manifest.GetGlobalConfigSchema() {
		if schema.GetKey() != "limits" {
			continue
		}
		var jsonSchema struct {
			Properties map[string]struct {
				Minimum int `json:"minimum"`
				Maximum int `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema.GetJsonSchema()), &jsonSchema); err != nil {
			t.Fatalf("parse json_schema: %v", err)
		}
		if got := jsonSchema.Properties["audnexus_per_minute"]; got.Minimum != provider.MinAudnexusPerMinute || got.Maximum != provider.MaxAudnexusPerMinute {
			t.Errorf("schema range = %d to %d, client range = %d to %d", got.Minimum, got.Maximum, provider.MinAudnexusPerMinute, provider.MaxAudnexusPerMinute)
		}
		fields := schema.GetAdminForm().GetFields()
		if len(fields) != 1 || fields[0].GetKey() != "audnexus_per_minute" {
			t.Fatalf("limits form fields = %v, want audnexus_per_minute alone", fields)
		}
		if got := int(fields[0].GetDefaultValue().GetNumberValue()); got != audnexusPerMinuteFromEntries(nil) {
			t.Errorf("manifest default = %d, code default = %d", got, audnexusPerMinuteFromEntries(nil))
		}
		return
	}
	t.Fatal("manifest has no \"limits\" global config schema")
}

// Every store the "audible" form offers must be one the provider knows, or
// picking it would silently query the default store instead.
func TestManifestAudibleSchemaMatchesProvider(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, schema := range manifest.GetGlobalConfigSchema() {
		if schema.GetKey() != "audible" {
			continue
		}
		var jsonSchema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(schema.GetJsonSchema()), &jsonSchema); err != nil {
			t.Fatalf("parse json_schema: %v", err)
		}
		fields := schema.GetAdminForm().GetFields()
		if len(fields) != 1 || fields[0].GetKey() != "region" {
			t.Fatalf("audible form fields = %v, want region alone", fields)
		}
		var options []string
		for _, option := range fields[0].GetOptions() {
			options = append(options, option.GetValue())
		}
		if enum := jsonSchema.Properties["region"].Enum; !slices.Equal(options, enum) {
			t.Errorf("form options = %v, schema enum = %v", options, enum)
		}
		for _, code := range options {
			if !provider.IsAudibleRegion(code) {
				t.Errorf("form offers %q, which the provider does not know", code)
			}
		}
		if got := fields[0].GetDefaultValue().GetStringValue(); got != provider.DefaultAudibleRegion {
			t.Errorf("manifest default = %q, code default = %q", got, provider.DefaultAudibleRegion)
		}
		return
	}
	t.Fatal("manifest has no \"audible\" global config schema")
}

// The host asks GetMetadata for a book by the ASIN in its tags only when the
// capability declares that key; without it every tagged book goes through a
// search first.
func TestManifestDeclaresASINLookup(t *testing.T) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, capability := range manifest.GetCapabilities() {
		if capability.GetId() != capabilityID {
			continue
		}
		keys := capability.GetMetadata().GetFields()["lookup_provider_ids"].GetListValue().GetValues()
		if len(keys) != 1 || keys[0].GetStringValue() != "asin" {
			t.Fatalf("lookup_provider_ids = %v, want [asin]", keys)
		}
		return
	}
	t.Fatalf("manifest has no %q capability", capabilityID)
}
