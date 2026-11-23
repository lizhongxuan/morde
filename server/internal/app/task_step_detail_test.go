package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestTaskStepDetailIsBoundToItsIdentityAndTruncatedSafely(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	document := TaskStepDetailDocument{Object: "src/main.go", Input: "Workspace request; file contents are omitted.", Result: "Read completed."}
	ciphertext, digest, size, err := sealTaskStepDetail(sealer, "u1234567", "q1234567", "s1234567", 4, document)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.OpenWithAAD(ciphertext, taskStepDetailAAD("u1234567", "q1234567", "s1234567", 4))
	if err != nil || int32(len(plain)) != size || !bytes.Equal(sha256Bytes(plain), digest) {
		t.Fatalf("open stored step detail: err=%v", err)
	}
	if _, err := sealer.OpenWithAAD(ciphertext, taskStepDetailAAD("u8765432", "q1234567", "s1234567", 4)); err == nil {
		t.Fatal("step detail ciphertext decrypted for a different owner")
	}
	var decoded TaskStepDetailDocument
	if err := json.Unmarshal(plain, &decoded); err != nil || decoded != document {
		t.Fatalf("decoded detail=%+v err=%v", decoded, err)
	}
	truncated, wasTruncated := truncateTaskDetail(strings.Repeat("a", 4096)+"界", false)
	if !wasTruncated || len(truncated) > 4096 || !utf8.ValidString(truncated) {
		t.Fatalf("truncated detail bytes=%d valid=%t truncated=%t", len(truncated), utf8.ValidString(truncated), wasTruncated)
	}
	cleaned, wasTruncated := truncateTaskDetail("unsafe\xfftext", false)
	if !wasTruncated || !utf8.ValidString(cleaned) {
		t.Fatalf("invalid UTF-8 was not safely normalized: %q", cleaned)
	}
}

func TestTaskToolInputSummaryShowsOnlyValidatedSearchQuery(t *testing.T) {
	if got := taskToolInputSummary(cloudDSHWebSearchToolName, `{"query":"AI news","time_range":"week"}`); got != "Search query: AI news (time range: week)." {
		t.Fatalf("safe search input summary = %q", got)
	}
	credential := "sk-proj-" + strings.Repeat("A", 40)
	got := taskToolInputSummary(cloudDSHWebSearchToolName, `{"query":"`+credential+`"}`)
	if strings.Contains(got, credential) || strings.Contains(got, "Search query:") {
		t.Fatalf("credential-like search query was exposed: %q", got)
	}
	if got := taskToolInputSummary(cloudDSHWebSearchToolName, `{"query":"AI\nnews"}`); strings.Contains(got, "AI\nnews") {
		t.Fatalf("multiline search query was exposed: %q", got)
	}
}

func TestTaskToolContentEvidenceCapturesBoundedPublicWebResult(t *testing.T) {
	full := strings.Repeat("网页来源证据", 1200)
	encoded, err := json.Marshal(cloudDSHMCPOutput{Text: []string{full}})
	if err != nil {
		t.Fatal(err)
	}
	evidence := taskToolContentEvidence(cloudDSHWebReadToolName, AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)})
	digest := sha256.Sum256([]byte(full))
	if evidence == nil || evidence.SHA256 != hex.EncodeToString(digest[:]) || !evidence.Truncated ||
		len(evidence.Preview) > maxTaskStepContentEvidencePreviewBytes || !utf8.ValidString(evidence.Preview) ||
		!strings.HasPrefix(full, evidence.Preview) {
		t.Fatalf("bounded web content evidence = %+v", evidence)
	}
	credential := "sk-proj-" + strings.Repeat("A", 40)
	secret, err := json.Marshal(cloudDSHMCPOutput{Text: []string{"public result contains " + credential}})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []AgentToolResult{
		{Status: AgentToolSucceeded, Output: string(secret)},
		{Status: AgentToolFailed, Output: string(encoded)},
	} {
		if taskToolContentEvidence(cloudDSHWebReadToolName, result) != nil {
			t.Fatal("failed or credential-bearing web output produced review content")
		}
	}
	if taskToolContentEvidence("morde_mcp_private_connector", AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)}) != nil {
		t.Fatal("private connector output was copied into review evidence")
	}
}

