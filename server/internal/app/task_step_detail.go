package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
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
	if evidence := value.ShellExecution; evidence != nil &&
		(evidence.ExitCode < 0 || evidence.ExitCode > 255 || evidence.StdoutBytes < 0 || evidence.StderrBytes < 0 ||
			evidence.StderrBytes > maxAgentToolOutputBytes || evidence.StdoutBytes > maxAgentToolOutputBytes-evidence.StderrBytes) {
		return false
	}
	if evidence := value.ContentEvidence; evidence != nil &&
		(!validSyncDigest(evidence.SHA256) || evidence.Preview == "" || len(evidence.Preview) > maxTaskStepContentEvidencePreviewBytes ||
			!utf8.ValidString(evidence.Preview) || strings.ContainsRune(evidence.Preview, '\x00')) {
		return false
	}
	if len(value.SourceLocations) > maxTaskStepSourceLocations {
		return false
	}
	for _, source := range value.SourceLocations {
		if source.ID == "" {
			if source.Kind != taskStepSourceLocationKindPublicWebPage || !validTaskStepSourceLocationFields(source) {
				return false
			}
		} else if !validTaskStepSourceLocationDocument(source) {
			return false
		}
		if source.Kind == taskStepSourceLocationKindPublicWebPage &&
			(value.ContentEvidence == nil || source.ContentSHA256 != value.ContentEvidence.SHA256) {
			return false
		}
	}
	if !validTaskStepExecution(value.Execution) {
		return false
	}
	if value.Execution != nil && (value.Execution.Stdout != nil || value.Execution.Stderr != nil) && value.ShellExecution == nil {
		return false
	}
	if value.ExecutionWorkspaceID != "" && !validRuntimeWorkspaceID(value.ExecutionWorkspaceID) {
		return false
	}
	if taskStepExecutionRequiresWorkspace(value.Execution) && value.ExecutionWorkspaceID == "" {
		return false
	}
	return true
}

