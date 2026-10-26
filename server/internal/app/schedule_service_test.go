package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleStoreStub struct {
	ScheduleStore
	created      schedule.Schedule
	createRecord IdempotencyRecord
	createCalls  int
	updated      schedule.Schedule
	updateCalls  int
	due          []schedule.Schedule
	commit       ScheduleOccurrenceCommit
	commitCalls  int
	commitOK     bool
	stored       schedule.Schedule
	setStatus    schedule.Status
	setVersion   int64
	setCalls     int
	taskDetails  TaskDetails
	actionRecord IdempotencyRecord
	actionID     string
	actionCalls  int
	err          error
}

func (s *scheduleStoreStub) CreateSchedule(_ context.Context, value schedule.Schedule, record IdempotencyRecord) (schedule.Schedule, bool, error) {
	s.createCalls++
	s.created, s.createRecord = value, record
	if s.err != nil {
		return schedule.Schedule{}, false, s.err
	}
	return value, false, nil
}

func (s *scheduleStoreStub) GetSchedule(_ context.Context, _, _ string) (schedule.Schedule, error) {
	return s.stored, s.err
}

func (s *scheduleStoreStub) UpdateSchedule(_ context.Context, _ string, value schedule.Schedule, expected int64) (schedule.Schedule, error) {
	s.updateCalls++
	s.setVersion = expected
	s.updated = value
	if s.err != nil {
		return schedule.Schedule{}, s.err
	}
	return value, nil
}

func (s *scheduleStoreStub) SetScheduleStatus(_ context.Context, record IdempotencyRecord, _ string, status schedule.Status, expected int64) (schedule.Schedule, error) {
	s.setCalls++
	s.setStatus, s.setVersion = status, expected
	s.actionRecord = record
	value := s.stored
	value.Status = status
	if s.err != nil {
		return schedule.Schedule{}, s.err
	}
	return value, nil
}

func (s *scheduleStoreStub) ListDueSchedules(context.Context, time.Time, int32) ([]schedule.Schedule, error) {
	return s.due, s.err
}

func (s *scheduleStoreStub) CommitScheduleOccurrence(_ context.Context, value ScheduleOccurrenceCommit) (schedule.Run, bool, error) {
	s.commitCalls++
	s.commit = value
	return schedule.Run{}, s.commitOK, s.err
}

func (s *scheduleStoreStub) GetTask(context.Context, string, string) (TaskDetails, error) {
	return s.taskDetails, s.err
}

func (s *scheduleStoreStub) RequestTaskAction(_ context.Context, record IdempotencyRecord, _ string, _ task.Action, _ task.Status, requestID string) (TaskActionResult, error) {
	s.actionCalls++
	s.actionRecord, s.actionID = record, requestID
	return TaskActionResult{}, s.err
}

type scheduleAuthorizerStub struct {
	calls   int
	owner   string
	kind    identity.Kind
	tokenID string
	hostID  string
	scopes  []identity.Scope
	require identity.Scope
	err     error
}

func (a *scheduleAuthorizerStub) AuthorizeTaskAction(_ context.Context, owner string, kind identity.Kind, tokenID, hostID string, scopes []identity.Scope, required identity.Scope) error {
	a.calls++
	a.owner, a.kind, a.tokenID, a.hostID, a.require = owner, kind, tokenID, hostID, required
	a.scopes = append([]identity.Scope(nil), scopes...)
	return a.err
}

