package config

import (
	"reflect"
	"testing"
)

func TestConfigDoesNotExposeFusionMode(t *testing.T) {
	if _, ok := reflect.TypeOf(Config{}).FieldByName("FusionMode"); ok {
		t.Fatal("Config should not expose fusion_mode; FusionGate always fuses when workers are configured")
	}
}

func TestParseIgnoresLegacyFusionMode(t *testing.T) {
	cfg, err := parse([]byte(`{
		"providers": [
			{"name":"reviewer","base_url":"http://example.test/v1","model_name":"model","api_key":"key"}
		],
		"groups": [
			{"name":"coding","reviewer":"reviewer","providers":["reviewer"]}
		],
		"fusion_mode": "direct"
	}`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(cfg.Groups) != 1 || len(cfg.Groups[0].Providers) != 1 {
		t.Fatalf("group providers = %#v, want one configured worker", cfg.Groups)
	}
}

func TestValidateRejectsGroupWithoutWorkers(t *testing.T) {
	cfg, err := parse([]byte(`{
		"providers": [
			{"name":"reviewer","base_url":"http://example.test/v1","model_name":"model","api_key":"key"}
		],
		"groups": [
			{"name":"coding","reviewer":"reviewer","providers":[]}
		]
	}`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	errs := cfg.Validate()
	if len(errs) == 0 {
		t.Fatal("Validate should reject groups without worker providers")
	}
}
