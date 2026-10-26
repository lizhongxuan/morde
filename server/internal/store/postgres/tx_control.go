package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const controlTransitionSelect = `
SELECT host_id, owner_id, request_id, action, phase,
       expected_control_version, blocked_control_version, target_control_version,
       task_ids, task_ids_saved, drained_receipt, failure_reason, created_at, updated_at
FROM host_control_transitions`

type controlHostSnapshot struct {
	ID             string
	OwnerID        string
	State          host.State
	ControlState   host.ControlState
	ControlVersion int64
}

// BeginControlTransition installs the fail-closed Host fence and its durable
// transition record in one transaction. All locks are acquired Host first,
// then transition, so concurrent requests for one Host serialize consistently.
func (s *Store) BeginControlTransition(ctx context.Context, request control.BeginRequest) (control.Transition, error) {
	if err := request.Validate(); err != nil {
		return control.Transition{}, err
	}
	var result control.Transition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockControlHost(ctx, tx, request.HostID, request.OwnerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("lock host for control transition: %w", err)
		}

		existing, err := loadControlTransition(ctx, tx, request.HostID, request.RequestID, true)
		if err == nil {
			if existing.OwnerID != request.OwnerID || existing.Action != request.Action || existing.ExpectedControlVersion != request.ExpectedControlVersion {
				return controlIdempotencyConflict()
			}
			result = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read control transition request: %w", err)
		}

		if current.State != host.StateRunning {
			return controlStateConflict("host is not running")
		}
		if current.ControlVersion != request.ExpectedControlVersion {
			return controlStateConflict("host control version changed")
		}
		sourceState, transitionState, err := controlStatesForAction(request.Action)
		if err != nil {
			return err
		}
		if current.ControlState != sourceState {
			return controlStateConflict("host control state does not allow this action")
		}

		blockedVersion := request.ExpectedControlVersion + 1
		targetVersion := blockedVersion + 1
		updated, err := tx.Exec(ctx, `
UPDATE hosts
SET control_state=$3, control_version=$4, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state='running' AND deleted_at IS NULL
  AND control_state=$5 AND control_version=$6`,
			request.HostID, request.OwnerID, string(transitionState), blockedVersion,
			string(sourceState), request.ExpectedControlVersion)
		if err != nil {
			return fmt.Errorf("advance host control fence: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return controlStateConflict("host control state changed")
		}

		_, err = tx.Exec(ctx, `
INSERT INTO host_control_transitions (
  host_id, owner_id, request_id, action, phase, expected_control_version,
  blocked_control_version, target_control_version, task_ids, task_ids_saved
) VALUES ($1,$2,$3,$4,'blocking',$5,$6,$7,'[]'::jsonb,false)`,
			request.HostID, request.OwnerID, request.RequestID, string(request.Action),
			request.ExpectedControlVersion, blockedVersion, targetVersion)
		if err != nil {
			if isUniqueViolation(err) {
				return controlStateConflict("host already has an active control transition")
			}
			return fmt.Errorf("insert host control transition: %w", err)
		}
		result, err = loadControlTransition(ctx, tx, request.HostID, request.RequestID, false)
		if err != nil {
			return fmt.Errorf("read new control transition: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Transition{}, err
	}
	return result, nil
}

func (s *Store) GetControlTransition(ctx context.Context, hostID, requestID string) (control.Transition, error) {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "control transition not found")
	}
	value, err := loadControlTransition(ctx, s.pool, hostID, requestID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "control transition not found")
	}
	if err != nil {
		return control.Transition{}, fmt.Errorf("get control transition: %w", err)
	}
	return value, nil
}

func (s *Store) GetActiveControlTransition(ctx context.Context, hostID string) (control.Transition, error) {
	if !domain.ValidHostID(hostID) {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "control transition not found")
	}
	value, err := scanControlTransition(s.pool.QueryRow(ctx, controlTransitionSelect+`
WHERE host_id=$1 AND phase <> 'completed'`, hostID))
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Transition{}, domain.NewError(domain.CodeNotFound, "active control transition not found")
	}
	if err != nil {
		return control.Transition{}, fmt.Errorf("get active control transition: %w", err)
	}
	return value, nil
}

