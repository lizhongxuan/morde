package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestPostgresScheduleOccurrenceAtomicallyCreatesTaskGraphAndDeduplicatesConcurrentPolls(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	scheduled := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	value, createRecord := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireRunOnce,
		[]schedule.NotificationEvent{schedule.NotifyFailed})
	next, err := schedule.FirstOccurrence(timing, time.Now().UTC())
	if err != nil || next.Key != value.NextOccurrenceKey {
		t.Fatalf("created next occurrence = %#v, %v; schedule=%#v", next, err, value)
	}
	commit := app.ScheduleOccurrenceCommit{
		ScheduleID: value.ID, OwnerID: owner.ID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
		Occurrence:         schedule.Occurrence{Key: next.Key, ScheduledAt: next.ScheduledAt, CheckAt: next.CheckAt, Status: schedule.RunQueued},
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyFailed},
	}
	start := make(chan struct{})
	type outcome struct {
		run       schedule.Run
		committed bool
		err       error
	}
	results := make(chan outcome, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			run, committed, err := store.CommitScheduleOccurrence(ctx, commit)
			results <- outcome{run: run, committed: committed, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)
	var run schedule.Run
	committedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("commit concurrent schedule occurrence: %v", result.err)
		}
		if result.committed {
			committedCount++
			run = result.run
		}
	}
	if committedCount != 1 || run.TaskID == nil || run.Status != schedule.RunQueued || run.ScheduleVersion != value.Version ||
		len(run.NotificationEvents) != 1 || run.NotificationEvents[0] != schedule.NotifyFailed {
		t.Fatalf("concurrent commit result = committed %d times, run=%#v", committedCount, run)
	}
	var conversations, tasks, messages, events, runs int
	if err := store.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversations WHERE owner_id=$1),
(SELECT count(*) FROM tasks WHERE owner_id=$1),
(SELECT count(*) FROM conversation_messages WHERE owner_id=$1),
(SELECT count(*) FROM task_events e JOIN tasks t ON t.id=e.task_id WHERE t.owner_id=$1),
(SELECT count(*) FROM schedule_runs WHERE schedule_id=$2)`, owner.ID, value.ID).
		Scan(&conversations, &tasks, &messages, &events, &runs); err != nil {
		t.Fatal(err)
	}
	if conversations != 1 || tasks != 1 || messages != 1 || events != 1 || runs != 1 {
		t.Fatalf("atomic schedule task graph counts = conversations:%d tasks:%d messages:%d events:%d runs:%d", conversations, tasks, messages, events, runs)
	}
	var conversationID, messageID string
	if err := store.pool.QueryRow(ctx, `SELECT c.id, m.id FROM conversations c
