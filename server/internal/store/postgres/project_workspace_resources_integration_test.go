package postgres

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestWorkspaceOwnershipTransferRevokesProjectResourceAndKeepsProjectHistory(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 4, 20_000, 40_000)
	newOwner := createTestUser(t, store, 4, 20_000, 40_000)
	member := createTestUser(t, store, 4, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, oldOwner.ID, "Project transfer")
	projectValue := createProjectStoreFixture(t, ctx, store, oldOwner.ID, "Workspace history", "Keep project history", "workspace-transfer-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, oldOwner.ID, member.ID, projectValue, project.RoleMember, 1,
		"workspace-transfer-invite-01", "workspace-transfer-accept-01"); err != nil {
		t.Fatal(err)
	}
	currentProject, err := store.GetProject(ctx, oldOwner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddProjectResource(ctx, oldOwner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, currentProject.Version, "workspace-transfer-resource-01"); err != nil {
		t.Fatalf("attach Workspace to old project: %v", err)
	}
	content := project.Content{
		ProjectID: projectValue.ID, Kind: project.ContentMemory, ID: "m_0123abcd", Revision: 1,
		Body: json.RawMessage(`{"content":"Project history stays with the project"}`), UpdatedBy: oldOwner.ID,
	}
	if _, _, err := store.ApplyProjectContentMutation(ctx, oldOwner.ID, content, 0, false, "workspace-transfer-content-01"); err != nil {
		t.Fatalf("create project history: %v", err)
	}
	resources, err := store.ListProjectResources(ctx, oldOwner.ID, projectValue.ID, app.PageRequest{Limit: 20})
	if err != nil || len(resources.Items) != 1 || resources.Items[0].ResourceID != workspaceValue.ID {
		t.Fatalf("old project Workspace resource = %#v, err=%v", resources, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_ownership_transfers
(id,workspace_id,from_owner_id,to_owner_id,state,expected_revision)
VALUES ('r1234567',$1,$2,$3,'pending',0)`, workspaceValue.ID, oldOwner.ID, newOwner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListProjectResources(ctx, oldOwner.ID, projectValue.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("old project resource listing during pending transfer = %v, want Workspace busy", err)
	}
	if _, replay, err := store.AddProjectResource(ctx, oldOwner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, currentProject.Version, "workspace-transfer-resource-01"); domain.CodeOf(err) != domain.CodeWorkspaceBusy || replay {
		t.Fatalf("replay Workspace project resource during pending transfer = replay %t, err %v, want Workspace busy", replay, err)
	}
	if got, err := store.GetProjectContent(ctx, member.ID, projectValue.ID, content.Kind, content.ID); err != nil || !sameJSON(got.Body, content.Body) {
		t.Fatalf("project history during Workspace transfer = %#v, err=%v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='accepting',operation_id='o1234567',version=version+1,updated_at=clock_timestamp()
WHERE id='r1234567' AND workspace_id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceProjectResourcesForTransfer(ctx, tx, workspaceValue.ID); err != nil {
			return err
		}
		var currentOwnerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceValue.ID).Scan(&currentOwnerID); err != nil {
			return err
		}
		if currentOwnerID != oldOwner.ID {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace owner changed before transfer")
		}
		if err := revokeWorkspaceProjectResourcesForTransfer(ctx, tx, workspaceValue.ID, oldOwner.ID, newOwner.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_ownership_history SET transfer_id='r1234567',ended_at=clock_timestamp()
WHERE workspace_id=$1 AND owner_id=$2 AND ended_at IS NULL`, workspaceValue.ID, oldOwner.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspaces SET owner_id=$2,updated_at=clock_timestamp() WHERE id=$1`, workspaceValue.ID, newOwner.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_volumes SET owner_id=$2,updated_at=clock_timestamp() WHERE workspace_id=$1`, workspaceValue.ID, newOwner.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_ownership_history(workspace_id,owner_id,transfer_id)
VALUES ($1,$2,'r1234567')`, workspaceValue.ID, newOwner.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='accepted',accepted_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp()
WHERE id='r1234567' AND workspace_id=$1 AND state='accepting'`, workspaceValue.ID)
		return err
	}); err != nil {
		t.Fatalf("accept Workspace ownership with project resource revocation: %v", err)
	}
	resources, err = store.ListProjectResources(ctx, oldOwner.ID, projectValue.ID, app.PageRequest{Limit: 20})
	if err != nil || len(resources.Items) != 0 {
		t.Fatalf("old project resources after transfer = %#v, err=%v", resources, err)
	}
	var active bool
	var resourceVersion int64
	if err := pool.QueryRow(ctx, `SELECT active,version FROM project_resources
WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`, projectValue.ID, workspaceValue.ID).Scan(&active, &resourceVersion); err != nil {
		t.Fatal(err)
	}
	if active || resourceVersion != 2 {
		t.Fatalf("transferred Workspace project resource active=%t version=%d, want inactive version 2", active, resourceVersion)
	}
	if _, replay, err := store.AddProjectResource(ctx, oldOwner.ID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, currentProject.Version, "workspace-transfer-resource-01"); domain.CodeOf(err) != domain.CodeNotFound || replay {
		t.Fatalf("replay Workspace project resource after transfer = replay %t, err %v, want not found", replay, err)
	}
	if got, err := store.GetProjectContent(ctx, member.ID, projectValue.ID, content.Kind, content.ID); err != nil || !sameJSON(got.Body, content.Body) {
		t.Fatalf("project history after Workspace transfer = %#v, err=%v", got, err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := requireIndependentResourceAccessInTx(ctx, tx, oldOwner.ID, project.ResourceWorkspace, workspaceValue.ID, true)
		return err
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("former Workspace owner resource access = %v, want not found", err)
	}
	newProject := createProjectStoreFixture(t, ctx, store, newOwner.ID, "New Workspace project", "Explicit new authorization", "workspace-transfer-new-project-01")
	newResources, err := store.ListProjectResources(ctx, newOwner.ID, newProject.ID, app.PageRequest{Limit: 20})
	if err != nil || len(newResources.Items) != 0 {
		t.Fatalf("new project without explicit resource grant = %#v, err=%v", newResources, err)
	}
	if _, _, err := store.AddProjectResource(ctx, newOwner.ID, project.Resource{
		ProjectID: newProject.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, 1, "workspace-transfer-new-resource-01"); err != nil {
		t.Fatalf("new owner explicitly adds Workspace resource: %v", err)
	}
}

func sameJSON(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}
