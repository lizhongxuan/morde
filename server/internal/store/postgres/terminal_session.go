package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5"
)

const terminalSessionPendingTimeout = 60 * time.Second

func (s *Store) EnsureTerminalSession(ctx context.Context, value app.TerminalSession, replace bool) (app.TerminalSession, bool, error) {
	if !domain.ValidHostID(value.HostID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.ActorID) ||
		value.RuntimeGeneration <= 0 || value.ControlVersion < 0 || !domain.ValidOperationID(value.AttemptID) || value.State != app.TerminalSessionCreating || value.AIOSessionID != "" ||
		(value.WorkspaceID != "" && !domain.ValidWorkspaceID(value.WorkspaceID)) {
		return app.TerminalSession{}, false, domain.InvalidArgument("terminal_session", "terminal session fields are invalid")
	}
	var result app.TerminalSession
	created := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		// Serialize ensure and explicit replacement per Host/actor; the partial unique index is the durable race guard.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('terminal-session:' || $1::text || ':' || $2::text, 0))`, value.HostID, value.ActorID); err != nil {
			return fmt.Errorf("lock terminal session: %w", err)
		}
		current, err := terminalSessionByHostActor(ctx, tx, value.HostID, value.ActorID, true)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find current terminal session: %w", err)
		}
		if exists && !replace {
			if current.RuntimeGeneration != value.RuntimeGeneration && current.State != app.TerminalSessionInterrupted {
				if _, err := tx.Exec(ctx, `UPDATE host_terminal_sessions SET state='interrupted', aio_session_id=NULL, updated_at=clock_timestamp() WHERE id=$1 AND state <> 'abandoned'`, current.ID); err != nil {
					return fmt.Errorf("interrupt stale terminal session: %w", err)
				}
				current.State = app.TerminalSessionInterrupted
				current.AIOSessionID = ""
			} else if current.State == app.TerminalSessionCreating {
				updated, err := markStaleTerminalSessionUnknown(ctx, tx, current.ID)
				if err != nil {
					return fmt.Errorf("expire stale terminal session: %w", err)
				}
				if updated {
					current.State = app.TerminalSessionUnknown
				}
			}
			result = current
			return nil
		}
		if exists && replace && current.RuntimeGeneration == value.RuntimeGeneration && current.State == app.TerminalSessionCreating {
			updated, err := markStaleTerminalSessionUnknown(ctx, tx, current.ID)
			if err != nil {
				return fmt.Errorf("expire stale terminal session: %w", err)
			}
			if !updated {
				result = current
				return nil
			}
		}
		if replace && exists {
			if _, err := tx.Exec(ctx, `UPDATE host_terminal_sessions SET state='abandoned', updated_at=clock_timestamp() WHERE id=$1 AND state <> 'abandoned'`, current.ID); err != nil {
				return fmt.Errorf("abandon previous terminal session: %w", err)
			}
		}
		result, err = insertTerminalSession(ctx, tx, value)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return app.TerminalSession{}, false, err
	}
	return result, created, nil
}