JOIN conversation_messages m ON m.conversation_id=c.id AND m.owner_id=c.owner_id
WHERE c.owner_id=$1 AND m.task_id=$2`, owner.ID, *run.TaskID).Scan(&conversationID, &messageID); err != nil {
		t.Fatal(err)
	}
	assertAccountChangeLog(t, ctx, store, owner.ID,
		expectedAccountChange{objectType: "schedule", objectID: value.ID, revision: value.Version},
		expectedAccountChange{objectType: "conversation", objectID: conversationID, revision: 1},
		expectedAccountChange{objectType: "message", objectID: messageID, revision: 1},
		expectedAccountChange{objectType: "schedule", objectID: value.ID, revision: value.Version + 1},
	)
	var authKind string
	var executionEpoch int64
	if err := store.pool.QueryRow(ctx, `SELECT auth_kind,execution_epoch FROM tasks WHERE id=$1`, *run.TaskID).Scan(&authKind, &executionEpoch); err != nil {
		t.Fatal(err)
	}
	if authKind != string(identity.KindSession) || executionEpoch != value.ExecutionEpoch {
		t.Fatalf("scheduled task authorization/epoch = %s/%d, want session/%d", authKind, executionEpoch, value.ExecutionEpoch)
	}
	loaded, err := store.GetSchedule(ctx, owner.ID, value.ID)
	if err != nil || loaded.Status != schedule.StatusCompleted || loaded.Version != value.Version+1 || loaded.LastRun == nil || loaded.LastRun.ID != run.ID {
		t.Fatalf("completed schedule = %#v, %v", loaded, err)
	}
	assertScheduleSyncRevision(t, store, owner.ID, value.ID, loaded.Version, 4)
	runsPage, err := store.ListScheduleRuns(ctx, owner.ID, value.ID, nil, app.PageRequest{Limit: 10})
	if err != nil || len(runsPage.Items) != 1 || runsPage.Items[0].TaskID == nil || *runsPage.Items[0].TaskID != *run.TaskID {
		t.Fatalf("schedule run history = %#v, %v", runsPage, err)
	}
	page, err := store.ListSchedules(ctx, owner.ID, nil, schedule.StatusCompleted, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != value.ID {
		t.Fatalf("schedule page = %#v, %v", page, err)
	}
	replayed, wasReplay, err := store.CreateSchedule(ctx, value, createRecord)
	if err != nil || !wasReplay || replayed.ID != value.ID || replayed.Status != schedule.StatusCompleted {
		t.Fatalf("schedule create replay = %#v replay=%t err=%v", replayed, wasReplay, err)
	}
}

func TestPostgresScheduleObservationConfigAndRunSnapshotPersist(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	scheduled := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireRunOnce,
		[]schedule.NotificationEvent{schedule.NotifyCompleted})
	observation := &schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText}
	value.Observation, value.ObservationConfigHash = observation, observation.ConfigHash()
	updated, err := store.UpdateSchedule(ctx, owner.ID, value, value.Version)
	if err != nil || updated.Observation == nil || updated.Observation.ConfigHash() != updated.ObservationConfigHash {
		t.Fatalf("persist schedule observation = %#v, %v", updated, err)
	}
	for _, invalid := range []string{
		`{"url":"https://example.test/status","selector":"#status","field":null}`,
		`{"url":"https://example.test/status","selector":"` + strings.Repeat("#", 1025) + `","field":"text"}`,
	} {
		if _, err := store.pool.Exec(ctx, `UPDATE schedules SET observation_spec = $1::jsonb WHERE id = $2`, invalid, updated.ID); err == nil {
			t.Fatalf("invalid persisted observation spec accepted: %s", invalid)
		}
	}
	var scheduledContentBytes int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&scheduledContentBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=used_bytes WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	run, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
		ScheduleID: updated.ID, OwnerID: owner.ID, ExpectedVersion: updated.Version, ExpectedEpoch: updated.ExecutionEpoch,
		Occurrence:         schedule.Occurrence{Key: updated.NextOccurrenceKey, ScheduledAt: updated.NextRunAt, CheckAt: *updated.NextCheckAt, Status: schedule.RunQueued},
		NotificationEvents: updated.NotificationEvents,
	})
	if err != nil || !committed || run.Observation == nil || run.Observation.Selector != "#status" ||
		run.ObservationConfigHash != observation.ConfigHash() || run.ObservationOutcome != schedule.ObservationUnknown {
		t.Fatalf("commit observation run snapshot = %#v committed=%t err=%v", run, committed, err)
	}
	var usageAfterRun int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usageAfterRun); err != nil || usageAfterRun != scheduledContentBytes {
		t.Fatalf("schedule occurrence changed personal content usage = %d, want %d, err=%v", usageAfterRun, scheduledContentBytes, err)
	}
	page, err := store.ListScheduleRuns(ctx, owner.ID, updated.ID, nil, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Observation == nil || page.Items[0].Observation.URL != observation.URL ||
		page.Items[0].ObservationOutcome != schedule.ObservationUnknown {
		t.Fatalf("load observation run snapshot = %#v, %v", page, err)
	}
}

func TestPostgresScheduleOverlapCreatesSkipWithoutSecondTask(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	active, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "schedule-overlap-task", "existing scheduled task"))
	if err != nil {
		t.Fatal(err)
	}
	scheduled := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyCompleted})
	priorRunID := mustID(t, schedule.NewRunID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO schedule_runs
(id,schedule_id,owner_id,schedule_version,occurrence_key,scheduled_at,status,task_id,notification_events)
VALUES ($1,$2,$3,$4,$5,$6,'queued',$7,$8)`, priorRunID, value.ID, owner.ID, value.Version,
		"previous-occurrence", scheduled.Add(-24*time.Hour), active.Task.ID, []string{"completed"}); err != nil {
		t.Fatal(err)
	}
	next := schedule.Occurrence{Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt, Status: schedule.RunQueued}
	run, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
		ScheduleID: value.ID, OwnerID: owner.ID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
		Occurrence: next, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
	})
	if err != nil || !committed || run.Status != schedule.RunSkipped || run.SkipReason == nil || *run.SkipReason != schedule.SkipOverlap || run.TaskID != nil {
		t.Fatalf("overlap commit = %#v committed=%t err=%v", run, committed, err)
	}
	if run.FinishedAt == nil {
		t.Fatal("overlap skip should be recorded as a finished run")
	}
	loaded, err := store.GetSchedule(ctx, owner.ID, value.ID)
	if err != nil || loaded.LastRun == nil || loaded.LastRun.ID != run.ID || loaded.LastRun.Status != schedule.RunSkipped ||
		loaded.CurrentRun == nil || loaded.CurrentRun.TaskID == nil || *loaded.CurrentRun.TaskID != active.Task.ID {
		t.Fatalf("overlap schedule last/current runs = latest %#v current %#v err=%v", loaded.LastRun, loaded.CurrentRun, err)
	}
	var taskCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE owner_id=$1`, owner.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("tasks after overlap skip = %d, err=%v", taskCount, err)
	}
}

