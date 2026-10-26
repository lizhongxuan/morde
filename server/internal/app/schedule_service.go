package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const (
	scheduleReadScope   identity.Scope = identity.ScopeScheduleRead
	scheduleManageScope identity.Scope = identity.ScopeScheduleManage
	defaultScheduleSize int32          = 50
	maxScheduleSize     int32          = 100
)

type ScheduleService struct {
	store      ScheduleStore
	authorizer TaskAuthorizer
	models     ConfiguredModelCatalog
}

func NewScheduleService(store ScheduleStore, authorizer TaskAuthorizer, models ConfiguredModelCatalog) (*ScheduleService, error) {
	if store == nil || authorizer == nil || models == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "schedule service dependencies are required")
	}
	return &ScheduleService{store: store, authorizer: authorizer, models: models}, nil
}

func (s *ScheduleService) Create(ctx context.Context, principal identity.Principal, command CreateScheduleCommand, idempotencyKey string) (schedule.Schedule, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return schedule.Schedule{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return schedule.Schedule{}, err
	}
	command.ModelID = strings.TrimSpace(command.ModelID)
	if !domain.ValidHostID(command.HostID) || command.ModelID == "" || len(command.ModelID) > 256 || !validBoundedText(command.Objective, 32768) {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "host, model, and objective are required and valid")
	}
	if err := command.Timing.Validate(); err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
	}
	var observation *schedule.ObservationSpec
	if command.Observation != nil {
		value := *command.Observation
		if err := value.Validate(); err != nil {
			return schedule.Schedule{}, domain.InvalidArgument("observation", err.Error())
		}
		observation = &value
	}
	budget, err := normalizedBudget(command.Budget)
	if err != nil {
		return schedule.Schedule{}, err
	}
	command.Budget = budget
	if command.MisfirePolicy == "" {
		command.MisfirePolicy = schedule.MisfireSkip
	}
	if !command.MisfirePolicy.Valid() {
		return schedule.Schedule{}, domain.InvalidArgument("misfire_policy", "misfire policy is invalid")
	}
	if command.NotificationEvents == nil {
		command.NotificationEvents = []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser}
	}
	if err := validateNotificationEvents(command.NotificationEvents); err != nil {
		return schedule.Schedule{}, err
	}
	authorization, err := s.authorizeHost(ctx, principal, command.HostID, scheduleManageScope)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if err := s.validateModel(ctx, principal.UserID, command.ModelID); err != nil {
		return schedule.Schedule{}, err
	}
	now := time.Now().UTC()
	next, err := schedule.FirstOccurrence(command.Timing, now)
	if err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
	}
	id, err := schedule.NewID()
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("generate schedule id: %w", err)
	}
	value := schedule.Schedule{
		ID: id, OwnerID: principal.UserID, Version: 1, Status: schedule.StatusEnabled,
		HostID: command.HostID, ModelID: command.ModelID, Objective: command.Objective, Timing: command.Timing,
		Budget: command.Budget, Authorization: authorization, MisfirePolicy: command.MisfirePolicy, Observation: observation,
		NotificationEvents: command.NotificationEvents, NextOccurrenceKey: next.Key,
		NextRunAt: next.ScheduledAt, NextCheckAt: &next.CheckAt,
	}
	if observation != nil {
		value.ObservationConfigHash = observation.ConfigHash()
	}
	record, err := idempotencyRecord(principal.UserID, "schedule.create", idempotencyKey, command, id, "")
	if err != nil {
		return schedule.Schedule{}, err
	}
	result, _, err := s.store.CreateSchedule(ctx, value, record)
	return result, err
}