func TestScheduleCreateAcceptsScheduleManageWithoutTaskSubmitAndStoresSnapshot(t *testing.T) {
	store := &scheduleStoreStub{}
	authorizer := &scheduleAuthorizerStub{}
	service, err := NewScheduleService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeScheduleManage, identity.ScopeComputerObserve},
	}
	value, err := service.Create(context.Background(), principal, CreateScheduleCommand{
		HostID: "h1234567", ModelID: "model-a", Objective: "run a report",
		Timing:      schedule.Timing{Kind: schedule.TimingOnce, RunAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), TimeZone: "UTC"},
		Observation: &schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText},
	}, "schedule-create-1")
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.ValidID(value.ID) || store.createCalls != 1 || store.created.Authorization.Kind != string(identity.KindAPIKey) ||
		store.created.Authorization.TokenID != principal.TokenID || len(store.created.Authorization.Scopes) != 2 {
		t.Fatalf("created schedule authorization snapshot = %#v; stored=%#v calls=%d", value.Authorization, store.created, store.createCalls)
	}
	if store.created.Observation == nil || store.created.ObservationConfigHash != store.created.Observation.ConfigHash() || store.created.BaselineFingerprint != "" {
		t.Fatalf("created schedule observation state = %#v", store.created)
	}
	if authorizer.require != identity.ScopeScheduleManage || authorizer.calls != 1 {
		t.Fatalf("authorization call = %#v; expected schedule manage", authorizer)
	}
}

func TestScheduleUpdateResetsObservationBaselineOnlyWhenConfigurationChanges(t *testing.T) {
	old := &schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText}
	value := schedule.Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 2, Status: schedule.StatusEnabled,
		HostID: "h1234567", ModelID: "model-a", Objective: "inspect status",
		Timing:        schedule.Timing{Kind: schedule.TimingOnce, RunAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), TimeZone: "UTC"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 1000},
		Authorization: task.Authorization{Kind: string(identity.KindSession)}, MisfirePolicy: schedule.MisfireSkip,
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted}, Observation: old,
		ObservationConfigHash: old.ConfigHash(), BaselineFingerprint: strings.Repeat("a", 64),
	}
	store := &scheduleStoreStub{stored: value}
	service, err := NewScheduleService(store, &scheduleAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: value.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}

	unchanged := *old
	if _, err := service.Update(context.Background(), principal, value.ID, UpdateScheduleCommand{ExpectedVersion: value.Version, ObservationSet: true, Observation: &unchanged}); err != nil {
		t.Fatal(err)
	}
	if store.updated.BaselineFingerprint != value.BaselineFingerprint || store.updated.ObservationConfigHash != value.ObservationConfigHash {
		t.Fatalf("unchanged observation config lost baseline: %#v", store.updated)
	}

	changed := *old
	changed.Selector = ".status"
	if _, err := service.Update(context.Background(), principal, value.ID, UpdateScheduleCommand{ExpectedVersion: value.Version, ObservationSet: true, Observation: &changed}); err != nil {
		t.Fatal(err)
	}
	if store.updated.BaselineFingerprint != "" || store.updated.ObservationConfigHash != changed.ConfigHash() {
		t.Fatalf("changed observation config did not reset baseline: %#v", store.updated)
	}

	if _, err := service.Update(context.Background(), principal, value.ID, UpdateScheduleCommand{ExpectedVersion: value.Version, ObservationSet: true}); err != nil {
		t.Fatal(err)
	}
	if store.updated.Observation != nil || store.updated.ObservationConfigHash != "" || store.updated.BaselineFingerprint != "" {
		t.Fatalf("cleared observation retained state: %#v", store.updated)
	}
}

