package worker

import (
	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type cloudDSHReadOnlyRecoveryCheckpoint struct {
	ThroughSequence int64
	Digest          []byte
	ToolsetDigest   []byte
}

func cloudDSHModelRecoveryStartAllowed(state task.ModelRecoveryState, callID, recoveryDSHCallID string,
	attemptStarted, attemptSucceeded bool) (beginAttempt, replayOnly, allowed bool) {
	switch state {
	case task.ModelRecoveryScheduled:
		if !attemptStarted {
			return callID == recoveryDSHCallID, false, callID == recoveryDSHCallID
		}
		if !attemptSucceeded || callID == recoveryDSHCallID {
			return false, false, false
		}
		return false, false, true
	case task.ModelRecoveryAttempting:
		if !attemptStarted {
			return false, callID == recoveryDSHCallID, callID == recoveryDSHCallID
		}
		if !attemptSucceeded || callID == recoveryDSHCallID {
			return false, false, false
		}
		return false, false, true
	default:
		return false, false, true
	}
}

// cloudDSHReadOnlyRecoveryCheckpointFor accepts only a root session whose
// durable checkpoint and complete model-visible tool collection are known.
// The allowlist is deliberately closed to public web search, page reads,
// user-attached Task files, and the root-only result-evidence annotation tool.
// Attachment reads remain scoped to the current Task and task:read; the
// annotation tool only records untrusted references against exact final text.
// Other tools, including Workspace, personal-history, and project reads, are
// outside this recovery path.
func cloudDSHReadOnlyRecoveryCheckpointFor(value task.Task, state *cloudDSHSessionState,
	call app.ModelCall, hasChildSessions bool) (cloudDSHReadOnlyRecoveryCheckpoint, bool) {
	if cloudDSHReadOnlyRecoveryCheckpointFailureReason(value, state, call, hasChildSessions) != "" {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	if state.resume {
		if state.restoreThrough < 0 || len(state.restoreDigest) != 32 || state.readThrough != state.restoreThrough {
			return cloudDSHReadOnlyRecoveryCheckpoint{}, false
		}
	} else if state.restoreThrough != -1 || state.readThrough != -1 {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	if call.Task.RecoveryDSHCallID == "" {
		if state.through != call.Task.ThroughSequence {
			return cloudDSHReadOnlyRecoveryCheckpoint{}, false
		}
	} else if call.Task.ThroughSequence != state.restoreThrough || state.through < state.restoreThrough {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	digest, ok := cloudDSHToolDefinitionsSummary(state.definitions)
	if !ok {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	checkpointThrough := state.through
	checkpointDigest := state.checkpointDigest
	if call.Task.RecoveryDSHCallID != "" {
		checkpointThrough = state.restoreThrough
		checkpointDigest = state.restoreDigest
	}
	return cloudDSHReadOnlyRecoveryCheckpoint{ThroughSequence: checkpointThrough,
		Digest: append([]byte(nil), checkpointDigest...), ToolsetDigest: append([]byte(nil), digest[:]...)}, true
}

func cloudDSHReadOnlyRecoveryCheckpointFailureReason(value task.Task, state *cloudDSHSessionState,
	call app.ModelCall, hasChildSessions bool) string {
	if state == nil || call.Source != app.ModelCallTask || call.Task == nil || hasChildSessions || state.background ||
		state.parentSessionID != "" || state.identity.TaskID != value.ID || state.identity.OwnerID != value.OwnerID ||
		state.identity.AgentID != value.AgentInstanceID || state.identity.Generation != value.AgentGeneration ||
		state.identity.SessionID != value.AgentSessionID || state.identity.AuthorizationVersion != value.Authorization.Version ||
		state.identity.SessionID != call.Task.AgentSessionID || call.Task.TaskID != value.ID ||
		call.Task.OwnerID != value.OwnerID || call.Task.InitiatorID != value.InitiatorID ||
		call.Task.LeaseOwner != value.Lease.Owner || call.Task.LeaseVersion != value.Lease.Version ||
		call.Task.AgentInstanceID != value.AgentInstanceID || call.Task.AgentGeneration != value.AgentGeneration ||
		call.Task.AgentRootSessionID != value.AgentSessionID || call.Task.AuthorizationVersion != value.Authorization.Version ||
		call.Task.EngineVersion != value.AgentEngineVersion || call.Task.ProfileVersion != value.AgentProfileVersion ||
		value.CurrentRevision == nil || value.DesiredRevision == nil || *value.CurrentRevision != *value.DesiredRevision {
		return "identity_or_task_fence_changed"
	}
	if state.through < 0 || len(state.checkpointDigest) != 32 {
		return "current_checkpoint_missing"
	}
	if !state.toolSelectionVerified || !state.historyReadyForModelRecovery() {
		return "checkpoint_history_not_reconciled"
	}
	if state.canDelegate || len(state.agentRequests) != 0 {
		return "delegation_or_agent_request_active"
	}
	if !cloudDSHRecoveryActivitiesSafeForCheckpoint(state, call) {
		return "active_activity_not_reconciled"
	}
	if !cloudDSHReadOnlyRecoveryToolset(state.definitions) {
		return "toolset_not_read_only"
	}
	if state.resume {
		if state.restoreThrough < 0 || len(state.restoreDigest) != 32 || state.readThrough != state.restoreThrough {
			return "pinned_checkpoint_not_fully_read"
		}
	} else if state.restoreThrough != -1 || state.readThrough != -1 {
		return "fresh_checkpoint_cursor_changed"
	}
	if call.Task.RecoveryDSHCallID == "" {
		if state.through != call.Task.ThroughSequence {
			return "failed_call_checkpoint_sequence_changed"
		}
	} else if call.Task.ThroughSequence != state.restoreThrough || state.through < state.restoreThrough {
		return "recovery_checkpoint_sequence_changed"
	}
	if _, ok := cloudDSHToolDefinitionsSummary(state.definitions); !ok {
		return "toolset_digest_unavailable"
	}
	return ""
}

// The runtime reports activity observations asynchronously. During a model
// recovery both the failed call and its exact recovery call can appear active;
// accept only those identities and completed, reconciled read-only tools.
func cloudDSHRecoveryActivitiesSafeForCheckpoint(state *cloudDSHSessionState, call app.ModelCall) bool {
	if state == nil || len(state.activeActivities) == 0 {
		return true
	}
	modelCallIDs := map[string]struct{}{call.CallID: {}}
	if call.Task != nil && call.Task.RecoveryDSHCallID != "" {
		failedCallID, err := app.DSHModelCallID(call.Task.TaskID, call.Task.AgentSessionID, call.Task.ThroughSequence)
		if err != nil {
			return false
		}
		modelCallIDs[failedCallID] = struct{}{}
	}
	seenModelActivityIDs := make(map[string]struct{}, len(modelCallIDs))
	for activityID, phase := range state.activeActivities {
		switch phase {
		case "model":
			if _, matches := modelCallIDs[activityID]; !matches || activityID == "" {
				return false
			}
			if _, duplicate := seenModelActivityIDs[activityID]; duplicate {
				return false
			}
			seenModelActivityIDs[activityID] = struct{}{}
		case "tool":
			if state.history == nil {
				return false
			}
			toolCall := state.history.calls[activityID]
			if toolCall == nil || !toolCall.callSeen || !toolCall.resultSeen || !toolCall.reconciled ||
				toolCall.status != app.AgentToolSucceeded || !cloudDSHReadOnlyRecoveryToolName(toolCall.name) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func cloudDSHReadOnlyRecoveryToolName(name string) bool {
	switch name {
	case "morde_web_search", "morde_web_read", "morde_read_task_attachment", cloudDSHResultEvidenceToolName:
		return true
	default:
		return false
	}
}

func (state *cloudDSHSessionState) historyReadyForModelRecovery() bool {
	return state != nil && state.history != nil && state.history.nextSeq == state.through+1 && state.history.allCallsReconciled()
}

func cloudDSHReadOnlyRecoveryToolset(definitions []app.AgentToolDefinition) bool {
	if !validCloudDSHToolDefinitions(definitions) {
		return false
	}
	for _, definition := range definitions {
		if definition.Name == "morde_read_task_attachment" {
			if definition.RequiredScope != identity.ScopeTaskRead || !definition.Strict {
				return false
			}
			continue
		}
		if !cloudDSHReadOnlyRecoveryToolName(definition.Name) {
			return false
		}
	}
	return true
}
