package app

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxTaskStepExecutionTextBytes       = 8192
	maxTaskStepExecutionOutputBytes     = 192 << 10
	maxTaskStepExecutionOutputJSONBytes = 192 << 10
)

type taskStepShellOutput struct {
	ExitCode                      int                                  `json:"exit_code"`
	TimedOut                      bool                                 `json:"timed_out"`
	Stdout                        string                               `json:"stdout"`
	Stderr                        string                               `json:"stderr"`
	StdoutTruncated               bool                                 `json:"stdout_truncated"`
	StderrTruncated               bool                                 `json:"stderr_truncated"`
	TaskID                        string                               `json:"task_id,omitempty"`
	WorkspaceID                   string                               `json:"workspace_id,omitempty"`
	BindingVersion                int64                                `json:"binding_version,omitempty"`
	WorkspaceViewID               string                               `json:"workspace_view_id,omitempty"`
	WorkspaceCheckpointRevision   *int64                               `json:"workspace_checkpoint_revision,omitempty"`
	WorkspaceManifestSHA256Before string                               `json:"workspace_manifest_sha256_before,omitempty"`
	WorkspaceManifestSHA256After  string                               `json:"workspace_manifest_sha256_after,omitempty"`
	WorkspaceSnapshotSHA256Before string                               `json:"workspace_snapshot_sha256_before,omitempty"`
	WorkspaceSnapshotSHA256After  string                               `json:"workspace_snapshot_sha256_after,omitempty"`
	VerificationCheck             *TaskValidationCheckExecutionReceipt `json:"verification_check,omitempty"`
}

// TaskValidationCheckExecutionReceipt is written by the server-side tool
// broker into the durable operation result after it matches a typed plan
// check to the exact shell command and validates the Runtime Workspace facts.
// It contains identifiers and digests only, never command output or file data.
type TaskValidationCheckExecutionReceipt struct {
	Version        int    `json:"version"`
	TaskRevision   int64  `json:"task_revision"`
	PlanRevision   int64  `json:"plan_revision"`
	CheckID        string `json:"check_id"`
	Kind           string `json:"kind"`
	Basis          string `json:"basis"`
	CommandSHA256  string `json:"command_sha256"`
	CheckSHA256    string `json:"check_sha256"`
	CoverageSHA256 string `json:"coverage_sha256"`
	RequestSHA256  string `json:"request_sha256"`
}

var taskStepSecretAssignment = regexp.MustCompile("(?i)((?:--)?(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|authorization|credential|client[_-]?secret|private[_-]?key|access[_-]?key|refresh[_-]?token)[\"']?\\s*(?:=|:|\\s)\\s*)(?:\"[^\"]*\"|'[^']*'|[^\\s,;}&]+)")
var taskStepSecretURL = regexp.MustCompile("(?i)(https?://)[^\\s/@]+:[^\\s/@]+@")
var taskStepPrivateKey = regexp.MustCompile("(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----")
var taskStepSensitiveCommand = regexp.MustCompile("(?i)(?:\\b(?:printenv|env)\\b|\\b(?:aws\\s+(?:configure|sts|iam|secretsmanager|sso)|gcloud\\s+auth|az\\s+account\\s+get-access-token|security\\s+find-generic-password|ssh-add\\s+-L)\\b|(?:^|[\\s/])(?:\\.env|\\.ssh|\\.gnupg|\\.aws|\\.azure|\\.kube|\\.docker|\\.netrc|\\.git-credentials|\\.config/(?:gh|gcloud))(?:[./\\s]|$)|(?:^|[\\s/])[^\\s]+\\.(?:pem|p12|pfx|keystore|key)(?:[\\s]|$)|\\$\\{?[a-z_][a-z0-9_]*(?:password|passwd|secret|token|key|credential))")