func sealTaskStepDetail(sealer *Sealer, ownerID, taskID, stepID string, revision int64, value TaskStepDetailDocument) ([]byte, []byte, int32, error) {
	if sealer == nil || ownerID == "" || taskID == "" || stepID == "" || revision < 0 || !validTaskStepDetailDocument(value) {
		return nil, nil, 0, fmt.Errorf("task step detail identity or document is invalid")
	}
	for _, source := range value.SourceLocations {
		if !validTaskStepSourceLocationID(source.ID) {
			return nil, nil, 0, fmt.Errorf("task step source location identity is invalid")
		}
	}
	plain, err := json.Marshal(value)
	if err != nil || len(plain) == 0 || len(plain) > MaxTaskStepDetailPayloadBytes {
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
	call                     ModelToolCall
	current                  task.Task
	digest                   []byte
	sealer                   *Sealer
	displayObject            string
	shellExecution           *TaskStepShellExecutionEvidence
	shellOutput              *taskStepShellOutput
	executionStart           time.Time
	executionMS              *int64
	contentEvidence          *TaskStepContentEvidence
	sourceLocations          []TaskStepSourceLocation
	phaseOverride            string
	failureReason            string
	verificationCheck        *task.PlanVerificationCheck
	verificationPlanRevision int64
	verificationStepID       string
}

const maxTaskStepContentEvidencePreviewBytes = 4096

const (
	taskStepSourceLocationKindPublicWebPage        = "public_web_page"
	taskStepSourceLocationKindWorkspaceFile        = "workspace_file"
	taskStepSourceLocationKindWorkspaceSearchMatch = "workspace_search_match"
	taskStepSourceLocationStateRequestedURIOnly    = "requested_uri_only"
	taskStepSourceLocationStateWorkspaceRevision   = "immutable_workspace_revision"
	taskStepSourceLocationStateWorkspaceLive       = "live_workspace_directory"
)

const maxTaskStepSourceLocations = cloudDSHWorkspaceSearchMaxResults

const taskToolExecutionFailureReason = "tool_execution_failed"
const taskToolInvalidArgumentsFailureReason = "invalid_tool_arguments"
const taskToolAdvisoryGateFailureReason = "advisory_gate_rejected"

func validCloudDSHToolActivityFence(ctx context.Context, current task.Task, call ModelToolCall) bool {
	return ctx != nil && call.TaskFence != nil && call.TaskFence.TaskID == current.ID && call.TaskFence.OwnerID == current.OwnerID &&
		call.TaskFence.AgentSessionID != "" && current.CurrentRevision != nil && *current.CurrentRevision >= 0 &&
		call.TaskFence.AgentInstanceID == current.AgentInstanceID && call.TaskFence.AgentGeneration == current.AgentGeneration &&
		call.TaskFence.AuthorizationVersion == current.Authorization.Version &&
		call.TaskFence.LeaseOwner == current.Lease.Owner && call.TaskFence.LeaseVersion == current.Lease.Version
}

// taskToolTestFailureReason marks a shell step that ran a test command which
// failed (non-zero exit or timeout). It is the durable failing-test record
// legacy E03 step marker. It is not used as a write or completion gate.
const taskToolTestFailureReason = "test_failed"

// taskToolTestPassedReason marks a shell step that ran a test command which
// passed (exit code 0, no timeout). Together with taskToolTestFailureReason
// it is the durable test-run record the E07 completion gate reads: a failing
// run and a later passing run of the same tool call (same request digest)
// prove the new test turned green. The field name is historical: since E03
// it doubles as the test-run record, not only as a failure reason.
const taskToolTestPassedReason = "test_passed"

// taskToolTestSuitePassedReason marks a shell step whose passing run included
// a full-suite test command (no test-name filter), the second half of the
// E07 gate. A run that only passes a selected test is stamped test_passed.
const taskToolTestSuitePassedReason = "test_suite_passed"

func withCloudDSHToolActivity(ctx context.Context, current task.Task, call ModelToolCall, sealer *Sealer) context.Context {
	if !validCloudDSHToolActivityFence(ctx, current, call) {
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

// withInvalidRuntimeToolArgumentActivity records only a digest and the normal
// redacted tool summary for malformed arguments. The digest is domain
// separated from executable operation hashes so a malformed payload can never
// be mistaken for a dispatched operation during Task-step reconciliation.
func withInvalidRuntimeToolArgumentActivity(ctx context.Context, current task.Task, call ModelToolCall, sealer *Sealer) context.Context {
	if !validCloudDSHToolActivityFence(ctx, current, call) || call.TaskFence.AgentInstanceID == "" ||
		call.TaskFence.AgentGeneration < 1 || call.TaskFence.InitiatorID != current.InitiatorID ||
		len(call.Arguments) > maxAgentToolArgsBytes || !utf8.ValidString(call.Arguments) {
		return ctx
	}
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, cloudDSHToolActivity{call: call, current: current,
		digest: cloudDSHInvalidRuntimeToolArgumentsDigest(call.Name, call.Arguments), sealer: sealer})
}

func cloudDSHInvalidRuntimeToolArgumentsDigest(name, rawArguments string) []byte {
	digest := sha256.Sum256([]byte("morde-dsh-invalid-runtime-tool-arguments-v1\x00" + name + "\x00" + rawArguments))
	return append([]byte(nil), digest[:]...)
}

func cloudDSHToolActivityFromContext(ctx context.Context) (cloudDSHToolActivity, bool) {
	value, ok := ctx.Value(cloudDSHToolActivityKey{}).(cloudDSHToolActivity)
	return value, ok
}

func withTaskToolResultEvidence(ctx context.Context, phase, failureReason string) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return ctx
	}
	activity.phaseOverride, activity.failureReason = phase, failureReason
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func withTaskToolShellExecutionEvidence(ctx context.Context, name string, result AgentToolResult) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return ctx
	}
	activity.shellExecution = taskToolShellExecutionEvidence(name, result)
	if activity.shellExecution != nil {
		var output taskStepShellOutput
		if decodeStrictRuntimeResult([]byte(result.Output), &output) == nil {
			activity.shellOutput = &output
		}
	}
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func withTaskToolContentEvidence(ctx context.Context, name string, result AgentToolResult) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return ctx
	}
	activity.contentEvidence = taskToolContentEvidence(name, result)
	activity.sourceLocations = taskToolSourceLocations(name, activity.call.Arguments, activity.contentEvidence)
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func taskToolSourceLocations(name, rawArguments string, evidence *TaskStepContentEvidence) []TaskStepSourceLocation {
	if name != cloudDSHWebReadToolName || evidence == nil {
		return nil
	}
	arguments, _, err := builtinSearchArguments(name, rawArguments)
	if err != nil {
		return nil
	}
	var input struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		return nil
	}
	uri, ok := canonicalTaskStepPublicHTTPSURI(input.URL)
	if !ok {
		return nil
	}
	return []TaskStepSourceLocation{{Kind: taskStepSourceLocationKindPublicWebPage, URI: uri,
		State: taskStepSourceLocationStateRequestedURIOnly, ContentSHA256: evidence.SHA256}}
}

