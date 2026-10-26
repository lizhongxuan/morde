package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const siteColumns = `id, owner_id, host_id, workspace_id, state, current_release_id, rollback_release_id, created_at, updated_at`
const releaseColumns = `site_id, id, workspace_id, source_revision, manifest_sha256, service_port, state, reservation_bytes, artifact_file_count, artifact_bytes, materialized_at, health_checked_at, failure_code, created_at, updated_at`

// CreateSite persists one owner-owned site on a running Host with an active
// Workspace binding. The Host row lock serializes registration with drain.
func (s *Store) CreateSite(ctx context.Context, value sitedomain.Site) (sitedomain.Site, error) {
	if err := value.Validate(); err != nil {
		return sitedomain.Site{}, err
	}
	if value.State != sitedomain.StateActive || value.CurrentReleaseID != "" || value.RollbackReleaseID != "" {
		return sitedomain.Site{}, domain.InvalidArgument("site", "new site fields are invalid")
	}
	var created sitedomain.Site
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockSiteHost(ctx, tx, value.HostID, value.OwnerID, true); err != nil {
			return err
		}
		draining, err := hostSiteDrainExists(ctx, tx, value.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		if err := requireActiveWorkspaceBinding(ctx, tx, value.HostID, value.OwnerID, value.WorkspaceID); err != nil {
			return err
		}
		query := `INSERT INTO sites (id, owner_id, host_id, workspace_id, state)
VALUES ($1, $2, $3, $4, 'active') RETURNING ` + siteColumns
		row := tx.QueryRow(ctx, query, value.ID, value.OwnerID, value.HostID, value.WorkspaceID)
		created, err = scanSite(row)
		if err != nil {
			return siteWriteError("create site", err)
		}
		return nil
	})
	if err != nil {
		return sitedomain.Site{}, err
	}
	return created, nil
}

// CreateCandidateRelease pins the immutable source revision and computed
// digest while reserving storage; it does not execute Workspace code.
func (s *Store) CreateCandidateRelease(ctx context.Context, value sitedomain.Release, maxReleaseBytes, maxOwnerBytes int64) (sitedomain.Release, error) {
	if err := value.ValidateCandidateRequest(); err != nil || maxReleaseBytes <= 0 || maxOwnerBytes <= 0 {
		return sitedomain.Release{}, domain.InvalidArgument("site.release", "new candidate fields are invalid")
	}
	var created sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		siteValue, hostState, err := lockSiteHostForUpdate(ctx, tx, value.SiteID)
		if err != nil {
			return err
		}
		if hostState != "running" || siteValue.State != sitedomain.StateActive {
			return siteConflict("site is not accepting candidate releases")
		}
		draining, err := hostSiteDrainExists(ctx, tx, siteValue.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		if value.WorkspaceID != siteValue.WorkspaceID {
			return siteConflict("candidate source does not match site Workspace")
		}
		if err := requireActiveWorkspaceBinding(ctx, tx, siteValue.HostID, siteValue.OwnerID, siteValue.WorkspaceID); err != nil {
			return err
		}
		manifestSHA256, revisionBytes, err := workspaceRevisionManifestSHA256(ctx, tx, value.WorkspaceID, value.WorkspaceRevision)
		if err != nil {
			return err
		}
		value.ManifestSHA256 = manifestSHA256
		if err := value.Validate(); err != nil {
			return err
		}
		if revisionBytes > maxReleaseBytes {
			return domain.NewError(domain.CodeCapacityExceeded, "Site release exceeds its configured byte limit")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('site-owner:' || $1::text, 0))`, siteValue.OwnerID); err != nil {
			return fmt.Errorf("lock Site storage owner quota: %w", err)
		}
		var usedBytes int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE
  WHEN r.materialized_at IS NOT NULL THEN r.artifact_bytes
  WHEN r.state = 'candidate' THEN r.reservation_bytes
  ELSE 0
END), 0)
FROM site_releases r
JOIN sites s ON s.id = r.site_id
WHERE s.owner_id = $1`, siteValue.OwnerID).Scan(&usedBytes); err != nil {
			return fmt.Errorf("read Site storage owner quota: %w", err)
		}
		if revisionBytes > maxOwnerBytes-usedBytes {
			return domain.NewError(domain.CodeQuotaExceeded, "Site storage owner quota exceeded")
		}
		query := `INSERT INTO site_releases (site_id, id, workspace_id, source_revision, manifest_sha256, service_port, state, reservation_bytes)
VALUES ($1, $2, $3, $4, $5, $6, 'candidate', $7) RETURNING ` + releaseColumns
		created, err = scanRelease(tx.QueryRow(ctx, query,
			value.SiteID, value.ID, value.WorkspaceID, value.WorkspaceRevision, value.ManifestSHA256, value.ServicePort, revisionBytes,
		))
		if err != nil {
			return siteWriteError("create candidate release", err)
		}
		return nil
	})
	if err != nil {
		return sitedomain.Release{}, err
	}
	return created, nil
}

