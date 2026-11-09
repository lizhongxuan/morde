package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"

	"github.com/jackc/pgx/v5"
)

var goalTriggerStepIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

func (s *Store) ClaimGoalAgentTriggerEvents(ctx context.Context, workerID string, lease time.Duration, limit int32) ([]app.GoalAgentTriggerEvent, error) {
	if ctx == nil || workerID == "" || len(workerID) > 128 || lease < time.Minute || lease > 5*time.Minute || lease%time.Second != 0 || limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("goal_trigger_claim", "Goal trigger claim is invalid")
	}
	rows, err := s.pool.Query(ctx, `WITH claimable AS (
  SELECT e.id FROM goal_agent_trigger_events e
  JOIN owner_execution_controls gate ON gate.owner_id=e.owner_id AND gate.state='enabled'
  WHERE e.service_instance_id=(SELECT instance_id FROM service_identity WHERE singleton=true)
    AND (e.status='queued' OR (e.status='processing' AND e.lease_until<=clock_timestamp()))
  ORDER BY e.id FOR UPDATE OF e SKIP LOCKED LIMIT $3
)
UPDATE goal_agent_trigger_events e
SET status='processing',lease_owner=$1,lease_until=clock_timestamp()+($2::bigint*interval '1 second'),
    attempt_count=e.attempt_count+1
FROM claimable c WHERE e.id=c.id
RETURNING e.id,e.owner_id,e.goal_id,e.goal_revision,e.step_id,e.authorization_snapshot::text,e.model_connection_id,e.lease_owner`,
		workerID, int64(lease/time.Second), limit)
	if err != nil {
		return nil, fmt.Errorf("claim Goal agent trigger events: %w", err)
	}
	defer rows.Close()
	result := make([]app.GoalAgentTriggerEvent, 0)
	for rows.Next() {
		var value app.GoalAgentTriggerEvent
		var authorization string
		if err := rows.Scan(&value.ID, &value.OwnerID, &value.GoalID, &value.GoalRevision, &value.StepID,
			&authorization, &value.ModelID, &value.LeaseOwner); err != nil {
			return nil, fmt.Errorf("read claimed Goal agent trigger: %w", err)
		}
		decoder := json.NewDecoder(strings.NewReader(authorization))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value.Authorization); err != nil || !validGoalAgentTriggerEventIdentity(value) {
			return nil, domain.NewError(domain.CodeInternal, "stored Goal agent trigger is invalid")
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return nil, domain.NewError(domain.CodeInternal, "stored Goal agent trigger authorization has trailing data")
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed Goal agent triggers: %w", err)
	}
	return result, nil
}

func (s *Store) LoadGoalAgentTriggerEvent(ctx context.Context, event app.GoalAgentTriggerEvent) (app.GoalAgentTriggerEvent, bool, error) {
	if !validGoalAgentTriggerEventIdentity(event) {
		return app.GoalAgentTriggerEvent{}, false, domain.InvalidArgument("event", "Goal agent trigger identity is invalid")
	}
	var loaded app.GoalAgentTriggerEvent
	ready := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var setting []byte
		err := tx.QueryRow(ctx, `SELECT p.body FROM personal_objects p JOIN account_object_versions v
  ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id
 AND v.object_type=p.object_type AND v.object_id=p.object_id AND NOT v.is_deleted
WHERE p.service_instance_id=(SELECT instance_id FROM service_identity WHERE singleton=true)
  AND p.owner_id=$1 AND p.object_type='skill_setting' AND p.object_id='builtin_goal_followups' AND p.body IS NOT NULL
FOR SHARE OF p,v`, event.OwnerID).Scan(&setting)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Goal follow-up setting: %w", err)
		}
		var preference struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(setting, &preference) != nil || !preference.Enabled {
			return nil
		}
		var body []byte
		var currentRevision int64
		err = tx.QueryRow(ctx, `SELECT p.body,v.revision
FROM personal_objects p JOIN account_object_versions v
  ON v.service_instance_id=p.service_instance_id AND v.owner_id=p.owner_id AND v.object_type=p.object_type AND v.object_id=p.object_id
WHERE p.service_instance_id=(SELECT instance_id FROM service_identity WHERE singleton=true)
  AND p.owner_id=$1 AND p.object_type='goal' AND p.object_id=$2 AND p.body IS NOT NULL AND NOT v.is_deleted
FOR SHARE OF p,v`, event.OwnerID, event.GoalID).Scan(&body, &currentRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Goal for automatic follow-up: %w", err)
		}
		if currentRevision != event.GoalRevision {
			return nil
		}
		var goal struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Steps       []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
				Note   string `json:"note"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(body, &goal); err != nil {
			return domain.NewError(domain.CodeInternal, "stored Goal for automatic follow-up is invalid")
		}
		for _, step := range goal.Steps {
			if step.ID == event.StepID && step.Status == "blocked" {
				loaded = event
				loaded.GoalTitle, loaded.GoalSummary, loaded.StepTitle, loaded.StepNote = goal.Title, goal.Description, step.Title, step.Note
				ready = true
				break
			}
		}
		return nil
	})
	return loaded, ready, err
}

func (s *Store) SetGoalAgentTriggerModel(ctx context.Context, event app.GoalAgentTriggerEvent, modelID string) (string, error) {
	if !validGoalAgentTriggerEventIdentity(event) || !domain.ValidModelConnectionID(modelID) {
		return "", domain.InvalidArgument("model_id", "Goal trigger model identity is invalid")
	}
	var selected string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentStatus, leaseOwner string
		var leaseActive bool
		if err := tx.QueryRow(ctx, `SELECT status,COALESCE(lease_owner,''),model_connection_id,lease_until>clock_timestamp()
