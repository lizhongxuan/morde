package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func createS03RunningTask(t *testing.T, fixture routeActionsTestFixture, clientID string) (task.Task, string) {
	t.Helper()
	ctx := context.Background()
	initial := fixture.receive(t, clientID, "Prepare the requested work", app.ConversationRoutingInput{})
	snapshot, err := fixture.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatalf("load initial route snapshot: %v", err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := fixture.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("create initial Task: %v", err)
	}
	var taskID string
	if err := fixture.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := fixture.store.ClaimNextTask(ctx, "s03-tool-fence-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim routed Task: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	return claimed, taskID
}

type s03AgentTaskFixture struct {
	route      routeActionsTestFixture
	task       task.Task
	host       host.Host
	fence      app.AgentInstanceFence
	modelFence app.ModelTaskFence
}

func createS03RunningAgentTask(t *testing.T, fixture routeActionsTestFixture, clientID string, hostBound bool,
	connectorIDs ...string) s03AgentTaskFixture {
	t.Helper()
	ctx := context.Background()
	var target host.Host
	var targetID string
	if hostBound {
		target = createRunningControlHost(t, ctx, fixture.store, fixture.ownerID, host.ControlAI, 0)
		target = setAgentTestHostRuntime(t, fixture.store, target, "s03-agent-runtime")
		targetID = target.ID
	}
	input := app.ConversationRoutingInput{HostID: targetID, MCPConnectorIDs: append([]string(nil), connectorIDs...)}
	initial := fixture.receive(t, clientID, "Prepare the requested work", input)
	snapshot, err := fixture.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatalf("load initial agent route snapshot: %v", err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := fixture.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("create initial agent Task: %v", err)
	}
	claimed, found, err := fixture.store.ClaimNextTask(ctx, "s03-agent-task-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim routed agent Task: found=%t err=%v", found, err)
	}
	if hostBound {
		if _, err := fixture.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, claimed.ID, target.ID); err != nil {
			t.Fatalf("bind routed Task to test Host: %v", err)
		}
		claimed.HostID = target.ID
	}
	key := make([]byte, 32)
	key[0] = 0x53
	authenticator, err := app.NewAgentStopAuthenticator("s03-agent-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(fixture.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 0x39
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "s03-agent-instance", AgentSessionID: "s03-agent-session", SupervisorID: "s03-agent-supervisor",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed {
		t.Fatalf("start routed agent Task: replayed=%t err=%v", replayed, err)
	}
	details, err := fixture.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		t.Fatalf("reload started agent Task: %v", err)
	}
	claimed = details.Task
	if hostBound {
		claimed.HostID = target.ID
	}
	modelFence := app.ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AgentRootSessionID: lease.Fence.AgentSessionID,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		AuthorizationVersion: lease.Fence.AuthorizationVersion, ThroughSequence: 0,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	return s03AgentTaskFixture{route: fixture, task: claimed, host: target, fence: lease.Fence, modelFence: modelFence}
}

func (f s03AgentTaskFixture) call(callID, name, arguments string) app.ModelToolCall {
	return app.ModelToolCall{CallID: callID, Name: name, Arguments: arguments, TaskFence: &f.modelFence}
}

func s03RevisionPointer(value int64) *int64 { return &value }

func rootAgentCheckpointBaselineForTest(t *testing.T, store *Store, taskID, ownerID, rootSessionID string) int64 {
	t.Helper()
	var through int64
	if err := store.pool.QueryRow(context.Background(), `SELECT root_checkpoint_through_seq FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 AND parent_session_id IS NULL`, taskID, ownerID, rootSessionID).Scan(&through); err != nil {
		t.Fatalf("read root Agent checkpoint baseline: %v", err)
	}
	return through
}

func TestAdmitAgentToolCallLinearizesWithConversationSteer(t *testing.T) {
	t.Run("steer accepted first", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-admission-steer-first", false)
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-admission-steer-first-message", "Change the requested scope.")
		call := fixture.call("call_s03_steer_first", "web_search", `{"query":"old question"}`)
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, false)
		if err != nil || admission.Status != app.AgentToolCallSuperseded || len(admission.PendingMessages) != 1 ||
			admission.PendingMessages[0].ID != message.ID || admission.PendingMessages[0].Content != message.Content {
			t.Fatalf("steer-first admission = %+v err=%v; want locked exact superseding input", admission, err)
		}
	})
	t.Run("admission linearized first", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-admission-call-first", false)
		call := fixture.call("call_s03_call_first", "web_search", `{"query":"accepted question"}`)
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, false)
		if err != nil || admission.Status != app.AgentToolCallAdmitted || len(admission.PendingMessages) != 0 {
			t.Fatalf("call-first admission = %+v err=%v; want one admitted start", admission, err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-admission-call-first-message", "Update the next turn.")
		var status string
		if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("steer after admitted call status=%s err=%v; want pending for next turn", status, err)
		}
	})
}

func TestAppliedSteerSupersedesToolCallsAcrossAdmissionAndDispatch(t *testing.T) {
	t.Run("applied before admission returns exact replay snapshot", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-applied-before-admission", false)
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-applied-before-admission-message", "Use the changed requirements.")
		saveS03RootCheckpoint(t, fixture, 0, 2, &message)
		var appliedCheckpointThrough int64
		if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT claimed_checkpoint_through_seq
FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).Scan(&appliedCheckpointThrough); err != nil || appliedCheckpointThrough != 2 {
			t.Fatalf("persisted applied checkpoint through=%d err=%v; want 2", appliedCheckpointThrough, err)
		}
		call := fixture.call("call_s03_applied_before_admission", "web_search", `{"query":"stale query"}`)
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, false)
		if err != nil || admission.Status != app.AgentToolCallSuperseded || len(admission.PendingMessages) != 1 ||
			admission.PendingMessages[0].ID != message.ID || admission.PendingMessages[0].Content != message.Content {
			t.Fatalf("applied-before-admission result = %+v err=%v; want exact supersession snapshot", admission, err)
		}
		replayed, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, true)
		if err != nil || replayed.Status != app.AgentToolCallSuperseded || len(replayed.PendingMessages) != 1 ||
			replayed.PendingMessages[0].ID != message.ID || replayed.PendingMessages[0].Content != message.Content {
			t.Fatalf("applied supersession replay = %+v err=%v; want persisted original message", replayed, err)
		}
		var operations int
		if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_hub_operations
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3`, fixture.task.ID, fixture.task.OwnerID,
			app.AgentOperationKey(call.CallID)).Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("superseded call created Hub dispatch record count=%d err=%v; want zero", operations, err)
		}
	})

	t.Run("admission before applied steer is stopped at final dispatch", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-applied-before-dispatch", false)
		callID := "call_s03_applied_before_dispatch"
		call := fixture.call(callID, "morde_hub_files_target_v1", `{"action":"files"}`)
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, false)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("pre-steer admission = %+v err=%v", admission, err)
		}
		targetID := mustID(t, domain.NewClientInstallationID)
		if _, err := fixture.route.store.pool.Exec(context.Background(), `INSERT INTO client_installations
