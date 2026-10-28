package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type meteredProbeModel struct {
	calls int
	usage app.ModelUsage
}

func (m *meteredProbeModel) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	m.calls++
	if len(request.Input) != 1 {
		return app.ModelResponse{}, domain.InvalidArgument("request", "unexpected model probe")
	}
	if request.RequiredTool == "morde_capability_probe" {
		return app.ModelResponse{Text: "red", Usage: m.usage, ToolCalls: []app.ModelToolCall{{CallID: "probe-call", Name: request.RequiredTool, Arguments: `{"color":"red"}`}}}, nil
	}
	text := "OK"
	if len(request.Input[0].Images) == 1 {
		text = "RED"
	}
	return app.ModelResponse{Usage: m.usage, Text: text}, nil
}

func (*meteredProbeModel) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, domain.NewError(domain.CodeInternal, "unmetered probe was called")
}

func modelProbeFixture(t *testing.T, usage app.ModelUsage) (*Store, *app.ModelGatewayService, *meteredProbeModel, identity.Principal, string) {
	t.Helper()
	store, credentials, _, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,50000,1)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	model := &meteredProbeModel{usage: usage}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := models.Create(ctx, principal, app.ModelConnectionCommand{Name: "probe model", BaseURL: "https://model.example/v1", ModelName: "test-model", APIKey: "synthetic-key"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	return store, gateway, model, principal, created.ID
}

func TestPostgresModelProbeUsesBudgetAndSettlesOnlyOnce(t *testing.T) {
	store, gateway, model, principal, modelID := modelProbeFixture(t, app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4})
	ctx := context.Background()
	result, err := gateway.CheckModel(ctx, principal, modelID, "probe-request-0001")
	if err != nil || !result.Text || !result.Tools || !result.Vision || model.calls != 1 {
		t.Fatalf("probe result = %#v, calls=%d, err=%v", result, model.calls, err)
	}
	var state string
	var tokens int64
	if err := store.pool.QueryRow(ctx, `SELECT state,input_tokens+output_tokens FROM model_call_ledger WHERE owner_id=$1 AND source='probe'`, principal.UserID).Scan(&state, &tokens); err != nil || state != "settled" || tokens != 12 {
		t.Fatalf("probe ledger = %q %d, %v", state, tokens, err)
	}
	if _, err := gateway.CheckModel(ctx, principal, modelID, "probe-request-0001"); domain.CodeOf(err) != domain.CodeConflict || model.calls != 1 {
		t.Fatalf("probe replay = %v, calls=%d", err, model.calls)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher)
 SELECT 'p7654321',owner_id,'second model',base_url,model_name,api_key_cipher FROM model_connections WHERE id=$1`, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CheckModel(ctx, principal, "p7654321", "probe-request-0001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict || model.calls != 1 {
		t.Fatalf("probe key reused for another model = %v, calls=%d", err, model.calls)
	}
	connection, err := store.GetModelConnection(ctx, modelID)
	if err != nil || connection.CheckedAt == nil || !connection.Capabilities.Vision {
		t.Fatalf("checked model = %#v, %v", connection, err)
	}
}

func TestPostgresModelProbeUnknownUsageIsNotMarkedChecked(t *testing.T) {
	store, gateway, model, principal, modelID := modelProbeFixture(t, app.ModelUsage{})
	ctx := context.Background()
	if _, err := gateway.CheckModel(ctx, principal, modelID, "probe-request-0001"); domain.CodeOf(err) != domain.CodeInvalidState || model.calls != 1 {
		t.Fatalf("unknown probe = %v, calls=%d", err, model.calls)
	}
	connection, err := store.GetModelConnection(ctx, modelID)
	if err != nil || connection.CheckedAt != nil {
		t.Fatalf("unknown probe marked model checked: %#v, %v", connection, err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='probe'`, principal.UserID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("unknown probe ledger = %q, %v", state, err)
	}
}

