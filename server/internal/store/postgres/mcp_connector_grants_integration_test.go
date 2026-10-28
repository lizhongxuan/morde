package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestTaskMCPConnectorGrantAtomicCreationAndAppendSelection(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	other := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	connectors := createEnabledMCPGrantConnectors(t, store, owner.ID, owner.ID, other.ID)
	disabledConnector := createDisabledMCPGrantConnector(t, store, owner.ID)

	base := newTestTaskSubmission(t, owner.ID, "", "grant-atomic-submit", "use connector")
	base.MCPConnectorIDs = []string{connectors[0].ID}
	created, err := store.SubmitTask(ctx, base)
	if err != nil {
		t.Fatalf("submit task with connector grant: %v", err)
	}
	grants, err := store.ListTaskMCPConnectorGrants(ctx, owner.ID, created.Task.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("list task grants = %#v, %v", grants, err)
	}
	if grants[0].ConnectorID != connectors[0].ID || grants[0].ConnectorRevision != connectors[0].Revision ||
		grants[0].CredentialVersion != connectors[0].CredentialVersion || grants[0].GrantVersion != 1 ||
		grants[0].ToolSchema != nil || grants[0].ToolSchemaSHA256 != nil {
		t.Fatalf("grant did not snapshot connector versions: %#v connector=%#v", grants[0], connectors[0])
	}
	if foreignGrants, err := store.ListTaskMCPConnectorGrants(ctx, other.ID, created.Task.ID); err != nil || len(foreignGrants) != 0 {
		t.Fatalf("foreign owner saw task grants: %#v, %v", foreignGrants, err)
	}

	// An active Task may repeat its existing selection, or omit it to retain it,
	// but cannot silently widen or replace its grants.
	wrongSelection := newTestTaskSubmission(t, owner.ID, "", "grant-append-widen", "try another connector")
	wrongSelection.Task.ConversationID = created.Conversation.ID
	wrongSelection.Message.ConversationID = created.Conversation.ID
	wrongSelection.MCPConnectorIDs = []string{connectors[1].ID}
	if _, err := store.AppendMessage(ctx, wrongSelection); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("append with changed grant set error = %v, want conflict", err)
	}

	exactSelection := newTestTaskSubmission(t, owner.ID, "", "grant-append-exact", "continue with same connector")
	exactSelection.Task.ConversationID = created.Conversation.ID
	exactSelection.Message.ConversationID = created.Conversation.ID
	exactSelection.MCPConnectorIDs = []string{connectors[0].ID}
	if _, err := store.AppendMessage(ctx, exactSelection); err != nil {
		t.Fatalf("append with exact grant set: %v", err)
	}
	emptySelection := newTestTaskSubmission(t, owner.ID, "", "grant-append-retain", "continue without grant list")
	emptySelection.Task.ConversationID = created.Conversation.ID
	emptySelection.Message.ConversationID = created.Conversation.ID
	if _, err := store.AppendMessage(ctx, emptySelection); err != nil {
		t.Fatalf("append with omitted grant list: %v", err)
	}
	grants, err = store.ListTaskMCPConnectorGrants(ctx, owner.ID, created.Task.ID)
	if err != nil || len(grants) != 1 || grants[0].ConnectorID != connectors[0].ID {
		t.Fatalf("append changed persisted grants: %#v, %v", grants, err)
	}

	// A foreign-owned ID is reported the same as an unknown ID, and the failed
	// selection rolls the Task, conversation, and idempotency reservation back.
	for _, test := range []struct {
		name        string
		connectorID string
	}{
		{name: "foreign", connectorID: connectors[2].ID},
		{name: "disabled", connectorID: disabledConnector.ID},
		{name: "missing", connectorID: "k9999999"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var conversationsBefore int
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE owner_id=$1`, owner.ID).Scan(&conversationsBefore); err != nil {
				t.Fatal(err)
			}
			submission := newTestTaskSubmission(t, owner.ID, "", "grant-invalid-"+test.name, "must roll back")
			submission.MCPConnectorIDs = []string{test.connectorID}
			if _, err := store.SubmitTask(ctx, submission); domain.CodeOf(err) != domain.CodeNotFound {
				t.Fatalf("submit invalid connector error = %v, want not found", err)
			}
			var tasks, keys, conversationsAfter int
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id=$1`, submission.Task.ID).Scan(&tasks); err != nil {
				t.Fatal(err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`, owner.ID, submission.Idempotency.Operation, submission.Idempotency.Key).Scan(&keys); err != nil {
				t.Fatal(err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE owner_id=$1`, owner.ID).Scan(&conversationsAfter); err != nil {
				t.Fatal(err)
			}
			if tasks != 0 || keys != 0 || conversationsAfter != conversationsBefore {
				t.Fatalf("failed grant selection left rows: tasks=%d keys=%d conversations=%d->%d", tasks, keys, conversationsBefore, conversationsAfter)
			}
		})
	}

	// Reusing the same idempotency key for another selection conflicts before
	// attempting to create a second Task.
	first := newTestTaskSubmission(t, owner.ID, "", "grant-hash-conflict", "same request")
	first.Idempotency.RequestHash = hashBytes("same-request")
	first.MCPConnectorIDs = []string{connectors[0].ID}
	if _, err := store.SubmitTask(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := newTestTaskSubmission(t, owner.ID, "", "grant-hash-conflict", "same request")
	second.Idempotency.RequestHash = hashBytes("different-request")
	second.MCPConnectorIDs = []string{connectors[1].ID}
	if _, err := store.SubmitTask(ctx, second); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed grant selection with reused idempotency key error = %v", err)
	}
}

