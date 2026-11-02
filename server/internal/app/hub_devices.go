package app

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const maxDeviceDisplayNameBytes = 100

type DeviceInstallationStore interface {
	GetClientInstallation(context.Context, string) (identity.ClientInstallation, error)
	RenameClientInstallation(context.Context, string, string, string) error
}

type DeviceInfo struct {
	ID          string
	DisplayName string
	Platform    string
	ExpiresAt   time.Time
	Revoked     bool
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	Online      bool
}

type DeviceService struct {
	clients *ClientLoginService
	store   DeviceInstallationStore
	hub     HubDeviceConnections
}

func NewDeviceService(clients *ClientLoginService, store DeviceInstallationStore, hub HubDeviceConnections) (*DeviceService, error) {
	if clients == nil || store == nil || hub == nil {
		return nil, domain.InvalidArgument("dependencies", "device service dependencies are required")
	}
	return &DeviceService{clients: clients, store: store, hub: hub}, nil
}

func (s *DeviceService) List(ctx context.Context, principal identity.Principal) ([]DeviceInfo, error) {
	items, err := s.clients.List(ctx, principal)
	if err != nil {
		return nil, err
	}
	devices := make([]DeviceInfo, 0, len(items))
	for _, item := range items {
		devices = append(devices, DeviceInfo{
			ID: item.ID, DisplayName: item.DisplayName, Platform: item.Platform,
			ExpiresAt: item.ExpiresAt, Revoked: item.Revoked, CreatedAt: item.CreatedAt,
			LastUsedAt: item.LastUsedAt, Online: s.hub.InstallationOnline(principal.UserID, item.ID),
		})
	}
	return devices, nil
}

func (s *DeviceService) Rename(ctx context.Context, principal identity.Principal, id, displayName string) error {
	if err := validateClientAccountPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidClientInstallationID(id) {
		return domain.InvalidArgument("id", "invalid device id")
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || !utf8.ValidString(displayName) || len(displayName) > maxDeviceDisplayNameBytes {
		return domain.InvalidArgument("display_name", "invalid device name")
	}
	installation, err := s.store.GetClientInstallation(ctx, id)
	if err != nil {
		return err
	}
	if installation.OwnerID != principal.UserID || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return domain.NewError(domain.CodeNotFound, "device not found")
	}
	if err := s.store.RenameClientInstallation(ctx, principal.UserID, id, displayName); err != nil {
		return err
	}
	s.hub.RenameInstallation(id, displayName)
	return nil
}

func (s *DeviceService) Revoke(ctx context.Context, principal identity.Principal, id, ip string) error {
	err := s.clients.Revoke(ctx, principal, id, ip)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeUpstreamError {
			return err
		}
		// ClientLogin may report a sync-reconciliation error after its revocation transaction committed.
		installation, checkErr := s.store.GetClientInstallation(ctx, id)
		if checkErr != nil || installation.OwnerID != principal.UserID || installation.RevokedAt == nil {
			return err
		}
	}
	s.hub.CloseInstallation(id)
	return err
}
