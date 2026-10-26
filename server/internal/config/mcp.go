package config

import "fmt"

type MCP struct {
	Enabled            bool `yaml:"enabled"`
	MaxOutputBytes     int  `yaml:"max_output_bytes"`
	ScreenshotMaxWidth int  `yaml:"screenshot_max_width"`
}

func (m *MCP) setDefaults() {
	*m = MCP{Enabled: true, MaxOutputBytes: 65536, ScreenshotMaxWidth: 1280}
}

func (m MCP) validate() error {
	if m.MaxOutputBytes <= 0 {
		return fmt.Errorf("mcp.max_output_bytes must be positive")
	}
	if m.ScreenshotMaxWidth <= 0 {
		return fmt.Errorf("mcp.screenshot_max_width must be positive")
	}
	return nil
}
