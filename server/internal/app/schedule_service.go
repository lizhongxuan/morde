package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const (
	defaultSchedulePageSize int32 = 50
	maxSchedulePageSize     int32 = 100
	defaultDueScheduleLimit int32 = 100
	maxDueScheduleLimit     int32 = 100
)

type ScheduleAuthorizationValidator interface {
	ValidateScheduleAuthorization(context.Context, string, task.Authorization) error
}

type ScheduleService struct {
	store      ScheduleStore
	authorizer TaskAuthorizer
	validator  ScheduleAuthorizationValidator
	models     ConfiguredModelCatalog
}

func NewScheduleService(store ScheduleStore, authorizer TaskAuthorizer, models ConfiguredModelCatalog) (*ScheduleService, error) {
	validator, ok := authorizer.(ScheduleAuthorizationValidator)
	if store == nil || authorizer == nil || !ok || models == nil {
		return nil, errors.New("schedule service dependencies are required")
	}
	return &ScheduleService{store: store, authorizer: authorizer, validator: validator, models: models}, nil
}

func (s *ScheduleService) List(ctx context.Context, principal identity.Principal, status schedule.Status, page PageRequest) (SchedulePage, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleRead); err != nil {
		return SchedulePage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultSchedulePageSize
	}
	if page.Limit < 1 || page.Limit > maxSchedulePageSize || len(page.Cursor) > 1024 {
		return SchedulePage{}, domain.InvalidArgument("page", "schedule page request is out of range")
	}
	if status != "" && !status.Valid() {
		return SchedulePage{}, domain.InvalidArgument("status", "schedule status is invalid")
	}
	result, err := s.store.ListSchedules(ctx, principal.UserID, nil, status, page)
	if err != nil {
		return SchedulePage{}, err
	}
	if result.Items == nil {
		result.Items = []schedule.Schedule{}
	}
	return result, nil
}

func (s *ScheduleService) Get(ctx context.Context, principal identity.Principal, id string) (schedule.Schedule, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleRead); err != nil {
		return schedule.Schedule{}, err
	}
	if !schedule.ValidID(id) {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	return s.store.GetSchedule(ctx, principal.UserID, id)
}

func (s *ScheduleService) Create(ctx context.Context, principal identity.Principal, command CreateScheduleCommand, idempotencyKey string) (schedule.Schedule, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleManage); err != nil {
		return schedule.Schedule{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return schedule.Schedule{}, err
	}
	command.Objective = stringsTrim(command.Objective)
	if (command.HostID != "" && !domain.ValidHostID(command.HostID)) || command.ModelID == "" || len(command.ModelID) > 200 || command.Objective == "" || len(command.Objective) > 32768 {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "schedule fields are invalid")
	}
	if command.MisfirePolicy == "" {
		command.MisfirePolicy = schedule.MisfireSkip
	}
	if command.NotificationEvents == nil {
		command.NotificationEvents = []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser}
	}
	if err := validateScheduleResources(ctx, s.authorizer, principal, command.HostID, command.Objective, command.Observation); err != nil {
		return schedule.Schedule{}, err
	}
	configured, err := s.models.IsConfiguredModel(ctx, principal.UserID, command.ModelID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if !configured {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "model not found")
	}
	first, err := schedule.FirstOccurrence(command.Timing, time.Now().UTC())
	if err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
	}
	id, err := schedule.NewID()
	if err != nil {
		return schedule.Schedule{}, err
	}
	value := schedule.Schedule{
		ID: id, OwnerID: principal.UserID, Version: 1, Status: schedule.StatusEnabled,
		ExecutionEpoch: 1, // CreateSchedule replaces this validation placeholder with the locked owner epoch.
		HostID:         command.HostID, ModelID: command.ModelID, Objective: command.Objective,
		Timing: command.Timing, Budget: command.Budget, Authorization: scheduleAuthorization(principal),
		MisfirePolicy: command.MisfirePolicy, NotificationEvents: command.NotificationEvents,
		Observation: command.Observation, NextOccurrenceKey: first.Key, NextRunAt: first.ScheduledAt,
		NextCheckAt: &first.CheckAt,
	}
	if value.Observation != nil {
		value.ObservationConfigHash = value.Observation.ConfigHash()
	}
	if err := schedule.Validate(value); err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", err.Error())
	}
	requestHash, err := hashIdempotencyRequest(struct {
		HostID             string
		ModelID            string
		Objective          string
		Timing             schedule.Timing
		Budget             task.Budget
		Observation        *schedule.ObservationSpec
		MisfirePolicy      schedule.MisfirePolicy
		NotificationEvents []schedule.NotificationEvent
	}{command.HostID, command.ModelID, command.Objective, command.Timing, command.Budget, command.Observation, command.MisfirePolicy, command.NotificationEvents})
	if err != nil {
		return schedule.Schedule{}, err
	}
	record := IdempotencyRecord{OwnerID: principal.UserID, Operation: "schedule.create", Key: idempotencyKey,
		RequestHash: requestHash, ResourceID: id}
	created, _, err := s.store.CreateSchedule(ctx, value, record)
	if err != nil {
		return schedule.Schedule{}, err
	}
	return created, nil
}

