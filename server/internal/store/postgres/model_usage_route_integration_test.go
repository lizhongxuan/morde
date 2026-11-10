package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type routeModelClientTest struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
	reply   string
}

func (c *routeModelClientTest) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	count := c.calls.Add(1)
	if len(request.Tools) != 0 {
		return app.ModelResponse{}, errors.New("route call unexpectedly included tools")
	}
	if c.started != nil {
		if count > 1 {
			return app.ModelResponse{}, errors.New("unexpected repeated provider dispatch")
		}
		close(c.started)
		select {
		case <-c.release:
		case <-ctx.Done():
			return app.ModelResponse{}, ctx.Err()
		}
	}
	if c.err != nil {
		return app.ModelResponse{}, c.err
	}
	reply := c.reply
	if reply == "" {
		reply = `{"action":"new","task_id":"","question":""}`
	}
	return app.ModelResponse{Text: reply, StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}, nil
}
func (*routeModelClientTest) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected capability check")
}

type routeRateTest struct{ modelID string }

func (r routeRateTest) ProjectRateSnapshot(_ context.Context, id string) (project.RateSnapshot, error) {
	return project.RateSnapshot{Provider: "test", ModelID: r.modelID, Model: "test-model", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}, nil
}

func routeModelFixture(t *testing.T, useProject bool, client *routeModelClientTest) (*Store, *app.ModelGatewayService, identity.Principal, app.ConversationRouteClaim, app.ConversationRouteContext) {
	return routeModelFixtureWithMember(t, useProject, false, client)
}

