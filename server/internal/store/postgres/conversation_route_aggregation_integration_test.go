package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type slowAggregationRouteModel struct {
	rootID, childID, chatID, independentID string
	rootStarted, childStarted              chan struct{}
	rootRelease, childRelease              chan struct{}
}

type singleAggregationClaimQueue struct {
	claim app.ConversationRouteClaim
	used  bool
}

type resolvingAggregationActions struct {
	*Store
	resolveCandidate func(context.Context) error
}

func (a *resolvingAggregationActions) FailConversationRoute(ctx context.Context, claim app.ConversationRouteClaim, reason string) error {
	if err := a.resolveCandidate(ctx); err != nil {
		return err
	}
	return a.Store.FailConversationRoute(ctx, claim, reason)
}

func (a *resolvingAggregationActions) ResolveConversationRouteAggregation(ctx context.Context, claim app.ConversationRouteClaim) error {
	if err := a.resolveCandidate(ctx); err != nil {
		return err
	}
	return a.Store.ResolveConversationRouteAggregation(ctx, claim)
}

func TestPostgresConversationRouteAggregationMigrationPreservesLegacyAndEvidence(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(f.store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 197); err != nil {
		t.Fatalf("remove unused aggregation migration: %v", err)
	}
	legacy := acceptAggregationMessage(t, f, "route-aggregation-legacy", "Legacy request")
	if _, err := provider.UpTo(ctx, 198); err != nil {
		t.Fatalf("migrate legacy route: %v", err)
	}
	var state string
	var started, deadline, created time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT aggregation_state,aggregation_started_at,aggregation_deadline,created_at
FROM conversation_message_routes WHERE message_id=$1`, legacy.ID).Scan(&state, &started, &deadline, &created); err != nil ||
		state != "settled" || !started.Equal(created) || !deadline.Equal(created) {
		t.Fatalf("legacy route was delayed by migration: state=%s start=%s deadline=%s created=%s err=%v", state, started, deadline, created, err)
	}
	if _, err := provider.DownTo(ctx, 197); err != nil {
		t.Fatalf("roll back legacy-only aggregation fields: %v", err)
	}
	if _, err := provider.UpTo(ctx, 198); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_state='waiting' WHERE message_id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 197); err == nil || !strings.Contains(err.Error(), "cannot discard conversation route aggregation evidence") {
		t.Fatalf("migration discarded waiting aggregation evidence: %v", err)
	}
}

func (q *singleAggregationClaimQueue) ClaimConversationRoute(context.Context, string, time.Duration) (app.ConversationRouteClaim, bool, error) {
	if q.used {
		return app.ConversationRouteClaim{}, false, nil
	}
	q.used = true
	return q.claim, true, nil
}

func (*singleAggregationClaimQueue) ReleaseConversationRoute(context.Context, app.ConversationRouteClaim) error {
	return nil
}

func (m *slowAggregationRouteModel) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (m *slowAggregationRouteModel) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	switch claim.Message.ID {
	case m.rootID:
		if claim.RouteAttempt == 1 {
			close(m.rootStarted)
			select {
			case <-m.rootRelease:
			case <-ctx.Done():
				return app.ConversationRouteDecision{}, ctx.Err()
			}
			return app.ConversationRouteDecision{Action: app.ConversationRouteNew}, nil
		}
		ids := make([]string, 0, len(value.AggregationCandidates))
		for _, candidate := range value.AggregationCandidates {
			ids = append(ids, candidate.MessageID)
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, AggregateMessageIDs: ids}, nil
	case m.childID:
		if claim.RouteAttempt == 1 {
			close(m.childStarted)
			select {
			case <-m.childRelease:
			case <-ctx.Done():
				return app.ConversationRouteDecision{}, ctx.Err()
			}
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteJoin, TargetMessageID: m.rootID}, nil
	case m.chatID:
		return app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "Hello."}, nil
	case m.independentID:
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}, nil
	default:
		if app.ValidConversationRouteJoinTarget(m.rootID, value.RequestCandidates) {
			return app.ConversationRouteDecision{}, fmt.Errorf("outside-window request was offered the root as joinable")
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}, nil
	}
}

func acceptAggregationMessage(t *testing.T, f routeActionsTestFixture, clientID, content string) conversation.Message {
	t.Helper()
	input := app.ConversationRoutingInput{ModelID: "p1234567", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}}
	hash := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(context.Background(), app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: hash[:], Input: input, Authorization: f.authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestPostgresConversationRouteAggregationWaitsForSlowClassifiersButNotIndependentWork(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	root := acceptAggregationMessage(t, f, "route-slow-root", "Prepare a report")
	model := &slowAggregationRouteModel{rootID: root.ID, rootStarted: make(chan struct{}), childStarted: make(chan struct{}),
		rootRelease: make(chan struct{}), childRelease: make(chan struct{})}
	w, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{Queue: f.store, Actions: f.store, Models: model,
		WorkerID: "aggregation-slow-worker", LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond,
		MaxConcurrent: 3, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		found bool
		err   error
	}
	rootDone := make(chan result, 1)
	go func() { found, err := w.RunOnce(ctx); rootDone <- result{found: found, err: err} }()
	select {
	case <-model.rootStarted:
	case outcome := <-rootDone:
		var state, reason string
		_ = f.store.pool.QueryRow(ctx, `SELECT state,failure_reason FROM conversation_message_routes WHERE message_id=$1`, root.ID).Scan(&state, &reason)
		t.Fatalf("root worker exited before classification: found=%t err=%v state=%s reason=%s", outcome.found, outcome.err, state, reason)
	case <-ctx.Done():
		t.Fatal("root classifier did not start")
	}
	child := acceptAggregationMessage(t, f, "route-slow-child", "Include the conclusions")
	model.childID = child.ID
	chat := acceptAggregationMessage(t, f, "route-slow-chat", "Hello")
	model.chatID = chat.ID
	independent := acceptAggregationMessage(t, f, "route-slow-independent-inside", "Start another task separately")
	model.independentID = independent.ID
	var deadline, childAccepted, chatAccepted, independentAccepted time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT root.aggregation_deadline,child.aggregation_started_at,chat.aggregation_started_at,other.aggregation_started_at
FROM conversation_message_routes root,conversation_message_routes child,conversation_message_routes chat,conversation_message_routes other
WHERE root.message_id=$1 AND child.message_id=$2 AND chat.message_id=$3 AND other.message_id=$4`,
		root.ID, child.ID, chat.ID, independent.ID).Scan(&deadline, &childAccepted, &chatAccepted, &independentAccepted); err != nil ||
		childAccepted.After(deadline) || chatAccepted.After(deadline) || independentAccepted.After(deadline) {
		t.Fatalf("messages were not accepted in the root window: deadline=%s supplement=%s chat=%s independent=%s err=%v",
			deadline, childAccepted, chatAccepted, independentAccepted, err)
	}
	childDone := make(chan result, 1)
	go func() { found, err := w.RunOnce(ctx); childDone <- result{found: found, err: err} }()
	select {
	case <-model.childStarted:
	case outcome := <-childDone:
		t.Fatalf("supplement worker exited before classification: found=%t err=%v", outcome.found, outcome.err)
	case <-ctx.Done():
		t.Fatal("supplement classifier did not start")
	}
	if found, err := w.RunOnce(ctx); err != nil || !found {
		t.Fatalf("ordinary conversation was held behind aggregation: found=%t err=%v", found, err)
	}
	var replyCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE conversation_id=$1 AND role='assistant' AND content='Hello.'`, f.conversation.ID).Scan(&replyCount); err != nil || replyCount != 1 {
		t.Fatalf("ordinary conversation was not answered while the other classifiers were blocked: replies=%d err=%v", replyCount, err)
	}
	if found, err := w.RunOnce(ctx); err != nil || !found {
		t.Fatalf("in-window independent request was held behind aggregation: found=%t err=%v", found, err)
	}
	var taskCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND objective=$2`,
		f.conversation.ID, independent.Content).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("in-window independent request did not create its own Task: tasks=%d err=%v", taskCount, err)
	}
	if delay := time.Until(deadline) + 10*time.Millisecond; delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			t.Fatal("deadline did not elapse")
		}
	}
	close(model.rootRelease)
	if outcome := <-rootDone; outcome.err != nil || !outcome.found {
		t.Fatalf("slow root classifier: %+v", outcome)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id=$1 AND task_id IS NOT NULL`, root.ID).Scan(&taskCount); err != nil || taskCount != 0 {
		t.Fatalf("root started before its in-window supplement was classified: tasks=%d err=%v", taskCount, err)
	}
	late := acceptAggregationMessage(t, f, "route-slow-independent", "Start another task from scratch")
	if found, err := w.RunOnce(ctx); err != nil || !found {
		t.Fatalf("independent request was held behind aggregation: found=%t err=%v", found, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND objective=$2`,
		f.conversation.ID, late.Content).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("independent request did not create its own Task: tasks=%d err=%v", taskCount, err)
	}
	if found, err := w.RunOnce(ctx); err != nil || found {
		t.Fatalf("root finalized while its supplement classification was still in flight: found=%t err=%v", found, err)
	}
	close(model.childRelease)
	if outcome := <-childDone; outcome.err != nil || !outcome.found {
		t.Fatalf("slow supplement classifier: %+v", outcome)
	}
	for i := 0; i < 3; i++ {
		if found, err := w.RunOnce(ctx); err != nil {
			t.Fatalf("finalize slow aggregation: %v", err)
		} else if !found {
			break
		}
	}
	var objective, rootTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT t.id,t.objective FROM tasks t
JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, root.ID).Scan(&rootTaskID, &objective); err != nil {
		t.Fatalf("root Task was not created after supplement classification: %v", err)
	}
	if !strings.Contains(objective, root.Content) || !strings.Contains(objective, child.Content) || strings.Contains(objective, late.Content) || strings.Contains(objective, independent.Content) {
		t.Fatalf("root Task did not preserve its in-window supplement independently: %q", objective)
	}
	page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var childProjected bool
	for _, message := range page.Items {
		if message.ID == child.ID {
			childProjected = message.RelatedTaskID == rootTaskID && message.DuplicateOfMessageID == root.ID
		}
	}
	if !childProjected {
		t.Fatal("in-window supplement did not project the root Task")
	}
}

func TestPostgresConversationRouteAggregationUsesWindowAndExplicitMessageSelection(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-aggregate-root", "Root work", app.ConversationRoutingInput{})
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	started, err := f.store.BeginConversationRouteAggregation(ctx, root, rootSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew})
	if err != nil || !started {
		t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
	}
	var state string
	var window time.Duration
	var attempt int64
	if err := f.store.pool.QueryRow(ctx, `SELECT aggregation_state,aggregation_deadline-aggregation_started_at,route_attempt
FROM conversation_message_routes WHERE message_id=$1`, root.Message.ID).Scan(&state, &window, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != "waiting" || window != app.ConversationRouteAggregationWindow || attempt != root.RouteAttempt+1 {
		t.Fatalf("root aggregation deadline/attempt drifted: state=%s window=%s attempt=%d", state, window, attempt)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_deadline=clock_timestamp()+interval '10 seconds'
WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-no-early-claim", time.Minute); err != nil || found {
		t.Fatalf("waiting root was claimed before its fixed deadline: found=%t err=%v", found, err)
	}

	joined := make([]app.ConversationRouteClaim, 0, 2)
	for i, content := range []string{"Same-work supplement one", "Same-work supplement two"} {
		child := f.receive(t, []string{"route-aggregate-child-1", "route-aggregate-child-2"}[i], content, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		targetOK := app.ValidConversationRouteJoinTarget(root.Message.ID, snapshot.Context.RequestCandidates)
		if !targetOK {
			t.Fatalf("within-window same-scope child was not joinable: %+v", snapshot.Context.RequestCandidates)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, child, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
		}); err != nil {
			t.Fatalf("link same-work child %d: %v", i, err)
		}
		joined = append(joined, child)
	}
	// Move only the durable deadline forward to the current instant after the
	// two synthetic supplements were accepted inside the original fixed window.
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_deadline=clock_timestamp()+interval '5 milliseconds'
WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	finalRoot, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-finalize", time.Minute)
	if err != nil || !found || finalRoot.Message.ID != root.Message.ID || finalRoot.RouteAttempt != root.RouteAttempt+1 {
		t.Fatalf("waiting root was not recoverable after deadline: claim=%+v found=%t err=%v", finalRoot, found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, finalRoot)
	if err != nil || len(snapshot.Context.AggregationCandidates) != 2 {
		t.Fatalf("same-scope linked candidates missing after restart-style re-claim: candidates=%+v err=%v",
			snapshot.Context.AggregationCandidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, finalRoot, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, AggregateMessageIDs: []string{joined[0].Message.ID},
	}); err != nil {
		t.Fatal(err)
	}
	var taskID, objective string
	if err := f.store.pool.QueryRow(ctx, `SELECT id,objective FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskID, &objective); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(objective, "Root work") || !strings.Contains(objective, "Same-work supplement one") ||
		strings.Contains(objective, "Same-work supplement two") {
		t.Fatalf("Task body did not follow the model's explicit selection: %q", objective)
	}
	for i, child := range joined {
		var duplicateOf, routeState string
		var delivery conversation.DeliveryStatus
		if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(r.duplicate_of_message_id,''),r.state,m.delivery_status
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, child.Message.ID).
			Scan(&duplicateOf, &routeState, &delivery); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if duplicateOf != root.Message.ID || routeState != "routed" || delivery != conversation.DeliveryApplied {
				t.Fatalf("selected supplement projection changed: duplicate=%q route=%q delivery=%q", duplicateOf, routeState, delivery)
			}
		} else if duplicateOf != "" || routeState != "pending" || delivery != conversation.DeliveryPending {
			t.Fatalf("unselected request did not resume independently: duplicate=%q route=%q delivery=%q", duplicateOf, routeState, delivery)
		}
	}
	page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var selectedFound bool
	for _, message := range page.Items {
		if message.ID == joined[0].Message.ID {
			selectedFound = true
			if message.DuplicateOfMessageID != root.Message.ID || message.RelatedTaskID != taskID ||
				message.DeliveryStatus != conversation.DeliveryApplied {
				t.Fatalf("selected supplement did not project the root Task: %+v", message)
			}
		}
	}
	if !selectedFound {
		t.Fatal("selected supplement disappeared from the conversation")
	}
	resumed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-unselected", time.Minute)
	if err != nil || !found || resumed.Message.ID != joined[1].Message.ID {
		t.Fatalf("unselected message was not independently routable: claim=%+v found=%t err=%v", resumed, found, err)
	}
}

func TestPostgresConversationRouteStopFencesOpenRootClassifierAndFinalizeRace(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-stop-open-root", "Work that must not start", app.ConversationRoutingInput{})
	if err := f.store.pool.QueryRow(ctx, `UPDATE conversation_message_routes SET aggregation_deadline=clock_timestamp()+interval '10 seconds'
WHERE message_id=$1 RETURNING aggregation_deadline`, root.Message.ID).Scan(&root.AggregationDeadline); err != nil {
		t.Fatal(err)
	}
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	child := f.receive(t, "route-stop-open-child", "Supplement that must stop with its root", app.ConversationRoutingInput{})
	childSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
	if err != nil || !app.ValidConversationRouteJoinTarget(root.Message.ID, childSnapshot.Context.RequestCandidates) {
		t.Fatalf("same-scope supplement could not link to open root: candidates=%+v err=%v", childSnapshot.Context.RequestCandidates, err)
	}
	childSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, child, childSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatal(err)
	}
	stop := f.receive(t, "route-stop-open-control", "Stop the request before it starts", app.ConversationRoutingInput{})
	stopSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, stop)
	if err != nil {
		t.Fatal(err)
	}
	if !app.ValidConversationRoutePendingRootStop(root.Message.ID, stopSnapshot.Context.RequestCandidates) {
		t.Fatalf("open first-classification root was not exposed as a stoppable target: %+v", stopSnapshot.Context.RequestCandidates)
	}

	finalize := make(chan error, 1)
	err = f.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, stop); err != nil {
			return err
		}
		if err := requireConversationRouteScope(ctx, tx, stop, identity.ScopeTaskControl); err != nil {
			return err
		}
		current, err := loadConversationRouteSnapshotInTx(ctx, tx, stop, true)
		if err != nil {
			return err
		}
		if err := cancelPendingConversationRouteRoot(ctx, tx, stop, current, root.Message.ID); err != nil {
			return err
		}
		go func() {
			finalize <- f.store.ApplyConversationRoute(ctx, root, rootSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew})
		}()
		time.Sleep(10 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-finalize; domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale first classifier finalized a cancelled root: %v", err)
	}
	var taskCount int
	var routeState, aggregationState, delivery string
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.aggregation_state,m.delivery_status FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, root.Message.ID).Scan(&routeState, &aggregationState, &delivery); err != nil {
		t.Fatal(err)
	}
	if taskCount != 0 || routeState != "routed" || aggregationState != "cancelled" || delivery != string(conversation.DeliveryApplied) {
		t.Fatalf("pending-root stop was not durably projected: tasks=%d route=%s aggregation=%s delivery=%s",
			taskCount, routeState, aggregationState, delivery)
	}
	var stopState, stopDelivery string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,m.delivery_status FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, stop.Message.ID).Scan(&stopState, &stopDelivery); err != nil {
		t.Fatal(err)
	}
	if stopState != "routed" || stopDelivery != string(conversation.DeliveryApplied) {
		t.Fatalf("stop message did not receive a truthful terminal projection: route=%s delivery=%s", stopState, stopDelivery)
	}
	var childDuplicate, childRoute, childAggregation, childDelivery, childTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(r.duplicate_of_message_id,''),r.state,r.aggregation_state,m.delivery_status,COALESCE(m.task_id,'')
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, child.Message.ID).
		Scan(&childDuplicate, &childRoute, &childAggregation, &childDelivery, &childTaskID); err != nil {
		t.Fatal(err)
	}
	if childDuplicate != root.Message.ID || childRoute != "routed" || childAggregation != "cancelled" ||
		childDelivery != string(conversation.DeliveryApplied) || childTaskID != "" {
		t.Fatalf("linked supplement lost its terminal cancellation projection: duplicate=%s route=%s aggregation=%s delivery=%s task=%s",
			childDuplicate, childRoute, childAggregation, childDelivery, childTaskID)
	}
	var rootCancellationReplies int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE owner_id=$1 AND conversation_id=$2 AND role='assistant' AND content='这条请求已在任务创建前取消；未创建 Task，也未执行。'`,
		f.ownerID, f.conversation.ID).Scan(&rootCancellationReplies); err != nil {
		t.Fatal(err)
	}
	if rootCancellationReplies != 1 {
		t.Fatalf("root cancellation result was not visible exactly once: %d", rootCancellationReplies)
	}
}

func TestPostgresConversationRouteStopRequiresControlScope(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	installationID := createTestClientInstallation(t, f.store.pool, f.ownerID, nil)
	token := clientTestToken(t, f.ownerID, installationID, identity.KindClient, nil)
	token.Scopes = []identity.Scope{identity.ScopeScheduleRead, identity.ScopeScheduleManage}
	if _, err := f.store.CreateToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	f.authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: token.ID,
		ClientInstallationID: installationID, Version: 1, ExpiresAt: &token.ExpiresAt,
		Scopes: []string{string(identity.ScopeScheduleRead), string(identity.ScopeScheduleManage)}}
	root := f.receive(t, "route-stop-no-control-root", "Work still waiting", app.ConversationRoutingInput{})
	if err := f.store.pool.QueryRow(ctx, `UPDATE conversation_message_routes SET aggregation_deadline=clock_timestamp()+interval '10 seconds'
WHERE message_id=$1 RETURNING aggregation_deadline`, root.Message.ID).Scan(&root.AggregationDeadline); err != nil {
		t.Fatal(err)
	}
	stop := f.receive(t, "route-stop-no-control", "Stop that work", app.ConversationRoutingInput{})
	if err := f.store.ApplyConversationRoute(ctx, stop, app.ConversationRouteSnapshot{}, app.ConversationRouteDecision{
		Action: app.ConversationRouteStop, TargetMessageID: root.Message.ID,
	}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stop using a schedule-only credential = %v, want unauthenticated", err)
	}
	var state, aggregation, delivery string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.aggregation_state,m.delivery_status
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, root.Message.ID).
		Scan(&state, &aggregation, &delivery); err != nil {
		t.Fatal(err)
	}
	if state != "processing" || aggregation != "open" || delivery != string(conversation.DeliveryPending) {
		t.Fatalf("unauthorized stop changed root: state=%s aggregation=%s delivery=%s", state, aggregation, delivery)
	}
}

