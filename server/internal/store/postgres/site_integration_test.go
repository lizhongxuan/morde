package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testSiteOwnerMaxBytes = 1 << 40

func TestSiteListsAreOwnerScopedAndCursorOrdered(t *testing.T) {
	store, _, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	firstSite, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	secondSite, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}

	foreignOwner := createTestUser(t, store, 3, 20_000, 40_000)
	foreignWorkspace := createTestWorkspace(t, ctx, store, foreignOwner.ID, "site-list-foreign")
	foreignHost := createRunningSiteTestHost(t, ctx, store, foreignOwner.ID, foreignWorkspace.ID)
	foreignSite, err := store.CreateSite(ctx, newTestSite(t, foreignOwner.ID, foreignHost, foreignWorkspace.ID))
	if err != nil {
		t.Fatal(err)
	}

	allSites, err := store.ListSites(ctx, ownerID, app.PageRequest{Limit: 100})
	if err != nil || len(allSites.Items) != 2 || allSites.NextCursor != "" {
		t.Fatalf("ListSites(owner) = %#v, %v", allSites, err)
	}
	for _, value := range allSites.Items {
		if value.OwnerID != ownerID {
			t.Fatalf("ListSites leaked another owner's Site: %#v", value)
		}
	}
	firstPage, err := store.ListSites(ctx, ownerID, app.PageRequest{Limit: 1})
	if err != nil || len(firstPage.Items) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first Site page = %#v, %v", firstPage, err)
	}
	secondPage, err := store.ListSites(ctx, ownerID, app.PageRequest{Limit: 1, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Items) != 1 || secondPage.NextCursor != "" || firstPage.Items[0].ID == secondPage.Items[0].ID {
		t.Fatalf("second Site page = %#v, %v", secondPage, err)
	}

	for i, value := range []sitedomain.Site{firstSite, firstSite, secondSite} {
		candidate := newTestCandidate(t, value.ID, workspaceID)
		if _, err := store.CreateCandidateRelease(ctx, candidate, 1<<20, testSiteOwnerMaxBytes); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := store.FailCandidateRelease(ctx, value.ID, candidate.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	firstRelease := newTestCandidate(t, foreignSite.ID, foreignWorkspace.ID)
	if _, err := store.CreateCandidateRelease(ctx, firstRelease, 1<<20, testSiteOwnerMaxBytes); err != nil {
		t.Fatal(err)
	}

	releases, err := store.ListSiteReleases(ctx, ownerID, firstSite.ID, app.PageRequest{Limit: 1})
	if err != nil || len(releases.Items) != 1 || releases.NextCursor == "" {
		t.Fatalf("first Site release page = %#v, %v", releases, err)
	}
	releasePage2, err := store.ListSiteReleases(ctx, ownerID, firstSite.ID, app.PageRequest{Limit: 1, Cursor: releases.NextCursor})
	if err != nil || len(releasePage2.Items) != 1 || releasePage2.NextCursor != "" || releases.Items[0].ID == releasePage2.Items[0].ID {
		t.Fatalf("second Site release page = %#v, %v", releasePage2, err)
	}
	foreignReleases, err := store.ListSiteReleases(ctx, ownerID, foreignSite.ID, app.PageRequest{Limit: 10})
	if err != nil || len(foreignReleases.Items) != 0 || foreignReleases.Items == nil {
		t.Fatalf("foreign Site release query = %#v, %v; want an empty non-null page", foreignReleases, err)
	}
}

func TestSiteReleasePersistenceLifecycle(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()

	siteValue := newTestSite(t, ownerID, hostID, workspaceID)
	created, err := store.CreateSite(ctx, siteValue)
	if err != nil || created.ID != siteValue.ID || created.State != sitedomain.StateActive ||
		created.CurrentReleaseID != "" || created.RollbackReleaseID != "" {
		t.Fatalf("CreateSite() = %#v, %v", created, err)
	}

	first := newTestCandidate(t, created.ID, workspaceID)
	first, err = store.CreateCandidateRelease(ctx, first, 1<<20, testSiteOwnerMaxBytes)
	if err != nil || first.State != sitedomain.ReleaseCandidate {
		t.Fatalf("CreateCandidateRelease(first) = %#v, %v", first, err)
	}
	if _, _, err := store.PromoteCandidateRelease(ctx, created.ID, first.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promotion before materialization error = %v", err)
	}
	if _, err := store.RecordSiteReleaseHealthCheckSuccess(ctx, created.ID, first.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("health check before materialization error = %v", err)
	}
	first = completeEmptySiteRelease(t, store, ctx, created.ID, first.ID)
	if _, err := pool.Exec(ctx, `UPDATE site_releases SET state='healthy' WHERE site_id=$1 AND id=$2`, created.ID, first.ID); err == nil {
		t.Fatal("database accepted a healthy release without health-check evidence")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("missing health-check evidence failed for an unexpected reason: %v", err)
		}
	}
	if _, _, err := store.PromoteCandidateRelease(ctx, created.ID, first.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promotion without health check error = %v", err)
	}
	first, err = store.RecordSiteReleaseHealthCheckSuccess(ctx, created.ID, first.ID)
	if err != nil || first.HealthCheckedAt == nil {
		t.Fatalf("RecordSiteReleaseHealthCheckSuccess(first) = %#v, %v", first, err)
	}
	checkedAt := *first.HealthCheckedAt
	retriedCheck, err := store.RecordSiteReleaseHealthCheckSuccess(ctx, created.ID, first.ID)
	if err != nil || retriedCheck.HealthCheckedAt == nil || !retriedCheck.HealthCheckedAt.Equal(checkedAt) {
		t.Fatalf("idempotent RecordSiteReleaseHealthCheckSuccess() = %#v, %v", retriedCheck, err)
	}
	current, first, err := store.PromoteCandidateRelease(ctx, created.ID, first.ID)
	if err != nil || current.CurrentReleaseID != first.ID || current.RollbackReleaseID != "" || first.State != sitedomain.ReleaseHealthy {
		t.Fatalf("PromoteCandidateRelease(first) = site %#v release %#v, %v", current, first, err)
	}

	failed := newTestCandidate(t, created.ID, workspaceID)
	failed, err = store.CreateCandidateRelease(ctx, failed, 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = store.FailCandidateRelease(ctx, created.ID, failed.ID)
	if err != nil || failed.State != sitedomain.ReleaseFailed {
		t.Fatalf("FailCandidateRelease() = %#v, %v", failed, err)
	}
	storedSite, err := store.GetSite(ctx, created.ID)
	if err != nil || storedSite.CurrentReleaseID != first.ID || storedSite.RollbackReleaseID != "" {
		t.Fatalf("failed candidate changed release pointers: site %#v, %v", storedSite, err)
	}
	storedFailed, err := store.GetSiteRelease(ctx, created.ID, failed.ID)
	if err != nil || storedFailed.State != sitedomain.ReleaseFailed {
		t.Fatalf("read failed candidate = %#v, %v", storedFailed, err)
	}

	second := newTestCandidate(t, created.ID, workspaceID)
	second, err = store.CreateCandidateRelease(ctx, second, 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	second = completeEmptySiteRelease(t, store, ctx, created.ID, second.ID)
	if _, err := store.RecordSiteReleaseHealthCheckSuccess(ctx, created.ID, second.ID); err != nil {
		t.Fatalf("RecordSiteReleaseHealthCheckSuccess(second) = %v", err)
	}
	current, second, err = store.PromoteCandidateRelease(ctx, created.ID, second.ID)
	if err != nil || current.CurrentReleaseID != second.ID || current.RollbackReleaseID != first.ID || second.State != sitedomain.ReleaseHealthy {
		t.Fatalf("PromoteCandidateRelease(second) = site %#v release %#v, %v", current, second, err)
	}

	rolledBack, err := store.RollbackSiteRelease(ctx, created.ID, first.ID)
	if err != nil || rolledBack.CurrentReleaseID != first.ID || rolledBack.RollbackReleaseID != second.ID {
		t.Fatalf("RollbackSiteRelease(first) = %#v, %v", rolledBack, err)
	}
	if _, err := store.RollbackSiteRelease(ctx, created.ID, failed.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("RollbackSiteRelease(failed, non-target) error = %v", err)
	}
	if _, err := store.RollbackSiteRelease(ctx, created.ID, first.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("RollbackSiteRelease(current, non-target) error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sites SET rollback_release_id=current_release_id WHERE id=$1`, created.ID); err == nil {
		t.Fatal("database accepted equal non-null current and rollback release pointers")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("equal non-null release pointers failed for an unexpected reason: %v", err)
		}
	}

	var releaseCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_releases WHERE site_id=$1`, created.ID).Scan(&releaseCount); err != nil || releaseCount != 3 {
		t.Fatalf("persisted release count = %d, %v", releaseCount, err)
	}
}

func TestCreateCandidateReleaseComputesWorkspaceManifestDigest(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	siteValue := newTestSite(t, ownerID, hostID, workspaceID)
	siteValue, err := store.CreateSite(ctx, siteValue)
	if err != nil {
		t.Fatal(err)
	}
	emptySHA256 := sha256.Sum256(nil)
	file := workspace.File{Path: "index.html", SizeBytes: 0, SHA256: hex.EncodeToString(emptySHA256[:]), Mode: 0644, Chunks: []workspace.Chunk{}}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id, valid_from_revision, file_path, folded_path, size_bytes, sha256, mode, chunks)
VALUES ($1, 1, $2, $2, 0, $3, 420, '[]'::jsonb)`, workspaceID, file.Path, file.SHA256); err != nil {
		t.Fatalf("insert immutable Workspace revision file: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_revisions SET file_count=1 WHERE workspace_id=$1 AND revision=1`, workspaceID); err != nil {
		t.Fatalf("update Workspace revision manifest count: %v", err)
	}
	expectedDigest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}

	candidate := newTestCandidate(t, siteValue.ID, workspaceID)
	created, err := store.CreateCandidateRelease(ctx, candidate, 1<<20, testSiteOwnerMaxBytes)
	if err != nil || created.ManifestSHA256 != expectedDigest {
		t.Fatalf("CreateCandidateRelease with server-computed digest = %#v, %v", created, err)
	}
}

func TestCreateCandidateReleaseReservesOwnerQuotaAtomically(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	content := []byte("blog")
	_ = setSiteTestRevisionFile(t, pool, workspaceID, content)

	firstSite, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace := createTestWorkspace(t, ctx, store, ownerID, "site-quota-other")
	otherHostID := createRunningSiteTestHost(t, ctx, store, ownerID, otherWorkspace.ID)
	_ = setSiteTestRevisionFile(t, pool, otherWorkspace.ID, content)
	secondSite, err := store.CreateSite(ctx, newTestSite(t, ownerID, otherHostID, otherWorkspace.ID))
	if err != nil {
		t.Fatal(err)
	}

	first := newTestCandidate(t, firstSite.ID, workspaceID)
	second := newTestCandidate(t, secondSite.ID, otherWorkspace.ID)
	start := make(chan struct{})
	results := make(chan struct {
		requested sitedomain.Release
		created   sitedomain.Release
		err       error
	}, 2)
	for _, candidate := range []sitedomain.Release{first, second} {
		candidate := candidate
		go func() {
			<-start
			created, err := store.CreateCandidateRelease(ctx, candidate, 1<<20, 7)
			results <- struct {
				requested sitedomain.Release
				created   sitedomain.Release
				err       error
			}{candidate, created, err}
		}()
	}
	close(start)
	var accepted, rejected sitedomain.Release
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			accepted = result.created
		} else if domain.CodeOf(result.err) == domain.CodeQuotaExceeded {
			rejected = result.requested
		} else {
			t.Fatalf("concurrent candidate creation error = %v", result.err)
		}
	}
	if accepted.ID == "" || rejected.ID == "" || accepted.SiteID == rejected.SiteID {
		t.Fatalf("owner quota admitted an invalid candidate set: accepted=%#v rejected=%#v", accepted, rejected)
	}
	var reservedCount int
	var reservedBytes int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(reservation_bytes), 0) FROM site_releases
