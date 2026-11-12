package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func deferredTargetFixture(t *testing.T, withWorkspace bool) (*Store, *app.AgentInstanceService, app.AgentInstanceFence, task.Task, app.DeferredTaskTarget) {
	t.Helper()
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	var hostID, workspaceID string
	if withWorkspace {
		ws := createTestWorkspace(t, ctx, f.store, f.ownerID, "deferred-target")
		computer, binding := createRunningControlHostForWorkspace(t, ctx, f.store, f.ownerID, ws, host.ControlAI, 2)
		if _, err := f.store.TransitionWorkspaceBinding(ctx, computer.ID, ws.ID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, ws.Revision, 0); err != nil {
			t.Fatal(err)
		}
		hostID, workspaceID = computer.ID, ws.ID
	} else {
		computer := createTestHost(t, ctx, f.store, f.ownerID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
		hostID = computer.ID
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1,runtime_instance_id='runtime-deferred',control_version=2 WHERE id=$1`, hostID); err != nil {
		t.Fatal(err)
	}
	target := app.DeferredTaskTarget{HostID: hostID, WorkspaceID: workspaceID}
	claim := f.receive(t, "deferred-target-message-001", "Use the selected computer only if necessary", app.ConversationRoutingInput{HostID: hostID, WorkspaceID: workspaceID})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "deferred-task-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim deferred task: found=%t err=%v", found, err)
	}
	key := make([]byte, 32)
	key[0] = 3
	authenticator, err := app.NewAgentStopAuthenticator("deferred-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentInstanceService(f.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 7
	lease, _, err := service.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID,
		InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-deferred", AgentSessionID: "session-deferred",
		SupervisorID: "deferred-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: nonce, LeaseDuration: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	details, err := f.store.GetTask(ctx, claimed.ID, f.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	details.Task.ConversationContext = claimed.ConversationContext
	return f.store, service, lease.Fence, details.Task, target
}

func TestDeferredTargetChangesOnlyForExplicitRevisionIntent(t *testing.T) {
	for _, test := range []struct {
		name             string
		changeTaskTarget bool
	}{
		{name: "default selection"},
		{name: "explicit change", changeTaskTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, _, claimed, originalTarget := deferredTargetFixture(t, true)
			ctx := context.Background()
			instanceID, err := store.ServiceInstanceID(ctx)
			if err != nil {
				t.Fatal(err)
			}
			clientID := "revision-target-message-001"
			messageID := mustID(t, domain.NewMessageID)
			hash := sha256.Sum256([]byte(clientID))
			message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
				Message: conversation.Message{ID: messageID, OwnerID: claimed.OwnerID, ActorID: claimed.InitiatorID,
					ConversationID: claimed.ConversationID, Role: conversation.RoleUser, Content: "Use a different computer"},
				ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:], Authorization: claimed.Authorization,
				Input: app.ConversationRoutingInput{HostID: "h7654321", WorkspaceID: "w7654321", ApplicationID: "wechat",
					ChangeTaskTarget: test.changeTaskTarget, Budget: claimed.Budget},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$4,$5,$6,0,'applied')`, message.ID, claimed.ID, claimed.OwnerID, claimed.InitiatorID,
				claimed.ConversationID, message.Sequence); err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, claimed.ID); err != nil {
				t.Fatal(err)
			}
			got, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
			wantHostID, wantWorkspaceID, wantApplicationID := originalTarget.HostID, originalTarget.WorkspaceID, ""
			if test.changeTaskTarget {
				wantHostID, wantWorkspaceID, wantApplicationID = "h7654321", "w7654321", "wechat"
			}
			if err != nil || !found || got.HostID != wantHostID || got.WorkspaceID != wantWorkspaceID || got.ApplicationID != wantApplicationID {
				t.Fatalf("revised selection = %+v found=%t err=%v; want host=%q workspace=%q application=%q",
					got, found, err, wantHostID, wantWorkspaceID, wantApplicationID)
			}
		})
	}
}