// SaveControlTaskIDs stores the complete, immutable task snapshot. A saved
// empty list is distinct from a snapshot that has not yet been taken.
func (s *Store) SaveControlTaskIDs(ctx context.Context, hostID, requestID string, taskIDs []string) (control.Transition, error) {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) {
		return control.Transition{}, domain.InvalidArgument("transition", "control transition identity is invalid")
	}
	ids, err := normalizeControlTaskIDs(taskIDs)
	if err != nil {
		return control.Transition{}, err
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return control.Transition{}, fmt.Errorf("encode control task snapshot: %w", err)
	}
	var result control.Transition
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		value, current, err := lockTransitionAndHost(ctx, tx, hostID, requestID)
		if err != nil {
			return err
		}
		if value.TaskIDsSaved {
			if equalStrings(value.TaskIDs, ids) {
				result = value
				return nil
			}
			return controlStateConflict("control task snapshot is immutable")
		}
		if value.Phase == control.PhaseCompleted {
			return controlStateConflict("completed control transition has no task snapshot")
		}
		if err := verifyTransitionHost(current, value); err != nil {
			return err
		}
		if value.Phase != control.PhaseBlocking {
			return controlStateConflict("control task snapshot must be saved while blocking")
		}
		_, err = tx.Exec(ctx, `UPDATE host_control_transitions
SET task_ids=$3::jsonb, task_ids_saved=true, updated_at=clock_timestamp()
WHERE host_id=$1 AND request_id=$2`, hostID, requestID, encoded)
		if err != nil {
			return fmt.Errorf("save control task snapshot: %w", err)
		}
		result, err = loadControlTransition(ctx, tx, hostID, requestID, false)
		if err != nil {
			return fmt.Errorf("read saved control task snapshot: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Transition{}, err
	}
	return result, nil
}

// SetControlTransitionPhase advances the durable transition to blocking or
// draining. Repeated and older phase requests are harmless no-ops.
func (s *Store) SetControlTransitionPhase(ctx context.Context, hostID, requestID string, phase control.Phase) (control.Transition, error) {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) ||
		(phase != control.PhaseBlocking && phase != control.PhaseDraining && phase != control.PhaseDrained) {
		return control.Transition{}, domain.InvalidArgument("phase", "control transition phase is invalid")
	}
	var result control.Transition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		value, current, err := lockTransitionAndHost(ctx, tx, hostID, requestID)
		if err != nil {
			return err
		}
		if value.Phase == control.PhaseCompleted || controlPhaseRank(value.Phase) >= controlPhaseRank(phase) {
			result = value
			return nil
		}
		if phase == control.PhaseDraining && !value.TaskIDsSaved {
			return controlStateConflict("control task snapshot is not saved")
		}
		if phase == control.PhaseDrained && (value.Action != control.ActionRelease || !value.TaskIDsSaved || len(value.DrainedReceipt) != 0) {
			return controlStateConflict("only a quiesced release transition can be marked drained without a receipt")
		}
		if err := verifyTransitionHost(current, value); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE host_control_transitions
SET phase=$3, failure_reason='', updated_at=clock_timestamp()
WHERE host_id=$1 AND request_id=$2`, hostID, requestID, string(phase))
		if err != nil {
			return fmt.Errorf("advance control transition phase: %w", err)
		}
		result, err = loadControlTransition(ctx, tx, hostID, requestID, false)
		if err != nil {
			return fmt.Errorf("read advanced control transition: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Transition{}, err
	}
	return result, nil
}

// SaveControlDrainedReceipt persists the runtime-issued receipt before it can
// be used to open human input.
func (s *Store) SaveControlDrainedReceipt(ctx context.Context, hostID, requestID string, receipt json.RawMessage) (control.Transition, error) {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) {
		return control.Transition{}, domain.InvalidArgument("transition", "control transition identity is invalid")
	}
	if !validControlReceipt(receipt) {
		return control.Transition{}, domain.InvalidArgument("drained_receipt", "drained receipt is invalid")
	}
	var result control.Transition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		value, current, err := lockTransitionAndHost(ctx, tx, hostID, requestID)
		if err != nil {
			return err
		}
		if value.Action != control.ActionTakeOver {
			return controlStateConflict("release transition cannot store a drained receipt")
		}
		if value.Phase == control.PhaseDrained || value.Phase == control.PhaseCompleted {
			if receiptsEqual(value.DrainedReceipt, receipt) {
				result = value
				return nil
			}
			return controlIdempotencyConflict()
		}
		if err := verifyTransitionHost(current, value); err != nil {
			return err
		}
		if value.Phase != control.PhaseDraining || !value.TaskIDsSaved {
			return controlStateConflict("control transition is not ready for a drained receipt")
		}
		_, err = tx.Exec(ctx, `UPDATE host_control_transitions
