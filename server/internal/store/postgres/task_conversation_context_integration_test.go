package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskConversationContextFixesSubmissionBoundaries(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-first-task", "first request: context fact is Cedar"))
	if err != nil {
		t.Fatalf("submit first Task: %v", err)
	}
	seedCompletedAssistantContextMessage(t, store, first, "confirmed answer: Cedar is the saved name")

	secondSubmission := newTestTaskSubmission(t, owner.ID, "", "context-second-task", "second request: use the saved name")
	secondSubmission.Task.ConversationID = first.Conversation.ID
	second, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatalf("submit second Task: %v", err)
	}
	secondManifest := readTaskConversationContextManifest(t, ctx, store, second.Task.ID)
	if secondManifest.Scope != "personal" || secondManifest.ThroughSequence != 2 || secondManifest.MessageCount != 2 {
		t.Fatalf("SubmitTask manifest = %+v; want both confirmed messages through sequence 2", secondManifest)
	}
	secondClaim, found, err := store.ClaimNextTask(ctx, "context-submit-worker", time.Minute)
	if err != nil || !found || secondClaim.ID != second.Task.ID {
		t.Fatalf("claim second Task = %s, found=%t, err=%v", secondClaim.ID, found, err)
	}
	if secondClaim.ConversationContext == nil || secondClaim.ConversationContext.Scope != "personal" ||
		secondClaim.ConversationContext.ThroughSequence != 2 || len(secondClaim.ConversationContext.Messages) != 2 ||
		secondClaim.ConversationContext.Messages[0].Content != first.Message.Content ||
		secondClaim.ConversationContext.Messages[1].Content != "confirmed answer: Cedar is the saved name" ||
		len(secondClaim.ConversationContext.Digest) != sha256.Size {
		t.Fatalf("claimed SubmitTask context = %+v", secondClaim.ConversationContext)
	}
	for _, message := range secondClaim.ConversationContext.Messages {
		if message.Content == secondSubmission.Task.Objective {
			t.Fatal("current SubmitTask input was included in its preceding history")
		}
	}

	seedCompletedAssistantContextMessage(t, store, app.TaskSubmissionResult{Task: second.Task, Conversation: second.Conversation},
		"confirmed answer: the saved name is Cedar")
	thirdSubmission := newTestTaskSubmission(t, owner.ID, "", "context-third-task", "third request: continue the same topic")
	thirdSubmission.Task.ConversationID = first.Conversation.ID
	thirdSubmission.Message.ConversationID = first.Conversation.ID
	thirdSubmission.Idempotency = testIdempotency(owner.ID, "task.submit", "context-third-task")
	third, err := store.SubmitTask(ctx, thirdSubmission)
	if err != nil {
		t.Fatalf("submit third Task: %v", err)
	}
	thirdManifest := readTaskConversationContextManifest(t, ctx, store, third.Task.ID)
	if thirdManifest.Scope != "personal" || thirdManifest.ThroughSequence != 4 || thirdManifest.MessageCount != 4 {
		t.Fatalf("third Task manifest = %+v; want four confirmed messages through sequence 4", thirdManifest)
	}
	thirdClaim, found, err := store.ClaimNextTask(ctx, "context-append-worker", time.Minute)
	if err != nil || !found || thirdClaim.ID != third.Task.ID {
		t.Fatalf("claim third Task = %s, found=%t, err=%v", thirdClaim.ID, found, err)
	}
	if thirdClaim.ConversationContext == nil || thirdClaim.ConversationContext.ThroughSequence != 4 ||
		len(thirdClaim.ConversationContext.Messages) != 4 ||
		thirdClaim.ConversationContext.Messages[0].Content != first.Message.Content ||
		thirdClaim.ConversationContext.Messages[1].Content != "confirmed answer: Cedar is the saved name" ||
		thirdClaim.ConversationContext.Messages[2].Content != secondSubmission.Task.Objective ||
		thirdClaim.ConversationContext.Messages[3].Content != "confirmed answer: the saved name is Cedar" {
		t.Fatalf("claimed third Task context = %+v", thirdClaim.ConversationContext)
	}
	for _, message := range thirdClaim.ConversationContext.Messages {
		if message.Content == thirdSubmission.Task.Objective {
			t.Fatal("current Task input was included in its preceding history")
		}
	}
}