FROM goal_agent_trigger_events WHERE id=$1 FOR UPDATE`,
			event.ID).Scan(&currentStatus, &leaseOwner, &selected, &leaseActive); err != nil {
			return err
		}
		if currentStatus != "processing" || leaseOwner != event.LeaseOwner || !leaseActive {
			return domain.NewError(domain.CodeConflict, "Goal trigger lease changed")
		}
		if selected == "" {
			if _, err := tx.Exec(ctx, `UPDATE goal_agent_trigger_events SET model_connection_id=$2 WHERE id=$1`, event.ID, modelID); err != nil {
				return fmt.Errorf("persist Goal trigger model: %w", err)
			}
			selected = modelID
		}
		return nil
	})
	return selected, err
}

func (s *Store) CompleteGoalAgentTriggerEvent(ctx context.Context, event app.GoalAgentTriggerEvent, scheduleID string) (bool, error) {
	return s.finishGoalAgentTriggerEvent(ctx, event, "completed", scheduleID, "")
}

func (s *Store) SkipGoalAgentTriggerEvent(ctx context.Context, event app.GoalAgentTriggerEvent, reason string) (bool, error) {
	if reason == "" || len(reason) > 64 {
		return false, domain.InvalidArgument("reason", "Goal trigger skip reason is invalid")
	}
	return s.finishGoalAgentTriggerEvent(ctx, event, "skipped", "", reason)
}

func (s *Store) finishGoalAgentTriggerEvent(ctx context.Context, event app.GoalAgentTriggerEvent, status, scheduleID, reason string) (bool, error) {
	if !validGoalAgentTriggerEventIdentity(event) || (status == "completed" && !schedule.ValidID(scheduleID)) {
		return false, domain.InvalidArgument("event", "Goal trigger completion is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE goal_agent_trigger_events
SET status=$3,lease_owner=NULL,lease_until=NULL,schedule_id=NULLIF($4,''),skip_reason=NULLIF($5,''),processed_at=clock_timestamp()
WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until>clock_timestamp()`, event.ID, event.LeaseOwner, status, scheduleID, reason)
	if err != nil {
		return false, fmt.Errorf("finish Goal trigger outbox event: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) RetryGoalAgentTriggerEvent(ctx context.Context, event app.GoalAgentTriggerEvent) (bool, error) {
	if !validGoalAgentTriggerEventIdentity(event) {
		return false, domain.InvalidArgument("event", "Goal trigger identity is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE goal_agent_trigger_events
SET status=CASE WHEN attempt_count>=8 THEN 'skipped' ELSE 'queued' END,
    lease_owner=NULL,lease_until=NULL,skip_reason=CASE WHEN attempt_count>=8 THEN 'retry_limit' ELSE NULL END,
    processed_at=CASE WHEN attempt_count>=8 THEN clock_timestamp() ELSE NULL END
WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until>clock_timestamp()`, event.ID, event.LeaseOwner)
	if err != nil {
		return false, fmt.Errorf("release Goal trigger outbox event: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) ResolveGoalAgentTriggerModel(ctx context.Context, ownerID string) (string, error) {
	if !domain.ValidUserID(ownerID) {
		return "", domain.InvalidArgument("owner_id", "Goal trigger owner is invalid")
	}
	var modelID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		modelID, err = resolveRouteModelInTx(ctx, tx, app.ConversationRouteClaim{
			Message: conversation.Message{ActorID: ownerID},
		})
		return err
	})
	return modelID, err
}

func validGoalAgentTriggerEventIdentity(event app.GoalAgentTriggerEvent) bool {
	return event.ID > 0 && domain.ValidUserID(event.OwnerID) && personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, event.GoalID) == nil &&
		event.GoalRevision > 0 && goalTriggerStepIDPattern.MatchString(event.StepID) &&
		event.Authorization.Kind == string(identity.KindSession) && domain.ValidTokenID(event.Authorization.TokenID) &&
		event.Authorization.ClientInstallationID == "" && len(event.Authorization.Scopes) == 0 &&
		event.LeaseOwner != "" && len(event.LeaseOwner) <= 128
}

var _ app.GoalAgentTriggerStore = (*Store)(nil)
var _ app.GoalAgentTriggerModelResolver = (*Store)(nil)
