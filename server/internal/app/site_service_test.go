package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	sitedomain "morde/server/internal/domain/site"
	"morde/server/internal/domain/workspace"
)

type siteServiceStoreStub struct {
	site             sitedomain.Site
	sites            []sitedomain.Site
	releases         map[sitedomain.ReleaseID]sitedomain.Release
	listOwnerID      string
	listPage         PageRequest
	listReleasesSite sitedomain.ID
	listReleaseOwner string
	listReleasePage  PageRequest
	listReleaseCalls int
	listErr          error
	failureCode      sitedomain.FailureCode
	completeCalls    int
	failureCalls     int
	completeErr      error
	cancelOnComplete bool
	cancelRequest    context.CancelFunc
}

func (s *siteServiceStoreStub) CreateSite(_ context.Context, value sitedomain.Site) (sitedomain.Site, error) {
	if s.site.ID != "" {
		return sitedomain.Site{}, domain.NewError(domain.CodeConflict, "site already exists")
	}
	s.site = value
	return value, nil
}

func (s *siteServiceStoreStub) ListSites(_ context.Context, ownerID string, page PageRequest) (SitePage, error) {
	s.listOwnerID, s.listPage = ownerID, page
	return SitePage{Items: s.sites}, s.listErr
}

func (s *siteServiceStoreStub) ListSiteReleases(_ context.Context, ownerID string, siteID sitedomain.ID, page PageRequest) (SiteReleasePage, error) {
	s.listReleaseOwner, s.listReleasesSite, s.listReleasePage = ownerID, siteID, page
	s.listReleaseCalls++
	return SiteReleasePage{Items: []sitedomain.Release{}}, s.listErr
}

func (s *siteServiceStoreStub) CreateCandidateRelease(_ context.Context, value sitedomain.Release, _, _ int64) (sitedomain.Release, error) {
	if _, exists := s.releases[value.ID]; exists {
		return sitedomain.Release{}, domain.NewError(domain.CodeConflict, "candidate already exists")
	}
	if s.releases == nil {
		s.releases = make(map[sitedomain.ReleaseID]sitedomain.Release)
	}
	s.releases[value.ID] = value
	return value, nil
}

func (s *siteServiceStoreStub) CompleteSiteReleaseMaterialization(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, fileCount, totalBytes int64) (sitedomain.Release, error) {
	if s.cancelRequest != nil {
		s.cancelRequest()
		s.cancelRequest = nil
	}
	if err := ctx.Err(); err != nil {
		return sitedomain.Release{}, err
	}
	value := s.releases[releaseID]
	if value.SiteID != siteID {
		return sitedomain.Release{}, domain.NewError(domain.CodeNotFound, "release not found")
	}
	if s.completeErr != nil {
		if s.cancelOnComplete {
			value.State = sitedomain.ReleaseCancelled
			value.ReservationBytes = 0
			s.releases[releaseID] = value
		}
		return sitedomain.Release{}, s.completeErr
	}
	if value.MaterializedAt == nil {
		now := time.Now().UTC()
		value.MaterializedAt = &now
		value.ArtifactFileCount, value.ArtifactBytes, value.ReservationBytes = fileCount, totalBytes, totalBytes
		value.FailureCode = ""
		s.completeCalls++
	}
	s.releases[releaseID] = value
	return value, nil
}

func (s *siteServiceStoreStub) RecordSiteReleaseMaterializationFailure(_ context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, code sitedomain.FailureCode) (sitedomain.Release, error) {
	value := s.releases[releaseID]
	if value.SiteID != siteID {
		return sitedomain.Release{}, domain.NewError(domain.CodeNotFound, "release not found")
	}
	value.FailureCode = code
	s.failureCode = code
	s.failureCalls++
	s.releases[releaseID] = value
	return value, nil
}

func (s *siteServiceStoreStub) GetSite(_ context.Context, siteID sitedomain.ID) (sitedomain.Site, error) {
	if s.site.ID != siteID {
		return sitedomain.Site{}, domain.NewError(domain.CodeNotFound, "site not found")
	}
	return s.site, nil
}

func (s *siteServiceStoreStub) GetSiteRelease(_ context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	value, exists := s.releases[releaseID]
	if !exists || value.SiteID != siteID {
		return sitedomain.Release{}, domain.NewError(domain.CodeNotFound, "release not found")
	}
	return value, nil
}

func TestSiteServiceCreationIsOwnerScopedAndIdempotent(t *testing.T) {
	store := &siteServiceStoreStub{releases: make(map[sitedomain.ReleaseID]sitedomain.Release)}
	service := newTestSiteService(t, store, siteManifestPages{}, siteMaterializerBlocks{})
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}

	created, err := service.Create(context.Background(), principal, "h1234567", "w1234567", "create-site-001")
	if err != nil || created.ID != sitedomain.ID(idempotentSiteID(principal.UserID, "site.create", "create-site-001")) {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	replayed, err := service.Create(context.Background(), principal, "h1234567", "w1234567", "create-site-001")
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent Create() = %#v, %v", replayed, err)
	}
	if _, err := service.Create(context.Background(), principal, "h7654321", "w1234567", "create-site-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("Create() with reused key and changed request error = %v", err)
	}
	device := principal
	device.Kind = identity.KindDevice
	if _, err := service.Create(context.Background(), device, "h1234567", "w1234567", "create-site-002"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Create() with device credential error = %v", err)
	}
}

