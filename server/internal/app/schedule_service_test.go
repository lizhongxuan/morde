package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleServiceStoreFake struct {
	ScheduleStore
	values              map[string]schedule.Schedule
	createCalls         int
	createRecords       []IdempotencyRecord
	createReplay        map[string]scheduleCreateReplay
	listCalls           int
	listOwner           string
	listHostIDs         []string
	listStatus          schedule.Status
	listPage            PageRequest
	listResult          SchedulePage
	dueCalls            int
	dueNow              time.Time
	dueLimit            int32
	dueErr              error
	dueCommits          []ScheduleOccurrenceCommit
	dueCommitKeys       map[string]struct{}
	dueCommitErr        error
	dueTaskCount        int
	getCalls            int
	getOwner            string
	getID               string
	updateCalls         int
	updateOwner         string
	updateRecord        IdempotencyRecord
	updateRecords       []IdempotencyRecord
	updateReplay        map[string]scheduleUpdateReplay
	updatedValue        schedule.Schedule
	updateExpected      int64
	setStatusCalls      int
	statusRecord        IdempotencyRecord
	statusID            string
	statusTarget        schedule.Status
	statusExpected      int64
	statusAuthorization task.Authorization
	statusReplay        map[string]scheduleStatusReplay
	runsCalls           int
	runsOwner           string
	runsID              string
	runsHostIDs         []string
	runsPage            PageRequest
	runsResult          ScheduleRunPage
	taskActionCalls     int
	taskActionRecord    IdempotencyRecord
	taskActionTaskID    string
	taskActionAction    task.Action
	taskActionExpected  task.Status
	taskActionRequestID string
	createErr           error
	getErr              error
	updateErr           error
	statusErr           error
	listErr             error
	runsErr             error
	taskActionErr       error
}

type scheduleCreateReplay struct {
	hash  []byte
	value schedule.Schedule
}

type scheduleStatusReplay struct {
	hash  []byte
	value schedule.Schedule
}

type scheduleUpdateReplay struct {
	hash  []byte
	value schedule.Schedule
}

func newScheduleServiceStoreFake() *scheduleServiceStoreFake {
	return &scheduleServiceStoreFake{
		values:       make(map[string]schedule.Schedule),
		createReplay: make(map[string]scheduleCreateReplay),
		updateReplay: make(map[string]scheduleUpdateReplay),
		statusReplay: make(map[string]scheduleStatusReplay),
	}
}

func (s *scheduleServiceStoreFake) CreateSchedule(_ context.Context, value schedule.Schedule, record IdempotencyRecord) (schedule.Schedule, bool, error) {
	s.createCalls++
	s.createRecords = append(s.createRecords, record)
	if s.createErr != nil {
		return schedule.Schedule{}, false, s.createErr
	}
	key := scheduleServiceIdempotencyMapKey(record)
	if prior, ok := s.createReplay[key]; ok {
		if !bytes.Equal(prior.hash, record.RequestHash) {
			return schedule.Schedule{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused")
		}
		return prior.value, true, nil
	}
	s.values[value.ID] = value
	s.createReplay[key] = scheduleCreateReplay{hash: append([]byte(nil), record.RequestHash...), value: value}
	return value, false, nil
}

func (s *scheduleServiceStoreFake) ListSchedules(_ context.Context, ownerID string, hostIDs []string, status schedule.Status, page PageRequest) (SchedulePage, error) {
	s.listCalls++
	s.listOwner, s.listHostIDs, s.listStatus, s.listPage = ownerID, hostIDs, status, page
	return s.listResult, s.listErr
}

func (s *scheduleServiceStoreFake) ListDueSchedules(_ context.Context, now time.Time, limit int32) ([]schedule.Schedule, error) {
	s.dueCalls++
	s.dueNow, s.dueLimit = now, limit
	if s.dueErr != nil {
		return nil, s.dueErr
	}
	values := make([]schedule.Schedule, 0)
	for _, value := range s.values {
		if value.Status == schedule.StatusEnabled && value.NextCheckAt != nil && !value.NextCheckAt.After(now) {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].NextCheckAt.Equal(*values[j].NextCheckAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].NextCheckAt.Before(*values[j].NextCheckAt)
	})
	if len(values) > int(limit) {
		values = values[:limit]
	}
	return values, nil
}

