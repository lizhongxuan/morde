package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type taskTerminalDeliveryStoreStub struct {
	claimed       []app.TaskTerminalDeliveryClaim
	ready         []app.TaskTerminalDeliveryClaim
	facts         app.TaskTerminalDeliverySourceFacts
	readFactsErr  error
	unknown       int
	invalidated   int
	persisted     int
	published     int
	publishedText string
}

func (s *taskTerminalDeliveryStoreStub) ClaimTaskTerminalDeliverySources(_ context.Context, _ string, _ time.Duration, _ int32) ([]app.TaskTerminalDeliveryClaim, error) {
	claims := append([]app.TaskTerminalDeliveryClaim(nil), s.claimed...)
	s.claimed = nil
	return claims, nil
}

func (s *taskTerminalDeliveryStoreStub) ListTaskTerminalDeliveryReplyReady(_ context.Context, _ int32) ([]app.TaskTerminalDeliveryClaim, error) {
	return append([]app.TaskTerminalDeliveryClaim(nil), s.ready...), nil
}

func (s *taskTerminalDeliveryStoreStub) GetTaskTerminalDeliveryReplyReady(_ context.Context, id int64) (app.TaskTerminalDeliveryClaim, bool, error) {
	for _, claim := range s.ready {
		if claim.DeliveryID == id {
			return claim, true, nil
		}
	}
	return app.TaskTerminalDeliveryClaim{}, false, nil
}

func (s *taskTerminalDeliveryStoreStub) ReadTaskTerminalDeliverySourceFacts(_ context.Context,
	claim app.TaskTerminalDeliveryClaim) (app.TaskTerminalDeliverySourceFacts, bool, error) {
	if s.readFactsErr != nil {
		return app.TaskTerminalDeliverySourceFacts{}, false, s.readFactsErr
	}
	return s.facts, app.ValidTaskTerminalDeliverySourceFacts(claim, s.facts), nil
}

func (s *taskTerminalDeliveryStoreStub) MarkTaskTerminalDeliveryUnknownClaim(_ context.Context, _ app.TaskTerminalDeliveryClaim) (bool, error) {
	s.unknown++
	return true, nil
}

func (s *taskTerminalDeliveryStoreStub) PersistTaskTerminalDeliveryReplyReady(_ context.Context,
	claim app.TaskTerminalDeliveryClaim, ciphertext []byte) (app.TaskTerminalDeliveryClaim, bool, error) {
	s.persisted++
	ready := claim
	ready.State = app.TaskTerminalDeliveryStateReplyReady
	ready.ResponseKind = app.TaskTerminalDeliveryResponseSystem
	ready.LeaseOwner = ""
	ready.LeaseExpiresAt = time.Time{}
	ready.ResponseCiphertext = append([]byte(nil), ciphertext...)
	digest := sha256.Sum256(ciphertext)
	ready.ResponseSHA256 = digest[:]
	s.ready = []app.TaskTerminalDeliveryClaim{ready}
	return ready, true, nil
}

func (s *taskTerminalDeliveryStoreStub) InvalidateTaskTerminalDeliveryClaim(_ context.Context, _ app.TaskTerminalDeliveryClaim) (bool, error) {
	s.invalidated++
	return true, nil
}

func (s *taskTerminalDeliveryStoreStub) PublishTaskTerminalDeliveryReply(_ context.Context,
	_ app.TaskTerminalDeliveryClaim, content string) (bool, string, error) {
	s.published++
	s.publishedText = content
	return true, mustWorkerMessageID(), nil
}

type taskTerminalDeliveryGatewayStub struct {
	modelID    string
	resolveErr error
	recoveries []struct {
		response app.ModelResponse
		found    bool
		err      error
	}
	completeResponse app.ModelResponse
	completeStatus   app.TaskTerminalDeliveryModelCallDisposition
	completeErr      error
	resolveCalls     int
	completeCalls    int
	recoverCalls     int
	lastRequest      json.RawMessage
	lastFence        app.TaskTerminalDeliveryModelFence
}

func (g *taskTerminalDeliveryGatewayStub) ResolveTaskTerminalDeliveryModel(_ context.Context,
	fence app.TaskTerminalDeliveryModelFence) (string, error) {
	g.resolveCalls++
	g.lastFence = fence
	if g.resolveErr != nil {
		return "", g.resolveErr
	}
	return g.modelID, nil
}

