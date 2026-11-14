package codex

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const (
	MaxEventBytes      = 64 << 10
	MaxEventBatchBytes = 512 << 10
	MaxEventPageSize   = 500
	MaxSessionPageSize = 100
)

type Scope string

const (
	ScopeRead      Scope = "read"
	ScopeLive      Scope = "live"
	ScopeSend      Scope = "send"
	ScopeInterrupt Scope = "interrupt"
	ScopeApproval  Scope = "approval"
)

var allScopes = map[Scope]struct{}{
	ScopeRead: {}, ScopeLive: {}, ScopeSend: {}, ScopeInterrupt: {}, ScopeApproval: {},
}

func ValidScope(scope Scope) bool { _, ok := allScopes[scope]; return ok }

type SessionStatus string

const (
	SessionIdle            SessionStatus = "idle"
	SessionInProgress      SessionStatus = "in_progress"
	SessionWaitingApproval SessionStatus = "waiting_approval"
	SessionCompleted       SessionStatus = "completed"
	SessionFailed          SessionStatus = "failed"
	SessionUnknown         SessionStatus = "unknown"
)

func (status SessionStatus) Valid() bool {
	switch status {
	case SessionIdle, SessionInProgress, SessionWaitingApproval, SessionCompleted, SessionFailed, SessionUnknown:
		return true
	default:
		return false
	}
}

type Session struct {
	ID                string        `json:"id"`
	OwnerID           string        `json:"-"`
	InstallationID    string        `json:"installation_id"`
	ProfileKey        string        `json:"profile_key"`
	ThreadID          string        `json:"thread_id"`
	Title             string        `json:"title"`
	CWDLabel          string        `json:"cwd_label"`
	ProviderUpdatedAt *time.Time    `json:"updated_at"`
	Status            SessionStatus `json:"status"`
	Scopes            []Scope       `json:"scopes"`
	GrantVersion      int64         `json:"grant_version"`
	LastSequence      int64         `json:"last_sequence"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"-"`
}

type Discover struct {
	ProfileKey string        `json:"profile_key"`
	ThreadID   string        `json:"thread_id"`
	Title      string        `json:"title"`
	CWDLabel   string        `json:"cwd_label"`
	UpdatedAt  *time.Time    `json:"updated_at"`
	Status     SessionStatus `json:"status"`
	Scopes     []Scope       `json:"scopes"`
}

// ProfileConnectionRequest declares the exact scopes granted to one Codex
// profile on the authenticated desktop installation.
type ProfileConnectionRequest struct {
	ProfileKey string  `json:"profile_key"`
	Scopes     []Scope `json:"scopes"`
}

func (value ProfileConnectionRequest) Validate() error {
	if !ValidProfileKey(value.ProfileKey) || value.Scopes == nil || len(value.Scopes) < 2 || len(value.Scopes) > len(allScopes) {
		return errors.New("Codex profile connection is invalid")
	}
	seen := make(map[Scope]struct{}, len(value.Scopes))
	for _, scope := range value.Scopes {
		if !ValidScope(scope) {
			return errors.New("Codex profile connection scopes are invalid")
		}
		if _, duplicate := seen[scope]; duplicate {
			return errors.New("Codex profile connection scopes are invalid")
		}
		seen[scope] = struct{}{}
	}
	if _, ok := seen[ScopeRead]; !ok {
		return errors.New("Codex profile connection must include the read scope")
	}
	if _, ok := seen[ScopeLive]; !ok {
		return errors.New("Codex profile connection must include the live scope")
	}
	return nil
}

type ProfileConnection struct {
	InstallationID   string                  `json:"installation_id"`
	ProfileKey       string                  `json:"profile_key"`
	Scopes           []Scope                 `json:"scopes"`
	GrantVersion     int64                   `json:"grant_version"`
	Connected        bool                    `json:"connected"`
	UpdatedAt        time.Time               `json:"updated_at"`
	SessionDiscovery *SessionDiscoveryStatus `json:"session_discovery"`
	HistorySync      *HistorySyncProgress    `json:"history_sync"`
}