func TestTaskToolContentEvidenceExcludesWorkspaceReads(t *testing.T) {
	result := AgentToolResult{Status: AgentToolSucceeded, Output: `{"content":"private Workspace bytes"}`}
	if taskToolContentEvidence(cloudDSHWorkspaceReadToolName, result) != nil {
		t.Fatal("Workspace file content must not enter the independent review package")
	}
}

func TestTaskToolCallProgressSealsValidatedSearchQuery(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const leaseOwner, agentID, sessionID = "lease-owner", "agent_1", "session_1"
	const leaseVersion, generation, authorizationVersion, revision int64 = 4, 2, 3, 5
	current := task.Task{ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, AgentInstanceID: agentID,
		AgentGeneration: generation, AgentSessionID: sessionID, CurrentRevision: int64Pointer(revision),
		Authorization: task.Authorization{Version: authorizationVersion}, Lease: task.Lease{Owner: leaseOwner, Version: leaseVersion}}
	call := ModelToolCall{CallID: "search-call-1", Name: cloudDSHWebSearchToolName,
		Arguments: `{"query":"AI news","time_range":"week"}`,
		TaskFence: &ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: ownerID, LeaseOwner: leaseOwner,
			LeaseVersion: leaseVersion, AgentInstanceID: agentID, AgentGeneration: generation, AgentSessionID: sessionID,
			AuthorizationVersion: authorizationVersion}}
	activityContext := withCloudDSHToolActivity(context.Background(), current, call, sealer)
	webOutput, err := json.Marshal(cloudDSHMCPOutput{Text: []string{"Example public source: https://example.com; relevant passage."}})
	if err != nil {
		t.Fatal(err)
	}
	activityContext = withTaskToolContentEvidence(activityContext, call.Name,
		AgentToolResult{Status: AgentToolSucceeded, Output: string(webOutput)})
	activity, ok := cloudDSHToolActivityFromContext(activityContext)
	if !ok {
		t.Fatal("valid fenced search call did not produce task activity")
	}
	progress, err := taskToolCallProgress(activity, task.Step{ID: stepID, Status: task.StepSucceeded, Summary: "Search completed."})
	if err != nil || progress == nil || progress.DetailState != "available" {
		t.Fatalf("search task activity = %+v, err=%v", progress, err)
	}
	plain, err := sealer.OpenWithAAD(progress.DetailCiphertext, taskStepDetailAAD(ownerID, taskID, stepID, revision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.Input != "Search query: AI news (time range: week)." ||
		detail.ContentEvidence == nil || !strings.Contains(detail.ContentEvidence.Preview, "Example public source") {
		t.Fatalf("sealed search detail = %+v, err=%v", detail, err)
	}
}

func TestTaskToolCallProgressMarksOnlyExplicitConnectorRateLimitRetryable(t *testing.T) {
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const leaseOwner, agentID, sessionID = "lease-owner", "agent_1", "session_1"
	const leaseVersion, generation, authorizationVersion, revision int64 = 4, 2, 3, 5
	current := task.Task{ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, AgentInstanceID: agentID,
		AgentGeneration: generation, AgentSessionID: sessionID, CurrentRevision: int64Pointer(revision),
		Authorization: task.Authorization{Version: authorizationVersion}, Lease: task.Lease{Owner: leaseOwner, Version: leaseVersion}}
	call := ModelToolCall{CallID: "connector-call-1", Name: cloudDSHMCPCallToolName, Arguments: `{"connector_id":"k1234567"}`,
		TaskFence: &ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: ownerID, LeaseOwner: leaseOwner,
			LeaseVersion: leaseVersion, AgentInstanceID: agentID, AgentGeneration: generation, AgentSessionID: sessionID,
			AuthorizationVersion: authorizationVersion}}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, nil)
	ctx = withTaskToolResultEvidence(ctx, "execute", "connector_rate_limited")
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		t.Fatal("valid fenced connector call did not produce task activity")
	}
	progress, err := taskToolCallProgress(activity, task.Step{ID: stepID, Status: task.StepFailed, Phase: "prepare",
		Summary: "The connector rate limited this attempt; it was not retried."})
	if err != nil || progress == nil || progress.FailureReason != "connector_rate_limited" || !progress.Retryable || progress.Phase != "execute" {
		t.Fatalf("connector rate-limit task progress = %+v, err=%v", progress, err)
	}
	if !IsMCPConnectorStepRetryTool(progress.ToolName) || !SupportsSafeTaskStepRetryTool(progress.ToolName) {
		t.Fatalf("MCP connector retry tool was not admitted: %q", progress.ToolName)
	}
	ordinaryContext := withTaskToolResultEvidence(withCloudDSHToolActivity(context.Background(), current, call, nil), "execute", "")
	ordinaryActivity, _ := cloudDSHToolActivityFromContext(ordinaryContext)
	ordinary, err := taskToolCallProgress(ordinaryActivity, task.Step{ID: stepID, Status: task.StepFailed, Phase: "prepare"})
	if err != nil || ordinary == nil || ordinary.FailureReason != "" || ordinary.Retryable {
		t.Fatalf("ordinary dispatched MCP failure became retryable: progress=%+v err=%v", ordinary, err)
	}
}

