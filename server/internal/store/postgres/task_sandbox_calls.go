package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxTaskSandboxCallOutputBytes = 64 << 10

type taskSandboxCallScanner interface {
	Scan(...any) error
}

const taskSandboxCallSelect = `
SELECT id,task_id,owner_id,initiator_id,COALESCE(project_id,''),workspace_id,workspace_owner_id,
 input_revision,workspace_checkpoint,project_resource_version,context_digest,authorization_version,
 agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,
 lease_owner,lease_version,call_id,sandbox_id,sandbox_generation,request_hash,status,exit_code,output,
 result_sha256,unknown_code,created_at,updated_at
FROM task_sandbox_call_receipts`

func (s *Store) PrepareTaskSandboxCall(ctx context.Context, value app.TaskSandboxCall) (app.TaskSandboxCall, bool, error) {
	if !validTaskSandboxCall(value) || value.Status != task.OperationPrepared || value.SandboxID != "" || value.SandboxGeneration != 0 ||
		value.ExitCode != nil || value.Output != "" || value.ResultSHA256 != nil || value.UnknownCode != "" {
		return app.TaskSandboxCall{}, false, domain.InvalidArgument("sandbox_call", "task sandbox call fields are invalid")
	}
	var result app.TaskSandboxCall
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTaskSandboxCallLease(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskSandboxCallForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.CallID)
		if err == nil {
			if !sameTaskSandboxCallRequest(stored, value) {
				return idempotencyConflict()
			}
			if stored.Status == task.OperationPrepared && (stored.LeaseVersion != value.LeaseVersion || stored.LeaseOwner != value.LeaseOwner ||
				!sameTaskSandboxCallAgent(stored, value)) {
				if err := tx.QueryRow(ctx, `UPDATE task_sandbox_call_receipts
SET lease_owner=$2,lease_version=$3,agent_instance_id=$4,agent_generation=$5,agent_session_id=$6,
agent_engine_version=$7,agent_profile_version=$8,updated_at=clock_timestamp()
WHERE id=$1 AND status='prepared' RETURNING updated_at`, stored.ID, value.LeaseOwner, value.LeaseVersion,
					value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID, value.AgentEngineVersion, value.AgentProfileVersion).Scan(&stored.UpdatedAt); err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return domain.NewError(domain.CodeConflict, "prepared Task Sandbox call changed during lease recovery")
					}
					return fmt.Errorf("adopt prepared Task Sandbox call: %w", err)
				}
				stored.LeaseOwner, stored.LeaseVersion = value.LeaseOwner, value.LeaseVersion
				stored.AgentInstanceID, stored.AgentGeneration, stored.AgentSessionID = value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID
				stored.AgentEngineVersion, stored.AgentProfileVersion = value.AgentEngineVersion, value.AgentProfileVersion
			}
			result, replayed = stored, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read Task Sandbox call before prepare: %w", err)
		}
		var activeCall string
		err = tx.QueryRow(ctx, `SELECT call_id FROM task_sandbox_call_receipts
WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown') LIMIT 1 FOR UPDATE`, value.TaskID).Scan(&activeCall)
		if err == nil {
			return domain.NewError(domain.CodeConflict, "another Task Sandbox call is still active or requires reconciliation")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check active Task Sandbox call: %w", err)
		}
		command, err := tx.Exec(ctx, `INSERT INTO task_sandbox_call_receipts (
id,task_id,owner_id,initiator_id,project_id,project_resource_version,workspace_id,workspace_owner_id,
input_revision,workspace_checkpoint,context_digest,authorization_version,lease_owner,lease_version,
agent_instance_id,agent_generation,agent_session_id,agent_engine_version,agent_profile_version,
call_id,request_hash,status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,'prepared')`,
			value.ID, value.TaskID, value.OwnerID, value.InitiatorID, value.ProjectID, value.ProjectResourceVersion,
			value.WorkspaceID, value.WorkspaceOwnerID, value.InputRevision, value.WorkspaceCheckpoint,
			value.ContextDigest, value.AuthorizationVersion, value.LeaseOwner, value.LeaseVersion,
			value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID, value.AgentEngineVersion, value.AgentProfileVersion,
			value.CallID, value.RequestHash)
		if err != nil {
			return fmt.Errorf("persist prepared Task Sandbox call: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "Task Sandbox call was not prepared")
		}
		result, err = getTaskSandboxCallForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.CallID)
		if err != nil {
			return fmt.Errorf("read prepared Task Sandbox call: %w", err)
		}
		return nil
	})
	return result, replayed, err
}

