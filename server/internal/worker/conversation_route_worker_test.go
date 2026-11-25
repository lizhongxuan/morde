package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type routeWorkerStub struct {
	claim                app.ConversationRouteClaim
	snapshot             app.ConversationRouteSnapshot
	decision             app.ConversationRouteDecision
	contextWindow        int
	capacityKnown        bool
	capacityErr          error
	modelErr             error
	classifyErr          error
	applyErr             error
	checkModelCtx        bool
	applied              []app.ConversationRouteDecision
	failures             []string
	deferred             int
	resolves             int
	classifies           int
	requeued             bool
	requeueCalls         int
	contextRequeued      bool
	contextRequeueCalls  int
	rateLimitHandled     bool
	rateLimitRetryDelay  time.Duration
	rateLimitContextErr  error
	rateLimitHandleCalls int
	cancelAfterClassify  context.CancelFunc
	repairRequeues       int
	repairStatusErr      error
	repairRequeueErr     error
	classified           app.ConversationRouteContext
}

type routeAggregationWorkerStub struct {
	*routeWorkerStub
	beginCalls int
	started    bool
	beginErr   error
}

func (s *routeAggregationWorkerStub) BeginConversationRouteAggregation(context.Context, app.ConversationRouteClaim,
	app.ConversationRouteSnapshot, app.ConversationRouteDecision) (bool, error) {
	s.beginCalls++
	return s.started, s.beginErr
}

func (s *routeWorkerStub) ClaimConversationRoute(context.Context, string, time.Duration) (app.ConversationRouteClaim, bool, error) {
	return s.claim, true, nil
}
func (*routeWorkerStub) ReleaseConversationRoute(context.Context, app.ConversationRouteClaim) error {
	return nil
}
func (s *routeWorkerStub) LoadConversationRouteSnapshot(context.Context, app.ConversationRouteClaim) (app.ConversationRouteSnapshot, error) {
	return s.snapshot, nil
}
func (s *routeWorkerStub) ApplyConversationRoute(_ context.Context, _ app.ConversationRouteClaim, snapshot app.ConversationRouteSnapshot, decision app.ConversationRouteDecision) error {
	if s.applyErr != nil {
		return s.applyErr
	}
	s.snapshot = snapshot
	s.applied = append(s.applied, decision)
	return nil
}
func (s *routeWorkerStub) FailConversationRoute(_ context.Context, _ app.ConversationRouteClaim, reason string) error {
	s.failures = append(s.failures, reason)
	return nil
}
func (s *routeWorkerStub) DeferConversationRoute(context.Context, app.ConversationRouteClaim, time.Duration) error {
	s.deferred++
	return nil
}
func (*routeWorkerStub) ApplyNextConversationTaskRevision(context.Context) (bool, error) {
	return false, nil
}
func (s *routeWorkerStub) RequeueConversationRouteAfterPredecessor(context.Context, app.ConversationRouteClaim, app.ConversationRouteSnapshot) (bool, error) {
	s.requeueCalls++
	return s.requeued, nil
}
func (s *routeWorkerStub) RequeueConversationRouteAfterContextChange(context.Context, app.ConversationRouteClaim) (bool, error) {
	s.contextRequeueCalls++
	return s.contextRequeued, nil
}
func (s *routeWorkerStub) HandleConversationRouteRateLimit(ctx context.Context, _ app.ConversationRouteClaim, delay time.Duration) (bool, error) {
	s.rateLimitHandleCalls++
	s.rateLimitRetryDelay = delay
	s.rateLimitContextErr = ctx.Err()
	return s.rateLimitHandled, nil
}
func (s *routeWorkerStub) RequeueConversationRouteAfterInvalidOutput(_ context.Context, _ app.ConversationRouteClaim) (bool, error) {
	if s.repairRequeueErr != nil {
		return false, s.repairRequeueErr
	}
	if s.decision.InvalidOutput && s.repairRequeues == 0 {
		s.repairRequeues++
		s.claim.RouteAttempt++
		return true, nil
	}
	return false, nil
}
func (s *routeWorkerStub) IsConversationRouteRepairAttempt(context.Context, app.ConversationRouteClaim) (bool, error) {
	if s.repairStatusErr != nil {
		return false, s.repairStatusErr
	}
	return s.repairRequeues > 0, nil
}
func (s *routeWorkerStub) ResolveConversationRouteModel(ctx context.Context, _ app.ConversationRouteClaim) (string, error) {
	s.resolves++
	if s.checkModelCtx && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "p1234567", s.modelErr
}
func (s *routeWorkerStub) ResolveConversationRouteContextWindow(context.Context, app.ConversationRouteClaim, string) (int, bool, error) {
	if s.contextWindow < 4096 {
		return 4096, false, s.capacityErr
	}
	return s.contextWindow, s.capacityKnown, s.capacityErr
}
func (s *routeWorkerStub) ClassifyConversationRoute(_ context.Context, _ app.ConversationRouteClaim, value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	s.classifies++
	s.classified = value
	if s.cancelAfterClassify != nil {
		s.cancelAfterClassify()
	}
	return s.decision, s.classifyErr
}

func newRouteWorkerTest(t *testing.T) (*ConversationRouteWorker, *routeWorkerStub) {
	t.Helper()
	stub := &routeWorkerStub{claim: app.ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", CreatedAt: time.Now()},
		LeaseExpiresAt: time.Now().Add(time.Minute)}, contextWindow: 32_768, capacityKnown: true,
		snapshot: app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "hello"}}}
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: stub, Actions: stub, Models: stub,
		WorkerID: "route-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 2, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	return worker, stub
}

func TestConversationRouteFailureLogIncludesSafeErrorCode(t *testing.T) {
	stub := &routeWorkerStub{}
	core, logs := observer.New(zap.WarnLevel)
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: stub, Actions: stub, Models: stub,
		WorkerID: "route-log-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.failRoute(context.Background(), app.ConversationRouteClaim{}, "model_unavailable",
		domain.NewError(domain.CodeUnauthenticated, "sensitive detail")); err != nil {
		t.Fatal(err)
	}
	entries := logs.All()
	if len(entries) != 1 || entries[0].ContextMap()["failure_code"] != string(domain.CodeUnauthenticated) {
		t.Fatalf("failure log = %+v", entries)
	}
	if _, ok := entries[0].ContextMap()["error"]; ok {
		t.Fatalf("failure log exposed raw error: %+v", entries[0].ContextMap())
	}
}

func TestConversationRouteInvalidOutputLogContainsMetadataOnly(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	core, logs := observer.New(zap.WarnLevel)
	worker.options.Logger = zap.New(core)
	secret := "synthetic-private-model-output"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
		Question: "请求暂时无法安全分类，请稍后重试。", InvalidOutput: true,
		InvalidOutputLengthBytes: len(secret), InvalidOutputUTF8Valid: true}
	stub.repairRequeues = 1
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 {
		t.Fatalf("invalid output did not use the safe fallback: applied=%+v err=%v", stub.applied, err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("invalid output log entries = %+v", entries)
	}
	fields := entries[0].ContextMap()
	if fmt.Sprint(fields["invalid_output_length_bytes"]) != fmt.Sprint(len(secret)) || fields["invalid_output_utf8_valid"] != true ||
		fields["failure_code"] != "invalid_model_output" || strings.Contains(fmt.Sprint(fields), secret) {
		t.Fatalf("invalid output log leaked text or omitted safe metadata: %+v", fields)
	}
}

func TestConversationRouteWorkerPreservesOnlyStructuredFileIntentForTaskActions(t *testing.T) {
	newDecision := safeConversationRouteDecision(app.ConversationRouteContext{ActorID: "u1234567", Content: "请生成 PDF 文件"},
		app.ConversationRouteDecision{Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryRequired})
	if newDecision.Action != app.ConversationRouteNew || newDecision.FileDeliveryIntent != task.FileDeliveryRequired {
		t.Fatalf("new task file intent was changed: %+v", newDecision)
	}

	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "更新报告",
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3}}}
	update := safeConversationRouteDecision(value, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: "q1234567", ExpectedRevision: 3, FileDeliveryIntent: task.FileDeliveryNone})
	if update.Action != app.ConversationRouteUpdate || update.FileDeliveryIntent != task.FileDeliveryNone || update.ExpectedRevision != 3 {
		t.Fatalf("authorized revision file intent was changed: %+v", update)
	}

	for _, invalid := range []app.ConversationRouteDecision{
		{Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryIntent("maybe")},
		{Action: app.ConversationRouteReply, Reply: "Done.", FileDeliveryIntent: task.FileDeliveryRequired},
	} {
		decision := safeConversationRouteDecision(value, invalid)
		if decision.Action != app.ConversationRouteClarify || decision.FileDeliveryIntent != "" {
			t.Errorf("unsafe file intent reached route application: got=%+v source=%+v", decision, invalid)
		}
	}
}

func TestConversationRouteBudgetFailureLogIncludesStageAndCapacity(t *testing.T) {
	stub := &routeWorkerStub{}
	core, logs := observer.New(zap.WarnLevel)
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: stub, Actions: stub, Models: stub,
		WorkerID: "route-budget-log-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.New(core)})
	if err != nil {
		t.Fatal(err)
	}
	budgetErr := domain.NewError(domain.CodeResultTooLarge, "sensitive request detail")
	budgetErr.Details["stage"] = "worker_context_fit"
	budgetErr.Details["context_window_tokens"] = 4096
	budgetErr.Details["context_window_known"] = false
	if err := worker.failRoute(context.Background(), app.ConversationRouteClaim{}, "message_too_long", budgetErr); err != nil {
		t.Fatal(err)
	}
	fields := logs.All()[0].ContextMap()
	if fields["failure_stage"] != "worker_context_fit" || fmt.Sprint(fields["context_window_tokens"]) != "4096" ||
		fields["context_window_known"] != false {
		t.Fatalf("budget failure log omitted safe diagnostics: %+v", fields)
	}
	if _, ok := fields["error"]; ok {
		t.Fatalf("budget failure log exposed raw error: %+v", fields)
	}
}

func TestConversationRouteWorkerRoutesFirstMessageAfterModelClassification(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	found, err := worker.RunOnce(context.Background())
	if err != nil || !found || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteNew ||
		stub.snapshot.Context.ModelID != "p1234567" || stub.classifies != 1 {
		t.Fatalf("first route = %+v, model=%q classifications=%d found=%t err=%v", stub.applied, stub.snapshot.Context.ModelID, stub.classifies, found, err)
	}
}

func TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.claim.Message.Sequence = 2
	stub.snapshot.PreviousMessageID = "m2345678"
	stub.snapshot.PreviousSequence = 1
	stub.snapshot.PreviousRoutingState = "processing"
	stub.snapshot.Context.PreviousUnresolved = true
	stub.snapshot.Context.Content = "你好"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "你好。",
		ModelOutput: `{"action":"reply","reply":"你好。"}`}

	if found, err := worker.RunOnce(context.Background()); err != nil || !found {
		t.Fatalf("follow-up was not processed while its predecessor was unresolved: found=%t err=%v", found, err)
	}
	if stub.deferred != 0 || stub.resolves != 1 || stub.classifies != 1 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteReply || !stub.classified.PreviousUnresolved {
		t.Fatalf("independent reply was blocked by an unresolved predecessor: deferred=%d resolves=%d classifies=%d classified=%+v applied=%+v",
			stub.deferred, stub.resolves, stub.classifies, stub.classified, stub.applied)
	}

	stub.applied = nil
	stub.snapshot.Context.Content = "另外帮我查一下明天的天气"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew,
		ModelOutput: `{"action":"new","task_id":"","question":""}`}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.deferred != 0 ||
		stub.classifies != 2 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteNew {
		t.Fatalf("independent Task was blocked by an unresolved predecessor: deferred=%d classifies=%d applied=%+v found=%t err=%v",
			stub.deferred, stub.classifies, stub.applied, found, err)
	}

	stub.applied = nil
	stub.snapshot.PreviousSequence = 3
	stub.snapshot.PreviousRoutingState = "processing"
	stub.snapshot.Context.PreviousUnresolved = true
	stub.snapshot.Context.Content = "你好"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "你好。"}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.deferred != 0 ||
		stub.classifies != 3 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteReply ||
		!stub.classified.PreviousUnresolved {
		t.Fatalf("independent message was blocked when its predecessor arrived later: deferred=%d classifies=%d applied=%+v found=%t err=%v",
			stub.deferred, stub.classifies, stub.applied, found, err)
	}
}

