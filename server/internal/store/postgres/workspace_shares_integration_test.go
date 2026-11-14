package postgres

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceShareClaimIsSingleUse(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	first := createTestUser(t, store, 3, 10_000, 20_000)
	second := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "workspace share source")
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("local-only share token"))
	link, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareEdit, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(time.Hour),
		State: workspace.SharePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if link.WorkspaceName != source.Name || len(link.TokenHash) != sha256.Size {
		t.Fatalf("created share = %#v", link)
	}

	start := make(chan struct{})
	results := make(chan struct {
		claimant string
		link     workspace.ShareLink
		err      error
	}, 2)
	var workers sync.WaitGroup
	for _, userID := range []string{first.ID, second.ID} {
		workers.Add(1)
		go func(userID string) {
			defer workers.Done()
			<-start
			claimed, claimErr := store.StartWorkspaceShareClaim(ctx, tokenHash[:], userID)
			results <- struct {
				claimant string
				link     workspace.ShareLink
				err      error
			}{claimant: userID, link: claimed, err: claimErr}
		}(userID)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner string
	for result := range results {
		if result.err == nil {
			if winner != "" || result.link.State != workspace.ShareClaiming || result.link.ClaimantID != result.claimant {
				t.Fatalf("unexpected concurrent claim success: %#v", result)
			}
			winner = result.claimant
		} else if domain.CodeOf(result.err) != domain.CodeNotFound {
			t.Fatalf("concurrent claim error = %v, want not found", result.err)
		}
	}
	if winner == "" {
		t.Fatal("neither recipient claimed the link")
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], winner); err != nil {
		t.Fatalf("claim retry for reserved recipient failed: %v", err)
	}
	target := createTestWorkspace(t, ctx, store, winner, "workspace share copy")
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_share_links SET target_workspace_id=$2 WHERE id=$1 AND state='claiming'`, shareID, target.ID); err != nil {
		t.Fatalf("set test recipient copy target: %v", err)
	}
	if err := store.CompleteWorkspaceShareClaim(ctx, shareID, winner, target.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.GetWorkspaceShareByToken(ctx, tokenHash[:])
	if err != nil || claimed.State != workspace.ShareClaimed || claimed.TargetWorkspaceID != target.ID || claimed.ClaimantID != winner {
		t.Fatalf("completed share = %#v, err=%v", claimed, err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], winner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("claim after successful acceptance error = %v, want not found", err)
	}
	var storedHash []byte
	if err := store.pool.QueryRow(ctx, `SELECT token_hash FROM workspace_share_links WHERE id=$1`, shareID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if len(storedHash) != sha256.Size || storedHash == nil {
		t.Fatalf("stored token hash length = %d", len(storedHash))
	}
}

func TestPostgresWorkspaceShareCopyIsRecipientOwnedAndRecoversReservation(t *testing.T) {
	for _, permission := range []workspace.SharePermission{workspace.ShareView, workspace.ShareEdit} {
		t.Run(string(permission), func(t *testing.T) {
			store, _ := newTemporaryStore(t)
			ctx := context.Background()
			owner := createTestUser(t, store, 3, 10_000, 20_000)
			recipient := createTestUser(t, store, 4, 10_000, 20_000)
			source := createTestWorkspace(t, ctx, store, owner.ID, "share source")
			if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, source.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, source.ID); err != nil {
				t.Fatal(err)
			}
			shareID, err := domain.NewPendingRequestID()
			if err != nil {
				t.Fatal(err)
			}
			tokenHash := sha256.Sum256([]byte("recipient owned " + string(permission)))
			share, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
				ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
				Permission: permission, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(7 * 24 * time.Hour), State: workspace.SharePending,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID)
			if err != nil || claimed.State != workspace.ShareClaiming || claimed.ClaimantID != recipient.ID {
				t.Fatalf("start Workspace share claim = %#v, err=%v", claimed, err)
			}
			targetID, err := domain.NewWorkspaceID()
			if err != nil {
				t.Fatal(err)
			}
			reservationID, err := domain.NewPendingRequestID()
			if err != nil {
				t.Fatal(err)
			}
			requestHash := sha256.Sum256([]byte("copy:" + shareID))
			record := app.IdempotencyRecord{OwnerID: recipient.ID, Operation: "workspace.share.copy", Key: "ws-share-copy-" + shareID,
				RequestHash: requestHash[:], ResourceID: source.ID}
			readOnly := permission == workspace.ShareView
			copyRequest := app.WorkspaceCopyReservationRequest{
				ID: reservationID, SourceOwnerID: owner.ID, TargetOwnerID: recipient.ID, ShareID: shareID,
				SourceWorkspaceID: source.ID, TargetWorkspaceID: targetID, TargetName: share.WorkspaceName,
				ServiceInstanceID: "share-copy-test", SourceRevision: source.Revision, ReservedBytes: 32,
				MaxOwnerBytes: 1 << 30, TargetQuotaBytes: 1 << 30, ReadOnly: readOnly, Idempotency: record,
			}
			reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, copyRequest)
			if err != nil || replayed || reservation.SourceOwnerID != owner.ID || reservation.TargetOwnerID != recipient.ID ||
				reservation.ShareID != shareID || reservation.ReadOnly != readOnly || reservation.State != workspace.CopyReserved {
				t.Fatalf("create recipient copy reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
			}
			volumeRequest := app.WorkspaceVolumeCopyRequest{
				ReservationID: reservation.ID, ServiceInstanceID: reservation.ServiceInstanceID,
				SourceOwnerID: owner.ID, TargetOwnerID: recipient.ID, SourceWorkspaceID: source.ID,
				TargetWorkspaceID: targetID, SourceRevision: source.Revision,
			}
			if err := store.CheckWorkspaceVolumeCopyReservation(ctx, volumeRequest); err != nil {
				t.Fatalf("check source/target owner copy fence: %v", err)
			}
			wrongOwnerRequest := volumeRequest
			wrongOwnerRequest.TargetOwnerID = owner.ID
			if err := store.CheckWorkspaceVolumeCopyReservation(ctx, wrongOwnerRequest); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
				t.Fatalf("copy fence with wrong target owner error = %v, want workspace busy", err)
			}
			var reserved int64
			if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, recipient.ID).Scan(&reserved); err != nil || reserved != 32 {
				t.Fatalf("recipient quota reservation = %d, err=%v", reserved, err)
			}
			var reservedSourceDuringCopy int64
			if err := store.pool.QueryRow(ctx, `SELECT COALESCE((SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1),0)`, owner.ID).Scan(&reservedSourceDuringCopy); err != nil || reservedSourceDuringCopy != 0 {
				t.Fatalf("source owner quota changed during copy reservation = %d, err=%v", reservedSourceDuringCopy, err)
			}
			if _, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, source.ID, app.WorkspaceFileOperationCommand{
				Action: app.WorkspaceFileRename, SourcePath: "before.txt", TargetPath: "after.txt", ExpectedCurrentRevision: source.Revision, WriteEnabled: true,
			}, syncTestIdempotency(owner.ID, "workspace.file_operation", "share-source-freeze", source.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
				t.Fatalf("source write while its share copy is reserved = %v, want workspace busy", err)
			}
			if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyReserved, workspace.Copying); err != nil {
				t.Fatal(err)
			}
			if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.Copying, workspace.CopyUnknown); err != nil {
				t.Fatal(err)
			}
			if err := store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, recipient.ID, reservation.ID); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
				t.Fatalf("unknown copy safe-release error = %v, want workspace conflict", err)
			}
			var retainedTarget string
			if err := store.pool.QueryRow(ctx, `SELECT COALESCE(target_workspace_id,'') FROM workspace_share_links WHERE id=$1`, shareID).
				Scan(&retainedTarget); err != nil || retainedTarget != targetID {
				t.Fatalf("unknown copy target fence = %q, err=%v", retainedTarget, err)
			}
			retry, found, err := store.GetWorkspaceCopyReservationByIdempotency(ctx, record, share.WorkspaceName, "share-copy-test", 1<<30)
			if err != nil || !found || retry.ID != reservation.ID || retry.TargetOwnerID != recipient.ID || retry.State != workspace.CopyUnknown {
				t.Fatalf("read recoverable recipient copy = %#v, found=%t, err=%v", retry, found, err)
			}
			reservation, replayed, err = store.CreateWorkspaceCopyReservation(ctx, copyRequest)
			if err != nil || !replayed || reservation.State != workspace.CopyUnknown || reservation.TargetWorkspaceID != targetID {
				t.Fatalf("retry recipient copy reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
			}
			if err := store.SetWorkspaceCopyReservationState(ctx, reservation.ID, workspace.CopyUnknown, workspace.Copying); err != nil {
				t.Fatal(err)
			}
			mode, modified := uint32(0o755), time.Now().UnixNano()
			root := []workspace.Directory{{Path: ".", Mode: &mode, ModTimeNS: &modified, MetadataKnown: true}}
			if err := store.StageWorkspaceCopy(ctx, reservation.ID, recipient.ID, share.WorkspaceName, []workspace.File{}, []string{}, root,
				[]app.WorkspaceBlockAllocation{}, 1<<30); err != nil {
				t.Fatalf("stage recipient Workspace copy: %v", err)
			}
			if err := store.CompleteWorkspaceCopy(ctx, reservation.ID, recipient.ID, 1<<30); err != nil {
				t.Fatalf("complete recipient Workspace copy: %v", err)
			}
			if err := store.CompleteWorkspaceShareClaim(ctx, shareID, recipient.ID, targetID); err != nil {
				t.Fatalf("complete Workspace share claim: %v", err)
			}

			var sourceOwner, sourceState string
			var sourceRevision int64
			if err := store.pool.QueryRow(ctx, `SELECT owner_id,state,revision FROM workspaces WHERE id=$1`, source.ID).
				Scan(&sourceOwner, &sourceState, &sourceRevision); err != nil || sourceOwner != owner.ID || sourceState != string(workspace.StateReady) || sourceRevision != source.Revision {
				t.Fatalf("source Workspace changed: owner=%q state=%q revision=%d err=%v", sourceOwner, sourceState, sourceRevision, err)
			}
			var targetOwner, targetState string
			var targetReadOnly bool
			if err := store.pool.QueryRow(ctx, `SELECT owner_id,state,is_read_only FROM workspaces WHERE id=$1`, targetID).
				Scan(&targetOwner, &targetState, &targetReadOnly); err != nil || targetOwner != recipient.ID || targetState != string(workspace.StateReady) || targetReadOnly != readOnly {
				t.Fatalf("recipient Workspace copy = owner=%q state=%q readOnly=%v err=%v", targetOwner, targetState, targetReadOnly, err)
			}
			var targetBindings int64
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_bindings WHERE workspace_id=$1`, targetID).Scan(&targetBindings); err != nil || targetBindings != 0 {
				t.Fatalf("recipient Workspace Host bindings = %d, err=%v", targetBindings, err)
			}
			var currentOwner string
			var transferID *string
			if err := store.pool.QueryRow(ctx, `SELECT owner_id,transfer_id FROM workspace_ownership_history WHERE workspace_id=$1 AND ended_at IS NULL`, targetID).
				Scan(&currentOwner, &transferID); err != nil || currentOwner != recipient.ID || transferID != nil {
				t.Fatalf("recipient ownership history = owner=%q transfer=%v err=%v", currentOwner, transferID, err)
			}
			var transfers, recipientReserved, sourceReserved int64
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_ownership_transfers WHERE workspace_id=$1`, targetID).Scan(&transfers); err != nil || transfers != 0 {
				t.Fatalf("recipient copy ownership transfers = %d, err=%v", transfers, err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, recipient.ID).Scan(&recipientReserved); err != nil || recipientReserved != 0 {
				t.Fatalf("recipient copy quota reservation after completion = %d, err=%v", recipientReserved, err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT COALESCE((SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1),0)`, owner.ID).Scan(&sourceReserved); err != nil || sourceReserved != 0 {
				t.Fatalf("source owner quota changed = %d, err=%v", sourceReserved, err)
			}
		})
	}
}