// Timing covers only a call actually dispatched to the Host, never recovery.
func withTaskToolExecutionStart(ctx context.Context) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return ctx
	}
	activity.executionStart, activity.executionMS = time.Now(), nil
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func withTaskToolExecutionEnd(ctx context.Context) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok || activity.executionStart.IsZero() {
		return ctx
	}
	milliseconds := time.Since(activity.executionStart).Milliseconds()
	if milliseconds < 0 {
		return ctx
	}
	activity.executionMS = &milliseconds
	activity.executionStart = time.Time{}
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func validTaskStepExecution(value *TaskStepExecution) bool {
	if value == nil {
		return true
	}
	for _, field := range []*string{value.Location, value.Parameters, value.Command, value.WorkingDirectory} {
		if field != nil && (!utf8.ValidString(*field) || len(*field) > maxTaskStepExecutionTextBytes || strings.ContainsRune(*field, '\x00')) {
			return false
		}
	}
	outputJSONBytes := 0
	for _, field := range []*string{value.Stdout, value.Stderr} {
		if field != nil && (!utf8.ValidString(*field) || len(*field) > maxTaskStepExecutionOutputBytes || strings.ContainsRune(*field, '\x00')) {
			return false
		}
		if field != nil {
			encoded, err := json.Marshal(*field)
			if err != nil || len(encoded) > maxTaskStepExecutionOutputJSONBytes-outputJSONBytes {
				return false
			}
			outputJSONBytes += len(encoded)
		}
	}
	if value.DurationMS != nil && *value.DurationMS < 0 ||
		value.ExitCode != nil && (*value.ExitCode < 0 || *value.ExitCode > 255) {
		return false
	}
	for _, size := range []*int{value.StdoutTotalBytes, value.StderrTotalBytes, value.StdoutOmittedBytes, value.StderrOmittedBytes} {
		if size != nil && (*size < 0 || *size > maxAgentToolOutputBytes) {
			return false
		}
	}
	if !validTaskStepOutputTruncation(value.StdoutTruncated, value.StdoutRuntimeTruncated, value.StdoutDetailTruncated) ||
		!validTaskStepOutputTruncation(value.StderrTruncated, value.StderrRuntimeTruncated, value.StderrDetailTruncated) {
		return false
	}
	return true
}

func validTaskStepOutputTruncation(combined, runtime, detail *bool) bool {
	if runtime == nil && detail == nil {
		return true
	}
	if combined == nil {
		return false
	}
	want := runtime != nil && *runtime || detail != nil && *detail
	return *combined == want
}

func taskToolExecutionDetails(activity cloudDSHToolActivity) (*TaskStepExecution, bool) {
	var result TaskStepExecution
	var truncated bool
	_, hostTool := runtimeToolMappingByName(activity.call.Name)
	if hostTool && activity.current.HostID != "" {
		location := "云端电脑"
		result.Location = &location
	} else if activity.current.WorkspaceID != "" && strings.HasPrefix(activity.call.Name, "morde_workspace_") {
		location := "云端工作区"
		result.Location = &location
	}
	result.DurationMS = activity.executionMS
	if value := taskToolExecutionParameters(activity.call.Name, activity.call.Arguments); value != "" {
		result.Parameters = &value
	}
	if activity.call.Name == runtimeToolShellExec {
		var arguments struct {
			Command string `json:"command"`
			CWD     string `json:"cwd"`
		}
		commandSensitive := true
		if json.Unmarshal([]byte(activity.call.Arguments), &arguments) == nil {
			command, changed, cut := taskStepDisclosureText(arguments.Command)
			if command != "" {
				result.Command = &command
			}
			commandSensitive = changed || taskStepSensitiveCommand.MatchString(arguments.Command)
			truncated = truncated || cut
			if arguments.CWD != "" && !cloudDSHWorkspacePathIsSensitive(arguments.CWD) {
				cwd, _, cut := taskStepDisclosureText(arguments.CWD)
				if cwd != "" {
					result.WorkingDirectory = &cwd
				}
				truncated = truncated || cut
			}
		}
		if evidence, output := activity.shellExecution, activity.shellOutput; evidence != nil && output != nil {
			result.ExitCode, result.TimedOut = &evidence.ExitCode, &evidence.TimedOut
			if commandSensitive {
				hidden := "[内容已隐藏：命令可能包含凭据]"
				result.Stdout, result.Stderr = &hidden, &hidden
				stdoutDetailTruncated, stderrDetailTruncated := false, false
				result.StdoutRuntimeTruncated, result.StderrRuntimeTruncated = &output.StdoutTruncated, &output.StderrTruncated
				result.StdoutDetailTruncated, result.StderrDetailTruncated = &stdoutDetailTruncated, &stderrDetailTruncated
				stdoutTruncated := output.StdoutTruncated
				stderrTruncated := output.StderrTruncated
				result.StdoutTruncated, result.StderrTruncated = &stdoutTruncated, &stderrTruncated
			} else {
				stdout, stderr := taskStepOutputDisclosurePair(output.Stdout, output.StdoutTruncated, output.Stderr,
					output.StderrTruncated, maxTaskStepExecutionOutputJSONBytes)
				result.Stdout, result.StdoutTotalBytes, result.StdoutOmittedBytes = &stdout.text, stdout.totalBytes, stdout.omittedBytes
				result.Stderr, result.StderrTotalBytes, result.StderrOmittedBytes = &stderr.text, stderr.totalBytes, stderr.omittedBytes
				result.StdoutTruncated, result.StdoutRuntimeTruncated, result.StdoutDetailTruncated =
					&stdout.truncated, &stdout.runtimeTruncated, &stdout.detailTruncated
				result.StderrTruncated, result.StderrRuntimeTruncated, result.StderrDetailTruncated =
					&stderr.truncated, &stderr.runtimeTruncated, &stderr.detailTruncated
			}
		}
	}
	if result.Location == nil && result.Parameters == nil && result.Command == nil && result.WorkingDirectory == nil &&
		result.DurationMS == nil && result.ExitCode == nil && result.Stdout == nil && result.Stderr == nil {
		return nil, truncated
	}
	if taskStepExecutionRequiresWorkspace(&result) && taskToolExecutionWorkspaceID(activity) == "" {
		return nil, truncated
	}
	return &result, truncated
}

