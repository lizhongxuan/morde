package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type cloudDSHCoordinatorIdentityWire struct {
	Role                 string `json:"context_role"`
	ConversationID       string `json:"conversation_id"`
	MessageID            string `json:"message_id"`
	OwnerID              string `json:"owner_id"`
	ActorID              string `json:"actor_id"`
	AgentID              string `json:"agent_id"`
	Generation           int64  `json:"generation"`
	AuthorizationVersion int64  `json:"authorization_version"`
	SessionID            string `json:"session_id"`
	Purpose              string `json:"purpose"`
	OperationID          string `json:"operation_id"`
}

type cloudDSHCoordinatorFrame struct {
	Type                 string          `json:"type"`
	Role                 string          `json:"context_role"`
	ConversationID       string          `json:"conversation_id"`
	MessageID            string          `json:"message_id"`
	OwnerID              string          `json:"owner_id"`
	ActorID              string          `json:"actor_id"`
	AgentID              string          `json:"agent_id"`
	Generation           int64           `json:"generation"`
	AuthorizationVersion int64           `json:"authorization_version"`
	SessionID            string          `json:"session_id"`
	Purpose              string          `json:"purpose"`
	OperationID          string          `json:"operation_id"`
	ID                   string          `json:"id,omitempty"`
	From                 *int64          `json:"from_seq,omitempty"`
	Through              *int64          `json:"through_seq,omitempty"`
	Payload              string          `json:"payload,omitempty"`
	SHA256               string          `json:"sha256,omitempty"`
	Request              json.RawMessage `json:"request,omitempty"`
	State                string          `json:"state,omitempty"`
	Result               string          `json:"result,omitempty"`
	Code                 string          `json:"code,omitempty"`
	Reason               string          `json:"reason,omitempty"`
	ProcessExited        bool            `json:"process_exited,omitempty"`
}

const cloudDSHTaskValidationReviewRole = "task_validation_review"

type cloudDSHTaskValidationReviewIdentityWire struct {
	ContextRole         string `json:"context_role"`
	OwnerID             string `json:"owner_id"`
	ActorID             string `json:"actor_id"`
	TaskID              string `json:"task_id"`
	TaskRevision        int64  `json:"task_revision"`
	ResultEventSequence int64  `json:"result_event_sequence"`
	ResultSHA256        string `json:"result_sha256"`
	ClaimGeneration     int64  `json:"claim_generation"`
	LeaseOwner          string `json:"lease_owner"`
	SessionID           string `json:"session_id"`
	Purpose             string `json:"purpose"`
}

type cloudDSHTaskValidationReviewFrame struct {
	Type                string          `json:"type"`
	ContextRole         string          `json:"context_role"`
	OwnerID             string          `json:"owner_id"`
	ActorID             string          `json:"actor_id"`
	TaskID              string          `json:"task_id"`
	TaskRevision        int64           `json:"task_revision"`
	ResultEventSequence int64           `json:"result_event_sequence"`
	ResultSHA256        string          `json:"result_sha256"`
	ClaimGeneration     int64           `json:"claim_generation"`
	LeaseOwner          string          `json:"lease_owner"`
	SessionID           string          `json:"session_id"`
	Purpose             string          `json:"purpose"`
	ID                  string          `json:"id,omitempty"`
	FromSequence        int64           `json:"from_seq,omitempty"`
	ThroughSequence     int64           `json:"through_seq,omitempty"`
	Payload             string          `json:"payload,omitempty"`
	SHA256              string          `json:"sha256,omitempty"`
	Request             json.RawMessage `json:"request,omitempty"`
	Result              string          `json:"result,omitempty"`
	Code                string          `json:"code,omitempty"`
	Reason              string          `json:"reason,omitempty"`
	State               string          `json:"state,omitempty"`
	ProcessExited       bool            `json:"process_exited,omitempty"`
	Index               int64           `json:"index,omitempty"`
	Chunk               json.RawMessage `json:"chunk,omitempty"`
	OK                  bool            `json:"ok,omitempty"`
	FailureCode         string          `json:"failure_code,omitempty"`
}

type cloudDSHTaskValidationReviewStart struct {
	Type    string `json:"type"`
	Version int64  `json:"version"`
	cloudDSHTaskValidationReviewIdentityWire
	Model        string `json:"model"`
	Tools        []any  `json:"tools"`
	SystemPrompt string `json:"system_prompt"`
	Prompt       string `json:"prompt"`
	MaxTokens    *int64 `json:"max_tokens,omitempty"`
}

type cloudDSHTaskValidationReview struct {
	ctx           context.Context
	session       *cloudDSHAccountRuntimeSession
	fence         app.TaskValidationReviewModelFence
	identity      cloudDSHTaskValidationReviewIdentityWire
	sequence      int64
	events        chan cloudDSHRead
	startedResult chan error
	finished      chan struct{}
	slots         chan struct{}
	mu            sync.Mutex
	startOnce     sync.Once
	finishOnce    sync.Once
	slotOnce      sync.Once
	admitted      bool
	started       bool
	terminal      bool
	failure       error
}

func cloudDSHTaskValidationReviewIdentityFields() []string {
	return []string{"context_role", "owner_id", "actor_id", "task_id", "task_revision", "result_event_sequence",
		"result_sha256", "claim_generation", "lease_owner", "session_id", "purpose"}
}

