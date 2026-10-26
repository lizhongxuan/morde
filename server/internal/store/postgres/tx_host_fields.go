package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"

	"github.com/jackc/pgx/v5"
)

func (s *Store) SetSandboxID(ctx context.Context, hostID string, state host.State, sandboxID *string) error {
	result, err := s.pool.Exec(ctx, `
UPDATE hosts SET sandbox_id = $3, updated_at = now()
WHERE id = $1 AND state = $2 AND create_attempted_at IS NOT NULL`, hostID, string(state), sandboxID)
	if err != nil {
		return fmt.Errorf("set host sandbox id: %w", err)
	}
	if result.RowsAffected() != 0 {
		return nil
	}
	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("check host sandbox id: %w", err)
	}
	return hostStateConflict(current)
}

// SetRuntimeInstanceID persists the instance observed by the authenticated
// runtime handshake only while the Host generation and control fence are
// unchanged. Delayed handshakes are harmless after a recreate or control
// transition.
func (s *Store) SetRuntimeInstanceID(ctx context.Context, hostID string, generation, controlVersion int64, expectedInstanceID, instanceID string) error {
	if !domain.ValidHostID(hostID) || generation <= 0 || controlVersion < 0 || strings.TrimSpace(instanceID) == "" || len(instanceID) > 128 {
		return domain.NewError(domain.CodeInvalidArgument, "runtime instance identity is invalid")
	}
	result, err := s.pool.Exec(ctx, `
UPDATE hosts SET runtime_instance_id = $5, updated_at = now()
WHERE id = $1 AND runtime_generation = $2 AND control_version = $3
  AND state IN ('creating', 'running') AND deleted_at IS NULL
  AND runtime_instance_id IS NOT DISTINCT FROM NULLIF($4, '')`, hostID, generation, controlVersion, expectedInstanceID, instanceID)
	if err != nil {
		return fmt.Errorf("set runtime instance id: %w", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("check runtime instance fence: %w", err)
	}
	return hostStateConflict(current)
}

func (s *Store) SetHostExpiry(ctx context.Context, hostID string, expected host.State, expiresAt time.Time) (host.Host, error) {
	result, err := s.pool.Exec(ctx, `UPDATE hosts SET expires_at = $3, updated_at = now() WHERE id = $1 AND state = $2`, hostID, string(expected), expiresAt)
	return s.finishHostFieldUpdate(ctx, "set host expiry", hostID, result.RowsAffected(), err)
}

func (s *Store) finishHostFieldUpdate(ctx context.Context, operation, hostID string, rowsAffected int64, err error) (host.Host, error) {
	if err != nil {
		return host.Host{}, fmt.Errorf("%s: %w", operation, err)
	}
	if rowsAffected != 0 {
		return s.GetHost(ctx, hostID)
	}
	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return host.Host{}, fmt.Errorf("check host state after %s: %w", operation, err)
	}
	return host.Host{}, hostStateConflict(current)
}

var _ app.HostStore = (*Store)(nil)
