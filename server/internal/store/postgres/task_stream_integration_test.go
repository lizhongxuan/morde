package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

type cloudDSHStreamFixture struct {
	route          routeActionsTestFixture
	task           task.Task
	delta          app.CloudDSHTextDelta
	instanceID     string
	conversationID string
}

func TestCloudDSHStreamRechecksSharedHostGrantWithoutRequiringHostOnline(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	hostOwner := createPreRetirementTestUser(t, f.route.store, 4, 2000, 4096)
	computer := createRunningControlHost(t, ctx, f.route.store, hostOwner.ID, host.ControlAI, 0)
	computer = setAgentTestHostRuntime(t, f.route.store, computer, "stream-shared-host")
	grant := host.Member{HostID: computer.ID, OwnerID: hostOwner.ID, UserID: f.task.InitiatorID,
		Permissions: []host.Permission{host.PermissionDesktopControl}}
	if _, err := f.route.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	// Model a Host selected after the Agent was started, using the same grant
	// capture as the production one-way binding transaction.
	if err := f.route.store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, f.task.ID, computer.ID); err != nil {
			return err
		}
		target, err := lockAgentHostTarget(ctx, tx, f.task.ID, f.task.OwnerID)
		if err != nil {
			return err
		}
		hostFence, err := captureAgentHostFence(ctx, tx, agentTaskLock{HostID: computer.ID, InitiatorID: f.task.InitiatorID}, target)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_instances SET host_id=$3,host_owner_id=$4,host_actor_id=$5,
host_permission=$6,host_authorization_kind=$7,host_runtime_generation=$8,host_runtime_instance_id=$9,
host_control_version=$10,host_grant_fingerprint=$11 WHERE task_id=$1 AND agent_generation=$2`,
			f.task.ID, f.delta.Fence.AgentGeneration, hostFence.HostID, hostFence.OwnerID, hostFence.ActorID,
			hostFence.Permission, hostFence.AuthorizationKind, hostFence.RuntimeGeneration, hostFence.RuntimeInstanceID,
			hostFence.ControlVersion, hostFence.GrantFingerprint)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, computer.ID); err != nil {
		t.Fatal(err)
	}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("offline Host incorrectly blocked an authorized text delta: appended=%t err=%v", appended, err)
	}
	if err := f.route.store.RevokeHostMember(ctx, computer.ID, hostOwner.ID, f.task.InitiatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.UpsertHostMember(ctx, grant); err != nil {
		t.Fatal(err)
	}
	continued := f.delta
	continued.Text = "untrusted after regrant"
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, continued); err == nil || appended {
		t.Fatalf("regranted Host resumed an old stream: appended=%t err=%v", appended, err)
	}
	var content string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1`, f.delta.MessageID).
		Scan(&content); err != nil || content != f.delta.Text {
		t.Fatalf("rejected stream changed the previously visible fragment: %q err=%v", content, err)
	}
}

