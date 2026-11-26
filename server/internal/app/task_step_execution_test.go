package app

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestTaskStepExecutionIsSealedAndAvailableOnlyToTaskOwner(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{12}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const workspaceID = "w1234567"
	revision := int64(2)
	current := task.Task{ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, HostID: "h1234567",
		WorkspaceID: workspaceID, WorkspaceOwnerID: ownerID,
		AgentInstanceID: "agent_1", AgentGeneration: 1, CurrentRevision: &revision,
		Authorization: task.Authorization{Version: 1}, Lease: task.Lease{Owner: "worker", Version: 1}}
	call := ModelToolCall{CallID: "shell-call", Name: runtimeToolShellExec,
		Arguments: `{"workspace_id":"w1234567","command":"printf hello","cwd":"/workspace/demo"}`,
		TaskFence: &ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: ownerID, LeaseOwner: "worker",
			LeaseVersion: 1, AgentInstanceID: "agent_1", AgentGeneration: 1, AgentSessionID: "session_1", AuthorizationVersion: 1}}
	capturedOutput := strings.Repeat("captured-output-", 1024)
	runtimeOutput, err := json.Marshal(taskStepShellOutput{ExitCode: 0, Stdout: capturedOutput})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, sealer)
	ctx = withTaskToolExecutionStart(ctx)
	ctx = withTaskToolExecutionEnd(ctx)
	ctx = withTaskToolShellExecutionEvidence(ctx, call.Name, AgentToolResult{Status: AgentToolSucceeded,
		Output: string(runtimeOutput)})
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		t.Fatal("fenced tool call has no activity")
	}
	progress, err := taskToolCallProgress(activity, task.Step{ID: stepID, Status: task.StepSucceeded, Summary: "Completed."})
	if err != nil || progress == nil || progress.DetailState != "available" {
		t.Fatalf("tool activity: %+v, %v", progress, err)
	}
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: current, Steps: []task.Step{{ID: stepID, ToolName: runtimeToolShellExec,
			Status: task.StepSucceeded, TaskRevision: &revision, DetailState: "available"}}},
		workspace: workspace.Workspace{ID: workspaceID, OwnerID: ownerID, State: workspace.StateReady},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			Ciphertext: progress.DetailCiphertext, SHA256: progress.DetailSHA256, SizeBytes: progress.DetailSizeBytes},
	}
	authorizer := &taskAuthorizerStub{}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || detail.Execution == nil || detail.Execution.Command == nil || *detail.Execution.Command != "printf hello" ||
		detail.Execution.WorkingDirectory == nil || *detail.Execution.WorkingDirectory != "/workspace/demo" ||
		detail.Execution.Stdout == nil || *detail.Execution.Stdout != capturedOutput || len(*detail.Execution.Stdout) <= maxTaskStepExecutionTextBytes ||
		detail.Execution.StdoutTotalBytes == nil || *detail.Execution.StdoutTotalBytes != len(capturedOutput) ||
		detail.Execution.StdoutRuntimeTruncated == nil || *detail.Execution.StdoutRuntimeTruncated ||
		detail.Execution.StdoutDetailTruncated == nil || *detail.Execution.StdoutDetailTruncated ||
		detail.Execution.ExitCode == nil || *detail.Execution.ExitCode != 0 || detail.Execution.DurationMS == nil {
		t.Fatalf("authorized detail output length=%d, err=%v", func() int {
			if detail.Execution == nil || detail.Execution.Stdout == nil {
				return 0
			}
			return len(*detail.Execution.Stdout)
		}(), err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil || !bytes.Contains(encoded, []byte(`"execution"`)) || bytes.Contains(encoded, []byte(`"shell_execution"`)) {
		t.Fatalf("public detail has incorrect execution projection: %s, %v", encoded, err)
	}
	other := identity.Principal{UserID: "u8765432", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.GetStepDetail(context.Background(), other, taskID, stepID); err == nil {
		t.Fatal("another user could read the Task execution")
	}
	store.details.Steps[0].Status = task.StepUnknown
	unknown, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || unknown.Result == nil || !strings.Contains(*unknown.Result, "could not be confirmed") ||
		unknown.Execution == nil || unknown.Execution.Stdout != nil || unknown.Execution.ExitCode != nil ||
		unknown.Execution.StdoutTotalBytes != nil || unknown.Execution.StdoutTruncated != nil ||
		unknown.Execution.StdoutRuntimeTruncated != nil || unknown.Execution.StdoutDetailTruncated != nil {
		t.Fatalf("unknown step retained a success summary: %+v, %v", unknown, err)
	}
	store.details.Steps[0].Status = task.StepCancelled
	cancelled, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || cancelled.Result == nil || !strings.Contains(*cancelled.Result, "was cancelled") ||
		cancelled.Execution == nil || cancelled.Execution.Stdout != nil || cancelled.Execution.ExitCode != nil ||
		cancelled.Execution.StdoutTotalBytes != nil || cancelled.Execution.StdoutTruncated != nil ||
		cancelled.Execution.StdoutRuntimeTruncated != nil || cancelled.Execution.StdoutDetailTruncated != nil {
		t.Fatalf("cancelled step retained an unconfirmed output: %+v, %v", cancelled, err)
	}
}

func TestTaskToolExecutionDetailsSummarizeBrowserActionsWithoutSensitiveData(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		want      map[string]any
		forbid    []string
	}{
		{name: runtimeToolBrowserOpenTab,
			arguments: `{"url":"https://Example.test/private/path?access_token=synthetic-secret#frag"}`,
			want:      map[string]any{"site": "https://example.test"},
			forbid:    []string{"private", "access_token", "synthetic-secret", "frag"}},
		{name: runtimeToolBrowserClick,
			arguments: `{"target_id":"tab_private","role":"button","name":"Continue"}`,
			want:      map[string]any{"role": "button", "element": "Continue"},
			forbid:    []string{"tab_private"}},
		{name: runtimeToolBrowserType,
			arguments: `{"target_id":"tab_private","role":"textbox","name":"Search","text":"session_token=synthetic-secret"}`,
			want:      map[string]any{"role": "textbox", "element": "Search", "text_omitted": true},
			forbid:    []string{"tab_private", "session_token", "synthetic-secret"}},
		{name: runtimeToolBrowserPressKey,
			arguments: `{"target_id":"tab_private","key":"Enter"}`,
			want:      map[string]any{"key": "Enter"},
			forbid:    []string{"tab_private"}},
		{name: runtimeToolBrowserNavigate,
			arguments: `{"target_id":"tab_private","url":"https://user:password@example.test/path"}`,
			want:      map[string]any{},
			forbid:    []string{"user", "password", "example.test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail, _ := taskToolExecutionDetails(cloudDSHToolActivity{
				call:    ModelToolCall{Name: test.name, Arguments: test.arguments},
				current: task.Task{HostID: "h1234567"},
			})
			if len(test.want) == 0 {
				if detail == nil || detail.Parameters != nil {
					t.Fatalf("browser URL with credentials was not omitted: %+v", detail)
				}
				return
			}
			if detail == nil || detail.Parameters == nil {
				t.Fatalf("browser execution parameters were not recorded: name=%q direct=%q detail=%+v", test.name,
					taskToolExecutionParameters(test.name, test.arguments), detail)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(*detail.Parameters), &got); err != nil {
				t.Fatalf("decode recorded parameters %q: %v", *detail.Parameters, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("recorded browser parameters = %#v, want %#v", got, test.want)
			}
			for _, secret := range test.forbid {
				if strings.Contains(*detail.Parameters, secret) {
					t.Fatalf("browser execution parameters leaked %q: %s", secret, *detail.Parameters)
				}
			}
			if !validTaskStepExecution(detail) {
				t.Fatalf("browser execution parameters violate detail bounds: %+v", detail)
			}
			if !validTaskStepDetailDocument(TaskStepDetailDocument{Object: test.name, Execution: detail}) {
				t.Fatalf("browser action detail required unrelated Workspace state: %+v", detail)
			}
		})
	}
}

