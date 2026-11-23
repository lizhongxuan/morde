package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestNestedAgentSessionsEnforceDepthSubtreeAndCancellation(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "nested-agent-subtree", false)
	service, err := app.NewAgentChildSessionService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	root := fixture.fence.AgentSessionID
	baseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID, fixture.task.OwnerID, root)
	create := func(parent, key string, tools []string) app.AgentChildSession {
		t.Helper()
		child, replayed, err := service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
			ParentSessionID: parent, SpawnKey: key, RootCheckpointThroughSequence: baseline,
			Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: tools, Background: true})
		if err != nil || replayed || child.ParentSessionID != parent || child.RootCheckpointThroughSequence != baseline {
			t.Fatalf("create child of %q: child=%+v replayed=%t err=%v", parent, child, replayed, err)
		}
		return child
	}
	first := create(root, "first_parent", []string{"workspace_read", "workspace_search"})
	other := create(root, "other_parent", []string{"workspace_read"})
	grandchild := create(first.SessionID, "shared_spawn", []string{"workspace_read"})
	otherGrandchild := create(other.SessionID, "shared_spawn", []string{"workspace_read"})
	firstCancelTarget := create(first.SessionID, "first_cancel_target", []string{"workspace_read"})
	otherCancelTarget := create(other.SessionID, "other_cancel_target", []string{"workspace_read"})
	for _, actorAndJob := range []struct{ actor, job string }{
		{first.SessionID, firstCancelTarget.SessionID}, {other.SessionID, otherCancelTarget.SessionID},
	} {
		if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
			ActorSessionID: actorAndJob.actor, SessionID: actorAndJob.job, CallID: "same_cancel_call"}); err != nil {
			t.Fatalf("same cancellation call from separate coordinators collided: actor=%q err=%v", actorAndJob.actor, err)
		}
	}
	if grandchild.SessionID == otherGrandchild.SessionID {
		t.Fatal("different parents sharing a spawn key created the same child")
	}
	replayed, duplicate, err := service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: first.SessionID, SpawnKey: "shared_spawn", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}, Background: true})
	if err != nil || !duplicate || replayed.SessionID != grandchild.SessionID {
		t.Fatalf("replay nested spawn: child=%+v replayed=%t err=%v", replayed, duplicate, err)
	}
	_, _, err = service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: first.SessionID, SpawnKey: "shared_spawn", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"different work"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}, Background: true})
	if domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("nested spawn key accepted changed arguments: %v", err)
	}
	_, _, err = service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: first.SessionID, SpawnKey: "stale_root_baseline", RootCheckpointThroughSequence: baseline + 1,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("nested spawn accepted a mismatched root checkpoint baseline: %v", err)
	}
	_, _, err = service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: first.SessionID, SpawnKey: "extra_tool", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"shell_execute"}})
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("nested child exceeded its parent's tool allowlist: %v", err)
	}
	_, _, err = service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: grandchild.SessionID, SpawnKey: "too_deep", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("third-level delegation was not rejected: %v", err)
	}
	rootChildren, err := service.List(ctx, fixture.fence, root)
	if err != nil || len(rootChildren) != 2 || rootChildren[0].SessionID != first.SessionID || rootChildren[1].SessionID != other.SessionID {
		t.Fatalf("root child list = %+v err=%v", rootChildren, err)
	}
	firstChildren, err := service.List(ctx, fixture.fence, first.SessionID)
	if err != nil || len(firstChildren) != 2 || firstChildren[0].SessionID != grandchild.SessionID || firstChildren[1].SessionID != firstCancelTarget.SessionID {
		t.Fatalf("coordinator child list = %+v err=%v", firstChildren, err)
	}
	if child, err := service.Get(ctx, fixture.fence, root, grandchild.SessionID); err != nil || child.SessionID != grandchild.SessionID {
		t.Fatalf("root cannot inspect its descendant: child=%+v err=%v", child, err)
	}
	if _, err := service.Get(ctx, fixture.fence, other.SessionID, grandchild.SessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("sibling coordinator accessed another subtree: %v", err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
		ActorSessionID: other.SessionID, SessionID: grandchild.SessionID, CallID: "cancel_shared"}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("sibling coordinator cancelled another subtree: %v", err)
	}
	cancelled, duplicate, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
		ActorSessionID: root, SessionID: first.SessionID, CallID: "cancel_parent"})
	if err != nil || duplicate || !cancelled.CancelRequested {
		t.Fatalf("cancel parent: child=%+v replayed=%t err=%v", cancelled, duplicate, err)
	}
	for _, want := range []struct {
		id        string
		cancelled bool
	}{{grandchild.SessionID, true}, {other.SessionID, false}, {otherGrandchild.SessionID, false}} {
		child, err := service.Get(ctx, fixture.fence, root, want.id)
		if err != nil || child.CancelRequested != want.cancelled {
			t.Fatalf("cancellation state for %q: child=%+v err=%v", want.id, child, err)
		}
	}
	_, _, err = service.Create(ctx, app.AgentChildSessionCommand{Fence: fixture.fence,
		ParentSessionID: first.SessionID, SpawnKey: "new_after_cancel", RootCheckpointThroughSequence: baseline,
		Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", AllowedTools: []string{"workspace_read"}})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("cancelled parent spawned a new child: %v", err)
	}
	if _, replayed, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
		ActorSessionID: first.SessionID, SessionID: firstCancelTarget.SessionID, CallID: "same_cancel_call"}); err != nil || !replayed {
		t.Fatalf("cancelled actor could not reconcile its previously persisted cancellation: replayed=%t err=%v", replayed, err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fixture.fence,
		ActorSessionID: first.SessionID, SessionID: grandchild.SessionID, CallID: "new_cancel_call"}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("cancelled actor created a new cancellation operation: %v", err)
	}
	wrongOwner := fixture.fence
	wrongOwner.OwnerID = "u7654321"
	if _, err := service.Get(ctx, wrongOwner, root, grandchild.SessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("other owner accessed nested child: %v", err)
	}
	wrongTask := fixture.fence
	wrongTask.TaskID = "q7654321"
	if _, err := service.Get(ctx, wrongTask, root, grandchild.SessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("other task accessed nested child: %v", err)
	}
}