func TestPostgresModelGatewayHTTPSettlesUsage(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	portalLogin, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	portal, err := credentials.VerifySession(ctx, portalLogin.Credential)
	if err != nil {
		t.Fatal(err)
	}
	model := &meteredProbeModel{usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4}}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := models.Create(ctx, portal, app.ModelConnectionCommand{
		Name: "integration model", BaseURL: "https://model.example/v1", ModelName: "test-model", APIKey: "synthetic-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET capability_text=true,checked_at=now() WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,50000,1)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	clientLogin, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "model-http-login-01", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts: unusedWorkspaceSyncHostService{}, TLSAuthorizer: unusedWorkspaceSyncTLSAuthorizer{},
		Verifier: credentials, Auth: clientLoginHTTPAuth{credentials: credentials}, ClientLogin: clients,
		ModelGateway: gateway, Admin: clientLoginHTTPAdmin{}, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20,
		SessionTTL: 12 * time.Hour, HostScheme: "https", PortalDomain: "portal.example.test",
		HostSuffix: "example-hosts.test", PreviewPortMin: 1024,
		PortalFiles: fstest.MapFS{"admin/index.html": &fstest.MapFile{Data: []byte("<html>admin</html>")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"` + connection.ID + `","messages":[{"role":"user","content":"run metered model"}],"max_tokens":100}`
	request := httptest.NewRequest(http.MethodPost, "/api/model/v1/chat/completions", strings.NewReader(body))
	request.Host = "portal.example.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+clientLogin.ClientCredential)
	request.Header.Set("Idempotency-Key", "model-gateway-http-001")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"content":"OK"`) ||
		!strings.Contains(response.Body.String(), `"prompt_tokens":8`) || !strings.Contains(response.Body.String(), `"completion_tokens":4`) || model.calls != 1 {
		t.Fatalf("model gateway response = %d %s, provider calls=%d", response.Code, response.Body, model.calls)
	}
	var state, providerModel string
	var inputTokens, outputTokens int64
	if err := store.pool.QueryRow(ctx, `SELECT state,provider_model,input_tokens,output_tokens FROM model_call_ledger
 WHERE owner_id=$1 AND source='client' AND call_id=$2`, owner.ID, "model-gateway-http-001").Scan(&state, &providerModel, &inputTokens, &outputTokens); err != nil ||
		state != "settled" || providerModel != "test-model" || inputTokens != 8 || outputTokens != 4 {
		t.Fatalf("metered model call = %q %q %d/%d, %v", state, providerModel, inputTokens, outputTokens, err)
	}
}

func TestPostgresModelProbeCapabilitiesCanBeVerifiedIndependently(t *testing.T) {
	_, gateway, model, principal, modelID := modelProbeFixture(t, app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4})
	ctx := context.Background()
	text, err := gateway.CheckModelCapability(ctx, principal, modelID, "probe-text-0001", "text")
	if err != nil || !text.Text || text.Tools || text.Vision {
		t.Fatalf("text-only probe = %#v, %v", text, err)
	}
	tools, err := gateway.CheckModelCapability(ctx, principal, modelID, "probe-tools-001", "tools")
	if err != nil || !tools.Text || !tools.Tools || tools.Vision {
		t.Fatalf("tool probe = %#v, %v", tools, err)
	}
	vision, err := gateway.CheckModelCapability(ctx, principal, modelID, "probe-vision-01", "vision")
	if err != nil || !vision.Text || !vision.Tools || !vision.Vision || model.calls != 3 {
		t.Fatalf("merged vision probe = %#v, calls=%d, %v", vision, model.calls, err)
	}
	if _, err := gateway.CheckModelCapability(ctx, principal, modelID, "probe-text-0001", "tools"); domain.CodeOf(err) != domain.CodeIdempotencyConflict || model.calls != 3 {
		t.Fatalf("same key for different probes = %v, calls=%d", err, model.calls)
	}
}