func (s *scheduleServiceStoreFake) CommitScheduleOccurrence(_ context.Context, commit ScheduleOccurrenceCommit) (schedule.Run, bool, error) {
	s.dueCommits = append(s.dueCommits, commit)
	if s.dueCommitErr != nil {
		return schedule.Run{}, false, s.dueCommitErr
	}
	if s.dueCommitKeys == nil {
		s.dueCommitKeys = make(map[string]struct{})
	}
	key := commit.ScheduleID + "\x00" + commit.Occurrence.Key
	if _, exists := s.dueCommitKeys[key]; exists {
		return schedule.Run{}, false, nil
	}
	value, ok := s.values[commit.ScheduleID]
	if !ok || value.OwnerID != commit.OwnerID || value.Status != schedule.StatusEnabled ||
		value.Version != commit.ExpectedVersion || value.ExecutionEpoch != commit.ExpectedEpoch || value.NextCheckAt == nil ||
		value.NextOccurrenceKey != commit.Occurrence.Key || !commit.Occurrence.CheckAt.Equal(*value.NextCheckAt) {
		return schedule.Run{}, false, nil
	}
	if value.Timing.Kind == schedule.TimingOnce {
		if commit.NextOccurrence != nil {
			return schedule.Run{}, false, errors.New("one-time schedule commit cannot have next occurrence")
		}
		value.Status = schedule.StatusCompleted
		value.Version++
		value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = "", nil, nil
	} else {
		if commit.NextOccurrence == nil {
			return schedule.Run{}, false, errors.New("recurring schedule commit needs next occurrence")
		}
		expectedNext, err := schedule.NextOccurrence(value.Timing, value.NextOccurrenceKey)
		if err != nil {
			return schedule.Run{}, false, err
		}
		if expectedNext.Key != commit.NextOccurrence.Key || !expectedNext.CheckAt.Equal(commit.NextOccurrence.CheckAt) {
			return schedule.Run{}, false, errors.New("recurring schedule commit must advance one occurrence")
		}
		value.NextOccurrenceKey = commit.NextOccurrence.Key
		value.NextRunAt = commit.NextOccurrence.ScheduledAt
		nextCheckAt := commit.NextOccurrence.CheckAt
		value.NextCheckAt = &nextCheckAt
	}
	s.values[value.ID] = value
	s.dueCommitKeys[key] = struct{}{}
	if commit.Occurrence.Status == schedule.RunQueued {
		s.dueTaskCount++
	}
	return schedule.Run{ScheduleID: value.ID, ScheduleVersion: commit.ExpectedVersion,
		OccurrenceKey: commit.Occurrence.Key, ScheduledAt: commit.Occurrence.ScheduledAt,
		CoalescedCount: commit.Occurrence.CoalescedCount, Status: commit.Occurrence.Status,
		SkipReason: commit.Occurrence.SkipReason}, true, nil
}

