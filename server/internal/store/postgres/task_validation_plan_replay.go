package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskValidationReplayTask struct {
	ownerID, initiatorID, conversationID, projectID, workspaceID, hostID string
	inputRevision, checkpointRevision                                    pgtype.Int8
	contextDigest                                                        []byte
	currentRevision, desiredRevision, authVersion                        int64
}

type taskValidationReplayBinding struct {
	planStepID, executionKind, executionID, agentSessionID, callID, toolName  string
	taskRevision, planRevision, authorizationVersion                          int64
	requestDigest                                                             []byte
	createdAt                                                                 time.Time
	operationTaskRevision                                                     int64
	operationCallID, operationToolName, operationStatus, operationWorkspaceID string
	operationHostID, operationRuntimeInstanceID                               string
	operationRequestHash                                                      []byte
	operationBindingVersion, operationInputRevision, operationCheckpoint      int64
	operationRuntimeGeneration, operationControlVersion                       int64
	operationResult                                                           string
	stepTaskRevision, stepSequence                                            int64
	stepStatus, stepToolName, stepCallID, stepAgentSessionID                  string
	stepRequestDigest                                                         []byte
}

type taskValidationReplayShellResult struct {
	ExitCode                      int                                      `json:"exit_code"`
	TimedOut                      bool                                     `json:"timed_out"`
	Stdout                        string                                   `json:"stdout"`
	Stderr                        string                                   `json:"stderr"`
	StdoutTruncated               bool                                     `json:"stdout_truncated"`
	StderrTruncated               bool                                     `json:"stderr_truncated"`
	TaskID                        string                                   `json:"task_id"`
	WorkspaceID                   string                                   `json:"workspace_id"`
	BindingVersion                int64                                    `json:"binding_version"`
	WorkspaceViewID               string                                   `json:"workspace_view_id"`
	WorkspaceCheckpointRevision   *int64                                   `json:"workspace_checkpoint_revision"`
	WorkspaceManifestSHA256Before string                                   `json:"workspace_manifest_sha256_before"`
	WorkspaceManifestSHA256After  string                                   `json:"workspace_manifest_sha256_after"`
	WorkspaceSnapshotSHA256Before string                                   `json:"workspace_snapshot_sha256_before"`
	WorkspaceSnapshotSHA256After  string                                   `json:"workspace_snapshot_sha256_after"`
	VerificationCheck             *app.TaskValidationCheckExecutionReceipt `json:"verification_check,omitempty"`
}

// ReadTaskValidationReplayEvidence returns the typed-check facts for the
// active review claim. The review completion transaction reads the same
// durable sources again before setting the H07 server verdict.
func (s *Store) ReadTaskValidationReplayEvidence(ctx context.Context,
	claim app.TaskValidationReviewClaim) (*app.TaskValidationReplayEvidence, error) {
	if ctx == nil || !app.ValidTaskValidationReviewClaim(claim) {
		return nil, domain.InvalidArgument("task_validation_review_claim", "Task validation review claim is invalid")
	}
	current, err := s.IsCurrentTaskValidationReviewClaim(ctx, claim)
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, domain.NewError(domain.CodeConflict, "Task validation review claim is no longer current")
	}
	var evidence *app.TaskValidationReplayEvidence
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		plan, err := latestTaskPlan(ctx, tx, claim.TaskID, claim.TaskRevision)
		if err != nil {
			return err
		}
		if plan == nil || plan.FixPlan == nil {
			evidence = nil
			return nil
		}
		current, err := readTaskValidationReplayTask(ctx, tx, claim)
		if err != nil {
			return err
		}
		evidence, err = loadTaskValidationPlanCheckReplay(ctx, tx, claim, *plan, current, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return evidence, nil
}

func readTaskValidationReplayTask(ctx context.Context, tx pgx.Tx, claim app.TaskValidationReviewClaim) (taskValidationReplayTask, error) {
	var current taskValidationReplayTask
	err := tx.QueryRow(ctx, `SELECT owner_id,initiator_id,conversation_id,COALESCE(project_id,''),COALESCE(workspace_id,''),
COALESCE(host_id,''),input_revision,workspace_checkpoint_revision,COALESCE(context_digest,''::bytea),
current_revision,desired_revision,auth_version
FROM tasks WHERE id=$1`, claim.TaskID).Scan(&current.ownerID, &current.initiatorID, &current.conversationID,
		&current.projectID, &current.workspaceID, &current.hostID, &current.inputRevision, &current.checkpointRevision,
		&current.contextDigest, &current.currentRevision, &current.desiredRevision, &current.authVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskValidationReplayTask{}, missingTaskError()
	}
	if err != nil {
		return taskValidationReplayTask{}, fmt.Errorf("read Task Workspace identity for H07: %w", err)
	}
	if current.ownerID != claim.OwnerID || current.initiatorID != claim.InitiatorID || current.conversationID != claim.ConversationID ||
		current.currentRevision != claim.TaskRevision || current.desiredRevision != claim.TaskRevision {
		return taskValidationReplayTask{}, domain.NewError(domain.CodeConflict, "Task validation review result is no longer current")
	}
	return current, nil
}

