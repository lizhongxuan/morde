package app

import "strings"

// ValidConversationRouteStopDecision requires a matching model decision before
// task cancellation, including when a persisted route result is replayed.
func ValidConversationRouteStopDecision(value ConversationRouteContext, decision ConversationRouteDecision) bool {
	if decision.Action != ConversationRouteStop && decision.Action != ConversationRouteStopAll ||
		decision.InvalidOutput || strings.TrimSpace(decision.ModelOutput) == "" ||
		value.ContentTruncated || value.CurrentMessageHasAttachments || len(value.ContentSources) != 0 ||
		value.ResultReview != nil || value.PreviousMissing || value.ReferenceUnavailable ||
		(value.CandidateSetIncomplete || value.TaskCandidateSetIncomplete || value.RequestCandidateSetIncomplete) &&
			decision.Action != ConversationRouteStopAll && value.ReferenceTaskID == "" ||
		decision.Acknowledge || decision.ExplicitNew || decision.Question != "" || decision.Reply != "" ||
		decision.ToolName != "" || decision.ToolArguments != "" || len(decision.DependencyTaskIDs) != 0 || len(decision.AggregateMessageIDs) != 0 {
		return false
	}
	parsed := parseConversationRouteDecision(decision.ModelOutput, value.Content, value.Candidates, value.RequestCandidates,
		value.AggregationCandidates, value.ReferenceUnavailable, false)
	return !parsed.InvalidOutput && parsed.Action == decision.Action && parsed.TaskID == decision.TaskID &&
		parsed.TargetMessageID == decision.TargetMessageID && parsed.ExpectedRevision == decision.ExpectedRevision
}