func routeModelFixtureWithMember(t *testing.T, useProject, separateOwner bool, client *routeModelClientTest) (*Store, *app.ModelGatewayService, identity.Principal, app.ConversationRouteClaim, app.ConversationRouteContext) {
	t.Helper()
	store, _, _, principal, modelID := modelProbeFixture(t, app.ModelUsage{Known: true, InputTokens: 1, OutputTokens: 1})
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true,capability_tools=false WHERE id=$1`, modelID); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := store.pool.QueryRow(ctx, `SELECT expires_at FROM tokens WHERE id=$1`, principal.TokenID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	scopes := make([]string, 0, len(principal.Scopes))
	for _, scope := range principal.Scopes {
		scopes = append(scopes, string(scope))
	}
	value := createMainConversationForTest(t, store, principal.UserID)
	projectID := ""
	var budget *app.ProjectBudgetService
	if useProject {
		projectOwner := principal.UserID
		if separateOwner {
			projectOwner = createPreRetirementTestUser(t, store, 4, 20_000, 40_000).ID
		}
		p := createProjectStoreFixture(t, ctx, store, projectOwner, "route budget", "fixture", "route-project-create")
		if separateOwner {
			if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, projectOwner, principal.UserID, p, project.RoleMember,
				p.Version, "route-member-invite", "route-member-accept"); err != nil {
				t.Fatal(err)
			}
		}
		projectID = p.ID
		if _, err := store.SetProjectBudget(ctx, projectOwner, projectID, project.Budget{Currency: "USD", LimitMicros: 1_000_000, Version: 1}, 0, "route-project-budget"); err != nil {
			t.Fatal(err)
		}
		conversationID := mustID(t, domain.NewConversationID)
		hash := sha256.Sum256([]byte(conversationID))
		var err error
		value, _, err = store.CreateProjectConversation(ctx, projectOwner, conversation.Conversation{ID: conversationID,
			OwnerID: projectOwner, ProjectID: projectID}, app.IdempotencyRecord{OwnerID: projectOwner, Operation: "project_conversation.create",
			Key: "route-project-conversation", RequestHash: hash[:], ResourceID: conversationID})
		if err != nil {
			t.Fatal(err)
		}
		projects, err := app.NewProjectService(store)
		if err != nil {
			t.Fatal(err)
		}
		budget, err = app.NewProjectBudgetService(store, projects, routeRateTest{modelID: modelID})
		if err != nil {
			t.Fatal(err)
		}
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := app.ConversationRoutingInput{ProjectID: projectID, ModelID: modelID}
	hash, err := app.ConversationReceiptHash(value.ID, "hello route", input)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: value.OwnerID, ActorID: principal.UserID,
			ConversationID: value.ID, Role: conversation.RoleUser, Content: "hello route"},
		ServiceInstanceID: instanceID, ClientID: "route-client-message-01", Input: input, RequestHash: hash,
		Authorization: task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID,
			ClientInstallationID: principal.ClientInstallationID, Scopes: scopes, Version: 1, ExpiresAt: &expires},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := store.ClaimConversationRoute(ctx, "route-worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim route: %v", err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = modelID
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	models, err := app.NewModelConnectionService(store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	var gateway *app.ModelGatewayService
	if budget == nil {
		gateway, err = app.NewModelGatewayService(store, models, client)
	} else {
		gateway, err = app.NewModelGatewayService(store, models, client, budget)
	}
	if err != nil {
		t.Fatal(err)
	}
	return store, gateway, principal, claim, snapshot.Context
}

func TestRouteModelCallConcurrentDispatchAndRestartRecovery(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{})}
	defer releaseRouteTestClient(client)
	store, gateway, _, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()
	selected, err := gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != value.ModelID {
		t.Fatalf("resolve model: %q %v", selected, err)
	}
	result := make(chan error, 1)
	go func() {
		decision, err := gateway.ClassifyConversationRoute(ctx, claim, value)
		if err == nil && decision.Action != app.ConversationRouteNew {
			err = errors.New("unexpected route decision")
		}
		result <- err
	}()
	select {
	case <-client.started:
	case err := <-result:
		t.Fatalf("route did not reach provider: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("route did not reach provider in time")
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("second worker dispatched same call: %v", err)
	}
	close(client.release)
	if err := <-result; err != nil {
		t.Fatalf("first call: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	models, err := app.NewModelConnectionService(store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := app.NewModelGatewayService(store, models, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := restarted.ClassifyConversationRoute(ctx, claim, value)
	if err != nil || decision.Action != app.ConversationRouteNew || client.calls.Load() != 1 {
		t.Fatalf("recover stored route decision = %+v, calls=%d err=%v", decision, client.calls.Load(), err)
	}
	var routeState, ledgerState string
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state,(SELECT count(*) FROM model_call_ledger WHERE source='route')
FROM conversation_route_model_calls c JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&routeState, &ledgerState, &count); err != nil || routeState != "settled" || ledgerState != "settled" || count != 1 {
		t.Fatalf("route and account ledgers = %s %s %d %v", routeState, ledgerState, count, err)
	}
}

func TestRouteModelDirectReplySettlesReplaysAndAppliesOnce(t *testing.T) {
	client := &routeModelClientTest{reply: `{"action":"reply","task_id":"","question":"","reply":"答案是 42。"}`}
	store, gateway, _, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()

	decision, err := gateway.ClassifyConversationRoute(ctx, claim, value)
	if err != nil || decision.Action != app.ConversationRouteReply || decision.Reply != "答案是 42。" || decision.ModelOutput != client.reply {
		t.Fatalf("settle direct reply: decision=%+v err=%v", decision, err)
	}
	if client.calls.Load() != 1 {
		t.Fatalf("direct reply model call count = %d, want 1", client.calls.Load())
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	models, err := app.NewModelConnectionService(store, sealer, client, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := app.NewModelGatewayService(store, models, client)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.ClassifyConversationRoute(ctx, claim, value)
	if err != nil || !reflect.DeepEqual(replayed, decision) || client.calls.Load() != 1 {
		t.Fatalf("direct reply was not replayed from the settled route receipt: replay=%+v calls=%d err=%v", replayed, client.calls.Load(), err)
	}

	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = value.ModelID
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, replayed); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, replayed); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replayed direct reply was applied twice: %v", err)
	}
	var routeState, delivery, taskID, answer string
	var assistantCount int
	if err := store.pool.QueryRow(ctx, `SELECT r.state,m.delivery_status,COALESCE(m.task_id,''),
(SELECT count(*) FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation'),
(SELECT a.content FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation' LIMIT 1)
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).
		Scan(&routeState, &delivery, &taskID, &assistantCount, &answer); err != nil {
		t.Fatal(err)
	}
	if routeState != "routed" || delivery != "applied" || taskID != "" || assistantCount != 1 || answer != "答案是 42。" {
		t.Fatalf("direct reply route result = route:%s delivery:%s task:%s answers:%d content:%q",
			routeState, delivery, taskID, assistantCount, answer)
	}
	var stored app.ConversationRouteDecision
	if err := store.pool.QueryRow(ctx, `SELECT decision FROM conversation_route_model_calls WHERE message_id=$1`, claim.Message.ID).Scan(&stored); err != nil || !reflect.DeepEqual(stored, decision) {
		t.Fatalf("durable direct reply/model output differed from replay: stored=%+v want=%+v err=%v", stored, decision, err)
	}
}

func TestDSHCoordinatorDirectReplyUsesNoToolsModelAndAppliesOnce(t *testing.T) {
	client := &routeModelClientTest{reply: `{"action":"reply","task_id":"","question":"","reply":"答案是 42。"}`}
	store, gateway, _, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()
	var toolsSupported bool
	if err := store.pool.QueryRow(ctx, `SELECT capability_tools FROM model_connections WHERE id=$1`, value.ModelID).Scan(&toolsSupported); err != nil || toolsSupported {
		t.Fatalf("fixture must exercise a model without tool support: tools=%t err=%v", toolsSupported, err)
	}
	selected, err := gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != value.ModelID {
		t.Fatalf("resolve coordinator model: model=%q err=%v", selected, err)
	}
	routeRequest, err := app.ConversationRouteModelRequest(value)
	if err != nil || len(routeRequest.Input) != 1 {
		t.Fatalf("build authorized text route request: %+v err=%v", routeRequest, err)
	}
	raw, err := json.Marshal(map[string]any{
		"provider": "morde", "model": value.ModelID,
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": routeRequest.Instructions}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": routeRequest.Input[0].Text}}},
		},
		"tools": []any{}, "max_tokens": routeRequest.MaxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "coord_session_12345678"
	response, err := gateway.CompleteConversationCoordinatorModel(ctx, claim, value, sessionID, 0, raw)
	if err != nil || response.Text != client.reply || len(response.ToolCalls) != 0 || !response.Usage.Known ||
		response.Usage.InputTokens != 8 || response.Usage.OutputTokens != 3 || client.calls.Load() != 1 {
		t.Fatalf("coordinator model response=%+v provider_calls=%d err=%v", response, client.calls.Load(), err)
	}
	replayed, err := gateway.CompleteConversationCoordinatorModel(ctx, claim, value, sessionID, 0, raw)
	if err != nil || replayed.Text != response.Text || client.calls.Load() != 1 {
		t.Fatalf("coordinator model retry did not reuse its settled output: response=%+v calls=%d err=%v",
			replayed, client.calls.Load(), err)
	}
	decision := app.ParseConversationRouteDecisionForContext(response.Text, value)
	decision.ModelOutput = response.Text
	if decision.Action != app.ConversationRouteReply || decision.Reply != "答案是 42。" {
		t.Fatalf("coordinator direct reply decision = %+v", decision)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = value.ModelID
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
		t.Fatalf("apply coordinator direct reply: %v", err)
	}
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, decision); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("coordinator direct reply was applied twice: %v", err)
	}
	var routeState, delivery, taskID, answer string
	var assistantCount, routeCallCount int
	if err := store.pool.QueryRow(ctx, `SELECT r.state,m.delivery_status,COALESCE(m.task_id,''),
(SELECT count(*) FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation'),
(SELECT a.content FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' AND a.source_kind='conversation' LIMIT 1),
(SELECT count(*) FROM model_call_ledger WHERE source='route')
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).
		Scan(&routeState, &delivery, &taskID, &assistantCount, &answer, &routeCallCount); err != nil {
		t.Fatal(err)
	}
	if routeState != "routed" || delivery != "applied" || taskID != "" || assistantCount != 1 ||
		answer != "答案是 42。" || routeCallCount != 1 || client.calls.Load() != 1 {
		t.Fatalf("coordinator route outcome: state=%s delivery=%s task=%s answers=%d content=%q calls=%d provider_calls=%d",
			routeState, delivery, taskID, assistantCount, answer, routeCallCount, client.calls.Load())
	}
}

func TestCoordinatorResumesSettledLegacyRouteDecisionWithoutRedispatch(t *testing.T) {
	client := &routeModelClientTest{reply: `{"action":"reply","task_id":"","question":"","reply":"答案是 42。"}`}
	_, gateway, _, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()

	legacyDecision, err := gateway.ClassifyConversationRoute(ctx, claim, value)
	if err != nil || legacyDecision.Action != app.ConversationRouteReply || legacyDecision.ModelOutput != client.reply {
		t.Fatalf("settle legacy route decision: %+v %v", legacyDecision, err)
	}
	legacyRequest, err := app.ConversationRouteModelRequest(value)
	if err != nil || len(legacyRequest.Input) != 1 {
		t.Fatalf("build legacy coordinator model request: %+v %v", legacyRequest, err)
	}
	request, err := json.Marshal(map[string]any{
		"provider": "morde", "model": value.ModelID,
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": legacyRequest.Instructions}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": legacyRequest.Input[0].Text}}},
		},
		"tools": []any{}, "max_tokens": legacyRequest.MaxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := gateway.CompleteConversationCoordinatorModel(ctx, claim, value, "coord_session_12345678", 0, request)
	if err != nil || response.Text != client.reply || client.calls.Load() != 1 {
		t.Fatalf("resume prior route decision through coordinator: response=%+v calls=%d err=%v", response, client.calls.Load(), err)
	}
}

func TestRouteModelLeaseTakeoverHoldsAndRecoversLateResult(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{})}
	defer releaseRouteTestClient(client)
	store, gateway, _, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()
	result := make(chan error, 1)
	go func() { _, err := gateway.ClassifyConversationRoute(ctx, claim, value); result <- err }()
	select {
	case <-client.started:
	case err := <-result:
		t.Fatalf("route did not reach provider: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("route did not reach provider in time")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE message_id=$1`, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	newClaim, found, err := store.ClaimConversationRoute(ctx, "route-worker-2", time.Minute)
	if err != nil || !found || newClaim.LeaseVersion <= claim.LeaseVersion {
		t.Fatalf("take over route: found=%t err=%v", found, err)
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, newClaim, value); domain.CodeOf(err) != domain.CodeInvalidState || client.calls.Load() != 1 {
		t.Fatalf("takeover repeated uncertain call: %v calls=%d", err, client.calls.Load())
	}
	var routeState, ledgerState string
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id WHERE c.message_id=$1`, claim.Message.ID).
		Scan(&routeState, &ledgerState); err != nil || routeState != "unknown" || ledgerState != "unknown" {
		t.Fatalf("takeover did not retain unknown hold: %s %s %v", routeState, ledgerState, err)
	}
	close(client.release)
	if err := <-result; err != nil {
		t.Fatalf("late result could not settle: %v", err)
	}
	decision, err := gateway.ClassifyConversationRoute(ctx, newClaim, value)
	if err != nil || decision.Action != app.ConversationRouteNew || client.calls.Load() != 1 {
		t.Fatalf("late result recovery = %+v, calls=%d err=%v", decision, client.calls.Load(), err)
	}
}

func TestRouteModelRejectsForgedHistoryAndRevokedCredential(t *testing.T) {
	client := &routeModelClientTest{}
	store, gateway, principal, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()
	value.History = []app.ConversationRouteHistory{{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "foreign"}}
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); err == nil || client.calls.Load() != 0 {
		t.Fatalf("forged history reached provider: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ResolveConversationRouteModel(ctx, claim); domain.CodeOf(err) != domain.CodeUnauthenticated || client.calls.Load() != 0 {
		t.Fatalf("revoked route authorization accepted: %v", err)
	}
}

func TestRouteModelSelectionUsesAuthorizedTextPreferenceAndDefault(t *testing.T) {
	store, gateway, actor, claim, value := routeModelFixture(t, false, &routeModelClientTest{})
	ctx := context.Background()
	claim.Input.ModelID = ""
	metadata, err := json.Marshal(claim.Input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET request_metadata=$2::jsonb WHERE message_id=$1`, claim.Message.ID, metadata); err != nil {
		t.Fatal(err)
	}
	const otherModel = "p7654321"
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text,capability_tools)
 SELECT $2,NULL,'other route model',base_url,model_name,api_key_cipher,checked_at,true,false FROM model_connections WHERE id=$1`, value.ModelID, otherModel); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE admin_model_defaults SET model_connection_id=$1,version=1 WHERE id=1`, otherModel); err != nil {
		t.Fatal(err)
	}
	selected, err := gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != otherModel {
		t.Fatalf("platform default = %q %v", selected, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO user_preferences(user_id,selected_model_id) VALUES ($1,$2)
ON CONFLICT (user_id) DO UPDATE SET selected_model_id=EXCLUDED.selected_model_id`, actor.UserID, value.ModelID); err != nil {
		t.Fatal(err)
	}
	selected, err = gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != value.ModelID {
		t.Fatalf("user preference = %q %v", selected, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET checked_at=NULL,capability_text=false WHERE id=$1`, value.ModelID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ResolveConversationRouteModel(ctx, claim); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unchecked preference silently fell back: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE user_preferences SET selected_model_id='' WHERE user_id=$1`, actor.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE admin_model_defaults SET model_connection_id=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	selected, err = gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil || selected != otherModel {
		t.Fatalf("sole checked text model = %q %v", selected, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true WHERE id=$1`, value.ModelID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ResolveConversationRouteModel(ctx, claim); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("ambiguous models were silently selected: %v", err)
	}
}