func TestPostgresCompletedCloudResultIsConversationHistory(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "result-history-first", "Remember Cedar"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "result-history-worker", time.Minute)
	if err != nil || !found || claimed.ID != first.Task.ID {
		t.Fatalf("claim first task: found=%t err=%v", found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	progress := app.TaskProgress{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "I will remember Cedar.", PublishResultMessage: true,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{}`)}}
	if _, err := store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	page, err := store.ListMessages(ctx, owner.ID, first.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(page.Items) != 2 || page.Items[0].Role != "assistant" || page.Items[0].Content != progress.Result {
		t.Fatalf("persisted conversation messages = %+v, err=%v", page.Items, err)
	}
	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.ListMessages(ctx, other.ID, first.Conversation.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another owner accessed completed result: %v", err)
	}
	second := newTestTaskSubmission(t, owner.ID, "", "result-history-second", "What name did I mention?")
	second.Task.ConversationID = first.Conversation.ID
	if _, err := store.SubmitTask(ctx, second); err != nil {
		t.Fatal(err)
	}
	next, found, err := store.ClaimNextTask(ctx, "result-history-next", time.Minute)
	if err != nil || !found || next.ConversationContext == nil || len(next.ConversationContext.Messages) != 2 ||
		next.ConversationContext.Messages[1].Content != progress.Result {
		t.Fatalf("next task history = %+v, found=%t err=%v", next.ConversationContext, found, err)
	}
}

func TestPostgresTaskConversationContextEmptyAndConversationScoped(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	withHistory, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-isolation-seed", "private fact belongs to conversation A"))
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedAssistantContextMessage(t, store, withHistory, "conversation A answer")

	otherConversation, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-isolation-empty", "new conversation has no history"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := readTaskConversationContextManifest(t, ctx, store, otherConversation.Task.ID)
	if manifest.Scope != "personal" || manifest.ThroughSequence != 0 || manifest.MessageCount != 0 || len(manifest.DigestSHA256) != sha256.Size*2 {
		t.Fatalf("empty-history manifest = %+v; want explicit empty digest", manifest)
	}
	otherOwner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	crossOwner := newTestTaskSubmission(t, otherOwner.ID, "", "context-cross-owner", "must not read another account")
	crossOwner.Task.ConversationID = withHistory.Conversation.ID
	crossOwner.Message.ConversationID = withHistory.Conversation.ID
	if _, err := store.SubmitTask(ctx, crossOwner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner conversation submission error = %v; want not_found", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "context-isolation-worker", time.Minute)
	if err != nil || !found || claimed.ID != otherConversation.Task.ID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.ThroughSequence != 0 || len(claimed.ConversationContext.Messages) != 0 || len(claimed.ConversationContext.Digest) != sha256.Size {
		t.Fatalf("claim with empty history = %#v, found=%t, err=%v", claimed.ConversationContext, found, err)
	}
	for _, message := range claimed.ConversationContext.Messages {
		if strings.Contains(message.Content, "private fact belongs to conversation A") {
			t.Fatal("conversation A history crossed into a separate conversation")
		}
	}
}

func TestPostgresTaskConversationContextExcludesInterruptedAssistantOutput(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-interrupted-first", "first confirmed user message"))
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedAssistantContextMessage(t, store, first, "first confirmed assistant answer")
	secondSubmission := newTestTaskSubmission(t, owner.ID, "", "context-interrupted-second", "second user message")
	secondSubmission.Task.ConversationID = first.Conversation.ID
	second, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatal(err)
	}
	seedInterruptedAssistantContextMessage(t, store, second, "partial assistant output must not be history")
	thirdSubmission := newTestTaskSubmission(t, owner.ID, "", "context-interrupted-third", "current request")
	thirdSubmission.Task.ConversationID = first.Conversation.ID
	third, err := store.SubmitTask(ctx, thirdSubmission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "context-interrupted-worker", time.Minute)
	if err != nil || !found || claimed.ID != third.Task.ID || claimed.ConversationContext == nil ||
		len(claimed.ConversationContext.Messages) != 3 ||
		claimed.ConversationContext.Messages[0].Content != first.Message.Content ||
		claimed.ConversationContext.Messages[1].Content != "first confirmed assistant answer" ||
		claimed.ConversationContext.Messages[2].Content != secondSubmission.Task.Objective {
		t.Fatalf("context included unfinished assistant output: %#v, found=%t, err=%v", claimed.ConversationContext, found, err)
	}
}

func TestPostgresTaskConversationContextRejectsMissingHistoryAndInitialEvent(t *testing.T) {
	t.Run("history no longer matches fixed digest", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
		first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-corrupt-seed", "history user message"))
		if err != nil {
			t.Fatal(err)
		}
		assistantID := seedCompletedAssistantContextMessage(t, store, first, "history assistant message")
		secondSubmission := newTestTaskSubmission(t, owner.ID, "", "context-corrupt-next", "current request")
		secondSubmission.Task.ConversationID = first.Conversation.ID
		second, err := store.SubmitTask(ctx, secondSubmission)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `DELETE FROM conversation_messages WHERE id=$1 AND owner_id=$2 AND conversation_id=$3`,
			assistantID, owner.ID, first.Conversation.ID); err != nil {
			t.Fatalf("remove historical assistant row: %v", err)
		}
		if _, found, err := store.ClaimNextTask(ctx, "context-corrupt-worker", time.Minute); err != nil || found {
			t.Fatalf("claim with missing fixed history: found=%t err=%v", found, err)
		}
		var status, result string
		if err := store.pool.QueryRow(ctx, `SELECT status,result FROM tasks WHERE id=$1`, second.Task.ID).Scan(&status, &result); err != nil || status != string(task.StatusCancelling) || !strings.Contains(result, "无法恢复") {
			t.Fatalf("unrecoverable Task state=%q result=%q err=%v", status, result, err)
		}
	})

	t.Run("initial event missing", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
		created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-missing-event", "must not run without initial boundary"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `DELETE FROM task_events WHERE task_id=$1 AND sequence=1`, created.Task.ID); err != nil {
			t.Fatal(err)
		}
		if _, found, err := store.ClaimNextTask(ctx, "context-missing-event-worker", time.Minute); err != nil || found {
			t.Fatalf("claim with missing initial event: found=%t err=%v", found, err)
		}
		var status string
		if err := store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, created.Task.ID).Scan(&status); err != nil || status != string(task.StatusCancelling) {
			t.Fatalf("unrecoverable Task status=%q err=%v", status, err)
		}
	})
}

func TestPostgresTaskClaimWithCheckpointRequiresFixedConversationHistory(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "context-checkpoint-seed", "checkpoint prior user message"))
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedAssistantContextMessage(t, store, first, "checkpoint prior assistant answer")
	secondSubmission := newTestTaskSubmission(t, owner.ID, "", "context-checkpoint-next", "resume from dsh checkpoint")
	secondSubmission.Task.ConversationID = first.Conversation.ID
	second, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatal(err)
	}
	initialClaim, found, err := store.ClaimNextTask(ctx, "context-checkpoint-worker", time.Minute)
	if err != nil || !found || initialClaim.ID != second.Task.ID || initialClaim.ConversationContext == nil || len(initialClaim.ConversationContext.Messages) != 2 {
		t.Fatalf("initial claim context = %#v, found=%t, err=%v", initialClaim.ConversationContext, found, err)
	}
	digest := sha256.Sum256([]byte("verified local dsh checkpoint"))
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id='agent_context',agent_generation=1,
agent_session_id='session_context',agent_engine_version='dsh-test',agent_profile_version='profile-test',
checkpoint_through_seq=0,checkpoint_digest=$2,lease_owner='expired-context-worker',
lease_expires_at=clock_timestamp()-interval '1 second',updated_at=clock_timestamp() WHERE id=$1`, second.Task.ID, digest[:]); err != nil {
		t.Fatalf("seed recovered Task checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest) VALUES ($1,$2,'session_context',0,$3)`, second.Task.ID, owner.ID, digest[:]); err != nil {
		t.Fatalf("seed root Agent Session checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM conversation_messages WHERE owner_id=$1 AND conversation_id=$2 AND task_id=$3`,
		owner.ID, first.Conversation.ID, first.Task.ID); err != nil {
		t.Fatalf("retire old conversation rows after checkpoint: %v", err)
	}
	recovered, found, err := store.ClaimNextTask(ctx, "context-checkpoint-recovery", time.Minute)
	if err != nil || found {
		t.Fatalf("personal checkpoint with missing fixed conversation history was claimed: task=%q found=%t err=%v", recovered.ID, found, err)
	}
	var status, result string
	if err := store.pool.QueryRow(ctx, `SELECT status,result FROM tasks WHERE id=$1`, second.Task.ID).Scan(&status, &result); err != nil ||
		status != string(task.StatusCancelling) || !strings.Contains(result, "历史上下文无法恢复") {
		t.Fatalf("Task after missing fixed conversation history = status %q result %q err=%v", status, result, err)
	}
}

func TestPostgresScheduledTaskClaimHasExplicitEmptyConversationContext(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	scheduled := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireRunOnce, nil)
	next, err := schedule.FirstOccurrence(timing, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	run, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
		ScheduleID: value.ID, OwnerID: owner.ID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
		Occurrence: schedule.Occurrence{Key: next.Key, ScheduledAt: next.ScheduledAt, CheckAt: next.CheckAt, Status: schedule.RunQueued},
	})
	if err != nil || !committed || run.TaskID == nil {
		t.Fatalf("commit scheduled occurrence = %#v, committed=%t, err=%v", run, committed, err)
	}
	manifest := readTaskConversationContextManifest(t, ctx, store, *run.TaskID)
	if manifest.Scope != "personal" || manifest.ThroughSequence != 0 || manifest.MessageCount != 0 || len(manifest.DigestSHA256) != sha256.Size*2 {
		t.Fatalf("scheduled Task context manifest = %+v; want explicit empty context", manifest)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "context-schedule-worker", time.Minute)
	if err != nil || !found || claimed.ID != *run.TaskID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.Scope != "personal" || claimed.ConversationContext.ThroughSequence != 0 ||
		len(claimed.ConversationContext.Messages) != 0 || len(claimed.ConversationContext.Digest) != sha256.Size {
		t.Fatalf("claimed scheduled Task context = %#v, found=%t, err=%v", claimed.ConversationContext, found, err)
	}
}

func TestPostgresScheduledPersonalContextRequiresExactOwnerRunAndTask(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := createPreRetirementTestUser(t, store, 9, 20_000, 40_000)

	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	objects := []struct {
		objectType string
		objectID   string
		body       string
	}{
		{personal.ObjectTypeProfile, personal.ProfileObjectID, `{"name":"Context owner","avatar":"spark"}`},
		{personal.ObjectTypeSoul, "default", `{"name":"Morde","content":"Protect private data."}`},
		{personal.ObjectTypeMemory, "global", `{"content":"prefers short summaries"}`},
		{personal.ObjectTypeGoal, "g_schedule1", `{"title":"Finish report","status":"active","steps":[]}`},
		{personal.ObjectTypeSkillSetting, "builtin_memory", `{"enabled":true}`},
	}
	for index, object := range objects {
		mutation := personalObjectMutationForTest(t, owner.ID, fmt.Sprintf("schedule-context-%02d", index),
			object.objectType, object.objectID, 0, object.body, false)
		if _, err := store.ApplyPersonalMutation(ctx, principal, mutation); err != nil {
			t.Fatalf("seed %s/%s: %v", object.objectType, object.objectID, err)
		}
	}

	timing := schedule.Timing{Kind: schedule.TimingOnce,
		RunAt: time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second).Format(time.RFC3339), TimeZone: "UTC"}
	definition, _ := createPostgresSchedule(t, ctx, store, owner.ID, "", timing, schedule.MisfireRunOnce, nil)
	occurrence, err := schedule.FirstOccurrence(timing, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	run, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
		ScheduleID: definition.ID, OwnerID: owner.ID, ExpectedVersion: definition.Version, ExpectedEpoch: definition.ExecutionEpoch,
		Occurrence: schedule.Occurrence{Key: occurrence.Key, ScheduledAt: occurrence.ScheduledAt,
			CheckAt: occurrence.CheckAt, Status: schedule.RunQueued},
	})
	if err != nil || !committed || run.TaskID == nil {
		t.Fatalf("commit scheduled occurrence = %+v, committed=%t, err=%v", run, committed, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "scheduled-personal-context-worker", time.Minute)
	if err != nil || !found || claimed.ID != *run.TaskID {
		t.Fatalf("claim scheduled Task = %q, found=%t, err=%v", claimed.ID, found, err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !broker.CloudDSHPersonalMemoryProofEnabledForTask(claimed) ||
		broker.CloudDSHPersonalMemoryProofEnabledForTask(task.Task{Authorization: task.Authorization{Kind: string(identity.KindSession)}}) {
		t.Fatal("memory checkpoint gate was not scoped to the exact context route")
	}
	initialContext, err := broker.InitialContext(ctx, claimed)
	if err != nil || !strings.Contains(initialContext, "Context owner") || !strings.Contains(initialContext, "Protect private data") ||
		!strings.Contains(initialContext, "prefers short summaries") || !strings.Contains(initialContext, "Finish report") {
		t.Fatalf("scheduled context did not flow through the production broker: %q, %v", initialContext, err)
	}
	proof := strings.TrimPrefix(initialContext[strings.LastIndex(initialContext, "\n")+1:], app.CloudDSHMemoryProofPrefix)
	if proof == "" || broker.ValidatePersonalMemoryProof(ctx, claimed, proof, nil) != nil {
		t.Fatalf("scheduled personal context proof was not verifiable: %q", proof)
	}

	items, hasMore, err := store.ListScheduledPersonalContext(ctx, owner.ID, run.ID, claimed.ID, true, 10)
	if err != nil || hasMore || len(items) != 4 {
		t.Fatalf("scheduled personal snapshot = %d items, more=%t, err=%v", len(items), hasMore, err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ObjectType+"/"+item.ObjectID] = true
	}
	for _, key := range []string{"profile/" + personal.ProfileObjectID, "soul/default", "memory/global", "goal/g_schedule1"} {
		if !seen[key] {
			t.Errorf("authorized context omitted %s: %+v", key, items)
		}
	}
	if seen["skill_setting/builtin_memory"] {
		t.Fatal("memory setting leaked into the model context listing")
	}
	withoutMemory, more, err := store.ListScheduledPersonalContext(ctx, owner.ID, run.ID, claimed.ID, false, 10)
	if err != nil || more || len(withoutMemory) != 3 {
		t.Fatalf("memory opt-out snapshot = %d items, more=%t, err=%v", len(withoutMemory), more, err)
	}
	for _, item := range withoutMemory {
		if item.ObjectType == personal.ObjectTypeMemory {
			t.Fatal("memory was returned after opt-out")
		}
	}
	setting, err := store.GetScheduledPersonalContextObject(ctx, owner.ID, run.ID, claimed.ID, personal.ObjectTypeSkillSetting, "builtin_memory")
	if err != nil || setting.Revision != 1 || string(setting.Body) != `{"enabled": true}` && string(setting.Body) != `{"enabled":true}` {
		t.Fatalf("read authorized memory setting = %+v, err=%v", setting, err)
	}

	wrongRun, err := schedule.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	wrongTask, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, ownerID, runID, taskID string
	}{
		{"wrong run", owner.ID, wrongRun, claimed.ID},
		{"wrong task", owner.ID, run.ID, wrongTask},
		{"wrong owner", otherOwner.ID, run.ID, claimed.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := store.ListScheduledPersonalContext(ctx, test.ownerID, test.runID, test.taskID, true, 10); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("mismatched schedule context authorization = %v, want unauthenticated", err)
			}
		})
	}
	if _, err := store.GetScheduledPersonalContextObject(ctx, otherOwner.ID, run.ID, claimed.ID, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("cross-owner scheduled personal read = %v, want unauthenticated", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',finished_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetScheduledPersonalContextObject(ctx, owner.ID, run.ID, claimed.ID, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("terminal scheduled Task retained personal read access = %v", err)
	}
}

