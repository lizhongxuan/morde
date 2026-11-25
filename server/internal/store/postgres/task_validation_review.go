package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) ClaimTaskValidationReviews(ctx context.Context, workerID string, lease time.Duration, limit int32) ([]app.TaskValidationReviewClaim, error) {
	if strings.TrimSpace(workerID) != workerID || workerID == "" || len(workerID) > 128 ||
		lease < time.Second || lease > 5*time.Minute || lease%time.Second != 0 || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("task_validation_review_claim", "Task validation review claim is invalid")
	}
	claims := make([]app.TaskValidationReviewClaim, 0, limit)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE task_validation_reviews
SET state='failed',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE attempts>=8 AND ((state='pending' AND available_at<=clock_timestamp())
  OR (state='processing' AND lease_expires_at<=clock_timestamp()))`); err != nil {
			return fmt.Errorf("fail exhausted Task validation reviews: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_validation_reviews review
SET state='superseded',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE review.state IN ('pending','processing') AND NOT EXISTS (
  SELECT 1 FROM tasks task
  JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
    AND event.task_revision=review.task_revision AND event.event_type='task.result'
    AND event.result_content_sha256=review.result_sha256
  WHERE task.id=review.task_id AND task.owner_id=review.owner_id AND task.status='completed'
    AND task.current_revision=review.task_revision AND task.desired_revision=review.task_revision
    AND task.result<>'' AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
)`); err != nil {
			return fmt.Errorf("supersede stale Task validation reviews: %w", err)
		}
		rows, err := tx.Query(ctx, `WITH claimable AS (
  SELECT review.task_id,review.task_revision,task.initiator_id,task.conversation_id FROM task_validation_reviews review
  JOIN tasks task ON task.id=review.task_id AND task.owner_id=review.owner_id
  JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
    AND event.task_revision=review.task_revision AND event.event_type='task.result'
    AND event.result_content_sha256=review.result_sha256
  WHERE task.status='completed' AND task.current_revision=review.task_revision
    AND task.desired_revision=review.task_revision AND task.result<>''
    AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
    AND review.attempts<8
    AND ((review.state='pending' AND review.available_at<=clock_timestamp())
      OR (review.state='processing' AND review.lease_expires_at<=clock_timestamp()))
  ORDER BY review.available_at,review.created_at,review.task_id,review.task_revision
  FOR UPDATE OF review SKIP LOCKED LIMIT $3
)
UPDATE task_validation_reviews review
SET state='processing',attempts=review.attempts+1,claim_generation=review.claim_generation+1,
    lease_owner=$1,lease_expires_at=clock_timestamp()+($2::bigint*interval '1 second'),updated_at=clock_timestamp()
FROM claimable
WHERE review.task_id=claimable.task_id AND review.task_revision=claimable.task_revision
RETURNING review.owner_id,claimable.initiator_id,claimable.conversation_id,review.task_id,review.task_revision,
review.result_event_sequence,review.result_sha256,review.claim_generation,review.lease_owner,review.lease_expires_at`,
			workerID, int64(lease/time.Second), limit)
		if err != nil {
			return fmt.Errorf("claim Task validation reviews: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var claim app.TaskValidationReviewClaim
			if err := rows.Scan(&claim.OwnerID, &claim.InitiatorID, &claim.ConversationID, &claim.TaskID, &claim.TaskRevision, &claim.ResultEventSequence,
				&claim.ResultSHA256, &claim.ClaimGeneration, &claim.LeaseOwner, &claim.LeaseExpiresAt); err != nil {
				return fmt.Errorf("read claimed Task validation review: %w", err)
			}
			claims = append(claims, claim)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate claimed Task validation reviews: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Store) RetryTaskValidationReview(ctx context.Context, claim app.TaskValidationReviewClaim, delay time.Duration) (bool, error) {
	if !app.ValidTaskValidationReviewClaim(claim) || delay < 0 || delay > 30*time.Minute || delay%time.Second != 0 {
		return false, domain.InvalidArgument("task_validation_review_claim", "Task validation review retry is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE task_validation_reviews
SET state=CASE WHEN attempts>=8 THEN 'failed' ELSE 'pending' END,
    available_at=CASE WHEN attempts>=8 THEN available_at ELSE clock_timestamp()+($8::bigint*interval '1 second') END,
    lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE task_id=$1 AND task_revision=$2 AND state='processing' AND lease_owner=$3 AND claim_generation=$4
  AND lease_expires_at>clock_timestamp() AND owner_id=$5 AND result_event_sequence=$6 AND result_sha256=$7`,
		claim.TaskID, claim.TaskRevision, claim.LeaseOwner, claim.ClaimGeneration, claim.OwnerID,
		claim.ResultEventSequence, claim.ResultSHA256, int64(delay/time.Second))
	if err != nil {
		return false, fmt.Errorf("retry Task validation review: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) IsCurrentTaskValidationReviewClaim(ctx context.Context, claim app.TaskValidationReviewClaim) (bool, error) {
	if ctx == nil || !app.ValidTaskValidationReviewClaim(claim) {
		return false, domain.InvalidArgument("task_validation_review_claim", "Task validation review claim is invalid")
	}
	var current bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_validation_reviews review
  JOIN tasks task ON task.id=review.task_id AND task.owner_id=review.owner_id
  JOIN task_events event ON event.task_id=review.task_id AND event.sequence=review.result_event_sequence
    AND event.task_revision=review.task_revision AND event.event_type='task.result'
    AND event.result_content_sha256=review.result_sha256
  WHERE review.task_id=$1 AND review.owner_id=$2 AND review.task_revision=$3
    AND review.result_event_sequence=$4 AND review.result_sha256=$5
    AND review.state='processing' AND review.lease_owner=$6 AND review.claim_generation=$7
    AND review.lease_expires_at>clock_timestamp()
    AND task.initiator_id=$8 AND task.conversation_id=$9 AND task.status='completed'
    AND task.current_revision=$3 AND task.desired_revision=$3 AND task.result<>''
    AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256
)`, claim.TaskID, claim.OwnerID, claim.TaskRevision, claim.ResultEventSequence, claim.ResultSHA256,
		claim.LeaseOwner, claim.ClaimGeneration, claim.InitiatorID, claim.ConversationID).Scan(&current)
	if err != nil {
		return false, fmt.Errorf("verify active Task validation review claim: %w", err)
	}
	return current, nil
}

func lockTaskValidationReviewWorkspaceFence(ctx context.Context, tx pgx.Tx, taskID string) (projectTaskWorkspaceFence, string, string, error) {
	var workspaceID, projectID, actorID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(workspace_id,''),COALESCE(project_id,''),initiator_id FROM tasks WHERE id=$1`, taskID).
		Scan(&workspaceID, &projectID, &actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectTaskWorkspaceFence{}, "", "", missingTaskError()
	}
	if err != nil {
		return projectTaskWorkspaceFence{}, "", "", fmt.Errorf("read Task Workspace fence before validation completion: %w", err)
	}
	if projectID != "" {
		fence, err := lockProjectTaskWorkspaceFence(ctx, tx, taskID, false)
		if err != nil {
			return projectTaskWorkspaceFence{}, "", "", err
		}
		return fence, fence.workspaceID, fence.actorID, nil
	}
	if workspaceID != "" {
		ownerID, err := lockWorkspaceOwnershipForRead(ctx, tx, workspaceID)
		if err != nil || ownerID != actorID {
			return projectTaskWorkspaceFence{}, "", "", domain.NewError(domain.CodeForbidden, "Task Workspace is not available for validation")
		}
		var available bool
		if err := tx.QueryRow(ctx, `SELECT state='ready' AND deleted_at IS NULL FROM workspaces WHERE id=$1`, workspaceID).Scan(&available); err != nil || !available {
			return projectTaskWorkspaceFence{}, "", "", domain.NewError(domain.CodeForbidden, "Task Workspace is not available for validation")
		}
	}
	return projectTaskWorkspaceFence{}, workspaceID, actorID, nil
}

func (s *Store) CompleteTaskValidationReview(ctx context.Context, completion app.TaskValidationReviewCompletion) (bool, error) {
	claim := completion.Claim
	if ctx == nil || !app.ValidTaskValidationReviewClaim(claim) {
		return false, domain.InvalidArgument("task_validation_review_claim", "Task validation review completion is invalid")
	}
	var completed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		projectFence, workspaceID, workspaceTaskActorID, err := lockTaskValidationReviewWorkspaceFence(ctx, tx, claim.TaskID)
		if err != nil {
			return err
		}
		var result, initiatorID, conversationID string
		err = tx.QueryRow(ctx, `SELECT task.result,task.initiator_id,task.conversation_id FROM tasks task
JOIN task_events event ON event.task_id=task.id AND event.sequence=$4 AND event.task_revision=$3
  AND event.event_type='task.result' AND event.result_content_sha256=$5
WHERE task.id=$1 AND task.owner_id=$2 AND task.status='completed'
  AND task.current_revision=$3 AND task.desired_revision=$3 AND task.result<>''
  AND sha256(convert_to(task.result,'UTF8'))=$5
FOR UPDATE OF task`, claim.TaskID, claim.OwnerID, claim.TaskRevision, claim.ResultEventSequence, claim.ResultSHA256).
			Scan(&result, &initiatorID, &conversationID)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return fmt.Errorf("lock current Task validation review result: %w", err)
		}
		if initiatorID != claim.InitiatorID || conversationID != claim.ConversationID {
			return nil
		}
		taskRow, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: claim.TaskID, OwnerID: claim.OwnerID})
		if err != nil {
			return fmt.Errorf("read Task authorization for validation review completion: %w", err)
		}
		if taskRow.InitiatorID != workspaceTaskActorID || taskRow.WorkspaceID.Valid && taskRow.WorkspaceID.String != workspaceID ||
			!taskRow.WorkspaceID.Valid && workspaceID != "" {
			return domain.NewError(domain.CodeConflict, "Task Workspace binding changed before validation completed")
		}
		if projectFence.projectID != "" {
			if err := verifyProjectTaskWorkspaceFence(ctx, tx, claim.TaskID, projectFence); err != nil {
				return err
			}
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, taskRow)
		if err != nil {
			return err
		}
		if !authorized {
			return nil
		}
		scope, current, err := loadTaskValidationReviewScopeInTx(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !current {
			return nil
		}
		if scope.ExecutionCallID == "" || completion.ExecutionModel.CallID != scope.ExecutionCallID {
			return domain.NewError(domain.CodeConflict, "Task validation review is not bound to the current execution model response")
		}
		evidence, err := loadTaskValidationEvidence(ctx, tx, claim.TaskID, claim.TaskRevision, result)
		if err != nil {
			return err
		}
		if err := app.ValidateTaskValidationReviewCompletion(completion, evidence); err != nil {
			return err
		}
		facts, err := loadTaskValidationVerdictFacts(ctx, tx, claim, result, evidence, completion.Report, completion.ArtifactContentProofs)
		if err != nil {
			return err
		}
		verdict := app.SummarizeTaskValidationEvaluations(app.EvaluateTaskValidationReview(completion.Report, facts))
		if completion.SessionID != completion.ReviewerModel.SessionID {
			return domain.NewError(domain.CodeConflict, "Task validation review session does not match its model response")
		}
		if err := verifyTaskValidationReviewExecutionModelResponseInTx(ctx, tx, claim, completion.ExecutionModel); err != nil {
			return err
		}
		if err := verifyTaskValidationReviewModelResponseInTx(ctx, tx, claim, completion.ReviewerModel, completion.ResponseText); err != nil {
			return err
		}
		report, err := json.Marshal(completion.Report)
		if err != nil || len(report) > app.MaxTaskValidationReviewReportBytes {
			return domain.InvalidArgument("task_validation_review", "Task validation review report is invalid")
		}
		artifactProofs := completion.ArtifactContentProofs
		if artifactProofs == nil {
			artifactProofs = []app.TaskValidationReviewArtifactContentProof{}
		}
		artifactContentEvidence, err := json.Marshal(artifactProofs)
		if err != nil || len(artifactContentEvidence) > app.MaxTaskValidationReviewArtifactProofBytes {
			return domain.InvalidArgument("task_validation_review", "Task artifact content proof exceeds its limit")
		}
		updated, err := tx.Exec(ctx, `UPDATE task_validation_reviews review
SET state='reviewed',lease_owner=NULL,lease_expires_at=NULL,
    validation_state=$25,
    artifact_content_evidence=$26::jsonb,
    reviewer_session_id=$8,execution_model_id=$9,execution_connection_version=$10,execution_endpoint=$11,execution_configured_model=$12,
    execution_reported_model=$13,execution_model_family=$14,execution_catalog_version=$15,execution_model_call_id=$24,
    reviewer_model_id=$16,reviewer_connection_version=$17,reviewer_endpoint=$18,reviewer_configured_model=$19,reviewer_reported_model=$20,
    reviewer_model_family=$21,reviewer_catalog_version=$22,review_report=$23::jsonb,
    reviewed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE review.task_id=$1 AND review.task_revision=$3 AND review.owner_id=$2
  AND review.result_event_sequence=$4 AND review.result_sha256=$7
  AND review.state='processing' AND review.lease_owner=$5 AND review.claim_generation=$6
  AND review.lease_expires_at>clock_timestamp() AND review.execution_model_call_id=$24
  AND EXISTS (SELECT 1 FROM tasks task WHERE task.id=review.task_id AND task.owner_id=review.owner_id
    AND task.status='completed' AND task.current_revision=review.task_revision
    AND task.desired_revision=review.task_revision AND task.result<>''
    AND sha256(convert_to(task.result,'UTF8'))=review.result_sha256)`,
			claim.TaskID, claim.OwnerID, claim.TaskRevision, claim.ResultEventSequence, claim.LeaseOwner,
			claim.ClaimGeneration, claim.ResultSHA256,
			completion.SessionID,
			completion.ExecutionModel.ConnectionID, completion.ExecutionModel.ConnectionVersion,
			completion.ExecutionModel.Endpoint,
			completion.ExecutionModel.ConfiguredModel, completion.ExecutionModel.ReportedModel,
			completion.ExecutionModel.Identity.Family, completion.ExecutionModel.Identity.CatalogVersion,
			completion.ReviewerModel.ConnectionID, completion.ReviewerModel.ConnectionVersion,
			completion.ReviewerModel.Endpoint,
			completion.ReviewerModel.ConfiguredModel, completion.ReviewerModel.ReportedModel,
			completion.ReviewerModel.Identity.Family, completion.ReviewerModel.Identity.CatalogVersion, report,
			completion.ExecutionModel.CallID, string(verdict.State), artifactContentEvidence)
		if err != nil {
			return fmt.Errorf("complete Task validation review: %w", err)
		}
		completed = updated.RowsAffected() == 1
		return nil
	})
	return completed, err
}

func loadTaskValidationVerdictFacts(ctx context.Context, tx pgx.Tx, claim app.TaskValidationReviewClaim, result string,
	evidence *app.TaskValidationEvidenceRecord, report app.TaskValidationReviewReport,
	artifactProofs []app.TaskValidationReviewArtifactContentProof) (app.TaskValidationVerdictFacts, error) {
	var hasExecution, hasPendingClarifications bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_steps WHERE task_id=$1 AND (kind='tool' OR tool_name IS NOT NULL)
  UNION ALL SELECT 1 FROM task_operations WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_hub_operations WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_sandbox_call_receipts WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_workspace_view_deletions WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_agent_request_reconciliations WHERE task_id=$1
  UNION ALL SELECT 1 FROM task_agent_sessions WHERE task_id=$1 AND parent_session_id IS NOT NULL
  UNION ALL SELECT 1 FROM task_artifacts WHERE task_id=$1
), EXISTS (SELECT 1 FROM task_clarification_requests WHERE task_id=$1 AND status='pending')`, claim.TaskID).
		Scan(&hasExecution, &hasPendingClarifications); err != nil {
		return app.TaskValidationVerdictFacts{}, fmt.Errorf("read Task validation facts: %w", err)
	}
	// E09: the clarification history is known when no clarification is
	// still unanswered. Answered and cancelled requests both have a
	// terminal state with a timestamp, so the history is verifiable; a
	// pending request means the history is not closed yet.
	facts := app.TaskValidationVerdictFacts{ClarificationHistoryKnown: !hasPendingClarifications}
	eligibleContent := make(map[app.TaskValidationEvidenceRefRecord]struct{})
	if evidence != nil {
		eligibleWeb := make(map[app.TaskValidationEvidenceRefRecord]struct{})
		rows, err := tx.Query(ctx, `SELECT st.id,st.sequence,st.task_revision,COALESCE(st.tool_call_key,''),d.content_evidence_sha256
FROM task_steps st JOIN task_step_details d ON d.task_id=st.task_id AND d.step_id=st.id
WHERE st.task_id=$1 AND st.task_revision=$2 AND st.status='succeeded' AND st.detail_state='available'
  AND d.task_revision=$2 AND st.tool_name IN ('morde_web_search','morde_web_read')
  AND octet_length(d.content_evidence_sha256)=32`,
			claim.TaskID, evidence.TaskRevision)
		if err != nil {
			return app.TaskValidationVerdictFacts{}, fmt.Errorf("read Task web content evidence: %w", err)
		}
		for rows.Next() {
			var ref app.TaskValidationEvidenceRefRecord
			var digest []byte
			ref.Kind = app.TaskValidationEvidenceAction
			if err := rows.Scan(&ref.ID, &ref.Sequence, &ref.TaskRevision, &ref.CallID, &digest); err != nil {
				rows.Close()
				return app.TaskValidationVerdictFacts{}, fmt.Errorf("scan Task web content evidence: %w", err)
			}
			if len(digest) == sha256.Size && ref.ID != "" && ref.Sequence > 0 {
				eligibleWeb[ref] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return app.TaskValidationVerdictFacts{}, fmt.Errorf("iterate Task web content evidence: %w", err)
		}
		rows.Close()
		for ref := range eligibleWeb {
			eligibleContent[ref] = struct{}{}
		}
	}
	artifactContent, err := currentTaskValidationReviewArtifactContentRefsInTx(ctx, tx, claim, evidence, artifactProofs)
	if err != nil {
		return app.TaskValidationVerdictFacts{}, err
	}
	for ref := range artifactContent {
		eligibleContent[ref] = struct{}{}
	}
	if evidence != nil {
		facts.EvidenceContentVerified = verifiedTaskEvidenceContentCoverage(result, evidence.Claims, report.Findings, eligibleContent)
	}
	if hasExecution {
		facts.TaskExecution = app.TaskValidationFactPresent
		if err := loadTaskValidationExecutionFacts(ctx, tx, claim, &facts); err != nil {
			return app.TaskValidationVerdictFacts{}, err
		}
	} else {
		facts.TaskExecution = app.TaskValidationFactAbsent
		facts.IrreversibleAction = app.TaskValidationFactAbsent
		facts.RepeatedError = app.TaskValidationFactAbsent
	}
	return facts, nil
}

// loadTaskValidationExecutionFacts derives the H05/H06/H07 server facts
// (E09) from the durable ledger, independently of the execution broker
// that recorded it and of the review model. Every load failure fails
// closed: the review completion errors and the verdict stays pending
// instead of passing on partial facts.
//
//   - H05: approval-requiring steps and Host, MCP and Workspace operation
//     receipts must carry matching call identities and exact approved
//     request ID, version, revision and hash bindings. Unknown or incomplete
//     operation identities keep the fact pending. Hub and sandbox-specific
//     receipts still need their own coverage before all histories can be
//     considered independently verified.
//   - H06: repeated errors and their correction records, from the step
//     digests.
//   - H07: fix-task replay remains pending without immutable workspace
//     and coverage proof; other tasks use the verified read-back compare.
func loadTaskValidationExecutionFacts(ctx context.Context, tx pgx.Tx, claim app.TaskValidationReviewClaim, facts *app.TaskValidationVerdictFacts) error {
	steps, err := listTaskStepsWithActivity(ctx, tx, claim.TaskID)
	if err != nil {
		return fmt.Errorf("read Task validation steps: %w", err)
	}
	actions, unclassifiable := taskValidationApprovalActions(steps)
	ledgerActions, ledgerUnknown, err := listTaskValidationApprovalLedger(ctx, tx, claim.TaskID)
	if err != nil {
		return err
	}
	var uncovered bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_sandbox_call_receipts WHERE task_id=$1 AND status IN ('dispatched','succeeded','failed','unknown')
 UNION ALL SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND action='command' AND status IN ('dispatched','succeeded','failed','unknown')
 UNION ALL SELECT 1 FROM task_agent_request_reconciliations WHERE task_id=$1 AND phase='tool' AND state='pending'
)`, claim.TaskID).Scan(&uncovered); err != nil {
		return fmt.Errorf("read Task validation unclassified execution: %w", err)
	}
	// shortcut: Hub commands and sandbox receipts lack the same per-call
	// approval binding; keep the fact unknown until they can be reconciled.
	facts.IrreversibleAction, facts.ApprovalHistoryVerified = evaluateTaskValidationApprovalLedger(actions, ledgerActions, unclassifiable || ledgerUnknown || uncovered)
	facts.RepeatedError, facts.CorrectionRecordVerified = app.EvaluateRepeatedErrorCorrections(steps)
	plan, err := latestTaskPlan(ctx, tx, claim.TaskID, claim.TaskRevision)
	if err != nil {
		return fmt.Errorf("read Task validation plan: %w", err)
	}
	hasFixPlan := plan != nil && plan.FixPlan != nil
	facts.ReplayVerified = !hasFixPlan && app.EvaluateTaskValidationReplay(false, app.TestGateResult{}, facts.EvidenceContentVerified)
	// shortcut: shell exit stamps do not bind test coverage to an immutable
	// Workspace version; keep fix-task replay pending until that proof exists.
	return nil
}

type taskValidationLedgerAction struct {
	action        app.TaskApprovalAction
	status        string
	approvalID    string
	approvalHash  []byte
	boundApproval bool
}

// Each approval-bearing operation must carry the exact approved request
// binding. A succeeded step alone is not an operation receipt.
func listTaskValidationApprovalLedger(ctx context.Context, tx pgx.Tx, taskID string) ([]taskValidationLedgerAction, bool, error) {
	rows, err := tx.Query(ctx, `WITH operation AS (
 SELECT task_id,status,task_revision,call_id,tool_name,request_hash,approval_id,approval_version,approval_request_hash
 FROM task_operations WHERE task_id=$1
 UNION ALL
 SELECT task_id,status,task_revision,call_id,tool_name,request_hash,approval_id,approval_version,approval_request_hash
 FROM task_mcp_connector_operations WHERE task_id=$1
 UNION ALL
 SELECT task_id,state,task_revision,call_id,tool_name,request_hash,approval_id,approval_version,approval_request_hash
 FROM task_workspace_publish_receipts WHERE task_id=$1
)
SELECT COALESCE(o.call_id,''),COALESCE(o.tool_name,''),o.request_hash,o.approval_request_hash,o.task_revision,o.status,COALESCE(o.approval_id,''),
 COALESCE(p.status='approved' AND p.decision='approve' AND p.responded_by_user_id IS NOT NULL
   AND p.responded_at IS NOT NULL AND (p.expires_at IS NULL OR p.responded_at < p.expires_at)
   AND p.task_revision=o.task_revision AND p.call_id=o.call_id AND p.tool_name=o.tool_name
   AND p.request_hash=o.approval_request_hash,false)
FROM operation o LEFT JOIN task_pending_requests p
 ON p.task_id=o.task_id AND p.id=o.approval_id AND p.version=o.approval_version
WHERE o.status IN ('succeeded','failed','dispatched','unknown','pending')`, taskID)
	if err != nil {
		return nil, false, fmt.Errorf("read Task validation operation bindings: %w", err)
	}
	defer rows.Close()
	var result []taskValidationLedgerAction
	unknown := false
	for rows.Next() {
		var item taskValidationLedgerAction
		var revision pgtype.Int8
		if err := rows.Scan(&item.action.CallID, &item.action.ToolName, &item.action.RequestDigest, &item.approvalHash,
			&revision, &item.status, &item.approvalID, &item.boundApproval); err != nil {
			return nil, false, fmt.Errorf("scan Task validation operation binding: %w", err)
		}
		if revision.Valid {
			value := revision.Int64
			item.action.TaskRevision = &value
		}
		if item.status == "unknown" || item.status == "dispatched" || item.status == "pending" ||
			item.action.CallID == "" || item.action.ToolName == "" || item.action.TaskRevision == nil || len(item.action.RequestDigest) != 32 {
			unknown = true
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate Task validation operation bindings: %w", err)
	}
	return result, unknown, nil
}

func evaluateTaskValidationApprovalLedger(steps []app.TaskApprovalAction, ledger []taskValidationLedgerAction,
	unknown bool) (app.TaskValidationFactState, bool) {
	if unknown {
		return app.TaskValidationFactUnknown, false
	}
	present := len(steps) > 0
	verifiedBindings := true
	for _, item := range ledger {
		if item.approvalID == "" && !app.TaskToolRequiresPerCallApproval(item.action.ToolName) {
			continue
		}
		present = true
		if !item.boundApproval {
			verifiedBindings = false
		}
		matched := false
		for _, step := range steps {
			if taskValidationStepMatchesOperation(step, item) {
				matched = true
				break
			}
		}
		if !matched {
			verifiedBindings = false
		}
	}
	for _, step := range steps {
		matched := false
		for _, item := range ledger {
			if taskValidationStepMatchesOperation(step, item) {
				matched = true
				break
			}
		}
		if !matched {
			verifiedBindings = false
		}
	}
	if !present {
		return app.TaskValidationFactAbsent, false
	}
	return app.TaskValidationFactPresent, verifiedBindings
}

func taskValidationStepMatchesOperation(step app.TaskApprovalAction, item taskValidationLedgerAction) bool {
	return item.status == "succeeded" && item.action.CallID == step.CallID && item.action.ToolName == step.ToolName &&
		item.action.TaskRevision != nil && step.TaskRevision != nil && *item.action.TaskRevision == *step.TaskRevision &&
		(bytes.Equal(item.action.RequestDigest, step.RequestDigest) || bytes.Equal(item.approvalHash, step.RequestDigest)) && item.boundApproval
}

// taskValidationApprovalActions collects the executed approval-requiring
// actions from the step ledger. A succeeded step with no tool name cannot
// be classified, so unclassifiable is reported and the approval fact
// stays unknown instead of guessing.
func taskValidationApprovalActions(steps []task.Step) ([]app.TaskApprovalAction, bool) {
	actions := make([]app.TaskApprovalAction, 0)
	unclassifiable := false
	for _, step := range steps {
		if step.Status != task.StepSucceeded {
			continue
		}
		if step.ToolName == "" {
			unclassifiable = true
			continue
		}
		if !app.TaskToolRequiresPerCallApproval(step.ToolName) {
			continue
		}
		actions = append(actions, app.TaskApprovalAction{
			CallID:        step.ToolCallKey,
			ToolName:      step.ToolName,
			RequestDigest: step.RequestDigest,
			TaskRevision:  step.TaskRevision,
		})
	}
	return actions, unclassifiable
}

func currentTaskValidationReviewArtifactContentRefsInTx(ctx context.Context, tx pgx.Tx,
	claim app.TaskValidationReviewClaim, evidence *app.TaskValidationEvidenceRecord,
	proofs []app.TaskValidationReviewArtifactContentProof) (map[app.TaskValidationEvidenceRefRecord]struct{}, error) {
	eligible := make(map[app.TaskValidationEvidenceRefRecord]struct{})
	if err := app.ValidateTaskValidationReviewArtifactContentProofs(proofs, evidence); err != nil {
		return nil, err
	}
	for _, proof := range proofs {
		if !proof.TextContent || !proof.ContentComplete {
			continue
		}
		var current bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_artifacts artifact
  JOIN tasks task ON task.id=artifact.task_id AND task.owner_id=$2
  JOIN task_steps artifact_step ON artifact_step.task_id=artifact.task_id AND artifact_step.id=artifact.step_id
  JOIN workspaces workspace ON workspace.id=artifact.workspace_id
    AND workspace.owner_id=COALESCE(task.workspace_owner_id,task.owner_id)
    AND workspace.state='ready' AND workspace.deleted_at IS NULL
  JOIN workspace_revision_files current_file ON current_file.workspace_id=artifact.workspace_id
    AND current_file.file_path=artifact.path AND current_file.valid_to_revision IS NULL
  WHERE artifact.id=$3 AND artifact.task_id=$1 AND task.workspace_id=artifact.workspace_id
    AND artifact.workspace_id=$4 AND artifact.workspace_revision=$5
    AND artifact.path=$6 AND artifact.media_type=$7 AND artifact.size_bytes=$8 AND artifact.sha256=$9
    AND artifact_step.sequence=$10 AND artifact_step.task_revision=$11 AND artifact_step.status='succeeded'
    AND COALESCE(artifact_step.tool_call_key,'')=$12
    AND current_file.size_bytes=$8 AND current_file.sha256=$9
    AND (SELECT count(*) FROM task_steps source_step WHERE source_step.task_id=artifact.task_id
      AND source_step.task_revision=$11 AND source_step.tool_call_key=$12 AND source_step.status='succeeded')=1
)`, claim.TaskID, claim.OwnerID, proof.Ref.ID, proof.WorkspaceID, proof.Revision, proof.Path, proof.MediaType,
			proof.SizeBytes, proof.SHA256, proof.Ref.Sequence, proof.Ref.TaskRevision, proof.Ref.CallID).Scan(&current)
		if err != nil {
			return nil, fmt.Errorf("verify current Task artifact validation proof: %w", err)
		}
		if current {
			eligible[proof.Ref] = struct{}{}
		}
	}
	return eligible, nil
}

