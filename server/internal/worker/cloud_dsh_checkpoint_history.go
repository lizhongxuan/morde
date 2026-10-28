package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"morde/server/internal/app"
)

const cloudDSHCompletionInputLimit = 12 << 20

type cloudDSHCheckpointHistory struct {
	sessionID         string
	nextSeq           int64
	inputs            []app.ModelInput
	calls             map[string]*cloudDSHToolCall
	fileWrites        map[string]struct{}
	result            string
	assistantOutputs  []cloudDSHAssistantOutput
	bytes             int
	restoring         bool
	lastTurnCompleted bool
}

type cloudDSHAssistantOutput struct {
	sequence int64
	text     string
}

type cloudDSHToolCall struct {
	name           string
	arguments      []byte
	turn           int64
	step           int64
	callSeq        int64
	callSeen       bool
	historical     bool
	status         app.AgentToolResultStatus
	output         string
	resultText     string
	resultImages   []cloudDSHImageRef
	runtimeImages  []cloudDSHImageRef
	resultSeen     bool
	reconciled     bool
	outputRecorded bool
}

func newCloudDSHCheckpointHistory(sessionID string) *cloudDSHCheckpointHistory {
	return &cloudDSHCheckpointHistory{sessionID: sessionID, calls: make(map[string]*cloudDSHToolCall), fileWrites: make(map[string]struct{})}
}

func (h *cloudDSHCheckpointHistory) beginRestore() { h.restoring = true }

func (h *cloudDSHCheckpointHistory) finishRestore() { h.restoring = false }

func (h *cloudDSHCheckpointHistory) append(payload []byte, from, through int64) error {
	var page struct {
		Header struct {
			ID string `json:"id"`
		} `json:"header"`
		From   int64 `json:"from_seq"`
		Events []struct {
			Sequence        int64           `json:"seq"`
			Type            string          `json:"type"`
			Data            json.RawMessage `json:"data"`
			SurfaceOp       string          `json:"surfaceOp"`
			SourceEventSeqs []int64         `json:"sourceEventSeqs"`
		} `json:"events"`
	}
	if !uniqueCloudDSHJSON(payload) || json.Unmarshal(payload, &page) != nil || page.Header.ID != h.sessionID ||
		page.From != from || from != h.nextSeq || len(page.Events) == 0 || through < from ||
		int64(len(page.Events)) != through-from+1 {
		return errors.New("dsh checkpoint session history is invalid")
	}
	for index, event := range page.Events {
		if event.Sequence != from+int64(index) || event.Type == "" || len(event.Data) == 0 || !json.Valid(event.Data) {
			return errors.New("dsh checkpoint event sequence is invalid")
		}
		switch event.Type {
		case "assistant/message":
			if err := h.appendAssistantMessage(event.Sequence, event.Data); err != nil {
				return err
			}
		case "tool/call":
			if err := h.appendToolCall(event.Sequence, event.Data); err != nil {
				return err
			}
		case "tool/result":
			if err := h.appendToolResult(event.Sequence, event.Data, event.SurfaceOp, event.SourceEventSeqs); err != nil {
				return err
			}
		case "step/end":
			var ended struct {
				Turn int64 `json:"turn"`
				Step int64 `json:"step"`
			}
			if json.Unmarshal(event.Data, &ended) != nil || ended.Turn < 1 || ended.Step < 1 {
				return errors.New("dsh step checkpoint is invalid")
			}
			for _, call := range h.calls {
				if call.turn == ended.Turn && call.step == ended.Step && (!call.callSeen || !call.resultSeen) {
					return errors.New("dsh step closed with an unresolved tool call")
				}
			}
		case "turn/end":
			var ended struct {
				Turn   int64 `json:"turn"`
				Reason struct {
					Kind string `json:"kind"`
				} `json:"reason"`
			}
			if json.Unmarshal(event.Data, &ended) != nil || ended.Turn < 1 || ended.Reason.Kind == "" {
				return errors.New("dsh turn checkpoint is invalid")
			}
			for _, call := range h.calls {
				if call.turn == ended.Turn && (!call.callSeen || !call.resultSeen) {
					return errors.New("dsh turn closed with an unresolved tool call")
				}
			}
			h.lastTurnCompleted = ended.Reason.Kind == "completed"
		case "turn/start":
			h.lastTurnCompleted = false
		}
		if h.bytes > cloudDSHCompletionInputLimit {
			return errors.New("dsh file write history exceeds its limit")
		}
	}
	h.nextSeq = through + 1
	return nil
}