func TestPostgresConversationRouteStopAfterWindowBeforeTaskCreation(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-stop-after-window-root", "Work that must not start", app.ConversationRoutingInput{})
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET
aggregation_started_at=clock_timestamp()-interval '1 second',aggregation_deadline=clock_timestamp()-interval '650 milliseconds'
WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	stop := f.receive(t, "route-stop-after-window-control", "Stop that work", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, stop)
	if err != nil || !app.ValidConversationRoutePendingRootStop(root.Message.ID, snapshot.Context.RequestCandidates) {
		t.Fatalf("taskless root was not stoppable after the receipt window: candidates=%+v err=%v", snapshot.Context.RequestCandidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, stop, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteStop, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatal(err)
	}
	var state, aggregation string
	var tasks int
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.aggregation_state,
(SELECT count(*) FROM tasks WHERE conversation_id=$2) FROM conversation_message_routes r WHERE r.message_id=$1`,
		root.Message.ID, f.conversation.ID).Scan(&state, &aggregation, &tasks); err != nil || state != "routed" || aggregation != "cancelled" || tasks != 0 {
		t.Fatalf("late stop did not prevent Task creation: state=%s aggregation=%s tasks=%d err=%v", state, aggregation, tasks, err)
	}
}

func TestPostgresConversationRouteAggregationUnresolvedCandidateFailsSafely(t *testing.T) {
	for _, mode := range []string{"failed", "clarification", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			root := f.receive(t, "route-unresolved-root", "Prepare a report", app.ConversationRoutingInput{})
			rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			rootSnapshot.Context.ModelID = "p1234567"
			if started, err := f.store.BeginConversationRouteAggregation(ctx, root, rootSnapshot,
				app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
				t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
			}
			child := f.receive(t, "route-unresolved-child", "Please include this detail", app.ConversationRoutingInput{})
			switch mode {
			case "failed":
				if err := f.store.FailConversationRoute(ctx, child, "model_unavailable"); err != nil {
					t.Fatal(err)
				}
			case "clarification":
				snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
				if err != nil {
					t.Fatal(err)
				}
				snapshot.Context.ModelID = "p1234567"
				if err := f.store.ApplyConversationRoute(ctx, child, snapshot, app.ConversationRouteDecision{
					Action: app.ConversationRouteClarify, Question: "Which report should this detail be added to?",
				}); err != nil {
					t.Fatal(err)
				}
			}
			ageSeconds := 2
			if mode == "timeout" {
				ageSeconds = int(app.ConversationRouteAggregationMaxWait/time.Second) + 2
			}
			if _, err := f.store.pool.Exec(ctx, `WITH original AS (SELECT clock_timestamp()-($2::int*interval '1 second') AS started)
UPDATE conversation_message_routes SET aggregation_started_at=original.started,
aggregation_deadline=original.started+interval '350 milliseconds' FROM original WHERE message_id=$1`, root.Message.ID, ageSeconds); err != nil {
				t.Fatal(err)
			}
			if err := f.store.pool.QueryRow(ctx, `UPDATE conversation_message_routes child SET
aggregation_started_at=root.aggregation_started_at+interval '100 milliseconds',
aggregation_deadline=root.aggregation_started_at+interval '450 milliseconds'
FROM conversation_message_routes root WHERE child.message_id=$1 AND root.message_id=$2
RETURNING child.aggregation_started_at,child.aggregation_deadline`, child.Message.ID, root.Message.ID).
				Scan(&child.AggregationStartedAt, &child.AggregationDeadline); err != nil {
				t.Fatal(err)
			}
			w, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{Queue: f.store, Actions: f.store,
				Models: &slowAggregationRouteModel{}, WorkerID: "aggregation-unresolved-worker", LeaseDuration: time.Minute,
				PollInterval: 10 * time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			if found, err := w.RunOnce(ctx); err != nil || !found {
				t.Fatalf("root was left waiting for an unresolved candidate: found=%t err=%v", found, err)
			}
			var state, reason string
			var taskCount int
			if err := f.store.pool.QueryRow(ctx, `SELECT state,failure_reason,
(SELECT count(*) FROM tasks WHERE conversation_id=$2) FROM conversation_message_routes WHERE message_id=$1`,
				root.Message.ID, f.conversation.ID).Scan(&state, &reason, &taskCount); err != nil || taskCount != 0 {
				t.Fatalf("unresolved candidate led to an unsafe root result: state=%s reason=%s tasks=%d err=%v", state, reason, taskCount, err)
			}
			if mode == "clarification" {
				var clarification int
				if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant'
AND content LIKE '%消息关系尚不明确%'`, f.conversation.ID).Scan(&clarification); err != nil ||
					state != "waiting_user" || reason != "" || clarification != 1 {
					t.Fatalf("root did not present a truthful clarification: state=%s reason=%s messages=%d err=%v", state, reason, clarification, err)
				}
				return
			}
			if state != "failed" || reason != "routing_context_unavailable" {
				t.Fatalf("unresolved candidate did not expose a retryable route failure: state=%s reason=%s", state, reason)
			}
			page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			var visibleFailure bool
			for _, message := range page.Items {
				if message.ID == root.Message.ID {
					visibleFailure = message.RoutingStatus == conversation.RoutingFailed &&
						string(message.RoutingFailureReason) == "routing_context_unavailable" && message.TaskID == ""
				}
			}
			if !visibleFailure {
				t.Fatal("retryable aggregation failure was not visible on the original message")
			}
			if mode == "timeout" {
				snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
				if err != nil || !app.ValidConversationRouteJoinTarget(root.Message.ID, snapshot.Context.RequestCandidates) {
					t.Fatalf("late child could not associate with the failed root: candidates=%+v err=%v", snapshot.Context.RequestCandidates, err)
				}
				snapshot.Context.ModelID = "p1234567"
				if err := f.store.ApplyConversationRoute(ctx, child, snapshot, app.ConversationRouteDecision{
					Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
				}); err != nil {
					t.Fatal(err)
				}
			}
			principal := identity.Principal{UserID: f.ownerID, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
			if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, root.Message.ID,
				f.instanceID, "route-unresolved-retry-"+mode); err != nil {
				t.Fatalf("failed root could not be explicitly retried: %v", err)
			}
			if mode == "timeout" {
				retried, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-retry-worker", time.Minute)
				if err != nil || !found || retried.Message.ID != root.Message.ID {
					t.Fatalf("root retry was not claimable after the child resolved: claim=%+v found=%t err=%v", retried, found, err)
				}
				snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, retried)
				if err != nil || len(snapshot.Context.AggregationCandidates) != 1 {
					t.Fatalf("root retry lost the late child: candidates=%+v err=%v", snapshot.Context.AggregationCandidates, err)
				}
				snapshot.Context.ModelID = "p1234567"
				if err := f.store.ApplyConversationRoute(ctx, retried, snapshot, app.ConversationRouteDecision{
					Action: app.ConversationRouteNew, AggregateMessageIDs: []string{child.Message.ID},
				}); err != nil {
					t.Fatal(err)
				}
				var objective string
				if err := f.store.pool.QueryRow(ctx, `SELECT objective FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&objective); err != nil ||
					!strings.Contains(objective, child.Message.Content) {
					t.Fatalf("retried root did not include the resolved child: objective=%q err=%v", objective, err)
				}
			}
		})
	}
}

func TestPostgresConversationRouteAggregationIgnoresDifferentScopeAndAuthorization(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-isolated-root", "Prepare a report", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if started, err := f.store.BeginConversationRouteAggregation(ctx, root, snapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
		t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
	}
	otherScope := f.receive(t, "route-isolated-other-host", "Prepare a report", app.ConversationRoutingInput{HostID: "h1234567"})
	tokenID := mustID(t, domain.NewTokenID)
	_, secretHash, err := identity.NewCredential(identity.KindSession, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: f.ownerID, Kind: identity.KindSession,
		SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	f.authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}
	otherAuthorization := f.receive(t, "route-isolated-other-auth", "Prepare a report", app.ConversationRoutingInput{})
	if otherScope.AggregationStartedAt.After(root.AggregationDeadline) || otherAuthorization.AggregationStartedAt.After(root.AggregationDeadline) {
		t.Fatal("isolation candidates did not arrive inside the root window")
	}
	if delay := time.Until(root.AggregationDeadline) + 10*time.Millisecond; delay > 0 {
		time.Sleep(delay)
	}
	claimed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-isolation-worker", time.Minute)
	if err != nil || !found || claimed.Message.ID != root.Message.ID {
		t.Fatalf("other execution scopes or authorization delayed the root: claim=%+v found=%t err=%v", claimed, found, err)
	}
	finalSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claimed)
	if err != nil || finalSnapshot.AggregationUnresolved || len(finalSnapshot.Context.AggregationCandidates) != 0 {
		t.Fatalf("other scopes entered the aggregation: snapshot=%+v err=%v", finalSnapshot, err)
	}
}

func TestPostgresConversationRouteAggregationRechecksResolvedCandidateBeforeFailure(t *testing.T) {
	for _, mode := range []string{"reply", "independent"} {
		t.Run(mode, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			root := f.receive(t, "route-resolution-race-root", "Prepare a report", app.ConversationRoutingInput{})
			rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			if started, err := f.store.BeginConversationRouteAggregation(ctx, root, rootSnapshot,
				app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
				t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
			}
			content := "Hello"
			if mode == "independent" {
				content = "Start another task separately"
			}
			child := f.receive(t, "route-resolution-race-child", content, app.ConversationRoutingInput{})
			if _, err := f.store.pool.Exec(ctx, `WITH original AS (SELECT clock_timestamp()-interval '32 seconds' AS started)
UPDATE conversation_message_routes SET aggregation_started_at=original.started,
aggregation_deadline=original.started+interval '350 milliseconds' FROM original WHERE message_id=$1`, root.Message.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.store.pool.QueryRow(ctx, `UPDATE conversation_message_routes child SET
aggregation_started_at=root.aggregation_started_at+interval '100 milliseconds',
aggregation_deadline=root.aggregation_started_at+interval '450 milliseconds'
FROM conversation_message_routes root WHERE child.message_id=$1 AND root.message_id=$2
RETURNING child.aggregation_started_at,child.aggregation_deadline`, child.Message.ID, root.Message.ID).
				Scan(&child.AggregationStartedAt, &child.AggregationDeadline); err != nil {
				t.Fatal(err)
			}
			claimed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-resolution-race", time.Minute)
			if err != nil || !found || claimed.Message.ID != root.Message.ID {
				t.Fatalf("claim timed-out root: claim=%+v found=%t err=%v", claimed, found, err)
			}
			actions := &resolvingAggregationActions{Store: f.store, resolveCandidate: func(ctx context.Context) error {
				snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
				if err != nil {
					return err
				}
				snapshot.Context.ModelID = "p1234567"
				decision := app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "Hello."}
				if mode == "independent" {
					decision = app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}
				}
				return f.store.ApplyConversationRoute(ctx, child, snapshot, decision)
			}}
			w, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
				Queue: &singleAggregationClaimQueue{claim: claimed}, Actions: actions, Models: &slowAggregationRouteModel{},
				WorkerID: "aggregate-resolution-race", LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond,
				MaxConcurrent: 1, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			if found, err := w.RunOnce(ctx); err != nil || !found {
				t.Fatalf("settle timed-out root: found=%t err=%v", found, err)
			}
			var state, reason string
			if err := f.store.pool.QueryRow(ctx, `SELECT state,failure_reason FROM conversation_message_routes WHERE message_id=$1`,
				root.Message.ID).Scan(&state, &reason); err != nil || state != "pending" || reason != "" {
				t.Fatalf("resolved %s incorrectly failed the original request: state=%s reason=%s err=%v", mode, state, reason, err)
			}
			resumed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-resumed", time.Minute)
			if err != nil || !found || resumed.Message.ID != root.Message.ID {
				t.Fatalf("original request did not resume after %s: found=%t err=%v", mode, found, err)
			}
			finalSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, resumed)
			if err != nil || finalSnapshot.AggregationUnresolved {
				t.Fatalf("resolved %s remained a blocking candidate: snapshot=%+v err=%v", mode, finalSnapshot, err)
			}
			finalSnapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, resumed, finalSnapshot,
				app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatalf("resume original request after %s: %v", mode, err)
			}
			var rootTask, childTask string
			if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(root.task_id,''),COALESCE(child.task_id,'')
FROM conversation_messages root,conversation_messages child WHERE root.id=$1 AND child.id=$2`,
				root.Message.ID, child.Message.ID).Scan(&rootTask, &childTask); err != nil || rootTask == "" ||
				(mode == "reply" && childTask != "") || (mode == "independent" && (childTask == "" || childTask == rootTask)) {
				t.Fatalf("resolved %s did not remain separate: root=%q child=%q err=%v", mode, rootTask, childTask, err)
			}
		})
	}
}