func TestConversationRouteMetricsRecordAggregationWaitAndCorrection(t *testing.T) {
	registry := prometheus.NewRegistry()
	stub := &routeWorkerStub{
		claim: app.ConversationRouteClaim{
			Message: appRouteMessageForMetrics(), LeaseExpiresAt: time.Now().Add(time.Minute),
			AggregationStartedAt: time.Now().Add(-3 * time.Second),
		},
		snapshot: app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{
			ActorID: "u1234567", Content: "补充任务要求", ReferenceTaskID: "q1234567",
			Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3}},
		}},
		decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate, TaskID: "q1234567", ExpectedRevision: 3,
			ModelOutput: `{"action":"update","task_id":"q1234567","question":"","reply":""}`},
	}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: stub, Actions: stub, Models: stub, WorkerID: "route-metrics-test", LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(), Metrics: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if found, err := routeWorker.RunOnce(context.Background()); err != nil || !found || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteUpdate {
		t.Fatalf("route update = %#v, found=%t, err=%v", stub.applied, found, err)
	}
	if got := gatheredWorkerCounter(t, routeWorker.metrics.actions.WithLabelValues("update", "applied")); got != 1 {
		t.Fatalf("applied correction metric = %v, want 1", got)
	}
	if got := gatheredWorkerCounter(t, routeWorker.metrics.aggregationStates.WithLabelValues("corrected")); got != 1 {
		t.Fatalf("aggregation correction metric = %v, want 1", got)
	}
	for _, family := range mustGatherWorkerMetrics(t, registry) {
		if family.GetName() == "cloudhost_conversation_aggregation_wait_seconds" {
			histogram := family.GetMetric()[0].GetHistogram()
			if histogram.GetSampleCount() != 1 || histogram.GetSampleSum() < 2.9 {
				t.Fatalf("aggregation wait metric = %#v, want one 3s sample", histogram)
			}
			return
		}
	}
	t.Fatal("aggregation wait histogram was not gathered")
}

func TestConversationRouteMetricsRecordAggregationWaitState(t *testing.T) {
	registry := prometheus.NewRegistry()
	base := &routeWorkerStub{
		claim:    app.ConversationRouteClaim{Message: appRouteMessageForMetrics(), LeaseExpiresAt: time.Now().Add(time.Minute)},
		snapshot: app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "create a task"}},
		decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew},
	}
	stub := &routeAggregationWorkerStub{routeWorkerStub: base, started: true}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: stub, Actions: stub, Models: stub, WorkerID: "route-aggregation-metrics-test", LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(), Metrics: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if found, err := routeWorker.RunOnce(context.Background()); err != nil || !found || stub.beginCalls != 1 {
		t.Fatalf("aggregation start = found:%t begin:%d err:%v", found, stub.beginCalls, err)
	}
	if got := gatheredWorkerCounter(t, routeWorker.metrics.aggregationStates.WithLabelValues("waiting")); got != 1 {
		t.Fatalf("aggregation waiting metric = %v, want 1", got)
	}
}

func mustGatherWorkerMetrics(t *testing.T, registry prometheus.Gatherer) []*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return families
}

func gatheredWorkerCounter(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}

func appRouteMessageForMetrics() conversation.Message {
	return conversation.Message{ID: "m1234567", CreatedAt: time.Now().UTC()}
}

func TestConversationRouteWorkerWaitsOnlyAfterClassifiedNewAndNeverSlidesTheWindow(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		decision  app.ConversationRouteDecision
		modelErr  error
		wantWait  bool
		wantApply bool
	}{
		{name: "new", decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew}, wantWait: true},
		{name: "reply", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "Not a task."}, wantApply: true},
		{name: "status", decision: app.ConversationRouteDecision{Action: app.ConversationRouteStatus, TaskID: "q1234567", ExpectedRevision: 1}, wantApply: true},
		{name: "stop pending root", decision: app.ConversationRouteDecision{Action: app.ConversationRouteStop,
			TargetMessageID: "m2345678"}, wantApply: true},
		// An unknown model call no longer fails the route: the heuristic
		// fallback asks the user instead, so a clarification is applied
		// but aggregation is never started.
		{name: "unknown model call", modelErr: errors.New("route result or usage is unknown"), wantApply: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, stub := newRouteWorkerTest(t)
			stub.decision, stub.classifyErr = scenario.decision, scenario.modelErr
			if scenario.name == "status" {
				stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567",
					Status: task.StatusRunning, Revision: 1}}
			}
			if scenario.name == "stop pending root" {
				executionScope, err := app.ConversationRouteExecutionScopeDigest(app.ConversationRoutingInput{})
				if err != nil {
					t.Fatal(err)
				}
				controlScope, err := app.ConversationRouteControlScopeDigest(app.ConversationRoutingInput{})
				if err != nil {
					t.Fatal(err)
				}
				stub.snapshot.Context.ExecutionScopeDigest = executionScope
				stub.snapshot.Context.ControlScopeDigest = controlScope
				stub.snapshot.Context.RequestCandidates = []app.ConversationRouteRequestCandidate{{MessageID: "m2345678",
					Sequence: 1, Content: "root", RoutingStatus: conversation.RoutingProcessing,
					SameExecutionScope: true, SameControlScope: true, SameAuthorization: true,
					ExecutionScopeDigest: executionScope, ControlScopeDigest: controlScope, AggregationWaiting: true}}
			}
			actions := &routeAggregationWorkerStub{routeWorkerStub: stub, started: true}
			worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: stub, Actions: actions, Models: stub,
				WorkerID: "route-aggregation-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond,
				MaxConcurrent: 1, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = worker.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (actions.beginCalls == 1) != scenario.wantWait || (len(stub.applied) != 0) != scenario.wantApply {
				t.Fatalf("aggregation start status = calls:%d applied:%+v failures:%v, wantWait=%t",
					actions.beginCalls, stub.applied, stub.failures, scenario.wantWait)
			}
		})
	}
}

func TestConversationRouteWorkerAppliesSafeDirectReply(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	modelOutput := `{"action":"reply","task_id":"","question":"","reply":"你好。"}`
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "你好。", ModelOutput: modelOutput}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 {
		t.Fatalf("direct reply was not applied: decisions=%+v err=%v", stub.applied, err)
	}
	if got := stub.applied[0]; got.Action != app.ConversationRouteReply || got.TaskID != "" || got.ExpectedRevision != 0 ||
		got.Question != "" || got.Reply != "你好。" || got.ModelOutput != modelOutput {
		t.Fatalf("direct reply changed during worker validation: %+v", got)
	}
}

func TestConversationRouteWorkerRejectsUnsafeDirectReply(t *testing.T) {
	for _, test := range []struct {
		name     string
		context  app.ConversationRouteContext
		decision app.ConversationRouteDecision
		question string
	}{
		{name: "task target", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, TaskID: "q1234567", Reply: "answer"}, question: "我还不能安全确认这项操作的目标或范围，因此尚未执行。请补充具体目标，或说明要处理哪项任务。"},
		{name: "empty text", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply}, question: "我还不能安全确认这项操作的目标或范围，因此尚未执行。请补充具体目标，或说明要处理哪项任务。"},
		{name: "model output too long", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "answer", ModelOutput: strings.Repeat("x", app.MaxConversationRouteModelOutputBytes+1)}, question: "我还不能安全确认这项操作的目标或范围，因此尚未执行。请补充具体目标，或说明要处理哪项任务。"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := safeConversationRouteDecision(test.context, test.decision)
			if decision.Action != app.ConversationRouteClarify || decision.Question != test.question || decision.Reply != "" {
				t.Fatalf("unsafe direct reply survived validation: %+v", decision)
			}
		})
	}
}

func TestConversationRouteWorkerPreservesModelReplyForUnavailableTaskReference(t *testing.T) {
	decision := safeConversationRouteDecision(
		app.ConversationRouteContext{Content: "停止任务", ReferenceUnavailable: true},
		app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "我无法确认该任务，因此没有发送停止请求。"},
	)
	if decision.Action != app.ConversationRouteReply || decision.TaskID != "" || decision.Reply == "" {
		t.Fatalf("non-action model reply was replaced by keyword routing: %+v", decision)
	}
}

func TestConversationRouteWorkerRoutesWithinIncompleteCandidateSetSafely(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", CandidateSetIncomplete: true,
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3}}}
	for _, test := range []struct {
		name     string
		content  string
		decision app.ConversationRouteDecision
		want     app.ConversationRouteAction
	}{
		{name: "known task target", content: "继续这个任务", decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
			TaskID: "q1234567", ExpectedRevision: 3}, want: app.ConversationRouteUpdate},
		{name: "independent work", content: "分析一份新上传的无关日志", decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew}, want: app.ConversationRouteNew},
		{name: "standalone reply", content: "你好", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "你好。"}, want: app.ConversationRouteReply},
		{name: "unresolved control", content: "停止任务", decision: app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "请说明要停止哪个任务。"}, want: app.ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			value.Content = test.content
			got := safeConversationRouteDecision(value, test.decision)
			if got.Action != test.want {
				t.Fatalf("incomplete candidate decision = %+v, want %s", got, test.want)
			}
		})
	}
}

func TestConversationRouteWorkerFitsCandidatesWithinModelInputBudget(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	for index := 0; index < 40; index++ {
		stub.snapshot.Context.RequestCandidates = append(stub.snapshot.Context.RequestCandidates, app.ConversationRouteRequestCandidate{
			MessageID:     fmt.Sprintf("m%07d", index+1),
			Sequence:      int64(index + 1),
			Content:       strings.Repeat("x", 4096),
			RoutingStatus: conversation.RoutingRouted,
		})
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stub.classifies != 1 || len(stub.classified.RequestCandidates) >= len(stub.snapshot.Context.RequestCandidates) ||
		!stub.classified.CandidateSetIncomplete || len(stub.applied) != 1 {
		t.Fatalf("oversized candidate context did not shrink safely: classified=%d candidates=%d/%d incomplete=%t applied=%+v",
			stub.classifies, len(stub.classified.RequestCandidates), len(stub.snapshot.Context.RequestCandidates),
			stub.classified.CandidateSetIncomplete, stub.applied)
	}
}

func TestTrimConversationRouteCandidatesPreservesExplicitTargets(t *testing.T) {
	value := app.ConversationRouteContext{ReferenceTaskID: "q1234567", PreviousTaskID: "q1234567", PreviousMessageID: "m1234567",
		RequestCandidates: []app.ConversationRouteRequestCandidate{{MessageID: "m1234567"}, {MessageID: "m2345678"}},
		Candidates:        []app.ConversationRouteCandidate{{TaskID: "q1234567"}, {TaskID: "q2345678"}}}
	if !trimConversationRouteCandidates(&value) {
		t.Fatalf("candidate trimming failed: %+v", value)
	}
	if !trimConversationRouteCandidates(&value) {
		t.Fatalf("candidate trimming was not idempotent: %+v", value)
	}
	if !value.CandidateSetIncomplete || len(value.RequestCandidates) != 1 || value.RequestCandidates[0].MessageID != "m1234567" ||
		len(value.Candidates) != 1 || value.Candidates[0].TaskID != "q1234567" {
		t.Fatalf("candidate trimming lost an explicit target: %+v", value)
	}
}

func TestTrimConversationRouteCandidatesPreservesActiveTaskControlEvidence(t *testing.T) {
	value := app.ConversationRouteContext{
		ActorID: "u1234567", Content: "补充一项结果", CurrentRequestModelID: "p1234567", TimeZone: "Asia/Shanghai",
		PreviousMessageID: "m7654321", PreviousTaskID: "q7654321",
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567",
			Status: task.StatusRunning, Revision: 3}},
		RequestCandidates: []app.ConversationRouteRequestCandidate{
			{MessageID: "m1234567", Sequence: 1, TaskID: "q1234567", TaskStatus: task.StatusRunning,
				SameControlScope: true, SameAuthorization: true,
				RequestInput: app.ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"}},
			{MessageID: "m7654321", Sequence: 8, TaskID: "q7654321", TaskStatus: task.StatusCompleted},
			{MessageID: "m2345678", Sequence: 7, Content: "older unrelated request", RoutingStatus: conversation.RoutingRouted},
		},
	}
	if !trimConversationRouteCandidates(&value) || value.TaskCandidateSetIncomplete || !value.RequestCandidateSetIncomplete ||
		len(value.Candidates) != 1 || len(value.RequestCandidates) != 2 || value.RequestCandidates[0].TaskID != "q1234567" {
		t.Fatalf("trimming dropped active Task authorization evidence: %+v", value)
	}
	if !app.ValidConversationRouteSteerTarget("q1234567", value) {
		t.Fatalf("retained active Task evidence was not sufficient for a safe steer: %+v", value)
	}
}

