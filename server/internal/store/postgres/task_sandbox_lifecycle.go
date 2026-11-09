package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskSandboxLifecycleScanner interface {
	Scan(...any) error
}

const taskSandboxLifecycleSelect = `
SELECT id,task_id,owner_id,initiator_id,lease_owner,lease_version,authorization_version,
 agent_instance_id,agent_generation,guardian_nonce_sha256,sandbox_id,sandbox_generation,status,expires_at,
 unknown_code,termination_evidence,terminated_at,created_at,updated_at
FROM task_sandbox_lifecycles`

func (s *Store) PrepareTaskSandboxLifecycle(ctx context.Context, value app.TaskSandboxLifecycle) (app.TaskSandboxLifecycle, bool, error) {
	if !validTaskSandboxLifecycleFence(value) || value.Status != app.TaskSandboxPrepared || value.SandboxID != "" ||
		!domain.ValidOperationID(value.ID) || value.NonceSHA256 != "" || value.SandboxGeneration != 0 || value.ExpiresAt != nil || value.UnknownCode != "" ||
		len(value.TerminationEvidence) != 0 || value.TerminatedAt != nil {
		return app.TaskSandboxLifecycle{}, false, domain.InvalidArgument("task_sandbox", "Task Sandbox lifecycle fields are invalid")
	}
	var result app.TaskSandboxLifecycle
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxLifecycleFence(ctx, tx, value, task.StatusRunning); err != nil {
			return err
		}
		stored, err := getActiveTaskSandboxLifecycleForUpdate(ctx, tx, value.TaskID)
		if err == nil {
			if stored.Status != app.TaskSandboxPrepared {
				return domain.NewError(domain.CodeConflict, "Task Sandbox lifecycle has crossed a provider side-effect boundary")
			}
			if stored.ID == value.ID && sameTaskSandboxLifecycleFenceExceptGeneration(stored, value) &&
				stored.TaskID == value.TaskID && stored.OwnerID == value.OwnerID && stored.InitiatorID == value.InitiatorID {
				if stored.NonceSHA256 != "" {
					return domain.NewError(domain.CodeConflict, "prepared Task Sandbox lifecycle unexpectedly has a Guardian stop challenge")
				}
				result, replayed = stored, true
				return nil
			}
			// A prepared lifecycle has not called the provider yet. Only the
			// current Task lease and Agent generation may adopt that reservation.
			if stored.LeaseOwner == value.LeaseOwner && stored.LeaseVersion == value.LeaseVersion &&
				stored.AuthorizationVersion == value.AuthorizationVersion && stored.AgentInstanceID == value.AgentInstanceID &&
				stored.AgentGeneration == value.AgentGeneration {
				return domain.NewError(domain.CodeConflict, "prepared Task Sandbox lifecycle has a different operation ID")
			}
			if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles
SET id=$2,lease_owner=$3,lease_version=$4,authorization_version=$5,agent_instance_id=$6,agent_generation=$7,
 guardian_nonce_sha256='',updated_at=clock_timestamp()
WHERE id=$1 AND status='prepared'`, stored.ID, value.ID, value.LeaseOwner, value.LeaseVersion, value.AuthorizationVersion,
				value.AgentInstanceID, value.AgentGeneration); err != nil {
				return fmt.Errorf("adopt prepared Task Sandbox lifecycle: %w", err)
			}
			result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, stored.SandboxGeneration)
			if err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read active Task Sandbox lifecycle: %w", err)
		}
		var generation int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(sandbox_generation),0)+1 FROM task_sandbox_lifecycles WHERE task_id=$1`, value.TaskID).Scan(&generation); err != nil {
			return fmt.Errorf("allocate Task Sandbox generation: %w", err)
		}
		if generation < 1 {
			return domain.NewError(domain.CodeInvalidState, "Task Sandbox generation is exhausted")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_sandbox_lifecycles
(id,task_id,owner_id,initiator_id,lease_owner,lease_version,authorization_version,agent_instance_id,agent_generation,
 guardian_nonce_sha256,sandbox_generation,status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'prepared')`, value.ID, value.TaskID, value.OwnerID, value.InitiatorID,
			value.LeaseOwner, value.LeaseVersion, value.AuthorizationVersion, value.AgentInstanceID, value.AgentGeneration, value.NonceSHA256, generation); err != nil {
			return fmt.Errorf("prepare Task Sandbox lifecycle: %w", err)
		}
		result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, generation)
		return err
	})
	return result, replayed, err
}

func (s *Store) MarkTaskSandboxLifecycleCreating(ctx context.Context, value app.TaskSandboxLifecycle) (app.TaskSandboxLifecycle, error) {
	return s.transitionTaskSandboxLifecycle(ctx, value, []task.Status{task.StatusRunning}, []app.TaskSandboxLifecycleStatus{app.TaskSandboxPrepared}, app.TaskSandboxCreating, "", nil)
}

func (s *Store) BindTaskSandboxLifecycle(ctx context.Context, value app.TaskSandboxLifecycle, sandboxID string, expiresAt time.Time) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxProviderID(sandboxID) || value.SandboxGeneration < 1 || !expiresAt.After(time.Now()) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox", "Task Sandbox binding is invalid")
	}
	return s.transitionTaskSandboxLifecycle(ctx, value, []task.Status{task.StatusRunning}, []app.TaskSandboxLifecycleStatus{app.TaskSandboxCreating}, app.TaskSandboxRunning, sandboxID, &expiresAt)
}

func (s *Store) MarkTaskSandboxLifecycleUnknown(ctx context.Context, value app.TaskSandboxLifecycle, code string) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxLifecycleFence(value) || !validTaskSandboxLifecycleUnknownCode(code) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox.unknown_code", "Task Sandbox unknown result is invalid")
	}
	return s.transitionTaskSandboxLifecycle(ctx, value, []task.Status{task.StatusRunning, task.StatusCancelling},
		[]app.TaskSandboxLifecycleStatus{app.TaskSandboxCreating, app.TaskSandboxStopping}, app.TaskSandboxUnknown, "", nil, code)
}

func (s *Store) RequestTaskSandboxStop(ctx context.Context, value app.TaskSandboxLifecycle) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxLifecycleFence(value) || !validLifecycleIdentity(value) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox", "Task Sandbox stop fence is invalid")
	}
	var result app.TaskSandboxLifecycle
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxLifecycleFence(ctx, tx, value, task.StatusRunning, task.StatusCancelling); err != nil {
			return err
		}
		stored, err := getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		if err != nil {
			return err
		}
		if !sameTaskSandboxLifecycleIdentity(stored, value) {
			return lifecycleFenceConflict()
		}
		switch stored.Status {
		case app.TaskSandboxStopping:
			result = stored
			return nil
		case app.TaskSandboxRunning:
			if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles SET status='stopping',updated_at=clock_timestamp()
WHERE id=$1 AND status='running'`, stored.ID); err != nil {
				return fmt.Errorf("request Task Sandbox stop: %w", err)
			}
			result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
			return err
		case app.TaskSandboxPrepared:
			// No provider call has started, so this can be safely closed without
			// claiming that a remote sandbox was terminated.
			if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles SET status='terminated',terminated_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND status='prepared'`, stored.ID); err != nil {
				return fmt.Errorf("close unstarted Task Sandbox lifecycle: %w", err)
			}
			result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
			return err
		default:
			return domain.NewError(domain.CodeConflict, "Task Sandbox lifecycle cannot be stopped from its current state")
		}
	})
	return result, err
}

func (s *Store) TerminateTaskSandbox(ctx context.Context, value app.TaskSandboxLifecycle, evidence []byte) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxLifecycleFence(value) || !validLifecycleIdentity(value) || len(evidence) > 4096 ||
		(len(evidence) > 0 && value.SandboxID != "" && !validTaskSandboxGuardianNonceSHA256(value.NonceSHA256)) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox", "Task Sandbox termination fence is invalid")
	}
	var result app.TaskSandboxLifecycle
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxLifecycleFence(ctx, tx, value, task.StatusRunning, task.StatusCancelling); err != nil {
			return err
		}
		stored, err := getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		if err != nil {
			return err
		}
		if !sameTaskSandboxLifecycleIdentity(stored, value) {
			return lifecycleFenceConflict()
		}
		if stored.Status == app.TaskSandboxTerminated {
			if !bytesEqual(stored.TerminationEvidence, evidence) {
				return domain.NewError(domain.CodeConflict, "Task Sandbox termination evidence differs from the recorded result")
			}
			result = stored
			return nil
		}
		if stored.Status == app.TaskSandboxPrepared {
			if len(evidence) != 0 {
				return domain.NewError(domain.CodeInvalidState, "unstarted Task Sandbox cannot have termination evidence")
			}
		} else if (stored.Status != app.TaskSandboxStopping && stored.Status != app.TaskSandboxUnknown) || len(evidence) == 0 {
			return domain.NewError(domain.CodeConflict, "Task Sandbox termination requires a stopping or reconciled lifecycle and evidence")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles
SET status='terminated',unknown_code='',termination_evidence=$2,terminated_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=$1 AND status=$3`, stored.ID, evidence, stored.Status); err != nil {
			return fmt.Errorf("record Task Sandbox termination: %w", err)
		}
		result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		return err
	})
	return result, err
}