func (s *Store) MarkTaskSandboxCallDispatched(ctx context.Context, value app.TaskSandboxCall, sandboxID string, sandboxGeneration int64) (app.TaskSandboxCall, error) {
	if !validTaskSandboxCall(value) || sandboxID == "" || len(sandboxID) > 512 || strings.TrimSpace(sandboxID) != sandboxID ||
		hasControlCharacters(sandboxID) || sandboxGeneration < 1 {
		return app.TaskSandboxCall{}, domain.InvalidArgument("sandbox_call", "Task Sandbox dispatch fence is invalid")
	}
	var result app.TaskSandboxCall
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockTaskSandboxCallLease(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskSandboxCallForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.CallID)
		if err != nil {
			return fmt.Errorf("read prepared Task Sandbox call for dispatch: %w", err)
		}
		if !sameTaskSandboxCallRequest(stored, value) || stored.Status != task.OperationPrepared ||
			stored.LeaseVersion != value.LeaseVersion || stored.LeaseOwner != value.LeaseOwner || !sameTaskSandboxCallAgent(stored, value) {
			return domain.NewError(domain.CodeConflict, "Task Sandbox call is no longer prepared under this lease")
		}
		if err := tx.QueryRow(ctx, `UPDATE task_sandbox_call_receipts
SET status='dispatched',sandbox_id=$2,sandbox_generation=$3,updated_at=clock_timestamp()
WHERE id=$1 AND status='prepared' RETURNING updated_at`, stored.ID, sandboxID, sandboxGeneration).Scan(&stored.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "Task Sandbox call changed before dispatch")
			}
			return fmt.Errorf("mark Task Sandbox call dispatched: %w", err)
		}
		stored.Status, stored.SandboxID, stored.SandboxGeneration = task.OperationDispatched, sandboxID, sandboxGeneration
		result = stored
		return nil
	})
	return result, err
}

func (s *Store) CompleteTaskSandboxCall(ctx context.Context, value app.TaskSandboxCall, status task.OperationStatus, exitCode int32, output string) (app.TaskSandboxCall, error) {
	if !validTaskSandboxCall(value) || (status != task.OperationSucceeded && status != task.OperationFailed) ||
		status == task.OperationSucceeded && exitCode != 0 || !utf8.ValidString(output) || len(output) > maxTaskSandboxCallOutputBytes {
		return app.TaskSandboxCall{}, domain.InvalidArgument("sandbox_call.result", "Task Sandbox call result is invalid or too large")
	}
	resultHash := sha256.Sum256([]byte(output))
	var result app.TaskSandboxCall
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockTaskSandboxCallCurrentExecutionFence(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getTaskSandboxCallForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.CallID)
		if err != nil {
			return fmt.Errorf("read Task Sandbox call for completion: %w", err)
		}
		if !sameTaskSandboxCallRequest(stored, value) || !sameTaskSandboxCallDispatch(stored, value) {
			return idempotencyConflict()
		}
		if stored.Status == status && stored.ExitCode != nil && *stored.ExitCode == exitCode && stored.Output == output && bytes.Equal(stored.ResultSHA256, resultHash[:]) {
			result = stored
			return nil
		}
		if stored.Status != task.OperationDispatched {
			return domain.NewError(domain.CodeConflict, "Task Sandbox call is not awaiting a result")
		}
		if err := tx.QueryRow(ctx, `UPDATE task_sandbox_call_receipts
SET status=$2,exit_code=$3,output=$4,result_sha256=$5,updated_at=clock_timestamp()
WHERE id=$1 AND status='dispatched' RETURNING updated_at`, stored.ID, status, exitCode, output, resultHash[:]).Scan(&stored.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "Task Sandbox call changed before result commit")
			}
			return fmt.Errorf("complete Task Sandbox call: %w", err)
		}
		stored.Status, stored.ExitCode, stored.Output = status, int32Pointer(exitCode), output
		stored.ResultSHA256, stored.UnknownCode = append([]byte(nil), resultHash[:]...), ""
		result = stored
		return nil
	})
	return result, err
}

