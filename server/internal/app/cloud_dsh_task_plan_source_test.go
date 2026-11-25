package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type taskPlanSourceResolverTestStore struct {
	*cloudDSHBrokerTaskStore
	inputs  []task.PlanVerificationRequirementSource
	resolve func(task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error)
}

type taskPlanSourceWorkspaceStore struct{ *syncServiceStore }

func (s *taskPlanSourceWorkspaceStore) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if ownerID != s.workspace.OwnerID {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.syncServiceStore.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, limit)
}

func (s *taskPlanSourceWorkspaceStore) GetWorkspaceChangePageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, after string, limit int32) (workspace.ChangePage, error) {
	if ownerID != s.workspace.OwnerID {
		return workspace.ChangePage{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.syncServiceStore.GetWorkspaceChangePage(ctx, workspaceID, revision, after, limit)
}

type taskPlanProjectGuardStore struct {
	*taskPlanSourceResolverTestStore
	calls int
	err   error
}

func (s *taskPlanProjectGuardStore) WithProjectTaskWorkspaceAccess(ctx context.Context, current task.Task, write bool,
	operation func(context.Context) error) error {
	s.calls++
	if write || current.ProjectID == "" || operation == nil {
		return domain.NewError(domain.CodeForbidden, "project Workspace access denied")
	}
	if s.err != nil {
		return s.err
	}
	return operation(ctx)
}

func (s *taskPlanSourceResolverTestStore) ResolveCloudDSHTaskPlanRequirementSource(_ context.Context, _ task.Task,
	source task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error) {
	s.inputs = append(s.inputs, source)
	if s.resolve == nil {
		return task.PlanVerificationRequirementSource{}, errors.New("unexpected source resolution")
	}
	return s.resolve(source)
}

func newTaskPlanSourceTestBroker(t *testing.T, resolve func(task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error)) (
	*taskPlanSourceResolverTestStore, *CloudDSHToolBroker, task.Task) {
	t.Helper()
	base, broker, current := newBriefPlanBroker(t)
	store := &taskPlanSourceResolverTestStore{cloudDSHBrokerTaskStore: base, resolve: resolve}
	broker.store = store
	return store, broker, current
}

func taskPlanSourceArguments(locator string, messageID string, kind string) string {
	source := map[string]any{"kind": kind}
	if locator != "" {
		source["locator"] = locator
	}
	if messageID != "" {
		source["message_id"] = messageID
	}
	encoded, _ := json.Marshal(source)
	return `{"goal":"Fix the requested behavior","scope":"server implementation","verification_method":"run the required tests",` +
		`"fix_plan":{},"steps":[{"id":"verify","title":"Run required tests","status":"pending","completion_criteria":["required tests pass"],` +
		`"verification_applicability":"check","verification_check":{"kind":"test","basis":"required",` +
		`"command":"go test ./...","coverage_paths":["server"],"requirement_source":` + string(encoded) + `}}]}`
}

func resolvedUserTaskPlanSource(source task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error) {
	if source.Kind != task.PlanVerificationRequirementUserMessage {
		return task.PlanVerificationRequirementSource{}, domain.NewError(domain.CodeUnsupported, "project source unavailable")
	}
	if source.Locator == task.PlanVerificationRequirementCurrentTaskInputLocator {
		source.MessageID = "m1234567"
	}
	if source.MessageID == "" {
		return task.PlanVerificationRequirementSource{}, domain.InvalidArgument("source", "message id is missing")
	}
	digest := sha256.Sum256([]byte("verified source body"))
	source.Locator = ""
	source.ContentSHA256 = hex.EncodeToString(digest[:])
	return source, nil
}

func TestTaskPlanSubmissionResolvesUserSourceBeforeValidationAndPersistence(t *testing.T) {
	for _, test := range []struct {
		name      string
		locator   string
		messageID string
		modelSHA  string
	}{
		{name: "current task input", locator: task.PlanVerificationRequirementCurrentTaskInputLocator},
		{name: "exact message ID", messageID: "m7654321", modelSHA: strings.Repeat("0", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, broker, current := newTaskPlanSourceTestBroker(t, resolvedUserTaskPlanSource)
			arguments := taskPlanSourceArguments(test.locator, test.messageID, string(task.PlanVerificationRequirementUserMessage))
			if test.modelSHA != "" {
				arguments = strings.Replace(arguments, `"message_id":"`+test.messageID+`"`,
					`"message_id":"`+test.messageID+`","content_sha256":"`+test.modelSHA+`"`, 1)
			}
			call := briefPlanCall(current, "plan-source-"+strings.ReplaceAll(test.name, " ", "-"), arguments)
			result, err := broker.Execute(context.Background(), current, call)
			if err != nil || result.Status != AgentToolSucceeded {
				t.Fatalf("source-backed plan submission = %#v, %v", result, err)
			}
			if len(store.inputs) != 1 || store.inputs[0].ContentSHA256 != test.modelSHA {
				t.Fatalf("resolver input = %+v; want model digest %q passed only as untrusted input", store.inputs, test.modelSHA)
			}
			var output task.Plan
			if err := json.Unmarshal([]byte(result.Output), &output); err != nil {
				t.Fatalf("decode canonical plan result: %v", err)
			}
			source := output.Steps[0].VerificationCheck.RequirementSource
			if source == nil || source.Locator != "" || source.MessageID == "" || len(source.ContentSHA256) != 64 ||
				output.ValidateSubmission() != nil {
				t.Fatalf("tool output did not contain a canonical, valid source: %+v", output)
			}
			for _, progress := range store.committedTaskProgress {
				if commit := progress.CloudDSHTaskPlan; commit != nil {
					committedSource := commit.Plan.Steps[0].VerificationCheck.RequirementSource
					if committedSource == nil || committedSource.Locator != "" || committedSource.MessageID != source.MessageID ||
						committedSource.ContentSHA256 != source.ContentSHA256 {
						t.Fatalf("persisted source differs from canonical tool output: %+v", committedSource)
					}
					return
				}
			}
			t.Fatal("canonical plan was not persisted")
		})
	}
}

func TestTaskPlanSubmissionRejectsUnresolvableProjectSource(t *testing.T) {
	store, broker, current := newTaskPlanSourceTestBroker(t, resolvedUserTaskPlanSource)
	call := briefPlanCall(current, "plan-source-project", taskPlanSourceArguments("", "", string(task.PlanVerificationRequirementProjectFile)))
	// Replace the source with the minimum project-file locator. The tool input
	// intentionally omits content_sha256 so the server cannot mistake a model
	// hash for immutable file evidence.
	call.Arguments = strings.Replace(call.Arguments, `"kind":"project_file"`, `"kind":"project_file","workspace_path":"AGENTS.md","workspace_revision":3`, 1)
	result, err := broker.Execute(context.Background(), current, call)
	if err != nil || result.Status != AgentToolFailed || len(store.inputs) != 0 {
		t.Fatalf("unresolvable project source result = %#v err=%v inputs=%+v", result, err, store.inputs)
	}
	for _, progress := range store.committedTaskProgress {
		if progress.CloudDSHTaskPlan != nil {
			t.Fatal("project-file source without immutable bytes was persisted")
		}
	}
}

func taskPlanProjectSourceFixture(t *testing.T, projectTask bool, contents []byte) (
	*taskPlanSourceResolverTestStore, *CloudDSHToolBroker, task.Task, *syncServiceStore, *syncBlockMemory) {
	t.Helper()
	planStore, broker, current := newTaskPlanSourceTestBroker(t, nil)
	current.HostID = ""
	current.ExecutionLocation = "server"
	if projectTask {
		resourceVersion := int64(7)
		current.ProjectID = "j1234567"
		current.InitiatorID = "u7654321"
		current.WorkspaceOwnerID = current.OwnerID
		current.WorkspaceProjectResourceVersion = &resourceVersion
		broker.store = &taskPlanProjectGuardStore{taskPlanSourceResolverTestStore: planStore}
	} else {
		current.ProjectID = ""
		current.InitiatorID = current.OwnerID
		current.WorkspaceOwnerID = current.OwnerID
	}
	planStore.details.Task = current
	manifestStore := newSyncServiceStore()
	manifestStore.workspace.OwnerID = current.WorkspaceOwnerID
	manifestStore.workspace.Revision = *current.WorkspaceCheckpointRevision
	manifestStore.snapshots[*current.WorkspaceCheckpointRevision] = []workspace.File{syncServiceFile("AGENTS.md", contents)}
	blocks := newSyncBlockMemory()
	digest := sha256.Sum256(contents)
	blocks.data[hex.EncodeToString(digest[:])] = append([]byte(nil), contents...)
	service, err := NewWorkspaceSyncService(&taskPlanSourceWorkspaceStore{syncServiceStore: manifestStore}, blocks,
		workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	broker.workspaceTools = newCloudDSHWorkspaceTools(service)
	return planStore, broker, current, manifestStore, blocks
}

func taskPlanProjectSourceArguments(contentSHA string) string {
	arguments := taskPlanSourceArguments("", "", string(task.PlanVerificationRequirementProjectFile))
	source := `"kind":"project_file","workspace_path":"AGENTS.md","workspace_revision":0`
	if contentSHA != "" {
		source += `,"content_sha256":"` + contentSHA + `"`
	}
	return strings.Replace(arguments, `"kind":"project_file"`, source, 1)
}

func TestTaskPlanSubmissionResolvesProjectSourceFromVerifiedSnapshot(t *testing.T) {
	contents := []byte("Run go test ./server/internal/app before merging.")
	planStore, broker, current, manifestStore, _ := taskPlanProjectSourceFixture(t, false, contents)
	modelDigest := strings.Repeat("0", 64)
	call := briefPlanCall(current, "plan-project-source-verified", taskPlanProjectSourceArguments(modelDigest))
	result, err := broker.Execute(context.Background(), current, call)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("project-source plan submission = %#v, %v", result, err)
	}
	var plan task.Plan
	if err := json.Unmarshal([]byte(result.Output), &plan); err != nil {
		t.Fatalf("decode canonical plan: %v", err)
	}
	digest := sha256.Sum256(contents)
	canonicalDigest := hex.EncodeToString(digest[:])
	source := plan.Steps[0].VerificationCheck.RequirementSource
	if source == nil || source.Kind != task.PlanVerificationRequirementProjectFile || source.Locator != "" ||
		source.WorkspacePath != "AGENTS.md" || source.WorkspaceRevision == nil || *source.WorkspaceRevision != 0 ||
		source.ContentSHA256 != canonicalDigest || source.ContentSHA256 == modelDigest {
		t.Fatalf("project source was not canonicalized from the verified snapshot: %+v", source)
	}
	if len(planStore.inputs) != 0 || manifestStore.blockReadLockAcquires != 1 || manifestStore.blockReadLockReleases != 1 {
		t.Fatalf("project source used the wrong resolver or block lock: store inputs=%+v locks=%d/%d",
			planStore.inputs, manifestStore.blockReadLockAcquires, manifestStore.blockReadLockReleases)
	}
	for _, progress := range planStore.committedTaskProgress {
		if commit := progress.CloudDSHTaskPlan; commit != nil {
			stored := commit.Plan.Steps[0].VerificationCheck.RequirementSource
			if stored == nil || stored.ContentSHA256 != canonicalDigest || stored.WorkspacePath != "AGENTS.md" {
				t.Fatalf("persisted project source differs from canonical output: %+v", stored)
			}
			return
		}
	}
	t.Fatal("verified project source plan was not persisted")
}

func TestTaskPlanProjectSourceFailsClosedForUnavailableBytesAndLiveSource(t *testing.T) {
	for _, test := range []struct {
		name     string
		missing  bool
		corrupt  bool
		live     bool
		badRange bool
	}{
		{name: "missing block", missing: true},
		{name: "corrupt block", corrupt: true},
		{name: "live directory", live: true},
		{name: "outside task checkpoint", badRange: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			contents := []byte("run go test ./server")
			planStore, broker, current, manifestStore, blocks := taskPlanProjectSourceFixture(t, false, contents)
			if test.missing {
				blocks.data = map[string][]byte{}
			}
			if test.corrupt {
				for digest := range blocks.data {
					blocks.data[digest] = []byte("tampered")
				}
			}
			if test.live {
				broker.workspaceTools.workspaces.liveDirectorySource = true
				broker.workspaceTools.workspaces.currentDirectory = cloudDSHLiveWorkspaceDirectory{root: t.TempDir()}
			}
			arguments := taskPlanProjectSourceArguments("")
			if test.badRange {
				arguments = strings.Replace(arguments, `"workspace_revision":0`, `"workspace_revision":1`, 1)
			}
			result, err := broker.Execute(context.Background(), current, briefPlanCall(current,
				"plan-project-source-rejected-"+strings.ReplaceAll(test.name, " ", "-"), arguments))
			if err != nil || result.Status != AgentToolFailed {
				t.Fatalf("unverifiable project source result = %#v, %v", result, err)
			}
			for _, progress := range planStore.committedTaskProgress {
				if progress.CloudDSHTaskPlan != nil {
					t.Fatal("unverifiable project source was persisted")
				}
			}
			if test.live && manifestStore.blockReadLockAcquires != 0 {
				t.Fatal("live source reached immutable block verification")
			}
		})
	}
}

func TestTaskPlanProjectSourceUsesProjectWorkspaceGuard(t *testing.T) {
	contents := []byte("Run go test ./server/internal/app.")
	planStore, broker, current, _, _ := taskPlanProjectSourceFixture(t, true, contents)
	guardStore := broker.store.(*taskPlanProjectGuardStore)
	result, err := broker.Execute(context.Background(), current, briefPlanCall(current,
		"plan-project-source-guarded", taskPlanProjectSourceArguments("")))
	if err != nil || result.Status != AgentToolSucceeded || guardStore.calls != 1 || len(planStore.inputs) != 0 {
		t.Fatalf("project source guard calls=%d result=%#v err=%v storeInputs=%+v", guardStore.calls, result, err, planStore.inputs)
	}

	deniedStore, deniedBroker, deniedCurrent, _, _ := taskPlanProjectSourceFixture(t, true, contents)
	deniedGuard := deniedBroker.store.(*taskPlanProjectGuardStore)
	deniedGuard.err = domain.NewError(domain.CodeForbidden, "project access revoked")
	denied, err := deniedBroker.Execute(context.Background(), deniedCurrent, briefPlanCall(deniedCurrent,
		"plan-project-source-denied", taskPlanProjectSourceArguments("")))
	if err != nil || denied.Status != AgentToolFailed || deniedGuard.calls != 1 {
		t.Fatalf("revoked project source result = %#v, %v calls=%d", denied, err, deniedGuard.calls)
	}
	for _, progress := range deniedStore.committedTaskProgress {
		if progress.CloudDSHTaskPlan != nil {
			t.Fatal("project source was persisted after guard denial")
		}
	}
}

func TestLegacyProjectSourcePlanReplayDoesNotResolveOrChangeCanonicalReceipt(t *testing.T) {
	store, broker, current := newTaskPlanSourceTestBroker(t, func(source task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error) {
		return task.PlanVerificationRequirementSource{}, errors.New("legacy project source must not be re-resolved")
	})
	revision := int64(2)
	legacy := task.Plan{TaskRevision: *current.CurrentRevision, Revision: 1, Goal: "Fix the requested behavior",
		Scope: "server implementation", VerificationMethod: "run the required tests", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "verify", Title: "Run required tests", Status: task.PlanStepPending,
			DependsOn: []string{}, Outputs: []string{}, CompletionCriteria: []string{"required tests pass"},
			VerificationCheck: &task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest,
				Basis: task.PlanVerificationCheckRequired, Command: "go test ./...", CoveragePaths: []string{"server"},
				RequirementSource: &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementProjectFile,
					WorkspacePath: "AGENTS.md", WorkspaceRevision: &revision, ContentSHA256: strings.Repeat("a", 64)},
			}}}}
	inputSteps := append([]task.PlanStep(nil), legacy.Steps...)
	inputCheck := *inputSteps[0].VerificationCheck
	inputSource := *inputCheck.RequirementSource
	inputSource.ContentSHA256 = ""
	inputCheck.RequirementSource = &inputSource
	inputSteps[0].VerificationCheck = &inputCheck
	arguments, err := json.Marshal(cloudDSHTaskPlanInput{Goal: legacy.Goal, Scope: legacy.Scope,
		VerificationMethod: legacy.VerificationMethod, Steps: inputSteps})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	call := briefPlanCall(current, "plan-legacy-source-replay", string(arguments))
	replayed, err := broker.Reconcile(context.Background(), current, call, stringPointer(string(checkpoint)))
	if err != nil || replayed.Status != AgentToolSucceeded || replayed.Output != string(checkpoint) || len(store.inputs) != 0 {
		t.Fatalf("legacy project-source replay = %#v err=%v resolver_inputs=%+v", replayed, err, store.inputs)
	}

	receiptStore, receiptBroker, receiptCurrent := newTaskPlanSourceTestBroker(t, func(source task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error) {
		return task.PlanVerificationRequirementSource{}, errors.New("persisted project source receipt must not be re-resolved")
	})
	receiptCallID := "plan-legacy-source-receipt-replay"
	receiptStore.committedTaskProgress = append(receiptStore.committedTaskProgress, TaskProgress{CloudDSHTaskPlan: &CloudDSHTaskPlanCommit{
		TaskID: receiptCurrent.ID, OwnerID: receiptCurrent.OwnerID, AgentSessionID: receiptCurrent.AgentSessionID,
		AuthorizationVersion: receiptCurrent.Authorization.Version, CallID: receiptCallID, Plan: legacy,
	}})
	receiptCall := briefPlanCall(receiptCurrent, receiptCallID, string(arguments))
	replayed, err = receiptBroker.Reconcile(context.Background(), receiptCurrent, receiptCall, nil)
	if err != nil || replayed.Status != AgentToolSucceeded || replayed.Output != string(checkpoint) || len(receiptStore.inputs) != 0 {
		t.Fatalf("persisted project-source replay = %#v err=%v resolver_inputs=%+v", replayed, err, receiptStore.inputs)
	}
}

