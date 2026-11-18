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

const (
	cloudDSHToolResultPruneThresholdChars = 8192
	cloudDSHToolResultPruneHeadChars      = 4096
	cloudDSHToolResultPruneTailChars      = 1024
	cloudDSHToolResultPruneMarker         = "\n\n[... tool result middle pruned ...]\n\n"
)

type cloudDSHCheckpointHistory struct {
	sessionID         string
	nextSeq           int64
	inputs            []app.ModelInput
	calls             map[string]*cloudDSHToolCall
	fileWrites        map[string]struct{}
	systemNodes       map[int64]struct{}
	surfaceNodes      map[int64]struct{}
	compactions       map[string]*cloudDSHCompactionProof
	pendingPrune      *cloudDSHToolResultPruneProof
	result            string
	resultTurn        int64
	activeTurn        int64
	completedTurn     int64
	assistantOutputs  []cloudDSHAssistantOutput
	bytes             int
	restoring         bool
	lastTurnCompleted bool
}

type cloudDSHAssistantOutput struct {
	sequence int64
	text     string
}

type cloudDSHCompactionProof struct {
	startSeq        int64
	turn            int64
	sourceCommandID string
	summarySeq      int64
	start           int64
	end             int64
	shadowedSeqs    []int64
	summaryText     []string
	replacementSeq  int64
	ended           bool
}

type cloudDSHToolResultPruneProof struct {
	sequence   int64
	resultSeq  int64
	tokenCount int64
}

type cloudDSHToolResultCheckpoint struct {
	Turn    int64 `json:"turn"`
	Step    int64 `json:"step"`
	Message struct {
		ToolCallID string `json:"toolCallId"`
		IsError    bool   `json:"isError"`
		Source     struct {
			CallID string `json:"callId"`
		} `json:"source"`
		Content []json.RawMessage `json:"content"`
	} `json:"message"`
	Error json.RawMessage `json:"error"`
}

type cloudDSHToolResultContentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	Attachment json.RawMessage `json:"attachment"`
}

type cloudDSHToolCall struct {
	name              string
	arguments         []byte
	turn              int64
	step              int64
	callSeq           int64
	callSeen          bool
	historical        bool
	status            app.AgentToolResultStatus
	output            string
	resultText        string
	resultImages      []cloudDSHImageRef
	runtimeImages     []cloudDSHImageRef
	assistantSeq      int64
	resultSeq         int64
	surfaceResultSeq  int64
	surfaceResultData json.RawMessage
	resultSeen        bool
	reconciled        bool
	outputRecorded    bool
}

func newCloudDSHCheckpointHistory(sessionID string) *cloudDSHCheckpointHistory {
	return &cloudDSHCheckpointHistory{sessionID: sessionID, calls: make(map[string]*cloudDSHToolCall),
		fileWrites: make(map[string]struct{}), systemNodes: make(map[int64]struct{}), surfaceNodes: make(map[int64]struct{}),
		compactions: make(map[string]*cloudDSHCompactionProof)}
}

