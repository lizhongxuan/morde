package worker

import (
	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHReadOnlyRecoveryCheckpoint struct {
	ThroughSequence int64
	Digest          []byte
	ToolsetDigest   []byte
}

func cloudDSHModelRecoveryStartAllowed(state task.ModelRecoveryState, callID, failedCallID, recoveryCallID string,
	attemptStarted, attemptSucceeded bool) (beginAttempt, replayOnly, allowed bool) {
	switch state {
	case task.ModelRecoveryScheduled:
		if !attemptStarted {
			return callID == failedCallID, false, callID == failedCallID
		}
		if !attemptSucceeded || callID == failedCallID {
			return false, false, false
		}
		return false, false, true
	case task.ModelRecoveryAttempting:
		if !attemptStarted {
			return false, callID == failedCallID, callID == failedCallID
		}
		if !attemptSucceeded || callID == failedCallID {
			return false, false, false
		}
		return false, false, true
	default:
		return false, false, true
	}
}

// cloudDSHReadOnlyRecoveryCheckpointFor accepts only a root session whose
// durable checkpoint and complete model-visible tool collection are known.
// The allowlist is deliberately closed to public web search and page reads:
// other read-only tools can expose private or project data and are outside
// this recovery path.
func cloudDSHReadOnlyRecoveryCheckpointFor(value task.Task, state *cloudDSHSessionState,
	call app.ModelCall, hasChildSessions bool) (cloudDSHReadOnlyRecoveryCheckpoint, bool) {
	if state == nil || call.Source != app.ModelCallTask || call.Task == nil || hasChildSessions || state.background ||
		state.parentSessionID != "" || state.identity.TaskID != value.ID || state.identity.OwnerID != value.OwnerID ||
		state.identity.AgentID != value.AgentInstanceID || state.identity.Generation != value.AgentGeneration ||
		state.identity.SessionID != value.AgentSessionID ||
		state.identity.AuthorizationVersion != value.Authorization.Version ||
		state.identity.SessionID != call.Task.AgentSessionID || state.through < 0 || state.through != call.Task.ThroughSequence ||
		value.CurrentRevision == nil || value.DesiredRevision == nil || *value.CurrentRevision != *value.DesiredRevision ||
		call.Task.TaskID != value.ID || call.Task.OwnerID != value.OwnerID || call.Task.InitiatorID != value.InitiatorID ||
		call.Task.LeaseOwner != value.Lease.Owner || call.Task.LeaseVersion != value.Lease.Version ||
		call.Task.AgentInstanceID != value.AgentInstanceID || call.Task.AgentGeneration != value.AgentGeneration ||
		call.Task.AgentRootSessionID != value.AgentSessionID || call.Task.AuthorizationVersion != value.Authorization.Version ||
		call.Task.EngineVersion != value.AgentEngineVersion || call.Task.ProfileVersion != value.AgentProfileVersion ||
		len(state.checkpointDigest) != 32 || !state.resume || state.restoreThrough < 0 ||
		len(state.restoreDigest) != 32 || !state.toolSelectionVerified || !state.historyReadyForModelRecovery() ||
		state.readThrough != state.restoreThrough ||
		state.canDelegate || len(state.agentRequests) != 0 || len(state.activeActivities) != 0 ||
		!cloudDSHReadOnlyRecoveryToolset(state.definitions) {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	digest, ok := cloudDSHToolDefinitionsSummary(state.definitions)
	if !ok {
		return cloudDSHReadOnlyRecoveryCheckpoint{}, false
	}
	return cloudDSHReadOnlyRecoveryCheckpoint{ThroughSequence: state.through,
		Digest: append([]byte(nil), state.checkpointDigest...), ToolsetDigest: append([]byte(nil), digest[:]...)}, true
}

func (state *cloudDSHSessionState) historyReadyForModelRecovery() bool {
	return state != nil && state.history != nil && state.history.nextSeq == state.through+1 && state.history.allCallsReconciled()
}

func cloudDSHReadOnlyRecoveryToolset(definitions []app.AgentToolDefinition) bool {
	if !validCloudDSHToolDefinitions(definitions) {
		return false
	}
	for _, definition := range definitions {
		switch definition.Name {
		case "morde_web_search", "morde_web_read":
		default:
			return false
		}
	}
	return true
}