func TestPostgresSkippedScheduleDoesNotBlockLaterDueSchedules(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	events := []schedule.NotificationEvent{schedule.NotifyCompleted}

	revokedTime := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	revokedTiming := schedule.Timing{Kind: schedule.TimingOnce, RunAt: revokedTime.Format(time.RFC3339), TimeZone: "UTC"}
	revoked, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, revokedTiming, schedule.MisfireSkip, events)
	if _, err := store.pool.Exec(ctx, `UPDATE schedules SET auth_kind='api_key',auth_token_id=$2,auth_scopes=$3 WHERE id=$1`,
		revoked.ID, "t1234567", []string{string(identity.ScopeScheduleManage)}); err != nil {
		t.Fatal(err)
	}

	validTime := revokedTime.Add(time.Second)
	validTiming := schedule.Timing{Kind: schedule.TimingOnce, RunAt: validTime.Format(time.RFC3339), TimeZone: "UTC"}
	valid, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, validTiming, schedule.MisfireSkip, events)

	// The scheduler is not currently wired; this checks the storage boundary for its decision.
	for _, value := range []schedule.Schedule{revoked, valid} {
		due, err := store.ListDueSchedules(ctx, time.Now().UTC(), 1)
		if err != nil || len(due) != 1 || due[0].ID != value.ID {
			t.Fatalf("due schedule = %#v, %v; want %s", due, err, value.ID)
		}
		occurrence := schedule.Occurrence{Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt, Status: schedule.RunQueued}
		if value.ID == revoked.ID {
			reason := schedule.SkipAuthorizationEnded
			occurrence.Status, occurrence.SkipReason = schedule.RunSkipped, &reason
		}
		_, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
			ScheduleID: value.ID, OwnerID: owner.ID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
			Occurrence: occurrence, NotificationEvents: events,
		})
		if err != nil || !committed {
			t.Fatalf("commit occurrence %s = %t, %v", value.ID, committed, err)
		}
	}

	revokedResult, err := store.GetSchedule(ctx, owner.ID, revoked.ID)
	if err != nil || revokedResult.Status != schedule.StatusCompleted || revokedResult.LastRun == nil ||
		revokedResult.LastRun.Status != schedule.RunSkipped || revokedResult.LastRun.SkipReason == nil ||
		*revokedResult.LastRun.SkipReason != schedule.SkipAuthorizationEnded || revokedResult.LastRun.TaskID != nil {
		t.Fatalf("revoked schedule result = %#v, %v", revokedResult, err)
	}
	validResult, err := store.GetSchedule(ctx, owner.ID, valid.ID)
	if err != nil || validResult.Status != schedule.StatusCompleted || validResult.LastRun == nil ||
		validResult.LastRun.Status != schedule.RunQueued || validResult.LastRun.TaskID == nil {
		t.Fatalf("later schedule result = %#v, %v", validResult, err)
	}
	var tasks int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE owner_id=$1`, owner.ID).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("task count after revoked and valid schedules = %d, %v", tasks, err)
	}
}

func TestPostgresScheduleTriggerSerializesWithStopAllBarrier(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	scheduled := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyFailed})
	occurrence := schedule.Occurrence{Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt, Status: schedule.RunQueued}
	start := make(chan struct{})
	type triggerResult struct {
		run       schedule.Run
		committed bool
		err       error
	}
	triggerDone := make(chan triggerResult, 1)
	stopDone := make(chan error, 1)
	go func() {
		<-start
		run, committed, err := store.CommitScheduleOccurrence(ctx, app.ScheduleOccurrenceCommit{
			ScheduleID: value.ID, OwnerID: owner.ID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
			Occurrence: occurrence, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyFailed},
		})
		triggerDone <- triggerResult{run: run, committed: committed, err: err}
	}()
	go func() {
		<-start
		_, err := store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "schedule-stop-all-race"), 1)
		stopDone <- err
	}()
	close(start)
	trigger := <-triggerDone
	if err := <-stopDone; err != nil {
		t.Fatalf("stop-all during scheduled trigger: %v", err)
	}
	if trigger.err != nil && domain.CodeOf(trigger.err) != domain.CodeConflict {
		t.Fatalf("scheduled trigger race returned unexpected error: %v", trigger.err)
	}
	var status string
	var nextKey string
	if err := store.pool.QueryRow(ctx, `SELECT status,next_occurrence_key FROM schedules WHERE id=$1`, value.ID).Scan(&status, &nextKey); err != nil {
		t.Fatal(err)
	}
	var runCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM schedule_runs WHERE schedule_id=$1 AND occurrence_key=$2`, value.ID, occurrence.Key).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if trigger.committed {
		if trigger.run.TaskID == nil || runCount != 1 {
			t.Fatalf("trigger reported committed without one run/task: %#v, rows=%d", trigger.run, runCount)
		}
		if status != string(schedule.StatusCompleted) || nextKey != "" {
			t.Fatalf("one-time trigger linearized before stop-all but final schedule = status %s cursor %q", status, nextKey)
		}
		var taskStatus string
		if err := store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, *trigger.run.TaskID).Scan(&taskStatus); err != nil {
			t.Fatal(err)
		}
		if taskStatus != string(task.StatusCancelling) && taskStatus != string(task.StatusCancelled) {
			t.Fatalf("scheduled task escaped stop barrier with status %s", taskStatus)
		}
	} else {
		if runCount != 0 {
			t.Fatalf("uncommitted scheduled trigger left %d occurrence rows", runCount)
		}
		if status != string(schedule.StatusDisabled) || nextKey != "" {
			t.Fatalf("stop-all linearized before trigger but final schedule = status %s cursor %q", status, nextKey)
		}
	}
}