func (h *cloudDSHCheckpointHistory) clone() *cloudDSHCheckpointHistory {
	clone := *h
	clone.inputs = append([]app.ModelInput(nil), h.inputs...)
	clone.calls = make(map[string]*cloudDSHToolCall, len(h.calls))
	for id, call := range h.calls {
		callCopy := *call
		callCopy.arguments = append([]byte(nil), call.arguments...)
		callCopy.surfaceResultData = append(json.RawMessage(nil), call.surfaceResultData...)
		callCopy.resultImages = append([]cloudDSHImageRef(nil), call.resultImages...)
		callCopy.runtimeImages = append([]cloudDSHImageRef(nil), call.runtimeImages...)
		clone.calls[id] = &callCopy
	}
	clone.fileWrites = make(map[string]struct{}, len(h.fileWrites))
	for id := range h.fileWrites {
		clone.fileWrites[id] = struct{}{}
	}
	clone.systemNodes = make(map[int64]struct{}, len(h.systemNodes))
	for sequence := range h.systemNodes {
		clone.systemNodes[sequence] = struct{}{}
	}
	clone.surfaceNodes = make(map[int64]struct{}, len(h.surfaceNodes))
	for sequence := range h.surfaceNodes {
		clone.surfaceNodes[sequence] = struct{}{}
	}
	clone.compactions = make(map[string]*cloudDSHCompactionProof, len(h.compactions))
	for id, proof := range h.compactions {
		proofCopy := *proof
		proofCopy.shadowedSeqs = append([]int64(nil), proof.shadowedSeqs...)
		proofCopy.summaryText = append([]string(nil), proof.summaryText...)
		clone.compactions[id] = &proofCopy
	}
	if h.pendingPrune != nil {
		proofCopy := *h.pendingPrune
		clone.pendingPrune = &proofCopy
	}
	clone.assistantOutputs = append([]cloudDSHAssistantOutput(nil), h.assistantOutputs...)
	return &clone
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
			SurfaceOp       json.RawMessage `json:"surfaceOp"`
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
		if h.pendingPrune != nil && event.Type != "tool/result" {
			return errors.New("dsh tool-result prune was not immediately replaced")
		}
		var surfaceOp string
		replacement := false
		if len(event.SurfaceOp) != 0 && string(event.SurfaceOp) != "null" {
			if err := json.Unmarshal(event.SurfaceOp, &surfaceOp); err != nil {
				switch event.Type {
				case "system/message":
					err = h.replaceSystemNode(event.Sequence, event.Data, event.SurfaceOp, event.SourceEventSeqs)
				case "user/message":
					err = h.replaceCompactedUserNode(event.Sequence, event.Data, event.SurfaceOp, event.SourceEventSeqs)
				case "tool/result":
					err = nil
				default:
					err = errors.New("unsupported replacement event")
				}
				if err != nil {
					return errors.New("dsh checkpoint surface operation is invalid")
				}
				replacement = true
			} else if surfaceOp != "append" {
				return errors.New("dsh checkpoint surface operation is invalid")
			}
		}
		switch event.Type {
		case "system/message":
			if surfaceOp == "append" && cloudDSHSystemMessage(event.Data) {
				h.systemNodes[event.Sequence] = struct{}{}
				h.surfaceNodes[event.Sequence] = struct{}{}
			} else if surfaceOp == "append" {
				return errors.New("dsh system checkpoint is invalid")
			}
		case "user/message":
			if surfaceOp == "append" {
				if !cloudDSHUserMessage(event.Data) {
					return errors.New("dsh user checkpoint is invalid")
				}
				h.surfaceNodes[event.Sequence] = struct{}{}
			} else if !replacement && surfaceOp != "" {
				return errors.New("dsh user checkpoint surface operation is invalid")
			}
		case "assistant/message":
			if err := h.appendAssistantMessage(event.Sequence, event.Data); err != nil {
				return err
			}
			h.surfaceNodes[event.Sequence] = struct{}{}
		case "tool/call":
			if err := h.appendToolCall(event.Sequence, event.Data); err != nil {
				return err
			}
		case "tool/result":
			if h.pendingPrune != nil && !replacement {
				return errors.New("dsh tool-result prune was not replaced")
			}
			if replacement {
				if err := h.replacePrunedToolResult(event.Sequence, event.Data, event.SurfaceOp, event.SourceEventSeqs); err != nil {
					return err
				}
			} else {
				if err := h.appendToolResult(event.Sequence, event.Data, surfaceOp, event.SourceEventSeqs); err != nil {
					return err
				}
				h.surfaceNodes[event.Sequence] = struct{}{}
			}
		case "compaction/prune":
			if surfaceOp != "" || replacement || len(event.SourceEventSeqs) != 0 {
				return errors.New("dsh tool-result prune event is not log-only")
			}
			if err := h.beginToolResultPrune(event.Sequence, event.Data); err != nil {
				return err
			}
		case "compaction/start":
			if err := h.beginCompaction(event.Sequence, event.Data); err != nil {
				return err
			}
		case "compaction/summary":
			if err := h.summarizeCompaction(event.Sequence, event.Data); err != nil {
				return err
			}
		case "compaction/end":
			if err := h.endCompaction(event.Data); err != nil {
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
			h.completedTurn = 0
			if h.lastTurnCompleted {
				h.completedTurn = ended.Turn
			}
			h.activeTurn = 0
		case "turn/start":
			var started struct {
				Turn int64 `json:"turn"`
			}
			if json.Unmarshal(event.Data, &started) != nil || started.Turn < 1 {
				return errors.New("dsh turn start checkpoint is invalid")
			}
			h.activeTurn = started.Turn
			h.lastTurnCompleted = false
			h.completedTurn = 0
			h.result = ""
			h.resultTurn = 0
		}
		if h.bytes > cloudDSHCompletionInputLimit {
			return errors.New("dsh file write history exceeds its limit")
		}
	}
	h.nextSeq = through + 1
	return nil
}