func TestSiteServiceListsSitesAndOwnerScopedReleases(t *testing.T) {
	store := testSiteServiceStore()
	service := newTestSiteService(t, store, siteManifestPages{}, siteMaterializerBlocks{})
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	page := PageRequest{Limit: 7, Cursor: "opaque-cursor"}

	sites, err := service.List(context.Background(), principal, page)
	if err != nil || len(sites.Items) != 0 || store.listOwnerID != principal.UserID || store.listPage != page {
		t.Fatalf("List() = %#v, %v; owner=%q page=%#v", sites, err, store.listOwnerID, store.listPage)
	}

	if _, err := service.ListReleases(context.Background(), principal, store.site.ID, page); err != nil ||
		store.listReleaseCalls != 1 || store.listReleaseOwner != principal.UserID || store.listReleasesSite != store.site.ID || store.listReleasePage != page {
		t.Fatalf("ListReleases() = %v; calls=%d owner=%q site=%q page=%#v", err, store.listReleaseCalls, store.listReleaseOwner, store.listReleasesSite, store.listReleasePage)
	}

	other := principal
	other.UserID = "u7654321"
	if _, err := service.ListReleases(context.Background(), other, store.site.ID, page); domain.CodeOf(err) != domain.CodeNotFound || store.listReleaseCalls != 1 {
		t.Fatalf("ListReleases() by non-owner error=%v calls=%d; want hidden site and no release query", err, store.listReleaseCalls)
	}
	device := principal
	device.Kind = identity.KindDevice
	if _, err := service.List(context.Background(), device, page); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("List() by device credential error=%v; want session-only", err)
	}
}

func TestSiteServiceCandidatePinsRevisionAndIdempotencyKey(t *testing.T) {
	store := testSiteServiceStore()
	service := newTestSiteService(t, store, siteManifestPages{}, siteMaterializerBlocks{})
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	first, err := service.CreateCandidate(context.Background(), principal, store.site.ID, 4, 3000, "release-create-01")
	if err != nil || first.State != sitedomain.ReleaseCandidate || first.WorkspaceRevision != 4 {
		t.Fatalf("CreateCandidate() = %#v, %v", first, err)
	}
	replayed, err := service.CreateCandidate(context.Background(), principal, store.site.ID, 4, 3000, "release-create-01")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("idempotent CreateCandidate() = %#v, %v", replayed, err)
	}
	if _, err := service.CreateCandidate(context.Background(), principal, store.site.ID, 5, 3000, "release-create-01"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("CreateCandidate() with changed revision error = %v", err)
	}
	other := principal
	other.UserID = "u7654321"
	if _, err := service.CreateCandidate(context.Background(), other, store.site.ID, 4, 3000, "release-create-02"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("CreateCandidate() by non-owner error = %v", err)
	}
	if _, err := service.GetRelease(context.Background(), other, store.site.ID, first.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("GetRelease() by non-owner error = %v", err)
	}
}

func TestSiteServiceMaterializationStaysCandidateAndCanRetry(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	digest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	blockDigest := file.Chunks[0].SHA256
	blocks := siteMaterializerBlocks{blockDigest: []byte("hello")}
	store := testSiteServiceStore()
	release := siteRelease(digest)
	store.releases[release.ID] = release
	artifacts := &fakeSiteArtifactStore{}
	service := newTestSiteServiceWithArtifacts(t, store, pages, blocks, artifacts)
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}

	completed, err := service.Materialize(context.Background(), principal, store.site.ID, release.ID)
	if err != nil || completed.MaterializedAt == nil || completed.State != sitedomain.ReleaseCandidate || completed.HealthCheckedAt != nil {
		t.Fatalf("Materialize() = %#v, %v", completed, err)
	}
	retried, err := service.Materialize(context.Background(), principal, store.site.ID, release.ID)
	if err != nil || retried.MaterializedAt == nil || store.completeCalls != 1 || artifacts.beginCount != 1 {
		t.Fatalf("idempotent Materialize() = %#v, %v; completes=%d begins=%d", retried, err, store.completeCalls, artifacts.beginCount)
	}
}