func TestWorkspaceSearchExecutionParametersPreserveQueryAndOmitCredentials(t *testing.T) {
	const workspaceID = "w1234567"
	const query = "authorization check"
	activity := cloudDSHToolActivity{
		call: ModelToolCall{Name: cloudDSHWorkspaceSearchToolName,
			Arguments: `{"expected_revision":4,"query":"` + query + `"}`},
		current: task.Task{WorkspaceID: workspaceID},
	}
	detail, _ := taskToolExecutionDetails(activity)
	if detail == nil || detail.Parameters == nil {
		t.Fatalf("Workspace search query missing from execution detail: %+v", detail)
	}
	var parameters map[string]any
	if err := json.Unmarshal([]byte(*detail.Parameters), &parameters); err != nil {
		t.Fatalf("decode execution parameters %q: %v", *detail.Parameters, err)
	}
	if parameters["query"] != query {
		t.Fatalf("search query projection = %#v, want %q", parameters["query"], query)
	}
	if !taskStepExecutionRequiresWorkspace(cloudDSHWorkspaceSearchToolName, detail) {
		t.Fatal("Workspace search parameters bypassed the Workspace evidence gate")
	}
	if !validTaskStepDetailDocument(TaskStepDetailDocument{Object: cloudDSHWorkspaceSearchToolName,
		Execution: detail, ExecutionWorkspaceID: workspaceID}) {
		t.Fatal("authorized Workspace search detail did not satisfy the evidence boundary")
	}

	for _, secretQuery := range []string{
		"api_key=synthetic-secret-value",
		"Bearer " + strings.Repeat("A", 24),
		"https://user:password@example.test/private",
		"https://example.test/search?api_key=synthetic-secret-value",
	} {
		encoded, err := json.Marshal(map[string]string{"query": secretQuery})
		if err != nil {
			t.Fatal(err)
		}
		if got := taskToolExecutionParameters(cloudDSHWorkspaceSearchToolName, string(encoded)); got != "" {
			t.Fatalf("credential-bearing Workspace query was recorded: %q", got)
		}
	}
	for _, raw := range []string{
		`{"query":"line1\nline2"}`,
		`{"query":"` + strings.Repeat("q", 161) + `"}`,
	} {
		if got := taskToolExecutionParameters(cloudDSHWorkspaceSearchToolName, raw); got != "" {
			t.Fatalf("unsafe or overlong Workspace query was recorded: %q", got)
		}
	}
	boundary := strings.Repeat("q", 160)
	boundaryJSON := `{"query":"` + boundary + `"}`
	if got := taskToolExecutionParameters(cloudDSHWorkspaceSearchToolName, boundaryJSON); !strings.Contains(got, boundary) {
		t.Fatalf("160-rune Workspace query should be retained: %q", got)
	}
}