func loadTaskValidationPlanCheckReplay(ctx context.Context, tx pgx.Tx, claim app.TaskValidationReviewClaim,
	plan task.Plan, current taskValidationReplayTask, finalProofs ...*app.TaskValidationFinalWorkspaceSnapshotProof) (*app.TaskValidationReplayEvidence, error) {
	var finalProof *app.TaskValidationFinalWorkspaceSnapshotProof
	if len(finalProofs) > 0 {
		finalProof = finalProofs[0]
	}
	evidence := app.PendingTaskValidationReplayEvidence(&plan)
	if evidence == nil {
		return nil, nil
	}
	checkIDs := make([]string, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		if step.VerificationCheck != nil {
			checkIDs = append(checkIDs, step.ID)
		}
	}
	if len(checkIDs) == 0 {
		evidence.State = app.EvaluateTaskValidationPlanReplay(plan, evidence)
		return evidence, nil
	}
	if current.workspaceID == "" || !current.inputRevision.Valid || !current.checkpointRevision.Valid ||
		current.checkpointRevision.Int64 < current.inputRevision.Int64 || current.authVersion < 1 {
		evidence.State = app.TaskValidationReplayPending
		return evidence, nil
	}
	rows, err := tx.Query(ctx, `SELECT b.plan_step_id,b.execution_kind,b.execution_id,b.agent_session_id,b.call_id,b.tool_name,
b.task_revision,b.plan_revision,b.request_digest,b.authorization_version,b.created_at,
COALESCE(op.task_revision,-1),COALESCE(op.call_id,''),COALESCE(op.tool_name,''),COALESCE(op.status,''),
COALESCE(op.request_hash,''::bytea),COALESCE(op.workspace_id,''),COALESCE(op.host_id,''),
COALESCE(op.runtime_generation,0),COALESCE(op.runtime_instance_id,''),COALESCE(op.control_version,-1),
COALESCE(op.binding_version,0),
COALESCE(op.input_revision,-1),COALESCE(op.workspace_checkpoint_revision,-1),COALESCE(op.result,''),
COALESCE(step.task_revision,-1),COALESCE(step.sequence,0),COALESCE(step.status,''),COALESCE(step.tool_name,''),
COALESCE(step.tool_call_key,''),COALESCE(step.agent_session_id,''),COALESCE(step.request_digest,''::bytea)
FROM task_plan_step_execution_bindings b
LEFT JOIN task_operations op ON b.execution_kind='operation' AND op.id=b.execution_id AND op.task_id=b.task_id
LEFT JOIN task_steps step ON step.task_id=b.task_id AND step.agent_session_id=b.agent_session_id
 AND step.task_revision=b.task_revision AND step.tool_call_key=b.call_id AND step.tool_name=b.tool_name
WHERE b.owner_id=$1 AND b.task_id=$2 AND b.task_revision=$3 AND b.plan_revision=$4 AND b.plan_step_id=ANY($5::text[])
ORDER BY b.created_at,COALESCE(step.sequence,0)`, claim.OwnerID, claim.TaskID, claim.TaskRevision, plan.Revision, checkIDs)
	if err != nil {
		return nil, fmt.Errorf("read H07 plan-check execution bindings: %w", err)
	}
	defer rows.Close()
	latest := make(map[string]app.TaskValidationReplayCheckEvidence, len(checkIDs))
	latestSequence := make(map[string]int64, len(checkIDs))
	latestCreated := make(map[string]time.Time, len(checkIDs))
	for rows.Next() {
		var row taskValidationReplayBinding
		if err := rows.Scan(&row.planStepID, &row.executionKind, &row.executionID, &row.agentSessionID, &row.callID,
			&row.toolName, &row.taskRevision, &row.planRevision, &row.requestDigest, &row.authorizationVersion, &row.createdAt,
			&row.operationTaskRevision, &row.operationCallID, &row.operationToolName, &row.operationStatus,
			&row.operationRequestHash, &row.operationWorkspaceID, &row.operationHostID,
			&row.operationRuntimeGeneration, &row.operationRuntimeInstanceID, &row.operationControlVersion,
			&row.operationBindingVersion,
			&row.operationInputRevision, &row.operationCheckpoint, &row.operationResult,
			&row.stepTaskRevision, &row.stepSequence, &row.stepStatus, &row.stepToolName, &row.stepCallID,
			&row.stepAgentSessionID, &row.stepRequestDigest); err != nil {
			return nil, fmt.Errorf("scan H07 plan-check execution binding: %w", err)
		}
		fact := replayCheckFactFromBinding(claim, plan, current, row)
		_, exists := latest[row.planStepID]
		if exists {
			previousSequence := latestSequence[row.planStepID]
			previousCreated := latestCreated[row.planStepID]
			newer := fact.RunSequence > previousSequence ||
				(fact.RunSequence == previousSequence && row.createdAt.After(previousCreated)) ||
				(fact.RunSequence == 0 && row.createdAt.After(previousCreated))
			if !newer {
				continue
			}
		}
		latest[row.planStepID] = fact
		latestSequence[row.planStepID] = fact.RunSequence
		latestCreated[row.planStepID] = row.createdAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate H07 plan-check execution bindings: %w", err)
	}
	for index := range evidence.Checks {
		_, check := replayPlanStep(plan, evidence.Checks[index].CheckID)
		verified := check != nil && check.Basis == task.PlanVerificationCheckRiskSelected
		if check != nil {
			var err error
			verified, err = taskValidationPlanCheckRequirementSourceVerified(ctx, tx, claim, current, *check)
			if err != nil {
				return nil, err
			}
		}
		if checkFact, exists := latest[evidence.Checks[index].CheckID]; exists {
			evidence.Checks[index] = checkFact
		}
		evidence.Checks[index].RequirementSourceVerified = verified
	}
	if finalProof != nil {
		hostFence, err := readTaskValidationReplayHostFence(ctx, tx, current.hostID)
		if err != nil {
			return nil, err
		}
		baseline := app.TaskValidationReplaySnapshotBaseline{
			OwnerID: current.ownerID, InitiatorID: current.initiatorID, ConversationID: current.conversationID,
			TaskID: claim.TaskID, TaskRevision: current.currentRevision, HostID: current.hostID,
			HostRuntimeGeneration: hostFence.runtimeGeneration, HostRuntimeInstanceID: hostFence.runtimeInstanceID,
			HostControlVersion: hostFence.controlVersion, HostState: hostFence.state, HostControlState: hostFence.controlState,
			WorkspaceID: current.workspaceID, InputRevision: current.inputRevision.Int64,
			WorkspaceCheckpointRevision: current.checkpointRevision.Int64,
			InputManifestSHA256:         hex.EncodeToString(current.contextDigest),
		}
		if app.TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, baseline, evidence.Checks, finalProof) {
			evidence.FinalWorkspaceSnapshotVerified = true
			evidence.FinalWorkspaceSnapshotProof = finalProof
		}
	}
	evidence.State = app.EvaluateTaskValidationPlanReplay(plan, evidence)
	return evidence, nil
}