func (s *scheduleServiceStoreFake) GetSchedule(_ context.Context, ownerID, id string) (schedule.Schedule, error) {
	s.getCalls++
	s.getOwner, s.getID = ownerID, id
	if s.getErr != nil {
		return schedule.Schedule{}, s.getErr
	}
	value, ok := s.values[id]
	if !ok || value.OwnerID != ownerID {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	return value, nil
}

func (s *scheduleServiceStoreFake) UpdateSchedule(_ context.Context, ownerID string, record IdempotencyRecord, value schedule.Schedule, expectedVersion int64) (schedule.Schedule, error) {
	s.updateCalls++
	s.updateOwner, s.updateRecord, s.updatedValue, s.updateExpected = ownerID, record, value, expectedVersion
	s.updateRecords = append(s.updateRecords, record)
	if s.updateErr != nil {
		return schedule.Schedule{}, s.updateErr
	}
	key := scheduleServiceIdempotencyMapKey(record)
	if prior, ok := s.updateReplay[key]; ok {
		if !bytes.Equal(prior.hash, record.RequestHash) {
			return schedule.Schedule{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused")
		}
		return prior.value, nil
	}
	current, ok := s.values[value.ID]
	if !ok || current.OwnerID != ownerID {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if current.Version != expectedVersion {
		return schedule.Schedule{}, domain.NewError(domain.CodeConflict, "schedule version changed")
	}
	value.Version = current.Version + 1
	s.values[value.ID] = value
	s.updatedValue = value
	s.updateReplay[key] = scheduleUpdateReplay{hash: append([]byte(nil), record.RequestHash...), value: value}
	return value, nil
}

func (s *scheduleServiceStoreFake) SetScheduleStatus(_ context.Context, record IdempotencyRecord, id string, target schedule.Status, expectedVersion int64, authorization task.Authorization) (schedule.Schedule, error) {
	s.setStatusCalls++
	s.statusRecord, s.statusID, s.statusTarget = record, id, target
	s.statusExpected, s.statusAuthorization = expectedVersion, authorization
	if s.statusErr != nil {
		return schedule.Schedule{}, s.statusErr
	}
	key := scheduleServiceIdempotencyMapKey(record)
	if prior, ok := s.statusReplay[key]; ok {
		if !bytes.Equal(prior.hash, record.RequestHash) {
			return schedule.Schedule{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused")
		}
		return prior.value, nil
	}
	current, ok := s.values[id]
	if !ok || current.OwnerID != record.OwnerID {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if current.Version != expectedVersion {
		return schedule.Schedule{}, domain.NewError(domain.CodeConflict, "schedule version changed")
	}
	current.Status = target
	current.Version++
	current.Authorization = authorization
	s.values[id] = current
	s.statusReplay[key] = scheduleStatusReplay{hash: append([]byte(nil), record.RequestHash...), value: current}
	return current, nil
}

func (s *scheduleServiceStoreFake) ListScheduleRuns(_ context.Context, ownerID, id string, hostIDs []string, page PageRequest) (ScheduleRunPage, error) {
	s.runsCalls++
	s.runsOwner, s.runsID, s.runsHostIDs, s.runsPage = ownerID, id, hostIDs, page
	if value, ok := s.values[id]; !ok || value.OwnerID != ownerID {
		return ScheduleRunPage{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	return s.runsResult, s.runsErr
}

func (s *scheduleServiceStoreFake) RequestTaskAction(_ context.Context, record IdempotencyRecord, taskID string, action task.Action, expectedStatus task.Status, requestID string) (TaskActionResult, error) {
	s.taskActionCalls++
	s.taskActionRecord, s.taskActionTaskID = record, taskID
	s.taskActionAction, s.taskActionExpected, s.taskActionRequestID = action, expectedStatus, requestID
	return TaskActionResult{}, s.taskActionErr
}

func scheduleServiceIdempotencyMapKey(record IdempotencyRecord) string {
	return record.OwnerID + "\x00" + record.Operation + "\x00" + record.Key
}

type scheduleServiceAuthorizerFake struct {
	actionCalls       int
	ownerID           string
	kind              identity.Kind
	tokenID           string
	hostID            string
	scopes            []identity.Scope
	requiredScope     identity.Scope
	actionErr         error
	hostErrors        map[string]error
	validateCalls     int
	validateOwnerID   string
	validateAuth      task.Authorization
	validateAuthError error
}

type scheduleTaskActionOnlyFake struct{}

func (scheduleTaskActionOnlyFake) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (s *scheduleServiceAuthorizerFake) AuthorizeTaskAction(_ context.Context, ownerID string, kind identity.Kind, tokenID, hostID string, scopes []identity.Scope, required identity.Scope) error {
	s.actionCalls++
	s.ownerID, s.kind, s.tokenID, s.hostID, s.requiredScope = ownerID, kind, tokenID, hostID, required
	s.scopes = append([]identity.Scope(nil), scopes...)
	if err, ok := s.hostErrors[hostID]; ok {
		return err
	}
	return s.actionErr
}

func (s *scheduleServiceAuthorizerFake) ValidateScheduleAuthorization(_ context.Context, ownerID string, authorization task.Authorization) error {
	s.validateCalls++
	s.validateOwnerID, s.validateAuth = ownerID, authorization
	return s.validateAuthError
}

type scheduleServiceModelCatalogFake struct {
	configured bool
	err        error
	calls      int
	ownerID    string
	modelID    string
}

func (s *scheduleServiceModelCatalogFake) IsConfiguredModel(_ context.Context, ownerID, modelID string) (bool, error) {
	s.calls++
	s.ownerID, s.modelID = ownerID, modelID
	return s.configured, s.err
}

func newTestScheduleService(t *testing.T, store *scheduleServiceStoreFake, authorizer *scheduleServiceAuthorizerFake, models *scheduleServiceModelCatalogFake) *ScheduleService {
	t.Helper()
	service, err := NewScheduleService(store, authorizer, models)
	if err != nil {
		t.Fatalf("NewScheduleService(): %v", err)
	}
	return service
}

func validScheduleCommand() CreateScheduleCommand {
	return CreateScheduleCommand{
		HostID: "h1234567", ModelID: "model-a", Objective: "  check the weekly goal  ",
		Timing: validOnceScheduleTiming(),
		Budget: task.Budget{ActiveSeconds: 300, ToolCalls: 12, ModelTokens: 9000},
	}
}

func validOnceScheduleTiming() schedule.Timing {
	runAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	return schedule.Timing{Kind: schedule.TimingOnce, RunAt: runAt.Format(time.RFC3339), TimeZone: "UTC"}
}

func scheduleServicePrincipal() identity.Principal {
	return identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567",
	}
}

func clientSchedulePrincipal(scopes ...identity.Scope) identity.Principal {
	return identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: scopes,
	}
}

func validStoredSchedule(t *testing.T, status schedule.Status) schedule.Schedule {
	t.Helper()
	timing := validOnceScheduleTiming()
	first, err := schedule.FirstOccurrence(timing, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return schedule.Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 4, Status: status, ExecutionEpoch: 1,
		HostID: "h1234567", ModelID: "model-a", Objective: "check the weekly goal", Timing: timing,
		Budget:             task.Budget{ActiveSeconds: 300, ToolCalls: 12, ModelTokens: 9000},
		Authorization:      task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567"},
		MisfirePolicy:      schedule.MisfireSkip,
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser},
		NextOccurrenceKey:  first.Key, NextRunAt: first.ScheduledAt, NextCheckAt: &first.CheckAt,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

func storedDailyScheduleForProcessing(t *testing.T, policy schedule.MisfirePolicy) schedule.Schedule {
	t.Helper()
	timing := schedule.Timing{Kind: schedule.TimingDaily, StartsOn: "2030-01-01", LocalTime: "09:00", TimeZone: "UTC"}
	createdAt := time.Date(2030, time.January, 1, 8, 0, 0, 0, time.UTC)
	first, err := schedule.FirstOccurrence(timing, createdAt)
	if err != nil {
		t.Fatalf("FirstOccurrence() = %v", err)
	}
	return schedule.Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 2, Status: schedule.StatusEnabled, ExecutionEpoch: 3,
		HostID: "h1234567", ModelID: "model-a", Objective: "check the weekly goal", Timing: timing,
		Budget:             task.Budget{ActiveSeconds: 300, ToolCalls: 12, ModelTokens: 9000},
		Authorization:      task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567"},
		MisfirePolicy:      policy,
		NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser},
		NextOccurrenceKey:  first.Key, NextRunAt: first.ScheduledAt, NextCheckAt: &first.CheckAt,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func TestScheduleProcessDueSkipsEveryElapsedSlotAfterOutage(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := storedDailyScheduleForProcessing(t, schedule.MisfireSkip)
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	now := time.Date(2030, time.January, 5, 10, 0, 0, 0, time.UTC)

	processed, err := service.ProcessDue(context.Background(), now, 0)
	if err != nil || processed != 5 || len(store.dueCommits) != 5 || store.dueTaskCount != 0 {
		t.Fatalf("ProcessDue() = processed %d commits %d tasks %d err %v", processed, len(store.dueCommits), store.dueTaskCount, err)
	}
	for index, commit := range store.dueCommits {
		wantKey := time.Date(2030, time.January, index+1, 9, 0, 0, 0, time.UTC).Format("2006-01-02T15:04")
		if commit.Occurrence.Key != wantKey || commit.Occurrence.Status != schedule.RunMissed || commit.Occurrence.SkipReason != nil {
			t.Fatalf("missed occurrence %d = %#v, want %s missed", index, commit.Occurrence, wantKey)
		}
	}
	latest := store.values[value.ID]
	if latest.NextOccurrenceKey != "2030-01-06T09:00" || latest.NextCheckAt == nil ||
		!latest.NextCheckAt.Equal(time.Date(2030, time.January, 6, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("schedule after missed slots = %#v, want next future occurrence", latest)
	}
}

func TestScheduleProcessDueRunsAtMostOneCatchUpAfterOutage(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := storedDailyScheduleForProcessing(t, schedule.MisfireRunOnce)
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	now := time.Date(2030, time.January, 5, 10, 0, 0, 0, time.UTC)

	processed, err := service.ProcessDue(context.Background(), now, 0)
	if err != nil || processed != 5 || len(store.dueCommits) != 5 || store.dueTaskCount != 1 {
		t.Fatalf("ProcessDue() = processed %d commits %d tasks %d err %v", processed, len(store.dueCommits), store.dueTaskCount, err)
	}
	for index, commit := range store.dueCommits[:len(store.dueCommits)-1] {
		if commit.Occurrence.Status != schedule.RunMissed || commit.Occurrence.Key != time.Date(2030, time.January, index+1, 9, 0, 0, 0, time.UTC).Format("2006-01-02T15:04") {
			t.Fatalf("prior coalesced occurrence %d = %#v, want missed", index, commit.Occurrence)
		}
	}
	last := store.dueCommits[len(store.dueCommits)-1]
	if last.Occurrence.Key != "2030-01-05T09:00" || last.Occurrence.Status != schedule.RunQueued || last.Occurrence.CoalescedCount != 4 ||
		last.NextOccurrence == nil || last.NextOccurrence.Key != "2030-01-06T09:00" {
		t.Fatalf("catch-up occurrence = %#v next=%#v, want one latest run coalescing four missed slots", last.Occurrence, last.NextOccurrence)
	}
}

func TestScheduleProcessDueCoalescesEveryNHoursByElapsedTime(t *testing.T) {
	store := newScheduleServiceStoreFake()
	createdAt := time.Date(2030, time.March, 10, 6, 59, 30, 0, time.UTC)
	timing := schedule.Timing{Kind: schedule.TimingEveryNHours, IntervalHours: 1, TimeZone: "America/New_York"}
	first, err := schedule.FirstOccurrence(timing, createdAt)
	if err != nil {
		t.Fatalf("FirstOccurrence() = %v", err)
	}
	value := storedDailyScheduleForProcessing(t, schedule.MisfireRunOnce)
	value.Timing = timing
	value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = first.Key, first.ScheduledAt, &first.CheckAt
	value.CreatedAt, value.UpdatedAt = createdAt, createdAt
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	now := first.CheckAt.Add(4*time.Hour + 20*time.Minute)

	processed, err := service.ProcessDue(context.Background(), now, 0)
	if err != nil || processed != 5 || len(store.dueCommits) != 5 || store.dueTaskCount != 1 {
		t.Fatalf("ProcessDue() = processed %d commits %d tasks %d err %v", processed, len(store.dueCommits), store.dueTaskCount, err)
	}
	latest := store.dueCommits[len(store.dueCommits)-1]
	if latest.Occurrence.Key != "every_n_hours:2030-03-10T11:00:30Z" || latest.Occurrence.Status != schedule.RunQueued ||
		latest.Occurrence.CoalescedCount != 4 || latest.NextOccurrence == nil ||
		latest.NextOccurrence.Key != "every_n_hours:2030-03-10T12:00:30Z" {
		t.Fatalf("catch-up occurrence = %#v next=%#v", latest.Occurrence, latest.NextOccurrence)
	}
}

func TestScheduleProcessDueHonorsOneMinuteMisfireGrace(t *testing.T) {
	for _, test := range []struct {
		name       string
		delay      time.Duration
		wantStatus schedule.RunStatus
		wantTasks  int
	}{
		{name: "at grace boundary", delay: schedule.MisfireGrace, wantStatus: schedule.RunQueued, wantTasks: 1},
		{name: "past grace boundary", delay: schedule.MisfireGrace + time.Nanosecond, wantStatus: schedule.RunMissed},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newScheduleServiceStoreFake()
			value := storedDailyScheduleForProcessing(t, schedule.MisfireSkip)
			store.values[value.ID] = value
			service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
			now := value.NextCheckAt.Add(test.delay)

			processed, err := service.ProcessDue(context.Background(), now, 0)
			if err != nil || processed != 1 || len(store.dueCommits) != 1 || store.dueCommits[0].Occurrence.Status != test.wantStatus || store.dueTaskCount != test.wantTasks {
				t.Fatalf("ProcessDue() = processed %d commits %#v tasks %d err %v", processed, store.dueCommits, store.dueTaskCount, err)
			}
		})
	}
}

func TestScheduleProcessDueRejectsBatchLargerThanStoreLimit(t *testing.T) {
	store := newScheduleServiceStoreFake()
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	processed, err := service.ProcessDue(context.Background(), time.Now().UTC(), 101)
	if processed != 0 || domain.CodeOf(err) != domain.CodeInvalidArgument || store.dueCalls != 0 {
		t.Fatalf("ProcessDue(limit=101) = processed %d err=%v store calls=%d", processed, err, store.dueCalls)
	}
}

func TestNewScheduleServiceRequiresStoreAuthorizerValidatorAndModelCatalog(t *testing.T) {
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: true}
	if _, err := NewScheduleService(nil, authorizer, models); err == nil {
		t.Fatal("NewScheduleService accepted nil store")
	}
	if _, err := NewScheduleService(newScheduleServiceStoreFake(), nil, models); err == nil {
		t.Fatal("NewScheduleService accepted nil authorizer")
	}
	if _, err := NewScheduleService(newScheduleServiceStoreFake(), authorizer, nil); err == nil {
		t.Fatal("NewScheduleService accepted nil model catalog")
	}
	if _, err := NewScheduleService(newScheduleServiceStoreFake(), scheduleTaskActionOnlyFake{}, models); err == nil {
		t.Fatal("NewScheduleService accepted an authorizer without schedule authorization validation")
	}
}

func TestScheduleCreateAppliesDefaultsAndUsesIdempotencyRecord(t *testing.T) {
	store := newScheduleServiceStoreFake()
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: true}
	service := newTestScheduleService(t, store, authorizer, models)
	principal := scheduleServicePrincipal()
	command := validScheduleCommand()

	created, err := service.Create(context.Background(), principal, command, "schedule-create-001")
	if err != nil {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	if !schedule.ValidID(created.ID) || created.OwnerID != principal.UserID || created.Status != schedule.StatusEnabled ||
		created.Version != 1 || created.MisfirePolicy != schedule.MisfireSkip ||
		!reflect.DeepEqual(created.NotificationEvents, []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser}) ||
		created.NextOccurrenceKey == "" || created.NextCheckAt == nil || created.ExecutionEpoch < 1 {
		t.Fatalf("created Schedule missing normalized state/defaults: %#v", created)
	}
	if created.Objective != strings.TrimSpace(command.Objective) || store.createCalls != 1 || len(store.createRecords) != 1 {
		t.Fatalf("Create() did not normalize objective or persist: objective=%q calls=%d records=%#v", created.Objective, store.createCalls, store.createRecords)
	}
	record := store.createRecords[0]
	if record.OwnerID != principal.UserID || record.Operation != "schedule.create" || record.Key != "schedule-create-001" ||
		record.ResourceID != created.ID || len(record.RequestHash) != 32 || store.values[created.ID].ID != created.ID {
		t.Fatalf("schedule create idempotency record = %#v", record)
	}
	if authorizer.actionCalls != 1 || authorizer.ownerID != principal.UserID || authorizer.hostID != command.HostID ||
		authorizer.requiredScope != identity.ScopeScheduleManage || models.ownerID != principal.UserID || models.modelID != command.ModelID {
		t.Fatalf("create authorization/model checks not applied: auth=%+v models=%+v", authorizer, models)
	}

	replayed, err := service.Create(context.Background(), principal, command, "schedule-create-001")
	if err != nil || replayed.ID != created.ID || store.createCalls != 2 || !bytes.Equal(store.createRecords[0].RequestHash, store.createRecords[1].RequestHash) {
		t.Fatalf("idempotent Create() = %#v, %v calls=%d records=%#v", replayed, err, store.createCalls, store.createRecords)
	}
	changed := command
	changed.Objective = "different request"
	if _, err := service.Create(context.Background(), principal, changed, "schedule-create-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("Create() reused key with changed request error = %v", err)
	}
}

func TestScheduleCreateValidatesIANAZoneAndScheduleRule(t *testing.T) {
	for _, test := range []struct {
		name   string
		timing schedule.Timing
	}{
		{name: "unknown timezone", timing: schedule.Timing{Kind: schedule.TimingOnce, RunAt: "2030-01-01T09:00:00Z", TimeZone: "Mars/Olympus"}},
		{name: "duplicate weekdays", timing: schedule.Timing{Kind: schedule.TimingWeekly, StartsOn: "2030-01-01", LocalTime: "09:00", TimeZone: "UTC", Weekdays: []int{1, 1}}},
		{name: "weekday outside ISO range", timing: schedule.Timing{Kind: schedule.TimingWeekly, StartsOn: "2030-01-01", LocalTime: "09:00", TimeZone: "UTC", Weekdays: []int{8}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newScheduleServiceStoreFake()
			authorizer := &scheduleServiceAuthorizerFake{}
			models := &scheduleServiceModelCatalogFake{configured: true}
			service := newTestScheduleService(t, store, authorizer, models)
			command := validScheduleCommand()
			command.Timing = test.timing
			if _, err := service.Create(context.Background(), scheduleServicePrincipal(), command, "schedule-create-002"); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("Create() error = %v, want invalid_argument", err)
			}
			if store.createCalls != 0 {
				t.Fatalf("invalid rule reached storage: CreateSchedule calls=%d", store.createCalls)
			}
		})
	}
}

func TestScheduleCreateRequiresSessionOrScheduleGrantAndHostAuthorization(t *testing.T) {
	store := newScheduleServiceStoreFake()
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: true}
	service := newTestScheduleService(t, store, authorizer, models)
	command := validScheduleCommand()

	readOnlyClient := clientSchedulePrincipal(identity.ScopeScheduleRead)
	if _, err := service.Create(context.Background(), readOnlyClient, command, "schedule-create-003"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Create() with read-only client error = %v", err)
	}
	apiKey := identity.Principal{UserID: "u1234567", Kind: identity.KindAPIKey, TokenID: "t1234567", Scopes: []identity.Scope{identity.ScopeScheduleRead, identity.ScopeScheduleManage}}
	if _, err := service.Create(context.Background(), apiKey, command, "schedule-create-004"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("Create() with API key error = %v", err)
	}
	if store.createCalls != 0 || authorizer.actionCalls != 0 || models.calls != 0 {
		t.Fatalf("unauthorized create reached collaborators: store=%d authorizer=%d models=%d", store.createCalls, authorizer.actionCalls, models.calls)
	}

	authorizer.actionErr = domain.NewError(domain.CodeForbidden, "host permission denied")
	if _, err := service.Create(context.Background(), scheduleServicePrincipal(), command, "schedule-create-005"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Create() without host authorization error = %v, want not_found", err)
	}
	if authorizer.requiredScope != identity.ScopeScheduleManage || models.calls != 0 || store.createCalls != 0 {
		t.Fatalf("host authorization was bypassed or model checked early: auth=%+v models=%d store=%d", authorizer, models.calls, store.createCalls)
	}
}

func TestScheduleGetIsOwnerScopedAndRequiresReadCredential(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusEnabled)
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})

	owner := scheduleServicePrincipal()
	if got, err := service.Get(context.Background(), owner, value.ID); err != nil || got.ID != value.ID || store.getOwner != owner.UserID {
		t.Fatalf("owner Get() = %#v, %v owner=%q", got, err, store.getOwner)
	}
	otherOwner := owner
	otherOwner.UserID = "u7654321"
	if _, err := service.Get(context.Background(), otherOwner, value.ID); domain.CodeOf(err) != domain.CodeNotFound || store.getOwner != otherOwner.UserID {
		t.Fatalf("cross-owner Get() error=%v owner=%q, want owner-scoped not_found", err, store.getOwner)
	}
	readlessClient := clientSchedulePrincipal(identity.ScopeScheduleManage)
	before := store.getCalls
	if _, err := service.Get(context.Background(), readlessClient, value.ID); domain.CodeOf(err) != domain.CodeForbidden || store.getCalls != before {
		t.Fatalf("client without read scope Get() error=%v store calls=%d", err, store.getCalls)
	}
	invalidToken := owner
	invalidToken.TokenID = ""
	if _, err := service.Get(context.Background(), invalidToken, value.ID); domain.CodeOf(err) != domain.CodeUnauthenticated || store.getCalls != before {
		t.Fatalf("invalid credential Get() error=%v store calls=%d", err, store.getCalls)
	}
}

