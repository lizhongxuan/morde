package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type blockingConversationSummaryModels struct{ started chan struct{} }

type conversationSummaryTestGatewayResolver struct{ version time.Time }

type currentHistoryTestTaskStore struct{ *Store }

func (currentHistoryTestTaskStore) LoadDeferredTaskTarget(context.Context, task.Task) (app.DeferredTaskTarget, bool, error) {
	return app.DeferredTaskTarget{}, false, nil
}

func (r conversationSummaryTestGatewayResolver) ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (app.ModelGatewayEndpoint, error) {
	return app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "http://127.0.0.1", ModelID: "test"}, Version: r.version, Personal: true}, nil
}

func (r conversationSummaryTestGatewayResolver) ResolveProbeEndpoint(ctx context.Context, _ identity.Principal, _ string) (app.ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(ctx, "", "", false, false)
}

func (conversationSummaryTestGatewayResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string,
	_ time.Time, capabilities app.ModelCapabilities) (app.ModelCapabilities, error) {
	return capabilities, nil
}

func (*blockingConversationSummaryModels) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (*blockingConversationSummaryModels) ClassifyConversationRoute(context.Context, app.ConversationRouteClaim,
	app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请补充说明。"}, nil
}

func (*blockingConversationSummaryModels) ResolveConversationSummaryModel(context.Context, app.ConversationRouteClaim) (string, int, error) {
	return "p1234567", 100_000, nil
}

func (m *blockingConversationSummaryModels) CompactConversationSummary(ctx context.Context,
	_ worker.CloudDSHConversationSummaryRequest) (worker.CloudDSHConversationSummaryResult, error) {
	close(m.started)
	<-ctx.Done()
	return worker.CloudDSHConversationSummaryResult{}, ctx.Err()
}

func TestConversationMessageAcceptanceContinuesDuringSummaryModelCall(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	for index := 0; index < 18; index++ {
		claim := f.receive(t, fmt.Sprintf("summary-live-%03d", index), strings.Repeat("x", 4096), app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteReply, Reply: "已收到。",
		}); err != nil {
			t.Fatal(err)
		}
	}
	current := f.receive(t, "summary-live-current", "current request", app.ConversationRoutingInput{})
	if err := f.store.ReleaseConversationRoute(ctx, current); err != nil {
		t.Fatal(err)
	}
	models := &blockingConversationSummaryModels{started: make(chan struct{})}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	routeWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: f.store, Actions: f.store, Models: models, SummaryStore: f.store, SummaryInputs: f.store, SummarySealer: sealer,
		WorkerID: "summary-live-worker", LeaseDuration: 2 * time.Minute, PollInterval: time.Millisecond,
		MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	routeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := routeWorker.RunOnce(routeCtx)
		completed <- err
	}()
	select {
	case <-models.started:
	case <-time.After(10 * time.Second):
		t.Fatal("summary model call did not start")
	}
	accepted := f.receive(t, "summary-live-during", "accepted while summary is running", app.ConversationRoutingInput{})
	if accepted.Message.Content != "accepted while summary is running" {
		t.Fatalf("new message was blocked by summary model call: message=%+v", accepted.Message)
	}
	if err := f.store.ReleaseConversationRoute(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("summary route ended with %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("summary route did not stop after cancellation")
	}
}

