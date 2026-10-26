package template

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
)

type OnIdle string

const RuntimeBootstrapEntrypoint = "/usr/local/bin/morde-host-entrypoint"

const (
	OnIdleDisabled OnIdle = "disabled"
)

type Resource struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

type Resources struct {
	Requests Resource `yaml:"requests"`
	Limits   Resource `yaml:"limits"`
}

type Workspace struct {
	MountPath string `yaml:"mountPath"`
	Size      string `yaml:"size"`
}

type Lifecycle struct {
	TTL         time.Duration `yaml:"ttl"`
	IdleTimeout time.Duration `yaml:"idleTimeout"`
	OnIdle      OnIdle        `yaml:"onIdle"`
}

type Template struct {
	ID                   string    `yaml:"id"`
	Name                 string    `yaml:"name"`
	Description          string    `yaml:"description"`
	Image                string    `yaml:"image"`
	Entrypoint           []string  `yaml:"entrypoint"`
	Resources            Resources `yaml:"resources"`
	Workspace            Workspace `yaml:"workspace"`
	Lifecycle            Lifecycle `yaml:"lifecycle"`
	KeepWorkspaceDefault bool      `yaml:"keepWorkspaceDefault"`
	Isolation            string    `yaml:"isolation"`
}

func (t Template) Validate(profiles []string) error {
	if !domain.ValidTemplateID(t.ID) {
		return fmt.Errorf("template.id is invalid")
	}
	if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Image) == "" {
		return fmt.Errorf("template name and image are required")
	}
	if !pinnedImage(t.Image) {
		return fmt.Errorf("template.image must use a pinned sha256 digest")
	}
	if len(t.Entrypoint) == 0 {
		return fmt.Errorf("template.entrypoint is required")
	}
	for _, part := range t.Entrypoint {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("template.entrypoint contains an empty value")
		}
	}
	if len(t.Entrypoint) != 1 || t.Entrypoint[0] != RuntimeBootstrapEntrypoint {
		return fmt.Errorf("template.entrypoint must use the Morde runtime bootstrap")
	}
	requestCPU, err := CPUMilli(t.Resources.Requests.CPU)
	if err != nil {
		return fmt.Errorf("template.resources.requests.cpu: %w", err)
	}
	limitCPU, err := CPUMilli(t.Resources.Limits.CPU)
	if err != nil {
		return fmt.Errorf("template.resources.limits.cpu: %w", err)
	}
	requestMem, err := MemMiB(t.Resources.Requests.Memory)
	if err != nil {
		return fmt.Errorf("template.resources.requests.memory: %w", err)
	}
	limitMem, err := MemMiB(t.Resources.Limits.Memory)
	if err != nil {
		return fmt.Errorf("template.resources.limits.memory: %w", err)
	}
	if requestCPU > limitCPU || requestMem > limitMem {
		return fmt.Errorf("template resource requests exceed limits")
	}
	if strings.TrimSpace(t.Workspace.MountPath) == "" {
		return fmt.Errorf("template.workspace.mountPath is required")
	}
	if _, err := MemMiB(t.Workspace.Size); err != nil {
		return fmt.Errorf("template.workspace.size: %w", err)
	}
	if t.Lifecycle.TTL <= 0 || t.Lifecycle.IdleTimeout <= 0 {
		return fmt.Errorf("template lifecycle durations must be positive")
	}
	if t.Lifecycle.OnIdle != OnIdleDisabled {
		return fmt.Errorf("template.lifecycle.onIdle must be disabled until activity leases are available")
	}
	if !contains(profiles, t.Isolation) {
		return fmt.Errorf("template.isolation profile %q is not configured", t.Isolation)
	}
	return nil
}

func pinnedImage(image string) bool {
	const marker = "@sha256:"
	index := strings.LastIndex(image, marker)
	if index <= 0 || len(image)-index-len(marker) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(image[index+len(marker):])
	return err == nil && len(decoded) == 32
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
