package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestTaskCancelDiscardsApprovedMediaAndClosesPendingPublish(t *testing.T) {
	for _, receiptState := range []string{"none", "pending", "succeeded"} {
		name := receiptState + "-receipt"
		t.Run(name, func(t *testing.T) {
			store, _ := newTemporaryStore(t)
			ctx := context.Background()
			hostValue, workspaceValue, _, submitted, _ := newTaskWorkspacePublishTask(t, ctx, store, "media-cancel-"+name)
			if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='approval_required', lease_owner='', lease_expires_at=NULL WHERE id=$1`, submitted.ID); err != nil {
				t.Fatal(err)
			}
			pendingID := mustID(t, domain.NewPendingRequestID)
			operationID := mustID(t, domain.NewOperationID)
			callID := "media-cancel-call-" + name
			requestHash := sha256.Sum256([]byte("media-cancel:" + name))
			seedTaskMediaOperation(t, ctx, store, hostValue, submitted.ID, submitted.OwnerID, submitted.Lease.Version,
				operationID, callID, requestHash[:], "provider-op-cancel")
			if _, err := store.pool.Exec(ctx, `INSERT INTO task_pending_requests (
  id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash,
  runtime_generation, runtime_instance_id, control_version, status
) VALUES ($1,$2,1,'approval','Save generated media','Workspace output','A generated image is ready',
  'The file will be written to your Workspace',$3,'media.generate',$4,$5,$6,$7,'pending')`,
				pendingID, submitted.ID, callID, requestHash[:], hostValue.RuntimeGeneration, hostValue.RuntimeInstanceID, hostValue.ControlVersion); err != nil {
				t.Fatal(err)
			}
			content := []byte("approved media bytes")
			contentHash := sha256.Sum256(content)
			if err := store.CreateTaskMediaPreviewForApproval(ctx, app.CreateTaskMediaStagedObjectCommand{
				TaskID: submitted.ID, OwnerID: submitted.OwnerID, OperationID: operationID, RuntimeCallID: callID,
				PendingRequestID: pendingID, PendingRequestVersion: 1, MediaType: "image/png", SizeBytes: int64(len(content)),
				SHA256: contentHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour),
			}); err != nil {
				t.Fatalf("create Task media preview: %v", err)
			}
			if err := store.RespondToPendingRequest(ctx, app.IdempotencyRecord{
				OwnerID: submitted.OwnerID, Operation: "task.pending.respond", Key: "approve-media-" + name,
				RequestHash: hashBytes("approve-media:" + name), ResourceID: submitted.ID, RelatedID: pendingID,
			}, submitted.ID, pendingID, 1, task.PendingDecisionApprove); err != nil {
				t.Fatalf("approve Task media: %v", err)
			}
			claimed, found, err := store.ClaimNextTask(ctx, "media-cancel-worker", time.Minute)
			if err != nil || !found || claimed.ID != submitted.ID {
				t.Fatalf("claim approved Task: %#v found=%t err=%v", claimed, found, err)
			}
			operationKey := "task-workspace-publish:s" + operationID[1:] + ":" + workspaceValue.ID
			if receiptState != "none" {
				if _, err := store.pool.Exec(ctx, `INSERT INTO task_workspace_publish_receipts
(task_id, owner_id, workspace_id, operation_key, request_hash, base_revision, state, manifest_sha256, published_revision)
VALUES ($1,$2,$3,$4,$5,$6::bigint,$7,CASE WHEN $7='succeeded' THEN repeat('0',64) ELSE NULL END,
  CASE WHEN $7='succeeded' THEN $6::bigint ELSE NULL END)`, claimed.ID, claimed.OwnerID, workspaceValue.ID, operationKey,
					hashBytes("pending-media-publish:"+name), *claimed.WorkspaceCheckpointRevision, receiptState); err != nil {
					t.Fatalf("seed pending media publish receipt: %v", err)
				}
			}
			cancelID := mustID(t, domain.NewTaskActionID)
			_, cancelErr := store.RequestTaskAction(ctx, taskActionIdempotency(claimed.OwnerID, task.ActionCancel,
				"cancel-media-"+name, claimed.ID, task.StatusRunning, cancelID), claimed.ID, task.ActionCancel,
				task.StatusRunning, nil, cancelID)
			if receiptState == "succeeded" {
				if domain.CodeOf(cancelErr) != domain.CodeConflict {
					t.Fatalf("cancel before Task media Artifact is recorded = %v, want conflict", cancelErr)
				}
				var taskStatus, mediaState string
				if err := store.pool.QueryRow(ctx, `SELECT t.status, m.state FROM tasks t JOIN task_media_staged_objects m ON m.task_id=t.id WHERE t.id=$1 AND m.operation_id=$2`, claimed.ID, operationID).Scan(&taskStatus, &mediaState); err != nil || taskStatus != string(task.StatusRunning) || mediaState != string(app.TaskMediaPreviewSaving) {
					t.Fatalf("Task/media after deferred cancellation = %q/%q, err=%v", taskStatus, mediaState, err)
				}
				return
			}
			if cancelErr != nil {
				t.Fatalf("cancel Task with approved media: %v", cancelErr)
			}
			var mediaState string
			if err := store.pool.QueryRow(ctx, `SELECT state FROM task_media_staged_objects WHERE task_id=$1 AND operation_id=$2`, claimed.ID, operationID).Scan(&mediaState); err != nil || mediaState != string(app.TaskMediaPreviewDiscard) {
				t.Fatalf("media state after cancellation = %q, err=%v", mediaState, err)
			}
			if receiptState == "pending" {
				var receiptState string
				if err := store.pool.QueryRow(ctx, `SELECT state FROM task_workspace_publish_receipts WHERE task_id=$1 AND operation_key=$2`, claimed.ID, operationKey).Scan(&receiptState); err != nil || receiptState != string(app.TaskWorkspacePublishConflict) {
					t.Fatalf("publish receipt state after cancellation = %q, err=%v", receiptState, err)
				}
			}
			objects, err := store.ListTaskMediaObjectsForCleanup(ctx, time.Now().UTC(), 10)
			if err != nil || len(objects) != 1 || objects[0].OperationID != operationID {
				t.Fatalf("cancelled media cleanup candidates = %#v, err=%v", objects, err)
			}
		})
	}
}

func TestExpiredTaskMediaPreviewRejectsAndRequeuesApproval(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	hostValue, _, _, submitted, _ := newTaskWorkspacePublishTask(t, ctx, store, "media-expire-approval")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='approval_required', lease_owner='', lease_expires_at=NULL WHERE id=$1`, submitted.ID); err != nil {
		t.Fatal(err)
	}
	pendingID := mustID(t, domain.NewPendingRequestID)
	operationID := mustID(t, domain.NewOperationID)
	callID := "media-expire-call-123"
	requestHash := sha256.Sum256([]byte("expired media approval"))
	seedTaskMediaOperation(t, ctx, store, hostValue, submitted.ID, submitted.OwnerID, submitted.Lease.Version,
		operationID, callID, requestHash[:], "provider-op-expired")
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_pending_requests (
  id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash,
  runtime_generation, runtime_instance_id, control_version, status
) VALUES ($1,$2,1,'approval','Save generated media','Workspace output','A generated image is ready',
  'The file will be written to your Workspace',$3,'media.generate',$4,$5,$6,$7,'pending')`,
		pendingID, submitted.ID, callID, requestHash[:], hostValue.RuntimeGeneration, hostValue.RuntimeInstanceID, hostValue.ControlVersion); err != nil {
		t.Fatal(err)
	}
	content := []byte("expired preview bytes")
	contentHash := sha256.Sum256(content)
	if err := store.CreateTaskMediaPreviewForApproval(ctx, app.CreateTaskMediaStagedObjectCommand{
		TaskID: submitted.ID, OwnerID: submitted.OwnerID, OperationID: operationID, RuntimeCallID: callID,
		PendingRequestID: pendingID, PendingRequestVersion: 1, MediaType: "image/png", SizeBytes: int64(len(content)),
		SHA256: contentHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create Task media preview: %v", err)
	}
	if _, err := store.SetTaskMediaStagedObjectState(ctx, submitted.ID, operationID, app.TaskMediaPreviewPending, app.TaskMediaPreviewExpired); err != nil {
		t.Fatalf("expire Task media preview: %v", err)
	}
	if err := store.ExpireTaskMediaPreviewApproval(ctx, submitted.ID, operationID); err != nil {
		t.Fatalf("expire linked approval: %v", err)
	}
	details, err := store.GetTask(ctx, submitted.ID, submitted.OwnerID)
	if err != nil || details.Task.Status != task.StatusQueued {
		t.Fatalf("Task after media approval expiry = %#v, err=%v", details.Task, err)
	}
	if details.PendingRequest == nil || details.PendingRequest.Status != task.PendingRequestRejected || details.PendingRequest.Decision != task.PendingDecisionReject {
		t.Fatalf("pending approval after media expiry = %#v", details.PendingRequest)
	}
	if err := store.RespondToPendingRequest(ctx, app.IdempotencyRecord{
		OwnerID: submitted.OwnerID, Operation: "task.pending.respond", Key: "approve-expired-media",
		RequestHash: hashBytes("approve-expired-media"), ResourceID: submitted.ID, RelatedID: pendingID,
	}, submitted.ID, pendingID, 1, task.PendingDecisionApprove); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("approval of expired media error = %v, want conflict", err)
	}
}

func TestTaskMediaCleanupWaitsForSaveStepResult(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	_, _, _, submitted, _ := newTaskWorkspacePublishTask(t, ctx, store, "media-cleanup-step")
	operationID := mustID(t, domain.NewOperationID)
	stepID := "s" + operationID[1:]
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps (id, task_id, sequence, kind, status, title, summary)
VALUES ($1,$2,1,'artifact','running','Save generated media','Saving approved media')`, stepID, submitted.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := store.CanCleanupTaskMediaStagedObject(ctx, submitted.ID, operationID)
	if err != nil || ready {
		t.Fatalf("cleanup readiness while Task media step runs = %t, err=%v", ready, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_steps SET status='failed' WHERE id=$1 AND task_id=$2`, stepID, submitted.ID); err != nil {
		t.Fatal(err)
	}
	ready, err = store.CanCleanupTaskMediaStagedObject(ctx, submitted.ID, operationID)
	if err != nil || !ready {
		t.Fatalf("cleanup readiness after failed Task media step = %t, err=%v", ready, err)
	}
}