func removeDeferredIntentWorkspace(t *testing.T, store *Store, taskID string) {
	t.Helper()
	result, err := store.pool.Exec(context.Background(), `UPDATE conversation_message_routes
SET request_metadata=request_metadata-'workspace_id'
WHERE message_id=(SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1)`, taskID)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("remove explicit Workspace from deferred intent: rows=%d err=%v", result.RowsAffected(), err)
	}
}

func TestDeferredTaskWorkspaceResolvesFromHostOnlyIntentAndRechecksBinding(t *testing.T) {
	store, _, _, claimed, original := deferredTargetFixture(t, true)
	ctx := context.Background()
	removeDeferredIntentWorkspace(t, store, claimed.ID)

	intent, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found || intent.HostID != original.HostID || intent.WorkspaceID != "" {
		t.Fatalf("Host-only intent lookup probed Workspace: target=%+v found=%t err=%v", intent, found, err)
	}
	resolved, found, err := store.ResolveDeferredTaskWorkspace(ctx, claimed, intent)
	if err != nil || !found || resolved.HostID != original.HostID || resolved.WorkspaceID != original.WorkspaceID ||
		resolved.WorkspaceBindingVersion < 1 {
		t.Fatalf("resolve Host Workspace: target=%+v found=%t err=%v", resolved, found, err)
	}

	value, err := store.GetWorkspace(ctx, resolved.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.GetWorkspaceManifestPageForOwner(ctx, claimed.InitiatorID, value.ID, value.Revision, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	digestHex, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindDeferredTaskTarget(ctx, claimed, resolved, &value.Revision, digest)
	if err != nil || bound.HostID != original.HostID || bound.WorkspaceID != original.WorkspaceID {
		t.Fatalf("bind resolved Host Workspace: task=%+v err=%v", bound, err)
	}
}

func TestDeferredTaskWorkspaceBindingChangeBetweenResolveAndBindIsRejected(t *testing.T) {
	store, _, _, claimed, original := deferredTargetFixture(t, true)
	ctx := context.Background()
	removeDeferredIntentWorkspace(t, store, claimed.ID)
	intent, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found {
		t.Fatalf("load Host-only intent: found=%t err=%v", found, err)
	}
	resolved, found, err := store.ResolveDeferredTaskWorkspace(ctx, claimed, intent)
	if err != nil || !found {
		t.Fatalf("resolve Host Workspace: target=%+v found=%t err=%v", resolved, found, err)
	}
	value, err := store.GetWorkspace(ctx, resolved.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.GetWorkspaceManifestPageForOwner(ctx, claimed.InitiatorID, value.ID, value.Revision, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	digestHex, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET binding_version=binding_version+1 WHERE host_id=$1 AND workspace_id=$2`,
		original.HostID, original.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, resolved, &value.Revision, digest); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("stale Host Workspace binding version was accepted: %v", err)
	}
	details, err := store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.HostID != "" || details.Task.WorkspaceID != "" {
		t.Fatalf("Task target changed after stale binding check: task=%+v err=%v", details.Task, err)
	}
}

func TestDeferredTaskWorkspaceResolutionFailsClosedForBadBindings(t *testing.T) {
	for _, test := range []struct {
		name          string
		withWorkspace bool
		mutate        func(*testing.T, *Store, task.Task, app.DeferredTaskTarget)
	}{
		{name: "missing binding"},
		{name: "multiple non-detached bindings", withWorkspace: true, mutate: func(t *testing.T, store *Store, claimed task.Task, target app.DeferredTaskTarget) {
			ctx := context.Background()
			extra := createTestWorkspace(t, ctx, store, claimed.InitiatorID, "legacy-extra-host-workspace")
			if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_bindings
(id,owner_id,host_id,workspace_id,directory_name,state,binding_version,desired_revision,applied_revision,sync_state)
VALUES ($1,$2,$3,$4,$4,'active',1,$5,$5,'healthy')`, bindingID(target.HostID, extra.ID), claimed.InitiatorID,
				target.HostID, extra.ID, extra.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "failed binding", withWorkspace: true, mutate: func(t *testing.T, store *Store, _ task.Task, target app.DeferredTaskTarget) {
			if _, err := store.pool.Exec(context.Background(), `UPDATE workspace_bindings SET state='failed' WHERE host_id=$1`, target.HostID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unhealthy binding", withWorkspace: true, mutate: func(t *testing.T, store *Store, _ task.Task, target app.DeferredTaskTarget) {
			if _, err := store.pool.Exec(context.Background(), `UPDATE workspace_bindings SET sync_state='conflict' WHERE host_id=$1`, target.HostID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stale applied revision", withWorkspace: true, mutate: func(t *testing.T, store *Store, _ task.Task, target app.DeferredTaskTarget) {
			if _, err := store.pool.Exec(context.Background(), `UPDATE workspace_bindings SET desired_revision=desired_revision+1 WHERE host_id=$1`, target.HostID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, _, claimed, target := deferredTargetFixture(t, test.withWorkspace)
			removeDeferredIntentWorkspace(t, store, claimed.ID)
			if test.mutate != nil {
				test.mutate(t, store, claimed, target)
			}
			intent, found, err := store.LoadDeferredTaskTarget(context.Background(), claimed)
			if err != nil || !found || intent.WorkspaceID != "" {
				t.Fatalf("Host-only intent lookup failed early: target=%+v found=%t err=%v", intent, found, err)
			}
			resolved, found, err := store.ResolveDeferredTaskWorkspace(context.Background(), claimed, intent)
			if err == nil || found || resolved.WorkspaceID != "" {
				t.Fatalf("bad Host Workspace binding resolved: target=%+v found=%t err=%v", resolved, found, err)
			}
		})
	}
}

func TestDeferredSharedHostControlDoesNotGrantWorkspaceResolution(t *testing.T) {
	store, _, _, claimed, _ := deferredTargetFixture(t, false)
	ctx := context.Background()
	hostOwner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	workspaceValue := createTestWorkspace(t, ctx, store, hostOwner.ID, "shared-host-workspace")
	shared, binding := createRunningControlHostForWorkspace(t, ctx, store, hostOwner.ID, workspaceValue, host.ControlAI, 0)
	if _, err := store.TransitionWorkspaceBinding(ctx, shared.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertHostMember(ctx, host.Member{HostID: shared.ID, OwnerID: hostOwner.ID, UserID: claimed.InitiatorID,
		Permissions: []host.Permission{host.PermissionDesktopControl}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET request_metadata=jsonb_set(request_metadata,'{host_id}',to_jsonb($2::text))
WHERE message_id=(SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1)`, claimed.ID, shared.ID); err != nil {
		t.Fatal(err)
	}
	intent, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found || intent.HostID != shared.ID || intent.WorkspaceID != "" {
		t.Fatalf("load shared Host-only intent: target=%+v found=%t err=%v", intent, found, err)
	}
	if _, found, err := store.ResolveDeferredTaskWorkspace(ctx, claimed, intent); err == nil || found {
		t.Fatalf("desktop-control permission granted another user's Workspace: found=%t err=%v", found, err)
	}
}

func TestDeferredTargetDoesNotTreatRevisionAsOriginalMessage(t *testing.T) {
	store, _, _, claimed, _ := deferredTargetFixture(t, false)
	ctx := context.Background()
	// Model an independently submitted Task whose only routed message is a
	// later revision. It must retain its original target semantics.
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
SELECT m.id,$1,$2,$3,$4,m.sequence,0,'applied' FROM conversation_messages m
JOIN conversation_message_routes r ON r.message_id=m.id WHERE m.task_id=$1 AND m.role='user' LIMIT 1`,
		claimed.ID, claimed.OwnerID, claimed.InitiatorID, claimed.ConversationID); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || found {
		t.Fatalf("revision was treated as an original routed target: target=%+v found=%t err=%v", got, found, err)
	}
}

func TestDeferredTargetFromOtherServiceInstanceDoesNotBlockText(t *testing.T) {
	store, _, _, claimed, target := deferredTargetFixture(t, true)
	ctx := context.Background()
	messageID := ""
	if err := store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1`, claimed.ID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale routing record in an isolated database. Normal service
	// writes cannot create this state because of the service identity FK.
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role='replica'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET service_instance_id='11111111-1111-4111-8111-111111111111'::uuid WHERE message_id=$1`, messageID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found || !got.Unavailable || got.HostID != "" || got.WorkspaceID != "" || got.ApplicationID != "" {
		t.Fatalf("stale target leaked or blocked loading: target=%+v found=%t err=%v", got, found, err)
	}
	revision := int64(0)
	digest := make([]byte, 32)
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, target, &revision, digest); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("stale service target was bound for tool use: %v", err)
	}
}

func TestDeferredTargetSelectionUsesTaskCredentialOnlyWhenToolRuns(t *testing.T) {
	store, _, _, claimed, target := deferredTargetFixture(t, false)
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	otherID := mustID(t, domain.NewTokenID)
	_, hash, err := identity.NewCredential(identity.KindSession, otherID)
	if err != nil {
		t.Fatal(err)
	}
	otherExpiry := time.Now().Add(time.Hour).UTC()
	if _, err := store.CreateToken(ctx, identity.Token{ID: otherID, OwnerID: claimed.InitiatorID,
		Kind: identity.KindSession, SecretHash: hash, ExpiresAt: otherExpiry}); err != nil {
		t.Fatal(err)
	}
	appendRevision := func(clientID string, expected int64, authorization task.Authorization) {
		t.Helper()
		digest := sha256.Sum256([]byte(clientID))
		message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: claimed.OwnerID, ActorID: claimed.InitiatorID,
				ConversationID: claimed.ConversationID, Role: conversation.RoleUser, Content: "Use this selected computer"},
			ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: digest[:], Authorization: authorization,
			Input: app.ConversationRoutingInput{HostID: target.HostID, ChangeTaskTarget: true, Budget: claimed.Budget},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$4,$5,$6,$7,'applied')`, message.ID, claimed.ID, claimed.OwnerID, claimed.InitiatorID,
			claimed.ConversationID, message.Sequence, expected); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=$2,desired_revision=$2 WHERE id=$1`, claimed.ID, expected+1); err != nil {
			t.Fatal(err)
		}
	}
	verify := func() error {
		return store.WithTx(ctx, func(tx pgx.Tx) error {
			selection, found, err := loadDeferredTaskTargetInTx(ctx, tx, claimed)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("routed target was not found")
			}
			return verifyDeferredTargetSources(ctx, tx, claimed, selection, true, false)
		})
	}
	appendRevision("target-source-original-001", 0, claimed.Authorization)
	if err := verify(); err != nil {
		t.Fatalf("same credential was rejected: %v", err)
	}
	other := claimed.Authorization
	other.TokenID, other.ExpiresAt = otherID, &otherExpiry
	appendRevision("target-source-other-001", 1, other)
	got, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found || got.HostID != target.HostID {
		t.Fatalf("text path was blocked by selection source: target=%+v found=%t err=%v", got, found, err)
	}
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, target, nil, nil); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("other credential selected an executable target: %v", err)
	}
	appendRevision("target-source-original-002", 2, claimed.Authorization)
	if err := verify(); err != nil {
		t.Fatalf("latest same-credential selection was rejected: %v", err)
	}
}