func withTaskToolWorkspaceSourceLocations(ctx context.Context, current task.Task, name string, result AgentToolResult) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok || activity.call.Name != name {
		return ctx
	}
	activity.sourceLocations = taskToolWorkspaceSourceLocations(name, activity.call.Arguments, result, current)
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func taskToolWorkspaceSourceLocations(name, rawArguments string, result AgentToolResult, current task.Task) []TaskStepSourceLocation {
	if result.Status != AgentToolSucceeded || current.WorkspaceID == "" || current.WorkspaceCheckpointRevision == nil ||
		current.InputRevision == nil || *current.WorkspaceCheckpointRevision < *current.InputRevision ||
		(name != cloudDSHWorkspaceReadToolName && name != cloudDSHWorkspaceSearchToolName) {
		return nil
	}
	arguments, err := validateAgentToolArguments(rawArguments)
	if err != nil {
		return nil
	}
	var expectedRevision int64
	if name == cloudDSHWorkspaceReadToolName {
		var request cloudDSHWorkspaceReadArguments
		if decodeStrictRuntimeResult(arguments, &request) != nil {
			return nil
		}
		expectedRevision = request.ExpectedRevision
		if expectedRevision < *current.InputRevision || expectedRevision > *current.WorkspaceCheckpointRevision {
			return nil
		}
		var output cloudDSHWorkspaceReadOutput
		if decodeStrictRuntimeResult([]byte(result.Output), &output) != nil || output.WorkspaceID != current.WorkspaceID ||
			output.Revision != expectedRevision || output.Path != request.Path || output.SizeBytes < 0 ||
			output.SizeBytes > cloudDSHWorkspaceReadMaxBytes || !validSyncDigest(output.SHA256) || !utf8.ValidString(output.Content) ||
			int64(len(output.Content)) != output.SizeBytes {
			return nil
		}
		contentDigest := sha256.Sum256([]byte(output.Content))
		if hex.EncodeToString(contentDigest[:]) != output.SHA256 {
			return nil
		}
		state := taskStepWorkspaceSourceState(output.SourceMode, output.ManifestSHA256)
		if state == "" {
			return nil
		}
		workspaceRevision := output.Revision
		location := TaskStepSourceLocation{Kind: taskStepSourceLocationKindWorkspaceFile, WorkspaceID: output.WorkspaceID,
			Revision: &workspaceRevision, Path: output.Path, ManifestSHA256: output.ManifestSHA256,
			State: state, ContentSHA256: output.SHA256}
		if !validTaskStepSourceLocationFields(location) {
			return nil
		}
		return []TaskStepSourceLocation{location}
	}
	var request cloudDSHWorkspaceSearchArguments
	if decodeStrictRuntimeResult(arguments, &request) != nil {
		return nil
	}
	expectedRevision = request.ExpectedRevision
	if expectedRevision < *current.InputRevision || expectedRevision > *current.WorkspaceCheckpointRevision {
		return nil
	}
	var output cloudDSHWorkspaceSearchOutput
	if decodeStrictRuntimeResult([]byte(result.Output), &output) != nil || output.WorkspaceID != current.WorkspaceID ||
		output.Revision != expectedRevision || len(output.Matches) > maxTaskStepSourceLocations {
		return nil
	}
	state := taskStepWorkspaceSourceState(output.SourceMode, output.ManifestSHA256)
	if state == "" {
		return nil
	}
	locations := make([]TaskStepSourceLocation, 0, len(output.Matches))
	seen := make(map[string]struct{}, len(output.Matches))
	for _, match := range output.Matches {
		line := match.Line
		workspaceRevision := output.Revision
		location := TaskStepSourceLocation{Kind: taskStepSourceLocationKindWorkspaceSearchMatch, WorkspaceID: output.WorkspaceID,
			Revision: &workspaceRevision, Path: match.Path, Line: &line, ManifestSHA256: output.ManifestSHA256,
			State: state, ContentSHA256: match.ContentSHA256}
		if !validTaskStepSourceLocationFields(location) {
			return nil
		}
		identity, _ := json.Marshal(struct {
			WorkspaceID string `json:"workspace_id"`
			Revision    int64  `json:"revision"`
			Path        string `json:"path"`
			Line        int    `json:"line"`
		}{location.WorkspaceID, *location.Revision, location.Path, *location.Line})
		key := string(identity)
		if _, exists := seen[key]; exists {
			return nil
		}
		seen[key] = struct{}{}
		locations = append(locations, location)
	}
	return locations
}