func TestTaskMCPConnectorGrantSchemaCASAndRevocation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 10_000, 20_000)
	connector := createEnabledMCPGrantConnectors(t, store, owner.ID)[0]
	submission := newTestTaskSubmission(t, owner.ID, "", "grant-schema-task", "discover tools")
	submission.MCPConnectorIDs = []string{connector.ID}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		grant app.TaskMCPConnectorGrant
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, schema := range [][]byte{[]byte(`[{"name":"list_files"}]`), []byte(`[{"name":"read_file"}]`)} {
		schema := schema
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			digest := sha256.Sum256(schema)
			grant, err := store.SetTaskMCPConnectorToolSchema(ctx, owner.ID, created.Task.ID, connector.ID, 1, schema, digest[:])
			results <- outcome{grant: grant, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	var pinned []byte
	for result := range results {
		if result.err == nil {
			successes++
			pinned = result.grant.ToolSchema
		} else if domain.CodeOf(result.err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("concurrent schema CAS error = %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 || len(pinned) == 0 {
		t.Fatalf("schema CAS outcomes: successes=%d conflicts=%d schema=%s", successes, conflicts, pinned)
	}
	digest := sha256.Sum256(pinned)
	replayed, err := store.SetTaskMCPConnectorToolSchema(ctx, owner.ID, created.Task.ID, connector.ID, 1, pinned, digest[:])
	if err != nil || !bytes.Equal(replayed.ToolSchema, pinned) {
		t.Fatalf("same schema CAS replay = %#v, %v", replayed, err)
	}

	revoked, err := store.RevokeTaskMCPConnectorGrant(ctx, owner.ID, created.Task.ID, connector.ID, 1)
	if err != nil || revoked.GrantVersion != 2 || revoked.RevokedAt == nil {
		t.Fatalf("revoke grant = %#v, %v", revoked, err)
	}
	if _, err := store.RevokeTaskMCPConnectorGrant(ctx, owner.ID, created.Task.ID, connector.ID, 1); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale revoke error = %v, want conflict", err)
	}
	if _, err := store.SetTaskMCPConnectorToolSchema(ctx, owner.ID, created.Task.ID, connector.ID, 2, pinned, digest[:]); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("schema CAS after revoke error = %v, want conflict", err)
	}
	grants, err := store.ListTaskMCPConnectorGrants(ctx, owner.ID, created.Task.ID)
	if err != nil || len(grants) != 1 || grants[0].RevokedAt == nil || grants[0].GrantVersion != 2 {
		t.Fatalf("revoked grant history = %#v, %v", grants, err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM mcp_connectors WHERE owner_id=$1 AND id=$2`, owner.ID, connector.ID); err != nil {
		t.Fatalf("delete connector with historical task grant: %v", err)
	}
	grants, err = store.ListTaskMCPConnectorGrants(ctx, owner.ID, created.Task.ID)
	if err != nil || len(grants) != 1 || grants[0].ConnectorID != connector.ID || grants[0].RevokedAt == nil {
		t.Fatalf("connector deletion removed historical task grant: %#v, %v", grants, err)
	}
}

func createEnabledMCPGrantConnectors(t *testing.T, store *Store, ownerIDs ...string) []app.MCPConnectorRecord {
	t.Helper()
	ctx := context.Background()
	connectors := make([]app.MCPConnectorRecord, 0, len(ownerIDs))
	for index, ownerID := range ownerIDs {
		id := mustID(t, domain.NewMCPConnectorID)
		name := "grant-connector-" + ownerID + "-" + string(rune('a'+index))
		_, err := store.pool.Exec(ctx, `INSERT INTO mcp_connectors
(id, owner_id, name, endpoint_url, enabled, credential_cipher, credential_version, revision)
VALUES ($1,$2,$3,'https://mcp.example.com/mcp',true,$4,1,2)`, id, ownerID, name, bytes.Repeat([]byte{0x53}, 29))
		if err != nil {
			t.Fatalf("insert enabled MCP connector: %v", err)
		}
		connectors = append(connectors, app.MCPConnectorRecord{
			ID: id, OwnerID: ownerID, Revision: 2, CredentialVersion: 1,
		})
	}
	return connectors
}

func createDisabledMCPGrantConnector(t *testing.T, store *Store, ownerID string) app.MCPConnectorRecord {
	t.Helper()
	id := mustID(t, domain.NewMCPConnectorID)
	_, err := store.pool.Exec(context.Background(), `INSERT INTO mcp_connectors
(id, owner_id, name, endpoint_url, enabled, credential_version, revision)
VALUES ($1,$2,'grant-connector-disabled','https://mcp.example.com/mcp',false,0,1)`, id, ownerID)
	if err != nil {
		t.Fatalf("insert disabled MCP connector: %v", err)
	}
	return app.MCPConnectorRecord{ID: id, OwnerID: ownerID, Revision: 1}
}
