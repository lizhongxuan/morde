package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const scheduleTimingSchema = `{"type":"object","properties":{"kind":{"type":"string","enum":["once","daily","weekly"]},"run_at":{"type":"string"},"starts_on":{"type":"string"},"local_time":{"type":"string"},"time_zone":{"type":"string"},"weekdays":{"type":"array","items":{"type":"integer","minimum":1,"maximum":7}}},"required":["kind","time_zone"],"additionalProperties":false}`
const scheduleBudgetSchema = `{"type":"object","properties":{"active_seconds":{"type":"integer","minimum":1,"maximum":1800},"tool_calls":{"type":"integer","minimum":1,"maximum":100},"model_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["active_seconds","tool_calls","model_tokens"],"additionalProperties":false}`
const scheduleEventsSchema = `{"type":"array","items":{"type":"string","enum":["completed","failed","waiting_user"]},"uniqueItems":true}`
const scheduleObservationSchema = `{"type":"object","properties":{"url":{"type":"string","format":"uri","minLength":1,"maxLength":2048,"pattern":"^https?://"},"selector":{"type":"string","minLength":1,"maxLength":1024},"field":{"type":"string","enum":["text","value"]}},"required":["url","selector","field"],"additionalProperties":false}`

func scheduleToolSpecs() []toolSpec {
	createProperties := `"host_id":{"type":"string"},"model_id":{"type":"string","minLength":1,"maxLength":256},"objective":{"type":"string","minLength":1,"maxLength":32768},"timing":` + scheduleTimingSchema + `,"budget":` + scheduleBudgetSchema + `,"misfire_policy":{"type":"string","enum":["skip","run_once"]},"notification_events":` + scheduleEventsSchema + `,"observation":` + scheduleObservationSchema
	createRequired := `"host_id","model_id","objective","timing"`
	return []toolSpec{
		{name: "schedule_prepare", description: "Validate a proposed schedule and preview its next occurrence. An optional observation checks one CSS-selected web field after each scheduled task; its first successful sample becomes the baseline. This does not save it.", required: []string{"host_id", "model_id", "objective", "timing"}, properties: map[string]string{"host_id": "string", "model_id": "string", "objective": "string", "timing": "object", "budget": "object", "misfire_policy": "string", "notification_events": "array", "observation": "object"}, schema: `{"type":"object","properties":{` + createProperties + `},"required":[` + createRequired + `],"additionalProperties":false}`},
		{name: "schedule_create", description: "Persist a reviewed schedule. Requires schedule:manage and the target Host grant. Supply a fresh idempotency_key for a new schedule.", required: []string{"host_id", "model_id", "objective", "timing", "idempotency_key"}, properties: map[string]string{"host_id": "string", "model_id": "string", "objective": "string", "timing": "object", "budget": "object", "misfire_policy": "string", "notification_events": "array", "observation": "object", "idempotency_key": "string"}, schema: `{"type":"object","properties":{` + createProperties + `,"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":[` + createRequired + `,"idempotency_key"],"additionalProperties":false}`},
		{name: "schedule_list", description: "List schedules currently visible to this API Key.", properties: map[string]string{"status": "string", "limit": "integer", "cursor": "string"}, schema: `{"type":"object","properties":{"status":{"type":"string","enum":["enabled","disabled","completed"]},"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string","maxLength":1024}},"additionalProperties":false}`},
		{name: "schedule_get", description: "Get one schedule and its latest run summary.", required: []string{"schedule_id"}, properties: map[string]string{"schedule_id": "string"}, schema: `{"type":"object","properties":{"schedule_id":{"type":"string"}},"required":["schedule_id"],"additionalProperties":false}`},
		{name: "schedule_update", description: "Edit a schedule using expected_version to prevent overwriting a newer edit. Set observation to null to clear it; changing its URL, selector or field starts a new baseline.", required: []string{"schedule_id", "expected_version"}, properties: map[string]string{"schedule_id": "string", "expected_version": "integer", "host_id": "string", "model_id": "string", "objective": "string", "timing": "object", "budget": "object", "misfire_policy": "string", "notification_events": "array", "observation": "nullable-object"}, schema: `{"type":"object","properties":{"schedule_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"host_id":{"type":"string"},"model_id":{"type":"string"},"objective":{"type":"string"},"timing":` + scheduleTimingSchema + `,"budget":` + scheduleBudgetSchema + `,"misfire_policy":{"type":"string","enum":["skip","run_once"]},"notification_events":` + scheduleEventsSchema + `,"observation":{"oneOf":[` + scheduleObservationSchema + `,{"type":"null"}]}},"required":["schedule_id","expected_version"],"additionalProperties":false}`},
		{name: "schedule_enable", description: "Enable a disabled schedule. This does not resume or recreate a prior task.", required: []string{"schedule_id", "expected_version", "idempotency_key"}, properties: map[string]string{"schedule_id": "string", "expected_version": "integer", "idempotency_key": "string"}, schema: `{"type":"object","properties":{"schedule_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["schedule_id","expected_version","idempotency_key"],"additionalProperties":false}`},
		{name: "schedule_disable", description: "Disable future occurrences. Choose whether the current task should continue or be cancelled; cancellation remains stopping until confirmed.", required: []string{"schedule_id", "expected_version", "idempotency_key"}, properties: map[string]string{"schedule_id": "string", "expected_version": "integer", "current_run_action": "string", "idempotency_key": "string"}, defaults: map[string]any{"current_run_action": "continue"}, schema: `{"type":"object","properties":{"schedule_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"current_run_action":{"type":"string","enum":["continue","cancel"]},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["schedule_id","expected_version","idempotency_key"],"additionalProperties":false}`},
		{name: "schedule_runs", description: "List a schedule's durable run history.", required: []string{"schedule_id"}, properties: map[string]string{"schedule_id": "string", "limit": "integer", "cursor": "string"}, schema: `{"type":"object","properties":{"schedule_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string","maxLength":1024}},"required":["schedule_id"],"additionalProperties":false}`},
	}
}