(id,owner_id,display_name,platform,expires_at) VALUES ($1,$2,'S03 dispatch target','desktop',clock_timestamp()+interval '1 day')`,
			targetID, fixture.task.OwnerID); err != nil {
			t.Fatal(err)
		}
		call = fixture.call(callID, "morde_hub_files_"+targetID+"_v1", `{"action":"files"}`)
		operation := app.TaskHubOperation{OwnerID: fixture.task.OwnerID, TaskID: fixture.task.ID, Key: app.AgentOperationKey(callID),
			ToolName: call.Name, TargetInstallationID: targetID, Action: hub.ActionFiles,
			TaskAuthorizationVersion: fixture.task.Authorization.Version, TargetGrantVersion: 1, LeaseVersion: fixture.task.Lease.Version,
			RequestHash: hashBytes("S03 applied steer final dispatch"), Status: task.OperationPrepared}
		prepared, replayed, err := fixture.route.store.PrepareTaskHubOperation(context.Background(), operation, fixture.task.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare Hub operation before steer = %+v replayed=%t err=%v", prepared, replayed, err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-applied-before-dispatch-message", "Use the changed requirements.")
		saveS03RootCheckpoint(t, fixture, 0, 2, &message)
		err = fixture.route.store.MarkTaskHubOperationDispatched(context.Background(), prepared, fixture.task.Lease.Owner)
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("applied-steer final dispatch = %v pending=%+v; want zero dispatch", err, pending)
		}
		stored, err := fixture.route.store.GetTaskHubOperation(context.Background(), fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed || len(stored.ResultHash) != 32 {
			t.Fatalf("Hub operation after applied-steer gate = %+v err=%v", stored, err)
		}
		recovered, err := fixture.route.store.AdmitAgentToolCall(context.Background(), call, true)
		if err != nil || recovered.Status != app.AgentToolCallSuperseded || len(recovered.PendingMessages) != 1 ||
			recovered.PendingMessages[0].ID != message.ID || recovered.PendingMessages[0].Content != message.Content {
			t.Fatalf("applied-steer dispatch recovery = %+v err=%v", recovered, err)
		}
	})
}

func TestClaimedMessageMigrationBackfillsConservativeRootCheckpointUpperBound(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-claim-migration-backfill", false)
	ctx := context.Background()
	for sequence := int64(0); sequence <= 15; sequence++ {
		saveS03RootCheckpoint(t, fixture, sequence, sequence, nil)
	}
	message := seedLegacyPendingTaskMessageForTest(t, ctx, fixture.route.store, fixture.task.OwnerID,
		fixture.task.ConversationID, fixture.task.ID, "Historical applied steer", nil)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE conversation_messages
SET delivery_mode='steer',delivery_status='applied' WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_agent_message_deliveries
(message_id,task_id,owner_id,initiator_id,agent_instance_id,agent_generation,authorization_version,session_id,mode,claimed_through_seq)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'steer',$9)`, message.ID, fixture.task.ID, fixture.task.OwnerID, fixture.task.InitiatorID,
		fixture.fence.AgentInstanceID, fixture.fence.AgentGeneration, fixture.fence.AuthorizationVersion,
		fixture.fence.AgentSessionID, message.Sequence); err != nil {
		t.Fatal(err)
	}
	if message.Sequence == 15 {
		t.Fatalf("fixture needs distinct conversation and DSH sequences, both were %d", message.Sequence)
	}
	provider, closeProvider, err := newMigrationProvider(fixture.route.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 226); err != nil {
		t.Fatalf("roll back empty tool supersession migration: %v", err)
	}
	if err := Migrate(ctx, fixture.route.store.pool); err != nil {
		t.Fatalf("reapply tool supersession migration over claimed history: %v", err)
	}
	var claimedCheckpointThrough int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT claimed_checkpoint_through_seq
FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).Scan(&claimedCheckpointThrough); err != nil {
		t.Fatal(err)
	}
	if claimedCheckpointThrough != 15 {
		t.Fatalf("historical claimed message seq=%d backfilled checkpoint through=%d; want conservative root upper bound 15",
			message.Sequence, claimedCheckpointThrough)
	}
}