func TestBrowserExecutionParametersDoNotBypassWorkspaceEvidenceGate(t *testing.T) {
	safeBrowserDetail := TaskStepDetailDocument{Object: runtimeToolBrowserOpenTab,
		Execution: &TaskStepExecution{Parameters: stringPointer(`{"site":"https://example.test"}`)}}
	if !validTaskStepDetailDocument(safeBrowserDetail) {
		t.Fatal("sanitized browser parameters should not require an unrelated Workspace")
	}
	if !taskStepExecutionRequiresWorkspace(runtimeToolBrowserScreenshot,
		&TaskStepExecution{Parameters: stringPointer(`{"site":"https://example.test"}`)}) {
		t.Fatal("unprojected browser tool parameters unexpectedly bypassed Workspace visibility")
	}

	stdout := "unexpected output"
	stdoutBytes := len(stdout)
	outputFlags := false
	unexpectedBrowserOutput := TaskStepDetailDocument{Object: runtimeToolBrowserOpenTab,
		ShellExecution: &TaskStepShellExecutionEvidence{ExitCode: 0, StdoutBytes: stdoutBytes},
		Execution: &TaskStepExecution{Stdout: &stdout, StdoutTotalBytes: &stdoutBytes,
			StdoutTruncated: &outputFlags, StdoutRuntimeTruncated: &outputFlags, StdoutDetailTruncated: &outputFlags}}
	if validTaskStepDetailDocument(unexpectedBrowserOutput) {
		t.Fatal("browser output evidence must not bypass the Workspace evidence gate")
	}
	if !taskStepExecutionRequiresWorkspace(runtimeToolBrowserOpenTab, unexpectedBrowserOutput.Execution) {
		t.Fatal("unexpected browser output evidence did not require Workspace visibility")
	}
	unexpectedBrowserOutput.ExecutionWorkspaceID = "w1234567"
	if !validTaskStepDetailDocument(unexpectedBrowserOutput) {
		t.Fatal("browser output evidence with an explicit Workspace binding should pass structural validation")
	}
}