func cloudDSHSystemMessage(data json.RawMessage) bool {
	var value struct {
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
	}
	return json.Unmarshal(data, &value) == nil && value.Message.Role == "system"
}

func cloudDSHUserMessage(data json.RawMessage) bool {
	var value struct {
		Role string `json:"role"`
	}
	return json.Unmarshal(cloudDSHUserMessagePayload(data), &value) == nil && value.Role == "user"
}

func cloudDSHUserMessagePayload(data json.RawMessage) json.RawMessage {
	var value struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(data, &value) == nil && len(value.Message) > 0 && string(value.Message) != "null" {
		return value.Message
	}
	return data
}

func (h *cloudDSHCheckpointHistory) replaceSystemNode(sequence int64, data, raw json.RawMessage, sources []int64) error {
	var operation struct {
		Op    string          `json:"op"`
		Start json.RawMessage `json:"startSeq"`
		End   json.RawMessage `json:"endSeq"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var start, end int64
	if decoder.Decode(&operation) != nil || operation.Op != "replace" || len(operation.Start) == 0 || len(operation.End) == 0 ||
		string(operation.Start) == "null" || string(operation.End) == "null" ||
		string(operation.Start) == "-0" || string(operation.End) == "-0" ||
		json.Unmarshal(operation.Start, &start) != nil || json.Unmarshal(operation.End, &end) != nil ||
		start < 0 || start > 1<<53-1 || start != end || start >= sequence || len(sources) != 1 || sources[0] != start ||
		!cloudDSHSystemMessage(data) {
		return errors.New("dsh system replacement is invalid")
	}
	if _, exists := h.systemNodes[start]; !exists {
		return errors.New("dsh system replacement target is unavailable")
	}
	delete(h.systemNodes, start)
	delete(h.surfaceNodes, start)
	h.systemNodes[sequence] = struct{}{}
	h.surfaceNodes[sequence] = struct{}{}
	return nil
}

func (h *cloudDSHCheckpointHistory) beginCompaction(sequence int64, data json.RawMessage) error {
	fields, err := cloudDSHCompactionFields(data, "compactionId", "sourceCommandId", "turn")
	if err != nil {
		return err
	}
	var value struct {
		ID   string `json:"compactionId"`
		Turn int64  `json:"turn"`
	}
	if json.Unmarshal(data, &value) != nil || !cloudDSHID.MatchString(value.ID) || value.Turn < 1 || value.Turn != h.activeTurn ||
		sequence > 1<<53-1 || h.compactions[value.ID] != nil {
		return errors.New("dsh compaction start is invalid")
	}
	sourceCommandID := ""
	if raw := fields["sourceCommandId"]; raw != nil {
		if json.Unmarshal(raw, &sourceCommandID) != nil || !cloudDSHID.MatchString(sourceCommandID) {
			return errors.New("dsh compaction command identity is invalid")
		}
	}
	h.compactions[value.ID] = &cloudDSHCompactionProof{startSeq: sequence, turn: value.Turn, sourceCommandID: sourceCommandID}
	return nil
}

func (h *cloudDSHCheckpointHistory) summarizeCompaction(sequence int64, data json.RawMessage) error {
	fields, err := cloudDSHCompactionFields(data, "compactionId", "sourceCommandId", "summary", "shadowedRange", "shadowedSeqs",
		"shadowedTokenCount", "provider", "model", "maxTokens", "usage")
	if err != nil {
		return err
	}
	var value struct {
		ID      string `json:"compactionId"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		ShadowedRange struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"shadowedRange"`
		ShadowedSeqs       []int64 `json:"shadowedSeqs"`
		ShadowedTokenCount int64   `json:"shadowedTokenCount"`
		Provider           string  `json:"provider"`
		Model              string  `json:"model"`
	}
	if json.Unmarshal(data, &value) != nil {
		return errors.New("dsh compaction summary is invalid")
	}
	proof := h.compactions[value.ID]
	rangeFields, rangeErr := cloudDSHCompactionFields(fields["shadowedRange"], "start", "end")
	if proof == nil || proof.ended || proof.summarySeq != 0 || proof.turn != h.activeTurn || sequence <= proof.startSeq ||
		rangeErr != nil || len(rangeFields) != 2 ||
		value.ShadowedRange.Start < 0 || value.ShadowedRange.Start > value.ShadowedRange.End || value.ShadowedRange.End >= proof.startSeq ||
		value.ShadowedRange.End > 1<<53-1 || len(value.ShadowedSeqs) == 0 || value.ShadowedTokenCount < 1 ||
		value.Provider != "morde" || value.Model == "" || len(value.Model) > 256 || len(value.Summary) == 0 {
		return errors.New("dsh compaction summary identity is invalid")
	}
	if raw := fields["sourceCommandId"]; raw != nil {
		var sourceCommandID string
		if json.Unmarshal(raw, &sourceCommandID) != nil || !cloudDSHID.MatchString(sourceCommandID) || sourceCommandID != proof.sourceCommandID {
			return errors.New("dsh compaction command identity is invalid")
		}
	} else if proof.sourceCommandID != "" {
		return errors.New("dsh compaction command identity is missing")
	}
	var summaryBytes int
	for _, block := range value.Summary {
		if block.Type != "text" || !utf8.ValidString(block.Text) {
			return errors.New("dsh compaction summary content is invalid")
		}
		summaryBytes += len(block.Text)
	}
	if summaryBytes == 0 || summaryBytes > 256<<10 {
		return errors.New("dsh compaction summary exceeds its limit")
	}
	if maxTokens := fields["maxTokens"]; maxTokens != nil {
		var value int64
		if json.Unmarshal(maxTokens, &value) != nil || value < 1 || value > 32768 {
			return errors.New("dsh compaction output limit is invalid")
		}
	}
	shadowed := make(map[int64]struct{}, len(value.ShadowedSeqs))
	for index, sequence := range value.ShadowedSeqs {
		if sequence < value.ShadowedRange.Start || sequence > value.ShadowedRange.End || sequence > 1<<53-1 ||
			index > 0 && sequence <= value.ShadowedSeqs[index-1] {
			return errors.New("dsh compaction source range is invalid")
		}
		if _, active := h.surfaceNodes[sequence]; !active {
			return errors.New("dsh compaction source node is unavailable")
		}
		shadowed[sequence] = struct{}{}
	}
	for sequence := range h.surfaceNodes {
		if sequence >= value.ShadowedRange.Start && sequence <= value.ShadowedRange.End {
			if _, included := shadowed[sequence]; !included {
				return errors.New("dsh compaction source range is incomplete")
			}
		}
	}
	proof.summarySeq, proof.start, proof.end = sequence, value.ShadowedRange.Start, value.ShadowedRange.End
	proof.shadowedSeqs = append([]int64(nil), value.ShadowedSeqs...)
	proof.summaryText = make([]string, len(value.Summary))
	for index, block := range value.Summary {
		proof.summaryText[index] = block.Text
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) replaceCompactedUserNode(sequence int64, data, raw json.RawMessage, sources []int64) error {
	var operation struct {
		Op    string          `json:"op"`
		Start json.RawMessage `json:"startSeq"`
		End   json.RawMessage `json:"endSeq"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var start, end int64
	if decoder.Decode(&operation) != nil || operation.Op != "replace" || len(operation.Start) == 0 || len(operation.End) == 0 ||
		string(operation.Start) == "null" || string(operation.End) == "null" || string(operation.Start) == "-0" || string(operation.End) == "-0" ||
		json.Unmarshal(operation.Start, &start) != nil || json.Unmarshal(operation.End, &end) != nil || start < 0 || start > end ||
		start > 1<<53-1 || end > 1<<53-1 || end >= sequence || !cloudDSHUserMessage(data) {
		return errors.New("dsh compacted user replacement is invalid")
	}
	var message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source json.RawMessage `json:"source"`
	}
	if json.Unmarshal(cloudDSHUserMessagePayload(data), &message) != nil {
		return errors.New("dsh compacted user message is invalid")
	}
	var sourceFields map[string]json.RawMessage
	if json.Unmarshal(message.Source, &sourceFields) != nil || sourceFields == nil || len(sourceFields) < 2 || len(sourceFields) > 3 {
		return errors.New("dsh compacted user source is invalid")
	}
	for key := range sourceFields {
		if key != "kind" && key != "compactionId" && key != "sourceCommandId" {
			return errors.New("dsh compacted user source has an unexpected field")
		}
	}
	var kind, compactionID string
	if json.Unmarshal(sourceFields["kind"], &kind) != nil || kind != "compact-checkpoint" ||
		json.Unmarshal(sourceFields["compactionId"], &compactionID) != nil {
		return errors.New("dsh compacted user source identity is invalid")
	}
	proof := h.compactions[compactionID]
	if proof == nil || proof.ended || proof.summarySeq == 0 || proof.replacementSeq != 0 || proof.start != start || proof.end != end ||
		proof.summarySeq >= sequence || proof.startSeq >= proof.summarySeq || len(sources) != 2+len(proof.shadowedSeqs) ||
		sources[0] != proof.startSeq || sources[1] != proof.summarySeq {
		return errors.New("dsh compacted user source does not match a summary")
	}
	for index, source := range proof.shadowedSeqs {
		if sources[index+2] != source {
			return errors.New("dsh compacted user source range does not match its summary")
		}
	}
	if len(message.Content) != len(proof.summaryText)+2 || message.Content[0].Type != "text" ||
		message.Content[0].Text != cloudDSHCompactionPreamble+"\n\n<compacted-summary>" ||
		message.Content[len(message.Content)-1].Type != "text" ||
		message.Content[len(message.Content)-1].Text != "</compacted-summary>" {
		return errors.New("dsh compacted user framing is invalid")
	}
	for index, text := range proof.summaryText {
		if message.Content[index+1].Type != "text" || message.Content[index+1].Text != text {
			return errors.New("dsh compacted user summary does not match its proof")
		}
	}
	if sourceCommandID := sourceFields["sourceCommandId"]; sourceCommandID != nil {
		var value string
		if json.Unmarshal(sourceCommandID, &value) != nil || !cloudDSHID.MatchString(value) || value != proof.sourceCommandID {
			return errors.New("dsh compacted user command identity is invalid")
		}
	} else if proof.sourceCommandID != "" {
		return errors.New("dsh compacted user command identity is missing")
	}
	for _, call := range h.calls {
		assistantInRange := call.assistantSeq >= start && call.assistantSeq <= end
		callInRange := call.callSeq >= start && call.callSeq <= end
		resultInRange := call.surfaceResultSeq >= start && call.surfaceResultSeq <= end
		if assistantInRange != callInRange || callInRange != resultInRange || callInRange && (!call.callSeen || !call.resultSeen) {
			return errors.New("dsh compaction range splits a tool call")
		}
	}
	for _, shadowedSequence := range proof.shadowedSeqs {
		delete(h.surfaceNodes, shadowedSequence)
		delete(h.systemNodes, shadowedSequence)
	}
	h.surfaceNodes[sequence] = struct{}{}
	proof.replacementSeq = sequence
	return nil
}

