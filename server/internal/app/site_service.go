package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	sitedomain "morde/server/internal/domain/site"
)

type SiteStore interface {
	CreateSite(context.Context, sitedomain.Site) (sitedomain.Site, error)
	ListSites(context.Context, string, PageRequest) (SitePage, error)
	ListSiteReleases(context.Context, string, sitedomain.ID, PageRequest) (SiteReleasePage, error)
	CreateCandidateRelease(context.Context, sitedomain.Release, int64, int64) (sitedomain.Release, error)
	CompleteSiteReleaseMaterialization(context.Context, sitedomain.ID, sitedomain.ReleaseID, int64, int64) (sitedomain.Release, error)
	RecordSiteReleaseMaterializationFailure(context.Context, sitedomain.ID, sitedomain.ReleaseID, sitedomain.FailureCode) (sitedomain.Release, error)
	GetSite(context.Context, sitedomain.ID) (sitedomain.Site, error)
	GetSiteRelease(context.Context, sitedomain.ID, sitedomain.ReleaseID) (sitedomain.Release, error)
}

type SitePage struct {
	Items      []sitedomain.Site
	NextCursor string
}

type SiteReleasePage struct {
	Items      []sitedomain.Release
	NextCursor string
}

type SiteService struct {
	store       SiteStore
	materialize *SiteRevisionMaterializer
	maxRelease  int64
	maxOwner    int64
}

const siteMaterializationReconcileTimeout = 3 * time.Second

func NewSiteService(store SiteStore, manifest SiteRevisionManifestReader, blocks WorkspaceBlockReader, artifacts SiteArtifactStore, maxReleaseBytes, maxOwnerBytes int64) (*SiteService, error) {
	if store == nil || maxOwnerBytes <= 0 {
		return nil, errors.New("site store and positive owner quota are required")
	}
	materializer, err := NewSiteRevisionMaterializer(manifest, blocks, artifacts, maxReleaseBytes)
	if err != nil {
		return nil, err
	}
	return &SiteService{store: store, materialize: materializer, maxRelease: maxReleaseBytes, maxOwner: maxOwnerBytes}, nil
}

func (s *SiteService) Create(ctx context.Context, principal identity.Principal, hostID, workspaceID, idempotencyKey string) (sitedomain.Site, error) {
	if err := requireSiteSession(principal); err != nil {
		return sitedomain.Site{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return sitedomain.Site{}, err
	}
	id := sitedomain.ID(idempotentSiteID(principal.UserID, "site.create", idempotencyKey))
	value := sitedomain.Site{ID: id, OwnerID: principal.UserID, HostID: hostID, WorkspaceID: workspaceID, State: sitedomain.StateActive}
	created, err := s.store.CreateSite(ctx, value)
	if err == nil {
		return created, nil
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		return sitedomain.Site{}, err
	}
	existing, getErr := s.store.GetSite(ctx, id)
	if getErr != nil {
		return sitedomain.Site{}, err
	}
	if existing.OwnerID != principal.UserID || existing.HostID != hostID || existing.WorkspaceID != workspaceID {
		return sitedomain.Site{}, domain.NewError(domain.CodeIdempotencyConflict, "request conflicts with an earlier operation")
	}
	return existing, nil
}

func (s *SiteService) List(ctx context.Context, principal identity.Principal, page PageRequest) (SitePage, error) {
	if err := requireSiteSession(principal); err != nil {
		return SitePage{}, err
	}
	return s.store.ListSites(ctx, principal.UserID, page)
}

func (s *SiteService) ListReleases(ctx context.Context, principal identity.Principal, siteID sitedomain.ID, page PageRequest) (SiteReleasePage, error) {
	if err := requireSiteSession(principal); err != nil {
		return SiteReleasePage{}, err
	}
	if _, err := s.ownedSite(ctx, principal.UserID, siteID); err != nil {
		return SiteReleasePage{}, err
	}
	return s.store.ListSiteReleases(ctx, principal.UserID, siteID, page)
}

func (s *SiteService) CreateCandidate(ctx context.Context, principal identity.Principal, siteID sitedomain.ID, revision int64, servicePort int, idempotencyKey string) (sitedomain.Release, error) {
	if err := requireSiteSession(principal); err != nil {
		return sitedomain.Release{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return sitedomain.Release{}, err
	}
	siteValue, err := s.ownedSite(ctx, principal.UserID, siteID)
	if err != nil {
		return sitedomain.Release{}, err
	}
	releaseID := sitedomain.ReleaseID(idempotentReleaseID(principal.UserID, siteID, idempotencyKey))
	candidate := sitedomain.Release{
		ID: releaseID, SiteID: siteID, WorkspaceID: siteValue.WorkspaceID, WorkspaceRevision: revision,
		ServicePort: servicePort, State: sitedomain.ReleaseCandidate,
	}
	created, err := s.store.CreateCandidateRelease(ctx, candidate, s.maxRelease, s.maxOwner)
	if err == nil {
		return created, nil
	}
	if domain.CodeOf(err) != domain.CodeConflict {
		return sitedomain.Release{}, err
	}
	existing, getErr := s.store.GetSiteRelease(ctx, siteID, releaseID)
	if getErr != nil {
		return sitedomain.Release{}, err
	}
	if existing.WorkspaceID != candidate.WorkspaceID || existing.WorkspaceRevision != candidate.WorkspaceRevision || existing.ServicePort != candidate.ServicePort {
		return sitedomain.Release{}, domain.NewError(domain.CodeIdempotencyConflict, "request conflicts with an earlier operation")
	}
	return existing, nil
}

func (s *SiteService) Materialize(ctx context.Context, principal identity.Principal, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	if err := requireSiteSession(principal); err != nil {
		return sitedomain.Release{}, err
	}
	if _, err := s.ownedSite(ctx, principal.UserID, siteID); err != nil {
		return sitedomain.Release{}, err
	}
	release, err := s.store.GetSiteRelease(ctx, siteID, releaseID)
	if err != nil {
		return sitedomain.Release{}, err
	}
	if release.MaterializedAt != nil {
		return release, nil
	}
	if release.State != sitedomain.ReleaseCandidate {
		return sitedomain.Release{}, domain.NewError(domain.CodeConflict, "only a candidate Site release can be materialized")
	}
	result, err := s.materialize.Materialize(ctx, release)
	if err != nil {
		s.recordMaterializationFailure(ctx, siteID, releaseID, err)
		return sitedomain.Release{}, err
	}
	completed, err := s.store.CompleteSiteReleaseMaterialization(ctx, siteID, releaseID, result.FileCount, result.TotalBytes)
	if err != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteMaterializationReconcileTimeout)
		defer cancel()
		current, readErr := s.store.GetSiteRelease(reconcileCtx, siteID, releaseID)
		if readErr == nil {
			if current.MaterializedAt != nil {
				return current, nil
			}
			if current.State == sitedomain.ReleaseCancelled {
				cleanupErr := s.materialize.artifacts.Delete(siteID, releaseID, current.ManifestSHA256)
				return sitedomain.Release{}, errors.Join(err, cleanupErr)
			}
			if current.State == sitedomain.ReleaseCandidate {
				completed, retryErr := s.store.CompleteSiteReleaseMaterialization(reconcileCtx, siteID, releaseID, result.FileCount, result.TotalBytes)
				if retryErr == nil {
					return completed, nil
				}
				err = errors.Join(err, retryErr)
				current, readErr = s.store.GetSiteRelease(reconcileCtx, siteID, releaseID)
				if readErr == nil {
					if current.MaterializedAt != nil {
						return current, nil
					}
					if current.State == sitedomain.ReleaseCancelled {
						cleanupErr := s.materialize.artifacts.Delete(siteID, releaseID, current.ManifestSHA256)
						return sitedomain.Release{}, errors.Join(err, cleanupErr)
					}
				}
			}
		}
		s.recordMaterializationFailure(ctx, siteID, releaseID, err)
		return sitedomain.Release{}, errors.Join(err, readErr)
	}
	return completed, nil
}