func TestPostgresConversationRouteAggregationDefersReceiptCommittedAfterClaim(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-late-commit-root", "Prepare a report", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if started, err := f.store.BeginConversationRouteAggregation(ctx, root, snapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
		t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
	}
	if _, err := f.store.pool.Exec(ctx, `WITH original AS (SELECT clock_timestamp()-interval '2 seconds' AS started)
UPDATE conversation_message_routes SET aggregation_started_at=original.started,
aggregation_deadline=original.started+interval '350 milliseconds' FROM original WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-late-commit-worker", time.Minute)
	if err != nil || !found || claimed.Message.ID != root.Message.ID {
		t.Fatalf("claim root before late receipt became visible: claim=%+v found=%t err=%v", claimed, found, err)
	}
	child := f.receive(t, "route-late-commit-child", "Include the conclusions", app.ConversationRoutingInput{})
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes child SET
aggregation_started_at=root.aggregation_started_at+interval '100 milliseconds',
aggregation_deadline=root.aggregation_started_at+interval '450 milliseconds'
FROM conversation_message_routes root WHERE child.message_id=$1 AND root.message_id=$2`, child.Message.ID, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	w, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{Queue: &singleAggregationClaimQueue{claim: claimed},
		Actions: f.store, Models: &slowAggregationRouteModel{}, WorkerID: "aggregate-late-commit-worker", LeaseDuration: time.Minute,
		PollInterval: 10 * time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	if found, err := w.RunOnce(ctx); err != nil || !found {
		t.Fatalf("late in-window receipt was not safely deferred: found=%t err=%v", found, err)
	}
	var state, reason string
	var deferred bool
	var tasks int
	if err := f.store.pool.QueryRow(ctx, `SELECT state,failure_reason,available_after>clock_timestamp(),
(SELECT count(*) FROM tasks WHERE conversation_id=$2) FROM conversation_message_routes WHERE message_id=$1`,
		root.Message.ID, f.conversation.ID).Scan(&state, &reason, &deferred, &tasks); err != nil ||
		state != "pending" || reason != "" || !deferred || tasks != 0 {
		t.Fatalf("late receipt prematurely failed or started root: state=%s reason=%s deferred=%t tasks=%d err=%v", state, reason, deferred, tasks, err)
	}
}

func TestPostgresConversationRouteAggregationSettlesAtCapacity(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	root := f.receive(t, "route-capacity-root", "Prepare the report", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if started, err := f.store.BeginConversationRouteAggregation(ctx, root, snapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
		t.Fatalf("begin root aggregation: started=%t err=%v", started, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_deadline=clock_timestamp()+interval '10 seconds'
WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	children := make([]app.ConversationRouteClaim, 0, app.MaxConversationRouteAggregateMessages)
	for i := 0; i < app.MaxConversationRouteAggregateMessages; i++ {
		child := f.receive(t, fmt.Sprintf("route-capacity-child-%02d", i), fmt.Sprintf("Include section %d", i), app.ConversationRoutingInput{})
		childSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		childSnapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, child, childSnapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
		}); err != nil {
			t.Fatalf("link supplement %d: %v", i, err)
		}
		children = append(children, child)
	}
	readyRoot, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-capacity-worker", time.Minute)
	if err != nil || !found || readyRoot.Message.ID != root.Message.ID || !time.Now().Before(readyRoot.AggregationDeadline) {
		t.Fatalf("full aggregation was not claimable before its deadline: claim=%+v found=%t err=%v", readyRoot, found, err)
	}
	ninth := f.receive(t, "route-capacity-child-08", "Include section 8", app.ConversationRoutingInput{})
	ninthSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, ninth)
	if err != nil {
		t.Fatal(err)
	}
	ninthSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, ninth, ninthSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, readyRoot)
	if err != nil || len(finalSnapshot.Context.AggregationCandidates) != len(children) {
		t.Fatalf("aggregation capacity was not enforced: candidates=%+v err=%v", finalSnapshot.Context.AggregationCandidates, err)
	}
	ids := make([]string, 0, len(children))
	for _, child := range children {
		ids = append(ids, child.Message.ID)
	}
	finalSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, readyRoot, finalSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, AggregateMessageIDs: ids,
	}); err != nil {
		t.Fatal(err)
	}
	resumed, found, err := f.store.ClaimConversationRoute(ctx, "aggregate-overflow-worker", time.Minute)
	if err != nil || !found || resumed.Message.ID != ninth.Message.ID {
		t.Fatalf("overflow supplement was lost: claim=%+v found=%t err=%v", resumed, found, err)
	}
	resumedSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, resumed)
	if err != nil {
		t.Fatal(err)
	}
	resumedSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, resumed, resumedSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID,
	}); err != nil {
		t.Fatalf("overflow supplement could not follow the existing Task revision path: %v", err)
	}
	var tasks, revisions int
	if err := f.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE conversation_id=$1),