func TestPostgresWorkspaceShareCopySafeFailureReleasesTargetForSameRecipientRetry(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	recipient := createTestUser(t, store, 4, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "share source")
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("safe retry share"))
	share, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareEdit, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(7 * 24 * time.Hour), State: workspace.SharePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID); err != nil {
		t.Fatal(err)
	}
	firstTargetID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("safe retry:" + shareID))
	record := app.IdempotencyRecord{OwnerID: recipient.ID, Operation: "workspace.share.copy", Key: "ws-share-copy-" + shareID,
		RequestHash: requestHash[:], ResourceID: source.ID}
	request := app.WorkspaceCopyReservationRequest{
		ID: reservationID, SourceOwnerID: owner.ID, TargetOwnerID: recipient.ID, ShareID: shareID,
		SourceWorkspaceID: source.ID, TargetWorkspaceID: firstTargetID, TargetName: share.WorkspaceName,
		ServiceInstanceID: "share-safe-retry-test", SourceRevision: source.Revision, ReservedBytes: 32,
		MaxOwnerBytes: 1 << 30, TargetQuotaBytes: 1 << 30, Idempotency: record,
	}
	reservation, replayed, err := store.CreateWorkspaceCopyReservation(ctx, request)
	if err != nil || replayed || reservation.State != workspace.CopyReserved {
		t.Fatalf("create first safe-release reservation = %#v, replayed=%t, err=%v", reservation, replayed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='ready',ready_at=clock_timestamp() WHERE workspace_id=$1`, firstTargetID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, recipient.ID, reservation.ID); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("safe release with an already-ready target volume = %v, want workspace conflict", err)
	}
	var retainedTarget string
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(target_workspace_id,'') FROM workspace_share_links WHERE id=$1`, shareID).Scan(&retainedTarget); err != nil || retainedTarget != firstTargetID {
		t.Fatalf("target fence after unproven safe release = %q, err=%v", retainedTarget, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='pending',ready_at=NULL WHERE workspace_id=$1`, firstTargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=0 WHERE workspace_id=$1`, firstTargetID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, recipient.ID, reservation.ID); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("safe release after target materialization = %v, want workspace conflict", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=-1 WHERE workspace_id=$1`, firstTargetID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailWorkspaceCopyReservationBeforeSideEffects(ctx, recipient.ID, reservation.ID); err != nil {
		t.Fatalf("release copy before volume side effects: %v", err)
	}

	var claimantID, linkedTarget string
	var shareState string
	if err := store.pool.QueryRow(ctx, `SELECT state,COALESCE(claimant_id,''),COALESCE(target_workspace_id,'') FROM workspace_share_links WHERE id=$1`, shareID).
		Scan(&shareState, &claimantID, &linkedTarget); err != nil || shareState != string(workspace.ShareClaiming) || claimantID != recipient.ID || linkedTarget != "" {
		t.Fatalf("share claim after safe release = state=%q claimant=%q target=%q err=%v", shareState, claimantID, linkedTarget, err)
	}
	var targetState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspaces WHERE id=$1`, firstTargetID).Scan(&targetState); err != nil || targetState != string(workspace.StateDeleted) {
		t.Fatalf("unused copy target state = %q, err=%v", targetState, err)
	}
	var reservationCount, idempotencyCount, volumeCount, recipientReserved int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_copy_reservations WHERE id=$1`, reservation.ID).Scan(&reservationCount); err != nil || reservationCount != 0 {
		t.Fatalf("released copy reservation count = %d, err=%v", reservationCount, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`,
		recipient.ID, record.Operation, record.Key).Scan(&idempotencyCount); err != nil || idempotencyCount != 0 {
		t.Fatalf("released copy idempotency count = %d, err=%v", idempotencyCount, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_volumes WHERE workspace_id=$1`, firstTargetID).Scan(&volumeCount); err != nil || volumeCount != 0 {
		t.Fatalf("released copy volume count = %d, err=%v", volumeCount, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, recipient.ID).Scan(&recipientReserved); err != nil || recipientReserved != 0 {
		t.Fatalf("recipient reserved quota after safe release = %d, err=%v", recipientReserved, err)
	}

	secondTargetID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	secondReservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	request.ID, request.TargetWorkspaceID = secondReservationID, secondTargetID
	reservation, replayed, err = store.CreateWorkspaceCopyReservation(ctx, request)
	if err != nil || replayed || reservation.TargetWorkspaceID != secondTargetID || reservation.TargetOwnerID != recipient.ID {
		t.Fatalf("same-recipient safe retry = %#v, replayed=%t, err=%v", reservation, replayed, err)
	}
}

func TestPostgresWorkspaceShareExpiredUnclaimedLinkCannotBeClaimed(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	recipient := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "expired workspace share source")
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("local-only expired share token"))
	if _, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareView, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(-time.Minute),
		State: workspace.SharePending,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWorkspaceShareByToken(ctx, tokenHash[:]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired link preview error = %v, want not found", err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired link claim error = %v, want not found", err)
	}
}

func TestPostgresHostBoundWorkspaceCannotBeSharedOrCopiedAsWorkspace(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	recipient := createTestUser(t, store, 3, 10_000, 20_000)
	source := createTestWorkspace(t, ctx, store, owner.ID, "computer storage")
	shareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("local-only pre-host share token"))
	share, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: shareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareEdit, TokenHash: tokenHash[:], ExpiresAt: time.Now().Add(time.Hour), State: workspace.SharePending,
	})
	if err != nil {
		t.Fatal(err)
	}
	createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{source.ID})

	if _, err := store.GetWorkspaceShareByToken(ctx, tokenHash[:]); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("preview of pre-existing Host-bound share = %v, want not found", err)
	}
	if _, err := store.StartWorkspaceShareClaim(ctx, tokenHash[:], recipient.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("claim of pre-existing Host-bound share = %v, want not found", err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM workspace_share_links WHERE id=$1`, shareID).Scan(&state); err != nil || state != string(workspace.SharePending) {
		t.Fatalf("share state after rejected claim = %q, err=%v", state, err)
	}

	newShareID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	newTokenHash := sha256.Sum256([]byte("local-only post-host share token"))
	if _, err := store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: newShareID, OwnerID: owner.ID, SourceWorkspaceID: source.ID, SourceRevision: source.Revision,
		Permission: workspace.ShareEdit, TokenHash: newTokenHash[:], ExpiresAt: time.Now().Add(time.Hour), State: workspace.SharePending,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("create share for Host-bound computer storage = %v, want forbidden", err)
	}
	if matches, err := store.WorkspaceShareSourceMatches(ctx, owner.ID, source.ID, source.Revision); err != nil || matches {
		t.Fatalf("Host-bound share source matches = %t, err=%v; want false", matches, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE workspace_share_links SET state='claiming',claimant_id=$2 WHERE id=$1`, shareID, recipient.ID); err != nil {
		t.Fatal(err)
	}
	targetID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("host-bound share copy"))
	_, _, err = store.CreateWorkspaceCopyReservation(ctx, app.WorkspaceCopyReservationRequest{
		ID: reservationID, SourceOwnerID: owner.ID, TargetOwnerID: recipient.ID, ShareID: share.ID,
		SourceWorkspaceID: source.ID, TargetWorkspaceID: targetID, TargetName: share.WorkspaceName,
		ServiceInstanceID: "host-bound-share-test", SourceRevision: source.Revision, ReservedBytes: 0,
		MaxOwnerBytes: 1 << 30, TargetQuotaBytes: 1 << 30, Idempotency: app.IdempotencyRecord{
			OwnerID: recipient.ID, Operation: "workspace.share.copy", Key: "host-bound-share-copy-01",
			RequestHash: requestHash[:], ResourceID: source.ID,
		},
	})
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("copy reservation for Host-bound computer storage = %v, want forbidden", err)
	}
}