func taskStepExecutionRequiresWorkspace(value *TaskStepExecution) bool {
	return value != nil && (value.Parameters != nil || value.Command != nil || value.WorkingDirectory != nil ||
		value.ExitCode != nil || value.TimedOut != nil || value.Stdout != nil || value.Stderr != nil ||
		value.StdoutTotalBytes != nil || value.StderrTotalBytes != nil || value.StdoutOmittedBytes != nil ||
		value.StderrOmittedBytes != nil || value.StdoutTruncated != nil || value.StderrTruncated != nil ||
		value.StdoutRuntimeTruncated != nil || value.StderrRuntimeTruncated != nil ||
		value.StdoutDetailTruncated != nil || value.StderrDetailTruncated != nil)
}

func taskToolExecutionWorkspaceID(activity cloudDSHToolActivity) string {
	name := activity.call.Name
	switch name {
	case runtimeToolShellExec, runtimeToolFileRead, runtimeToolFileWrite, runtimeToolFileList, runtimeToolFileFind:
	case cloudDSHWorkspaceReadToolName, cloudDSHWorkspaceWriteToolName, cloudDSHWorkspaceListToolName, cloudDSHWorkspaceSearchToolName:
	default:
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(activity.call.Arguments), &fields) != nil {
		fields = nil
	}
	workspaceID := decodeSafeString(fields["workspace_id"], 32)
	if workspaceID == "" {
		workspaceID = activity.current.WorkspaceID
	}
	if !validRuntimeWorkspaceID(workspaceID) {
		return ""
	}
	if activity.current.WorkspaceID != "" && activity.current.WorkspaceID != workspaceID {
		return ""
	}
	return workspaceID
}

func taskToolExecutionParameters(name, raw string) string {
	switch name {
	case runtimeToolFileRead, runtimeToolFileWrite, runtimeToolFileList, runtimeToolFileFind,
		cloudDSHWorkspaceReadToolName, cloudDSHWorkspaceWriteToolName, cloudDSHWorkspaceListToolName, cloudDSHWorkspaceSearchToolName:
	default:
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return ""
	}
	selected := make(map[string]any)
	if path := decodeSafeString(fields["path"], 2048); path != "" && !cloudDSHWorkspacePathIsSensitive(path) {
		cleaned, _, _ := taskStepDisclosureText(path)
		selected["path"] = cleaned
	}
	for _, key := range []string{"start_line", "end_line"} {
		var number int64
		if len(fields[key]) > 0 && json.Unmarshal(fields[key], &number) == nil && number > 0 {
			selected[key] = number
		}
	}
	if len(selected) == 0 {
		return ""
	}
	encoded, err := json.Marshal(selected)
	if err != nil || len(encoded) > maxTaskStepExecutionTextBytes {
		return ""
	}
	return string(encoded)
}

func taskStepOutputDisclosure(raw string, hostTruncated bool) (*string, *int, *int, *bool) {
	result := taskStepOutputDisclosureBounded(raw, hostTruncated, maxTaskStepExecutionTextBytes)
	return &result.text, result.totalBytes, result.omittedBytes, &result.truncated
}

type taskStepOutputEvidence struct {
	text             string
	totalBytes       *int
	omittedBytes     *int
	truncated        bool
	runtimeTruncated bool
	detailTruncated  bool
	sanitizedBytes   int
	redacted         bool
}

func taskStepOutputDisclosureBounded(raw string, runtimeTruncated bool, maxBytes int) taskStepOutputEvidence {
	clean, redacted := taskStepSanitizedText(raw)
	text, detailTruncated := truncateTaskStepDisclosure(clean, maxBytes)
	result := taskStepOutputEvidence{
		text: text, truncated: runtimeTruncated || detailTruncated,
		runtimeTruncated: runtimeTruncated, detailTruncated: detailTruncated, sanitizedBytes: len(clean), redacted: redacted,
	}
	if !runtimeTruncated && !redacted {
		total := len(raw)
		result.totalBytes = &total
		if detailTruncated {
			omitted := len(clean) - len(text)
			result.omittedBytes = &omitted
		}
	}
	return result
}