func (s *ScheduleService) List(ctx context.Context, principal identity.Principal, filter ScheduleListRequest) (SchedulePage, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return SchedulePage{}, err
	}
	if err := validateSchedulePage(filter.Page); err != nil {
		return SchedulePage{}, err
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return SchedulePage{}, domain.InvalidArgument("status", "schedule status is invalid")
	}
	hostIDs, err := scheduleHostIDs(principal, scheduleReadScope)
	if err != nil {
		return SchedulePage{}, err
	}
	return s.store.ListSchedules(ctx, principal.UserID, hostIDs, filter.Status, normalizeSchedulePage(filter.Page))
}

func (s *ScheduleService) Get(ctx context.Context, principal identity.Principal, id string) (schedule.Schedule, error) {
	value, err := s.getAuthorized(ctx, principal, id, scheduleReadScope)
	if err != nil {
		return schedule.Schedule{}, err
	}
	return value, nil
}

func (s *ScheduleService) Update(ctx context.Context, principal identity.Principal, id string, command UpdateScheduleCommand) (schedule.Schedule, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return schedule.Schedule{}, err
	}
	if command.ExpectedVersion < 1 || (command.HostID == nil && command.ModelID == nil && command.Objective == nil && command.Timing == nil && command.Budget == nil && !command.ObservationSet && command.MisfirePolicy == nil && command.NotificationEvents == nil) {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "expected_version and at least one schedule field are required")
	}
	value, err := s.getAuthorized(ctx, principal, id, scheduleManageScope)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if value.Version != command.ExpectedVersion {
		return schedule.Schedule{}, scheduleVersionConflict(command.ExpectedVersion, value.Version)
	}
	if value.Status == schedule.StatusCompleted {
		return schedule.Schedule{}, domain.NewError(domain.CodeInvalidState, "completed schedule cannot be updated")
	}
	if command.HostID != nil {
		value.HostID = *command.HostID
	}
	if command.ModelID != nil {
		value.ModelID = strings.TrimSpace(*command.ModelID)
	}
	if command.Objective != nil {
		value.Objective = *command.Objective
	}
	if command.Timing != nil {
		value.Timing = *command.Timing
	}
	if command.Budget != nil {
		value.Budget = *command.Budget
	}
	if command.ObservationSet {
		previousHash := value.ObservationConfigHash
		previousBaseline := value.BaselineFingerprint
		value.Observation, value.ObservationConfigHash, value.BaselineFingerprint = nil, "", ""
		if command.Observation != nil {
			observation := *command.Observation
			if err := observation.Validate(); err != nil {
				return schedule.Schedule{}, domain.InvalidArgument("observation", err.Error())
			}
			value.Observation = &observation
			value.ObservationConfigHash = observation.ConfigHash()
			if value.ObservationConfigHash == previousHash {
				value.BaselineFingerprint = previousBaseline
			}
		}
	}
	if command.MisfirePolicy != nil {
		value.MisfirePolicy = *command.MisfirePolicy
	}
	if command.NotificationEvents != nil {
		value.NotificationEvents = append([]schedule.NotificationEvent(nil), (*command.NotificationEvents)...)
	}
	if !domain.ValidHostID(value.HostID) || value.ModelID == "" || len(value.ModelID) > 256 || !validBoundedText(value.Objective, 32768) {
		return schedule.Schedule{}, domain.InvalidArgument("schedule", "host, model, and objective are required and valid")
	}
	if err := value.Timing.Validate(); err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
	}
	if err := value.Budget.Validate(); err != nil {
		return schedule.Schedule{}, domain.InvalidArgument("budget", err.Error())
	}
	if !value.MisfirePolicy.Valid() {
		return schedule.Schedule{}, domain.InvalidArgument("misfire_policy", "misfire policy is invalid")
	}
	if err := validateNotificationEvents(value.NotificationEvents); err != nil {
		return schedule.Schedule{}, err
	}
	authorization, err := s.authorizeHost(ctx, principal, value.HostID, scheduleManageScope)
	if err != nil {
		return schedule.Schedule{}, err
	}
	value.Authorization = authorization
	if err := s.validateModel(ctx, principal.UserID, value.ModelID); err != nil {
		return schedule.Schedule{}, err
	}
	if value.Status == schedule.StatusEnabled {
		next, err := schedule.FirstOccurrence(value.Timing, time.Now().UTC())
		if err != nil {
			return schedule.Schedule{}, domain.InvalidArgument("timing", err.Error())
		}
		value.NextOccurrenceKey, value.NextRunAt, value.NextCheckAt = next.Key, next.ScheduledAt, &next.CheckAt
	}
	return s.store.UpdateSchedule(ctx, principal.UserID, value, command.ExpectedVersion)
}