func newCloudDSHStreamFixture(t *testing.T) cloudDSHStreamFixture {
	t.Helper()
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	initial := f.receive(t, "stream-original-message", "Explain this topic", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "stream-test-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim streaming task: found=%t err=%v", found, err)
	}
	key := make([]byte, 32)
	key[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("stream-test-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	lease, _, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "stream_agent",
		AgentSessionID: "stream_session", SupervisorID: "stream-test-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		t.Fatal(err)
	}
	checkpointFence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration, AgentSessionID: lease.Fence.AgentSessionID,
		AuthorizationVersion: lease.Fence.AuthorizationVersion, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"header": map[string]any{"id": lease.Fence.AgentSessionID}, "from_seq": 0, "events": []any{
		map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{"id": initial.Message.ID, "role": "user",
			"content": []any{map[string]string{"type": "text", "text": initial.Message.Content}}}},
		map[string]any{"seq": 1, "type": "turn/start", "data": map[string]any{"turn": 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: checkpointFence, From: 0, Through: 1, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatal(err)
	}
	modelFence := app.ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AgentRootSessionID: lease.Fence.AgentSessionID,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		ThroughSequence: 1, EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	callID, err := app.DSHModelCallID(claimed.ID, lease.Fence.AgentSessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,personal_model)
VALUES ($1,'task',$2,$3,'p1234567','test',$4,100,'dispatched',true)`, claimed.InitiatorID, callID, make([]byte, 32), claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_text_streaming_dispatches
(owner_id,source,call_id,task_id,task_owner_id,initiator_id,model_id,connection_version,task_revision,
 agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,lease_owner,lease_version,authorization_version,through_sequence,engine_version,profile_version)
SELECT $1,'task',$2,$3,$4,$1,'p1234567',updated_at,0,$5,$6,$7,$7,$8,$9,$10,1,$11,$12 FROM model_connections WHERE id='p1234567'`,
		claimed.InitiatorID, callID, claimed.ID, claimed.OwnerID, lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration,
		lease.Fence.AgentSessionID, lease.Fence.LeaseOwner, lease.Fence.LeaseVersion, lease.Fence.AuthorizationVersion,
		lease.Fence.EngineVersion, lease.Fence.ProfileVersion); err != nil {
		t.Fatal(err)
	}
	return cloudDSHStreamFixture{route: f, task: claimed, instanceID: f.instanceID, conversationID: f.conversation.ID,
		delta: app.CloudDSHTextDelta{Fence: modelFence, CallID: callID, MessageID: mustID(t, domain.NewMessageID),
			TurnID: mustID(t, domain.NewStepID), Text: "Hello "}}
}