func TestTaskStepExecutionRedactsSecretsAndReportsOnlyKnownTruncation(t *testing.T) {
	value := strings.Repeat("a", maxTaskStepExecutionTextBytes-1) + "界"
	output, total, omitted, truncated := taskStepOutputDisclosure(value, false)
	if output == nil || !utf8.ValidString(*output) || total == nil || *total != len(value) ||
		omitted == nil || *omitted != len("界") || truncated == nil || !*truncated {
		t.Fatalf("UTF-8 truncation: total=%v omitted=%v truncated=%v", total, omitted, truncated)
	}
	_, total, omitted, truncated = taskStepOutputDisclosure("partial", true)
	if total != nil || omitted != nil || truncated == nil || !*truncated {
		t.Fatal("Host-truncated output reported an invented total")
	}
	secret := "synthetic-password-value"
	output, total, omitted, _ = taskStepOutputDisclosure("password="+secret+"\nordinary output", false)
	if output == nil || strings.Contains(*output, secret) || !strings.Contains(*output, "[已隐藏]") || total != nil || omitted != nil {
		t.Fatalf("secret output was not safely redacted: %v", output)
	}
	for _, rawOutput := range []string{
		"Authorization: Bearer " + strings.Repeat("a", 32),
		"https://user:password@example.test/path",
		"-----BEGIN RSA PRIVATE KEY-----\nprivate-key-material\n-----END RSA PRIVATE KEY-----",
	} {
		redacted, knownTotal, knownOmitted, _ := taskStepOutputDisclosure(rawOutput, false)
		if redacted == nil || *redacted == rawOutput || knownTotal != nil || knownOmitted != nil || strings.Contains(*redacted, rawOutput) {
			t.Fatalf("secret output was not fully redacted: %v", redacted)
		}
	}
	raw := `{"workspace_id":"w1234567","command":"cat ~/.ssh/id_rsa","cwd":"/workspace/demo"}`
	result := AgentToolResult{Status: AgentToolSucceeded,
		Output: `{"exit_code":0,"timed_out":false,"stdout":"private-output","stderr":"","stdout_truncated":false,"stderr_truncated":false}`}
	var shell taskStepShellOutput
	if err := json.Unmarshal([]byte(result.Output), &shell); err != nil {
		t.Fatal(err)
	}
	detail, _ := taskToolExecutionDetails(cloudDSHToolActivity{call: ModelToolCall{Name: runtimeToolShellExec, Arguments: raw},
		shellExecution: taskToolShellExecutionEvidence(runtimeToolShellExec, result), shellOutput: &shell})
	if detail == nil || detail.Stdout == nil || strings.Contains(*detail.Stdout, "private-output") ||
		detail.StdoutTotalBytes != nil {
		t.Fatalf("sensitive command exposed output: %+v", detail)
	}
	runtimeOutput, err := json.Marshal(taskStepShellOutput{ExitCode: 0, Stdout: "partial", StdoutTruncated: true})
	if err != nil {
		t.Fatal(err)
	}
	truncatedResult := AgentToolResult{Status: AgentToolSucceeded, Output: string(runtimeOutput)}
	var truncatedShell taskStepShellOutput
	if err := json.Unmarshal(runtimeOutput, &truncatedShell); err != nil {
		t.Fatal(err)
	}
	truncatedDetails, _ := taskToolExecutionDetails(cloudDSHToolActivity{
		call:           ModelToolCall{Name: runtimeToolShellExec, Arguments: `{"workspace_id":"w1234567","command":"printf output"}`},
		shellExecution: taskToolShellExecutionEvidence(runtimeToolShellExec, truncatedResult), shellOutput: &truncatedShell,
	})
	if truncatedDetails == nil || truncatedDetails.Stdout == nil || *truncatedDetails.Stdout != "partial" ||
		truncatedDetails.StdoutTruncated == nil || !*truncatedDetails.StdoutTruncated ||
		truncatedDetails.StdoutRuntimeTruncated == nil || !*truncatedDetails.StdoutRuntimeTruncated ||
		truncatedDetails.StdoutDetailTruncated == nil || *truncatedDetails.StdoutDetailTruncated {
		t.Fatalf("Runtime truncation was misreported: %+v", truncatedDetails)
	}
	for _, command := range []string{"aws configure list", "gcloud auth print-access-token", "security find-generic-password -s example"} {
		arguments, err := json.Marshal(map[string]string{"workspace_id": "w1234567", "command": command})
		if err != nil {
			t.Fatal(err)
		}
		var output taskStepShellOutput
		if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
			t.Fatal(err)
		}
		detail, _ := taskToolExecutionDetails(cloudDSHToolActivity{call: ModelToolCall{Name: runtimeToolShellExec,
			Arguments: string(arguments)}, shellExecution: taskToolShellExecutionEvidence(runtimeToolShellExec, result), shellOutput: &output})
		if detail == nil || detail.Stdout == nil || strings.Contains(*detail.Stdout, "private-output") || detail.StdoutTotalBytes != nil {
			t.Fatalf("credential command %q exposed output: %+v", command, detail)
		}
	}
}