func TestRouteModelExpiredDispatchAppearsForAdminReconciliation(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{})}
	defer releaseRouteTestClient(client)
	store, gateway, admin, claim, value := routeModelFixture(t, false, client)
	ctx := context.Background()
	result := make(chan error, 1)
	go func() { _, err := gateway.ClassifyConversationRoute(ctx, claim, value); result <- err }()
	select {
	case <-client.started:
	case err := <-result:
		t.Fatalf("route did not reach provider: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("route did not reach provider in time")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE message_id=$1`, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListUnknownModelCalls(ctx, admin, admin.UserID, 100)
	if err != nil || len(items) != 1 || items[0].Source != app.ModelCallRoute {
		t.Fatalf("expired dispatched route is not visible to admin: %+v %v", items, err)
	}
	_, err = store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: admin.UserID, Source: app.ModelCallRoute,
		CallID: items[0].CallID, InputTokens: 8, OutputTokens: 3, Evidence: "provider usage confirmed"})
	if err != nil {
		t.Fatalf("reconcile expired route usage: %v", err)
	}
	close(client.release)
	if err := <-result; err == nil {
		t.Fatal("late provider result replaced an administratively reconciled unknown decision")
	}
	var routeState, accountState string
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id WHERE c.message_id=$1`, claim.Message.ID).
		Scan(&routeState, &accountState); err != nil || routeState != "unknown" || accountState != "settled" {
		t.Fatalf("reconciled route was invented: %s %s %v", routeState, accountState, err)
	}
}

func releaseRouteTestClient(client *routeModelClientTest) {
	select {
	case <-client.release:
	default:
		close(client.release)
	}
}

func prepareRouteTestReservation(t *testing.T, store *Store, claim app.ConversationRouteClaim, value app.ConversationRouteContext) app.ModelCallReservation {
	t.Helper()
	ctx := context.Background()
	var version time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, value.ModelID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("route-test-reservation"))
	call := app.ModelCallReservation{Source: app.ModelCallRoute, OwnerID: claim.Message.ActorID, ProjectID: claim.Input.ProjectID,
		ModelID: value.ModelID, ProviderModel: "test-model", ConnectionVersion: version, CallID: "route-test-reservation",
		RequestDigest: digest[:], ReservedTokens: 100, Route: &app.ConversationRouteModelFence{Claim: claim, Context: value}}
	var projectCall *app.ProjectModelReservation
	if call.ProjectID != "" {
		usage := project.Usage{InputTokens: 80, OutputTokens: 20}
		rate, err := (routeRateTest{modelID: call.ModelID}).ProjectRateSnapshot(ctx, call.ModelID)
		if err != nil {
			t.Fatal(err)
		}
		cost, err := rate.EstimateTokenCost(usage)
		if err != nil {
			t.Fatal(err)
		}
		projectDigest := sha256.Sum256([]byte("route-test-project-reservation"))
		projectCall = &app.ProjectModelReservation{ProjectID: call.ProjectID, ActorID: call.OwnerID, Source: call.Source,
			CallID: call.CallID, ModelID: call.ModelID, Usage: usage, ReservedTokens: call.ReservedTokens, Rate: rate,
			ReservedMicros: cost, RequestDigest: projectDigest[:], AccountCall: call}
	}
	if prior, err := store.PrepareConversationRouteModelCall(ctx, call, projectCall); err != nil || prior != nil {
		t.Fatalf("prepare route reservation: prior=%v err=%v", prior, err)
	}
	return call
}

func TestRouteModelRejectsGenericLedgerLifecycle(t *testing.T) {
	store, _, _, claim, value := routeModelFixture(t, false, &routeModelClientTest{})
	ctx := context.Background()
	call := prepareRouteTestReservation(t, store, claim, value)
	if err := store.ReserveModelCall(ctx, call); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("generic route reservation accepted: %v", err)
	}
	if err := store.BeginModelCallDispatch(ctx, call); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("generic route dispatch accepted: %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{OwnerID: call.OwnerID, Source: call.Source, CallID: call.CallID,
		Usage: app.ModelUsage{Known: true, InputTokens: 1, OutputTokens: 1}}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("generic route settlement accepted: %v", err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM conversation_route_model_calls WHERE message_id=$1`, claim.Message.ID).Scan(&state); err != nil || state != "reserved" {
		t.Fatalf("generic entry changed the route state: %q %v", state, err)
	}
}