func cloudDSHTaskValidationReviewIdentityForFence(fence app.TaskValidationReviewModelFence) cloudDSHTaskValidationReviewIdentityWire {
	claim := fence.Claim
	return cloudDSHTaskValidationReviewIdentityWire{ContextRole: cloudDSHTaskValidationReviewRole, OwnerID: claim.OwnerID,
		ActorID: fence.InitiatorID, TaskID: claim.TaskID, TaskRevision: claim.TaskRevision,
		ResultEventSequence: claim.ResultEventSequence, ResultSHA256: hex.EncodeToString(claim.ResultSHA256),
		ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner, SessionID: fence.SessionID,
		Purpose: cloudDSHTaskValidationReviewRole}
}

func (identity cloudDSHTaskValidationReviewIdentityWire) valid() bool {
	return identity.ContextRole == cloudDSHTaskValidationReviewRole && identity.Purpose == cloudDSHTaskValidationReviewRole &&
		cloudDSHID.MatchString(identity.OwnerID) && cloudDSHID.MatchString(identity.ActorID) && cloudDSHID.MatchString(identity.TaskID) &&
		cloudDSHID.MatchString(identity.SessionID) && identity.TaskRevision >= 0 && identity.TaskRevision <= 1<<53-1 && identity.ResultEventSequence > 0 &&
		identity.ResultEventSequence <= 1<<53-1 && validCloudDSHReviewDigest(identity.ResultSHA256) &&
		identity.ClaimGeneration > 0 && identity.ClaimGeneration <= 1<<53-1 && identity.LeaseOwner != "" &&
		identity.LeaseOwner == strings.TrimSpace(identity.LeaseOwner) && utf8.ValidString(identity.LeaseOwner) &&
		len(identity.LeaseOwner) <= 128 && !strings.ContainsRune(identity.LeaseOwner, '\x00')
}

func validCloudDSHReviewDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func validCloudDSHTaskValidationReviewModelFence(fence app.TaskValidationReviewModelFence) bool {
	return app.ValidTaskValidationReviewModelFence(fence) && fence.InitiatorID == fence.Claim.InitiatorID &&
		cloudDSHTaskValidationReviewIdentityForFence(fence).valid()
}

// StartTaskValidationReview binds the review transport to the account Runtime
// for the claimed task's owner and conversation before starting a tool-free DSH
// review turn.
func (m *CloudDSHAccountRuntimeManager) StartTaskValidationReview(ctx context.Context,
	fence app.TaskValidationReviewModelFence, start map[string]any) (*cloudDSHTaskValidationReview, error) {
	if m == nil || ctx == nil || !validCloudDSHTaskValidationReviewModelFence(fence) {
		return nil, errors.New("Task validation review identity is invalid")
	}
	session, err := m.Acquire(ctx, fence.Claim.OwnerID, fence.Claim.ConversationID)
	if err != nil {
		return nil, err
	}
	return session.StartTaskValidationReview(ctx, fence, start)
}

func cloudDSHTaskValidationReviewIdentityMatches(identity cloudDSHTaskValidationReviewIdentityWire,
	fence app.TaskValidationReviewModelFence) bool {
	return identity == cloudDSHTaskValidationReviewIdentityForFence(fence)
}

func decodeCloudDSHTaskValidationReviewIdentity(frame map[string]json.RawMessage) (cloudDSHTaskValidationReviewIdentityWire, error) {
	var identity cloudDSHTaskValidationReviewIdentityWire
	identityFrame := make(map[string]json.RawMessage, len(cloudDSHTaskValidationReviewIdentityFields()))
	for _, key := range cloudDSHTaskValidationReviewIdentityFields() {
		if value, ok := frame[key]; ok {
			identityFrame[key] = value
		}
	}
	encoded, err := json.Marshal(identityFrame)
	if err != nil {
		return cloudDSHTaskValidationReviewIdentityWire{}, errors.New("Task validation review identity is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&identity) != nil || decoder.Decode(new(any)) != io.EOF || !identity.valid() {
		return cloudDSHTaskValidationReviewIdentityWire{}, errors.New("Task validation review identity is invalid")
	}
	return identity, nil
}

func cloudDSHTaskValidationReviewBaseKeys() []string {
	return append([]string{"type"}, cloudDSHTaskValidationReviewIdentityFields()...)
}

func cloudDSHReviewID(value string) bool {
	return cloudDSHID.MatchString(value)
}

func cloudDSHReviewSequence(value int64) bool {
	return value >= 0 && value <= 1<<53-1
}

func cloudDSHReviewText(value string, maxBytes int) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes
}

func validCloudDSHTaskValidationReviewStart(raw []byte, fence app.TaskValidationReviewModelFence) bool {
	if len(raw) == 0 || len(raw) > cloudDSHMaxFrame || !utf8.Valid(raw) || !uniqueCloudDSHJSON(raw) {
		return false
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return false
	}
	expected := append([]string{"type", "version"}, cloudDSHTaskValidationReviewIdentityFields()...)
	expected = append(expected, "model", "tools", "system_prompt", "prompt")
	if _, ok := keys["max_tokens"]; ok {
		expected = append(expected, "max_tokens")
	}
	if !cloudDSHExactFrameKeys(keys, expected) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var start cloudDSHTaskValidationReviewStart
	if decoder.Decode(&start) != nil || decoder.Decode(new(any)) != io.EOF || start.Type != "task_validation_review_start" ||
		start.Version != 1 || !cloudDSHTaskValidationReviewIdentityMatches(start.cloudDSHTaskValidationReviewIdentityWire, fence) ||
		!validCloudDSHReviewModelRoute(start.Model) || start.Tools == nil || len(start.Tools) != 0 ||
		!validCloudDSHTaskValidationReviewPrompt(start.SystemPrompt, 64<<10) ||
		!validCloudDSHTaskValidationReviewPrompt(start.Prompt, cloudDSHMaxFrame/2) ||
		(start.MaxTokens != nil && (*start.MaxTokens < 1 || *start.MaxTokens > 1_000_000)) {
		return false
	}
	return true
}