func TestChildToolAdmissionUsesRootCheckpointBaseline(t *testing.T) {
	newChild := func(t *testing.T, fixture s03AgentTaskFixture, spawnKey string, rootThrough int64) (app.AgentChildSessionService, app.AgentChildSession, app.AgentChildSessionCommand) {
		t.Helper()
		service, err := app.NewAgentChildSessionService(fixture.route.store)
		if err != nil {
			t.Fatal(err)
		}
		command := app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
			SpawnKey: spawnKey, RootCheckpointThroughSequence: rootThrough,
			Arguments: []byte(`{"objective":"inspect safely"}`), Title: "Inspect", Summary: "inspect safely",
			AllowedTools: []string{"workspace_read"}}
		child, replayed, err := service.Create(context.Background(), command)
		if err != nil || replayed || child.RootCheckpointThroughSequence != rootThrough {
			t.Fatalf("create child root baseline=%d child=%+v replayed=%t err=%v", rootThrough, child, replayed, err)
		}
		return *service, child, command
	}

	t.Run("old child local sequence is fenced by root baseline two", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-root-baseline-old", false)
		saveS03RootCheckpoint(t, fixture, 0, 2, nil)
		rootCall := fixture.modelFence
		rootCall.ThroughSequence = 2
		spawn := app.ModelToolCall{CallID: "call_s03_child_old_spawn", Name: "dsh_delegate",
			Arguments: `{"objective":"inspect safely"}`, TaskFence: &rootCall}
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), spawn, false)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("child spawn admission at root through=2: %+v err=%v", admission, err)
		}
		children, child, _ := newChild(t, fixture, spawn.CallID, 2)
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-child-root-baseline-old-message", "Change the child work.")
		saveS03RootCheckpoint(t, fixture, 3, 4, &message)
		childFence := s03ChildToolFence(fixture, child, 6)
		childCall := app.ModelToolCall{CallID: "call_s03_child_local_six_old_root_two", Name: "workspace_read",
			Arguments: `{"path":"report.md"}`, TaskFence: &childFence}
		admission, err = fixture.route.store.AdmitAgentToolCall(context.Background(), childCall, false)
		if err != nil || admission.Status != app.AgentToolCallSuperseded || len(admission.PendingMessages) != 1 ||
			admission.PendingMessages[0].ID != message.ID {
			t.Fatalf("child call through=6/root baseline=2 = %+v err=%v; want applied steer supersession", admission, err)
		}
		_ = children
	})

	t.Run("new child baseline four does not compare local sequence six", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-root-baseline-new", false)
		saveS03RootCheckpoint(t, fixture, 0, 2, nil)
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-child-root-baseline-new-message", "Set the current requirements.")
		saveS03RootCheckpoint(t, fixture, 3, 4, &message)
		rootCall := fixture.modelFence
		rootCall.ThroughSequence = 4
		spawn := app.ModelToolCall{CallID: "call_s03_child_new_spawn", Name: "dsh_delegate",
			Arguments: `{"objective":"inspect safely"}`, TaskFence: &rootCall}
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), spawn, false)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("child spawn admission at root through=4: %+v err=%v", admission, err)
		}
		_, child, _ := newChild(t, fixture, spawn.CallID, 4)
		childFence := s03ChildToolFence(fixture, child, 6)
		childCall := app.ModelToolCall{CallID: "call_s03_child_local_six_root_four", Name: "workspace_read",
			Arguments: `{"path":"report.md"}`, TaskFence: &childFence}
		admission, err = fixture.route.store.AdmitAgentToolCall(context.Background(), childCall, false)
		if err != nil || admission.Status != app.AgentToolCallAdmitted || len(admission.PendingMessages) != 0 {
			t.Fatalf("child call through=6/root baseline=4 = %+v err=%v; want admitted", admission, err)
		}
	})

	t.Run("legacy child after applied steer fails once and cannot dispatch", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-unknown-baseline", true)
		saveS03RootCheckpoint(t, fixture, 0, 2, nil)
		service, child, _ := newChild(t, fixture, "call_s03_legacy_child_spawn", 2)
		_ = service
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-legacy-child-applied-steer", "Use the updated instructions.", fixture.host.ID)
		saveS03RootCheckpoint(t, fixture, 3, 4, &message)
		if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE task_agent_sessions
SET root_checkpoint_through_seq=-1,root_checkpoint_baseline_known=false
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fixture.task.ID, fixture.task.OwnerID, child.SessionID); err != nil {
			t.Fatal(err)
		}

		callID := "call_s03_legacy_child_read"
		childFence := s03ChildToolFence(fixture, child, 6)
		childCall := app.ModelToolCall{CallID: callID, Name: "workspace_read", Arguments: `{"path":"report.md"}`, TaskFence: &childFence}
		operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: fixture.task.ID,
			Key: app.AgentOperationKey(callID), RequestHash: hashBytes("legacy child read:" + callID), HostID: fixture.host.ID,
			RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
			ControlVersion: fixture.host.ControlVersion, LeaseVersion: fixture.task.Lease.Version,
			RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationPrepared}
		if _, replayed, err := fixture.route.store.CreateOperation(context.Background(), operation, fixture.task.Lease.Owner); err != nil || replayed {
			t.Fatalf("prepare legacy child operation: replayed=%t err=%v", replayed, err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), childCall, false)
			if err != nil || admission.Status != app.AgentToolCallBaselineUnverifiable || admission.Reason == "" || len(admission.PendingMessages) != 0 {
				t.Fatalf("legacy child admission attempt %d = %+v err=%v; want terminal unverifiable baseline", attempt+1, admission, err)
			}
		}
		_, err := fixture.route.store.UpdateOperation(context.Background(), app.OperationUpdate{TaskID: fixture.task.ID,
			OperationKey: operation.Key, LeaseOwner: fixture.task.Lease.Owner, LeaseVersion: fixture.task.Lease.Version,
			RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
			ControlVersion: fixture.host.ControlVersion, RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched})
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("legacy child prepared→dispatched gate = %v pending=%+v; want no dispatch", err, pending)
		}
		stored, err := fixture.route.store.GetOperation(context.Background(), fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed || stored.Result != app.TaskAgentToolSupersededOperationResult {
			t.Fatalf("legacy child operation after final gate = %+v err=%v; want terminal zero-dispatch marker", stored, err)
		}
	})
}