func TestProjectRouteMemberRevokedBeforeDispatchReleasesBothReservations(t *testing.T) {
	client := &routeModelClientTest{}
	store, _, actor, claim, value := routeModelFixtureWithMember(t, true, true, client)
	ctx := context.Background()
	call := prepareRouteTestReservation(t, store, claim, value)
	var accountOwner, projectActor, conversationOwner string
	if err := store.pool.QueryRow(ctx, `SELECT l.owner_id,p.actor_id,c.owner_id FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
JOIN project_budget_ledger p ON p.project_id=c.project_id AND p.source=c.source AND p.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&accountOwner, &projectActor, &conversationOwner); err != nil ||
		accountOwner != actor.UserID || projectActor != actor.UserID || conversationOwner == actor.UserID {
		t.Fatalf("project route attribution: account=%q actor=%q owner=%q err=%v", accountOwner, projectActor, conversationOwner, err)
	}
	current, err := store.GetProject(ctx, conversationOwner, claim.Input.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, conversationOwner, claim.Input.ProjectID, actor.UserID, current.Version, "route-member-remove"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginConversationRouteModelCall(ctx, call); err == nil || client.calls.Load() != 0 {
		t.Fatalf("revoked project member dispatched model: %v", err)
	}
	if _, err := store.MarkStaleModelCallsUnknown(ctx); err != nil {
		t.Fatal(err)
	}
	var routes, accounts, projects int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_route_model_calls),
 (SELECT count(*) FROM model_call_ledger WHERE source='route'),(SELECT count(*) FROM project_budget_ledger WHERE source='route')`).
		Scan(&routes, &accounts, &projects); err != nil || routes != 0 || accounts != 0 || projects != 0 {
		t.Fatalf("undispatched revoked reservation remained: route=%d account=%d project=%d err=%v", routes, accounts, projects, err)
	}
}