func TestScheduleCreateRequiresConfiguredModel(t *testing.T) {
	store := newScheduleServiceStoreFake()
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: false}
	service := newTestScheduleService(t, store, authorizer, models)
	if _, err := service.Create(context.Background(), scheduleServicePrincipal(), validScheduleCommand(), "schedule-create-006"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Create() with unconfigured model error = %v, want not_found", err)
	}
	if models.ownerID != "u1234567" || models.modelID != "model-a" || store.createCalls != 0 {
		t.Fatalf("configured model check = %+v, storage calls=%d", models, store.createCalls)
	}
}

func TestScheduleUpdateUsesExpectedVersionAndRevalidatesHostAndModel(t *testing.T) {
	store := newScheduleServiceStoreFake()
	original := validStoredSchedule(t, schedule.StatusEnabled)
	store.values[original.ID] = original
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: true}
	service := newTestScheduleService(t, store, authorizer, models)
	newObjective := "review the updated goal"
	command := UpdateScheduleCommand{
		ExpectedVersion: original.Version, Objective: &newObjective,
	}
	updated, err := service.Update(context.Background(), scheduleServicePrincipal(), original.ID, command, "schedule-update-001")
	if err != nil || updated.Objective != newObjective || updated.Version != original.Version+1 {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	if store.getOwner != original.OwnerID || store.updateOwner != original.OwnerID || store.updateExpected != original.Version ||
		store.updateRecord.OwnerID != original.OwnerID || store.updateRecord.Operation != "schedule.update" ||
		store.updateRecord.ResourceID != original.ID || store.updateRecord.Key != "schedule-update-001" || len(store.updateRecord.RequestHash) != 32 ||
		store.updatedValue.Objective != newObjective || store.updatedValue.Authorization.Kind != string(identity.KindSession) ||
		authorizer.requiredScope != identity.ScopeScheduleManage || models.ownerID != original.OwnerID || models.modelID != original.ModelID {
		t.Fatalf("Update() skipped owner/version/host/model validation: store=%+v auth=%+v models=%+v", store, authorizer, models)
	}
	replay, err := service.Update(context.Background(), scheduleServicePrincipal(), original.ID, command, "schedule-update-001")
	if err != nil || replay.Version != updated.Version || store.updateCalls != 2 ||
		!bytes.Equal(store.updateRecords[0].RequestHash, store.updateRecords[1].RequestHash) {
		t.Fatalf("idempotent Update() = %#v, %v calls=%d records=%#v", replay, err, store.updateCalls, store.updateRecords)
	}
	changedObjective := "different request"
	if _, err := service.Update(context.Background(), scheduleServicePrincipal(), original.ID, UpdateScheduleCommand{
		ExpectedVersion: original.Version, Objective: &changedObjective,
	}, "schedule-update-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("Update() reused key for changed request error = %v, want idempotency conflict", err)
	}

	before := store.updateCalls
	if _, err := service.Update(context.Background(), scheduleServicePrincipal(), original.ID, UpdateScheduleCommand{ExpectedVersion: 0}, "schedule-update-002"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.updateCalls != before {
		t.Fatalf("Update() accepted missing version: err=%v calls=%d", err, store.updateCalls)
	}
	stale := original.Version - 1
	if _, err := service.Update(context.Background(), scheduleServicePrincipal(), original.ID, UpdateScheduleCommand{ExpectedVersion: stale}, "schedule-update-stale-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Update() stale expected_version error = %v, want conflict", err)
	}

	completed := validStoredSchedule(t, schedule.StatusCompleted)
	store.values[completed.ID] = completed
	if _, err := service.Update(context.Background(), scheduleServicePrincipal(), completed.ID, UpdateScheduleCommand{ExpectedVersion: completed.Version}, "schedule-update-done-01"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Update() completed schedule error = %v, want invalid_state", err)
	}
}

