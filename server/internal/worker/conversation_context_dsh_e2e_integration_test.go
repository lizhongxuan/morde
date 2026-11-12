package worker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
	storepostgres "morde/server/internal/store/postgres"
)

func TestConversationSummaryDSHMainAndSideChatsContinueAfterLongHistoryRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real DSH subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	modelClient := &conversationContextE2EModelClient{}
	modelKey, err := sealer.Seal([]byte("local-synthetic-e2e-key"))
	if err != nil {
		t.Fatal(err)
	}
	window := 32_768
	model, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{ID: "p1234567", OwnerID: owner.ID,
		Name: "Local conversation context E2E", BaseURL: "https://models.example.invalid/v1", ModelName: "synthetic-e2e",
		ContextWindowTokens: &window, APIKeyCipher: modelKey})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	model.Capabilities.Text, model.CheckedAt = true, &checkedAt
	if _, err := store.UpdateModelConnection(ctx, model); err != nil {
		t.Fatal(err)
	}
	connectionService, err := app.NewModelConnectionService(store, sealer, modelClient, []string{"models.example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, connectionService, modelClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	credential := createF01Session(t, ctx, store, owner.ID)
	principal, err := credentials.VerifySession(ctx, credential)
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify local session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	mainConversation, _, err := conversations.EnsureMain(ctx, principal, app.EnsureMainConversationCommand{IdempotencyKey: "context-e2e-main"})
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conversationIDs := []string{mainConversation.ConversationID}
	for _, suffix := range []string{"one", "two"} {
		value, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
			Title: "Context E2E side chat " + suffix, IdempotencyKey: "context-e2e-side-" + suffix,
			ServiceInstanceID: serviceInstanceID,
		})
		if err != nil {
			t.Fatal(err)
		}
		conversationIDs = append(conversationIDs, value.ID)
	}
	accountRuntimes, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewConversationCoordinatorCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	supervisors := make(map[string]*f01RecoveringNodeAccountSupervisor, len(conversationIDs)+1)
	makeCoordinator := func(conversationID, leaseOwner string) *CloudDSHConversationCoordinator {
		t.Helper()
		supervisor := &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: script},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
		supervisors[leaseOwner] = supervisor
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: accountRuntimes, Supervisor: supervisor, Guardian: &f01NodeTaskGuardian{}, StopVerifier: stopVerifier,
			TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: leaseOwner, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := NewCloudDSHConversationCoordinator(CloudDSHConversationCoordinatorOptions{
			AccountRuntimes: manager, Models: gateway, Checkpoints: checkpoints, SummaryStore: store,
		})
		if err != nil {
			t.Fatal(err)
		}
		return coordinator
	}
	coordinators := make(map[string]*CloudDSHConversationCoordinator, len(conversationIDs))
	for index, conversationID := range conversationIDs {
		coordinators[conversationID] = makeCoordinator(conversationID, fmt.Sprintf("context-e2e-runtime-%d", index))
	}
	routeModels := &conversationContextE2ERouteModels{gateway: gateway, coordinators: coordinators}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: routeModels, SummaryStore: store, SummaryInputs: store, SummarySealer: sealer,
		WorkerID: "context-e2e-route-worker", LeaseDuration: 2 * time.Minute, PollInterval: time.Millisecond,
		MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, supervisor := range supervisors {
			if command := supervisor.cmd; command != nil && command.Process != nil && command.ProcessState == nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}
	}()
	acceptAndRoute := func(conversationID, content, clientID string) string {
		t.Helper()
		message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, conversationID, app.AcceptConversationMessageCommand{
			ServiceInstanceID: serviceInstanceID, ClientMessageID: clientID, IdempotencyKey: clientID,
			Content: content, ModelID: model.ID, Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		})
		if err != nil || replayed {
			t.Fatalf("accept conversation message %q: replayed=%t err=%v", clientID, replayed, err)
		}
		found, err := routeWorker.RunOnce(ctx)
		if err != nil || !found {
			t.Fatalf("route conversation message %q: found=%t err=%v", clientID, found, err)
		}
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, message.ID).Scan(&state); err != nil || state != "routed" {
			t.Fatalf("message %q route state=%q err=%v", clientID, state, err)
		}
		return message.ID
	}
	for chatIndex, conversationID := range conversationIDs {
		for cycle := 1; cycle <= 3; cycle++ {
			for source := 1; source <= 2; source++ {
				prefix := fmt.Sprintf("E2E source chat=%d cycle=%d source=%d confirmed durable fact. ", chatIndex, cycle, source)
				content := prefix + strings.Repeat("x", 11*1024-len(prefix))
				acceptAndRoute(conversationID, content, fmt.Sprintf("context-source-%d-%d-%d", chatIndex, cycle, source))
			}
			acceptAndRoute(conversationID, fmt.Sprintf("E2E_COMPACTION_TRIGGER chat=%d cycle=%d", chatIndex, cycle),
				fmt.Sprintf("context-compact-%d-%d", chatIndex, cycle))
			version := conversationContextE2ESummaryVersion(t, ctx, pool, serviceInstanceID, owner.ID, conversationID)
			if version != int64(cycle) {
				t.Fatalf("conversation %s compaction %d produced summary version %d", conversationID, cycle, version)
			}
		}
	}
	var oldMainFenceRuntimeID string
	mainSupervisor := supervisors["context-e2e-runtime-0"]
	mainManager := coordinators[mainConversation.ConversationID].runtimes.(cloudDSHCoordinatorAccountRuntimeManager).manager
	mainSession, err := mainManager.Acquire(
		ctx, owner.ID, mainConversation.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	oldMainFence := mainSession.runtime.Fence
	oldMainFenceRuntimeID = oldMainFence.RuntimeID
	oldCommand := mainSupervisor.cmd
	if oldCommand == nil || oldCommand.Process == nil {
		t.Fatal("main conversation did not start a real Node account Runtime")
	}
	if err := oldCommand.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("simulate main DSH Runtime process restart: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- oldCommand.Wait() }()
	select {
	case waitErr := <-waitDone:
		if waitErr == nil || oldCommand.ProcessState == nil || oldCommand.ProcessState.ExitCode() != -1 {
			t.Fatalf("wait for stopped main DSH Runtime: %v", waitErr)
		}
	case <-ctx.Done():
		t.Fatalf("main DSH Runtime did not stop: %v", ctx.Err())
	}
	for deadline := time.Now().Add(2 * time.Second); !mainSession.closed() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !mainSession.closed() {
		t.Fatal("main account Runtime manager did not observe the stopped DSH process")
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, oldMainFence.ServiceInstanceID, owner.ID, oldMainFence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	var bulkMessageIDs []string
	for index := 0; index < 840; index++ {
		prefix := fmt.Sprintf("Archive history row=%04d ", index)
		if index == 100 {
			prefix += "ORCHID-ARCHIVE-SENTINEL-8472 "
		}
		content := prefix + strings.Repeat("q", 10*1024-len(prefix))
		clientID := fmt.Sprintf("context-history-%04d", index)
		message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, mainConversation.ConversationID,
			app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: clientID, IdempotencyKey: clientID,
				Content: content, ModelID: model.ID, Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}})
		if err != nil || replayed {
			t.Fatalf("seed authorized long-history message %d: replayed=%t err=%v", index, replayed, err)
		}
		bulkMessageIDs = append(bulkMessageIDs, message.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=ANY($1::text[])`, bulkMessageIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET state='routed' WHERE message_id=ANY($1::text[])`, bulkMessageIDs); err != nil {
		t.Fatal(err)
	}
	var historyBytes int64
	if err := pool.QueryRow(ctx, `SELECT sum(octet_length(content)) FROM conversation_messages WHERE conversation_id=$1`, mainConversation.ConversationID).Scan(&historyBytes); err != nil {
		t.Fatal(err)
	}
	if historyBytes <= 8*1024*1024 {
		t.Fatalf("main conversation history is only %d bytes; want more than the 8 MiB restore threshold", historyBytes)
	}
	newMainCoordinator := makeCoordinator(mainConversation.ConversationID, "context-e2e-runtime-main-restarted")
	routeModels.coordinators[mainConversation.ConversationID] = newMainCoordinator
	acceptAndRoute(mainConversation.ConversationID, "E2E_ROUTE_TO_TASK: continue using the recovered archive history", "context-final-task-main")
	for index, conversationID := range conversationIDs[1:] {
		acceptAndRoute(conversationID, fmt.Sprintf("E2E_SIDE_CHAT_CONTINUES side=%d", index+1), fmt.Sprintf("context-side-after-restart-%d", index+1))
	}
	for _, conversationID := range conversationIDs {
		want := int64(3)
		if conversationID == mainConversation.ConversationID {
			want = 4
		}
		if got := conversationContextE2ESummaryVersion(t, ctx, pool, serviceInstanceID, owner.ID, conversationID); got != want {
			t.Fatalf("conversation %s summary head=%d, want %d", conversationID, got, want)
		}
	}
	currentRuntime, found, err := accountRuntimes.Get(ctx, mainConversation.ConversationID)
	if err != nil || !found || currentRuntime.Fence.RuntimeID == oldMainFenceRuntimeID || currentRuntime.Fence.RuntimeGeneration != oldMainFence.RuntimeGeneration+1 {
		t.Fatalf("main account Runtime was not recovered after restart: runtime=%+v found=%t err=%v", currentRuntime, found, err)
	}
	if _, err := accountRuntimes.Renew(ctx, oldMainFence, app.MaxAgentLease); err == nil {
		t.Fatal("old main account Runtime fence remained usable after restart")
	}
	var settledCompactions, committedOperations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='settled'), count(*)