// A sandbox may finish after its Task lease was recovered by a newer Agent.
// Keep the completion fence independent of the Workspace's live revision: the
// result belongs to the original snapshot and publication has its own CAS.
func lockTaskSandboxCallCurrentExecutionFence(ctx context.Context, tx pgx.Tx, value app.TaskSandboxCall) error {
	if value.InitiatorID != value.OwnerID || value.WorkspaceOwnerID != value.OwnerID || value.ProjectID != "" || value.ProjectResourceVersion != 0 {
		return domain.NewError(domain.CodeForbidden, "Task Sandbox is currently available only for personal Hostless Workspaces")
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, value.InitiatorID, nil)
	if err != nil {
		return err
	}
	var status, leaseOwner, agentInstanceID, agentSessionID, agentEngineVersion, agentProfileVersion string
	var executionLocation, workspaceID, initiatorID, projectID string
	var leaseVersion, executionEpoch, authVersion, agentGeneration, inputRevision, workspaceCheckpoint int64
	var leaseValid, hostless bool
	var contextDigest []byte
	err = tx.QueryRow(ctx, `SELECT status,lease_owner,lease_version,execution_epoch,
COALESCE(lease_expires_at>clock_timestamp(),false),auth_version,agent_instance_id,agent_generation,
agent_session_id,agent_engine_version,agent_profile_version,execution_location,host_id IS NULL,
COALESCE(workspace_id,''),COALESCE(input_revision,0),COALESCE(workspace_checkpoint_revision,0),
context_digest,initiator_id,COALESCE(project_id,'')
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, value.TaskID, value.OwnerID).Scan(
		&status, &leaseOwner, &leaseVersion, &executionEpoch, &leaseValid, &authVersion, &agentInstanceID,
		&agentGeneration, &agentSessionID, &agentEngineVersion, &agentProfileVersion, &executionLocation,
		&hostless, &workspaceID, &inputRevision, &workspaceCheckpoint, &contextDigest, &initiatorID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock Task Sandbox result execution fence: %w", err)
	}
	if status != string(task.StatusRunning) || leaseOwner != value.LeaseOwner || leaseVersion != value.LeaseVersion ||
		!leaseValid || executionEpoch != gate.epoch || authVersion != value.AuthorizationVersion ||
		agentInstanceID != value.AgentInstanceID || agentGeneration != value.AgentGeneration ||
		agentSessionID != value.AgentSessionID || agentEngineVersion != value.AgentEngineVersion ||
		agentProfileVersion != value.AgentProfileVersion || executionLocation != "server" || !hostless ||
		workspaceID != value.WorkspaceID || inputRevision != value.InputRevision || workspaceCheckpoint != value.WorkspaceCheckpoint ||
		!bytes.Equal(contextDigest, value.ContextDigest) || initiatorID != value.InitiatorID || projectID != "" {
		return domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox result is no longer bound to the current Task execution")
	}
	return nil
}

func (s *Store) MarkTaskSandboxCallUnknown(ctx context.Context, value app.TaskSandboxCall, code string) error {
	if !validTaskSandboxCall(value) || !validTaskSandboxUnknownCode(code) {
		return domain.InvalidArgument("sandbox_call", "Task Sandbox unknown result code is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, err := getTaskSandboxCallForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.CallID)
		if err != nil {
			return fmt.Errorf("read Task Sandbox call for reconciliation: %w", err)
		}
		if !sameTaskSandboxCallRequest(stored, value) || !sameTaskSandboxCallDispatch(stored, value) {
			return idempotencyConflict()
		}
		if stored.Status == task.OperationUnknown && stored.UnknownCode == code {
			return nil
		}
		if stored.Status != task.OperationDispatched {
			return domain.NewError(domain.CodeConflict, "Task Sandbox call cannot be marked unknown")
		}
		if _, err := tx.Exec(ctx, `UPDATE task_sandbox_call_receipts
SET status='unknown',unknown_code=$2,updated_at=clock_timestamp() WHERE id=$1 AND status='dispatched'`, stored.ID, code); err != nil {
			return fmt.Errorf("mark Task Sandbox call outcome unknown: %w", err)
		}
		return nil
	})
}

func (s *Store) GetTaskSandboxCall(ctx context.Context, ownerID, taskID, callID string) (app.TaskSandboxCall, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !validTaskSandboxCallID(callID) {
		return app.TaskSandboxCall{}, domain.NewError(domain.CodeNotFound, "Task Sandbox call not found")
	}
	value, err := scanTaskSandboxCall(s.pool.QueryRow(ctx, taskSandboxCallSelect+` WHERE owner_id=$1 AND task_id=$2 AND call_id=$3`, ownerID, taskID, callID))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskSandboxCall{}, domain.NewError(domain.CodeNotFound, "Task Sandbox call not found")
	}
	if err != nil {
		return app.TaskSandboxCall{}, fmt.Errorf("get Task Sandbox call: %w", err)
	}
	return value, nil
}

func lockTaskSandboxCallLease(ctx context.Context, tx pgx.Tx, value app.TaskSandboxCall) (sqlcgen.Task, error) {
	projectID, err := lockTaskInitiatorInTx(ctx, tx, value.TaskID, value.OwnerID, value.InitiatorID)
	if err != nil {
		return sqlcgen.Task{}, err
	}
	if projectID != "" || value.ProjectID != "" || value.ProjectResourceVersion != 0 || value.InitiatorID != value.OwnerID || value.WorkspaceOwnerID != value.OwnerID {
		return sqlcgen.Task{}, domain.NewError(domain.CodeForbidden, "Task Sandbox is currently available only for personal Hostless Workspaces")
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, value.InitiatorID, nil)
	if err != nil {
		return sqlcgen.Task{}, err
	}
	var status, leaseOwner string
	var leaseVersion, executionEpoch int64
	var leaseValid bool
	err = tx.QueryRow(ctx, `SELECT status,lease_owner,lease_version,execution_epoch,
COALESCE(lease_expires_at>clock_timestamp(),false) AND current_revision=desired_revision
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, value.TaskID, value.OwnerID).
		Scan(&status, &leaseOwner, &leaseVersion, &executionEpoch, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Task{}, missingTaskError()
	}
	if err != nil {
		return sqlcgen.Task{}, fmt.Errorf("lock Task Sandbox Task lease: %w", err)
	}
	if status != string(task.StatusRunning) || leaseOwner != value.LeaseOwner || leaseVersion != value.LeaseVersion ||
		!leaseValid || executionEpoch != gate.epoch {
		return sqlcgen.Task{}, taskLeaseConflict()
	}
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: value.TaskID, OwnerID: value.OwnerID})
	if err != nil {
		return sqlcgen.Task{}, fmt.Errorf("read Task Sandbox Task authorization: %w", err)
	}
	if row.HostID.Valid || row.ExecutionLocation != "server" || !row.WorkspaceID.Valid || row.WorkspaceID.String != value.WorkspaceID ||
		!row.InputRevision.Valid || row.InputRevision.Int64 != value.InputRevision ||
		!row.WorkspaceCheckpointRevision.Valid || row.WorkspaceCheckpointRevision.Int64 != value.WorkspaceCheckpoint ||
		row.AuthVersion != value.AuthorizationVersion || row.InitiatorID != value.InitiatorID ||
		row.ProjectID.Valid || row.WorkspaceProjectResourceVersion.Valid || row.WorkspaceOwnerID.Valid && row.WorkspaceOwnerID.String != value.OwnerID ||
		row.AgentInstanceID != value.AgentInstanceID || row.AgentGeneration != value.AgentGeneration || row.AgentSessionID != value.AgentSessionID ||
		row.AgentEngineVersion != value.AgentEngineVersion || row.AgentProfileVersion != value.AgentProfileVersion ||
		!bytes.Equal(row.ContextDigest, value.ContextDigest) {
		return sqlcgen.Task{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox Workspace or authorization snapshot changed")
	}
	authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
	if err != nil {
		return sqlcgen.Task{}, err
	}
	if !authorized {
		return sqlcgen.Task{}, domain.NewError(domain.CodeUnauthenticated, "Task Sandbox authorization is no longer valid")
	}
	if err := lockWorkspaceMutationFence(ctx, tx, value.WorkspaceID, false); err != nil {
		return sqlcgen.Task{}, err
	}
	var workspaceOwner, workspaceState string
	var workspaceRevision int64
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision FROM workspaces WHERE id=$1 FOR SHARE`, value.WorkspaceID).
		Scan(&workspaceOwner, &workspaceState, &workspaceRevision); errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.Task{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox Workspace is unavailable")
	} else if err != nil {
		return sqlcgen.Task{}, fmt.Errorf("lock Task Sandbox Workspace snapshot: %w", err)
	}
	if workspaceOwner != value.OwnerID || workspaceState != "ready" || workspaceRevision != value.WorkspaceCheckpoint {
		return sqlcgen.Task{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox Workspace revision changed")
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, value.WorkspaceID); err != nil {
		return sqlcgen.Task{}, err
	}
	if err := requireNoPendingWorkspaceLegacyImport(ctx, tx, value.WorkspaceID); err != nil {
		return sqlcgen.Task{}, err
	}
	return row, nil
}

func getTaskSandboxCallForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, callID string) (app.TaskSandboxCall, error) {
	value, err := scanTaskSandboxCall(tx.QueryRow(ctx, taskSandboxCallSelect+` WHERE owner_id=$1 AND task_id=$2 AND call_id=$3 FOR UPDATE`, ownerID, taskID, callID))
	if err != nil {
		return app.TaskSandboxCall{}, err
	}
	return value, nil
}

func scanTaskSandboxCall(row taskSandboxCallScanner) (app.TaskSandboxCall, error) {
	var value app.TaskSandboxCall
	var exitCode pgtype.Int4
	if err := row.Scan(&value.ID, &value.TaskID, &value.OwnerID, &value.InitiatorID, &value.ProjectID,
		&value.WorkspaceID, &value.WorkspaceOwnerID, &value.InputRevision, &value.WorkspaceCheckpoint,
		&value.ProjectResourceVersion, &value.ContextDigest, &value.AuthorizationVersion, &value.AgentInstanceID,
		&value.AgentGeneration, &value.AgentSessionID, &value.AgentEngineVersion, &value.AgentProfileVersion, &value.LeaseOwner,
		&value.LeaseVersion, &value.CallID, &value.SandboxID, &value.SandboxGeneration, &value.RequestHash,
		&value.Status, &exitCode, &value.Output, &value.ResultSHA256, &value.UnknownCode, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return app.TaskSandboxCall{}, err
	}
	if exitCode.Valid {
		value.ExitCode = int32Pointer(exitCode.Int32)
	}
	value.ContextDigest = append([]byte(nil), value.ContextDigest...)
	value.RequestHash = append([]byte(nil), value.RequestHash...)
	value.ResultSHA256 = append([]byte(nil), value.ResultSHA256...)
	value.Status = task.OperationStatus(value.Status)
	return value, nil
}

func validTaskSandboxCall(value app.TaskSandboxCall) bool {
	return domain.ValidOperationID(value.ID) && domain.ValidTaskID(value.TaskID) && domain.ValidUserID(value.OwnerID) &&
		domain.ValidUserID(value.InitiatorID) && domain.ValidWorkspaceID(value.WorkspaceID) && domain.ValidUserID(value.WorkspaceOwnerID) &&
		validTaskSandboxAgentIdentity(value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID, value.AgentEngineVersion, value.AgentProfileVersion) &&
		validTaskSandboxCallID(value.CallID) && value.LeaseOwner != "" && len(value.LeaseOwner) <= 255 &&
		strings.TrimSpace(value.LeaseOwner) == value.LeaseOwner && !hasControlCharacters(value.LeaseOwner) &&
		value.InputRevision >= 0 && value.WorkspaceCheckpoint >= value.InputRevision && value.AuthorizationVersion > 0 &&
		value.LeaseVersion > 0 && len(value.ContextDigest) == sha256.Size && len(value.RequestHash) == sha256.Size
}

func validTaskSandboxCallID(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !hasControlCharacters(value)
}

func validTaskSandboxUnknownCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func hasControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func sameTaskSandboxCallRequest(stored, requested app.TaskSandboxCall) bool {
	return stored.TaskID == requested.TaskID && stored.OwnerID == requested.OwnerID && stored.InitiatorID == requested.InitiatorID &&
		stored.ProjectID == requested.ProjectID && stored.ProjectResourceVersion == requested.ProjectResourceVersion &&
		stored.WorkspaceID == requested.WorkspaceID && stored.WorkspaceOwnerID == requested.WorkspaceOwnerID &&
		stored.InputRevision == requested.InputRevision && stored.WorkspaceCheckpoint == requested.WorkspaceCheckpoint &&
		stored.AuthorizationVersion == requested.AuthorizationVersion && stored.CallID == requested.CallID &&
		bytes.Equal(stored.ContextDigest, requested.ContextDigest) && bytes.Equal(stored.RequestHash, requested.RequestHash)
}

func sameTaskSandboxCallAgent(stored, requested app.TaskSandboxCall) bool {
	return stored.AgentInstanceID == requested.AgentInstanceID && stored.AgentGeneration == requested.AgentGeneration &&
		stored.AgentSessionID == requested.AgentSessionID && stored.AgentEngineVersion == requested.AgentEngineVersion &&
		stored.AgentProfileVersion == requested.AgentProfileVersion
}

func validTaskSandboxAgentIdentity(instanceID string, generation int64, sessionID, engineVersion, profileVersion string) bool {
	validText := func(value string, max int) bool {
		return value != "" && len(value) <= max && strings.TrimSpace(value) == value && !hasControlCharacters(value)
	}
	return validText(instanceID, 128) && generation > 0 && validText(sessionID, 128) &&
		validText(engineVersion, 128) && validText(profileVersion, 128)
}

func sameTaskSandboxCallDispatch(stored, requested app.TaskSandboxCall) bool {
	return stored.LeaseOwner == requested.LeaseOwner && stored.LeaseVersion == requested.LeaseVersion &&
		sameTaskSandboxCallAgent(stored, requested) &&
		stored.SandboxID != "" && stored.SandboxID == requested.SandboxID &&
		stored.SandboxGeneration > 0 && stored.SandboxGeneration == requested.SandboxGeneration
}

func int32Pointer(value int32) *int32 { return &value }

var _ app.TaskSandboxCallStore = (*Store)(nil)