func TestScheduleActionsEnableDisableAndReplayIdempotently(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusDisabled)
	store.values[value.ID] = value
	authorizer := &scheduleServiceAuthorizerFake{}
	models := &scheduleServiceModelCatalogFake{configured: true}
	service := newTestScheduleService(t, store, authorizer, models)
	principal := scheduleServicePrincipal()

	enable := ScheduleActionCommand{Action: schedule.ActionEnable, ExpectedVersion: value.Version}
	first, err := service.Action(context.Background(), principal, value.ID, enable, "schedule-enable-001")
	if err != nil || first.Schedule.Status != schedule.StatusEnabled || first.Schedule.Version != value.Version+1 {
		t.Fatalf("enable Action() = %#v, %v", first, err)
	}
	if store.statusTarget != schedule.StatusEnabled || store.statusExpected != value.Version ||
		store.statusRecord.Operation != "schedule.action" || store.statusRecord.OwnerID != principal.UserID ||
		store.statusRecord.ResourceID != value.ID || store.statusRecord.Key != "schedule-enable-001" || len(store.statusRecord.RequestHash) != 32 {
		t.Fatalf("enable status write/idempotency = %+v", store)
	}

	replay, err := service.Action(context.Background(), principal, value.ID, enable, "schedule-enable-001")
	if err != nil || replay.Schedule.Version != first.Schedule.Version || replay.Schedule.Status != schedule.StatusEnabled || store.setStatusCalls != 2 ||
		!bytes.Equal(store.statusReplay[scheduleServiceIdempotencyMapKey(store.statusRecord)].hash, store.statusRecord.RequestHash) {
		t.Fatalf("enable replay = %#v, %v calls=%d", replay, err, store.setStatusCalls)
	}

	if _, err := service.Action(context.Background(), principal, value.ID, ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: value.Version,
	}, "schedule-enable-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reusing enable key for disable error = %v, want idempotency conflict", err)
	}

	disable := ScheduleActionCommand{Action: schedule.ActionDisable, ExpectedVersion: first.Schedule.Version}
	second, err := service.Action(context.Background(), principal, value.ID, disable, "schedule-disable-001")
	if err != nil || second.Schedule.Status != schedule.StatusDisabled || second.Schedule.Version != first.Schedule.Version+1 ||
		store.statusTarget != schedule.StatusDisabled || store.statusExpected != first.Schedule.Version {
		t.Fatalf("disable Action() = %#v, %v status=%+v", second, err, store)
	}
	if authorizer.actionCalls != 4 || authorizer.requiredScope != identity.ScopeScheduleManage {
		t.Fatalf("schedule host authorization calls=%d last scope=%q", authorizer.actionCalls, authorizer.requiredScope)
	}
}

