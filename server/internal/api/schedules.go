package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleTimingRequest struct {
	Kind      schedule.TimingKind `json:"kind"`
	RunAt     string              `json:"run_at"`
	StartsOn  string              `json:"starts_on"`
	LocalTime string              `json:"local_time"`
	TimeZone  string              `json:"time_zone"`
	Weekdays  []int               `json:"weekdays"`
}

func (r scheduleTimingRequest) timing() schedule.Timing {
	return schedule.Timing{Kind: r.Kind, RunAt: r.RunAt, StartsOn: r.StartsOn, LocalTime: r.LocalTime, TimeZone: r.TimeZone, Weekdays: r.Weekdays}
}

type scheduleCreateRequest struct {
	HostID             string                       `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             scheduleTimingRequest        `json:"timing"`
	Budget             *taskBudgetRequest           `json:"budget"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
}

type scheduleUpdateRequest struct {
	ExpectedVersion    int64                         `json:"expected_version"`
	HostID             *string                       `json:"host_id"`
	ModelID            *string                       `json:"model_id"`
	Objective          *string                       `json:"objective"`
	Timing             *scheduleTimingRequest        `json:"timing"`
	Budget             *taskBudgetRequest            `json:"budget"`
	MisfirePolicy      *schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents *[]schedule.NotificationEvent `json:"notification_events"`
	Observation        json.RawMessage               `json:"observation"`
}

type scheduleActionRequest struct {
	Action           schedule.Action           `json:"action"`
	ExpectedVersion  int64                     `json:"expected_version"`
	CurrentRunAction schedule.CurrentRunAction `json:"current_run_action"`
}

type schedulePageResponse struct {
	Items      []scheduleResponse `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

type scheduleRunPageResponse struct {
	Items      []scheduleRunResponse `json:"items"`
	NextCursor *string               `json:"next_cursor"`
}

type scheduleResponse struct {
	ID                 string                       `json:"id"`
	Version            int64                        `json:"version"`
	Status             schedule.Status              `json:"status"`
	HostID             string                       `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             schedule.Timing              `json:"timing"`
	Budget             taskBudgetResponse           `json:"budget"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
	NextRunAt          *time.Time                   `json:"next_run_at"`
	LastRun            *scheduleRunResponse         `json:"last_run"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
}

