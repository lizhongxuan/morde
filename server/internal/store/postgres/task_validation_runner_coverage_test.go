package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestTaskValidationRunnerCoverageReceiptRequiresExactPlanScope(t *testing.T) {
	full := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeFullSuite,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	claim, current, row, shell, receipt := taskValidationRunnerCoverageFixture(full)
	if !validTaskValidationRunnerCoverageReceipt(&receipt, full, claim, current, row, shell) {
		t.Fatal("complete full-suite package and test inventories were rejected")
	}

	targeted := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeTargeted,
		Command: "go test -run=^TestTaskReplay$ ./server/internal/app", CoveragePaths: []string{"server/internal/app"},
		Selectors: []string{"TestTaskReplay"}}
	claim, current, row, shell, receipt = taskValidationRunnerCoverageFixture(targeted)
	receipt.ScopeMode = string(task.PlanVerificationScopeTargeted)
	receipt.PackageSelector = "./server/internal/app"
	receipt.TestSelector = "^TestTaskReplay$"
	commandDigest := sha256.Sum256([]byte(targeted.Command))
	receipt.ExactCommandSHA256 = hex.EncodeToString(commandDigest[:])
	if !validTaskValidationRunnerCoverageReceipt(&receipt, targeted, claim, current, row, shell) {
		t.Fatal("exact targeted package and selector inventory were rejected")
	}

	for name, mutate := range map[string]func(*task.PlanVerificationCheck, *app.TaskValidationRunnerCoverageReceipt){
		"package selector does not match command": func(_ *task.PlanVerificationCheck, proof *app.TaskValidationRunnerCoverageReceipt) {
			proof.PackageSelector = "./server/internal/worker"
		},
		"test selector does not match plan": func(check *task.PlanVerificationCheck, proof *app.TaskValidationRunnerCoverageReceipt) {
			check.Selectors[0] = "TestOther"
		},
		"targeted command cannot claim full suite": func(_ *task.PlanVerificationCheck, proof *app.TaskValidationRunnerCoverageReceipt) {
			proof.ScopeMode = string(task.PlanVerificationScopeFullSuite)
		},
		"receipt operation identity mismatch": func(_ *task.PlanVerificationCheck, proof *app.TaskValidationRunnerCoverageReceipt) {
			proof.OperationID = "o7654321"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedCheck := targeted
			changedCheck.Selectors = append([]string(nil), targeted.Selectors...)
			changedReceipt := receipt
			checkpoint := int64(7)
			changedReceipt.WorkspaceCheckpointRevision = &checkpoint
			mutate(&changedCheck, &changedReceipt)
			if validTaskValidationRunnerCoverageReceipt(&changedReceipt, changedCheck, claim, current, row, shell) {
				t.Fatal("mismatched plan/runner coverage scopes were accepted")
			}
		})
	}

	targetRoot := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeTargeted,
		Command: "go test -run=^TestTaskReplay$ .", CoveragePaths: []string{"."}, Selectors: []string{"TestTaskReplay"}}
	claim, current, row, shell, receipt = taskValidationRunnerCoverageFixture(targetRoot)
	receipt.ScopeMode = string(task.PlanVerificationScopeTargeted)
	receipt.PackageSelector = "."
	receipt.TestSelector = "^TestTaskReplay$"
	commandDigest = sha256.Sum256([]byte(targetRoot.Command))
	receipt.ExactCommandSHA256 = hex.EncodeToString(commandDigest[:])
	if !validTaskValidationRunnerCoverageReceipt(&receipt, targetRoot, claim, current, row, shell) {
		t.Fatal("exact current-directory package selector was rejected")
	}
}