func (s *ScheduleService) Action(ctx context.Context, principal identity.Principal, id string, command ScheduleActionCommand, idempotencyKey string) (ScheduleActionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ScheduleActionResult{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return ScheduleActionResult{}, err
	}
	value, err := s.getAuthorized(ctx, principal, id, scheduleManageScope)
	if err != nil {
		return ScheduleActionResult{}, err
	}
	if command.ExpectedVersion < 1 || (command.Action != schedule.ActionEnable && command.Action != schedule.ActionDisable) {
		return ScheduleActionResult{}, domain.InvalidArgument("action", "schedule action or expected_version is invalid")
	}
	if command.CurrentRunAction == "" {
		command.CurrentRunAction = schedule.CurrentRunContinue
	}
	if command.CurrentRunAction != schedule.CurrentRunContinue && command.CurrentRunAction != schedule.CurrentRunCancel {
		return ScheduleActionResult{}, domain.InvalidArgument("current_run_action", "current run action is invalid")
	}
	if command.Action == schedule.ActionEnable && command.CurrentRunAction != schedule.CurrentRunContinue {
		return ScheduleActionResult{}, domain.InvalidArgument("current_run_action", "enable cannot cancel a current run")
	}
	if value.Version != command.ExpectedVersion {
		return ScheduleActionResult{}, scheduleVersionConflict(command.ExpectedVersion, value.Version)
	}
	target := schedule.StatusDisabled
	if command.Action == schedule.ActionEnable {
		target = schedule.StatusEnabled
	}
	request := struct {
		ScheduleID string                    `json:"schedule_id"`
		Action     schedule.Action           `json:"action"`
		Version    int64                     `json:"expected_version"`
		CurrentRun schedule.CurrentRunAction `json:"current_run_action"`
	}{id, command.Action, command.ExpectedVersion, command.CurrentRunAction}
	record, err := idempotencyRecord(principal.UserID, "schedule.action", idempotencyKey, request, id, "")
	if err != nil {
		return ScheduleActionResult{}, err
	}
	updated, err := s.store.SetScheduleStatus(ctx, record, id, target, command.ExpectedVersion)
	if err != nil {
		return ScheduleActionResult{}, err
	}
	result := ScheduleActionResult{Schedule: updated, CurrentRun: updated.CurrentRun}
	if command.Action == schedule.ActionDisable && command.CurrentRunAction == schedule.CurrentRunCancel && result.CurrentRun != nil && result.CurrentRun.TaskID != nil {
		if err := s.cancelCurrentRun(ctx, principal.UserID, idempotencyKey, result.CurrentRun); err != nil {
			return ScheduleActionResult{}, err
		}
		if result.CurrentRun.Status.Active() {
			result.CurrentRun.Status = schedule.RunStopping
		}
	}
	return result, nil
}

func (s *ScheduleService) Runs(ctx context.Context, principal identity.Principal, id string, page PageRequest) (ScheduleRunPage, error) {
	if err := validateSchedulePage(page); err != nil {
		return ScheduleRunPage{}, err
	}
	value, err := s.getAuthorized(ctx, principal, id, scheduleReadScope)
	if err != nil {
		return ScheduleRunPage{}, err
	}
	hostIDs, err := scheduleHostIDs(principal, scheduleReadScope)
	if err != nil {
		return ScheduleRunPage{}, err
	}
	return s.store.ListScheduleRuns(ctx, value.OwnerID, value.ID, hostIDs, normalizeSchedulePage(page))
}