type scheduleRunResponse struct {
	ID                 string                      `json:"id"`
	ScheduleID         string                      `json:"schedule_id"`
	ScheduleVersion    int64                       `json:"schedule_version"`
	OccurrenceKey      string                      `json:"occurrence_key"`
	ScheduledAt        *time.Time                  `json:"scheduled_at"`
	CoalescedCount     int64                       `json:"coalesced_count"`
	Status             schedule.RunStatus          `json:"status"`
	SkipReason         *schedule.SkipReason        `json:"skip_reason"`
	TaskID             *string                     `json:"task_id"`
	ObservationOutcome schedule.ObservationOutcome `json:"observation_outcome"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
	FinishedAt         *time.Time                  `json:"finished_at"`
}

type scheduleActionResponse struct {
	Schedule   scheduleResponse     `json:"schedule"`
	CurrentRun *scheduleRunResponse `json:"current_run"`
}

func (h *api) registerSchedules(group *gin.RouterGroup, service ScheduleService) {
	if service == nil {
		return
	}
	group.GET("/schedules", h.listSchedules(service))
	group.POST("/schedules", h.createSchedule(service))
	group.GET("/schedules/:id", h.getSchedule(service))
	group.PATCH("/schedules/:id", h.updateSchedule(service))
	group.POST("/schedules/:id/actions", h.scheduleAction(service))
	group.GET("/schedules/:id/runs", h.listScheduleRuns(service))
}

func (h *api) listSchedules(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		status := schedule.Status(c.Query("status"))
		if status != "" && !status.Valid() {
			writeError(c, domain.InvalidArgument("status", "schedule status is invalid"))
			return
		}
		result, err := service.List(c.Request.Context(), principal, app.ScheduleListRequest{Status: status, Page: page})
		if err != nil {
			writeError(c, err)
			return
		}
		response := schedulePageResponse{Items: make([]scheduleResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			value, err := toScheduleResponse(principal.UserID, item)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, value)
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) createSchedule(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request scheduleCreateRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		budget := task.Budget{}
		if request.Budget != nil {
			budget = task.Budget{ActiveSeconds: request.Budget.ActiveSeconds, ToolCalls: request.Budget.ToolCalls, ModelTokens: request.Budget.ModelTokens}
		}
		value, err := service.Create(c.Request.Context(), principal, app.CreateScheduleCommand{
			HostID: request.HostID, ModelID: request.ModelID, Objective: request.Objective, Timing: request.Timing.timing(),
			Budget: budget, Observation: request.Observation, MisfirePolicy: request.MisfirePolicy, NotificationEvents: request.NotificationEvents,
		}, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toScheduleResponse(principal.UserID, value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, response)
	}
}

func (h *api) getSchedule(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", schedule.ValidID)
		if !ok {
			return
		}
		value, err := service.Get(c.Request.Context(), principal, id)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toScheduleResponse(principal.UserID, value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) updateSchedule(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", schedule.ValidID)
		if !ok {
			return
		}
		var request scheduleUpdateRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		command := app.UpdateScheduleCommand{ExpectedVersion: request.ExpectedVersion, HostID: request.HostID, ModelID: request.ModelID,
			Objective: request.Objective, MisfirePolicy: request.MisfirePolicy, NotificationEvents: request.NotificationEvents}
		if request.Observation != nil {
			command.ObservationSet = true
			if !bytes.Equal(bytes.TrimSpace(request.Observation), []byte("null")) {
				observation, err := decodeObservationSpec(request.Observation)
				if err != nil {
					writeError(c, domain.InvalidArgument("observation", "observation configuration is invalid"))
					return
				}
				command.Observation = observation
			}
		}
		if request.Timing != nil {
			timing := request.Timing.timing()
			command.Timing = &timing
		}
		if request.Budget != nil {
			budget := task.Budget{ActiveSeconds: request.Budget.ActiveSeconds, ToolCalls: request.Budget.ToolCalls, ModelTokens: request.Budget.ModelTokens}
			command.Budget = &budget
		}
		value, err := service.Update(c.Request.Context(), principal, id, command)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toScheduleResponse(principal.UserID, value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func (h *api) scheduleAction(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", schedule.ValidID)
		if !ok {
			return
		}
		var request scheduleActionRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		if request.Action == schedule.ActionEnable && request.CurrentRunAction != "" {
			writeError(c, domain.InvalidArgument("current_run_action", "current_run_action is only valid when disabling"))
			return
		}
		if request.CurrentRunAction == "" {
			request.CurrentRunAction = schedule.CurrentRunContinue
		}
		result, err := service.Action(c.Request.Context(), principal, id, app.ScheduleActionCommand{
			Action: request.Action, ExpectedVersion: request.ExpectedVersion, CurrentRunAction: request.CurrentRunAction,
		}, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toScheduleResponse(principal.UserID, result.Schedule)
		if err != nil {
			writeError(c, err)
			return
		}
		out := scheduleActionResponse{Schedule: response}
		if result.CurrentRun != nil {
			run, err := toScheduleRunResponse(*result.CurrentRun)
			if err != nil {
				writeError(c, err)
				return
			}
			out.CurrentRun = &run
		}
		c.JSON(http.StatusAccepted, out)
	}
}

func (h *api) listScheduleRuns(service ScheduleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", schedule.ValidID)
		if !ok {
			return
		}
		page, ok := requestPage(c)
		if !ok {
			return
		}
		result, err := service.Runs(c.Request.Context(), principal, id, page)
		if err != nil {
			writeError(c, err)
			return
		}
		response := scheduleRunPageResponse{Items: make([]scheduleRunResponse, 0, len(result.Items)), NextCursor: cursorPointer(result.NextCursor)}
		for _, item := range result.Items {
			value, err := toScheduleRunResponse(item)
			if err != nil {
				writeError(c, err)
				return
			}
			response.Items = append(response.Items, value)
		}
		c.JSON(http.StatusOK, response)
	}
}

func toScheduleResponse(ownerID string, item schedule.Schedule) (scheduleResponse, error) {
	if !schedule.ValidID(item.ID) || item.OwnerID != ownerID || item.Version < 1 || !item.Status.Valid() || !domain.ValidHostID(item.HostID) ||
		item.ModelID == "" || item.Objective == "" || item.Timing.Validate() != nil || !item.MisfirePolicy.Valid() || item.Budget.Validate() != nil || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return scheduleResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if item.Observation != nil && item.Observation.Validate() != nil {
		return scheduleResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	response := scheduleResponse{
		ID: item.ID, Version: item.Version, Status: item.Status, HostID: item.HostID, ModelID: item.ModelID, Objective: item.Objective,
		Timing: item.Timing, Budget: taskBudgetResponse{ActiveSeconds: item.Budget.ActiveSeconds, ToolCalls: item.Budget.ToolCalls, ModelTokens: item.Budget.ModelTokens},
		MisfirePolicy: item.MisfirePolicy, NotificationEvents: item.NotificationEvents, Observation: item.Observation, NextRunAt: item.NextRunAt,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	if item.LastRun != nil {
		run, err := toScheduleRunResponse(*item.LastRun)
		if err != nil {
			return scheduleResponse{}, err
		}
		response.LastRun = &run
	}
	return response, nil
}

func toScheduleRunResponse(item schedule.Run) (scheduleRunResponse, error) {
	if !schedule.ValidRunID(item.ID) || !schedule.ValidID(item.ScheduleID) || item.ScheduleVersion < 1 || item.OccurrenceKey == "" ||
		item.CoalescedCount < 0 || !item.Status.Valid() || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return scheduleRunResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if item.TaskID != nil && !domain.ValidTaskID(*item.TaskID) {
		return scheduleRunResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	outcome := item.ObservationOutcome
	if outcome == "" {
		outcome = schedule.ObservationUnknown
	}
	if !outcome.Valid() {
		return scheduleRunResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return scheduleRunResponse{
		ID: item.ID, ScheduleID: item.ScheduleID, ScheduleVersion: item.ScheduleVersion, OccurrenceKey: item.OccurrenceKey,
		ScheduledAt: item.ScheduledAt, CoalescedCount: item.CoalescedCount, Status: item.Status, SkipReason: item.SkipReason,
		TaskID: item.TaskID, ObservationOutcome: outcome, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, FinishedAt: item.FinishedAt,
	}, nil
}

func decodeObservationSpec(raw []byte) (*schedule.ObservationSpec, error) {
	var value schedule.ObservationSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, domain.InvalidArgument("observation", "observation configuration is invalid")
	}
	return &value, value.Validate()
}
