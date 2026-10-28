package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHSessionState struct {
	identity         cloudDSHIdentity
	parentSessionID  string
	spawnKey         string
	definitions      []app.AgentToolDefinition
	canDelegate      bool
	background       bool
	resume           bool
	through          int64
	restoreThrough   int64
	lastModel        int64
	readThrough      int64
	checkpointDigest []byte
	restoreDigest    []byte
	checkpointIDs    map[string]struct{}
	pageReadIDs      map[string]struct{}
	history          *cloudDSHCheckpointHistory
	images           *cloudDSHImageRegistry
	settled          bool
}

func newCloudDSHSessionState(fence app.AgentInstanceFence, identity cloudDSHIdentity, parentSessionID, spawnKey string,
	definitions []app.AgentToolDefinition, canDelegate, resume bool, through int64, digest []byte) *cloudDSHSessionState {
	history := newCloudDSHCheckpointHistory(identity.SessionID)
	if resume {
		history.beginRestore()
	}
	return &cloudDSHSessionState{identity: identity, parentSessionID: parentSessionID, spawnKey: spawnKey,
		definitions: definitions, canDelegate: canDelegate, resume: resume, through: through, restoreThrough: through, lastModel: -1, readThrough: -1,
		checkpointDigest: append([]byte(nil), digest...), restoreDigest: append([]byte(nil), digest...),
		checkpointIDs: make(map[string]struct{}), pageReadIDs: make(map[string]struct{}),
		history: history, images: newCloudDSHImageRegistry(cloudDSHModelFence(fence, identity.SessionID, through))}
}

func cloudDSHModelFence(fence app.AgentInstanceFence, sessionID string, through int64) app.ModelTaskFence {
	return app.ModelTaskFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, LeaseOwner: fence.LeaseOwner,
		LeaseVersion: fence.LeaseVersion, AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration,
		AgentSessionID: sessionID, AgentRootSessionID: fence.AgentSessionID, AuthorizationVersion: fence.AuthorizationVersion,
		ThroughSequence: through, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
}

func cloudDSHModelFencePointer(fence app.AgentInstanceFence, sessionID string, through int64) *app.ModelTaskFence {
	modelFence := cloudDSHModelFence(fence, sessionID, through)
	return &modelFence
}

func checkpointFenceForSession(fence app.AgentInstanceFence, sessionID string) app.AgentCheckpointFence {
	return app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AgentSessionID: sessionID, AgentRootSessionID: fence.AgentSessionID,
		AuthorizationVersion: fence.AuthorizationVersion, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
}

func cloudDSHNamespacedToolCallID(taskID, sessionID, callID string) string {
	input := "morde-dsh-tool-v1\n" + taskID + "\n" + sessionID + "\n" + callID
	digest := sha256.Sum256([]byte(input))
	return "dsh_" + hex.EncodeToString(digest[:])
}

func sameWorkerToolNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (e *CloudDSHExecutor) runCloudDSH(ctx context.Context, value task.Task, fence app.AgentInstanceFence,
	input io.WriteCloser, output io.ReadCloser, resume bool, completion *app.CloudDSHTaskCompletion) error {
	rootIdentity := cloudDSHIdentity{TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentID: fence.AgentInstanceID,
		SessionID: fence.AgentSessionID, Generation: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion}
	baseDefinitions := []app.AgentToolDefinition{}
	if e.options.Tools != nil {
		definitions, err := e.options.Tools.Definitions(ctx, value)
		if err != nil || !validCloudDSHToolDefinitions(definitions) {
			return errors.New("dsh tool definitions are unavailable")
		}
		baseDefinitions = definitions
	}
	rootDefinitions := append([]app.AgentToolDefinition(nil), baseDefinitions...)
	for _, definition := range baseDefinitions {
		if definition.Name == cloudDSHDelegateToolName || cloudDSHJobTool(definition.Name) {
			return errors.New("dsh tool definitions use a reserved name")
		}
	}
	canDelegate := false
	if e.options.Children != nil {
		rootDefinitions = append(rootDefinitions, cloudDSHJobTools()...)
		for _, definition := range baseDefinitions {
			if definition.Name != cloudDSHDelegateToolName && definition.Name != "ask_user" {
				canDelegate = true
				break
			}
		}
		if canDelegate && len(rootDefinitions) < 64 {
			rootDefinitions = append(rootDefinitions, cloudDSHDelegateTool(baseDefinitions))
		}
	}
	if !validCloudDSHToolDefinitions(rootDefinitions) {
		return errors.New("dsh tool definitions are unavailable")
	}
	rootThrough := int64(-1)
	var rootDigest []byte
	if resume {
		rootThrough, rootDigest = value.CheckpointThroughSequence, value.CheckpointDigest
	}
	states := map[string]*cloudDSHSessionState{rootIdentity.SessionID: newCloudDSHSessionState(fence, rootIdentity, "", "",
		rootDefinitions, canDelegate, resume, rootThrough, rootDigest)}
	backgroundJobs := make([]map[string]any, 0)
	if e.options.Children != nil {
		children, err := e.options.Children.List(ctx, fence, rootIdentity.SessionID)
		if err != nil {
			return errors.New("dsh background jobs are unavailable")
		}
		for _, child := range children {
			if !child.Background || child.Step.Status != task.StepRunning {
				continue
			}
			if child.ParentSessionID != rootIdentity.SessionID || !cloudDSHID.MatchString(child.SessionID) || !cloudDSHID.MatchString(child.SpawnKey) {
				return errors.New("dsh background job cannot be safely resumed")
			}
			if child.CancelRequested {
				// StartAgentInstance only admits a new generation after the previous
				// process tree has a verified stop receipt. The old child can no longer
				// run, but its last checkpoint cannot prove whether its final external
				// effect completed. Retire it as unknown and never restore its runtime
				// state, which avoids both false success and side-effect replay.
				const summary = "outcome unknown after cancellation and confirmed Agent restart"
				settled, _, err := e.options.Children.Settle(ctx, app.AgentChildSessionSettleCommand{
					Fence: fence, ActorSessionID: rootIdentity.SessionID, SessionID: child.SessionID,
					Status: task.StepUnknown, CheckpointThrough: child.CheckpointThrough,
					CheckpointDigest: append([]byte(nil), child.CheckpointDigest...), Summary: summary,
				})
				if err != nil || settled.TaskID != value.ID || settled.SessionID != child.SessionID ||
					settled.ParentSessionID != rootIdentity.SessionID || settled.Step.Status != task.StepUnknown ||
					settled.Step.Summary != summary || settled.CheckpointThrough != child.CheckpointThrough ||
					!bytes.Equal(settled.CheckpointDigest, child.CheckpointDigest) {
					return errors.New("dsh cancelled background job outcome could not be durably reconciled")
				}
				continue
			}
			if child.CheckpointThrough < 0 || len(child.CheckpointDigest) != sha256.Size {
				return errors.New("dsh background job cannot be safely resumed")
			}
			definitions, err := cloudDSHChildToolDefinitions(baseDefinitions, child.AllowedTools)
			if err != nil {
				return errors.New("dsh background job authorization is unavailable")
			}
			identity := rootIdentity
			identity.SessionID = child.SessionID
			state := newCloudDSHSessionState(fence, identity, rootIdentity.SessionID, child.SpawnKey, definitions,
				false, true, child.CheckpointThrough, child.CheckpointDigest)
			state.background = true
			states[child.SessionID] = state
			backgroundJobs = append(backgroundJobs, map[string]any{"task_id": identity.TaskID, "owner_id": identity.OwnerID,
				"agent_id": identity.AgentID, "session_id": identity.SessionID, "generation": identity.Generation,
				"authorization_version": identity.AuthorizationVersion, "parent_session_id": child.ParentSessionID,
				"spawn_key": child.SpawnKey, "title": child.Step.Title, "background": true, "cancel_requested": false,
				"step_status": child.Step.Status, "tools": cloudDSHToolPayload(definitions),
				"checkpoint_through_seq": child.CheckpointThrough, "checkpoint_sha256": hex.EncodeToString(child.CheckpointDigest)})
		}
	}
	frames := make(chan cloudDSHRead, 1)
	go readCloudDSHFrames(ctx, output, frames)
	pendingCancels := make(map[string]cloudDSHJobCancelPending)
	ready := false
	for {
		var item cloudDSHRead
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case item = <-frames:
		}
		if item.err != nil {
			return errors.New("dsh connection ended before a verified result")
		}
		frame, err := decodeCloudDSHFrame(item.frame)
		if err != nil {
			return err
		}
		if !ready {
			if frame.Type != "ready" || frame.Version != 1 || frame.Engine != "dsh" || frame.EngineVer != cloudDSHEngineVersion {
				return errors.New("dsh startup handshake is invalid")
			}
			maxTokens := value.Budget.ModelTokens
			if maxTokens > 2048 {
				maxTokens = 2048
			}
			start := map[string]any{"type": "start", "version": 1, "task_id": rootIdentity.TaskID, "owner_id": rootIdentity.OwnerID,
				"agent_id": rootIdentity.AgentID, "session_id": rootIdentity.SessionID, "generation": rootIdentity.Generation,
				"authorization_version": rootIdentity.AuthorizationVersion, "model": value.ModelID,
				"max_tokens": maxTokens, "tools": cloudDSHToolPayload(rootDefinitions)}
			if e.options.Children != nil {
				start["background_jobs"] = backgroundJobs
			}
			if resume {
				start["resume"] = true
				start["checkpoint_through_seq"] = rootThrough
				start["checkpoint_sha256"] = hex.EncodeToString(rootDigest)
			} else {
				start["prompt"] = value.Objective
			}
			if err := writeCloudDSHFrame(ctx, input, start); err != nil {
				return err
			}
			ready = true
			continue
		}
		if frame.Type == "failed" {
			return errors.New("dsh reported an unavailable runtime")
		}
		if frame.Type == "child_create" {
			if err := e.createCloudDSHChildSession(ctx, value, fence, frame, states, baseDefinitions, input); err != nil {
				return err
			}
			continue
		}
		state := states[frame.SessionID]
		if state == nil || frame.cloudDSHIdentity != state.identity || state.settled {
			return errors.New("dsh frame identity is invalid")
		}
		switch frame.Type {
		case "checkpoint_read":
			if !state.resume || !cloudDSHID.MatchString(frame.ID) || frame.From == nil || *frame.From != state.readThrough+1 ||
				*frame.From > state.restoreThrough || len(state.pageReadIDs) >= 100_000 {
				return errors.New("dsh checkpoint read request is invalid")
			}
			if _, exists := state.pageReadIDs[frame.ID]; exists {
				return errors.New("dsh checkpoint read identity is repeated")
			}
			state.pageReadIDs[frame.ID] = struct{}{}
			page, err := e.options.Checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{
				Fence: fence, SessionID: state.identity.SessionID,
				From: *frame.From, Through: state.restoreThrough, ThroughDigest: append([]byte(nil), state.restoreDigest...),
			})
			if err != nil || page.From != *frame.From || page.Through < page.From || page.Through > state.restoreThrough ||
				len(page.Payload) == 0 || len(page.Payload) > 1<<20 || !utf8.Valid(page.Payload) || !uniqueCloudDSHJSON(page.Payload) {
				return errors.New("dsh checkpoint page is unavailable")
			}
			if err := state.history.append(page.Payload, page.From, page.Through); err != nil {
				return err
			}
			digest := hex.EncodeToString(page.Digest)
			if !cloudDSHDigest(page.Payload, digest) || (page.Through == state.restoreThrough && !cloudDSHDigest(page.Payload, hex.EncodeToString(state.restoreDigest))) {
				return errors.New("dsh checkpoint page digest is invalid")
			}
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "checkpoint_page", "id": frame.ID, "ok": true,
				"task_id": state.identity.TaskID, "owner_id": state.identity.OwnerID, "agent_id": state.identity.AgentID,
				"generation": state.identity.Generation, "authorization_version": state.identity.AuthorizationVersion,
				"session_id": state.identity.SessionID, "from_seq": page.From, "through_seq": page.Through,
				"payload": string(page.Payload), "sha256": digest}); err != nil {
				return err
			}
			state.readThrough = page.Through
			if state.readThrough == state.restoreThrough {
				state.history.finishRestore()
			}
		case "checkpoint":
			if !cloudDSHID.MatchString(frame.ID) || frame.From == nil || frame.Through == nil ||
				*frame.From != state.through+1 || *frame.Through < *frame.From || *frame.Through > 1<<53-1 ||
				len(frame.Payload) == 0 || len(frame.Payload) > 1<<20 || !utf8.ValidString(frame.Payload) ||
				!uniqueCloudDSHJSON([]byte(frame.Payload)) || !cloudDSHDigest([]byte(frame.Payload), frame.SHA256) {
				return errors.New("dsh checkpoint is invalid")
			}
			if _, exists := state.checkpointIDs[frame.ID]; exists || len(state.checkpointIDs) >= 4096 {
				return errors.New("dsh checkpoint identity is repeated or exceeds the limit")
			}
			state.checkpointIDs[frame.ID] = struct{}{}
			digest, _ := hex.DecodeString(frame.SHA256)
			if _, err := e.options.Checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: checkpointFenceForSession(fence, state.identity.SessionID),
				From: *frame.From, Through: *frame.Through, Digest: digest, Payload: []byte(frame.Payload)}); err != nil {
				return errors.New("dsh checkpoint was not durably confirmed")
			}
			if err := state.history.append([]byte(frame.Payload), *frame.From, *frame.Through); err != nil {
				return err
			}
			state.through = *frame.Through
			state.checkpointDigest = append(state.checkpointDigest[:0], digest...)
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "checkpoint_ack", "id": frame.ID, "ok": true,
				"through_seq": state.through, "sha256": frame.SHA256,
				"task_id": state.identity.TaskID, "owner_id": state.identity.OwnerID, "agent_id": state.identity.AgentID,
				"generation": state.identity.Generation, "authorization_version": state.identity.AuthorizationVersion,
				"session_id": state.identity.SessionID}); err != nil {
				return err
			}
		case "model":
			if (state.resume && state.readThrough != state.restoreThrough) || !state.history.allCallsReconciled() ||
				frame.Through == nil || *frame.Through != state.through || state.through <= state.lastModel ||
				!cloudDSHDigest(frame.Request, frame.SHA256) || !validCloudDSHModelRequest(frame.Request, value.ModelID, state.definitions) {
				return errors.New("dsh model request is invalid")
			}
			callID, err := app.DSHModelCallID(value.ID, state.identity.SessionID, state.through)
			if err != nil || frame.ID != callID {
				return errors.New("dsh model identity is invalid")
			}
			state.lastModel = state.through
			cancelRequested, err := e.cloudDSHJobCancelRequested(ctx, fence, state)
			if err != nil {
				return errors.New("dsh background job authorization changed")
			}
			if cancelRequested {
				continue
			}
			response, err := e.options.Models.Complete(ctx, app.ModelCall{Source: app.ModelCallTask,
				Task:    cloudDSHModelFencePointer(fence, state.identity.SessionID, state.through),
				ModelID: value.ModelID, CallID: frame.ID, RawRequest: append(json.RawMessage(nil), frame.Request...), ImageResolver: state.images})
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err != nil || !validCloudDSHModelResponse(response, state.definitions) {
				return errors.New("dsh model result is unavailable or contains an unauthorized tool")
			}
			if err := writeCloudDSHModel(ctx, input, frame.ID, response); err != nil {
				return err
			}
		case "model_cancel":
			callID, err := app.DSHModelCallID(value.ID, state.identity.SessionID, state.lastModel)
			if state.lastModel < 0 || err != nil || frame.ID != callID {
				return errors.New("dsh model cancellation identity is invalid")
			}
		case "settled":
			if state.identity.SessionID != rootIdentity.SessionID || frame.Through == nil || *frame.Through != state.through ||
				state.through < 0 || frame.State != "completed_candidate" ||
				(state.resume && (state.readThrough != state.restoreThrough || state.lastModel < 0 && !state.history.lastTurnCompleted)) ||
				(!state.resume && state.lastModel < 0) {
				return errors.New("dsh did not produce a completed candidate")
			}
			candidate, err := state.history.completion(fence, state.through, state.checkpointDigest)
			if err != nil {
				return err
			}
			childSessionIDs := make([]string, 0, len(states)-1)
			for sessionID := range states {
				if sessionID != rootIdentity.SessionID {
					childSessionIDs = append(childSessionIDs, sessionID)
				}
			}
			sort.Strings(childSessionIDs)
			for _, sessionID := range childSessionIDs {
				child := states[sessionID]
				for _, childInput := range child.history.inputs {
					childInput.CallID = cloudDSHNamespacedToolCallID(value.ID, sessionID, childInput.CallID)
					candidate.Inputs = append(candidate.Inputs, childInput)
				}
			}
			*completion = candidate
			return ErrCloudDSHCompletionPending
		case "job_cancel_result":
			pending, exists := pendingCancels[frame.ID]
			if !exists || frame.cloudDSHIdentity != pending.identity || frame.JobID != pending.jobID ||
				(frame.Status != "cancelled" && frame.Status != "succeeded" && frame.Status != "failed" && frame.Status != "unknown") {
				return errors.New("dsh job cancellation response is invalid")
			}
			delete(pendingCancels, frame.ID)
			child, err := e.options.Children.Get(ctx, pending.fence, pending.identity.SessionID, pending.jobID)
			if err != nil || child.Step.Status == task.StepRunning || cloudDSHJobStatus(child) != frame.Status {
				return errors.New("dsh job cancellation was not durably confirmed")
			}
			result := app.AgentToolResult{Status: app.AgentToolSucceeded, Output: cloudDSHJobResult(child)}
			if err := pending.session.history.noteToolResult(pending.frame.CallID, cloudDSHJobCancelToolName,
				pending.arguments, result.Status, result.Output, nil); err != nil {
				return err
			}
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "tool_result", "id": pending.frame.ID,
				"task_id": pending.identity.TaskID, "owner_id": pending.identity.OwnerID, "agent_id": pending.identity.AgentID,
				"generation": pending.identity.Generation, "authorization_version": pending.identity.AuthorizationVersion,
				"session_id": pending.identity.SessionID, "call_id": pending.frame.CallID, "status": result.Status,
				"output": result.Output, "images": []any{}}); err != nil {
				return err
			}
		case "child_settle":
			if e.options.Children == nil || state.identity.SessionID == rootIdentity.SessionID || state.parentSessionID != rootIdentity.SessionID ||
				state.spawnKey != frame.CallID || !cloudDSHID.MatchString(frame.CallID) {
				return errors.New("dsh child Agent settlement is invalid")
			}
			if frame.Through == nil || *frame.Through != state.through {
				return errors.New("dsh child Agent settlement sequence is invalid")
			}
			if (frame.Status != string(task.StepSucceeded) && frame.Status != string(task.StepFailed)) || len(frame.Summary) > 2048 || !utf8.ValidString(frame.Summary) {
				return errors.New("dsh child Agent settlement result is invalid")
			}
			if (state.through < 0 && (frame.SHA256 != "" || len(state.checkpointDigest) != 0)) ||
				(state.through >= 0 && hex.EncodeToString(state.checkpointDigest) != frame.SHA256) {
				return errors.New("dsh child Agent settlement digest is invalid")
			}
			summary := strings.TrimSpace(frame.Summary)
			status := task.StepStatus(frame.Status)
			if status == task.StepSucceeded && (state.through < 0 || state.history.nextSeq != state.through+1 ||
				!state.history.lastTurnCompleted || strings.TrimSpace(state.history.result) == "" || summary != strings.TrimSpace(state.history.result)) {
				return errors.New("dsh child Agent success is not supported by its checkpoint")
			}
			settled, _, err := e.options.Children.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: fence,
				ActorSessionID: rootIdentity.SessionID, SessionID: state.identity.SessionID, Status: status,
				CheckpointThrough: state.through, CheckpointDigest: append([]byte(nil), state.checkpointDigest...), Summary: summary})
			if err != nil || settled.TaskID != value.ID || settled.SessionID != state.identity.SessionID ||
				settled.ParentSessionID != state.parentSessionID || settled.Step.Status != status || strings.TrimSpace(settled.Step.Summary) != summary ||
				settled.CheckpointThrough != state.through || !bytes.Equal(settled.CheckpointDigest, state.checkpointDigest) {
				return errors.New("dsh child Agent settlement was not durably confirmed")
			}
			if !state.background {
				if err := recordCloudDSHChildToolResult(states[state.parentSessionID], frame.CallID, settled); err != nil {
					return err
				}
			}
			state.settled = true
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "child_settle_ack", "call_id": frame.CallID,
				"session_id": state.identity.SessionID, "ok": true, "status": status, "summary": summary}); err != nil {
				return err
			}
		case "tool_call":
			if (e.options.Tools == nil && !cloudDSHJobTool(frame.Name)) || frame.Through == nil || *frame.Through != state.through || state.through < 0 ||
				!cloudDSHID.MatchString(frame.ID) || frame.CallID == "" || len(frame.CallID) > 256 || !cloudDSHID.MatchString(frame.Name) ||
				!cloudDSHToolAllowed(state.definitions, frame.Name) || frame.Name == cloudDSHDelegateToolName || !validCloudDSHJSONObject(frame.Arguments) {
				return errors.New("dsh tool call frame is invalid")
			}
			arguments, ok := canonicalCloudDSHJSON(frame.Arguments)
			recoveryCall := state.resume && state.history.isRecoveryCall(frame.CallID)
			if !ok || (recoveryCall && (state.readThrough != state.restoreThrough || state.history.validateRecoveryToolCall(frame.CallID, frame.Name, arguments) != nil)) ||
				(!recoveryCall && (!state.history.allHistoricalCallsReconciled() || state.history.validateToolCall(frame.CallID, frame.Name, arguments) != nil)) {
				return errors.New("dsh tool arguments are invalid")
			}
			cancelRequested, err := e.cloudDSHJobCancelRequested(ctx, fence, state)
			if err != nil {
				return errors.New("dsh background job authorization changed")
			}
			if cancelRequested {
				continue
			}
			toolCallID := frame.CallID
			if state.identity.SessionID != rootIdentity.SessionID {
				toolCallID = cloudDSHNamespacedToolCallID(value.ID, state.identity.SessionID, frame.CallID)
			}
			call := app.ModelToolCall{CallID: toolCallID, Name: frame.Name, Arguments: string(arguments)}
			var result app.AgentToolResult
			if cloudDSHJobTool(frame.Name) {
				if state.identity.SessionID != rootIdentity.SessionID || e.options.Children == nil {
					return errors.New("dsh job tool is unavailable to this session")
				}
				if recoveryCall {
					if output := state.history.recoveryOutput(frame.CallID); output != nil {
						result = app.AgentToolResult{Status: app.AgentToolSucceeded, Output: *output}
					}
				}
				if result.Status == "" {
					if frame.Name == cloudDSHJobCancelToolName {
						immediate, pending, jobID, cancelErr := e.cancelCloudDSHJob(ctx, fence, rootIdentity.SessionID, frame.Arguments)
						if cancelErr != nil {
							return errors.New("dsh background job cancellation could not be authorized")
						}
						if pending {
							pendingCancels[frame.ID] = cloudDSHJobCancelPending{session: state, fence: fence, frame: frame,
								arguments: arguments, identity: rootIdentity, jobID: jobID}
							if err := e.writeCloudDSHJobCancel(ctx, rootIdentity, frame.ID, jobID, input); err != nil {
								delete(pendingCancels, frame.ID)
								return err
							}
							continue
						}
						result = immediate
					} else {
						result, err = e.executeCloudDSHJobRead(ctx, value, fence, frame, states)
					}
				}
			} else if recoveryCall {
				recoveryTools, ok := e.options.Tools.(CloudDSHRecoveryTools)
				if !ok {
					return errors.New("dsh recovery broker is unavailable")
				}
				result, err = recoveryTools.Reconcile(ctx, value, call, state.history.recoveryOutput(frame.CallID))
			} else {
				result, err = e.options.Tools.Execute(ctx, value, call)
			}
			if err != nil {
				return errors.New("dsh tool execution could not be confirmed")
			}
			if result.Status == app.AgentToolUnknown || result.Status == app.AgentToolNeedsUser {
				if result.Status == app.AgentToolNeedsUser {
					return ErrCloudDSHWaitingForUser
				}
				return errors.New("dsh tool execution requires user attention")
			}
			if result.Status != app.AgentToolSucceeded && result.Status != app.AgentToolFailed {
				return errors.New("dsh tool returned an unsupported result")
			}
			if !validCloudDSHToolImageResult(frame.Name, result) {
				return errors.New("dsh tool returned an unauthorized image")
			}
			if !utf8.ValidString(result.Output) || len(result.Output) > cloudDSHMaxText {
				return errors.New("dsh tool result exceeds the IPC limit")
			}
			toolImages, err := state.images.add(result.Images)
			if err != nil {
				return errors.New("dsh tool image could not be validated")
			}
			if err := state.history.noteToolResult(frame.CallID, frame.Name, arguments, result.Status, result.Output, toolImages); err != nil {
				return err
			}
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "tool_result", "id": frame.ID,
				"task_id": state.identity.TaskID, "owner_id": state.identity.OwnerID, "agent_id": state.identity.AgentID,
				"generation": state.identity.Generation, "authorization_version": state.identity.AuthorizationVersion,
				"session_id": state.identity.SessionID, "call_id": frame.CallID, "status": result.Status, "output": result.Output, "images": toolImages}); err != nil {
				return err
			}
		default:
			return errors.New("unsupported dsh frame")
		}
	}
}