WHERE state='candidate' AND materialized_at IS NULL`).Scan(&reservedCount, &reservedBytes); err != nil || reservedCount != 1 || reservedBytes != int64(len(content)) {
		t.Fatalf("active quota reservations = (%d, %d), %v", reservedCount, reservedBytes, err)
	}
	if _, err := store.FailCandidateRelease(ctx, accepted.SiteID, accepted.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := store.CreateCandidateRelease(ctx, rejected, 1<<20, 7)
	if err != nil {
		t.Fatalf("candidate after failed reservation release: %v", err)
	}
	if _, err := store.CompleteSiteReleaseMaterialization(ctx, rejected.SiteID, retried.ID, 1, int64(len(content))); err != nil {
		t.Fatalf("materialize reserved Site release: %v", err)
	}
	thirdSite, err := store.CreateSite(ctx, newTestSite(t, ownerID, otherHostID, otherWorkspace.ID))
	if err != nil {
		t.Fatal(err)
	}
	third := newTestCandidate(t, thirdSite.ID, otherWorkspace.ID)
	if _, err := store.CreateCandidateRelease(ctx, third, 1<<20, 7); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("materialized release was not charged to owner quota: %v", err)
	}
}

func TestCreateCandidateReleaseRejectsOversizedRevisionBeforeReservingQuota(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	content := []byte("oversized")
	_ = setSiteTestRevisionFile(t, pool, workspaceID, content)
	siteValue, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	candidate := newTestCandidate(t, siteValue.ID, workspaceID)

	if _, err := store.CreateCandidateRelease(ctx, candidate, int64(len(content)-1), 1<<20); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("oversized candidate error = %v", err)
	}
	var count int
	var reservedBytes int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(reservation_bytes), 0) FROM site_releases WHERE site_id=$1`, siteValue.ID).Scan(&count, &reservedBytes); err != nil || count != 0 || reservedBytes != 0 {
		t.Fatalf("oversized candidate reserved quota: count=%d bytes=%d err=%v", count, reservedBytes, err)
	}
	if _, err := store.CreateCandidateRelease(ctx, candidate, int64(len(content)), 1<<20); err != nil {
		t.Fatalf("same candidate could not be created after correcting its limit: %v", err)
	}
}