func validCloudDSHReviewModelRoute(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && len(value) <= 256 &&
		!strings.ContainsRune(value, '\x00')
}

func validCloudDSHTaskValidationReviewPrompt(value string, maxBytes int) bool {
	return strings.TrimSpace(value) != "" && cloudDSHReviewText(value, maxBytes) &&
		!strings.ContainsRune(value, '\x00')
}

func validCloudDSHTaskValidationReviewModelRequest(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 1<<20 || !uniqueCloudDSHJSON(raw) {
		return false
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return false
	}
	var tools []json.RawMessage
	return json.Unmarshal(request["tools"], &tools) == nil && tools != nil && len(tools) == 0
}

func validCloudDSHTaskValidationReviewEventFrame(kind string, frame map[string]json.RawMessage) bool {
	base := cloudDSHTaskValidationReviewBaseKeys()
	identity, err := decodeCloudDSHTaskValidationReviewIdentity(frame)
	if err != nil {
		return false
	}
	var value cloudDSHTaskValidationReviewFrame
	encoded, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Type != kind ||
		(cloudDSHTaskValidationReviewIdentityWire{ContextRole: value.ContextRole, OwnerID: value.OwnerID, ActorID: value.ActorID,
			TaskID: value.TaskID, TaskRevision: value.TaskRevision, ResultEventSequence: value.ResultEventSequence,
			ResultSHA256: value.ResultSHA256, ClaimGeneration: value.ClaimGeneration, LeaseOwner: value.LeaseOwner,
			SessionID: value.SessionID, Purpose: value.Purpose}) != identity {
		return false
	}
	switch kind {
	case "checkpoint":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq", "through_seq", "payload", "sha256")) &&
			cloudDSHReviewID(value.ID) && cloudDSHReviewSequence(value.FromSequence) &&
			value.ThroughSequence >= value.FromSequence && cloudDSHReviewSequence(value.ThroughSequence) &&
			cloudDSHReviewText(value.Payload, cloudDSHMaxFrame/2) && cloudDSHDigest([]byte(value.Payload), value.SHA256)
	case "checkpoint_read":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq")) &&
			cloudDSHReviewID(value.ID) && cloudDSHReviewSequence(value.FromSequence)
	case "model":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "through_seq", "request", "sha256")) &&
			cloudDSHReviewID(value.ID) && cloudDSHReviewSequence(value.ThroughSequence) &&
			validCloudDSHTaskValidationReviewModelRequest(value.Request) && cloudDSHDigest(value.Request, value.SHA256)
	case "model_cancel":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id")) && cloudDSHReviewID(value.ID)
	case "task_validation_review_settled":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "through_seq", "result", "state")) &&
			cloudDSHReviewSequence(value.ThroughSequence) && value.Result != "" && strings.TrimSpace(value.Result) != "" &&
			cloudDSHReviewText(value.Result, app.MaxTaskValidationReviewReportBytes) && !strings.ContainsRune(value.Result, '\x00') &&
			value.State == "completed"
	case "task_validation_review_cancelled":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "through_seq")) &&
			value.ThroughSequence >= -1 && value.ThroughSequence <= 1<<53-1
	case "task_validation_review_failed":
		if cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code")) {
			return validCloudDSHTaskValidationReviewPrompt(value.Code, 128)
		}
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code", "reason", "process_exited")) &&
			validCloudDSHTaskValidationReviewPrompt(value.Code, 128) && validCloudDSHTaskValidationReviewPrompt(value.Reason, 128) && value.ProcessExited
	case "task_validation_review_terminated":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "reason", "state", "process_exited")) &&
			validCloudDSHTaskValidationReviewPrompt(value.Reason, 128) && value.State == "cancelled" && value.ProcessExited
	default:
		return false
	}
}

func validCloudDSHTaskValidationReviewRuntimeFrame(kind string, frame map[string]json.RawMessage) bool {
	runtimeFields := []string{"server_instance_id", "runtime_id", "runtime_generation", "runtime_sequence"}
	base := append(cloudDSHTaskValidationReviewBaseKeys(), runtimeFields...)
	if kind == "task_validation_review_process_ready" || kind == "task_validation_review_started" {
		identity, err := decodeCloudDSHTaskValidationReviewIdentity(frame)
		if err != nil || !cloudDSHExactFrameKeys(frame, func() []string {
			keys := append([]string(nil), base...)
			if kind == "task_validation_review_process_ready" {
				return append(keys, "container_pid")
			}
			return keys
		}()) {
			return false
		}
		var sequence, pid int64
		if json.Unmarshal(frame["runtime_sequence"], &sequence) != nil || sequence < 1 || sequence > 1<<53-1 {
			return false
		}
		if kind == "task_validation_review_process_ready" {
			return identity.valid() && json.Unmarshal(frame["container_pid"], &pid) == nil && pid > 0
		}
		return identity.valid()
	}
	clean := make(map[string]json.RawMessage, len(frame))
	for key, value := range frame {
		if key != "server_instance_id" && key != "runtime_id" && key != "runtime_generation" && key != "runtime_sequence" {
			clean[key] = value
		}
	}
	if !validCloudDSHTaskValidationReviewEventFrame(kind, clean) {
		return false
	}
	var sequence int64
	if json.Unmarshal(frame["runtime_sequence"], &sequence) != nil || sequence < 1 || sequence > 1<<53-1 ||
		len(frame) != len(clean)+len(runtimeFields) {
		return false
	}
	for _, key := range runtimeFields {
		if _, ok := frame[key]; !ok {
			return false
		}
	}
	return true
}

