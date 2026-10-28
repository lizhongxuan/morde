package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const cloudDSHJobOutputMaxBytes = 32 << 10

type cloudDSHJobCancelPending struct {
	session   *cloudDSHSessionState
	fence     app.AgentInstanceFence
	frame     cloudDSHFrame
	arguments []byte
	identity  cloudDSHIdentity
	jobID     string
}

type cloudDSHJobView struct {
	JobID        string `json:"job_id"`
	Status       string `json:"status"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	OutputCursor int64  `json:"output_cursor"`
}

func cloudDSHChildToolDefinitions(parent []app.AgentToolDefinition, names []string) ([]app.AgentToolDefinition, error) {
	if !validCloudDSHStoredToolNames(names) {
		return nil, errors.New("stored child tool names are invalid")
	}
	byName := make(map[string]app.AgentToolDefinition, len(parent))
	for _, definition := range parent {
		if definition.Name == cloudDSHDelegateToolName || cloudDSHJobTool(definition.Name) || definition.Name == "ask_user" {
			continue
		}
		byName[definition.Name] = definition
	}
	definitions := make([]app.AgentToolDefinition, 0, len(names))
	for _, name := range names {
		definition, ok := byName[name]
		if !ok {
			return nil, errors.New("stored child tool exceeds the parent allowlist")
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func validCloudDSHStoredToolNames(names []string) bool {
	if len(names) > 64 {
		return false
	}
	previous := ""
	for _, name := range names {
		if !cloudDSHID.MatchString(name) || name <= previous {
			return false
		}
		previous = name
	}
	return true
}

func (e *CloudDSHExecutor) cloudDSHJobCancelRequested(ctx context.Context, fence app.AgentInstanceFence, state *cloudDSHSessionState) (bool, error) {
	if state == nil || !state.background {
		return false, nil
	}
	child, err := e.options.Children.Get(ctx, fence, fence.AgentSessionID, state.identity.SessionID)
	if err != nil || !child.Background || child.ParentSessionID != fence.AgentSessionID || child.SessionID != state.identity.SessionID {
		return false, errors.New("background job authorization changed")
	}
	return child.CancelRequested, nil
}

func cloudDSHJobStatus(child app.AgentChildSession) string {
	switch child.Step.Status {
	case task.StepRunning:
		if child.CancelRequested {
			return "cancelling"
		}
		return "running"
	case task.StepSucceeded:
		return "succeeded"
	case task.StepFailed:
		if child.CancelRequested {
			return "cancelled"
		}
		return "failed"
	case task.StepUnknown:
		return "unknown"
	case task.StepQueued:
		return "queued"
	case task.StepWaitingUser:
		return "waiting_user"
	default:
		return "unknown"
	}
}

func cloudDSHJobResult(child app.AgentChildSession) string {
	result, _ := json.Marshal(cloudDSHJobView{JobID: child.SessionID, Status: cloudDSHJobStatus(child),
		Title: child.Step.Title, Summary: child.Step.Summary, OutputCursor: child.CheckpointThrough})
	return string(result)
}

func (e *CloudDSHExecutor) executeCloudDSHJobRead(ctx context.Context, _ task.Task, fence app.AgentInstanceFence,
	frame cloudDSHFrame, states map[string]*cloudDSHSessionState) (app.AgentToolResult, error) {
	if frame.Name == cloudDSHJobListToolName {
		var arguments struct{}
		if err := decodeCloudDSHJobArguments(frame.Arguments, &arguments); err != nil {
			return app.AgentToolResult{}, err
		}
		children, err := e.options.Children.List(ctx, fence, fence.AgentSessionID)
		if err != nil {
			return app.AgentToolResult{}, err
		}
		jobs := make([]cloudDSHJobView, 0, len(children))
		for _, child := range children {
			if child.Background {
				jobs = append(jobs, cloudDSHJobView{JobID: child.SessionID, Status: cloudDSHJobStatus(child),
					Title: child.Step.Title, Summary: child.Step.Summary, OutputCursor: child.CheckpointThrough})
			}
		}
		output, err := json.Marshal(jobs)
		if err != nil || len(output) > cloudDSHMaxText {
			return app.AgentToolResult{}, errors.New("background job list exceeds the result limit")
		}
		return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: string(output)}, nil
	}
	if frame.Name == cloudDSHJobGetToolName {
		var arguments struct {
			JobID string `json:"job_id"`
		}
		if err := decodeCloudDSHJobArguments(frame.Arguments, &arguments); err != nil || !cloudDSHID.MatchString(arguments.JobID) {
			return app.AgentToolResult{}, errors.New("background job identity is invalid")
		}
		child, err := e.options.Children.Get(ctx, fence, fence.AgentSessionID, arguments.JobID)
		if err != nil || !child.Background {
			return app.AgentToolResult{}, errors.New("background job is unavailable")
		}
		return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: cloudDSHJobResult(child)}, nil
	}
	if frame.Name != cloudDSHJobOutputToolName {
		return app.AgentToolResult{}, errors.New("unsupported background job read tool")
	}
	var arguments struct {
		JobID    string `json:"job_id"`
		AfterSeq int64  `json:"after_seq"`
	}
	if err := decodeCloudDSHJobArguments(frame.Arguments, &arguments); err != nil || !cloudDSHID.MatchString(arguments.JobID) || arguments.AfterSeq < -1 {
		return app.AgentToolResult{}, errors.New("background job output cursor is invalid")
	}
	child, err := e.options.Children.Get(ctx, fence, fence.AgentSessionID, arguments.JobID)
	if err != nil || !child.Background || arguments.AfterSeq > child.CheckpointThrough {
		return app.AgentToolResult{}, errors.New("background job is unavailable")
	}
	state := states[child.SessionID]
	output, cursor, err := e.cloudDSHJobOutput(ctx, fence, child, state, arguments.AfterSeq)
	if err != nil {
		return app.AgentToolResult{}, err
	}
	result, err := json.Marshal(struct {
		JobID      string `json:"job_id"`
		Status     string `json:"status"`
		NextCursor int64  `json:"next_cursor"`
		Output     string `json:"output"`
	}{JobID: child.SessionID, Status: cloudDSHJobStatus(child), NextCursor: cursor, Output: output})
	if err != nil || len(result) > cloudDSHMaxText {
		return app.AgentToolResult{}, errors.New("background job output exceeds the result limit")
	}
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: string(result)}, nil
}

func (e *CloudDSHExecutor) cloudDSHJobOutput(ctx context.Context, fence app.AgentInstanceFence, child app.AgentChildSession,
	state *cloudDSHSessionState, after int64) (string, int64, error) {
	if state != nil && state.history != nil && state.through == child.CheckpointThrough {
		return state.history.outputAfter(after, cloudDSHJobOutputMaxBytes)
	}
	if after >= child.CheckpointThrough {
		return "", after, nil
	}
	history := newCloudDSHCheckpointHistory(child.SessionID)
	history.beginRestore()
	from := int64(0)
	var totalBytes int
	for from <= child.CheckpointThrough {
		page, err := e.options.Checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{Fence: fence,
			SessionID: child.SessionID, From: from, Through: child.CheckpointThrough, ThroughDigest: append([]byte(nil), child.CheckpointDigest...)})
		if err != nil || page.From != from || page.Through < from || page.Through > child.CheckpointThrough ||
			len(page.Payload) == 0 || len(page.Payload) > 1<<20 || !cloudDSHDigest(page.Payload, hex.EncodeToString(page.Digest)) {
			return "", after, errors.New("background job checkpoint is unavailable")
		}
		totalBytes += len(page.Payload)
		if totalBytes > 8<<20 {
			return "", after, errors.New("background job history exceeds the output limit")
		}
		if err := history.append(page.Payload, page.From, page.Through); err != nil {
			return "", after, err
		}
		if page.Through >= after+1 {
			break
		}
		from = page.Through + 1
	}
	return history.outputAfter(after, cloudDSHJobOutputMaxBytes)
}

func (e *CloudDSHExecutor) cancelCloudDSHJob(ctx context.Context, fence app.AgentInstanceFence, actorSessionID string,
	raw json.RawMessage) (app.AgentToolResult, bool, string, error) {
	var arguments struct {
		JobID string `json:"job_id"`
	}
	if err := decodeCloudDSHJobArguments(raw, &arguments); err != nil || !cloudDSHID.MatchString(arguments.JobID) {
		return app.AgentToolResult{}, false, "", errors.New("background job identity is invalid")
	}
	child, err := e.options.Children.Get(ctx, fence, actorSessionID, arguments.JobID)
	if err != nil || !child.Background {
		return app.AgentToolResult{}, false, "", errors.New("background job is unavailable")
	}
	if child.Step.Status != task.StepRunning {
		return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: cloudDSHJobResult(child)}, false, child.SessionID, nil
	}
	updated, _, err := e.options.Children.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: fence,
		ActorSessionID: actorSessionID, SessionID: child.SessionID})
	if err != nil || updated.Step.Status != task.StepRunning || !updated.Background || !updated.CancelRequested {
		return app.AgentToolResult{}, false, "", errors.New("background job cancel request was not durably saved")
	}
	return app.AgentToolResult{}, true, child.SessionID, nil
}

func (e *CloudDSHExecutor) writeCloudDSHJobCancel(ctx context.Context, identity cloudDSHIdentity, id, jobID string, input io.WriteCloser) error {
	if !cloudDSHID.MatchString(id) || !cloudDSHID.MatchString(jobID) {
		return errors.New("background job cancellation identity is invalid")
	}
	return writeCloudDSHFrame(ctx, input, map[string]any{"type": "job_cancel", "id": id,
		"task_id": identity.TaskID, "owner_id": identity.OwnerID, "agent_id": identity.AgentID,
		"generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
		"session_id": identity.SessionID, "job_id": jobID})
}

func decodeCloudDSHJobArguments(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || len(raw) > 4096 || !uniqueCloudDSHJSON(raw) || decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("background job tool arguments are invalid")
	}
	return nil
}