func TestPostgresScheduleEnableDisableUsesOptimisticVersionAndRecomputesNextRun(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	timing := schedule.Timing{Kind: schedule.TimingDaily, StartsOn: time.Now().UTC().Format("2006-01-02"), LocalTime: "23:59", TimeZone: "UTC"}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyWaitingUser})
	disabled, err := store.SetScheduleStatus(ctx, scheduleActionIdempotency(owner.ID, value.ID, "schedule-disable-key"), value.ID,
		schedule.StatusDisabled, value.Version, value.Authorization)
	if err != nil || disabled.Status != schedule.StatusDisabled || disabled.Version != value.Version+1 || disabled.NextCheckAt != nil || disabled.NextOccurrenceKey != "" {
		t.Fatalf("disabled schedule = %#v, %v", disabled, err)
	}
	enabled, err := store.SetScheduleStatus(ctx, scheduleActionIdempotency(owner.ID, value.ID, "schedule-enable-key"), value.ID,
		schedule.StatusEnabled, disabled.Version, value.Authorization)
	if err != nil || enabled.Status != schedule.StatusEnabled || enabled.Version != disabled.Version+1 || enabled.NextCheckAt == nil || enabled.NextOccurrenceKey == "" {
		t.Fatalf("re-enabled schedule = %#v, %v", enabled, err)
	}
	if _, err := store.SetScheduleStatus(ctx, scheduleActionIdempotency(owner.ID, value.ID, "schedule-stale-key"), value.ID,
		schedule.StatusDisabled, value.Version, value.Authorization); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale schedule version error = %v", err)
	}
}

