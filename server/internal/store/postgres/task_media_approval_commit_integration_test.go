package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskMediaApprovalCommitIsAtomic(t *testing.T) {
	for _, failAfterMediaCommit := range []bool{false, true} {
		name := "commit"
		if failAfterMediaCommit {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			store, pool := newTemporaryStore(t)
			ctx := context.Background()
			owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
			hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
			const runtimeInstanceID = "media-runtime-instance"
			if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
				t.Fatal(err)
			}
			submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "media-approval-"+name, "generate video"))
			if err != nil {
				t.Fatal(err)
			}
			claimed, found, err := store.ClaimNextTask(ctx, "task-media-approval-worker", time.Minute)
			if err != nil || !found || claimed.ID != submitted.Task.ID {
				t.Fatalf("claim media Task: found=%t task=%+v err=%v", found, claimed, err)
			}

			const operationID = "o8345678"
			const pendingID = "r8345678"
			const toolCallID = "call_media_approval"
			const runtimeCallID = "mdl_83456781234567890123456789012345678901234567890123456789012345"
			requestHash := sha256.Sum256([]byte("video.generate:{prompt:cinematic}"))
			if _, err := pool.Exec(ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, runtime_instance_id, control_version,
 lease_version, runtime_lease_version, status, external_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,'dispatched','provider-op-834')`, operationID, claimed.ID,
				app.AgentOperationKey(toolCallID), requestHash[:], hostValue.ID, hostValue.RuntimeGeneration, runtimeInstanceID,
				hostValue.ControlVersion, claimed.Lease.Version); err != nil {
				t.Fatal(err)
			}
			ledgerDigest := sha256.Sum256([]byte("metered video generation call"))
			if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id, source, call_id, request_digest, model_id, provider_model, task_id, reserved_tokens, state, input_tokens, output_tokens, settled_at)
VALUES ($1,'task',$2,$3,'model-test','provider/video-1',$4,100,'settled',20,30,clock_timestamp())`, owner.ID,
				runtimeCallID, ledgerDigest[:], claimed.ID); err != nil {
				t.Fatal(err)
			}

			pending := &task.PendingRequest{
				ID: pendingID, TaskID: claimed.ID, Version: 1, Kind: "approval", Title: "Save generated video",
				Object: "Video preview", Content: "Review the generated video", Impact: "Creates a Workspace artifact",
				CallID: toolCallID, MediaPreviewOperationID: operationID, ToolName: "video.generate", RequestHash: requestHash[:],
				RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: runtimeInstanceID,
				ControlVersion: hostValue.ControlVersion, Status: task.PendingRequestPending,
			}
			mediaHash := sha256.Sum256([]byte("staged video"))
			media := &app.TaskMediaApprovalCommit{
				OperationID: operationID, RuntimeCallID: runtimeCallID, ProviderOperationID: "provider-op-834",
				OperationResult: `{"preview":"ready"}`, MediaType: "video/mp4", SizeBytes: 1024,
				SHA256: mediaHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
			}
			usage := claimed.Usage
			if failAfterMediaCommit {
				usage.ToolCalls = int64(claimed.Budget.ToolCalls) + 1
			}
			payload, err := json.Marshal(map[string]string{"status": string(task.StatusWaitingUser), "reason": "approval_required"})
			if err != nil {
				t.Fatal(err)
			}
			var eventSeqBefore int64
			if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, claimed.ID).Scan(&eventSeqBefore); err != nil {
				t.Fatal(err)
			}
			progress := app.TaskProgress{
				TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
				Status: task.StatusWaitingUser, WaitingReason: "approval_required", Usage: usage,
				Event: task.Event{Type: "task.waiting_user", Payload: payload}, PendingRequest: pending, TaskMediaApproval: media,
			}
			_, err = store.CommitTaskProgress(ctx, progress)
			if failAfterMediaCommit {
				if domain.CodeOf(err) != domain.CodeQuotaExceeded {
					t.Fatalf("invalid usage commit error = %v, want quota exceeded", err)
				}
				var operationStatus, operationResult, taskStatus string
				var stagedCount, pendingCount int
				var eventSeqAfter int64
				if err := pool.QueryRow(ctx, `SELECT status,result FROM task_operations WHERE id=$1`, operationID).Scan(&operationStatus, &operationResult); err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRow(ctx, `SELECT status,event_seq FROM tasks WHERE id=$1`, claimed.ID).Scan(&taskStatus, &eventSeqAfter); err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_media_staged_objects WHERE task_id=$1`, claimed.ID).Scan(&stagedCount); err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_pending_requests WHERE task_id=$1`, claimed.ID).Scan(&pendingCount); err != nil {
					t.Fatal(err)
				}
				if operationStatus != string(task.OperationDispatched) || operationResult != "" || taskStatus != string(task.StatusRunning) || eventSeqAfter != eventSeqBefore || stagedCount != 0 || pendingCount != 0 {
					t.Fatalf("failed Task progress left partial media commit: operation=%s task=%s seq=%d/%d staged=%d pending=%d",
						operationStatus, taskStatus, eventSeqAfter, eventSeqBefore, stagedCount, pendingCount)
				}
				return
			}
			if err != nil {
				t.Fatalf("commit media approval: %v", err)
			}

			details, err := store.GetTaskMediaOperationDetails(ctx, claimed.ID, operationID)
			if err != nil || details.OperationStatus != string(task.OperationSucceeded) || details.TokenState != "settled" ||
				details.InputTokens == nil || *details.InputTokens != 20 || details.OutputTokens == nil || *details.OutputTokens != 30 ||
				details.State != app.TaskMediaPreviewPending || details.RuntimeCallID != runtimeCallID || details.ProviderOperationID != "provider-op-834" {
				t.Fatalf("committed media operation = %#v, err=%v", details, err)
			}
			operation, err := store.GetOperation(ctx, owner.ID, claimed.ID, app.AgentOperationKey(toolCallID))
			if err != nil || operation.Status != task.OperationSucceeded || operation.ExternalID != "provider-op-834" || operation.Result != media.OperationResult {
				t.Fatalf("committed generic operation = %#v, err=%v", operation, err)
			}
			taskDetails, err := store.GetTask(ctx, claimed.ID, owner.ID)
			if err != nil || taskDetails.Task.Status != task.StatusWaitingUser || taskDetails.PendingRequest == nil ||
				taskDetails.PendingRequest.MediaPreviewOperationID != operationID {
				t.Fatalf("committed media approval = %#v, err=%v", taskDetails.PendingRequest, err)
			}
			var events []string
			rows, err := pool.Query(ctx, `SELECT event_type,payload FROM task_events WHERE task_id=$1 AND sequence>$2 ORDER BY sequence`, claimed.ID, eventSeqBefore)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var eventType string
				var eventPayload []byte
				if err := rows.Scan(&eventType, &eventPayload); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				if bytes.Contains(eventPayload, []byte("provider-op-834")) || bytes.Contains(eventPayload, []byte("staged video")) {
					t.Fatalf("Task event leaked provider operation or media bytes: %s", eventPayload)
				}
				events = append(events, eventType)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			if len(events) != 2 || events[0] != "task.operation" || events[1] != "task.waiting_user" {
				t.Fatalf("committed media event order = %v", events)
			}
		})
	}
}
