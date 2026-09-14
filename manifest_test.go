package main

import (
	"encoding/json"
	"testing"

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