func TestPostgresScheduleDefinitionsPublishBusinessVersionsAndSyncProjection(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	timing := schedule.Timing{
		Kind: schedule.TimingDaily, StartsOn: time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"),
		LocalTime: "10:00", TimeZone: "UTC",
	}
	value, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyCompleted})
	assertScheduleSyncRevision(t, store, owner.ID, value.ID, value.Version, 1)

	value.Objective = "updated scheduled objective"
	updated, err := store.UpdateSchedule(ctx, owner.ID, value, value.Version)
	if err != nil || updated.Version != value.Version+1 {
		t.Fatalf("update schedule definition = %#v, %v", updated, err)
	}
	assertScheduleSyncRevision(t, store, owner.ID, updated.ID, updated.Version, 2)

	disabled, err := store.SetScheduleStatus(ctx, scheduleActionIdempotency(owner.ID, updated.ID, "sync-disable-key"), updated.ID,
		schedule.StatusDisabled, updated.Version, updated.Authorization)
	if err != nil || disabled.Version != updated.Version+1 {
		t.Fatalf("disable schedule definition = %#v, %v", disabled, err)
	}
	assertScheduleSyncRevision(t, store, owner.ID, disabled.ID, disabled.Version, 3)

	enabled, err := store.SetScheduleStatus(ctx, scheduleActionIdempotency(owner.ID, disabled.ID, "sync-enable-key"), disabled.ID,
		schedule.StatusEnabled, disabled.Version, disabled.Authorization)
	if err != nil || enabled.Version != disabled.Version+1 {
		t.Fatalf("enable schedule definition = %#v, %v", enabled, err)
	}
	assertScheduleSyncRevision(t, store, owner.ID, enabled.ID, enabled.Version, 4)

	changes, err := store.GetPersonalChanges(ctx, principal, 0, nil, 10)
	if err != nil || len(changes.Items) != 4 || changes.ThroughSequence != 4 {
		t.Fatalf("schedule definition changes = %+v, %v", changes, err)
	}
	for i, wantRevision := range []int64{1, 2, 3, 4} {
		change := changes.Items[i]
		if change.ObjectType != "schedule" || change.ObjectID != enabled.ID || change.Revision != wantRevision || change.Operation != "upsert" {
			t.Fatalf("schedule definition change %d = %+v", i, change)
		}
	}

	snapshot, err := store.GetPersonalSnapshotBatch(ctx, principal, nil, 10)
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].ObjectType != "schedule" ||
		snapshot.Items[0].ObjectID != enabled.ID || snapshot.Items[0].Revision != enabled.Version {
		t.Fatalf("schedule snapshot = %+v, %v", snapshot, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Items[0].Body, &body); err != nil {
		t.Fatalf("decode schedule snapshot body: %v", err)
	}
	var objective string
	if err := json.Unmarshal(body["objective"], &objective); err != nil || objective != enabled.Objective {
		t.Fatalf("schedule snapshot objective = %q, %v; body=%s", objective, err, snapshot.Items[0].Body)
	}
	for _, forbidden := range []string{"auth_kind", "auth_token_id", "auth_scopes", "client_installation_id", "authorization", "execution_epoch", "next_run_at"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("schedule sync body leaked %q: %s", forbidden, snapshot.Items[0].Body)
		}
	}
	var duplicateBodies int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_objects WHERE owner_id=$1 AND object_type='schedule' AND object_id=$2`, owner.ID, enabled.ID).Scan(&duplicateBodies); err != nil || duplicateBodies != 0 {
		t.Fatalf("schedule duplicated in personal_objects = %d, %v", duplicateBodies, err)
	}
}

func TestPostgresScheduleDefinitionChangeRollbackAndOwnerIsolation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	ownerA := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ownerB := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	principalA := createPersonalReadSessionPrincipal(t, store, ownerA.ID)
	principalB := createPersonalReadSessionPrincipal(t, store, ownerB.ID)
	hostA := createRunningControlHost(t, ctx, store, ownerA.ID, "ai", 0)
	hostB := createRunningControlHost(t, ctx, store, ownerB.ID, "ai", 0)
	timing := schedule.Timing{Kind: schedule.TimingDaily,
		StartsOn: time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"), LocalTime: "10:00", TimeZone: "UTC"}
	scheduleA, _ := createPostgresSchedule(t, ctx, store, ownerA.ID, hostA.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyCompleted})
	scheduleB, _ := createPostgresSchedule(t, ctx, store, ownerB.ID, hostB.ID, timing, schedule.MisfireSkip,
		[]schedule.NotificationEvent{schedule.NotifyCompleted})

	if _, err := store.pool.Exec(ctx, `UPDATE account_change_counters SET last_sequence=9223372036854775807 WHERE owner_id=$1`, ownerA.ID); err != nil {
		t.Fatal(err)
	}
	changed := scheduleA
	changed.Objective = "must roll back"
	if _, err := store.UpdateSchedule(ctx, ownerA.ID, changed, scheduleA.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("schedule update with exhausted change sequence error = %v, want invalid_state", err)
	}
	rolledBack, err := store.GetSchedule(ctx, ownerA.ID, scheduleA.ID)
	if err != nil || rolledBack.Version != scheduleA.Version || rolledBack.Objective != scheduleA.Objective {
		t.Fatalf("schedule mutation survived sync-log failure = %#v, %v", rolledBack, err)
	}
	assertScheduleSyncRevision(t, store, ownerA.ID, scheduleA.ID, scheduleA.Version, int64(9223372036854775807))

	snapshotA, err := store.GetPersonalSnapshotBatch(ctx, principalA, nil, 10)
	if err != nil || len(snapshotA.Items) != 1 || snapshotA.Items[0].ObjectID != scheduleA.ID {
		t.Fatalf("owner A schedule snapshot = %+v, %v", snapshotA, err)
	}
	snapshotB, err := store.GetPersonalSnapshotBatch(ctx, principalB, nil, 10)
	if err != nil || len(snapshotB.Items) != 1 || snapshotB.Items[0].ObjectID != scheduleB.ID {
		t.Fatalf("owner B schedule snapshot = %+v, %v", snapshotB, err)
	}
	changesA, err := store.GetPersonalChanges(ctx, principalA, 0, nil, 10)
	if err != nil || len(changesA.Items) != 1 || changesA.Items[0].ObjectID != scheduleA.ID {
		t.Fatalf("owner A schedule changes = %+v, %v", changesA, err)
	}
	changesB, err := store.GetPersonalChanges(ctx, principalB, 0, nil, 10)
	if err != nil || len(changesB.Items) != 1 || changesB.Items[0].ObjectID != scheduleB.ID {
		t.Fatalf("owner B schedule changes = %+v, %v", changesB, err)
	}
	if _, err := store.GetSchedule(ctx, ownerA.ID, scheduleB.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner schedule read error = %v, want not_found", err)
	}
}

func assertScheduleSyncRevision(t *testing.T, store *Store, ownerID, scheduleID string, wantRevision, wantSequence int64) {
	t.Helper()
	var revision, sequence, logRevision int64
	var operation string
	if err := store.pool.QueryRow(context.Background(), `SELECT revision FROM account_object_versions