FROM model_call_ledger WHERE owner_id=$1 AND source='compaction'`, owner.ID).Scan(&settledCompactions, &committedOperations); err != nil || settledCompactions != 10 {
		t.Fatalf("DSH compaction model calls settled=%d err=%v", settledCompactions, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_compaction_operations WHERE owner_id=$1 AND state='committed'`, owner.ID).Scan(&committedOperations); err != nil || committedOperations != 10 {
		t.Fatalf("committed DSH compaction operations=%d err=%v", committedOperations, err)
	}
	var retainedResponses int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_compaction_operations
WHERE owner_id=$1 AND state='committed' AND octet_length(response_ciphertext)>0`, owner.ID).Scan(&retainedResponses); err != nil || retainedResponses != 0 {
		t.Fatalf("committed compaction operations retain %d temporary model responses: %v", retainedResponses, err)
	}
	modelClient.mu.Lock()
	summaryCalls, routeCalls := modelClient.summaryCalls, modelClient.routeCalls
	modelClient.mu.Unlock()
	if summaryCalls != 10 || routeCalls != 30 {
		t.Fatalf("synthetic model calls: summaries=%d routes=%d, want 10 and 30", summaryCalls, routeCalls)
	}
	var claimed task.Task
	claimed, found, err = store.ClaimNextTask(ctx, "context-e2e-history-task", time.Minute)
	if err != nil || !found || claimed.ConversationID != mainConversation.ConversationID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.Summary == nil || claimed.ConversationContext.Summary.Version != 4 {
		t.Fatalf("new Task did not receive the latest fixed summary: found=%t task=%+v err=%v", found, claimed, err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close local Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(conversationContextE2ETaskStore{store}, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := toolBroker.Definitions(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	historyToolFound := false
	for _, definition := range definitions {
		historyToolFound = historyToolFound || definition.Name == "morde_current_conversation_history_search"
	}
	if !historyToolFound {
		t.Fatal("fixed-context Task did not expose the authorized original-history search tool")
	}
	search, err := toolBroker.Execute(ctx, claimed, app.ModelToolCall{CallID: "context-e2e-history-search",
		Name: "morde_current_conversation_history_search", Arguments: `{"query":"ORCHID-ARCHIVE-SENTINEL-8472","limit":1}`})
	if err != nil || search.Status != app.AgentToolSucceeded {
		t.Fatalf("search original history through DSH tool broker: result=%+v err=%v", search, err)
	}
	var searchResult struct {
		Matches []struct {
			MessageID string `json:"message_id"`
			Content   string `json:"content"`
		} `json:"matches"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal([]byte(search.Output), &searchResult); err != nil || len(searchResult.Matches) != 1 ||
		!strings.Contains(searchResult.Matches[0].Content, "ORCHID-ARCHIVE-SENTINEL-8472") || !searchResult.Partial {
		t.Fatalf("bounded history search did not return the matching original with a partial marker: result=%+v err=%v", searchResult, err)
	}
	var pages, listed, foundSentinel int
	cursor := ""
	for pages < 20 {
		page, err := conversations.ListMessages(ctx, principal, mainConversation.ConversationID, app.PageRequest{Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		listed += len(page.Items)
		for _, message := range page.Items {
			if strings.Contains(message.Content, "ORCHID-ARCHIVE-SENTINEL-8472") {
				foundSentinel++
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if pages < 2 || listed < 840 || foundSentinel != 1 {
		t.Fatalf("authorized original-history pagination: pages=%d listed=%d sentinel=%d", pages, listed, foundSentinel)
	}
}

type conversationContextE2EModelClient struct {
	mu           sync.Mutex
	summaryCalls int
	routeCalls   int
}

func (c *conversationContextE2EModelClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	text := request.Instructions
	for _, input := range request.Input {
		text += "\n" + input.Text
		for _, part := range input.Parts {
			text += "\n" + part.Text
		}
	}
	response := `{"action":"reply","task_id":"","question":"","reply":"已收到，继续保留在当前会话。","dependency_task_ids":[]}`
	c.mu.Lock()
	if strings.Contains(text, "你负责为一个持续进行的 Morde AI 对话生成紧凑、可靠的续聊摘要") {
		c.summaryCalls++
		response = "持续会话摘要：保留已确认资料、决策与约束，后续继续从当前消息推进。"
	} else {
		c.routeCalls++
		if strings.Contains(text, "E2E_ROUTE_TO_TASK") {
			response = `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`
		}
	}
	c.mu.Unlock()
	return app.ModelResponse{Text: response, Usage: app.ModelUsage{InputTokens: 128, OutputTokens: 32, Known: true}, StopReason: "stop"}, nil
}

func (*conversationContextE2EModelClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true}, nil
}

type conversationContextE2ERouteModels struct {
	gateway      *app.ModelGatewayService
	coordinators map[string]*CloudDSHConversationCoordinator
}

type conversationContextE2ETaskStore struct{ *storepostgres.Store }

func (conversationContextE2ETaskStore) LoadDeferredTaskTarget(context.Context, task.Task) (app.DeferredTaskTarget, bool, error) {
	return app.DeferredTaskTarget{}, false, nil
}

func (m *conversationContextE2ERouteModels) ResolveConversationRouteModel(ctx context.Context, claim app.ConversationRouteClaim) (string, error) {
	return m.coordinators[claim.Message.ConversationID].ResolveConversationRouteModel(ctx, claim)
}

func (m *conversationContextE2ERouteModels) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	return m.coordinators[claim.Message.ConversationID].ClassifyConversationRoute(ctx, claim, value)
}

func (m *conversationContextE2ERouteModels) ResolveConversationSummaryModel(ctx context.Context,
	claim app.ConversationRouteClaim) (string, int, error) {
	return m.gateway.ResolveConversationSummaryModel(ctx, claim)
}

func (m *conversationContextE2ERouteModels) CompactConversationSummary(ctx context.Context,
	request CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error) {
	return m.coordinators[request.Claim.Message.ConversationID].CompactConversationSummary(ctx, request)
}

func conversationContextE2ESummaryVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	serviceInstanceID, ownerID, conversationID string) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT head_version FROM conversation_summary_heads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$2 AND conversation_id=$3 AND scope_kind='personal' AND scope_id=''`,
		serviceInstanceID, ownerID, conversationID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}
