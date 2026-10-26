package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type agentBatchContextKey struct{}

type agentBatchScope struct {
	Item        task.BatchItem
	Fingerprint string
}

func (a *Agent) runBatch(ctx context.Context, claimed task.Task, initial TaskDetails) (AgentRunResult, error) {
	if len(initial.BatchItems) == 0 || len(initial.BatchItems) > 100 {
		return a.pauseForRecovery(ctx, initial.Task, initial, initial.Task.Usage, "batch_checkpoint_invalid")
	}
	if err := validateAgentBatchItems(initial.Task.ID, initial.BatchItems); err != nil {
		return a.pauseForRecovery(ctx, initial.Task, initial, initial.Task.Usage, "batch_checkpoint_invalid")
	}

	for _, snapshot := range initial.BatchItems {
		details, err := a.refreshTask(ctx, claimed)
		if err != nil {
			return AgentRunResult{}, err
		}
		current := details.Task
		if current.Status != task.StatusRunning {
			return agentResult(current), nil
		}
		item, exists := agentBatchItem(details.BatchItems, snapshot.ID)
		if !exists || item.Key != snapshot.Key || item.Input != snapshot.Input {
			return a.pauseForRecovery(ctx, current, details, current.Usage, "batch_input_changed")
		}
		scope := agentBatchScope{Item: item, Fingerprint: agentBatchFingerprint(current, item)}
		found, valid, err := a.hasBatchFingerprint(ctx, current, scope)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeRequestTooLarge {
				return a.pauseForRecovery(ctx, current, details, current.Usage, "recovery_event_history_exceeds_bound")
			}
			if domain.CodeOf(err) == domain.CodeInvalidState {
				return a.pauseForRecovery(ctx, current, details, current.Usage, "batch_checkpoint_invalid")
			}
			return AgentRunResult{}, err
		}
		if !valid {
			return a.pauseForRecovery(ctx, current, details, current.Usage, "batch_input_changed")
		}
		if item.Status == task.BatchItemQueued && current.Usage.ModelTokens == nil && hasTerminalAgentBatchItem(details.BatchItems) {
			return a.pauseForRecovery(ctx, current, details, current.Usage, "batch_model_usage_unknown")
		}
		if !found && item.Status != task.BatchItemQueued {
			return a.pauseForRecovery(ctx, current, details, current.Usage, "batch_checkpoint_missing")
		}
		itemCtx := withAgentBatchScope(ctx, scope)
		if !found {
			step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "batch_item", task.StepRunning,
				"Checkpoint batch item: "+item.Key, "Saved the immutable input fingerprint before processing this item.")
			if err != nil {
				return AgentRunResult{}, err
			}
			if _, err := a.commit(itemCtx, current, step, cloneTaskUsage(current.Usage), task.StatusRunning, "", current.Result,
				"task.step", map[string]any{"phase": "input_checkpoint"}); err != nil {
				return AgentRunResult{}, err
			}
		}
		if item.Status == task.BatchItemSucceeded || item.Status == task.BatchItemFailed {
			continue
		}
		if item.Status != task.BatchItemRunning {
			if _, err := a.updateBatchItem(ctx, current, item, task.BatchItemRunning, "", ""); err != nil {
				return AgentRunResult{}, err
			}
		}

		result, runErr := a.Run(itemCtx, claimed)
		latest, refreshErr := a.refreshTask(ctx, claimed)
		if refreshErr != nil {
			return AgentRunResult{}, refreshErr
		}
		if ctx.Err() != nil {
			return result, errors.Join(ctx.Err(), runErr)
		}
		if latest.Task.Status != task.StatusRunning {
			return agentResult(latest.Task), nil
		}
		updatedItem, exists := agentBatchItem(latest.BatchItems, item.ID)
		if !exists {
			return AgentRunResult{}, domain.NewError(domain.CodeInvalidState, "batch item disappeared while processing")
		}
		if updatedItem.Status == task.BatchItemSucceeded || updatedItem.Status == task.BatchItemFailed {
			continue
		}
		if result.Status == task.StatusWaitingUser {
			return result, runErr
		}
		if runErr != nil {
			return result, runErr
		}
		if updatedItem.Status != task.BatchItemRunning {
			return a.pauseForRecovery(ctx, latest.Task, latest, latest.Task.Usage, "batch_checkpoint_invalid")
		}
	}

	details, err := a.refreshTask(ctx, claimed)
	if err != nil {
		return AgentRunResult{}, err
	}
	if details.Task.Status != task.StatusRunning {
		return agentResult(details.Task), nil
	}
	succeeded, failed := 0, 0
	for _, item := range details.BatchItems {
		switch item.Status {
		case task.BatchItemSucceeded:
			succeeded++
		case task.BatchItemFailed:
			failed++
		default:
			return a.pauseForRecovery(ctx, details.Task, details, details.Task.Usage, "batch_checkpoint_incomplete")
		}
	}
	usage := cloneTaskUsage(details.Task.Usage)
	sequence := maxStepSequence(details.Steps) + 1
	if failed > 0 {
		step, err := newAgentStep(details.Task.ID, sequence, "batch_result", task.StepFailed, "Batch finished with failed items",
			fmt.Sprintf("Completed %d of %d batch items; %d failed.", succeeded, len(details.BatchItems), failed))
		if err != nil {
			return AgentRunResult{}, err
		}
		result := fmt.Sprintf("Task stopped: %d of %d batch items failed.", failed, len(details.BatchItems))
		if _, err := a.commit(ctx, details.Task, step, usage, task.StatusFailed, "", result, "task.result", map[string]any{
			"batch_items_succeeded": succeeded, "batch_items_failed": failed,
		}); err != nil {
			return AgentRunResult{}, err
		}
		return AgentRunResult{TaskID: details.Task.ID, Status: task.StatusFailed, Result: result, Usage: usage}, &AgentError{Kind: AgentErrorTool}
	}
	step, err := newAgentStep(details.Task.ID, sequence, "batch_result", task.StepSucceeded, "Batch completed",
		fmt.Sprintf("Completed all %d batch items.", len(details.BatchItems)))
	if err != nil {
		return AgentRunResult{}, err
	}
	result := fmt.Sprintf("Completed all %d batch items.", len(details.BatchItems))
	if _, err := a.commit(ctx, details.Task, step, usage, task.StatusCompleted, "", result, "task.result", map[string]any{
		"batch_items_succeeded": succeeded, "batch_items_failed": 0,
	}); err != nil {
		return AgentRunResult{}, err
	}
	return AgentRunResult{TaskID: details.Task.ID, Status: task.StatusCompleted, Result: result, Usage: usage}, nil
}

