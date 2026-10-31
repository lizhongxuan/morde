package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/syncnodequota"
)

type workspaceSyncQuotaSocketServiceStub struct{}

func (workspaceSyncQuotaSocketServiceStub) Reserve(context.Context, string, syncnodequota.ReserveRequest) (syncnodequota.Receipt, error) {
	return syncnodequota.Receipt{}, nil
}

func (workspaceSyncQuotaSocketServiceStub) Settle(context.Context, string, string) (syncnodequota.Receipt, error) {
	return syncnodequota.Receipt{}, nil
}

func (workspaceSyncQuotaSocketServiceStub) Abort(_ context.Context, _ string, reservationID string) (syncnodequota.Receipt, error) {
	return syncnodequota.Receipt{ReservationID: reservationID, State: "aborted", BaseRevision: 0}, nil
}

func TestWorkspaceSyncQuotaSocketsCreatesOwnerPrivateEndpoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "quota-sockets")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &WorkspaceSyncNodeQuotaSockets{
		root: root, groupID: os.Getegid(), service: workspaceSyncQuotaSocketServiceStub{},
		ctx: ctx, cancel: cancel, owners: make(map[string]*syncnodequota.Endpoint),
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close quota socket manager: %v", err)
		}
	})

	if err := manager.EnsureOwner(context.Background(), "u1234567"); err != nil {
		t.Fatal(err)
	}
	ownerDir := filepath.Join(root, "")
	entries, err := os.ReadDir(ownerDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("quota root entries = %v, %v", entries, err)
	}
	socketPath := filepath.Join(ownerDir, entries[0].Name(), "quota.sock")
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o660 {
		t.Fatalf("owner socket = %v, %v", info, err)
	}
	if err := manager.EnsureOwner(context.Background(), "u1234567"); err != nil {
		t.Fatalf("idempotent EnsureOwner: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("closed endpoint still exists: %v", err)
	}
}