func validCloudDSHTaskValidationReviewResponseFrame(kind string, frame map[string]json.RawMessage,
	fence app.TaskValidationReviewModelFence) bool {
	identity, err := decodeCloudDSHTaskValidationReviewIdentity(frame)
	if err != nil || !cloudDSHTaskValidationReviewIdentityMatches(identity, fence) {
		return false
	}
	base := cloudDSHTaskValidationReviewBaseKeys()
	var value cloudDSHTaskValidationReviewFrame
	encoded, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Type != kind {
		return false
	}
	switch kind {
	case "task_validation_review_cancel":
		return cloudDSHExactFrameKeys(frame, base)
	case "checkpoint_ack":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "ok", "through_seq", "sha256")) &&
			cloudDSHReviewID(value.ID) && cloudDSHReviewSequence(value.ThroughSequence) && validCloudDSHReviewDigest(value.SHA256)
	case "checkpoint_page":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "ok", "from_seq", "through_seq", "payload", "sha256")) &&
			cloudDSHReviewID(value.ID) && cloudDSHReviewSequence(value.FromSequence) &&
			value.ThroughSequence >= value.FromSequence && cloudDSHReviewSequence(value.ThroughSequence) &&
			cloudDSHReviewText(value.Payload, cloudDSHMaxFrame/2) && cloudDSHDigest([]byte(value.Payload), value.SHA256)
	case "model_chunk":
		if !cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "index", "chunk")) ||
			!cloudDSHReviewID(value.ID) || value.Index < 0 || value.Index > 1<<53-1 || len(value.Chunk) == 0 ||
			!uniqueCloudDSHJSON(value.Chunk) {
			return false
		}
		var chunk map[string]json.RawMessage
		return json.Unmarshal(value.Chunk, &chunk) == nil && chunk != nil
	case "model_end":
		if cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "ok")) {
			return cloudDSHReviewID(value.ID)
		}
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "ok", "failure_code")) &&
			cloudDSHReviewID(value.ID) && !value.OK && value.FailureCode == "CONTEXT_WINDOW_EXCEEDED"
	default:
		return false
	}
}

func (s *cloudDSHAccountRuntimeSession) StartTaskValidationReview(ctx context.Context,
	fence app.TaskValidationReviewModelFence, start map[string]any) (*cloudDSHTaskValidationReview, error) {
	if ctx == nil || !validCloudDSHTaskValidationReviewModelFence(fence) || start == nil ||
		fence.Claim.OwnerID != s.runtime.Fence.OwnerID || fence.Claim.ConversationID != s.runtime.Fence.ConversationID || s.closed() {
		return nil, errors.New("Task validation review AccountRuntime identity is invalid")
	}
	startCopy := make(map[string]any, len(start)+4)
	for key, value := range start {
		startCopy[key] = value
	}
	startRaw, err := json.Marshal(startCopy)
	if err != nil || !validCloudDSHTaskValidationReviewStart(startRaw, fence) {
		return nil, errors.New("Task validation review start frame is invalid")
	}
	s.mu.Lock()
	if s.reviewSlots == nil {
		s.reviewSlots = make(chan struct{}, max(1, s.runtime.MaxConcurrentTasks))
	}
	slots := s.reviewSlots
	s.mu.Unlock()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.reviewStartMu.Lock()
	sequenceLocked := true
	defer func() {
		if sequenceLocked {
			s.reviewStartMu.Unlock()
		}
	}()
	review := &cloudDSHTaskValidationReview{ctx: ctx, session: s, fence: fence,
		identity: cloudDSHTaskValidationReviewIdentityForFence(fence), events: make(chan cloudDSHRead, 32),
		startedResult: make(chan error, 1), finished: make(chan struct{}), slots: slots}
	s.mu.Lock()
	if s.failed != nil || s.stopping || s.reviews[fence.SessionID] != nil {
		s.mu.Unlock()
		review.releaseSlot()
		return nil, errors.New("account Agent Runtime is unavailable for this review session")
	}
	if s.reviewSequence >= 1<<53-1 {
		s.mu.Unlock()
		review.releaseSlot()
		return nil, errors.New("Task validation review sequence is exhausted")
	}
	s.reviewSequence++
	review.sequence = s.reviewSequence
	if s.reviews == nil {
		s.reviews = make(map[string]*cloudDSHTaskValidationReview)
	}
	s.reviews[fence.SessionID] = review
	s.mu.Unlock()
	for key, value := range map[string]any{"server_instance_id": s.runtime.Fence.ServiceInstanceID,
		"runtime_id": s.runtime.Fence.RuntimeID, "runtime_generation": s.runtime.Fence.RuntimeGeneration,
		"runtime_sequence": review.sequence} {
		startCopy[key] = value
	}
	if err := s.input.WriteFrame(ctx, startCopy); err != nil {
		review.fail(errors.New("account Agent Runtime could not start the review session"))
		s.removeTaskValidationReview(review)
		s.reviewStartMu.Unlock()
		sequenceLocked = false
		return nil, errors.New("account Agent Runtime could not start the review session")
	}
	s.reviewStartMu.Unlock()
	sequenceLocked = false
	select {
	case err := <-review.startedResult:
		if err != nil {
			s.removeTaskValidationReview(review)
			return nil, err
		}
		return review, nil
	case <-review.finished:
		err := review.failureError()
		s.removeTaskValidationReview(review)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("Task validation review ended before readiness")
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = review.writeFrame(cancelCtx, map[string]any{"type": "task_validation_review_cancel"})
		cancel()
		return nil, ctx.Err()
	}
}

