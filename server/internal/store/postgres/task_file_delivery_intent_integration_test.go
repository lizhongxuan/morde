package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresTaskBatchFileDeliveryIntentIsImmutableAcrossRetryAndReplay(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "batch-file-intent-retry")
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "batch-file-intent-retry", "process these records")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("batch file intent workspace manifest")
	requiredItemID := mustID(t, domain.NewBatchItemID)
	noneItemID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{
		{ID: requiredItemID, Key: "file-result", Input: "produce a report", FileDeliveryIntent: task.FileDeliveryRequired},
		{ID: noneItemID, Key: "chat-result", Input: "explain the result", FileDeliveryIntent: task.FileDeliveryNone},
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit structured batch intents: %v", err)
	}
	replayed, err := store.SubmitTask(ctx, submission)
	if err != nil || !replayed.Replayed || replayed.Task.ID != created.Task.ID {
		t.Fatalf("replay structured batch submission = %#v err=%v", replayed, err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 2 ||
		batchItemByID(t, details.BatchItems, requiredItemID).FileDeliveryIntent != task.FileDeliveryRequired ||
		batchItemByID(t, details.BatchItems, noneItemID).FileDeliveryIntent != task.FileDeliveryNone {
		t.Fatalf("read submitted batch intent = %#v err=%v", details.BatchItems, err)
	}

	claimed, found, err := store.ClaimNextTask(ctx, "file-intent-retry-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim batch Task: claimed=%t task=%q err=%v", found, claimed.ID, err)
	}
	requiredItem := batchItemByID(t, details.BatchItems, requiredItemID)
	requiredItem.FileDeliveryIntent = task.FileDeliveryNone
	if _, err := store.UpdateBatchItem(ctx, owner.ID, requiredItem, claimed.Lease.Owner, claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("attempt to change immutable batch item intent error = %v", err)
	}
	requiredItem = batchItemByID(t, details.BatchItems, requiredItemID)
	requiredItem.Status = task.BatchItemRunning
	requiredItem, err = store.UpdateBatchItem(ctx, owner.ID, requiredItem, claimed.Lease.Owner, claimed.Lease.Version)
	if err != nil {
		t.Fatalf("start required batch item: %v", err)
	}
	requiredItem.Status = task.BatchItemFailed
	requiredItem.Error = "temporary failure"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, requiredItem, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("fail required batch item: %v", err)
	}
	chatItem := batchItemByID(t, details.BatchItems, noneItemID)
	chatItem.Status = task.BatchItemRunning
	chatItem, err = store.UpdateBatchItem(ctx, owner.ID, chatItem, claimed.Lease.Owner, claimed.Lease.Version)
	if err != nil {
		t.Fatalf("start chat-only batch item: %v", err)
	}
	chatItem.Status = task.BatchItemSucceeded
	chatItem.Result = "explained in chat"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, chatItem, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("complete chat-only batch item: %v", err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "batch item failed",
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "batch_result", Status: task.StepFailed, Title: "Batch item failed"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed"}`)},
	}); err != nil {
		t.Fatalf("fail Task after item failure: %v", err)
	}
	retry := testIdempotency(owner.ID, "task.batch.retry", "required-item-retry")
	retry.ResourceID, retry.RelatedID = created.Task.ID, requiredItemID
	if err := store.RetryFailedBatchItem(ctx, retry, created.Task.ID, requiredItemID); err != nil {
		t.Fatalf("retry required batch item: %v", err)
	}
	if err := store.RetryFailedBatchItem(ctx, retry, created.Task.ID, requiredItemID); err != nil {
		t.Fatalf("replay required batch item retry: %v", err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	retried := batchItemByID(t, details.BatchItems, requiredItemID)
	if retried.Attempt != 2 || retried.FileDeliveryIntent != task.FileDeliveryRequired || details.Task.Status != task.StatusQueued {
		t.Fatalf("retry changed or dropped structured intent: item=%#v task_status=%q", retried, details.Task.Status)
	}
}

func TestPostgresBatchFileDeliveryClarificationResolvesExactAttemptAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "batch-file-intent-clarification", "review these records")
	itemID := mustID(t, domain.NewBatchItemID)
	classificationStepID := mustID(t, domain.NewStepID)
	clarificationStepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	question := "Does this item require a saved file, or is a chat answer enough?"
	submission.Task.Status = task.StatusWaitingUser
	submission.Task.WaitingReason = "clarification_required"
	submission.Task.CurrentStep = clarificationStepID
	submission.InitialEvent = task.Event{Type: "task.waiting_user", Payload: []byte(`{"status":"waiting_user","reason":"clarification_required"}`)}
	submission.InitialSteps = []task.Step{
		{ID: classificationStepID, TaskID: submission.Task.ID, Sequence: 1, Kind: "verification", Status: task.StepSucceeded,
			Title: "Check batch item output intent", Summary: "The requested output type needs confirmation."},
		{ID: clarificationStepID, TaskID: submission.Task.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser,
			Title: "Confirm batch item output", Summary: question},
	}
	submission.BatchItems = []task.BatchItem{{ID: itemID, Key: "row-a", Input: "prepare a report", Attempt: 1,
		FileDeliveryIntent: task.FileDeliveryUnclear}}
	submission.InitialClarificationRequest = &task.ClarificationRequest{
		ID: requestID, TaskID: submission.Task.ID, Version: 1, ModelStepID: classificationStepID,
		CallID: app.FileDeliveryIntentClarificationCallID, Question: question, BatchItemID: itemID,
		BatchAttempt: 1, Status: task.ClarificationPending,
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit batch file clarification: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.FileDeliveryIntent != task.FileDeliveryNone ||
		len(details.BatchItems) != 1 || details.BatchItems[0].FileDeliveryIntent != task.FileDeliveryUnclear ||
		details.BatchItems[0].ID != itemID || details.ClarificationRequests[0].ID != requestID {
		t.Fatalf("stored batch file clarification = %#v err=%v", details, err)
	}

	answer := "No saved file, answer in chat."
	objective := submission.Task.Objective + "\n\nUser clarification about file delivery: " + answer
	record := testIdempotency(owner.ID, "task.clarification.answer", "batch-file-answer")
	record.ResourceID, record.RelatedID = created.Task.ID, requestID
	if err := store.AnswerClarificationRequest(ctx, record, created.Task.ID, requestID, 1, answer, task.FileDeliveryNone, objective); err != nil {
		t.Fatalf("answer exact batch file clarification: %v", err)
	}
	if err := store.AnswerClarificationRequest(ctx, record, created.Task.ID, requestID, 1, answer, task.FileDeliveryNone, objective); err != nil {
		t.Fatalf("replay batch file clarification answer: %v", err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.FileDeliveryIntent != task.FileDeliveryNone ||
		details.Task.Objective != objective || len(details.BatchItems) != 1 ||
		details.BatchItems[0].FileDeliveryIntent != task.FileDeliveryNone || details.BatchItems[0].Attempt != 1 ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].Status != task.ClarificationAnswered {
		t.Fatalf("resolved batch file clarification = %#v err=%v", details, err)
	}
}