func modelUsageFixture(t *testing.T) (*Store, identity.Principal, app.ModelCallReservation) {
	t.Helper()
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "model-login-0001", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,1000,3)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	var version time.Time
	err = store.pool.QueryRow(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,
 capability_text,capability_tools,capability_vision,checked_at) VALUES ('p1234567',$1,'model','https://model.example/v1','test-model',
 decode(repeat('01',29),'hex'),true,true,true,now()) RETURNING updated_at`, owner.ID).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("request"))
	return store, principal, app.ModelCallReservation{Source: app.ModelCallClient, OwnerID: owner.ID, Principal: principal,
		ModelID: "p1234567", ProviderModel: "test-model", ConnectionVersion: version, CallID: "test-call-0001",
		RequestDigest: digest[:], ReservedTokens: 100}
}

func TestPostgresModelReservationDeduplicatesConcurrentCallsAndSettlesOnce(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.ReserveModelCall(ctx, reservation)
		}()
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if domain.CodeOf(err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("successful/conflicting reservations = %d/%d", success, conflicts)
	}
	other := reservation
	digest := sha256.Sum256([]byte("different"))
	other.RequestDigest = digest[:]
	if err := store.ReserveModelCall(ctx, other); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different request with same key = %v", err)
	}
	settlement := app.ModelCallSettlement{Source: app.ModelCallClient, OwnerID: principal.UserID, CallID: reservation.CallID,
		Usage: app.ModelUsage{Known: true, InputTokens: 20, OutputTokens: 15}}
	if err := store.SettleModelCall(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, settlement); err != nil {
		t.Fatalf("same settlement retry = %v", err)
	}
	var calls int
	var total int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*),sum(input_tokens+output_tokens) FROM model_call_ledger WHERE owner_id=$1`, principal.UserID).Scan(&calls, &total); err != nil || calls != 1 || total != 35 {
		t.Fatalf("recorded usage = %d calls, %d tokens, %v", calls, total, err)
	}
	settlement.Usage.OutputTokens++
	if err := store.SettleModelCall(ctx, settlement); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed settlement = %v", err)
	}
}

func TestPostgresModelBudgetAndUnknownUsageFailClosed(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `DELETE FROM model_account_budgets WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("missing budget = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,99,1)`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("exhausted budget = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_account_budgets SET token_limit=1000 WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	next := reservation
	next.CallID = "test-call-0002"
	if err := store.ReserveModelCall(ctx, next); domain.CodeOf(err) != domain.CodeRateLimited {
		t.Fatalf("concurrent limit = %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: reservation.Source, OwnerID: principal.UserID, CallID: reservation.CallID, Unknown: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, next); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unknown usage should prevent new calls: %v", err)
	}
}

func TestPostgresModelReservationRechecksRevocationAndModelVersion(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET updated_at=updated_at+interval '1 second' WHERE id=$1`, reservation.ModelID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("stale model connection = %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, reservation.ModelID).Scan(&reservation.ConnectionVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=now() WHERE id=$1`, principal.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation = %v", err)
	}
}

