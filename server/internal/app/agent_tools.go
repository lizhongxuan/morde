package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func validBoundedText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maxRunes
}

type AgentHostFence struct {
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	AIAllowed         bool
}

// AgentHostFenceReader reads the current Host generation and AI control fence.
type AgentHostFenceReader interface {
	ReadTaskFence(ctx context.Context, ownerID, hostID string) (AgentHostFence, error)
}

const agentAskUserToolName = "ask_user"

const cloudDSHPlanStepIDArgumentName = "morde_plan_step_id"
const cloudDSHPlanRevisionArgumentName = "morde_plan_revision"

// cloudDSHPlanStepArguments separates the plan association from the arguments
// that are delivered to a business tool. The association is model-supplied
// metadata and is only trusted after the Store validates it against the
// current persisted plan and dependency evidence.
type cloudDSHPlanStepArguments struct {
	StepID       string
	PlanRevision int64
	BusinessArgs json.RawMessage
	HasStepID    bool
}

func extractCloudDSHPlanStepArguments(raw string) (cloudDSHPlanStepArguments, error) {
	arguments, err := validateAgentToolArguments(raw)
	if err != nil {
		return cloudDSHPlanStepArguments{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &fields); err != nil || fields == nil {
		return cloudDSHPlanStepArguments{}, errors.New("tool arguments must be an object")
	}
	encodedStepID, hasStepID := fields[cloudDSHPlanStepIDArgumentName]
	encodedRevision, hasPlanRevision := fields[cloudDSHPlanRevisionArgumentName]
	if !hasStepID && !hasPlanRevision {
		return cloudDSHPlanStepArguments{BusinessArgs: arguments}, nil
	}
	if !hasStepID || !hasPlanRevision {
		return cloudDSHPlanStepArguments{}, errors.New("plan step metadata is incomplete")
	}
	var stepID string
	if json.Unmarshal(encodedStepID, &stepID) != nil || !validCloudDSHPlanStepID(stepID) {
		return cloudDSHPlanStepArguments{}, errors.New("plan step metadata is invalid")
	}
	var planRevision int64
	if json.Unmarshal(encodedRevision, &planRevision) != nil || planRevision < 1 {
		return cloudDSHPlanStepArguments{}, errors.New("plan revision metadata is invalid")
	}
	delete(fields, cloudDSHPlanStepIDArgumentName)
	delete(fields, cloudDSHPlanRevisionArgumentName)
	businessArgs, err := json.Marshal(fields)
	if err != nil || !validAgentJSONObject(businessArgs, maxAgentToolArgsBytes) {
		return cloudDSHPlanStepArguments{}, errors.New("business tool arguments are invalid")
	}
	return cloudDSHPlanStepArguments{StepID: stepID, PlanRevision: planRevision, BusinessArgs: businessArgs, HasStepID: true}, nil
}

func hasCloudDSHPlanStepMetadata(raw string) bool {
	arguments, err := validateAgentToolArguments(raw)
	if err != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(arguments, &fields) != nil || fields == nil {
		return false
	}
	_, hasStepID := fields[cloudDSHPlanStepIDArgumentName]
	_, hasRevision := fields[cloudDSHPlanRevisionArgumentName]
	return hasStepID || hasRevision
}

func validCloudDSHPlanStepID(value string) bool {
	if len(value) == 0 || len(value) > 40 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			(index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
}

var agentAskUserTool = AgentToolDefinition{
	ModelTool: ModelTool{
		Name:        agentAskUserToolName,
		Description: "Ask the user for one missing detail needed to complete the task. Use alone; execution pauses until the user answers.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","minLength":1,"maxLength":4096}},"required":["question"],"additionalProperties":false}`),
		Strict:      true,
	},
	RequiredScope: identity.ScopeComputerObserve,
}

func validUTF8(value string) bool { return utf8.ValidString(value) }

func sameAgentControlFence(left, right AgentHostFence) bool {
	return left.RuntimeGeneration == right.RuntimeGeneration && left.ControlVersion == right.ControlVersion &&
		left.AIAllowed == right.AIAllowed &&
		(left.RuntimeInstanceID == "" || right.RuntimeInstanceID == "" || left.RuntimeInstanceID == right.RuntimeInstanceID)
}

type AgentToolRequest struct {
	Task      task.Task
	Call      ModelToolCall
	Arguments json.RawMessage
	// OperationRequestHash preserves the exact durable DSH call identity when
	// Business arguments have had reserved execution metadata removed. It is
	// consumed only by Host operation reconciliation.
	OperationRequestHash []byte
	OperationKey         string
	OperationID          string
	RequiredScope        identity.Scope
	Fence                AgentHostFence
	WorkspaceID          string
	Approval             *AgentToolApprovalDecision
}

// AgentToolApprovalDecision is supplied only after the authenticated task
// store resolves the exact pending request. Tool executors must still verify
// the current request and Host fence before honoring it.
type AgentToolApprovalDecision struct {
	PendingID   string
	Version     int64
	Decision    task.PendingDecision
	RequestHash []byte
}

type AgentToolDefinition struct {
	ModelTool
	RequiredScope     identity.Scope
	MCPConnectorID    string
	MCPRemoteToolName string
}

type AgentToolPreparationStatus string

const (
	AgentToolReady            AgentToolPreparationStatus = "ready"
	AgentToolApprovalRequired AgentToolPreparationStatus = "approval_required"
)

type AgentApprovalRequest struct {
	Object  string `json:"object"`
	Content string `json:"content"`
	Impact  string `json:"impact"`
	// RejectionConsequence states in plain text what happens if the user
	// rejects the action. It may be blank for approvals created before E05.
	RejectionConsequence string `json:"rejection_consequence,omitempty"`
}

type AgentToolPreparation struct {
	Status AgentToolPreparationStatus
	// TriggerRule names the action-matrix category that required approval
	// (task.ApprovalTrigger*). Empty when the preparation does not require
	// approval or the rule is unknown.
	TriggerRule    string
	Approval       AgentApprovalRequest
	Fence          AgentHostFence
	WorkspaceID    string
	BindingVersion int64
	Token          string
}

type AgentToolResultStatus string

const (
	AgentToolSucceeded AgentToolResultStatus = "succeeded"
	AgentToolFailed    AgentToolResultStatus = "failed"
	AgentToolUnknown   AgentToolResultStatus = "unknown"
	AgentToolNeedsUser AgentToolResultStatus = "needs_user"
	// AgentToolNotFound means an exact fenced runtime query confirmed that the
	// persisted operation ID has no runtime journal row. The caller may rearm
	// only that same Morde operation ID before dispatching it again.
	AgentToolNotFound AgentToolResultStatus = "not_found"
)

type AgentToolResult struct {
	Status              AgentToolResultStatus
	TerminalForAttempt  bool
	WaitingReason       string
	ExternalID          string
	Output              string
	Images              []ModelImage
	SupersedingMessages []AgentConversationMessage
}

// AgentModelFailureDiagnostic contains only safe, allowlisted details about a
// rejected model response. It never carries model text or tool arguments.
type AgentModelFailureDiagnostic struct {
	Stage      string
	Category   string
	ReasonCode string
	ToolName   string
	ToolCallID string
	// LadderExhausted marks a terminal state where the E12 retry ladder
	// spent its budget (Attempts retries). pauseRecovery then records the
	// ladder-exhausted terminal instead of the generic recovery pause.
	LadderExhausted bool
	// Attempts is the number of retries spent before exhaustion.
	Attempts int
	// Reason is the FailoverReason the ladder exhausted on; it selects the
	// §4.3 user-visible case for the terminal text.
	Reason FailoverReason
}

// AgentToolTerminalAttemptFinisher durably closes a Task after the worker has
// resolved the DSH request that produced a terminal tool result.
type AgentToolTerminalAttemptFinisher interface {
	FailTerminalToolAttempt(context.Context, task.Task, ModelToolCall) error
}

type AgentVerificationRequest struct {
	Task                task.Task
	StepID              string
	Claim               string
	Evidence            []AgentFileEvidence
	ScheduleObservation *schedule.ObservationSpec
	Fence               AgentHostFence
}

// AgentFileEvidence is a candidate produced from a successful file_write
// call/output pair. Runtime verification independently computes the same
// metadata from the current Workspace file before an artifact is persisted.
type AgentFileEvidence struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	CallID        string `json:"-"`
	DurableCallID string `json:"-"`
	WorkspaceID   string `json:"-"`
	OperationKey  string `json:"-"`
	RequestHash   []byte `json:"-"`
	Result        string `json:"-"`
}

// FileEvidenceReadRequestHash identifies an internal read-only verification
// operation by its exact durable file_write sources. The operation key is
// versioned so rows created before this proof was part of the identity cannot
// be replayed with weaker evidence.
func FileEvidenceReadRequestHash(operationKey string, sources []OperationEvidenceSource) ([32]byte, error) {
	if !strings.HasPrefix(operationKey, "agent:verify:v2:") || len(operationKey) > 255 ||
		len(sources) == 0 || len(sources) > maxAgentVerifyFiles {
		return [32]byte{}, errors.New("file evidence read identity is invalid")
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if source.CallID == "" || len(source.CallID) > 256 || !utf8.ValidString(source.CallID) || strings.ContainsRune(source.CallID, '\x00') ||
			source.OperationKey != AgentOperationKey(source.CallID) || !domain.ValidWorkspaceID(source.WorkspaceID) ||
			source.BindingVersion < 1 || len(source.RequestHash) != sha256.Size ||
			!validWorkspaceRelativePath(source.Path) || source.Size < 0 || source.Size > maxAgentVerifyFileBytes ||
			len(source.SHA256) != sha256.Size*2 || source.SHA256 != strings.ToLower(source.SHA256) ||
			len(source.Result) == 0 || !validAgentJSONObject([]byte(source.Result), maxAgentToolOutputBytes) {
			return [32]byte{}, errors.New("file evidence source is invalid")
		}
		if _, err := hex.DecodeString(source.SHA256); err != nil {
			return [32]byte{}, errors.New("file evidence source digest is invalid")
		}
		key := source.OperationKey + "\x00" + source.Path
		if _, exists := seen[key]; exists {
			return [32]byte{}, errors.New("file evidence source is duplicated")
		}
		seen[key] = struct{}{}
	}
	encoded, err := json.Marshal(struct {
		Version      int                       `json:"version"`
		OperationKey string                    `json:"operation_key"`
		Sources      []OperationEvidenceSource `json:"sources"`
	}{Version: 1, OperationKey: operationKey, Sources: sources})
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode file evidence read identity: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

const (
	maxAgentVerifyFiles      = 16
	maxAgentVerifyFileBytes  = 1 << 20
	maxAgentVerifyTotalBytes = 8 << 20
)

type AgentVerificationStatus string

const (
	AgentVerified      AgentVerificationStatus = "verified"
	AgentNotVerified   AgentVerificationStatus = "not_verified"
	AgentVerifyUnknown AgentVerificationStatus = "unknown"
)

type AgentVerificationResult struct {
	Status                 AgentVerificationStatus
	WaitingReason          string
	Summary                string
	Observation            string
	ObservationFingerprint string
	Artifacts              []AgentVerifiedArtifact
}

func validAgentObservationFingerprint(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// AgentVerifiedArtifact is metadata independently observed by ToolExecutor.Verify.
// It must never be populated from the model's completion claim.
type AgentVerifiedArtifact struct {
	Label        string
	Path         string
	MediaType    string
	SourceCallID string `json:"-"`
	WorkspaceID  string
	Revision     int64
	SizeBytes    int64
	SHA256       string
}

const maxAgentVerifiedArtifacts = 64

func agentOperationKey(callID string) string {
	return AgentOperationKey(callID)
}

// AgentOperationKey returns the stable operation key for a provider tool-call ID.
func AgentOperationKey(callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return "agent:" + hex.EncodeToString(sum[:])
}

func agentOperationHash(name string, arguments json.RawMessage) ([32]byte, error) {
	encoded, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: arguments})
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode task operation: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func validateAgentVerificationArtifacts(result AgentVerificationResult) error {
	if len(result.Artifacts) > maxAgentVerifiedArtifacts || (result.Status != AgentVerified && len(result.Artifacts) != 0) {
		return errors.New("verification artifacts are not valid for this result")
	}
	seen := make(map[string]struct{}, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if !validBoundedText(artifact.Label, 256) || len(artifact.Path) > 4096 || len(artifact.MediaType) > 256 ||
			(artifact.SourceCallID != "" && SafeTaskValidationCallID(artifact.SourceCallID) != artifact.SourceCallID) ||
			!domain.ValidWorkspaceID(artifact.WorkspaceID) || artifact.Revision < 0 || artifact.SizeBytes < 0 || !validAgentObservationFingerprint(artifact.SHA256) ||
			strings.ContainsRune(artifact.Path, '\x00') || strings.Contains(artifact.Path, "\\") || path.IsAbs(artifact.Path) ||
			artifact.Path == "." || artifact.Path == ".." || strings.HasPrefix(path.Clean(artifact.Path), "../") || path.Clean(artifact.Path) != artifact.Path {
			return errors.New("verification artifact metadata is invalid")
		}
		mediaType, _, err := mime.ParseMediaType(artifact.MediaType)
		if err != nil || !strings.Contains(mediaType, "/") || strings.TrimSpace(artifact.MediaType) != artifact.MediaType {
			return errors.New("verification artifact media type is invalid")
		}
		if _, exists := seen[artifact.Path]; exists {
			return errors.New("verification returned duplicate artifact paths")
		}
		seen[artifact.Path] = struct{}{}
	}
	return nil
}

// agentFileWriteEvidence accepts only a unique, ordered function_call and
// function_call_output pair whose successful runtime output agrees with the
// original path, byte count, and any returned digest. Model text is ignored.
func agentFileWriteEvidence(inputs []ModelInput) ([]AgentFileEvidence, error) {
	type indexedInput struct {
		index int
		item  ModelInput
	}
	calls := make(map[string][]indexedInput)
	outputs := make(map[string][]indexedInput)
	for index, item := range inputs {
		switch item.Type {
		case "function_call":
			calls[item.CallID] = append(calls[item.CallID], indexedInput{index: index, item: item})
		case "function_call_output":
			outputs[item.CallID] = append(outputs[item.CallID], indexedInput{index: index, item: item})
		}
	}

	latest := make(map[string]AgentFileEvidence)
	order := make([]string, 0, len(inputs))
	orderedPaths := make(map[string]struct{})
	for _, outputItem := range inputs {
		if outputItem.Type != "function_call_output" {
			continue
		}
		callItems := calls[outputItem.CallID]
		outputItems := outputs[outputItem.CallID]
		if len(callItems) != 1 || len(outputItems) != 1 || callItems[0].item.Name != runtimeToolFileWrite ||
			outputItems[0].index <= callItems[0].index {
			continue
		}
		var args struct {
			WorkspaceID string `json:"workspace_id"`
			Path        string `json:"path"`
			Content     string `json:"content"`
		}
		argumentBytes := []byte(callItems[0].item.Arguments)
		if !validAgentJSONObject(argumentBytes, maxAgentToolArgsBytes) || decodeStrictRuntimeResult(argumentBytes, &args) != nil ||
			!domain.ValidWorkspaceID(args.WorkspaceID) || !validWorkspaceRelativePath(args.Path) {
			continue
		}
		var output struct {
			Path         string `json:"path"`
			BytesWritten int    `json:"bytes_written"`
			SHA256       string `json:"sha256"`
		}
		outputBytes := []byte(outputItems[0].item.Output)
		content := []byte(args.Content)
		digest := sha256.Sum256(content)
		if !validAgentJSONObject(outputBytes, maxAgentToolOutputBytes) || decodeStrictRuntimeResult(outputBytes, &output) != nil ||
			output.Path != args.Path || output.BytesWritten != len(content) || (output.SHA256 != "" && output.SHA256 != fmt.Sprintf("%x", digest[:])) {
			continue
		}

		operationKey := agentOperationKey(outputItem.CallID)
		requestHash, err := agentOperationHash(runtimeToolFileWrite, argumentBytes)
		if err != nil || operationKey == "" || len(operationKey) > 255 {
			continue
		}
		evidenceKey := args.WorkspaceID + "\x00" + args.Path
		// A later successful write replaces the earlier candidate, even if it is
		// over the evidence limit. Otherwise an old digest could be checked
		// against a file that the Agent has since overwritten.
		if len(content) > maxAgentVerifyFileBytes {
			delete(latest, evidenceKey)
			continue
		}
		if _, exists := orderedPaths[evidenceKey]; !exists {
			order = append(order, evidenceKey)
			orderedPaths[evidenceKey] = struct{}{}
		}
		latest[evidenceKey] = AgentFileEvidence{
			Path: args.Path, Size: int64(len(content)), SHA256: fmt.Sprintf("%x", digest[:]),
			CallID: SafeTaskValidationCallID(outputItem.CallID), DurableCallID: outputItem.CallID, WorkspaceID: args.WorkspaceID,
			OperationKey: operationKey, RequestHash: append([]byte(nil), requestHash[:]...), Result: outputItem.Output,
		}
	}

	evidence := make([]AgentFileEvidence, 0, len(latest))
	var total int64
	for _, candidatePath := range order {
		candidate, exists := latest[candidatePath]
		if !exists {
			continue
		}
		total += candidate.Size
		if len(evidence)+1 > maxAgentVerifyFiles || total > maxAgentVerifyTotalBytes {
			return nil, errors.New("file verification evidence exceeds its bounds")
		}
		evidence = append(evidence, candidate)
	}
	return evidence, nil
}

func validWorkspaceRelativePath(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsRune(value, '\x00') && !strings.Contains(value, "\\") &&
		!path.IsAbs(value) && value != "." && value != ".." && path.Clean(value) == value && !strings.HasPrefix(value, "../")
}

var agentToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	maxAgentToolSchemaBytes = 64 << 10
	maxAgentToolArgsBytes   = 32 << 10
	maxAgentJSONDepth       = 32
	maxAgentJSONTokens      = 4096
	maxAgentToolOutputBytes = 64 << 10
	maxAgentImageBytes      = 2 << 20
	maxAgentTotalImageBytes = 4 << 20
)

func validateAgentTools(tools []AgentToolDefinition) (map[string]AgentToolDefinition, error) {
	if len(tools) > 64 {
		return nil, errors.New("too many tools")
	}
	byName := make(map[string]AgentToolDefinition, len(tools))
	for _, tool := range tools {
		if !agentToolNamePattern.MatchString(tool.Name) || len(tool.Description) > 4096 || len(tool.Parameters) > maxAgentToolSchemaBytes ||
			!validAgentJSONObject(tool.Parameters, maxAgentToolSchemaBytes) {
			return nil, errors.New("invalid tool definition")
		}
		switch tool.RequiredScope {
		case identity.ScopeMCPControl:
			fixedSelectionTool := (tool.Name == cloudDSHMCPDiscoverToolName || tool.Name == cloudDSHMCPCallToolName) &&
				tool.MCPConnectorID == "" && tool.MCPRemoteToolName == ""
			if !fixedSelectionTool && (!domain.ValidMCPConnectorID(tool.MCPConnectorID) || tool.MCPRemoteToolName == "" || len(tool.MCPRemoteToolName) > 128 ||
				!utf8.ValidString(tool.MCPRemoteToolName) || strings.IndexFunc(tool.MCPRemoteToolName, unicode.IsControl) >= 0) {
				return nil, errors.New("invalid MCP connector tool definition")
			}
		case identity.ScopeTaskRead, identity.ScopeTaskSubmit, identity.ScopeComputerObserve, identity.ScopeComputerOperate, identity.ScopeScheduleRead, identity.ScopeScheduleManage:
			if tool.MCPConnectorID != "" || tool.MCPRemoteToolName != "" {
				return nil, errors.New("non-MCP tool contains connector identity")
			}
		default:
			return nil, errors.New("invalid tool definition")
		}
		if _, exists := byName[tool.Name]; exists {
			return nil, errors.New("duplicate tool definition")
		}
		byName[tool.Name] = tool
	}
	return byName, nil
}

func validateAgentToolArguments(raw string) (json.RawMessage, error) {
	if len(raw) > maxAgentToolArgsBytes || !validAgentJSONObject([]byte(raw), maxAgentToolArgsBytes) {
		return nil, errors.New("invalid tool arguments")
	}
	return append(json.RawMessage(nil), strings.TrimSpace(raw)...), nil
}

func validAgentJSONObject(raw []byte, maxBytes int) bool {
	if len(raw) == 0 || len(raw) > maxBytes || !utf8.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	delim, ok := first.(json.Delim)
	if err != nil || !ok || delim != '{' {
		return false
	}
	tokens := 1
	if err := scanAgentJSONObject(decoder, 1, &tokens); err != nil {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return true
}

func scanAgentJSONObject(decoder *json.Decoder, depth int, tokens *int) error {
	if depth > maxAgentJSONDepth {
		return errors.New("JSON nesting limit exceeded")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		*tokens++
		if *tokens > maxAgentJSONTokens {
			return errors.New("JSON token limit exceeded")
		}
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("JSON object key is invalid")
		}
		if _, exists := seen[key]; exists {
			return errors.New("duplicate JSON object key")
		}
		seen[key] = struct{}{}
		if err := scanAgentJSONValue(decoder, depth+1, tokens); err != nil {
			return err
		}
	}
	closeToken, err := decoder.Token()
	if err != nil || closeToken != json.Delim('}') {
		return errors.New("JSON object is not closed")
	}
	return nil
}

func scanAgentJSONValue(decoder *json.Decoder, depth int, tokens *int) error {
	*tokens++
	if *tokens > maxAgentJSONTokens || depth > maxAgentJSONDepth {
		return errors.New("JSON complexity limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return scanAgentJSONObject(decoder, depth, tokens)
	case '[':
		for decoder.More() {
			if err := scanAgentJSONValue(decoder, depth+1, tokens); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("JSON array is not closed")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func validateAgentImages(images []ModelImage) error {
	total := 0
	for _, image := range images {
		if len(image.Data) == 0 || len(image.Data) > maxAgentImageBytes || !validAgentImageType(image.MediaType) {
			return errors.New("invalid tool image")
		}
		total += len(image.Data)
		if total > maxAgentTotalImageBytes {
			return errors.New("tool images exceed size limit")
		}
	}
	return nil
}

func validAgentImageType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func agentScopeSnapshot(scopes []string) []identity.Scope {
	result := make([]identity.Scope, 0, len(scopes))
	for _, scope := range scopes {
		result = append(result, identity.Scope(scope))
	}
	return result
}
