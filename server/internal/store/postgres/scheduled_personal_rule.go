package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
)

func resolveSchedulePersonalRuleBindingInTx(ctx context.Context, tx pgx.Tx, ownerID string,
	selection *schedule.PersonalRuleBinding) (*schedule.PersonalRuleBinding, error) {
	if selection == nil {
		return nil, nil
	}
	if personal.ValidateObjectIdentifier(personal.ObjectTypeRuleCandidate, selection.CandidateID) != nil ||
		selection.CandidateRevision < 1 {
		return nil, domain.InvalidArgument("personal_rule", "personal rule selection is invalid")
	}
	instanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	var candidateRevision int64
	var candidateBody []byte
	err = tx.QueryRow(ctx, `SELECT v.revision,p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id=$4
  AND NOT v.is_deleted AND p.body IS NOT NULL
FOR SHARE OF v,p`, instanceID, ownerID, personal.ObjectTypeRuleCandidate, selection.CandidateID).
		Scan(&candidateRevision, &candidateBody)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && candidateRevision != selection.CandidateRevision {
		return nil, domain.NewError(domain.CodeConflict, "the selected personal rule changed or is no longer available")
	}
	if err != nil {
		return nil, fmt.Errorf("read selected scheduled personal rule: %w", err)
	}
	if personal.ValidateObjectBody(personal.ObjectTypeRuleCandidate, selection.CandidateID, candidateBody) != nil {
		return nil, domain.NewError(domain.CodeInternal, "stored scheduled personal rule is invalid")
	}
	var candidate personal.RuleCandidateBody
	if err := json.Unmarshal(candidateBody, &candidate); err != nil || candidate.Status != personal.RuleCandidateActive {
		return nil, domain.NewError(domain.CodeConflict, "the selected personal rule is no longer active")
	}
	var sourceRevision int64
	var sourceDeleted bool
	var sourceBody []byte
	err = tx.QueryRow(ctx, `SELECT v.revision,v.is_deleted,p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id=$4
FOR SHARE OF v,p`, instanceID, ownerID, personal.ObjectTypeMemory, candidate.SourceMemoryID).
		Scan(&sourceRevision, &sourceDeleted, &sourceBody)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (sourceDeleted || sourceRevision != candidate.SourceMemoryRevision) {
		return nil, domain.NewError(domain.CodeConflict, "the selected rule source memory changed or is no longer available")
	}
	if err != nil {
		return nil, fmt.Errorf("read scheduled personal rule source memory: %w", err)
	}
	if personal.ValidateObjectBody(personal.ObjectTypeMemory, candidate.SourceMemoryID, sourceBody) != nil {
		return nil, domain.NewError(domain.CodeInternal, "stored scheduled personal rule source memory is invalid")
	}
	if err := ensureSchedulePersonalMemoryEnabledInTx(ctx, tx, instanceID, ownerID); err != nil {
		return nil, err
	}
	return &schedule.PersonalRuleBinding{
		CandidateID: selection.CandidateID, CandidateRevision: selection.CandidateRevision,
		SourceMemoryID: candidate.SourceMemoryID, SourceMemoryRevision: candidate.SourceMemoryRevision,
	}, nil
}

func marshalPersonalRuleBinding(value *schedule.PersonalRuleBinding) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode schedule personal rule binding: %w", err)
	}
	return encoded, nil
}

func lockSchedulePersonalRuleOwner(ctx context.Context, tx pgx.Tx, ownerID string) error {
	if !domain.ValidUserID(ownerID) {
		return domain.InvalidArgument("owner_id", "personal rule owner is invalid")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1893099217))`, ownerID); err != nil {
		return fmt.Errorf("serialize scheduled personal rule changes: %w", err)
	}
	return nil
}

func ensureSchedulePersonalMemoryEnabledInTx(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) error {
	var body []byte
	err := tx.QueryRow(ctx, `SELECT p.body
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id='builtin_memory'
  AND NOT v.is_deleted AND p.body IS NOT NULL
