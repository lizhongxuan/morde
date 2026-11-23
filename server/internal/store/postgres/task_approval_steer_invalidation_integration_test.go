package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestConversationRouteUpdateCancelsPendingApprovalWithoutAffectingIndependentTask(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	createWaitingApproval := func(fixture routeActionsTestFixture, clientID, worker string) (string, task.PendingRequest) {
		t.Helper()
		taskID := createConversationRouteActionTask(t, fixture, clientID, "Prepare an export")
		claimed, found, err := fixture.store.ClaimNextTask(ctx, worker, time.Minute)
		if err != nil || !found || claimed.ID != taskID || claimed.CurrentRevision == nil {
			t.Fatalf("claim Task %s: id=%q found=%t revision=%v err=%v", taskID, claimed.ID, found, claimed.CurrentRevision, err)
		}
		stepID, pendingID := mustID(t, domain.NewStepID), mustID(t, domain.NewPendingRequestID)
		revision := *claimed.CurrentRevision
		request := task.PendingRequest{
			ID: pendingID, TaskID: taskID, Version: 1, TaskRevision: &revision, Kind: "approval",
			Title: "Approve export", Object: "report.csv", Content: "Export this exact report",
			Impact: "The report will leave this Host.", CallID: "call-" + clientID, ToolName: "file_export",
			RequestHash: hashBytes("export:" + clientID), RuntimeGeneration: 1,
			RuntimeInstanceID: "test-runtime-" + clientID, ControlVersion: 1, Status: task.PendingRequestPending,
		}
		if _, err := fixture.store.CommitTaskProgress(ctx, app.TaskProgress{
			TaskID: taskID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
			Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
			Steps: []task.Step{{ID: stepID, TaskID: taskID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
				Title: request.Title, Summary: request.Content}},
			Event:          task.Event{TaskID: taskID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
			PendingRequest: &request,
		}); err != nil {
			t.Fatalf("commit pending approval for Task %s: %v", taskID, err)
		}
		return taskID, request
	}

	targetID, targetApproval := createWaitingApproval(f, "approval-steer-target", "approval-steer-target-worker")
	independent := f
	independent.conversation = createMainConversationForTest(t, f.store, f.ownerID)
	independentID, independentApproval := createWaitingApproval(independent, "approval-steer-independent", "approval-steer-independent-worker")

	steer := f.receive(t, "approval-steer-update", "Change the export to JSON", app.ConversationRoutingInput{
		ReferenceTaskID: targetID,
	})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, steer)
	if err != nil {
		t.Fatalf("load Task update snapshot: %v", err)
	}
	var targetRevision int64
	var targetFound bool
	for _, candidate := range snapshot.Context.Candidates {
		if candidate.TaskID == targetID {
			targetRevision = candidate.Revision
			targetFound = true
		}
	}
	if !targetFound || targetRevision < 0 || targetRevision != *targetApproval.TaskRevision {
		t.Fatalf("target Task revision candidate=%d, approval=%d candidates=%+v", targetRevision,
			*targetApproval.TaskRevision, snapshot.Context.Candidates)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, steer, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: targetID, ExpectedRevision: targetRevision,
	}); err != nil {
		t.Fatalf("route explicit Task update: %v", err)
	}

	updated, err := f.store.GetTask(ctx, targetID, f.ownerID)
	if err != nil || updated.Task.Status != task.StatusPausing || updated.PendingRequest == nil ||
		updated.PendingRequest.ID != targetApproval.ID || updated.PendingRequest.Status != task.PendingRequestCancelled {
		t.Fatalf("updated Task retained an actionable approval: status=%s pending=%+v err=%v",
			updated.Task.Status, updated.PendingRequest, err)
	}
	staleResponse := testIdempotency(f.ownerID, "task.pending.respond", "approval-steer-stale-response")
	staleResponse.ResourceID, staleResponse.RelatedID = targetID, targetApproval.ID
	if err := f.store.RespondToPendingRequest(ctx, staleResponse, targetID, targetApproval.ID,
		targetApproval.Version, task.PendingDecisionApprove); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("approve after a same-Task update = %v, want conflict", err)
	}
	afterStaleResponse, err := f.store.GetTask(ctx, targetID, f.ownerID)
	if err != nil || afterStaleResponse.Task.Status != task.StatusPausing || afterStaleResponse.PendingRequest == nil ||
		afterStaleResponse.PendingRequest.Status != task.PendingRequestCancelled {
		t.Fatalf("stale approval changed target Task: status=%s pending=%+v err=%v",
			afterStaleResponse.Task.Status, afterStaleResponse.PendingRequest, err)
	}

	independentResponse := testIdempotency(f.ownerID, "task.pending.respond", "approval-steer-independent-response")
	independentResponse.ResourceID, independentResponse.RelatedID = independentID, independentApproval.ID
	if err := f.store.RespondToPendingRequest(ctx, independentResponse, independentID,
		independentApproval.ID, independentApproval.Version, task.PendingDecisionApprove); err != nil {
		t.Fatalf("approve independent Task after target update: %v", err)
	}
	independentDetails, err := f.store.GetTask(ctx, independentID, f.ownerID)
	if err != nil || independentDetails.Task.Status != task.StatusQueued || independentDetails.PendingRequest == nil ||
		independentDetails.PendingRequest.Status != task.PendingRequestApproved {
		t.Fatalf("independent Task approval was affected: status=%s pending=%+v err=%v",
			independentDetails.Task.Status, independentDetails.PendingRequest, err)
	}
}
