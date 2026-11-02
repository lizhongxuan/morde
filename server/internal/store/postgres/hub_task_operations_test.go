package postgres

import (
	"bytes"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
)

func TestTaskHubFilePutStateValidationAndReceiptBinding(t *testing.T) {
	base := app.TaskHubOperation{
		OwnerID: "u1234567", TaskID: "q1234567", Key: "agent:" + strings.Repeat("a", 64),
		ToolName: "morde_hub_file_put_i1234567_v2", TargetInstallationID: "i1234567", Action: hub.ActionFilePut,
		TaskAuthorizationVersion: 1, TargetGrantVersion: 2, LeaseVersion: 1, RequestHash: bytes.Repeat([]byte{2}, 32),
		WorkspaceID: "w1234567", WorkspacePath: "reports/result.txt", WorkspaceSizeBytes: 6,
		WorkspaceSHA256: strings.Repeat("b", 64), SourceCheckpointRevision: 3,
	}
	localReceiptHash := bytes.Repeat([]byte{1}, 32)
	valid := func(status task.OperationStatus, stage string) app.TaskHubOperation {
		value := base
		value.Status, value.SyncStage = status, stage
		return value
	}

	for _, status := range []task.OperationStatus{task.OperationPrepared, task.OperationDispatched, task.OperationFailed, task.OperationUnknown} {
		if !validTaskHubWriteState(valid(status, app.TaskHubWriteStagePending)) {
			t.Errorf("pending stage rejected status %q", status)
		}
	}
	for _, status := range []task.OperationStatus{task.OperationDispatched, task.OperationUnknown} {
		value := valid(status, app.TaskHubWriteStageLocal)
		value.LocalReceiptHash = localReceiptHash
		if !validTaskHubWriteState(value) {
			t.Errorf("local stage rejected status %q", status)
		}
		value.SyncStage, value.SyncSessionID = app.TaskHubWriteStageSyncPending, "p1234567"
		if !validTaskHubWriteState(value) {
			t.Errorf("sync-pending stage rejected status %q", status)
		}
	}
	cloudSynced := valid(task.OperationSucceeded, app.TaskHubWriteStageCloudSynced)
	cloudSynced.LocalReceiptHash, cloudSynced.SyncSessionID = localReceiptHash, "p1234567"
	cloudSynced.SyncRevision, cloudSynced.ResultHash = 4, localReceiptHash
	if !validTaskHubWriteState(cloudSynced) {
		t.Fatal("cloud-synced stage rejected a complete receipt")
	}
	cloudSynced.SyncSessionID = ""
	if validTaskHubWriteState(cloudSynced) {
		t.Fatal("cloud-synced stage accepted a missing session")
	}
	if validTaskHubAction(hub.ActionFilePut) {
		t.Fatal("file.put was added to the Hub action allowlist before target transport acceptance")
	}
	if validTaskHubOperation(valid(task.OperationPrepared, app.TaskHubWriteStagePending)) ||
		!validTaskHubOperationState(valid(task.OperationPrepared, app.TaskHubWriteStagePending)) {
		t.Fatal("file.put storage state must remain representable behind the closed action allowlist")
	}

	first, err := taskHubFilePutLocalReceiptHash(base, []byte("device receipt"))
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.TargetGrantVersion++
	second, err := taskHubFilePutLocalReceiptHash(changed, []byte("device receipt"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("local receipt hash was not bound to the target grant version")
	}
	changed = base
	changed.WorkspacePath = "reports/other.txt"
	third, err := taskHubFilePutLocalReceiptHash(changed, []byte("device receipt"))
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("local receipt hash was not bound to the Workspace path")
	}
}
