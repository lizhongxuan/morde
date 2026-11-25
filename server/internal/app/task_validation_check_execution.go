package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"morde/server/internal/domain/task"
)

func withTaskValidationPlanCheck(ctx context.Context, planRevision int64, stepID string, check *task.PlanVerificationCheck) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok || planRevision < 1 || stepID == "" || check == nil {
		return ctx
	}
	value := *check
	value.CoveragePaths = append([]string(nil), check.CoveragePaths...)
	if check.RequirementSource != nil {
		source := *check.RequirementSource
		if source.WorkspaceRevision != nil {
			revision := *source.WorkspaceRevision
			source.WorkspaceRevision = &revision
		}
		value.RequirementSource = &source
	}
	activity.verificationCheck = &value
	activity.verificationPlanRevision = planRevision
	activity.verificationStepID = stepID
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func taskValidationPlanCheckDigests(check task.PlanVerificationCheck) (command, wholeCheck, coverage string, ok bool) {
	checkBytes, err := json.Marshal(check)
	if err != nil {
		return "", "", "", false
	}
	coverageBytes, err := json.Marshal(check.CoveragePaths)
	if err != nil {
		return "", "", "", false
	}
	commandDigest := sha256.Sum256([]byte(check.Command))
	checkDigest := sha256.Sum256(checkBytes)
	coverageDigest := sha256.Sum256(coverageBytes)
	return hex.EncodeToString(commandDigest[:]), hex.EncodeToString(checkDigest[:]), hex.EncodeToString(coverageDigest[:]), true
}

// attestTaskValidationShellResult adds a server-owned receipt to a validated
// Runtime response. The broker calls it only after matching the selected plan
// step to the exact command and Workspace. Missing or inconsistent Runtime
// facts leave the operation usable but unable to prove H07.
func attestTaskValidationShellResult(current task.Task, planRevision int64, checkID string,
	check task.PlanVerificationCheck, bindingVersion int64, requestDigest []byte, output string) (string, bool) {
	if current.CurrentRevision == nil || current.WorkspaceCheckpointRevision == nil || current.WorkspaceID == "" ||
		planRevision < 1 || checkID == "" || bindingVersion < 1 || len(requestDigest) != sha256.Size ||
		len(output) == 0 || len(output) > maxAgentToolOutputBytes {
		return output, false
	}
	var shell taskStepShellOutput
	if decodeStrictRuntimeResult([]byte(output), &shell) != nil || shell.ExitCode < 0 || shell.ExitCode > 255 ||
		shell.TaskID != current.ID || shell.WorkspaceID != current.WorkspaceID || shell.BindingVersion != bindingVersion ||
		shell.WorkspaceViewID == "" || !taskWorkspaceViewIDPattern.MatchString(shell.WorkspaceViewID) ||
		shell.WorkspaceCheckpointRevision == nil || *shell.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
		!validSyncDigest(shell.WorkspaceManifestSHA256Before) || !validSyncDigest(shell.WorkspaceManifestSHA256After) {
		return output, false
	}
	commandDigest, checkDigest, coverageDigest, ok := taskValidationPlanCheckDigests(check)
	if !ok {
		return output, false
	}
	shell.VerificationCheck = &TaskValidationCheckExecutionReceipt{
		Version: 1, TaskRevision: *current.CurrentRevision, PlanRevision: planRevision, CheckID: checkID,
		Kind: string(check.Kind), Basis: string(check.Basis), CommandSHA256: commandDigest,
		CheckSHA256: checkDigest, CoverageSHA256: coverageDigest, RequestSHA256: hex.EncodeToString(requestDigest),
	}
	encoded, err := json.Marshal(shell)
	if err != nil || len(encoded) > maxAgentToolOutputBytes {
		return output, false
	}
	return string(encoded), true
}

func validTaskPlanVerificationCommand(check task.PlanVerificationCheck) bool {
	command := check.Command
	if command == "" || strings.TrimSpace(command) != command || strings.ContainsAny(command, "&|;\n\r<>$`\\\"'()#!") {
		return false
	}
	if check.Kind == task.PlanVerificationCheckTest {
		return isDirectTestCommand(command)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 || fields[0] == "env" || fields[0] == "sudo" || strings.Contains(fields[0], "=") {
		return false
	}
	for _, field := range fields {
		if strings.ContainsAny(field, "\x00") {
			return false
		}
	}
	return true
}
