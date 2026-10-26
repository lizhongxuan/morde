package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type scheduleMCPRecorder struct {
	createCalls   int
	createCommand app.CreateScheduleCommand
	createKey     string
	principal     identity.Principal
	updateCommand app.UpdateScheduleCommand
	updateID      string
}

func (s *scheduleMCPRecorder) Create(_ context.Context, principal identity.Principal, command app.CreateScheduleCommand, key string) (schedule.Schedule, error) {
	s.createCalls++
	s.principal, s.createCommand, s.createKey = principal, command, key
	next, _ := schedule.FirstOccurrence(command.Timing, time.Now().UTC())
	now := time.Now().UTC()
	return schedule.Schedule{ID: "j1234567", Version: 1, Status: schedule.StatusEnabled, HostID: command.HostID,
		ModelID: command.ModelID, Objective: command.Objective, Timing: command.Timing, Budget: command.Budget,
		MisfirePolicy: command.MisfirePolicy, NotificationEvents: command.NotificationEvents,
		NextRunAt: next.ScheduledAt, CreatedAt: now, UpdatedAt: now}, nil
}
func (*scheduleMCPRecorder) List(context.Context, identity.Principal, app.ScheduleListRequest) (app.SchedulePage, error) {
	return app.SchedulePage{}, nil
}
func (*scheduleMCPRecorder) Get(context.Context, identity.Principal, string) (schedule.Schedule, error) {
	return schedule.Schedule{}, nil
}
func (s *scheduleMCPRecorder) Update(_ context.Context, _ identity.Principal, id string, command app.UpdateScheduleCommand) (schedule.Schedule, error) {
	s.updateID, s.updateCommand = id, command
	return schedule.Schedule{ID: id, Version: 2, Status: schedule.StatusEnabled, HostID: "h1234567", ModelID: "p1234567", Objective: "check project changes",
		Timing: schedule.Timing{Kind: schedule.TimingOnce, RunAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		Budget: task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 1000}, MisfirePolicy: schedule.MisfireSkip,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, nil
}
func (*scheduleMCPRecorder) Action(context.Context, identity.Principal, string, app.ScheduleActionCommand, string) (app.ScheduleActionResult, error) {
	return app.ScheduleActionResult{}, nil
}
func (*scheduleMCPRecorder) Runs(context.Context, identity.Principal, string, app.PageRequest) (app.ScheduleRunPage, error) {
	return app.ScheduleRunPage{}, nil
}

func TestScheduleMCPPrepareAndCreateShareAuthorizedScheduleContract(t *testing.T) {
	principal := taskPrincipal(identity.ScopeScheduleRead, identity.ScopeScheduleManage)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_schedule": principal}}
	schedules := &scheduleMCPRecorder{}
	handler, err := New(Deps{
		PreviewPortMin: 1024, Config: config.MCP{Enabled: true, MaxOutputBytes: 4096, ScreenshotMaxWidth: 1280},
		Domains: config.Domains{Scheme: "https", HostSuffix: "hosts.example.test"},
		Hosts:   &hostServiceStub{value: host.Host{ID: "h1234567", State: host.StateRunning}}, Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{},
		Tasks: newTaskServiceStub(), Schedules: schedules, Auth: verifier, Audit: &auditStub{}, Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	session := connectTaskMCP(t, server.URL, "chk_schedule")
	timing := map[string]any{"kind": "once", "run_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "time_zone": "UTC"}
	arguments := map[string]any{
		"host_id": "h1234567", "model_id": "p1234567", "objective": "check project changes", "timing": timing,
		"budget":              map[string]any{"active_seconds": 60, "tool_calls": 2, "model_tokens": 1000},
		"notification_events": []any{"failed"},
		"observation":         map[string]any{"url": "https://example.test/status", "selector": "#status", "field": "text"},
	}
	prepared, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "schedule_prepare", Arguments: arguments})
	var preview map[string]any
	if err == nil && prepared != nil && !prepared.IsError {
		err = json.Unmarshal([]byte(textResult(prepared)), &preview)
	}
	if err != nil || prepared == nil || prepared.IsError || preview["valid"] != true {
		t.Fatalf("schedule_prepare result=%+v err=%v", prepared, err)
	}
	if schedules.createCalls != 0 {
		t.Fatal("schedule_prepare persisted a schedule")
	}

	createArgs := make(map[string]any, len(arguments)+1)
	for key, value := range arguments {
		createArgs[key] = value
	}
	createArgs["idempotency_key"] = "schedule-create-001"
	created, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "schedule_create", Arguments: createArgs})
	if err != nil || created.IsError || schedules.createCalls != 1 {
		t.Fatalf("schedule_create result=%+v err=%v calls=%d", created, err, schedules.createCalls)
	}
	if schedules.createKey != "schedule-create-001" || schedules.principal.UserID != principal.UserID ||
		schedules.createCommand.HostID != "h1234567" || schedules.createCommand.ModelID != "p1234567" ||
		schedules.createCommand.Budget != (task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 1000}) ||
		len(schedules.createCommand.NotificationEvents) != 1 || schedules.createCommand.NotificationEvents[0] != schedule.NotifyFailed {
		t.Fatalf("schedule create mapping = key %q, principal %+v, command %#v", schedules.createKey, schedules.principal, schedules.createCommand)
	}
	if schedules.createCommand.Observation == nil || schedules.createCommand.Observation.URL != "https://example.test/status" {
		t.Fatalf("schedule create observation = %#v", schedules.createCommand.Observation)
	}
	updated, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "schedule_update", Arguments: map[string]any{
		"schedule_id": "j1234567", "expected_version": 1, "observation": nil,
	}})
	if err != nil || updated == nil || updated.IsError || schedules.updateID != "j1234567" || !schedules.updateCommand.ObservationSet || schedules.updateCommand.Observation != nil {
		t.Fatalf("schedule_update clear observation result=%+v message=%q err=%v id=%q command=%#v", updated, textResult(updated), err, schedules.updateID, schedules.updateCommand)
	}

	verifier.set("chk_schedule", taskPrincipal(identity.ScopeScheduleRead))
	denied, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "schedule_create", Arguments: createArgs})
	if err != nil || denied == nil || !denied.IsError || schedules.createCalls != 1 {
		t.Fatalf("schedule_create without manage scope result=%+v err=%v calls=%d", denied, err, schedules.createCalls)
	}

	verifier.set("chk_schedule", principal)
	outsideHost := make(map[string]any, len(arguments))
	for key, value := range arguments {
		outsideHost[key] = value
	}
	outsideHost["host_id"] = "h7654321"
	denied, err = session.CallTool(context.Background(), &sdk.CallToolParams{Name: "schedule_prepare", Arguments: outsideHost})
	if err != nil || denied == nil || !denied.IsError {
		t.Fatalf("schedule_prepare outside Host grant result=%+v err=%v", denied, err)
	}

	var output scheduleOutput
	if err := json.Unmarshal([]byte(textResult(created)), &output); err != nil || output.ID != "j1234567" || output.HostID != "h1234567" {
		t.Fatalf("schedule_create output = %#v, %v", output, err)
	}
}