func TestScheduleClientGrantCanReadAndManageButReadOnlyCannotManage(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusEnabled)
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	readOnly := clientSchedulePrincipal(identity.ScopeScheduleRead)
	page, err := service.List(context.Background(), readOnly, schedule.StatusEnabled, PageRequest{})
	if err != nil || page.Items == nil || store.listOwner != readOnly.UserID {
		t.Fatalf("client schedule read = %#v, %v", page, err)
	}
	if _, err := service.Action(context.Background(), readOnly, value.ID, ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: value.Version,
	}, "schedule-client-deny-01"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("read-only client Action() error = %v, want forbidden", err)
	}
	if store.setStatusCalls != 0 {
		t.Fatalf("read-only client reached status storage %d times", store.setStatusCalls)
	}

	grant := clientSchedulePrincipal(identity.ScopeScheduleRead, identity.ScopeScheduleManage)
	result, err := service.Action(context.Background(), grant, value.ID, ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: value.Version,
	}, "schedule-client-disable-01")
	if err != nil || result.Schedule.Status != schedule.StatusDisabled || store.statusAuthorization.Kind != string(identity.KindClient) ||
		!reflect.DeepEqual(store.statusAuthorization.Scopes, []string{string(identity.ScopeScheduleRead), string(identity.ScopeScheduleManage)}) {
		t.Fatalf("granted client action = %#v, %v auth=%+v", result, err, store.statusAuthorization)
	}

	extraScope := clientSchedulePrincipal(identity.ScopeScheduleRead, identity.ScopeScheduleManage, identity.ScopeTaskRead)
	if _, err := service.Action(context.Background(), extraScope, value.ID, ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: value.Version,
	}, "schedule-client-extra-01"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client grant with extra scope error = %v, want forbidden", err)
	}
}