func taskStepWorkspaceSourceState(sourceMode, manifestSHA256 string) string {
	switch sourceMode {
	case cloudDSHWorkspaceSourceModeImmutableRevision:
		if validSyncDigest(manifestSHA256) {
			return taskStepSourceLocationStateWorkspaceRevision
		}
	case cloudDSHWorkspaceSourceModeLiveDirectory:
		if manifestSHA256 == "" {
			return taskStepSourceLocationStateWorkspaceLive
		}
	}
	return ""
}

func validTaskStepSourceLocationDocument(source TaskStepSourceLocation) bool {
	return validTaskStepSourceLocationID(source.ID) && validTaskStepSourceLocationFields(source)
}

func validTaskStepSourceLocationFields(source TaskStepSourceLocation) bool {
	if !validSyncDigest(source.ContentSHA256) {
		return false
	}
	switch source.Kind {
	case taskStepSourceLocationKindPublicWebPage:
		canonical, ok := canonicalTaskStepPublicHTTPSURI(source.URI)
		return ok && canonical == source.URI && source.State == taskStepSourceLocationStateRequestedURIOnly &&
			source.WorkspaceID == "" && source.Revision == nil && source.Path == "" && source.Line == nil && source.ManifestSHA256 == ""
	case taskStepSourceLocationKindWorkspaceFile, taskStepSourceLocationKindWorkspaceSearchMatch:
		if source.URI != "" || !validRuntimeWorkspaceID(source.WorkspaceID) || source.Revision == nil || *source.Revision < 0 ||
			validateCloudDSHWorkspacePath(source.Path) != nil || cloudDSHWorkspacePathIsSensitive(source.Path) {
			return false
		}
		switch source.State {
		case taskStepSourceLocationStateWorkspaceRevision:
			if !validSyncDigest(source.ManifestSHA256) {
				return false
			}
		case taskStepSourceLocationStateWorkspaceLive:
			if source.ManifestSHA256 != "" {
				return false
			}
		default:
			return false
		}
		if source.Kind == taskStepSourceLocationKindWorkspaceFile {
			return source.Line == nil
		}
		return source.Line != nil && *source.Line > 0
	default:
		return false
	}
}

