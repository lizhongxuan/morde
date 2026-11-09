package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

// TestDSHScheduleToolCreatesDurableWorkAfterTheInitiatingRequestEnds exercises
// the real DSH Node process and broker path before handing work to the durable
// ScheduleWorker and TaskWorker queues. All model output and task data are local
// synthetic values; no provider or remote service is called.
func TestDSHScheduleToolCreatesDurableWorkAfterTheInitiatingRequestEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner task execution: %v", err)
	}

	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatalf("create audit service: %v", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatalf("create credential service: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify local session: principal=%#v err=%v", principal, err)
	}

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatalf("open local Workspace blocks: %v", err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close local Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatalf("create Workspace sync service: %v", err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatalf("create test sealer: %v", err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatalf("create local runtime control: %v", err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatalf("create Runtime tool executor: %v", err)
	}

	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatalf("create personal mutation service: %v", err)
	}
	for _, object := range []struct {
		mutationID string
		objectType string
		objectID   string
		body       string
	}{
		{"schedule-context-profile", personal.ObjectTypeProfile, personal.ProfileObjectID,
			`{"name":"Scheduled context owner","avatar":"dragon"}`},
		{"schedule-context-soul", personal.ObjectTypeSoul, "default",
			`{"name":"Morde","content":"Scheduled tasks protect private data."}`},
		{"schedule-context-memory-setting", personal.ObjectTypeSkillSetting, "builtin_memory", `{"enabled":true}`},
		{"schedule-context-memory", personal.ObjectTypeMemory, "global",
			`{"content":"scheduled runtime preference: prefer concise reports"}`},
		{"schedule-context-goal", personal.ObjectTypeGoal, "g_schedule1",
			`{"title":"scheduled context goal","status":"active","steps":[]}`},
	} {
		if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
			MutationID: object.mutationID, ObjectType: object.objectType, ObjectID: object.objectID, Body: json.RawMessage(object.body),
		}); err != nil {
			t.Fatalf("seed scheduled personal object %s/%s: %v", object.objectType, object.objectID, err)
		}
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x62}, 32))
	if err != nil {
		t.Fatalf("create personal sync service: %v", err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatalf("create goal service: %v", err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatalf("create personal data quota service: %v", err)
	}
	personalAttachments, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatalf("create personal attachment service: %v", err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatalf("create record capture service: %v", err)
	}
	schedules, err := app.NewScheduleService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create Schedule service: %v", err)
	}
	toolBroker, err := app.NewCloudDSHToolBrokerWithPersonalDataAndSchedules(
		store, runtimeTools, nil, nil, recordCapture, personalData, personalSync, goals, personalAttachments, nil, schedules,
	)
	if err != nil {
		t.Fatalf("create DSH tool broker: %v", err)
	}
	tools := &scheduleChainTools{broker: toolBroker}

	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create Task service: %v", err)
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	submission, err := tasks.Submit(requestCtx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Please run the synthetic scheduled task. No file needed; answer in chat.",
		Budget:         task.Budget{ActiveSeconds: 60, ToolCalls: 5, ModelTokens: 1000},
		IdempotencyKey: "f01-schedule-tool-submit-001",
	})
	if err != nil {
		cancelRequest()
		t.Fatalf("submit local initiating task: %v", err)
	}
	model := &scheduleChainModel{initialTaskID: submission.Task.ID, scheduleCreated: make(chan struct{})}

	node, err := exec.LookPath("node")
	if err != nil {
		cancelRequest()
		t.Skip("local Node is unavailable; the real DSH subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		cancelRequest()
		t.Fatalf("resolve local Node path: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		cancelRequest()
		t.Fatalf("resolve local DSH entrypoint: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		cancelRequest()
		t.Fatalf("locate local DSH entrypoint: %v", err)
	}
	stopKey := bytes.Repeat([]byte{0x63}, 32)
	instances, accountRuntimes, supervisor := newF01ConversationRuntime(t, store, node, script, "f01-schedule-tool-e2e", stopKey)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		cancelRequest()
		t.Fatalf("create Agent checkpoint service: %v", err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		cancelRequest()
		t.Fatalf("create Agent child session service: %v", err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "f01-schedule-tool-e2e", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: tools, Children: children, Completion: toolBroker,
	})
	if err != nil {
		cancelRequest()
		t.Fatalf("create Cloud DSH executor: %v", err)
	}
	backgroundTaskWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "f01-schedule-tool-task-worker", LeaseDuration: 30 * time.Second,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		cancelRequest()
		t.Fatalf("create durable TaskWorker: %v", err)
	}
	backgroundScheduleWorker, err := NewScheduleWorker(ScheduleWorkerOptions{
		Service: schedules, PollInterval: 10 * time.Millisecond, BatchSize: 10, Logger: zap.NewNop(),
	})
	if err != nil {
		cancelRequest()
		t.Fatalf("create durable ScheduleWorker: %v", err)
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	taskWorkerDone := make(chan error, 1)
	go func() { taskWorkerDone <- backgroundTaskWorker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorkers()
		select {
		case err := <-taskWorkerDone:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("TaskWorker stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	})
	scheduleWorkerDone := make(chan error, 1)
	go func() { scheduleWorkerDone <- backgroundScheduleWorker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorkers()
		select {
		case err := <-scheduleWorkerDone:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("ScheduleWorker stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ScheduleWorker did not stop")
		}
	})

	select {
	case <-model.scheduleCreated:
		// The second model turn is reached only after DSH has received the tool
		// result from the registered schedule tool and the broker has committed it.
	case <-ctx.Done():
		cancelRequest()
		diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer diagnosticCancel()
		details, detailErr := store.GetTask(diagnosticCtx, submission.Task.ID, owner.ID)
		events, eventsErr := store.ListTaskEvents(diagnosticCtx, owner.ID, submission.Task.ID, 0, 30)
		t.Fatalf("DSH did not finish schedule tool round: %v; task=%#v taskErr=%v events=%#v eventsErr=%v rootModelCalls=%d toolDefs=%t toolCalls=%d supervisorOutput=%q",
			ctx.Err(), details.Task, detailErr, events, eventsErr, model.rootCallCount(), tools.definitionSeen.Load(), tools.scheduleExecutions.Load(), supervisor.output())
	}
	if !tools.definitionSeen.Load() || tools.scheduleExecutions.Load() != 1 {
		cancelRequest()
		t.Fatalf("DSH schedule registration/execution = definition %t, calls %d; want available and called once",
			tools.definitionSeen.Load(), tools.scheduleExecutions.Load())
	}
	// Simulate the user closing the initiating page after schedule creation.
	// Background workers use only the service lifecycle context above.
	cancelRequest()
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("initiating request context = %v; want canceled", requestCtx.Err())
	}

	root, err := waitForScheduleChainTask(ctx, store, submission.Task.ID, owner.ID)
	if err != nil || root.Task.Status != task.StatusCompleted || root.Task.Result != "The local one-time schedule was saved." {
		t.Fatalf("initiating DSH task after request cancellation = status %q result %q err=%v",
			root.Task.Status, root.Task.Result, err)
	}

	schedulePage, err := schedules.List(ctx, principal, "", app.PageRequest{Limit: 10})
	if err != nil || len(schedulePage.Items) != 1 {
		t.Fatalf("schedules created through DSH tool = %#v err=%v; want one durable schedule", schedulePage.Items, err)
	}
	scheduled := schedulePage.Items[0]
	if scheduled.Title != "Local schedule chain" || scheduled.Objective != "Complete the scheduled synthetic task." ||
		scheduled.ModelID != submission.Task.ModelID || scheduled.HostID != "" || scheduled.NextRunAt == nil {
		t.Fatalf("persisted schedule = %#v; want owner/model-bound hostless schedule", scheduled)
	}

	var run schedule.Run
	runDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(runDeadline) {
		runs, runErr := schedules.Runs(ctx, principal, scheduled.ID, app.PageRequest{Limit: 10})
		if runErr != nil {
			err = runErr
			break
		}
		if len(runs.Items) == 1 {
			run = runs.Items[0]
			if run.TaskID != nil && run.Status == schedule.RunCompleted {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if run.TaskID == nil || run.Status != schedule.RunCompleted {
		diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer diagnosticCancel()
		scheduledDetails, taskErr := store.GetTask(diagnosticCtx, derefScheduleChainTaskID(run.TaskID), owner.ID)
		scheduledEvents, eventsErr := store.ListTaskEvents(diagnosticCtx, owner.ID, derefScheduleChainTaskID(run.TaskID), 0, 30)
		eventTypes := make([]string, 0, len(scheduledEvents))
		for _, event := range scheduledEvents {
			eventTypes = append(eventTypes, fmt.Sprintf("%d:%s", event.Sequence, event.Type))
		}
		rootDetails, rootErr := store.GetTask(diagnosticCtx, submission.Task.ID, owner.ID)
		t.Fatalf("durable ScheduleWorker/TaskWorker run = %#v err=%v; scheduledTask={status:%q result:%q step:%q lease:%#v} taskErr=%v eventTypes=%v eventsErr=%v root={status:%q result:%q} rootErr=%v modelCalls={root:%d scheduled:%d context_verified:%t missing_context:%q response_returned:%t} runtimeFrames=%s; want one completed run",
			run, err, scheduledDetails.Task.Status, scheduledDetails.Task.Result, scheduledDetails.Task.CurrentStep, scheduledDetails.Task.Lease,
			taskErr, eventTypes, eventsErr, rootDetails.Task.Status, rootDetails.Task.Result, rootErr, model.rootCallCount(), model.scheduledCalls(),
			model.scheduledContextVerified.Load(), model.getScheduledContextFailure(), model.scheduledResponseReturned.Load(),
			f01AccountRuntimeFrameSummary([]byte(supervisor.output())))
	}
	scheduledTask, err := store.GetTask(ctx, *run.TaskID, owner.ID)
	if err != nil || scheduledTask.Task.Status != task.StatusCompleted || scheduledTask.Task.Result != "Scheduled synthetic work completed." {
		t.Fatalf("scheduled Task = status %q result %q err=%v", scheduledTask.Task.Status, scheduledTask.Task.Result, err)
	}
	if model.scheduledCalls() != 1 {
		t.Fatalf("scheduled Task model calls = %d; want exactly one", model.scheduledCalls())
	}
	if !model.scheduledContextVerified.Load() {
		t.Fatal("scheduled Task model call did not receive the owner's verified SOUL/profile/memory/goal context")
	}
	if processed, err := schedules.ProcessDue(ctx, time.Now().UTC(), 10); err != nil || processed != 0 {
		t.Fatalf("repeat due processing = %d err=%v; want no duplicate occurrence", processed, err)
	}
	notifications, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatalf("create notification service: %v", err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count != 2 {
		t.Fatalf("process root and scheduled result notifications = %d err=%v; want one for each completed Task", count, err)
	}
	page, err := notifications.List(ctx, principal, false, app.PageRequest{Limit: 10})
	completedTaskIDs := make(map[string]notification.Kind, len(page.Items))
	for _, item := range page.Items {
		completedTaskIDs[item.TaskID] = item.Kind
	}
	if err != nil || len(page.Items) != 2 || completedTaskIDs[*run.TaskID] != notification.KindCompleted ||
		completedTaskIDs[submission.Task.ID] != notification.KindCompleted {
		t.Fatalf("owner notification page = %#v err=%v; want completion notifications for root and scheduled Tasks", page.Items, err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count != 0 {
		t.Fatalf("repeat notification processing = %d err=%v; want no duplicate", count, err)
	}
}

// TestGoalBlockedOptInRunsOneShotScheduleThroughDSHOnce composes the existing
// durable Goal outbox, ScheduleWorker, TaskWorker, and local DSH runtime. The
// model is synthetic; the only external process is the repository's local Node
// DSH harness, and all state lives in the temporary PostgreSQL database.
func TestGoalBlockedOptInRunsOneShotScheduleThroughDSHOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled',epoch=epoch+1 WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner task execution: %v", err)
	}

	credential := createF01Session(t, ctx, store, owner.ID)
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatalf("create audit service: %v", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatalf("create credential service: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, credential)
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify local session: principal=%#v err=%v", principal, err)
	}

	// Give the trigger resolver a checked, local-only model connection. The
	// synthetic client below handles calls, so this connection is never used to
	// contact a model provider.
	modelID := "p1234567"
	if _, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: modelID, OwnerID: owner.ID, Name: "Synthetic Goal follow-up", BaseURL: "https://models.example.invalid/v1",
		ModelName: "synthetic-goal-followup", APIKeyCipher: bytes.Repeat([]byte{0x71}, 32),
	}); err != nil {
		t.Fatalf("create local synthetic model connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true WHERE id=$1`, modelID); err != nil {
		t.Fatalf("mark local synthetic text model as checked: %v", err)
	}
	if _, err := store.SaveUserPreferences(ctx, owner.ID, "", modelID); err != nil {
		t.Fatalf("select local synthetic model: %v", err)
	}

	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatalf("create personal mutation service: %v", err)
	}
	apply := func(mutationID, objectType, objectID string, revision int64, body string) {
		t.Helper()
		_, applyErr := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
			MutationID: mutationID, ObjectType: objectType, ObjectID: objectID, BaseRevision: revision,
			Body: json.RawMessage(body),
		})
		if applyErr != nil {
			t.Fatalf("apply personal mutation %s: %v", mutationID, applyErr)
		}
	}
	apply("goal-followup-opt-in", personal.ObjectTypeSkillSetting, "builtin_goal_followups", 0, `{"enabled":true}`)
	goalID := "g_trigger1"
	goalBody := func(stepStatus string) string {
		return `{"title":"Quarterly launch","description":"Prepare the release","status":"active","steps":[{"id":"step_1","title":"Prepare the release notes","status":"` + stepStatus + `","note":"Waiting for final review"}]}`
	}
	apply("goal-followup-create", personal.ObjectTypeGoal, goalID, 0, goalBody("pending"))
	apply("goal-followup-block", personal.ObjectTypeGoal, goalID, 1, goalBody("blocked"))
	var outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM goal_agent_trigger_events WHERE owner_id=$1 AND goal_id=$2`, owner.ID, goalID).Scan(&outboxCount); err != nil || outboxCount != 1 {
		t.Fatalf("blocked opt-in Goal outbox events = %d err=%v; want one", outboxCount, err)
	}

	// Consume the Goal-step source event first so the final count below can
	// specifically assert one task-completion notification for the DSH result.
	notifications, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatalf("create notification service: %v", err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count != 1 {
		t.Fatalf("process blocked-step notification = %d err=%v; want one source notification", count, err)
	}

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatalf("open local Workspace blocks: %v", err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close local Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatalf("create Workspace sync service: %v", err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatalf("create test sealer: %v", err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatalf("create local runtime control: %v", err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatalf("create Runtime tool executor: %v", err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x73}, 32))
	if err != nil {
		t.Fatalf("create personal sync service: %v", err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatalf("create Goal service: %v", err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatalf("create personal data quota service: %v", err)
	}
	personalAttachments, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatalf("create personal attachment service: %v", err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatalf("create record capture service: %v", err)
	}
	schedules, err := app.NewScheduleService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create Schedule service: %v", err)
	}
	toolBroker, err := app.NewCloudDSHToolBrokerWithPersonalDataAndSchedules(
		store, runtimeTools, nil, nil, recordCapture, personalData, personalSync, goals, personalAttachments, nil, schedules,
	)
	if err != nil {
		t.Fatalf("create DSH tool broker: %v", err)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable; the real DSH subprocess cannot run")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatalf("resolve local Node path: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatalf("resolve local DSH entrypoint: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("locate local DSH entrypoint: %v", err)
	}
	instances, accountRuntimes, supervisor := newF01ConversationRuntime(t, store, node, script, "f01-goal-followup-e2e", bytes.Repeat([]byte{0x74}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatalf("create Agent checkpoint service: %v", err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatalf("create Agent child session service: %v", err)
	}
	model := &goalFollowupChainModel{}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "f01-goal-followup-e2e", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: toolBroker, Children: children, Completion: toolBroker,
	})
	if err != nil {
		t.Fatalf("create Cloud DSH executor: %v", err)
	}
	tasks, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "f01-goal-followup-task-worker", LeaseDuration: 30 * time.Second,
		PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create durable TaskWorker: %v", err)
	}
	scheduleWorker, err := NewScheduleWorker(ScheduleWorkerOptions{
		Service: schedules, PollInterval: 10 * time.Millisecond, BatchSize: 10, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create durable ScheduleWorker: %v", err)
	}
	triggerService, err := app.NewGoalAgentTriggerService(store, store, schedules, "f01-goal-followup-trigger", time.Minute)
	if err != nil {
		t.Fatalf("create Goal trigger service: %v", err)
	}
	triggerWorker, err := NewGoalAgentTriggerWorker(GoalAgentTriggerWorkerOptions{
		Service: triggerService, PollInterval: 10 * time.Millisecond, BatchSize: 10, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create Goal trigger worker: %v", err)
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	startWorker := func(name string, run func(context.Context) error) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- run(workerCtx) }()
		t.Cleanup(func() {
			cancelWorkers()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%s stopped with error: %v", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s did not stop", name)
			}
		})
		return done
	}
	taskWorkerDone := startWorker("TaskWorker", tasks.Run)
	scheduleWorkerDone := startWorker("ScheduleWorker", scheduleWorker.Run)
	triggerWorkerDone := startWorker("GoalAgentTriggerWorker", triggerWorker.Run)
	_ = taskWorkerDone
	_ = scheduleWorkerDone
	_ = triggerWorkerDone

	var created schedule.Schedule
	deadline := time.Now().Add(28 * time.Second)
	var run schedule.Run
	for time.Now().Before(deadline) {
		page, listErr := schedules.List(ctx, principal, "", app.PageRequest{Limit: 10})
		if listErr != nil {
			t.Fatalf("list triggered Schedules: %v", listErr)
		}
		if len(page.Items) > 1 {
			t.Fatalf("Goal trigger created duplicate Schedules: %#v", page.Items)
		}
		if len(page.Items) == 1 {
			created = page.Items[0]
			if created.ExecutionPolicy != schedule.ExecutionPolicyGoalFollowupReadOnly || created.HostID != "" ||
				created.Timing.Kind != schedule.TimingOnce || created.ModelID != modelID {
				t.Fatalf("Goal trigger Schedule = %#v; want one hostless, one-shot, read-only schedule", created)
			}
			runs, runsErr := schedules.Runs(ctx, principal, created.ID, app.PageRequest{Limit: 10})
			if runsErr != nil {
				t.Fatalf("list Goal trigger Schedule runs: %v", runsErr)
			}
			if len(runs.Items) > 1 {
				t.Fatalf("one-shot Goal Schedule produced duplicate runs: %#v", runs.Items)
			}
			if len(runs.Items) == 1 {
				run = runs.Items[0]
				if run.Status == schedule.RunCompleted && run.TaskID != nil {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Goal follow-up chain did not complete: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if created.ID == "" || run.Status != schedule.RunCompleted || run.TaskID == nil {
		t.Fatalf("Goal trigger Schedule run = %#v; want one completed run; DSH model calls=%d runtime=%s",
			run, model.calls.Load(), f01AccountRuntimeFrameSummary([]byte(supervisor.output())))
	}
	if model.calls.Load() != 1 {
		t.Fatalf("read-only Goal follow-up DSH model calls = %d; want one", model.calls.Load())
	}
	details, err := store.GetTask(ctx, *run.TaskID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != "I reviewed the blocked step and recommend confirming the reviewer and setting a new due date." {
		t.Fatalf("Goal follow-up Task result = status %q result %q err=%v", details.Task.Status, details.Task.Result, err)
	}
	if details.Task.HostID != "" || details.Task.WorkspaceID != "" {
		t.Fatalf("read-only Goal follow-up Task acquired a computer or Workspace: host=%q workspace=%#v", details.Task.HostID, details.Task.WorkspaceID)
	}
	var scopes []string
	if err := pool.QueryRow(ctx, `SELECT auth_scopes FROM tasks WHERE id=$1 AND owner_id=$2`, *run.TaskID, owner.ID).Scan(&scopes); err != nil {
		t.Fatalf("read persisted Goal follow-up Task scopes: %v", err)
	}
	wantScopes := make([]string, 0, len(identity.GoalFollowupScheduleTaskExecutionScopes()))
	for _, scope := range identity.GoalFollowupScheduleTaskExecutionScopes() {
		wantScopes = append(wantScopes, string(scope))
	}
	if len(scopes) != len(wantScopes) {
		t.Fatalf("read-only Goal Task scopes = %v; want exactly %v", scopes, wantScopes)
	}
	for _, want := range wantScopes {
		found := false
		for _, got := range scopes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("read-only Goal Task scopes = %v; missing %q", scopes, want)
		}
	}

	var outboxStatus, storedScheduleID string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,schedule_id,attempt_count FROM goal_agent_trigger_events WHERE owner_id=$1 AND goal_id=$2 AND step_id='step_1'`, owner.ID, goalID).
		Scan(&outboxStatus, &storedScheduleID, &attempts); err != nil || outboxStatus != "completed" || storedScheduleID != created.ID || attempts != 1 {
		t.Fatalf("Goal trigger outbox = status %q schedule %q attempts %d err=%v; want completed, linked once, one claim", outboxStatus, storedScheduleID, attempts, err)
	}
	if processed, err := triggerService.ProcessPending(ctx, 10); err != nil || processed != 0 {
		t.Fatalf("repeat Goal trigger poll = %d err=%v; want no duplicate Schedule", processed, err)
	}
	if processed, err := schedules.ProcessDue(ctx, time.Now().UTC(), 10); err != nil || processed != 0 {
		t.Fatalf("repeat one-shot Schedule poll = %d err=%v; want no duplicate run", processed, err)
	}
	page, err := schedules.List(ctx, principal, "", app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("Schedules after duplicate polls = %d err=%v; want one", len(page.Items), err)
	}
	runs, err := schedules.Runs(ctx, principal, created.ID, app.PageRequest{Limit: 10})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].TaskID == nil || *runs.Items[0].TaskID != *run.TaskID {
		t.Fatalf("runs after duplicate polls = %#v err=%v; want exactly the original Task", runs.Items, err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count != 1 {
		t.Fatalf("process Goal follow-up completion notification = %d err=%v; want one", count, err)
	}
	notificationPage, err := notifications.List(ctx, principal, false, app.PageRequest{Limit: 10})
	completionCount := 0
	for _, item := range notificationPage.Items {
		if item.TaskID == *run.TaskID && item.Kind == notification.KindCompleted {
			completionCount++
		}
	}
	if err != nil || completionCount != 1 {
		t.Fatalf("Goal follow-up task completion notifications = %d page=%#v err=%v; want exactly one", completionCount, notificationPage.Items, err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count != 0 {
		t.Fatalf("repeat completion notification poll = %d err=%v; want no duplicate", count, err)
	}
}

type goalFollowupChainModel struct {
	calls atomic.Int32
}

func (m *goalFollowupChainModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID == "" || call.Source != app.ModelCallTask {
		return app.ModelResponse{}, errors.New("Goal follow-up DSH model call has no Task fence")
	}
	for _, tool := range call.Request.Tools {
		switch tool.Name {
		case "nanomuse_skill_read", "morde_read_task_attachment", "morde_workspace_list", "morde_workspace_read", "morde_workspace_search":
		default:
			return app.ModelResponse{}, fmt.Errorf("read-only Goal follow-up registered disallowed tool %q", tool.Name)
		}
	}
	m.calls.Add(1)
	return app.ModelResponse{
		Text:       "I reviewed the blocked step and recommend confirming the reviewer and setting a new due date.",
		StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 15},
	}, nil
}

type scheduleChainModel struct {
	mu                        sync.Mutex
	initialTaskID             string
	rootCalls                 int
	scheduledCount            int
	scheduledContextVerified  atomic.Bool
	scheduledResponseReturned atomic.Bool
	scheduledContextFailure   string
	scheduleCreated           chan struct{}
}

func (m *scheduleChainModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("local DSH model call has no task fence")
	}
	usage := app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 1}
	if call.Task.TaskID != m.initialTaskID {
		m.mu.Lock()
		m.scheduledCount++
		m.mu.Unlock()
		for _, expected := range []string{"Scheduled context owner", "Scheduled tasks protect private data.",
			"scheduled runtime preference: prefer concise reports", "scheduled context goal"} {
			if !bytes.Contains(call.RawRequest, []byte(expected)) {
				m.mu.Lock()
				m.scheduledContextFailure = expected
				m.mu.Unlock()
				return app.ModelResponse{}, fmt.Errorf("scheduled Task request omitted personal context %q", expected)
			}
		}
		m.scheduledContextVerified.Store(true)
		m.scheduledResponseReturned.Store(true)
		return app.ModelResponse{Text: "Scheduled synthetic work completed.", StopReason: "stop", Usage: usage}, nil
	}
	m.mu.Lock()
	m.rootCalls++
	callNumber := m.rootCalls
	m.mu.Unlock()
	if callNumber == 1 {
		runAt := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second).Format(time.RFC3339)
		arguments, err := json.Marshal(map[string]any{
			"title": "Local schedule chain", "objective": "Complete the scheduled synthetic task.",
			"timing": map[string]any{
				"kind": "once", "run_at": runAt, "starts_on": "", "local_time": "", "time_zone": "UTC",
				"weekdays": []int{}, "interval_hours": 0, "anchor_at": "",
			},
		})
		if err != nil {
			return app.ModelResponse{}, fmt.Errorf("encode schedule tool arguments: %w", err)
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{
			CallID: "f01-schedule-create-call-001", Name: "morde_schedule_create", Arguments: string(arguments),
		}}, StopReason: "tool_calls", Usage: usage}, nil
	}
	if callNumber != 2 {
		return app.ModelResponse{}, fmt.Errorf("unexpected initiating task model turn %d", callNumber)
	}
	if _, ok := f01ToolResult(call.RawRequest, "f01-schedule-create-call-001"); !ok {
		return app.ModelResponse{}, errors.New("DSH model turn did not contain the persisted schedule tool result")
	}
	close(m.scheduleCreated)
	return app.ModelResponse{Text: "The local one-time schedule was saved.", StopReason: "stop", Usage: usage}, nil
}

func (m *scheduleChainModel) getScheduledContextFailure() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scheduledContextFailure
}

