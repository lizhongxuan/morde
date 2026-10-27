package postgres

import (
	"context"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

// Existing store integration fixtures do not construct a HostService.
type credentialDefaultHostNoop struct{}

func (credentialDefaultHostNoop) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return host.Host{}, false, nil
}
