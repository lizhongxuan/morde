package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	maxEventPage            = 100
	maxRecoveryEvents       = 2048
	maxRecoveryEventPayload = 4 << 20
)

type agentRecoveryCheckpoint struct {
	Inputs         []ModelInput
	PendingCalls   []ModelToolCall
	VerifyClaim    string
	ModelTurns     int
	ModelStep      task.Step
	Reason         string
	CountedCallIDs map[string]bool
}

type recoveryTurn struct {
	stepID        string
	turn          int
	text          string
	calls         []ModelToolCall
	usageKnown    bool
	imagesOmitted []string
	preparedCalls map[string]bool
}

// recoverAgentCheckpoint rebuilds the text-only model history from persisted
// events and operation rows. It resumes only persisted calls with their
// original CallIDs and operation keys; it never asks a model to recreate them.
func (a *Agent) recoverAgentCheckpoint(ctx context.Context, current task.Task, details TaskDetails, batchScope *agentBatchScope) (agentRecoveryCheckpoint, bool, error) {
	checkpoint := agentRecoveryCheckpoint{Inputs: initialAgentInputs(current, batchScope)}
	events, err := a.listAllTaskEvents(ctx, current.OwnerID, current.ID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeRequestTooLarge {
			checkpoint.Reason = "recovery_event_history_exceeds_bound"
			return checkpoint, false, nil
		}
		return checkpoint, false, err
	}
	var turn *recoveryTurn
	var unresolvedModelStep bool
	checkpoint.CountedCallIDs = make(map[string]bool)
	for _, event := range events {
		if event.Type != "task.step" {
			continue
		}
		var entry struct {
			StepID string          `json:"step_id"`
			Kind   string          `json:"kind"`
			Status task.StepStatus `json:"status"`
			Detail json.RawMessage `json:"detail"`
		}
		if err := json.Unmarshal(event.Payload, &entry); err != nil {
			checkpoint.Reason = "recovery_checkpoint_invalid"
			return checkpoint, false, nil
		}
		var detail struct {
			Turn             int             `json:"turn"`
			Text             string          `json:"text"`
			ToolCalls        []ModelToolCall `json:"tool_calls"`
			UsageKnown       *bool           `json:"usage_known"`
			Observation      string          `json:"observation"`
			Verified         *bool           `json:"verified"`
			OperationKey     string          `json:"operation_key"`
			ImagesPresent    *bool           `json:"images_present"`
			ImagesOmitted    bool            `json:"images_omitted"`
			BatchItemID      string          `json:"batch_item_id"`
			BatchAttempt     int32           `json:"batch_attempt"`
			InputFingerprint string          `json:"input_fingerprint"`
			Phase            string          `json:"phase"`
			OperationStatus  string          `json:"operation_status"`
			Reused           bool            `json:"reused"`
		}
		if len(entry.Detail) > 0 {
			if err := json.Unmarshal(entry.Detail, &detail); err != nil {
				checkpoint.Reason = "recovery_checkpoint_invalid"
				return checkpoint, false, nil
			}
		}
		if batchScope != nil {
			if detail.BatchItemID != batchScope.Item.ID {
				continue
			}
			attempt := detail.BatchAttempt
			if attempt == 0 {
				// Checkpoints written before attempt tracking belong to the initial attempt.
				attempt = 1
			}
			if attempt != batchScope.Item.Attempt {
				continue
			}
			if detail.InputFingerprint != batchScope.Fingerprint {
				checkpoint.Reason = "batch_input_changed"
				return checkpoint, false, nil
			}
		} else if detail.BatchItemID != "" {
			continue
		}
		switch entry.Kind {
		case "model":
			switch entry.Status {
			case task.StepRunning:
				unresolvedModelStep = true
			case task.StepSucceeded:
				unresolvedModelStep = false
				if detail.Turn != checkpoint.ModelTurns+1 || detail.UsageKnown == nil || len(detail.Text) > maxAgentResponseBytes || !utf8.ValidString(detail.Text) {
					checkpoint.Reason = "recovery_checkpoint_invalid"
					return checkpoint, false, nil
				}
				if turn != nil {
					completed, ok, reason, err := a.recoverTurn(ctx, current, *turn, checkpoint.Inputs, batchScope, details.ClarificationRequests)
					if err != nil {
						return checkpoint, false, err
					}
					if !ok || len(completed.pending) > 0 || completed.verifyClaim != "" {
						if reason == "" {
							reason = "recovery_checkpoint_order_invalid"
						}
						checkpoint.Reason = reason
						return checkpoint, false, nil
					}
					checkpoint.Inputs = completed.inputs
				}
				turn = &recoveryTurn{stepID: entry.StepID, turn: detail.Turn, text: detail.Text, calls: detail.ToolCalls, usageKnown: *detail.UsageKnown,
					preparedCalls: make(map[string]bool)}
				checkpoint.ModelTurns++
				if detail.Turn > checkpoint.ModelTurns {
					checkpoint.ModelTurns = detail.Turn
				}
			case task.StepUnknown:
				unresolvedModelStep = true
			default:
				checkpoint.Reason = "recovery_checkpoint_invalid"
				return checkpoint, false, nil
			}
		case "tool":
			if turn == nil {
				checkpoint.Reason = "recovery_checkpoint_order_invalid"
				return checkpoint, false, nil
			}
			if detail.Phase == "prepare" && detail.OperationKey != "" {
				turn.preparedCalls[detail.OperationKey] = true
			}
			if detail.ImagesPresent != nil && *detail.ImagesPresent {
				checkpoint.Reason = "recovery_image_context_unavailable"
				return checkpoint, false, nil
			}
			if detail.OperationStatus != "" && detail.ImagesPresent == nil && !detail.ImagesOmitted && !detail.Reused {
				checkpoint.Reason = "recovery_image_policy_unavailable"
				return checkpoint, false, nil
			}
			if detail.ImagesOmitted {
				if detail.OperationKey == "" {
					checkpoint.Reason = "recovery_checkpoint_invalid"
					return checkpoint, false, nil
				}
				turn.imagesOmitted = append(turn.imagesOmitted, detail.OperationKey)
			}
		case "verification":
			if turn == nil || len(turn.calls) > 0 {
				checkpoint.Reason = "recovery_checkpoint_order_invalid"
				return checkpoint, false, nil
			}
			if entry.Status == task.StepRunning {
				// Verification begins with a running event and later appends a
				// terminal event for the same step. Keep the model claim pending
				// until that terminal event so a crash cannot lose it.
				continue
			}
			if entry.Status == task.StepSucceeded && detail.Verified != nil && !*detail.Verified {
				if detail.Observation == "" || len(detail.Observation) > maxAgentResponseBytes {
					checkpoint.Reason = "recovery_checkpoint_invalid"
					return checkpoint, false, nil
				}
				if stringsTrimmed(turn.text) {
					checkpoint.Inputs = append(checkpoint.Inputs, ModelInput{Type: "message", Role: "assistant", Text: turn.text})
				}
				checkpoint.Inputs = append(checkpoint.Inputs, ModelInput{Type: "message", Role: "user", Text: "Untrusted runtime observation: " + detail.Observation})
				turn = nil
			} else if entry.Status == task.StepRunning || entry.Status == task.StepUnknown {
				if !stringsTrimmed(turn.text) {
					checkpoint.Reason = "recovery_checkpoint_invalid"
					return checkpoint, false, nil
				}
				checkpoint.Inputs = append(checkpoint.Inputs, ModelInput{Type: "message", Role: "assistant", Text: turn.text})
				checkpoint.VerifyClaim = turn.text
				checkpoint.ModelStep = findStep(details.Steps, turn.stepID)
				turn = nil
			} else {
				checkpoint.Reason = "recovery_checkpoint_order_invalid"
				return checkpoint, false, nil
			}
		}
	}
	if unresolvedModelStep {
		checkpoint.Reason = "model_result_unknown"
		return checkpoint, false, nil
	}
	if turn != nil {
		completed, ok, reason, err := a.recoverTurn(ctx, current, *turn, checkpoint.Inputs, batchScope, details.ClarificationRequests)
		if err != nil {
			return checkpoint, false, err
		}
		if !ok {
			checkpoint.Reason = reason
			return checkpoint, false, nil
		}
		checkpoint.Inputs = completed.inputs
		checkpoint.PendingCalls = completed.pending
		checkpoint.CountedCallIDs = completed.counted
		checkpoint.VerifyClaim = completed.verifyClaim
		checkpoint.ModelStep = findStep(details.Steps, turn.stepID)
		if completed.modelUsageUnknown && len(completed.pending) > 0 {
			checkpoint.Reason = "model_usage_unknown"
			return checkpoint, false, nil
		}
	}
	if checkpoint.VerifyClaim != "" && !checkpoint.ModelStepExists() {
		checkpoint.Reason = "recovery_checkpoint_invalid"
		return checkpoint, false, nil
	}
	return checkpoint, true, nil
}