func verifiedTaskEvidenceContentCoverage(result string, claims []app.TaskValidationEvidenceClaimRecord,
	findings []app.TaskValidationReviewFinding, eligible map[app.TaskValidationEvidenceRefRecord]struct{}) bool {
	if len(claims) == 0 || len(eligible) == 0 {
		return false
	}
	var evidenceFinding *app.TaskValidationReviewFinding
	for i := range findings {
		if findings[i].ID == app.TaskHardCheckEvidence {
			evidenceFinding = &findings[i]
			break
		}
	}
	if evidenceFinding == nil || evidenceFinding.Assessment != app.TaskValidationReviewSupports {
		return false
	}
	findingRefs := make(map[app.TaskValidationEvidenceRefRecord]struct{}, len(evidenceFinding.EvidenceRefs))
	for _, ref := range evidenceFinding.EvidenceRefs {
		findingRefs[ref] = struct{}{}
	}
	ordered := append([]app.TaskValidationEvidenceClaimRecord(nil), claims...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartByte < ordered[j].StartByte })
	coveredUntil := 0
	for _, claim := range ordered {
		start, end := int(claim.StartByte), int(claim.EndByte)
		if start < coveredUntil || start < 0 || end <= start || end > len(result) || strings.TrimSpace(result[coveredUntil:start]) != "" {
			return false
		}
		claimHasReviewedSource := false
		for _, ref := range claim.EvidenceRefs {
			if _, ok := eligible[ref]; !ok {
				continue
			}
			if _, reviewed := findingRefs[ref]; reviewed {
				claimHasReviewedSource = true
				break
			}
		}
		if !claimHasReviewedSource {
			return false
		}
		coveredUntil = end
	}
	return strings.TrimSpace(result[coveredUntil:]) == ""
}