(SELECT count(*) FROM task_revision_intents WHERE message_id=$2)`, f.conversation.ID, ninth.Message.ID).
		Scan(&tasks, &revisions); err != nil || tasks != 1 || revisions != 1 {
		t.Fatalf("overflow supplement did not revise the one Task: tasks=%d revisions=%d err=%v", tasks, revisions, err)
	}
}

func TestPostgresConversationRouteStopCancelsAttachedRootAndRetainsAttachmentMetadata(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	clientID := "route-stop-attached-root"
	clientAttachmentID := "client-attached-001"
	attachmentID := "pa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	uploadID := "pu_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	input := app.ConversationRoutingInput{ModelID: "p1234567", ClientAttachmentIDs: []string{clientAttachmentID},
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}}
	requestHash := sha256.Sum256([]byte(clientID))
	message := conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
		ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "Process the attached document only if the request starts."}
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{Message: message,
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: requestHash[:], Input: input, Authorization: f.authorization})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO personal_attachment_uploads
(service_instance_id,owner_id,upload_id,attachment_id,object_type,object_id,base_revision,size_bytes,sha256,media_type,
 request_hash,idempotency_key,state,reservation_id,commit_idempotency_key,result_revision,change_sequence,expires_at)
VALUES ($1::uuid,$2,$3,$4,'library_item','synthetic-attachment-metadata',0,0,$5,'application/octet-stream',
 decode(repeat('01',32),'hex'),'synthetic-upload-key','committed','synthetic-reservation','synthetic-commit',1,1,clock_timestamp()+interval '1 day')`,
		f.instanceID, f.ownerID, uploadID, attachmentID, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO conversation_message_attachments
