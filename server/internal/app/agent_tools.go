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
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

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

func sameAgentControlFence(left, right AgentHostFence) bool {
	return left.RuntimeGeneration == right.RuntimeGeneration && left.ControlVersion == right.ControlVersion &&
		left.AIAllowed == right.AIAllowed &&
		(left.RuntimeInstanceID == "" || right.RuntimeInstanceID == "" || left.RuntimeInstanceID == right.RuntimeInstanceID)
}

type AgentModelResolver interface {
	ResolveEndpoint(ctx context.Context, ownerID, modelID string) (ModelEndpoint, error)
}

type AgentToolRequest struct {
	Task          task.Task
	Call          ModelToolCall
	Arguments     json.RawMessage
	OperationKey  string
	OperationID   string
	RequiredScope identity.Scope
	Fence         AgentHostFence
	WorkspaceID   string
	Approval      *AgentToolApprovalDecision
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
	RequiredScope identity.Scope
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
}

type AgentToolPreparation struct {
	Status         AgentToolPreparationStatus
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
	Status     AgentToolResultStatus
	ExternalID string
	Output     string
	Images     []ModelImage
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
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	WorkspaceID  string `json:"-"`
	OperationKey string `json:"-"`
	RequestHash  []byte `json:"-"`
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
	Label     string
	Path      string
	MediaType string
}

const maxAgentVerifiedArtifacts = 64

func validateAgentVerificationArtifacts(result AgentVerificationResult) error {
	if len(result.Artifacts) > maxAgentVerifiedArtifacts || (result.Status != AgentVerified && len(result.Artifacts) != 0) {
		return errors.New("verification artifacts are not valid for this result")
	}
	seen := make(map[string]struct{}, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if !validBoundedText(artifact.Label, 256) || len(artifact.Path) > 4096 || len(artifact.MediaType) > 256 ||
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
// original path and byte count. Model completion text is deliberately ignored.
func agentFileWriteEvidence(inputs []ModelInput, batchScope *agentBatchScope) ([]AgentFileEvidence, error) {
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
		}
		outputBytes := []byte(outputItems[0].item.Output)
		if !validAgentJSONObject(outputBytes, maxAgentToolOutputBytes) || decodeStrictRuntimeResult(outputBytes, &output) != nil ||
			output.Path != args.Path || output.BytesWritten != len([]byte(args.Content)) {
			continue
		}

		content := []byte(args.Content)
		operationKey := agentOperationKeyForBatch(outputItem.CallID, batchScope)
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
		digest := sha256.Sum256(content)
		if _, exists := orderedPaths[evidenceKey]; !exists {
			order = append(order, evidenceKey)
			orderedPaths[evidenceKey] = struct{}{}
		}
		latest[evidenceKey] = AgentFileEvidence{
			Path: args.Path, Size: int64(len(content)), SHA256: fmt.Sprintf("%x", digest[:]),
			WorkspaceID: args.WorkspaceID, OperationKey: operationKey, RequestHash: append([]byte(nil), requestHash[:]...),
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

// ToolExecutor is the only path from the Agent into a Host. Prepare is
// side-effect free. Both Prepare and Execute must recheck current user
// authorization, remaining budget, Host control state, and the expected fence;
// Execute must atomically reject stale fences and never replay an unknown result.
// Verify must independently inspect Host/runtime evidence and must not treat the
// model's claim as proof of completion.
type ToolExecutor interface {
	Definitions(context.Context, task.Task) ([]AgentToolDefinition, error)
	Prepare(context.Context, AgentToolRequest) (AgentToolPreparation, error)
	Execute(context.Context, AgentToolRequest, AgentToolPreparation) (AgentToolResult, error)
	Verify(context.Context, AgentVerificationRequest) (AgentVerificationResult, error)
}

// AgentOperationReconciler checks the durable runtime journal for an operation
// left unresolved by a worker crash. It must never dispatch the operation.
type AgentOperationReconciler interface {
	ReconcileOperation(context.Context, AgentToolRequest, task.Operation) (AgentToolResult, error)
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
			!validAgentJSONObject(tool.Parameters, maxAgentToolSchemaBytes) ||
			(tool.RequiredScope != identity.ScopeComputerObserve && tool.RequiredScope != identity.ScopeComputerOperate &&
				tool.RequiredScope != identity.ScopeScheduleRead && tool.RequiredScope != identity.ScopeScheduleManage) {
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

func modelToolInput(call ModelToolCall) ModelInput {
	return ModelInput{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments}
}
