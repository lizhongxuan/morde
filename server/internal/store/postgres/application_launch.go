package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

const applicationLaunchSelect = `
SELECT host_id, owner_id, launch_id, application_id, runtime_generation,
       runtime_instance_id, control_version, status, error_code, created_at, updated_at
FROM host_application_launches`

func (s *Store) ReserveApplicationLaunch(ctx context.Context, value app.ApplicationLaunch) (app.ApplicationLaunch, bool, error) {
	if err := value.Validate(); err != nil || value.Status != app.ApplicationLaunchStarting {
		return app.ApplicationLaunch{}, false, domain.InvalidArgument("application_launch", "application launch request is invalid")
	}
	var result app.ApplicationLaunch
	created := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, value.HostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		hostRow, err := queries.GetHost(ctx, value.HostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read Host before application launch reservation: %w", err)
		}
		current := hostDomain(hostRow)
		if current.OwnerID != value.OwnerID || current.DeletedAt != nil {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		existing, err := loadApplicationLaunch(ctx, tx, value.HostID, value.ID, true)
		if err == nil {
			if !sameApplicationLaunchIdentity(existing, value) {
				return applicationLaunchConflict("launch id is already bound to another Host fence")
			}
			result = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing application launch: %w", err)
		}
		var active bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM host_application_launches
  WHERE host_id=$1 AND status IN ('starting','running','stopping','unknown')
)`, value.HostID).Scan(&active); err != nil {
			return fmt.Errorf("check active Host application launch: %w", err)
		}
		if active {
			return applicationLaunchConflict("another application launch is active or unresolved")
		}
		if current.State != host.StateRunning || current.RuntimeGeneration != value.RuntimeGeneration ||
			current.RuntimeInstanceID != value.RuntimeInstanceID || current.ControlVersion != value.ControlVersion ||
			(current.ControlState != host.ControlAI && current.ControlState != host.ControlUser) {
			return domain.NewError(domain.CodeInvalidState, "Host runtime fence changed before application launch")
		}
		_, err = tx.Exec(ctx, `
INSERT INTO host_application_launches (
  host_id, owner_id, launch_id, application_id, runtime_generation,
  runtime_instance_id, control_version, status, error_code
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'')`,
			value.HostID, value.OwnerID, value.ID, value.ApplicationID, value.RuntimeGeneration,
			value.RuntimeInstanceID, value.ControlVersion, app.ApplicationLaunchStarting)
		if err != nil {
			return fmt.Errorf("reserve Host application launch: %w", err)
		}
		result, err = loadApplicationLaunch(ctx, tx, value.HostID, value.ID, false)
		if err != nil {
			return fmt.Errorf("read Host application launch reservation: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return app.ApplicationLaunch{}, false, err
	}
	return result, created, nil
}

func (s *Store) GetLatestApplicationLaunch(ctx context.Context, hostID, ownerID, appID string) (app.ApplicationLaunch, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) || !app.ValidHostApplicationID(appID) {
		return app.ApplicationLaunch{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	value, err := scanApplicationLaunch(s.pool.QueryRow(ctx, applicationLaunchSelect+`
WHERE host_id=$1 AND owner_id=$2 AND application_id=$3
ORDER BY created_at DESC, launch_id DESC LIMIT 1`, hostID, ownerID, appID))
	return value, applicationLaunchReadError("get current application launch", err)
}

func (s *Store) GetApplicationLaunch(ctx context.Context, hostID, ownerID, appID, launchID string) (app.ApplicationLaunch, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(ownerID) || !app.ValidHostApplicationID(appID) || !app.ValidApplicationLaunchID(launchID) {
		return app.ApplicationLaunch{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	value, err := scanApplicationLaunch(s.pool.QueryRow(ctx, applicationLaunchSelect+`
WHERE host_id=$1 AND owner_id=$2 AND application_id=$3 AND launch_id=$4`, hostID, ownerID, appID, launchID))
	return value, applicationLaunchReadError("get application launch", err)
}

func (s *Store) UpdateApplicationLaunch(ctx context.Context, value app.ApplicationLaunch) (app.ApplicationLaunch, error) {
	if err := value.Validate(); err != nil {
		return app.ApplicationLaunch{}, err
	}
	var result app.ApplicationLaunch
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, value.HostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		hostRow, err := queries.GetHost(ctx, value.HostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read Host before application launch update: %w", err)
		}
		currentHost := hostDomain(hostRow)
		stored, err := loadApplicationLaunch(ctx, tx, value.HostID, value.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "application launch not found")
		}
		if err != nil {
			return fmt.Errorf("read application launch before update: %w", err)
		}
		if !sameApplicationLaunchFence(stored, value) {
			return applicationLaunchConflict("application launch fence changed")
		}
		if stored.Status == app.ApplicationLaunchStopped {
			result = stored
			return nil
		}
		if currentHost.OwnerID != value.OwnerID || currentHost.DeletedAt != nil {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		currentGenerationMatches := currentHost.State == host.StateRunning && currentHost.RuntimeGeneration == value.RuntimeGeneration &&
			(currentHost.ControlState == host.ControlAI || currentHost.ControlState == host.ControlUser)
		if !currentGenerationMatches && value.Status != app.ApplicationLaunchUnknown {
			return applicationLaunchConflict("Host runtime fence changed before application launch update")
		}
		if !validApplicationLaunchTransition(stored.Status, value.Status) {
			return domain.NewError(domain.CodeInvalidState, "application launch status transition is invalid")
		}
		_, err = tx.Exec(ctx, `
UPDATE host_application_launches
SET status=$4, error_code=$5, updated_at=clock_timestamp()
WHERE host_id=$1 AND owner_id=$2 AND launch_id=$3`, value.HostID, value.OwnerID, value.ID, value.Status, value.ErrorCode)
		if err != nil {
			return fmt.Errorf("update Host application launch: %w", err)
		}
		result, err = loadApplicationLaunch(ctx, tx, value.HostID, value.ID, false)
		if err != nil {
			return fmt.Errorf("read updated Host application launch: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.ApplicationLaunch{}, err
	}
	return result, nil
}

func loadApplicationLaunch(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, hostID, launchID string, lock bool) (app.ApplicationLaunch, error) {
	query := applicationLaunchSelect + ` WHERE host_id=$1 AND launch_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanApplicationLaunch(queryer.QueryRow(ctx, query, hostID, launchID))
}