func (h *cloudDSHCheckpointHistory) endCompaction(data json.RawMessage) error {
	fields, err := cloudDSHCompactionFields(data, "compactionId", "sourceCommandId", "turn", "error")
	if err != nil {
		return err
	}
	var value struct {
		ID   string `json:"compactionId"`
		Turn int64  `json:"turn"`
	}
	if json.Unmarshal(data, &value) != nil {
		return errors.New("dsh compaction end is invalid")
	}
	proof := h.compactions[value.ID]
	errorValue := fields["error"]
	hasError := errorValue != nil && string(errorValue) != "null"
	if errorValue != nil && !hasError || proof == nil || proof.ended || proof.turn != value.Turn ||
		(proof.summarySeq > 0 && proof.replacementSeq == 0 && !hasError) || (proof.summarySeq == 0 && !hasError) {
		return errors.New("dsh compaction end does not match its transaction")
	}
	if raw := fields["sourceCommandId"]; raw != nil {
		var sourceCommandID string
		if json.Unmarshal(raw, &sourceCommandID) != nil || !cloudDSHID.MatchString(sourceCommandID) || sourceCommandID != proof.sourceCommandID {
			return errors.New("dsh compaction end command identity is invalid")
		}
	} else if proof.sourceCommandID != "" {
		return errors.New("dsh compaction end command identity is missing")
	}
	proof.ended = true
	return nil
}

