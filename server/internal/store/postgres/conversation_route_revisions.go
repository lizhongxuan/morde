package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

// beginConversationTaskRevision fences the old execution in the same
// transaction that records the user's modification. It never claims that a
// new revision has started: the existing stop worker must first verify every
// operation and the whole Agent process tree has stopped.
func beginConversationTaskRevision(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, candidate app.ConversationRouteCandidate) error {
	var status, waitingReason, interruptedOwner, interruptedAgent, stopID string
	var desired, leaseVersion, interruptedVersion, interruptedGeneration int64
	var safeAfter time.Time
	err := tx.QueryRow(ctx, `SELECT status,waiting_reason,desired_revision,lease_version,lease_owner,agent_instance_id,agent_generation,
GREATEST(COALESCE(runtime_lease_safe_after,'-infinity'::timestamptz),
COALESCE(lease_expires_at,'-infinity'::timestamptz),clock_timestamp())
FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 FOR UPDATE`,
		candidate.TaskID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).
		Scan(&status, &waitingReason, &desired, &leaseVersion, &interruptedOwner, &interruptedAgent, &interruptedGeneration, &safeAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	if err != nil {
		return fmt.Errorf("lock task for revision: %w", err)
	}
	if desired != candidate.Revision || desired == math.MaxInt64 || leaseVersion == math.MaxInt64 {
		return domain.NewError(domain.CodeConflict, "task revision changed before modification")
	}
	interruptedVersion = leaseVersion
	if status == string(task.StatusPausing) {
		err := tx.QueryRow(ctx, `SELECT COALESCE(stop_request_id,''),interrupted_lease_owner,interrupted_lease_version,
interrupted_agent_instance_id,interrupted_agent_generation FROM task_revision_intents
WHERE task_id=$1 AND state='stopping' ORDER BY expected_revision LIMIT 1`, candidate.TaskID).
			Scan(&stopID, &interruptedOwner, &interruptedVersion, &interruptedAgent, &interruptedGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task is being stopped for another reason")
		}
		if err != nil {
			return fmt.Errorf("check task revision stop: %w", err)
		}
	} else if status != string(task.StatusQueued) && status != string(task.StatusRunning) &&
		status != string(task.StatusWaitingUser) && status != string(task.StatusPaused) {
		return domain.NewError(domain.CodeConflict, "task cannot be adjusted in its current state")
	}
	if status == string(task.StatusPaused) && waitingReason != "" {
		return domain.NewError(domain.CodeConflict, "paused task requires independent recovery before adjustment")
	}
	if status != string(task.StatusPausing) && status != string(task.StatusPaused) {
		stopID, err = domain.NewTaskActionID()
		if err != nil {
			return err
		}
		if err := cancelPendingTaskRequest(ctx, tx, candidate.TaskID); err != nil {
			return err
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='pausing',lease_version=lease_version+1,
lease_owner='',lease_expires_at=NULL,waiting_reason='',finished_at=NULL,event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status=$3 AND desired_revision=$4 RETURNING event_seq`,
			candidate.TaskID, claim.Message.OwnerID, status, desired).Scan(&sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task changed before revision stop")
		}
		if err != nil {
			return fmt.Errorf("fence task revision execution: %w", err)
		}
		if err := interruptAssistantMessageSnapshot(ctx, tx, candidate.TaskID, "", true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET cancel_requested=true,updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, candidate.TaskID); err != nil {
			return fmt.Errorf("fence connector calls during task revision: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_stop_requests(id,task_id,action,status,safe_after)
VALUES ($1,$2,'pause','pending',$3)`, stopID, candidate.TaskID, safeAfter); err != nil {
			return fmt.Errorf("persist task revision stop: %w", err)
		}
		if err := insertTaskStatusEvent(ctx, tx, candidate.TaskID, sequence, task.StatusPausing, task.ActionPause, stopID); err != nil {
			return err
		}
		if err := requestActiveTaskAgentChildCancellations(ctx, tx, candidate.TaskID, claim.Message.OwnerID); err != nil {
			return err
		}
		if err := settleStoppedCurrentTaskAgentChildrenIfProven(ctx, tx, candidate.TaskID, claim.Message.OwnerID); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `UPDATE tasks SET desired_revision=desired_revision+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 AND desired_revision=$5`,
		candidate.TaskID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID, desired)
	if err != nil {
		return fmt.Errorf("advance desired task revision: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "task revision changed before modification")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state,stop_request_id,
interrupted_lease_owner,interrupted_lease_version,interrupted_agent_instance_id,interrupted_agent_generation)
VALUES ($1,$2,$3,$4,$5,$6,$7,'stopping',NULLIF($8,''),$9,$10,$11,$12)`, claim.Message.ID, candidate.TaskID, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID, claim.Message.Sequence, desired, stopID, interruptedOwner, interruptedVersion, interruptedAgent, interruptedGeneration); err != nil {
		return fmt.Errorf("persist task revision intent: %w", err)
	}
	return nil
}

// ApplyNextConversationTaskRevision resumes only after the ordinary stop
// worker has proved the old process and its operations stopped. Pending
// revision messages remain visible as waiting until their new checkpoint is
// durably acknowledged.
func (s *Store) ApplyNextConversationTaskRevision(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, domain.InvalidArgument("context", "revision context is required")
	}
	var applied bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskID, ownerID, initiatorID, conversationID, projectID, instanceID string
		err := tx.QueryRow(ctx, `SELECT i.task_id,i.owner_id,i.initiator_id,i.conversation_id,COALESCE(t.project_id,''),r.service_instance_id::text
FROM task_revision_intents i JOIN tasks t ON t.id=i.task_id AND t.owner_id=i.owner_id AND t.initiator_id=i.initiator_id AND t.conversation_id=i.conversation_id
JOIN conversation_message_routes r ON r.message_id=i.message_id
JOIN service_identity service ON service.singleton=true AND service.instance_id=r.service_instance_id
WHERE i.state='stopping' AND i.available_after<=clock_timestamp() AND t.status='paused'
ORDER BY i.created_at,i.message_sequence LIMIT 1`).Scan(&taskID, &ownerID, &initiatorID, &conversationID, &projectID, &instanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find stopped task revision: %w", err)
		}
		deferRetry := func() error {
			_, err := tx.Exec(ctx, `UPDATE task_revision_intents SET available_after=clock_timestamp()+interval '5 seconds',updated_at=clock_timestamp()
WHERE task_id=$1 AND state='stopping'`, taskID)
			return err
		}
		receipt := app.ConversationMessageReceipt{ServiceInstanceID: instanceID,
			Message: conversation.Message{OwnerID: ownerID, ActorID: initiatorID, ConversationID: conversationID},
			Input:   app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeUnauthenticated {
				return deferRetry()
			}
			return err
		}
		var status, waitingReason string
		var current, desired, leaseVersion int64
		err = tx.QueryRow(ctx, `SELECT status,waiting_reason,current_revision,desired_revision,lease_version FROM tasks
WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 AND project_id IS NOT DISTINCT FROM NULLIF($5,'') FOR UPDATE`,
			taskID, ownerID, initiatorID, conversationID, projectID).Scan(&status, &waitingReason, &current, &desired, &leaseVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return deferRetry()
		}
		if err != nil {
			return fmt.Errorf("lock stopped task revision: %w", err)
		}
		if status != string(task.StatusPaused) || waitingReason != "" || current >= desired || leaseVersion == math.MaxInt64 {
			return deferRetry()
		}
		var unsafe bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_instances WHERE task_id=$1 AND state<>'stopped')
 OR EXISTS(SELECT 1 FROM tasks t LEFT JOIN task_agent_instances a ON a.task_id=t.id
   AND a.agent_generation=t.agent_generation AND a.agent_instance_id=t.agent_instance_id
   WHERE t.id=$1 AND t.agent_instance_id<>'' AND (a.task_id IS NULL OR a.state<>'stopped'))
 OR EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS(SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown'))
 OR EXISTS(SELECT 1 FROM model_call_ledger WHERE task_id=$1 AND state IN ('dispatched','unknown'))
 OR EXISTS(SELECT 1 FROM task_stop_requests WHERE task_id=$1 AND status='pending')`, taskID).Scan(&unsafe)
		if err != nil {
			return fmt.Errorf("verify task revision stop: %w", err)
		}
		if unsafe {
			return deferRetry()
		}
		stored, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: taskID, OwnerID: ownerID})
		if err != nil {
			return fmt.Errorf("load stopped task authorization: %w", err)
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, stored)
		if err != nil {
			return err
		}
		if !authorized {
			return deferRetry()
		}
		fileIntent := task.FileDeliveryIntent(stored.FileDeliveryIntent)
		if !fileIntent.Valid() {
			return domain.NewError(domain.CodeInvalidState, "task file delivery intent is invalid")
		}
		target, _, err := loadDeferredTaskTargetInTx(ctx, tx, rowTaskDomain(stored))
		if err != nil {
			return err
		}
		workspaceSelected := stored.WorkspaceID.Valid || target.target.WorkspaceID != ""
		hostSelected := target.target.HostID
		type revision struct {
			claim    app.ConversationRouteClaim
			expected int64
		}
		rows, err := tx.Query(ctx, `SELECT i.message_id,i.message_sequence,i.expected_revision,m.content,r.request_metadata,r.authorization_snapshot,r.state
FROM task_revision_intents i JOIN conversation_message_routes r ON r.message_id=i.message_id
JOIN conversation_messages m ON m.id=i.message_id AND m.owner_id=i.owner_id AND m.actor_id=i.initiator_id AND m.conversation_id=i.conversation_id
WHERE i.task_id=$1 AND i.owner_id=$2 AND i.initiator_id=$3 AND i.conversation_id=$4 AND i.state='stopping'
 AND r.service_instance_id=$5::uuid AND r.owner_id=i.owner_id AND r.actor_id=i.initiator_id AND r.conversation_id=i.conversation_id
 AND m.role='user' AND m.task_id IS NULL
ORDER BY i.expected_revision FOR UPDATE OF i,r,m`, taskID, ownerID, initiatorID, conversationID, instanceID)
		if err != nil {
			return fmt.Errorf("lock pending task revisions: %w", err)
		}
		var revisions []revision
		for rows.Next() {
			var item revision
			var metadata, authorization []byte
			var routeState string
			item.claim.ServiceInstanceID = instanceID
			item.claim.Message.OwnerID, item.claim.Message.ActorID, item.claim.Message.ConversationID = ownerID, initiatorID, conversationID
			if err := rows.Scan(&item.claim.Message.ID, &item.claim.Message.Sequence, &item.expected, &item.claim.Message.Content,
				&metadata, &authorization, &routeState); err != nil {
				rows.Close()
				return fmt.Errorf("scan pending task revision: %w", err)
			}
			if json.Unmarshal(metadata, &item.claim.Input) != nil || json.Unmarshal(authorization, &item.claim.Authorization) != nil ||
				item.claim.Input.ProjectID != projectID || routeState != "waiting_adjustment" || item.expected != current+int64(len(revisions)) {
				rows.Close()
				return deferRetry()
			}
			revisions = append(revisions, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read pending task revisions: %w", err)
		}
		rows.Close()
		if len(revisions) == 0 || len(revisions) > 127 || desired-current != int64(len(revisions)) {
			return deferRetry()
		}
		var pendingCount int
		var unrelatedPending bool
		if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(bool_or(NOT ((
 NOT EXISTS(SELECT 1 FROM task_revision_intents prior WHERE prior.message_id=m.id AND prior.task_id=m.task_id)
 AND EXISTS(SELECT 1 FROM conversation_message_routes r WHERE r.message_id=m.id AND r.state='routed')) OR
 EXISTS(SELECT 1 FROM task_revision_intents prior WHERE prior.message_id=m.id AND prior.task_id=m.task_id AND prior.state='applied'))),false)
FROM conversation_messages m WHERE m.task_id=$1 AND m.owner_id=$2 AND m.role='user' AND m.delivery_status='pending'`, taskID, ownerID).
			Scan(&pendingCount, &unrelatedPending); err != nil {
			return fmt.Errorf("verify pending task messages before revision: %w", err)
		}
		if unrelatedPending || pendingCount+len(revisions) > 128 {
			return deferRetry()
		}
		for _, revision := range revisions {
			ok, err := revisionAuthorizationCurrent(ctx, tx, revision.claim)
			if err != nil {
				return err
			}
			if !ok {
				return deferRetry()
			}
			if err := validateInitialRouteAttachments(ctx, tx, revision.claim); err != nil {
				if domain.CodeOf(err) == domain.CodeInvalidState {
					return deferRetry()
				}
				return err
			}
			if revision.claim.Input.HostID != "" && revision.claim.Input.HostID != hostSelected {
				hostSelected = revision.claim.Input.HostID
				workspaceSelected = stored.WorkspaceID.Valid
			}
			if revision.claim.Input.WorkspaceID != "" {
				workspaceSelected = true
			}
			if directive, specified := app.RoutedFileDeliveryDirective(revision.claim.Message.Content, workspaceSelected); specified {
				fileIntent = directive
			}
		}
		for _, revision := range revisions {
			result, err := tx.Exec(ctx, `UPDATE conversation_messages SET task_id=$2,delivery_status='pending'
WHERE id=$1 AND task_id IS NULL AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5`,
				revision.claim.Message.ID, taskID, ownerID, initiatorID, conversationID)
			if err != nil || result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "revision message changed before delivery")
			}
			if _, err := tx.Exec(ctx, `UPDATE task_revision_intents SET state='applied',updated_at=clock_timestamp() WHERE message_id=$1 AND state='stopping'`, revision.claim.Message.ID); err != nil {
				return fmt.Errorf("apply task revision intent: %w", err)
			}
		}
		var sequence int64
		err = tx.QueryRow(ctx, `UPDATE tasks SET status='queued',current_revision=desired_revision,lease_version=lease_version+1,file_delivery_intent=$5,
lease_owner='',lease_expires_at=NULL,waiting_reason='',event_seq=event_seq+1,updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND status='paused' AND current_revision=$3 AND desired_revision=$4 RETURNING event_seq`,
			taskID, ownerID, current, desired, string(fileIntent)).Scan(&sequence)
		if err != nil {
			return fmt.Errorf("queue revised task: %w", err)
		}
		payload, err := json.Marshal(map[string]any{"status": "queued", "reason": "revision_applied", "revision": desired})
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.status", Payload: payload}); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func revisionAuthorizationCurrent(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (bool, error) {
	auth := claim.Authorization
	if (auth.Kind != string(identity.KindSession) && auth.Kind != string(identity.KindClient)) ||
		!domain.ValidTokenID(auth.TokenID) || auth.Version < 1 || auth.ExpiresAt == nil || !auth.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	previous := identity.Principal{Kind: identity.Kind(auth.Kind)}
	for _, scope := range auth.Scopes {
		previous.Scopes = append(previous.Scopes, identity.Scope(scope))
	}
	if !previous.HasScope(identity.ScopeTaskSubmit) || !previous.HasScope(identity.ScopeTaskControl) ||
		(auth.Kind == string(identity.KindClient) && !domain.ValidClientInstallationID(auth.ClientInstallationID)) {
		return false, nil
	}
	var active bool
	var scopes []string
	err := tx.QueryRow(ctx, `SELECT u.status='approved' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
 AND t.kind=$3 AND COALESCE(t.client_installation_id,'')=$4,t.scopes
FROM users u JOIN tokens t ON t.owner_id=u.id WHERE u.id=$1 AND t.id=$2 FOR SHARE OF u,t`,
		claim.Message.ActorID, auth.TokenID, auth.Kind, auth.ClientInstallationID).Scan(&active, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check revision credential: %w", err)
	}
	if !active {
		return false, nil
	}
	current := identity.Principal{Kind: identity.Kind(auth.Kind)}
	for _, scope := range scopes {
		current.Scopes = append(current.Scopes, identity.Scope(scope))
	}
	if !current.HasScope(identity.ScopeTaskSubmit) || !current.HasScope(identity.ScopeTaskControl) {
		return false, nil
	}
	if auth.ClientInstallationID != "" {
		if err := checkModelInstallation(ctx, tx, auth.ClientInstallationID, claim.Message.ActorID); err != nil {
			if domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}