func taskStepSourceLocationID(ownerID, taskID, stepID string, revision int64, callID string, source TaskStepSourceLocation) string {
	source.ID = ""
	encoded, err := json.Marshal(struct {
		OwnerID  string                 `json:"owner_id"`
		TaskID   string                 `json:"task_id"`
		StepID   string                 `json:"step_id"`
		Revision int64                  `json:"task_revision"`
		CallID   string                 `json:"call_id"`
		Source   TaskStepSourceLocation `json:"source"`
	}{ownerID, taskID, stepID, revision, callID, source})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(append([]byte("morde-task-step-source-location-v1\x00"), encoded...))
	return "src_" + hex.EncodeToString(digest[:16])
}

func validTaskStepSourceLocationID(value string) bool {
	if len(value) != 36 || !strings.HasPrefix(value, "src_") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "src_"))
	return err == nil && len(decoded) == 16 && "src_"+hex.EncodeToString(decoded) == value
}

func canonicalTaskStepPublicHTTPSURI(raw string) (string, bool) {
	if raw == "" || len(raw) > 2048 || !utf8.ValidString(raw) || strings.ContainsRune(raw, '\x00') ||
		cloudDSHHistoryCredentialPattern.MatchString(raw) {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", false
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery, parsed.Fragment, parsed.RawFragment, parsed.ForceQuery = "", "", "", false
	return parsed.String(), true
}

func taskToolContentEvidence(name string, result AgentToolResult) *TaskStepContentEvidence {
	if (name != cloudDSHWebSearchToolName && name != cloudDSHWebReadToolName) || result.Status != AgentToolSucceeded ||
		len(result.Output) == 0 || len(result.Output) > maxAgentToolOutputBytes || !utf8.ValidString(result.Output) {
		return nil
	}
	var output cloudDSHMCPOutput
	if json.Unmarshal([]byte(result.Output), &output) != nil || output.IsError || output.UnsupportedContent ||
		len(output.Text) > 256 || len(output.Images) != 0 ||
		(len(output.StructuredContent) > 0 && (len(output.StructuredContent) > 64<<10 || !validCloudDSHMCPJSON(output.StructuredContent))) {
		return nil
	}
	var content strings.Builder
	for _, text := range output.Text {
		if len(text) > 64<<10 || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
			return nil
		}
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(text)
	}
	if len(output.StructuredContent) > 0 {
		if !validCloudDSHMCPJSON(output.StructuredContent) {
			return nil
		}
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.Write(output.StructuredContent)
	}
	full := content.String()
	if full == "" || cloudDSHHistoryCredentialPattern.MatchString(full) {
		return nil
	}
	preview := full
	truncated := len(preview) > maxTaskStepContentEvidencePreviewBytes
	if truncated {
		preview = preview[:maxTaskStepContentEvidencePreviewBytes]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
	}
	digest := sha256.Sum256([]byte(full))
	return &TaskStepContentEvidence{SHA256: hex.EncodeToString(digest[:]), Preview: preview, Truncated: truncated}
}

