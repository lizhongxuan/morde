package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/worker"
)

func TestProjectCheckpointRecoveryRevalidatesSnapshotAndRestartsRealNode(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	member := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Recovery project", "Shared conversation", "project-recovery-create")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-recovery-invite", "project-recovery-accept"); err != nil {
		t.Fatal(err)
	}
	memberWorkspace := createTestWorkspace(t, ctx, store, member.ID, "member recovery Workspace")
	if _, err := pool.Exec(ctx, `UPDATE workspace_revision_directories
SET mode=493, mod_time_ns=1
WHERE workspace_id=$1 AND valid_from_revision=0 AND directory_path='.' AND valid_to_revision IS NULL`, memberWorkspace.ID); err != nil {
		t.Fatalf("seed observed member Workspace root metadata: %v", err)
	}
	memberHost, binding := createRunningControlHostForWorkspace(t, ctx, store, member.ID, memberWorkspace, host.ControlAI, 0)
	if _, err := store.TransitionWorkspaceBinding(ctx, memberHost.ID, memberWorkspace.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, memberWorkspace.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='project-recovery-host' WHERE id=$1`, memberHost.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_members(host_id,owner_id,user_id,permissions)
VALUES ($1,$2,$3,ARRAY['desktop:control','workspace:file:read'])`, memberHost.ID, member.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{ProjectID: projectValue.ID,
		Kind: project.ResourceWorkspace, ResourceID: memberWorkspace.ID, Version: 1}, currentProject.Version, "project-recovery-workspace-resource"); err != nil {
		t.Fatal(err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	if _, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: conversationID, OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Recovery",
	}, testIdempotency(owner.ID, "project_conversation.create", "project-recovery-conversation")); err != nil {
		t.Fatal(err)
	}
	historyID := mustID(t, domain.NewMessageID)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: historyID, OwnerID: owner.ID, ActorID: member.ID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: "original project fact: Cedar", SourceKind: "history_import"})
	}); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'project recovery test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(member.ID))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("project-recovery-message"))
	message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: member.ID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: "Use the original project fact"},
		ServiceInstanceID: instanceID, ClientID: "project-recovery-message", RequestHash: hash[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		Input: app.ConversationRoutingInput{ProjectID: projectValue.ID, ModelID: "p1234567", HostID: memberHost.ID, WorkspaceID: memberWorkspace.ID,
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "project-recovery-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project message: found=%t err=%v", found, err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	first, found, err := store.ClaimNextTask(ctx, "project-recovery-first", time.Minute)
	if err != nil || !found || first.ConversationContext == nil || len(first.ConversationContext.Messages) != 1 {
		t.Fatalf("claim first project Task: found=%t context=%+v err=%v", found, first.ConversationContext, err)
	}
	model := &revisionBlockingModel{started: make(chan struct{})}
	executor := revisionTestExecutor(t, store, model)
	executionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	firstResult := make(chan error, 1)
	go func() { firstResult <- executor.ExecuteTask(executionCtx, first) }()
	select {
	case <-model.started:
	case err := <-firstResult:
		t.Fatalf("project Runtime ended before first model call: %v", err)
	case <-time.After(25 * time.Second):
		t.Fatal("project Runtime did not start")
	}
	active, err := store.GetTask(ctx, first.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialPage, err := store.GetWorkspaceManifestPageForOwner(ctx, member.ID, memberWorkspace.ID, memberWorkspace.Revision, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest, err := workspace.ManifestTreeWithMetadataSHA256(initialPage.Files, initialPage.EmptyDirectories, initialPage.Directories)
	if err != nil {
		t.Fatal(err)
	}
	workspaceDigest, err := hex.DecodeString(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	selected := app.DeferredTaskTarget{HostID: memberHost.ID, WorkspaceID: memberWorkspace.ID}
	var bound task.Task
	bindErr := store.WithProjectTaskWorkspaceSelection(ctx, active.Task, selected, false, func(bindingCtx context.Context, ownerID string) error {
		if ownerID != member.ID {
			return domain.NewError(domain.CodeForbidden, "synthetic Workspace owner changed")
		}
		var err error
		bound, err = store.BindDeferredTaskTarget(bindingCtx, active.Task, selected, &memberWorkspace.Revision, workspaceDigest)
		return err
	})
	if bindErr != nil || bound.WorkspaceOwnerID != member.ID || bound.OwnerID != owner.ID || bound.InitiatorID != member.ID ||
		bound.WorkspaceProjectResourceVersion == nil {
		t.Fatalf("authorized project Workspace binding lost owner identity: workspace_owner=%s owner=%s actor=%s err=%v",
			bound.WorkspaceOwnerID, bound.OwnerID, bound.InitiatorID, bindErr)
	}
	exerciseProjectWorkspacePublishLockOrder(t, ctx, store, bound, owner.ID)
	exerciseProjectWorkspacePublicationWithQueuedRevocation(t, ctx, store, bound, owner.ID, initialPage, manifestDigest)
	cancel()
	select {
	case err := <-firstResult:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("original project Runtime stop error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("original project Runtime did not stop")
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	// A recovery claim must not wait on a project lock while holding its Task.
	projectLock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = projectLock.Rollback(ctx) }()
	if _, _, err := lockProjectForActorInTx(ctx, projectLock, member.ID, projectValue.ID, project.RoleMember, false); err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		value task.Task
		found bool
		err   error
	}
	claimed := make(chan claimResult, 1)
	go func() {
		value, found, err := store.ClaimNextTask(ctx, "project-recovery-second", time.Minute)
		claimed <- claimResult{value: value, found: found, err: err}
	}()
	var resumed task.Task
	select {
	case result := <-claimed:
		if result.err != nil || !result.found || result.value.ID != first.ID || result.value.CheckpointThroughSequence < 0 || result.value.ConversationContext != nil {
			t.Fatalf("claim project checkpoint: found=%t task=%+v err=%v", result.found, result.value, result.err)
		}
		resumed = result.value
	case <-time.After(10 * time.Second):
		t.Fatal("project recovery claim waited for membership after taking the Task lock")
	}
	if err := projectLock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: member.ID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: "later unrelated fact", SourceKind: "history_import"})
	}); err != nil {
		t.Fatal(err)
	}
	original, err := store.LoadProjectTaskContext(ctx, resumed)
	if err != nil || original == nil || len(original.Messages) != 1 || original.Messages[0].Content != "original project fact: Cedar" {
		t.Fatalf("recovered original project history = %+v err=%v", original, err)
	}
	for _, mutate := range []func(*task.Task){
		func(v *task.Task) { v.Lease.Version++ },
		func(v *task.Task) { v.AgentGeneration++ },
		func(v *task.Task) { v.CheckpointDigest = make([]byte, 32) },
		func(v *task.Task) { v.Authorization.TokenID = testSessionTokenID(owner.ID) },
	} {
		forged := resumed
		mutate(&forged)
		if _, err := store.LoadProjectTaskContext(ctx, forged); err == nil {
			t.Fatal("changed recovery identity was accepted")
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET content='tampered history' WHERE id=$1`, historyID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, resumed); err == nil {
		t.Fatal("changed original history was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET content='original project fact: Cedar' WHERE id=$1`, historyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET desired_revision=desired_revision+1 WHERE id=$1`, resumed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, resumed); err == nil {
		t.Fatal("superseded revision was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET desired_revision=current_revision WHERE id=$1`, resumed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, resumed); err == nil {
		t.Fatal("revoked credential was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	resumed = exerciseProjectRecoveryResourceFences(t, ctx, store, resumed)
	if err := executor.ExecuteTask(ctx, resumed); !errors.Is(err, worker.ErrCloudDSHCompletionPending) {
		t.Fatalf("resume project checkpoint with real Node: %v", err)
	}
	model.mu.Lock()
	calls := append([]app.ModelCall(nil), model.calls...)
	model.mu.Unlock()
	if len(calls) != 2 || calls[1].Task == nil || calls[1].Task.InitiatorID != member.ID {
		t.Fatalf("recovery used wrong actor or repeated model input: call count=%d", len(calls))
	}
	input := string(calls[1].RawRequest)
	if !strings.Contains(input, "original project fact: Cedar") || strings.Contains(input, "later unrelated fact") {
		t.Fatal("recovered model context differs from the original snapshot")
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, resumed.ID); err != nil {
		t.Fatal(err)
	}
	resumed, found, err = store.ClaimNextTask(ctx, "project-recovery-after-restart", time.Minute)
	if err != nil || !found || resumed.ID != first.ID || resumed.CheckpointThroughSequence < 0 {
		t.Fatalf("claim second project recovery: found=%t err=%v", found, err)
	}
	currentProject, err = store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	verified := make(chan error, 1)
	removed := make(chan error, 1)
	go func() {
		<-start
		_, err := store.LoadProjectTaskContext(ctx, resumed)
		verified <- err
	}()
	go func() {
		<-start
		removed <- store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "project-recovery-remove")
	}()
	close(start)
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("remove project member during recovery: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("project member removal deadlocked with recovery")
	}
	select {
	case err := <-verified:
		// The concurrent read may linearize on either side of the removal.
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("project recovery read failed unexpectedly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("project recovery deadlocked with member removal")
	}
	if _, err := store.LoadProjectTaskContext(ctx, resumed); err == nil {
		t.Fatal("removed project member retained recovery access")
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, resumed.ID); err != nil {
		t.Fatal(err)
	}
	resumed, found, err = store.ClaimNextTask(ctx, "project-recovery-after-removal", time.Minute)
	if err != nil || !found || resumed.ID != first.ID {
		t.Fatalf("removed member's checkpoint was silently skipped: found=%t err=%v", found, err)
	}
	if err := executor.ExecuteTask(ctx, resumed); err == nil {
		t.Fatal("Runtime started for a removed project member")
	}
	model.mu.Lock()
	beforeReentry := len(model.calls)
	model.mu.Unlock()
	if beforeReentry != 2 {
		t.Fatal("removed member reached the model")
	}
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status,waiting_reason FROM tasks WHERE id=$1`, resumed.ID).Scan(&status, &reason); err != nil ||
		status != "pausing" || reason != "project_recovery_unavailable" {
		t.Fatalf("revoked recovery did not enter safe stop: status=%s reason=%s err=%v", status, reason, err)
	}
	if err := store.PauseProjectTaskRecovery(ctx, resumed); err == nil {
		t.Fatal("stale recovery lease issued another stop")
	}
	if _, err := pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND status='pending'`, resumed.ID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := store.ClaimNextTaskStop(ctx, "project-recovery-stop", time.Minute)
	if err != nil || !found || stop.Task.ID != resumed.ID {
		t.Fatalf("claim project recovery stop: found=%t err=%v", found, err)
	}
	var acknowledged time.Time
	if err := pool.QueryRow(ctx, `SELECT guardian_stop_acknowledged_at FROM task_agent_instances
WHERE task_id=$1 AND agent_generation=$2 AND guardian_task_stop_ack_required`, resumed.ID, resumed.AgentGeneration).Scan(&acknowledged); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_agent_instances SET guardian_stop_acknowledged_at=NULL
WHERE task_id=$1 AND agent_generation=$2`, resumed.ID, resumed.AgentGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTaskStop(ctx, resumed.ID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unconfirmed Guardian stop was treated as safe: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, resumed.ID).Scan(&status); err != nil || status != "pausing" {
		t.Fatalf("unconfirmed recovery stop left pausing state: %s err=%v", status, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_agent_instances SET guardian_stop_acknowledged_at=$3
WHERE task_id=$1 AND agent_generation=$2`, resumed.ID, resumed.AgentGeneration, acknowledged); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.CompleteTaskStop(ctx, resumed.ID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion)
	if err != nil || stopped.Status != task.StatusWaitingUser || stopped.WaitingReason != "project_recovery_unavailable" {
		t.Fatalf("confirmed recovery stop did not become visible: status=%s reason=%s err=%v", stopped.Status, stopped.WaitingReason, err)
	}
}

func exerciseProjectWorkspacePublishLockOrder(t *testing.T, ctx context.Context, store *Store, value task.Task, projectOwnerID string) {
	t.Helper()
	currentProject, err := store.GetProject(ctx, projectOwnerID, value.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := workspace.NewOwnershipTransferID()
	if err != nil {
		t.Fatal(err)
	}
	guardEntered, runNested, releaseGuard := make(chan struct{}), make(chan struct{}), make(chan struct{})
	guardDone, nestedDone := make(chan error, 1), make(chan error, 1)
	released := false
	defer func() {
		if !released {
			close(releaseGuard)
		}
	}()
	go func() {
		guardDone <- store.WithProjectTaskWorkspaceAccess(ctx, value, true, func(guardedCtx context.Context) error {
			close(guardEntered)
			select {
			case <-runNested:
			case <-releaseGuard:
				return context.Canceled
			case <-guardedCtx.Done():
				return guardedCtx.Err()
			}
			err := store.WithTx(guardedCtx, func(tx pgx.Tx) error {
				if err := lockWorkspaceMutationFence(guardedCtx, tx, value.WorkspaceID, false); err != nil {
					return err
				}
				if _, err := requireOwnerExecutionEnabled(guardedCtx, tx, value.InitiatorID, nil); err != nil {
					return err
				}
				if err := lockTaskHostOwnerGates(guardedCtx, tx, value.InitiatorID, value.WorkspaceOwnerID); err != nil {
					return err
				}
				fence, err := lockProjectTaskWorkspaceFence(guardedCtx, tx, value.ID, true)
				if err != nil {
					return err
				}
				var locked string
				if err := tx.QueryRow(guardedCtx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, value.ID).Scan(&locked); err != nil {
					return err
				}
				if err := verifyProjectTaskWorkspaceFence(guardedCtx, tx, value.ID, fence); err != nil {
					return err
				}
				return requireProjectWorkspacePublishAgent(guardedCtx, tx, value.ID, value.OwnerID, fence)
			})
			nestedDone <- err
			select {
			case <-releaseGuard:
				return err
			case <-guardedCtx.Done():
				return guardedCtx.Err()
			}
		})
	}()
	select {
	case <-guardEntered:
	case err := <-guardDone:
		t.Fatalf("project Workspace guard did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace guard did not acquire its locks")
	}
	removeCtx, cancelRemove := context.WithCancel(ctx)
	defer cancelRemove()
	removeStarted, removeDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(removeStarted)
		removeDone <- store.RemoveProjectResource(removeCtx, projectOwnerID, value.ProjectID, project.ResourceWorkspace,
			value.WorkspaceID, currentProject.Version, "project-workspace-guard-remove")
	}()
	revokeCtx, cancelRevoke := context.WithCancel(ctx)
	defer cancelRevoke()
	revokeStarted, revokeDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(revokeStarted)
		revokeDone <- store.RevokeHostMember(revokeCtx, value.HostID, value.WorkspaceOwnerID, projectOwnerID)
	}()
	transferStarted, transferDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(transferStarted)
		_, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, value.WorkspaceOwnerID, value.WorkspaceID, projectOwnerID,
			*value.WorkspaceCheckpointRevision, transferID,
			transferRecord(value.WorkspaceOwnerID, "workspace.transfer.create", "project-workspace-guard-transfer", transferID))
		transferDone <- err
	}()
	<-removeStarted
	<-revokeStarted
	<-transferStarted
	select {
	case err := <-removeDone:
		t.Fatalf("project resource removal passed a live publish guard: %v", err)
	case err := <-transferDone:
		t.Fatalf("Workspace transfer passed a live publish guard: %v", err)
	case err := <-revokeDone:
		t.Fatalf("Host grant revocation passed a live publish guard: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(runNested)
	select {
	case err := <-nestedDone:
		if err != nil {
			t.Fatalf("nested publish lost its live authorization: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested publish deadlocked behind queued revocation or transfer")
	}
	cancelRemove()
	cancelRevoke()
	select {
	case err := <-removeDone:
		if err == nil {
			t.Fatal("cancelled project resource removal unexpectedly committed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled project resource removal did not return")
	}
	select {
	case err := <-revokeDone:
		if err == nil {
			t.Fatal("cancelled Host grant revocation unexpectedly committed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Host grant revocation did not return")
	}
	close(releaseGuard)
	released = true
	select {
	case err := <-guardDone:
		if err != nil {
			t.Fatalf("project Workspace publish guard failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace publish guard did not release")
	}
	select {
	case err := <-transferDone:
		if err == nil {
			t.Fatal("transfer unexpectedly accepted an actively bound Workspace")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Workspace transfer did not resume after publish guard release")
	}
}

func exerciseProjectWorkspacePublicationWithQueuedRevocation(t *testing.T, ctx context.Context, store *Store, value task.Task,
	projectOwnerID string, initial workspace.ManifestPage, inputManifestSHA string) {
	t.Helper()
	if value.InputRevision == nil || value.WorkspaceCheckpointRevision == nil || value.WorkspaceProjectResourceVersion == nil {
		t.Fatal("project Workspace checkpoint is unavailable")
	}
	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	service, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("synthetic project publication")
	contentDigest := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(contentDigest[:])
	files := append([]workspace.File(nil), initial.Files...)
	files = append(files, workspace.File{Kind: workspace.FileKindRegular, Path: "synthetic-result.txt",
		SizeBytes: int64(len(content)), SHA256: contentSHA, Mode: 0644, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: contentSHA, SizeBytes: int64(len(content))}}})
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, initial.EmptyDirectories, initial.Directories)
	if err != nil {
		t.Fatal(err)
	}
	var generation, controlVersion, bindingVersion int64
	var runtimeInstance string
	if err := store.pool.QueryRow(ctx, `SELECT h.runtime_generation,COALESCE(h.runtime_instance_id,''),h.control_version,b.binding_version
FROM hosts h JOIN workspace_bindings b ON b.host_id=h.id AND b.workspace_id=$2
WHERE h.id=$1 AND b.state='active'`, value.HostID, value.WorkspaceID).
		Scan(&generation, &runtimeInstance, &controlVersion, &bindingVersion); err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("synthetic project Workspace publication"))
	command := app.TaskWorkspacePublishCommand{OwnerID: value.OwnerID, TaskID: value.ID, ProjectID: value.ProjectID,
		InitiatorID: value.InitiatorID, WorkspaceOwnerID: value.WorkspaceOwnerID, WorkspaceID: value.WorkspaceID,
		HostID: value.HostID, LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version,
		BindingVersion: bindingVersion, InputRevision: *value.InputRevision, BaseRevision: *value.WorkspaceCheckpointRevision,
		HostFence: app.AgentHostFence{RuntimeGeneration: generation, RuntimeInstanceID: runtimeInstance, ControlVersion: controlVersion, AIAllowed: true},
		ViewID:    "v" + strings.Repeat("a", 64), InputManifestSHA256: inputManifestSHA,
		OperationKey: "agent:project-workspace-concurrent-publish", RequestHash: requestHash[:]}
	if receipt, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil || receipt.State != app.TaskWorkspacePublishPending {
		t.Fatalf("reserve project Workspace publication: receipt=%+v err=%v", receipt, err)
	}
	projectValue, err := store.GetProject(ctx, projectOwnerID, value.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := workspace.NewOwnershipTransferID()
	if err != nil {
		t.Fatal(err)
	}
	blockRequested, releaseBlock := make(chan struct{}), make(chan struct{})
	type publicationResult struct {
		receipt app.TaskWorkspacePublishReceipt
		err     error
	}
	published := make(chan publicationResult, 1)
	released := false
	defer func() {
		if !released {
			close(releaseBlock)
		}
	}()
	go func() {
		receipt, err := service.CommitTaskWorkspacePublish(ctx, command, files, initial.EmptyDirectories, initial.Directories, manifestSHA,
			func(blockCtx context.Context, digest string) ([]byte, error) {
				if digest != contentSHA {
					return nil, domain.NewError(domain.CodeNotFound, "unexpected synthetic Workspace block")
				}
				close(blockRequested)
				select {
				case <-releaseBlock:
					return append([]byte(nil), content...), nil
				case <-blockCtx.Done():
					return nil, blockCtx.Err()
				}
			})
		published <- publicationResult{receipt: receipt, err: err}
	}()
	select {
	case <-blockRequested:
	case result := <-published:
		t.Fatalf("project Workspace publication stopped before block write: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace publication did not reach block write")
	}
	removed := make(chan error, 1)
	go func() {
		removed <- store.RemoveProjectResource(ctx, projectOwnerID, value.ProjectID, project.ResourceWorkspace,
			value.WorkspaceID, projectValue.Version, "project-workspace-publish-remove")
	}()
	transferred := make(chan error, 1)
	go func() {
		_, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, value.WorkspaceOwnerID, value.WorkspaceID, projectOwnerID,
			*value.WorkspaceCheckpointRevision, transferID,
			transferRecord(value.WorkspaceOwnerID, "workspace.transfer.create", "project-workspace-publish-transfer", transferID))
		transferred <- err
	}()
	select {
	case err := <-removed:
		t.Fatalf("resource was removed during the Workspace block write: %v", err)
	case err := <-transferred:
		t.Fatalf("Workspace transfer passed an in-flight publication: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseBlock)
	released = true
	var receipt app.TaskWorkspacePublishReceipt
	select {
	case result := <-published:
		if result.err != nil || result.receipt.State != app.TaskWorkspacePublishSucceeded ||
			result.receipt.PublishedRevision != command.BaseRevision+1 {
			t.Fatalf("project Workspace publication failed: receipt=%+v err=%v", result.receipt, result.err)
		}
		receipt = result.receipt
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace publication deadlocked with queued revocation")
	}
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("project resource removal failed after publication: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("project resource removal did not resume after publication")
	}
	select {
	case err := <-transferred:
		if err == nil {
			t.Fatal("Workspace transfer unexpectedly accepted an active Host binding")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Workspace transfer did not resume after publication")
	}
	if _, err := service.VerifyCommittedArtifact(ctx, app.TaskArtifactProof{OwnerID: value.WorkspaceOwnerID,
		WorkspaceID: value.WorkspaceID, Revision: receipt.PublishedRevision, Path: "synthetic-result.txt",
		SizeBytes: int64(len(content)), SHA256: contentSHA}); err != nil {
		t.Fatalf("committed synthetic Workspace output failed block readback: %v", err)
	}
	if err := store.WithProjectTaskWorkspaceAccess(ctx, value, true, func(context.Context) error {
		t.Fatal("revoked project Workspace dispatched another write")
		return nil
	}); err == nil {
		t.Fatal("removed project Workspace remained writable")
	}
	// Restore this isolated fixture's exact resource snapshot for the separate
	// checkpoint recovery scenarios in the enclosing test.
	if _, err := store.pool.Exec(ctx, `UPDATE project_resources SET active=true,version=$3
WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		value.ProjectID, value.WorkspaceID, *value.WorkspaceProjectResourceVersion); err != nil {
		t.Fatal(err)
	}
}

func exerciseProjectRecoveryResourceFences(t *testing.T, ctx context.Context, store *Store, value task.Task) task.Task {
	t.Helper()
	pool := store.pool
	if _, err := store.LoadProjectTaskContext(ctx, value); err != nil {
		t.Fatalf("authorized project Workspace recovery was rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE project_resources SET active=false WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		value.ProjectID, value.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, value); err == nil {
		t.Fatal("removed project Workspace remained authorized for checkpoint recovery")
	}
	if _, err := pool.Exec(ctx, `UPDATE project_resources SET active=true,version=version+1 WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		value.ProjectID, value.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, value); err == nil {
		t.Fatal("re-added Workspace resource revived an old checkpoint")
	}
	if _, err := pool.Exec(ctx, `UPDATE project_resources SET version=$3 WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		value.ProjectID, value.WorkspaceID, *value.WorkspaceProjectResourceVersion); err != nil {
		t.Fatal(err)
	}
	transferID := mustID(t, domain.NewPendingRequestID)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_ownership_transfers(id,workspace_id,from_owner_id,to_owner_id,state,expected_revision)
VALUES ($1,$2,$3,$4,'pending',$5)`, transferID, value.WorkspaceID, value.WorkspaceOwnerID, value.OwnerID, *value.WorkspaceCheckpointRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	value, found, err := store.ClaimNextTask(ctx, "project-recovery-workspace-frozen", time.Minute)
	if err != nil || !found {
		t.Fatalf("frozen Workspace recovery was silently skipped: found=%t err=%v", found, err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, value); err == nil {
		t.Fatal("frozen Workspace remained authorized for checkpoint recovery")
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='cancelled' WHERE id=$1`, transferID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='failed' WHERE id=$1`, value.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadProjectTaskContext(ctx, value); err != nil {
		t.Fatalf("temporarily offline Host blocked authorized history recovery: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, value.HostID); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestProjectTaskContextIncludesOnlyProvenSharedHistory(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	member := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Shared history", "Verified results", "project-history-create")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-history-invite", "project-history-accept"); err != nil {
		t.Fatal(err)
	}
	conversationID := mustID(t, domain.NewConversationID)
	shared, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: conversationID, OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Shared results",
	}, testIdempotency(owner.ID, "project_conversation.create", "project-history-conversation"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := resultAggregationFixture{store: store, ownerID: owner.ID, conversation: shared}
	first := fixture.createTask(t, owner.ID, "completed owner request one")
	second := fixture.createTask(t, owner.ID, "completed owner request two")
	if !fixture.complete(t, &first, "verified owner answer one") || !fixture.complete(t, &second, "verified owner answer two") {
		t.Fatal("completed project results were not published")
	}
	var batchID string
	if err := pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches WHERE state='published'`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	pending := fixture.createTask(t, owner.ID, "unfinished owner request")
	importedID := mustID(t, domain.NewMessageID)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: importedID, OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: "unverified imported history", SourceKind: "history_import"})
	}); err != nil {
		t.Fatal(err)
	}
	personalConversation := createMainConversationForTest(t, store, owner.ID)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: personalConversation.ID, Role: conversation.RoleUser, Content: "private personal fact", SourceKind: "history_import"})
	}); err != nil {
		t.Fatal(err)
	}
	readEvidence := func(id string) bool {
		t.Helper()
		var proven bool
		if err := pool.QueryRow(ctx, `SELECT `+projectSharedHistoryEvidenceSQL+` FROM conversation_messages m WHERE m.id=$1`, id).Scan(&proven); err != nil {
			t.Fatal(err)
		}
		return proven
	}
	if !readEvidence(first.UserMessageID) || !readEvidence(second.UserMessageID) || !readEvidence(batchID) ||
		readEvidence(pending.UserMessageID) || readEvidence(importedID) {
		t.Fatal("shared history evidence did not distinguish published and unverified messages")
	}
	capture := func() (*task.ConversationContext, error) {
		var captured task.ConversationContext
		err := store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			captured, _, err = captureTaskConversationContextForTask(ctx, tx, task.Task{OwnerID: owner.ID,
				InitiatorID: member.ID, ProjectID: projectValue.ID, ConversationID: conversationID})
			return err
		})
		return &captured, err
	}
	value, err := capture()
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, message := range value.Messages {
		joined += "\n" + message.Content
	}
	if !strings.Contains(joined, "completed owner request one") || !strings.Contains(joined, "verified owner answer two") ||
		strings.Contains(joined, "unfinished owner request") || strings.Contains(joined, "unverified imported history") ||
		strings.Contains(joined, "private personal fact") {
		t.Fatal("project task snapshot did not preserve the proven shared-history boundary")
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET content='tampered batch' WHERE id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	if readEvidence(first.UserMessageID) || readEvidence(batchID) {
		t.Fatal("tampered batch proved shared history")
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET content=$2 WHERE id=$1`, batchID,
		"verified owner answer one\n\nverified owner answer two"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET result='changed result' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if readEvidence(first.UserMessageID) || readEvidence(batchID) {
		t.Fatal("changed Task result proved shared history")
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET result=$2 WHERE id=$1`, first.ID, first.Content); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending' WHERE id=$1`, first.UserMessageID); err != nil {
		t.Fatal(err)
	}
	if readEvidence(first.UserMessageID) {
		t.Fatal("pending user message entered shared history")
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "project-history-remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(); err == nil {
		t.Fatal("removed project member retained access to shared history")
	}
}
