package mcp

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	mcpTaskPageLimit = 50
	mcpTaskItemLimit = 20
)

var taskSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:chk|chs|chx)_[A-Za-z0-9._~-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`),
	regexp.MustCompile(`(?i)\b(?:api[-_ ]?key|aio[-_ ]?key|access[-_ ]?token|password|secret|authorization)\b\s*[:=]\s*[^\s,;]+`),
}

type taskBudgetOutput struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

type taskUsageOutput struct {
	ActiveSeconds int64  `json:"active_seconds"`
	ToolCalls     int64  `json:"tool_calls"`
	ModelTokens   *int64 `json:"model_tokens"`
}

type taskSummaryOutput struct {
	ID             string           `json:"id"`
	HostID         string           `json:"host_id"`
	ApplicationID  string           `json:"application_id,omitempty"`
	ConversationID string           `json:"conversation_id"`
	ModelID        string           `json:"model_id"`
	Objective      string           `json:"objective"`
	Status         task.Status      `json:"status"`
	CurrentStep    string           `json:"current_step,omitempty"`
	Budget         taskBudgetOutput `json:"budget"`
	Usage          taskUsageOutput  `json:"usage"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	TaskURL        string           `json:"task_url,omitempty"`
}

type taskPageOutput struct {
	Items      []taskSummaryOutput `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Truncated  bool                `json:"truncated,omitempty"`
}

type taskDetailOutput struct {
	taskSummaryOutput
	Result               string                          `json:"result,omitempty"`
	WaitingReason        string                          `json:"waiting_reason,omitempty"`
	Steps                []taskStepOutput                `json:"steps"`
	BatchItems           []taskBatchItemOutput           `json:"batch_items"`
	Artifacts            []taskArtifactOutput            `json:"artifacts"`
	PendingRequest       *taskPendingRequestOutput       `json:"pending_request,omitempty"`
	ClarificationRequest *taskClarificationRequestOutput `json:"clarification_request,omitempty"`
	FinishedAt           *time.Time                      `json:"finished_at,omitempty"`
	Truncated            bool                            `json:"truncated,omitempty"`
}

type taskStepOutput struct {
	ID        string          `json:"id"`
	Sequence  int64           `json:"sequence"`
	Kind      string          `json:"kind"`
	Status    task.StepStatus `json:"status"`
	Title     string          `json:"title,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type taskBatchItemOutput struct {
	ID      string               `json:"id"`
	Key     string               `json:"key"`
	Status  task.BatchItemStatus `json:"status"`
	Attempt int32                `json:"attempt"`
	Result  string               `json:"result,omitempty"`
	Error   string               `json:"error,omitempty"`
}

type taskArtifactOutput struct {
	ID           string `json:"id"`
	StepID       string `json:"step_id"`
	Label        string `json:"label"`
	Path         string `json:"path"`
	MediaType    string `json:"media_type"`
	BatchItemID  string `json:"batch_item_id,omitempty"`
	BatchAttempt int32  `json:"batch_attempt,omitempty"`
}

type taskPendingRequestOutput struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Object  string `json:"object"`
	Content string `json:"content"`
	Impact  string `json:"impact"`
}

type taskClarificationRequestOutput struct {
	ID       string `json:"id"`
	Version  int64  `json:"version"`
	Question string `json:"question"`
}

