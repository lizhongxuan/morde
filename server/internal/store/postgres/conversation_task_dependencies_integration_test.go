package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestConversationRouteTaskDependencyWaitsForAndReceivesExactResult(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	createRoutedTask := func(clientID, content string, dependencyTaskIDs []string) string {
		t.Helper()
		claim := f.receive(t, clientID, content, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		decision := app.ConversationRouteDecision{Action: app.ConversationRouteNew, DependencyTaskIDs: dependencyTaskIDs}
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
			t.Fatal(err)
		}
		var taskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}

	prerequisiteID := createRoutedTask("dependency-a", "Prepare source data", nil)
	dependentID := createRoutedTask("dependency-b", "Use the prepared source data", []string{prerequisiteID})
	var requiredRevision int64
	if err := f.store.pool.QueryRow(ctx, `SELECT required_revision FROM task_dependencies WHERE task_id=$1 AND dependency_task_id=$2`,
		dependentID, prerequisiteID).Scan(&requiredRevision); err != nil || requiredRevision != 0 {
		t.Fatalf("dependency revision = %d err=%v", requiredRevision, err)
	}

	claimed, found, err := f.store.ClaimNextTask(ctx, "dependency-worker", time.Minute)
	if err != nil || !found || claimed.ID != prerequisiteID {
		t.Fatalf("claim prerequisite: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	if _, found, err := f.store.ClaimNextTask(ctx, "dependency-worker", time.Minute); err != nil || found {
		t.Fatalf("dependent Task ran before its prerequisite completed: found=%t err=%v", found, err)
	}

	stepID := mustID(t, domain.NewStepID)
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: prerequisiteID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.StatusCompleted, CurrentStep: stepID, Result: "Revenue was 42.",
		Steps: []task.Step{{ID: stepID, TaskID: prerequisiteID, Sequence: 1, Kind: "result", Status: task.StepSucceeded}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"Revenue was 42."}`)}}); err != nil {
		t.Fatalf("complete prerequisite: %v", err)
	}
	claimed, found, err = f.store.ClaimNextTask(ctx, "dependency-worker", time.Minute)
	if err != nil || !found || claimed.ID != dependentID || len(claimed.DependencyResults) != 1 ||
		claimed.DependencyResults[0].TaskID != prerequisiteID || claimed.DependencyResults[0].RequiredRevision != 0 ||
		claimed.DependencyResults[0].Result != "Revenue was 42." {
		t.Fatalf("dependent Task did not receive the exact prerequisite result: task=%+v found=%t err=%v", claimed, found, err)
	}
}