func (s *cloudDSHAccountRuntimeSession) routeTaskValidationReviewFrame(kind string, frame map[string]json.RawMessage) error {
	if !validCloudDSHTaskValidationReviewRuntimeFrame(kind, frame) {
		return errors.New("account Agent Runtime Task validation review frame fields are invalid")
	}
	identity, err := decodeCloudDSHTaskValidationReviewIdentity(frame)
	if err != nil {
		return err
	}
	var sequence int64
	if json.Unmarshal(frame["runtime_sequence"], &sequence) != nil {
		return errors.New("account Agent Runtime Task validation review sequence is invalid")
	}
	s.mu.Lock()
	review := s.reviews[identity.SessionID]
	if review == nil || !cloudDSHTaskValidationReviewIdentityMatches(identity, review.fence) || sequence != review.sequence {
		s.mu.Unlock()
		return errors.New("account Agent Runtime frame does not match the registered Task validation review")
	}
	s.mu.Unlock()
	if kind == "task_validation_review_process_ready" {
		review.mu.Lock()
		if review.admitted || review.started || review.terminal {
			review.mu.Unlock()
			return errors.New("Task validation review process was admitted twice")
		}
		review.admitted = true
		review.mu.Unlock()
		return s.input.WriteFrame(review.ctx, cloudDSHTaskValidationReviewAccountFrame(s.runtime.Fence, review.sequence,
			review.identity, map[string]any{"type": "task_validation_review_process_admit"}))
	}
	if kind == "task_validation_review_started" {
		review.mu.Lock()
		if !review.admitted || review.started || review.terminal {
			review.mu.Unlock()
			return errors.New("Task validation review start response is invalid")
		}
		review.started = true
		review.mu.Unlock()
		review.startOnce.Do(func() { review.startedResult <- nil })
		return nil
	}
	review.mu.Lock()
	if review.terminal || (kind != "task_validation_review_failed" && kind != "task_validation_review_terminated" && !review.started) {
		review.mu.Unlock()
		return errors.New("Task validation review frame arrived outside its active session")
	}
	review.mu.Unlock()
	for _, key := range []string{"server_instance_id", "runtime_id", "runtime_generation", "runtime_sequence"} {
		delete(frame, key)
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return errors.New("Task validation review frame could not be decoded")
	}
	select {
	case review.events <- cloudDSHRead{frame: encoded}:
	case <-review.ctx.Done():
		return nil
	default:
		review.fail(errors.New("Task validation review event queue is full"))
		s.removeTaskValidationReview(review)
		return nil
	}
	if kind == "task_validation_review_settled" || kind == "task_validation_review_cancelled" ||
		kind == "task_validation_review_failed" || kind == "task_validation_review_terminated" {
		if kind == "task_validation_review_failed" {
			review.startOnce.Do(func() { review.startedResult <- errors.New("Task validation review process failed") })
		}
		review.markTerminal()
		s.removeTaskValidationReview(review)
	}
	return nil
}

func cloudDSHTaskValidationReviewAccountFrame(runtime app.AccountAgentRuntimeFence, sequence int64,
	identity cloudDSHTaskValidationReviewIdentityWire, frame map[string]any) map[string]any {
	result := make(map[string]any, len(frame)+len(cloudDSHTaskValidationReviewIdentityFields())+5)
	for key, value := range frame {
		result[key] = value
	}
	for key, value := range map[string]any{"server_instance_id": runtime.ServiceInstanceID, "owner_id": identity.OwnerID,
		"runtime_id": runtime.RuntimeID, "runtime_generation": runtime.RuntimeGeneration, "runtime_sequence": sequence,
		"context_role": identity.ContextRole, "actor_id": identity.ActorID, "task_id": identity.TaskID,
		"task_revision": identity.TaskRevision, "result_event_sequence": identity.ResultEventSequence,
		"result_sha256": identity.ResultSHA256, "claim_generation": identity.ClaimGeneration,
		"lease_owner": identity.LeaseOwner, "session_id": identity.SessionID, "purpose": identity.Purpose} {
		result[key] = value
	}
	return result
}

func (s *cloudDSHAccountRuntimeSession) removeTaskValidationReview(review *cloudDSHTaskValidationReview) {
	s.mu.Lock()
	if current := s.reviews[review.identity.SessionID]; current == review &&
		cloudDSHTaskValidationReviewIdentityMatches(review.identity, current.fence) {
		delete(s.reviews, review.identity.SessionID)
	}
	s.mu.Unlock()
	review.releaseSlot()
}

func (r *cloudDSHTaskValidationReview) read(ctx context.Context) (cloudDSHTaskValidationReviewFrame, error) {
	if ctx == nil {
		return cloudDSHTaskValidationReviewFrame{}, errors.New("Task validation review read context is required")
	}
	for {
		select {
		case item := <-r.events:
			if item.err != nil {
				return cloudDSHTaskValidationReviewFrame{}, item.err
			}
			frame, err := decodeCloudDSHTaskValidationReviewFrame(item.frame)
			if err != nil || !cloudDSHTaskValidationReviewIdentityMatches(cloudDSHTaskValidationReviewIdentityFromFrame(frame), r.fence) {
				return cloudDSHTaskValidationReviewFrame{}, errors.New("Task validation review frame identity is invalid")
			}
			return frame, nil
		default:
		}
		select {
		case item := <-r.events:
			if item.err != nil {
				return cloudDSHTaskValidationReviewFrame{}, item.err
			}
			frame, err := decodeCloudDSHTaskValidationReviewFrame(item.frame)
			if err != nil || !cloudDSHTaskValidationReviewIdentityMatches(cloudDSHTaskValidationReviewIdentityFromFrame(frame), r.fence) {
				return cloudDSHTaskValidationReviewFrame{}, errors.New("Task validation review frame identity is invalid")
			}
			return frame, nil
		case <-ctx.Done():
			return cloudDSHTaskValidationReviewFrame{}, ctx.Err()
		case <-r.finished:
			r.mu.Lock()
			failure := r.failure
			r.mu.Unlock()
			if failure != nil {
				return cloudDSHTaskValidationReviewFrame{}, failure
			}
			return cloudDSHTaskValidationReviewFrame{}, errors.New("Task validation review ended without a terminal frame")
		}
	}
}

