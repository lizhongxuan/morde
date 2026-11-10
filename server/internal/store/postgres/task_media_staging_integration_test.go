package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/taskmedia"
)

func TestPostgresTaskMediaStagedObjectStorePreviewAndCleanup(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "media-task", "generate media"))
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "o1234567"
	const pendingRequestID = "r1234567"
	const runtimeCallID = "media-call-123"
	requestHash := sha256.Sum256([]byte("media request"))
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='approval_required' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, control_version, lease_version, status, external_id)
VALUES ($1,$2,'media-call',$3,$4,0,0,1,'succeeded','provider-op-1')`, operationID, submitted.Task.ID, requestHash[:], hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id, source, call_id, request_digest, model_id, provider_model, task_id, reserved_tokens, state, input_tokens, output_tokens, settled_at)
VALUES ($1,'task',$2,$3,'model-test','provider/image-1',$4,100,'settled',20,30,clock_timestamp())`, owner.ID, runtimeCallID, requestHash[:], submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash, runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,1,'approval','Save generated media','image','Review preview','Creates a Workspace artifact',$3,'image.generate',$4,0,'instance-1',0,'pending')`,
		pendingRequestID, submitted.Task.ID, runtimeCallID, requestHash[:]); err != nil {
		t.Fatal(err)
	}

	content := []byte("synthetic media bytes")
	contentHash := sha256.Sum256(content)
	command := app.CreateTaskMediaStagedObjectCommand{TaskID: submitted.Task.ID, OwnerID: owner.ID, OperationID: operationID,
		RuntimeCallID: runtimeCallID, PendingRequestID: pendingRequestID, PendingRequestVersion: 1,
		MediaType: "image/png", SizeBytes: int64(len(content)), SHA256: contentHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := store.CreateTaskMediaPreviewForApproval(ctx, command); err != nil {
		t.Fatalf("create staged preview: %v", err)
	}
	taskDetails, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || taskDetails.PendingRequest == nil || taskDetails.PendingRequest.MediaPreviewOperationID != operationID {
		t.Fatalf("Task approval preview operation link = %#v, err=%v", taskDetails.PendingRequest, err)
	}
	details, err := store.GetTaskMediaOperationDetails(ctx, submitted.Task.ID, operationID)
	if err != nil || details.OwnerID != owner.ID || details.OperationStatus != string(task.OperationSucceeded) ||
		details.TokenState != "settled" || details.InputTokens == nil || *details.InputTokens != 20 || details.OutputTokens == nil || *details.OutputTokens != 30 ||
		details.State != app.TaskMediaPreviewPending || details.CleanupState != app.TaskMediaCleanupPending {
		t.Fatalf("staged operation details = %#v, err=%v", details, err)
	}
	preview, err := store.GetTaskMediaPreviewForOwner(ctx, owner.ID, submitted.Task.ID, operationID, time.Now().UTC())
	if err != nil || preview.OperationID != operationID {
		t.Fatalf("owner preview lookup = %#v, err=%v", preview, err)
	}
	if _, err := store.GetTaskMediaPreviewForOwner(ctx, "u7654321", submitted.Task.ID, operationID, time.Now().UTC()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner's preview lookup error = %v", err)
	}

	responseHash := sha256.Sum256([]byte("reject media preview"))
	response := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.pending.respond", Key: "reject-media-preview",
		RequestHash: responseHash[:], ResourceID: submitted.Task.ID, RelatedID: pendingRequestID}
	if err := store.RespondToPendingRequest(ctx, response, submitted.Task.ID, pendingRequestID, 1, task.PendingDecisionReject); err != nil {
		t.Fatalf("reject media preview: %v", err)
	}
	details, err = store.GetTaskMediaOperationDetails(ctx, submitted.Task.ID, operationID)
	if err != nil || details.State != app.TaskMediaPreviewDiscard {
		t.Fatalf("staged object after rejection = %#v, err=%v", details, err)
	}
	cleanup, err := store.ListTaskMediaObjectsForCleanup(ctx, time.Now().UTC(), 10)
	if err != nil || len(cleanup) != 1 || cleanup[0].OperationID != operationID {
		t.Fatalf("cleanup list = %#v, err=%v", cleanup, err)
	}
	if err := store.MarkTaskMediaStagedObjectCleanupComplete(ctx, submitted.Task.ID, operationID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteCleanedTaskMediaStagedObject(ctx, submitted.Task.ID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTaskMediaOperationDetails(ctx, submitted.Task.ID, operationID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted staged object lookup error = %v", err)
	}
}

func TestPostgresTaskMediaPreviewServiceReadsVerifiedLocalContent(t *testing.T) {
	store, ctx, ownerID, taskID := createPostgresTaskMediaPreviewFixture(t, "verified-preview", "o6234567", "r6234567", "media-call-623")
	contentStore, err := taskmedia.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := contentStore.Close(); err != nil {
			t.Errorf("close task media content store: %v", err)
		}
	})
	want := []byte("synthetic media bytes")
	content, err := contentStore.Stage(ctx, ownerID, taskID, "o6234567", "image/png", bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	if content.SizeBytes != int64(len(want)) || content.MediaType != "image/png" {
		t.Fatalf("staged local content metadata = %#v", content)
	}
	service, err := app.NewTaskMediaPreviewService(store, contentStore)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	preview, err := service.OpenPreview(ctx, principal, taskID, "o6234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := preview.Body.Close(); err != nil {
			t.Errorf("close verified media preview: %v", err)
		}
	}()
	got, err := io.ReadAll(preview.Body)
	if err != nil || !bytes.Equal(got, want) || preview.MediaType != content.MediaType || preview.SizeBytes != content.SizeBytes {
		t.Fatalf("verified preview = %q/%s/%d, err=%v", got, preview.MediaType, preview.SizeBytes, err)
	}
}

func TestPostgresTaskMediaApprovalSavesPreviewAndRejectsExpiredPreview(t *testing.T) {
	for _, test := range []struct {
		name       string
		operation  string
		pending    string
		call       string
		expired    bool
		wantStatus task.PendingRequestStatus
		wantState  app.TaskMediaStagedObjectState
		wantError  domain.ErrorCode
	}{
		{name: "approve", operation: "o3234567", pending: "r3234567", call: "media-call-323", wantStatus: task.PendingRequestApproved, wantState: app.TaskMediaPreviewSaving},
		{name: "expired approval", operation: "o4234567", pending: "r4234567", call: "media-call-423", expired: true,
			wantStatus: task.PendingRequestPending, wantState: app.TaskMediaPreviewPending, wantError: domain.CodeConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, ctx, ownerID, taskID := createPostgresTaskMediaPreviewFixture(t, test.name, test.operation, test.pending, test.call)
			if test.expired {
				if _, err := store.pool.Exec(ctx, `UPDATE task_media_staged_objects SET expires_at=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND operation_id=$2`, taskID, test.operation); err != nil {
					t.Fatal(err)
				}
			}
			requestHash := sha256.Sum256([]byte("approve " + test.pending))
			record := app.IdempotencyRecord{OwnerID: ownerID, Operation: "task.pending.respond", Key: "approve-" + test.pending,
				RequestHash: requestHash[:], ResourceID: taskID, RelatedID: test.pending}
			err := store.RespondToPendingRequest(ctx, record, taskID, test.pending, 1, task.PendingDecisionApprove)
			if (test.wantError == "" && err != nil) || (test.wantError != "" && domain.CodeOf(err) != test.wantError) {
				t.Fatalf("approval error = %v, want code %q", err, test.wantError)
			}
			details, err := store.GetTaskMediaOperationDetails(ctx, taskID, test.operation)
			if err != nil || details.State != test.wantState {
				t.Fatalf("staged preview after approval = %#v, err=%v", details, err)
			}
			taskDetails, err := store.GetTask(ctx, taskID, ownerID)
			if err != nil || taskDetails.PendingRequest == nil || taskDetails.PendingRequest.Status != test.wantStatus {
				t.Fatalf("pending request after approval = %#v, err=%v", taskDetails.PendingRequest, err)
			}
		})
	}
}

func TestPostgresTaskCancellationDiscardsPendingMediaPreview(t *testing.T) {
	store, ctx, ownerID, taskID := createPostgresTaskMediaPreviewFixture(t, "cancel", "o5234567", "r5234567", "media-call-523")
	actionID := "x5234567"
	record := taskActionIdempotency(ownerID, task.ActionCancel, "cancel-media-preview", taskID, task.StatusWaitingUser, actionID)
	if _, err := store.RequestTaskAction(ctx, record, taskID, task.ActionCancel, task.StatusWaitingUser, nil, actionID); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTaskMediaOperationDetails(ctx, taskID, "o5234567")
	if err != nil || details.State != app.TaskMediaPreviewDiscard {
		t.Fatalf("staged preview after Task cancellation = %#v, err=%v", details, err)
	}
	taskDetails, err := store.GetTask(ctx, taskID, ownerID)
	if err != nil || taskDetails.PendingRequest == nil || taskDetails.PendingRequest.Status != task.PendingRequestCancelled {
		t.Fatalf("pending request after Task cancellation = %#v, err=%v", taskDetails.PendingRequest, err)
	}
}

func createPostgresTaskMediaPreviewFixture(t *testing.T, name, operationID, pendingID, runtimeCallID string) (*Store, context.Context, string, string) {
	t.Helper()
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "media-"+name, "generate media"))
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("media request " + name))
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='approval_required' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, control_version, lease_version, status)
VALUES ($1,$2,'media-call',$3,$4,0,0,1,'succeeded')`, operationID, submitted.Task.ID, requestHash[:], hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id, source, call_id, request_digest, model_id, provider_model, task_id, reserved_tokens, state, input_tokens, output_tokens, settled_at)
VALUES ($1,'task',$2,$3,'model-test','provider/image-1',$4,100,'settled',20,30,clock_timestamp())`, owner.ID, runtimeCallID, requestHash[:], submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash, runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,1,'approval','Save generated media','image','Review preview','Creates a Workspace artifact',$3,'image.generate',$4,0,'instance-1',0,'pending')`,
		pendingID, submitted.Task.ID, runtimeCallID, requestHash[:]); err != nil {
		t.Fatal(err)
	}
	contentHash := sha256.Sum256([]byte("synthetic media bytes"))
	if err := store.CreateTaskMediaPreviewForApproval(ctx, app.CreateTaskMediaStagedObjectCommand{
		TaskID: submitted.Task.ID, OwnerID: owner.ID, OperationID: operationID, RuntimeCallID: runtimeCallID,
		PendingRequestID: pendingID, PendingRequestVersion: 1, MediaType: "image/png", SizeBytes: int64(len("synthetic media bytes")),
		SHA256: contentHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return store, ctx, owner.ID, submitted.Task.ID
}

func TestPostgresTaskMediaPreviewLinkRejectsStaleApprovalAtomically(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "media-stale", "generate media"))
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "o2234567"
	const pendingRequestID = "r2234567"
	const runtimeCallID = "media-call-223"
	requestHash := sha256.Sum256([]byte("stale media request"))
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='approval_required' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, control_version, lease_version, status)
VALUES ($1,$2,'media-call',$3,$4,0,0,1,'succeeded')`, operationID, submitted.Task.ID, requestHash[:], hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id, source, call_id, request_digest, model_id, provider_model, task_id, reserved_tokens, state, input_tokens, output_tokens, settled_at)
VALUES ($1,'task',$2,$3,'model-test','provider/image-1',$4,100,'settled',20,30,clock_timestamp())`, owner.ID, runtimeCallID, requestHash[:], submitted.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash, runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,1,'approval','Save generated media','image','Review preview','Creates a Workspace artifact',$3,'image.generate',$4,0,'instance-1',0,'pending')`,
		pendingRequestID, submitted.Task.ID, runtimeCallID, requestHash[:]); err != nil {
		t.Fatal(err)
	}

	content := []byte("synthetic media bytes")
	contentHash := sha256.Sum256(content)
	err = store.CreateTaskMediaPreviewForApproval(ctx, app.CreateTaskMediaStagedObjectCommand{
		TaskID: submitted.Task.ID, OwnerID: owner.ID, OperationID: operationID, RuntimeCallID: runtimeCallID,
		PendingRequestID: pendingRequestID, PendingRequestVersion: 2, MediaType: "image/png", SizeBytes: int64(len(content)),
		SHA256: contentHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale approval link error = %v", err)
	}
	if _, err := store.GetTaskMediaOperationDetails(ctx, submitted.Task.ID, operationID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("staged metadata survived stale approval rollback: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_media_staged_objects WHERE task_id=$1`, submitted.Task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("staged row count after stale approval = %d, err=%v", count, err)
	}
}