// SetTaskSandboxGuardianStopChallenge persists only a fresh stop challenge's
// digest. It can be rotated while stopping/unknown, so a lost Guardian response
// can be retried without retaining the raw nonce across worker restarts.
func (s *Store) SetTaskSandboxGuardianStopChallenge(ctx context.Context, value app.TaskSandboxLifecycle,
	nonceSHA256 string) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxLifecycleFence(value) || !validLifecycleIdentity(value) || !validTaskSandboxGuardianNonceSHA256(nonceSHA256) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox.guardian_nonce", "Task Sandbox Guardian stop challenge is invalid")
	}
	var result app.TaskSandboxLifecycle
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxLifecycleFence(ctx, tx, value, task.StatusRunning, task.StatusCancelling); err != nil {
			return err
		}
		stored, err := getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		if err != nil {
			return err
		}
		if !sameTaskSandboxLifecycleFenceExceptGuardianNonce(stored, value) ||
			(stored.Status != app.TaskSandboxStopping && stored.Status != app.TaskSandboxUnknown) {
			return lifecycleFenceConflict()
		}
		if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles
SET guardian_nonce_sha256=$2,updated_at=clock_timestamp()
WHERE id=$1 AND status IN ('stopping','unknown')`, stored.ID, nonceSHA256); err != nil {
			return fmt.Errorf("persist Task Sandbox Guardian stop challenge: %w", err)
		}
		result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		return err
	})
	return result, err
}

func (s *Store) GetTaskSandboxLifecycle(ctx context.Context, ownerID, taskID string, generation int64) (app.TaskSandboxLifecycle, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || generation < 1 {
		return app.TaskSandboxLifecycle{}, domain.NewError(domain.CodeNotFound, "Task Sandbox lifecycle not found")
	}
	value, err := scanTaskSandboxLifecycle(s.pool.QueryRow(ctx, taskSandboxLifecycleSelect+
		` WHERE owner_id=$1 AND task_id=$2 AND sandbox_generation=$3`, ownerID, taskID, generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskSandboxLifecycle{}, domain.NewError(domain.CodeNotFound, "Task Sandbox lifecycle not found")
	}
	if err != nil {
		return app.TaskSandboxLifecycle{}, fmt.Errorf("get Task Sandbox lifecycle: %w", err)
	}
	return value, nil
}

func (s *Store) transitionTaskSandboxLifecycle(ctx context.Context, value app.TaskSandboxLifecycle, taskStatuses []task.Status,
	from []app.TaskSandboxLifecycleStatus, to app.TaskSandboxLifecycleStatus, sandboxID string, expiresAt *time.Time, unknownCode ...string) (app.TaskSandboxLifecycle, error) {
	if !validTaskSandboxLifecycleFence(value) || !validLifecycleIdentity(value) {
		return app.TaskSandboxLifecycle{}, domain.InvalidArgument("task_sandbox", "Task Sandbox lifecycle fence is invalid")
	}
	var result app.TaskSandboxLifecycle
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxLifecycleFence(ctx, tx, value, taskStatuses...); err != nil {
			return err
		}
		stored, err := getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		if err != nil {
			return err
		}
		if !sameTaskSandboxLifecycleIdentity(stored, value) {
			return lifecycleFenceConflict()
		}
		if !containsLifecycleStatus(from, stored.Status) {
			return domain.NewError(domain.CodeConflict, "Task Sandbox lifecycle state changed")
		}
		// Only the lease/Agent generation that prepared a provider operation can
		// cross the side-effect boundary or commit its returned sandbox handle.
		if to == app.TaskSandboxCreating || to == app.TaskSandboxRunning {
			if !sameTaskSandboxLifecycleFence(stored, value) {
				return lifecycleFenceConflict()
			}
		}
		code := ""
		if len(unknownCode) > 0 {
			code = unknownCode[0]
		}
		if to == app.TaskSandboxRunning && (!validTaskSandboxProviderID(sandboxID) || expiresAt == nil || !expiresAt.After(time.Now())) {
			return domain.InvalidArgument("task_sandbox.binding", "Task Sandbox binding is invalid")
		}
		if to == app.TaskSandboxRunning {
			var future bool
			if err := tx.QueryRow(ctx, `SELECT $1::timestamptz > clock_timestamp()`, *expiresAt).Scan(&future); err != nil {
				return fmt.Errorf("validate Task Sandbox expiry: %w", err)
			}
			if !future {
				return domain.InvalidArgument("task_sandbox.expires_at", "Task Sandbox binding has already expired")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE task_sandbox_lifecycles
SET status=$2,sandbox_id=CASE WHEN $3='' THEN sandbox_id ELSE $3 END,
 expires_at=CASE WHEN $4::timestamptz IS NULL THEN expires_at ELSE $4 END,
 unknown_code=$5,updated_at=clock_timestamp()
WHERE id=$1`, stored.ID, to, sandboxID, expiresAt, code); err != nil {
			return fmt.Errorf("transition Task Sandbox lifecycle: %w", err)
		}
		result, err = getTaskSandboxLifecycleForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.SandboxGeneration)
		return err
	})
	return result, err
}