func TestPostgresTaskModelReservationRequiresCurrentFenceAndCommittedCheckpoint(t *testing.T) {
	store, principal, reservation := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'test')`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("checkpoint"))
	_, err := store.pool.Exec(ctx, `INSERT INTO tasks(id,owner_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,
 auth_client_installation_id,budget_active_seconds,budget_tool_calls,budget_model_tokens,usage_model_tokens,
 lease_owner,lease_version,lease_expires_at,execution_location,auth_version,auth_expires_at,agent_instance_id,
 agent_generation,agent_session_id,agent_engine_version,agent_profile_version,checkpoint_through_seq,checkpoint_digest)
 VALUES ('q1234567',$1,'c1234567','p1234567','test','running','client',$2,$3,600,10,125,0,
 'worker-1',3,clock_timestamp()+interval '1 hour','server',7,clock_timestamp()+interval '1 hour',
 'agent-1',2,'session-1','engine','profile',0,$4)`, principal.UserID, principal.TokenID, principal.ClientInstallationID, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ('q1234567',$1,'session-1',0,$2)`, principal.UserID, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ('q1234567',$1,'agent-1',2,'session-1','engine','profile','supervisor-1',$2,'worker-1',3,7,30000,
 clock_timestamp()+interval '30 seconds','active')`, principal.UserID, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: principal.UserID, LeaseOwner: "worker-1", LeaseVersion: 3,
		AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1", AuthorizationVersion: 7, ThroughSequence: 0,
		EngineVersion: "engine", ProfileVersion: "profile"}
	reservation.Source, reservation.Task, reservation.Principal = app.ModelCallTask, &fence, identity.Principal{}
	reservation.CallID, err = app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("missing checkpoint = %v", err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
 VALUES ('q1234567',$1,'agent-1',2,'session-1','session-1',0,0,$2,decode('01','hex'),'engine','profile',7,3)`, principal.UserID, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	stale := fence
	stale.AgentGeneration++
	reservation.Task = &stale
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale Agent generation = %v", err)
	}
	reservation.Task = &fence
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	settlement := app.ModelCallSettlement{Source: reservation.Source, OwnerID: principal.UserID, CallID: reservation.CallID,
		Usage: app.ModelUsage{Known: true, InputTokens: 11, OutputTokens: 7}}
	if err := store.SettleModelCall(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleModelCall(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	var used int64
	if err := store.pool.QueryRow(ctx, `SELECT usage_model_tokens FROM tasks WHERE id='q1234567'`).Scan(&used); err != nil || used != 18 {
		t.Fatalf("task charged %d tokens, %v", used, err)
	}
	childStepID := mustID(t, domain.NewStepID)
	childSessionID := "child_" + childStepID
	childDigest := sha256.Sum256([]byte("child checkpoint"))
	requestHash := sha256.Sum256([]byte("child request"))
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title,summary)
VALUES ($1,'q1234567',1,'subagent','running','Child model work','Running')`, childStepID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,parent_session_id,step_id,spawn_key,request_hash,checkpoint_through_seq,checkpoint_digest)
VALUES ('q1234567',$1,$2,'session-1',$3,'spawn_child_1',$4,0,$5)`, principal.UserID, childSessionID, childStepID, requestHash[:], childDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,
agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ('q1234567',$1,'agent-1',2,$2,'session-1',0,0,$3,decode('01','hex'),'engine','profile',7,3)`,
		principal.UserID, childSessionID, childDigest[:]); err != nil {
		t.Fatal(err)
	}
	childFence := fence
	childFence.AgentSessionID, childFence.AgentRootSessionID = childSessionID, "session-1"
	childReservation := reservation
	childReservation.Task = &childFence
	childReservation.CallID, err = app.DSHModelCallID(childFence.TaskID, childFence.AgentSessionID, childFence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	childReservation.ReservedTokens = 108
	if err := store.ReserveModelCall(ctx, childReservation); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("child exceeded the Task budget remaining after the root call: %v", err)
	}
	childReservation.ReservedTokens = reservation.ReservedTokens
	if err := store.ReserveModelCall(ctx, childReservation); err != nil {
		t.Fatalf("reserve model call for committed child checkpoint: %v", err)
	}
	if err := store.SettleModelCall(ctx, app.ModelCallSettlement{Source: childReservation.Source, OwnerID: principal.UserID,
		CallID: childReservation.CallID, Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	wrongChildHead := childFence
	wrongChildHead.ThroughSequence = 1
	childReservation.Task = &wrongChildHead
	childReservation.CallID, err = app.DSHModelCallID(wrongChildHead.TaskID, wrongChildHead.AgentSessionID, wrongChildHead.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, childReservation); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("child model call used an uncommitted checkpoint head: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_steps SET status='succeeded' WHERE task_id='q1234567' AND id=$1`, childStepID); err != nil {
		t.Fatal(err)
	}
	childReservation.Task = &childFence
	childReservation.CallID, err = app.DSHModelCallID(childFence.TaskID, childFence.AgentSessionID, childFence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, childReservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("settled child Session still authorized a model call: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_steps SET status='running' WHERE task_id='q1234567' AND id=$1`, childStepID); err != nil {
		t.Fatal(err)
	}
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	host := createTestHost(t, ctx, store, principal.UserID, capacity, nil)
	workspace := createTestWorkspace(t, ctx, store, principal.UserID, "model-task")
	other := createTestUser(t, store, 3, 10_000, 20_000)
	foreignHost := createTestHost(t, ctx, store, other.ID, capacity, nil)
	foreignWorkspace := createTestWorkspace(t, ctx, store, other.ID, "foreign-model-task")
	assertForeignKey := func(err error) {
		t.Helper()
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23503" {
			t.Fatalf("cross-account resource must be rejected by ownership constraint: %v", err)
		}
	}
	_, err = store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, fence.TaskID, foreignHost.ID)
	assertForeignKey(err)
	_, err = store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2,input_revision=0 WHERE id=$1`, fence.TaskID, foreignWorkspace.ID)
	assertForeignKey(err)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2,workspace_id=$3,input_revision=0 WHERE id=$1`, fence.TaskID, host.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	reservation.CallID = "stopped-host-model-call"
	if err := store.ReserveModelCall(ctx, reservation); err != nil {
		t.Fatalf("pure model step with stopped Host: %v", err)
	}
	settlement.CallID = reservation.CallID
	if err := store.SettleModelCall(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	reservation.CallID = "deleted-host-model-call"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='deleted',deleted_at=now() WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("deleted task Host = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='stopped',deleted_at=NULL WHERE id=$1`, host.ID); err != nil {
		t.Fatal(err)
	}
	reservation.CallID = "deleted-workspace-model-call"
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleted',deleted_at=now() WHERE id=$1`, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("deleted task Workspace = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='ready',deleted_at=NULL WHERE id=$1`, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=now() WHERE id=$1`, principal.ClientInstallationID); err != nil {
		t.Fatal(err)
	}
	reservation.CallID = "another-model-call"
	if err := store.ReserveModelCall(ctx, reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked task installation = %v", err)
	}
}