func commitRouteTestSummary(t *testing.T, f routeActionsTestFixture, claim app.ConversationRouteClaim, baseline int64,
	text string, sources []app.ConversationSummarySource) {
	t.Helper()
	identity, err := app.ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	inputDigest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", baseline, text)))
	operation, _, err := f.store.ClaimConversationSummaryOperation(context.Background(), app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: operationID, InputSHA256: inputDigest[:], BaselineVersion: baseline,
		LeaseOwner: "route-summary-history-test", LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointDigest := sha256.Sum256([]byte("checkpoint:" + text))
	if _, err := f.store.SaveConversationSummaryCheckpoint(context.Background(), app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: operationID, LeaseOwner: operation.LeaseOwner, LeaseVersion: operation.LeaseVersion,
		SessionID: fmt.Sprintf("route-summary-%s-%d", claim.Message.ConversationID, baseline+1),
		Through:   claim.Message.Sequence, SHA256: checkpointDigest[:],
	}); err != nil {
		t.Fatal(err)
	}
	callID := fmt.Sprintf("route-summary-call-%s-%d", claim.Message.ConversationID, baseline+1)
	if err := f.store.AttachConversationSummaryModelCall(context.Background(), claim, operationID, operation.LeaseOwner,
		operation.LeaseVersion, callID); err != nil {
		t.Fatal(err)
	}
	sealed, err := app.SealConversationSummary(testConversationSummarySealer(t), app.ConversationSummaryDocument{
		Identity: identity, Version: baseline + 1, OperationID: operationID, Text: text, Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CommitConversationSummary(context.Background(), app.ConversationSummaryCommit{
		Claim: claim, Identity: identity, OperationID: operationID, LeaseOwner: operation.LeaseOwner,
		LeaseVersion: operation.LeaseVersion, BaselineVersion: baseline, SummarySHA256: sealed.SummarySHA256,
		SourcesSHA256: sealed.SourcesSHA256, Ciphertext: sealed.Ciphertext, Sources: sealed.Sources,
	}); err != nil {
		t.Fatal(err)
	}
}

func commitProjectRouteTestSummary(t *testing.T, store *Store, claim app.ConversationRouteClaim, text string,
	sources []app.ConversationSummarySource) {
	t.Helper()
	ctx := context.Background()
	head, err := store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Identity.ScopeKind != app.ConversationSummaryProjectScope || head.Identity.ScopeID != claim.Input.ProjectID {
		t.Fatalf("read project summary head: head=%+v err=%v", head, err)
	}
	operationID := mustID(t, domain.NewOperationID)
	inputDigest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", head.Version, text)))
	operation, _, err := store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: operationID, InputSHA256: inputDigest[:], BaselineVersion: head.Version,
		LeaseOwner: "project-summary-history-test", LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointDigest := sha256.Sum256([]byte("checkpoint:" + text))
	if _, err := store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: operationID, LeaseOwner: operation.LeaseOwner, LeaseVersion: operation.LeaseVersion,
		SessionID: fmt.Sprintf("project-summary-%s-%d", claim.Message.ConversationID, head.Version+1),
		Through:   claim.Message.Sequence, SHA256: checkpointDigest[:],
	}); err != nil {
		t.Fatal(err)
	}
	callID := fmt.Sprintf("project-summary-call-%s-%d", claim.Message.ConversationID, head.Version+1)
	if err := store.AttachConversationSummaryModelCall(ctx, claim, operationID, operation.LeaseOwner, operation.LeaseVersion, callID); err != nil {
		t.Fatal(err)
	}
	sealed, err := app.SealConversationSummary(testConversationSummarySealer(t), app.ConversationSummaryDocument{
		Identity: head.Identity, Version: head.Version + 1, OperationID: operationID, Text: text, Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitConversationSummary(ctx, app.ConversationSummaryCommit{
		Claim: claim, Identity: head.Identity, OperationID: operationID, LeaseOwner: operation.LeaseOwner,
		LeaseVersion: operation.LeaseVersion, BaselineVersion: head.Version, SummarySHA256: sealed.SummarySHA256,
		SourcesSHA256: sealed.SourcesSHA256, Ciphertext: sealed.Ciphertext, Sources: sources,
	}); err != nil {
		t.Fatal(err)
	}
}

func testConversationSummarySealer(t *testing.T) *app.Sealer {
	t.Helper()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

func createRouteTaskWithSummary(t *testing.T, f routeActionsTestFixture, text string,
	sources func(app.ConversationRouteClaim, app.ConversationRouteClaim) []app.ConversationSummarySource) (app.ConversationRouteClaim, app.ConversationRouteClaim, string) {
	t.Helper()
	ctx := context.Background()
	prior := f.receive(t, "task-summary-prior", "the earlier conversation fact", app.ConversationRoutingInput{})
	priorSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	priorSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, prior, priorSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteReply, Reply: "已记录。",
	}); err != nil {
		t.Fatal(err)
	}
	claim := f.receive(t, "task-summary-current", "use what we discussed before", app.ConversationRoutingInput{})
	commitRouteTestSummary(t, f, claim, 0, text, sources(prior, claim))
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || snapshot.Summary == nil || snapshot.Summary.Version != 1 {
		t.Fatalf("load current summary before Task creation: summary=%+v err=%v", snapshot.Summary, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("read routed Task ID = %q err=%v", taskID, err)
	}
	return prior, claim, taskID
}

func conversationSummaryMessageSource(id, content string, sequence int64) app.ConversationSummarySource {
	digest := sha256.Sum256([]byte(content))
	return app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: id, Sequence: sequence, SHA256: digest[:]}
}

func TestTaskConversationContextFixesSummaryVersionAndUncoveredHistory(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	prior, claim, taskID := createRouteTaskWithSummary(t, f, "Old summary snapshot.", func(prior, _ app.ConversationRouteClaim) []app.ConversationSummarySource {
		return []app.ConversationSummarySource{conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)}
	})
	later := f.receive(t, "task-summary-later", "another message arrives", app.ConversationRoutingInput{})
	commitRouteTestSummary(t, f, later, 1, "Newer summary snapshot.", []app.ConversationSummarySource{
		conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence),
	})
	if err := f.store.ReleaseConversationRoute(ctx, later); err != nil {
		t.Fatal(err)
	}
	var headVersion int64
	if err := f.store.pool.QueryRow(ctx, `SELECT head_version FROM conversation_summary_heads WHERE owner_id=$1 AND conversation_id=$2`,
		f.ownerID, claim.Message.ConversationID).Scan(&headVersion); err != nil || headVersion != 2 {
		t.Fatalf("summary head version = %d, err=%v; want 2", headVersion, err)
	}
	var restored *task.ConversationContext
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		restored, err = readTaskConversationContext(ctx, tx, task.Task{ID: taskID, OwnerID: f.ownerID,
			InitiatorID: f.ownerID, ConversationID: claim.Message.ConversationID, Authorization: f.authorization})
		return err
	}); err != nil {
		t.Fatalf("re-read fixed Task context after summary update: %v", err)
	}
	if restored == nil || restored.Summary == nil || restored.Summary.Version != 1 {
		t.Fatalf("re-read fixed Task summary = %+v; want version 1", restored)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "task-summary-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.Summary == nil || claimed.ConversationContext.Summary.Version != 1 {
		t.Fatalf("Task context did not retain its fixed summary version: task=%+v found=%t err=%v", claimed, found, err)
	}
	for _, message := range claimed.ConversationContext.Messages {
		if message.ID == prior.Message.ID {
			t.Fatalf("summary source message was duplicated in raw Task history: %+v", message)
		}
	}
	sealer := testConversationSummarySealer(t)
	priorDigest := sha256.Sum256([]byte(prior.Message.Content))
	document, err := app.OpenConversationSummary(sealer, app.StoredConversationSummary{Identity: app.ConversationSummaryIdentity{
		ServiceInstanceID: claimed.ConversationContext.Summary.ServiceInstanceID, OwnerID: f.ownerID,
		ActorID: f.ownerID, ConversationID: claim.Message.ConversationID, ScopeKind: app.ConversationSummaryPersonalScope,
		AuthorizationVersion: 1}, Version: claimed.ConversationContext.Summary.Version,
		OperationID: claimed.ConversationContext.Summary.OperationID, SummarySHA256: claimed.ConversationContext.Summary.SummarySHA256,
		SourcesSHA256: claimed.ConversationContext.Summary.SourcesSHA256, Ciphertext: claimed.ConversationContext.Summary.Ciphertext,
		Sources: []app.ConversationSummarySource{{Kind: app.ConversationSummarySourceMessage, ID: prior.Message.ID,
			Sequence: prior.Message.Sequence, SHA256: priorDigest[:]}}})
	if err != nil || document.Text != "Old summary snapshot." {
		t.Fatalf("Task summary text = %q err=%v", document.Text, err)
	}
}