func markStaleTerminalSessionUnknown(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE host_terminal_sessions SET state='unknown', updated_at=clock_timestamp()
WHERE id=$1 AND state='creating' AND created_at <= clock_timestamp() - ($2 * interval '1 second')`, id, terminalSessionPendingTimeout.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) CompleteTerminalSession(ctx context.Context, reservation app.TerminalSession, aioSessionID string) (bool, error) {
	if !validTerminalReservation(reservation) || !validAIOSessionID(aioSessionID) {
		return false, domain.InvalidArgument("terminal_session", "terminal session completion is invalid")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE host_terminal_sessions
SET state='ready', aio_session_id=$5, updated_at=clock_timestamp()
WHERE host_id=$1 AND actor_id=$2 AND runtime_generation=$3 AND attempt_id=$4 AND state='creating'
  AND EXISTS (SELECT 1 FROM hosts WHERE id=$1 AND owner_id=$6 AND state='running' AND deleted_at IS NULL
    AND runtime_generation=$3 AND control_version=$7 AND control_state='user')`,
		reservation.HostID, reservation.ActorID, reservation.RuntimeGeneration, reservation.AttemptID, aioSessionID,
		reservation.OwnerID, reservation.ControlVersion)
	if err != nil {
		return false, fmt.Errorf("complete terminal session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) MarkTerminalSessionUnknown(ctx context.Context, reservation app.TerminalSession) error {
	if !validTerminalReservation(reservation) {
		return domain.InvalidArgument("terminal_session", "terminal session reservation is invalid")
	}
	_, err := s.pool.Exec(ctx, `
UPDATE host_terminal_sessions
SET state='unknown', updated_at=clock_timestamp()
WHERE host_id=$1 AND actor_id=$2 AND runtime_generation=$3 AND attempt_id=$4 AND state='creating'`,
		reservation.HostID, reservation.ActorID, reservation.RuntimeGeneration, reservation.AttemptID)
	if err != nil {
		return fmt.Errorf("mark terminal session unknown: %w", err)
	}
	return nil
}

func (s *Store) MarkTerminalSessionInterrupted(ctx context.Context, hostID, actorID string, generation, controlVersion int64, sessionID string) (bool, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(actorID) || generation <= 0 || controlVersion < 0 || !validAIOSessionID(sessionID) {
		return false, domain.InvalidArgument("terminal_session", "terminal session identity is invalid")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE host_terminal_sessions
SET state='interrupted', aio_session_id=NULL, updated_at=clock_timestamp()
WHERE host_id=$1 AND actor_id=$2 AND runtime_generation=$3 AND control_version=$4
  AND aio_session_id=$5 AND state='ready'`, hostID, actorID, generation, controlVersion, sessionID)
	if err != nil {
		return false, fmt.Errorf("mark terminal session interrupted: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

type terminalSessionQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func terminalSessionByHostActor(ctx context.Context, q terminalSessionQuerier, hostID, actorID string, lock bool) (app.TerminalSession, error) {
	query := `SELECT id, host_id, owner_id, actor_id, runtime_generation, control_version, COALESCE(workspace_id, ''), attempt_id,
COALESCE(aio_session_id, ''), state, created_at, updated_at
FROM host_terminal_sessions WHERE host_id=$1 AND actor_id=$2 AND state <> 'abandoned' ORDER BY id DESC LIMIT 1`
	if lock {
		query += ` FOR UPDATE`
	}
	var value app.TerminalSession
	err := q.QueryRow(ctx, query, hostID, actorID).Scan(
		&value.ID, &value.HostID, &value.OwnerID, &value.ActorID, &value.RuntimeGeneration, &value.ControlVersion,
		&value.WorkspaceID, &value.AttemptID, &value.AIOSessionID, &value.State, &value.CreatedAt, &value.UpdatedAt,
	)
	return value, err
}

func insertTerminalSession(ctx context.Context, tx pgx.Tx, value app.TerminalSession) (app.TerminalSession, error) {
	var result app.TerminalSession
	err := tx.QueryRow(ctx, `
INSERT INTO host_terminal_sessions (host_id, owner_id, actor_id, runtime_generation, control_version, workspace_id, attempt_id, state)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,'creating')
	RETURNING id, host_id, owner_id, actor_id, runtime_generation, control_version, COALESCE(workspace_id,''), attempt_id,
COALESCE(aio_session_id,''), state, created_at, updated_at`,
		value.HostID, value.OwnerID, value.ActorID, value.RuntimeGeneration, value.ControlVersion, value.WorkspaceID, value.AttemptID,
	).Scan(&result.ID, &result.HostID, &result.OwnerID, &result.ActorID, &result.RuntimeGeneration, &result.ControlVersion, &result.WorkspaceID,
		&result.AttemptID, &result.AIOSessionID, &result.State, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return app.TerminalSession{}, fmt.Errorf("reserve terminal session: %w", err)
	}
	return result, nil
}

func validTerminalReservation(value app.TerminalSession) bool {
	return domain.ValidHostID(value.HostID) && domain.ValidUserID(value.OwnerID) && domain.ValidUserID(value.ActorID) &&
		value.RuntimeGeneration > 0 && value.ControlVersion >= 0 && domain.ValidOperationID(value.AttemptID) && value.State == app.TerminalSessionCreating && value.AIOSessionID == ""
}

func validAIOSessionID(value string) bool {
	if len(value) == 0 || len(value) > 255 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("-_.~", r):
		default:
			return false
		}
	}
	return true
}

var _ app.TerminalSessionStore = (*Store)(nil)