func TestTrimConversationRouteCandidatesPreservesTaskWithActiveControlEvidence(t *testing.T) {
	value := app.ConversationRouteContext{
		ActorID: "u1234567", CurrentRequestModelID: "p1234567", TimeZone: "Asia/Shanghai",
		PreviousMessageID: "m7654321", PreviousTaskID: "q7654321",
		Candidates: []app.ConversationRouteCandidate{
			{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3},
			{TaskID: "q7654321", InitiatorID: "u1234567", Status: task.StatusCompleted, Revision: 2},
			{TaskID: "q2345678", InitiatorID: "u1234567", Status: task.StatusQueued, Revision: 1},
		},
		RequestCandidates: []app.ConversationRouteRequestCandidate{
			{MessageID: "m1234567", Sequence: 1, TaskID: "q1234567", TaskStatus: task.StatusRunning,
				SameControlScope: true, SameAuthorization: true,
				RequestInput: app.ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"}},
			{MessageID: "m7654321", Sequence: 8, TaskID: "q7654321", TaskStatus: task.StatusCompleted},
		},
	}
	if !trimConversationRouteCandidates(&value) || !value.TaskCandidateSetIncomplete || len(value.Candidates) != 2 ||
		value.Candidates[0].TaskID != "q1234567" || value.Candidates[1].TaskID != "q7654321" {
		t.Fatalf("unrelated task was not trimmed while preserving the active control target: %+v", value)
	}
	if trimConversationRouteCandidates(&value) || len(value.Candidates) != 2 || value.Candidates[0].TaskID != "q1234567" {
		t.Fatalf("active Task candidate was trimmed away from its authorization evidence: %+v", value)
	}
}

func TestConversationRouteWorkerClassifiesEveryFreeTextMessage(t *testing.T) {
	for _, test := range []struct {
		content  string
		decision app.ConversationRouteDecision
		want     app.ConversationRouteAction
	}{
		{"帮我整理资料", app.ConversationRouteDecision{Action: app.ConversationRouteNew}, app.ConversationRouteNew},
		{"停止任务", app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请明确停止范围。"}, app.ConversationRouteClarify},
		{"查看进度", app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请指出要查询的任务。"}, app.ConversationRouteClarify},
	} {
		t.Run(test.content, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Content = test.content
			stub.decision = test.decision
			if _, err := worker.RunOnce(context.Background()); err != nil || stub.resolves != 1 || stub.classifies != 1 ||
				len(stub.applied) != 1 || stub.applied[0].Action != test.want {
				t.Fatalf("free text bypassed semantic classification: content=%q resolutions=%d classifications=%d decisions=%+v err=%v",
					test.content, stub.resolves, stub.classifies, stub.applied, err)
			}
		})
	}
	t.Run("unavailable referenced task", func(t *testing.T) {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.Context.Content = "停止任务"
		stub.snapshot.Context.ReferenceTaskID = "q1234567"
		stub.snapshot.Context.ReferenceUnavailable = true
		stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: app.UnavailableRouteReferenceMessage}
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.resolves != 1 || stub.classifies != 1 ||
			len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify {
			t.Fatalf("referenced free text bypassed semantic classification: resolutions=%d classifications=%d decisions=%+v err=%v",
				stub.resolves, stub.classifies, stub.applied, err)
		}
	})
	t.Run("classifier unavailable", func(t *testing.T) {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.Context.Content = "帮我整理资料"
		stub.classifyErr = errors.New("synthetic model is unavailable")
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.classifies != 1 ||
			len(stub.failures) != 0 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify {
			t.Fatalf("classifier failure was not clarified: classifications=%d failures=%v decisions=%+v err=%v",
				stub.classifies, stub.failures, stub.applied, err)
		}
	})
}

func TestConversationRouteWorkerRetriesInvalidOutputOnceThenFallsBack(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请求暂时无法安全分类，请稍后重试。", InvalidOutput: true}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.repairRequeues != 1 ||
		len(stub.failures) != 0 || len(stub.applied) != 0 {
		t.Fatalf("invalid output was not requeued for one repair: found=%t err=%v requeues=%d failures=%v applied=%+v",
			found, err, stub.repairRequeues, stub.failures, stub.applied)
	}
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "你好。"}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || !stub.classified.Repair ||
		len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteReply {
		t.Fatalf("repair attempt did not use the bounded repair context: found=%t err=%v repair=%t applied=%+v",
			found, err, stub.classified.Repair, stub.applied)
	}
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请求暂时无法安全分类，请稍后重试。", InvalidOutput: true}
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.repairRequeues != 1 ||
		len(stub.failures) != 0 || len(stub.applied) != 2 || stub.applied[1].Action != app.ConversationRouteClarify {
		t.Fatalf("second invalid output did not fall back to clarification: found=%t err=%v requeues=%d failures=%v applied=%+v",
			found, err, stub.repairRequeues, stub.failures, stub.applied)
	}
}

func routeWorkerFallbackSteerContext(stub *routeWorkerStub) {
	scopeDigest := sha256.Sum256([]byte("fallback steer test scope"))
	stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{
		{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1},
	}
	stub.snapshot.Context.PreviousTaskID = "q1234567"
	stub.snapshot.Context.CurrentRequestModelID = "p1234567"
	stub.snapshot.Context.TimeZone = "Asia/Shanghai"
	stub.snapshot.Context.ControlScopeDigest = scopeDigest[:]
	stub.snapshot.Context.RequestCandidates = []app.ConversationRouteRequestCandidate{
		{MessageID: "m0000001", Sequence: 1, TaskID: "q1234567", TaskStatus: task.StatusRunning,
			RoutingStatus:    conversation.RoutingRouted,
			SameControlScope: true, SameAuthorization: true, ControlScopeDigest: scopeDigest[:],
			RequestInput: app.ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"}},
	}
}

func TestConversationRouteWorkerClarifiesContinuationOnClassifyFailure(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "补充一点：把标题改成红色"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("classify failure executed an unclassified continuation: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerClarifiesHighRiskChangeOnClassifyFailure(t *testing.T) {
	for _, content := range []string{
		"换成另一台电脑",
		"把工作目录换成 /workspace/new",
		"补充一下，把收件人改成 Alice",
		"调整一下任务计划和授权范围",
	} {
		t.Run(content, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			routeWorkerFallbackSteerContext(stub)
			stub.snapshot.Context.Content = content
			stub.classifyErr = errors.New("provider unavailable")
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
				stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" {
				t.Fatalf("high-risk change was applied as a low-risk supplement: content=%q failures=%v applied=%+v err=%v",
					content, stub.failures, stub.applied, err)
			}
		})
	}
}