func (s *service) task(ctx context.Context, principal identity.Principal, name string, args map[string]any) (any, error) {
	switch name {
	case "task_submit":
		budget := task.Budget{}
		if raw, ok := args["budget"]; ok {
			var err error
			budget, err = taskBudgetArg(raw)
			if err != nil {
				return nil, err
			}
		}
		result, err := s.deps.Tasks.Submit(ctx, principal, app.SubmitTaskCommand{
			HostID: stringArg(args, "host_id"), ModelID: stringArg(args, "model_id"),
			ApplicationID:  stringArg(args, "application_id"),
			ConversationID: stringArg(args, "conversation_id"), Objective: stringArg(args, "objective"),
			Budget: budget, IdempotencyKey: stringArg(args, "idempotency_key"),
		})
		if err != nil {
			return nil, err
		}
		value, err := taskSummary(principal, result.Task, s.taskURL(result.Task.ID))
		if err != nil {
			return nil, err
		}
		return map[string]any{"task": value, "replayed": result.Replayed}, nil
	case "task_list":
		limit := int32(mcpTaskPageLimit)
		if value, ok := intArg(args, "limit"); ok {
			limit = int32(value)
		}
		page, err := s.deps.Tasks.List(ctx, principal, app.TaskListRequest{
			Status: task.Status(stringArg(args, "status")),
			Page:   app.PageRequest{Limit: limit, Cursor: stringArg(args, "cursor")},
		})
		if err != nil {
			return nil, err
		}
		output := taskPageOutput{Items: make([]taskSummaryOutput, 0, min(len(page.Items), int(limit))), NextCursor: page.NextCursor}
		for index, value := range page.Items {
			if index >= int(limit) {
				output.Truncated = true
				break
			}
			item, err := taskSummary(principal, value, s.taskURL(value.ID))
			if err != nil {
				return nil, err
			}
			output.Items = append(output.Items, item)
		}
		return output, nil
	case "task_get":
		taskID := stringArg(args, "task_id")
		details, err := s.deps.Tasks.Get(ctx, principal, taskID)
		if err != nil {
			return nil, err
		}
		if details.Task.ID != taskID {
			return nil, domain.NewError(domain.CodeInternal, "internal error")
		}
		return taskDetail(principal, details, s.taskURL(details.Task.ID))
	case "task_pause", "task_resume", "task_cancel":
		action := task.Action(strings.TrimPrefix(name, "task_"))
		result, err := s.deps.Tasks.RequestAction(ctx, principal, stringArg(args, "task_id"), action,
			task.Status(stringArg(args, "expected_status")), stringArg(args, "idempotency_key"))
		if err != nil {
			return nil, err
		}
		if result.Task.ID != stringArg(args, "task_id") || result.Task.OwnerID != principal.UserID || result.Action != action {
			return nil, domain.NewError(domain.CodeInternal, "internal error")
		}
		unsettled := make([]string, 0, min(len(result.UnsettledOperationIDs), mcpTaskItemLimit))
		for _, id := range result.UnsettledOperationIDs {
			if !domain.ValidOperationID(id) {
				return nil, domain.NewError(domain.CodeInternal, "internal error")
			}
			if len(unsettled) == mcpTaskItemLimit {
				break
			}
			unsettled = append(unsettled, id)
		}
		return map[string]any{
			"task_id": result.Task.ID, "action": result.Action, "status": result.Task.Status,
			"unsettled_operation_ids": unsettled, "task_url": s.taskURL(result.Task.ID),
		}, nil
	case "task_respond":
		taskID := stringArg(args, "task_id")
		version, _ := intArg(args, "version")
		key := stringArg(args, "idempotency_key")
		if pendingID := stringArg(args, "pending_id"); pendingID != "" {
			if err := s.deps.Tasks.RespondToPendingRequest(ctx, principal, taskID, pendingID, version,
				task.PendingDecision(stringArg(args, "decision")), key); err != nil {
				return nil, err
			}
			return map[string]any{"task_id": taskID, "pending_id": pendingID, "version": version, "responded": true}, nil
		}
		clarificationID := stringArg(args, "clarification_id")
		if err := s.deps.Tasks.AnswerClarificationRequest(ctx, principal, taskID, clarificationID, version,
			stringArg(args, "answer"), key); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": taskID, "clarification_id": clarificationID, "version": version, "responded": true}, nil
	default:
		return nil, domain.NewError(domain.CodeInternal, "unknown task tool")
	}
}

func (s *service) taskURL(taskID string) string {
	portal := strings.TrimSpace(s.deps.Domains.Portal)
	if portal == "" || !domain.ValidTaskID(taskID) {
		return ""
	}
	return (&url.URL{Scheme: s.deps.Domains.Scheme, Host: portal, Path: "/tasks/" + taskID}).String()
}