func TestScheduleDisableCanRequestCurrentRunCancellation(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusEnabled)
	taskID := "q1234567"
	value.CurrentRun = &schedule.Run{ID: "y1234567", ScheduleID: value.ID, ScheduleVersion: value.Version, Status: schedule.RunWaitingUser, TaskID: &taskID}
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})

	result, err := service.Action(context.Background(), scheduleServicePrincipal(), value.ID, ScheduleActionCommand{
		Action: schedule.ActionDisable, ExpectedVersion: value.Version, CurrentRunAction: schedule.CurrentRunCancel,
	}, "schedule-cancel-run-001")
	if err != nil || result.Schedule.Status != schedule.StatusDisabled || store.taskActionCalls != 1 ||
		store.taskActionTaskID != taskID || store.taskActionAction != task.ActionCancel ||
		store.taskActionExpected != task.StatusWaitingUser || !domain.ValidTaskActionID(store.taskActionRequestID) ||
		store.taskActionRecord.Operation != "schedule.cancel_run" || store.taskActionRecord.Key != "schedule-cancel-run-001" ||
		store.taskActionRecord.ResourceID != store.taskActionRequestID || store.taskActionRecord.RelatedID != taskID {
		t.Fatalf("cancel current run Action() = %#v, %v store=%+v", result, err, store)
	}
}

