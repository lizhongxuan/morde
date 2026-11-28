package worker

import (
	"errors"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

var errCloudDSHWebResearchToolUnavailable = errors.New("required web research tool is unavailable")

const (
	cloudDSHWebResearchSearchToolName = "morde_web_search"
	cloudDSHWebResearchReadToolName   = "morde_web_read"
	cloudDSHDefaultOutputTokenCap     = int64(2048)
	cloudDSHWebResearchOutputTokenCap = int64(1024)
)

// cloudDSHTaskLegacyModelOutputTokens keeps the existing DSH cap for ordinary
// Tasks while giving the initial public-research profile enough room for a
// concise evidence-based answer without reserving an unnecessary 2048 tokens.
func cloudDSHTaskLegacyModelOutputTokens(value task.Task) int64 {
	cap := value.Budget.ModelTokens
	if cap > cloudDSHDefaultOutputTokenCap {
		cap = cloudDSHDefaultOutputTokenCap
	}
	if cap > cloudDSHWebResearchOutputTokenCap && cloudDSHWebResearchProfileActive(value) {
		cap = cloudDSHWebResearchOutputTokenCap
	}
	return cap
}

// cloudDSHTaskProfileToolDefinitions applies the Task's least-privilege tool
// profile only to its first revision. The returned child policy must be used
// to decide whether delegation and background child Agents are exposed.
func cloudDSHTaskProfileToolDefinitions(value task.Task, definitions []app.AgentToolDefinition) ([]app.AgentToolDefinition, bool, error) {
	unchanged := append([]app.AgentToolDefinition(nil), definitions...)
	context := value.ConversationContext
	if context == nil {
		return unchanged, true, nil
	}
	if !context.AgentToolProfile.Valid() {
		return nil, false, errors.New("Task Agent tool profile is invalid")
	}
	if !initialCloudDSHTaskRevision(value) || context.AgentToolProfile == "" {
		return unchanged, true, nil
	}
	if context.AgentToolProfile != task.AgentToolProfileWebResearch {
		return unchanged, true, nil
	}

	filtered := make([]app.AgentToolDefinition, 0, 3)
	hasSearch, hasRead := false, false
	for _, definition := range definitions {
		switch definition.Name {
		case cloudDSHWebResearchSearchToolName:
			filtered = append(filtered, definition)
			hasSearch = true
		case cloudDSHWebResearchReadToolName:
			filtered = append(filtered, definition)
			hasRead = true
		default:
			if app.IsCloudDSHClarificationToolName(definition.Name) {
				filtered = append(filtered, definition)
			}
		}
	}
	if !hasSearch || !hasRead {
		return nil, false, errCloudDSHWebResearchToolUnavailable
	}
	return filtered, false, nil
}

func initialCloudDSHTaskRevision(value task.Task) bool {
	return value.CurrentRevision == nil || *value.CurrentRevision == 0
}

func cloudDSHTaskProfileSkipsInitialPersonalContext(value task.Task) bool {
	return value.ConversationContext != nil && initialCloudDSHTaskRevision(value) &&
		value.ConversationContext.Scope == "personal" &&
		value.ConversationContext.AgentToolProfile == task.AgentToolProfileWebResearch
}