func TestTaskCurrentConversationHistorySearchRunsThroughBroker(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	prior, claim, _ := createRouteTaskWithSummary(t, f, "A compressed snapshot.", func(prior, _ app.ConversationRouteClaim) []app.ConversationSummarySource {
		return []app.ConversationSummarySource{conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)}
	})
	claimed, found, err := f.store.ClaimNextTask(context.Background(), "current-history-tool-test", time.Minute)
	if err != nil || !found || claimed.ConversationContext == nil || claimed.ConversationContext.Summary == nil {
		t.Fatalf("claim Task with a fixed summary: found=%t context=%+v err=%v", found, claimed.ConversationContext, err)
	}
	broker, err := app.NewCloudDSHToolBroker(currentHistoryTestTaskStore{f.store}, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	foundTool := false
	for _, definition := range definitions {
		foundTool = foundTool || definition.Name == "morde_current_conversation_history_search"
	}
	if !foundTool {
		t.Fatal("current conversation history tool was not exposed to the fixed-context Task")
	}
	result, err := broker.Execute(context.Background(), claimed, app.ModelToolCall{CallID: "call_current_history_postgres",
		Name: "morde_current_conversation_history_search", Arguments: `{"query":"earlier conversation fact","limit":3}`})
	if err != nil || result.Status != app.AgentToolSucceeded {
		t.Fatalf("current history tool result=%+v err=%v", result, err)
	}
	var output struct {
		Matches []struct {
			MessageID string    `json:"message_id"`
			Content   string    `json:"content"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"matches"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Matches) != 1 || output.Matches[0].MessageID != prior.Message.ID ||
		output.Matches[0].Content != prior.Message.Content || output.Matches[0].CreatedAt.IsZero() || output.Partial {
		t.Fatalf("tool did not retrieve the summary-covered original message in the current conversation: %+v", output)
	}
	if output.Matches[0].MessageID == claim.Message.ID {
		t.Fatal("tool searched the current Task prompt beyond its fixed history boundary")
	}
}

func assertTaskSummaryRecoveryFailsBeforeClaim(t *testing.T, f routeActionsTestFixture, taskID string) {
	t.Helper()
	ctx := context.Background()
	claimed, found, err := f.store.ClaimNextTask(ctx, "task-summary-recovery-fence", time.Minute)
	if err != nil || found {
		t.Fatalf("Task with invalid fixed summary was claimed: task=%q found=%t err=%v", claimed.ID, found, err)
	}
	var status, result string
	if err := f.store.pool.QueryRow(ctx, `SELECT status,result FROM tasks WHERE id=$1`, taskID).Scan(&status, &result); err != nil ||
		status != string(task.StatusCancelling) || !strings.Contains(result, "历史上下文无法恢复") {
		t.Fatalf("Task did not follow the fenced recovery failure path: status=%q result=%q err=%v", status, result, err)
	}
	var agentCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_instances WHERE task_id=$1`, taskID).Scan(&agentCount); err != nil || agentCount != 0 {
		t.Fatalf("invalid summary started %d DSH instances: err=%v", agentCount, err)
	}
}

func TestTaskConversationSummaryFailsClosedWhenSourcesOrMemoryChange(t *testing.T) {
	t.Run("suppressed message source", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		prior, _, taskID := createRouteTaskWithSummary(t, f, "Summary from a message.", func(prior, _ app.ConversationRouteClaim) []app.ConversationSummarySource {
			return []app.ConversationSummarySource{conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)}
		})
		if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO personal_history_suppressions(service_instance_id,owner_id,message_id)
VALUES ($1::uuid,$2,$3)`, f.instanceID, f.ownerID, prior.Message.ID); err != nil {
			t.Fatal(err)
		}
		assertTaskSummaryRecoveryFailsBeforeClaim(t, f, taskID)
	})

	t.Run("corrupted source proof", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		_, claim, taskID := createRouteTaskWithSummary(t, f, "Summary with a pinned proof.", func(prior, _ app.ConversationRouteClaim) []app.ConversationSummarySource {
			return []app.ConversationSummarySource{conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)}
		})
		result, err := f.store.pool.Exec(context.Background(), `UPDATE conversation_summary_sources SET source_digest=decode(repeat('00',32),'hex')
WHERE owner_id=$1 AND conversation_id=$2 AND summary_version=1`, f.ownerID, claim.Message.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if result.RowsAffected() == 0 {
			t.Fatal("corrupt summary proof did not match a stored source")
		}
		assertTaskSummaryRecoveryFailsBeforeClaim(t, f, taskID)
	})

	t.Run("memory disabled", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
		memory := personalObjectMutationForTest(t, f.ownerID, "summary-memory-create", personal.ObjectTypeMemory, "global", 0,
			`{"content":"The user prefers morning meetings.","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, memory); err != nil {
			t.Fatal(err)
		}
		_, _, taskID := createRouteTaskWithSummary(t, f, "Summary from a personal memory.", func(_, claim app.ConversationRouteClaim) []app.ConversationSummarySource {
			identityValue, err := app.ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			source := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMemory, ID: "global",
				ObjectType: personal.ObjectTypeMemory, Version: 1}
			source.SHA256 = app.ConversationSummaryMemorySourceDigest(identityValue, source)
			return []app.ConversationSummarySource{source}
		})
		disabled := personalObjectMutationForTest(t, f.ownerID, "summary-memory-disable", personal.ObjectTypeSkillSetting, "builtin_memory", 0,
			`{"enabled":false}`, false)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, disabled); err != nil {
			t.Fatal(err)
		}
		assertTaskSummaryRecoveryFailsBeforeClaim(t, f, taskID)
	})

	t.Run("memory version changed", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
		memory := personalObjectMutationForTest(t, f.ownerID, "summary-memory-create", personal.ObjectTypeMemory, "global", 0,
			`{"content":"The user prefers morning meetings.","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, memory); err != nil {
			t.Fatal(err)
		}
		_, _, taskID := createRouteTaskWithSummary(t, f, "Summary from a personal memory.", func(_, claim app.ConversationRouteClaim) []app.ConversationSummarySource {
			identityValue, err := app.ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			source := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMemory, ID: "global",
				ObjectType: personal.ObjectTypeMemory, Version: 1}
			source.SHA256 = app.ConversationSummaryMemorySourceDigest(identityValue, source)
			return []app.ConversationSummarySource{source}
		})
		updated := personalObjectMutationForTest(t, f.ownerID, "summary-memory-update", personal.ObjectTypeMemory, "global", 1,
			`{"content":"The user prefers afternoon meetings.","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, updated); err != nil {
			t.Fatal(err)
		}
		assertTaskSummaryRecoveryFailsBeforeClaim(t, f, taskID)
	})

	t.Run("memory change invalidates an uncommitted compaction", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		ctx := context.Background()
		principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
		if _, err := f.store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, f.ownerID,
			"summary-memory-operation-create", personal.ObjectTypeMemory, "global", 0,
			`{"content":"The user prefers morning meetings.","category":"fact"}`, false)); err != nil {
			t.Fatal(err)
		}
		claim := f.receive(t, "summary-memory-operation-current", "Prepare the next meeting.", app.ConversationRoutingInput{})
		inputDigest := sha256.Sum256([]byte("memory-bound in-flight input"))
		operation, _, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
			Claim: claim, InputSHA256: inputDigest[:], BaselineVersion: 0,
			LeaseOwner: "memory-change-test", LeaseDuration: 30 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, f.ownerID,
			"summary-memory-operation-update", personal.ObjectTypeMemory, "global", 1,
			`{"content":"The user prefers afternoon meetings.","category":"fact"}`, false)); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_compaction_operations WHERE operation_id=$1`, operation.OperationID).Scan(&state); err != nil || state != "invalidated" {
			t.Fatalf("uncommitted compaction state=%q err=%v", state, err)
		}
		if _, err := f.store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
			Claim: claim, OperationID: operation.OperationID, LeaseOwner: operation.LeaseOwner, LeaseVersion: operation.LeaseVersion,
			SessionID: "memory-change-stale-session", Through: claim.Message.Sequence, SHA256: inputDigest[:],
		}); domain.CodeOf(err) == "" {
			t.Fatal("in-flight compaction checkpoint was accepted after memory changed")
		}
	})

	t.Run("deleting memory invalidates matching history and summary", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
		memory := personalObjectMutationForTest(t, f.ownerID, "summary-memory-delete-create", personal.ObjectTypeMemory, "global", 0,
			`{"content":"earlier conversation fact","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, memory); err != nil {
			t.Fatal(err)
		}
		prior, claim, taskID := createRouteTaskWithSummary(t, f, "Summary from a personal memory and message.", func(prior, claim app.ConversationRouteClaim) []app.ConversationSummarySource {
			identityValue, err := app.ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			memorySource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMemory, ID: "global",
				ObjectType: personal.ObjectTypeMemory, Version: 1}
			memorySource.SHA256 = app.ConversationSummaryMemorySourceDigest(identityValue, memorySource)
			return []app.ConversationSummarySource{
				conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence), memorySource,
			}
		})
		deletion := personalObjectMutationForTest(t, f.ownerID, "summary-memory-delete", personal.ObjectTypeMemory, "global", 1, "", true)
		if _, err := f.store.ApplyPersonalMutation(context.Background(), principal, deletion); err != nil {
			t.Fatal(err)
		}
		assertTaskSummaryRecoveryFailsBeforeClaim(t, f, taskID)
		var suppressed int
		var summaryState, raw string
		if err := f.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM personal_history_suppressions WHERE owner_id=$1 AND message_id=$2`,
			f.ownerID, prior.Message.ID).Scan(&suppressed); err != nil || suppressed != 1 {
			t.Fatalf("deleted memory history suppression count=%d err=%v", suppressed, err)
		}
		if err := f.store.pool.QueryRow(context.Background(), `SELECT state FROM conversation_summary_versions WHERE owner_id=$1 AND conversation_id=$2 AND version=1`,
			f.ownerID, claim.Message.ConversationID).Scan(&summaryState); err != nil || summaryState != "invalid" {
			t.Fatalf("memory-dependent summary state=%q err=%v", summaryState, err)
		}
		if err := f.store.pool.QueryRow(context.Background(), `SELECT content FROM conversation_messages WHERE id=$1`, prior.Message.ID).Scan(&raw); err != nil || raw != prior.Message.Content {
			t.Fatalf("memory deletion changed the raw conversation record: %q err=%v", raw, err)
		}
	})

	t.Run("in-flight task rechecks changed memory before tools", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		ctx := context.Background()
		principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
		memory := personalObjectMutationForTest(t, f.ownerID, "summary-memory-live-create", personal.ObjectTypeMemory, "global", 0,
			`{"content":"The user prefers morning meetings.","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(ctx, principal, memory); err != nil {
			t.Fatal(err)
		}
		_, _, taskID := createRouteTaskWithSummary(t, f, "Summary from a personal memory.", func(_, claim app.ConversationRouteClaim) []app.ConversationSummarySource {
			identityValue, err := app.ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			source := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMemory, ID: "global",
				ObjectType: personal.ObjectTypeMemory, Version: 1}
			source.SHA256 = app.ConversationSummaryMemorySourceDigest(identityValue, source)
			return []app.ConversationSummarySource{source}
		})
		claimed, found, err := f.store.ClaimNextTask(ctx, "summary-live-task", time.Minute)
		if err != nil || !found || claimed.ID != taskID || claimed.ConversationContext == nil || claimed.ConversationContext.Summary == nil {
			t.Fatalf("claim Task before memory change: found=%t err=%v", found, err)
		}
		updated := personalObjectMutationForTest(t, f.ownerID, "summary-memory-live-update", personal.ObjectTypeMemory, "global", 1,
			`{"content":"The user prefers afternoon meetings.","category":"fact"}`, false)
		if _, err := f.store.ApplyPersonalMutation(ctx, principal, updated); err != nil {
			t.Fatal(err)
		}
		broker, err := app.NewCloudDSHToolBroker(currentHistoryTestTaskStore{f.store}, &app.RuntimeToolExecutor{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := broker.Execute(ctx, claimed, app.ModelToolCall{CallID: "call_after_memory_change",
			Name: "morde_current_conversation_history_search", Arguments: `{"query":"meeting","limit":1}`}); err == nil {
			t.Fatal("in-flight Task used a summary after its memory source changed")
		}
		var toolCalls, steps int64
		if err := f.store.pool.QueryRow(ctx, `SELECT usage_tool_calls,(SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.step')
FROM tasks WHERE id=$1`, taskID).Scan(&toolCalls, &steps); err != nil || toolCalls != 0 || steps != 0 {
			t.Fatalf("invalidated summary caused a tool side effect: calls=%d steps=%d err=%v", toolCalls, steps, err)
		}
	})
}