func (g *taskTerminalDeliveryGatewayStub) CompleteTaskTerminalDeliveryDSHModel(_ context.Context,
	fence app.TaskTerminalDeliveryModelFence, _ string, raw json.RawMessage) (app.ModelResponse,
	app.TaskTerminalDeliveryModelCallDisposition, error) {
	g.completeCalls++
	g.lastFence = fence
	g.lastRequest = append([]byte(nil), raw...)
	return g.completeResponse, g.completeStatus, g.completeErr
}

func (g *taskTerminalDeliveryGatewayStub) RecoverTaskTerminalDeliveryDSHModelResponse(_ context.Context,
	fence app.TaskTerminalDeliveryModelFence, raw json.RawMessage) (app.ModelResponse, bool, error) {
	g.recoverCalls++
	g.lastFence = fence
	g.lastRequest = append([]byte(nil), raw...)
	if len(g.recoveries) == 0 {
		return app.ModelResponse{}, false, nil
	}
	value := g.recoveries[0]
	g.recoveries = g.recoveries[1:]
	return value.response, value.found, value.err
}

func TestTaskTerminalDeliveryWorkerUnknownPublishesOneSystemFallback(t *testing.T) {
	store, claim := taskTerminalDeliveryTestStore(t, app.TaskTerminalDeliveryStateClaimed)
	providerErr := errors.New("provider outcome is ambiguous")
	gateway := &taskTerminalDeliveryGatewayStub{modelID: "p1234567", completeStatus: app.TaskTerminalDeliveryModelUnknown,
		completeErr: providerErr}
	worker := newTaskTerminalDeliveryTestWorker(t, store, gateway)
	found, err := worker.RunOnce(context.Background())
	if !found || err != nil || store.unknown != 0 || store.persisted != 1 || store.published != 1 || gateway.completeCalls != 1 ||
		!strings.Contains(store.publishedText, "这次没有拿到已核实的结果") {
		t.Fatalf("unknown model call handling: found=%t err=%v unknown=%d fallback=%d published=%d completes=%d text=%q",
			found, err, store.unknown, store.persisted, store.published, gateway.completeCalls, store.publishedText)
	}
	if gateway.lastFence.ModelCallID != claim.ModelCallID || gateway.lastFence.LeaseOwner != claim.LeaseOwner {
		t.Fatalf("Gateway dispatch fence changed claim identity: %+v", gateway.lastFence)
	}
}