func TestTaskValidationRunnerCoverageReceiptFailsClosedOnIncompleteFacts(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeFullSuite,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	claim, current, row, shell, receipt := taskValidationRunnerCoverageFixture(check)
	for name, mutate := range map[string]func(*app.TaskValidationRunnerCoverageReceipt, *taskValidationReplayShellResult){
		"missing package inventory": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.ExecutedPackageInventorySHA256 = hashTextDigest("different packages")
		},
		"missing test root": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.ExecutedTestInventoryCount = 0
		},
		"failed test": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.FailedTestCount = 1
		},
		"skipped test": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.SkippedTestCount = 1
		},
		"runner parser incomplete": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.ParserComplete = false
		},
		"shell output truncated": func(_ *app.TaskValidationRunnerCoverageReceipt, result *taskValidationReplayShellResult) {
			result.StdoutTruncated = true
		},
		"workspace changed during run": func(_ *app.TaskValidationRunnerCoverageReceipt, result *taskValidationReplayShellResult) {
			result.WorkspaceSnapshotSHA256After = hashTextDigest("changed workspace")
		},
		"unrecognized runner contract": func(proof *app.TaskValidationRunnerCoverageReceipt, _ *taskValidationReplayShellResult) {
			proof.ScopeContract = "shell exit code means tests passed"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedReceipt := receipt
			checkpoint := int64(7)
			changedReceipt.WorkspaceCheckpointRevision = &checkpoint
			changedShell := shell
			mutate(&changedReceipt, &changedShell)
			if validTaskValidationRunnerCoverageReceipt(&changedReceipt, check, claim, current, row, changedShell) {
				t.Fatal("incomplete or mismatched runner facts were accepted")
			}
		})
	}
}

func taskValidationRunnerCoverageFixture(check task.PlanVerificationCheck) (app.TaskValidationReviewClaim,
	taskValidationReplayTask, taskValidationReplayBinding, taskValidationReplayShellResult,
	app.TaskValidationRunnerCoverageReceipt) {
	claim := app.TaskValidationReviewClaim{TaskID: "t1234567"}
	current := taskValidationReplayTask{workspaceID: "w1234567"}
	row := taskValidationReplayBinding{executionID: "o1234567", operationBindingVersion: 2, operationCheckpoint: 7}
	viewID := "v" + hashTextDigest("workspace view")
	snapshot := hashTextDigest("workspace snapshot")
	shell := taskValidationReplayShellResult{ExitCode: 0, WorkspaceViewID: viewID,
		WorkspaceSnapshotSHA256Before: snapshot, WorkspaceSnapshotSHA256After: snapshot}
	checkpoint := int64(row.operationCheckpoint)
	commandDigest := sha256.Sum256([]byte(check.Command))
	packageInventory := hashTextDigest("test-bearing packages")
	testInventory := hashTextDigest("test and example roots")
	receipt := app.TaskValidationRunnerCoverageReceipt{
		Version: 1, OperationID: row.executionID, Runner: "go", RunnerVersion: "go version go1.25.3 linux/amd64",
		ScopeMode: string(check.ScopeMode), ScopeContract: taskValidationRunnerCoverageScopeContract,
		PackageSelector: "./...", ExactCommandSHA256: hex.EncodeToString(commandDigest[:]),
		TaskID: claim.TaskID, WorkspaceID: current.workspaceID,
		WorkspaceDirectory: claim.TaskID + "/" + viewID, BindingVersion: row.operationBindingVersion,
		WorkspaceViewID: viewID, WorkspaceCheckpointRevision: &checkpoint, WorkspaceCWD: ".",
		WorkspaceSnapshotSHA256Before: snapshot, WorkspaceSnapshotSHA256After: snapshot,
		DiscoveredSourceInventorySHA256: hashTextDigest("source files"), DiscoveredSourceInventoryCount: 1,
		DiscoveredPackageInventorySHA256: packageInventory, DiscoveredPackageInventoryCount: 1,
		ExecutedPackageInventorySHA256: packageInventory, ExecutedPackageInventoryCount: 1,
		DiscoveredTestInventorySHA256: testInventory, DiscoveredTestInventoryCount: 1,
		ExecutedTestInventorySHA256: testInventory, ExecutedTestInventoryCount: 1,
		PassedTestCount: 1, ParserComplete: true,
	}
	return claim, current, row, shell, receipt
}

func hashTextDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