func TestRouteRecoveryReleasesExpiredUndispatchedReservation(t *testing.T) {
	client := &routeModelClientTest{}
	store, gateway, _, claim, value := routeModelFixture(t, true, client)
	ctx := context.Background()
	prepareRouteTestReservation(t, store, claim, value)
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE message_id=$1`, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(ctx); err != nil || marked != 0 {
		t.Fatalf("undispatched call marked unknown: %d %v", marked, err)
	}
	var routes, accounts, projects int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_route_model_calls),
 (SELECT count(*) FROM model_call_ledger WHERE source='route'),(SELECT count(*) FROM project_budget_ledger WHERE source='route')`).
		Scan(&routes, &accounts, &projects); err != nil || routes != 0 || accounts != 0 || projects != 0 {
		t.Fatalf("expired undispatched reservation remained: route=%d account=%d project=%d err=%v", routes, accounts, projects, err)
	}
	newClaim, found, err := store.ClaimConversationRoute(ctx, "route-worker-recovered", time.Minute)
	if err != nil || !found {
		t.Fatalf("reclaim released route: found=%t err=%v", found, err)
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, newClaim, value); err != nil || client.calls.Load() != 1 {
		t.Fatalf("released route did not run once: calls=%d err=%v", client.calls.Load(), err)
	}
}