func TestCompleteSiteReleaseMaterializationVerifiesPinnedRevisionAndRetriesIdempotently(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	siteValue, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.CreateCandidateRelease(ctx, newTestCandidate(t, siteValue.ID, workspaceID), 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE site_releases SET state='healthy' WHERE site_id=$1 AND id=$2`, siteValue.ID, candidate.ID); err == nil {
		t.Fatal("database accepted a healthy release without materialization evidence")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || (pgErr.ConstraintName != "site_releases_materialization_check" && pgErr.ConstraintName != "site_releases_health_check_evidence") {
			t.Fatalf("unmaterialized release was rejected for an unexpected reason: %v", err)
		}
	}
	if _, err := store.CompleteSiteReleaseMaterialization(ctx, siteValue.ID, candidate.ID, 1, 0); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("materialization with mismatched file count error = %v", err)
	}
	if stored, err := store.GetSiteRelease(ctx, siteValue.ID, candidate.ID); err != nil || stored.MaterializedAt != nil {
		t.Fatalf("mismatched materialization changed release: %#v, %v", stored, err)
	}
	completed := completeEmptySiteRelease(t, store, ctx, siteValue.ID, candidate.ID)
	if completed.MaterializedAt == nil || completed.ArtifactFileCount != 0 || completed.ArtifactBytes != 0 {
		t.Fatalf("completed release summary = %#v", completed)
	}
	retried, err := store.CompleteSiteReleaseMaterialization(ctx, siteValue.ID, candidate.ID, 0, 0)
	if err != nil || retried.MaterializedAt == nil {
		t.Fatalf("idempotent materialization retry = %#v, %v", retried, err)
	}
	if _, err := store.CompleteSiteReleaseMaterialization(ctx, siteValue.ID, candidate.ID, 1, 0); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("materialization retry with different summary error = %v", err)
	}
}

func TestSiteReleaseMaterializationFailureCodeIsBoundedAndClearedOnSuccess(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	siteValue, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.CreateCandidateRelease(ctx, newTestCandidate(t, siteValue.ID, workspaceID), 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}

	failed, err := store.RecordSiteReleaseMaterializationFailure(ctx, siteValue.ID, candidate.ID, sitedomain.FailureSourceUnavailable)
	if err != nil || failed.FailureCode != sitedomain.FailureSourceUnavailable || failed.State != sitedomain.ReleaseCandidate {
		t.Fatalf("RecordSiteReleaseMaterializationFailure() = %#v, %v", failed, err)
	}
	if _, err := store.RecordSiteReleaseMaterializationFailure(ctx, siteValue.ID, candidate.ID, sitedomain.FailureCode("raw error text")); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unapproved failure code error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE site_releases SET failure_code='raw error text' WHERE site_id=$1 AND id=$2`, siteValue.ID, candidate.ID); err == nil {
		t.Fatal("database accepted unapproved materialization failure text")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "site_releases_failure_code_check" {
			t.Fatalf("failure-code constraint rejected data unexpectedly: %v", err)
		}
	}

	completed, err := store.CompleteSiteReleaseMaterialization(ctx, siteValue.ID, candidate.ID, 0, 0)
	if err != nil || completed.FailureCode != "" || completed.MaterializedAt == nil || completed.State != sitedomain.ReleaseCandidate {
		t.Fatalf("successful materialization did not clear failure: %#v, %v", completed, err)
	}
}

