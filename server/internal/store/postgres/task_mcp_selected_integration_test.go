package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type selectedMCPAgentFixture struct {
	route     routeActionsTestFixture
	task      task.Task
	instances *app.AgentInstanceService
	fence     app.AgentInstanceFence
	nonce     []byte
	key       []byte
}

func startSelectedMCPAgent(t *testing.T, f *selectedMCPAgentFixture, index int) {
	t.Helper()
	ctx := context.Background()
	claimed, found, err := f.route.store.ClaimNextTask(ctx, "selected-mcp-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim selected MCP Task: found=%t err=%v", found, err)
	}
	if f.task.ID != "" && claimed.ID != f.task.ID {
		t.Fatalf("claimed Task %s instead of %s", claimed.ID, f.task.ID)
	}
	f.task = claimed
	f.nonce = bytes.Repeat([]byte{byte(index + 1)}, 32)
	sessionID := claimed.AgentSessionID
	if sessionID == "" {
		sessionID = "selected-session-a"
	}
	f.fence = app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "selected-agent-" + string(rune('a'+index)), AgentSessionID: sessionID,
		SupervisorID: "selected-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "test-engine", ProfileVersion: "test-profile"}
	lease, replayed, err := f.instances.Start(ctx, app.AgentInstanceStart{Fence: f.fence, SupervisorNonce: f.nonce, LeaseDuration: 20 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start selected MCP Agent: replay=%t err=%v", replayed, err)
	}
	f.fence = lease.Fence
	f.task.AgentInstanceID, f.task.AgentGeneration, f.task.AgentSessionID = f.fence.AgentInstanceID, f.fence.AgentGeneration, f.fence.AgentSessionID
	f.task.AgentEngineVersion, f.task.AgentProfileVersion = f.fence.EngineVersion, f.fence.ProfileVersion
}

func newSelectedMCPAgentFixture(t *testing.T, connectorIDs []string, createConnectors func(routeActionsTestFixture) []string) selectedMCPAgentFixture {
	t.Helper()
	f := selectedMCPAgentFixture{route: newRouteActionsTestFixture(t), key: bytes.Repeat([]byte{3}, 32)}
	if createConnectors != nil {
		connectorIDs = createConnectors(f.route)
	}
	ctx := context.Background()
	claim := f.route.receive(t, "selected-mcp-original", "use my selected connector", app.ConversationRoutingInput{MCPConnectorIDs: connectorIDs})
	snapshot, err := f.route.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.route.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	if err := f.route.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&f.task.ID); err != nil {
		t.Fatal(err)
	}
	authenticator, err := app.NewAgentStopAuthenticator("selected-supervisor", f.key)
	if err != nil {
		t.Fatal(err)
	}
	f.instances, err = app.NewAgentInstanceService(f.route.store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	startSelectedMCPAgent(t, &f, 0)
	return f
}