type DiscoveryState string

const (
	DiscoverySyncing DiscoveryState = "syncing"
	DiscoverySynced  DiscoveryState = "synced"
	DiscoveryPartial DiscoveryState = "partial"
)

func (state DiscoveryState) Valid() bool {
	switch state {
	case DiscoverySyncing, DiscoverySynced, DiscoveryPartial:
		return true
	default:
		return false
	}
}

type DiscoveryErrorCode string

const (
	DiscoveryErrorProfileUnavailable DiscoveryErrorCode = "profile_unavailable"
	DiscoveryErrorSessionListFailed  DiscoveryErrorCode = "session_list_failed"
	DiscoveryErrorConnectionLost     DiscoveryErrorCode = "connection_lost"
	DiscoveryErrorUnknown            DiscoveryErrorCode = "unknown"
)

func (code DiscoveryErrorCode) Valid() bool {
	switch code {
	case DiscoveryErrorProfileUnavailable, DiscoveryErrorSessionListFailed, DiscoveryErrorConnectionLost, DiscoveryErrorUnknown:
		return true
	default:
		return false
	}
}

// ProfileSessionDiscoveryUpdate contains the client-reported profile-level
// discovery state. It does not claim that all historical events were uploaded.
type ProfileSessionDiscoveryUpdate struct {
	SessionDiscovery *SessionDiscoveryUpdate    `json:"session_discovery"`
	HistorySync      *HistorySyncProgressUpdate `json:"history_sync"`
}

type SessionDiscoveryUpdate struct {
	State     DiscoveryState      `json:"state"`
	ErrorCode *DiscoveryErrorCode `json:"error_code"`
}

type SessionDiscoveryStatus struct {
	State     DiscoveryState      `json:"state"`
	ErrorCode *DiscoveryErrorCode `json:"error_code"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// HistorySyncProgress is a count summary reported by the PC. It is separate
// from discovery state and does not assert that every event is present remotely.
type HistorySyncProgress struct {
	DiscoveredSessions      int64 `json:"discovered_sessions"`
	HistoryCompleteSessions int64 `json:"history_complete_sessions"`
	PendingUploadEventCount int64 `json:"pending_upload_event_count"`
}

type HistorySyncProgressUpdate struct {
	DiscoveredSessions      *int64 `json:"discovered_sessions"`
	HistoryCompleteSessions *int64 `json:"history_complete_sessions"`
	PendingUploadEventCount *int64 `json:"pending_upload_event_count"`
}

func (value ProfileSessionDiscoveryUpdate) Validate() error {
	if value.SessionDiscovery == nil || value.HistorySync == nil || value.HistorySync.DiscoveredSessions == nil ||
		value.HistorySync.HistoryCompleteSessions == nil || value.HistorySync.PendingUploadEventCount == nil {
		return errors.New("Codex profile session discovery and history sync fields are required")
	}
	if !value.SessionDiscovery.State.Valid() {
		return errors.New("Codex session discovery state is invalid")
	}
	if value.SessionDiscovery.ErrorCode != nil && !value.SessionDiscovery.ErrorCode.Valid() {
		return errors.New("Codex session discovery error code is invalid")
	}
	if value.SessionDiscovery.State == DiscoverySynced && value.SessionDiscovery.ErrorCode != nil {
		return errors.New("successful Codex session discovery cannot include an error code")
	}
	if value.SessionDiscovery.State == DiscoverySyncing && value.SessionDiscovery.ErrorCode != nil {
		return errors.New("running Codex session discovery cannot include an error code")
	}
	if value.SessionDiscovery.State == DiscoveryPartial && value.SessionDiscovery.ErrorCode == nil {
		return errors.New("partial Codex session discovery requires an error code")
	}
	if *value.HistorySync.DiscoveredSessions < 0 || *value.HistorySync.HistoryCompleteSessions < 0 ||
		*value.HistorySync.PendingUploadEventCount < 0 ||
		*value.HistorySync.HistoryCompleteSessions > *value.HistorySync.DiscoveredSessions {
		return errors.New("Codex history sync counts are invalid")
	}
	return nil
}

var profileKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var nativeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,512}$`)
var questionVersionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidProfileKey(value string) bool {
	return profileKeyPattern.MatchString(value) && !strings.Contains(strings.ToUpper(value), "CODEX_HOME")
}