func verifyTaskValidationReviewExecutionModelResponseInTx(ctx context.Context, tx pgx.Tx,
	claim app.TaskValidationReviewClaim, model app.TaskValidationReviewModel) error {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_validation_reviews review
  JOIN task_model_call_responses response ON response.owner_id=review.owner_id
    AND response.source='task' AND response.call_id=review.execution_model_call_id
  JOIN model_call_ledger ledger ON ledger.owner_id=response.owner_id AND ledger.source=response.source
    AND ledger.call_id=response.call_id AND ledger.task_id=response.task_id AND ledger.state='settled'
    AND ledger.input_tokens IS NOT NULL AND ledger.output_tokens IS NOT NULL
  JOIN task_agent_sessions session ON session.task_id=response.task_id AND session.session_id=response.agent_session_id
    AND session.task_revision=$3
  JOIN tasks task ON task.id=response.task_id AND task.owner_id=response.task_owner_id AND task.initiator_id=response.initiator_id
    AND task.status='completed' AND task.current_revision=$3 AND task.desired_revision=$3
    AND task.agent_session_id=response.agent_root_session_id
  WHERE review.owner_id=$9 AND review.task_id=$1 AND review.task_revision=$3
    AND review.result_event_sequence=$10 AND review.result_sha256=$11
    AND review.state='processing' AND review.lease_owner=$12 AND review.claim_generation=$13
    AND review.lease_expires_at>clock_timestamp()
    AND response.owner_id=$2 AND response.task_id=$1 AND response.call_id=$4
    AND response.task_owner_id=$9 AND response.initiator_id=$2 AND response.response_digest=$5
    AND response.model_id=$6 AND response.provider_model=$7 AND response.connection_version=$8
    AND response.provider_endpoint=$14 AND response.provider_reported_model=$15
    AND response.agent_session_id=$16 AND response.through_sequence=$17
)`, claim.TaskID, claim.InitiatorID, claim.TaskRevision, model.CallID, model.ResponseDigest,
		model.ConnectionID, model.ConfiguredModel, model.ConnectionVersion, claim.OwnerID, claim.ResultEventSequence,
		claim.ResultSHA256, claim.LeaseOwner, claim.ClaimGeneration, model.Endpoint, model.ReportedModel,
		model.SessionID, model.ThroughSequence).Scan(&found)
	if err != nil {
		return fmt.Errorf("verify Task execution model response: %w", err)
	}
	if !found {
		return domain.NewError(domain.CodeConflict, "Task execution model response is not bound to the current Task revision")
	}
	return nil
}

func verifyTaskValidationReviewModelResponseInTx(ctx context.Context, tx pgx.Tx,
	claim app.TaskValidationReviewClaim, model app.TaskValidationReviewModel, responseText string) error {
	expectedCallID, err := app.DSHTaskValidationReviewModelCallID(app.TaskValidationReviewModelFence{
		Claim: claim, InitiatorID: claim.InitiatorID, SessionID: model.SessionID, ThroughSequence: model.ThroughSequence,
	})
	if err != nil || expectedCallID != model.CallID {
		return domain.NewError(domain.CodeConflict, "Task validation review model call identity is invalid")
	}
	textDigest := sha256.Sum256([]byte(responseText))
	var found bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_validation_review_model_responses response
  JOIN model_call_ledger ledger ON ledger.owner_id=response.owner_id AND ledger.source=response.source
    AND ledger.call_id=response.call_id AND ledger.task_id=response.task_id AND ledger.state='settled'
    AND ledger.input_tokens IS NOT NULL AND ledger.output_tokens IS NOT NULL
  WHERE response.owner_id=$2 AND response.task_id=$1 AND response.task_revision=$3
    AND response.result_event_sequence=$4 AND response.result_sha256=$5 AND response.claim_generation=$6
    AND response.lease_owner=$7 AND response.session_id=$8 AND response.through_sequence=$9
    AND response.call_id=$10 AND response.response_digest=$11 AND response.model_id=$12
    AND response.provider_model=$13 AND response.connection_version=$14 AND response.source='review'
    AND response.provider_endpoint=$15 AND response.provider_reported_model=$16
    AND response.response_text_sha256=$17
)`, claim.TaskID, claim.InitiatorID, claim.TaskRevision, claim.ResultEventSequence, claim.ResultSHA256,
		claim.ClaimGeneration, claim.LeaseOwner, model.SessionID, model.ThroughSequence, model.CallID, model.ResponseDigest,
		model.ConnectionID, model.ConfiguredModel, model.ConnectionVersion, model.Endpoint, model.ReportedModel, textDigest[:]).Scan(&found)
	if err != nil {
		return fmt.Errorf("verify Task validation review model response: %w", err)
	}
	if !found {
		return domain.NewError(domain.CodeConflict, "Task validation review response is not bound to the active claim")
	}
	return nil
}