SET phase='drained', drained_receipt=$3::jsonb, failure_reason='', updated_at=clock_timestamp()
WHERE host_id=$1 AND request_id=$2`, hostID, requestID, []byte(receipt))
		if err != nil {
			return fmt.Errorf("save control drained receipt: %w", err)
		}
		result, err = loadControlTransition(ctx, tx, hostID, requestID, false)
		if err != nil {
			return fmt.Errorf("read control drained receipt: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Transition{}, err
	}
	return result, nil
}

// CompleteControlTransition opens the target Host mode only after the caller
// has confirmed the matching runtime action. This transaction intentionally
// locks and updates only Host then transition rows, never task rows.
func (s *Store) CompleteControlTransition(ctx context.Context, hostID, requestID string) (control.Transition, error) {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) {
		return control.Transition{}, domain.InvalidArgument("transition", "control transition identity is invalid")
	}
	var result control.Transition
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		value, current, err := lockTransitionAndHost(ctx, tx, hostID, requestID)
		if err != nil {
			return err
		}
		if value.Phase == control.PhaseCompleted {
			result = value
			return nil
		}
		if err := verifyTransitionHost(current, value); err != nil {
			return err
		}
		if !value.TaskIDsSaved {
			return controlStateConflict("control task snapshot is not saved")
		}
		var finalState host.ControlState
		switch value.Action {
		case control.ActionTakeOver:
			if value.Phase != control.PhaseDrained || len(value.DrainedReceipt) == 0 {
				return controlStateConflict("takeover requires a persisted drained receipt")
			}
			finalState = host.ControlUser
		case control.ActionRelease:
			if value.Phase != control.PhaseDrained {
				return controlStateConflict("release transition is not ready to complete")
			}
			finalState = host.ControlAI
		default:
			return domain.InvalidArgument("action", "control action is invalid")
		}

		updated, err := tx.Exec(ctx, `UPDATE hosts
SET control_state=$4, control_version=$5, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2 AND state='running' AND deleted_at IS NULL
  AND control_state=$3 AND control_version=$6`,
			hostID, value.OwnerID, string(transitionStateForAction(value.Action)),
			string(finalState), value.TargetControlVersion, value.BlockedControlVersion)
		if err != nil {
			return fmt.Errorf("complete host control transition: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return controlStateConflict("host control fence changed before completion")
		}
		_, err = tx.Exec(ctx, `UPDATE host_control_transitions
SET phase='completed', failure_reason='', updated_at=clock_timestamp()
WHERE host_id=$1 AND request_id=$2 AND phase <> 'completed'`, hostID, requestID)
		if err != nil {
			return fmt.Errorf("complete control transition record: %w", err)
		}
		result, err = loadControlTransition(ctx, tx, hostID, requestID, false)
		if err != nil {
			return fmt.Errorf("read completed control transition: %w", err)
		}
		return nil
	})
	if err != nil {
		return control.Transition{}, err
	}
	return result, nil
}

// RecordControlTransitionFailure records a bounded, non-secret reason while
// deliberately preserving both the current phase and transitional Host mode.
func (s *Store) RecordControlTransitionFailure(ctx context.Context, hostID, requestID, reason string) error {
	if !domain.ValidHostID(hostID) || !validControlRequestID(requestID) {
		return domain.InvalidArgument("transition", "control transition identity is invalid")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > control.MaxFailureReasonBytes {
		return domain.InvalidArgument("failure_reason", "control transition failure reason is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		value, current, err := lockTransitionAndHost(ctx, tx, hostID, requestID)
		if err != nil {
			return err
		}
		if value.Phase == control.PhaseCompleted {
			return controlStateConflict("completed control transition cannot fail")
		}
		if err := verifyTransitionHost(current, value); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE host_control_transitions
SET failure_reason=$3, updated_at=clock_timestamp()
WHERE host_id=$1 AND request_id=$2`, hostID, requestID, reason)
		if err != nil {
			return fmt.Errorf("record control transition failure: %w", err)
		}
		return nil
	})
}