func TestTaskStepOutputDisclosureSeparatesRuntimeAndDetailTruncation(t *testing.T) {
	detailLimited := taskStepOutputDisclosureBounded("0123456789abcdef", false, 12)
	if detailLimited.text != "0123456789ab" || !detailLimited.truncated || detailLimited.runtimeTruncated ||
		!detailLimited.detailTruncated || detailLimited.totalBytes == nil || *detailLimited.totalBytes != 16 ||
		detailLimited.omittedBytes == nil || *detailLimited.omittedBytes != 4 {
		t.Fatalf("server-limited output evidence = %+v", detailLimited)
	}
	runtimeLimited := taskStepOutputDisclosureBounded("captured prefix", true, 64)
	if runtimeLimited.text != "captured prefix" || !runtimeLimited.truncated || !runtimeLimited.runtimeTruncated ||
		runtimeLimited.detailTruncated || runtimeLimited.totalBytes != nil || runtimeLimited.omittedBytes != nil {
		t.Fatalf("Runtime-limited output evidence = %+v", runtimeLimited)
	}
	stdout, stderr := taskStepOutputDisclosurePair(strings.Repeat("s", 10), false, strings.Repeat("e", 10), false, 18)
	if !stdout.detailTruncated || !stderr.detailTruncated || stdout.runtimeTruncated || stderr.runtimeTruncated ||
		taskStepJSONStringBytes(stdout.text)+taskStepJSONStringBytes(stderr.text) > 18 {
		t.Fatalf("combined server detail cap was not applied: stdout=%+v stderr=%+v", stdout, stderr)
	}
}

func TestTaskStepExecutionBoundsCombinedJSONOutput(t *testing.T) {
	stdout, stderr := strings.Repeat("\n", 60<<10), strings.Repeat("\n", 60<<10)
	value := &TaskStepExecution{Stdout: &stdout, Stderr: &stderr}
	if validTaskStepExecution(value) {
		t.Fatal("output whose JSON-escaped streams exceed the detail budget was accepted")
	}
	tooLarge := strings.Repeat("x", (64<<10)+1)
	document := TaskStepDetailDocument{Object: runtimeToolShellExec, Input: "Run command.", Result: "Completed.",
		ShellExecution: &TaskStepShellExecutionEvidence{StdoutBytes: len(tooLarge)},
		Execution:      &TaskStepExecution{Stdout: &tooLarge}, ExecutionWorkspaceID: "w1234567"}
	if validTaskStepDetailDocument(document) {
		t.Fatal("detail accepted Runtime stdout above the existing 64 KiB total output bound")
	}
}

func TestTaskServiceHidesWorkspaceExecutionAfterProjectAccessRevocation(t *testing.T) {
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const workspaceID, projectID = "w1234567", "j1234567"
	const revision = int64(4)
	sealer, err := NewSealer(bytes.Repeat([]byte{13}, 32))
	if err != nil {
		t.Fatal(err)
	}
	revisionValue := revision
	workspaceIDValue := workspaceID
	command, stdout := "printf secret-data", "secret-data"
	document := TaskStepDetailDocument{Object: runtimeToolShellExec, Input: "Run a command.", Result: "Shell completed.",
		ShellExecution: &TaskStepShellExecutionEvidence{StdoutBytes: len(stdout)},
		Execution:      &TaskStepExecution{Command: &command, Stdout: &stdout}, ExecutionWorkspaceID: workspaceIDValue}
	ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, revision, document)
	if err != nil {
		t.Fatal(err)
	}
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID, WorkspaceID: workspaceID,
			WorkspaceOwnerID: ownerID, ProjectID: projectID}, Steps: []task.Step{{ID: stepID, ToolName: runtimeToolShellExec,
			Status: task.StepSucceeded, TaskRevision: &revisionValue, DetailState: "available"}}},
		workspace: workspace.Workspace{ID: workspaceID, OwnerID: ownerID, State: workspace.StateReady},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			Ciphertext: ciphertext, SHA256: digest, SizeBytes: size},
	}
	authorizer := &taskAuthorizerStub{}
	access := &taskStepDetailProjectAccessStub{err: domain.NewError(domain.CodeForbidden, "project membership was revoked")}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer, projectAccess: access}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || detail.DetailState != "restricted" || detail.Execution != nil || detail.Object != nil ||
		detail.Result != nil || access.role != project.RoleMember {
		t.Fatalf("revoked project execution = %+v, err=%v; want restricted detail without execution", detail, err)
	}
}