(service_instance_id,owner_id,conversation_id,message_id,upload_id,attachment_id) VALUES ($1::uuid,$2,$3,$4,$5,$6)`,
		f.instanceID, f.ownerID, f.conversation.ID, message.ID, uploadID, attachmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO conversation_attachment_slots
(message_id,client_attachment_id,service_instance_id,owner_id,attachment_id) VALUES ($1,$2,$3::uuid,$4,$5)`,
		message.ID, clientAttachmentID, f.instanceID, f.ownerID, attachmentID); err != nil {
		t.Fatal(err)
	}
	root, found, err := f.store.ClaimConversationRoute(ctx, "attached-root-worker", time.Minute)
	if err != nil || !found || root.Message.ID != message.ID || len(root.Input.ClientAttachmentIDs) != 1 {
		t.Fatalf("attached root was not claimed with its slot: claim=%+v found=%t err=%v", root, found, err)
	}
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	if started, err := f.store.BeginConversationRouteAggregation(ctx, root, rootSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || started {
		t.Fatalf("attachment request entered text-only aggregation: started=%t err=%v", started, err)
	}
	stop := f.receive(t, "route-stop-attached-control", "Stop the attached request", app.ConversationRoutingInput{})
	stopSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, stop)
	if err != nil {
		t.Fatal(err)
	}
	if !app.ValidConversationRoutePendingRootStop(root.Message.ID, stopSnapshot.Context.RequestCandidates) {
		t.Fatalf("attached root could not be explicitly stopped in the live window: candidates=%+v", stopSnapshot.Context.RequestCandidates)
	}
	decision := app.ParseConversationRouteDecisionForContext(`{"action":"stop","task_id":"","target_message_id":"`+root.Message.ID+`"}`, stopSnapshot.Context)
	if decision.Action != app.ConversationRouteStop || decision.TargetMessageID != root.Message.ID {
		t.Fatalf("model contract rejected attachment-free stop target: %+v", decision)
	}
	stopSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, stop, stopSnapshot, decision); err != nil {
		t.Fatal(err)
	}
	var taskCount, attachmentCount, slotCount int
	var routeState, aggregationState, delivery string
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,r.aggregation_state,m.delivery_status FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, message.ID).Scan(&routeState, &aggregationState, &delivery); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_attachments WHERE message_id=$1`, message.ID).Scan(&attachmentCount); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_attachment_slots WHERE message_id=$1`, message.ID).Scan(&slotCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 0 || routeState != "routed" || aggregationState != "cancelled" ||
		delivery != string(conversation.DeliveryApplied) || attachmentCount != 1 || slotCount != 1 {
		t.Fatalf("attached request was not stopped without data loss: tasks=%d route=%s aggregation=%s delivery=%s attachments=%d slots=%d",
			taskCount, routeState, aggregationState, delivery, attachmentCount, slotCount)
	}
}