func TestCloudDSHStreamReusesVisibleMessageForFinalResult(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("first stream delta: appended=%t err=%v", appended, err)
	}
	var firstSequence int64
	var firstStatus, firstContent string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT sequence,output_status,content FROM conversation_messages WHERE id=$1`, f.delta.MessageID).
		Scan(&firstSequence, &firstStatus, &firstContent); err != nil || firstStatus != "streaming" || firstContent != f.delta.Text {
		t.Fatalf("first visible message: sequence=%d status=%s content=%q err=%v", firstSequence, firstStatus, firstContent, err)
	}
	// A later independent message may arrive after streaming became visible.
	// The established stream keeps its identity and is not merged retroactively.
	later := f.route.receive(t, "stream-later-message", "Another task", app.ConversationRoutingInput{})
	if later.Message.Sequence <= firstSequence {
		t.Fatalf("stream did not retain its order: stream=%d later=%d", firstSequence, later.Message.Sequence)
	}
	before, err := f.route.store.LoadConversationRouteSnapshot(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range before.Context.History {
		if item.MessageID == f.delta.MessageID {
			t.Fatal("an unfinished stream entered route model history")
		}
	}
	second := f.delta
	second.Text = "world"
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); err != nil || !appended {
		t.Fatalf("continued visible stream: appended=%t err=%v", appended, err)
	}
	step := task.Step{ID: f.delta.TurnID, TaskID: f.task.ID, Sequence: 2, Kind: "result", Status: task.StepSucceeded, Title: "Result"}
	if _, err := f.route.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: f.task.ID, LeaseOwner: f.task.Lease.Owner,
		LeaseVersion: f.task.Lease.Version, Status: task.StatusCompleted, CurrentStep: step.ID, Result: "Hello world",
		Steps: []task.Step{step}, PublishResultMessage: true, Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"Hello world"}`)}}); err != nil {
		t.Fatal(err)
	}
	var messageID, status, content string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT message_id,status,content FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).
		Scan(&messageID, &status, &content); err != nil || messageID != f.delta.MessageID || status != "completed" || content != "Hello world" {
		t.Fatalf("final stream identity changed: message=%s status=%s content=%q err=%v", messageID, status, content, err)
	}
	var finalSequence, revision, eventSequence int64
	if err := f.route.store.pool.QueryRow(ctx, `SELECT sequence,output_status,content,output_task_revision,output_result_event_sequence
FROM conversation_messages WHERE task_id=$1 AND role='assistant' AND id=$2`, f.task.ID, f.delta.MessageID).
		Scan(&finalSequence, &status, &content, &revision, &eventSequence); err != nil || finalSequence != firstSequence || status != "completed" || content != "Hello world" || revision != 0 || eventSequence < 1 {
		t.Fatalf("final stream row: sequence=%d status=%s content=%q revision=%d event=%d err=%v", finalSequence, status, content, revision, eventSequence, err)
	}
	page, err := f.route.store.ListMessages(ctx, f.task.OwnerID, f.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var listed *conversation.Message
	for i := range page.Items {
		if page.Items[i].ID == f.delta.MessageID {
			listed = &page.Items[i]
		}
	}
	if listed == nil || listed.OutputStatus != conversation.OutputCompleted || listed.OutputTaskRevision == nil || *listed.OutputTaskRevision != 0 ||
		listed.OutputResultEventSequence == nil || *listed.OutputResultEventSequence != eventSequence || listed.Sequence != firstSequence {
		t.Fatalf("list lost stream projection: %+v", listed)
	}
	// Later revisions may replace the current snapshot; the original completed
	// message remains valid only through its own matching result-event evidence.
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE task_assistant_message_snapshots SET message_id=$2,status='interrupted',message_sequence=NULL,result_event_sequence=NULL
WHERE task_id=$1`, f.task.ID, mustID(t, domain.NewMessageID)); err != nil {
		t.Fatal(err)
	}
	after, err := f.route.store.LoadConversationRouteSnapshot(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range after.Context.History {
		found = found || item.MessageID == f.delta.MessageID
	}
	if !found {
		t.Fatal("completed stream was lost when the current snapshot changed")
	}
	if err := f.route.store.WithTx(ctx, func(tx pgx.Tx) error {
		history, err := readTaskConversationContextMessages(ctx, tx, f.task.OwnerID, f.conversationID, later.Message.Sequence-1, nil, true)
		if err != nil {
			return err
		}
		for _, item := range history {
			if item.ID == f.delta.MessageID {
				return nil
			}
		}
		t.Error("completed stream was lost from Task context")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tamper := range []struct {
		name, query string
	}{
		{"content", `UPDATE conversation_messages SET content='changed after completion' WHERE id=$1`},
		{"event type", `UPDATE task_events SET event_type='task.step' WHERE task_id=$1 AND sequence=$2`},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			if tamper.name == "content" {
				if _, err := f.route.store.pool.Exec(ctx, tamper.query, f.delta.MessageID); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, _ = f.route.store.pool.Exec(ctx, `UPDATE conversation_messages SET content='Hello world' WHERE id=$1`, f.delta.MessageID)
				}()
			} else {
				if _, err := f.route.store.pool.Exec(ctx, tamper.query, f.task.ID, eventSequence); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, _ = f.route.store.pool.Exec(ctx, `UPDATE task_events SET event_type='task.result' WHERE task_id=$1 AND sequence=$2`, f.task.ID, eventSequence)
				}()
			}
			snapshot, err := f.route.store.LoadConversationRouteSnapshot(ctx, later)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range snapshot.Context.History {
				if item.MessageID == f.delta.MessageID {
					t.Fatal("changed result was admitted to route history")
				}
			}
			if err := f.route.store.WithTx(ctx, func(tx pgx.Tx) error {
				history, err := readTaskConversationContextMessages(ctx, tx, f.task.OwnerID, f.conversationID, later.Message.Sequence-1, nil, true)
				if err != nil {
					return err
				}
				for _, item := range history {
					if item.ID == f.delta.MessageID {
						t.Error("changed result was admitted to Task history")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE task_events SET task_revision=1 WHERE task_id=$1 AND sequence=$2`, f.task.ID, eventSequence); err == nil {
		t.Fatal("completed output allowed its result revision to change")
	}
}

func TestCloudDSHStreamFirstDeltaRejectsCompetingConversationWork(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	f.route.receive(t, "stream-competing-message", "An unrelated task", app.ConversationRoutingInput{})
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || appended {
		t.Fatalf("competing work allowed first stream: appended=%t err=%v", appended, err)
	}
	var snapshots int
	if err := f.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatalf("suppressed stream created a snapshot: count=%d err=%v", snapshots, err)
	}
}

