package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMCPDefaults(t *testing.T) {
	var m MCP
	m.setDefaults()
	if !m.Enabled || m.MaxOutputBytes != 65536 || m.ScreenshotMaxWidth != 1280 {
		t.Fatalf("unexpected defaults: %+v", m)
	}
}

func TestMCPValidate(t *testing.T) {
	for name, m := range map[string]MCP{
		"valid disabled": {Enabled: false, MaxOutputBytes: 1024, ScreenshotMaxWidth: 640},
		"zero output":    {Enabled: true, MaxOutputBytes: 0, ScreenshotMaxWidth: 640},
		"zero width":     {Enabled: true, MaxOutputBytes: 1024, ScreenshotMaxWidth: 0},
	} {
		t.Run(name, func(t *testing.T) {
			err := m.validate()
			wantErr := name != "valid disabled"
			if (err != nil) != wantErr {
				t.Fatalf("validate() error = %v, want error %v", err, wantErr)
			}
		})
	}
}

func TestMCPYAMLDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want MCP
	}{
		{name: "omitted", yaml: "{}\n", want: MCP{Enabled: true, MaxOutputBytes: 65536, ScreenshotMaxWidth: 1280}},
		{name: "explicit values", yaml: "mcp:\n  enabled: false\n  max_output_bytes: 4096\n  screenshot_max_width: 640\n", want: MCP{Enabled: false, MaxOutputBytes: 4096, ScreenshotMaxWidth: 640}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg struct {
				MCP MCP `yaml:"mcp"`
			}
			cfg.MCP.setDefaults()
			decoder := yaml.NewDecoder(strings.NewReader(tt.yaml))
			decoder.KnownFields(true)
			if err := decoder.Decode(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.MCP != tt.want {
				t.Fatalf("MCP = %+v, want %+v", cfg.MCP, tt.want)
			}
		})
	}
}