// f01AccountRuntimeFrameSummary keeps schedule integration diagnostics useful
// without printing user prompts, tool arguments, or assistant response text.
func f01AccountRuntimeFrameSummary(output []byte) string {
	type frame struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		State string `json:"state"`
	}
	counts := make(map[string]int)
	protocolErrors := make([]string, 0, 2)
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var value frame
		if json.Unmarshal(line, &value) != nil || value.Type == "" {
			continue
		}
		counts[value.Type]++
		if value.Type == "protocol_error" && value.Code != "" && len(protocolErrors) < 2 {
			protocolErrors = append(protocolErrors, value.Code)
		}
	}
	return fmt.Sprintf("%v protocol_errors=%v", counts, protocolErrors)
}

func (m *scheduleChainModel) scheduledCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scheduledCount
}

func (m *scheduleChainModel) rootCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootCalls
}

func derefScheduleChainTaskID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type scheduleChainTools struct {
	broker             *app.CloudDSHToolBroker
	definitionSeen     atomic.Bool
	scheduleExecutions atomic.Int32
}

func (t *scheduleChainTools) Definitions(ctx context.Context, value task.Task) ([]app.AgentToolDefinition, error) {
	definitions, err := t.broker.Definitions(ctx, value)
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if definition.Name == "morde_schedule_create" {
			t.definitionSeen.Store(true)
			break
		}
	}
	return definitions, nil
}

