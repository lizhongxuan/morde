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

func TestDeferredTargetUsesLatestAppliedRevisionIntent(t *testing.T) {
	store, _, _, claimed, target := deferredTargetFixture(t, true)
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
		Input: app.ConversationRoutingInput{HostID: "h7654321", Budget: claimed.Budget},
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
	if err != nil || !found || got.HostID != "h7654321" || got.WorkspaceID != "" || got.HostID == target.HostID {
		t.Fatalf("revised selection was not used safely: target=%+v found=%t err=%v", got, found, err)
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
			Input: app.ConversationRoutingInput{HostID: target.HostID, Budget: claimed.Budget},
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