func TestScheduleProcessDueRechecksSnapshotAndCommitsNotifications(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(-20 * time.Second)
	next, err := schedule.FirstOccurrence(schedule.Timing{
		Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &scheduleStoreStub{commitOK: true, due: []schedule.Schedule{{
		ID: "j1234567", OwnerID: "u1234567", Version: 2, Status: schedule.StatusEnabled,
		HostID: "h1234567", ModelID: "model-a", Objective: "scheduled task", Timing: schedule.Timing{
			Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC",
		}, Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Authorization: task.Authorization{Kind: string(identity.KindAPIKey), TokenID: "t1234567", Scopes: []string{
			string(identity.ScopeScheduleManage), string(identity.ScopeComputerObserve),
		}}, MisfirePolicy: schedule.MisfireRunOnce,
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyFailed},
		NextOccurrenceKey:  next.Key, NextRunAt: next.ScheduledAt, NextCheckAt: &next.CheckAt, ExecutionEpoch: 4,
	}}}
	authorizer := &scheduleAuthorizerStub{}
	service, err := NewScheduleService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessDue(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessDue() = %t, %v", processed, err)
	}
	if authorizer.calls != 1 || authorizer.kind != identity.KindAPIKey || authorizer.tokenID != "t1234567" ||
		authorizer.require != identity.ScopeScheduleManage || authorizer.hostID != "h1234567" {
		t.Fatalf("scheduled token recheck = %#v", authorizer)
	}
	if store.commitCalls != 1 || store.commit.Occurrence.Status != schedule.RunQueued ||
		len(store.commit.NotificationEvents) != 1 || store.commit.NotificationEvents[0] != schedule.NotifyFailed {
		t.Fatalf("committed occurrence = %#v, calls=%d", store.commit, store.commitCalls)
	}
}

func TestScheduleProcessDueRejectsRevokedAPIKeyBeforeCreatingTask(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	scheduled := now.Add(-20 * time.Second)
	timing := schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"}
	next, err := schedule.FirstOccurrence(timing, now)
	if err != nil {
		t.Fatal(err)
	}

	store := &scheduleStoreStub{commitOK: true, due: []schedule.Schedule{{
		ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: schedule.StatusEnabled,
		HostID: "h1234567", ModelID: "model-a", Objective: "check the project",
		Timing: timing, Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Authorization: task.Authorization{Kind: string(identity.KindAPIKey), TokenID: "t1234567", Scopes: []string{
			string(identity.ScopeScheduleManage),
		}},
		MisfirePolicy: schedule.MisfireRunOnce, NextOccurrenceKey: next.Key,
		NextRunAt: next.ScheduledAt, NextCheckAt: &next.CheckAt, ExecutionEpoch: 1,
	}}}

	authorizer, _, hosts, _, tokens, _ := newCredentialFixture(t)
	hosts.values = append(hosts.values, host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning})
	tokens.values["t1234567"] = identity.Token{
		ID: "t1234567", OwnerID: "u1234567", Kind: identity.KindAPIKey, HostIDs: []string{"h1234567"},
		Scopes: []identity.Scope{identity.ScopeScheduleManage}, ExpiresAt: now.Add(time.Hour),
	}
	if err := tokens.RevokeToken(context.Background(), "t1234567", "u1234567"); err != nil {
		t.Fatal(err)
	}

	service, err := NewScheduleService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessDue(context.Background())
	if err != nil || !processed || store.commitCalls != 1 ||
		store.commit.Occurrence.Status != schedule.RunSkipped || store.commit.Occurrence.SkipReason == nil ||
		*store.commit.Occurrence.SkipReason != schedule.SkipAuthorizationEnded {
		t.Fatalf("revoked-key occurrence = processed %t, error %v, commit %#v, calls %d", processed, err, store.commit.Occurrence, store.commitCalls)
	}
}