type scheduleCreateArgs struct {
	HostID             string                       `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             schedule.Timing              `json:"timing"`
	Budget             *scheduleBudget              `json:"budget,omitempty"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy,omitempty"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events,omitempty"`
	Observation        *schedule.ObservationSpec    `json:"observation,omitempty"`
	IdempotencyKey     string                       `json:"idempotency_key,omitempty"`
}

type scheduleBudget struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

func (b scheduleBudget) taskBudget() task.Budget {
	return task.Budget{ActiveSeconds: b.ActiveSeconds, ToolCalls: b.ToolCalls, ModelTokens: b.ModelTokens}
}

type scheduleOutput struct {
	ID                 string                       `json:"id"`
	Version            int64                        `json:"version"`
	Status             schedule.Status              `json:"status"`
	HostID             string                       `json:"host_id"`
	ModelID            string                       `json:"model_id"`
	Objective          string                       `json:"objective"`
	Timing             schedule.Timing              `json:"timing"`
	Budget             taskBudgetOutput             `json:"budget"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events"`
	Observation        *schedule.ObservationSpec    `json:"observation"`
	NextRunAt          *time.Time                   `json:"next_run_at,omitempty"`
	LastRun            *scheduleRunOutput           `json:"last_run,omitempty"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
}

type scheduleRunOutput struct {
	ID                 string                      `json:"id"`
	ScheduleVersion    int64                       `json:"schedule_version"`
	OccurrenceKey      string                      `json:"occurrence_key"`
	ScheduledAt        *time.Time                  `json:"scheduled_at,omitempty"`
	CoalescedCount     int64                       `json:"coalesced_count"`
	Status             schedule.RunStatus          `json:"status"`
	SkipReason         *schedule.SkipReason        `json:"skip_reason,omitempty"`
	TaskID             *string                     `json:"task_id,omitempty"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
	FinishedAt         *time.Time                  `json:"finished_at,omitempty"`
	ObservationOutcome schedule.ObservationOutcome `json:"observation_outcome"`
}