func TestCreateCandidateReleaseRejectsInvalidWorkspaceRevisionManifest(t *testing.T) {
	emptySHA256 := sha256.Sum256(nil)
	fileSHA256 := hex.EncodeToString(emptySHA256[:])
	tests := []struct {
		name       string
		fileCount  int64
		totalBytes int64
		chunks     string
	}{
		{name: "file count mismatch", fileCount: 0, chunks: `[]`},
		{name: "byte count mismatch", fileCount: 1, totalBytes: 1, chunks: `[]`},
		{name: "invalid file chunks", fileCount: 1, chunks: `[{"sha256":"bad","size_bytes":1}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
			ctx := context.Background()
			siteValue, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id, valid_from_revision, file_path, folded_path, size_bytes, sha256, mode, chunks)
VALUES ($1, 1, 'index.html', 'index.html', 0, $2, 420, $3::jsonb)`, workspaceID, fileSHA256, test.chunks); err != nil {
				t.Fatalf("insert Workspace revision file: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workspace_revisions SET file_count=$2, total_bytes=$3
WHERE workspace_id=$1 AND revision=1`, workspaceID, test.fileCount, test.totalBytes); err != nil {
				t.Fatalf("set Workspace revision manifest metadata: %v", err)
			}

			candidate := newTestCandidate(t, siteValue.ID, workspaceID)
			if _, err := store.CreateCandidateRelease(ctx, candidate, 1<<20, testSiteOwnerMaxBytes); domain.CodeOf(err) != domain.CodeInternal {
				t.Fatalf("CreateCandidateRelease() error = %v, want stored-manifest error", err)
			}
			var releaseCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM site_releases WHERE site_id=$1`, siteValue.ID).Scan(&releaseCount); err != nil || releaseCount != 0 {
				t.Fatalf("release rows after rejected candidate = %d, %v; want 0", releaseCount, err)
			}
		})
	}
}