// ProcessDue commits one occurrence per call; the worker loops and backs off between calls.
func (s *ScheduleService) ProcessDue(ctx context.Context) (bool, error) {
	values, err := s.store.ListDueSchedules(ctx, time.Now().UTC(), 1)
	if err != nil || len(values) == 0 {
		return false, err
	}
	value := values[0]
	if value.NextCheckAt == nil || value.NextOccurrenceKey == "" {
		return false, domain.NewError(domain.CodeInternal, "stored schedule has no next occurrence")
	}
	occurrence := schedule.Occurrence{
		Key: value.NextOccurrenceKey, ScheduledAt: value.NextRunAt, CheckAt: *value.NextCheckAt, Status: schedule.RunQueued,
	}
	notificationEvents := append([]schedule.NotificationEvent(nil), value.NotificationEvents...)
	if occurrence.ScheduledAt == nil {
		reason := schedule.SkipDSTGap
		occurrence.Status, occurrence.SkipReason = schedule.RunSkipped, &reason
	}
	now := time.Now().UTC()
	if occurrence.CheckAt.After(now) {
		return false, nil
	}
	if occurrence.ScheduledAt != nil && schedule.IsMisfire(*occurrence.ScheduledAt, now) {
		if value.MisfirePolicy == schedule.MisfireSkip {
			occurrence.Status = schedule.RunMissed
		} else {
			latest, coalesced, err := schedule.LatestDue(value.Timing, occurrence, now)
			if err != nil {
				return false, fmt.Errorf("coalesce missed schedule occurrences: %w", err)
			}
			occurrence = latest
			occurrence.CoalescedCount = coalesced
		}
	}
	var next *schedule.Occurrence
	if value.Timing.Kind != schedule.TimingOnce {
		nextValue, err := schedule.NextOccurrence(value.Timing, occurrence.Key)
		if err != nil {
			return false, fmt.Errorf("calculate next schedule occurrence: %w", err)
		}
		next = &nextValue
	}
	if occurrence.Status == schedule.RunQueued {
		principal := schedulePrincipal(value)
		if _, err := s.authorizeHost(ctx, principal, value.HostID, scheduleManageScope); err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeUnauthenticated, domain.CodeForbidden, domain.CodeNotFound:
			default:
				return false, err
			}
			reason := schedule.SkipAuthorizationEnded
			occurrence.Status, occurrence.SkipReason = schedule.RunSkipped, &reason
		}
		if occurrence.Status == schedule.RunQueued {
			if err := s.validateModel(ctx, value.OwnerID, value.ModelID); err != nil {
				return false, err
			}
		}
	}
	_, committed, err := s.store.CommitScheduleOccurrence(ctx, ScheduleOccurrenceCommit{
		ScheduleID: value.ID, OwnerID: value.OwnerID, ExpectedVersion: value.Version, ExpectedEpoch: value.ExecutionEpoch,
		Occurrence: occurrence, NextOccurrence: next, NotificationEvents: notificationEvents,
	})
	return committed, err
}

func (s *ScheduleService) cancelCurrentRun(ctx context.Context, ownerID, idempotencyKey string, current *schedule.Run) error {
	if current.TaskID == nil || !domain.ValidTaskID(*current.TaskID) {
		return nil
	}
	details, err := s.store.GetTask(ctx, *current.TaskID, ownerID)
	if err != nil {
		return err
	}
	status := details.Task.Status
	if status != task.StatusQueued && status != task.StatusRunning && status != task.StatusWaitingUser && status != task.StatusPaused {
		return nil
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		return fmt.Errorf("generate schedule task cancellation id: %w", err)
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(current.ScheduleID+":"+idempotencyKey)))
	record, err := idempotencyRecord(ownerID, "task.action.cancel", key, struct {
		ScheduleID string      `json:"schedule_id"`
		TaskID     string      `json:"task_id"`
		Status     task.Status `json:"expected_status"`
	}{current.ScheduleID, *current.TaskID, status}, requestID, *current.TaskID)
	if err != nil {
		return err
	}
	_, err = s.store.RequestTaskAction(ctx, record, *current.TaskID, task.ActionCancel, status, requestID)
	return err
}

