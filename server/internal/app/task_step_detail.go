package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain/task"
)

func taskStepDetailAAD(ownerID, taskID, stepID string, revision int64) []byte {
	value, _ := json.Marshal(struct {
		OwnerID  string `json:"owner_id"`
		TaskID   string `json:"task_id"`
		StepID   string `json:"step_id"`
		Revision int64  `json:"task_revision"`
	}{ownerID, taskID, stepID, revision})
	return value
}

func validTaskStepDetailDocument(value TaskStepDetailDocument) bool {
	for _, field := range []string{value.Object, value.Input, value.Result} {
		if !utf8.ValidString(field) || len(field) > 8192 || strings.ContainsRune(field, '\x00') {
			return false
		}
	}
	return true
}

func sealTaskStepDetail(sealer *Sealer, ownerID, taskID, stepID string, revision int64, value TaskStepDetailDocument) ([]byte, []byte, int32, error) {
	if sealer == nil || ownerID == "" || taskID == "" || stepID == "" || revision < 0 || !validTaskStepDetailDocument(value) {
		return nil, nil, 0, fmt.Errorf("task step detail identity or document is invalid")
	}
	plain, err := json.Marshal(value)
	if err != nil || len(plain) == 0 || len(plain) > 64<<10 {
		return nil, nil, 0, fmt.Errorf("task step detail exceeds its storage limit")
	}
	ciphertext, err := sealer.SealWithAAD(plain, taskStepDetailAAD(ownerID, taskID, stepID, revision))
	if err != nil {
		return nil, nil, 0, err
	}
	digest := sha256.Sum256(plain)
	return ciphertext, digest[:], int32(len(plain)), nil
}

type cloudDSHToolActivityKey struct{}

type cloudDSHToolActivity struct {
	call          ModelToolCall
	current       task.Task
	digest        []byte
	sealer        *Sealer
	displayObject string
}

func withCloudDSHToolActivity(ctx context.Context, current task.Task, call ModelToolCall, sealer *Sealer) context.Context {
	if call.TaskFence == nil || call.TaskFence.TaskID != current.ID || call.TaskFence.OwnerID != current.OwnerID ||
		call.TaskFence.AgentSessionID == "" || current.CurrentRevision == nil || *current.CurrentRevision < 0 ||
		call.TaskFence.AgentInstanceID != current.AgentInstanceID || call.TaskFence.AgentGeneration != current.AgentGeneration ||
		call.TaskFence.AuthorizationVersion != current.Authorization.Version ||
		call.TaskFence.LeaseOwner != current.Lease.Owner || call.TaskFence.LeaseVersion != current.Lease.Version {
		return ctx
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return ctx
	}
	digest, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, cloudDSHToolActivity{call: call, current: current,
		digest: append([]byte(nil), digest[:]...), sealer: sealer})
}

func cloudDSHToolActivityFromContext(ctx context.Context) (cloudDSHToolActivity, bool) {
	value, ok := ctx.Value(cloudDSHToolActivityKey{}).(cloudDSHToolActivity)
	return value, ok
}

func taskToolCallProgress(activity cloudDSHToolActivity, step task.Step) (*TaskToolCallProgress, error) {
	fence := activity.call.TaskFence
	if fence == nil || len(activity.digest) != sha256.Size || activity.current.CurrentRevision == nil || step.ID == "" {
		return nil, nil
	}
	object := taskToolObject(activity.call.Name, activity.call.Arguments)
	if activity.displayObject != "" {
		object = activity.displayObject
	}
	detail := TaskStepDetailDocument{Object: object,
		Input: taskToolInputSummary(activity.call.Name, activity.call.Arguments), Result: taskToolResultSummary(step)}
	truncated := false
	detail.Object, truncated = truncateTaskDetail(detail.Object, truncated)
	detail.Input, truncated = truncateTaskDetail(detail.Input, truncated)
	detail.Result, truncated = truncateTaskDetail(detail.Result, truncated)
	phase := string(step.Status)
	if step.Phase == "prepare" {
		phase = step.Phase
	}
	progress := &TaskToolCallProgress{AgentSessionID: fence.AgentSessionID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion,
		TaskRevision: *activity.current.CurrentRevision, CallKey: activity.call.CallID, ToolName: activity.call.Name,
		RequestDigest: append([]byte(nil), activity.digest...), Phase: phase, DetailState: "not_recorded",
		Truncated: truncated}
	if step.Status == task.StepFailed && phase == "prepare" {
		progress.FailureReason, progress.Retryable = "not_dispatched", true
	}
	if activity.sealer != nil {
		ciphertext, digest, size, err := sealTaskStepDetail(activity.sealer, activity.current.OwnerID, activity.current.ID, step.ID,
			progress.TaskRevision, detail)
		if err != nil {
			return nil, err
		}
		progress.DetailState, progress.DetailCiphertext, progress.DetailSHA256, progress.DetailSizeBytes = "available", ciphertext, digest, size
	}
	return progress, nil
}

// SupportsSafeTaskStepRetryTool reports whether the existing Host operation
// runner can bind a tool name to its task operation journal.
func SupportsSafeTaskStepRetryTool(name string) bool {
	_, ok := runtimeToolMappingByName(name)
	return ok
}

