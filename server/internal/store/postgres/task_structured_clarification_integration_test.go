package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresStructuredClarificationIsVersionBoundIdempotentAndIsolated(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "structured-clarification-submit", "prepare a deployment"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "structured-clarification-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v found=%t err=%v", claimed, found, err)
	}
	modelStepID := mustID(t, domain.NewStepID)
	stepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	questions := []task.ClarificationQuestion{
		{ID: "environment", Prompt: "Which environment?", Type: task.ClarificationSingleChoice, Required: true,
			Options: []task.ClarificationQuestionOption{{ID: "staging", Label: "Staging"}, {ID: "production", Label: "Production"}}},
		{ID: "notes", Prompt: "Any extra notes?", Type: task.ClarificationFreeText, AllowSkip: true,
			Options: []task.ClarificationQuestionOption{}},
	}
	request := &task.ClarificationRequest{ID: requestID, TaskID: claimed.ID, Version: 3, ModelStepID: modelStepID,
		CallID: "call-ask-user", Question: "Please answer the deployment questions.", Questions: questions, Status: task.ClarificationPending}
	progress := app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "clarification_required",
		Usage: task.Usage{ActiveSeconds: 2, ToolCalls: 1},
		Steps: []task.Step{
			{ID: modelStepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask the model", Summary: "Model asked the user."},
			{ID: stepID, TaskID: claimed.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "The Agent needs input", Summary: request.Question},
		},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"clarification_required"}`)}, ClarificationRequest: request,
	}
	for _, invalid := range []task.ClarificationRequest{
		func() task.ClarificationRequest {
			value := *request
			value.CallID = task.RouteFileDeliveryIntentClarificationCallID
			return value
		}(),
		func() task.ClarificationRequest {
			value := *request
			value.CallID = app.FileDeliveryIntentClarificationCallID
			return value
		}(),
		func() task.ClarificationRequest {
			value := *request
			value.BatchItemID = "b1234567"
			value.BatchAttempt = 1
			return value
		}(),
	} {
		invalidProgress := progress
		invalidRequest := invalid
		invalidProgress.ClarificationRequest = &invalidRequest
		if _, err := store.CommitTaskProgress(ctx, invalidProgress); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("non-general structured clarification source was accepted: call_id=%q batch=%q err=%v", invalid.CallID, invalid.BatchItemID, err)
		}
	}
	if _, err := store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || len(details.ClarificationRequests) != 1 ||
		details.ClarificationRequests[0].Version != 3 || len(details.ClarificationRequests[0].Questions) != 2 {
		t.Fatalf("restored question group = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	answers := []task.ClarificationAnswer{
		{QuestionID: "environment", OptionIDs: []string{"staging"}},
		{QuestionID: "notes", Skipped: true, OptionIDs: []string{}},
	}
	answersJSON, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.clarification.structured_answer", Key: "structured-answer-key",
		RequestHash: hashBytes(string(answersJSON)), ResourceID: claimed.ID, RelatedID: requestID}
	otherRecord := record
	otherRecord.OwnerID, otherRecord.Key = other.ID, "other-account-answer"
	otherRecord.RequestHash = hashBytes("other-account")
	if err := store.AnswerStructuredClarificationRequest(ctx, otherRecord, claimed.ID, requestID, 3, answers); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-account structured answer error = %v", err)
	}
	legacyRecord := record
	legacyRecord.Operation, legacyRecord.Key = "task.clarification.answer", "legacy-answer-key"
	legacyRecord.RequestHash = hashBytes("legacy answer")
	if err := store.AnswerClarificationRequest(ctx, legacyRecord, claimed.ID, requestID, 3, "legacy answer", "", ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("legacy endpoint accepted a structured group: %v", err)
	}
	if err := store.AnswerStructuredClarificationRequest(ctx, record, claimed.ID, requestID, 3, answers); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerStructuredClarificationRequest(ctx, record, claimed.ID, requestID, 3, answers); err != nil {
		t.Fatalf("idempotent structured answer replay: %v", err)
	}
	stale := record
	stale.Key, stale.RequestHash = "stale-structured-answer", hashBytes("stale")
	if err := store.AnswerStructuredClarificationRequest(ctx, stale, claimed.ID, requestID, 2, answers); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale clarification group version error = %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.WaitingReason != "" ||
		details.Task.Lease.Version != claimed.Lease.Version+1 || details.Task.FileDeliveryIntent != task.FileDeliveryNone ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].Status != task.ClarificationAnswered ||
		details.ClarificationRequests[0].AnswerFormat != task.ClarificationAnswerStructured ||
		!json.Valid([]byte(details.ClarificationRequests[0].Answer)) || !strings.Contains(details.ClarificationRequests[0].Answer, `"question_group_id":"`+requestID+`"`) ||
		!strings.Contains(details.ClarificationRequests[0].Answer, `"skipped":true`) {
		t.Fatalf("Task after structured answer = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	var pendingApprovals int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_pending_requests WHERE task_id=$1 AND status='pending'`, claimed.ID).Scan(&pendingApprovals); err != nil || pendingApprovals != 0 {
		t.Fatalf("structured skip changed approval state: pending=%d err=%v", pendingApprovals, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	responseCount := 0
	for _, event := range events {
		if event.Type == "task.response" {
			responseCount++
		}
	}
	if err != nil || responseCount != 1 {
		t.Fatalf("structured response event count=%d events=%#v err=%v", responseCount, events, err)
	}
}