func TestAgentChildCreateAndCancelFinalGatesRespectSteerOrder(t *testing.T) {
	newChildService := func(t *testing.T, fixture s03AgentTaskFixture) (*app.AgentChildSessionService, app.AgentChildSessionCommand) {
		t.Helper()
		rootCheckpointThroughSequence := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID,
			fixture.task.OwnerID, fixture.fence.AgentSessionID)
		service, err := app.NewAgentChildSessionService(fixture.route.store)
		if err != nil {
			t.Fatal(err)
		}
		return service, app.AgentChildSessionCommand{Fence: fixture.fence, ParentSessionID: fixture.fence.AgentSessionID,
			RootCheckpointThroughSequence: rootCheckpointThroughSequence,
			SpawnKey:                      "call_s03_child_create", Arguments: []byte(`{"title":"Collect notes","objective":"Read the current report"}`),
			Title: "Collect notes", Summary: "Read the current report", AllowedTools: []string{"workspace_read"}}
	}

	t.Run("create superseded before child exists", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-steer-first", false)
		service, command := newChildService(t, fixture)
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-child-steer-first-message", "Change the delegated task.")
		_, _, err := service.Create(context.Background(), command)
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("steer-first child create = %v pending=%+v; want superseded with input snapshot", err, pending)
		}
		var children int
		if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND parent_session_id=$2`,
			fixture.task.ID, fixture.fence.AgentSessionID).Scan(&children); err != nil || children != 0 {
			t.Fatalf("children after rejected create=%d err=%v; want zero", children, err)
		}
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), fixture.call(command.SpawnKey, "dsh_delegate", string(command.Arguments)), true)
		if err != nil || admission.Status != app.AgentToolCallSuperseded || len(admission.PendingMessages) != 1 || admission.PendingMessages[0].ID != message.ID {
			t.Fatalf("recovered child admission = %+v err=%v; want stored supersession", admission, err)
		}
	})

	t.Run("existing child survives later steer", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-create-first", false)
		service, command := newChildService(t, fixture)
		created, replayed, err := service.Create(context.Background(), command)
		if err != nil || replayed {
			t.Fatalf("initial child create = %+v replayed=%t err=%v", created, replayed, err)
		}
		acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-child-create-first-message", "Update the next turn.")
		recovered, replayed, err := service.Create(context.Background(), command)
		if err != nil || !replayed || recovered.SessionID != created.SessionID {
			t.Fatalf("existing child recovery = %+v replayed=%t err=%v", recovered, replayed, err)
		}
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), fixture.call(command.SpawnKey,
			"dsh_delegate", string(command.Arguments)), true)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("existing child recovery admission = %+v err=%v; want admitted", admission, err)
		}
	})

	t.Run("existing child replay keeps stored baseline after root advances", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-child-replay-advanced-root", false)
		saveS03RootCheckpoint(t, fixture, 0, 2, nil)
		service, command := newChildService(t, fixture)
		created, replayed, err := service.Create(context.Background(), command)
		if err != nil || replayed || created.RootCheckpointThroughSequence != 2 {
			t.Fatalf("initial child create = %+v replayed=%t err=%v; want stored baseline 2", created, replayed, err)
		}

		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-child-replay-advanced-root-message", "Continue with updated instructions.")
		saveS03RootCheckpoint(t, fixture, 3, 4, &message)
		if currentRootBaseline := rootAgentCheckpointBaselineForTest(t, fixture.route.store, fixture.task.ID,
			fixture.task.OwnerID, fixture.fence.AgentSessionID); currentRootBaseline != 4 {
			t.Fatalf("current root baseline = %d; want 4", currentRootBaseline)
		}

		recovered, replayed, err := service.Create(context.Background(), command)
		if err != nil || !replayed || recovered.SessionID != created.SessionID || recovered.RootCheckpointThroughSequence != 2 {
			t.Fatalf("replay at stored baseline 2 after root advanced to 4 = %+v replayed=%t err=%v",
				recovered, replayed, err)
		}

		changedBaseline := command
		changedBaseline.RootCheckpointThroughSequence = 4
		if _, _, err := service.Create(context.Background(), changedBaseline); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			t.Fatalf("child replay accepted changed baseline: %v", err)
		}
		changedArguments := command
		changedArguments.Arguments = []byte(`{"objective":"different objective"}`)
		if _, _, err := service.Create(context.Background(), changedArguments); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			t.Fatalf("child replay accepted changed arguments: %v", err)
		}
	})

	t.Run("steer blocks new child cancellation", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-cancel-steer-first", false)
		service, create := newChildService(t, fixture)
		create.Background = true
		child, _, err := service.Create(context.Background(), create)
		if err != nil {
			t.Fatal(err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-cancel-steer-first-message", "Do not cancel the running job; change its instructions.")
		cancelCallID := "call_s03_cancel_after_steer"
		_, _, err = service.RequestCancel(context.Background(), app.AgentChildSessionCancelCommand{Fence: fixture.fence,
			ActorSessionID: fixture.fence.AgentSessionID, SessionID: child.SessionID, CallID: cancelCallID})
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("steer-first child cancel = %v pending=%+v", err, pending)
		}
		stored, err := service.Get(context.Background(), fixture.fence, fixture.fence.AgentSessionID, child.SessionID)
		if err != nil || stored.CancelRequested {
			t.Fatalf("child after rejected cancel = %+v err=%v; want no cancellation", stored, err)
		}
	})

	t.Run("durable cancellation survives later steer", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-cancel-first", false)
		service, create := newChildService(t, fixture)
		create.Background = true
		child, _, err := service.Create(context.Background(), create)
		if err != nil {
			t.Fatal(err)
		}
		cancelCallID := "call_s03_cancel_first"
		command := app.AgentChildSessionCancelCommand{Fence: fixture.fence, ActorSessionID: fixture.fence.AgentSessionID,
			SessionID: child.SessionID, CallID: cancelCallID}
		cancelled, replayed, err := service.RequestCancel(context.Background(), command)
		if err != nil || replayed || !cancelled.CancelRequested {
			t.Fatalf("initial child cancel = %+v replayed=%t err=%v", cancelled, replayed, err)
		}
		acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-cancel-first-message", "Change the next task after canceling this job.")
		recovered, replayed, err := service.RequestCancel(context.Background(), command)
		if err != nil || !replayed || !recovered.CancelRequested {
			t.Fatalf("durable child cancel recovery = %+v replayed=%t err=%v", recovered, replayed, err)
		}
		arguments, _ := json.Marshal(map[string]string{"job_id": child.SessionID})
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), fixture.call(cancelCallID, "dsh_job_cancel", string(arguments)), true)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("durable child cancel admission = %+v err=%v; want admitted", admission, err)
		}
	})
}

func TestTaskOperationDispatchGateAndRecoveryRespectSteerOrder(t *testing.T) {
	newOperation := func(t *testing.T, fixture s03AgentTaskFixture, callID string) task.Operation {
		t.Helper()
		return task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: fixture.task.ID, Key: app.AgentOperationKey(callID),
			RequestHash: hashBytes("S03 side-effect request:" + callID), HostID: fixture.host.ID,
			RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
			ControlVersion: fixture.host.ControlVersion, LeaseVersion: fixture.task.Lease.Version,
			RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationPrepared}
	}
	toDispatch := func(fixture s03AgentTaskFixture, operation task.Operation) app.OperationUpdate {
		return app.OperationUpdate{TaskID: fixture.task.ID, OperationKey: operation.Key, LeaseOwner: fixture.task.Lease.Owner,
			LeaseVersion: fixture.task.Lease.Version, RuntimeGeneration: fixture.host.RuntimeGeneration,
			RuntimeInstanceID: fixture.host.RuntimeInstanceID, ControlVersion: fixture.host.ControlVersion,
			RuntimeLeaseVersion: fixture.task.Lease.Version, Status: task.OperationDispatched}
	}

	t.Run("steer wins before prepared dispatch", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-operation-steer-first", true)
		operation := newOperation(t, fixture, "call_s03_operation_steer_first")
		prepared, replayed, err := fixture.route.store.CreateOperation(context.Background(), operation, fixture.task.Lease.Owner)
		if err != nil || replayed || prepared.Status != task.OperationPrepared {
			t.Fatalf("prepare operation = %+v replayed=%t err=%v", prepared, replayed, err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-operation-steer-first-message", "Change the requested action.", fixture.host.ID)
		_, err = fixture.route.store.UpdateOperation(context.Background(), toDispatch(fixture, prepared))
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("prepared→dispatched gate = %v pending=%+v", err, pending)
		}
		stored, err := fixture.route.store.GetOperation(context.Background(), fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed || stored.Result != app.TaskAgentToolSupersededOperationResult {
			t.Fatalf("superseded prepared operation = %+v err=%v", stored, err)
		}
		if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, message.ID); err != nil {
			t.Fatal(err)
		}
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), fixture.call("call_s03_operation_steer_first", "terminal_exec", `{}`), true)
		if err != nil || admission.Status != app.AgentToolCallSuperseded || len(admission.PendingMessages) != 1 ||
			admission.PendingMessages[0].ID != message.ID || admission.PendingMessages[0].Content != message.Content {
			t.Fatalf("post-commit applied-message recovery = %+v err=%v; want stored exact message ID and content", admission, err)
		}
	})

	t.Run("dispatch wins before steer", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-operation-dispatch-first", true)
		operation := newOperation(t, fixture, "call_s03_operation_dispatch_first")
		prepared, _, err := fixture.route.store.CreateOperation(context.Background(), operation, fixture.task.Lease.Owner)
		if err != nil {
			t.Fatal(err)
		}
		gate, err := fixture.route.store.UpdateOperation(context.Background(), toDispatch(fixture, prepared))
		if err != nil || gate.Status != task.OperationDispatched {
			t.Fatalf("prepared→dispatched before steer = %+v err=%v", gate, err)
		}
		acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-operation-dispatch-first-message", "Update the following turn.", fixture.host.ID)
		admission, err := fixture.route.store.AdmitAgentToolCall(context.Background(), fixture.call("call_s03_operation_dispatch_first", "terminal_exec", `{}`), true)
		if err != nil || admission.Status != app.AgentToolCallAdmitted {
			t.Fatalf("dispatched operation recovery after steer = %+v err=%v; want actual reconciliation", admission, err)
		}
		stored, err := fixture.route.store.GetOperation(context.Background(), fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationDispatched {
			t.Fatalf("dispatched operation after steer = %+v err=%v", stored, err)
		}
	})
}

func TestMCPHubAndFileFetchFinalDispatchGatesReturnPendingInput(t *testing.T) {
	t.Run("MCP operation", func(t *testing.T) {
		route := newRouteActionsTestFixture(t)
		ctx := context.Background()
		connectorID := mustID(t, domain.NewMCPConnectorID)
		if _, err := route.store.pool.Exec(ctx, `INSERT INTO mcp_connectors (id,owner_id,name,endpoint_url,enabled,credential_version,revision)
		VALUES ($1,$2,'S03 connector','https://connector.example.test/mcp',true,0,1)`, connectorID, route.ownerID); err != nil {
			t.Fatal(err)
		}
		fixture := createS03RunningAgentTask(t, route, "s03-mcp-steer-first", false, connectorID)
		message := seedS03PendingSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "Search for the updated question.")
		grant, err := fixture.route.store.GrantSelectedTaskMCPConnector(ctx, fixture.task, connectorID)
		if err != nil {
			t.Fatal(err)
		}
		callID := "call_s03_mcp_steer_first"
		operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: fixture.task.ID,
			OwnerID: fixture.task.OwnerID, ConnectorID: connectorID, Key: app.AgentOperationKey(callID), ToolName: "search",
			ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion, GrantVersion: grant.GrantVersion, LeaseVersion: fixture.task.Lease.Version,
			RequestHash: hashBytes("S03 MCP request"), Status: task.OperationPrepared}
		prepared, replayed, err := fixture.route.store.PrepareMCPConnectorOperation(ctx, operation, fixture.task.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare MCP operation = %+v replayed=%t err=%v", prepared, replayed, err)
		}
		_, err = fixture.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.task.Lease.Owner)
		var pending *app.TaskInputPendingError
		if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("MCP final dispatch gate = %v pending=%+v", err, pending)
		}
		stored, err := fixture.route.store.GetMCPConnectorOperation(ctx, fixture.task.OwnerID, fixture.task.ID, connectorID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed || stored.Result != app.TaskAgentToolSupersededOperationResult {
			t.Fatalf("MCP operation after rejected dispatch = %+v err=%v", stored, err)
		}
	})

	t.Run("Hub operation", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-hub-steer-first", false)
		ctx := context.Background()
		targetID := mustID(t, domain.NewClientInstallationID)
		if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'S03 Hub target','desktop',clock_timestamp()+interval '1 day')`, targetID, fixture.task.OwnerID); err != nil {
			t.Fatal(err)
		}
		callID := "call_s03_hub_steer_first"
		operation := app.TaskHubOperation{OwnerID: fixture.task.OwnerID, TaskID: fixture.task.ID, Key: app.AgentOperationKey(callID),
			ToolName: "morde_hub_files_" + targetID + "_v1", TargetInstallationID: targetID, Action: hub.ActionFiles,
			TaskAuthorizationVersion: fixture.task.Authorization.Version, TargetGrantVersion: 1, LeaseVersion: fixture.task.Lease.Version,
			RequestHash: hashBytes("S03 Hub request"), Status: task.OperationPrepared}
		prepared, replayed, err := fixture.route.store.PrepareTaskHubOperation(ctx, operation, fixture.task.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare Hub operation = %+v replayed=%t err=%v", prepared, replayed, err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-hub-steer-first-message", "Use a different Hub action.")
		if err := fixture.route.store.MarkTaskHubOperationDispatched(ctx, prepared, fixture.task.Lease.Owner); err == nil {
			t.Fatal("Hub dispatch gate accepted a stale operation after steer")
		} else {
			var pending *app.TaskInputPendingError
			if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
				t.Fatalf("Hub final dispatch gate = %v pending=%+v", err, pending)
			}
		}
		stored, err := fixture.route.store.GetTaskHubOperation(ctx, fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed || len(stored.ResultHash) != 32 {
			t.Fatalf("Hub operation after rejected dispatch = %+v err=%v", stored, err)
		}
	})

	t.Run("file.fetch authorization", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-file-fetch-steer-first", false)
		ctx := context.Background()
		if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, fixture.task.ID); err != nil {
			t.Fatal(err)
		}
		fixture.task.CurrentRevision, fixture.task.DesiredRevision = s03RevisionPointer(1), s03RevisionPointer(1)
		targetID := mustID(t, domain.NewClientInstallationID)
		if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'S03 file source','desktop',clock_timestamp()+interval '1 day')`, targetID, fixture.task.OwnerID); err != nil {
			t.Fatal(err)
		}
		callID := "call_s03_file_fetch_steer_first"
		operation := app.TaskHubOperation{OwnerID: fixture.task.OwnerID, TaskID: fixture.task.ID, Key: app.AgentOperationKey(callID),
			ToolName: "morde_hub_file_fetch_" + targetID + "_v5", TargetInstallationID: targetID, Action: hub.ActionFileFetch,
			TaskAuthorizationVersion: fixture.task.Authorization.Version, TargetGrantVersion: 5, TaskRevision: 1,
			LeaseVersion: fixture.task.Lease.Version, RequestHash: hashBytes("S03 file.fetch request"), Status: task.OperationPrepared}
		prepared, replayed, err := fixture.route.store.PrepareTaskHubOperation(ctx, operation, fixture.task.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare file.fetch Hub operation = %+v replayed=%t err=%v", prepared, replayed, err)
		}
		fetchID := mustID(t, domain.NewOperationID)
		fetch := app.TaskFileFetch{ID: fetchID, OwnerID: fixture.task.OwnerID, TaskID: fixture.task.ID,
			OperationKey: prepared.Key, SourceInstallationID: targetID, TargetGrantVersion: 5,
			TaskAuthorizationVersion: fixture.task.Authorization.Version, TaskRevision: 1, RelativePath: "reports/current.txt",
			Status: task.FileFetchQueued}
		if _, replayed, err := fixture.route.store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: fetch}, fixture.task.Lease.Owner); err != nil || replayed {
			t.Fatalf("create queued file.fetch = replayed:%t err:%v", replayed, err)
		}
		message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
			fixture.task.ID, "s03-file-fetch-steer-first-message", "Fetch the updated file.")
		ticketHash := sha256.Sum256([]byte("synthetic S03 ticket"))
		expires := time.Now().Add(time.Minute)
		dispatched, err := fixture.route.store.DispatchTaskFileFetch(ctx, prepared, fetchID, ticketHash[:], &expires, true, fixture.task.Lease.Owner)
		var pending *app.TaskInputPendingError
		if dispatched || !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
			t.Fatalf("file.fetch final dispatch gate = dispatched:%t err:%v pending:%+v", dispatched, err, pending)
		}
		if _, err := fixture.route.store.GetTaskFileFetch(ctx, fixture.task.OwnerID, fixture.task.ID, fetchID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("queued fetch after supersession = %v; want deleted without dispatch", err)
		}
		stored, err := fixture.route.store.GetTaskHubOperation(ctx, fixture.task.OwnerID, fixture.task.ID, operation.Key)
		if err != nil || stored.Status != task.OperationFailed {
			t.Fatalf("file.fetch Hub operation after rejected dispatch = %+v err=%v", stored, err)
		}
	})
}

func acceptS03TaskSteer(t *testing.T, store *Store, ownerID, conversationID, taskID, clientID, content string, hostIDs ...string) app.AgentConversationMessage {
	t.Helper()
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(ownerID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'S03 route test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())
ON CONFLICT (id) DO NOTHING`, ownerID); err != nil {
		t.Fatal(err)
	}
	fixture := routeActionsTestFixture{store: store, ownerID: ownerID, instanceID: instanceID,
		conversation:  conversation.Conversation{ID: conversationID},
		authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}}
	input := app.ConversationRoutingInput{ReferenceTaskID: taskID}
	if len(hostIDs) > 0 {
		input.HostID = hostIDs[0]
	}
	claim := fixture.receiveInConversation(t, conversationID, clientID, content, input)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatalf("load steer route snapshot: %v", err)
	}
	var revision int64
	found := false
	for _, candidate := range snapshot.Context.Candidates {
		if candidate.TaskID == taskID {
			revision, found = candidate.Revision, true
			break
		}
	}
	if !found {
		t.Fatalf("steer route did not include target Task %s: %+v", taskID, snapshot.Context.Candidates)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteSteer, TaskID: taskID, ExpectedRevision: revision,
		ModelOutput: `{"action":"steer","task_id":"` + taskID + `","acknowledge":false}`,
	}); err != nil {
		t.Fatalf("accept Task steer: %v", err)
	}
	var message app.AgentConversationMessage
	var mode string
	if err := store.pool.QueryRow(ctx, `SELECT id,content,sequence,delivery_mode FROM conversation_messages WHERE id=$1
 AND delivery_status='pending' AND task_id=$2`, claim.Message.ID, taskID).
		Scan(&message.ID, &message.Content, &message.Sequence, &mode); err != nil {
		var persistedTaskID, persistedStatus, persistedMode, routeState string
		_ = store.pool.QueryRow(ctx, `SELECT COALESCE(task_id,''),delivery_status,delivery_mode FROM conversation_messages WHERE id=$1`,
			claim.Message.ID).Scan(&persistedTaskID, &persistedStatus, &persistedMode)
		_ = store.pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, claim.Message.ID).Scan(&routeState)
		t.Fatalf("read accepted Task steer: %v; persisted task=%q status=%s mode=%s route=%s expected=%s", err,
			persistedTaskID, persistedStatus, persistedMode, routeState, taskID)
	}
	message.Mode = app.AgentMessageDeliveryMode(mode)
	return message
}