func workspaceRevisionManifestSHA256(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) (string, int64, error) {
	var expectedFileCount, expectedBytes int64
	if err := tx.QueryRow(ctx, `SELECT file_count, total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`,
		workspaceID, revision).Scan(&expectedFileCount, &expectedBytes); errors.Is(err, pgx.ErrNoRows) {
		return "", 0, domain.NewError(domain.CodeNotFound, "workspace revision not found")
	} else if err != nil {
		return "", 0, fmt.Errorf("read Workspace revision manifest metadata: %w", err)
	}
	if expectedFileCount > workspace.MaxSyncFiles {
		return "", 0, domain.NewError(domain.CodeRequestTooLarge, "workspace revision manifest exceeds the file limit")
	}
	if expectedFileCount < 0 || expectedBytes < 0 {
		return "", 0, domain.NewError(domain.CodeInternal, "stored Workspace revision metadata is invalid")
	}

	rows, err := tx.Query(ctx, `
SELECT file_path, size_bytes, sha256, mode, chunks
FROM workspace_revision_files
WHERE workspace_id=$1 AND valid_from_revision <= $2
  AND (valid_to_revision IS NULL OR valid_to_revision > $2)
ORDER BY file_path`, workspaceID, revision)
	if err != nil {
		return "", 0, fmt.Errorf("read Workspace revision manifest: %w", err)
	}
	defer rows.Close()
	var fileCount int64
	var totalBytes int64
	var readErr error
	digest, err := workspace.ManifestSHA256Stream(func(yield func(workspace.File) error) error {
		for rows.Next() {
			var path, digest string
			var size int64
			var mode int32
			var chunks []byte
			if err := rows.Scan(&path, &size, &digest, &mode, &chunks); err != nil {
				readErr = fmt.Errorf("scan Workspace revision manifest: %w", err)
				return readErr
			}
			file, err := workspaceFileFromRow(path, size, digest, mode, chunks)
			if err != nil {
				readErr = domain.NewError(domain.CodeInternal, "stored Workspace revision manifest is invalid")
				return readErr
			}
			if size > expectedBytes-totalBytes {
				readErr = domain.NewError(domain.CodeInternal, "stored Workspace revision byte count is invalid")
				return readErr
			}
			totalBytes += size
			fileCount++
			if err := yield(file); err != nil {
				readErr = domain.NewError(domain.CodeInternal, "stored Workspace revision manifest is invalid")
				return readErr
			}
		}
		if err := rows.Err(); err != nil {
			readErr = fmt.Errorf("iterate Workspace revision manifest: %w", err)
			return readErr
		}
		if fileCount != expectedFileCount || totalBytes != expectedBytes {
			readErr = domain.NewError(domain.CodeInternal, "stored Workspace revision manifest count is invalid")
			return readErr
		}
		return nil
	})
	if err != nil {
		if readErr != nil {
			return "", 0, readErr
		}
		return "", 0, domain.NewError(domain.CodeInternal, "stored Workspace revision manifest is invalid")
	}
	return digest, expectedBytes, nil
}