func (a *Agent) completeBatchItem(ctx context.Context, current task.Task, scope agentBatchScope, step task.Step, usage task.Usage, result string, detail any) (AgentRunResult, error) {
	if len(result) > agentUserVisibleResultBytes || !validUTF8(result) {
		return a.fail(ctx, current, step, usage, AgentErrorModelOutput, nil)
	}
	if _, err := a.updateBatchItem(ctx, current, scope.Item, task.BatchItemSucceeded, result, ""); err != nil {
		return AgentRunResult{}, err
	}
	if _, err := a.commit(ctx, current, step, usage, task.StatusRunning, "", current.Result, "task.step", detail); err != nil {
		return AgentRunResult{}, err
	}
	return AgentRunResult{TaskID: current.ID, Status: task.StatusRunning, Usage: usage}, nil
}

func (a *Agent) updateBatchItem(ctx context.Context, current task.Task, item task.BatchItem, status task.BatchItemStatus, result, failure string) (task.BatchItem, error) {
	item.TaskID = current.ID
	item.Status = status
	item.Result = result
	item.Error = failure
	return a.store.UpdateBatchItem(ctx, current.OwnerID, item, current.Lease.Owner, current.Lease.Version)
}

func (a *Agent) hasBatchFingerprint(ctx context.Context, current task.Task, scope agentBatchScope) (bool, bool, error) {
	events, err := a.listAllTaskEvents(ctx, current.OwnerID, current.ID)
	if err != nil {
		return false, false, err
	}
	found := false
	for _, event := range events {
		if event.Type != "task.step" {
			continue
		}
		var entry struct {
			Detail struct {
				BatchItemID      string `json:"batch_item_id"`
				BatchAttempt     int32  `json:"batch_attempt"`
				InputFingerprint string `json:"input_fingerprint"`
			} `json:"detail"`
		}
		if err := json.Unmarshal(event.Payload, &entry); err != nil {
			return false, false, domain.NewError(domain.CodeInvalidState, "batch checkpoint event is invalid")
		}
		if entry.Detail.BatchItemID != scope.Item.ID {
			continue
		}
		attempt := entry.Detail.BatchAttempt
		if attempt == 0 {
			// Checkpoints written before attempt tracking belong to the initial attempt.
			attempt = 1
		}
		if attempt != scope.Item.Attempt {
			continue
		}
		found = true
		if entry.Detail.InputFingerprint != scope.Fingerprint {
			return true, false, nil
		}
	}
	return found, true, nil
}