func seedS03PendingSteer(t *testing.T, store *Store, ownerID, conversationID, taskID, content string) app.AgentConversationMessage {
	t.Helper()
	message := seedLegacyPendingTaskMessageForTest(t, context.Background(), store, ownerID, conversationID, taskID, content, nil)
	if _, err := store.pool.Exec(context.Background(), `UPDATE conversation_messages SET delivery_mode='steer' WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	return app.AgentConversationMessage{ID: message.ID, Content: message.Content, Sequence: message.Sequence,
		Mode: app.AgentMessageDeliverySteer}
}

func saveS03RootCheckpoint(t *testing.T, fixture s03AgentTaskFixture, from, through int64, message *app.AgentConversationMessage) {
	t.Helper()
	ctx := context.Background()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(fixture.route.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentCheckpointFence{TaskID: fixture.task.ID, OwnerID: fixture.task.OwnerID, InitiatorID: fixture.task.InitiatorID,
		AgentInstanceID: fixture.fence.AgentInstanceID, AgentGeneration: fixture.fence.AgentGeneration,
		AgentSessionID: fixture.fence.AgentSessionID, AgentRootSessionID: fixture.fence.AgentSessionID,
		AuthorizationVersion: fixture.fence.AuthorizationVersion, LeaseOwner: fixture.fence.LeaseOwner,
		LeaseVersion: fixture.fence.LeaseVersion, EngineVersion: fixture.fence.EngineVersion, ProfileVersion: fixture.fence.ProfileVersion}
	if _, err := checkpoints.PendingMessages(ctx, fence); err != nil {
		t.Fatalf("bind pending S03 checkpoint messages: %v", err)
	}
	events := make([]any, 0, through-from+1)
	for sequence := from; sequence <= through; sequence++ {
		if message != nil && sequence == from {
			var content any = message.Content
			if len(message.AttachmentIDs) > 0 {
				content = []map[string]string{{"type": "text", "text": message.Content},
					{"type": "text", "text": "[MORDE_TASK_ATTACHMENT_IDS_V1:" + strings.Join(message.AttachmentIDs, ",") + "]"}}
			}
			events = append(events, map[string]any{"seq": sequence, "type": "agent/inbox/spliced", "data": map[string]any{
				"target": "next-step", "start": 0, "inserted": []any{map[string]any{"id": message.ID, "role": "user",
					"content": content, "source": map[string]string{"kind": "user"}}},
			}})
			continue
		}
		if message != nil && sequence == through {
			events = append(events, map[string]any{"seq": sequence, "type": "user/message", "data": map[string]any{
				"id": message.ID, "role": "user", "content": message.Content,
			}})
			continue
		}
		events = append(events, map[string]any{"seq": sequence, "type": "agent/state", "data": map[string]string{"state": "active"}})
	}
	payload, err := json.Marshal(map[string]any{"header": map[string]string{"id": fence.AgentSessionID}, "from_seq": from, "events": events})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: from, Through: through, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("save S03 root checkpoint through %d: %v", through, err)
	}
}

func s03ChildToolFence(fixture s03AgentTaskFixture, child app.AgentChildSession, throughSequence int64) app.ModelTaskFence {
	fence := fixture.modelFence
	fence.AgentSessionID = child.SessionID
	fence.AgentRootSessionID = fixture.fence.AgentSessionID
	fence.ThroughSequence = throughSequence
	return fence
}

func attachS03WorkspaceToTask(t *testing.T, store *Store, claimed task.Task, taskName string) (workspace.Workspace, app.TaskWorkspacePublishCommand) {
	t.Helper()
	ctx := context.Background()
	workspaceValue := createTestWorkspace(t, ctx, store, claimed.OwnerID, taskName)
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, claimed.OwnerID, host.ControlAI, 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, taskWorkspacePublishRuntimeID); err != nil {
		t.Fatal(err)
	}
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: claimed.OwnerID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2,workspace_id=$3,workspace_owner_id=$6,input_revision=$4,
workspace_checkpoint_revision=$4,context_digest=$5 WHERE id=$1`, claimed.ID, hostValue.ID, workspaceValue.ID,
		workspaceValue.Revision, contextDigest, claimed.OwnerID); err != nil {
		t.Fatalf("attach Workspace to routed Task: %v", err)
	}
	requestHash := sha256.Sum256([]byte("S03 Workspace publish:" + taskName))
	command := app.TaskWorkspacePublishCommand{
		OwnerID: claimed.OwnerID, TaskID: claimed.ID, WorkspaceID: workspaceValue.ID, HostID: hostValue.ID,
		InitiatorID: claimed.InitiatorID,
		LeaseOwner:  claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		BindingVersion: binding.BindingVersion, InputRevision: workspaceValue.Revision, BaseRevision: workspaceValue.Revision,
		HostFence: app.AgentHostFence{RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: taskWorkspacePublishRuntimeID,
			ControlVersion: hostValue.ControlVersion, AIAllowed: true},
		ViewID: "v" + strings.Repeat("a", 64), InputManifestSHA256: manifestSHA,
		OperationKey: "task-workspace-publish:s1234567:" + workspaceValue.ID, RequestHash: requestHash[:],
	}
	return workspaceValue, command
}