func TestTaskPlanRequirementSourceToolSchemaUsesServerResolvedDigest(t *testing.T) {
	definition := cloudDSHTaskPlanDefinition()
	if _, err := validateAgentTools([]AgentToolDefinition{definition}); err != nil {
		t.Fatalf("task plan tool schema is invalid: %v", err)
	}
	if !strings.Contains(definition.Description, cloudDSHCurrentHistorySearchToolName) ||
		!strings.Contains(definition.Description, "locator=current_task_input") ||
		!strings.Contains(definition.Description, "fixed through_sequence") ||
		!strings.Contains(definition.Description, "can omit messages accepted after Task start") ||
		!strings.Contains(definition.Description, "Never infer or invent an ID") {
		t.Fatalf("plan source instructions do not provide a truthful ID lookup path: %s", definition.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	steps := properties["steps"].(map[string]any)
	stepItem := steps["items"].(map[string]any)
	check := stepItem["properties"].(map[string]any)["verification_check"].(map[string]any)
	source := check["properties"].(map[string]any)["requirement_source"].(map[string]any)
	sourceProperties := source["properties"].(map[string]any)
	if _, ok := sourceProperties["content_sha256"]; !ok {
		t.Fatal("source schema does not expose optional canonical digest")
	}
	for _, required := range source["required"].([]any) {
		if required == "content_sha256" {
			t.Fatal("plan tool still requires the model to compute a source digest")
		}
	}
	locator := sourceProperties["locator"].(map[string]any)
	if values := locator["enum"].([]any); len(values) != 1 || values[0] != task.PlanVerificationRequirementCurrentTaskInputLocator {
		t.Fatalf("current task input locator schema = %+v", locator)
	}
}

func TestTaskPlanSourceIdentityDoesNotProveOmittedFullSuiteCoverage(t *testing.T) {
	// The accepted user source explicitly requires the full test suite, while
	// the submitted plan records only a focused check. Source identity and a
	// passing receipt prove what source/check were recorded, not that the plan
	// captured every explicit requirement. No keyword-based inference is used.
	source := &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./internal/app", CoveragePaths: []string{"server/internal/app"}, RequirementSource: source}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the requested behavior", Scope: "server/internal/app",
		VerificationMethod: "run the required test suite", CostEstimateState: "unknown", FixPlan: &task.FixPlan{},
		Steps: []task.PlanStep{{ID: "verify-focused", Title: "Run focused tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	if err := plan.Validate(); err != nil {
		t.Fatalf("fixture plan is invalid: %v", err)
	}
	fact := taskValidationReplayCheckFixture(plan, "verify-focused", check, 1)
	fact.RequirementSourceVerified = true
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision,
		FinalWorkspaceSnapshotVerified: true, Checks: []TaskValidationReplayCheckEvidence{fact}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("focused check with a verified source and final Workspace snapshot closed an unverified requirement set: got %q, want pending", got)
	}
}