func TestConversationRouteSelectsOlderCausallyValidSummaryAndRejectsStaleDispatch(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	source := f.receive(t, "route-summary-fallback-source", "authorized older fact", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, source, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	descendant := f.receive(t, "route-summary-fallback-descendant", "另外写一首短诗", app.ConversationRoutingInput{
		PreviousClientMessageID: "route-summary-fallback-parent",
	})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, descendant)
	if err != nil || !snapshot.PreviousMissing {
		t.Fatalf("descendant did not retain its missing predecessor fence: snapshot=%+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	var modelVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&modelVersion); err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(f.store, conversationSummaryTestGatewayResolver{version: modelVersion}, &routeModelClientTest{
		reply: `{"action":"new","task_id":"","question":""}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := gateway.ClassifyConversationRoute(ctx, descendant, snapshot.Context)
	if err != nil {
		t.Fatalf("classify independent descendant: %v", err)
	}
	if err := f.store.ApplyConversationRoute(ctx, descendant, snapshot, decision); err != nil {
		t.Fatal(err)
	}
	parent := f.receive(t, "route-summary-fallback-parent", "请检查刚才的内容", app.ConversationRoutingInput{})
	middle := f.receive(t, "route-summary-fallback-middle", "continue other work", app.ConversationRoutingInput{})
	sourceDigest := sha256.Sum256([]byte(source.Message.Content))
	oldSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: source.Message.ID,
		Sequence: source.Message.Sequence, SHA256: sourceDigest[:]}
	commitRouteTestSummary(t, f, middle, 0, "version one", []app.ConversationSummarySource{oldSource})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, middle)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, middle, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	current := f.receive(t, "route-summary-fallback-current", "route a later message", app.ConversationRoutingInput{})
	descendantDigest := sha256.Sum256([]byte(descendant.Message.Content))
	descendantSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: descendant.Message.ID,
		Sequence: descendant.Message.Sequence, SHA256: descendantDigest[:]}
	commitRouteTestSummary(t, f, current, 1, "version two contains a causal descendant", []app.ConversationSummarySource{oldSource, descendantSource})
	parentSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, parent)
	if err != nil || parentSnapshot.Summary == nil || parentSnapshot.Summary.Version != 1 {
		t.Fatalf("route did not fall back to a causally valid older summary: summary=%+v err=%v", parentSnapshot.Summary, err)
	}
	sealer := testConversationSummarySealer(t)
	document, err := app.OpenConversationSummary(sealer, *parentSnapshot.Summary)
	if err != nil || document.Text != "version one" {
		t.Fatalf("selected older summary could not be verified: document=%+v err=%v", document, err)
	}
	modelContext := parentSnapshot.Context
	modelContext.ModelID, modelContext.SummaryText = "p1234567", document.Text
	modelContext.History = app.ConversationRouteHistoryWithoutSummarySources(modelContext.History, parentSnapshot.Summary.Sources)
	fence := app.ConversationRouteModelFence{Claim: parent, Context: modelContext}
	checkFence := func() error {
		return f.store.WithTx(ctx, func(tx pgx.Tx) error {
			return checkConversationRouteModelFence(ctx, tx, fence, "p1234567")
		})
	}
	if err := checkFence(); err != nil {
		t.Fatalf("dispatch fence rejected the proven older summary: %v", err)
	}
	future := f.receive(t, "route-summary-fallback-future", "another later message", app.ConversationRoutingInput{})
	commitRouteTestSummary(t, f, future, 2, "version three remains authorized", []app.ConversationSummarySource{oldSource})
	if err := checkFence(); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch fence accepted a superseded summary context: %v", err)
	}
}

func TestConversationSummaryIsolatedAcrossMainAndSideChats(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	conversationIDs := []string{f.conversation.ID}
	for _, key := range []string{"summary-side-chat-a", "summary-side-chat-b"} {
		id := mustID(t, domain.NewConversationID)
		created, _, err := f.store.CreateConversation(ctx, conversation.Conversation{ID: id, OwnerID: f.ownerID},
			testIdempotency(f.ownerID, "conversation.create", key))
		if err != nil {
			t.Fatal(err)
		}
		conversationIDs = append(conversationIDs, created.ID)
	}
	type chatSource struct {
		source app.ConversationSummarySource
	}
	sources := make([]chatSource, 0, len(conversationIDs))
	for index, conversationID := range conversationIDs {
		sourceText := fmt.Sprintf("chat %d private fact", index)
		claim := f.receiveInConversation(t, conversationID, fmt.Sprintf("summary-chat-source-%d", index), sourceText, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteReply, Reply: "已收到。",
		}); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(sourceText))
		sources = append(sources, chatSource{source: app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage,
			ID: claim.Message.ID, Sequence: claim.Message.Sequence, SHA256: digest[:]}})
	}
	sealer := testConversationSummarySealer(t)
	for index, conversationID := range conversationIDs {
		claim := f.receiveInConversation(t, conversationID, fmt.Sprintf("summary-chat-current-%d", index), "continue this chat", app.ConversationRoutingInput{})
		text := fmt.Sprintf("summary unique to chat %d", index)
		commitRouteTestSummary(t, f, claim, 0, text, []app.ConversationSummarySource{sources[index].source})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil || snapshot.Summary == nil || snapshot.Summary.Identity.ConversationID != conversationID {
			t.Fatalf("chat %d loaded another conversation's summary identity: summary=%+v err=%v", index, snapshot.Summary, err)
		}
		document, err := app.OpenConversationSummary(sealer, *snapshot.Summary)
		if err != nil || document.Text != text {
			t.Fatalf("chat %d summary leaked or changed: document=%+v err=%v", index, document, err)
		}
	}
}

func TestConversationSummaryPersistenceLeasesReplayAndInvalidation(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	sourceClaim := f.receive(t, "summary-source-001", "The user prefers morning meetings.", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, sourceClaim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, sourceClaim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	sourceClaim2 := f.receive(t, "summary-source-002", "The user is free after 10 a.m.", app.ConversationRoutingInput{})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, sourceClaim2)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, sourceClaim2, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claim := f.receive(t, "summary-current-001", "Plan the next meeting.", app.ConversationRoutingInput{})
	identity, err := app.ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	head, err := f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 0 {
		t.Fatalf("initial summary head = %+v, %v", head, err)
	}
	inputDigest := sha256.Sum256([]byte("authorized history and latest source"))
	operationIDs := []string{mustID(t, domain.NewOperationID), mustID(t, domain.NewOperationID)}
	type claimResult struct {
		operation app.ConversationSummaryOperation
		err       error
		worker    string
	}
	results := make(chan claimResult, len(operationIDs))
	for index, operationID := range operationIDs {
		worker := "summary-worker-" + string(rune('a'+index))
		go func(operationID, worker string) {
			operation, _, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
				Claim: claim, OperationID: operationID, InputSHA256: inputDigest[:], BaselineVersion: 0,
				LeaseOwner: worker, LeaseDuration: 30 * time.Second,
			})
			results <- claimResult{operation: operation, err: err, worker: worker}
		}(operationID, worker)
	}
	var winner claimResult
	conflicts := 0
	for range operationIDs {
		result := <-results
		if result.err == nil {
			winner = result
		} else if domain.CodeOf(result.err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("claim competing summary worker: %v", result.err)
		}
	}
	if winner.err != nil || winner.operation.OperationID == "" || conflicts != 1 {
		t.Fatalf("summary claim winner=%+v conflicts=%d", winner, conflicts)
	}
	oldLeaseOwner, oldLeaseVersion := winner.operation.LeaseOwner, winner.operation.LeaseVersion
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_summary_heads SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
		identity.ServiceInstanceID, identity.OwnerID, identity.ActorID, identity.ConversationID, identity.ScopeKind, identity.ScopeID); err != nil {
		t.Fatal(err)
	}
	recovered, replayed, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, InputSHA256: inputDigest[:], BaselineVersion: 0,
		LeaseOwner: "summary-worker-recovered", LeaseDuration: 30 * time.Second,
	})
	if err != nil || replayed || recovered.OperationID != winner.operation.OperationID || recovered.LeaseVersion <= oldLeaseVersion {
		t.Fatalf("recover expired summary lease = %+v replayed=%t err=%v", recovered, replayed, err)
	}
	checkpointDigest := sha256.Sum256([]byte("durable DSH checkpoint identity"))
	checkpoint := app.ConversationSummaryCheckpoint{Claim: claim, OperationID: recovered.OperationID,
		LeaseOwner: recovered.LeaseOwner, LeaseVersion: recovered.LeaseVersion, SessionID: "summary-session-1",
		Through: claim.Message.Sequence + 100, SHA256: checkpointDigest[:]}
	invalidCheckpoint := checkpoint
	invalidCheckpoint.Through = 1 << 53
	if _, err := f.store.SaveConversationSummaryCheckpoint(ctx, invalidCheckpoint); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("checkpoint exceeded the DSH sequence limit: %v", err)
	}
	if _, err := f.store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: recovered.OperationID, LeaseOwner: oldLeaseOwner, LeaseVersion: oldLeaseVersion,
		SessionID: checkpoint.SessionID, Through: checkpoint.Through, SHA256: checkpointDigest[:],
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale summary worker checkpoint returned %v", err)
	}
	if _, err := f.store.SaveConversationSummaryCheckpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AttachConversationSummaryModelCall(ctx, claim, recovered.OperationID, recovered.LeaseOwner,
		recovered.LeaseVersion, "summary-call-1"); err != nil {
		t.Fatal(err)
	}
	sourceDigest := sha256.Sum256([]byte(sourceClaim.Message.Content))
	sources := []app.ConversationSummarySource{{Kind: app.ConversationSummarySourceMessage, ID: sourceClaim.Message.ID,
		Sequence: sourceClaim.Message.Sequence, SHA256: sourceDigest[:]}}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	document := app.ConversationSummaryDocument{Identity: identity, Version: 1, OperationID: recovered.OperationID,
		Text: "The user prefers morning meetings.", Sources: sources}
	sealed, err := app.SealConversationSummary(sealer, document)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Ciphertext, []byte(document.Text)) {
		t.Fatal("summary plaintext was stored in the ciphertext")
	}
	foreignClaim := receiveConversationSummarySourceForOtherAccount(t, f.store, identity.ServiceInstanceID)
	foreignDigest := sha256.Sum256([]byte(foreignClaim.Message.Content))
	foreignSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: foreignClaim.Message.ID,
		Sequence: foreignClaim.Message.Sequence, SHA256: foreignDigest[:]}
	foreignDocument := document
	foreignDocument.Sources = []app.ConversationSummarySource{foreignSource}
	foreignSealed, err := app.SealConversationSummary(sealer, foreignDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CommitConversationSummary(ctx, app.ConversationSummaryCommit{
		Claim: claim, Identity: identity, OperationID: recovered.OperationID, LeaseOwner: recovered.LeaseOwner,
		LeaseVersion: recovered.LeaseVersion, BaselineVersion: 0, SummarySHA256: foreignSealed.SummarySHA256,
		SourcesSHA256: foreignSealed.SourcesSHA256, Ciphertext: foreignSealed.Ciphertext, Sources: foreignSealed.Sources,
	}); domain.CodeOf(err) == "" {
		t.Fatal("committed a summary with a source from another conversation")
	}
	head, err = f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 0 {
		t.Fatalf("failed source proof advanced summary head: %+v, %v", head, err)
	}
	if err := f.store.CommitConversationSummary(ctx, app.ConversationSummaryCommit{
		Claim: claim, Identity: identity, OperationID: recovered.OperationID, LeaseOwner: recovered.LeaseOwner,
		LeaseVersion: recovered.LeaseVersion, BaselineVersion: 0, SummarySHA256: sealed.SummarySHA256,
		SourcesSHA256: sealed.SourcesSHA256, Ciphertext: sealed.Ciphertext, Sources: sealed.Sources,
	}); err != nil {
		t.Fatal(err)
	}
	secondOperationID := mustID(t, domain.NewOperationID)
	secondInputDigest := sha256.Sum256([]byte("same source plus next authorized message"))
	secondOperation, _, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: secondOperationID, InputSHA256: secondInputDigest[:], BaselineVersion: 1,
		LeaseOwner: "summary-worker-v2", LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondCheckpointDigest := sha256.Sum256([]byte("durable DSH checkpoint version 2"))
	if _, err := f.store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: secondOperationID, LeaseOwner: secondOperation.LeaseOwner,
		LeaseVersion: secondOperation.LeaseVersion, SessionID: "summary-session-2",
		Through: claim.Message.Sequence + 200, SHA256: secondCheckpointDigest[:],
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AttachConversationSummaryModelCall(ctx, claim, secondOperationID, secondOperation.LeaseOwner,
		secondOperation.LeaseVersion, "summary-call-2"); err != nil {
		t.Fatal(err)
	}
	sourceDigest2 := sha256.Sum256([]byte(sourceClaim2.Message.Content))
	sources2 := append(append([]app.ConversationSummarySource(nil), sources...), app.ConversationSummarySource{
		Kind: app.ConversationSummarySourceMessage, ID: sourceClaim2.Message.ID,
		Sequence: sourceClaim2.Message.Sequence, SHA256: sourceDigest2[:],
	})
	document2 := app.ConversationSummaryDocument{Identity: identity, Version: 2, OperationID: secondOperationID,
		Text: "The user prefers morning meetings and is free after 10 a.m.", Sources: sources2}
	sealed2, err := app.SealConversationSummary(sealer, document2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CommitConversationSummary(ctx, app.ConversationSummaryCommit{
		Claim: claim, Identity: identity, OperationID: secondOperationID, LeaseOwner: secondOperation.LeaseOwner,
		LeaseVersion: secondOperation.LeaseVersion, BaselineVersion: 1, SummarySHA256: sealed2.SummarySHA256,
		SourcesSHA256: sealed2.SourcesSHA256, Ciphertext: sealed2.Ciphertext, Sources: sealed2.Sources,
	}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := f.store.ReadConversationSummary(ctx, claim)
	if err != nil || !found {
		t.Fatalf("read committed summary found=%t err=%v", found, err)
	}
	opened, err := app.OpenConversationSummary(sealer, stored)
	if err != nil || opened.Text != document2.Text || opened.Identity != identity || stored.Version != 2 || stored.Previous == nil || *stored.Previous != 1 {
		t.Fatalf("open committed summary = %+v, %v", opened, err)
	}
	replayOperation, wasReplay, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: recovered.OperationID, InputSHA256: inputDigest[:], BaselineVersion: 0,
		LeaseOwner: "summary-worker-replayed", LeaseDuration: 30 * time.Second,
	})
	if err != nil || !wasReplay || replayOperation.State != "committed" {
		t.Fatalf("committed summary operation replay = %+v replayed=%t err=%v", replayOperation, wasReplay, err)
	}
	changedInput := sha256.Sum256([]byte("different history"))
	if _, _, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: recovered.OperationID, InputSHA256: changedInput[:], BaselineVersion: 0,
		LeaseOwner: "summary-worker-replayed", LeaseDuration: 30 * time.Second,
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed operation input returned %v", err)
	}
	if _, _, err := f.store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: mustID(t, domain.NewOperationID), InputSHA256: inputDigest[:], BaselineVersion: 0,
		LeaseOwner: "summary-worker-stale", LeaseDuration: 30 * time.Second,
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale baseline accepted: %v", err)
	}
	invalidated, err := f.store.InvalidateConversationSummarySource(ctx, app.ConversationSummarySourceInvalidation{
		OwnerID: f.ownerID, Kind: app.ConversationSummarySourceMessage, ID: sourceClaim2.Message.ID,
		Sequence: sourceClaim2.Message.Sequence, Reason: "source_suppressed",
	})
	if err != nil || invalidated != 1 {
		t.Fatalf("invalidate summary source count=%d err=%v", invalidated, err)
	}
	head, err = f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 1 {
		t.Fatalf("summary did not fall back to prior valid version: %+v, %v", head, err)
	}
	stored, found, err = f.store.ReadConversationSummary(ctx, claim)
	if err != nil || !found || stored.Version != 1 {
		t.Fatalf("valid prior summary was not readable: version=%d found=%t err=%v", stored.Version, found, err)
	}
	invalidated, err = f.store.InvalidateConversationSummarySource(ctx, app.ConversationSummarySourceInvalidation{
		OwnerID: f.ownerID, Kind: app.ConversationSummarySourceMessage, ID: sourceClaim.Message.ID,
		Sequence: sourceClaim.Message.Sequence, Reason: "source_suppressed",
	})
	if err != nil || invalidated != 1 {
		t.Fatalf("invalidate prior summary source count=%d err=%v", invalidated, err)
	}
	head, err = f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 0 {
		t.Fatalf("invalidated summary chain head = %+v, %v", head, err)
	}
	if _, found, err := f.store.ReadConversationSummary(ctx, claim); err != nil || found {
		t.Fatalf("invalidated summary remained readable: found=%t err=%v", found, err)
	}
	var rawContent, rawContent2 string
	if err := f.store.pool.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1`, sourceClaim.Message.ID).Scan(&rawContent); err != nil || rawContent != sourceClaim.Message.Content {
		t.Fatalf("source message was changed or lost: %q err=%v", rawContent, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1`, sourceClaim2.Message.ID).Scan(&rawContent2); err != nil || rawContent2 != sourceClaim2.Message.Content {
		t.Fatalf("second source message was changed or lost: %q err=%v", rawContent2, err)
	}
}

func TestProjectConversationSummaryRequiresCurrentMembership(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	claim := projectHistoryClaim(t, f, "project-summary-member-001")
	head, err := f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Identity.ScopeKind != app.ConversationSummaryProjectScope || head.Identity.ScopeID != f.projectID || head.Identity.ScopeVersion < 1 {
		t.Fatalf("project summary scope = %+v, %v", head, err)
	}
	foreign := claim
	foreign.Message.ActorID = f.outsider.UserID
	foreign.Authorization.TokenID = f.outsider.TokenID
	if _, err := f.store.ReadConversationSummaryHead(ctx, foreign); err == nil {
		t.Fatal("nonmember read a project conversation summary head")
	}
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadConversationSummaryHead(ctx, claim); err == nil {
		t.Fatal("revoked project member kept access to summary state")
	}
}

func TestProjectTaskSummaryIsInvalidatedWhenMemberIsRemoved(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'project summary test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	const sourceText = "member-only prior project detail"
	sourceID := projectHistoryInsertMessage(t, f, f.member.UserID, "", "", sourceText, conversation.RoleUser)
	var sourceSequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT sequence FROM conversation_messages WHERE id=$1`, sourceID).Scan(&sourceSequence); err != nil {
		t.Fatal(err)
	}
	claim := projectHistoryClaim(t, f, "project-summary-removal-task")
	source := conversationSummaryMessageSource(sourceID, sourceText, sourceSequence)
	commitProjectRouteTestSummary(t, f.store, claim, "A member-scoped project summary.", []app.ConversationSummarySource{source})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || snapshot.Summary == nil || snapshot.Summary.Version != 1 {
		t.Fatalf("read project summary before Task creation: summary=%+v err=%v", snapshot.Summary, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil || taskID == "" {
		t.Fatalf("read project Task ID=%q err=%v", taskID, err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "project-summary-removal-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID || claimed.ConversationContext == nil || claimed.ConversationContext.Summary == nil {
		t.Fatalf("claim Task with project summary: found=%t context=%+v err=%v", found, claimed.ConversationContext, err)
	}
	projectValue, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveProjectMember(ctx, f.owner.UserID, f.projectID, f.member.UserID, projectValue.Version,
		"project-summary-remove-member"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_summary_versions WHERE owner_id=$1 AND actor_id=$2
 AND conversation_id=$3 AND scope_kind='project' AND scope_id=$4 AND version=1`,
		f.owner.UserID, f.member.UserID, f.conversationID, f.projectID).Scan(&state); err != nil || state != "invalid" {
		t.Fatalf("revoked member project summary state=%q err=%v", state, err)
	}
	if err := f.store.ValidateTaskConversationSummary(ctx, claimed); err == nil {
		t.Fatal("revoked project member Task summary still validated")
	}
	broker, err := app.NewCloudDSHToolBroker(currentHistoryTestTaskStore{f.store}, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Execute(ctx, claimed, app.ModelToolCall{CallID: "call_after_project_revocation",
		Name: "morde_current_conversation_history_search", Arguments: `{"query":"project detail","limit":1}`}); err == nil {
		t.Fatal("revoked project Task used the summary or searched the conversation")
	}
	var toolCalls int64
	if err := f.store.pool.QueryRow(ctx, `SELECT usage_tool_calls FROM tasks WHERE id=$1`, taskID).Scan(&toolCalls); err != nil || toolCalls != 0 {
		t.Fatalf("revoked Task consumed a tool call: calls=%d err=%v", toolCalls, err)
	}
}

func TestConversationSummaryMigrationUpgradesExistingSchema(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; PostgreSQL migration integration tests were not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_summary_migration_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	config := baseConfig.Copy()
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop migration test database: %v", err)
		}
		adminPool.Close()
	})
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 173); err != nil {
		closeProvider()
		t.Fatalf("prepare existing schema at migration 173: %v", err)
	}
	closeProvider()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil || version != 173 {
		t.Fatalf("existing schema version = %d, err=%v", version, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("upgrade existing schema: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil || version != 183 {
		t.Fatalf("upgraded schema version = %d, want 183, err=%v", version, err)
	}
	var summariesExist, memorySourcesExist bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.conversation_summary_heads') IS NOT NULL,
to_regclass('public.personal_memory_sources') IS NOT NULL`).Scan(&summariesExist, &memorySourcesExist); err != nil || !summariesExist || !memorySourcesExist {
		t.Fatalf("conversation context migrations were not applied: summaries=%t memory_sources=%t err=%v", summariesExist, memorySourcesExist, err)
	}
}

func TestConversationSummaryMigrationRefusesDestructiveRollback(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	prior := f.receive(t, "summary-rollback-prior", "Keep this raw source during rollback.", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(context.Background(), prior)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(context.Background(), prior, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteReply, Reply: "Source retained.",
	}); err != nil {
		t.Fatal(err)
	}
	claim := f.receive(t, "summary-rollback-preserves-history", "Continue from the prior source.", app.ConversationRoutingInput{})
	commitRouteTestSummary(t, f, claim, 0, "A compacted summary.", []app.ConversationSummarySource{
		conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence),
	})
	provider, closeProvider, err := newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(context.Background(), 174); err == nil || !strings.Contains(err.Error(), "cannot remove conversation compaction model call records") {
		t.Fatalf("rollback with a persisted summary model call was allowed: %v", err)
	}
	var versionState, rawContent string
	if err := f.store.pool.QueryRow(context.Background(), `SELECT state FROM conversation_summary_versions WHERE owner_id=$1 AND conversation_id=$2 AND version=1`,
		f.ownerID, claim.Message.ConversationID).Scan(&versionState); err != nil || versionState != "valid" {
		t.Fatalf("summary was changed by refused rollback: state=%q err=%v", versionState, err)
	}
	if err := f.store.pool.QueryRow(context.Background(), `SELECT content FROM conversation_messages WHERE id=$1`, prior.Message.ID).Scan(&rawContent); err != nil || rawContent != prior.Message.Content {
		t.Fatalf("raw history was changed by refused rollback: content=%q err=%v", rawContent, err)
	}
}

func receiveConversationSummarySourceForOtherAccount(t *testing.T, store *Store, instanceID string) app.ConversationRouteClaim {
	t.Helper()
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2000, 4096)
	token, err := store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p2234567',$1,'summary source','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	clientID := "summary-other-account-source"
	requestHash := sha256.Sum256([]byte(clientID))
	message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: conversationValue.ID, Role: conversation.RoleUser, Content: "Private content from another account."},
		ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: requestHash[:],
		Input:         app.ConversationRoutingInput{ModelID: "p2234567", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "summary-other-account-worker", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim other account message: found=%t err=%v", found, err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p2234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	return claim
}
