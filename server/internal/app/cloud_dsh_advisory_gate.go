package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// Advisory-gate rejections are known pre-dispatch results, not tool transport
// failures. Their stable summaries also let checkpoint replay return the same
// result without executing a call that was already rejected.
func advisoryGateOutput(summary string) string {
	switch summary {
	case "Task plan brief is incomplete; no action was executed.":
		return "The task plan needs a complete brief: fill in the scope, verification method and step acceptance criteria from context, then resubmit."
	case "Task plan is invalid; no action was executed.":
		return "The task plan is invalid or has a dependency cycle. Correct the plan and resubmit."
	case "Reproduction test is missing; no action was executed.":
		return "Run a failing reproduction test before changing code, then retry."
	case "File is outside the fix plan; no action was executed.":
		return "This file is outside fix_plan.changed_files. Update the fix plan if the change is needed, then retry."
	default:
		return ""
	}
}

func (b *CloudDSHToolBroker) advisoryGateResult(ctx context.Context, current task.Task, details TaskDetails,
	call ModelToolCall, recovering bool, checkpointOutput *string, rejection string) (AgentToolResult, bool, error) {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok || !validCloudDSHToolActivityFence(ctx, current, call) ||
		activity.current.CurrentRevision == nil || current.CurrentRevision == nil ||
		*activity.current.CurrentRevision != *current.CurrentRevision ||
		!sameCloudDSHAuthorization(activity.current.Authorization, current.Authorization) {
		return AgentToolResult{}, false, nil
	}
	prior, found, err := matchingInvalidRuntimeArgumentStep(details.Steps, current, activity)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	if found && prior.Status == task.StepFailed && prior.Phase == "prepare" {
		if output := advisoryGateOutput(prior.Summary); output != "" {
			if prior.FailureReason != "" || prior.Retryable {
				return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "recorded pre-dispatch rejection is invalid")
			}
			rejection = prior.Summary
		} else if rejection != "" {
			return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "prior tool result does not match a pre-dispatch rejection")
		}
	} else if found && rejection != "" {
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "prior tool operation must be reconciled")
	}
	output := advisoryGateOutput(rejection)
	if output == "" {
		return AgentToolResult{}, false, nil
	}
	switch call.Name {
	case runtimeToolFileWrite:
		if err := b.runtime.authorize(ctx, current, identity.ScopeComputerOperate); err != nil {
			return AgentToolResult{}, true, err
		}
	case cloudDSHWorkspaceWriteToolName, cloudDSHTaskPlanToolName:
		if err := b.authorizeSkillTask(ctx, current); err != nil {
			return AgentToolResult{}, true, err
		}
	}
	// A record or an uncertain lookup means the operation may already have
	// happened; never turn that situation into permission to retry.
	switch call.Name {
	case cloudDSHTaskPlanToolName:
		reader, available := b.store.(CloudDSHTaskPlanReceiptReader)
		if !available {
			return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "task plan receipt storage is unavailable")
		}
		_, err = reader.GetCloudDSHTaskPlanReceipt(ctx, current.ID, current.OwnerID, call.TaskFence.AgentSessionID, call.CallID)
	case cloudDSHWorkspaceWriteToolName:
		if b.workspaceTools == nil || b.workspaceTools.workspaces == nil {
			return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "Workspace receipt storage is unavailable")
		}
		_, err = b.workspaceTools.workspaces.FindTaskWorkspacePublishReceipt(ctx, current.OwnerID, current.ID, current.WorkspaceID, agentOperationKey(call.CallID))
	case runtimeToolFileWrite:
		_, err = b.store.GetOperation(ctx, current.OwnerID, current.ID, agentOperationKey(call.CallID))
	default:
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "unsupported advisory gate")
	}
	if domain.CodeOf(err) != domain.CodeNotFound {
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "prior tool operation could not be ruled out")
	}
	result := AgentToolResult{Status: AgentToolFailed, Output: output}
	if checkpointOutput != nil && (!recovering || !found || *checkpointOutput != dshToolResultText(result)) {
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "checkpointed rejection does not match its recorded result")
	}
	if found {
		return result, true, nil
	}
	if recovering && checkpointOutput != nil {
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "checkpointed rejection has no recorded result")
	}
	usage := cloneTaskUsage(current.Usage)
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepFailed,
		"Review tool request", rejection)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	if usage.ToolCalls >= current.Budget.ToolCalls {
		step.Status, step.Summary = task.StepWaitingUser, "The task tool call budget is exhausted."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
			return AgentToolResult{}, true, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "tool_call_budget"}, true, nil
	}
	usage.ToolCalls++
	ctx = withTaskToolResultEvidence(ctx, "prepare", taskToolAdvisoryGateFailureReason)
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "phase": "prepare", "failed": true,
	}); err != nil {
		return AgentToolResult{}, true, err
	}
	return result, true, nil
}