func TestCommitTaskProgressCompletionReturnsLockedPendingInputSnapshot(t *testing.T) {
	fixture := newRouteActionsTestFixture(t)
	claimed, taskID := createS03RunningTask(t, fixture, "s03-completion-initial")
	message := acceptS03TaskSteer(t, fixture.store, fixture.ownerID, claimed.ConversationID, taskID,
		"s03-completion-steer", "Change the answer to include the new requirement.")
	stepID := mustID(t, domain.NewStepID)
	payload, err := json.Marshal(map[string]string{"status": string(task.StatusCompleted), "source": "s03-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.store.CommitTaskProgress(context.Background(), app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "stale answer", Usage: claimed.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded,
			Title: "Complete Task", Summary: "Old completion candidate."}},
		PublishResultMessage: true,
		Event:                task.Event{TaskID: claimed.ID, Type: "task.result", Payload: payload},
	})
	var pending *app.TaskInputPendingError
	if !errors.As(err, &pending) || !errors.Is(err, app.ErrTaskInputPending) || len(pending.PendingMessages) != 1 {
		t.Fatalf("completion result = err:%v pending:%+v; want typed pending-input snapshot", err, pending)
	}
	got := pending.PendingMessages[0]
	if got.ID != message.ID || got.Content != message.Content || got.Mode != app.AgentMessageDeliverySteer || got.Sequence != message.Sequence {
		t.Fatalf("completion pending snapshot = %+v; want exact accepted steer %+v", got, message)
	}
	details, err := fixture.store.GetTask(context.Background(), claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.Status != task.StatusRunning || details.Task.Result != "" {
		t.Fatalf("Task after rejected completion = status:%s result:%q err:%v", details.Task.Status, details.Task.Result, err)
	}
	var resultEvents, assistantMessages int
	if err := fixture.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, taskID).Scan(&resultEvents); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND role='assistant'`, taskID).Scan(&assistantMessages); err != nil {
		t.Fatal(err)
	}
	if resultEvents != 0 || assistantMessages != 0 {
		t.Fatalf("rejected completion leaked result event/message: events=%d assistant_messages=%d", resultEvents, assistantMessages)
	}
}

func TestCommitTaskProgressCompletionFenceRejectsAppliedSteerAfterCandidate(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "s03-completion-applied-steer", false)
	ctx := context.Background()
	saveS03RootCheckpoint(t, fixture, 0, 0, nil)
	var checkpointDigest []byte
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT checkpoint_digest FROM tasks WHERE id=$1`, fixture.task.ID).Scan(&checkpointDigest); err != nil || len(checkpointDigest) != 32 {
		t.Fatalf("read completion candidate checkpoint digest: %x err=%v", checkpointDigest, err)
	}
	message := acceptS03TaskSteer(t, fixture.route.store, fixture.task.OwnerID, fixture.task.ConversationID,
		fixture.task.ID, "s03-completion-applied-steer-message", "Replace the result with the new requirement.")
	saveS03RootCheckpoint(t, fixture, 1, 2, &message)
	stepID := mustID(t, domain.NewStepID)
	var stepSequence int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, fixture.task.ID).Scan(&stepSequence); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"status": string(task.StatusCompleted), "source": "s03-completion-fence"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.route.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: fixture.task.ID, LeaseOwner: fixture.task.Lease.Owner, LeaseVersion: fixture.task.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "stale pre-steer result", Usage: fixture.task.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: fixture.task.ID, Sequence: stepSequence, Kind: "result", Status: task.StepSucceeded,
			Title: "Complete Task", Summary: "Candidate built before the applied steer."}},
		PublishResultMessage: true,
		AgentCompletionFence: &app.TaskAgentCompletionFence{AgentSessionID: fixture.fence.AgentSessionID,
			AgentInstanceID: fixture.fence.AgentInstanceID, AgentGeneration: fixture.fence.AgentGeneration,
			AuthorizationVersion: fixture.fence.AuthorizationVersion, CheckpointThroughSequence: 0,
			CheckpointDigest: checkpointDigest},
		Event: task.Event{TaskID: fixture.task.ID, Type: "task.result", Payload: payload},
	})
	var pending *app.TaskInputPendingError
	if !errors.As(err, &pending) || !errors.Is(err, app.ErrTaskInputPending) || len(pending.PendingMessages) != 1 ||
		pending.PendingMessages[0].ID != message.ID || pending.PendingMessages[0].Content != message.Content {
		t.Fatalf("applied-steer completion result = err:%v pending:%+v; want typed exact snapshot", err, pending)
	}
	details, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || details.Task.Status != task.StatusRunning || details.Task.Result != "" {
		t.Fatalf("Task after applied-steer completion gate = status:%s result:%q err:%v", details.Task.Status, details.Task.Result, err)
	}
	var resultEvents, assistantMessages int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, fixture.task.ID).Scan(&resultEvents); err != nil {
		t.Fatal(err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND role='assistant'`, fixture.task.ID).Scan(&assistantMessages); err != nil {
		t.Fatal(err)
	}
	if resultEvents != 0 || assistantMessages != 0 {
		t.Fatalf("applied-steer completion published stale result: events=%d assistant_messages=%d", resultEvents, assistantMessages)
	}
}

func TestTaskWorkspacePublishFinalGateStoresAndReplaysSupersedingMessageIDs(t *testing.T) {
	fixture := newRouteActionsTestFixture(t)
	store := fixture.store
	ctx := context.Background()
	claimed, taskID := createS03RunningTask(t, fixture, "s03-steer-publish-initial")
	message := acceptS03TaskSteer(t, store, claimed.OwnerID, claimed.ConversationID, taskID,
		"s03-publish-steer", "Before publishing, change the report.")
	workspaceValue, command := attachS03WorkspaceToTask(t, store, claimed, "s03-steer-publish")
	service, _ := taskWorkspacePublishTestService(t, store)
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatalf("reserve Task Workspace publish before steer: %v", err)
	}
	file := syncTestFile("reports/s03.txt", []byte("candidate output"))
	files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	reads := 0
	_, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA, func(_ context.Context, digest string) ([]byte, error) {
		reads++
		if digest != file.SHA256 {
			t.Fatalf("requested block digest=%s want=%s", digest, file.SHA256)
		}
		return []byte("candidate output"), nil
	})
	var pending *app.TaskInputPendingError
	if !errors.As(err, &pending) || len(pending.PendingMessages) != 1 || pending.PendingMessages[0].ID != message.ID {
		t.Fatalf("Workspace publish = err:%v pending:%+v; want typed supersession", err, pending)
	}
	if reads != 1 {
		t.Fatalf("manifest block reads before commit gate = %d; want one verification read and zero commits", reads)
	}
	receipt, err := store.FindTaskWorkspacePublishReceipt(ctx, claimed.OwnerID, claimed.ID, workspaceValue.ID, command.OperationKey)
	if err != nil || receipt.State != app.TaskWorkspacePublishSuperseded ||
		len(receipt.SupersedingMessages) != 1 || receipt.SupersedingMessages[0].ID != message.ID {
		t.Fatalf("superseded Workspace receipt = %+v err=%v", receipt, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	_, replayErr := store.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA)
	var replayPending *app.TaskInputPendingError
	if !errors.As(replayErr, &replayPending) || len(replayPending.PendingMessages) != 1 || replayPending.PendingMessages[0].ID != message.ID {
		t.Fatalf("replayed Workspace publish = err:%v pending:%+v; want stored ID snapshot after apply", replayErr, replayPending)
	}
	currentWorkspace, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || currentWorkspace.Revision != 0 {
		t.Fatalf("Workspace revision after superseded publish = %d err=%v; want no commit", currentWorkspace.Revision, err)
	}
}

func TestTaskWorkspacePublishThenSteerKeepsSingleCommittedReceipt(t *testing.T) {
	fixture := newRouteActionsTestFixture(t)
	store := fixture.store
	ctx := context.Background()
	claimed, taskID := createS03RunningTask(t, fixture, "s03-publish-before-steer-initial")
	workspaceValue, command := attachS03WorkspaceToTask(t, store, claimed, "s03-publish-before-steer")
	service, _ := taskWorkspacePublishTestService(t, store)
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatalf("reserve Task Workspace publish before commit: %v", err)
	}
	file := syncTestFile("reports/s03.txt", []byte("committed output"))
	files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	reads := 0
	first, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA, func(_ context.Context, digest string) ([]byte, error) {
		reads++
		if digest != file.SHA256 {
			t.Fatalf("requested block digest=%s want=%s", digest, file.SHA256)
		}
		return []byte("committed output"), nil
	})
	if err != nil || first.State != app.TaskWorkspacePublishSucceeded {
		t.Fatalf("first Workspace publish = %+v err=%v", first, err)
	}
	message := acceptS03TaskSteer(t, store, claimed.OwnerID, claimed.ConversationID, taskID,
		"s03-publish-after-steer", "Revise the next version after this publish.")
	second, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA, func(context.Context, string) ([]byte, error) {
		t.Fatalf("replayed committed receipt unexpectedly read a block after steer %s", message.ID)
		return nil, nil
	})
	if err != nil || second.State != app.TaskWorkspacePublishSucceeded || second.PublishedRevision != first.PublishedRevision || reads != 1 {
		t.Fatalf("replayed committed Workspace publish = %+v first=%+v reads=%d err=%v", second, first, reads, err)
	}
	currentWorkspace, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || currentWorkspace.Revision != 1 {
		t.Fatalf("Workspace revision after replay = %d err=%v; want one committed revision", currentWorkspace.Revision, err)
	}
}