func taskStepOutputDisclosurePair(stdout string, stdoutRuntimeTruncated bool, stderr string, stderrRuntimeTruncated bool,
	maxJSONBytes int) (taskStepOutputEvidence, taskStepOutputEvidence) {
	stdoutEvidence := taskStepOutputDisclosureBounded(stdout, stdoutRuntimeTruncated, maxTaskStepExecutionOutputBytes)
	stderrEvidence := taskStepOutputDisclosureBounded(stderr, stderrRuntimeTruncated, maxTaskStepExecutionOutputBytes)
	stdoutJSONBytes := taskStepJSONStringBytes(stdoutEvidence.text)
	stderrJSONBytes := taskStepJSONStringBytes(stderrEvidence.text)
	totalJSONBytes := stdoutJSONBytes + stderrJSONBytes
	if totalJSONBytes <= maxJSONBytes || maxJSONBytes < 4 {
		return stdoutEvidence, stderrEvidence
	}
	stdoutBodyBytes, stderrBodyBytes := stdoutJSONBytes-2, stderrJSONBytes-2
	remainingBodyBytes := maxJSONBytes - 4
	totalBodyBytes := stdoutBodyBytes + stderrBodyBytes
	stdoutBudget := 2
	if totalBodyBytes > 0 {
		stdoutBudget += remainingBodyBytes * stdoutBodyBytes / totalBodyBytes
	}
	stderrBudget := maxJSONBytes - stdoutBudget
	stdoutEvidence = truncateTaskStepOutputEvidenceJSON(stdoutEvidence, stdoutBudget)
	stderrEvidence = truncateTaskStepOutputEvidenceJSON(stderrEvidence, stderrBudget)
	return stdoutEvidence, stderrEvidence
}

func taskStepJSONStringBytes(value string) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return maxTaskStepExecutionOutputJSONBytes + 1
	}
	return len(encoded)
}

func truncateTaskStepOutputEvidenceJSON(value taskStepOutputEvidence, maxJSONBytes int) taskStepOutputEvidence {
	text, truncated := truncateTaskStepDisclosureJSON(value.text, maxJSONBytes)
	if !truncated {
		return value
	}
	value.text = text
	value.detailTruncated = true
	value.truncated = true
	if value.totalBytes != nil && !value.redacted && !value.runtimeTruncated {
		omitted := value.sanitizedBytes - len(text)
		if omitted < 0 {
			omitted = 0
		}
		value.omittedBytes = &omitted
	} else {
		value.omittedBytes = nil
	}
	return value
}

func truncateTaskStepDisclosureJSON(value string, maxJSONBytes int) (string, bool) {
	if taskStepJSONStringBytes(value) <= maxJSONBytes {
		return value, false
	}
	if maxJSONBytes < 2 {
		return "", value != ""
	}
	boundaries := []int{0}
	for offset := 0; offset < len(value); {
		_, width := utf8.DecodeRuneInString(value[offset:])
		offset += width
		boundaries = append(boundaries, offset)
	}
	low, high := 0, len(boundaries)-1
	for low < high {
		middle := low + (high-low+1)/2
		if taskStepJSONStringBytes(value[:boundaries[middle]]) <= maxJSONBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return value[:boundaries[low]], true
}

// Redaction happens before truncation so a long secret cannot be partially shown.
func taskStepDisclosureText(raw string) (string, bool, bool) {
	clean, redacted := taskStepSanitizedText(raw)
	text, truncated := truncateTaskStepDisclosure(clean, maxTaskStepExecutionTextBytes)
	return text, redacted, truncated
}

func taskStepSanitizedText(raw string) (string, bool) {
	clean := strings.ToValidUTF8(raw, "�")
	clean = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, clean)
	clean = taskStepPrivateKey.ReplaceAllString(clean, "[已隐藏私钥]")
	clean = cloudDSHHistoryCredentialPattern.ReplaceAllString(clean, "[已隐藏凭据]")
	clean = taskStepSecretURL.ReplaceAllString(clean, "$1[已隐藏]@")
	clean = taskStepSecretAssignment.ReplaceAllString(clean, "$1[已隐藏]")
	return clean, clean != raw
}

func truncateTaskStepDisclosure(value string, maxBytes int) (string, bool) {
	if maxBytes < 1 {
		return "", value != ""
	}
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}
