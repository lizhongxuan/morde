package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
)

func TestWorkspaceTransferManifestReaderIsLimitedToTheFrozenTransfer(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	target := createTestUser(t, store, 4, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, owner.ID, "transfer manifest")
	transferID, err := workspace.NewOwnershipTransferID()
	if err != nil {
		t.Fatal(err)
	}
	transfer, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, target.ID, 0, transferID,
		transferRecord(owner.ID, "workspace.transfer.create", "transfer-manifest-01", transferID))
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.GetWorkspaceManifestPageForOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, transfer.ID, 0, "", "", 10)
	if err != nil || page.WorkspaceID != workspaceValue.ID || page.Revision != 0 {
		t.Fatalf("frozen transfer manifest = %#v, err=%v", page, err)
	}
	if _, err := store.GetWorkspaceManifestPageForOwner(ctx, owner.ID, workspaceValue.ID, 0, "", "", 10); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("ordinary manifest read during transfer = %v, want Workspace busy", err)
	}
	if _, err := store.GetWorkspaceManifestPageForOwnershipTransfer(ctx, target.ID, workspaceValue.ID, transfer.ID, 0, "", "", 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("recipient read source manifest before acceptance = %v, want not found", err)
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.StartWorkspaceOwnershipTransferAcceptance(ctx, target.ID, workspaceValue.ID, transfer.ID, operationID,
		transferRecord(target.ID, "workspace.transfer.accept", "transfer-manifest-accept-01", transfer.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWorkspaceManifestPageForOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, transfer.ID, 0, "", "", 10); err != nil {
		t.Fatalf("accepting transfer could not resume projection preparation: %v", err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='cancelled',operation_id=NULL,version=version+1
WHERE id=$1 AND workspace_id=$2 AND state='accepting'`, transfer.ID, workspaceValue.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetWorkspaceManifestPageForOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, transfer.ID, 0, "", "", 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cancelled transfer read committed files = %v, want not found", err)
	}
}