func taskToolShellExecutionEvidence(name string, result AgentToolResult) *TaskStepShellExecutionEvidence {
	if name != runtimeToolShellExec || (result.Status != AgentToolSucceeded && result.Status != AgentToolFailed) {
		return nil
	}
	var shell taskStepShellOutput
	if decodeStrictRuntimeResult([]byte(result.Output), &shell) != nil || shell.ExitCode < 0 || shell.ExitCode > 255 ||
		(result.Status == AgentToolSucceeded) == (shell.ExitCode != 0 || shell.TimedOut) {
		return nil
	}
	stdoutBytes, stderrBytes := len(shell.Stdout), len(shell.Stderr)
	if stdoutBytes > maxAgentToolOutputBytes-stderrBytes || stderrBytes > maxAgentToolOutputBytes {
		return nil
	}
	return &TaskStepShellExecutionEvidence{ExitCode: shell.ExitCode, TimedOut: shell.TimedOut,
		StdoutBytes: stdoutBytes, StderrBytes: stderrBytes, StdoutTruncated: shell.StdoutTruncated,
		StderrTruncated: shell.StderrTruncated}
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
	execution, executionTruncated := taskToolExecutionDetails(activity)
	detail := TaskStepDetailDocument{Object: object, Input: taskToolInputSummary(activity.call.Name, activity.call.Arguments),
		Result: taskToolResultSummary(step), ShellExecution: activity.shellExecution, ContentEvidence: activity.contentEvidence,
		SourceLocations: activity.sourceLocations, Execution: execution, ExecutionWorkspaceID: taskToolExecutionWorkspaceID(activity)}
	for index := range detail.SourceLocations {
		detail.SourceLocations[index].ID = taskStepSourceLocationID(activity.current.OwnerID, activity.current.ID, step.ID,
			*activity.current.CurrentRevision, activity.call.CallID, detail.SourceLocations[index])
		if detail.SourceLocations[index].ID == "" {
			return nil, fmt.Errorf("task tool source location identity is invalid")
		}
	}
	truncated := executionTruncated
	detail.Object, truncated = truncateTaskDetail(detail.Object, truncated)
	detail.Input, truncated = truncateTaskDetail(detail.Input, truncated)
	detail.Result, truncated = truncateTaskDetail(detail.Result, truncated)
	phase := string(step.Status)
	if step.Phase == "prepare" {
		phase = step.Phase
	}
	if activity.phaseOverride != "" {
		phase = activity.phaseOverride
	}
	progress := &TaskToolCallProgress{AgentSessionID: fence.AgentSessionID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion,
		TaskRevision: *activity.current.CurrentRevision, CallKey: activity.call.CallID, ToolName: activity.call.Name,
		RequestDigest: append([]byte(nil), activity.digest...), Phase: phase, DetailState: "not_recorded",
		Truncated: truncated}
	if activity.contentEvidence != nil {
		contentDigest, err := hex.DecodeString(activity.contentEvidence.SHA256)
		if err != nil || len(contentDigest) != sha256.Size {
			return nil, fmt.Errorf("task tool content evidence digest is invalid")
		}
		progress.ContentEvidenceSHA256 = contentDigest
	}
	if step.Status == task.StepFailed && phase == "prepare" &&
		(activity.failureReason == taskToolInvalidArgumentsFailureReason || activity.failureReason == taskToolAdvisoryGateFailureReason) {
		// The same Agent can correct the call. Do not expose a separate
		// user-triggered step retry for this pre-dispatch result.
	} else if step.Status == task.StepFailed && activity.failureReason == "connector_rate_limited" {
		progress.FailureReason, progress.Retryable = activity.failureReason, true
	} else if step.Status == task.StepFailed && activity.failureReason == taskToolExecutionFailureReason && phase == "execute" {
		progress.FailureReason = activity.failureReason
		if reason := failingTestFailureReason(activity); reason != "" {
			progress.FailureReason = reason
		}
	} else if step.Status == task.StepFailed && phase == "prepare" {
		progress.FailureReason, progress.Retryable = "not_dispatched", true
	} else if step.Status == task.StepSucceeded {
		// Passing-test record (E07): a succeeded shell run of a test command
		// is stamped so Complete() can prove the failing reproduction test
		// turned green and the full suite passed from real shell runs, not
		// from a model claim. An unparsable run stamps nothing and the gate
		// fails closed.
		progress.FailureReason = testPassReason(activity)
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
	return ok || IsMCPConnectorStepRetryTool(name)
}

func IsMCPConnectorStepRetryTool(name string) bool {
	return name == cloudDSHMCPCallToolName
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
				Query      json.RawMessage `json:"query"`
				TimeRange  json.RawMessage `json:"time_range"`
				Categories json.RawMessage `json:"categories"`
			}
			if json.Unmarshal(arguments, &input) == nil {
				if query := decodeSafeString(input.Query, 512); query != "" {
					newsCategory := decodeSafeString(input.Categories, 16) == "news"
					if timeRange := decodeSafeString(input.TimeRange, 16); timeRange != "" {
						details := []string{"time range: " + timeRange}
						if newsCategory {
							details = append([]string{"category: news"}, details...)
						}
						return "Search query: " + query + " (" + strings.Join(details, "; ") + ")."
					}
					if newsCategory {
						return "Search query: " + query + " (category: news)."
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