func TestScheduleListAndRunsValidateStatusAndPaginationBounds(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusEnabled)
	store.values[value.ID] = value
	store.listResult = SchedulePage{Items: nil, NextCursor: "list-next"}
	store.runsResult = ScheduleRunPage{Items: nil, NextCursor: "runs-next"}
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	principal := scheduleServicePrincipal()

	listed, err := service.List(context.Background(), principal, schedule.StatusDisabled, PageRequest{})
	if err != nil || listed.Items == nil || listed.NextCursor != "list-next" || store.listOwner != principal.UserID ||
		store.listHostIDs != nil || store.listStatus != schedule.StatusDisabled || store.listPage != (PageRequest{Limit: 50}) {
		t.Fatalf("default List() = %#v, %v store=%+v", listed, err, store)
	}
	runs, err := service.Runs(context.Background(), principal, value.ID, PageRequest{Cursor: "run-cursor"})
	if err != nil || runs.Items == nil || runs.NextCursor != "runs-next" || store.runsOwner != principal.UserID ||
		store.runsID != value.ID || store.runsHostIDs != nil || store.runsPage != (PageRequest{Limit: 50, Cursor: "run-cursor"}) {
		t.Fatalf("default Runs() = %#v, %v store=%+v", runs, err, store)
	}

	for _, test := range []struct {
		name   string
		status schedule.Status
		page   PageRequest
	}{
		{name: "invalid status", status: schedule.Status("archived"), page: PageRequest{Limit: 10}},
		{name: "negative limit", status: schedule.StatusEnabled, page: PageRequest{Limit: -1}},
		{name: "limit over maximum", status: schedule.StatusEnabled, page: PageRequest{Limit: 101}},
		{name: "cursor over maximum", status: schedule.StatusEnabled, page: PageRequest{Limit: 10, Cursor: strings.Repeat("x", 1025)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := store.listCalls
			if _, err := service.List(context.Background(), principal, test.status, test.page); domain.CodeOf(err) != domain.CodeInvalidArgument || store.listCalls != before {
				t.Fatalf("List() err=%v calls=%d, want invalid_argument before store", err, store.listCalls)
			}
		})
	}
	for _, page := range []PageRequest{{Limit: -1}, {Limit: 101}, {Limit: 1, Cursor: strings.Repeat("x", 1025)}} {
		before := store.runsCalls
		if _, err := service.Runs(context.Background(), principal, value.ID, page); domain.CodeOf(err) != domain.CodeInvalidArgument || store.runsCalls != before {
			t.Errorf("Runs(%+v) err=%v calls=%d, want invalid_argument before store", page, err, store.runsCalls)
		}
	}
}

func TestScheduleUpdateRejectsInvalidTimingBeforeStorage(t *testing.T) {
	store := newScheduleServiceStoreFake()
	value := validStoredSchedule(t, schedule.StatusEnabled)
	store.values[value.ID] = value
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	invalid := schedule.Timing{Kind: schedule.TimingDaily, StartsOn: "2030-02-31", LocalTime: "25:00", TimeZone: "Not/AZone"}
	if _, err := service.Update(context.Background(), scheduleServicePrincipal(), value.ID, UpdateScheduleCommand{
		ExpectedVersion: value.Version, Timing: &invalid,
	}, "schedule-update-invalid-01"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.updateCalls != 0 {
		t.Fatalf("Update() invalid timing error=%v update calls=%d", err, store.updateCalls)
	}
}

func TestScheduleServiceStoreErrorsPropagate(t *testing.T) {
	store := newScheduleServiceStoreFake()
	store.listErr = errors.New("store unavailable")
	service := newTestScheduleService(t, store, &scheduleServiceAuthorizerFake{}, &scheduleServiceModelCatalogFake{configured: true})
	if _, err := service.List(context.Background(), scheduleServicePrincipal(), "", PageRequest{}); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("List() store error = %v", err)
	}
}