func lockTaskSandboxLifecycleFence(ctx context.Context, tx pgx.Tx, value app.TaskSandboxLifecycle, allowed ...task.Status) error {
	if !validTaskSandboxLifecycleFence(value) {
		return domain.InvalidArgument("task_sandbox", "Task Sandbox execution fence is invalid")
	}
	projectID, err := lockTaskInitiatorInTx(ctx, tx, value.TaskID, value.OwnerID, value.InitiatorID)
	if err != nil {
		return err
	}
	if projectID != "" {
		return domain.NewError(domain.CodeForbidden, "Task Sandbox is currently limited to personal Hostless Tasks")
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, value.InitiatorID, nil)
	if err != nil {
		return err
	}
	var status, leaseOwner, agentInstanceID, initiatorID, projectIDValue, executionLocation string
	var leaseVersion, authorizationVersion, executionEpoch, agentGeneration int64
	var leaseValid, hostless bool
	err = tx.QueryRow(ctx, `SELECT status,lease_owner,lease_version,execution_epoch,auth_version,
COALESCE(lease_expires_at>clock_timestamp(),false),agent_instance_id,agent_generation,
execution_location,host_id IS NULL,initiator_id,COALESCE(project_id,'')
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, value.TaskID, value.OwnerID).Scan(
		&status, &leaseOwner, &leaseVersion, &executionEpoch, &authorizationVersion, &leaseValid, &agentInstanceID,
		&agentGeneration, &executionLocation, &hostless, &initiatorID, &projectIDValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "Task Sandbox Task not found")
	}
	if err != nil {
		return fmt.Errorf("lock Task Sandbox Task fence: %w", err)
	}
	if !containsTaskStatus(allowed, task.Status(status)) || leaseOwner != value.LeaseOwner || leaseVersion != value.LeaseVersion ||
		!leaseValid || executionEpoch != gate.epoch || authorizationVersion != value.AuthorizationVersion ||
		agentInstanceID != value.AgentInstanceID || agentGeneration != value.AgentGeneration || executionLocation != "server" ||
		!hostless || initiatorID != value.OwnerID || projectIDValue != "" {
		return lifecycleFenceConflict()
	}
	return nil
}

func getActiveTaskSandboxLifecycleForUpdate(ctx context.Context, tx pgx.Tx, taskID string) (app.TaskSandboxLifecycle, error) {
	return scanTaskSandboxLifecycle(tx.QueryRow(ctx, taskSandboxLifecycleSelect+
		` WHERE task_id=$1 AND status IN ('prepared','creating','running','stopping','unknown') FOR UPDATE`, taskID))
}

func getTaskSandboxLifecycleForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID string, generation int64) (app.TaskSandboxLifecycle, error) {
	value, err := scanTaskSandboxLifecycle(tx.QueryRow(ctx, taskSandboxLifecycleSelect+
		` WHERE owner_id=$1 AND task_id=$2 AND sandbox_generation=$3 FOR UPDATE`, ownerID, taskID, generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskSandboxLifecycle{}, domain.NewError(domain.CodeNotFound, "Task Sandbox lifecycle not found")
	}
	if err != nil {
		return app.TaskSandboxLifecycle{}, fmt.Errorf("read Task Sandbox lifecycle: %w", err)
	}
	return value, nil
}

func scanTaskSandboxLifecycle(row taskSandboxLifecycleScanner) (app.TaskSandboxLifecycle, error) {
	var value app.TaskSandboxLifecycle
	var expiry, terminated pgtype.Timestamptz
	var status string
	if err := row.Scan(&value.ID, &value.TaskID, &value.OwnerID, &value.InitiatorID, &value.LeaseOwner,
		&value.LeaseVersion, &value.AuthorizationVersion, &value.AgentInstanceID, &value.AgentGeneration,
		&value.NonceSHA256, &value.SandboxID, &value.SandboxGeneration, &status, &expiry, &value.UnknownCode,
		&value.TerminationEvidence, &terminated, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return app.TaskSandboxLifecycle{}, err
	}
	value.Status = app.TaskSandboxLifecycleStatus(status)
	if expiry.Valid {
		value.ExpiresAt = &expiry.Time
	}
	if terminated.Valid {
		value.TerminatedAt = &terminated.Time
	}
	return value, nil
}

func sameTaskSandboxLifecycleFence(stored, requested app.TaskSandboxLifecycle) bool {
	return sameTaskSandboxLifecycleIdentity(stored, requested) && sameTaskSandboxLifecycleFenceExceptGeneration(stored, requested)
}

func sameTaskSandboxLifecycleFenceExceptGeneration(stored, requested app.TaskSandboxLifecycle) bool {
	return stored.LeaseOwner == requested.LeaseOwner &&
		stored.LeaseVersion == requested.LeaseVersion && stored.AuthorizationVersion == requested.AuthorizationVersion &&
		stored.AgentInstanceID == requested.AgentInstanceID && stored.AgentGeneration == requested.AgentGeneration
}

func sameTaskSandboxLifecycleIdentity(stored, requested app.TaskSandboxLifecycle) bool {
	return sameTaskSandboxLifecycleIdentityExceptGuardianNonce(stored, requested) &&
		stored.NonceSHA256 == requested.NonceSHA256
}

func sameTaskSandboxLifecycleIdentityExceptGuardianNonce(stored, requested app.TaskSandboxLifecycle) bool {
	return stored.ID == requested.ID && stored.TaskID == requested.TaskID && stored.OwnerID == requested.OwnerID &&
		stored.InitiatorID == requested.InitiatorID && stored.SandboxGeneration == requested.SandboxGeneration &&
		stored.SandboxID == requested.SandboxID && sameOptionalLifecycleTime(stored.ExpiresAt, requested.ExpiresAt)
}

func sameTaskSandboxLifecycleFenceExceptGuardianNonce(stored, requested app.TaskSandboxLifecycle) bool {
	return sameTaskSandboxLifecycleIdentityExceptGuardianNonce(stored, requested) &&
		sameTaskSandboxLifecycleFenceExceptGeneration(stored, requested)
}

func sameOptionalLifecycleTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validTaskSandboxGuardianNonceSHA256(value string) bool {
	return validPostgresSHA256(value)
}

func validLifecycleIdentity(value app.TaskSandboxLifecycle) bool {
	return domain.ValidOperationID(value.ID) && value.SandboxGeneration > 0
}

func validTaskSandboxLifecycleFence(value app.TaskSandboxLifecycle) bool {
	return domain.ValidTaskID(value.TaskID) && domain.ValidUserID(value.OwnerID) &&
		domain.ValidUserID(value.InitiatorID) && value.OwnerID == value.InitiatorID &&
		value.LeaseOwner != "" && len(value.LeaseOwner) <= 255 && strings.TrimSpace(value.LeaseOwner) == value.LeaseOwner &&
		!hasControlCharacters(value.LeaseOwner) && value.LeaseVersion > 0 && value.AuthorizationVersion > 0 &&
		value.AgentInstanceID != "" && len(value.AgentInstanceID) <= 128 && strings.TrimSpace(value.AgentInstanceID) == value.AgentInstanceID &&
		!hasControlCharacters(value.AgentInstanceID) && value.AgentGeneration > 0
}

func validTaskSandboxLifecycleUnknownCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func validTaskSandboxProviderID(value string) bool {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func containsLifecycleStatus(values []app.TaskSandboxLifecycleStatus, target app.TaskSandboxLifecycleStatus) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsTaskStatus(values []task.Status, target task.Status) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func lifecycleFenceConflict() error {
	return domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox lifecycle is no longer bound to the current Task lease and Agent generation")
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

var _ app.TaskSandboxLifecycleStore = (*Store)(nil)