func (h *cloudDSHCheckpointHistory) appendAssistantMessage(sequence int64, data json.RawMessage) error {
	var event struct {
		Turn    int64 `json:"turn"`
		Step    int64 `json:"step"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &event) != nil || event.Turn < 1 || event.Step < 1 || event.Message.Role != "assistant" || event.Message.Content == nil {
		return errors.New("dsh assistant checkpoint is invalid")
	}
	var text strings.Builder
	for _, block := range event.Message.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
			continue
		}
		if block.Type != "tool-call" {
			continue
		}
		if !cloudDSHID.MatchString(block.ID) || !cloudDSHID.MatchString(block.Name) {
			return errors.New("dsh tool call checkpoint identity is invalid")
		}
		arguments := []byte(block.Arguments)
		if !validCloudDSHJSONObject(arguments) {
			return errors.New("dsh tool arguments in checkpoint are invalid")
		}
		canonical, ok := canonicalCloudDSHJSON(arguments)
		if !ok {
			return errors.New("dsh tool arguments are invalid")
		}
		if _, exists := h.calls[block.ID]; exists || len(h.calls) >= 100 {
			return errors.New("dsh checkpoint tool call identity is repeated or over budget")
		}
		call := &cloudDSHToolCall{name: block.Name, arguments: canonical, turn: event.Turn, step: event.Step, callSeq: -1, historical: h.restoring}
		h.calls[block.ID] = call
		if block.Name == "file_write" {
			if len(canonical) > 32<<10 {
				return errors.New("dsh file write checkpoint exceeds its limit")
			}
			h.fileWrites[block.ID] = struct{}{}
			h.inputs = append(h.inputs, app.ModelInput{Type: "function_call", CallID: block.ID, Name: block.Name, Arguments: string(canonical)})
			h.bytes += len(canonical) + len(block.ID) + len(block.Name)
		}
	}
	if text.Len() > app.MaxAssistantMessageBytes || !utf8.ValidString(text.String()) {
		return errors.New("dsh assistant result exceeds its limit")
	}
	h.result = text.String()
	if h.result != "" {
		h.assistantOutputs = append(h.assistantOutputs, cloudDSHAssistantOutput{sequence: sequence, text: h.result})
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) outputAfter(after int64, maxBytes int) (string, int64, error) {
	if after < -1 || after >= h.nextSeq || maxBytes < 1 {
		return "", after, errors.New("dsh output cursor is invalid")
	}
	var output strings.Builder
	cursor := after
	overflow := false
	for _, item := range h.assistantOutputs {
		if item.sequence <= after {
			continue
		}
		separator := 0
		if output.Len() > 0 {
			separator = 1
		}
		if output.Len()+separator+len(item.text) > maxBytes {
			if output.Len() == 0 && len(item.text) > maxBytes {
				return "", after, errors.New("dsh output message exceeds the cursor page limit")
			}
			overflow = true
			break
		}
		if separator != 0 {
			output.WriteByte('\n')
		}
		output.WriteString(item.text)
		cursor = item.sequence
	}
	if !overflow {
		cursor = h.nextSeq - 1
	}
	return output.String(), cursor, nil
}

func (h *cloudDSHCheckpointHistory) appendToolCall(sequence int64, data json.RawMessage) error {
	var event struct {
		Turn      int64  `json:"turn"`
		Step      int64  `json:"step"`
		CallID    string `json:"callId"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if json.Unmarshal(data, &event) != nil || event.Turn < 1 || event.Step < 1 || !cloudDSHID.MatchString(event.CallID) {
		return errors.New("dsh tool call checkpoint is invalid")
	}
	call := h.calls[event.CallID]
	arguments := []byte(event.Arguments)
	canonical, ok := canonicalCloudDSHJSON(arguments)
	if call == nil || call.callSeen || call.resultSeen || !ok || call.turn != event.Turn || call.step != event.Step ||
		call.name != event.Name || !bytes.Equal(call.arguments, canonical) {
		return errors.New("dsh tool call does not match its assistant checkpoint")
	}
	call.callSeq, call.callSeen = sequence, true
	return nil
}

func (h *cloudDSHCheckpointHistory) appendToolResult(sequence int64, data json.RawMessage, surfaceOp string, sourceSeqs []int64) error {
	var event struct {
		Turn    int64 `json:"turn"`
		Step    int64 `json:"step"`
		Message struct {
			ToolCallID string `json:"toolCallId"`
			IsError    bool   `json:"isError"`
			Source     struct {
				CallID string `json:"callId"`
			} `json:"source"`
			Content []struct {
				Type       string          `json:"type"`
				Text       string          `json:"text"`
				Attachment json.RawMessage `json:"attachment"`
			} `json:"content"`
		} `json:"message"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &event) != nil || event.Turn < 1 || event.Step < 1 || event.Message.ToolCallID == "" ||
		event.Message.ToolCallID != event.Message.Source.CallID || event.Message.IsError || len(event.Error) != 0 && string(event.Error) != "null" ||
		surfaceOp != "append" || len(sourceSeqs) != 1 || sourceSeqs[0] >= sequence {
		return errors.New("dsh tool result checkpoint is invalid")
	}
	call := h.calls[event.Message.ToolCallID]
	if call == nil || !call.callSeen || call.resultSeen || sourceSeqs[0] != call.callSeq || call.turn != event.Turn || call.step != event.Step ||
		len(event.Message.Content) < 1 || len(event.Message.Content) > 2 || event.Message.Content[0].Type != "text" {
		return errors.New("dsh tool result does not match its durable tool call")
	}
	var imageRefs []cloudDSHImageRef
	if len(event.Message.Content) == 2 {
		block := event.Message.Content[1]
		if block.Type != "image" || (call.name != "desktop_screenshot" && call.name != "browser_screenshot") {
			return errors.New("dsh tool result contains an unauthorized image")
		}
		ref, err := decodeCloudDSHImageRef(block.Attachment)
		if err != nil {
			return err
		}
		imageRefs = append(imageRefs, ref)
	}
	if call.status != "" && !sameCloudDSHImageRefs(call.runtimeImages, imageRefs) {
		return errors.New("dsh tool result image differs from the reconciled Host result")
	}
	output := event.Message.Content[0].Text
	if len(output) > 64<<10 || !utf8.ValidString(output) {
		return errors.New("dsh tool result checkpoint exceeds its limit")
	}
	call.resultText, call.resultImages, call.resultSeen = output, imageRefs, true
	if call.status != "" {
		if cloudDSHToolResultText(call.status, call.output) != output {
			return errors.New("dsh tool result differs from the reconciled runtime result")
		}
		call.reconciled = true
		h.recordFileWriteOutput(event.Message.ToolCallID, output)
	}
	return nil
}

func cloudDSHToolResultText(status app.AgentToolResultStatus, output string) string {
	if status == app.AgentToolFailed {
		return "Tool failed: " + output
	}
	return output
}

func (h *cloudDSHCheckpointHistory) recordFileWriteOutput(callID, output string) {
	if _, isFileWrite := h.fileWrites[callID]; !isFileWrite || h.calls[callID].outputRecorded {
		return
	}
	h.calls[callID].outputRecorded = true
	h.inputs = append(h.inputs, app.ModelInput{Type: "function_call_output", CallID: callID, Output: output})
	h.bytes += len(output) + len(callID)
}

func (h *cloudDSHCheckpointHistory) validateToolCall(callID, name string, arguments []byte) error {
	call := h.calls[callID]
	canonical, ok := canonicalCloudDSHJSON(arguments)
	if call == nil || !call.callSeen || call.historical || !ok || call.name != name || !bytes.Equal(call.arguments, canonical) || call.resultSeen || call.status != "" {
		return errors.New("dsh tool call does not match its durable checkpoint")
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) validateRecoveryToolCall(callID, name string, arguments []byte) error {
	call := h.calls[callID]
	canonical, ok := canonicalCloudDSHJSON(arguments)
	if call == nil || !call.historical || !call.callSeen || !ok || call.name != name || !bytes.Equal(call.arguments, canonical) || call.reconciled {
		return errors.New("dsh recovery tool call does not match its pinned checkpoint")
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) isRecoveryCall(callID string) bool {
	call := h.calls[callID]
	return call != nil && call.historical
}

func (h *cloudDSHCheckpointHistory) recoveryOutput(callID string) *string {
	call := h.calls[callID]
	if call == nil || !call.resultSeen {
		return nil
	}
	output := call.resultText
	return &output
}

func (h *cloudDSHCheckpointHistory) noteToolResult(callID, name string, arguments []byte, status app.AgentToolResultStatus, output string, images []cloudDSHToolImage) error {
	call := h.calls[callID]
	canonical, ok := canonicalCloudDSHJSON(arguments)
	if call == nil || !ok || call.name != name || !bytes.Equal(call.arguments, canonical) || call.status != "" ||
		(status != app.AgentToolSucceeded && status != app.AgentToolFailed) || len(output) > 64<<10 || !utf8.ValidString(output) {
		return errors.New("dsh file write runtime result is invalid")
	}
	call.status, call.output, call.runtimeImages = status, output, cloudDSHImageReferences(images)
	h.bytes += len(output)
	if call.resultSeen {
		if cloudDSHToolResultText(status, output) != call.resultText || !sameCloudDSHImageRefs(call.runtimeImages, call.resultImages) {
			return errors.New("dsh checkpoint result differs from the runtime ledger")
		}
		call.reconciled = true
		h.recordFileWriteOutput(callID, call.resultText)
	}
	if h.bytes > cloudDSHCompletionInputLimit {
		return errors.New("dsh file write history exceeds its limit")
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) allCallsReconciled() bool {
	for _, call := range h.calls {
		if !call.callSeen || !call.resultSeen || !call.reconciled {
			return false
		}
	}
	return true
}

func (h *cloudDSHCheckpointHistory) allHistoricalCallsReconciled() bool {
	for _, call := range h.calls {
		if call.historical && (!call.callSeen || !call.resultSeen || !call.reconciled) {
			return false
		}
	}
	return true
}

func (h *cloudDSHCheckpointHistory) completion(fence app.AgentInstanceFence, through int64, digest []byte) (app.CloudDSHTaskCompletion, error) {
	if h == nil || h.sessionID != fence.AgentSessionID || h.nextSeq != through+1 || through < 0 || len(digest) != 32 || strings.TrimSpace(h.result) == "" {
		return app.CloudDSHTaskCompletion{}, errors.New("dsh final checkpoint does not match session history")
	}
	if !h.allCallsReconciled() {
		return app.CloudDSHTaskCompletion{}, errors.New("dsh tool call is not reconciled against the runtime ledger")
	}
	return app.CloudDSHTaskCompletion{
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration,
		AuthorizationVersion: fence.AuthorizationVersion, AgentSessionID: fence.AgentSessionID,
		CheckpointThroughSequence: through, CheckpointDigest: append([]byte(nil), digest...),
		Result: h.result, Inputs: append([]app.ModelInput(nil), h.inputs...),
	}, nil
}
