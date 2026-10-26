package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestPostgresNotificationConsumptionDeduplicatesAndScopesOwners(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)

	completed := newTestTaskSubmission(t, owner.ID, hostValue.ID, "notification-completed", "private task input")
	created, err := store.SubmitTask(ctx, completed)
	if err != nil {
		t.Fatal(err)
	}
	completedClaim, found, err := store.ClaimNextTask(ctx, "notification-worker", time.Minute)
	if err != nil || !found || completedClaim.ID != created.Task.ID {
		t.Fatalf("claim completed task = %#v, found=%t, err=%v", completedClaim, found, err)
	}
	completedStep := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: completedClaim.ID, LeaseOwner: completedClaim.Lease.Owner, LeaseVersion: completedClaim.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: completedStep, Result: "private task result",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: completedStep, TaskID: completedClaim.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Finished", Summary: "private task result"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed","result":"private task result"}`)},
	}); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	waiting := newTestTaskSubmission(t, owner.ID, hostValue.ID, "notification-approval", "private approval input")
	waitingCreated, err := store.SubmitTask(ctx, waiting)
	if err != nil {
		t.Fatal(err)
	}
	waitingClaim, found, err := store.ClaimNextTask(ctx, "notification-worker", time.Minute)
	if err != nil || !found || waitingClaim.ID != waitingCreated.Task.ID {
		t.Fatalf("claim approval task = %#v, found=%t, err=%v", waitingClaim, found, err)
	}
	pendingID := mustID(t, domain.NewPendingRequestID)
	stepID := mustID(t, domain.NewStepID)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: waitingClaim.ID, Version: 1, Kind: "approval", Title: "private approval title",
		Object: "private object", Content: "private approval content", Impact: "private impact",
		CallID: "notification-call", ToolName: "private_tool", RequestHash: hashBytes("notification approval"),
		RuntimeGeneration: 2, RuntimeInstanceID: "runtime-instance-2", ControlVersion: 3, Status: task.PendingRequestPending,
	}
	waitingPayload, err := json.Marshal(map[string]any{"detail": map[string]any{
		"pending_request_id": pendingID, "pending_request_version": 1, "pending_request_kind": "approval",
		"approval": map[string]string{"content": "private approval content"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: waitingClaim.ID, LeaseOwner: waitingClaim.Lease.Owner, LeaseVersion: waitingClaim.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: waitingClaim.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser, Title: request.Title, Summary: request.Content}},
		Event: task.Event{Type: "task.waiting_user", Payload: waitingPayload}, PendingRequest: request,
	}); err != nil {
		t.Fatalf("wait for approval: %v", err)
	}

	events, err := store.ListUnprocessedNotificationEvents(ctx, 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("unprocessed notification events = %#v, err=%v", events, err)
	}
	var completedEvent, waitingEvent notification.TaskEvent
	for _, event := range events {
		switch event.TaskID {
		case completedClaim.ID:
			completedEvent = event
		case waitingClaim.ID:
			waitingEvent = event
		}
	}
	if completedEvent.TaskID == "" || waitingEvent.TaskID == "" {
		t.Fatalf("completion or approval event missing: %#v", events)
	}

	consumeResults := make(chan struct {
		consumed bool
		err      error
	}, 2)
	effects := []notification.EventEffect{notificationEffectForTest(t, completedEvent), notificationEffectForTest(t, completedEvent)}
	var group sync.WaitGroup
	for _, effect := range effects {
		group.Add(1)
		go func(effect notification.EventEffect) {
			defer group.Done()
			consumed, err := store.ConsumeTaskNotificationEvent(ctx, completedEvent, effect.Notification, effect.ClosePending)
			consumeResults <- struct {
				consumed bool
				err      error
			}{consumed, err}
		}(effect)
	}
	group.Wait()
	close(consumeResults)
	consumedCount := 0
	for result := range consumeResults {
		if result.err != nil {
			t.Fatalf("concurrent completion consume: %v", result.err)
		}
		if result.consumed {
			consumedCount++
		}
	}
	if consumedCount != 1 {
		t.Fatalf("concurrent event consumers succeeded %d times, want one", consumedCount)
	}
	approvalEffect := notificationEffectForTest(t, waitingEvent)
	if !approvalEffect.Notification.Actionable {
		t.Fatal("current pending approval was not marked actionable")
	}
	if consumed, err := store.ConsumeTaskNotificationEvent(ctx, waitingEvent, approvalEffect.Notification, approvalEffect.ClosePending); err != nil || !consumed {
		t.Fatalf("consume approval event = consumed:%t err:%v", consumed, err)
	}

	page, err := store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("owner notifications = %#v, err=%v", page, err)
	}
	var completionNotification, approvalNotification notification.Notification
	for _, value := range page.Items {
		switch value.Kind {
		case notification.KindCompleted:
			completionNotification = value
		case notification.KindApprovalRequired:
			approvalNotification = value
		}
	}
	if completionNotification.ID == "" || approvalNotification.ID == "" || !approvalNotification.Actionable || approvalNotification.HandledAt != nil {
		t.Fatalf("completion/approval notifications = %#v", page.Items)
	}
	for _, value := range page.Items {
		if strings.Contains(value.Summary, "private") || strings.Contains(value.Summary, "tool") {
			t.Fatalf("notification leaked task content: %#v", value)
		}
	}
	var storedSummary string
	if err := store.pool.QueryRow(ctx, `SELECT summary FROM notifications WHERE id=$1`, completionNotification.ID).Scan(&storedSummary); err != nil || strings.Contains(storedSummary, "private") {
		t.Fatalf("stored notification summary %q, err=%v", storedSummary, err)
	}

	ownerPage, err := store.ListNotifications(ctx, otherOwner.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(ownerPage.Items) != 0 {
		t.Fatalf("other owner notifications = %#v, err=%v", ownerPage, err)
	}
	if _, err := store.MarkNotificationRead(ctx, otherOwner.ID, completionNotification.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner mark-read error = %v", err)
	}

	firstUnread, err := store.ListNotifications(ctx, owner.ID, true, app.PageRequest{Limit: 1})
	if err != nil || len(firstUnread.Items) != 1 || firstUnread.NextCursor == "" {
		t.Fatalf("first unread page = %#v, err=%v", firstUnread, err)
	}
	if _, err := store.MarkNotificationRead(ctx, owner.ID, firstUnread.Items[0].ID); err != nil {
		t.Fatalf("mark notification read: %v", err)
	}
	secondUnread, err := store.ListNotifications(ctx, owner.ID, true, app.PageRequest{Limit: 1, Cursor: firstUnread.NextCursor})
	if err != nil || len(secondUnread.Items) != 1 || secondUnread.Items[0].ID == firstUnread.Items[0].ID {
		t.Fatalf("remaining unread page = %#v, err=%v", secondUnread, err)
	}

	response := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "notification-approval-response",
		RequestHash: hashBytes("notification approval response"), ResourceID: waitingClaim.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, response, waitingClaim.ID, pendingID, 1, task.PendingDecisionApprove); err != nil {
		t.Fatalf("respond to approval: %v", err)
	}
	page, err = store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range page.Items {
		if value.ID == approvalNotification.ID && (value.Actionable || value.HandledAt == nil) {
			t.Fatalf("approval remained actionable or unhandled before worker consumed response: %#v", value)
		}
	}

	responseEvents, err := store.ListUnprocessedNotificationEvents(ctx, 100)
	if err != nil || len(responseEvents) != 1 || responseEvents[0].Type != "task.response" {
		t.Fatalf("response notification events = %#v, err=%v", responseEvents, err)
	}
	responseEffect, err := notification.DeriveEventEffect(responseEvents[0], "")
	if err != nil || !responseEffect.ClosePending {
		t.Fatalf("response effect = %#v, err=%v", responseEffect, err)
	}
	if consumed, err := store.ConsumeTaskNotificationEvent(ctx, responseEvents[0], responseEffect.Notification, responseEffect.ClosePending); err != nil || !consumed {
		t.Fatalf("consume response event = consumed:%t err:%v", consumed, err)
	}
	remaining, err := store.ListUnprocessedNotificationEvents(ctx, 100)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("unchanged notification poll returned events %#v, err=%v", remaining, err)
	}
	page, err = store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range page.Items {
		if value.ID == approvalNotification.ID && (value.Actionable || value.HandledAt == nil) {
			t.Fatalf("processed approval notification is not handled: %#v", value)
		}
	}
}

func notificationEffectForTest(t *testing.T, event notification.TaskEvent) notification.EventEffect {
	t.Helper()
	id := ""
	if event.Type == "task.result" || event.Type == "task.waiting_user" {
		var err error
		id, err = notification.NewID()
		if err != nil {
			t.Fatal(err)
		}
	}
	effect, err := notification.DeriveEventEffect(event, id)
	if err != nil {
		t.Fatal(err)
	}
	return effect
}

func TestPostgresNotificationCanceledConsumeCanBeRetried(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "notification-retry", "retry event")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "notification-retry-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim task: found=%t err=%v", found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "private failure detail",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "model", Status: task.StepFailed, Title: "Failed", Summary: "private failure detail"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed","detail":"private failure detail"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("unprocessed events = %#v, err=%v", events, err)
	}
	effect := notificationEffectForTest(t, events[0])
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ConsumeTaskNotificationEvent(canceled, events[0], effect.Notification, effect.ClosePending); !errors.Is(err, context.Canceled) {
		t.Fatalf("consume with a canceled context = %v", err)
	}
	remaining, err := store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("canceled consume recorded a receipt: events=%#v err=%v", remaining, err)
	}
	if consumed, err := store.ConsumeTaskNotificationEvent(ctx, events[0], effect.Notification, effect.ClosePending); err != nil || !consumed {
		t.Fatalf("retry consume = consumed:%t err:%v", consumed, err)
	}
	remaining, err = store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("successful retry left events %#v, err=%v", remaining, err)
	}
}

func TestPostgresClarificationNotificationIsActionableWhileRequestIsPending(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "notification-clarification", "clarification task")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "notification-clarification-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim clarification task: found=%t err=%v", found, err)
	}
	modelStepID := mustID(t, domain.NewStepID)
	clarificationStepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	request := &task.ClarificationRequest{
		ID: requestID, TaskID: created.Task.ID, Version: 1, ModelStepID: modelStepID,
		CallID: "clarification-call", Question: "private clarification question", Status: task.ClarificationPending,
	}
	payload, err := json.Marshal(map[string]any{"detail": map[string]any{
		"clarification_id": requestID, "clarification_version": 1, "call_id": request.CallID,
		"question": request.Question,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: clarificationStepID, WaitingReason: "clarification_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{
			{ID: modelStepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask", Summary: "private question"},
			{ID: clarificationStepID, TaskID: claimed.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "Need a detail", Summary: "private question"},
		},
		Event: task.Event{Type: "task.waiting_user", Payload: payload}, ClarificationRequest: request,
	}); err != nil {
		t.Fatalf("persist clarification request: %v", err)
	}
	events, err := store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(events) != 1 || events[0].Type != "task.waiting_user" {
		t.Fatalf("clarification notification events = %#v, err=%v", events, err)
	}
	effect := notificationEffectForTest(t, events[0])
	if effect.Notification == nil || effect.Notification.Kind != notification.KindClarification || effect.Notification.RequestID != requestID || !effect.Notification.Actionable {
		t.Fatalf("clarification effect = %#v", effect)
	}
	if consumed, err := store.ConsumeTaskNotificationEvent(ctx, events[0], effect.Notification, effect.ClosePending); err != nil || !consumed {
		t.Fatalf("consume clarification event = consumed:%t err:%v", consumed, err)
	}
	page, err := store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != notification.KindClarification || !page.Items[0].Actionable {
		t.Fatalf("clarification notification page = %#v, err=%v", page, err)
	}
	if strings.Contains(page.Items[0].Summary, "private") || strings.Contains(page.Items[0].Summary, request.Question) {
		t.Fatalf("clarification notice exposed question: %#v", page.Items[0])
	}
}

func TestPostgresScheduledNotificationsUseRunSnapshot(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)

	completedTask := commitNotificationTerminalTask(t, store, ctx, owner.ID, hostValue.ID, "schedule-notify-completed", task.StatusCompleted)
	failedTask := commitNotificationTerminalTask(t, store, ctx, owner.ID, hostValue.ID, "schedule-notify-failed", task.StatusFailed)
	waitingTask := commitNotificationWaitingTask(t, store, ctx, owner.ID, hostValue.ID, "schedule-notify-waiting")
	attachNotificationScheduleSnapshot(t, store, owner.ID, hostValue.ID, completedTask, task.StatusCompleted, []string{"failed"})
	attachNotificationScheduleSnapshot(t, store, owner.ID, hostValue.ID, failedTask, task.StatusFailed, []string{"failed"})
	attachNotificationScheduleSnapshot(t, store, owner.ID, hostValue.ID, waitingTask, task.StatusWaitingUser, []string{"completed", "failed"})

	events, err := store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("scheduled task events = %#v, err=%v", events, err)
	}
	for _, event := range events {
		wantEnabled := event.TaskID == failedTask
		if event.NotificationEnabled != wantEnabled {
			t.Errorf("scheduled event %s notification_enabled=%t, want %t", event.TaskID, event.NotificationEnabled, wantEnabled)
		}
		effect := notificationEffectForTest(t, event)
		if wantEnabled && (effect.Notification == nil || effect.Notification.Kind != notification.KindFailed) {
			t.Errorf("subscribed failed event effect = %#v", effect)
		}
		switch event.TaskID {
		case completedTask:
			if effect.Notification != nil || !effect.ClosePending {
				t.Errorf("unsubscribed completion event effect = %#v", effect)
			}
		case waitingTask:
			if effect.Notification != nil || effect.ClosePending {
				t.Errorf("unsubscribed waiting-user event effect = %#v", effect)
			}
		}
		if consumed, err := store.ConsumeTaskNotificationEvent(ctx, event, effect.Notification, effect.ClosePending); err != nil || !consumed {
			t.Fatalf("consume scheduled event %s = consumed:%t err:%v", event.TaskID, consumed, err)
		}
	}
	page, err := store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TaskID != failedTask || page.Items[0].Kind != notification.KindFailed {
		t.Fatalf("scheduled notifications = %#v, err=%v", page, err)
	}
}

func TestPostgresScheduledObservationBaselinesAndSuppressesOnlyNoChange(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	spec := schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	configHash := spec.ConfigHash()
	scheduleID := mustID(t, schedule.NewID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO schedules (
id,owner_id,version,status,execution_epoch,host_id,model_id,objective,timing,
budget_active_seconds,budget_tool_calls,budget_model_tokens,auth_kind,misfire_policy,notification_events,
observation_spec,observation_config_hash,baseline_fingerprint
) VALUES ($1,$2,1,'disabled',1,$3,'model-test','observe page field','{}'::jsonb,60,10,100,'session','skip',ARRAY['completed']::text[],$4::jsonb,$5,'')`,
		scheduleID, owner.ID, hostValue.ID, specJSON, configHash); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name        string
		fingerprint string
		runHash     string
		baseline    string
		outcome     schedule.ObservationOutcome
		notify      bool
	}{
		{name: "first observation", fingerprint: strings.Repeat("a", 64), runHash: configHash, baseline: strings.Repeat("a", 64), outcome: schedule.ObservationBaseline, notify: true},
		{name: "unchanged observation", fingerprint: strings.Repeat("a", 64), runHash: configHash, baseline: strings.Repeat("a", 64), outcome: schedule.ObservationNoChange, notify: false},
		{name: "changed observation", fingerprint: strings.Repeat("b", 64), runHash: configHash, baseline: strings.Repeat("b", 64), outcome: schedule.ObservationChanged, notify: true},
		{name: "unknown observation", runHash: configHash, baseline: strings.Repeat("b", 64), outcome: schedule.ObservationUnknown, notify: true},
		{name: "changed configuration", fingerprint: strings.Repeat("c", 64), runHash: strings.Repeat("d", 64), baseline: strings.Repeat("b", 64), outcome: schedule.ObservationUnknown, notify: true},
	}
	for i, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "observation-"+string(rune('a'+i)), "observe page field")
			created, err := store.SubmitTask(ctx, submission)
			if err != nil {
				t.Fatal(err)
			}
			claimed, found, err := store.ClaimNextTask(ctx, "observation-worker", time.Minute)
			if err != nil || !found || claimed.ID != created.Task.ID {
				t.Fatalf("claim task = %#v, found=%t, err=%v", claimed, found, err)
			}
			runID := mustID(t, schedule.NewRunID)
			if _, err := store.pool.Exec(ctx, `INSERT INTO schedule_runs (
id,schedule_id,owner_id,schedule_version,occurrence_key,scheduled_at,status,task_id,notification_events,
observation_spec,observation_config_hash,observation_outcome,observation_fingerprint
) VALUES ($1,$2,$3,1,$4,clock_timestamp(),'queued',$5,ARRAY['completed']::text[],$6::jsonb,$7,'unknown','')`,
				runID, scheduleID, owner.ID, "occurrence-"+string(rune('a'+i)), created.Task.ID, specJSON, check.runHash); err != nil {
				t.Fatal(err)
			}
			details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
			if err != nil || details.ScheduleObservation == nil || *details.ScheduleObservation != spec {
				t.Fatalf("scheduled task observation = %#v, err=%v", details.ScheduleObservation, err)
			}
			stepID := mustID(t, domain.NewStepID)
			if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
				TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
				Status: task.StatusCompleted, CurrentStep: stepID, Result: "checked field",
				Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1}, ObservationFingerprint: check.fingerprint,
				Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "verification", Status: task.StepSucceeded, Title: "Check field", Summary: "checked field"}},
				Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed","result":"checked field"}`)},
			}); err != nil {
				t.Fatalf("commit task completion: %v", err)
			}
			var outcome, fingerprint, baseline string
			if err := store.pool.QueryRow(ctx, `SELECT observation_outcome,observation_fingerprint FROM schedule_runs WHERE id=$1`, runID).
				Scan(&outcome, &fingerprint); err != nil {
				t.Fatal(err)
			}
			if err := store.pool.QueryRow(ctx, `SELECT baseline_fingerprint FROM schedules WHERE id=$1`, scheduleID).Scan(&baseline); err != nil {
				t.Fatal(err)
			}
			if schedule.ObservationOutcome(outcome) != check.outcome || (check.fingerprint != "" && fingerprint != check.fingerprint) {
				t.Fatalf("stored run outcome/fingerprint = %q/%q; want %q/%q", outcome, fingerprint, check.outcome, check.fingerprint)
			}
			if baseline != check.baseline {
				t.Fatalf("baseline = %q; want %q", baseline, check.baseline)
			}
			events, err := store.ListUnprocessedNotificationEvents(ctx, 10)
			if err != nil || len(events) != 1 || events[0].TaskID != created.Task.ID || events[0].NotificationEnabled != check.notify {
				t.Fatalf("completion notification events = %#v, err=%v; want notify=%t", events, err, check.notify)
			}
			var eventPayload map[string]any
			if err := json.Unmarshal(events[0].Payload, &eventPayload); err != nil || eventPayload["observation_outcome"] != string(check.outcome) {
				t.Fatalf("task result payload = %#v, err=%v; want structured outcome %q", eventPayload, err, check.outcome)
			}
			effect := notificationEffectForTest(t, events[0])
			if check.notify != (effect.Notification != nil) {
				t.Fatalf("completion notification effect = %#v; notify=%t", effect, check.notify)
			}
			if consumed, err := store.ConsumeTaskNotificationEvent(ctx, events[0], effect.Notification, effect.ClosePending); err != nil || !consumed {
				t.Fatalf("consume completion event = consumed:%t err=%v", consumed, err)
			}
		})
	}
}

