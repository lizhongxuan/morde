package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

// ScheduleService is the application boundary used by the Schedule HTTP
// adapter. Ownership and authorization decisions remain in the app service.
type ScheduleService interface {
	List(context.Context, identity.Principal, schedule.Status, app.PageRequest) (app.SchedulePage, error)
	Get(context.Context, identity.Principal, string) (schedule.Schedule, error)
	Create(context.Context, identity.Principal, app.CreateScheduleCommand, string) (schedule.Schedule, error)
	Update(context.Context, identity.Principal, string, app.UpdateScheduleCommand, string) (schedule.Schedule, error)
	Action(context.Context, identity.Principal, string, app.ScheduleActionCommand, string) (app.ScheduleActionResult, error)
	Runs(context.Context, identity.Principal, string, app.PageRequest) (app.ScheduleRunPage, error)
}

type scheduleRoutesService = ScheduleService

type scheduleBudgetRequest struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

type scheduleCreateRequest struct {
	HostID             string                       `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             schedule.Timing              `json:"timing"`
	Budget             *scheduleBudgetRequest       `json:"budget"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
}

type scheduleUpdateRequest struct {
	ExpectedVersion    *int64                        `json:"expected_version"`
	HostID             json.RawMessage               `json:"host_id"`
	ModelID            *string                       `json:"model_id"`
	Objective          *string                       `json:"objective"`
	Timing             *schedule.Timing              `json:"timing"`
	Budget             *scheduleBudgetRequest        `json:"budget"`
	MisfirePolicy      *schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents *[]schedule.NotificationEvent `json:"notification_events"`
	Observation        json.RawMessage               `json:"observation"`
}

type scheduleActionRequest struct {
	Action           schedule.Action           `json:"action"`
	ExpectedVersion  *int64                    `json:"expected_version"`
	CurrentRunAction schedule.CurrentRunAction `json:"current_run_action"`
}

type scheduleBudgetResponse struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

type scheduleRunResponse struct {
	ID                 string                       `json:"id"`
	ScheduleID         string                       `json:"schedule_id"`
	ScheduleVersion    int64                        `json:"schedule_version"`
	OccurrenceKey      string                       `json:"occurrence_key"`
	ScheduledAt        *time.Time                   `json:"scheduled_at"`
	CoalescedCount     int64                        `json:"coalesced_count"`
	Status             schedule.RunStatus           `json:"status"`
	SkipReason         *schedule.SkipReason         `json:"skip_reason"`
	TaskID             *string                      `json:"task_id"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
	ObservationOutcome schedule.ObservationOutcome  `json:"observation_outcome"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
	FinishedAt         *time.Time                   `json:"finished_at"`
}

