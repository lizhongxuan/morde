package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestProjectTaskWorkspaceGrantIsPinnedAtRealBinding(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	value := createTestWorkspace(t, ctx, f.store, f.owner.UserID, "synthetic project Workspace grant")
	computer, binding := createRunningControlHostForWorkspace(t, ctx, f.store, f.owner.UserID, value, host.ControlAI, 0)
	if _, err := f.store.TransitionWorkspaceBinding(ctx, computer.ID, value.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, value.Revision, 0); err != nil {
		t.Fatal(err)
	}
	currentProject, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.AddProjectResource(ctx, f.owner.UserID, project.Resource{ProjectID: f.projectID,
		Kind: project.ResourceWorkspace, ResourceID: value.ID, Version: 1}, currentProject.Version, "project-workspace-grant-resource"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic project Workspace selection"))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "Read the selected project Workspace"},
		ServiceInstanceID: f.instanceID, ClientID: "project-workspace-grant-message", RequestHash: digest[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567", WorkspaceID: value.ID,
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "project-workspace-grant-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project message: found=%t err=%v", found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "project-workspace-grant-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim project Task: found=%t err=%v", found, err)
	}
	model := &revisionBlockingModel{started: make(chan struct{})}
	executor := revisionTestExecutor(t, f.store, model)
	executionCtx, cancelExecution := context.WithCancel(ctx)
	executionDone := make(chan struct{})
	executionError := make(chan error, 1)
	go func() {
		executionError <- executor.ExecuteTask(executionCtx, claimed)
		close(executionDone)
	}()
	t.Cleanup(func() {
		cancelExecution()
		select {
		case <-executionDone:
		case <-time.After(20 * time.Second):
			t.Error("project Workspace test Runtime did not stop")
		}
	})
	select {
	case <-model.started:
	case <-executionDone:
		t.Fatalf("project Workspace test Runtime exited before the model call: %v", <-executionError)
	case <-time.After(25 * time.Second):
		t.Fatal("project Workspace test Runtime did not start")
	}
	details, err := f.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	claimed = details.Task
	selected := app.DeferredTaskTarget{WorkspaceID: value.ID}
	if err := f.store.WithProjectTaskWorkspaceSelection(ctx, claimed, selected, false, func(context.Context, string) error {
		t.Fatal("private Workspace manifest was read without an independent grant")
		return nil
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("project membership alone admitted a private Workspace: %v", err)
	}
	memberGrant := host.Member{HostID: computer.ID, OwnerID: f.owner.UserID, UserID: f.member.UserID,
		Permissions: []host.Permission{host.PermissionWorkspaceFileRead}}
	if _, err := f.store.UpsertHostMember(ctx, memberGrant); err != nil {
		t.Fatal(err)
	}
	var bound task.Task
	var snapshotDigest []byte
	readStarted, finishRead, finishSelection := make(chan struct{}), make(chan struct{}), make(chan struct{})
	selectionDone, bindingDone := make(chan error, 1), make(chan error, 1)
	selectionCtx, cancelSelection := context.WithCancel(ctx)
	defer cancelSelection()
	go func() {
		selectionDone <- f.store.WithProjectTaskWorkspaceSelection(selectionCtx, claimed, selected, false, func(bindingCtx context.Context, ownerID string) error {
			if ownerID != f.owner.UserID {
				return domain.NewError(domain.CodeForbidden, "synthetic Workspace owner changed")
			}
			page, err := f.store.GetWorkspaceManifestPageForOwner(bindingCtx, ownerID, value.ID, value.Revision, "", "", 100)
			if err != nil {
				return err
			}
			hexDigest, err := workspace.ManifestTreeWithMetadataSHA256(page.Files, page.EmptyDirectories, page.Directories)
			if err != nil {
				return err
			}
			snapshotDigest, err = hex.DecodeString(hexDigest)
			if err != nil {
				return err
			}
			close(readStarted)
			select {
			case <-finishRead:
			case <-bindingCtx.Done():
				return bindingCtx.Err()
			}
			bound, err = f.store.BindDeferredTaskWorkspace(bindingCtx, claimed, value.ID, value.Revision, snapshotDigest)
			bindingDone <- err
			select {
			case <-finishSelection:
			case <-bindingCtx.Done():
				return bindingCtx.Err()
			}
			return err
		})
	}()
	select {
	case <-readStarted:
	case err := <-selectionDone:
		t.Fatalf("project Workspace selection ended before manifest read: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace selection did not read its manifest")
	}
	revokeCtx, cancelRevoke := context.WithCancel(ctx)
	defer cancelRevoke()
	revocation := make(chan error, 1)
	go func() {
		revocation <- f.store.RevokeHostMember(revokeCtx, computer.ID, f.owner.UserID, f.member.UserID)
	}()
	select {
	case err := <-revocation:
		t.Fatalf("Workspace grant changed during the protected manifest read: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(finishRead)
	select {
	case err := <-bindingDone:
		if err != nil {
			t.Fatalf("Workspace binding deadlocked or failed during queued revocation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Workspace binding deadlocked during queued revocation")
	}
	cancelRevoke()
	select {
	case err := <-revocation:
		if err == nil {
			t.Fatal("cancelled Workspace grant revocation unexpectedly committed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Workspace grant revocation did not return")
	}
	close(finishSelection)
	select {
	case err = <-selectionDone:
	case <-time.After(10 * time.Second):
		t.Fatal("project Workspace selection did not commit")
	}
	if err != nil || bound.WorkspaceOwnerID != f.owner.UserID || bound.InitiatorID != f.member.UserID ||
		bound.WorkspaceProjectResourceVersion == nil {
		t.Fatalf("independently authorized project Workspace did not bind: %+v err=%v", bound, err)
	}
	var initialVersion string
	if err := f.store.pool.QueryRow(ctx, `SELECT grant_version::text FROM task_workspace_grant_fences
WHERE task_id=$1 AND workspace_id=$2 AND actor_id=$3`, bound.ID, value.ID, f.member.UserID).Scan(&initialVersion); err != nil {
		t.Fatalf("original Workspace grant was not pinned: %v", err)
	}
	called := false
	if err := f.store.WithProjectTaskWorkspaceAccess(ctx, bound, false, func(context.Context) error {
		called = true
		return nil
	}); err != nil || !called {
		t.Fatalf("original read-only Workspace grant was rejected: called=%t err=%v", called, err)
	}
	if err := f.store.WithProjectTaskWorkspaceAccess(ctx, bound, true, func(context.Context) error {
		t.Fatal("non-owner Workspace write was dispatched")
		return nil
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-owner Workspace write was not rejected: %v", err)
	}
	if err := f.store.RevokeHostMember(ctx, computer.ID, f.owner.UserID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithProjectTaskWorkspaceAccess(ctx, bound, false, func(context.Context) error {
		t.Fatal("revoked Workspace read was dispatched")
		return nil
	}); err == nil {
		t.Fatal("revoked Workspace grant remained usable")
	}
	if _, err := f.store.UpsertHostMember(ctx, memberGrant); err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithProjectTaskWorkspaceSelection(ctx, claimed, selected, false, func(bindingCtx context.Context, _ string) error {
		_, err := f.store.BindDeferredTaskWorkspace(bindingCtx, claimed, value.ID, value.Revision, snapshotDigest)
		return err
	}); err == nil {
		t.Fatal("regranted Workspace silently replaced the Task's original grant")
	}
	if err := f.store.WithProjectTaskWorkspaceAccess(ctx, bound, false, func(context.Context) error {
		t.Fatal("regranted Workspace revived an old Task")
		return nil
	}); err == nil {
		t.Fatal("regranted Workspace revived an old Task")
	}
	var pinnedVersion, currentVersion string
	if err := f.store.pool.QueryRow(ctx, `SELECT f.grant_version::text,m.grant_version::text
FROM task_workspace_grant_fences f JOIN host_members m ON m.host_id=f.host_id AND m.owner_id=f.workspace_owner_id AND m.user_id=f.actor_id
WHERE f.task_id=$1`, bound.ID).Scan(&pinnedVersion, &currentVersion); err != nil ||
		pinnedVersion != initialVersion || pinnedVersion == currentVersion {
		t.Fatalf("old grant proof was replaced or regrant version did not advance: err=%v", err)
	}
}