func TestPostgresSubmittedTaskRunsMeteredModelAfterRealCheckpoint(t *testing.T) {
	store, principal, _ := modelUsageFixture(t)
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.Seal([]byte("synthetic-provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET api_key_cipher=$2,updated_at=clock_timestamp() WHERE id=$1`, "p1234567", ciphertext); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	submission := newTestTaskSubmission(t, principal.UserID, "", "metered-task-0001", "hello")
	submission.Task.ModelID = "p1234567"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expires}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	if created.Task.Usage.ModelTokens == nil || *created.Task.Usage.ModelTokens != 0 {
		t.Fatalf("new task usage must begin at zero: %#v", created.Task.Usage.ModelTokens)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "metered-worker", 25*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim task = %+v, %t, %v", claimed, ok, err)
	}
	key := make([]byte, 32)
	key[0] = 23
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	started, replayed, err := agents.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: principal.UserID, AgentInstanceID: "agent-1", AgentSessionID: "session-1", SupervisorID: "supervisor-1",
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AuthorizationVersion: 1,
		EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}, SupervisorNonce: nonce, LeaseDuration: 30 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start agent = %+v, replayed=%t, %v", started, replayed, err)
	}
	checkpoint, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"header":{"id":"session-1"},"from_seq":0,"events":[{"seq":0,"type":"message"}]}`)
	digest := sha256.Sum256(payload)
	checkpointFence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: principal.UserID, AgentInstanceID: started.Fence.AgentInstanceID,
		AgentGeneration: started.Fence.AgentGeneration, AgentSessionID: started.Fence.AgentSessionID, AuthorizationVersion: 1,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}
	if _, err := checkpoint.Save(ctx, app.AgentCheckpointCommand{Fence: checkpointFence, From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatal(err)
	}
	model := &meteredProbeModel{usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 4}}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"model.example"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: claimed.ID, OwnerID: principal.UserID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AgentSessionID: started.Fence.AgentSessionID, AuthorizationVersion: 1, ThroughSequence: 0,
		EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":10}`)}
	response, err := gateway.Complete(ctx, call)
	if err != nil || response.Text != "OK" || model.calls != 1 {
		t.Fatalf("model response = %#v, calls=%d, %v", response, model.calls, err)
	}
	if _, err := gateway.Complete(ctx, call); domain.CodeOf(err) != domain.CodeConflict || model.calls != 1 {
		t.Fatalf("repeated task model call = %v, calls=%d", err, model.calls)
	}
	result, err := store.GetTask(ctx, claimed.ID, principal.UserID)
	if err != nil || result.Task.Usage.ModelTokens == nil || *result.Task.Usage.ModelTokens != 12 {
		t.Fatalf("task model usage = %#v, %v", result.Task.Usage.ModelTokens, err)
	}
	receipt := signAgentLifecycleReceipt(t, key, started.Fence, nonce)
	if _, err := agents.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Complete(ctx, call); domain.CodeOf(err) != domain.CodeUnauthenticated || model.calls != 1 {
		t.Fatalf("stopped agent still authorized a model call: calls=%d err=%v", model.calls, err)
	}
}