type taskValidationReplayHostFence struct {
	state, controlState, runtimeInstanceID string
	runtimeGeneration, controlVersion      int64
}

func readTaskValidationReplayHostFence(ctx context.Context, tx pgx.Tx, hostID string) (taskValidationReplayHostFence, error) {
	var fence taskValidationReplayHostFence
	if !domain.ValidHostID(hostID) {
		return fence, nil
	}
	err := tx.QueryRow(ctx, `SELECT state,control_state,runtime_generation,COALESCE(runtime_instance_id,''),control_version
FROM hosts WHERE id=$1 FOR SHARE`, hostID).Scan(&fence.state, &fence.controlState,
		&fence.runtimeGeneration, &fence.runtimeInstanceID, &fence.controlVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskValidationReplayHostFence{}, nil
	}
	if err != nil {
		return taskValidationReplayHostFence{}, fmt.Errorf("read current Host fence for H07 proof: %w", err)
	}
	return fence, nil
}

func replayCheckFactFromBinding(claim app.TaskValidationReviewClaim, plan task.Plan,
	current taskValidationReplayTask, row taskValidationReplayBinding) app.TaskValidationReplayCheckEvidence {
	step, check := replayPlanStep(plan, row.planStepID)
	fact := app.TaskValidationReplayCheckEvidence{CheckID: row.planStepID, Status: app.TaskValidationReplayPending,
		TaskID: claim.TaskID, TaskRevision: claim.TaskRevision, PlanRevision: plan.Revision,
		OperationID: row.executionID, CallID: row.callID, RunSequence: row.stepSequence,
		Kind: checkKind(check), Basis: checkBasis(check), CoveragePaths: checkCoverage(check),
		RequirementSource: checkRequirementSource(check)}
	if row.planStepID == "" || step.ID != row.planStepID || check == nil {
		return fact
	}
	if check.Basis == task.PlanVerificationCheckRiskSelected {
		fact.RequirementSourceVerified = true
	}
	if row.executionKind != "operation" || row.executionID == "" || row.agentSessionID == "" || row.callID == "" ||
		row.toolName != "shell_exec" || row.taskRevision != claim.TaskRevision || row.planRevision != plan.Revision ||
		row.authorizationVersion != current.authVersion || len(row.requestDigest) != sha256.Size ||
		row.operationTaskRevision != claim.TaskRevision || row.operationCallID != row.callID ||
		row.operationToolName != row.toolName || !bytes.Equal(row.requestDigest, row.operationRequestHash) ||
		row.operationWorkspaceID != current.workspaceID || row.operationHostID != current.hostID ||
		row.operationRuntimeGeneration < 1 || row.operationRuntimeInstanceID == "" || row.operationControlVersion < 0 ||
		row.operationBindingVersion < 1 ||
		row.operationInputRevision != current.inputRevision.Int64 || row.operationCheckpoint != current.checkpointRevision.Int64 ||
		row.stepTaskRevision != claim.TaskRevision || row.stepSequence < 1 || row.stepToolName != row.toolName ||
		row.stepCallID != row.callID || row.stepAgentSessionID != row.agentSessionID ||
		!bytes.Equal(row.stepRequestDigest, row.requestDigest) || len(row.operationResult) == 0 || len(row.operationResult) > app.MaxTaskValidationReviewReportBytes*16 {
		return fact
	}
	var shell taskValidationReplayShellResult
	decoder := json.NewDecoder(bytes.NewReader([]byte(row.operationResult)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&shell) != nil || shell.ExitCode < 0 || shell.ExitCode > 255 || shell.VerificationCheck == nil ||
		shell.TaskID != claim.TaskID || shell.WorkspaceID != current.workspaceID || shell.BindingVersion != row.operationBindingVersion ||
		shell.WorkspaceCheckpointRevision == nil || *shell.WorkspaceCheckpointRevision != row.operationCheckpoint ||
		!validReplayWorkspaceViewID(shell.WorkspaceViewID) || !validReplayDigest(shell.WorkspaceManifestSHA256Before) ||
		!validReplayDigest(shell.WorkspaceManifestSHA256After) || !validReplayDigest(shell.WorkspaceSnapshotSHA256Before) ||
		!validReplayDigest(shell.WorkspaceSnapshotSHA256After) {
		return fact
	}
	commandSHA256, checkSHA256, coverageSHA256, ok := replayPlanCheckDigests(*check)
	proof := shell.VerificationCheck
	if !ok || proof.Version != 2 || proof.TaskRevision != claim.TaskRevision || proof.PlanRevision != plan.Revision ||
		proof.CheckID != row.planStepID || proof.Kind != string(check.Kind) || proof.Basis != string(check.Basis) ||
		proof.CommandSHA256 != commandSHA256 || proof.CheckSHA256 != checkSHA256 || proof.CoverageSHA256 != coverageSHA256 ||
		proof.RequestSHA256 != hex.EncodeToString(row.requestDigest) ||
		(row.operationStatus != "succeeded" && row.operationStatus != "failed") ||
		(row.operationStatus == "succeeded" && row.stepStatus != "succeeded") ||
		(row.operationStatus == "failed" && row.stepStatus != "failed") {
		return fact
	}
	fact.RequestSHA256 = proof.RequestSHA256
	fact.CommandSHA256, fact.CheckSHA256, fact.CoverageSHA256 = proof.CommandSHA256, proof.CheckSHA256, proof.CoverageSHA256
	fact.ExitCode, fact.TimedOut = shell.ExitCode, shell.TimedOut
	fact.WorkspaceID, fact.WorkspaceBindingVersion = shell.WorkspaceID, shell.BindingVersion
	fact.WorkspaceCheckpointRevision, fact.WorkspaceViewID = *shell.WorkspaceCheckpointRevision, shell.WorkspaceViewID
	fact.WorkspaceManifestSHA256Before, fact.WorkspaceManifestSHA256After = shell.WorkspaceManifestSHA256Before, shell.WorkspaceManifestSHA256After
	fact.WorkspaceSnapshotSHA256Before, fact.WorkspaceSnapshotSHA256After = shell.WorkspaceSnapshotSHA256Before, shell.WorkspaceSnapshotSHA256After
	fact.HostID, fact.HostRuntimeGeneration, fact.HostRuntimeInstanceID = row.operationHostID, row.operationRuntimeGeneration, row.operationRuntimeInstanceID
	fact.HostControlVersion, fact.InputRevision = row.operationControlVersion, row.operationInputRevision
	if len(current.contextDigest) == sha256.Size {
		fact.InputManifestSHA256 = hex.EncodeToString(current.contextDigest)
	}
	if row.operationStatus == "failed" && (shell.ExitCode != 0 || shell.TimedOut) {
		fact.Status = app.TaskValidationReplayFail
	} else if row.operationStatus == "succeeded" && shell.ExitCode == 0 && !shell.TimedOut {
		fact.Status = app.TaskValidationReplayPass
	}
	return fact
}

func replayPlanStep(plan task.Plan, id string) (task.PlanStep, *task.PlanVerificationCheck) {
	for _, step := range plan.Steps {
		if step.ID == id {
			return step, step.VerificationCheck
		}
	}
	return task.PlanStep{}, nil
}

func checkKind(check *task.PlanVerificationCheck) task.PlanVerificationCheckKind {
	if check == nil {
		return ""
	}
	return check.Kind
}

func checkBasis(check *task.PlanVerificationCheck) task.PlanVerificationCheckBasis {
	if check == nil {
		return ""
	}
	return check.Basis
}

func checkCoverage(check *task.PlanVerificationCheck) []string {
	if check == nil {
		return nil
	}
	return append([]string(nil), check.CoveragePaths...)
}

func checkRequirementSource(check *task.PlanVerificationCheck) *task.PlanVerificationRequirementSource {
	if check == nil || check.RequirementSource == nil {
		return nil
	}
	source := *check.RequirementSource
	if source.WorkspaceRevision != nil {
		revision := *source.WorkspaceRevision
		source.WorkspaceRevision = &revision
	}
	return &source
}

func taskValidationPlanCheckRequirementSourceVerified(ctx context.Context, tx pgx.Tx, claim app.TaskValidationReviewClaim,
	current taskValidationReplayTask, check task.PlanVerificationCheck) (bool, error) {
	if check.Basis == task.PlanVerificationCheckRiskSelected {
		return check.RequirementSource == nil, nil
	}
	if check.Basis != task.PlanVerificationCheckRequired || check.RequirementSource == nil {
		return false, nil
	}
	source := check.RequirementSource
	switch source.Kind {
	case task.PlanVerificationRequirementUserMessage:
		if !domain.ValidMessageID(source.MessageID) || source.WorkspacePath != "" || source.WorkspaceRevision != nil {
			return false, nil
		}
		var content string
		err := tx.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1 AND owner_id=$2
AND conversation_id=$3 AND task_id=$4 AND role='user' AND delivery_status='applied'`,
			source.MessageID, claim.OwnerID, claim.ConversationID, claim.TaskID).Scan(&content)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read H07 requirement message source: %w", err)
		}
		digest := sha256.Sum256([]byte(content))
		return hex.EncodeToString(digest[:]) == source.ContentSHA256, nil
	case task.PlanVerificationRequirementProjectFile:
		if current.projectID == "" || current.workspaceID == "" || source.WorkspacePath == "" || source.WorkspaceRevision == nil ||
			!current.checkpointRevision.Valid || *source.WorkspaceRevision > current.checkpointRevision.Int64 || source.MessageID != "" {
			return false, nil
		}
		var digest string
		err := tx.QueryRow(ctx, `SELECT file.sha256 FROM workspace_revision_files file
JOIN workspace_revisions revision ON revision.workspace_id=file.workspace_id AND revision.revision=$3
WHERE file.workspace_id=$1 AND file.file_path=$2 AND file.valid_from_revision<=$3
 AND (file.valid_to_revision IS NULL OR file.valid_to_revision>$3)`, current.workspaceID, source.WorkspacePath,
			*source.WorkspaceRevision).Scan(&digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read H07 project-file requirement source: %w", err)
		}
		return digest == source.ContentSHA256, nil
	default:
		return false, nil
	}
}

func replayPlanCheckDigests(check task.PlanVerificationCheck) (command, wholeCheck, coverage string, ok bool) {
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

func validReplayDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != string(bytes.ToLower([]byte(value))) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validReplayWorkspaceViewID(value string) bool {
	if len(value) != 65 || value[0] != 'v' || value != string(bytes.ToLower([]byte(value))) {
		return false
	}
	_, err := hex.DecodeString(value[1:])
	return err == nil
}

var _ app.TaskValidationReplayEvidenceReader = (*Store)(nil)