func TestSafeConversationRouteDecisionPreservesModelSteerAndRequiresSafeTarget(t *testing.T) {
	for _, content := range []string{"换成另一台电脑", "把工作目录换成 /workspace/new", "把收件人改成 Alice", "调整授权范围"} {
		t.Run("model classifies "+content, func(t *testing.T) {
			_, stub := newRouteWorkerTest(t)
			routeWorkerFallbackSteerContext(stub)
			context := stub.snapshot.Context
			context.Content = content
			decision := safeConversationRouteDecision(context, app.ConversationRouteDecision{
				Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 1,
			})
			if decision.Action != app.ConversationRouteSteer || decision.TaskID != "q1234567" {
				t.Fatalf("message keywords rewrote the structured model action: %+v", decision)
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(*app.ConversationRouteContext)
	}{
		{name: "wrong owner", mutate: func(value *app.ConversationRouteContext) { value.Candidates[0].InitiatorID = "u7654321" }},
		{name: "inactive task", mutate: func(value *app.ConversationRouteContext) { value.Candidates[0].Status = task.StatusWaitingUser }},
		{name: "authorization mismatch", mutate: func(value *app.ConversationRouteContext) { value.RequestCandidates[0].SameAuthorization = false }},
		{name: "control scope mismatch", mutate: func(value *app.ConversationRouteContext) { value.RequestCandidates[0].SameControlScope = false }},
		{name: "stale revision", mutate: func(value *app.ConversationRouteContext) { value.Candidates[0].Revision++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &routeWorkerStub{}
			routeWorkerFallbackSteerContext(stub)
			context := stub.snapshot.Context
			context.Content = "继续这个任务，并调整授权范围。"
			test.mutate(&context)
			decision := safeConversationRouteDecision(context, app.ConversationRouteDecision{
				Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 1,
			})
			if decision.Action != app.ConversationRouteClarify || decision.TaskID != "" {
				t.Fatalf("unsafe or unverified Task target was accepted: %+v", decision)
			}
		})
	}
}

func TestSafeConversationRouteDecisionPreservesToolAndKeepsStructuralGuards(t *testing.T) {
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteTool, ToolName: "morde_web_search",
		ToolArguments: `{"query":"日本央行利率政策"}`}
	for _, content := range []string{
		"请搜索公开网站关于日本央行利率政策的最新信息。",
		"能否帮我找一下日本央行利率政策的最新公开资料？",
		"有没有关于日本央行利率政策的最新公开信息？",
	} {
		value := app.ConversationRouteContext{ActorID: "u1234567", Content: content}
		if got := safeConversationRouteDecision(value, decision); got.Action != app.ConversationRouteTool {
			t.Errorf("search wording changed the structured Tool action for %q: %+v", content, got)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*app.ConversationRouteContext)
	}{
		{name: "empty request", mutate: func(value *app.ConversationRouteContext) { value.Content = " " }},
		{name: "attachment", mutate: func(value *app.ConversationRouteContext) { value.CurrentMessageHasAttachments = true }},
		{name: "truncated message", mutate: func(value *app.ConversationRouteContext) { value.ContentTruncated = true }},
		{name: "unavailable reference", mutate: func(value *app.ConversationRouteContext) {
			value.ReferenceTaskID = "q1234567"
			value.ReferenceUnavailable = true
		}},
		{name: "unresolved predecessor", mutate: func(value *app.ConversationRouteContext) { value.PreviousUnresolved = true }},
		{name: "incomplete candidates", mutate: func(value *app.ConversationRouteContext) { value.CandidateSetIncomplete = true }},
		{name: "incomplete history", mutate: func(value *app.ConversationRouteContext) { value.HistoryIncomplete = true }},
		{name: "result review", mutate: func(value *app.ConversationRouteContext) { value.ResultReview = &app.ConversationTaskResultReview{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := app.ConversationRouteContext{ActorID: "u1234567", Content: "有没有关于日本央行利率政策的最新公开信息？"}
			test.mutate(&value)
			if got := safeConversationRouteDecision(value, decision); got.Action != app.ConversationRouteClarify || got.TaskID != "" {
				t.Fatalf("structurally ineligible Tool action was accepted: %+v", got)
			}
		})
	}
	if got := safeConversationRouteDecision(app.ConversationRouteContext{ActorID: "u1234567", Content: "query"},
		app.ConversationRouteDecision{Action: app.ConversationRouteTool, ToolName: "morde_personal_update", ToolArguments: `{"field":"name"}`}); got.Action != app.ConversationRouteClarify {
		t.Fatalf("non-allowlisted Tool action was accepted: %+v", got)
	}
}

func TestConversationRouteWorkerClarifiesExplicitNewSignalWhenClassificationFails(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "新开一个任务，帮我写个脚本"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("explicit new-task signal executed without a model decision: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerClarifiesIndependentWorkWhenClassificationFails(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "请帮我开发一个独立的验收工具"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("independent work executed without a model decision: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
	worker, stub = newRouteWorkerTest(t)
	stub.snapshot.Context.PreviousTaskID = "q1234567"
	stub.snapshot.Context.Content = "请开发一个验收工具并修改项目文件"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || !strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("independent work executed after classification failure: applied=%+v err=%v", stub.applied, err)
	}
}

func TestConversationRouteWorkerClarifiesDocumentDeliveryWhenClassificationFails(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Candidates = nil
	stub.snapshot.Context.Content = "请帮我写一个设计方案文档，并提供下载"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("document delivery executed after classification failure: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
	worker, stub = newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "请帮我写一个独立的设计方案文档，并提供下载"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || !strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("document delivery executed after classification failure: applied=%+v err=%v", stub.applied, err)
	}

	for _, value := range []app.ConversationRouteContext{
		{Content: "请帮我写一个设计方案文档，并提供下载", PreviousUnresolved: true},
		{Content: "请帮我写一个设计方案文档，并提供下载", ReferenceUnavailable: true},
		{Content: "请帮我写一个设计方案文档，并提供下载", ResultReview: &app.ConversationTaskResultReview{}},
		{Content: "不要生成文档，只用聊天文字说明"},
	} {
		if decision := conversationRouteClassifyFallback(value); decision.Action == app.ConversationRouteNew {
			t.Fatalf("document fallback bypassed a safety condition: context=%+v decision=%+v", value, decision)
		}
	}
}

func TestConversationRouteWorkerClarifiesExplicitSearchWhenClassificationFails(t *testing.T) {
	for _, test := range []struct {
		name               string
		previousUnresolved bool
		want               app.ConversationRouteAction
	}{
		{name: "no active task", want: app.ConversationRouteClarify},
		{name: "unresolved predecessor", previousUnresolved: true, want: app.ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			routeWorkerFallbackSteerContext(stub)
			stub.snapshot.Context.Candidates = nil
			stub.snapshot.Context.PreviousUnresolved = test.previousUnresolved
			stub.snapshot.Context.Content = "请帮我搜索 Rust 新版本的特性"
			stub.classifyErr = errors.New("provider unavailable")
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
				stub.applied[0].Action != test.want || stub.applied[0].TaskID != "" ||
				!strings.Contains(stub.applied[0].Question, "尚未执行") {
				t.Fatalf("search request executed without a model decision: applied=%+v want=%s failures=%v err=%v",
					stub.applied, test.want, stub.failures, err)
			}
		})
	}
	for _, content := range []string{"请帮我搜索 Rust 新版本的特性", "重新开始一个新任务"} {
		decision := conversationRouteClassifyFallback(app.ConversationRouteContext{
			Content: content, ResultReview: &app.ConversationTaskResultReview{},
		})
		if decision.Action == app.ConversationRouteNew {
			t.Fatalf("fallback for %q bypassed the task result review: %+v", content, decision)
		}
	}
}

func TestConversationRouteWorkerFallbackDoesNotExecuteUnclassifiedTaskControls(t *testing.T) {
	for _, test := range []struct {
		content string
	}{
		{content: "请继续当前任务并读取已授权 Workspace 中的结果"},
		{content: "现在请帮我继续当前任务并读取结果"},
		{content: "请接着这个任务"},
		{content: "Please continue this task"},
		{content: "请继续当前旁聊里唯一一条现有任务：开始一个隔离的步骤06正向验收 Task。状态为等待你处理；刚才同一 Task 的 file_write 已完成，现在只用它的 Workspace file_read 读取既有相对路径。"},
		{content: "请查看当前任务状态"},
		{content: "请停止当前任务"},
	} {
		t.Run(test.content, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			routeWorkerFallbackSteerContext(stub)
			stub.snapshot.Context.Content = test.content
			stub.classifyErr = errors.New("provider unavailable")
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
				stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
				stub.applied[0].ExpectedRevision != 0 || !strings.Contains(stub.applied[0].Question, "尚未执行") {
				t.Fatalf("unclassified task control was not clarified without execution: failures=%v applied=%+v err=%v",
					stub.failures, stub.applied, err)
			}
		})
	}
}

func TestConversationRouteWorkerUsesModelSemanticForStopAll(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "把还没有结束的工作全部撤掉"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteStopAll,
		ModelOutput: `{"action":"stop_all","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteStopAll || stub.applied[0].TaskID != "" {
		t.Fatalf("model-classified semantic stop-all was rejected: failures=%v applied=%+v err=%v", stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerUsesModelSemanticForSingleTaskStop(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "把这个工作结束掉"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: "q1234567", ExpectedRevision: 1,
		ModelOutput: `{"action":"stop","task_id":"q1234567"}`}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteStop || stub.applied[0].TaskID != "q1234567" ||
		stub.applied[0].ExpectedRevision != 1 {
		t.Fatalf("model-classified single-task stop was rejected or retargeted: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerNeverStopsByKeywordWhenClassificationFails(t *testing.T) {
	for _, content := range []string{
		"现在请你停止当前任务",
		"请停止全部任务",
		"把还没有结束的工作全部撤掉",
		"Could you cancel everything that is still running?",
	} {
		t.Run(content, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			routeWorkerFallbackSteerContext(stub)
			stub.snapshot.Context.Content = content
			stub.classifyErr = errors.New("provider unavailable")
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
				stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
				!strings.Contains(stub.applied[0].Question, "尚未执行") {
				t.Fatalf("classification failure triggered a keyword stop or unclear fallback: failures=%v applied=%+v err=%v",
					stub.failures, stub.applied, err)
			}
		})
	}
}

func TestConversationRouteWorkerClarifiesWaitingUserTaskControlOnClassifyFailure(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Candidates[0].Status = task.StatusWaitingUser
	stub.snapshot.Context.RequestCandidates[0].TaskStatus = task.StatusWaitingUser
	stub.snapshot.Context.Content = "请继续当前旁聊里唯一一条现有任务：开始一个隔离的步骤06正向验收 Task。状态为等待你处理；刚才同一 Task 的 file_write 已完成，现在只用它的 Workspace file_read 读取既有相对路径。"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("waiting-user Task continuation executed without a model decision: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerFallbackClarifiesAmbiguousControlTarget(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{
		{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1},
		{TaskID: "q2345678", InitiatorID: "u1234567", Status: task.StatusWaitingUser, Revision: 2},
	}
	stub.snapshot.Context.Content = "请继续这个任务"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || strings.Contains(stub.applied[0].Question, "分类暂时不可用") ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("ambiguous task control was not clarified contextually: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerFallbackDoesNotGuessNewWorkOrControlTarget(t *testing.T) {
	candidates := []app.ConversationRouteCandidate{
		{TaskID: "q1234567", InitiatorID: "u1234567", Objective: "迁移网站", Status: task.StatusRunning, Revision: 1},
		{TaskID: "q2345678", InitiatorID: "u1234567", Objective: "整理报告", Status: task.StatusRunning, Revision: 2},
	}
	for _, test := range []struct {
		name    string
		context app.ConversationRouteContext
	}{
		{"unresolved predecessor", app.ConversationRouteContext{Content: "请修复这个任务中的构建错误", PreviousUnresolved: true}},
		{"dependent work", app.ConversationRouteContext{Content: "实现发布页面，等整理报告任务完成后再开始", Candidates: candidates}},
		{"explicit new dependent work", app.ConversationRouteContext{Content: "请新开一个任务，等整理报告任务完成后再开始", Candidates: candidates}},
		{"explicit new waits for task ID", app.ConversationRouteContext{Content: "请新开一个任务，等任务 q2345678 完成后开始", Candidates: candidates}},
		{"existing deliverable", app.ConversationRouteContext{Content: "把上一个任务的报告生成 PDF 文件并提供下载", Candidates: candidates}},
		{"high risk change", app.ConversationRouteContext{Content: "请开发一个独立的验收工具，并把工作目录换到 /workspace/new", Candidates: candidates}},
		{"explicit new high risk change", app.ConversationRouteContext{Content: "请新开一个任务，把当前任务工作目录换到 /workspace/new", Candidates: candidates}},
		{"incomplete candidates", app.ConversationRouteContext{Content: "请搜索 Go 的最新版本", CandidateSetIncomplete: true}},
		{"pasted file directive", app.ConversationRouteContext{Content: "请翻译下面的文章：\n请生成文档并提供下载", ContentTruncated: true}},
		{"two named tasks", app.ConversationRouteContext{Content: "请停止迁移网站任务和整理报告任务", Candidates: candidates, PreviousTaskID: "q1234567"}},
		{"conflicting control actions", app.ConversationRouteContext{Content: "请停止当前任务并继续另一个任务", Candidates: candidates[:1], PreviousTaskID: "q1234567"}},
		{"stop and new task", app.ConversationRouteContext{Content: "请停止当前任务并新开一个任务", Candidates: candidates[:1], PreviousTaskID: "q1234567"}},
		{"incomplete named candidates", app.ConversationRouteContext{Content: "请停止迁移网站任务", Candidates: candidates, PreviousTaskID: "q2345678", CandidateSetIncomplete: true}},
		{"unknown task name", app.ConversationRouteContext{Content: "请停止账务任务", Candidates: candidates, PreviousTaskID: "q1234567"}},
		{"another task", app.ConversationRouteContext{Content: "请停止另一个任务", Candidates: candidates, PreviousTaskID: "q1234567"}},
		{"unknown task ID", app.ConversationRouteContext{Content: "请停止当前任务 q9999999", Candidates: candidates, PreviousTaskID: "q1234567"}},
		{"pause is not cancellation", app.ConversationRouteContext{Content: "请暂停当前任务", Candidates: candidates[:1], PreviousTaskID: "q1234567"}},
		{"english pause is not cancellation", app.ConversationRouteContext{Content: "Please pause this task", Candidates: candidates[:1], PreviousTaskID: "q1234567"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.context.ActorID = "u1234567"
			decision := conversationRouteClassifyFallback(test.context)
			if decision.Action != app.ConversationRouteClarify || decision.TaskID != "" {
				t.Fatalf("ambiguous fallback took an action: %+v", decision)
			}
		})
	}
}

func TestConversationRouteWorkerFallbackUsesExplicitTaskID(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "请停止任务 q2345678", PreviousTaskID: "q1234567",
		Candidates: []app.ConversationRouteCandidate{
			{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1},
			{TaskID: "q2345678", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 2},
		}}
	for _, invalidOutput := range []bool{false, true} {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.Context = value
		if invalidOutput {
			stub.decision = app.ConversationRouteDecision{InvalidOutput: true}
			stub.repairRequeues = 1
		} else {
			stub.classifyErr = errors.New("synthetic classifier unavailable")
		}
		if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
			stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" {
			t.Fatalf("keyword fallback stopped a task with explicit ID after invalid=%t: applied=%+v err=%v", invalidOutput, stub.applied, err)
		}
	}
}

func TestConversationRouteWorkerHonorsModelControlSemantics(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "请停止当前任务"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteNew {
		t.Fatalf("model's semantic action was replaced by text keywords: applied=%+v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.snapshot.Context.Content = "请暂停当前任务"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请说明你希望暂停还是停止。"}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question != "请说明你希望暂停还是停止。" {
		t.Fatalf("model clarification for pause was replaced by a keyword decision: applied=%+v err=%v", stub.applied, err)
	}
}

func TestConversationRouteWorkerFallbackDoesNotApplyHighRiskControlChange(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "请继续当前任务，把工作目录换到 /workspace/new"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" {
		t.Fatalf("high-risk control update escaped clarification: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerAsksUserWhenClassifyFallbackCannotDecide(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "帮我查一下天气怎么样"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question == "" {
		t.Fatalf("uncertain message was not clarified: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerClarificationDistinguishesMissingFromUnresolvedPredecessor(t *testing.T) {
	// A predecessor that merely ended unresolved (waiting_user after a
	// clarification) is known state, so ask how this message relates to it.
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.Context.PreviousUnresolved = true
	stub.snapshot.Context.Content = "你看看上面那条信息说了什么"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify ||
		stub.applied[0].Question != "上一条请求还没有完成，这条消息尚未执行。请补充你希望完成的目标，以及它与上一项工作的关系。" {
		t.Fatalf("unresolved predecessor was treated as missing context: applied=%+v err=%v", stub.applied, err)
	}
	// A predecessor row that does not exist at all still asks for context.
	worker, stub = newRouteWorkerTest(t)
	stub.snapshot.PreviousMissing = true
	stub.snapshot.Context.PreviousMissing = true
	stub.snapshot.Context.PreviousUnresolved = true
	stub.snapshot.Context.Content = "你看看上面那条信息说了什么"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify ||
		stub.applied[0].Question != "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。" {
		t.Fatalf("missing predecessor was not treated as missing context: applied=%+v err=%v", stub.applied, err)
	}
}

func TestConversationRouteWorkerClarifiesTrustedPreviousTaskStopOnClassifyFailure(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	routeWorkerFallbackSteerContext(stub)
	stub.snapshot.Context.Content = "停止这个任务"
	stub.classifyErr = errors.New("provider unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].TaskID != "" ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") {
		t.Fatalf("classifier failure did not safely clarify the stop request: failures=%v applied=%+v err=%v",
			stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerNeverStopsAnUnrelatedOrStaleTask(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision app.ConversationRouteDecision
	}{
		{name: "unknown target", decision: app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: "q3456789", ExpectedRevision: 3}},
		{name: "reference target drift", decision: app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: "q2345678", ExpectedRevision: 2}},
		{name: "stale revision", decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate, TaskID: "q1234567", ExpectedRevision: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{
				{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3},
				{TaskID: "q2345678", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 2},
			}
			stub.snapshot.Context.ReferenceTaskID = "q1234567"
			stub.decision = test.decision
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := stub.applied[len(stub.applied)-1]; got.Action != app.ConversationRouteClarify || got.TaskID != "" {
				t.Fatalf("unsafe decision was applied: %+v", got)
			}
		})
	}
}

func TestConversationRouteWorkerHandlesUnavailableReference(t *testing.T) {
	for _, control := range []string{"停止任务", "继续", "查看任务状态", "修改任务"} {
		t.Run(control, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.ReferenceTaskID = "q1234567"
			stub.snapshot.Context.ReferenceUnavailable = true
			stub.snapshot.Context.Content = control
			stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: "q1234567"}
			if _, err := worker.RunOnce(context.Background()); err != nil || stub.resolves != 1 || stub.classifies != 1 ||
				len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify ||
				stub.applied[0].Question != app.UnavailableRouteReferenceMessage {
				t.Fatalf("classified control of unavailable Task was not rejected: applied=%+v resolves=%d classifies=%d err=%v",
					stub.applied, stub.resolves, stub.classifies, err)
			}
		})
	}
	t.Run("new work", func(t *testing.T) {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.Context.ReferenceTaskID = "q1234567"
		stub.snapshot.Context.ReferenceUnavailable = true
		stub.snapshot.Context.Content = "重新整理今年的财报"
		stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.classifies != 1 ||
			len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteNew {
			t.Fatalf("new work did not reach semantic routing: applied=%+v classifies=%d err=%v", stub.applied, stub.classifies, err)
		}
	})
	t.Run("model control", func(t *testing.T) {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.Context.ReferenceTaskID = "q1234567"
		stub.snapshot.Context.ReferenceUnavailable = true
		stub.snapshot.Context.Content = "帮我检查之前的任务进度"
		stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteStatus, TaskID: "q1234567"}
		if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
			stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question != app.UnavailableRouteReferenceMessage {
			t.Fatalf("model control reached unavailable Task: applied=%+v err=%v", stub.applied, err)
		}
	})
}

func TestConversationRouteWorkerClassifiesMissingPredecessorSafely(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.PreviousMissing = true
	stub.snapshot.Context.PreviousMissing = true
	stub.snapshot.Context.PreviousUnresolved = true
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteStop, TaskID: "q1234567"}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.classifies != 1 || stub.applied[0].Action != app.ConversationRouteClarify ||
		stub.applied[0].Question != "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。" {
		t.Fatalf("missing predecessor allowed a task action: applied=%v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteNew || stub.classifies != 2 {
		t.Fatalf("independent work could not be classified: applied=%v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.snapshot.Context.Content = "2+2 等于多少？"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "答案是 4。"}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteReply || stub.applied[0].Reply != "答案是 4。" {
		t.Fatalf("independent reply was blocked: applied=%v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.snapshot.Context.Content = "继续上面的任务"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
		Question: "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。"}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify ||
		stub.applied[0].Question != "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。" {
		t.Fatalf("missing task was not clarified: applied=%v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.snapshot.Context.Content = "把上面的内容缩短"
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请提供需要缩短的原文。"}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question != "请提供需要缩短的原文。" {
		t.Fatalf("specific clarification was lost: applied=%v err=%v", stub.applied, err)
	}
}

func TestConversationRouteWorkerRejectsCyclicPredecessorWithoutTaskAction(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.PreviousCycle = true
	if _, err := worker.RunOnce(context.Background()); err != nil || stub.classifies != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question != "消息顺序存在循环，请重新发送。" {
		t.Fatalf("cycle was not safely clarified: actions=%+v classifications=%d err=%v", stub.applied, stub.classifies, err)
	}
}

func TestConversationRouteWorkerRequeuesOnlySafelyRecoveredPredecessorRace(t *testing.T) {
	for _, safe := range []bool{false, true} {
		worker, stub := newRouteWorkerTest(t)
		stub.snapshot.PreviousMissing = true
		stub.snapshot.Context.PreviousMissing = true
		stub.snapshot.Context.PreviousUnresolved = true
		stub.classifyErr = domain.NewError(domain.CodeConflict, "routing snapshot changed")
		stub.requeued = safe
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.requeueCalls != 1 ||
			(safe && (len(stub.failures) != 0 || len(stub.applied) != 0)) ||
			(!safe && (len(stub.failures) != 0 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify ||
				stub.applied[0].Question != "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。")) {
			t.Fatalf("predecessor race safe=%t requeues=%d failures=%v applied=%+v err=%v", safe, stub.requeueCalls, stub.failures, stub.applied, err)
		}
	}
}

func TestConversationRouteWorkerRequeuesStaleContextAfterClassification(t *testing.T) {
	for _, safe := range []bool{false, true} {
		worker, stub := newRouteWorkerTest(t)
		stub.classifyErr = domain.NewError(domain.CodeConflict, "authorized route candidates changed")
		stub.contextRequeued = safe
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.contextRequeueCalls != 1 || stub.deferred != 0 ||
			(safe && (len(stub.failures) != 0 || len(stub.applied) != 0)) ||
			(!safe && (len(stub.failures) != 1 || stub.failures[0] != "routing_context_unavailable" || len(stub.applied) != 0)) {
			t.Fatalf("stale context safe=%t requeues=%d deferred=%d failures=%v applied=%v err=%v",
				safe, stub.contextRequeueCalls, stub.deferred, stub.failures, stub.applied, err)
		}
	}
}

func TestConversationRouteWorkerFailsPersistentApplyConflict(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.applyErr = domain.NewError(domain.CodeConflict, "route action context changed")
	if _, err := worker.RunOnce(context.Background()); err != nil || stub.deferred != 0 || len(stub.failures) != 1 ||
		stub.failures[0] != "routing_context_unavailable" || len(stub.applied) != 0 {
		t.Fatalf("apply conflict should become a recoverable failure: deferred=%d failures=%v applied=%v err=%v",
			stub.deferred, stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerKeepsApplyLeaseTimeoutRetry(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.claim.LeaseExpiresAt = time.Now().Add(-time.Second)
	stub.applyErr = context.DeadlineExceeded
	if _, err := worker.RunOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) || len(stub.failures) != 0 || len(stub.applied) != 0 {
		t.Fatalf("lease timeout should remain retryable: failures=%v applied=%v err=%v", stub.failures, stub.applied, err)
	}
}

func TestConversationRouteWorkerDefersExpiredLeaseBeforeModelResolution(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.claim.LeaseExpiresAt = time.Now().Add(900 * time.Millisecond)
	stub.checkModelCtx = true
	if found, err := worker.RunOnce(context.Background()); err != nil || !found || stub.deferred != 1 ||
		len(stub.failures) != 0 || len(stub.applied) != 0 || stub.classifies != 0 {
		t.Fatalf("expired pre-dispatch lease should defer safely: found=%t deferred=%d failures=%v applied=%v classifications=%d err=%v",
			found, stub.deferred, stub.failures, stub.applied, stub.classifies, err)
	}
}

func TestConversationRouteWorkerFallsBackWhenModelClassificationFails(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		err         error
		wantFailure string
	}{
		{name: "provider unavailable", err: errors.New("provider unavailable")},
		{name: "model budget exceeded", err: domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")},
		{name: "provider context limit", err: domain.NewError(domain.CodeResultTooLarge, "provider context window exceeded"), wantFailure: "message_too_long"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}
			stub.classifyErr = scenario.err
			if _, err := worker.RunOnce(context.Background()); err != nil {
				t.Fatalf("classification failure was not handled: err=%v", err)
			}
			if scenario.wantFailure != "" {
				if len(stub.failures) != 1 || stub.failures[0] != scenario.wantFailure || len(stub.applied) != 0 {
					t.Fatalf("oversized input did not fail closed: failures=%v applied=%v", stub.failures, stub.applied)
				}
				return
			}
			if len(stub.failures) != 0 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify || stub.applied[0].Question == "" {
				t.Fatalf("model failure was not clarified: failures=%v applied=%v", stub.failures, stub.applied)
			}
		})
	}
}

func TestConversationRouteWorkerHandlesOnlyConfirmedBoundedRateLimits(t *testing.T) {
	providerRateLimit := func(delay time.Duration) *domain.Error {
		err := domain.NewError(domain.CodeRateLimited, "provider rate limited")
		err.Details["reason"] = string(app.ModelErrorRateLimited)
		err.Details["stage"] = "provider_status"
		err.RetryAfter = delay
		if delay > 24*time.Hour {
			err.Details["retry_after_exceeds_durable_horizon"] = "true"
		}
		return err
	}
	for _, test := range []struct {
		name                string
		err                 error
		wantDelay           time.Duration
		wantDelayLow        time.Duration
		wantDelayHigh       time.Duration
		wantHandler         bool
		cancelAfterClassify bool
	}{
		{name: "provider retry after", err: providerRateLimit(1500 * time.Millisecond), wantDelay: 2 * time.Second, wantHandler: true},
		{name: "worker context cancelled after confirmed rejection", err: providerRateLimit(1500 * time.Millisecond), wantDelay: 2 * time.Second, wantHandler: true, cancelAfterClassify: true},
		{name: "bounded fallback", err: providerRateLimit(0), wantDelayLow: 5 * time.Second, wantDelayHigh: 8 * time.Second, wantHandler: true},
		{name: "long provider retry after", err: providerRateLimit(5 * time.Minute), wantDelay: 5 * time.Minute, wantHandler: true},
		{name: "provider retry after at durable horizon", err: providerRateLimit(24 * time.Hour), wantDelay: 24 * time.Hour, wantHandler: true},
		{name: "provider retry after beyond durable horizon ends route", err: providerRateLimit(48 * time.Hour), wantDelay: 0, wantHandler: true},
		{name: "unclassified rate limit", err: domain.NewError(domain.CodeRateLimited, "rate limited")},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.classifyErr = test.err
			stub.rateLimitHandled = true
			ctx := context.Background()
			if test.cancelAfterClassify {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				stub.cancelAfterClassify = cancel
			}
			if found, err := worker.RunOnce(ctx); err != nil || !found {
				t.Fatalf("rate-limited route processing: found=%t err=%v", found, err)
			}
			if test.wantHandler {
				delayOK := stub.rateLimitRetryDelay == test.wantDelay
				if test.wantDelayLow != 0 {
					delayOK = stub.rateLimitRetryDelay >= test.wantDelayLow && stub.rateLimitRetryDelay <= test.wantDelayHigh
				}
				if stub.rateLimitHandleCalls != 1 || !delayOK || len(stub.failures) != 0 || stub.rateLimitContextErr != nil {
					t.Fatalf("rate-limit handling calls=%d delay=%s recovery_ctx_err=%v failures=%v, want one handling with delay %s and a live recovery context",
						stub.rateLimitHandleCalls, stub.rateLimitRetryDelay, stub.rateLimitContextErr, stub.failures, test.wantDelay)
				}
			} else if stub.rateLimitHandleCalls != 0 || len(stub.failures) != 0 || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify {
				t.Fatalf("unconfirmed rate limit was not clarified: calls=%d failures=%v applied=%+v", stub.rateLimitHandleCalls, stub.failures, stub.applied)
			}
		})
	}
}

func TestConversationRouteRateLimitDelayUsesRetryLadder(t *testing.T) {
	providerRateLimit := func(delay time.Duration) *domain.Error {
		err := domain.NewError(domain.CodeRateLimited, "provider rate limited")
		err.Details["reason"] = string(app.ModelErrorRateLimited)
		err.Details["stage"] = "provider_status"
		err.RetryAfter = delay
		if delay > 24*time.Hour {
			err.Details["retry_after_exceeds_durable_horizon"] = "true"
		}
		return err
	}
	claim := func(attempt int64) app.ConversationRouteClaim {
		return app.ConversationRouteClaim{RouteAttempt: attempt}
	}
	if delay, ok := conversationRouteRateLimitDelay(claim(0), providerRateLimit(90*time.Second)); !ok || delay != 90*time.Second {
		t.Fatalf("Retry-After 90s delay = %v, %t, want 90s", delay, ok)
	}
	if delay, ok := conversationRouteRateLimitDelay(claim(0), providerRateLimit(1500*time.Millisecond)); !ok || delay != 2*time.Second {
		t.Fatalf("Retry-After 1500ms delay = %v, %t, want 2s (rounded up)", delay, ok)
	}
	if delay, ok := conversationRouteRateLimitDelay(claim(0), providerRateLimit(48*time.Hour)); !ok || delay != 0 {
		t.Fatalf("over-horizon Retry-After delay = %v, %t, want (0, true) to end the attempt", delay, ok)
	}
	// No Retry-After: first ladder step is the jittered 5s base (5-8s after
	// whole-second rounding), not the old fixed 1s.
	if delay, ok := conversationRouteRateLimitDelay(claim(0), providerRateLimit(0)); !ok || delay < 5*time.Second || delay > 8*time.Second {
		t.Fatalf("first ladder delay = %v, %t, want 5s-8s", delay, ok)
	}
	// Second step doubles the base (10-15s, rounded up to 10-16s).
	if delay, ok := conversationRouteRateLimitDelay(claim(1), providerRateLimit(0)); !ok || delay < 10*time.Second || delay > 16*time.Second {
		t.Fatalf("second ladder delay = %v, %t, want 10s-16s", delay, ok)
	}
	// Past the three-retry budget the route ends instead of requeueing.
	if delay, ok := conversationRouteRateLimitDelay(claim(3), providerRateLimit(0)); !ok || delay != 0 {
		t.Fatalf("spent ladder delay = %v, %t, want (0, true)", delay, ok)
	}
	// Anything but a confirmed provider 429 is not a ladder retry.
	if _, ok := conversationRouteRateLimitDelay(claim(0), domain.NewError(domain.CodeRateLimited, "rate limited")); ok {
		t.Fatal("unconfirmed rate limit was treated as a ladder retry")
	}
	timeout := domain.NewError(domain.CodeUpstreamTimeout, "model connection check failed")
	timeout.Details["reason"] = string(app.ModelErrorTimeout)
	timeout.Details["stage"] = "provider_status"
	if _, ok := conversationRouteRateLimitDelay(claim(0), timeout); ok {
		t.Fatal("provider timeout was treated as a rate-limit ladder retry")
	}
}

func TestConversationRouteWorkerRoutesLongMessagesAndExplainsStorageLimit(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	stub.snapshot.Context.Content = strings.Repeat("a", 40<<10)
	stub.claim.Message.Content = stub.snapshot.Context.Content
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 || len(stub.failures) != 0 {
		t.Fatalf("40 KiB request should be routed intact: failures=%v actions=%v err=%v", stub.failures, stub.applied, err)
	}
	if stub.snapshot.Context.Content != strings.Repeat("a", 40<<10) || !stub.classified.ContentTruncated ||
		stub.classified.CurrentMessageID != stub.claim.Message.ID {
		t.Fatalf("routing preview changed the accepted message source: snapshot=%+v classified=%+v", stub.snapshot.Context, stub.classified)
	}
	stub.applied = nil
	stub.classifies = 0
	stub.failures = nil
	stub.snapshot.Context.Content = strings.Repeat("a", 200<<10+1)
	stub.claim.Message.Content = stub.snapshot.Context.Content
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 || len(stub.failures) != 0 ||
		stub.classifies != 1 || !stub.classified.ContentTruncated {
		t.Fatalf("200 KiB input should route through a bounded source reference: failures=%v actions=%v context=%+v err=%v",
			stub.failures, stub.applied, stub.classified, err)
	}
	stub.applied = nil
	stub.classifies = 0
	stub.failures = nil
	stub.snapshot.Context.Content = strings.Repeat("a", app.MaxConversationMessageBytes+1)
	stub.claim.Message.Content = stub.snapshot.Context.Content
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "message_too_long" || len(stub.applied) != 0 || stub.classifies != 0 {
		t.Fatalf("input above the durable message limit should fail before model dispatch: failures=%v actions=%v err=%v", stub.failures, stub.applied, err)
	}
	stub.failures = nil
	stub.snapshot.Context.Content = strings.Repeat("a", 64<<10)
	stub.claim.Message.Content = stub.snapshot.Context.Content
	stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1, Objective: strings.Repeat("b", 64<<10)}}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 0 || stub.classifies != 1 || len(stub.applied) != 1 {
		t.Fatalf("oversized candidate should be bounded before model dispatch: failures=%v classifications=%d context=%+v err=%v",
			stub.failures, stub.classifies, stub.classified, err)
	}
	request, err := app.ConversationRouteModelRequest(stub.classified)
	var modelInput struct {
		ActiveTasks []app.ConversationRouteCandidate `json:"active_tasks"`
	}
	decodeErr := error(nil)
	if err == nil {
		decodeErr = json.Unmarshal([]byte(request.Input[0].Text), &modelInput)
	}
	if err != nil || decodeErr != nil || len(modelInput.ActiveTasks) != 1 ||
		!modelInput.ActiveTasks[0].ObjectiveTruncated || len(modelInput.ActiveTasks[0].Objective) >= 64<<10 {
		t.Fatalf("oversized task objective was not sent as a bounded preview: request=%+v tasks=%+v err=%v decode=%v",
			request, modelInput, err, decodeErr)
	}
	stub.failures = nil
	stub.snapshot.Context.Candidates[0].Objective = "short"
	stub.claim.Message.Content = stub.snapshot.Context.Content
	stub.classifies = 0
	stub.applied = nil
	stub.classifyErr = domain.NewError(domain.CodeResultTooLarge, "input exceeds limit")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "message_too_long" {
		t.Fatalf("gateway input limit was not surfaced: failures=%v err=%v", stub.failures, err)
	}
}

func TestConversationRouteWorkerClarifiesLongArticleWhenClassificationFails(t *testing.T) {
	content := "好现在你帮我看下这篇文章说了什么：\n# Morde MVP\n" + strings.Repeat("文章正文。", 24_000)
	for _, test := range []struct {
		name            string
		activeCandidate bool
		want            app.ConversationRouteAction
	}{
		{name: "independent long article", want: app.ConversationRouteClarify},
		{name: "independent article with active task", activeCandidate: true, want: app.ConversationRouteClarify},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Content = content
			stub.claim.Message.Content = content
			stub.classifyErr = errors.New("synthetic route provider unavailable")
			if test.activeCandidate {
				stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{
					TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1,
				}}
			}
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 || len(stub.failures) != 0 {
				t.Fatalf("long-message classification fallback failed: applied=%+v failures=%v err=%v", stub.applied, stub.failures, err)
			}
			if got := stub.applied[0].Action; got != test.want {
				t.Fatalf("long-message fallback action = %s, want %s; classified=%+v", got, test.want, stub.classified)
			}
			if !stub.classified.ContentTruncated || stub.classified.CurrentMessageID != stub.claim.Message.ID {
				t.Fatalf("fallback did not retain the original message identity: %+v", stub.classified)
			}
			digest := sha256.Sum256([]byte(content))
			if stub.classified.ContentBytes != int64(len(content)) || !bytes.Equal(stub.classified.ContentSHA256, digest[:]) {
				t.Fatalf("fallback did not retain the full message size and digest: bytes=%d digest=%x, want bytes=%d digest=%x",
					stub.classified.ContentBytes, stub.classified.ContentSHA256, len(content), digest)
			}
		})
	}
}

func TestTruncatedArticleRestartTextDoesNotChangeModelFailureFallback(t *testing.T) {
	content := "这是一份文档：\n正文里写着重新开始新任务。\n" + strings.Repeat("正文。", 1000)
	preview, _ := app.ConversationRouteTextPreview(content, 2048)
	digest := sha256.Sum256([]byte(content))
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: preview, ContentTruncated: true,
		CurrentMessageID: "m2345678", ContentBytes: int64(len(content)), ContentSHA256: digest[:],
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusWaitingUser}}}
	if decision := conversationRouteClassifyFallback(value); decision.Action != app.ConversationRouteClarify {
		t.Fatalf("model failure used article text to start a Task: %+v", decision)
	}
	if decision := safeConversationRouteDecision(value, app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}); decision.Action != app.ConversationRouteNew {
		t.Fatalf("valid model action was overridden by article text: %+v", decision)
	}
}

func TestConversationRouteWorkerInvalidOutputClarifiesLongDuplicateWithoutKeywordFallback(t *testing.T) {
	content := "感动你终于不报错了.好现在你帮我看下这篇文章说了什么：\n# 长文章\n" + strings.Repeat("正文。", 3000)
	digest := sha256.Sum256([]byte(content))
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.Context.Content = content
	stub.claim.Message.Content = content
	stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{
		TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusWaitingUser, Revision: 1,
	}}
	scope := sha256.Sum256([]byte("same execution scope"))
	stub.snapshot.Context.ExecutionScopeDigest = scope[:]
	stub.snapshot.Context.RequestCandidates = []app.ConversationRouteRequestCandidate{{
		MessageID: "m7654321", Sequence: 1, ContentTruncated: true, ContentSHA256: digest[:],
		TaskID: "q1234567", TaskStatus: task.StatusWaitingUser, RoutingStatus: conversation.RoutingRouted,
		SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: scope[:],
	}}
	stub.decision = app.ConversationRouteDecision{InvalidOutput: true}
	for range 2 {
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteClarify ||
		!strings.Contains(stub.applied[0].Question, "尚未执行") || strings.Contains(stub.applied[0].Question, "重新开始") {
		t.Fatalf("long-message classification used article wording as an action fallback: applied=%+v", stub.applied)
	}
}

func TestConversationRouteWorkerRoutesClaimedProjectAttachment(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.claim.Input.ProjectID = "project-test"
	stub.claim.Input.ClientAttachmentIDs = []string{"attachment-0001"}
	stub.snapshot.Context.Content = ""
	// The real queue claims a project message only after every declared slot
	// has a committed attachment. Once claimed it follows normal routing.
	if _, err := worker.RunOnce(context.Background()); err != nil || stub.deferred != 0 || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteNew || stub.classifies != 0 {
		t.Fatalf("committed project attachment was not routed: deferred=%d applied=%v err=%v", stub.deferred, stub.applied, err)
	}
}

type concurrentRouteWorkerHarness struct {
	mu              sync.Mutex
	claims          []app.ConversationRouteClaim
	snapshots       map[string]app.ConversationRouteSnapshot
	classificationA chan struct{}
	releaseA        chan struct{}
	completed       chan string
}

func (h *concurrentRouteWorkerHarness) ClaimConversationRoute(context.Context, string, time.Duration) (app.ConversationRouteClaim, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.claims) == 0 {
		return app.ConversationRouteClaim{}, false, nil
	}
	claim := h.claims[0]
	h.claims = h.claims[1:]
	return claim, true, nil
}

func (*concurrentRouteWorkerHarness) ReleaseConversationRoute(context.Context, app.ConversationRouteClaim) error {
	return nil
}

func (h *concurrentRouteWorkerHarness) LoadConversationRouteSnapshot(_ context.Context, claim app.ConversationRouteClaim) (app.ConversationRouteSnapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshots[claim.Message.ID], nil
}

func (h *concurrentRouteWorkerHarness) ApplyConversationRoute(_ context.Context, claim app.ConversationRouteClaim, _ app.ConversationRouteSnapshot, _ app.ConversationRouteDecision) error {
	h.completed <- claim.Message.ID
	return nil
}

func (*concurrentRouteWorkerHarness) FailConversationRoute(context.Context, app.ConversationRouteClaim, string) error {
	return nil
}

func (*concurrentRouteWorkerHarness) DeferConversationRoute(context.Context, app.ConversationRouteClaim, time.Duration) error {
	return nil
}

func (*concurrentRouteWorkerHarness) ApplyNextConversationTaskRevision(context.Context) (bool, error) {
	return false, nil
}

func (*concurrentRouteWorkerHarness) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (h *concurrentRouteWorkerHarness) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim, _ app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	switch claim.Message.ID {
	case "m1234567":
		close(h.classificationA)
		select {
		case <-ctx.Done():
			return app.ConversationRouteDecision{}, ctx.Err()
		case <-h.releaseA:
			return app.ConversationRouteDecision{Action: app.ConversationRouteNew}, nil
		}
	case "m2345678":
		select {
		case <-ctx.Done():
			return app.ConversationRouteDecision{}, ctx.Err()
		case <-h.classificationA:
			return app.ConversationRouteDecision{Action: app.ConversationRouteNew}, nil
		}
	default:
		return app.ConversationRouteDecision{}, errors.New("unexpected route claim")
	}
}

func TestConversationRouteWorkerRunsIndependentMessageWhileOtherClassificationIsBlocked(t *testing.T) {
	newClaim := func(messageID string) app.ConversationRouteClaim {
		return app.ConversationRouteClaim{
			Message:        conversation.Message{ID: messageID, CreatedAt: time.Now()},
			LeaseExpiresAt: time.Now().Add(time.Minute),
		}
	}
	harness := &concurrentRouteWorkerHarness{
		claims: []app.ConversationRouteClaim{
			newClaim("m1234567"),
			newClaim("m2345678"),
		},
		snapshots: map[string]app.ConversationRouteSnapshot{
			"m1234567": {Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "first independent request"}},
			"m2345678": {Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "second independent request"}},
		},
		classificationA: make(chan struct{}),
		releaseA:        make(chan struct{}),
		completed:       make(chan string, 2),
	}
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: harness, Actions: harness, Models: harness, WorkerID: "route-parallel-test",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- worker.Run(ctx) }()

	select {
	case <-harness.classificationA:
	case <-time.After(2 * time.Second):
		t.Fatal("first message did not enter the blocked classifier")
	}
	select {
	case completedID := <-harness.completed:
		if completedID != "m2345678" {
			t.Fatalf("completed message while first classifier was blocked = %q, want second message", completedID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second independent message did not complete while first classification was blocked")
	}

	close(harness.releaseA)
	select {
	case completedID := <-harness.completed:
		if completedID != "m1234567" {
			t.Fatalf("completion after releasing first classifier = %q, want first message", completedID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first message did not complete after releasing its classifier")
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("worker run ended with %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

type summaryInputPageStub struct {
	page app.ConversationSummaryInputPage
	next *app.ConversationSummaryInputPage
}

func (s summaryInputPageStub) LoadConversationSummaryInputPage(_ context.Context, request app.ConversationSummaryInputPageRequest) (app.ConversationSummaryInputPage, error) {
	if request.Cursor != nil && s.next != nil {
		return *s.next, nil
	}
	return s.page, nil
}

type summaryRouteModelsStub struct {
	*routeWorkerStub
	request CloudDSHConversationSummaryRequest
	result  CloudDSHConversationSummaryResult
	window  int
}

func (s *summaryRouteModelsStub) ResolveConversationSummaryModel(context.Context, app.ConversationRouteClaim) (string, int, error) {
	return "p1234567", s.window, nil
}

func (s *summaryRouteModelsStub) EstimateConversationSummaryTokenPressure(_ context.Context,
	request CloudDSHConversationSummaryEstimateRequest) ([]int, error) {
	counts := make([]int, len(request.Sources))
	pressure := 128
	for index, source := range request.Sources {
		pressure += utf8.RuneCountInString(source.Content)
		counts[index] = pressure
	}
	return counts, nil
}

func (s *summaryRouteModelsStub) CompactConversationSummary(_ context.Context, request CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error) {
	s.request = request
	return s.result, nil
}

type summaryStoreStub struct {
	app.ConversationSummaryStore
	identity  app.ConversationSummaryIdentity
	operation app.ConversationSummaryOperation
	commit    app.ConversationSummaryCommit
	commitErr error
	releases  []app.ConversationSummaryOperationRelease
}

func (s *summaryStoreStub) ClaimConversationSummaryOperation(_ context.Context, command app.ClaimConversationSummaryOperation) (app.ConversationSummaryOperation, bool, error) {
	s.operation = app.ConversationSummaryOperation{Identity: s.identity, OperationID: "o1234567",
		InputSHA256: command.InputSHA256, BaselineVersion: command.BaselineVersion, State: "claimed",
		LeaseOwner: command.LeaseOwner, LeaseVersion: 1, CheckpointThrough: -1}
	return s.operation, false, nil
}

func (s *summaryStoreStub) CommitConversationSummary(_ context.Context, command app.ConversationSummaryCommit) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.commit = command
	return nil
}

func (s *summaryStoreStub) ReleaseConversationSummaryOperation(_ context.Context, command app.ConversationSummaryOperationRelease) error {
	s.releases = append(s.releases, command)
	return nil
}

func newSummaryRouteWorkerHarness(t *testing.T, fullText string, contextWindow int,
	result CloudDSHConversationSummaryResult) (*ConversationRouteWorker, *routeWorkerStub,
	*summaryRouteModelsStub, *summaryStoreStub, *app.Sealer, app.ConversationSummaryIdentity, []byte) {
	t.Helper()
	claim, routeContext := coordinatorTestClaimAndContext()
	claim.Message.ID, claim.Message.Sequence, claim.Message.Content = "m2345678", 2, "new message"
	claim.LeaseExpiresAt = time.Now().Add(time.Minute)
	identity, err := app.ConversationSummaryScopeForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := sha256.Sum256([]byte(fullText))
	source := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: sourceDigest[:]}
	page := app.ConversationSummaryInputPage{Identity: identity, BoundarySequence: 1, Head: app.ConversationSummaryHead{Identity: identity}}
	for offset := 0; offset < len(fullText); {
		end := min(len(fullText), offset+app.MaxConversationSummaryInputPageBytes)
		for end < len(fullText) && !utf8.ValidString(fullText[:end]) {
			end--
		}
		page.Sources = append(page.Sources, app.ConversationSummaryPromptSource{Source: source, Role: "user",
			ByteOffset: int64(offset), Content: fullText[offset:end]})
		offset = end
	}
	store := &summaryStoreStub{identity: identity}
	route := &routeWorkerStub{claim: claim,
		snapshot: app.ConversationRouteSnapshot{Context: routeContext}, decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew}}
	models := &summaryRouteModelsStub{routeWorkerStub: route, result: result, window: contextWindow}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: route, Actions: route,
		Models: models, SummaryStore: store, SummaryInputs: summaryInputPageStub{page: page}, SummarySealer: sealer,
		WorkerID: "route-summary-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	return worker, route, models, store, sealer, identity, sourceDigest[:]
}

func TestConversationRouteWorkerCommitsBoundedDSHSummaryBeforeRouting(t *testing.T) {
	worker, route, models, store, sealer, identity, sourceDigest := newSummaryRouteWorkerHarness(t,
		strings.Repeat("事实", 13_000), 100_000,
		CloudDSHConversationSummaryResult{Text: "short summary", ModelID: "p1234567", SessionID: "summary-session", Through: 12})
	found, err := worker.RunOnce(context.Background())
	if err != nil || !found || len(store.commit.Sources) != 1 || store.commit.BaselineVersion != 0 ||
		store.commit.OperationID != "o1234567" || len(store.commit.Ciphertext) == 0 || len(store.releases) != 0 ||
		len(models.request.Sources) != 2 || models.request.Sources[1].ByteOffset != int64(len(models.request.Sources[0].Content)) ||
		len(models.request.Sources[0].Content) > app.MaxConversationSummaryInputPageBytes || models.classifies != 1 ||
		len(route.applied) != 1 {
		t.Fatalf("summary maintenance result: found=%t commit=%+v releases=%+v request_sources=%d classifications=%d applied=%d err=%v",
			found, store.commit, store.releases, len(models.request.Sources), models.classifies, len(route.applied), err)
	}
	opened, err := app.OpenConversationSummary(sealer, app.StoredConversationSummary{Identity: identity, Version: 1,
		OperationID:   "o1234567",
		SummarySHA256: store.commit.SummarySHA256, SourcesSHA256: store.commit.SourcesSHA256,
		Ciphertext: store.commit.Ciphertext, Sources: store.commit.Sources})
	if err != nil || opened.Text != "short summary" || !bytes.Equal(opened.Sources[0].SHA256, sourceDigest[:]) {
		t.Fatalf("committed summary failed to reopen: %+v err=%v", opened, err)
	}
}

func TestConversationRouteWorkerTriggersSummaryByTokenPressureBeforeTransportWatermark(t *testing.T) {
	worker, route, models, store, _, _, _ := newSummaryRouteWorkerHarness(t, strings.Repeat("x", 22_000), 32_768,
		CloudDSHConversationSummaryResult{Text: "short summary", ModelID: "p1234567", SessionID: "summary-session", Through: 12})
	committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
	if err != nil || !committed || len(models.request.Sources) != 1 ||
		conversationSummaryMaintenanceInputBytes("", models.request.Sources) >= conversationSummaryMaintenanceMaxInputBytes*3/4 ||
		len(store.commit.Ciphertext) == 0 {
		t.Fatalf("token-pressure summary committed=%t sources=%d input_bytes=%d commit=%+v err=%v",
			committed, len(models.request.Sources), conversationSummaryMaintenanceInputBytes("", models.request.Sources), store.commit, err)
	}
}

func TestConversationRouteWorkerTrimsSummaryAtWholeSourceTokenBoundary(t *testing.T) {
	worker, route, models, store, _, identity, _ := newSummaryRouteWorkerHarness(t, "placeholder", 16_384,
		CloudDSHConversationSummaryResult{Text: "short summary", ModelID: "p1234567", SessionID: "summary-session", Through: 12})
	route.claim.Message.Sequence = 3
	firstText, secondText := strings.Repeat("x", 8_000), strings.Repeat("y", 6_000)
	firstDigest, secondDigest := sha256.Sum256([]byte(firstText)), sha256.Sum256([]byte(secondText))
	firstSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: firstDigest[:]}
	secondSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234568", Sequence: 2, SHA256: secondDigest[:]}
	worker.options.SummaryInputs = summaryInputPageStub{page: app.ConversationSummaryInputPage{
		Identity: identity, BoundarySequence: 2, Head: app.ConversationSummaryHead{Identity: identity},
		Sources: []app.ConversationSummaryPromptSource{
			{Source: firstSource, Role: "user", Content: firstText},
			{Source: secondSource, Role: "assistant", Content: secondText},
		},
	}}
	committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
	if err != nil || !committed || len(models.request.Sources) != 1 || models.request.Sources[0].Source.ID != firstSource.ID ||
		len(store.commit.Sources) != 1 || store.commit.Sources[0].ID != firstSource.ID {
		t.Fatalf("summary crossed a source token boundary: committed=%t request=%+v proofs=%+v err=%v",
			committed, models.request.Sources, store.commit.Sources, err)
	}
}

func TestConversationRouteWorkerMarksPartiallyMigratedHistory(t *testing.T) {
	worker, route, _, store, sealer, identity, _ := newSummaryRouteWorkerHarness(t,
		strings.Repeat("x", 80<<10), 100_000,
		CloudDSHConversationSummaryResult{Text: "Older history summary.", ModelID: "p1234567", SessionID: "summary-session", Through: 12})
	route.claim.Message.Sequence = 3
	firstText := strings.Repeat("x", 80<<10)
	firstDigest := sha256.Sum256([]byte(firstText))
	firstSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: firstDigest[:]}
	firstPage := app.ConversationSummaryInputPage{Identity: identity, BoundarySequence: 2,
		Head: app.ConversationSummaryHead{Identity: identity}}
	for offset := 0; offset < len(firstText); {
		end := min(len(firstText), offset+app.MaxConversationSummaryInputPageBytes)
		firstPage.Sources = append(firstPage.Sources, app.ConversationSummaryPromptSource{Source: firstSource,
			Role: "user", Content: firstText[offset:end], ByteOffset: int64(offset)})
		offset = end
	}
	firstPage.Next = &app.ConversationSummaryInputCursor{Identity: identity, BoundarySequence: 2, Sequence: 1,
		KindOrder: 0, ID: firstSource.ID, ByteOffset: int64(len(firstText))}
	secondText := "A later source that remains for the next incremental pass."
	secondDigest := sha256.Sum256([]byte(secondText))
	secondPage := app.ConversationSummaryInputPage{Identity: identity, BoundarySequence: 2,
		Head: app.ConversationSummaryHead{Identity: identity}, Sources: []app.ConversationSummaryPromptSource{{
			Source: app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234568", Sequence: 2, SHA256: secondDigest[:]},
			Role:   "user", Content: secondText,
		}}}
	worker.options.SummaryInputs = summaryInputPageStub{page: firstPage, next: &secondPage}
	committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
	if err != nil || !committed || len(store.commit.Sources) != 1 || store.commit.Sources[0].ID != firstSource.ID {
		t.Fatalf("partial history compaction committed=%t sources=%+v err=%v", committed, store.commit.Sources, err)
	}
	opened, err := app.OpenConversationSummary(sealer, app.StoredConversationSummary{Identity: identity, Version: 1,
		OperationID: "o1234567", SummarySHA256: store.commit.SummarySHA256, SourcesSHA256: store.commit.SourcesSHA256,
		Ciphertext: store.commit.Ciphertext, Sources: store.commit.Sources})
	if err != nil || !strings.HasPrefix(opened.Text, conversationSummaryIncompleteNotice+"\n\n") {
		t.Fatalf("partial history was not marked in stored summary: text=%q err=%v", opened.Text, err)
	}
}

func TestConversationRouteWorkerResumesCommittedHistoryCoverage(t *testing.T) {
	worker, route, _, store, sealer, identity, _ := newSummaryRouteWorkerHarness(t, "", 100_000,
		CloudDSHConversationSummaryResult{Text: "Complete history summary.", ModelID: "p1234567", SessionID: "summary-session", Through: 12})
	route.claim.Message.Sequence = 3
	firstText := "First migrated history source."
	firstDigest := sha256.Sum256([]byte(firstText))
	firstSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: firstDigest[:]}
	prior, err := app.SealConversationSummary(sealer, app.ConversationSummaryDocument{Identity: identity, Version: 1,
		OperationID: "o1234567", Text: conversationSummaryIncompleteNotice + "\n\nFirst migrated history summary.", Sources: []app.ConversationSummarySource{firstSource}})
	if err != nil {
		t.Fatal(err)
	}
	secondText := strings.Repeat("y", 80<<10)
	secondDigest := sha256.Sum256([]byte(secondText))
	secondSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234568", Sequence: 2, SHA256: secondDigest[:]}
	page := app.ConversationSummaryInputPage{Identity: identity, BoundarySequence: 2,
		Head: app.ConversationSummaryHead{Identity: identity, Version: 1, Digest: prior.SummarySHA256}, Summary: &prior}
	for offset := 0; offset < len(secondText); {
		end := min(len(secondText), offset+app.MaxConversationSummaryInputPageBytes)
		page.Sources = append(page.Sources, app.ConversationSummaryPromptSource{Source: secondSource,
			Role: "user", Content: secondText[offset:end], ByteOffset: int64(offset)})
		offset = end
	}
	worker.options.SummaryInputs = summaryInputPageStub{page: page}
	committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
	if err != nil || !committed || store.commit.BaselineVersion != 1 || len(store.commit.Sources) != 2 {
		t.Fatalf("incremental coverage commit=%+v committed=%t err=%v", store.commit, committed, err)
	}
	opened, err := app.OpenConversationSummary(sealer, app.StoredConversationSummary{Identity: identity, Version: 2,
		OperationID: "o1234567", SummarySHA256: store.commit.SummarySHA256, SourcesSHA256: store.commit.SourcesSHA256,
		Ciphertext: store.commit.Ciphertext, Sources: store.commit.Sources})
	if err != nil || strings.HasPrefix(opened.Text, conversationSummaryIncompleteNotice) {
		t.Fatalf("completed incremental coverage retained incomplete marker: text=%q err=%v", opened.Text, err)
	}
}

func TestConversationRouteWorkerRejectsInvalidSummaryResultsAndKeepsRouting(t *testing.T) {
	for _, test := range []struct {
		name         string
		text         string
		modelID      string
		contextBytes int
		commitErr    error
	}{
		{name: "empty", modelID: "p1234567", contextBytes: 100_000},
		{name: "oversized", text: strings.Repeat("x", conversationSummaryMaintenanceMaxOutputBytes+1), modelID: "p1234567", contextBytes: 100_000},
		{name: "model changed", text: "short", modelID: "p7654321", contextBytes: 100_000},
		{name: "not smaller", text: strings.Repeat("x", 12_000), modelID: "p1234567", contextBytes: 16_000},
		{name: "commit failed", text: "short", modelID: "p1234567", contextBytes: 100_000, commitErr: errors.New("synthetic commit failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Repeat("a", 10_000)
			if test.contextBytes == 100_000 {
				body = strings.Repeat("事实", 13_000)
			}
			worker, route, models, store, _, _, _ := newSummaryRouteWorkerHarness(t, body, test.contextBytes,
				CloudDSHConversationSummaryResult{Text: test.text, ModelID: test.modelID, SessionID: "summary-session", Through: 12})
			store.commitErr = test.commitErr
			found, err := worker.RunOnce(context.Background())
			if err != nil || !found || len(store.commit.Ciphertext) != 0 || len(store.releases) != 1 || store.releases[0].State != "failed" ||
				len(route.applied) != 1 || models.classifies != 1 {
				t.Fatalf("invalid summary did not fail closed while routing continued: found=%t commit=%+v releases=%+v applied=%d classifications=%d err=%v",
					found, store.commit, store.releases, len(route.applied), models.classifies, err)
			}
		})
	}
}

func TestConversationRouteWorkerUsesOnlyOpenedSummaryAndUncoveredHistory(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "opened"
		if corrupt {
			name = "cannot-open"
		}
		t.Run(name, func(t *testing.T) {
			claim, routeContext := coordinatorTestClaimAndContext()
			claim.Message.Sequence, claim.Message.Content = 3, "继续"
			claim.LeaseExpiresAt = time.Now().Add(time.Minute)
			routeContext.Content = claim.Message.Content
			routeContext.History = []app.ConversationRouteHistory{
				{MessageID: "m2345678", Sequence: 1, Role: conversation.RoleUser, Content: "旧消息"},
				{MessageID: "m3456789", Sequence: 2, TaskID: "q1234567", TaskRevision: 1, EventSequence: 1,
					Role: conversation.RoleAssistant, Content: "已发布结果"},
			}
			identity, err := app.ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			messageDigest := sha256.Sum256([]byte("旧消息"))
			resultDigest := sha256.Sum256([]byte("已发布结果"))
			sources := []app.ConversationSummarySource{
				{Kind: app.ConversationSummarySourceMessage, ID: "m2345678", Sequence: 1, SHA256: messageDigest[:]},
				{Kind: app.ConversationSummarySourceTask, ID: "q1234567", Version: 1, Sequence: 1, SHA256: resultDigest[:]},
			}
			sealer, err := app.NewSealer(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			operationID := "o1234567"
			stored, err := app.SealConversationSummary(sealer, app.ConversationSummaryDocument{
				Identity: identity, Version: 1, OperationID: operationID, Text: "已核实摘要", Sources: sources,
			})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				stored.Ciphertext[0] ^= 0xff
			}
			routeContext.SummaryVersion, routeContext.SummarySHA256 = stored.Version, append([]byte(nil), stored.SummarySHA256...)
			routeContext.SummarySourcesSHA256 = append([]byte(nil), stored.SourcesSHA256...)
			route := &routeWorkerStub{claim: claim, snapshot: app.ConversationRouteSnapshot{Context: routeContext, Summary: &stored},
				decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew}}
			worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: route, Actions: route, Models: route,
				SummarySealer: sealer, WorkerID: "route-summary-context-test", LeaseDuration: time.Minute,
				PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			found, err := worker.RunOnce(context.Background())
			if err != nil || !found || route.classifies != 1 || len(route.applied) != 1 {
				t.Fatalf("route was not classified and applied: found=%t classified=%d applied=%d err=%v", found, route.classifies, len(route.applied), err)
			}
			if corrupt {
				if route.classified.SummaryVersion != 0 || route.classified.SummaryText != "" || len(route.classified.History) != 2 {
					t.Fatalf("unopenable summary was used or its raw history was dropped: %+v", route.classified)
				}
			} else if route.classified.SummaryVersion != 1 || route.classified.SummaryText != "已核实摘要" || len(route.classified.History) != 0 ||
				len(route.snapshot.Context.History) != 2 {
				t.Fatalf("summary routing context or immutable snapshot is wrong: classified=%+v snapshot=%+v", route.classified, route.snapshot)
			}
		})
	}
}