func (value Discover) Validate() error {
	if !ValidProfileKey(value.ProfileKey) || !nativeIDPattern.MatchString(value.ThreadID) ||
		!validVisibleString(value.Title, 0, 512) || !validVisibleString(value.CWDLabel, 0, 256) ||
		strings.ContainsAny(value.CWDLabel, "/\\") || !value.Status.Valid() {
		return errors.New("Codex thread metadata is invalid")
	}
	if value.UpdatedAt != nil && (value.UpdatedAt.IsZero() || value.UpdatedAt.After(time.Now().UTC().Add(24*time.Hour))) {
		return errors.New("Codex thread update time is invalid")
	}
	if len(value.Scopes) > len(allScopes) {
		return errors.New("Codex grant scopes are invalid")
	}
	seen := make(map[Scope]struct{}, len(value.Scopes))
	for _, scope := range value.Scopes {
		if !ValidScope(scope) {
			return errors.New("Codex grant scopes are invalid")
		}
		if _, duplicate := seen[scope]; duplicate {
			return errors.New("Codex grant scopes are invalid")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

type EventKind string

const (
	EventUserMessage       EventKind = "user_message"
	EventAssistantMessage  EventKind = "assistant_message"
	EventAssistantDelta    EventKind = "assistant_delta"
	EventReasoningSummary  EventKind = "reasoning_summary"
	EventPlan              EventKind = "plan"
	EventTurnStarted       EventKind = "turn_started"
	EventTurnCompleted     EventKind = "turn_completed"
	EventTurnFailed        EventKind = "turn_failed"
	EventTurnCancelled     EventKind = "turn_cancelled"
	EventToolCall          EventKind = "tool_call"
	EventToolResult        EventKind = "tool_result"
	EventToolCallSummary   EventKind = "tool_call_summary"
	EventToolResultSummary EventKind = "tool_result_summary"
	EventCommandSummary    EventKind = "command_summary"
	EventFileChange        EventKind = "file_change"
	EventFileChangeSummary EventKind = "file_change_summary"
	EventChildAgent        EventKind = "child_agent"
	EventSubagentSummary   EventKind = "subagent_summary"
	EventQuestion          EventKind = "question"
	EventApprovalRequest   EventKind = "approval_request"
	EventApprovalStatus    EventKind = "approval_status"
	EventCommandStatus     EventKind = "command_status"
	EventUnsupported       EventKind = "unsupported"
)

var eventPayloadKeys = map[EventKind]map[string]struct{}{
	EventUserMessage:       keys("text", "segment_id"),
	EventAssistantMessage:  keys("text", "segment_id"),
	EventAssistantDelta:    keys("text", "segment_id", "delta_index"),
	EventReasoningSummary:  keys("text"),
	EventPlan:              keys("summary", "items", "explanation"),
	EventTurnStarted:       keys("status"),
	EventTurnCompleted:     keys("status", "summary"),
	EventTurnFailed:        keys("status", "error_code", "summary"),
	EventTurnCancelled:     keys("status", "summary"),
	EventToolCall:          keys("tool", "summary", "call_id", "status", "input", "input_part", "input_parts"),
	EventToolResult:        keys("tool", "summary", "output", "call_id", "output_part", "output_parts", "exit_code", "status"),
	EventToolCallSummary:   keys("tool", "summary", "call_id", "status"),
	EventToolResultSummary: keys("tool", "summary", "output", "exit_code", "status"),
	EventCommandSummary:    keys("summary", "output_summary", "exit_code", "status"),
	EventFileChange:        keys("path_label", "change", "summary", "changes"),
	EventFileChangeSummary: keys("path_label", "change", "summary", "changes"),
	EventChildAgent:        keys("agent_label", "status", "summary"),
	EventSubagentSummary:   keys("agent_label", "status", "summary"),
	EventQuestion:          keys("question_request_id", "question_version", "status", "questions"),
	EventApprovalRequest:   keys("approval_id", "approval_version", "kind", "summary", "object", "detail", "impact"),
	EventApprovalStatus:    keys("approval_id", "approval_version", "status", "summary"),
	EventCommandStatus:     keys("command_id", "type", "status", "result_code"),
	EventUnsupported:       keys("provider_type", "diagnostic"),
}

func keys(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

type EventInput struct {
	ProviderEventID string         `json:"provider_event_id"`
	Kind            EventKind      `json:"kind"`
	Source          EventSource    `json:"source"`
	OccurredAt      time.Time      `json:"occurred_at"`
	TurnID          string         `json:"turn_id,omitempty"`
	ItemID          string         `json:"item_id,omitempty"`
	ParentItemID    string         `json:"parent_item_id,omitempty"`
	Status          string         `json:"status,omitempty"`
	Payload         map[string]any `json:"payload"`
}

type EventSource string

const (
	EventSourceSnapshot EventSource = "snapshot"
	EventSourceLive     EventSource = "live"
	EventSourceApproval EventSource = "approval"
)

func (source EventSource) Valid() bool {
	switch source {
	case EventSourceSnapshot, EventSourceLive, EventSourceApproval:
		return true
	default:
		return false
	}
}

func (event EventInput) RequiredScope() Scope {
	switch event.Source {
	case EventSourceSnapshot:
		return ScopeRead
	case EventSourceLive:
		return ScopeLive
	case EventSourceApproval:
		return ScopeApproval
	default:
		return ""
	}
}

type Event struct {
	SessionID       string         `json:"session_id"`
	Sequence        int64          `json:"sequence"`
	ProviderEventID string         `json:"provider_event_id"`
	Kind            EventKind      `json:"kind"`
	OccurredAt      time.Time      `json:"occurred_at"`
	TurnID          string         `json:"turn_id,omitempty"`
	ItemID          string         `json:"item_id,omitempty"`
	ParentItemID    string         `json:"parent_item_id,omitempty"`
	Status          string         `json:"status,omitempty"`
	Payload         map[string]any `json:"payload"`
	StoredAt        time.Time      `json:"stored_at"`
}

type EventAck struct {
	ProviderEventID string `json:"provider_event_id"`
	Sequence        int64  `json:"sequence"`
}

type EventPage struct {
	Items           []Event `json:"items"`
	CurrentSequence int64   `json:"current_sequence"`
	NextCursor      *string `json:"next_cursor"`
}

type EventBatch struct {
	Events []EventInput `json:"events"`
}

type EventBatchAck struct {
	Items        []EventAck `json:"items"`
	LastSequence int64      `json:"last_sequence"`
}

func (value EventInput) Validate() error {
	if !nativeIDPattern.MatchString(value.ProviderEventID) || eventPayloadKeys[value.Kind] == nil ||
		value.OccurredAt.IsZero() || value.OccurredAt.After(time.Now().UTC().Add(24*time.Hour)) ||
		!validOptionalNativeID(value.TurnID) || !validOptionalNativeID(value.ItemID) ||
		!validOptionalNativeID(value.ParentItemID) || !validOptionalVisibleString(value.Status, 128) || value.Payload == nil ||
		!value.validSourceForKind() {
		return errors.New("Codex event fields are invalid")
	}
	if value.Kind != EventCommandStatus && strings.HasPrefix(value.ProviderEventID, "morde:command:") {
		return errors.New("Codex command event IDs are reserved")
	}
	allowed := eventPayloadKeys[value.Kind]
	if value.Kind == EventCommandStatus && (!validCommandStatusPayload(value.Payload) ||
		value.Status != stringValue(value.Payload["status"])) {
		return errors.New("Codex command status event is invalid")
	}
	if value.Kind == EventQuestion && !validQuestionEvent(value.Payload) {
		return errors.New("Codex question event is invalid")
	}
	if !validToolEventPayload(value.Kind, value.Payload) {
		return errors.New("Codex tool event payload is invalid")
	}
	for key, item := range value.Payload {
		if _, ok := allowed[key]; !ok || !validEventPayloadValue(value.Kind, key, item) {
			return errors.New("Codex event payload contains unsupported fields")
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxEventBytes {
		return errors.New("Codex event exceeds the maximum size")
	}
	return nil
}

const (
	MaxToolPayloadChunkBytes = 24 << 10
	maxToolPayloadParts      = 4096
)

// Tool input/output is transported in UTF-8-safe chunks. The chunk metadata is
// optional for historical events, but new chunked events must be self-contained
// so consumers can reconstruct one call without guessing.
func validToolEventPayload(kind EventKind, payload map[string]any) bool {
	switch kind {
	case EventToolCall:
		input, hasInput := payload["input"]
		partValue, hasPart := payload["input_part"]
		partsValue, hasParts := payload["input_parts"]
		if !hasInput && !hasPart && !hasParts {
			return true // Older events may contain only a summary and call ID.
		}
		text, textOK := input.(string)
		part, partOK := nonNegativePayloadInt(partValue)
		parts, partsOK := positivePayloadInt(partsValue)
		return textOK && validToolPayloadChunk(text) && validToolCallID(payload["call_id"]) &&
			hasPart && partOK && hasParts && partsOK && parts <= maxToolPayloadParts && part < parts
	case EventToolResult:
		callID, hasCallID := payload["call_id"]
		partValue, hasPart := payload["output_part"]
		partsValue, hasParts := payload["output_parts"]
		if !hasPart && !hasParts {
			return true // Legacy results may have a call ID but no chunk metadata.
		}
		output, textOK := payload["output"].(string)
		part, partOK := nonNegativePayloadInt(partValue)
		if !hasCallID || !hasPart || !textOK || !validToolPayloadChunk(output) || !validToolCallID(callID) || !partOK {
			return false
		}
		if hasParts {
			parts, partsOK := positivePayloadInt(partsValue)
			if !partsOK || parts > maxToolPayloadParts || part >= parts {
				return false
			}
		}
		return true
	default:
		return true
	}
}

func validToolCallID(value any) bool {
	text, ok := value.(string)
	return ok && nativeIDPattern.MatchString(text)
}

func validToolPayloadChunk(value string) bool {
	return utf8.ValidString(value) && len(value) <= MaxToolPayloadChunkBytes && !strings.ContainsRune(value, '\x00')
}

func nonNegativePayloadInt(value any) (int64, bool) {
	integer, ok := payloadInt64(value)
	return integer, ok && integer >= 0 && integer < maxToolPayloadParts
}

func positivePayloadInt(value any) (int64, bool) {
	integer, ok := payloadInt64(value)
	return integer, ok && integer > 0
}

func payloadInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed < 0 || typed > maxToolPayloadParts || typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return integer, true
	default:
		return 0, false
	}
}

func (value EventInput) validSourceForKind() bool {
	if value.Kind == EventCommandStatus {
		return value.Source == ""
	}
	if !value.Source.Valid() {
		return false
	}

	switch value.Kind {
	case EventQuestion:
		return value.Source == EventSourceLive
	case EventApprovalRequest, EventApprovalStatus:
		return value.Source == EventSourceApproval
	case EventUnsupported:
		if stringValue(value.Payload["provider_type"]) == "user_question" {
			// Historical snapshots may contain App Server user-input items that
			// are only exposed as unsupported diagnostics. Their source still
			// determines the required read or approval scope.
			return value.Source == EventSourceSnapshot || value.Source == EventSourceApproval
		}
		return value.Source == EventSourceSnapshot || value.Source == EventSourceLive
	default:
		return value.Source == EventSourceSnapshot || value.Source == EventSourceLive
	}
}

func validCommandStatusPayload(payload map[string]any) bool {
	if len(payload) != 4 || !domain.ValidCodexCommandID(stringValue(payload["command_id"])) {
		return false
	}
	commandType := CommandType(stringValue(payload["type"]))
	if commandType != CommandSend && commandType != CommandInterrupt && commandType != CommandApproval && commandType != CommandAnswerQuestion {
		return false
	}
	if !CommandStatus(stringValue(payload["status"])).Valid() {
		return false
	}
	resultCode, ok := payload["result_code"].(string)
	return ok && len(resultCode) <= 128 && commandResultCodePattern.MatchString(resultCode)
}

var commandResultCodePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{0,128}$`)

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func validEventPayloadValue(kind EventKind, key string, value any) bool {
	if kind == EventQuestion && key == "questions" {
		return validQuestions(value)
	}
	if kind == EventQuestion && key == "status" {
		status := stringValue(value)
		return status == "pending" || status == "answered" || status == "stale"
	}
	if kind == EventQuestion && key == "question_request_id" {
		return validOpaqueQuestionID(value)
	}
	if kind == EventQuestion && key == "question_version" {
		return questionVersionPattern.MatchString(stringValue(value))
	}
	return validPayloadValue(value)
}

func validQuestionEvent(payload map[string]any) bool {
	version := stringValue(payload["question_version"])
	status := stringValue(payload["status"])
	if !validOpaqueQuestionID(payload["question_request_id"]) || !questionVersionPattern.MatchString(version) {
		return false
	}
	switch status {
	case "pending":
		questions, exists := payload["questions"]
		return exists && len(payload) == 4 && validQuestions(questions)
	case "answered", "stale":
		_, hasQuestions := payload["questions"]
		return !hasQuestions && len(payload) == 3
	default:
		return false
	}
}

func validQuestions(value any) bool {
	questions, ok := value.([]any)
	if !ok || len(questions) == 0 || len(questions) > 64 {
		return false
	}
	seenIDs := make(map[string]struct{}, len(questions))
	for _, value := range questions {
		question, ok := value.(map[string]any)
		if !ok || len(question) < 4 || len(question) > 6 {
			return false
		}
		for key := range question {
			if key != "id" && key != "header" && key != "question" && key != "options" && key != "is_secret" && key != "is_other" {
				return false
			}
		}
		if !validOpaqueQuestionID(question["id"]) || !validQuestionText(question["header"], 4096) ||
			!validQuestionText(question["question"], 4096) {
			return false
		}
		questionID := question["id"].(string)
		if _, duplicate := seenIDs[questionID]; duplicate {
			return false
		}
		seenIDs[questionID] = struct{}{}
		for _, key := range []string{"is_secret", "is_other"} {
			if flag, exists := question[key]; exists {
				if _, ok := flag.(bool); !ok {
					return false
				}
			}
		}
		options, ok := question["options"].([]any)
		if !ok || len(options) > 64 {
			return false
		}
		for _, value := range options {
			option, ok := value.(map[string]any)
			if !ok || len(option) < 1 || len(option) > 2 {
				return false
			}
			for key := range option {
				if key != "label" && key != "description" {
					return false
				}
			}
			if !validQuestionText(option["label"], 4096) {
				return false
			}
			if description, exists := option["description"]; exists && !validQuestionText(description, 4096) {
				return false
			}
		}
	}
	return true
}

func validQuestionAnswers(answers map[string][]string) bool {
	if len(answers) == 0 || len(answers) > 64 {
		return false
	}
	for questionID, values := range answers {
		if !validOpaqueQuestionID(questionID) || len(values) == 0 || len(values) > 64 {
			return false
		}
		for _, answer := range values {
			if !validQuestionAnswer(answer) {
				return false
			}
		}
	}
	return true
}

func validQuestionAnswer(value string) bool {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > 4096 || strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}

func validOpaqueQuestionID(value any) bool {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || len(text) == 0 || len(text) > 512 || strings.TrimSpace(text) == "" {
		return false
	}
	for _, character := range text {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validQuestionText(value any, maximum int) bool {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || len(text) > maximum {
		return false
	}
	for _, character := range text {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}

func validPayloadValue(value any) bool {
	switch typed := value.(type) {
	case string:
		return utf8.ValidString(typed) && len(typed) <= MaxEventBytes/2 && !strings.ContainsRune(typed, '\x00')
	case bool, float64, json.Number:
		return true
	case int, int32, int64:
		return true
	case []any:
		if len(typed) > 256 {
			return false
		}
		for _, item := range typed {
			text, ok := item.(string)
			if !ok || !utf8.ValidString(text) || len(text) > 4096 || strings.ContainsRune(text, '\x00') {
				return false
			}
		}
		return true
	case []string:
		if len(typed) > 256 {
			return false
		}
		for _, text := range typed {
			if !utf8.ValidString(text) || len(text) > 4096 || strings.ContainsRune(text, '\x00') {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (batch EventBatch) Validate() (int, error) {
	if len(batch.Events) == 0 || len(batch.Events) > 256 {
		return 0, errors.New("Codex event batch is invalid")
	}
	seen := make(map[string]struct{}, len(batch.Events))
	total := 0
	for _, event := range batch.Events {
		if err := event.Validate(); err != nil {
			return 0, err
		}
		if event.Kind == EventCommandStatus {
			return 0, errors.New("Codex command status events are server-managed")
		}
		if _, duplicate := seen[event.ProviderEventID]; duplicate {
			return 0, errors.New("Codex event batch contains duplicate provider IDs")
		}
		seen[event.ProviderEventID] = struct{}{}
		encoded, err := json.Marshal(event)
		if err != nil {
			return 0, errors.New("Codex event batch is invalid")
		}
		total += len(encoded)
		if total > MaxEventBatchBytes {
			return 0, errors.New("Codex event batch exceeds the maximum size")
		}
	}
	encoded, err := json.Marshal(batch)
	if err != nil || len(encoded) > MaxEventBatchBytes {
		return 0, errors.New("Codex event batch exceeds the maximum size")
	}
	return total, nil
}

type CommandType string

const (
	CommandSend           CommandType = "send"
	CommandInterrupt      CommandType = "interrupt"
	CommandApproval       CommandType = "approval"
	CommandAnswerQuestion CommandType = "answer_question"
)

type CommandStatus string

const (
	CommandWaitingPC       CommandStatus = "waiting_pc"
	CommandClaimed         CommandStatus = "claimed"
	CommandForwarded       CommandStatus = "forwarded"
	CommandRunning         CommandStatus = "running"
	CommandWaitingApproval CommandStatus = "waiting_approval"
	CommandCompleted       CommandStatus = "completed"
	CommandFailed          CommandStatus = "failed"
	CommandUnknown         CommandStatus = "unknown"
)

func (status CommandStatus) Valid() bool {
	switch status {
	case CommandWaitingPC, CommandClaimed, CommandForwarded, CommandRunning, CommandWaitingApproval,
		CommandCompleted, CommandFailed, CommandUnknown:
		return true
	default:
		return false
	}
}

func (status CommandStatus) ValidResult() bool {
	switch status {
	case CommandForwarded, CommandRunning, CommandWaitingApproval, CommandCompleted, CommandFailed, CommandUnknown:
		return true
	default:
		return false
	}
}

type CommandInput struct {
	Type              CommandType         `json:"type"`
	Text              string              `json:"text,omitempty"`
	TurnID            string              `json:"turn_id,omitempty"`
	ApprovalID        string              `json:"approval_id,omitempty"`
	ApprovalVersion   string              `json:"approval_version,omitempty"`
	QuestionRequestID string              `json:"question_request_id,omitempty"`
	QuestionVersion   string              `json:"question_version,omitempty"`
	Answers           map[string][]string `json:"answers,omitempty"`
	Decision          string              `json:"decision,omitempty"`
}

func (command CommandInput) Validate() error {
	switch command.Type {
	case CommandSend:
		if !validCommandText(command.Text) || command.TurnID != "" || command.ApprovalID != "" || command.ApprovalVersion != "" ||
			command.QuestionRequestID != "" || command.QuestionVersion != "" || command.Answers != nil || command.Decision != "" {
			return errors.New("Codex send command is invalid")
		}
	case CommandInterrupt:
		if !validVisibleString(command.TurnID, 1, 512) || command.Text != "" || command.ApprovalID != "" || command.ApprovalVersion != "" ||
			command.QuestionRequestID != "" || command.QuestionVersion != "" || command.Answers != nil || command.Decision != "" {
			return errors.New("Codex interrupt command is invalid")
		}
	case CommandApproval:
		if !validVisibleString(command.ApprovalID, 1, 512) || !validVisibleString(command.ApprovalVersion, 1, 256) ||
			(command.Decision != "approve" && command.Decision != "reject") || command.Text != "" || command.TurnID != "" ||
			command.QuestionRequestID != "" || command.QuestionVersion != "" || command.Answers != nil {
			return errors.New("Codex approval command is invalid")
		}
	case CommandAnswerQuestion:
		if !validOpaqueQuestionID(command.QuestionRequestID) || !questionVersionPattern.MatchString(command.QuestionVersion) ||
			!validQuestionAnswers(command.Answers) || command.Text != "" || command.TurnID != "" || command.ApprovalID != "" ||
			command.ApprovalVersion != "" || command.Decision != "" {
			return errors.New("Codex question answer command is invalid")
		}
	default:
		return errors.New("Codex command type is invalid")
	}
	return nil
}

func validCommandText(value string) bool {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > 32768 || strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}

func (command CommandInput) RequiredScope() Scope {
	if command.Validate() != nil {
		return ""
	}
	switch command.Type {
	case CommandSend:
		return ScopeSend
	case CommandAnswerQuestion:
		return ScopeSend
	case CommandInterrupt:
		return ScopeInterrupt
	case CommandApproval:
		return ScopeApproval
	default:
		return ""
	}
}

type Command struct {
	ID                string        `json:"id"`
	SessionID         string        `json:"session_id"`
	InstallationID    string        `json:"installation_id,omitempty"`
	ProfileKey        string        `json:"profile_key,omitempty"`
	ThreadID          string        `json:"thread_id,omitempty"`
	GrantVersion      int64         `json:"grant_version"`
	Type              CommandType   `json:"type"`
	Status            CommandStatus `json:"status"`
	Input             CommandInput  `json:"input"`
	ResultCode        string        `json:"result_code,omitempty"`
	ProviderRequestID string        `json:"provider_request_id,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	ClaimedAt         *time.Time    `json:"claimed_at,omitempty"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

type CommandResult struct {
	Status            CommandStatus `json:"status"`
	ProviderRequestID string        `json:"provider_request_id,omitempty"`
	ResultCode        string        `json:"result_code,omitempty"`
}

type SessionPage struct {
	Items      []Session `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

func RequiredScopeForCommand(command CommandInput) Scope { return command.RequiredScope() }

func ValidateSessionID(id string) bool { return domain.ValidCodexSessionID(id) }
func ValidateCommandID(id string) bool { return domain.ValidCodexCommandID(id) }

func validVisibleString(value string, minimum, maximum int) bool {
	return utf8.ValidString(value) && len(value) >= minimum && len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validOptionalVisibleString(value string, maximum int) bool {
	return value == "" || validVisibleString(value, 1, maximum)
}

func validOptionalNativeID(value string) bool {
	return value == "" || nativeIDPattern.MatchString(value)
}