func TestRouteRecoveryFencesConcurrentDispatchAndLateResult(t *testing.T) {
	client := &routeModelClientTest{started: make(chan struct{}), release: make(chan struct{})}
	defer releaseRouteTestClient(client)
	store, gateway, _, claim, value := routeModelFixture(t, true, client)
	ctx := context.Background()
	result := make(chan error, 1)
	go func() { _, err := gateway.ClassifyConversationRoute(ctx, claim, value); result <- err }()
	select {
	case <-client.started:
	case err := <-result:
		t.Fatalf("route did not reach provider: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("route did not reach provider in time")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_call_ledger SET created_at=clock_timestamp()-interval '3 minutes' WHERE owner_id=$1 AND source='route'`, claim.Message.ActorID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(ctx); err != nil || marked != 0 {
		t.Fatalf("live route was recovered by age: marked=%d err=%v", marked, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE message_id=$1`, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	if marked, err := store.MarkStaleModelCallsUnknown(ctx); err != nil || marked != 1 {
		t.Fatalf("expired route dispatch not held: marked=%d err=%v", marked, err)
	}
	var routeState, accountState, projectState string
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state,p.status FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
JOIN project_budget_ledger p ON p.project_id=c.project_id AND p.source=c.source AND p.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&routeState, &accountState, &projectState); err != nil ||
		routeState != "unknown" || accountState != "unknown" || projectState != "unknown" {
		t.Fatalf("expired route holds: %s %s %s err=%v", routeState, accountState, projectState, err)
	}
	close(client.release)
	if err := <-result; err != nil {
		t.Fatalf("late provider result did not settle: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state,p.status FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
JOIN project_budget_ledger p ON p.project_id=c.project_id AND p.source=c.source AND p.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&routeState, &accountState, &projectState); err != nil ||
		routeState != "settled" || accountState != "settled" || projectState != "settled" || client.calls.Load() != 1 {
		t.Fatalf("late route settlement: %s %s %s calls=%d err=%v", routeState, accountState, projectState, client.calls.Load(), err)
	}
}

func TestProjectRouteBudgetFailureDoesNotReserveEitherLedger(t *testing.T) {
	client := &routeModelClientTest{}
	store, gateway, _, claim, value := routeModelFixture(t, true, client)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE project_budgets SET limit_micros=1 WHERE project_id=$1`, claim.Input.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); domain.CodeOf(err) != domain.CodeQuotaExceeded || client.calls.Load() != 0 {
		t.Fatalf("route ignored project budget: %v calls=%d", err, client.calls.Load())
	}
	var accountCount, projectCount int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_call_ledger WHERE source='route'),
 (SELECT count(*) FROM project_budget_ledger WHERE source='route')`).Scan(&accountCount, &projectCount); err != nil || accountCount != 0 || projectCount != 0 {
		t.Fatalf("budget failure left partial reservations: account=%d project=%d err=%v", accountCount, projectCount, err)
	}
}

func TestProjectRouteUnknownUsageReconcilesBothLedgersWithoutDecision(t *testing.T) {
	client := &routeModelClientTest{err: errors.New("synthetic provider interruption")}
	store, gateway, admin, claim, value := routeModelFixture(t, true, client)
	ctx := context.Background()
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); err == nil {
		t.Fatal("provider interruption reported a decision")
	}
	var callID, routeState, accountState, projectState string
	if err := store.pool.QueryRow(ctx, `SELECT c.call_id,c.state,l.state,p.status FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
JOIN project_budget_ledger p ON p.project_id=c.project_id AND p.source=c.source AND p.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&callID, &routeState, &accountState, &projectState); err != nil ||
		routeState != "unknown" || accountState != "unknown" || projectState != "unknown" {
		t.Fatalf("unknown route holds = %s %s %s %v", routeState, accountState, projectState, err)
	}
	if _, err := store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: admin.UserID, Source: app.ModelCallRoute,
		CallID: callID, InputTokens: 8, OutputTokens: 3, Evidence: "provider usage confirmed"}); err != nil {
		t.Fatalf("reconcile route usage: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state,p.status FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
JOIN project_budget_ledger p ON p.project_id=c.project_id AND p.source=c.source AND p.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&routeState, &accountState, &projectState); err != nil ||
		routeState != "unknown" || accountState != "settled" || projectState != "settled" {
		t.Fatalf("reconciled route states = %s %s %s %v", routeState, accountState, projectState, err)
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); domain.CodeOf(err) != domain.CodeInvalidState || client.calls.Load() != 1 {
		t.Fatalf("reconciliation triggered another route call: %v calls=%d", err, client.calls.Load())
	}
}

