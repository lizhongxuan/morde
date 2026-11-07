package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func waitForMCPBlockedTransaction(t *testing.T, ctx context.Context, store *Store, blockerPID int32) int32 {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blockedPID int32
		if err := store.pool.QueryRow(ctx, `SELECT COALESCE((SELECT pid FROM pg_stat_activity
WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blockerPID).Scan(&blockedPID); err != nil {
			t.Fatal(err)
		}
		if blockedPID != 0 {
			return blockedPID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("transaction did not block behind PID %d: %v", blockerPID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// The token authorizing a later connector selection can differ from the Task's
// original token. Dispatch must serialize with revocation of that exact token.
func TestSelectedMCPDispatchSerializesWithRevisionCredentialRevocation(t *testing.T) {
	for _, revocationFirst := range []bool{true, false} {
		name := "dispatch_before_revocation"
		if revocationFirst {
			name = "revocation_before_dispatch"
		}
		t.Run(name, func(t *testing.T) {
			var connectorID string
			f := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
				connectorID = createEnabledMCPGrantConnectors(t, route.store, route.ownerID)[0].ID
				return []string{connectorID}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tokenID := mustID(t, domain.NewTokenID)
			_, secretHash, err := identity.NewCredential(identity.KindSession, tokenID)
			if err != nil {
				t.Fatal(err)
			}
			token, err := f.route.store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: f.route.ownerID,
				Kind: identity.KindSession, SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			f.route.authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}
			f.revise(t, 1, []string{connectorID})
			grant, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID)
			if err != nil {
				t.Fatal(err)
			}
			operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: f.task.ID, OwnerID: f.task.OwnerID,
				ConnectorID: connectorID, ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion,
				GrantVersion: grant.GrantVersion, LeaseVersion: f.task.Lease.Version, Key: "revision-revoke-race", ToolName: "lookup",
				RequestHash: hashBytes("synthetic-revoke-race"), Status: task.OperationPrepared}
			prepared, replayed, err := f.route.store.PrepareMCPConnectorOperation(ctx, operation, f.task.Lease.Owner)
			if err != nil || replayed {
				t.Fatalf("prepare race operation: replayed=%t err=%v", replayed, err)
			}

			tx, err := f.route.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			var blockerPID int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			if revocationFirst {
				if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, token.ID); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := f.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, f.task.Lease.Owner)
					done <- err
				}()
				waitForMCPBlockedTransaction(t, ctx, f.route.store, blockerPID)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-done; domain.CodeOf(err) != domain.CodeForbidden {
					t.Fatalf("dispatch accepted revoked revision credential: %v", err)
				}
				current, err := f.route.store.GetMCPConnectorOperation(ctx, prepared.OwnerID, prepared.TaskID, prepared.ConnectorID, prepared.Key)
				if err != nil || current.Status != task.OperationPrepared {
					t.Fatalf("revoked operation changed state: status=%s err=%v", current.Status, err)
				}
				return
			}

			var lockedID string
			if err := tx.QueryRow(ctx, `SELECT id FROM task_mcp_connector_operations WHERE id=$1 FOR UPDATE`, prepared.ID).Scan(&lockedID); err != nil {
				t.Fatal(err)
			}
			type dispatchResult struct {
				operation app.MCPConnectorOperation
				err       error
			}
			dispatchDone := make(chan dispatchResult, 1)
			go func() {
				value, err := f.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, f.task.Lease.Owner)
				dispatchDone <- dispatchResult{operation: value, err: err}
			}()
			dispatchPID := waitForMCPBlockedTransaction(t, ctx, f.route.store, blockerPID)
			revoked := make(chan error, 1)
			go func() {
				_, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, token.ID)
				revoked <- err
			}()
			waitForMCPBlockedTransaction(t, ctx, f.route.store, dispatchPID)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			dispatch := <-dispatchDone
			if dispatch.err != nil || dispatch.operation.Status != task.OperationDispatched {
				t.Fatalf("authorized dispatch did not commit before revocation: status=%s err=%v", dispatch.operation.Status, dispatch.err)
			}
			if err := <-revoked; err != nil {
				t.Fatal(err)
			}
			// The committed call may already be in flight. It must remain unknown
			// until reconciled rather than being automatically sent again.
			if err := f.route.store.MarkMCPConnectorOperationUnknown(ctx, dispatch.operation, f.task.Lease.Owner, "synthetic unknown outcome"); err != nil {
				t.Fatal(err)
			}
			prior, replayed, err := f.route.store.PrepareMCPConnectorOperation(ctx, prepared, f.task.Lease.Owner)
			if err != nil || !replayed || prior.Status != task.OperationUnknown {
				t.Fatalf("unknown operation was not replayed safely: status=%s replayed=%t err=%v", prior.Status, replayed, err)
			}
			next := prepared
			next.ID = mustID(t, domain.NewOperationID)
			next.Key = "revision-revoke-next"
			if _, _, err := f.route.store.PrepareMCPConnectorOperation(ctx, next, f.task.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("new call was allowed while prior operation was unknown: %v", err)
			}
		})
	}
}