FOR SHARE OF v,p`, instanceID, ownerID, personal.ObjectTypeSkillSetting).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // The existing account default is enabled unless explicitly disabled.
	}
	if err != nil {
		return fmt.Errorf("read scheduled personal memory setting: %w", err)
	}
	if personal.ValidateObjectBody(personal.ObjectTypeSkillSetting, "builtin_memory", body) != nil {
		return domain.NewError(domain.CodeInternal, "stored scheduled personal memory setting is invalid")
	}
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &setting); err != nil {
		return domain.NewError(domain.CodeInternal, "stored scheduled personal memory setting is invalid")
	}
	if !setting.Enabled {
		return domain.NewError(domain.CodeConflict, "personal memory is disabled for this account")
	}
	return nil
}

func schedulePersonalRuleBindingValidInTx(ctx context.Context, tx pgx.Tx, ownerID string,
	binding *schedule.PersonalRuleBinding) (bool, error) {
	if binding == nil {
		return true, nil
	}
	resolved, err := resolveSchedulePersonalRuleBindingInTx(ctx, tx, ownerID, binding)
	if err != nil {
		switch domain.CodeOf(err) {
		case domain.CodeConflict, domain.CodeForbidden, domain.CodeNotFound:
			return false, nil
		default:
			return false, err
		}
	}
	return resolved != nil && *resolved == *binding, nil
}

type revokedSchedule struct {
	id                     string
	status                 schedule.Status
	version                int64
	currentBindingAffected bool
}

// lockSchedulesForPersonalRuleRevocation follows the schedule-before-account
// data lock order used by schedule edits and due-run commits.
func lockSchedulesForPersonalRuleRevocation(ctx context.Context, tx pgx.Tx, ownerID string,
	mutation personal.Mutation) ([]revokedSchedule, error) {
	candidateID, memoryID, all := personalRuleRevocationTarget(mutation)
	if candidateID == "" && memoryID == "" && !all {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT s.id,s.status,s.version,
(s.personal_rule_binding IS NOT NULL AND (($2::text <> '' AND s.personal_rule_binding->>'candidate_id'=$2)
    OR ($3::text <> '' AND s.personal_rule_binding->>'source_memory_id'=$3) OR $4::boolean))
FROM schedules s
WHERE s.owner_id=$1 AND (
  (s.personal_rule_binding IS NOT NULL AND (($2::text <> '' AND s.personal_rule_binding->>'candidate_id'=$2)
    OR ($3::text <> '' AND s.personal_rule_binding->>'source_memory_id'=$3) OR $4::boolean))
  OR EXISTS (SELECT 1 FROM schedule_runs r JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id
    WHERE r.schedule_id=s.id AND r.owner_id=s.owner_id
      AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
      AND r.personal_rule_binding IS NOT NULL
      AND (($2::text <> '' AND r.personal_rule_binding->>'candidate_id'=$2)
        OR ($3::text <> '' AND r.personal_rule_binding->>'source_memory_id'=$3) OR $4::boolean))
)
ORDER BY s.id FOR UPDATE OF s`, ownerID, candidateID, memoryID, all)
	if err != nil {
		return nil, fmt.Errorf("lock schedules affected by personal rule revocation: %w", err)
	}
	defer rows.Close()
	values := make([]revokedSchedule, 0)
	for rows.Next() {
		var value revokedSchedule
		var status string
		if err := rows.Scan(&value.id, &status, &value.version, &value.currentBindingAffected); err != nil {
			return nil, fmt.Errorf("read schedule affected by personal rule revocation: %w", err)
		}
		value.status = schedule.Status(status)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedules affected by personal rule revocation: %w", err)
	}
	return values, nil
}

func personalRuleRevocationTarget(mutation personal.Mutation) (candidateID, memoryID string, all bool) {
	switch mutation.ObjectType {
	case personal.ObjectTypeRuleCandidate:
		if mutation.Delete {
			candidateID = mutation.ObjectID
		}
	case personal.ObjectTypeMemory:
		memoryID = mutation.ObjectID
	case personal.ObjectTypeSkillSetting:
		if mutation.ObjectID == "builtin_memory" {
			all = mutation.Delete
			if !mutation.Delete {
				var setting struct {
					Enabled bool `json:"enabled"`
				}
				all = json.Unmarshal(mutation.Body, &setting) == nil && !setting.Enabled
			}
		}
	}
	return candidateID, memoryID, all
}

func revokePersonalRuleSchedulesInTx(ctx context.Context, tx pgx.Tx, ownerID string, values []revokedSchedule,
	mutation personal.Mutation) error {
	if len(values) == 0 {
		return nil
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.id)
	}
	candidateID, memoryID, all := personalRuleRevocationTarget(mutation)
	rows, err := tx.Query(ctx, `SELECT t.id FROM schedule_runs r
JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id
WHERE r.owner_id=$1 AND r.schedule_id=ANY($2::text[])
  AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
  AND r.personal_rule_binding IS NOT NULL
  AND (($3::text <> '' AND r.personal_rule_binding->>'candidate_id'=$3)
    OR ($4::text <> '' AND r.personal_rule_binding->>'source_memory_id'=$4) OR $5::boolean)
ORDER BY t.id FOR UPDATE OF t`, ownerID, ids, candidateID, memoryID, all)
	if err != nil {
		return fmt.Errorf("lock Tasks affected by personal rule revocation: %w", err)
	}
	taskIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read Task affected by personal rule revocation: %w", err)
		}
		taskIDs = append(taskIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Tasks affected by personal rule revocation: %w", err)
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `UPDATE schedule_runs r SET personal_rule_validation_status='revoked',
personal_rule_revoked_at=clock_timestamp(),updated_at=clock_timestamp()
FROM tasks t
WHERE t.id=r.task_id AND t.owner_id=r.owner_id AND r.owner_id=$1 AND r.schedule_id=ANY($2::text[])
  AND t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
  AND r.personal_rule_binding IS NOT NULL AND r.personal_rule_validation_status <> 'revoked'
  AND (($3::text <> '' AND r.personal_rule_binding->>'candidate_id'=$3)
    OR ($4::text <> '' AND r.personal_rule_binding->>'source_memory_id'=$4) OR $5::boolean)`,
		ownerID, ids, candidateID, memoryID, all); err != nil {
		return fmt.Errorf("record scheduled personal rule revocation for active runs: %w", err)
	}
	for _, id := range taskIDs {
		if err := requestTaskCancellationForAuthorization(ctx, tx, id, ownerID); err != nil {
			return fmt.Errorf("request stop for Task affected by personal rule revocation: %w", err)
		}
	}
	for _, value := range values {
		if !value.currentBindingAffected || value.status != schedule.StatusEnabled {
			continue
		}
		var nextVersion int64
		err := tx.QueryRow(ctx, `UPDATE schedules SET status='disabled',version=version+1,next_occurrence_key='',
next_run_at=NULL,next_check_at=NULL,updated_at=clock_timestamp()
WHERE owner_id=$1 AND id=$2 AND status='enabled' AND version=$3 RETURNING version`, ownerID, value.id, value.version).Scan(&nextVersion)
		if err != nil {
			return fmt.Errorf("disable schedule with revoked personal rule: %w", err)
		}
		if err := recordScheduleDefinitionChange(ctx, tx, ownerID, value.id, nextVersion); err != nil {
			return err
		}
	}
	return nil
}