func TestTaskToolCallProgressStoresOnlySafeShellExecutionEvidence(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const leaseOwner, agentID, sessionID = "lease-owner", "agent_1", "session_1"
	const leaseVersion, generation, authorizationVersion, revision int64 = 4, 2, 3, 5
	current := task.Task{ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, AgentInstanceID: agentID,
		AgentGeneration: generation, AgentSessionID: sessionID, CurrentRevision: int64Pointer(revision),
		Authorization: task.Authorization{Version: authorizationVersion}, Lease: task.Lease{Owner: leaseOwner, Version: leaseVersion}}
	call := ModelToolCall{CallID: "shell-call-1", Name: runtimeToolShellExec, Arguments: `{"command":"go test ./... --token=private-command-value"}`,
		TaskFence: &ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: ownerID, LeaseOwner: leaseOwner,
			LeaseVersion: leaseVersion, AgentInstanceID: agentID, AgentGeneration: generation, AgentSessionID: sessionID,
			AuthorizationVersion: authorizationVersion}}
	result := AgentToolResult{Status: AgentToolFailed, Output: `{"exit_code":1,"timed_out":false,"stdout":"private-stdout-value","stderr":"private-stderr-value","stdout_truncated":false,"stderr_truncated":true}`}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, sealer)
	ctx = withTaskToolShellExecutionEvidence(ctx, call.Name, result)
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		t.Fatal("valid fenced shell call did not produce task activity")
	}
	progress, err := taskToolCallProgress(activity, task.Step{ID: stepID, Status: task.StepFailed, Summary: "Shell failed."})
	if err != nil || progress == nil || progress.DetailState != "available" {
		t.Fatalf("shell task activity = %+v, err=%v", progress, err)
	}
	plain, err := sealer.OpenWithAAD(progress.DetailCiphertext, taskStepDetailAAD(ownerID, taskID, stepID, revision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.ShellExecution == nil ||
		*detail.ShellExecution != (TaskStepShellExecutionEvidence{ExitCode: 1, StdoutBytes: len("private-stdout-value"),
			StderrBytes: len("private-stderr-value"), StderrTruncated: true}) {
		t.Fatalf("sealed shell evidence = %+v, err=%v", detail.ShellExecution, err)
	}
	for _, private := range []string{"private-command-value", "private-stdout-value", "private-stderr-value"} {
		if strings.Contains(string(plain), private) {
			t.Fatalf("sealed shell evidence exposed private text %q", private)
		}
	}
	if taskToolShellExecutionEvidence(runtimeToolShellExec, AgentToolResult{Status: AgentToolSucceeded, Output: result.Output}) != nil ||
		taskToolShellExecutionEvidence(runtimeToolShellExec, AgentToolResult{Status: AgentToolFailed, Output: strings.TrimSuffix(result.Output, "}")}) != nil ||
		taskToolShellExecutionEvidence("file_read", result) != nil {
		t.Fatal("invalid, status-mismatched, or non-shell result produced shell evidence")
	}
}

func TestTaskToolCallProgressUsesMCPConnectorLabelAndOmitsAlias(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{6}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const leaseOwner, agentID, sessionID = "lease-owner", "agent_1", "session_1"
	const leaseVersion, generation, authorizationVersion, revision int64 = 4, 2, 3, 5
	current := task.Task{ID: taskID, OwnerID: ownerID, InitiatorID: ownerID, AgentInstanceID: agentID,
		AgentGeneration: generation, AgentSessionID: sessionID, CurrentRevision: int64Pointer(revision),
		Authorization: task.Authorization{Version: authorizationVersion}, Lease: task.Lease{Owner: leaseOwner, Version: leaseVersion}}
	alias := cloudDSHMCPToolName("k1234567", "lookup")
	call := ModelToolCall{CallID: "mcp-call-1", Name: alias, Arguments: `{"query":"private"}`,
		TaskFence: &ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: ownerID, LeaseOwner: leaseOwner,
			LeaseVersion: leaseVersion, AgentInstanceID: agentID, AgentGeneration: generation, AgentSessionID: sessionID,
			AuthorizationVersion: authorizationVersion}}
	activity, ok := cloudDSHToolActivityFromContext(withCloudDSHToolActivity(context.Background(), current, call, sealer))
	if !ok {
		t.Fatal("valid fenced MCP call did not produce task activity")
	}
	activity.displayObject = "Synthetic"
	progress, err := taskToolCallProgress(activity, task.Step{ID: stepID, Status: task.StepSucceeded, Summary: "MCP call completed."})
	if err != nil || progress == nil || progress.ToolName != alias || progress.DetailState != "available" {
		t.Fatalf("MCP task activity = %+v, err=%v", progress, err)
	}
	plain, err := sealer.OpenWithAAD(progress.DetailCiphertext, taskStepDetailAAD(ownerID, taskID, stepID, revision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.Object != "Synthetic" || strings.Contains(string(plain), alias) ||
		strings.Contains(string(plain), "private") {
		t.Fatalf("MCP detail exposes an internal alias or raw parameters: %+v, err=%v", detail, err)
	}
}

func int64Pointer(value int64) *int64 { return &value }

type taskStepDetailStoreStub struct {
	TaskStore
	details   TaskDetails
	record    TaskStepDetailRecord
	workspace workspace.Workspace
	calls     int
}

func (s *taskStepDetailStoreStub) GetTask(_ context.Context, taskID, actorID string) (TaskDetails, error) {
	if taskID != s.details.Task.ID || actorID != s.details.Task.OwnerID {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	return s.details, nil
}

func (s *taskStepDetailStoreStub) GetTaskStepDetail(_ context.Context, actorID, taskID, stepID string) (TaskStepDetailRecord, error) {
	s.calls++
	if actorID != s.record.OwnerID || taskID != s.record.TaskID || stepID != s.record.StepID {
		return TaskStepDetailRecord{}, domain.NewError(domain.CodeNotFound, "task step detail not found")
	}
	return s.record, nil
}

func (s *taskStepDetailStoreStub) GetWorkspace(_ context.Context, workspaceID string) (workspace.Workspace, error) {
	if s.workspace.ID != workspaceID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.workspace, nil
}

type taskStepDetailProjectAccessStub struct {
	err  error
	role project.Role
}

func (s *taskStepDetailProjectAccessStub) RequireAccess(_ context.Context, _ identity.Principal, _ string, role project.Role) (project.Member, error) {
	s.role = role
	return project.Member{}, s.err
}

func TestTaskServiceStepDetailRequiresTaskReadAndValidatesStoredRecord(t *testing.T) {
	sealer, err := NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const revision int64 = 3
	document := TaskStepDetailDocument{Object: "src/main.go", Input: "Read the requested file.", Result: "Read completed."}
	ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, revision, document)
	if err != nil {
		t.Fatal(err)
	}
	revisionValue := revision
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID, EventSequence: 8},
			Steps: []task.Step{{ID: stepID, ToolName: "morde_workspace_file_read", TaskRevision: &revisionValue, DetailState: "available"}}},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			EventSequence: 9, Ciphertext: ciphertext, SHA256: digest, SizeBytes: size},
	}
	authorizer := &taskAuthorizerStub{}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	value, err := service.GetStepDetail(context.Background(), principal, taskID, stepID)
	if err != nil || value.DetailState != "available" || value.Object == nil || *value.Object != document.Object ||
		value.Input == nil || *value.Input != document.Input || value.Result == nil || *value.Result != document.Result || value.EventSequence != 9 {
		t.Fatalf("authorized step detail = %+v, err=%v", value, err)
	}
	if store.calls != 1 || authorizer.lastScope != identity.ScopeTaskRead {
		t.Fatalf("detail read calls=%d task scope=%q", store.calls, authorizer.lastScope)
	}

	authorizer.err = domain.NewError(domain.CodeForbidden, "task access revoked")
	if _, err := service.GetStepDetail(context.Background(), principal, taskID, stepID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked Task detail error = %v; want hidden 404", err)
	}
	if store.calls != 1 {
		t.Fatalf("unauthorized detail reached encrypted store %d times", store.calls-1)
	}
	authorizer.err = nil
	if _, err := service.GetStepDetail(context.Background(), principal, taskID, "s7654321"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign or missing step error = %v; want hidden 404", err)
	}
	if store.calls != 1 {
		t.Fatalf("missing step reached encrypted store %d times", store.calls-1)
	}

	store.record.SHA256 = bytes.Repeat([]byte{0}, 32)
	if _, err := service.GetStepDetail(context.Background(), principal, taskID, stepID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("tampered step detail error = %v; want integrity failure", err)
	}
}