func TestSiteReleaseRejectsNonRoutableHostServicePortsInDatabase(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()
	siteValue, err := store.CreateSite(ctx, newTestSite(t, ownerID, hostID, workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.CreateCandidateRelease(ctx, newTestCandidate(t, siteValue.ID, workspaceID), 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{22, 80, 1023, 8080, 5900, 6080, 8090, 8091, 8100, 9222} {
		_, err := pool.Exec(ctx, `UPDATE site_releases SET service_port=$3 WHERE site_id=$1 AND id=$2`, siteValue.ID, candidate.ID, port)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "site_releases_service_port_policy_check" {
			t.Errorf("database accepted non-routable service port %d or failed unexpectedly: %v", port, err)
		}
	}
}

func TestHostSiteDrainSerializesRegistrationAndBlocksPromotion(t *testing.T) {
	store, _, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()

	existing := newTestSite(t, ownerID, hostID, workspaceID)
	existing, err := store.CreateSite(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	candidate := newTestCandidate(t, existing.ID, workspaceID)
	candidate, err = store.CreateCandidateRelease(ctx, candidate, 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	candidate = completeEmptySiteRelease(t, store, ctx, existing.ID, candidate.ID)
	if _, err := store.RecordSiteReleaseHealthCheckSuccess(ctx, existing.ID, candidate.ID); err != nil {
		t.Fatalf("RecordSiteReleaseHealthCheckSuccess(concurrent candidate) = %v", err)
	}

	concurrent := newTestSite(t, ownerID, hostID, workspaceID)
	start := make(chan struct{})
	createResult := make(chan error, 1)
	promoteResult := make(chan struct {
		site    sitedomain.Site
		release sitedomain.Release
		err     error
	}, 1)
	releaseResult := make(chan struct {
		value   host.Host
		changed bool
		err     error
	}, 1)
	go func() {
		<-start
		_, err := store.CreateSite(ctx, concurrent)
		createResult <- err
	}()
	go func() {
		<-start
		promotedSite, promotedRelease, err := store.PromoteCandidateRelease(ctx, existing.ID, candidate.ID)
		promoteResult <- struct {
			site    sitedomain.Site
			release sitedomain.Release
			err     error
		}{promotedSite, promotedRelease, err}
	}()
	go func() {
		<-start
		value, changed, err := store.BeginHostRelease(ctx, hostID, false)
		releaseResult <- struct {
			value   host.Host
			changed bool
			err     error
		}{value, changed, err}
	}()
	close(start)

	createErr := <-createResult
	promotion := <-promoteResult
	release := <-releaseResult
	if release.err != nil || !release.changed || release.value.State != host.StateStopping {
		t.Fatalf("BeginHostRelease() = %#v, changed=%t, err=%v", release.value, release.changed, release.err)
	}
	if domain.CodeOf(createErr) != domain.CodeConflict && createErr != nil {
		t.Fatalf("concurrent CreateSite() error = %v", createErr)
	}
	occupancy, err := store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || !occupancy.DrainInProgress || !occupancy.Occupied() || !containsSiteID(occupancy.SiteIDs, existing.ID) {
		t.Fatalf("Host occupancy after release admission = %#v, %v", occupancy, err)
	}
	if createErr == nil && !containsSiteID(occupancy.SiteIDs, concurrent.ID) {
		t.Fatalf("release missed a site committed before its Host lock: %#v", occupancy)
	}
	if createErr != nil && containsSiteID(occupancy.SiteIDs, concurrent.ID) {
		t.Fatalf("rejected site appears in release occupancy: %#v", occupancy)
	}
	if promotion.err != nil && domain.CodeOf(promotion.err) != domain.CodeConflict {
		t.Fatalf("concurrent PromoteCandidateRelease() error = %v", promotion.err)
	}
	if promotion.err == nil && (promotion.site.CurrentReleaseID != candidate.ID || promotion.release.State != sitedomain.ReleaseHealthy) {
		t.Fatalf("promotion that won the Host lock was not committed before drain: %#v %#v", promotion.site, promotion.release)
	}

	if _, _, err := store.PromoteCandidateRelease(ctx, existing.ID, candidate.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promotion while Host is stopping error = %v", err)
	}
	newCandidate := newTestCandidate(t, existing.ID, workspaceID)
	if _, err := store.CreateCandidateRelease(ctx, newCandidate, 1<<20, testSiteOwnerMaxBytes); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("candidate creation while Host is stopping error = %v", err)
	}
	if _, changed, err := store.BeginHostRelease(ctx, hostID, false); err != nil || changed {
		t.Fatalf("retry BeginHostRelease() while stopping: changed=%t err=%v", changed, err)
	}
	if _, err := store.CompleteHostRelease(ctx, hostID, 2); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("CompleteHostRelease with stale generation error = %v", err)
	}
	occupancy, err = store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || !occupancy.DrainInProgress || len(occupancy.SiteIDs) == 0 {
		t.Fatalf("failed release completion must retain drain barrier: %#v, %v", occupancy, err)
	}
	if stored, err := store.GetSite(ctx, existing.ID); err != nil || stored.State != sitedomain.StateDraining {
		t.Fatalf("site state after failed completion = %#v, %v", stored, err)
	}
	wantCandidateBeforeCompletion := sitedomain.ReleaseCandidate
	wantCandidateState := sitedomain.ReleaseCancelled
	wantCurrentRelease := sitedomain.ReleaseID("")
	if promotion.err == nil {
		wantCandidateBeforeCompletion = sitedomain.ReleaseHealthy
		wantCandidateState = sitedomain.ReleaseHealthy
		wantCurrentRelease = candidate.ID
	}
	if stored, err := store.GetSiteRelease(ctx, existing.ID, candidate.ID); err != nil || stored.State != wantCandidateBeforeCompletion {
		t.Fatalf("candidate state after failed completion = %#v, want %q, err=%v", stored, wantCandidateBeforeCompletion, err)
	}

	if _, err := store.CompleteHostRelease(ctx, hostID, 1); err != nil {
		t.Fatalf("CompleteHostRelease() after sandbox cleanup retry: %v", err)
	}
	occupancy, err = store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || occupancy.DrainInProgress || occupancy.Occupied() {
		t.Fatalf("GetHostSiteOccupancy() after completed drain = %#v, %v", occupancy, err)
	}
	storedSite, err := store.GetSite(ctx, existing.ID)
	if err != nil || storedSite.State != sitedomain.StateStopped || storedSite.CurrentReleaseID != wantCurrentRelease {
		t.Fatalf("site after drain = %#v, %v", storedSite, err)
	}
	storedCandidate, err := store.GetSiteRelease(ctx, existing.ID, candidate.ID)
	if err != nil || storedCandidate.State != wantCandidateState {
		t.Fatalf("candidate after drain = %#v, %v", storedCandidate, err)
	}
	if storedSite.CurrentReleaseID != wantCurrentRelease {
		t.Fatalf("current release after drain = %q, want %q", storedSite.CurrentReleaseID, wantCurrentRelease)
	}
}

func TestHostReleaseDeferralsDoNotStartSiteDrain(t *testing.T) {
	store, pool, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()

	siteValue := newTestSite(t, ownerID, hostID, workspaceID)
	siteValue, err := store.CreateSite(ctx, siteValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_state='user' WHERE id=$1`, hostID); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release while user-controlled = %#v, changed=%t, err=%v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostID, false)
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_state='ai' WHERE id=$1`, hostID); err != nil {
		t.Fatal(err)
	}

	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id) VALUES ($1,$2,$3)`, conversationID, ownerID, hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO tasks (id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind,
                   budget_active_seconds, budget_tool_calls, budget_model_tokens)
VALUES ($1,$2,$3,$4,'model-site-test','active work','queued','session',60,10,1000)`, taskID, ownerID, conversationID, hostID); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release with active task = %#v, changed=%t, err=%v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostID, false)
	if value, err := store.GetSite(ctx, siteValue.ID); err != nil || value.State != sitedomain.StateActive {
		t.Fatalf("site changed during deferred release = %#v, %v", value, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostID, false); err != nil || !changed || value.State != host.StateStopping {
		t.Fatalf("release after task completed = %#v, changed=%t, err=%v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostID, true)
	if value, err := store.GetSite(ctx, siteValue.ID); err != nil || value.State != sitedomain.StateDraining {
		t.Fatalf("site after release accepted = %#v, %v", value, err)
	}
	if _, err := store.CompleteHostRelease(ctx, hostID, 1); err != nil {
		t.Fatalf("complete accepted release: %v", err)
	}
}

func assertHostSiteDrainState(t *testing.T, store *Store, ctx context.Context, hostID string, want bool) {
	t.Helper()
	var exists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_site_drains WHERE host_id=$1)`, hostID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != want {
		t.Fatalf("Host drain barrier exists=%t, want %t", exists, want)
	}
}

func TestHostDeletionDrainPersistsUntilDeletedTransition(t *testing.T) {
	store, _, ownerID, hostID, workspaceID := createSiteTestFixture(t)
	ctx := context.Background()

	siteValue := newTestSite(t, ownerID, hostID, workspaceID)
	siteValue, err := store.CreateSite(ctx, siteValue)
	if err != nil {
		t.Fatal(err)
	}
	candidate := newTestCandidate(t, siteValue.ID, workspaceID)
	candidate, err = store.CreateCandidateRelease(ctx, candidate, 1<<20, testSiteOwnerMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := detachSiteFixtureWorkspace(ctx, store, hostID, workspaceID); err != nil {
		t.Fatal(err)
	}

	deleting, err := store.TransitionHost(ctx, hostID, host.StateRunning, host.StateDeleting, "")
	if err != nil || deleting.State != host.StateDeleting {
		t.Fatalf("TransitionHost(running, deleting) = %#v, %v", deleting, err)
	}
	occupancy, err := store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || !occupancy.DrainInProgress || !containsSiteID(occupancy.SiteIDs, siteValue.ID) {
		t.Fatalf("Host occupancy after delete admission = %#v, %v", occupancy, err)
	}

	newSite := newTestSite(t, ownerID, hostID, workspaceID)
	if _, err := store.CreateSite(ctx, newSite); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("CreateSite while Host is deleting error = %v", err)
	}
	if _, _, err := store.PromoteCandidateRelease(ctx, siteValue.ID, candidate.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("PromoteCandidateRelease while Host is deleting error = %v", err)
	}
	if stored, err := store.GetSite(ctx, siteValue.ID); err != nil || stored.State != sitedomain.StateDraining {
		t.Fatalf("site while external cleanup is pending = %#v, %v", stored, err)
	}
	if stored, err := store.GetSiteRelease(ctx, siteValue.ID, candidate.ID); err != nil || stored.State != sitedomain.ReleaseCandidate {
		t.Fatalf("candidate while external cleanup is pending = %#v, %v", stored, err)
	}

	// No deleted transition is issued while the external sandbox/volume cleanup
	// is considered failed; the committed barrier must remain in force for retry.
	occupancy, err = store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || !occupancy.DrainInProgress || !occupancy.Occupied() {
		t.Fatalf("Host occupancy during delete retry = %#v, %v", occupancy, err)
	}

	// This state transition is the database completion boundary. The production
	// worker calls it only after sandbox and volume cleanup has succeeded.
	deleted, err := store.TransitionHost(ctx, hostID, host.StateDeleting, host.StateDeleted, "")
	if err != nil || deleted.State != host.StateDeleted {
		t.Fatalf("TransitionHost(deleting, deleted) after cleanup = %#v, %v", deleted, err)
	}
	occupancy, err = store.GetHostSiteOccupancy(ctx, hostID)
	if err != nil || occupancy.DrainInProgress || occupancy.Occupied() {
		t.Fatalf("Host occupancy after delete completion = %#v, %v", occupancy, err)
	}
	storedSite, err := store.GetSite(ctx, siteValue.ID)
	if err != nil || storedSite.State != sitedomain.StateStopped {
		t.Fatalf("site after delete completion = %#v, %v", storedSite, err)
	}
	storedCandidate, err := store.GetSiteRelease(ctx, siteValue.ID, candidate.ID)
	if err != nil || storedCandidate.State != sitedomain.ReleaseCancelled {
		t.Fatalf("candidate after delete completion = %#v, %v", storedCandidate, err)
	}
}

func detachSiteFixtureWorkspace(ctx context.Context, store *Store, hostID, workspaceID string) error {
	binding, err := store.GetWorkspaceBinding(ctx, hostID, workspaceID)
	if err != nil {
		return err
	}
	unbinding, err := store.TransitionWorkspaceBinding(ctx, hostID, workspaceID, binding.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, binding.DesiredRevision, binding.AppliedRevision)
	if err != nil {
		return err
	}
	_, err = store.TransitionWorkspaceBinding(ctx, hostID, workspaceID, unbinding.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, unbinding.DesiredRevision, unbinding.AppliedRevision)
	return err
}

func createSiteTestFixture(t *testing.T) (*Store, *pgxpool.Pool, string, string, string) {
	t.Helper()
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "site-test")
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-site-test"
	if err := store.SetSandboxID(ctx, created.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, 1, 0, "", "runtime-site-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, created.ID, workspaceValue.ID, 1, workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes) VALUES ($1, 1, 0, 0)`, workspaceValue.ID); err != nil {
		t.Fatalf("create immutable Workspace revision for release: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET revision=1 WHERE id=$1`, workspaceValue.ID); err != nil {
		t.Fatalf("advance Workspace revision for release: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=1, applied_revision=1 WHERE host_id=$1 AND workspace_id=$2`, created.ID, workspaceValue.ID); err != nil {
		t.Fatalf("prepare immutable Workspace revision for release: %v", err)
	}
	return store, pool, owner.ID, created.ID, workspaceValue.ID
}

func createRunningSiteTestHost(t *testing.T, ctx context.Context, store *Store, ownerID, workspaceID string) string {
	t.Helper()
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, ownerID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceID}))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-site-quota-test"
	if err := store.SetSandboxID(ctx, created.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, 1, 0, "", "runtime-site-quota-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, created.ID, workspaceID, 1, workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes) VALUES ($1, 1, 0, 0)`, workspaceID); err != nil {
		t.Fatalf("create Workspace revision for quota Host: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1 WHERE id=$1`, workspaceID); err != nil {
		t.Fatalf("advance Workspace revision for quota Host: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET desired_revision=1, applied_revision=1 WHERE host_id=$1 AND workspace_id=$2`, created.ID, workspaceID); err != nil {
		t.Fatalf("prepare Workspace revision for quota Host: %v", err)
	}
	return created.ID
}

func setSiteTestRevisionFile(t *testing.T, pool *pgxpool.Pool, workspaceID string, content []byte) string {
	t.Helper()
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	file := workspace.File{
		Path: "index.html", SizeBytes: int64(len(content)), SHA256: digest, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: int64(len(content))}},
	}
	encodedChunks, err := json.Marshal(file.Chunks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE workspace_revisions SET file_count=1, total_bytes=$2 WHERE workspace_id=$1 AND revision=1`, workspaceID, len(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO workspace_revision_files
(workspace_id, valid_from_revision, file_path, folded_path, size_bytes, sha256, mode, chunks)
VALUES ($1, 1, 'index.html', 'index.html', $2, $3, 420, $4::jsonb)`, workspaceID, len(content), digest, encodedChunks); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	return manifestSHA
}

func newTestSite(t *testing.T, ownerID, hostID, workspaceID string) sitedomain.Site {
	t.Helper()
	id, err := sitedomain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return sitedomain.Site{ID: id, OwnerID: ownerID, HostID: hostID, WorkspaceID: workspaceID, State: sitedomain.StateActive}
}

func newTestCandidate(t *testing.T, siteID sitedomain.ID, workspaceID string) sitedomain.Release {
	t.Helper()
	id, err := sitedomain.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	return sitedomain.Release{
		ID: id, SiteID: siteID, WorkspaceID: workspaceID, WorkspaceRevision: 1,
		ServicePort: 3000, State: sitedomain.ReleaseCandidate,
	}
}

func completeEmptySiteRelease(t *testing.T, store *Store, ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) sitedomain.Release {
	t.Helper()
	completed, err := store.CompleteSiteReleaseMaterialization(ctx, siteID, releaseID, 0, 0)
	if err != nil {
		t.Fatalf("CompleteSiteReleaseMaterialization() = %v", err)
	}
	return completed
}

func containsSiteID(values []sitedomain.ID, target sitedomain.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