func (s *SiteService) GetRelease(ctx context.Context, principal identity.Principal, siteID sitedomain.ID, releaseID sitedomain.ReleaseID) (sitedomain.Release, error) {
	if err := requireSiteSession(principal); err != nil {
		return sitedomain.Release{}, err
	}
	if _, err := s.ownedSite(ctx, principal.UserID, siteID); err != nil {
		return sitedomain.Release{}, err
	}
	return s.store.GetSiteRelease(ctx, siteID, releaseID)
}

func (s *SiteService) ownedSite(ctx context.Context, ownerID string, siteID sitedomain.ID) (sitedomain.Site, error) {
	value, err := s.store.GetSite(ctx, siteID)
	if err != nil {
		return sitedomain.Site{}, err
	}
	if value.OwnerID != ownerID {
		return sitedomain.Site{}, domain.NewError(domain.CodeNotFound, "site not found")
	}
	return value, nil
}

func (s *SiteService) recordMaterializationFailure(ctx context.Context, siteID sitedomain.ID, releaseID sitedomain.ReleaseID, err error) {
	if ctx.Err() != nil {
		return
	}
	code := sitedomain.FailureStorageUnavailable
	switch domain.CodeOf(err) {
	case domain.CodeWorkspaceConflict:
		code = sitedomain.FailureSourceChanged
	case domain.CodeConflict, domain.CodeNotFound, domain.CodeWorkspaceBusy:
		code = sitedomain.FailureSourceUnavailable
	case domain.CodeCapacityExceeded, domain.CodeQuotaExceeded, domain.CodeRequestTooLarge:
		code = sitedomain.FailureCapacityExceeded
	}
	_, _ = s.store.RecordSiteReleaseMaterializationFailure(ctx, siteID, releaseID, code)
}

func requireSiteSession(principal identity.Principal) error {
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return validatePrincipal(principal)
}

func idempotentSiteID(ownerID, operation, key string) string {
	return "site_" + idempotentIDDigest(ownerID+"\x00"+operation+"\x00"+key)
}

func idempotentReleaseID(ownerID string, siteID sitedomain.ID, key string) string {
	return "rel_" + idempotentIDDigest(ownerID+"\x00site.release.create\x00"+string(siteID)+"\x00"+key)
}

func idempotentIDDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
