package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

const (
	hubDeviceTestOwnerID        = "u1234567"
	hubDeviceTestOtherOwnerID   = "u7654321"
	hubDeviceTestInstallationID = "i1234567"
)

type hubDeviceClientStore struct {
	ClientLoginStore
	installations   []identity.ClientInstallation
	getErr          error
	renameErr       error
	revokeErr       error
	revokeCommitted bool
	renamedOwner    string
	renamedID       string
	renamedName     string
	revokedID       string
	revokedOwner    string
}

func (s *hubDeviceClientStore) GetClientInstallation(_ context.Context, id string) (identity.ClientInstallation, error) {
	if s.getErr != nil {
		return identity.ClientInstallation{}, s.getErr
	}
	for _, installation := range s.installations {
		if installation.ID == id {
			return installation, nil
		}
	}
	return identity.ClientInstallation{}, domain.NewError(domain.CodeNotFound, "client installation not found")
}

func (s *hubDeviceClientStore) ListClientInstallations(context.Context, string) ([]identity.ClientInstallation, error) {
	return append([]identity.ClientInstallation(nil), s.installations...), nil
}

func (s *hubDeviceClientStore) RevokeClientInstallation(_ context.Context, id, ownerID, _ string, _ audit.ActorKind, _ func(context.Context) error) error {
	s.revokedID, s.revokedOwner = id, ownerID
	if s.revokeCommitted {
		now := time.Now()
		for i := range s.installations {
			if s.installations[i].ID == id && s.installations[i].OwnerID == ownerID {
				s.installations[i].RevokedAt = &now
			}
		}
	}
	return s.revokeErr
}

func (s *hubDeviceClientStore) RenameClientInstallation(_ context.Context, ownerID, id, name string) error {
	s.renamedOwner, s.renamedID, s.renamedName = ownerID, id, name
	return s.renameErr
}

type hubDeviceConnectionsStub struct {
	online      map[string]bool
	renameCalls [][2]string
	closed      []string
}

func (s *hubDeviceConnectionsStub) InstallationOnline(ownerID, id string) bool {
	return s.online[ownerID+":"+id]
}

func (s *hubDeviceConnectionsStub) RenameInstallation(id, name string) {
	s.renameCalls = append(s.renameCalls, [2]string{id, name})
}

func (s *hubDeviceConnectionsStub) CloseInstallation(id string) {
	s.closed = append(s.closed, id)
}

func hubDeviceFixture() (*DeviceService, *hubDeviceClientStore, *hubDeviceConnectionsStub) {
	store := &hubDeviceClientStore{installations: []identity.ClientInstallation{{
		ID: hubDeviceTestInstallationID, OwnerID: hubDeviceTestOwnerID, DisplayName: "Work Mac", Platform: "desktop",
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now().Add(-time.Hour),
	}}}
	connections := &hubDeviceConnectionsStub{online: map[string]bool{hubDeviceTestOwnerID + ":" + hubDeviceTestInstallationID: true}}
	clients := &ClientLoginService{store: store}
	service, err := NewDeviceService(clients, store, connections)
	if err != nil {
		panic(err)
	}
	return service, store, connections
}

func hubDeviceSession(ownerID string) identity.Principal {
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func TestDeviceListReusesInstallationsAndAddsPresence(t *testing.T) {
	service, _, _ := hubDeviceFixture()
	items, err := service.List(context.Background(), hubDeviceSession(hubDeviceTestOwnerID))
	if err != nil || len(items) != 1 || items[0].ID != hubDeviceTestInstallationID || items[0].Platform != "desktop" || !items[0].Online {
		t.Fatalf("List() = %#v, %v", items, err)
	}
}

func TestDeviceRenameRequiresOwnedActiveInstallationAndValidName(t *testing.T) {
	service, store, connections := hubDeviceFixture()
	if err := service.Rename(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "  Travel Mac  "); err != nil {
		t.Fatalf("Rename() = %v", err)
	}
	if store.renamedOwner != hubDeviceTestOwnerID || store.renamedID != hubDeviceTestInstallationID || store.renamedName != "Travel Mac" ||
		len(connections.renameCalls) != 1 || connections.renameCalls[0] != [2]string{hubDeviceTestInstallationID, "Travel Mac"} {
		t.Fatalf("rename store=%#v connection updates=%#v", store, connections.renameCalls)
	}

	for _, name := range []string{"", " \t ", string(make([]byte, 101))} {
		if err := service.Rename(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, name); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("Rename(name=%q) error = %v", name, err)
		}
	}
	if len(connections.renameCalls) != 1 {
		t.Fatalf("invalid name updated Hub connection: %#v", connections.renameCalls)
	}

	store.installations[0].OwnerID = hubDeviceTestOtherOwnerID
	if err := service.Rename(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "Other"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner Rename() error = %v", err)
	}
	if store.renamedName != "Travel Mac" || len(connections.renameCalls) != 1 {
		t.Fatal("cross-owner rename had a side effect")
	}
}

func TestDeviceRenameReturnsStoreErrorsWithoutUpdatingConnection(t *testing.T) {
	service, store, connections := hubDeviceFixture()
	store.renameErr = errors.New("database unavailable")
	if err := service.Rename(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "Travel Mac"); err == nil {
		t.Fatal("Rename() swallowed store error")
	}
	if len(connections.renameCalls) != 0 {
		t.Fatalf("Hub connection updated after store failure: %#v", connections.renameCalls)
	}
}

func TestDeviceRevokeUsesClientLoginAndClosesConnectionOnlyAfterCommit(t *testing.T) {
	service, store, connections := hubDeviceFixture()
	store.revokeErr = errors.New("transaction failed")
	if err := service.Revoke(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "127.0.0.1"); err == nil {
		t.Fatal("Revoke() swallowed client store error")
	}
	if len(connections.closed) != 0 {
		t.Fatalf("Hub connection closed before successful revocation: %#v", connections.closed)
	}
	store.revokeErr = domain.NewError(domain.CodeUpstreamError, "workspace sync needs reconciliation")
	store.revokeCommitted = true
	if err := service.Revoke(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "127.0.0.1"); domain.CodeOf(err) != domain.CodeUpstreamError {
		t.Fatalf("post-commit Revoke() error = %v", err)
	}
	if len(connections.closed) != 1 {
		t.Fatalf("Hub connection remained open after committed revocation: %#v", connections.closed)
	}
	store.revokeCommitted = false
	store.revokeErr = nil
	if err := service.Revoke(context.Background(), hubDeviceSession(hubDeviceTestOwnerID), hubDeviceTestInstallationID, "127.0.0.1"); err != nil {
		t.Fatalf("Revoke() = %v", err)
	}
	if store.revokedOwner != hubDeviceTestOwnerID || store.revokedID != hubDeviceTestInstallationID || len(connections.closed) != 2 || connections.closed[1] != hubDeviceTestInstallationID {
		t.Fatalf("revoke store=%#v connections=%#v", store, connections.closed)
	}
}
