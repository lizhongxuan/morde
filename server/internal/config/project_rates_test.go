package config

import (
	"strings"
	"testing"
)

func TestProjectModelRatesLoadAndValidate(t *testing.T) {
	yaml := strings.Replace(validConfigYAML(t.TempDir()), "models:\n  allowed_hosts: [api.openai.com]\n", "models:\n  allowed_hosts: [api.openai.com]\n  project_rates:\n    - model_id: p1234567\n      provider: operator\n      model: model-x\n      currency: USD\n      version: 3\n      input_micros_per_million: 100\n      output_micros_per_million: 200\n", 1)
	cfg, err := parseConfig(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models.ProjectRates) != 1 || cfg.Models.ProjectRates[0].Snapshot().ModelID != "p1234567" {
		t.Fatalf("loaded project rates = %#v", cfg.Models.ProjectRates)
	}
	cfg.Models.ProjectRates = append(cfg.Models.ProjectRates, cfg.Models.ProjectRates[0])
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "duplicate model_id") {
		t.Fatalf("duplicate project rates error = %v", err)
	}
}