func enqueueTaskValidationReviewInTx(ctx context.Context, tx pgx.Tx, ownerID, taskID string,
	taskRevision, resultSequence int64, result string) error {
	digest := sha256.Sum256([]byte(result))
	var rootSessionID string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(agent_session_id,'') FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).Scan(&rootSessionID); err != nil {
		return fmt.Errorf("read Task root session for review: %w", err)
	}
	var executionModelCallID *string
	if rootSessionID != "" {
		var callID string
		err := tx.QueryRow(ctx, `SELECT call_id FROM task_model_call_responses
WHERE owner_id=$1 AND task_id=$2 AND agent_session_id=$3 AND agent_root_session_id=$3
ORDER BY through_sequence DESC,created_at DESC,call_id DESC LIMIT 1`, ownerID, taskID, rootSessionID).Scan(&callID)
		if err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("read Task execution model response for review: %w", err)
		}
		if err == nil {
			executionModelCallID = &callID
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_validation_reviews
(task_id,owner_id,task_revision,result_event_sequence,result_sha256,execution_model_call_id)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (task_id,task_revision) DO NOTHING`, taskID, ownerID, taskRevision, resultSequence, digest[:], executionModelCallID); err != nil {
		return fmt.Errorf("enqueue Task validation review: %w", err)
	}
	var storedSequence int64
	var storedDigest []byte
	var storedExecutionModelCallID string
	if err := tx.QueryRow(ctx, `SELECT result_event_sequence,result_sha256,COALESCE(execution_model_call_id,'') FROM task_validation_reviews
WHERE task_id=$1 AND task_revision=$2`, taskID, taskRevision).Scan(&storedSequence, &storedDigest, &storedExecutionModelCallID); err != nil {
		return fmt.Errorf("verify Task validation review identity: %w", err)
	}
	expectedExecutionModelCallID := ""
	if executionModelCallID != nil {
		expectedExecutionModelCallID = *executionModelCallID
	}
	if storedSequence != resultSequence || !bytes.Equal(storedDigest, digest[:]) || storedExecutionModelCallID != expectedExecutionModelCallID {
		return domain.NewError(domain.CodeIdempotencyConflict, "Task revision is already bound to a different review result")
	}
	return nil
}