func lockTransitionAndHost(ctx context.Context, tx pgx.Tx, hostID, requestID string) (control.Transition, controlHostSnapshot, error) {
	current, err := lockControlHost(ctx, tx, hostID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Transition{}, controlHostSnapshot{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return control.Transition{}, controlHostSnapshot{}, fmt.Errorf("lock host for control transition: %w", err)
	}
	value, err := loadControlTransition(ctx, tx, hostID, requestID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return control.Transition{}, controlHostSnapshot{}, domain.NewError(domain.CodeNotFound, "control transition not found")
	}
	if err != nil {
		return control.Transition{}, controlHostSnapshot{}, fmt.Errorf("lock control transition: %w", err)
	}
	if current.OwnerID != value.OwnerID {
		return control.Transition{}, controlHostSnapshot{}, domain.NewError(domain.CodeNotFound, "control transition not found")
	}
	return value, current, nil
}

func lockControlHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string) (controlHostSnapshot, error) {
	query := `SELECT id, owner_id, state, control_state, control_version FROM hosts WHERE id=$1`
	args := []any{hostID}
	if ownerID != "" {
		query += ` AND owner_id=$2`
		args = append(args, ownerID)
	}
	query += ` FOR UPDATE`
	var result controlHostSnapshot
	err := tx.QueryRow(ctx, query, args...).Scan(&result.ID, &result.OwnerID, &result.State, &result.ControlState, &result.ControlVersion)
	return result, err
}

func loadControlTransition(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, hostID, requestID string, lock bool) (control.Transition, error) {
	query := controlTransitionSelect + ` WHERE host_id=$1 AND request_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanControlTransition(queryer.QueryRow(ctx, query, hostID, requestID))
}

func scanControlTransition(row pgx.Row) (control.Transition, error) {
	var value control.Transition
	var action, phase string
	var taskIDsJSON, receiptJSON []byte
	if err := row.Scan(
		&value.HostID, &value.OwnerID, &value.RequestID, &action, &phase,
		&value.ExpectedControlVersion, &value.BlockedControlVersion, &value.TargetControlVersion,
		&taskIDsJSON, &value.TaskIDsSaved, &receiptJSON, &value.FailureReason,
		&value.CreatedAt, &value.UpdatedAt,
	); err != nil {
		return control.Transition{}, err
	}
	value.Action = control.Action(action)
	value.Phase = control.Phase(phase)
	if err := json.Unmarshal(taskIDsJSON, &value.TaskIDs); err != nil {
		return control.Transition{}, fmt.Errorf("decode control task snapshot: %w", err)
	}
	if value.TaskIDs == nil {
		value.TaskIDs = []string{}
	}
	if receiptJSON != nil {
		value.DrainedReceipt = append(json.RawMessage(nil), receiptJSON...)
	}
	return value, nil
}

func verifyTransitionHost(current controlHostSnapshot, transition control.Transition) error {
	if current.State != host.StateRunning || current.OwnerID != transition.OwnerID ||
		current.ControlVersion != transition.BlockedControlVersion ||
		current.ControlState != transitionStateForAction(transition.Action) {
		return controlStateConflict("host control fence changed during transition")
	}
	return nil
}

func controlStatesForAction(action control.Action) (host.ControlState, host.ControlState, error) {
	switch action {
	case control.ActionTakeOver:
		return host.ControlAI, host.ControlTakingOver, nil
	case control.ActionRelease:
		return host.ControlUser, host.ControlReturning, nil
	default:
		return "", "", domain.InvalidArgument("action", "control action is invalid")
	}
}

func transitionStateForAction(action control.Action) host.ControlState {
	if action == control.ActionTakeOver {
		return host.ControlTakingOver
	}
	return host.ControlReturning
}

func normalizeControlTaskIDs(taskIDs []string) ([]string, error) {
	result := append([]string{}, taskIDs...)
	for _, taskID := range result {
		if !domain.ValidTaskID(taskID) {
			return nil, domain.InvalidArgument("task_ids", "control task id is invalid")
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, domain.InvalidArgument("task_ids", "control task ids must be unique")
		}
	}
	return result, nil
}

func validControlRequestID(requestID string) bool {
	return control.ValidRequestID(requestID)
}

func validControlReceipt(receipt json.RawMessage) bool {
	if len(receipt) == 0 || len(receipt) > 64<<10 || !json.Valid(receipt) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(receipt, &value) == nil && len(value) > 0
}

func receiptsEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == len(right)
	}
	var leftValue, rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&leftValue) != nil || rightDecoder.Decode(&rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func controlPhaseRank(phase control.Phase) int {
	switch phase {
	case control.PhaseBlocking:
		return 1
	case control.PhaseDraining:
		return 2
	case control.PhaseDrained:
		return 3
	case control.PhaseCompleted:
		return 4
	default:
		return 0
	}
}

func controlStateConflict(message string) error {
	return domain.NewError(domain.CodeInvalidState, message)
}

func controlIdempotencyConflict() error {
	return domain.NewError(domain.CodeIdempotencyConflict, "control request id was used for a different request")
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