func TestTaskServiceStepDetailHidesWorkspaceDataAfterProjectAccessRevocation(t *testing.T) {
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const workspaceID, projectID = "w1234567", "j1234567"
	revision := int64(2)
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID, WorkspaceID: workspaceID,
			ProjectID: projectID, WorkspaceOwnerID: ownerID}, Steps: []task.Step{{ID: stepID, ToolName: "morde_workspace_file_read",
			Title: "Read private/report.csv", Summary: "Private file contents", TaskRevision: &revision, DetailState: "available"}}},
		workspace: workspace.Workspace{ID: workspaceID, OwnerID: ownerID, State: workspace.StateReady, Revision: 3},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			Ciphertext: []byte("must not be read")},
	}
	authorizer := &taskAuthorizerStub{}
	access := &taskStepDetailProjectAccessStub{err: domain.NewError(domain.CodeForbidden, "project membership was revoked")}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer, projectAccess: access}
	principal := taskTestPrincipal()

	detail, err := service.GetStepDetail(context.Background(), principal, taskID, stepID)
	if err != nil || detail.DetailState != "restricted" || detail.Object != nil || detail.Input != nil || detail.Result != nil {
		t.Fatalf("revoked project detail = %+v, err=%v; want restricted content-free response", detail, err)
	}
	if store.calls != 0 || access.role != project.RoleMember {
		t.Fatalf("revoked project detail reached encrypted store %d times or used role %q", store.calls, access.role)
	}
	masked, err := service.Get(context.Background(), principal, taskID)
	if err != nil || len(masked.Steps) != 1 || masked.Steps[0].Title != "Workspace action" ||
		masked.Steps[0].Summary != "The Workspace resource is not available with the current permissions." || masked.Steps[0].DetailState != "restricted" {
		t.Fatalf("revoked project Task projection = %+v, err=%v", masked.Steps, err)
	}
}