func cloudDSHTaskValidationReviewIdentityFromFrame(frame cloudDSHTaskValidationReviewFrame) cloudDSHTaskValidationReviewIdentityWire {
	return cloudDSHTaskValidationReviewIdentityWire{ContextRole: frame.ContextRole, OwnerID: frame.OwnerID, ActorID: frame.ActorID,
		TaskID: frame.TaskID, TaskRevision: frame.TaskRevision, ResultEventSequence: frame.ResultEventSequence,
		ResultSHA256: frame.ResultSHA256, ClaimGeneration: frame.ClaimGeneration, LeaseOwner: frame.LeaseOwner,
		SessionID: frame.SessionID, Purpose: frame.Purpose}
}

func decodeCloudDSHTaskValidationReviewFrame(raw []byte) (cloudDSHTaskValidationReviewFrame, error) {
	var frame cloudDSHTaskValidationReviewFrame
	if len(raw) == 0 || len(raw) > cloudDSHMaxFrame || !utf8.Valid(raw) || !uniqueCloudDSHJSON(raw) {
		return frame, errors.New("Task validation review JSON frame is invalid")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return frame, errors.New("Task validation review frame is not an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF || !validCloudDSHTaskValidationReviewEventFrame(frame.Type, keys) {
		return cloudDSHTaskValidationReviewFrame{}, errors.New("Task validation review frame fields are invalid")
	}
	return frame, nil
}

func (r *cloudDSHTaskValidationReview) writeFrame(ctx context.Context, frame map[string]any) error {
	if ctx == nil || frame == nil {
		return errors.New("Task validation review frame context and payload are required")
	}
	frame = cloudDSHTaskValidationReviewResponseForIdentity(r.identity, frame)
	encoded, err := json.Marshal(frame)
	if err != nil || len(encoded) > cloudDSHMaxFrame || !utf8.Valid(encoded) || !uniqueCloudDSHJSON(encoded) {
		return errors.New("Task validation review response frame is invalid")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(encoded, &raw) != nil || raw == nil {
		return errors.New("Task validation review response frame is invalid")
	}
	var kind string
	if json.Unmarshal(raw["type"], &kind) != nil || !validCloudDSHTaskValidationReviewResponseFrame(kind, raw, r.fence) {
		return fmt.Errorf("Task validation review response frame %q identity or fields are invalid", kind)
	}
	r.session.mu.Lock()
	registered := r.session.reviews[r.identity.SessionID] == r
	r.session.mu.Unlock()
	if !registered {
		return errors.New("Task validation review session is no longer registered")
	}
	r.mu.Lock()
	terminal, started := r.terminal, r.started
	r.mu.Unlock()
	if terminal || (kind != "task_validation_review_cancel" && !started) {
		return errors.New("Task validation review response is not allowed in its current state")
	}
	return r.session.input.WriteFrame(ctx, cloudDSHTaskValidationReviewAccountFrame(r.session.runtime.Fence,
		r.sequence, r.identity, frame))
}

func cloudDSHTaskValidationReviewResponseForIdentity(identity cloudDSHTaskValidationReviewIdentityWire,
	frame map[string]any) map[string]any {
	result := make(map[string]any, len(frame)+len(cloudDSHTaskValidationReviewIdentityFields()))
	for key, value := range frame {
		result[key] = value
	}
	for key, value := range map[string]any{
		"context_role": identity.ContextRole, "owner_id": identity.OwnerID, "actor_id": identity.ActorID,
		"task_id": identity.TaskID, "task_revision": identity.TaskRevision, "result_event_sequence": identity.ResultEventSequence,
		"result_sha256": identity.ResultSHA256, "claim_generation": identity.ClaimGeneration, "lease_owner": identity.LeaseOwner,
		"session_id": identity.SessionID, "purpose": identity.Purpose,
	} {
		result[key] = value
	}
	return result
}

func (r *cloudDSHTaskValidationReview) stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Task validation review stop context is required")
	}
	r.mu.Lock()
	terminal := r.terminal
	r.mu.Unlock()
	if !terminal {
		if err := r.writeFrame(ctx, map[string]any{"type": "task_validation_review_cancel"}); err != nil {
			return err
		}
		select {
		case <-r.finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.session.removeTaskValidationReview(r)
	return nil
}

func (r *cloudDSHTaskValidationReview) markTerminal() {
	r.mu.Lock()
	if r.terminal {
		r.mu.Unlock()
		return
	}
	r.terminal = true
	r.mu.Unlock()
	r.finishOnce.Do(func() { close(r.finished) })
	r.releaseSlot()
}

func (r *cloudDSHTaskValidationReview) fail(err error) {
	if err == nil {
		err = errors.New("Task validation review AccountRuntime failed")
	}
	r.mu.Lock()
	if r.failure == nil {
		r.failure = err
	}
	r.mu.Unlock()
	r.startOnce.Do(func() { r.startedResult <- err })
	r.markTerminal()
	select {
	case r.events <- cloudDSHRead{err: err}:
	default:
	}
}

func (r *cloudDSHTaskValidationReview) failureError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *cloudDSHTaskValidationReview) releaseSlot() {
	r.slotOnce.Do(func() {
		if r.slots != nil {
			select {
			case <-r.slots:
			default:
			}
		}
	})
}

func cloudDSHCoordinatorFrameBaseKeys() []string {
	return []string{"type", "server_instance_id", "owner_id", "runtime_id", "runtime_generation",
		"context_role", "conversation_id", "message_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"}
}

func cloudDSHExactFrameKeys(frame map[string]json.RawMessage, expected []string) bool {
	if len(frame) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := frame[key]; !ok {
			return false
		}
	}
	return true
}

