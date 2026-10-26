package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/runtimeclient"
)

func newApplicationInstallHost(t *testing.T, store *Store) app.ApplicationInstall {
	t.Helper()
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 200_000, 400_000)
	value := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if err := store.SetRuntimeInstanceID(ctx, value.ID, value.RuntimeGeneration, value.ControlVersion, "", "runtime-install-test"); err != nil {
		t.Fatal(err)
	}
	return app.ApplicationInstall{
		HostID: value.ID, OwnerID: owner.ID, RequestID: "install-recovery-01", ApplicationID: app.WechatApplicationID,
		RuntimeGeneration: value.RuntimeGeneration, RuntimeInstanceID: "runtime-install-test",
		ControlVersion: value.ControlVersion, Status: app.ApplicationInstallUnknown,
	}
}

func TestApplicationInstallPersistsAndBlocksHostReleaseUntilTerminal(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	request := newApplicationInstallHost(t, store)
	reserved, created, err := store.ReserveApplicationInstall(ctx, request)
	if err != nil || !created || reserved.Status != app.ApplicationInstallUnknown {
		t.Fatalf("reserve application install = %#v, %t, %v", reserved, created, err)
	}
	// A fresh Store represents a Server restart after an ambiguous Runtime POST.
	restarted := New(pool)
	pending, err := restarted.GetPendingApplicationInstall(ctx, request.HostID, request.OwnerID)
	if err != nil || pending.RequestID != request.RequestID || pending.RuntimeGeneration != request.RuntimeGeneration {
		t.Fatalf("recover pending request = %#v, %v", pending, err)
	}
	if _, changed, err := restarted.BeginHostRelease(ctx, request.HostID, true); domain.CodeOf(err) != domain.CodeConflict || changed {
		t.Fatalf("release before confirmation = changed %t, err %v", changed, err)
	}
	if _, err := restarted.TransitionHost(ctx, request.HostID, host.StateRunning, host.StateDeleting, ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delete before confirmation = %v", err)
	}
	if _, created, err := restarted.ReserveApplicationInstall(ctx, request); err != nil || created {
		t.Fatalf("same request retry = created %t, err %v", created, err)
	}
	other := request
	other.RequestID = "install-other-02"
	if _, created, err := restarted.ReserveApplicationInstall(ctx, other); domain.CodeOf(err) != domain.CodeConflict || created {
		t.Fatalf("different request while unresolved = created %t, err %v", created, err)
	}
	createdAt := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	accepted := runtimeclient.ApplicationInstallOperation{
		ID: "app_1234", ApplicationID: app.WechatApplicationID, Status: runtimeclient.OperationAccepted,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if _, err := restarted.RecordApplicationInstallOperation(ctx, request.HostID, request.OwnerID, other.RequestID, accepted); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("mismatched request ID accepted as recovery: %v", err)
	}
	if _, changed, err := restarted.BeginHostRelease(ctx, request.HostID, true); domain.CodeOf(err) != domain.CodeConflict || changed {
		t.Fatalf("release after mismatched request ID = changed %t, err %v", changed, err)
	}
	if _, err := restarted.RecordApplicationInstallOperation(ctx, request.HostID, request.OwnerID, request.RequestID, accepted); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := restarted.BeginHostRelease(ctx, request.HostID, true); domain.CodeOf(err) != domain.CodeConflict || changed {
		t.Fatalf("release while install accepted = changed %t, err %v", changed, err)
	}
	wrong := accepted
	wrong.ID = "app_other"
	if _, err := restarted.RecordApplicationInstallOperation(ctx, request.HostID, request.OwnerID, request.RequestID, wrong); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("mismatched operation ID = %v", err)
	}
	failed := accepted
	failed.Status = runtimeclient.OperationFailed
	failed.UpdatedAt = createdAt.Add(time.Second)
	failed.ErrorCode = "storage_error"
	terminal, err := restarted.RecordApplicationInstallOperation(ctx, request.HostID, request.OwnerID, request.RequestID, failed)
	if err != nil || !terminal.Terminal() {
		t.Fatalf("record terminal install = %#v, %v", terminal, err)
	}
	if _, err := restarted.GetPendingApplicationInstall(ctx, request.HostID, request.OwnerID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("pending after terminal confirmation = %v", err)
	}
	if _, changed, err := restarted.BeginHostRelease(ctx, request.HostID, true); err != nil || !changed {
		t.Fatalf("release after terminal confirmation = changed %t, err %v", changed, err)
	}
}

func TestApplicationInstallReservationSerializesWithHostRelease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	request := newApplicationInstallHost(t, store)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var created, changed bool
	var reserveErr, releaseErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, created, reserveErr = store.ReserveApplicationInstall(ctx, request)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, changed, releaseErr = store.BeginHostRelease(ctx, request.HostID, false)
	}()
	close(start)
	wg.Wait()
	if created {
		if reserveErr != nil || changed || domain.CodeOf(releaseErr) != domain.CodeConflict {
			t.Fatalf("release crossed install reservation: reserve=%v, release=%v, changed=%t", reserveErr, releaseErr, changed)
		}
		return
	}
	if reserveErr == nil || !changed || releaseErr != nil {
		t.Fatalf("install reservation crossed release: reserve=%v, release=%v, changed=%t", reserveErr, releaseErr, changed)
	}
}