// CompleteSiteReleaseMaterialization records only a fully verified immutable artifact.
func (s *Store) CompleteSiteReleaseMaterialization(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, fileCount, totalBytes int64) (sitedomain.Release, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) || fileCount < 0 || totalBytes < 0 {
		return sitedomain.Release{}, domain.InvalidArgument("site.materialization", "materialization identity or size is invalid")
	}
	var updated sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		siteValue, hostState, err := lockSiteHostForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		release, err := getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		if err != nil {
			return err
		}
		if release.MaterializedAt != nil {
			if release.ArtifactFileCount != fileCount || release.ArtifactBytes != totalBytes {
				return siteConflict("materialized Site release summary does not match the recorded artifact")
			}
			updated = release
			return nil
		}
		if hostState != "running" || siteValue.State != sitedomain.StateActive || release.State != sitedomain.ReleaseCandidate {
			return siteConflict("Site release is not accepting materialization")
		}
		draining, err := hostSiteDrainExists(ctx, tx, siteValue.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		if err := requireActiveWorkspaceBinding(ctx, tx, siteValue.HostID, siteValue.OwnerID, siteValue.WorkspaceID); err != nil {
			return err
		}
		var expectedFiles, expectedBytes int64
		if err := tx.QueryRow(ctx, `SELECT file_count, total_bytes FROM workspace_revisions WHERE workspace_id=$1 AND revision=$2`,
			release.WorkspaceID, release.WorkspaceRevision).Scan(&expectedFiles, &expectedBytes); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace revision not found")
		} else if err != nil {
			return fmt.Errorf("read Site artifact source summary: %w", err)
		}
		if expectedFiles != fileCount || expectedBytes != totalBytes || release.ReservationBytes != totalBytes {
			return siteConflict("materialized Site release does not match its pinned Workspace revision")
		}
		tag, err := tx.Exec(ctx, `UPDATE site_releases SET artifact_file_count=$3, artifact_bytes=$4, materialized_at=now(), failure_code=NULL, updated_at=now()
WHERE site_id=$1 AND id=$2 AND state='candidate' AND materialized_at IS NULL`, siteID, releaseID, fileCount, totalBytes)
		if err != nil {
			return siteWriteError("complete Site release materialization", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("Site release changed before materialization was recorded")
		}
		updated, err = getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		return err
	})
	if err != nil {
		return sitedomain.Release{}, err
	}
	return updated, nil
}

// RecordSiteReleaseMaterializationFailure stores only an approved error class.
// The candidate remains retryable and no caller-provided error text is kept.
func (s *Store) RecordSiteReleaseMaterializationFailure(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, code sitedomain.FailureCode) (sitedomain.Release, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) || !sitedomain.ValidFailureCode(code) || code == "" {
		return sitedomain.Release{}, domain.InvalidArgument("site.materialization", "materialization failure is invalid")
	}
	var updated sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		release, err := getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		if err != nil {
			return err
		}
		if release.MaterializedAt != nil {
			updated = release
			return nil
		}
		if release.State != sitedomain.ReleaseCandidate {
			return siteConflict("Site release is not retryable")
		}
		query := `UPDATE site_releases SET failure_code=$3, updated_at=now()
WHERE site_id=$1 AND id=$2 AND state='candidate' AND materialized_at IS NULL RETURNING ` + releaseColumns
		updated, err = scanRelease(tx.QueryRow(ctx, query, siteID, releaseID, code))
		return err
	})
	if err != nil {
		return sitedomain.Release{}, err
	}
	return updated, nil
}