func TestCloudDSHStreamFencesPersonalProofRevisionAndModelVersion(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	if _, err := f.route.store.pool.Exec(ctx, `INSERT INTO personal_objects(service_instance_id,owner_id,object_type,object_id,body)
VALUES ($1::uuid,$2,$3,$4,'{"name":"Synthetic"}'::jsonb)`, f.instanceID, f.task.OwnerID, personal.ObjectTypeProfile, personal.ProfileObjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `INSERT INTO account_object_versions(service_instance_id,owner_id,object_type,object_id,revision,is_deleted)
VALUES ($1::uuid,$2,$3,$4,1,false)`, f.instanceID, f.task.OwnerID, personal.ObjectTypeProfile, personal.ProfileObjectID); err != nil {
		t.Fatal(err)
	}
	f.delta.PersonalProof = &app.CloudDSHStreamPersonalProof{Enabled: true, SettingRevision: 0, Objects: []app.CloudDSHMemoryVersion{
		{ObjectType: personal.ObjectTypeProfile, ID: personal.ProfileObjectID, Revision: 1},
	}}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("valid personal proof rejected: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE account_object_versions SET revision=2 WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
		f.instanceID, f.task.OwnerID, personal.ObjectTypeProfile, personal.ProfileObjectID); err != nil {
		t.Fatal(err)
	}
	second := f.delta
	second.Text = "stale"
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); appended || domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed personal data allowed old text: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE account_object_versions SET revision=1 WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4`,
		f.instanceID, f.task.OwnerID, personal.ObjectTypeProfile, personal.ProfileObjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	page, err := f.route.store.ListMessages(ctx, f.task.OwnerID, f.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var projected conversation.OutputStatus
	for _, message := range page.Items {
		if message.ID == f.delta.MessageID {
			projected = message.OutputStatus
		}
	}
	if projected != conversation.OutputInterrupted {
		t.Fatalf("stale revision appeared live: %s", projected)
	}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); appended || domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old revision allowed text: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=0 WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	forged := second
	forged.Fence.AgentGeneration++
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, forged); appended || domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("forged Agent allowed text: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.route.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); appended || domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked task credential allowed text: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, f.route.authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE model_connections SET updated_at=clock_timestamp() WHERE id='p1234567'`); err != nil {
		t.Fatal(err)
	}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); appended || domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("changed model connection allowed old text: appended=%t err=%v", appended, err)
	}
}

func TestCloudDSHStreamDefaultsPersonalMemoryOnAndFencesOptOut(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	f.delta.PersonalProof = &app.CloudDSHStreamPersonalProof{Enabled: true, SettingRevision: 0, Objects: []app.CloudDSHMemoryVersion{}}
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("missing preference should use default-on personal memory: appended=%t err=%v", appended, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `INSERT INTO personal_objects(service_instance_id,owner_id,object_type,object_id,body)
VALUES ($1::uuid,$2,$3,'builtin_memory','{"enabled":false}'::jsonb)`, f.instanceID, f.task.OwnerID, personal.ObjectTypeSkillSetting); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `INSERT INTO account_object_versions(service_instance_id,owner_id,object_type,object_id,revision,is_deleted)
VALUES ($1::uuid,$2,$3,'builtin_memory',1,false)`, f.instanceID, f.task.OwnerID, personal.ObjectTypeSkillSetting); err != nil {
		t.Fatal(err)
	}
	second := f.delta
	second.Text = "should be rejected after memory opt-out"
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, second); appended || domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("explicit memory opt-out did not fence the prior stream: appended=%t err=%v", appended, err)
	}
}