func (e *CloudDSHExecutor) createCloudDSHChildSession(ctx context.Context, value task.Task, fence app.AgentInstanceFence,
	frame cloudDSHFrame, states map[string]*cloudDSHSessionState, parentDefinitions []app.AgentToolDefinition, input io.WriteCloser) error {
	parent := states[frame.SessionID]
	if e.options.Children == nil || parent == nil || !parent.canDelegate || parent.identity.SessionID != fence.AgentSessionID ||
		frame.cloudDSHIdentity != parent.identity || frame.Through == nil || *frame.Through != parent.through || !cloudDSHID.MatchString(frame.CallID) {
		return errors.New("dsh child Agent request identity is invalid")
	}
	arguments, definitions, err := validateCloudDSHDelegateArguments(frame.Arguments, parentDefinitions)
	canonical, canonicalOK := canonicalCloudDSHJSON(frame.Arguments)
	recoveryCall := parent.resume && parent.history.isRecoveryCall(frame.CallID)
	if err != nil || !canonicalOK || (recoveryCall && (parent.readThrough != parent.restoreThrough ||
		parent.history.validateRecoveryToolCall(frame.CallID, cloudDSHDelegateToolName, canonical) != nil)) ||
		(!recoveryCall && (!parent.history.allHistoricalCallsReconciled() ||
			parent.history.validateToolCall(frame.CallID, cloudDSHDelegateToolName, canonical) != nil)) {
		return errors.New("dsh child Agent request does not match the checkpointed tool call")
	}
	allowedTools := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		allowedTools = append(allowedTools, definition.Name)
	}
	child, _, err := e.options.Children.Create(ctx, app.AgentChildSessionCommand{Fence: fence, ParentSessionID: fence.AgentSessionID,
		SpawnKey: frame.CallID, Arguments: canonical, Title: strings.TrimSpace(arguments.Title), AllowedTools: allowedTools,
		Background: arguments.RunInBackground})
	if err != nil || child.TaskID != value.ID || child.ParentSessionID != fence.AgentSessionID || child.SessionID == "" ||
		!sameWorkerToolNames(child.AllowedTools, allowedTools) {
		return errors.New("dsh child Agent could not be durably created")
	}
	if arguments.RunInBackground {
		if child.Step.Status == task.StepUnknown && !child.CancelRequested {
			return errors.New("dsh background job has an unknown outcome")
		}
		output, err := json.Marshal(struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}{JobID: child.SessionID, Status: cloudDSHJobStatus(child)})
		if err != nil {
			return errors.New("dsh background job handle was not durably reconciled")
		}
		handle := string(output)
		if recoveryCall {
			if recorded := parent.history.recoveryOutput(frame.CallID); recorded != nil {
				var previous struct {
					JobID string `json:"job_id"`
				}
				if json.Unmarshal([]byte(*recorded), &previous) != nil || previous.JobID != child.SessionID {
					return errors.New("dsh background job recovery handle changed")
				}
				// Keep the handle recorded by the previous generation, then
				// reconcile it against the checkpoint. A following job_get returns
				// the current terminal status without rewriting history.
				handle = *recorded
			}
		}
		if parent.history.noteToolResult(frame.CallID, cloudDSHDelegateToolName, canonical,
			app.AgentToolSucceeded, handle, nil) != nil {
			return errors.New("dsh background job handle was not durably reconciled")
		}
	} else if child.Step.Status == task.StepSucceeded || child.Step.Status == task.StepFailed {
		if err := recordCloudDSHChildToolResult(parent, frame.CallID, child); err != nil {
			return err
		}
	}
	identity := parent.identity
	identity.SessionID = child.SessionID
	if child.Step.Status == task.StepRunning {
		if existing := states[child.SessionID]; existing != nil {
			if existing.spawnKey != frame.CallID || existing.identity != identity || existing.background != arguments.RunInBackground {
				return errors.New("dsh child Agent session identity was reused")
			}
		} else {
			resume := child.CheckpointThrough >= 0
			states[child.SessionID] = newCloudDSHSessionState(fence, identity, fence.AgentSessionID, frame.CallID,
				definitions, false, resume, child.CheckpointThrough, child.CheckpointDigest)
			states[child.SessionID].background = arguments.RunInBackground
		}
	}
	return writeCloudDSHFrame(ctx, input, map[string]any{"type": "child_session", "call_id": frame.CallID,
		"task_id": identity.TaskID, "owner_id": identity.OwnerID, "agent_id": identity.AgentID, "session_id": identity.SessionID,
		"generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
		"parent_session_id": child.ParentSessionID, "objective": arguments.Objective,
		"tools": cloudDSHToolPayload(definitions), "checkpoint_through_seq": child.CheckpointThrough,
		"checkpoint_sha256": hex.EncodeToString(child.CheckpointDigest), "step_status": child.Step.Status, "summary": child.Step.Summary})
}

func recordCloudDSHChildToolResult(parent *cloudDSHSessionState, callID string, child app.AgentChildSession) error {
	if parent == nil || parent.history == nil {
		return errors.New("dsh child Agent parent history is unavailable")
	}
	call := parent.history.calls[callID]
	if call == nil || call.name != cloudDSHDelegateToolName || !call.callSeen {
		return errors.New("dsh child Agent result does not match a checkpointed delegation")
	}
	var status app.AgentToolResultStatus
	output := strings.TrimSpace(child.Step.Summary)
	switch child.Step.Status {
	case task.StepSucceeded:
		status = app.AgentToolSucceeded
		if output == "" {
			output = "Child agent completed."
		}
	case task.StepFailed:
		status = app.AgentToolFailed
		if output == "" {
			output = "Child agent failed."
		}
	default:
		return errors.New("dsh child Agent result is not terminal")
	}
	return parent.history.noteToolResult(callID, cloudDSHDelegateToolName, call.arguments, status, output, nil)
}