func TestSiteServiceReconcilesPublishedArtifactAfterRequestCancellation(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	digest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	blocks := siteMaterializerBlocks{file.Chunks[0].SHA256: []byte("hello")}
	store := testSiteServiceStore()
	release := siteRelease(digest)
	store.releases[release.ID] = release
	artifacts := &fakeSiteArtifactStore{}
	service := newTestSiteServiceWithArtifacts(t, store, pages, blocks, artifacts)
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.cancelRequest = cancel

	completed, err := service.Materialize(ctx, principal, store.site.ID, release.ID)
	if err != nil || completed.MaterializedAt == nil || store.completeCalls != 1 || !artifacts.writer.committed {
		t.Fatalf("Materialize() after artifact publication and request cancellation = %#v, %v; completes=%d committed=%t", completed, err, store.completeCalls, artifacts.writer.committed)
	}
}

func TestSiteServiceRemovesUnaccountedArtifactAfterReleaseCancellation(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	digest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	blocks := siteMaterializerBlocks{file.Chunks[0].SHA256: []byte("hello")}
	store := testSiteServiceStore()
	release := siteRelease(digest)
	store.releases[release.ID] = release
	store.completeErr = domain.NewError(domain.CodeConflict, "Site is draining")
	store.cancelOnComplete = true
	artifacts := &fakeSiteArtifactStore{}
	service := newTestSiteServiceWithArtifacts(t, store, pages, blocks, artifacts)
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}

	if _, err := service.Materialize(context.Background(), principal, store.site.ID, release.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("Materialize() after concurrent cancellation error = %v", err)
	}
	if artifacts.deleteCall != 1 || artifacts.deleted[0] != string(store.site.ID)+"/"+string(release.ID)+"/"+digest {
		t.Fatalf("cancelled release cleanup = %v, calls=%d", artifacts.deleted, artifacts.deleteCall)
	}
	if store.failureCalls != 0 {
		t.Fatalf("cancelled release was incorrectly marked as a materialization failure: %d", store.failureCalls)
	}
}

func TestSiteServicePreservesArtifactWhenCompletionStateIsUncertain(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	digest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	blocks := siteMaterializerBlocks{file.Chunks[0].SHA256: []byte("hello")}
	store := testSiteServiceStore()
	release := siteRelease(digest)
	store.releases[release.ID] = release
	store.completeErr = domain.NewError(domain.CodeConflict, "temporary completion failure")
	artifacts := &fakeSiteArtifactStore{}
	service := newTestSiteServiceWithArtifacts(t, store, pages, blocks, artifacts)
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}

	if _, err := service.Materialize(context.Background(), principal, store.site.ID, release.ID); err == nil {
		t.Fatal("Materialize() with completion failure succeeded")
	}
	if artifacts.deleteCall != 0 || store.failureCalls != 1 {
		t.Fatalf("uncertain candidate handling: deletes=%d failures=%d", artifacts.deleteCall, store.failureCalls)
	}
}

func TestSiteServiceMaterializationPersistsOnlySafeFailureAndSkipsCancellation(t *testing.T) {
	file := siteFile("index.html", 0644, "hello")
	digest, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	pages := siteManifestPages{"": {WorkspaceID: siteMaterializerWorkspaceID, Revision: 4, Files: []workspace.File{file}}}
	store := testSiteServiceStore()
	release := siteRelease(digest)
	store.releases[release.ID] = release
	service := newTestSiteService(t, store, pages, siteMaterializerBlocks{})
	principal := identity.Principal{UserID: store.site.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}

	if _, err := service.Materialize(context.Background(), principal, store.site.ID, release.ID); err == nil {
		t.Fatal("Materialize() with a missing block succeeded")
	}
	if store.failureCode != sitedomain.FailureStorageUnavailable || store.failureCalls != 1 {
		t.Fatalf("persisted failure code = %q, calls=%d", store.failureCode, store.failureCalls)
	}
	if value := store.releases[release.ID]; value.State != sitedomain.ReleaseCandidate || value.FailureCode != sitedomain.FailureStorageUnavailable {
		t.Fatalf("failed attempt changed candidate state or stored error: %#v", value)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Materialize(ctx, principal, store.site.ID, release.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Materialize() error = %v", err)
	}
	if store.failureCalls != 1 {
		t.Fatalf("cancelled attempt persisted failure, calls=%d", store.failureCalls)
	}
}

func newTestSiteService(t *testing.T, store SiteStore, pages SiteRevisionManifestReader, blocks WorkspaceBlockReader) *SiteService {
	t.Helper()
	return newTestSiteServiceWithArtifacts(t, store, pages, blocks, &fakeSiteArtifactStore{})
}

func newTestSiteServiceWithArtifacts(t *testing.T, store SiteStore, pages SiteRevisionManifestReader, blocks WorkspaceBlockReader, artifacts SiteArtifactStore) *SiteService {
	t.Helper()
	service, err := NewSiteService(store, pages, blocks, artifacts, 1<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testSiteServiceStore() *siteServiceStoreStub {
	return &siteServiceStoreStub{
		site:     sitedomain.Site{ID: "site_0123456789abcdef", OwnerID: "u1234567", HostID: "h1234567", WorkspaceID: siteMaterializerWorkspaceID, State: sitedomain.StateActive},
		releases: make(map[sitedomain.ReleaseID]sitedomain.Release),
	}
}