func (s *ScheduleService) Update(ctx context.Context, principal identity.Principal, id string, command UpdateScheduleCommand, idempotencyKey string) (schedule.Schedule, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleManage); err != nil {
		return schedule.Schedule{}, err
	}
	if !schedule.ValidID(id) || command.ExpectedVersion < 1 {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "schedule update fields are invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return schedule.Schedule{}, err
	}
	current, err := s.store.GetSchedule(ctx, principal.UserID, id)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if current.Status == schedule.StatusCompleted {
		return schedule.Schedule{}, domain.NewError(domain.CodeInvalidState, "completed schedule cannot be edited")
	}
	if command.HostID != nil {
		current.HostID = *command.HostID
	}
	if command.ModelID != nil {
		current.ModelID = *command.ModelID
	}
	if command.Objective != nil {
		current.Objective = *command.Objective
	}
	if command.Timing != nil {
		current.Timing = *command.Timing
		first, err := schedule.FirstOccurrence(current.Timing, time.Now().UTC())
		if err != nil {
			return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
		}
		current.NextOccurrenceKey, current.NextRunAt, current.NextCheckAt = first.Key, first.ScheduledAt, &first.CheckAt
	}
	if command.Budget != nil {
		current.Budget = *command.Budget
	}
	if command.ObservationSet {
		current.Observation = command.Observation
		current.ObservationConfigHash = ""
		current.BaselineFingerprint = ""
		if current.Observation != nil {
			current.ObservationConfigHash = current.Observation.ConfigHash()
		}
	}
	if command.MisfirePolicy != nil {
		current.MisfirePolicy = *command.MisfirePolicy
	}
	if command.NotificationEvents != nil {
		current.NotificationEvents = append([]schedule.NotificationEvent(nil), (*command.NotificationEvents)...)
	}
	current.Authorization = scheduleAuthorization(principal)
	if err := validateScheduleResources(ctx, s.authorizer, principal, current.HostID, current.Objective, current.Observation); err != nil {
		return schedule.Schedule{}, err
	}
	configured, err := s.models.IsConfiguredModel(ctx, principal.UserID, current.ModelID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if !configured {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "model not found")
	}
	if err := schedule.Validate(current); err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", err.Error())
	}
	requestHash, err := hashIdempotencyRequest(struct {
		ScheduleID string                `json:"schedule_id"`
		Command    UpdateScheduleCommand `json:"command"`
	}{id, command})
	if err != nil {
		return schedule.Schedule{}, err
	}
	record := IdempotencyRecord{OwnerID: principal.UserID, Operation: "schedule.update", Key: idempotencyKey,
		RequestHash: requestHash, ResourceID: id}
	return s.store.UpdateSchedule(ctx, principal.UserID, record, current, command.ExpectedVersion)
}

func (s *ScheduleService) Action(ctx context.Context, principal identity.Principal, id string, command ScheduleActionCommand, idempotencyKey string) (ScheduleActionResult, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleManage); err != nil {
		return ScheduleActionResult{}, err
	}
	if !schedule.ValidID(id) || command.ExpectedVersion < 1 ||
		(command.Action != schedule.ActionEnable && command.Action != schedule.ActionDisable) ||
		(command.CurrentRunAction != "" && command.CurrentRunAction != schedule.CurrentRunContinue && command.CurrentRunAction != schedule.CurrentRunCancel) {
		return ScheduleActionResult{}, domain.InvalidArgument("schedule_action", "schedule action fields are invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return ScheduleActionResult{}, err
	}
	current, err := s.store.GetSchedule(ctx, principal.UserID, id)
	if err != nil {
		return ScheduleActionResult{}, err
	}
	if err := validateScheduleResources(ctx, s.authorizer, principal, current.HostID, current.Objective, current.Observation); err != nil {
		return ScheduleActionResult{}, err
	}
	if command.CurrentRunAction == schedule.CurrentRunCancel && current.CurrentRun != nil && current.CurrentRun.TaskID != nil {
		if err := s.cancelCurrentRun(ctx, principal, id, idempotencyKey, *current.CurrentRun); err != nil {
			return ScheduleActionResult{}, err
		}
	}
	target := schedule.StatusDisabled
	if command.Action == schedule.ActionEnable {
		target = schedule.StatusEnabled
	}
	requestHash, err := hashIdempotencyRequest(struct {
		ScheduleID       string                    `json:"schedule_id"`
		Action           schedule.Action           `json:"action"`
		ExpectedVersion  int64                     `json:"expected_version"`
		CurrentRunAction schedule.CurrentRunAction `json:"current_run_action"`
	}{id, command.Action, command.ExpectedVersion, command.CurrentRunAction})
	if err != nil {
		return ScheduleActionResult{}, err
	}
	record := IdempotencyRecord{OwnerID: principal.UserID, Operation: "schedule.action", Key: idempotencyKey,
		RequestHash: requestHash, ResourceID: id}
	updated, err := s.store.SetScheduleStatus(ctx, record, id, target, command.ExpectedVersion, scheduleAuthorization(principal))
	if err != nil {
		return ScheduleActionResult{}, err
	}
	return ScheduleActionResult{Schedule: updated, CurrentRun: updated.CurrentRun}, nil
}