func TestProjectRoutePersonalModelReconcilesOnlyActorLedger(t *testing.T) {
	client := &routeModelClientTest{err: errors.New("synthetic provider interruption")}
	store, gateway, admin, claim, value := routeModelFixtureWithMember(t, true, true, client)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET owner_id=$1 WHERE id=$2`, admin.UserID, value.ModelID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ClassifyConversationRoute(ctx, claim, value); err == nil {
		t.Fatal("personal model interruption reported a decision")
	}
	var callID, accountState, routeState string
	var personal bool
	var projectCount int
	if err := store.pool.QueryRow(ctx, `SELECT c.call_id,c.state,l.state,l.personal_model,
 (SELECT count(*) FROM project_budget_ledger WHERE source='route') FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&callID, &routeState, &accountState, &personal, &projectCount); err != nil ||
		routeState != "unknown" || accountState != "unknown" || !personal || projectCount != 0 {
		t.Fatalf("personal project route attribution: route=%s account=%s personal=%t project=%d err=%v", routeState, accountState, personal, projectCount, err)
	}
	if _, err := store.ReconcileUnknownModelCall(ctx, admin, app.ModelUsageReconcileCommand{OwnerID: admin.UserID, Source: app.ModelCallRoute,
		CallID: callID, InputTokens: 8, OutputTokens: 3, Evidence: "provider usage confirmed"}); err != nil {
		t.Fatalf("reconcile personal project route usage: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT c.state,l.state FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1`, claim.Message.ID).Scan(&routeState, &accountState); err != nil || routeState != "unknown" || accountState != "settled" {
		t.Fatalf("personal project route reconciliation: route=%s account=%s err=%v", routeState, accountState, err)
	}
}