func scanApplicationLaunch(row pgx.Row) (app.ApplicationLaunch, error) {
	var value app.ApplicationLaunch
	err := row.Scan(&value.HostID, &value.OwnerID, &value.ID, &value.ApplicationID,
		&value.RuntimeGeneration, &value.RuntimeInstanceID, &value.ControlVersion,
		&value.Status, &value.ErrorCode, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func sameApplicationLaunchFence(left, right app.ApplicationLaunch) bool {
	return left.HostID == right.HostID && left.OwnerID == right.OwnerID && left.ID == right.ID &&
		left.ApplicationID == right.ApplicationID && left.RuntimeGeneration == right.RuntimeGeneration &&
		left.RuntimeInstanceID == right.RuntimeInstanceID && left.ControlVersion == right.ControlVersion
}

func sameApplicationLaunchIdentity(left, right app.ApplicationLaunch) bool {
	return left.HostID == right.HostID && left.OwnerID == right.OwnerID && left.ID == right.ID &&
		left.ApplicationID == right.ApplicationID && left.RuntimeGeneration == right.RuntimeGeneration
}

func validApplicationLaunchTransition(from, to string) bool {
	switch from {
	case app.ApplicationLaunchStarting:
		return to == app.ApplicationLaunchStarting || to == app.ApplicationLaunchRunning || to == app.ApplicationLaunchStopping ||
			to == app.ApplicationLaunchStopped || to == app.ApplicationLaunchExited || to == app.ApplicationLaunchFailed || to == app.ApplicationLaunchUnknown
	case app.ApplicationLaunchRunning:
		return to == app.ApplicationLaunchRunning || to == app.ApplicationLaunchStopping || to == app.ApplicationLaunchStopped ||
			to == app.ApplicationLaunchExited || to == app.ApplicationLaunchFailed || to == app.ApplicationLaunchUnknown
	case app.ApplicationLaunchStopping:
		return to == app.ApplicationLaunchStopping || to == app.ApplicationLaunchStopped || to == app.ApplicationLaunchExited ||
			to == app.ApplicationLaunchFailed || to == app.ApplicationLaunchUnknown
	case app.ApplicationLaunchUnknown:
		return to == app.ApplicationLaunchStarting || to == app.ApplicationLaunchRunning || to == app.ApplicationLaunchStopping ||
			to == app.ApplicationLaunchStopped || to == app.ApplicationLaunchExited || to == app.ApplicationLaunchFailed || to == app.ApplicationLaunchUnknown
	default:
		return from == to
	}
}

func applicationLaunchReadError(action string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

func applicationLaunchConflict(message string) error {
	return domain.NewError(domain.CodeConflict, message)
}

var _ app.ApplicationLaunchStore = (*Store)(nil)