func TestCloudDSHStreamRevisionAndCancelKeepInterruptedText(t *testing.T) {
	for _, action := range []string{"revision", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newCloudDSHStreamFixture(t)
			ctx := context.Background()
			if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
				t.Fatalf("initial text: appended=%t err=%v", appended, err)
			}
			if action == "revision" {
				change := f.route.receive(t, "stream-revision-message", "Change the answer", app.ConversationRoutingInput{ReferenceTaskID: f.task.ID})
				snapshot, err := f.route.store.LoadConversationRouteSnapshot(ctx, change)
				if err != nil || len(snapshot.Context.Candidates) != 1 {
					t.Fatalf("revision candidate: %+v err=%v", snapshot.Context.Candidates, err)
				}
				snapshot.Context.ModelID = "p1234567"
				if err := f.route.store.ApplyConversationRoute(ctx, change, snapshot, app.ConversationRouteDecision{
					Action: app.ConversationRouteUpdate, TaskID: f.task.ID, ExpectedRevision: snapshot.Context.Candidates[0].Revision,
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				requestID := mustID(t, domain.NewTaskActionID)
				record := taskActionIdempotency(f.task.InitiatorID, task.ActionCancel, "stream-cancel-action", f.task.ID, task.StatusRunning, requestID)
				if _, err := f.route.store.RequestTaskAction(ctx, record, f.task.ID, task.ActionCancel, task.StatusRunning, nil, requestID); err != nil {
					t.Fatal(err)
				}
			}
			var messageID, content, status string
			if err := f.route.store.pool.QueryRow(ctx, `SELECT message_id,content,status FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).
				Scan(&messageID, &content, &status); err != nil || messageID != f.delta.MessageID || content != f.delta.Text || status != "interrupted" {
				t.Fatalf("stopped stream was not retained: id=%s content=%q status=%s err=%v", messageID, content, status, err)
			}
			if err := f.route.store.pool.QueryRow(ctx, `SELECT content,output_status FROM conversation_messages WHERE id=$1`, messageID).
				Scan(&content, &status); err != nil || content != f.delta.Text || status != "interrupted" {
				t.Fatalf("stopped conversation message was not retained: content=%q status=%s err=%v", content, status, err)
			}
			if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); appended || err == nil {
				t.Fatalf("stopped execution accepted another delta: appended=%t err=%v", appended, err)
			}
		})
	}
}

func TestCloudDSHStreamArchivesInterruptedPriorRevision(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("initial stream: appended=%t err=%v", appended, err)
	}
	if err := f.route.store.WithTx(ctx, func(tx pgx.Tx) error {
		return interruptAssistantMessageSnapshot(ctx, tx, f.task.ID, "", true)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE model_text_streaming_dispatches SET task_revision=1 WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		f.task.InitiatorID, f.delta.CallID); err != nil {
		t.Fatal(err)
	}
	next := f.delta
	next.MessageID = mustID(t, domain.NewMessageID)
	next.TurnID = mustID(t, domain.NewStepID)
	next.Text = "New revision"
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, next); err != nil || !appended {
		t.Fatalf("new revision failed to stream: appended=%t err=%v", appended, err)
	}
	var id, status, content string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT message_id,status,content FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).
		Scan(&id, &status, &content); err != nil || id != next.MessageID || status != "streaming" || content != next.Text {
		t.Fatalf("new stream snapshot: id=%s status=%s content=%q err=%v", id, status, content, err)
	}
	var oldSequence, newSequence, oldRevision, newRevision int64
	if err := f.route.store.pool.QueryRow(ctx, `SELECT sequence,output_status,content,output_task_revision FROM conversation_messages WHERE id=$1`, f.delta.MessageID).
		Scan(&oldSequence, &status, &content, &oldRevision); err != nil || status != "interrupted" || content != f.delta.Text || oldRevision != 0 {
		t.Fatalf("old conversation stream changed: status=%s content=%q revision=%d err=%v", status, content, oldRevision, err)
	}
	if err := f.route.store.pool.QueryRow(ctx, `SELECT sequence,output_status,content,output_task_revision FROM conversation_messages WHERE id=$1`, next.MessageID).
		Scan(&newSequence, &status, &content, &newRevision); err != nil || status != "streaming" || content != next.Text || newRevision != 1 || newSequence <= oldSequence {
		t.Fatalf("new conversation stream: status=%s content=%q revision=%d oldSequence=%d newSequence=%d err=%v", status, content, newRevision, oldSequence, newSequence, err)
	}
}

func TestCloudDSHStreamDoesNotDiscardLegacyUnarchivedSnapshot(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); err != nil || !appended {
		t.Fatalf("initial stream: appended=%t err=%v", appended, err)
	}
	if err := f.route.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := interruptAssistantMessageSnapshot(ctx, tx, f.task.ID, "", true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM conversation_messages WHERE id=$1`, f.delta.MessageID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE model_text_streaming_dispatches SET task_revision=1 WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		f.task.InitiatorID, f.delta.CallID); err != nil {
		t.Fatal(err)
	}
	next := f.delta
	next.MessageID = mustID(t, domain.NewMessageID)
	next.TurnID = mustID(t, domain.NewStepID)
	if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, next); err != nil || appended {
		t.Fatalf("unarchived snapshot was replaced: appended=%t err=%v", appended, err)
	}
	var messageID, status, content string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT message_id,status,content FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).
		Scan(&messageID, &status, &content); err != nil || messageID != f.delta.MessageID || status != "interrupted" || content != f.delta.Text {
		t.Fatalf("legacy snapshot changed: id=%s status=%s content=%q err=%v", messageID, status, content, err)
	}
}

