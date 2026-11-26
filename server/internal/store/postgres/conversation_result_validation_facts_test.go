package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationResultValidationFactsBindRequirementsAndChecksWithoutOverstatingPass(t *testing.T) {
	revision := int64(12)
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./...", ScopeMode: task.PlanVerificationScopeFullSuite, CoveragePaths: []string{"server"},
		RequirementIDs: []task.PlanVerificationRequirementID{"req-test"}}
	plan := &task.Plan{TaskRevision: 4, Revision: 7, FixPlan: &task.FixPlan{},
		RequirementManifest: &task.PlanVerificationRequirementManifest{Version: 1,
			Obligations: []task.PlanVerificationObligation{{ID: "req-test", Source: task.PlanVerificationRequirementSource{
				Kind: task.PlanVerificationRequirementProjectFile, WorkspacePath: "AGENTS.md", WorkspaceRevision: &revision}}}},
		Steps: []task.PlanStep{
			{ID: "required-suite", VerificationCheck: &check},
			{ID: "risk-check", VerificationCheck: &task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest,
				Basis: task.PlanVerificationCheckRiskSelected, Command: "go test -run=^TestFocused$ ./server",
				ScopeMode: task.PlanVerificationScopeTargeted, CoveragePaths: []string{"server"}, Selectors: []string{"TestFocused"}}},
			{ID: "unresolved-step"},
		}}
	replay := &app.TaskValidationReplayEvidence{State: app.TaskValidationReplayPending,
		RequirementSetVerified: false, FinalWorkspaceSnapshotVerified: false,
		Checks: []app.TaskValidationReplayCheckEvidence{
			{CheckID: "required-suite", Status: app.TaskValidationReplayPass, OperationID: "op1234567", RunSequence: 9,
				ExitCode: 0, Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
				ScopeMode: task.PlanVerificationScopeFullSuite, CoveragePaths: []string{"server"},
				RequirementIDs: []task.PlanVerificationRequirementID{"req-test"}, RequirementSourceVerified: true},
			{CheckID: "risk-check", Status: app.TaskValidationReplayPending, Kind: task.PlanVerificationCheckTest,
				Basis: task.PlanVerificationCheckRiskSelected, ScopeMode: task.PlanVerificationScopeTargeted,
				CoveragePaths: []string{"server"}, Selectors: []string{"TestFocused"}},
		}, PlanStepsWithoutChecks: []string{"unresolved-step"}}
	facts := conversationResultDeliveryFacts{validationPlan: plan, validationReplay: replay}
	lines := conversationResultValidationFactLines(app.ConversationTaskResultSource{TaskID: "q1234567", TaskRevision: 4}, facts)
	receipt := strings.Join(lines, "\n")
	for _, want := range []string{
		"q1234567 r4 修复验证 Plan r7：H07 pending",
		"要求集完整性 待验证",
		"已列检查不能代表所有用户/项目要求均已覆盖",
		"命令退出码 0；实际覆盖范围 待验证",
		"要求 req-test → Workspace r12:AGENTS.md（来源已核验）",
		"执行记录 op1234567（步骤序号 9）",
		"风险选择（非用户/项目明确要求）",
		"无可绑定的执行回执",
		"未绑定检查的计划步骤：unresolved-step",
	} {
		if !strings.Contains(receipt, want) {
			t.Errorf("validation receipt omits %q: %q", want, receipt)
		}
	}
	if strings.Contains(receipt, "全量验证通过") || strings.Contains(receipt, "实际覆盖范围 已验证") {
		t.Fatalf("partial check evidence was overstated as complete: %q", receipt)
	}
}

func TestConversationResultReceiptLoadsCurrentFixPlanReplay(t *testing.T) {
	fixture := newResultAggregationFixture(t)
	value := fixture.createTask(t, fixture.ownerID, "fix a synthetic issue")
	ctx := context.Background()
	plan := task.Plan{TaskRevision: 0, Revision: 1, Goal: "Fix the synthetic issue", CostEstimateState: "unknown",
		FixPlan:             &task.FixPlan{ChangedFiles: []string{"server/example.go"}, Risks: []string{"regression"}, Alternatives: []string{"revert"}},
		RequirementManifest: &task.PlanVerificationRequirementManifest{Version: 1, Obligations: []task.PlanVerificationObligation{}},
		Steps: []task.PlanStep{{ID: "focused-check", Title: "Run focused check", Status: task.PlanStepPending,
			CompletionCriteria: []string{"check completes"}, VerificationApplicability: task.PlanVerificationApplicabilityCheck,
			VerificationCheck: &task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest,
				Basis: task.PlanVerificationCheckRiskSelected, Command: "go test -run=^TestFocused$ ./server",
				ScopeMode: task.PlanVerificationScopeTargeted, CoveragePaths: []string{"server"},
				Selectors: []string{"TestFocused"}}}}}
	digest, err := plan.RequirementManifestDigest()
	if err != nil {
		t.Fatalf("digest empty requirement manifest: %v", err)
	}
	plan.RequirementManifest.Digest = digest
	if err := plan.Validate(); err != nil {
		t.Fatalf("validate synthetic fix plan: %v", err)
	}
	tx, err := fixture.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1 WHERE id=$1 RETURNING event_seq`, value.ID).Scan(&sequence); err != nil {
		t.Fatalf("advance Task event sequence for plan: %v", err)
	}
	planEvent, err := json.Marshal(appTaskPlanEvent{StepID: "focused-check", Sequence: sequence,
		TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload,task_revision)
VALUES ($1,$2,'task.plan',$3::jsonb,$4)`, value.ID, sequence, string(planEvent), plan.TaskRevision); err != nil {
		t.Fatalf("insert current fix plan: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !fixture.complete(t, &value, "fixed the synthetic issue") {
		t.Fatal("completed fix result was not queued")
	}
	fixture.runNextReview(t)
	var receipt string
	if err := fixture.store.pool.QueryRow(ctx, `SELECT message.content
FROM conversation_result_batches batch JOIN conversation_messages message ON message.id=batch.published_message_id
JOIN conversation_result_sources source ON source.batch_message_id=batch.message_id
WHERE batch.owner_id=$1 AND batch.state='published' AND source.task_id=$2 AND message.role='assistant'`,
		fixture.ownerID, value.ID).Scan(&receipt); err != nil {
		t.Fatalf("read published fix result receipt: %v", err)
	}
	for _, want := range []string{
		value.ID + " r0 修复验证 Plan r1：H07 pending",
		"检查 focused-check（test/风险选择（非用户/项目明确要求），targeted）：无可绑定的执行回执",
		"范围：路径 server；选择器 TestFocused",
		"该计划没有可信的要求集完整性证明",
	} {
		if !strings.Contains(receipt, want) {
			t.Errorf("published result receipt omits %q: %q", want, receipt)
		}
	}
}