func cloudDSHCompactionFields(data json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return nil, errors.New("dsh compaction event is invalid")
	}
	for key := range fields {
		valid := false
		for _, candidate := range allowed {
			if key == candidate {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("dsh compaction event has an unexpected field")
		}
	}
	return fields, nil
}

const cloudDSHCompactionPreamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation to free up context. Treat the captured context as established background and build on it without restating it. Continue the task directly from the messages that follow, without acknowledging this checkpoint."

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
		call := &cloudDSHToolCall{name: block.Name, arguments: canonical, turn: event.Turn, step: event.Step,
			callSeq: -1, resultSeq: -1, surfaceResultSeq: -1, assistantSeq: sequence, historical: h.restoring}
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
	h.resultTurn = event.Turn
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

func (h *cloudDSHCheckpointHistory) beginToolResultPrune(sequence int64, data json.RawMessage) error {
	fields, err := cloudDSHCompactionFields(data, "shadowedRange", "shadowedSeqs", "shadowedTokenCount")
	if err != nil || len(fields) != 3 || h.activeTurn < 1 || h.pendingPrune != nil {
		return errors.New("dsh tool-result prune checkpoint is invalid")
	}
	var event struct {
		Range struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"shadowedRange"`
		Seqs       []int64 `json:"shadowedSeqs"`
		TokenCount int64   `json:"shadowedTokenCount"`
	}
	rangeFields, rangeErr := cloudDSHCompactionFields(fields["shadowedRange"], "start", "end")
	if json.Unmarshal(data, &event) != nil || rangeErr != nil || len(rangeFields) != 2 ||
		event.Range.Start < 0 || event.Range.Start != event.Range.End || event.Range.End >= sequence || event.Range.End > 1<<53-1 ||
		len(event.Seqs) != 1 || event.Seqs[0] != event.Range.Start || event.TokenCount < 1 {
		return errors.New("dsh tool-result prune range is invalid")
	}
	if _, active := h.surfaceNodes[event.Range.Start]; !active {
		return errors.New("dsh tool-result prune source is not active")
	}
	for _, call := range h.calls {
		if call.resultSeen && call.surfaceResultSeq == event.Range.Start {
			h.pendingPrune = &cloudDSHToolResultPruneProof{sequence: sequence, resultSeq: event.Range.Start, tokenCount: event.TokenCount}
			return nil
		}
	}
	return errors.New("dsh tool-result prune source is not a known tool result")
}

func (h *cloudDSHCheckpointHistory) appendToolResult(sequence int64, data json.RawMessage, surfaceOp string, sourceSeqs []int64) error {
	var event cloudDSHToolResultCheckpoint
	if json.Unmarshal(data, &event) != nil || event.Turn < 1 || event.Step < 1 || event.Message.ToolCallID == "" ||
		event.Message.ToolCallID != event.Message.Source.CallID || event.Message.IsError || len(event.Error) != 0 && string(event.Error) != "null" ||
		surfaceOp != "append" || len(sourceSeqs) != 1 || sourceSeqs[0] >= sequence {
		return errors.New("dsh tool result checkpoint is invalid")
	}
	call := h.calls[event.Message.ToolCallID]
	if call == nil || !call.callSeen || call.resultSeen || sourceSeqs[0] != call.callSeq || call.turn != event.Turn || call.step != event.Step ||
		len(event.Message.Content) < 1 || len(event.Message.Content) > 1+cloudDSHImageCountLimitForTool(call.name) {
		return errors.New("dsh tool result does not match its durable tool call")
	}
	imageRefs, output, err := cloudDSHToolResultContent(event.Message.Content)
	if err != nil || !utf8.ValidString(output) || len(output) > 64<<10 {
		return errors.New("dsh tool result checkpoint exceeds its limit")
	}
	if call.status != "" && !sameCloudDSHImageRefs(call.runtimeImages, imageRefs) {
		return errors.New("dsh tool result image differs from the reconciled Host result")
	}
	if call.status != "" && cloudDSHToolResultText(call.status, call.output) != output {
		return errors.New("dsh tool result differs from the reconciled runtime result")
	}
	call.resultText, call.resultImages, call.resultSeen, call.resultSeq = output, imageRefs, true, sequence
	call.surfaceResultSeq = sequence
	call.surfaceResultData = append(json.RawMessage(nil), data...)
	if call.status != "" {
		call.reconciled = true
		h.recordFileWriteOutput(event.Message.ToolCallID, output)
	}
	return nil
}

func (h *cloudDSHCheckpointHistory) replacePrunedToolResult(sequence int64, data, rawSurfaceOp json.RawMessage, sourceSeqs []int64) error {
	proof := h.pendingPrune
	var operation struct {
		Op    string          `json:"op"`
		Start json.RawMessage `json:"startSeq"`
		End   json.RawMessage `json:"endSeq"`
	}
	decoder := json.NewDecoder(bytes.NewReader(rawSurfaceOp))
	decoder.DisallowUnknownFields()
	var start, end int64
	if proof == nil || proof.sequence != sequence-1 || decoder.Decode(&operation) != nil || operation.Op != "replace" ||
		len(operation.Start) == 0 || len(operation.End) == 0 || string(operation.Start) == "null" || string(operation.End) == "null" ||
		string(operation.Start) == "-0" || string(operation.End) == "-0" || json.Unmarshal(operation.Start, &start) != nil ||
		json.Unmarshal(operation.End, &end) != nil || start != proof.resultSeq || end != proof.resultSeq ||
		len(sourceSeqs) != 1 || sourceSeqs[0] != proof.resultSeq || proof.tokenCount < 1 || proof.tokenCount > 64<<10 {
		return errors.New("dsh tool-result prune replacement is invalid")
	}
	var call *cloudDSHToolCall
	for _, candidate := range h.calls {
		if candidate.resultSeen && candidate.surfaceResultSeq == proof.resultSeq {
			call = candidate
			break
		}
	}
	if call == nil || !cloudDSHToolResultDataWithoutContentEqual(call.surfaceResultData, data) {
		return errors.New("dsh tool-result prune changed unrelated result data")
	}
	var original, replacement cloudDSHToolResultCheckpoint
	if json.Unmarshal(call.surfaceResultData, &original) != nil || json.Unmarshal(data, &replacement) != nil ||
		original.Turn != replacement.Turn || original.Step != replacement.Step || original.Message.ToolCallID != replacement.Message.ToolCallID ||
		original.Message.Source.CallID != replacement.Message.Source.CallID || original.Message.IsError || replacement.Message.IsError ||
		len(replacement.Error) != 0 && string(replacement.Error) != "null" {
		return errors.New("dsh tool-result prune result identity changed")
	}
	originalImages, originalText, originalErr := cloudDSHToolResultContent(original.Message.Content)
	replacementImages, replacementText, replacementErr := cloudDSHToolResultContent(replacement.Message.Content)
	if originalErr != nil || replacementErr != nil || utf8.RuneCountInString(originalText) <= cloudDSHToolResultPruneThresholdChars ||
		replacementText != cloudDSHPrunedToolResultText(originalText) || !sameCloudDSHImageRefs(originalImages, replacementImages) ||
		!cloudDSHToolResultTextBlockMetadataEqual(original.Message.Content[0], replacement.Message.Content[0]) ||
		len(original.Message.Content) != len(replacement.Message.Content) {
		return errors.New("dsh tool-result prune content is invalid")
	}
	for index := 1; index < len(original.Message.Content); index++ {
		if !bytes.Equal(canonicalCloudDSHJSONMust(original.Message.Content[index]), canonicalCloudDSHJSONMust(replacement.Message.Content[index])) {
			return errors.New("dsh tool-result prune changed a rich content block")
		}
	}
	if _, active := h.surfaceNodes[proof.resultSeq]; !active {
		return errors.New("dsh tool-result prune source is no longer active")
	}
	delete(h.surfaceNodes, proof.resultSeq)
	h.surfaceNodes[sequence] = struct{}{}
	call.surfaceResultSeq = sequence
	call.surfaceResultData = append(json.RawMessage(nil), data...)
	h.pendingPrune = nil
	return nil
}

func cloudDSHToolResultContent(content []json.RawMessage) ([]cloudDSHImageRef, string, error) {
	if len(content) == 0 {
		return nil, "", errors.New("dsh tool result content is empty")
	}
	var text string
	var images []cloudDSHImageRef
	for index, raw := range content {
		var block cloudDSHToolResultContentBlock
		if json.Unmarshal(raw, &block) != nil {
			return nil, "", errors.New("dsh tool result content block is invalid")
		}
		if index == 0 {
			if block.Type != "text" || !utf8.ValidString(block.Text) {
				return nil, "", errors.New("dsh tool result text is invalid")
			}
			text = block.Text
			continue
		}
		if block.Type != "image" {
			return nil, "", errors.New("dsh tool result contains an unauthorized image")
		}
		ref, err := decodeCloudDSHImageRef(block.Attachment)
		if err != nil {
			return nil, "", err
		}
		images = append(images, ref)
	}
	return images, text, nil
}

func cloudDSHPrunedToolResultText(text string) string {
	runes := []rune(text)
	return string(runes[:cloudDSHToolResultPruneHeadChars]) + cloudDSHToolResultPruneMarker +
		string(runes[len(runes)-cloudDSHToolResultPruneTailChars:])
}

func cloudDSHToolResultTextBlockMetadataEqual(original, replacement json.RawMessage) bool {
	var oldBlock, newBlock map[string]json.RawMessage
	if json.Unmarshal(original, &oldBlock) != nil || json.Unmarshal(replacement, &newBlock) != nil {
		return false
	}
	delete(oldBlock, "text")
	delete(newBlock, "text")
	return bytes.Equal(canonicalCloudDSHJSONMustMap(oldBlock), canonicalCloudDSHJSONMustMap(newBlock))
}

func cloudDSHToolResultDataWithoutContentEqual(original, replacement json.RawMessage) bool {
	var oldData, newData map[string]json.RawMessage
	if json.Unmarshal(original, &oldData) != nil || json.Unmarshal(replacement, &newData) != nil || len(oldData) != len(newData) {
		return false
	}
	var oldMessage, newMessage map[string]json.RawMessage
	if json.Unmarshal(oldData["message"], &oldMessage) != nil || json.Unmarshal(newData["message"], &newMessage) != nil ||
		len(oldMessage) != len(newMessage) {
		return false
	}
	delete(oldMessage, "content")
	delete(newMessage, "content")
	oldData["message"], _ = json.Marshal(oldMessage)
	newData["message"], _ = json.Marshal(newMessage)
	return bytes.Equal(canonicalCloudDSHJSONMustMap(oldData), canonicalCloudDSHJSONMustMap(newData))
}

func canonicalCloudDSHJSONMust(raw []byte) []byte {
	canonical, _ := canonicalCloudDSHJSON(raw)
	return canonical
}

func canonicalCloudDSHJSONMustMap(value map[string]json.RawMessage) []byte {
	encoded, _ := json.Marshal(value)
	return canonicalCloudDSHJSONMust(encoded)
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

func (h *cloudDSHCheckpointHistory) hasCompletedResult() bool {
	return h != nil && h.lastTurnCompleted && h.completedTurn > 0 && h.resultTurn == h.completedTurn && strings.TrimSpace(h.result) != ""
}

func (h *cloudDSHCheckpointHistory) completion(fence app.AgentInstanceFence, through int64, digest []byte) (app.CloudDSHTaskCompletion, error) {
	if h == nil || h.sessionID != fence.AgentSessionID || h.nextSeq != through+1 || through < 0 || len(digest) != 32 ||
		!h.hasCompletedResult() {
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