func commitNotificationWaitingTask(t *testing.T, store *Store, ctx context.Context, ownerID, hostID, key string) string {
	t.Helper()
	submission := newTestTaskSubmission(t, ownerID, hostID, key, "scheduled task waiting for attention")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "notification-schedule-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim scheduled waiting task %s: claimed=%#v found=%t err=%v", key, claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "host_fence_changed",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "verification", Status: task.StepWaitingUser, Title: "Waiting", Summary: "private wait detail"}},
		Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"host_fence_changed"}`)},
	}); err != nil {
		t.Fatalf("commit scheduled waiting task: %v", err)
	}
	return created.Task.ID
}

func commitNotificationTerminalTask(t *testing.T, store *Store, ctx context.Context, ownerID, hostID, key string, status task.Status) string {
	t.Helper()
	submission := newTestTaskSubmission(t, ownerID, hostID, key, "scheduled task")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "notification-schedule-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim scheduled task %s: claimed=%#v found=%t err=%v", key, claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	stepStatus := task.StepSucceeded
	if status == task.StatusFailed {
		stepStatus = task.StepFailed
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: status, CurrentStep: stepID, Result: "scheduled result",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "model", Status: stepStatus, Title: "Run", Summary: "scheduled result"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"terminal","result":"scheduled result"}`)},
	}); err != nil {
		t.Fatalf("commit scheduled task result: %v", err)
	}
	return created.Task.ID
}

func attachNotificationScheduleSnapshot(t *testing.T, store *Store, ownerID, hostID, taskID string, status task.Status, events []string) {
	t.Helper()
	ctx := context.Background()
	scheduleID := mustID(t, schedule.NewID)
	runID := mustID(t, schedule.NewRunID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO schedules (
id,owner_id,version,status,execution_epoch,host_id,model_id,objective,timing,
budget_active_seconds,budget_tool_calls,budget_model_tokens,auth_kind,misfire_policy,notification_events
) VALUES ($1,$2,1,'disabled',1,$3,'model-test','scheduled test','{}'::jsonb,60,10,100,'session','skip',$4::text[])`,
		scheduleID, ownerID, hostID, events); err != nil {
		t.Fatalf("insert schedule snapshot owner: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO schedule_runs (
id,schedule_id,owner_id,schedule_version,occurrence_key,scheduled_at,status,task_id,notification_events
) VALUES ($1,$2,$3,1,$4,clock_timestamp(),$5,$6,$7::text[])`, runID, scheduleID, ownerID, "test:"+taskID, string(status), taskID, events); err != nil {
		t.Fatalf("insert schedule run snapshot: %v", err)
	}
}