func TestTaskTerminalDeliveryWorkerLogsSafeSourceFactsFailureCode(t *testing.T) {
	store, _ := taskTerminalDeliveryTestStore(t, app.TaskTerminalDeliveryStateClaimed)
	const privateDetail = "private source detail must not be logged"
	store.readFactsErr = domain.NewError(domain.CodeInvalidState, privateDetail)
	core, logs := observer.New(zap.ErrorLevel)
	worker := newTaskTerminalDeliveryTestWorker(t, store, &taskTerminalDeliveryGatewayStub{modelID: "p1234567"})
	worker.options.Logger = zap.New(core)

	worked, err := worker.RunOnce(context.Background())
	entries := logs.FilterMessage("task terminal delivery processing failed").All()
	if !worked || err == nil || len(entries) != 1 {
		t.Fatalf("source facts failure logging: worked=%t err=%v entries=%d", worked, err, len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["stage"] != "read_source_facts" || fields["failure_code"] != string(domain.CodeInvalidState) ||
		fields["error_type"] != "*domain.Error" || strings.Contains(fmt.Sprint(fields), privateDetail) {
		t.Fatalf("source facts failure log fields are not safely classified: %#v", fields)
	}
}

func TestTaskTerminalDeliveryWorkerNotDispatchedUsesSystemStateReply(t *testing.T) {
	store, _ := taskTerminalDeliveryTestStore(t, app.TaskTerminalDeliveryStateClaimed)
	gateway := &taskTerminalDeliveryGatewayStub{modelID: "p1234567", completeStatus: app.TaskTerminalDeliveryModelNotDispatched,
		completeErr: errors.New("model budget unavailable")}
	worker := newTaskTerminalDeliveryTestWorker(t, store, gateway)
	found, err := worker.RunOnce(context.Background())
	if !found || err != nil || store.persisted != 1 || store.published != 1 ||
		!strings.HasPrefix(store.publishedText, "这次没有拿到已核实的结果，任务已中断。") ||
		!strings.Contains(store.publishedText, app.TaskTerminalDeliveryFailureExplanation("provider_unavailable")) ||
		strings.Contains(store.publishedText, "provider_unavailable") ||
		!strings.Contains(store.publishedText, "任务详情") ||
		strings.Contains(store.publishedText, "已完成 2 步") || strings.Contains(store.publishedText, "对账") ||
		strings.Contains(store.publishedText, "再试一次") || strings.Contains(store.publishedText, "正在查") {
		t.Fatalf("definite pre-dispatch fallback: found=%t err=%v persisted=%d published=%d text=%q",
			found, err, store.persisted, store.published, store.publishedText)
	}
	if gateway.recoverCalls != 1 || gateway.completeCalls != 1 {
		t.Fatalf("unexpected call path: recoveries=%d completes=%d", gateway.recoverCalls, gateway.completeCalls)
	}
	var request map[string]any
	if err := json.Unmarshal(gateway.lastRequest, &request); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(request)
	for _, internal := range []string{"u1234567", "c1234567", "q1234567", "source_sha256", "facts_sha256", "objective_sha256", "failure_code", "provider_unavailable"} {
		if bytes.Contains(encoded, []byte(internal)) {
			t.Fatalf("internal delivery identity leaked into model request: %s", encoded)
		}
	}
}

func TestTaskTerminalDeliveryWorkerRecoversReadyModelReplyWithoutRedispatch(t *testing.T) {
	store, _ := taskTerminalDeliveryTestStore(t, app.TaskTerminalDeliveryStateReplyReady)
	response := app.ModelResponse{Text: "The task stopped because its provider was unavailable.", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 10, OutputTokens: 8}}
	gateway := &taskTerminalDeliveryGatewayStub{modelID: "p1234567"}
	gateway.recoveries = append(gateway.recoveries, struct {
		response app.ModelResponse
		found    bool
		err      error
	}{response: response, found: true})
	worker := newTaskTerminalDeliveryTestWorker(t, store, gateway)
	found, err := worker.RunOnce(context.Background())
	if !found || err != nil || store.published != 1 || store.persisted != 0 || gateway.completeCalls != 0 ||
		store.publishedText != response.Text || gateway.lastFence.LeaseOwner != "" {
		t.Fatalf("reply-ready response recovery: found=%t err=%v published=%d persisted=%d completes=%d content=%q fence=%+v",
			found, err, store.published, store.persisted, gateway.completeCalls, store.publishedText, gateway.lastFence)
	}
}

func TestBuildTaskTerminalDeliveryModelRequestVerifiesDigestAndOmitsInternalIdentity(t *testing.T) {
	store, claim := taskTerminalDeliveryTestStore(t, app.TaskTerminalDeliveryStateClaimed)
	raw, err := app.BuildTaskTerminalDeliveryDSHModelRequest(store.facts.ModelID, store.facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, internal := range []string{claim.TaskID, claim.OwnerID, claim.ConversationID, "source_sha256", "facts_sha256", "objective_sha256"} {
		if bytes.Contains(raw, []byte(internal)) {
			t.Fatalf("internal identity in model request: %s", raw)
		}
	}
	for _, fact := range []string{
		app.TaskTerminalDeliverySafeNextStep(app.TaskTerminalDeliverySourceFailure),
		app.TaskTerminalDeliveryFailureExplanation("provider_unavailable"), `\"failure_explanation\"`,
		"never expose message IDs, source hashes, checkpoint markers, internal tool names, or internal status/error codes",
		"Do not list step counts or treat operational progress steps as evidence of task results",
		"do not suggest repeating a potentially side-effecting action"} {
		if !bytes.Contains(raw, []byte(fact)) {
			t.Fatalf("model request omitted progress or safe-delivery rule %q: %s", fact, raw)
		}
	}
	if bytes.Contains(raw, []byte("provider_unavailable")) || bytes.Contains(raw, []byte("failure_code")) ||
		bytes.Contains(raw, []byte("step_counts")) || bytes.Contains(raw, []byte("succeeded")) {
		t.Fatalf("model request exposed an internal failure code instead of a user-facing fact: %s", raw)
	}
	tampered := store.facts
	tampered.FailureCode = "untrusted_code"
	if _, err := app.BuildTaskTerminalDeliveryDSHModelRequest(tampered.ModelID, tampered); err == nil {
		t.Fatal("tampered model facts passed digest verification")
	}
}

func TestTaskTerminalDeliveryWaitingReasonUsesSafeCategories(t *testing.T) {
	for _, test := range []struct {
		reason string
		want   string
	}{
		{reason: "tool_result_unknown", want: "unknown_outcome"},
		{reason: "mcp_result_unknown", want: "unknown_outcome"},
		{reason: "dsh_recovery_unresolved", want: "reconciliation_required"},
		{reason: "model_retry_ladder_exhausted", want: "external_dependency"},
		{reason: "model_context_limit", want: "external_dependency"},
		{reason: "tool_call_budget", want: "external_dependency"},
		{reason: "unexpected_private_reason", want: "reconciliation_required"},
	} {
		safeReason := app.SafeTaskTerminalDeliveryWaitingReason(test.reason)
		if safeReason != test.want {
			t.Fatalf("waiting reason %q mapped to %q, want %q", test.reason, safeReason, test.want)
		}
		fallback := terminalDeliverySystemStateReply(app.TaskTerminalDeliverySourceFacts{
			Identity:      app.TaskTerminalDeliveryIdentity{SourceKind: app.TaskTerminalDeliverySourceWaiting},
			WaitingReason: safeReason,
		})
		if !strings.Contains(fallback, app.TaskTerminalDeliveryWaitingExplanation(safeReason)) ||
			strings.Contains(fallback, test.reason) || strings.Contains(fallback, test.want) {
			t.Fatalf("waiting fallback exposed a code or omitted the user-facing explanation: %q", fallback)
		}
	}
	diagnosis := "模型服务暂时不可用。"
	withDiagnosis := terminalDeliverySystemStateReply(app.TaskTerminalDeliverySourceFacts{
		Identity:      app.TaskTerminalDeliveryIdentity{SourceKind: app.TaskTerminalDeliverySourceWaiting},
		WaitingReason: "external_dependency", RecoveryDiagnosis: diagnosis,
	})
	if !strings.Contains(withDiagnosis, diagnosis) || strings.Contains(withDiagnosis, "当前没有可确认的后续操作") {
		t.Fatalf("waiting fallback ignored the safe recovery diagnosis: %q", withDiagnosis)
	}
}

func TestTerminalDeliverySystemStateReplyDoesNotInventNewsResultsOrRetryActions(t *testing.T) {
	failure := terminalDeliverySystemStateReply(app.TaskTerminalDeliverySourceFacts{
		Identity:   app.TaskTerminalDeliveryIdentity{SourceKind: app.TaskTerminalDeliverySourceFailure},
		TaskStatus: "failed", FailureCode: "unknown",
		StepCounts:       app.TaskTerminalDeliveryStepCounts{Total: 1, Succeeded: 1},
		ObjectiveSummary: "你看看今天有什么新闻",
	})
	if !strings.Contains(failure, "这次没有拿到已核实的结果，任务已中断") ||
		!strings.Contains(failure, "原因暂未查明") || strings.Contains(failure, "新闻") ||
		strings.Contains(failure, "已完成 1 步") || strings.Contains(failure, "再试一次") || strings.Contains(failure, "对账") {
		t.Fatalf("unknown failure claimed progress, result, or retry: %q", failure)
	}
	waiting := terminalDeliverySystemStateReply(app.TaskTerminalDeliverySourceFacts{
		Identity:   app.TaskTerminalDeliveryIdentity{SourceKind: app.TaskTerminalDeliverySourceWaiting},
		TaskStatus: "waiting_user", WaitingReason: "reconciliation_required",
		StepCounts: app.TaskTerminalDeliveryStepCounts{Total: 1, Succeeded: 1},
	})
	if !strings.Contains(waiting, "这次任务没有形成可核实的答复") || !strings.Contains(waiting, "现有记录无法确定具体的中断原因") ||
		strings.Contains(waiting, "结果待核对") || strings.Contains(waiting, "状态待确认") ||
		strings.Contains(waiting, "对账") || strings.Contains(waiting, "再试一次") || strings.Contains(waiting, "正在查") {
		t.Fatalf("unknown waiting result offered an unsupported action: %q", waiting)
	}
}

func TestTaskTerminalDeliveryFailureReasonReturnsToConversationInPlainLanguage(t *testing.T) {
	for _, test := range []struct {
		code        string
		explanation string
	}{
		{code: "provider_unavailable", explanation: "模型服务当前不可用"},
		{code: "provider_timeout", explanation: "模型服务未能在时限内回复"},
		{code: "provider_rate_limited", explanation: "模型服务暂时限制了本次请求"},
		{code: "model_unavailable", explanation: "当前配置的模型不可用"},
		{code: "model_output_invalid", explanation: "模型返回内容未通过格式校验"},
		{code: "model_budget_exhausted", explanation: "模型调用预算已用尽"},
		{code: "tool_failed", explanation: "某项操作失败"},
		{code: "task_execution_failed", explanation: "任务执行未能完成"},
		{code: "unexpected_private_failure", explanation: "无法确认具体原因"},
	} {
		fallback := terminalDeliverySystemStateReply(app.TaskTerminalDeliverySourceFacts{
			Identity:    app.TaskTerminalDeliveryIdentity{SourceKind: app.TaskTerminalDeliverySourceFailure},
			FailureCode: test.code,
		})
		if !strings.Contains(fallback, test.explanation) || strings.Contains(fallback, test.code) || strings.Contains(fallback, "原因代码") {
			t.Fatalf("failure reason %q was not safely explained to the user: %q", test.code, fallback)
		}
	}
}

func taskTerminalDeliveryTestStore(t *testing.T, state string) (*taskTerminalDeliveryStoreStub, app.TaskTerminalDeliveryClaim) {
	t.Helper()
	sourceDigest := bytes.Repeat([]byte{0x55}, 32)
	claim := app.TaskTerminalDeliveryClaim{TaskTerminalDeliveryIdentity: app.TaskTerminalDeliveryIdentity{
		DeliveryID: 1, TaskID: "q1234567", OwnerID: "u1234567", ConversationID: "c1234567", InitiatorID: "u1234567",
		TaskRevision: 2, AuthorizationVersion: 1, SourceKind: app.TaskTerminalDeliverySourceFailure,
		SourceEventSequence: 4, SourceEventType: "task.result", SourceSHA256: sourceDigest,
	}, State: state, ClaimGeneration: 1, ModelCallID: "terminal_delivery_1"}
	if state == app.TaskTerminalDeliveryStateClaimed {
		claim.LeaseOwner = "terminal-worker"
		claim.LeaseExpiresAt = time.Now().Add(time.Minute)
	} else {
		claim.ResponseKind = app.TaskTerminalDeliveryResponseModel
		claim.ResponseCiphertext = []byte("synthetic encrypted gateway response")
		digest := sha256.Sum256(claim.ResponseCiphertext)
		claim.ResponseSHA256 = digest[:]
	}
	if !app.ValidTaskTerminalDeliveryClaim(claim) {
		t.Fatal("test terminal delivery claim is invalid")
	}
	objectiveDigest := sha256.Sum256([]byte("Run the requested report."))
	facts := app.TaskTerminalDeliverySourceFacts{Identity: claim.TaskTerminalDeliveryIdentity,
		TaskStatus: "failed", ModelID: "p1234567", ObjectiveSummary: "Run the requested report.",
		StepCounts:      app.TaskTerminalDeliveryStepCounts{Total: 4, Succeeded: 2, Failed: 1, Unconfirmed: 1},
		ObjectiveSHA256: objectiveDigest[:], FailureCode: "provider_unavailable"}
	factsDigest, err := app.TaskTerminalDeliveryFactsDigest(facts)
	if err != nil {
		t.Fatal(err)
	}
	facts.FactsSHA256 = factsDigest
	store := &taskTerminalDeliveryStoreStub{facts: facts}
	if state == app.TaskTerminalDeliveryStateClaimed {
		store.claimed = []app.TaskTerminalDeliveryClaim{claim}
	} else {
		store.ready = []app.TaskTerminalDeliveryClaim{claim}
	}
	return store, claim
}

func newTaskTerminalDeliveryTestWorker(t *testing.T, store *taskTerminalDeliveryStoreStub,
	gateway *taskTerminalDeliveryGatewayStub) *TaskTerminalDeliveryWorker {
	t.Helper()
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewTaskTerminalDeliveryWorker(TaskTerminalDeliveryWorkerOptions{Store: store, Models: gateway,
		ReplySealer: sealer, WorkerID: "terminal-worker", LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func mustWorkerMessageID() string {
	messageID, err := domain.NewMessageID()
	if err != nil {
		panic(err)
	}
	return messageID
}