func (s *ScheduleService) getAuthorized(ctx context.Context, principal identity.Principal, id string, scope identity.Scope) (schedule.Schedule, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return schedule.Schedule{}, err
	}
	if !schedule.ValidID(id) {
		return schedule.Schedule{}, domain.NewError(domain.CodeNotFound, "schedule not found")
	}
	value, err := s.store.GetSchedule(ctx, principal.UserID, id)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if _, err := s.authorizeHost(ctx, principal, value.HostID, scope); err != nil {
		return schedule.Schedule{}, hideTaskAccessError(err)
	}
	return value, nil
}

func (s *ScheduleService) authorizeHost(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if !domain.ValidHostID(hostID) {
		return task.Authorization{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if principal.Kind == identity.KindAPIKey {
		if !principal.HasScope(scope) {
			return task.Authorization{}, domain.NewError(domain.CodeForbidden, "schedule scope is required")
		}
		if !principal.AllowsHost(hostID) {
			return task.Authorization{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
	}
	if err := s.authorizer.AuthorizeTaskAction(ctx, principal.UserID, principal.Kind, principal.TokenID, hostID, principal.Scopes, scope); err != nil {
		return task.Authorization{}, err
	}
	return authorizationSnapshot(principal), nil
}

func (s *ScheduleService) validateModel(ctx context.Context, ownerID, modelID string) error {
	configured, err := s.models.IsConfiguredModel(ctx, ownerID, modelID)
	if err != nil {
		return fmt.Errorf("check configured schedule model: %w", err)
	}
	if !configured {
		return domain.InvalidArgument("model_id", "model is not configured for this user")
	}
	return nil
}

func schedulePrincipal(value schedule.Schedule) identity.Principal {
	scopes := make([]identity.Scope, 0, len(value.Authorization.Scopes))
	for _, scope := range value.Authorization.Scopes {
		scopes = append(scopes, identity.Scope(scope))
	}
	return identity.Principal{
		UserID: value.OwnerID, Kind: identity.Kind(value.Authorization.Kind), TokenID: value.Authorization.TokenID,
		HostIDs: []string{value.HostID}, Scopes: scopes,
	}
}

func scheduleHostIDs(principal identity.Principal, scope identity.Scope) ([]string, error) {
	if principal.Kind == identity.KindSession || principal.Kind == identity.KindClient {
		return nil, nil
	}
	if principal.Kind != identity.KindAPIKey || !principal.HasScope(scope) {
		return nil, domain.NewError(domain.CodeForbidden, "schedule scope is required")
	}
	return append([]string{}, principal.HostIDs...), nil
}

func validateNotificationEvents(values []schedule.NotificationEvent) error {
	seen := make(map[schedule.NotificationEvent]struct{}, len(values))
	for _, value := range values {
		if !value.Valid() {
			return domain.InvalidArgument("notification_events", "notification event is invalid")
		}
		if _, ok := seen[value]; ok {
			return domain.InvalidArgument("notification_events", "notification events must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateSchedulePage(page PageRequest) error {
	if page.Limit > maxScheduleSize {
		return domain.InvalidArgument("limit", "page limit is out of range")
	}
	return validatePageRequest(page)
}

func normalizeSchedulePage(page PageRequest) PageRequest {
	if page.Limit == 0 {
		page.Limit = defaultScheduleSize
	}
	return page
}

func scheduleVersionConflict(expected, actual int64) error {
	conflict := domain.NewError(domain.CodeConflict, "schedule version changed")
	conflict.Details["expected_version"] = expected
	conflict.Details["actual_version"] = actual
	return conflict
}