// Keep this response explicit: the domain Schedule also contains owner and
// authorization credentials, which are not part of the public API shape.
type scheduleResponse struct {
	ID                 string                       `json:"id"`
	Version            int64                        `json:"version"`
	Status             schedule.Status              `json:"status"`
	HostID             *string                      `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             schedule.Timing              `json:"timing"`
	Budget             scheduleBudgetResponse       `json:"budget"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
	NextRunAt          *time.Time                   `json:"next_run_at"`
	LastRun            *scheduleRunResponse         `json:"last_run"`
	CurrentRun         *scheduleRunResponse         `json:"current_run"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
}

type scheduleListResponse struct {
	Items      []scheduleResponse `json:"items"`
	NextCursor string             `json:"next_cursor"`
}

type scheduleRunListResponse struct {
	Items      []scheduleRunResponse `json:"items"`
	NextCursor string                `json:"next_cursor"`
}

type scheduleActionResponse struct {
	Schedule   scheduleResponse     `json:"schedule"`
	CurrentRun *scheduleRunResponse `json:"current_run"`
}

var scheduleIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

// registerScheduleRoutes is kept separate from api.New so the root wiring can
// supply ScheduleService without coupling this adapter to the api dependency
// container.
func (h *api) registerSchedules(group *gin.RouterGroup, service ScheduleService) {
	group.GET("/schedules", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		status, ok := scheduleStatusQuery(c)
		if !ok {
			return
		}
		page, ok := schedulePageRequest(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, status, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]scheduleResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, toScheduleResponse(value))
		}
		c.JSON(http.StatusOK, scheduleListResponse{Items: items, NextCursor: result.NextCursor})
	})

	group.POST("/schedules", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request scheduleCreateRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		key, ok := scheduleIdempotencyKey(c)
		if !ok {
			return
		}
		command := app.CreateScheduleCommand{
			HostID: request.HostID, ModelID: request.ModelID, Objective: request.Objective,
			Timing: request.Timing, MisfirePolicy: request.MisfirePolicy,
			NotificationEvents: request.NotificationEvents, Observation: request.Observation,
		}
		if request.Budget != nil {
			command.Budget = task.Budget{
				ActiveSeconds: request.Budget.ActiveSeconds,
				ToolCalls:     request.Budget.ToolCalls,
				ModelTokens:   request.Budget.ModelTokens,
			}
		}
		value, err := service.Create(c.Request.Context(), principal, command, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, toScheduleResponse(value))
	})

	group.GET("/schedules/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "schedule_id", schedule.ValidID)
		if !ok {
			return
		}
		value, err := service.Get(c.Request.Context(), principal, id)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toScheduleResponse(value))
	})

	group.PUT("/schedules/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "schedule_id", schedule.ValidID)
		if !ok {
			return
		}
		var request scheduleUpdateRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		key, ok := scheduleIdempotencyKey(c)
		if !ok {
			return
		}
		if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 {
			writeError(c, domain.InvalidArgument("expected_version", "expected_version is required and must be positive"))
			return
		}
		hostID, err := parseScheduleHostID(request.HostID)
		if err != nil {
			writeError(c, invalidScheduleBody())
			return
		}
		command := app.UpdateScheduleCommand{
			ExpectedVersion: *request.ExpectedVersion, HostID: hostID, ModelID: request.ModelID,
			Objective: request.Objective, Timing: request.Timing, MisfirePolicy: request.MisfirePolicy,
			NotificationEvents: request.NotificationEvents,
		}
		if request.Budget != nil {
			budget := task.Budget{
				ActiveSeconds: request.Budget.ActiveSeconds,
				ToolCalls:     request.Budget.ToolCalls,
				ModelTokens:   request.Budget.ModelTokens,
			}
			command.Budget = &budget
		}
		if len(request.Observation) != 0 {
			command.ObservationSet = true
			if !bytes.Equal(bytes.TrimSpace(request.Observation), []byte("null")) {
				var observation schedule.ObservationSpec
				if err := decodeStrictJSONBytes(request.Observation, &observation); err != nil {
					writeError(c, invalidScheduleBody())
					return
				}
				command.Observation = &observation
			}
		}
		value, err := service.Update(c.Request.Context(), principal, id, command, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toScheduleResponse(value))
	})

	group.POST("/schedules/:id/actions", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "schedule_id", schedule.ValidID)
		if !ok {
			return
		}
		var request scheduleActionRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 {
			writeError(c, domain.InvalidArgument("expected_version", "expected_version is required and must be positive"))
			return
		}
		key, ok := scheduleIdempotencyKey(c)
		if !ok {
			return
		}
		result, err := service.Action(c.Request.Context(), principal, id, app.ScheduleActionCommand{
			Action: request.Action, ExpectedVersion: *request.ExpectedVersion, CurrentRunAction: request.CurrentRunAction,
		}, key)
		if err != nil {
			writeError(c, err)
			return
		}
		var currentRun *scheduleRunResponse
		if result.CurrentRun != nil {
			mapped := toScheduleRunResponse(*result.CurrentRun)
			currentRun = &mapped
		}
		c.JSON(http.StatusOK, scheduleActionResponse{
			Schedule: toScheduleResponse(result.Schedule), CurrentRun: currentRun,
		})
	})

	group.GET("/schedules/:id/runs", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "schedule_id", schedule.ValidID)
		if !ok {
			return
		}
		page, ok := schedulePageRequest(c)
		if !ok {
			return
		}
		result, err := service.Runs(c.Request.Context(), principal, id, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]scheduleRunResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, toScheduleRunResponse(value))
		}
		c.JSON(http.StatusOK, scheduleRunListResponse{Items: items, NextCursor: result.NextCursor})
	})
}

func registerScheduleRoutes(group *gin.RouterGroup, service scheduleRoutesService) {
	(&api{}).registerSchedules(group, service)
}

func scheduleStatusQuery(c *gin.Context) (schedule.Status, bool) {
	value, ok := singleQuery(c, "status")
	if !ok {
		return "", false
	}
	if _, supplied := c.Request.URL.Query()["status"]; supplied && !schedule.Status(value).Valid() {
		writeError(c, domain.InvalidArgument("status", "schedule status is invalid"))
		return "", false
	}
	return schedule.Status(value), true
}

func schedulePageRequest(c *gin.Context) (app.PageRequest, bool) {
	page, ok := requestPage(c)
	if !ok {
		return app.PageRequest{}, false
	}
	if _, supplied := c.Request.URL.Query()["cursor"]; supplied && page.Cursor == "" {
		writeError(c, domain.InvalidArgument("cursor", "page cursor is invalid"))
		return app.PageRequest{}, false
	}
	return page, true
}

func scheduleIdempotencyKey(c *gin.Context) (string, bool) {
	key, ok := requiredIdempotencyKey(c)
	if !ok {
		return "", false
	}
	if !scheduleIdempotencyKeyPattern.MatchString(key) {
		writeError(c, domain.InvalidArgument("Idempotency-Key", "idempotency key is invalid"))
		return "", false
	}
	return key, true
}

func toScheduleResponse(value schedule.Schedule) scheduleResponse {
	var hostID *string
	if value.HostID != "" {
		hostID = &value.HostID
	}
	result := scheduleResponse{
		ID: value.ID, Version: value.Version, Status: value.Status, HostID: hostID, ModelID: value.ModelID,
		Objective: value.Objective, Timing: value.Timing,
		Budget: scheduleBudgetResponse{
			ActiveSeconds: value.Budget.ActiveSeconds,
			ToolCalls:     value.Budget.ToolCalls,
			ModelTokens:   value.Budget.ModelTokens,
		},
		MisfirePolicy: value.MisfirePolicy, NotificationEvents: value.NotificationEvents,
		Observation: value.Observation, NextRunAt: value.NextRunAt,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
	if result.NotificationEvents == nil {
		result.NotificationEvents = []schedule.NotificationEvent{}
	}
	if value.LastRun != nil {
		lastRun := toScheduleRunResponse(*value.LastRun)
		result.LastRun = &lastRun
	}
	if value.CurrentRun != nil {
		currentRun := toScheduleRunResponse(*value.CurrentRun)
		result.CurrentRun = &currentRun
	}
	return result
}

func parseScheduleHostID(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		hostID := ""
		return &hostID, nil
	}
	var hostID string
	if err := json.Unmarshal(raw, &hostID); err != nil {
		return nil, err
	}
	return &hostID, nil
}

func toScheduleRunResponse(value schedule.Run) scheduleRunResponse {
	events := value.NotificationEvents
	if events == nil {
		events = []schedule.NotificationEvent{}
	}
	return scheduleRunResponse{
		ID: value.ID, ScheduleID: value.ScheduleID, ScheduleVersion: value.ScheduleVersion,
		OccurrenceKey: value.OccurrenceKey, ScheduledAt: value.ScheduledAt, CoalescedCount: value.CoalescedCount,
		Status: value.Status, SkipReason: value.SkipReason, TaskID: value.TaskID,
		NotificationEvents: events, Observation: value.Observation, ObservationOutcome: value.ObservationOutcome,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, FinishedAt: value.FinishedAt,
	}
}

func decodeStrictJSONBytes(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func invalidScheduleBody() error {
	return domain.InvalidArgument("body", "invalid schedule request")
}
