package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskFileFetchTransferResumeContentLifecycleAndCancel(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Synthetic PC','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert target installation: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "file-fetch-transfer", "read one local report")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "file-fetch-transfer-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: found=%t task=%+v err=%v", found, claimed, err)
	}
	revision := int64(1)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=$3,desired_revision=$3,status='running'
WHERE id=$1 AND owner_id=$2`, claimed.ID, owner.ID, revision); err != nil {
		t.Fatalf("set current Task revision: %v", err)
	}

	newFetch := func(operationKey, relativePath string) (app.TaskHubOperation, app.TaskFileFetch, []byte) {
		t.Helper()
		operation := app.TaskHubOperation{
			OwnerID: owner.ID, TaskID: claimed.ID, Key: operationKey, ToolName: "morde_hub_file_fetch_" + targetID + "_v5",
			TargetInstallationID: targetID, Action: hub.ActionFileFetch,
			TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 5,
			TaskRevision: revision, LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("file.fetch " + operationKey),
			Status: task.OperationPrepared,
		}
		prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare %s: replayed=%t err=%v", operationKey, replayed, err)
		}
		fetchID := mustID(t, domain.NewOperationID)
		fetch := app.TaskFileFetch{
			ID: fetchID, OwnerID: owner.ID, TaskID: claimed.ID, OperationKey: prepared.Key,
			SourceInstallationID: targetID, TargetGrantVersion: prepared.TargetGrantVersion,
			TaskAuthorizationVersion: prepared.TaskAuthorizationVersion, TaskRevision: revision,
			RelativePath: relativePath, Status: task.FileFetchQueued,
		}
		fetch, replayed, err = store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: fetch}, claimed.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("create %s: replayed=%t err=%v", operationKey, replayed, err)
		}
		ticketHash := sha256.Sum256([]byte("synthetic-ticket-" + operationKey))
		expires := time.Now().UTC().Add(app.TaskFileFetchTicketLifetime)
		dispatched, err := store.DispatchTaskFileFetch(ctx, prepared, fetch.ID, ticketHash[:], &expires, true, claimed.Lease.Owner)
		if err != nil || !dispatched {
			t.Fatalf("dispatch %s: dispatched=%t err=%v", operationKey, dispatched, err)
		}
		return prepared, fetch, ticketHash[:]
	}
	manifest := func(data []byte) app.TaskFileFetchManifestRequest {
		version := sha256.Sum256([]byte("synthetic source version"))
		digest := sha256.Sum256(data)
		return app.TaskFileFetchManifestRequest{
			SourceVersion: hex.EncodeToString(version[:]), SourceSHA256: hex.EncodeToString(digest[:]),
			SizeBytes: int64(len(data)), MediaType: "text/plain", SourceDirectoryName: "Synthetic reports",
		}
	}
	chunkDigest := func(data []byte) string {
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:])
	}
	putAll := func(fetchID string, ticketHash []byte, startSequence int32, data []byte, size int64) {
		t.Helper()
		for sequence, offset := startSequence, int64(0); offset < int64(len(data)); sequence, offset = sequence+1, offset+size {
			end := offset + size
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			chunk := data[offset:end]
			if _, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, fetchID, ticketHash, sequence, chunkDigest(chunk), chunk); err != nil {
				t.Fatalf("put chunk %d: %v", sequence, err)
			}
		}
	}

	// A 600 KiB synthetic report crosses the first 512 KiB boundary.
	data := bytes.Repeat([]byte("synthetic local report\n"), 27_307)
	operation, fetch, ticketHash := newFetch("agent:file-fetch-main", "reports/summary.txt")
	request := manifest(data)
	manifestReceipt, err := store.AcceptTaskFileFetchManifest(ctx, claimed.ID, fetch.ID, ticketHash, request)
	if err != nil || manifestReceipt.Status != string(task.FileFetchTransferring) || manifestReceipt.ConfirmedSequences == nil || len(manifestReceipt.ConfirmedSequences) != 0 {
		t.Fatalf("pin manifest: receipt=%+v err=%v", manifestReceipt, err)
	}
	first := data[:app.TaskFileFetchChunkBytes]
	firstDigest := chunkDigest(first)
	chunkReceipt, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, fetch.ID, ticketHash, 0, firstDigest, first)
	if err != nil || chunkReceipt.ConfirmedBytes != int64(len(first)) || chunkReceipt.Replayed {
		t.Fatalf("store first chunk: receipt=%+v err=%v", chunkReceipt, err)
	}
	replayedChunk, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, fetch.ID, ticketHash, 0, firstDigest, first)
	if err != nil || !replayedChunk.Replayed || replayedChunk.ConfirmedBytes != int64(len(first)) {
		t.Fatalf("replay first chunk: receipt=%+v err=%v", replayedChunk, err)
	}
	changed := bytes.Clone(first)
	changed[0] ^= 0xff
	if _, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, fetch.ID, ticketHash, 0, chunkDigest(changed), changed); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed confirmed chunk err=%v; want conflict", err)
	}
	if err := store.MarkTaskHubOperationUnknown(ctx, operation, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark file.fetch result unknown: %v", err)
	}
	resumed, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || !replayed || resumed.Status != task.OperationPrepared {
		t.Fatalf("prepare same-fetch retry: operation=%+v replayed=%t err=%v", resumed, replayed, err)
	}
	newTicketHash := sha256.Sum256([]byte("synthetic-refreshed-ticket"))
	newExpiry := time.Now().UTC().Add(app.TaskFileFetchTicketLifetime)
	dispatched, err := store.DispatchTaskFileFetch(ctx, resumed, fetch.ID, newTicketHash[:], &newExpiry, true, claimed.Lease.Owner)
	if err != nil || !dispatched {
		t.Fatalf("refresh ticket for same fetch: dispatched=%t err=%v", dispatched, err)
	}
	manifestReceipt, err = store.AcceptTaskFileFetchManifest(ctx, claimed.ID, fetch.ID, newTicketHash[:], request)
	if err != nil || len(manifestReceipt.ConfirmedSequences) != 1 || manifestReceipt.ConfirmedSequences[0] != 0 {
		t.Fatalf("resume manifest: receipt=%+v err=%v", manifestReceipt, err)
	}
	changedDirectoryName := request
	changedDirectoryName.SourceDirectoryName += " renamed"
	if _, err := store.AcceptTaskFileFetchManifest(ctx, claimed.ID, fetch.ID, newTicketHash[:], changedDirectoryName); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed source directory display name err=%v; want conflict", err)
	}
	putAll(fetch.ID, newTicketHash[:], 1, data[app.TaskFileFetchChunkBytes:], app.TaskFileFetchChunkBytes)
	completed, err := store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, fetch.ID)
	if err != nil || completed.Status != task.FileFetchVerifying || completed.ConfirmedBytes != int64(len(data)) ||
		completed.SourceDirectoryName != request.SourceDirectoryName {
		t.Fatalf("verified transfer state: fetch=%+v err=%v", completed, err)
	}
	commitRequest := app.TaskFileFetchCommitRequest{SourceVersion: request.SourceVersion, SourceSHA256: request.SourceSHA256, SizeBytes: request.SizeBytes}
	commit, err := store.CommitTaskFileFetch(ctx, claimed.ID, fetch.ID, newTicketHash[:], commitRequest, mustID(t, domain.NewOperationID))
	if err != nil || commit.Status != string(task.FileFetchCompleted) || commit.FetchID != fetch.ID || commit.TaskRevision != revision ||
		commit.TargetGrantVersion != 5 || !domain.ValidOperationID(commit.ContentID) || !commit.ExpiresAt.After(time.Now()) {
		t.Fatalf("commit file fetch: receipt=%+v err=%v", commit, err)
	}
	completed, err = store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, fetch.ID)
	if err != nil || completed.ContentState != task.FileFetchContentRetained || completed.MediaType != "text/plain" ||
		completed.SourceDirectoryName != request.SourceDirectoryName {
		t.Fatalf("retained content projection: fetch=%+v err=%v", completed, err)
	}
	parsed, err := store.SetTaskFileFetchParseResult(ctx, owner.ID, claimed.ID, fetch.ID, task.FileFetchParseExtracted, "")
	if err != nil || parsed.ParseStatus != task.FileFetchParseExtracted || parsed.ParseError != "" {
		t.Fatalf("save completed transfer parse state: fetch=%+v err=%v", parsed, err)
	}
	parsedReplay, err := store.SetTaskFileFetchParseResult(ctx, owner.ID, claimed.ID, fetch.ID, task.FileFetchParseExtracted, "")
	if err != nil || parsedReplay.ParseStatus != task.FileFetchParseExtracted {
		t.Fatalf("idempotently recover parse state: fetch=%+v err=%v", parsedReplay, err)
	}
	if _, err := store.SetTaskFileFetchParseResult(ctx, owner.ID, claimed.ID, fetch.ID, task.FileFetchParseFailed, "invalid_document"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("conflicting final parse state err=%v; want conflict", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
		t.Fatalf("revoke source installation after commit: %v", err)
	}
	body, _, err := store.OpenTaskFileFetchContent(ctx, owner.ID, claimed.ID, fetch.ID)
	if err != nil {
		t.Fatalf("read committed content after source revocation: %v", err)
	}
	actual, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("read retained bytes: equal=%t len=%d err=%v", bytes.Equal(actual, data), len(actual), err)
	}
	deleted, err := store.DeleteTaskFileFetchContent(ctx, owner.ID, claimed.ID, fetch.ID)
	if err != nil || deleted.ContentState != task.FileFetchContentDeleted || deleted.DeletedAt == nil {
		t.Fatalf("delete retained content: fetch=%+v err=%v", deleted, err)
	}
	deletedAgain, err := store.DeleteTaskFileFetchContent(ctx, owner.ID, claimed.ID, fetch.ID)
	if err != nil || deletedAgain.ContentState != task.FileFetchContentDeleted || deletedAgain.DeletedAt == nil || !deletedAgain.DeletedAt.Equal(*deleted.DeletedAt) {
		t.Fatalf("repeat content delete: fetch=%+v err=%v", deletedAgain, err)
	}
	if _, _, err := store.OpenTaskFileFetchContent(ctx, owner.ID, claimed.ID, fetch.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("read deleted content err=%v; want not_found", err)
	}
	items, err := store.ListTaskFileFetches(ctx, owner.ID, claimed.ID)
	if err != nil || len(items) != 1 || items[0].ContentState != task.FileFetchContentDeleted || items[0].DeletedAt == nil ||
		items[0].ParseStatus != task.FileFetchParseExtracted {
		t.Fatalf("list deleted fetch tombstone: items=%+v err=%v", items, err)
	}

	// Cancellation removes in-flight data and revokes the PC ticket.
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=NULL WHERE id=$1`, targetID); err != nil {
		t.Fatalf("restore synthetic target: %v", err)
	}
	_, expiryFetch, expiryTicketHash := newFetch("agent:file-fetch-expired", "reports/expired.txt")
	expiryData := []byte("expire me")
	expiryManifest := manifest(expiryData)
	if _, err := store.AcceptTaskFileFetchManifest(ctx, claimed.ID, expiryFetch.ID, expiryTicketHash, expiryManifest); err != nil {
		t.Fatalf("pin expiry manifest: %v", err)
	}
	if _, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, expiryFetch.ID, expiryTicketHash, 0, chunkDigest(expiryData), expiryData); err != nil {
		t.Fatalf("store expiry chunk: %v", err)
	}
	if _, err := store.CommitTaskFileFetch(ctx, claimed.ID, expiryFetch.ID, expiryTicketHash,
		app.TaskFileFetchCommitRequest{SourceVersion: expiryManifest.SourceVersion, SourceSHA256: expiryManifest.SourceSHA256, SizeBytes: expiryManifest.SizeBytes},
		mustID(t, domain.NewOperationID)); err != nil {
		t.Fatalf("commit expiry fetch: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_file_fetches SET expires_at=clock_timestamp()-interval '1 second' WHERE fetch_id=$1`, expiryFetch.ID); err != nil {
		t.Fatalf("expire synthetic retained copy: %v", err)
	}
	cleaned, err := store.CleanupExpiredTaskFileFetches(ctx, time.Now().UTC().Add(time.Second), app.TaskFileFetchCleanupBatch)
	if err != nil || cleaned < 1 {
		t.Fatalf("clean expired retained copy: cleaned=%d err=%v", cleaned, err)
	}
	expired, err := store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, expiryFetch.ID)
	if err != nil || expired.ContentState != task.FileFetchContentExpired || expired.DeletedAt != nil {
		t.Fatalf("expired content projection: fetch=%+v err=%v", expired, err)
	}
	_, cancelFetch, cancelTicketHash := newFetch("agent:file-fetch-cancel", "reports/cancel.txt")
	cancelData := []byte("cancel me")
	cancelManifest := manifest(cancelData)
	if _, err := store.AcceptTaskFileFetchManifest(ctx, claimed.ID, cancelFetch.ID, cancelTicketHash, cancelManifest); err != nil {
		t.Fatalf("pin cancellation manifest: %v", err)
	}
	if _, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, cancelFetch.ID, cancelTicketHash, 0, chunkDigest(cancelData), cancelData); err != nil {
		t.Fatalf("store cancellation chunk: %v", err)
	}
	cancelled, err := store.CancelTaskFileFetch(ctx, owner.ID, claimed.ID, cancelFetch.ID)
	if err != nil || cancelled.Status != task.FileFetchCancelled || cancelled.ConfirmedBytes != 0 {
		t.Fatalf("cancel transfer: fetch=%+v err=%v", cancelled, err)
	}
	var chunkCount int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_file_fetch_chunks WHERE fetch_id=$1`, cancelFetch.ID).Scan(&chunkCount); err != nil || chunkCount != 0 {
		t.Fatalf("cancelled staging chunks: count=%d err=%v", chunkCount, err)
	}
	var storedTicket []byte
	if err := store.pool.QueryRow(ctx, `SELECT ticket_hash FROM task_file_fetches WHERE fetch_id=$1`, cancelFetch.ID).Scan(&storedTicket); err != nil || len(storedTicket) != 0 {
		t.Fatalf("cancelled ticket digest remains: len=%d err=%v", len(storedTicket), err)
	}

	// A source change after partial transfer fails the owner projection and drops staged bytes.
	failureOperation, failureFetch, failureTicketHash := newFetch("agent:file-fetch-failed", "reports/changed.txt")
	failureData := []byte("partial bytes")
	failureManifest := manifest(failureData)
	if _, err := store.AcceptTaskFileFetchManifest(ctx, claimed.ID, failureFetch.ID, failureTicketHash, failureManifest); err != nil {
		t.Fatalf("pin failed fetch manifest: %v", err)
	}
	if _, err := store.PutTaskFileFetchChunk(ctx, claimed.ID, failureFetch.ID, failureTicketHash, 0,
		chunkDigest(failureData), failureData); err != nil {
		t.Fatalf("store failed fetch partial chunk: %v", err)
	}
	if err := store.FailTaskFileFetch(ctx, failureOperation, failureFetch.ID, task.FileFetchFailureSourceChanged); err != nil {
		t.Fatalf("fail changed-source transfer: %v", err)
	}
	failed, err := store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, failureFetch.ID)
	if err != nil || failed.Status != task.FileFetchFailed || failed.FailureReason != task.FileFetchFailureSourceChanged || failed.ConfirmedBytes != 0 {
		t.Fatalf("failed transfer projection = %+v err=%v", failed, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_file_fetch_chunks WHERE fetch_id=$1`, failureFetch.ID).Scan(&chunkCount); err != nil || chunkCount != 0 {
		t.Fatalf("failed staging chunks: count=%d err=%v", chunkCount, err)
	}
}
