package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
)

func TestPostgresHubTaskOperationIsIdempotentAndTerminal(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "hub-task-operation", "inspect target")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", found, claimed, err)
	}
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:hub-test-operation",
		ToolName: "morde_hub_files_" + targetID + "_v1", TargetInstallationID: targetID, Action: hub.ActionFiles,
		TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 1,
		LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("bounded files request"), Status: task.OperationPrepared,
	}

	var wait sync.WaitGroup
	preparedResults := make(chan app.TaskHubOperation, 2)
	replayedResults := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
			preparedResults <- prepared
			replayedResults <- replayed
			errs <- err
		}()
	}
	wait.Wait()
	close(preparedResults)
	close(replayedResults)
	close(errs)
	var prepared app.TaskHubOperation
	newRows, replayedRows := 0, 0
	for value := range preparedResults {
		prepared = value
	}
	for replayed := range replayedResults {
		if replayed {
			replayedRows++
		} else {
			newRows++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent prepare: %v", err)
		}
	}
	if newRows != 1 || replayedRows != 1 || prepared.Status != task.OperationPrepared {
		t.Fatalf("concurrent prepares created %d rows and replayed %d; last operation=%+v", newRows, replayedRows, prepared)
	}
	if _, _, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner); err != nil {
		t.Fatalf("idempotent prepare: %v", err)
	}
	changed := operation
	changed.RequestHash = hashBytes("changed request")
	if _, _, err := store.PrepareTaskHubOperation(ctx, changed, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same operation key with changed request hash = %v", err)
	}

	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate dispatch = %v", err)
	}
	resultHash := hashBytes("metadata only; body is not persisted")
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, resultHash); err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, resultHash); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, hashBytes("different result")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed completion hash = %v", err)
	}
	stored, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || stored.Status != task.OperationSucceeded || string(stored.ResultHash) != string(resultHash) {
		t.Fatalf("stored successful operation = %+v err=%v", stored, err)
	}

	preparedFailure := operation
	preparedFailure.Key = "agent:hub-prepared-failure"
	preparedFailure.RequestHash = hashBytes("request rejected before dispatch")
	preparedFailure, _, err = store.PrepareTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationSucceeded, resultHash); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("prepared operation reported success without dispatch = %v", err)
	}
	failureHash := hashBytes("request was rejected before dispatch")
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, failureHash); err != nil {
		t.Fatalf("record failure before dispatch: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, failureHash); err != nil {
		t.Fatalf("idempotent pre-dispatch failure: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, resultHash); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed pre-dispatch failure hash = %v", err)
	}
	storedFailure, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, preparedFailure.Key)
	if err != nil || storedFailure.Status != task.OperationFailed || string(storedFailure.ResultHash) != string(failureHash) {
		t.Fatalf("stored pre-dispatch failure = %+v err=%v", storedFailure, err)
	}

	unknown := operation
	unknown.Key = "agent:hub-unknown-operation"
	unknown.RequestHash = hashBytes("unknown request")
	unknownPrepared, _, err := store.PrepareTaskHubOperation(ctx, unknown, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationUnknown(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if err := store.MarkTaskHubOperationUnknown(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("repeat unknown settlement: %v", err)
	}
	unknownReplay, replayed, err := store.PrepareTaskHubOperation(ctx, unknown, claimed.Lease.Owner)
	if err != nil || !replayed || unknownReplay.Status != task.OperationUnknown {
		t.Fatalf("unknown operation replay = %+v replayed=%t err=%v", unknownReplay, replayed, err)
	}
}

func TestPostgresHubTaskOperationRechecksTargetBeforeDispatch(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "hub-target-revoke", "inspect target")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-target-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", found, claimed, err)
	}
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:hub-target-revoke", ToolName: "morde_hub_screenshot_" + targetID + "_v1",
		TargetInstallationID: targetID, Action: hub.ActionScreenshot, TaskAuthorizationVersion: claimed.Authorization.Version,
		TargetGrantVersion: 1, LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("screenshot request"), Status: task.OperationPrepared,
	}
	prepared, _, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("dispatch after target revocation = %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationFailed, hashBytes("target revoked")); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("pre-dispatch failure after target revocation = %v", err)
	}
	stored, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("revoked target crossed dispatch fence: operation=%+v err=%v", stored, err)
	}
}