// RecordSiteReleaseHealthCheckSuccess records a completed bounded probe by a
// trusted internal caller. It is not exposed through the public API.
func (s *Store) RecordSiteReleaseHealthCheckSuccess(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) {
		return sitedomain.Release{}, domain.InvalidArgument("site.health_check", "health check identity is invalid")
	}
	var updated sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		siteValue, hostState, err := lockSiteHostForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		release, err := getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		if err != nil {
			return err
		}
		if release.HealthCheckedAt != nil {
			updated = release
			return nil
		}
		if hostState != "running" || siteValue.State != sitedomain.StateActive || release.State != sitedomain.ReleaseCandidate || release.MaterializedAt == nil {
			return siteConflict("Site release is not eligible for a health check")
		}
		draining, err := hostSiteDrainExists(ctx, tx, siteValue.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		if err := requireActiveWorkspaceBinding(ctx, tx, siteValue.HostID, siteValue.OwnerID, siteValue.WorkspaceID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE site_releases SET health_checked_at=now(), updated_at=now()
WHERE site_id=$1 AND id=$2 AND state='candidate' AND materialized_at IS NOT NULL AND health_checked_at IS NULL`, siteID, releaseID)
		if err != nil {
			return siteWriteError("record Site release health check", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("Site release changed before its health check was recorded")
		}
		updated, err = getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		return err
	})
	if err != nil {
		return sitedomain.Release{}, err
	}
	return updated, nil
}

// PromoteCandidateRelease changes the release and both site pointers in one
// transaction. A recorded candidate health check is required before promotion.
func (s *Store) PromoteCandidateRelease(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Site, sitedomain.Release, error) {
	var updatedSite sitedomain.Site
	var updatedRelease sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		siteValue, hostState, err := lockSiteHostForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		if hostState != "running" || siteValue.State != sitedomain.StateActive {
			return siteConflict("site is not accepting candidate releases")
		}
		draining, err := hostSiteDrainExists(ctx, tx, siteValue.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		release, err := getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		if err != nil {
			return err
		}
		updatedSite, updatedRelease, err = siteValue.PromoteCandidate(release)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE site_releases SET state='healthy', updated_at=now()
WHERE site_id=$1 AND id=$2 AND state='candidate'`, siteID, releaseID)
		if err != nil {
			return fmt.Errorf("mark candidate release healthy: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("candidate release changed before promotion")
		}
		var rollback any
		if updatedSite.RollbackReleaseID != "" {
			rollback = string(updatedSite.RollbackReleaseID)
		}
		tag, err = tx.Exec(ctx, `UPDATE sites SET current_release_id=$2, rollback_release_id=$3, updated_at=now()
WHERE id=$1 AND state='active'`, siteID, updatedSite.CurrentReleaseID, rollback)
		if err != nil {
			return fmt.Errorf("switch current site release: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("site changed before candidate promotion")
		}
		updatedSite, err = getSiteForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		updatedRelease, err = getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		return err
	})
	if err != nil {
		return sitedomain.Site{}, sitedomain.Release{}, err
	}
	return updatedSite, updatedRelease, nil
}

