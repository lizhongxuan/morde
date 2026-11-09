package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

const (
	t09PersonalChildWriteCallID = "t09_child_save_memory"
	t09PersonalChildSummary     = "Saved the user's preference."
	t09PersonalRecoveryAnswer   = "I restored the completed child result and finished the task."
)

// TestCloudDSHPersonalChildRecoveryAfterNodeRestart exercises the positive
// personal-task recovery path with the real Node/DSH subprocess and PostgreSQL
// checkpoints, child sessions, personal mutation, and write receipt. The model
// is synthetic and this test never uses a provider API key.
func TestCloudDSHPersonalChildRecoveryAfterNodeRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner Task execution: %v", err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify local session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x39}, 32))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace block store: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "t09-enable-memory", ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory",
		Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
		t.Fatalf("enable personal memory for the test owner: %v", err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalFiles, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatal(err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalBroker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, runtimeTools, nil, nil,
		recordCapture, personalData, personalSync, goals, personalFiles, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker := &t09ObservedPersonalBroker{CloudDSHToolBroker: personalBroker}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Remember my preference and finish the task.",
		Budget: task.Budget{ActiveSeconds: 100, ToolCalls: 8, ModelTokens: 2000}, IdempotencyKey: "t09-personal-child-recovery-001",
	})
	if err != nil {
		t.Fatalf("submit personal Task: %v", err)
	}
	claimedScopes := []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	submitted.Task.Authorization.Scopes = claimedScopes
	if _, err := pool.Exec(ctx, `UPDATE tasks SET auth_scopes=$2 WHERE id=$1`, submitted.Task.ID, claimedScopes); err != nil {
		t.Fatalf("install the test Task's authorized personal data scopes: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "t09-personal-child-worker-old", 5*time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID || claimed.ConversationContext == nil || claimed.ConversationContext.Scope != "personal" {
		t.Fatalf("claim personal Task with frozen context: task=%#v found=%t err=%v", claimed, found, err)
	}
	if err := personal.ValidateObjectIdentifier(personal.ObjectTypeMemory, "global"); err != nil {
		t.Fatalf("test memory identifier is invalid: %v", err)
	}

	node, dshScript := f01NodeRuntimePaths(t)
	stopKey := bytes.Repeat([]byte{0x54}, 32)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimeStopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	supervisorID := "t09-personal-child-recovery"
	supervisorAuth, err := app.NewAgentStopAuthenticator(supervisorID, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator(supervisorID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
	if err != nil {
		t.Fatal(err)
	}
	accountControl, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	makeSupervisor := func() *f01RecoveringNodeAccountSupervisor {
		t.Helper()
		return &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: filepath.Join(filepath.Dir(dshScript), "bin-account.mjs")},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
	}
	makeRuntimeManager := func(supervisor *f01RecoveringNodeAccountSupervisor, leaseOwner string) *CloudDSHAccountRuntimeManager {
		t.Helper()
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: accountControl, Supervisor: supervisor, Guardian: &f01SignedNodeTaskGuardian{key: privateKey},
			StopVerifier: runtimeStopVerifier, TaskStopper: instances, LeaseOwner: leaseOwner, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	firstSupervisor := makeSupervisor()
	firstRuntimeManager := makeRuntimeManager(firstSupervisor, "t09-personal-child-runtime-old")
	defer func() {
		if command := firstSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	checkpointService, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := &t09SettledChildCheckpointFailure{
		AgentCheckpointService: checkpointService, failed: make(chan struct{}), release: make(chan struct{}),
	}
	defer checkpoints.releaseFailure()
	model := &t09PersonalChildRecoveryModel{roots: make(map[int64]int), children: make(map[string]int)}
	executorOptions := func(manager *CloudDSHAccountRuntimeManager) CloudDSHExecutorOptions {
		return CloudDSHExecutorOptions{
			ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, SupervisorID: supervisorID,
			AccountRuntimes: manager, Instances: instances, Checkpoints: checkpoints, Children: children,
			Models: model, Tools: broker, Completion: broker,
		}
	}
	firstExecutor, err := NewCloudDSHExecutor(executorOptions(firstRuntimeManager))
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- firstExecutor.ExecuteTask(ctx, claimed) }()
	select {
	case <-checkpoints.failed:
	case <-ctx.Done():
		t.Fatalf("parent did not checkpoint the settled child result: %v; root_calls=%d child_calls=%d tool_error=%v",
			ctx.Err(), model.rootCount(1), model.childCount(), broker.lastError())
	case firstErr := <-firstDone:
		t.Fatalf("first DSH session stopped before the injected checkpoint interruption: %v; root_calls=%d child_calls=%d tool_error=%v",
			firstErr, model.rootCount(1), model.childCount(), broker.lastError())
	}
	activeTask, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeFence := app.AgentInstanceFence{
		TaskID: activeTask.Task.ID, OwnerID: activeTask.Task.OwnerID, InitiatorID: activeTask.Task.InitiatorID,
		AgentInstanceID: activeTask.Task.AgentInstanceID, AgentGeneration: activeTask.Task.AgentGeneration,
		AgentSessionID: activeTask.Task.AgentSessionID, SupervisorID: supervisorID,
		LeaseOwner: activeTask.Task.Lease.Owner, LeaseVersion: activeTask.Task.Lease.Version,
		AuthorizationVersion: activeTask.Task.Authorization.Version, EngineVersion: cloudDSHEngineVersion,
		ProfileVersion: cloudDSHProfileVersion,
	}
	activeChildren, err := children.List(ctx, activeFence, activeFence.AgentSessionID)
	if err != nil || len(activeChildren) != 1 || activeChildren[0].Step.Status != task.StepSucceeded || activeChildren[0].CancelRequested ||
		activeChildren[0].CheckpointThrough < 0 || len(activeChildren[0].CheckpointDigest) != 32 ||
		activeChildren[0].Step.Summary != t09PersonalChildSummary {
		t.Fatalf("child was not durably settled before the injected restart: children=%#v err=%v", activeChildren, err)
	}
	childBeforeRestart := activeChildren[0]
	var durableChildCheckpoint bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_agent_checkpoints
WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND agent_root_session_id=$4
AND through_seq=$5 AND digest=$6)`, submitted.Task.ID, owner.ID, childBeforeRestart.SessionID,
		activeFence.AgentSessionID, childBeforeRestart.CheckpointThrough, childBeforeRestart.CheckpointDigest).Scan(&durableChildCheckpoint); err != nil || !durableChildCheckpoint {
		t.Fatalf("settled child checkpoint was not durably stored: found=%t err=%v", durableChildCheckpoint, err)
	}
	checkpoints.releaseFailure()
	var firstErr error
	select {
	case firstErr = <-firstDone:
		if firstErr == nil || errors.Is(firstErr, ErrCloudDSHCompletionPending) {
			t.Fatalf("first DSH session result = %v, want checkpoint interruption after child settlement", firstErr)
		}
	case <-ctx.Done():
		t.Fatalf("first DSH session did not stop after the injected checkpoint interruption: %v", ctx.Err())
	}
	oldProcess := firstSupervisor.cmd
	if oldProcess == nil || oldProcess.Process == nil {
		t.Fatal("old Node account Runtime was not available for restart simulation")
	}
	if err := oldProcess.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("stop old Node account Runtime: %v", err)
	}
	if waitErr := oldProcess.Wait(); waitErr == nil || oldProcess.ProcessState == nil || oldProcess.ProcessState.ExitCode() != -1 {
		t.Fatalf("wait for interrupted Node account Runtime: %v (task error: %v)", waitErr, firstErr)
	}
	firstSupervisor.cmd = nil

	beforeRestart, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || beforeRestart.Task.Status != task.StatusRunning || beforeRestart.Task.AgentGeneration != 1 ||
		beforeRestart.Task.CheckpointThroughSequence < 0 || len(beforeRestart.Task.CheckpointDigest) != 32 {
		t.Fatalf("personal Task checkpoint before recovery: task=%#v err=%v", beforeRestart.Task, err)
	}
	rootFence := app.AgentInstanceFence{
		TaskID: beforeRestart.Task.ID, OwnerID: beforeRestart.Task.OwnerID, InitiatorID: beforeRestart.Task.InitiatorID,
		AgentInstanceID: beforeRestart.Task.AgentInstanceID, AgentGeneration: beforeRestart.Task.AgentGeneration,
		AgentSessionID: beforeRestart.Task.AgentSessionID, SupervisorID: supervisorID,
		LeaseOwner: beforeRestart.Task.Lease.Owner, LeaseVersion: beforeRestart.Task.Lease.Version,
		AuthorizationVersion: beforeRestart.Task.Authorization.Version, EngineVersion: cloudDSHEngineVersion,
		ProfileVersion: cloudDSHProfileVersion,
	}
	oldRuntime, err := getT09RuntimeFence(ctx, firstRuntimeManager, submitted.Conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4`,
		oldRuntime.ServiceInstanceID, owner.ID, oldRuntime.RuntimeID, oldRuntime.RuntimeGeneration); err != nil {
		t.Fatalf("expire old account Runtime lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, submitted.Task.ID); err != nil {
		t.Fatalf("expire interrupted Task lease: %v", err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "t09-personal-child-worker-new", 5*time.Minute)
	if err != nil || !found || recovered.ID != submitted.Task.ID || recovered.AgentSessionID != rootFence.AgentSessionID ||
		recovered.CheckpointThroughSequence != beforeRestart.Task.CheckpointThroughSequence ||
		!bytes.Equal(recovered.CheckpointDigest, beforeRestart.Task.CheckpointDigest) {
		t.Fatalf("claim Task at persisted checkpoint: task=%#v found=%t err=%v", recovered, found, err)
	}
	secondSupervisor := makeSupervisor()
	secondRuntimeManager := makeRuntimeManager(secondSupervisor, "t09-personal-child-runtime-new")
	defer func() {
		if command := secondSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	secondExecutor, err := NewCloudDSHExecutor(executorOptions(secondRuntimeManager))
	if err != nil {
		t.Fatal(err)
	}
	if err := secondExecutor.ExecuteTask(ctx, recovered); err != nil {
		t.Fatalf("recover personal Task from settled child checkpoint: %v; root=%d child=%d", err,
			model.rootCount(2), model.childCount())
	}

	completed, err := store.GetTask(ctx, submitted.Task.ID, owner.ID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != t09PersonalRecoveryAnswer || completed.Task.AgentGeneration != 2 {
		t.Fatalf("recovered personal Task result: task=%#v err=%v", completed.Task, err)
	}
	var childSessionID, childStatus, childSummary string
	var childThrough int64
	var childDigest []byte
	if err := pool.QueryRow(ctx, `SELECT s.session_id,st.status,st.summary,s.checkpoint_through_seq,s.checkpoint_digest
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.session_id=$2`, submitted.Task.ID, childBeforeRestart.SessionID).Scan(
		&childSessionID, &childStatus, &childSummary, &childThrough, &childDigest); err != nil || childSessionID != childBeforeRestart.SessionID ||
		childStatus != string(task.StepSucceeded) || childSummary != t09PersonalChildSummary || childThrough != childBeforeRestart.CheckpointThrough ||
		!bytes.Equal(childDigest, childBeforeRestart.CheckpointDigest) {
		t.Fatalf("settled child changed during parent recovery: session=%s status=%s summary=%q checkpoint=%d/%x err=%v",
			childSessionID, childStatus, childSummary, childThrough, childDigest, err)
	}
	if model.rootCount(2) != 1 || model.childCount() != 2 {
		t.Fatalf("model execution after recovery: root_generation_2=%d total_child_calls=%d, want 1 and 2", model.rootCount(2), model.childCount())
	}
	remembered, err := personalData.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	var rememberedBody struct {
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	decodeErr := json.Unmarshal(remembered.Body, &rememberedBody)
	if err != nil || decodeErr != nil || remembered.Revision != 1 || rememberedBody.Content != "prefers concise answers" || rememberedBody.Category != "preference" {
		t.Fatalf("child personal write was not persisted exactly once: object=%#v err=%v decode_err=%v", remembered, err, decodeErr)
	}
}

type t09SettledChildCheckpointFailure struct {
	*app.AgentCheckpointService
	mu          sync.Mutex
	failed      chan struct{}
	release     chan struct{}
	injected    bool
	failedOnce  sync.Once
	releaseOnce sync.Once
}

func (c *t09SettledChildCheckpointFailure) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	if command.Fence.AgentSessionID == command.Fence.AgentRootSessionID {
		var payload struct {
			Events []struct {
				Type string `json:"type"`
				Data struct {
					Message struct {
						ToolCallID string `json:"toolCallId"`
					} `json:"message"`
				} `json:"data"`
			} `json:"events"`
		}
		if json.Unmarshal(command.Payload, &payload) == nil {
			for _, event := range payload.Events {
				if event.Type == "tool/result" && event.Data.Message.ToolCallID == "t09_delegate_memory_child" {
					c.mu.Lock()
					shouldFail := !c.injected
					c.injected = true
					c.mu.Unlock()
					if shouldFail {
						c.failedOnce.Do(func() { close(c.failed) })
						select {
						case <-c.release:
							return false, errors.New("synthetic restart after settled child and before parent result checkpoint")
						case <-ctx.Done():
							return false, ctx.Err()
						}
					}
				}
			}
		}
	}
	return c.AgentCheckpointService.Save(ctx, command)
}

func (c *t09SettledChildCheckpointFailure) releaseFailure() {
	c.releaseOnce.Do(func() { close(c.release) })
}

type t09ObservedPersonalBroker struct {
	*app.CloudDSHToolBroker
	mu           sync.Mutex
	executeError error
}

func (b *t09ObservedPersonalBroker) Execute(ctx context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	result, err := b.CloudDSHToolBroker.Execute(ctx, claimed, call)
	if err != nil {
		b.mu.Lock()
		if b.executeError == nil {
			b.executeError = err
		}
		b.mu.Unlock()
	}
	return result, err
}

func (b *t09ObservedPersonalBroker) lastError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.executeError
}

type t09PersonalChildRecoveryModel struct {
	mu       sync.Mutex
	roots    map[int64]int
	children map[string]int
}

func (m *t09PersonalChildRecoveryModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model call omitted its Agent fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}
	if call.Task.AgentSessionID == call.Task.AgentRootSessionID {
		m.mu.Lock()
		m.roots[call.Task.AgentGeneration]++
		callNumber := m.roots[call.Task.AgentGeneration]
		m.mu.Unlock()
		if call.Task.AgentGeneration == 1 && callNumber == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "t09_delegate_memory_child", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Save a preference","objective":"Save the authorized preference and report completion.","tool_names":["morde_personal_object_update"]}`}},
				StopReason: "tool_calls", Usage: usage}, nil
		}
		if call.Task.AgentGeneration > 1 {
			return app.ModelResponse{Text: t09PersonalRecoveryAnswer, StopReason: "stop", Usage: usage}, nil
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected root model call generation=%d call=%d", call.Task.AgentGeneration, callNumber)
	}

	childKey := fmt.Sprintf("%s/%d", call.Task.AgentSessionID, call.Task.AgentGeneration)
	m.mu.Lock()
	m.children[childKey]++
	callNumber := m.children[childKey]
	m.mu.Unlock()
	if call.Task.AgentGeneration > 1 {
		return app.ModelResponse{}, errors.New("settled child was incorrectly resumed")
	}
	if callNumber == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: t09PersonalChildWriteCallID, Name: "morde_personal_object_update",
			Arguments: `{"object_type":"memory","object_id":"global","base_revision":0,"body":"{\"content\":\"prefers concise answers\",\"category\":\"preference\"}"}`}},
			StopReason: "tool_calls", Usage: usage}, nil
	}
	if callNumber == 2 {
		return app.ModelResponse{Text: t09PersonalChildSummary, StopReason: "stop", Usage: usage}, nil
	}
	return app.ModelResponse{}, fmt.Errorf("unexpected child model call generation=%d call=%d", call.Task.AgentGeneration, callNumber)
}

func (m *t09PersonalChildRecoveryModel) rootCount(generation int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roots[generation]
}

func (m *t09PersonalChildRecoveryModel) childCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, count := range m.children {
		total += count
	}
	return total
}

func getT09RuntimeFence(ctx context.Context, manager *CloudDSHAccountRuntimeManager, conversationID string) (app.AccountAgentRuntimeFence, error) {
	manager.mu.Lock()
	session := manager.sessions[conversationID]
	manager.mu.Unlock()
	if session == nil {
		return app.AccountAgentRuntimeFence{}, errors.New("old account Runtime session is missing")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.runtime.Fence, nil
}