// MatchesTaskStepRetryCall binds the one-time retry identity to the exact
// arguments restored from the pinned DSH checkpoint.
func MatchesTaskStepRetryCall(intent task.StepRetryIntent, call ModelToolCall) bool {
	fence := call.TaskFence
	if fence == nil || call.CallID != intent.RetryCallID || call.Name != intent.ToolName ||
		fence.TaskID != intent.TaskID || fence.OwnerID != intent.OwnerID || fence.AgentSessionID != intent.AgentSessionID ||
		fence.AgentRootSessionID != intent.AgentSessionID || fence.AuthorizationVersion != intent.AuthorizationVersion ||
		fence.ThroughSequence != intent.CheckpointThroughSequence {
		return false
	}
	return MatchesTaskStepRetryArguments(intent, call.Name, call.Arguments)
}

func MatchesTaskStepRetryArguments(intent task.StepRetryIntent, name, rawArguments string) bool {
	if name != intent.ToolName || !SupportsSafeTaskStepRetryTool(name) {
		return false
	}
	arguments, err := canonicalStepRetryArguments(rawArguments)
	if err != nil {
		return false
	}
	digest, err := agentOperationHash(name, arguments)
	return err == nil && bytes.Equal(digest[:], intent.RequestDigest)
}

func canonicalStepRetryArguments(raw string) (json.RawMessage, error) {
	validated, err := validateAgentToolArguments(raw)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(validated))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

// ConsumeTaskStepRetryIntent forwards the one-time admission check to the
// durable Store. The worker uses this boundary before entering the existing
// tool broker.
func (s *AgentCheckpointService) ConsumeTaskStepRetryIntent(ctx context.Context, intent task.StepRetryIntent,
	fence AgentInstanceFence) error {
	consumer, ok := s.store.(TaskStepRetryIntentConsumer)
	if !ok {
		return fmt.Errorf("task step retry admission is unavailable")
	}
	return consumer.ConsumeTaskStepRetryIntent(ctx, intent, fence)
}

func taskToolResultSummary(step task.Step) string {
	switch step.Status {
	case task.StepQueued:
		return "The tool call is queued."
	case task.StepRunning:
		return "The tool call is running."
	case task.StepWaitingUser:
		return "The task is waiting for your response before continuing."
	case task.StepUnknown:
		return "The tool result could not be confirmed and needs reconciliation."
	case task.StepSucceeded, task.StepFailed:
		return step.Summary
	default:
		return "The tool result is unavailable."
	}
}

func taskToolObject(name, raw string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return name
	}
	if strings.HasPrefix(name, "morde_workspace_") {
		if path := decodeSafeString(fields["path"], 512); path != "" && validateCloudDSHWorkspacePath(path) == nil && !cloudDSHWorkspacePathIsSensitive(path) {
			return path
		}
	}
	if name == cloudDSHWebSearchToolName || name == cloudDSHWebReadToolName {
		rawURL := decodeSafeString(fields["url"], 2048)
		if rawURL == "" {
			rawURL = decodeSafeString(fields["url_to_read"], 2048)
		}
		if rawURL == "" {
			return name
		}
		if parsed, err := url.Parse(rawURL); err == nil && parsed.IsAbs() && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
			parsed.User = nil
			parsed.RawQuery, parsed.Fragment = "", ""
			return parsed.String()
		}
	}
	return name
}

func taskToolInputSummary(name, rawArguments string) string {
	switch {
	case strings.HasPrefix(name, "morde_workspace_"):
		return "Workspace request; file contents and write payloads are omitted."
	case name == cloudDSHWebSearchToolName:
		arguments, _, err := builtinSearchArguments(name, rawArguments)
		if err == nil {
			var input struct {
				Query     json.RawMessage `json:"query"`
				TimeRange json.RawMessage `json:"time_range"`
			}
			if json.Unmarshal(arguments, &input) == nil {
				if query := decodeSafeString(input.Query, 512); query != "" {
					if timeRange := decodeSafeString(input.TimeRange, 16); timeRange != "" {
						return "Search query: " + query + " (time range: " + timeRange + ")."
					}
					return "Search query: " + query
				}
			}
		}
		return "Web search request; query was omitted because it could not be safely displayed."
	case name == cloudDSHWebReadToolName:
		return "Web request; raw query parameters and page contents are omitted."
	case strings.HasPrefix(name, "morde_mcp_"):
		return "Connector request; raw parameters and response are omitted."
	default:
		return "Tool request; raw parameters and output are omitted for safety."
	}
}

func decodeSafeString(raw json.RawMessage, maxBytes int) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) || len(value) > maxBytes ||
		strings.ContainsRune(value, '\x00') || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return strings.TrimSpace(value)
}

func truncateTaskDetail(value string, alreadyTruncated bool) (string, bool) {
	if !utf8.ValidString(value) {
		value, alreadyTruncated = strings.ToValidUTF8(value, "�"), true
	}
	if len(value) <= 4096 {
		return value, alreadyTruncated
	}
	for len(value) > 4093 || !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…", true
}