func agentBatchFingerprint(current task.Task, item task.BatchItem) string {
	encoded, _ := json.Marshal(struct {
		TaskID        string `json:"task_id"`
		ApplicationID string `json:"application_id,omitempty"`
		Objective     string `json:"objective"`
		ItemID        string `json:"item_id"`
		Key           string `json:"key"`
		Input         string `json:"input"`
	}{current.ID, current.ApplicationID, current.Objective, item.ID, item.Key, item.Input})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validateAgentBatchItems(taskID string, items []task.BatchItem) error {
	seenIDs := make(map[string]bool, len(items))
	seenKeys := make(map[string]bool, len(items))
	for _, item := range items {
		if !domain.ValidBatchItemID(item.ID) || item.TaskID != taskID || !validBoundedText(item.Key, 128) || !validBoundedText(item.Input, 16384) ||
			item.Attempt < 1 || seenIDs[item.ID] || seenKeys[item.Key] {
			return domain.NewError(domain.CodeInvalidState, "batch item identity is invalid")
		}
		seenIDs[item.ID] = true
		seenKeys[item.Key] = true
		switch item.Status {
		case task.BatchItemQueued, task.BatchItemRunning, task.BatchItemSucceeded, task.BatchItemFailed, task.BatchItemUnknown:
		default:
			return domain.NewError(domain.CodeInvalidState, "batch item status is invalid")
		}
	}
	return nil
}

func agentBatchItem(items []task.BatchItem, itemID string) (task.BatchItem, bool) {
	for _, item := range items {
		if item.ID == itemID {
			return item, true
		}
	}
	return task.BatchItem{}, false
}

func hasTerminalAgentBatchItem(items []task.BatchItem) bool {
	for _, item := range items {
		if item.Status == task.BatchItemSucceeded || item.Status == task.BatchItemFailed {
			return true
		}
	}
	return false
}

func hasAgentBatchCheckpoint(steps []task.Step) bool {
	for _, step := range steps {
		if step.Kind == "batch_item" || step.Kind == "batch_result" {
			return true
		}
	}
	return false
}

func batchItemMatchesScope(current task.Task, items []task.BatchItem, scope agentBatchScope) bool {
	item, ok := agentBatchItem(items, scope.Item.ID)
	return ok && item.TaskID == current.ID && item.Key == scope.Item.Key && item.Input == scope.Item.Input &&
		item.Attempt == scope.Item.Attempt &&
		agentBatchFingerprint(current, item) == scope.Fingerprint
}

func initialAgentInputs(current task.Task, scope *agentBatchScope) []ModelInput {
	objective := current.Objective
	if current.ApplicationID == WechatApplicationID {
		objective = "Target application: WeChat (wechat) on Host " + current.HostID + ". Verify the application window, focus, and login state before acting. If any is uncertain, ask the user before interacting with another window.\n\n" + objective
	}
	if scope == nil {
		return []ModelInput{{Type: "message", Role: "user", Text: objective}}
	}
	return []ModelInput{{Type: "message", Role: "user", Text: objective + "\n\nProcess only this batch item.\nItem key: " + scope.Item.Key + "\nItem input:\n" + scope.Item.Input}}
}

func agentOperationKeyForBatch(callID string, scope *agentBatchScope) string {
	if scope == nil {
		return agentOperationKey(callID)
	}
	keyMaterial := scope.Item.ID + "\x00" + callID
	if scope.Item.Attempt > 1 {
		keyMaterial += "\x00attempt:" + fmt.Sprint(scope.Item.Attempt)
	}
	digest := sha256.Sum256([]byte(keyMaterial))
	return "agent:batch:" + hex.EncodeToString(digest[:])
}

func agentBatchScopeFromContext(ctx context.Context) *agentBatchScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(agentBatchContextKey{}).(agentBatchScope)
	if scope.Item.ID == "" {
		return nil
	}
	return &scope
}

func withAgentBatchScope(ctx context.Context, scope agentBatchScope) context.Context {
	return context.WithValue(ctx, agentBatchContextKey{}, scope)
}

func withoutAgentBatchScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentBatchContextKey{}, agentBatchScope{})
}

func addAgentBatchCheckpoint(ctx context.Context, detail any) any {
	scope := agentBatchScopeFromContext(ctx)
	if scope == nil {
		return detail
	}
	values, ok := detail.(map[string]any)
	if !ok {
		values = map[string]any{}
		if detail != nil {
			values["value"] = detail
		}
	} else {
		copied := make(map[string]any, len(values)+2)
		for key, value := range values {
			copied[key] = value
		}
		values = copied
	}
	values["batch_item_id"] = scope.Item.ID
	values["batch_attempt"] = scope.Item.Attempt
	values["input_fingerprint"] = scope.Fingerprint
	return values
}

func validUTF8(value string) bool {
	return utf8.ValidString(value)
}