func taskSummary(principal identity.Principal, value task.Task, taskURL string) (taskSummaryOutput, error) {
	if !domain.ValidTaskID(value.ID) || value.OwnerID != principal.UserID || !domain.ValidHostID(value.HostID) ||
		(value.ApplicationID != "" && !app.ValidHostApplicationID(value.ApplicationID)) || !validMCPTaskStatus(value.Status) {
		return taskSummaryOutput{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return taskSummaryOutput{
		ID: value.ID, HostID: value.HostID, ApplicationID: value.ApplicationID, ConversationID: value.ConversationID, ModelID: boundedText(value.ModelID, 256),
		Objective: boundedText(value.Objective, 1024), Status: value.Status, CurrentStep: boundedText(value.CurrentStep, 256),
		Budget: taskBudgetOutput{ActiveSeconds: value.Budget.ActiveSeconds, ToolCalls: value.Budget.ToolCalls, ModelTokens: value.Budget.ModelTokens},
		Usage:  taskUsageOutput{ActiveSeconds: value.Usage.ActiveSeconds, ToolCalls: value.Usage.ToolCalls, ModelTokens: value.Usage.ModelTokens}, CreatedAt: value.CreatedAt,
		UpdatedAt: value.UpdatedAt, TaskURL: taskURL,
	}, nil
}

func taskDetail(principal identity.Principal, details app.TaskDetails, taskURL string) (taskDetailOutput, error) {
	summary, err := taskSummary(principal, details.Task, taskURL)
	if err != nil {
		return taskDetailOutput{}, err
	}
	output := taskDetailOutput{
		taskSummaryOutput: summary, Result: boundedText(details.Task.Result, 8192),
		WaitingReason: boundedText(details.Task.WaitingReason, 512), Steps: make([]taskStepOutput, 0, min(len(details.Steps), mcpTaskItemLimit)),
		BatchItems: make([]taskBatchItemOutput, 0, min(len(details.BatchItems), mcpTaskItemLimit)),
		Artifacts:  make([]taskArtifactOutput, 0, min(len(details.Artifacts), mcpTaskItemLimit)), FinishedAt: details.Task.FinishedAt,
		Truncated: len(details.Steps) > mcpTaskItemLimit || len(details.BatchItems) > mcpTaskItemLimit || len(details.Artifacts) > mcpTaskItemLimit,
	}
	for index, value := range details.Steps {
		if index >= mcpTaskItemLimit {
			break
		}
		if !domain.ValidStepID(value.ID) || value.TaskID != details.Task.ID || value.Sequence < 1 {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		output.Steps = append(output.Steps, taskStepOutput{
			ID: value.ID, Sequence: value.Sequence, Kind: boundedText(value.Kind, 128), Status: value.Status,
			Title: boundedText(value.Title, 256), Summary: boundedText(value.Summary, 1000), CreatedAt: value.CreatedAt,
		})
	}
	for index, value := range details.BatchItems {
		if index >= mcpTaskItemLimit {
			break
		}
		if !domain.ValidBatchItemID(value.ID) || value.TaskID != details.Task.ID || value.Attempt < 1 {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		output.BatchItems = append(output.BatchItems, taskBatchItemOutput{
			ID: value.ID, Key: boundedText(value.Key, 128), Status: value.Status, Attempt: value.Attempt,
			Result: boundedText(value.Result, 1000), Error: boundedText(value.Error, 512),
		})
	}
	for index, value := range details.Artifacts {
		if index >= mcpTaskItemLimit {
			break
		}
		if !domain.ValidArtifactID(value.ID) || !domain.ValidStepID(value.StepID) || value.TaskID != details.Task.ID {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		output.Artifacts = append(output.Artifacts, taskArtifactOutput{
			ID: value.ID, StepID: value.StepID, Label: boundedText(value.Label, 256), Path: boundedText(value.Path, 1024),
			MediaType: boundedText(value.MediaType, 128), BatchItemID: boundedText(value.BatchItemID, 64), BatchAttempt: value.BatchAttempt,
		})
	}
	if pending := details.PendingRequest; pending != nil && pending.Status == task.PendingRequestPending {
		if details.Task.Status != task.StatusWaitingUser || pending.TaskID != details.Task.ID || !domain.ValidPendingRequestID(pending.ID) || pending.Version < 1 || pending.Kind != "approval" {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		output.PendingRequest = &taskPendingRequestOutput{
			ID: pending.ID, Version: pending.Version, Kind: boundedText(pending.Kind, 128), Title: boundedText(pending.Title, 512),
			Object: boundedText(pending.Object, 512), Content: boundedText(pending.Content, 512), Impact: boundedText(pending.Impact, 512),
		}
	}
	for _, clarification := range details.ClarificationRequests {
		if clarification.Status != task.ClarificationPending {
			continue
		}
		if details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "clarification_required" || clarification.TaskID != details.Task.ID || !domain.ValidPendingRequestID(clarification.ID) || clarification.Version < 1 {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if output.ClarificationRequest != nil {
			return taskDetailOutput{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		output.ClarificationRequest = &taskClarificationRequestOutput{
			ID: clarification.ID, Version: clarification.Version, Question: boundedText(clarification.Question, 2048),
		}
	}
	return output, nil
}

func boundedText(value string, maxRunes int) string {
	if !utf8.ValidString(value) {
		return ""
	}
	for _, pattern := range taskSecretPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func validMCPTaskStatus(value task.Status) bool {
	switch value {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusPaused,
		task.StatusCancelling, task.StatusCompleted, task.StatusFailed, task.StatusCancelled:
		return true
	default:
		return false
	}
}