WHERE owner_id=$1 AND object_type='schedule' AND object_id=$2 AND NOT is_deleted`, ownerID, scheduleID).Scan(&revision); err != nil {
		t.Fatalf("read schedule sync revision: %v", err)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT last_sequence FROM account_change_counters WHERE owner_id=$1`, ownerID).Scan(&sequence); err != nil {
		t.Fatalf("read account change sequence: %v", err)
	}
	if err := store.pool.QueryRow(context.Background(), `SELECT revision,operation FROM account_change_log
WHERE owner_id=$1 AND object_type='schedule' AND object_id=$2 ORDER BY change_sequence DESC LIMIT 1`, ownerID, scheduleID).Scan(&logRevision, &operation); err != nil {
		t.Fatalf("read schedule change log: %v", err)
	}
	if revision != wantRevision || logRevision != wantRevision || sequence != wantSequence || operation != "upsert" {
		t.Fatalf("schedule sync state revision=%d log_revision=%d sequence=%d operation=%q, want revision=%d sequence=%d upsert",
			revision, logRevision, sequence, operation, wantRevision, wantSequence)
	}
}

func createPostgresSchedule(t *testing.T, ctx context.Context, store *Store, ownerID, hostID string, timing schedule.Timing,
	policy schedule.MisfirePolicy, events []schedule.NotificationEvent) (schedule.Schedule, app.IdempotencyRecord) {
	t.Helper()
	occurrence, err := schedule.FirstOccurrence(timing, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	id := mustID(t, schedule.NewID)
	value := schedule.Schedule{
		ID: id, OwnerID: ownerID, Version: 1, Status: schedule.StatusEnabled, ExecutionEpoch: 1,
		HostID: hostID, ModelID: "model-test", Objective: "scheduled objective", Timing: timing,
		Budget:        task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: testSessionTokenID(ownerID)}, MisfirePolicy: policy,
		NotificationEvents: events, NextOccurrenceKey: occurrence.Key, NextRunAt: occurrence.ScheduledAt,
		NextCheckAt: &occurrence.CheckAt,
	}
	record := testIdempotency(ownerID, "schedule.create", "schedule-create-"+id)
	record.ResourceID = id
	created, replayed, err := store.CreateSchedule(ctx, value, record)
	if err != nil || replayed || created.ID != id {
		t.Fatalf("create schedule = %#v replayed=%t err=%v", created, replayed, err)
	}
	return created, record
}

func scheduleActionIdempotency(ownerID, scheduleID, key string) app.IdempotencyRecord {
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "schedule.action", Key: key, RequestHash: hashBytes("schedule.action:" + key),
		ResourceID: scheduleID,
	}
}