func TestScheduleProcessDueAppliesMisfirePolicyOnlyAfterGrace(t *testing.T) {
	for _, test := range []struct {
		policy schedule.MisfirePolicy
		want   schedule.RunStatus
	}{
		{policy: schedule.MisfireSkip, want: schedule.RunMissed},
		{policy: schedule.MisfireRunOnce, want: schedule.RunQueued},
	} {
		t.Run(string(test.policy), func(t *testing.T) {
			scheduled := time.Now().UTC().Add(-schedule.MisfireGrace - time.Minute).Truncate(time.Second)
			next, err := schedule.FirstOccurrence(schedule.Timing{
				Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC",
			}, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			store := &scheduleStoreStub{commitOK: true, due: []schedule.Schedule{{
				ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: schedule.StatusEnabled,
				HostID: "h1234567", ModelID: "model-a", Objective: "scheduled task",
				Timing:        schedule.Timing{Kind: schedule.TimingOnce, RunAt: scheduled.Format(time.RFC3339), TimeZone: "UTC"},
				Budget:        task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
				Authorization: task.Authorization{Kind: string(identity.KindSession)}, MisfirePolicy: test.policy,
				NextOccurrenceKey: next.Key, NextRunAt: next.ScheduledAt, NextCheckAt: &next.CheckAt, ExecutionEpoch: 2,
			}}}
			authorizer := &scheduleAuthorizerStub{}
			service, err := NewScheduleService(store, authorizer, &modelCatalogStub{configured: true})
			if err != nil {
				t.Fatal(err)
			}
			processed, err := service.ProcessDue(context.Background())
			if err != nil || !processed || store.commit.Occurrence.Status != test.want {
				t.Fatalf("ProcessDue() = %t, %v; occurrence=%#v want status %s", processed, err, store.commit.Occurrence, test.want)
			}
			if test.policy == schedule.MisfireSkip && authorizer.calls != 0 {
				t.Fatalf("missed occurrence unexpectedly rechecked task authorization %d times", authorizer.calls)
			}
		})
	}
}

func TestScheduleProcessDueKeepsDSTGapLedgerIndependentOfMisfire(t *testing.T) {
	timing := schedule.Timing{Kind: schedule.TimingDaily, StartsOn: "2026-03-08", LocalTime: "02:30", TimeZone: "America/New_York"}
	occurrence, err := schedule.FirstOccurrence(timing, time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC))
	if err != nil || occurrence.SkipReason == nil || *occurrence.SkipReason != schedule.SkipDSTGap {
		t.Fatalf("DST gap occurrence = %#v, %v", occurrence, err)
	}
	store := &scheduleStoreStub{commitOK: true, due: []schedule.Schedule{{
		ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: schedule.StatusEnabled,
		HostID: "h1234567", ModelID: "model-a", Objective: "scheduled task", Timing: timing,
		Budget:        task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		Authorization: task.Authorization{Kind: string(identity.KindSession)}, MisfirePolicy: schedule.MisfireSkip,
		NextOccurrenceKey: occurrence.Key, NextCheckAt: &occurrence.CheckAt, ExecutionEpoch: 2,
	}}}
	authorizer := &scheduleAuthorizerStub{}
	service, err := NewScheduleService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessDue(context.Background())
	if err != nil || !processed || store.commit.Occurrence.Status != schedule.RunSkipped ||
		store.commit.Occurrence.SkipReason == nil || *store.commit.Occurrence.SkipReason != schedule.SkipDSTGap || authorizer.calls != 0 {
		t.Fatalf("DST gap processing = processed=%t err=%v commit=%#v authCalls=%d", processed, err, store.commit, authorizer.calls)
	}
}

func TestScheduleActionCancellationKeyFitsMaximumIdempotencyKey(t *testing.T) {
	current := schedule.Run{
		ID: "y1234567", ScheduleID: "j1234567", Status: schedule.RunRunning,
		TaskID: stringPointer("q1234567"),
	}
	latest := schedule.Run{
		ID: "y7654321", ScheduleID: "j1234567", Status: schedule.RunSkipped,
		SkipReason: skipReasonPointer(schedule.SkipOverlap),
	}
	store := &scheduleStoreStub{
		stored: schedule.Schedule{ID: "j1234567", OwnerID: "u1234567", Version: 3, Status: schedule.StatusEnabled,
			HostID: "h1234567", LastRun: &latest, CurrentRun: &current},
		taskDetails: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", Status: task.StatusRunning}},
	}
	service, err := NewScheduleService(store, &scheduleAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 128)
	result, err := service.Action(context.Background(), taskTestPrincipal(), "j1234567", ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: 3, CurrentRunAction: schedule.CurrentRunCancel,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if store.setCalls != 1 || store.actionCalls != 1 || len(store.actionRecord.Key) > 128 || len(store.actionRecord.Key) != 64 ||
		store.actionRecord.Operation != "task.action.cancel" || result.CurrentRun == nil || result.CurrentRun.ID != current.ID ||
		result.CurrentRun.Status != schedule.RunStopping || result.Schedule.LastRun == nil || result.Schedule.LastRun.ID != latest.ID {
		t.Fatalf("cancel delegation = key=%q action=%#v result=%#v", store.actionRecord.Key, store.actionID, result)
	}
}

func stringPointer(value string) *string { return &value }

func skipReasonPointer(value schedule.SkipReason) *schedule.SkipReason { return &value }
