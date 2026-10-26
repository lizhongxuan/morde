package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/runtimeclient"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

const applicationInstallSelect = `
SELECT host_id, owner_id, request_id, application_id, runtime_generation,
       runtime_instance_id, control_version, status, operation, created_at, updated_at
FROM host_application_installs`

func (s *Store) ReserveApplicationInstall(ctx context.Context, value app.ApplicationInstall) (app.ApplicationInstall, bool, error) {
	if value.Status != app.ApplicationInstallUnknown || value.Operation != nil || value.Validate() != nil {
		return app.ApplicationInstall{}, false, domain.InvalidArgument("application_install", "application install reservation is invalid")
	}
	var result app.ApplicationInstall
	created := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, value.HostID); err != nil {
			return err
		}
		hostRow, err := sqlcgen.New(tx).GetHost(ctx, value.HostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read Host before application install: %w", err)
		}
		current := hostDomain(hostRow)
		if current.OwnerID != value.OwnerID || current.DeletedAt != nil {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		existing, err := loadApplicationInstall(ctx, tx, value.HostID, value.RequestID, true)
		if err == nil {
			if existing.OwnerID != value.OwnerID || existing.ApplicationID != value.ApplicationID ||
				existing.RuntimeGeneration != value.RuntimeGeneration {
				return domain.NewError(domain.CodeIdempotencyConflict, "install request is bound to another Host generation")
			}
			if !existing.Terminal() && current.State != host.StateRunning {
				return domain.NewError(domain.CodeConflict, "Host is no longer running")
			}
			result = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing application install: %w", err)
		}
		if current.State != host.StateRunning || current.RuntimeGeneration != value.RuntimeGeneration ||
			current.RuntimeInstanceID != value.RuntimeInstanceID || current.ControlVersion != value.ControlVersion ||
			(current.ControlState != host.ControlAI && current.ControlState != host.ControlUser) {
			return domain.NewError(domain.CodeConflict, "Host runtime fence changed before application install")
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM host_application_installs
  WHERE host_id=$1 AND status IN ('unknown','accepted','running')
)`, value.HostID).Scan(&pending); err != nil {
			return fmt.Errorf("check pending application install: %w", err)
		}
		if pending {
			return domain.NewError(domain.CodeConflict, "another application install is unresolved")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_application_installs (
  host_id, owner_id, request_id, application_id, runtime_generation,
  runtime_instance_id, control_version, status
) VALUES ($1,$2,$3,$4,$5,$6,$7,'unknown')`, value.HostID, value.OwnerID, value.RequestID,
			value.ApplicationID, value.RuntimeGeneration, value.RuntimeInstanceID, value.ControlVersion); err != nil {
			return fmt.Errorf("reserve Host application install: %w", err)
		}
		result, err = loadApplicationInstall(ctx, tx, value.HostID, value.RequestID, false)
		if err != nil {
			return fmt.Errorf("read Host application install reservation: %w", err)
		}
		created = true
		return nil
	})
	return result, created, err
}

func (s *Store) GetPendingApplicationInstall(ctx context.Context, hostID, ownerID string) (app.ApplicationInstall, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) {
		return app.ApplicationInstall{}, domain.NewError(domain.CodeNotFound, "application install not found")
	}
	value, err := scanApplicationInstall(s.pool.QueryRow(ctx, applicationInstallSelect+`
WHERE host_id=$1 AND owner_id=$2 AND status IN ('unknown','accepted','running')`, hostID, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ApplicationInstall{}, domain.NewError(domain.CodeNotFound, "application install not found")
	}
	if err != nil {
		return app.ApplicationInstall{}, fmt.Errorf("read pending application install: %w", err)
	}
	return value, nil
}

func (s *Store) RecordApplicationInstallOperation(ctx context.Context, hostID, ownerID, requestID string, operation runtimeclient.ApplicationInstallOperation) (app.ApplicationInstall, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) || !runtimeclient.ValidApplicationInstallOperation(operation) {
		return app.ApplicationInstall{}, domain.InvalidArgument("application_install", "application install result is invalid")
	}
	var result app.ApplicationInstall
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, hostID); err != nil {
			return err
		}
		currentRow, err := sqlcgen.New(tx).GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read Host before application install update: %w", err)
		}
		stored, err := loadApplicationInstall(ctx, tx, hostID, requestID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "application install not found")
		}
		if err != nil {
			return fmt.Errorf("read application install before update: %w", err)
		}
		if stored.OwnerID != ownerID || operation.ApplicationID != stored.ApplicationID {
			return domain.NewError(domain.CodeConflict, "application install identity changed")
		}
		current := hostDomain(currentRow)
		if current.OwnerID != ownerID || current.DeletedAt != nil || current.State != host.StateRunning ||
			current.RuntimeGeneration != stored.RuntimeGeneration {
			return domain.NewError(domain.CodeConflict, "Host generation changed before application install update")
		}
		if stored.Operation != nil {
			if stored.Operation.ID != operation.ID || !stored.Operation.CreatedAt.Equal(operation.CreatedAt) {
				return domain.NewError(domain.CodeConflict, "application install operation changed")
			}
			if stored.Terminal() || operation.UpdatedAt.Before(stored.Operation.UpdatedAt) ||
				(operation.Status == runtimeclient.OperationAccepted && stored.Status == string(runtimeclient.OperationRunning)) {
				result = stored
				return nil
			}
		}
		encoded, err := json.Marshal(operation)
		if err != nil {
			return fmt.Errorf("encode application install operation: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE host_application_installs
SET status=$4, operation_id=$5, operation=$6, updated_at=clock_timestamp()
WHERE host_id=$1 AND owner_id=$2 AND request_id=$3`, hostID, ownerID, requestID,
			operation.Status, operation.ID, encoded); err != nil {
			return fmt.Errorf("record application install operation: %w", err)
		}
		result, err = loadApplicationInstall(ctx, tx, hostID, requestID, false)
		return err
	})
	return result, err
}

func loadApplicationInstall(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, hostID, requestID string, lock bool) (app.ApplicationInstall, error) {
	query := applicationInstallSelect + ` WHERE host_id=$1 AND request_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanApplicationInstall(queryer.QueryRow(ctx, query, hostID, requestID))
}

func scanApplicationInstall(row pgx.Row) (app.ApplicationInstall, error) {
	var value app.ApplicationInstall
	var encoded []byte
	if err := row.Scan(&value.HostID, &value.OwnerID, &value.RequestID, &value.ApplicationID,
		&value.RuntimeGeneration, &value.RuntimeInstanceID, &value.ControlVersion,
		&value.Status, &encoded, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return app.ApplicationInstall{}, err
	}
	if len(encoded) != 0 {
		var operation runtimeclient.ApplicationInstallOperation
		if err := json.Unmarshal(encoded, &operation); err != nil {
			return app.ApplicationInstall{}, fmt.Errorf("decode application install operation: %w", err)
		}
		value.Operation = &operation
	}
	if err := value.Validate(); err != nil {
		return app.ApplicationInstall{}, err
	}
	return value, nil
}

func checkPendingApplicationInstall(ctx context.Context, tx pgx.Tx, hostID string) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM host_application_installs
  WHERE host_id=$1 AND status IN ('unknown','accepted','running')
)`, hostID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending Host application install: %w", err)
	}
	if pending {
		return domain.NewError(domain.CodeConflict, "application install outcome is not confirmed")
	}
	return nil
}