func TestCloudDSHStreamConcurrentMessageSettlementAndRevocation(t *testing.T) {
	for _, operation := range []string{"message", "settlement", "revocation"} {
		t.Run(operation, func(t *testing.T) {
			f := newCloudDSHStreamFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			type streamResult struct {
				appended bool
				err      error
			}
			start := make(chan struct{})
			streamDone := make(chan streamResult, 1)
			otherDone := make(chan error, 1)
			go func() {
				<-start
				appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta)
				streamDone <- streamResult{appended: appended, err: err}
			}()
			go func() {
				<-start
				switch operation {
				case "message":
					digest := sha256.Sum256([]byte("stream-concurrent-message"))
					messageID, err := domain.NewMessageID()
					if err == nil {
						_, _, err = f.route.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
							Message: conversation.Message{ID: messageID, OwnerID: f.task.OwnerID, ActorID: f.task.InitiatorID,
								ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "New message"},
							ServiceInstanceID: f.instanceID, ClientID: "stream-concurrent-message", RequestHash: digest[:], Authorization: f.route.authorization,
							Input: app.ConversationRoutingInput{ModelID: "p1234567", Budget: f.task.Budget},
						})
					}
					otherDone <- err
				case "settlement":
					otherDone <- f.route.store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallTask,
						OwnerID: f.task.InitiatorID, CallID: f.delta.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}})
				case "revocation":
					otherDone <- f.route.store.RevokeToken(ctx, f.route.authorization.TokenID, f.task.InitiatorID)
				}
			}()
			close(start)
			var stream streamResult
			select {
			case stream = <-streamDone:
			case <-ctx.Done():
				t.Fatal("concurrent stream did not finish")
			}
			select {
			case err := <-otherDone:
				if err != nil {
					t.Fatalf("concurrent %s failed: %v", operation, err)
				}
			case <-ctx.Done():
				t.Fatalf("concurrent %s did not finish", operation)
			}
			if stream.err != nil && domain.CodeOf(stream.err) != domain.CodeUnauthenticated && domain.CodeOf(stream.err) != domain.CodeInvalidState {
				t.Fatalf("concurrent stream failed unexpectedly: %v", stream.err)
			}
			if operation != "message" {
				if appended, err := f.route.store.AppendCloudDSHTextDelta(ctx, f.delta); appended || err == nil {
					t.Fatalf("%s allowed a later delta: appended=%t err=%v", operation, appended, err)
				}
			}
			if stream.appended && operation == "revocation" {
				var status string
				if err := f.route.store.pool.QueryRow(ctx, `SELECT status FROM task_assistant_message_snapshots WHERE task_id=$1`, f.task.ID).
					Scan(&status); err != nil || status != "interrupted" {
					t.Fatalf("revocation did not retain interrupted text: status=%s err=%v", status, err)
				}
				if err := f.route.store.pool.QueryRow(ctx, `SELECT output_status FROM conversation_messages WHERE id=$1`, f.delta.MessageID).
					Scan(&status); err != nil || status != "interrupted" {
					t.Fatalf("revocation left visible message streaming: status=%s err=%v", status, err)
				}
			}
		})
	}
}