func (s *ScheduleService) Runs(ctx context.Context, principal identity.Principal, id string, page PageRequest) (ScheduleRunPage, error) {
	if err := validateSchedulePrincipal(principal, identity.ScopeScheduleRead); err != nil {
		return ScheduleRunPage{}, err
	}
	if !schedule.ValidID(id) {
		return ScheduleRunPage{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	if page.Limit == 0 {
		page.Limit = defaultSchedulePageSize
	}
	if page.Limit < 1 || page.Limit > maxSchedulePageSize || len(page.Cursor) > 1024 {
		return ScheduleRunPage{}, domain.InvalidArgument("page", "schedule page request is out of range")
	}
	result, err := s.store.ListScheduleRuns(ctx, principal.UserID, id, nil, page)
	if err != nil {
		return ScheduleRunPage{}, err
	}
	if result.Items == nil {
		result.Items = []schedule.Run{}
	}
	return result, nil
}

// ProcessDue is invoked by the background schedule worker. The credential is
// checked before planning and is checked again under lock by CommitScheduleOccurrence.
func (s *ScheduleService) ProcessDue(ctx context.Context, now time.Time, limit int32) (int, error) {
	if ctx == nil {
		return 0, errors.New("schedule processing context is required")
	}
	if now.IsZero() {
		return 0, domain.InvalidArgument("now", "schedule processing time is required")
	}
	if limit == 0 {
		limit = defaultDueScheduleLimit
	}
	if limit < 1 || limit > maxDueScheduleLimit {
		return 0, domain.InvalidArgument("limit", "schedule batch size is out of range")
	}
	values, err := s.store.ListDueSchedules(ctx, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("list due schedules: %w", err)
	}
	processed := 0
	for _, value := range values {
		latestDueKey := value.NextOccurrenceKey
		if value.MisfirePolicy == schedule.MisfireRunOnce && value.NextCheckAt != nil && value.NextOccurrenceKey != "" {
			current := schedule.Occurrence{Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt}
			latest, _, err := schedule.LatestDue(value.Timing, current, now.UTC())
			if err != nil {
				return processed, fmt.Errorf("find latest due occurrence for schedule %s: %w", value.ID, err)
			}
			latestDueKey = latest.Key
		}
		coalescedCount := int64(0)
		for {
			occurrence, next, err := planDueSchedule(value, now.UTC(), coalescedCount, latestDueKey)
			if err != nil {
				return processed, fmt.Errorf("plan due schedule %s: %w", value.ID, err)
			}
			if err := s.validator.ValidateScheduleAuthorization(ctx, value.OwnerID, value.Authorization); err != nil {
				if domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound {
					reason := schedule.SkipAuthorizationEnded
					occurrence.Status, occurrence.CoalescedCount = schedule.RunSkipped, 0
					if occurrence.ScheduledAt != nil {
						occurrence.SkipReason = &reason
					}
				} else {
					return processed, fmt.Errorf("validate schedule authorization %s: %w", value.ID, err)
				}
			}
			run, committed, err := s.store.CommitScheduleOccurrence(ctx, ScheduleOccurrenceCommit{
				ScheduleID: value.ID, OwnerID: value.OwnerID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
				Occurrence: occurrence, NextOccurrence: next, NotificationEvents: value.NotificationEvents,
			})
			if err != nil {
				return processed, fmt.Errorf("commit schedule occurrence %s/%s: %w", value.ID, occurrence.Key, err)
			}
			if !committed {
				break
			}
			processed++
			if next == nil || next.CheckAt.After(now.UTC()) || run.Status == schedule.RunQueued {
				break
			}
			if value.MisfirePolicy == schedule.MisfireRunOnce {
				coalescedCount++
			}
			value.NextOccurrenceKey, value.NextRunAt = next.Key, next.ScheduledAt
			nextCheckAt := next.CheckAt
			value.NextCheckAt = &nextCheckAt
		}
	}
	return processed, nil
}

func planDueSchedule(value schedule.Schedule, now time.Time, coalescedCount int64, latestDueKey string) (schedule.Occurrence, *schedule.Occurrence, error) {
	if value.NextCheckAt == nil || value.NextOccurrenceKey == "" {
		return schedule.Occurrence{}, nil, domain.NewError(domain.CodeInvalidState, "due schedule has no next occurrence")
	}
	occurrence := schedule.Occurrence{Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt, Status: schedule.RunQueued}
	if occurrence.ScheduledAt == nil {
		reason := schedule.SkipDSTGap
		occurrence.Status, occurrence.SkipReason = schedule.RunSkipped, &reason
	}
	if schedule.IsMisfire(occurrence.CheckAt, now) {
		if value.MisfirePolicy == schedule.MisfireSkip || latestDueKey != occurrence.Key {
			if occurrence.ScheduledAt != nil {
				occurrence.Status = schedule.RunMissed
			}
		} else {
			// This is the latest elapsed slot. Prior missed slots were already
			// recorded during this poll; only this one may create a catch-up task.
			occurrence.CoalescedCount = coalescedCount
		}
	}
	var next *schedule.Occurrence
	if value.Timing.Kind != schedule.TimingOnce {
		upcoming, err := schedule.NextOccurrence(value.Timing, occurrence.Key)
		if err != nil {
			return schedule.Occurrence{}, nil, err
		}
		next = &upcoming
	}
	return occurrence, next, nil
}

func (s *ScheduleService) cancelCurrentRun(ctx context.Context, principal identity.Principal, scheduleID, key string, run schedule.Run) error {
	if run.TaskID == nil || !domain.ValidTaskID(*run.TaskID) || !validScheduleRunCancellable(run.Status) {
		return nil
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return err
	}
	request, err := idempotencyRecord(principal.UserID, "schedule.cancel_run", key, struct {
		ScheduleID     string      `json:"schedule_id"`
		RunID          string      `json:"run_id"`
		ExpectedStatus task.Status `json:"expected_status"`
	}{scheduleID, run.ID, task.Status(run.Status)}, requestID, *run.TaskID)
	if err != nil {
		return err
	}
	_, err = s.store.RequestTaskAction(ctx, request, *run.TaskID, task.ActionCancel, task.Status(run.Status), requestID)
	return err
}

func validScheduleRunCancellable(status schedule.RunStatus) bool {
	return status == schedule.RunQueued || status == schedule.RunRunning || status == schedule.RunWaitingUser || status == schedule.RunPaused
}

func validateSchedulePrincipal(principal identity.Principal, required identity.Scope) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "client installation is unavailable")
	}
	if !principal.HasScope(required) {
		return domain.NewError(domain.CodeForbidden, "schedule scope is required")
	}
	if principal.Kind == identity.KindClient && required == identity.ScopeScheduleManage && !identity.IsScheduleGrantScopeSet(principal.Scopes) {
		return domain.NewError(domain.CodeForbidden, "schedule management grant is required")
	}
	return nil
}

func validateScheduleHost(ctx context.Context, authorizer TaskAuthorizer, principal identity.Principal, hostID string) error {
	if !domain.ValidHostID(hostID) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if !principal.AllowsHost(hostID) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err := authorizer.AuthorizeTaskAction(ctx, principal.UserID, principal.Kind, principal.TokenID, hostID,
		principal.Scopes, identity.ScopeScheduleManage); err != nil {
		return hideTaskAccessError(err)
	}
	return nil
}

func validateScheduleResources(ctx context.Context, authorizer TaskAuthorizer, principal identity.Principal, hostID, objective string, observation *schedule.ObservationSpec) error {
	if hostID == "" {
		if observation != nil || ClassifyFileDeliveryIntent(objective, false) != task.FileDeliveryNone {
			return domain.InvalidArgument("schedule", "hostless schedule cannot request computer or workspace capabilities")
		}
		return nil
	}
	return validateScheduleHost(ctx, authorizer, principal, hostID)
}

func scheduleAuthorization(principal identity.Principal) task.Authorization {
	value := authorizationSnapshot(principal)
	if principal.Kind == identity.KindClient {
		value.Scopes = []string{string(identity.ScopeScheduleRead), string(identity.ScopeScheduleManage)}
	}
	return value
}

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}
