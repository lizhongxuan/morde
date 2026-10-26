package app

import (
	"context"
	"errors"
	"time"
)

type SandboxState string

const (
	SandboxPending    SandboxState = "pending"
	SandboxRunning    SandboxState = "running"
	SandboxTerminated SandboxState = "terminated"
	SandboxFailed     SandboxState = "failed"
	SandboxUnknown    SandboxState = "unknown"

	RuntimeDataVolumeMountPath = "/var/lib/morde-runtime"
	RuntimeDataVolumeStorage   = "1Gi"
	ProfileVolumeMountPath     = "/home/gem"
	ProfileVolumeStorage       = "5Gi"
	HostAppsVolumeMountPath    = "/opt/morde/apps"
	HostAppsVolumeStorage      = "10Gi"
)

type SandboxResources struct {
	CPU    string
	Memory string
}

type SandboxVolume struct {
	Name      string
	MountPath string
	ClaimName string
	Storage   string
}

type SandboxCreateRequest struct {
	Image      string
	Entrypoint []string
	Timeout    time.Duration
	Requests   SandboxResources
	Limits     SandboxResources
	Env        map[string]string
	Metadata   map[string]string
	Volumes    []SandboxVolume
}

type Sandbox struct {
	ID        string
	State     SandboxState
	Reason    string
	Metadata  map[string]string
	ExpiresAt time.Time
	CreatedAt time.Time
}

type Upstream struct {
	BaseURL string
	Headers map[string]string
	APIKey  string
}

type SandboxClient interface {
	Create(ctx context.Context, req SandboxCreateRequest) (Sandbox, error)
	Get(ctx context.Context, id string) (Sandbox, error)
	List(ctx context.Context, metadata map[string]string) ([]Sandbox, error)
	Delete(ctx context.Context, id string) error
	Renew(ctx context.Context, id string, expiresAt time.Time) (time.Time, error)
	Endpoint(ctx context.Context, id string, port int) (Upstream, error)
}

type SandboxRouter interface {
	For(profile string) (SandboxClient, error)
	Profiles() []string
}

type HostProbe interface {
	Ready(ctx context.Context, u Upstream) error
}

var (
	ErrSandboxNotFound    = errors.New("sandbox not found")
	ErrSandboxConflict    = errors.New("sandbox state conflict")
	ErrSandboxUnsupported = errors.New("sandbox operation unsupported")
	ErrSandboxUnavailable = errors.New("sandbox service unavailable")
)