type recoveredTurn struct {
	inputs            []ModelInput
	pending           []ModelToolCall
	counted           map[string]bool
	verifyClaim       string
	modelUsageUnknown bool
}

func (a *Agent) recoverTurn(ctx context.Context, current task.Task, turn recoveryTurn, baseInputs []ModelInput, batchScope *agentBatchScope, clarifications []task.ClarificationRequest) (recoveredTurn, bool, string, error) {
	result := recoveredTurn{inputs: append([]ModelInput(nil), baseInputs...), counted: make(map[string]bool)}
	if stringsTrimmed(turn.text) {
		result.inputs = append(result.inputs, ModelInput{Type: "message", Role: "assistant", Text: turn.text})
	}
	if len(turn.calls) == 0 {
		if !stringsTrimmed(turn.text) {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		result.verifyClaim = turn.text
		return result, true, "", nil
	}
	definitions, err := a.tools.Definitions(ctx, current)
	if err != nil {
		return result, false, "recovery_requires_reconciliation", nil
	}
	definitionsByName, err := validateAgentTools(definitions)
	if err != nil {
		return result, false, "recovery_checkpoint_invalid", nil
	}
	if batchScope == nil {
		if _, exists := definitionsByName[agentAskUserToolName]; exists {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		definitionsByName[agentAskUserToolName] = agentAskUserTool
	}
	for _, key := range turn.imagesOmitted {
		if key == "" {
			return result, false, "recovery_checkpoint_invalid", nil
		}
	}
	knownCall := make(map[string]struct{}, len(turn.calls))
	seenCallID := make(map[string]struct{}, len(turn.calls))
	if len(turn.calls) > maxAgentToolCallsPerTurn {
		return result, false, "recovery_checkpoint_invalid", nil
	}
	for _, call := range turn.calls {
		if call.Name != agentAskUserToolName {
			continue
		}
		if len(turn.calls) != 1 || batchScope != nil {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		question, err := parseAgentClarificationQuestion(call.Arguments)
		if err != nil {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		var request *task.ClarificationRequest
		for i := range clarifications {
			if clarifications[i].ModelStepID == turn.stepID && clarifications[i].CallID == call.CallID {
				if request != nil {
					return result, false, "recovery_checkpoint_invalid", nil
				}
				request = &clarifications[i]
			}
		}
		result.inputs = append(result.inputs, modelToolInput(call))
		if request == nil {
			result.pending = append(result.pending, call)
			result.modelUsageUnknown = !turn.usageKnown
			return result, true, "", nil
		}
		if request.Question != question || request.TaskID != current.ID || request.Status != task.ClarificationAnswered || request.Answer == "" ||
			!utf8.ValidString(request.Answer) || utf8.RuneCountInString(request.Answer) > 16384 {
			return result, false, "clarification_response_unavailable", nil
		}
		result.inputs = append(result.inputs, ModelInput{Type: "function_call_output", CallID: call.CallID, Output: request.Answer})
		result.modelUsageUnknown = !turn.usageKnown
		return result, true, "", nil
	}
	for _, call := range turn.calls {
		arguments, err := validateAgentToolArguments(call.Arguments)
		_, knownTool := definitionsByName[call.Name]
		if err != nil || call.CallID == "" || !knownTool || !utf8.ValidString(call.CallID) || len(call.CallID) > 256 {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		if _, exists := seenCallID[call.CallID]; exists {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		seenCallID[call.CallID] = struct{}{}
		key := agentOperationKeyForBatch(call.CallID, batchScope)
		knownCall[key] = struct{}{}
		_ = arguments
		result.inputs = append(result.inputs, modelToolInput(call))
	}
	for _, key := range turn.imagesOmitted {
		if _, ok := knownCall[key]; !ok {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		result.inputs = append(result.inputs, ModelInput{Type: "message", Role: "user", Text: "The tool returned an unsupported or oversized image; it was omitted."})
	}
	type operationOutcome struct {
		call      ModelToolCall
		operation task.Operation
		pending   bool
		terminal  bool
	}
	outcomes := make([]operationOutcome, 0, len(turn.calls))
	for _, call := range turn.calls {
		arguments, _ := validateAgentToolArguments(call.Arguments)
		definition := definitionsByName[call.Name]
		key := agentOperationKeyForBatch(call.CallID, batchScope)
		operation, err := a.store.GetOperation(ctx, current.OwnerID, current.ID, key)
		if domain.CodeOf(err) == domain.CodeNotFound {
			outcomes = append(outcomes, operationOutcome{call: call, pending: true})
			if turn.preparedCalls[key] {
				result.counted[call.CallID] = true
			}
			continue
		}
		if err != nil {
			return result, false, "recovery_requires_reconciliation", err
		}
		hash, hashErr := agentOperationHash(call.Name, arguments)
		if hashErr != nil || !bytes.Equal(operation.RequestHash, hash[:]) ||
			operation.TaskID != current.ID || operation.Key != key || operation.HostID != current.HostID {
			return result, false, "recovery_checkpoint_invalid", nil
		}
		switch operation.Status {
		case task.OperationPrepared, task.OperationSucceeded, task.OperationFailed:
			if operation.LeaseVersion > current.Lease.Version {
				return result, false, "recovery_checkpoint_invalid", nil
			}
		case task.OperationUnknown, task.OperationDispatched:
			if operation.LeaseVersion != current.Lease.Version {
				return result, false, "recovery_checkpoint_invalid", nil
			}
		default:
			return result, false, "recovery_checkpoint_invalid", nil
		}
		result.counted[call.CallID] = true
		if operation.Status == task.OperationUnknown || operation.Status == task.OperationDispatched {
			recovered, ok, err := a.reconcilePersistedOperation(ctx, current, operation, call, arguments, definition.RequiredScope)
			if err != nil {
				return result, false, "recovery_requires_reconciliation", err
			}
			if !ok {
				return result, false, "runtime_operation_unknown", nil
			}
			operation = recovered
		}
		switch operation.Status {
		case task.OperationSucceeded, task.OperationFailed:
			outcomes = append(outcomes, operationOutcome{call: call, operation: operation, terminal: true})
		case task.OperationPrepared:
			outcomes = append(outcomes, operationOutcome{call: call, operation: operation, pending: true})
		default:
			outcomes = append(outcomes, operationOutcome{call: call, operation: operation, pending: true})
		}
	}
	// Agent executes calls in order. A later committed result after an earlier
	// pending call means the durable history cannot be reconstructed safely.
	seenPending := false
	for _, outcome := range outcomes {
		if outcome.pending {
			seenPending = true
			result.pending = append(result.pending, outcome.call)
			continue
		}
		if seenPending || !outcome.terminal {
			return result, false, "recovery_checkpoint_order_invalid", nil
		}
		result.inputs = append(result.inputs, ModelInput{Type: "function_call_output", CallID: outcome.call.CallID, Output: boundedToolOutput(outcome.operation.Result)})
	}
	result.modelUsageUnknown = !turn.usageKnown
	return result, true, "", nil
}

func (a *Agent) reconcilePersistedOperation(ctx context.Context, current task.Task, operation task.Operation, call ModelToolCall, arguments json.RawMessage, scope identity.Scope) (task.Operation, bool, error) {
	reconciler, ok := a.tools.(AgentOperationReconciler)
	if !ok || operation.RuntimeLeaseVersion < 1 || operation.RuntimeInstanceID == "" {
		return operation, false, nil
	}
	result, err := reconciler.ReconcileOperation(ctx, AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: operation.Key,
		OperationID: operation.ID, RequiredScope: scope,
		Fence: AgentHostFence{
			RuntimeGeneration: operation.RuntimeGeneration, RuntimeInstanceID: operation.RuntimeInstanceID,
			ControlVersion: operation.ControlVersion, AIAllowed: true,
		},
	}, operation)
	if err != nil {
		if ctx.Err() != nil {
			return operation, false, ctx.Err()
		}
		return operation, false, nil
	}
	var status task.OperationStatus
	switch result.Status {
	case AgentToolNotFound:
		// The exact, authenticated runtime query proved that this operation ID
		// has no runtime journal row. Rearm the same durable Morde operation ID
		// under the new lease; never allocate a replacement ID or replay without
		// this proof.
		updated, err := a.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operation.Key, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationPrepared,
			RuntimeGeneration: operation.RuntimeGeneration, RuntimeInstanceID: operation.RuntimeInstanceID,
			ControlVersion: operation.ControlVersion, RuntimeLeaseVersion: current.Lease.Version,
			RearmAfterNotFound: true,
		})
		return updated, err == nil, err
	case AgentToolSucceeded:
		status = task.OperationSucceeded
	case AgentToolFailed:
		status = task.OperationFailed
	default:
		return operation, false, nil
	}
	if len(result.Output) > maxAgentToolOutputBytes {
		return operation, false, nil
	}
	updated, err := a.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: operation.Key, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: status, ExternalID: boundedExternalID(result.ExternalID),
		Result: boundedToolOutput(result.Output),
	})
	return updated, err == nil, err
}

func (a *Agent) listAllTaskEvents(ctx context.Context, ownerID, taskID string) ([]task.Event, error) {
	var result []task.Event
	var after int64
	totalBytes := 0
	for {
		events, err := a.store.ListTaskEvents(ctx, ownerID, taskID, after, maxEventPage)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			totalBytes += len(event.Payload)
			if totalBytes > maxRecoveryEventPayload || len(result)+1 > maxRecoveryEvents {
				return nil, domain.NewError(domain.CodeRequestTooLarge, "recovery event history exceeds its bound")
			}
			result = append(result, event)
		}
		if len(events) == 0 || len(events) < int(maxEventPage) {
			return result, nil
		}
		after = events[len(events)-1].Sequence
	}
}

func findStep(steps []task.Step, id string) task.Step {
	for _, step := range steps {
		if step.ID == id {
			return step
		}
	}
	return task.Step{}
}

func (checkpoint agentRecoveryCheckpoint) ModelStepExists() bool {
	return checkpoint.ModelStep.ID != ""
}

func (a *Agent) pauseForRecovery(ctx context.Context, current task.Task, details TaskDetails, usage task.Usage, reason string) (AgentRunResult, error) {
	sequence := maxStepSequence(details.Steps) + 1
	title := "Review recovered task progress"
	summary := "The persisted model and runtime checkpoints could not safely continue this task."
	switch reason {
	case "batch_input_changed":
		title = "Batch input changed"
		summary = "The saved batch input no longer matches its persisted fingerprint, so this item was not executed."
	case "batch_checkpoint_missing", "batch_checkpoint_invalid", "batch_checkpoint_incomplete":
		title = "Batch progress needs review"
		summary = "The saved per item progress is incomplete or invalid, so the batch was paused safely."
	case "batch_model_usage_unknown":
		title = "Batch paused because model usage is unknown"
		summary = "The model did not report usage for a completed item, so the Agent paused before starting another item."
	}
	step, err := newAgentStep(current.ID, sequence, "recovery", task.StepWaitingUser, title, summary)
	if err != nil {
		return AgentRunResult{}, err
	}
	if reason == "" {
		reason = "recovery_checkpoint_invalid"
	}
	return a.wait(ctx, current, step, usage, reason, map[string]any{"resume_requires_persisted_context": true})
}

func stringsTrimmed(value string) bool {
	return strings.TrimSpace(value) != ""
}
