package worker

import (
	"context"
	"encoding/json"
	"errors"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type conversationRouteInternalToolTaskStore interface {
	app.ConversationRouteInternalToolStore
	GetTask(context.Context, string, string) (app.TaskDetails, error)
	CommitTaskProgress(context.Context, app.TaskProgress) (task.Event, error)
}

type conversationRouteInternalToolBroker interface {
	Execute(context.Context, task.Task, app.ModelToolCall) (app.AgentToolResult, error)
}

type ConversationRouteInternalToolExecutor struct {
	store  conversationRouteInternalToolTaskStore
	broker conversationRouteInternalToolBroker
}

func NewConversationRouteInternalToolExecutor(store conversationRouteInternalToolTaskStore,
	broker conversationRouteInternalToolBroker) (*ConversationRouteInternalToolExecutor, error) {
	if store == nil || broker == nil {
		return nil, errors.New("internal route tool store and Broker are required")
	}
	return &ConversationRouteInternalToolExecutor{store: store, broker: broker}, nil
}

func (e *ConversationRouteInternalToolExecutor) ExecuteInternalToolTask(ctx context.Context, claimed task.Task) error {
	if e == nil || e.store == nil || e.broker == nil || ctx == nil || !claimed.InternalTool ||
		claimed.ID == "" || claimed.OwnerID == "" || claimed.Status != task.StatusRunning || claimed.Lease.Owner == "" || claimed.Lease.Version < 1 {
		return domain.InvalidArgument("internal_route_tool", "internal route tool claim is invalid")
	}
	call, err := e.store.GetConversationRouteInternalToolCall(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	if call.TaskID != claimed.ID || call.OwnerID != claimed.OwnerID {
		return domain.NewError(domain.CodeInvalidState, "internal route tool call does not match its Task")
	}
	result, toolErr := e.broker.Execute(ctx, claimed, app.ModelToolCall{CallID: call.CallID, Name: call.ToolName, Arguments: call.Arguments})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	details, err := e.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	if details.Task.Status == task.StatusWaitingUser || details.Task.Status == task.StatusCompleted ||
		details.Task.Status == task.StatusFailed || details.Task.Status == task.StatusCancelled {
		return nil
	}
	if details.Task.Status != task.StatusRunning {
		return domain.NewError(domain.CodeConflict, "internal route tool Task is no longer running")
	}
	reply := "网页工具未能返回已确认结果。本次调用不会自动重试；请重新提出查询。"
	completedCall := toolErr == nil && (result.Status == app.AgentToolSucceeded || result.Status == app.AgentToolFailed)
	if completedCall {
		reply = app.ConversationRouteDirectToolReply(call.ToolName, result)
	}
	currentStep := details.Task.CurrentStep
	var steps []task.Step
	if !domain.ValidStepID(currentStep) {
		stepID, err := domain.NewStepID()
		if err != nil {
			return err
		}
		currentStep = stepID
		stepStatus, summary := task.StepFailed, "工具未能返回已确认结果。"
		if completedCall && result.Status == app.AgentToolSucceeded {
			stepStatus, summary = task.StepSucceeded, "只读网页工具已返回结果。"
		} else if completedCall {
			summary = "只读网页工具执行失败。"
		}
		steps = []task.Step{{ID: stepID, TaskID: claimed.ID, Kind: "tool", Status: stepStatus,
			Title: "网页工具调用", Summary: summary, Attempt: 1}}
	}
	payload, err := json.Marshal(map[string]string{"status": "completed", "source": "conversation_route_direct_tool"})
	if err != nil {
		return err
	}
	_, err = e.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.StatusCompleted, CurrentStep: currentStep, Result: reply,
		Usage: details.Task.Usage, Steps: steps, Event: task.Event{Type: "task.result", Payload: payload}, PublishResultMessage: true})
	return err
}

func (e *ConversationRouteInternalToolExecutor) PublishPendingReplies(ctx context.Context) (bool, error) {
	if e == nil || e.store == nil || ctx == nil {
		return false, domain.InvalidArgument("internal_route_tool", "internal route tool reply store is unavailable")
	}
	return e.store.PublishPendingConversationRouteInternalToolReplies(ctx)
}