func (f *selectedMCPAgentFixture) revise(t *testing.T, index int, connectorIDs []string) task.Task {
	t.Helper()
	ctx := context.Background()
	claim := f.route.receive(t, "selected-mcp-revision-"+string(rune('a'+index)), "update connector selection",
		app.ConversationRoutingInput{ReferenceTaskID: f.task.ID, MCPConnectorIDs: connectorIDs})
	snapshot, err := f.route.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("load selected MCP revision: candidates=%d err=%v", len(snapshot.Context.Candidates), err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.route.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: f.task.ID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorIDsOrFirst(connectorIDs)); err == nil {
		t.Fatal("old Agent could use a connector after the revision fenced its lease")
	}
	receipt := app.AgentStopReceipt{Fence: f.fence, SupervisorNonce: f.nonce, TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, f.key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	if _, err := f.instances.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("confirm synthetic Agent stop: %v", err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second'
WHERE task_id=$1 AND status='pending'`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	stop, found, err := f.route.store.ClaimNextTaskStop(ctx, "selected-mcp-stop", time.Minute)
	if err != nil || !found || stop.Task.ID != f.task.ID {
		t.Fatalf("claim revision stop: found=%t err=%v", found, err)
	}
	if _, err := f.route.store.CompleteTaskStop(ctx, f.task.ID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if applied, err := f.route.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
		t.Fatalf("apply selected MCP revision: applied=%t err=%v", applied, err)
	}
	previous := f.task
	startSelectedMCPAgent(t, f, index)
	return previous
}

func connectorIDsOrFirst(ids []string) string {
	if len(ids) != 0 {
		return ids[0]
	}
	return "k1234567"
}

func TestSelectedTaskMCPGrantRequiresCurrentRevisionAndExplicitReselection(t *testing.T) {
	var connectorID, otherID string
	f := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
		connectors := createEnabledMCPGrantConnectors(t, route.store, route.ownerID, route.ownerID)
		connectorID, otherID = connectors[0].ID, connectors[1].ID
		return []string{connectorID}
	})
	ctx := context.Background()
	ids, err := f.route.store.ListSelectedTaskMCPConnectors(ctx, f.task)
	if err != nil || len(ids) != 1 || ids[0] != connectorID {
		t.Fatalf("initial selection = %v err=%v", ids, err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, otherID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("unselected connector was granted: %v", err)
	}
	grant, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID)
	if err != nil || grant.GrantVersion != 1 {
		t.Fatalf("grant selected connector = %+v err=%v", grant, err)
	}
	schema := []byte(`[{"name":"lookup","description":"synthetic","input_schema":{"type":"object"}}]`)
	digest := sha256.Sum256(schema)
	if _, err := f.route.store.PinSelectedTaskMCPSchema(ctx, f.task, connectorID, grant.GrantVersion, schema, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.RevokeTaskMCPConnectorGrant(ctx, f.task.OwnerID, f.task.ID, connectorID, grant.GrantVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revoked grant was restored by the same revision: %v", err)
	}
	old := f.revise(t, 1, nil)
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, old, connectorID); err == nil {
		t.Fatal("old Agent lease regained access after revision")
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("omitted selection restored a revoked grant: %v", err)
	}
	f.revise(t, 2, []string{connectorID})
	renewed, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID)
	if err != nil || renewed.GrantVersion <= grant.GrantVersion || len(renewed.ToolSchema) != 0 {
		t.Fatalf("explicit reselection did not create a fresh grant: %+v err=%v", renewed, err)
	}
	f.revise(t, 3, []string{otherID})
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("replaced selection retained the prior connector: %v", err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, otherID); err != nil {
		t.Fatalf("replacement connector was not granted: %v", err)
	}
}

func TestSelectedTaskMCPDispatchRejectsRevokedCredential(t *testing.T) {
	var connectorID string
	f := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
		connectorID = createEnabledMCPGrantConnectors(t, route.store, route.ownerID)[0].ID
		return []string{connectorID}
	})
	ctx := context.Background()
	grant, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID)
	if err != nil {
		t.Fatal(err)
	}
	operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: f.task.ID, OwnerID: f.task.OwnerID,
		ConnectorID: connectorID, ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion,
		GrantVersion: grant.GrantVersion, LeaseVersion: f.task.Lease.Version, Key: "selected-mcp-operation", ToolName: "lookup",
		RequestHash: hashBytes("synthetic-selected-mcp-operation"), Status: task.OperationPrepared}
	prepared, replayed, err := f.route.store.PrepareMCPConnectorOperation(ctx, operation, f.task.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare selected MCP call: replay=%t err=%v", replayed, err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.task.Authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, f.task.Lease.Owner); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("MCP call dispatched after credential revocation: %v", err)
	}
	var state string
	if err := f.route.store.pool.QueryRow(ctx, `SELECT status FROM task_mcp_connector_operations WHERE id=$1`, prepared.ID).Scan(&state); err != nil || state != string(task.OperationPrepared) {
		t.Fatalf("revoked MCP call changed ledger state=%s err=%v", state, err)
	}
}

func TestSelectedTaskMCPRechecksRevisionSenderCredential(t *testing.T) {
	var connectorID string
	f := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
		connectorID = createEnabledMCPGrantConnectors(t, route.store, route.ownerID)[0].ID
		return []string{connectorID}
	})
	ctx := context.Background()
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("grant explicitly reselected connector: %v", err)
	}
	operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: f.task.ID, OwnerID: f.task.OwnerID,
		ConnectorID: connectorID, ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion,
		GrantVersion: grant.GrantVersion, LeaseVersion: f.task.Lease.Version, Key: "revision-sender-mcp-operation", ToolName: "lookup",
		RequestHash: hashBytes("synthetic-revision-sender-mcp"), Status: task.OperationPrepared}
	prepared, _, err := f.route.store.PrepareMCPConnectorOperation(ctx, operation, f.task.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revision sender's revoked credential retained access: %v", err)
	}
	if _, err := f.route.store.MarkMCPConnectorOperationDispatched(ctx, prepared, f.task.Lease.Owner); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("revision sender's revoked credential dispatched MCP: %v", err)
	}
}

func TestSelectedTaskMCPRejectsStaleServiceInstance(t *testing.T) {
	var connectorID string
	f := newSelectedMCPAgentFixture(t, nil, func(route routeActionsTestFixture) []string {
		connectorID = createEnabledMCPGrantConnectors(t, route.store, route.ownerID)[0].ID
		return []string{connectorID}
	})
	ctx := context.Background()
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); err != nil {
		t.Fatal(err)
	}
	// Simulate restored stale route evidence inside this isolated test schema.
	if _, err := f.route.store.pool.Exec(ctx, `ALTER TABLE conversation_message_routes DISABLE TRIGGER ALL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET service_instance_id='11111111-1111-4111-8111-111111111111'
WHERE message_id=(SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1)`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `ALTER TABLE conversation_message_routes ENABLE TRIGGER ALL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.GrantSelectedTaskMCPConnector(ctx, f.task, connectorID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale routed selection fell back to a standalone grant: %v", err)
	}
}

func TestSelectedProjectMCPGrantRechecksMemberAndProjectAuthorization(t *testing.T) {
	for _, revoke := range []string{"project grant", "member"} {
		t.Run(revoke, func(t *testing.T) {
			project := newProjectAttachmentFixture(t)
			ctx := context.Background()
			connector := createEnabledMCPGrantConnectors(t, project.store, project.owner.UserID)[0]
			projectGrant, err := project.store.GrantProjectMCPConnector(ctx, project.owner.UserID, project.projectID, connector.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := project.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic project model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, project.member.UserID); err != nil {
				t.Fatal(err)
			}
			token, err := project.store.GetTokenByID(ctx, project.member.TokenID)
			if err != nil {
				t.Fatal(err)
			}
			messageID := mustID(t, domain.NewMessageID)
			digest := sha256.Sum256([]byte("project-selected-mcp"))
			message, _, err := project.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
				Message: conversation.Message{ID: messageID, OwnerID: project.owner.UserID, ActorID: project.member.UserID,
					ConversationID: project.conversationID, Role: conversation.RoleUser, Content: "Use the selected shared connector"},
				ServiceInstanceID: project.instanceID, ClientID: "project-selected-mcp-001", RequestHash: digest[:],
				Input: app.ConversationRoutingInput{ProjectID: project.projectID, ModelID: "p1234567", MCPConnectorIDs: []string{connector.ID},
					Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
				Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, found, err := project.store.ClaimConversationRoute(ctx, "project-mcp-route", time.Minute)
			if err != nil || !found || claim.Message.ID != message.ID {
				t.Fatalf("claim project MCP message: found=%t err=%v", found, err)
			}
			snapshot, err := project.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := project.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			f := selectedMCPAgentFixture{route: routeActionsTestFixture{store: project.store}, key: bytes.Repeat([]byte{3}, 32)}
			if err := project.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, message.ID).Scan(&f.task.ID); err != nil {
				t.Fatal(err)
			}
			authenticator, err := app.NewAgentStopAuthenticator("selected-supervisor", f.key)
			if err != nil {
				t.Fatal(err)
			}
			f.instances, err = app.NewAgentInstanceService(project.store, authenticator)
			if err != nil {
				t.Fatal(err)
			}
			startSelectedMCPAgent(t, &f, 0)
			grant, err := project.store.GrantSelectedTaskMCPConnector(ctx, f.task, connector.ID)
			if err != nil || grant.ConnectorOwnerID != project.owner.UserID || grant.ProjectID != project.projectID || grant.ProjectGrantVersion != projectGrant.Grant.Version {
				t.Fatalf("project selected connector grant = %+v err=%v", grant, err)
			}
			operation := app.MCPConnectorOperation{ID: mustID(t, domain.NewOperationID), TaskID: f.task.ID, OwnerID: f.task.OwnerID,
				ConnectorID: connector.ID, ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion,
				GrantVersion: grant.GrantVersion, LeaseVersion: f.task.Lease.Version, Key: "project-selected-mcp-operation", ToolName: "lookup",
				RequestHash: hashBytes("synthetic-project-mcp"), Status: task.OperationPrepared}
			prepared, _, err := project.store.PrepareMCPConnectorOperation(ctx, operation, f.task.Lease.Owner)
			if err != nil {
				t.Fatal(err)
			}
			if revoke == "project grant" {
				if _, err := project.store.RevokeProjectMCPConnector(ctx, project.owner.UserID, project.projectID, connector.ID, projectGrant.Grant.Version); err != nil {
					t.Fatal(err)
				}
			} else {
				current, err := project.store.GetProject(ctx, project.owner.UserID, project.projectID)
				if err != nil {
					t.Fatal(err)
				}
				if err := project.store.RemoveProjectMember(ctx, project.owner.UserID, project.projectID, project.member.UserID,
					current.Version, "selected-mcp-remove-member-001"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := project.store.GrantSelectedTaskMCPConnector(ctx, f.task, connector.ID); err == nil {
				t.Fatalf("MCP grant survived %s revocation", revoke)
			}
			if _, err := project.store.MarkMCPConnectorOperationDispatched(ctx, prepared, f.task.Lease.Owner); err == nil {
				t.Fatalf("MCP operation dispatched after %s revocation", revoke)
			}
		})
	}
}