// FailCandidateRelease is safe to call after the builder or health check
// reports failure; it never changes the current or rollback pointers.
func (s *Store) FailCandidateRelease(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	var updated sitedomain.Release
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockSiteHostForUpdate(ctx, tx, siteID); err != nil {
			return err
		}
		siteValue, err := getSiteForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		release, err := getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		if err != nil {
			return err
		}
		_, failed, err := siteValue.FailCandidate(release)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE site_releases SET state='failed',
reservation_bytes=CASE WHEN materialized_at IS NULL THEN 0 ELSE reservation_bytes END, updated_at=now()
WHERE site_id=$1 AND id=$2 AND state='candidate'`, siteID, releaseID)
		if err != nil {
			return fmt.Errorf("fail candidate release: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("candidate release changed before failure was recorded")
		}
		updated = failed
		updated, err = getSiteReleaseForUpdate(ctx, tx, siteID, releaseID)
		return err
	})
	if err != nil {
		return sitedomain.Release{}, err
	}
	return updated, nil
}

// RollbackSiteRelease accepts only the site's persisted rollback target and
// swaps the two healthy pointers atomically.
func (s *Store) RollbackSiteRelease(ctx context.Context, siteID sitedomain.ID, targetID sitedomain.ReleaseID) (sitedomain.Site, error) {
	var updated sitedomain.Site
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		siteValue, hostState, err := lockSiteHostForUpdate(ctx, tx, siteID)
		if err != nil {
			return err
		}
		if hostState != "running" || siteValue.State != sitedomain.StateActive {
			return siteConflict("site is not accepting rollback")
		}
		draining, err := hostSiteDrainExists(ctx, tx, siteValue.HostID)
		if err != nil {
			return err
		}
		if draining {
			return siteConflict("Host is draining sites")
		}
		target, err := getSiteReleaseForUpdate(ctx, tx, siteID, targetID)
		if err != nil {
			return err
		}
		updated, err = siteValue.Rollback(target)
		if err != nil {
			return err
		}
		var rollback any
		if updated.RollbackReleaseID != "" {
			rollback = string(updated.RollbackReleaseID)
		}
		tag, err := tx.Exec(ctx, `UPDATE sites SET current_release_id=$2, rollback_release_id=$3, updated_at=now()
WHERE id=$1 AND state='active'`, siteID, updated.CurrentReleaseID, rollback)
		if err != nil {
			return fmt.Errorf("switch site rollback target: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return siteConflict("site changed before rollback")
		}
		updated, err = getSiteForUpdate(ctx, tx, siteID)
		return err
	})
	if err != nil {
		return sitedomain.Site{}, err
	}
	return updated, nil
}

// beginHostSiteDrain is called only while the Host row is locked. Every
// registration, release, and deletion path uses this Host-to-Site lock order.
func beginHostSiteDrain(ctx context.Context, tx pgx.Tx, hostID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO host_site_drains (host_id) VALUES ($1) ON CONFLICT (host_id) DO NOTHING`, hostID); err != nil {
		return fmt.Errorf("record Host site drain: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sites SET state='draining', updated_at=now()
WHERE host_id=$1 AND state='active'`, hostID); err != nil {
		return fmt.Errorf("mark Host sites draining: %w", err)
	}
	return nil
}

// completeHostSiteDrain runs only after the caller has confirmed Host sandbox
// deletion. The enclosing transaction also commits the Host terminal state.
func completeHostSiteDrain(ctx context.Context, tx pgx.Tx, hostID string) error {
	if err := beginHostSiteDrain(ctx, tx, hostID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT `+siteColumns+` FROM sites WHERE host_id=$1 ORDER BY id FOR UPDATE`, hostID)
	if err != nil {
		return fmt.Errorf("lock Host sites for drain completion: %w", err)
	}
	var values []sitedomain.Site
	for rows.Next() {
		value, err := scanSite(rows)
		if err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list Host sites for drain completion: %w", err)
	}
	rows.Close()

	for _, value := range values {
		if value.State != sitedomain.StateStopped {
			if value.State == sitedomain.StateActive {
				value, err = value.BeginDrain()
				if err != nil {
					return err
				}
			}
			stopped, err := value.ConfirmStopped()
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE sites SET state='stopped', updated_at=now() WHERE id=$1 AND state <> 'stopped'`, value.ID)
			if err != nil {
				return fmt.Errorf("record stopped site after Host release: %w", err)
			}
			if tag.RowsAffected() != 1 || stopped.State != sitedomain.StateStopped {
				return siteConflict("site changed before Host drain completion")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE site_releases SET state='cancelled',
reservation_bytes=CASE WHEN materialized_at IS NULL THEN 0 ELSE reservation_bytes END, updated_at=now()
WHERE site_id=$1 AND state='candidate'`, value.ID); err != nil {
			return fmt.Errorf("cancel candidate releases after Host release: %w", err)
		}
	}
	occupancy, err := hostSiteOccupancy(ctx, tx, hostID)
	if err != nil {
		return err
	}
	if occupancy.Occupied() {
		return siteConflict("Host still has sites awaiting drain")
	}
	tag, err := tx.Exec(ctx, `DELETE FROM host_site_drains WHERE host_id=$1`, hostID)
	if err != nil {
		return fmt.Errorf("complete Host site drain: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return siteConflict("Host site drain is not active")
	}
	return nil
}

// GetHostSiteOccupancy returns the durable drain decision facts for a Host.
func (s *Store) GetHostSiteOccupancy(ctx context.Context, hostID string) (sitedomain.HostOccupancy, error) {
	if !domain.ValidHostID(hostID) {
		return sitedomain.HostOccupancy{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	var result sitedomain.HostOccupancy
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockSiteHost(ctx, tx, hostID, "", false); err != nil {
			return err
		}
		var err error
		result, err = hostSiteOccupancy(ctx, tx, hostID)
		return err
	})
	if err != nil {
		return sitedomain.HostOccupancy{}, err
	}
	return result, nil
}

func (s *Store) GetSite(ctx context.Context, siteID sitedomain.ID) (sitedomain.Site, error) {
	if !sitedomain.ValidID(siteID) {
		return sitedomain.Site{}, domain.InvalidArgument("site_id", "site id is invalid")
	}
	return scanSite(s.pool.QueryRow(ctx, `SELECT `+siteColumns+` FROM sites WHERE id=$1`, siteID))
}

func (s *Store) ListSites(ctx context.Context, ownerID string, page app.PageRequest) (app.SitePage, error) {
	if !domain.ValidUserID(ownerID) {
		return app.SitePage{}, domain.InvalidArgument("owner_id", "owner identity is invalid")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.SitePage{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+siteColumns+`
FROM sites
WHERE owner_id=$1 AND (NOT $2::boolean OR (created_at,id)<($3,$4))
ORDER BY created_at DESC,id DESC LIMIT $5`, ownerID, cursorAt.Valid, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.SitePage{}, fmt.Errorf("list sites: %w", err)
	}
	values := make([]sitedomain.Site, 0)
	for rows.Next() {
		value, err := scanSite(rows)
		if err != nil {
			rows.Close()
			return app.SitePage{}, fmt.Errorf("scan site: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.SitePage{}, fmt.Errorf("iterate sites: %w", err)
	}
	rows.Close()
	result := app.SitePage{Items: values}
	result.NextCursor = nextPageCursor(values, limit, func(value sitedomain.Site) (string, time.Time) { return string(value.ID), value.CreatedAt })
	if int32(len(values)) > limit {
		result.Items = values[:limit]
	}
	return result, nil
}

func (s *Store) ListSiteReleases(ctx context.Context, ownerID string, siteID sitedomain.ID, page app.PageRequest) (app.SiteReleasePage, error) {
	if !domain.ValidUserID(ownerID) || !sitedomain.ValidID(siteID) {
		return app.SiteReleasePage{}, domain.InvalidArgument("site_id", "site identity is invalid")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.SiteReleasePage{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT r.site_id,r.id,r.workspace_id,r.source_revision,r.manifest_sha256,r.service_port,r.state,r.reservation_bytes,r.artifact_file_count,r.artifact_bytes,r.materialized_at,r.health_checked_at,r.failure_code,r.created_at,r.updated_at
FROM sites s
JOIN site_releases r ON r.site_id=s.id
WHERE s.owner_id=$1 AND s.id=$2 AND (NOT $3::boolean OR (r.created_at,r.id)<($4,$5))
ORDER BY r.created_at DESC,r.id DESC LIMIT $6`, ownerID, siteID, cursorAt.Valid, cursorAt, cursorID, limit+1)
	if err != nil {
		return app.SiteReleasePage{}, fmt.Errorf("list site releases: %w", err)
	}
	values := make([]sitedomain.Release, 0)
	for rows.Next() {
		value, err := scanRelease(rows)
		if err != nil {
			rows.Close()
			return app.SiteReleasePage{}, fmt.Errorf("scan site release: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.SiteReleasePage{}, fmt.Errorf("iterate site releases: %w", err)
	}
	rows.Close()
	result := app.SiteReleasePage{Items: values}
	result.NextCursor = nextPageCursor(values, limit, func(value sitedomain.Release) (string, time.Time) { return string(value.ID), value.CreatedAt })
	if int32(len(values)) > limit {
		result.Items = values[:limit]
	}
	return result, nil
}

func (s *Store) GetSiteRelease(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	if !sitedomain.ValidID(siteID) || !sitedomain.ValidReleaseID(releaseID) {
		return sitedomain.Release{}, domain.InvalidArgument("release_id", "release identity is invalid")
	}
	return scanRelease(s.pool.QueryRow(ctx, `SELECT `+releaseColumns+` FROM site_releases WHERE site_id=$1 AND id=$2`, siteID, releaseID))
}

func lockSiteHost(ctx context.Context, tx pgx.Tx, hostID, ownerID string, requireRunning bool) (string, string, error) {
	if !domain.ValidHostID(hostID) {
		return "", "", domain.InvalidArgument("host_id", "host id is invalid")
	}
	var actualOwner, state string
	var deletedAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT owner_id, state, deleted_at FROM hosts WHERE id=$1 FOR UPDATE`, hostID).
		Scan(&actualOwner, &state, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return "", "", fmt.Errorf("lock Host for site transaction: %w", err)
	}
	if ownerID != "" && actualOwner != ownerID {
		return "", "", domain.NewError(domain.CodeNotFound, "host not found")
	}
	if requireRunning && (deletedAt.Valid || state != "running") {
		return actualOwner, state, siteConflict("Host is not available for site changes")
	}
	return actualOwner, state, nil
}

func lockSiteHostForUpdate(ctx context.Context, tx pgx.Tx, siteID sitedomain.ID) (sitedomain.Site, string, error) {
	if !sitedomain.ValidID(siteID) {
		return sitedomain.Site{}, "", domain.InvalidArgument("site_id", "site id is invalid")
	}
	var hostID string
	if err := tx.QueryRow(ctx, `SELECT host_id FROM sites WHERE id=$1`, siteID).Scan(&hostID); errors.Is(err, pgx.ErrNoRows) {
		return sitedomain.Site{}, "", domain.NewError(domain.CodeNotFound, "site not found")
	} else if err != nil {
		return sitedomain.Site{}, "", fmt.Errorf("read Host for site lock: %w", err)
	}
	_, hostState, err := lockSiteHost(ctx, tx, hostID, "", false)
	if err != nil {
		return sitedomain.Site{}, "", err
	}
	siteValue, err := getSiteForUpdate(ctx, tx, siteID)
	if err != nil {
		return sitedomain.Site{}, "", err
	}
	return siteValue, hostState, nil
}

func getSiteForUpdate(ctx context.Context, tx pgx.Tx, siteID sitedomain.ID) (sitedomain.Site, error) {
	return scanSite(tx.QueryRow(ctx, `SELECT `+siteColumns+` FROM sites WHERE id=$1 FOR UPDATE`, siteID))
}

func getSiteReleaseForUpdate(ctx context.Context, tx pgx.Tx, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	if !sitedomain.ValidReleaseID(releaseID) {
		return sitedomain.Release{}, domain.InvalidArgument("release_id", "release id is invalid")
	}
	return scanRelease(tx.QueryRow(ctx, `SELECT `+releaseColumns+` FROM site_releases WHERE site_id=$1 AND id=$2 FOR UPDATE`, siteID, releaseID))
}

func requireActiveWorkspaceBinding(ctx context.Context, tx pgx.Tx, hostID, ownerID, workspaceID string) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM workspace_bindings wb
  JOIN workspaces w ON w.id=wb.workspace_id AND w.owner_id=wb.owner_id
  WHERE wb.host_id=$1 AND wb.owner_id=$2 AND wb.workspace_id=$3 AND wb.state='active'
    AND w.state='ready' AND w.deleted_at IS NULL
)`, hostID, ownerID, workspaceID).Scan(&active)
	if err != nil {
		return fmt.Errorf("check active Workspace binding for site: %w", err)
	}
	if !active {
		return domain.NewError(domain.CodeConflict, "site Workspace must be actively bound to its Host")
	}
	return nil
}

func hostSiteDrainExists(ctx context.Context, tx pgx.Tx, hostID string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_site_drains WHERE host_id=$1)`, hostID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check Host site drain barrier: %w", err)
	}
	return exists, nil
}

func hostSiteOccupancy(ctx context.Context, tx pgx.Tx, hostID string) (sitedomain.HostOccupancy, error) {
	result := sitedomain.HostOccupancy{HostID: hostID, SiteIDs: []sitedomain.ID{}}
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_site_drains WHERE host_id=$1)`, hostID).Scan(&result.DrainInProgress)
	if err != nil {
		return sitedomain.HostOccupancy{}, fmt.Errorf("read Host site drain barrier: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT id FROM sites WHERE host_id=$1 AND state <> 'stopped' ORDER BY id`, hostID)
	if err != nil {
		return sitedomain.HostOccupancy{}, fmt.Errorf("list Host sites awaiting drain: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return sitedomain.HostOccupancy{}, fmt.Errorf("scan Host site occupancy: %w", err)
		}
		result.SiteIDs = append(result.SiteIDs, sitedomain.ID(id))
	}
	if err := rows.Err(); err != nil {
		return sitedomain.HostOccupancy{}, fmt.Errorf("iterate Host site occupancy: %w", err)
	}
	return result, nil
}