func TestTaskServiceKeepsShellEvidenceInternalButAvailableToReviewers(t *testing.T) {
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const revision = int64(3)
	sealer, err := NewSealer(bytes.Repeat([]byte{10}, 32))
	if err != nil {
		t.Fatal(err)
	}
	evidence := &TaskStepShellExecutionEvidence{ExitCode: 0, StdoutBytes: 12}
	document := TaskStepDetailDocument{Object: runtimeToolShellExec,
		Input: "Tool request; raw parameters and output are omitted for safety.", Result: "Shell completed.", ShellExecution: evidence}
	ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, revision, document)
	if err != nil {
		t.Fatal(err)
	}
	revisionValue := revision
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID},
			Steps: []task.Step{{ID: stepID, ToolName: runtimeToolShellExec, TaskRevision: &revisionValue, DetailState: "available"}}},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			Ciphertext: ciphertext, SHA256: digest, SizeBytes: size},
	}
	authorizer := &taskAuthorizerStub{}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || detail.ShellExecution == nil || *detail.ShellExecution != *evidence {
		t.Fatalf("shell evidence available to internal reviewer = %+v, err=%v", detail.ShellExecution, err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil || bytes.Contains(encoded, []byte("shell_execution")) || bytes.Contains(encoded, []byte("content_evidence")) {
		t.Fatalf("public TaskStepDetail exposed review-only evidence: %s, err=%v", encoded, err)
	}
}

func TestTaskServiceBindsWebContentEvidenceAndKeepsItInternal(t *testing.T) {
	const ownerID, taskID, stepID = "u1234567", "q1234567", "s1234567"
	const revision = int64(3)
	sealer, err := NewSealer(bytes.Repeat([]byte{11}, 32))
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := sha256.Sum256([]byte("source passage"))
	contentEvidence := &TaskStepContentEvidence{SHA256: hex.EncodeToString(contentDigest[:]), Preview: "source passage"}
	document := TaskStepDetailDocument{Object: cloudDSHWebReadToolName, Input: "Read authorized source.", Result: "Read completed.", ContentEvidence: contentEvidence}
	ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, revision, document)
	if err != nil {
		t.Fatal(err)
	}
	revisionValue := revision
	store := &taskStepDetailStoreStub{
		details: TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID},
			Steps: []task.Step{{ID: stepID, ToolName: cloudDSHWebReadToolName, Status: task.StepSucceeded, TaskRevision: &revisionValue, DetailState: "available"}}},
		record: TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID, TaskRevision: revision,
			Ciphertext: ciphertext, SHA256: digest, ContentEvidenceSHA256: contentDigest[:], SizeBytes: size},
	}
	authorizer := &taskAuthorizerStub{}
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: authorizer}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	detail, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID)
	if err != nil || detail.ContentEvidence == nil || *detail.ContentEvidence != *contentEvidence {
		t.Fatalf("web content evidence available to internal reviewer = %+v, err=%v", detail.ContentEvidence, err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil || bytes.Contains(encoded, []byte("content_evidence")) || bytes.Contains(encoded, []byte("source passage")) {
		t.Fatalf("public TaskStepDetail exposed review-only content: %s, err=%v", encoded, err)
	}
	store.record.ContentEvidenceSHA256 = make([]byte, sha256.Size)
	if _, err := service.GetStepDetail(context.Background(), taskTestPrincipal(), taskID, stepID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched content evidence identity error = %v, want invalid state", err)
	}
}
