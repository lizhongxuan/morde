package app

import (
	"context"
	"errors"
	"fmt"
	"math"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type HostResourceSnapshot struct {
	HostID      string
	State       host.State
	Resources   HostResources
	Version     int64
	UpdateState string
}

type SandboxResourceStatus struct {
	Resources   HostResources
	ContainerID string
	StartedAt   string
}

type SandboxResourceClient interface {
	ReadResources(context.Context, string) (SandboxResourceStatus, error)
	UpdateResources(context.Context, string, SandboxResourceStatus, HostResources) (SandboxResourceStatus, error)
}

type OnlineHostResourceUpdate struct {
	HostID            string
	RuntimeProfile    string
	SandboxID         string
	RuntimeInstanceID string
	RuntimeGeneration int64
	Old               HostResources
	OldRuntime        HostResources
	Target            HostResources
	ReservedVersion   int64
	ActorID           string
	ActorKind         audit.ActorKind
}

type UpdateStoppedHostResourcesParams struct {
	HostID          string
	ExpectedVersion int64
	Resources       HostResources
	Capacity        Capacity
	ActorID         string
	ActorKind       audit.ActorKind
}

type HostResourceStore interface {
	GetHostResources(context.Context, string) (HostResourceSnapshot, error)
	UpdateStoppedHostResources(context.Context, UpdateStoppedHostResourcesParams) (HostResourceSnapshot, error)
	BeginOnlineHostResources(context.Context, UpdateStoppedHostResourcesParams) (OnlineHostResourceUpdate, error)
	CompleteOnlineHostResources(context.Context, OnlineHostResourceUpdate) (HostResourceSnapshot, error)
	AbortOnlineHostResources(context.Context, OnlineHostResourceUpdate, bool) error
}

type AdminHostResourcesService struct {
	store    HostResourceStore
	capacity Capacity
	router   SandboxRouter
}

func NewAdminHostResourcesService(store HostResourceStore, capacity Capacity, router SandboxRouter) (*AdminHostResourcesService, error) {
	if store == nil || router == nil || capacity.CPUMilli <= 0 || capacity.MemMiB <= 0 {
		return nil, errors.New("admin host resource store, capacity and sandbox router are required")
	}
	return &AdminHostResourcesService{store: store, capacity: capacity, router: router}, nil
}

func (s *AdminHostResourcesService) Get(ctx context.Context, principal identity.Principal, hostID string) (HostResourceSnapshot, error) {
	if err := authorizeAdmin(principal); err != nil {
		return HostResourceSnapshot{}, err
	}
	if !domain.ValidHostID(hostID) {
		return HostResourceSnapshot{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return s.store.GetHostResources(ctx, hostID)
}

func (s *AdminHostResourcesService) Update(ctx context.Context, principal identity.Principal, hostID string, resources HostResources, expectedVersion int64) (HostResourceSnapshot, error) {
	if err := authorizeAdmin(principal); err != nil {
		return HostResourceSnapshot{}, err
	}
	if !domain.ValidHostID(hostID) {
		return HostResourceSnapshot{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if expectedVersion < 1 || resources.CPUMilli < 1 || resources.MemMiB < 1 ||
		resources.CPUMilli > math.MaxInt32 || resources.MemMiB > math.MaxInt32 {
		return HostResourceSnapshot{}, domain.InvalidArgument("host.resources", "host resource values or expected version are invalid")
	}
	params := UpdateStoppedHostResourcesParams{
		HostID: hostID, ExpectedVersion: expectedVersion, Resources: resources,
		Capacity: s.capacity, ActorID: adminActorID(principal), ActorKind: adminActorKind(principal),
	}
	result, err := s.store.UpdateStoppedHostResources(ctx, params)
	if domain.CodeOf(err) != domain.CodeUnsupported {
		return result, err
	}
	update, err := s.store.BeginOnlineHostResources(ctx, params)
	if err != nil {
		return HostResourceSnapshot{}, err
	}
	client, err := s.router.For(update.RuntimeProfile)
	if err != nil {
		if abortErr := s.store.AbortOnlineHostResources(ctx, update, false); abortErr != nil {
			return HostResourceSnapshot{}, abortErr
		}
		return HostResourceSnapshot{}, domain.NewError(domain.CodeUnsupported, "online host resource adjustment is unavailable")
	}
	resourcesClient, ok := client.(SandboxResourceClient)
	if !ok {
		if abortErr := s.store.AbortOnlineHostResources(ctx, update, false); abortErr != nil {
			return HostResourceSnapshot{}, abortErr
		}
		return HostResourceSnapshot{}, domain.NewError(domain.CodeUnsupported, "online host resource adjustment is unavailable")
	}
	before, err := resourcesClient.ReadResources(ctx, update.SandboxID)
	if err != nil {
		if abortErr := s.store.AbortOnlineHostResources(ctx, update, false); abortErr != nil {
			return HostResourceSnapshot{}, abortErr
		}
		return HostResourceSnapshot{}, fmt.Errorf("read running host resources: %w", err)
	}
	if before.Resources != update.OldRuntime {
		_ = s.store.AbortOnlineHostResources(ctx, update, true)
		return HostResourceSnapshot{}, domain.NewError(domain.CodeConflict, "running resources differ from recorded resources")
	}
	_, updateErr := resourcesClient.UpdateResources(ctx, update.SandboxID, before, update.Target)
	after, readErr := resourcesClient.ReadResources(ctx, update.SandboxID)
	if updateErr == nil && readErr == nil && sameResourceProcess(before, after) && after.Resources == update.Target {
		result, err = s.store.CompleteOnlineHostResources(ctx, update)
		if err == nil {
			return result, nil
		}
		updateErr = err
	}
	// A failed call may have applied one limit. Release the reservation only after
	// the original values have been restored and verified on the same process.
	if readErr == nil && sameResourceProcess(before, after) {
		if after.Resources != update.OldRuntime {
			_, _ = resourcesClient.UpdateResources(ctx, update.SandboxID, after, update.OldRuntime)
			after, readErr = resourcesClient.ReadResources(ctx, update.SandboxID)
		}
		if readErr == nil && sameResourceProcess(before, after) && after.Resources == update.OldRuntime {
			if abortErr := s.store.AbortOnlineHostResources(ctx, update, false); abortErr != nil {
				return HostResourceSnapshot{}, abortErr
			}
			if updateErr != nil {
				return HostResourceSnapshot{}, fmt.Errorf("adjust running host resources: %w", updateErr)
			}
			return HostResourceSnapshot{}, domain.NewError(domain.CodeConflict, "running resource change could not be confirmed")
		}
	}
	if abortErr := s.store.AbortOnlineHostResources(ctx, update, true); abortErr != nil {
		return HostResourceSnapshot{}, abortErr
	}
	return HostResourceSnapshot{}, domain.NewError(domain.CodeUpstreamError, "running resource result is unknown; capacity remains reserved")
}

func sameResourceProcess(a, b SandboxResourceStatus) bool {
	return a.ContainerID != "" && a.StartedAt != "" && a.ContainerID == b.ContainerID && a.StartedAt == b.StartedAt
}