func TestDeferredTargetDoesNotCheckComputerUntilToolUse(t *testing.T) {
	store, service, fence, claimed, target := deferredTargetFixture(t, false)
	ctx := context.Background()
	got, found, err := store.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found || got != target {
		t.Fatalf("load selected target: target=%+v found=%t err=%v", got, found, err)
	}
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, target, nil, nil); err == nil {
		t.Fatal("offline computer was bound before its runtime was ready")
	}
	var taskHost, instanceHost *string
	if err := store.pool.QueryRow(ctx, `SELECT t.host_id,i.host_id FROM tasks t JOIN task_agent_instances i ON i.task_id=t.id WHERE t.id=$1`, claimed.ID).
		Scan(&taskHost, &instanceHost); err != nil || taskHost != nil || instanceHost != nil {
		t.Fatalf("failed binding changed task or agent: task=%v agent=%v err=%v", taskHost, instanceHost, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='running',control_state='ai' WHERE id=$1`, target.HostID); err != nil {
		t.Fatal(err)
	}
	const callers = 2
	var group sync.WaitGroup
	results := make(chan error, callers)
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			bound, err := store.BindDeferredTaskTarget(ctx, claimed, target, nil, nil)
			if err == nil && bound.HostID != target.HostID {
				err = domain.NewError(domain.CodeInvalidState, "bound the wrong Host")
			}
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent identical target binding: %v", err)
		}
	}
	if _, err := service.Renew(ctx, fence, 20*time.Second); err != nil {
		t.Fatalf("Agent could not renew its newly bound Host fence: %v", err)
	}
	wrong := claimed
	wrong.AgentGeneration++
	if _, _, err := store.LoadDeferredTaskTarget(ctx, wrong); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale Agent read deferred target: %v", err)
	}
}

func TestDeferredSharedComputerRechecksMemberGrant(t *testing.T) {
	store, service, fence, claimed, _ := deferredTargetFixture(t, false)
	ctx := context.Background()
	hostOwner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	shared := createRunningControlHost(t, ctx, store, hostOwner.ID, host.ControlAI, 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1,runtime_instance_id='shared-runtime',control_version=2 WHERE id=$1`, shared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET request_metadata=jsonb_set(request_metadata,'{host_id}',to_jsonb($2::text))
WHERE message_id=(SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1)`, claimed.ID, shared.ID); err != nil {
		t.Fatal(err)
	}
	target := app.DeferredTaskTarget{HostID: shared.ID}
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, target, nil, nil); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("shared computer was usable without a grant: %v", err)
	}
	if _, err := store.UpsertHostMember(ctx, host.Member{HostID: shared.ID, OwnerID: hostOwner.ID, UserID: claimed.InitiatorID,
		Permissions: []host.Permission{host.PermissionDesktopControl}}); err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindDeferredTaskTarget(ctx, claimed, target, nil, nil)
	if err != nil || bound.HostID != shared.ID {
		t.Fatalf("authorized shared computer could not be bound: task=%+v err=%v", bound, err)
	}
	if err := store.RevokeHostMember(ctx, shared.ID, hostOwner.ID, claimed.InitiatorID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(ctx, fence, 20*time.Second); err == nil {
		t.Fatal("Agent renewed after the shared computer grant was revoked")
	}
}

func TestDeferredWorkspaceBindsOnlyMatchingCurrentSnapshot(t *testing.T) {
	store, _, _, claimed, target := deferredTargetFixture(t, true)
	ctx := context.Background()
	hostOnly := target
	hostOnly.WorkspaceID = ""
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, hostOnly, nil, nil); err != nil {
		t.Fatal(err)
	}
	ws, err := store.GetWorkspace(ctx, target.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	revision := ws.Revision
	digest := make([]byte, 32)
	digest[0] = 1
	bound, err := store.BindDeferredTaskTarget(ctx, claimed, target, &revision, digest)
	if err != nil || bound.HostID != target.HostID || bound.WorkspaceID != target.WorkspaceID || bound.InputRevision == nil || *bound.InputRevision != revision {
		t.Fatalf("deferred Workspace snapshot: task=%+v err=%v", bound, err)
	}
	wrong := target
	wrong.WorkspaceID = "w7654321"
	if _, err := store.BindDeferredTaskTarget(ctx, claimed, wrong, &revision, digest); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("unselected Workspace was accepted: %v", err)
	}
}

func TestDeferredWorkspaceRebindFollowsTaskCheckpointAfterWrite(t *testing.T) {
	store, _, _, claimed, target := deferredTargetFixture(t, true)
	ctx := context.Background()
	value, err := store.GetWorkspace(ctx, target.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.GetWorkspaceManifestPageForOwner(ctx, claimed.InitiatorID, target.WorkspaceID, value.Revision, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	digestHex, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindDeferredTaskTarget(ctx, claimed, target, &value.Revision, digest)
	if err != nil {
		t.Fatalf("bind initial Workspace snapshot: %v", err)
	}

	checkpoint := value.Revision + 1
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes,directory_count)
SELECT workspace_id,$2,file_count,total_bytes,directory_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$3`,
		target.WorkspaceID, checkpoint, value.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=$2,updated_at=clock_timestamp() WHERE id=$1`,
		target.WorkspaceID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=$3,applied_revision=$3,sync_state='healthy'
WHERE host_id=$1 AND workspace_id=$2`, target.HostID, target.WorkspaceID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET workspace_checkpoint_revision=$2 WHERE id=$1`, bound.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTask(ctx, bound.ID, bound.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	current.Task.ConversationContext = bound.ConversationContext
	rebound, err := store.BindDeferredTaskTarget(ctx, current.Task, target, bound.InputRevision, bound.ContextDigest)
	if err != nil || rebound.WorkspaceCheckpointRevision == nil || *rebound.WorkspaceCheckpointRevision != checkpoint {
		t.Fatalf("same task could not rebind after its committed Workspace write: task=%+v err=%v", rebound, err)
	}

	externalRevision := checkpoint + 1
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id,revision,file_count,total_bytes,directory_count)
SELECT workspace_id,$2,file_count,total_bytes,directory_count FROM workspace_revisions WHERE workspace_id=$1 AND revision=$3`,
		target.WorkspaceID, externalRevision, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=$2,updated_at=clock_timestamp() WHERE id=$1`,
		target.WorkspaceID, externalRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=$3,applied_revision=$3,sync_state='healthy'
WHERE host_id=$1 AND workspace_id=$2`, target.HostID, target.WorkspaceID, externalRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindDeferredTaskTarget(ctx, rebound, target, bound.InputRevision, bound.ContextDigest); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("Task accepted a Workspace revision changed outside its checkpoint: %v", err)
	}
}