func seedCompletedAssistantContextMessage(t *testing.T, store *Store, source app.TaskSubmissionResult, content string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',lease_owner='',lease_expires_at=NULL,
finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, source.Task.ID); err != nil {
		t.Fatalf("complete context source Task: %v", err)
	}
	var sequence int64
	if err := store.pool.QueryRow(ctx, `UPDATE conversations SET message_seq=message_seq+1
WHERE id=$1 AND owner_id=$2 RETURNING message_seq`, source.Conversation.ID, source.Conversation.OwnerID).Scan(&sequence); err != nil {
		t.Fatalf("reserve context assistant sequence: %v", err)
	}
	messageID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,model_id,source_kind,sequence)
VALUES ($1,$2,$3,'assistant',$4,$5,$6,'task',$7)`, messageID, source.Conversation.ID, source.Conversation.OwnerID,
		content, source.Task.ID, source.Task.ModelID, sequence); err != nil {
		t.Fatalf("insert completed context assistant message: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,message_sequence)
VALUES ($1,$2,$3,$4,'completed',$5)`, source.Task.ID, messageID, turnID, content, sequence); err != nil {
		t.Fatalf("insert completed assistant snapshot: %v", err)
	}
	return messageID
}

func seedInterruptedAssistantContextMessage(t *testing.T, store *Store, source app.TaskSubmissionResult, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',lease_owner='',lease_expires_at=NULL,
finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, source.Task.ID); err != nil {
		t.Fatalf("complete interrupted context source Task: %v", err)
	}
	var sequence int64
	if err := store.pool.QueryRow(ctx, `UPDATE conversations SET message_seq=message_seq+1
WHERE id=$1 AND owner_id=$2 RETURNING message_seq`, source.Conversation.ID, source.Conversation.OwnerID).Scan(&sequence); err != nil {
		t.Fatalf("reserve interrupted assistant sequence: %v", err)
	}
	messageID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,model_id,source_kind,sequence)
VALUES ($1,$2,$3,'assistant',$4,$5,$6,'task',$7)`, messageID, source.Conversation.ID, source.Conversation.OwnerID,
		content, source.Task.ID, source.Task.ModelID, sequence); err != nil {
		t.Fatalf("insert interrupted assistant row: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,message_sequence)
VALUES ($1,$2,$3,$4,'interrupted',NULL)`, source.Task.ID, messageID, turnID, content); err != nil {
		t.Fatalf("insert interrupted assistant snapshot: %v", err)
	}
}

func readTaskConversationContextManifest(t *testing.T, ctx context.Context, store *Store, taskID string) taskConversationContextManifest {
	t.Helper()
	var payload []byte
	if err := store.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND sequence=1`, taskID).Scan(&payload); err != nil {
		t.Fatalf("read initial Task event: %v", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode initial Task event: %v", err)
	}
	var manifest taskConversationContextManifest
	if err := json.Unmarshal(event["conversation_context"], &manifest); err != nil {
		t.Fatalf("decode context manifest: %v", err)
	}
	return manifest
}