func decodeScheduleArgs(args map[string]any, target any) error {
	data, err := json.Marshal(args)
	if err != nil {
		return domain.InvalidArgument("arguments", "schedule arguments are invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.InvalidArgument("arguments", "schedule arguments are invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return domain.InvalidArgument("arguments", "schedule arguments are invalid")
	}
	return nil
}

func decodeScheduleObservation(raw []byte) (*schedule.ObservationSpec, error) {
	var value schedule.ObservationSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, domain.InvalidArgument("observation", "observation configuration is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, domain.InvalidArgument("observation", "observation configuration is invalid")
	}
	if err := value.Validate(); err != nil {
		return nil, domain.InvalidArgument("observation", err.Error())
	}
	return &value, nil
}

func (s *service) schedule(ctx context.Context, principal identity.Principal, name string, args map[string]any) (any, error) {
	switch name {
	case "schedule_prepare", "schedule_create":
		var input scheduleCreateArgs
		if err := decodeScheduleArgs(args, &input); err != nil {
			return nil, err
		}
		command, err := input.command()
		if err != nil {
			return nil, err
		}
		if name == "schedule_prepare" {
			next, err := schedule.FirstOccurrence(command.Timing, time.Now().UTC())
			if err != nil {
				return nil, domain.InvalidArgument("timing", err.Error())
			}
			budget := command.Budget
			if budget == (task.Budget{}) {
				budget = task.Budget{ActiveSeconds: task.MaxActiveSeconds, ToolCalls: task.MaxToolCalls, ModelTokens: task.MaxModelTokens}
			}
			return map[string]any{"valid": true, "host_id": command.HostID, "model_id": command.ModelID, "objective": command.Objective,
				"timing": command.Timing, "budget": budget, "misfire_policy": command.MisfirePolicy, "notification_events": command.NotificationEvents,
				"observation": command.Observation, "next_run_at": next.ScheduledAt, "occurrence_key": next.Key}, nil
		}
		if !app.ValidMCPIdempotencyKey(input.IdempotencyKey) {
			return nil, domain.InvalidArgument("idempotency_key", "a unique idempotency key of 8 to 128 safe characters is required")
		}
		value, err := s.deps.Schedules.Create(ctx, principal, command, input.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		return toScheduleOutput(value), nil
	case "schedule_list":
		limit, _ := intArg(args, "limit")
		if limit == 0 {
			limit = 50
		}
		page, err := s.deps.Schedules.List(ctx, principal, app.ScheduleListRequest{Status: schedule.Status(stringArg(args, "status")), Page: app.PageRequest{Limit: int32(limit), Cursor: stringArg(args, "cursor")}})
		if err != nil {
			return nil, err
		}
		items := make([]scheduleOutput, 0, len(page.Items))
		for _, value := range page.Items {
			items = append(items, toScheduleOutput(value))
		}
		return map[string]any{"items": items, "next_cursor": page.NextCursor}, nil
	case "schedule_get":
		value, err := s.deps.Schedules.Get(ctx, principal, stringArg(args, "schedule_id"))
		if err != nil {
			return nil, err
		}
		return toScheduleOutput(value), nil
	case "schedule_update":
		var input struct {
			ScheduleID         string                        `json:"schedule_id"`
			ExpectedVersion    int64                         `json:"expected_version"`
			HostID             *string                       `json:"host_id,omitempty"`
			ModelID            *string                       `json:"model_id,omitempty"`
			Objective          *string                       `json:"objective,omitempty"`
			Timing             *schedule.Timing              `json:"timing,omitempty"`
			Budget             *scheduleBudget               `json:"budget,omitempty"`
			MisfirePolicy      *schedule.MisfirePolicy       `json:"misfire_policy,omitempty"`
			NotificationEvents *[]schedule.NotificationEvent `json:"notification_events,omitempty"`
			Observation        json.RawMessage               `json:"observation,omitempty"`
		}
		if err := decodeScheduleArgs(args, &input); err != nil {
			return nil, err
		}
		if input.HostID == nil && input.ModelID == nil && input.Objective == nil && input.Timing == nil && input.Budget == nil && input.MisfirePolicy == nil && input.NotificationEvents == nil && input.Observation == nil {
			return nil, domain.InvalidArgument("schedule", "at least one schedule field is required")
		}
		var budget *task.Budget
		if input.Budget != nil {
			value := input.Budget.taskBudget()
			budget = &value
		}
		command := app.UpdateScheduleCommand{ExpectedVersion: input.ExpectedVersion, HostID: input.HostID, ModelID: input.ModelID, Objective: input.Objective, Timing: input.Timing, Budget: budget, MisfirePolicy: input.MisfirePolicy, NotificationEvents: input.NotificationEvents}
		if input.Observation != nil {
			command.ObservationSet = true
			if !bytes.Equal(bytes.TrimSpace(input.Observation), []byte("null")) {
				observation, err := decodeScheduleObservation(input.Observation)
				if err != nil {
					return nil, err
				}
				command.Observation = observation
			}
		}
		value, err := s.deps.Schedules.Update(ctx, principal, input.ScheduleID, command)
		if err != nil {
			return nil, err
		}
		return toScheduleOutput(value), nil
	case "schedule_enable", "schedule_disable":
		var input struct {
			ScheduleID       string                    `json:"schedule_id"`
			ExpectedVersion  int64                     `json:"expected_version"`
			CurrentRunAction schedule.CurrentRunAction `json:"current_run_action,omitempty"`
			IdempotencyKey   string                    `json:"idempotency_key"`
		}
		if err := decodeScheduleArgs(args, &input); err != nil {
			return nil, err
		}
		if input.CurrentRunAction == "" {
			input.CurrentRunAction = schedule.CurrentRunContinue
		}
		action := schedule.Action(strings.TrimPrefix(name, "schedule_"))
		result, err := s.deps.Schedules.Action(ctx, principal, input.ScheduleID, app.ScheduleActionCommand{Action: action, ExpectedVersion: input.ExpectedVersion, CurrentRunAction: input.CurrentRunAction}, input.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		return map[string]any{"schedule": toScheduleOutput(result.Schedule), "current_run": runOutputPtr(result.CurrentRun)}, nil
	case "schedule_runs":
		limit, _ := intArg(args, "limit")
		if limit == 0 {
			limit = 50
		}
		page, err := s.deps.Schedules.Runs(ctx, principal, stringArg(args, "schedule_id"), app.PageRequest{Limit: int32(limit), Cursor: stringArg(args, "cursor")})
		if err != nil {
			return nil, err
		}
		items := make([]scheduleRunOutput, 0, len(page.Items))
		for _, value := range page.Items {
			items = append(items, toRunOutput(value))
		}
		return map[string]any{"items": items, "next_cursor": page.NextCursor}, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, "unknown schedule tool")
	}
}

func (a scheduleCreateArgs) command() (app.CreateScheduleCommand, error) {
	if !domain.ValidHostID(a.HostID) || strings.TrimSpace(a.ModelID) == "" || len(a.ModelID) > 256 || strings.TrimSpace(a.Objective) == "" || len(a.Objective) > 32768 {
		return app.CreateScheduleCommand{}, domain.InvalidArgument("schedule", "host, model, and objective are required and valid")
	}
	command := app.CreateScheduleCommand{HostID: a.HostID, ModelID: a.ModelID, Objective: a.Objective, Timing: a.Timing, Observation: a.Observation, MisfirePolicy: a.MisfirePolicy, NotificationEvents: a.NotificationEvents}
	if a.Budget != nil {
		command.Budget = a.Budget.taskBudget()
	}
	if command.Budget != (task.Budget{}) {
		if err := command.Budget.Validate(); err != nil {
			return app.CreateScheduleCommand{}, domain.InvalidArgument("budget", err.Error())
		}
	}
	if command.MisfirePolicy == "" {
		command.MisfirePolicy = schedule.MisfireSkip
	}
	if !command.MisfirePolicy.Valid() {
		return app.CreateScheduleCommand{}, domain.InvalidArgument("misfire_policy", "misfire policy is invalid")
	}
	if command.NotificationEvents == nil {
		command.NotificationEvents = []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser}
	}
	seen := map[schedule.NotificationEvent]bool{}
	for _, event := range command.NotificationEvents {
		if !event.Valid() || seen[event] {
			return app.CreateScheduleCommand{}, domain.InvalidArgument("notification_events", "notification events are invalid or duplicated")
		}
		seen[event] = true
	}
	if err := command.Timing.Validate(); err != nil {
		return app.CreateScheduleCommand{}, domain.InvalidArgument("timing", err.Error())
	}
	if command.Observation != nil {
		if err := command.Observation.Validate(); err != nil {
			return app.CreateScheduleCommand{}, domain.InvalidArgument("observation", err.Error())
		}
	}
	return command, nil
}

func toScheduleOutput(value schedule.Schedule) scheduleOutput {
	return scheduleOutput{ID: value.ID, Version: value.Version, Status: value.Status, HostID: value.HostID, ModelID: value.ModelID,
		Objective: boundedText(value.Objective, 1024), Timing: value.Timing, Budget: taskBudgetOutput{ActiveSeconds: value.Budget.ActiveSeconds, ToolCalls: value.Budget.ToolCalls, ModelTokens: value.Budget.ModelTokens},
		MisfirePolicy: value.MisfirePolicy, NotificationEvents: append([]schedule.NotificationEvent(nil), value.NotificationEvents...), Observation: value.Observation, NextRunAt: value.NextRunAt,
		LastRun: runOutputPtr(value.LastRun), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func runOutputPtr(value *schedule.Run) *scheduleRunOutput {
	if value == nil {
		return nil
	}
	output := toRunOutput(*value)
	return &output
}
func toRunOutput(value schedule.Run) scheduleRunOutput {
	outcome := value.ObservationOutcome
	if outcome == "" {
		outcome = schedule.ObservationUnknown
	}
	return scheduleRunOutput{ID: value.ID, ScheduleVersion: value.ScheduleVersion, OccurrenceKey: value.OccurrenceKey, ScheduledAt: value.ScheduledAt, CoalescedCount: value.CoalescedCount, Status: value.Status, SkipReason: value.SkipReason, TaskID: value.TaskID, ObservationOutcome: outcome, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, FinishedAt: value.FinishedAt}
}