func TestDeferredHostlessWorkspaceBindsOnlySelectedSnapshot(t *testing.T) {
	store, service, fence, claimed, target := deferredTargetFixture(t, true)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, target.HostID); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, target.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.GetWorkspaceManifestPageForOwner(ctx, claimed.InitiatorID, target.WorkspaceID, value.Revision, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	digestHex, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindDeferredTaskWorkspace(ctx, claimed, "w7654321", value.Revision, digest); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("unselected Workspace was bound: %v", err)
	}
	bound, err := store.BindDeferredTaskWorkspace(ctx, claimed, target.WorkspaceID, value.Revision, digest)
	if err != nil || bound.HostID != "" || bound.WorkspaceID != target.WorkspaceID || bound.InputRevision == nil || *bound.InputRevision != value.Revision {
		t.Fatalf("hostless Workspace binding: task=%+v err=%v", bound, err)
	}
	if _, err := store.BindDeferredTaskWorkspace(ctx, claimed, target.WorkspaceID, value.Revision, digest); err != nil {
		t.Fatalf("idempotent binding failed: %v", err)
	}
	if _, err := service.Renew(ctx, fence, 20*time.Second); err != nil {
		t.Fatalf("Workspace binding broke Agent lease: %v", err)
	}
	wrong := claimed
	wrong.AgentGeneration++
	if _, err := store.BindDeferredTaskWorkspace(ctx, wrong, target.WorkspaceID, value.Revision, digest); err == nil {
		t.Fatal("stale Agent bound a Workspace")
	}
}