func TestTaskModelDispatchRejectsPendingRevision(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'test')`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("revision checkpoint"))
	if _, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,
 lease_owner,lease_version,lease_expires_at,execution_location,auth_version,auth_expires_at,agent_instance_id,
 agent_generation,agent_session_id,agent_engine_version,agent_profile_version,checkpoint_through_seq,checkpoint_digest)
 VALUES ('q1234567',$1,'c1234567','p1234567','test','running','client',$2,$3,600,10,125,0,
 'worker-1',3,clock_timestamp()+interval '1 hour','server',7,clock_timestamp()+interval '1 hour',
 'agent-1',2,'session-1','engine','profile',0,$4)`, principal.UserID, principal.TokenID, principal.ClientInstallationID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ('q1234567',$1,'session-1',0,$2)`, principal.UserID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ('q1234567',$1,'agent-1',2,'session-1','engine','profile','supervisor-1',$2,'worker-1',3,7,30000,
 clock_timestamp()+interval '30 seconds','active')`, principal.UserID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
 VALUES ('q1234567',$1,'agent-1',2,'session-1','session-1',0,0,$2,decode('01','hex'),'engine','profile',7,3)`, principal.UserID, digest[:]); err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: principal.UserID, InitiatorID: principal.UserID, LeaseOwner: "worker-1", LeaseVersion: 3,
		AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1", AuthorizationVersion: 7, ThroughSequence: 0,
		EngineVersion: "engine", ProfileVersion: "profile"}
	reservation.Source, reservation.Task, reservation.Principal = app.ModelCallTask, &fence, identity.Principal{}
	var err error
	reservation.CallID, err = app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatalf("reserve current revision: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginModelCallDispatch(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("old revision was dispatched: %v", err)
	}
	if err := store.AbandonModelCallReservation(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("old revision reserved another call: %v", err)
	}
}
