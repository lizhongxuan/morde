package config

import (
	"fmt"
	"os"
	"time"
)

type Gateway struct {
	WorkbenchDir       string        `yaml:"workbench_dir"`
	UpstreamCacheTTL   time.Duration `yaml:"upstream_cache_ttl"`
	CredentialCacheTTL time.Duration `yaml:"credential_cache_ttl"`
	PreviewPortMin     int           `yaml:"preview_port_min"`
	PreviewPortDeny    []int         `yaml:"preview_port_deny"`
	MaxBodyBytes       int64         `yaml:"max_body_bytes"`
}

func (g Gateway) validate() error {
	if g.WorkbenchDir == "" {
		return fmt.Errorf("gateway.workbench_dir is required")
	}
	info, err := os.Stat(g.WorkbenchDir)
	if err != nil {
		return fmt.Errorf("gateway.workbench_dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gateway.workbench_dir must be a directory")
	}
	if g.PreviewPortMin < 1 || g.PreviewPortMin > 65535 {
		return fmt.Errorf("gateway.preview_port_min must be between 1 and 65535")
	}
	seenPorts := make(map[int]struct{}, len(g.PreviewPortDeny))
	for _, port := range g.PreviewPortDeny {
		if port < 1 || port > 65535 {
			return fmt.Errorf("gateway.preview_port_deny contains a port outside 1..65535")
		}
		if _, exists := seenPorts[port]; exists {
			return fmt.Errorf("gateway.preview_port_deny contains duplicate port %d", port)
		}
		seenPorts[port] = struct{}{}
	}
	if g.MaxBodyBytes <= 0 {
		return fmt.Errorf("gateway.max_body_bytes must be greater than zero")
	}
	return nil
}