func validCloudDSHCoordinatorRuntimeFrame(kind string, frame map[string]json.RawMessage) bool {
	base := cloudDSHCoordinatorFrameBaseKeys()
	switch kind {
	case "coordinator_process_ready":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "runtime_sequence", "container_pid"))
	case "coordinator_started":
		return cloudDSHExactFrameKeys(frame, base)
	case "checkpoint":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq", "through_seq", "payload", "sha256"))
	case "checkpoint_read":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq"))
	case "model":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "through_seq", "request", "sha256"))
	case "model_cancel":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id"))
	case "coordinator_settled":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "state", "through_seq", "result"))
	case "coordinator_failed":
		if cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code")) {
			return true
		}
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code", "reason", "process_exited"))
	case "coordinator_terminated":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "reason", "state", "process_exited"))
	default:
		return false
	}
}

func decodeCloudDSHCoordinatorIdentity(frame map[string]json.RawMessage) (app.ConversationCoordinatorIdentity, error) {
	var wire cloudDSHCoordinatorIdentityWire
	identityFrame := make(map[string]json.RawMessage, 11)
	for _, key := range []string{"context_role", "conversation_id", "message_id", "owner_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"} {
		if value, ok := frame[key]; ok {
			identityFrame[key] = value
		}
	}
	encoded, err := json.Marshal(identityFrame)
	if err != nil {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator identity is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.Role != "coordinator" ||
		!cloudDSHID.MatchString(wire.OwnerID) || !cloudDSHID.MatchString(wire.ActorID) || !cloudDSHID.MatchString(wire.AgentID) ||
		!cloudDSHID.MatchString(wire.SessionID) || wire.Generation < 1 || wire.AuthorizationVersion < 1 ||
		(wire.Purpose != "conversation" && wire.Purpose != "compaction") ||
		(wire.Purpose == "conversation" && wire.OperationID != "") ||
		(wire.Purpose == "compaction" && !domain.ValidOperationID(wire.OperationID)) {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator identity is invalid")
	}
	// The service instance ID is inherited from the account Runtime fence, so
	// callers fill it after validating the outer Runtime identity.
	if !domain.ValidConversationID(wire.ConversationID) || !domain.ValidMessageID(wire.MessageID) {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator scope is invalid")
	}
	return app.ConversationCoordinatorIdentity{OwnerID: wire.OwnerID, ActorID: wire.ActorID,
		ConversationID: wire.ConversationID, MessageID: wire.MessageID, AgentID: wire.AgentID,
		Generation: wire.Generation, AuthorizationVersion: wire.AuthorizationVersion, SessionID: wire.SessionID,
		Purpose: wire.Purpose, OperationID: wire.OperationID}, nil
}

func (c *cloudDSHAccountCoordinator) identityMatches(value app.ConversationCoordinatorIdentity) bool {
	return c.identity == value
}

func (c *cloudDSHAccountCoordinator) read(ctx context.Context) (cloudDSHCoordinatorFrame, error) {
	if ctx == nil {
		return cloudDSHCoordinatorFrame{}, errors.New("coordinator read context is required")
	}
	select {
	case <-ctx.Done():
		return cloudDSHCoordinatorFrame{}, ctx.Err()
	case <-c.ctx.Done():
		return cloudDSHCoordinatorFrame{}, c.ctx.Err()
	case item := <-c.events:
		if item.err != nil {
			return cloudDSHCoordinatorFrame{}, item.err
		}
		frame, err := decodeCloudDSHCoordinatorFrame(item.frame)
		frameIdentity := frame.identity()
		frameIdentity.ServiceInstanceID = c.identity.ServiceInstanceID
		if err != nil || !c.identityMatches(frameIdentity) {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame identity is invalid")
		}
		return frame, nil
	}
}

func (f cloudDSHCoordinatorFrame) identity() app.ConversationCoordinatorIdentity {
	return app.ConversationCoordinatorIdentity{ServiceInstanceID: "", OwnerID: f.OwnerID, ActorID: f.ActorID,
		ConversationID: f.ConversationID, MessageID: f.MessageID, AgentID: f.AgentID,
		Generation: f.Generation, AuthorizationVersion: f.AuthorizationVersion, SessionID: f.SessionID,
		Purpose: f.Purpose, OperationID: f.OperationID}
}

func decodeCloudDSHCoordinatorFrame(raw []byte) (cloudDSHCoordinatorFrame, error) {
	var frame cloudDSHCoordinatorFrame
	if !uniqueCloudDSHJSON(raw) || !utf8.Valid(raw) {
		return frame, errors.New("DSH coordinator frame JSON is invalid")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return frame, errors.New("DSH coordinator frame is not an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF || frame.Role != "coordinator" ||
		!cloudDSHID.MatchString(frame.OwnerID) || !cloudDSHID.MatchString(frame.ActorID) || !cloudDSHID.MatchString(frame.AgentID) || !cloudDSHID.MatchString(frame.SessionID) ||
		(frame.Purpose != "conversation" && frame.Purpose != "compaction") ||
		(frame.Purpose == "conversation" && frame.OperationID != "") || (frame.Purpose == "compaction" && !domain.ValidOperationID(frame.OperationID)) ||
		frame.Generation < 1 || frame.AuthorizationVersion < 1 || frame.ConversationID == "" || frame.MessageID == "" {
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame fields are invalid")
	}
	base := []string{"type", "context_role", "conversation_id", "message_id", "owner_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"}
	var expected []string
	switch frame.Type {
	case "checkpoint":
		expected = append(append([]string(nil), base...), "id", "from_seq", "through_seq", "payload", "sha256")
	case "checkpoint_read":
		expected = append(append([]string(nil), base...), "id", "from_seq")
	case "model":
		expected = append(append([]string(nil), base...), "id", "through_seq", "request", "sha256")
	case "model_cancel":
		expected = append(append([]string(nil), base...), "id")
	case "coordinator_settled":
		expected = append(append([]string(nil), base...), "state", "through_seq", "result")
	case "coordinator_failed":
		expected = append(append([]string(nil), base...), "code")
		if !cloudDSHExactFrameKeys(keys, expected) {
			expected = append(append([]string(nil), base...), "code", "reason", "process_exited")
		}
	case "coordinator_terminated":
		expected = append(append([]string(nil), base...), "reason", "state", "process_exited")
	default:
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame has unsupported type")
	}
	if !cloudDSHExactFrameKeys(keys, expected) {
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame has unsupported fields")
	}
	switch frame.Type {
	case "coordinator_settled":
		if frame.State != "completed" {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator settlement state is invalid")
		}
	case "coordinator_failed":
		if frame.Code == "" || len(frame.Code) > 128 || (frame.ProcessExited != (frame.Reason != "")) {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator failure details are invalid")
		}
	case "coordinator_terminated":
		if frame.Reason == "" || len(frame.Reason) > 128 || frame.State != "cancelled" || !frame.ProcessExited {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator termination details are invalid")
		}
	}
	return frame, nil
}

type cloudDSHAccountCoordinator struct {
	ctx           context.Context
	session       *cloudDSHAccountRuntimeSession
	identity      app.ConversationCoordinatorIdentity
	sequence      int64
	events        chan cloudDSHRead
	startedResult chan error
	finished      chan struct{}
	slots         chan struct{}
	mu            sync.Mutex
	startOnce     sync.Once
	finishOnce    sync.Once
	slotOnce      sync.Once
	checkpointIDs map[string]struct{}
	pageReadIDs   map[string]struct{}
	through       int64
	readThrough   int64
	admitted      bool
	started       bool
	terminal      bool
	failure       error
}

func (c *cloudDSHAccountCoordinator) releaseSlot() {
	c.slotOnce.Do(func() {
		if c.slots != nil {
			select {
			case <-c.slots:
			default:
			}
		}
	})
}

func (c *cloudDSHAccountCoordinator) markTerminal() {
	c.mu.Lock()
	if c.terminal {
		c.mu.Unlock()
		return
	}
	c.terminal = true
	c.mu.Unlock()
	c.finishOnce.Do(func() { close(c.finished) })
	c.releaseSlot()
}

func (c *cloudDSHAccountCoordinator) fail(err error) {
	if err == nil {
		err = errors.New("DSH coordinator runtime failed")
	}
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	c.startOnce.Do(func() { c.startedResult <- err })
	c.markTerminal()
	select {
	case c.events <- cloudDSHRead{err: err}:
	default:
	}
}

func (s *cloudDSHAccountRuntimeSession) removeCoordinator(identity app.ConversationCoordinatorIdentity,
	coordinator *cloudDSHAccountCoordinator) error {
	s.mu.Lock()
	if current := s.coordinators[identity.SessionID]; current == coordinator && current.identity == identity {
		delete(s.coordinators, identity.SessionID)
	}
	s.mu.Unlock()
	coordinator.releaseSlot()
	return nil
}

func (m *CloudDSHAccountRuntimeManager) StopCoordinator(ctx context.Context, session *cloudDSHAccountRuntimeSession,
	identity app.ConversationCoordinatorIdentity) error {
	if ctx == nil || session == nil {
		return errors.New("coordinator stop context and runtime are required")
	}
	session.mu.Lock()
	coordinator := session.coordinators[identity.SessionID]
	session.mu.Unlock()
	if coordinator == nil {
		return nil
	}
	if !coordinator.identityMatches(identity) {
		return errors.New("coordinator stop identity does not match")
	}
	coordinator.mu.Lock()
	terminal := coordinator.terminal
	coordinator.mu.Unlock()
	if !terminal {
		if err := session.writeCoordinatorFrame(ctx, identity, map[string]any{"type": "coordinator_cancel",
			"context_role": "coordinator", "conversation_id": identity.ConversationID, "message_id": identity.MessageID,
			"owner_id": identity.OwnerID, "actor_id": identity.ActorID, "agent_id": identity.AgentID, "generation": identity.Generation,
			"authorization_version": identity.AuthorizationVersion, "session_id": identity.SessionID}); err != nil {
			return err
		}
		select {
		case <-coordinator.finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return session.removeCoordinator(identity, coordinator)
}