func (t *scheduleChainTools) InitialContext(ctx context.Context, value task.Task) (string, error) {
	return t.broker.InitialContext(ctx, value)
}

func (t *scheduleChainTools) ValidatePersonalMemoryProof(ctx context.Context, value task.Task, proof string,
	additional []app.CloudDSHMemoryVersion) error {
	return t.broker.ValidatePersonalMemoryProof(ctx, value, proof, additional)
}

func (t *scheduleChainTools) CloudDSHPersonalMemoryProofEnabled() bool {
	return t.broker.CloudDSHPersonalMemoryProofEnabled()
}

func (t *scheduleChainTools) CloudDSHPersonalMemoryProofEnabledForTask(value task.Task) bool {
	return t.broker.CloudDSHPersonalMemoryProofEnabledForTask(value)
}

func (t *scheduleChainTools) PausePersonalMemoryRecovery(ctx context.Context, value task.Task) error {
	return t.broker.PausePersonalMemoryRecovery(ctx, value)
}

func (t *scheduleChainTools) Execute(ctx context.Context, value task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	if call.Name == "morde_schedule_create" {
		t.scheduleExecutions.Add(1)
	}
	return t.broker.Execute(ctx, value, call)
}

func waitForScheduleChainTask(ctx context.Context, store interface {
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}, id, ownerID string) (app.TaskDetails, error) {
	for {
		value, err := store.GetTask(ctx, id, ownerID)
		if err == nil && (value.Task.Status == task.StatusCompleted || value.Task.Status == task.StatusFailed || value.Task.Status == task.StatusCancelled) {
			return value, nil
		}
		if err != nil {
			return app.TaskDetails{}, err
		}
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
