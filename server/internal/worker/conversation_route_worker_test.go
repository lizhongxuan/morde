package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type routeWorkerStub struct {
	claim        app.ConversationRouteClaim
	snapshot     app.ConversationRouteSnapshot
	decision     app.ConversationRouteDecision
	modelErr     error
	classifyErr  error
	applied      []app.ConversationRouteDecision
	failures     []string
	deferred     int
	resolves     int
	classifies   int
	requeued     bool
	requeueCalls int
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
func (s *routeWorkerStub) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	s.resolves++
	return "p1234567", s.modelErr
}
func (s *routeWorkerStub) ClassifyConversationRoute(context.Context, app.ConversationRouteClaim, app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	s.classifies++
	return s.decision, s.classifyErr
}

func newRouteWorkerTest(t *testing.T) (*ConversationRouteWorker, *routeWorkerStub) {
	t.Helper()
	stub := &routeWorkerStub{claim: app.ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", CreatedAt: time.Now()},
		LeaseExpiresAt: time.Now().Add(time.Minute)}, snapshot: app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "hello"}}}
	worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: stub, Actions: stub, Models: stub,
		WorkerID: "route-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 2, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	return worker, stub
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
		{name: "task target", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, TaskID: "q1234567", Reply: "answer"}, question: "请说明要修改哪个任务，或是否开始一个新任务。"},
		{name: "empty text", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply}, question: "请说明要修改哪个任务，或是否开始一个新任务。"},
		{name: "model output too long", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "answer", ModelOutput: strings.Repeat("x", app.MaxConversationRouteModelOutputBytes+1)}, question: "请说明要修改哪个任务，或是否开始一个新任务。"},
		{name: "unavailable control", context: app.ConversationRouteContext{Content: "停止任务", ReferenceUnavailable: true}, decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "I cannot stop it."}, question: app.UnavailableRouteReferenceMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := safeConversationRouteDecision(test.context, test.decision)
			if decision.Action != app.ConversationRouteClarify || decision.Question != test.question || decision.Reply != "" {
				t.Fatalf("unsafe direct reply survived validation: %+v", decision)
			}
		})
	}
}

func TestConversationRouteWorkerClassifiesEveryFreeTextMessage(t *testing.T) {
	for _, content := range []string{"帮我整理资料", "停止任务", "查看进度"} {
		t.Run(content, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Content = content
			stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
			if _, err := worker.RunOnce(context.Background()); err != nil || stub.resolves != 1 || stub.classifies != 1 ||
				len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteNew {
				t.Fatalf("free text bypassed semantic classification: content=%q resolutions=%d classifications=%d decisions=%+v err=%v",
					content, stub.resolves, stub.classifies, stub.applied, err)
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
			len(stub.failures) != 1 || stub.failures[0] != "model_unavailable" || len(stub.applied) != 0 {
			t.Fatalf("unclassified free text created a task: classifications=%d failures=%v decisions=%+v err=%v",
				stub.classifies, stub.failures, stub.applied, err)
		}
	})
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
		stub.applied[0].Question != "前一条消息尚未确认，请先处理或重试。" {
		t.Fatalf("missing predecessor allowed a task action: applied=%v err=%v", stub.applied, err)
	}
	stub.applied = nil
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action != app.ConversationRouteNew || stub.classifies != 2 {
		t.Fatalf("independent work could not be classified: applied=%v err=%v", stub.applied, err)
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
		if _, err := worker.RunOnce(context.Background()); err != nil || stub.requeueCalls != 1 || len(stub.applied) != 0 ||
			(safe && len(stub.failures) != 0) || (!safe && (len(stub.failures) != 1 || stub.failures[0] != "model_unavailable")) {
			t.Fatalf("predecessor race safe=%t requeues=%d failures=%v err=%v", safe, stub.requeueCalls, stub.failures, err)
		}
	}
}

func TestConversationRouteWorkerRecordsModelFailureWithoutTaskAction(t *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{name: "provider unavailable", err: errors.New("provider unavailable")},
		{name: "model budget exceeded", err: domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			worker, stub := newRouteWorkerTest(t)
			stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1}}
			stub.classifyErr = scenario.err
			if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "model_unavailable" || len(stub.applied) != 0 {
				t.Fatalf("model failure affected task routing: failures=%v applied=%v err=%v", stub.failures, stub.applied, err)
			}
		})
	}
}

func TestConversationRouteWorkerPreservesLongMessagesAndExplainsOversizedInput(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	stub.snapshot.Context.Content = strings.Repeat("a", 40<<10)
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 || len(stub.failures) != 0 {
		t.Fatalf("40 KiB request should be routed intact: failures=%v actions=%v err=%v", stub.failures, stub.applied, err)
	}
	stub.applied = nil
	stub.classifies = 0
	stub.snapshot.Context.Content = strings.Repeat("a", app.MaxConversationAgentInputBytes+1)
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "message_too_long" || len(stub.applied) != 0 || stub.classifies != 0 {
		t.Fatalf("oversized input should fail before model dispatch: failures=%v actions=%v err=%v", stub.failures, stub.applied, err)
	}
	stub.failures = nil
	stub.snapshot.Context.Content = strings.Repeat("a", 64<<10)
	stub.snapshot.Context.Candidates = []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1, Objective: strings.Repeat("b", 64<<10)}}
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "message_too_long" || stub.classifies != 0 {
		t.Fatalf("oversized routing context should fail before model dispatch: failures=%v classifications=%d err=%v", stub.failures, stub.classifies, err)
	}
	stub.failures = nil
	stub.snapshot.Context.Candidates[0].Objective = "short"
	stub.classifyErr = domain.NewError(domain.CodeResultTooLarge, "input exceeds limit")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.failures) != 1 || stub.failures[0] != "message_too_long" {
		t.Fatalf("gateway input limit was not surfaced: failures=%v err=%v", stub.failures, err)
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