func scanSite(row pgx.Row) (sitedomain.Site, error) {
	var value sitedomain.Site
	var state string
	var current, rollback pgtype.Text
	if err := row.Scan(&value.ID, &value.OwnerID, &value.HostID, &value.WorkspaceID, &state, &current, &rollback, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return sitedomain.Site{}, siteReadError("site", err)
	}
	value.State = sitedomain.State(state)
	if current.Valid {
		value.CurrentReleaseID = sitedomain.ReleaseID(current.String)
	}
	if rollback.Valid {
		value.RollbackReleaseID = sitedomain.ReleaseID(rollback.String)
	}
	if err := value.Validate(); err != nil {
		return sitedomain.Site{}, fmt.Errorf("stored site is invalid: %w", err)
	}
	return value, nil
}

func scanRelease(row pgx.Row) (sitedomain.Release, error) {
	var value sitedomain.Release
	var state string
	var port int32
	var materializedAt, healthCheckedAt pgtype.Timestamptz
	var failureCode pgtype.Text
	if err := row.Scan(&value.SiteID, &value.ID, &value.WorkspaceID, &value.WorkspaceRevision, &value.ManifestSHA256, &port, &state,
		&value.ReservationBytes, &value.ArtifactFileCount, &value.ArtifactBytes, &materializedAt, &healthCheckedAt, &failureCode, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return sitedomain.Release{}, siteReadError("site release", err)
	}
	value.ServicePort = int(port)
	value.State = sitedomain.ReleaseState(state)
	if materializedAt.Valid {
		value.MaterializedAt = &materializedAt.Time
	}
	if healthCheckedAt.Valid {
		value.HealthCheckedAt = &healthCheckedAt.Time
	}
	if failureCode.Valid {
		value.FailureCode = sitedomain.FailureCode(failureCode.String)
	}
	if err := value.Validate(); err != nil {
		return sitedomain.Release{}, fmt.Errorf("stored site release is invalid: %w", err)
	}
	return value, nil
}

func siteReadError(resource string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, resource+" not found")
	}
	return fmt.Errorf("read %s: %w", resource, err)
}

func siteWriteError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return domain.NewError(domain.CodeConflict, "site identity or candidate already exists")
		case "23503":
			return domain.NewError(domain.CodeNotFound, "site source revision or owner resource not found")
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func siteConflict(message string) error { return domain.NewError(domain.CodeConflict, message) }
