package site

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"morde/server/internal/domain"
)

type ID string
type ReleaseID string
type State string
type ReleaseState string
type FailureCode string

const (
	StateActive   State = "active"
	StateDraining State = "draining"
	StateStopped  State = "stopped"
)

const (
	ReleaseCandidate ReleaseState = "candidate"
	ReleaseHealthy   ReleaseState = "healthy"
	ReleaseFailed    ReleaseState = "failed"
	ReleaseCancelled ReleaseState = "cancelled"
)

const (
	FailureSourceChanged      FailureCode = "source_changed"
	FailureSourceUnavailable  FailureCode = "source_unavailable"
	FailureCapacityExceeded   FailureCode = "capacity_exceeded"
	FailureStorageUnavailable FailureCode = "storage_unavailable"
)

var (
	siteIDPattern    = regexp.MustCompile(`^site_[a-f0-9]{16}$`)
	releaseIDPattern = regexp.MustCompile(`^rel_[a-f0-9]{16}$`)
	digestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Site struct {
	ID                ID
	OwnerID           string
	HostID            string
	WorkspaceID       string
	State             State
	CurrentReleaseID  ReleaseID
	RollbackReleaseID ReleaseID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Release struct {
	ID                ReleaseID
	SiteID            ID
	WorkspaceID       string
	WorkspaceRevision int64
	ManifestSHA256    string
	ServicePort       int
	State             ReleaseState
	ReservationBytes  int64
	ArtifactFileCount int64
	ArtifactBytes     int64
	MaterializedAt    *time.Time
	HealthCheckedAt   *time.Time
	FailureCode       FailureCode
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// HostOccupancy is the durable view used by Host lifecycle code before it
// stops or deletes a Host. SiteIDs contains every site that still needs drain.
type HostOccupancy struct {
	HostID          string
	SiteIDs         []ID
	DrainInProgress bool
}

func NewID() (ID, error) {
	value, err := newID("site_")
	return ID(value), err
}

func NewReleaseID() (ReleaseID, error) {
	value, err := newID("rel_")
	return ReleaseID(value), err
}

func newID(prefix string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate site identity: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func ValidID(value ID) bool { return siteIDPattern.MatchString(string(value)) }

func ValidReleaseID(value ReleaseID) bool { return releaseIDPattern.MatchString(string(value)) }

func (s Site) Validate() error {
	if !ValidID(s.ID) || !domain.ValidUserID(s.OwnerID) || !domain.ValidHostID(s.HostID) || !domain.ValidWorkspaceID(s.WorkspaceID) {
		return domain.InvalidArgument("site", "site identity is invalid")
	}
	if s.State != StateActive && s.State != StateDraining && s.State != StateStopped {
		return domain.InvalidArgument("site.state", "site state is invalid")
	}
	if s.CurrentReleaseID != "" && !ValidReleaseID(s.CurrentReleaseID) ||
		s.RollbackReleaseID != "" && !ValidReleaseID(s.RollbackReleaseID) ||
		s.CurrentReleaseID != "" && s.CurrentReleaseID == s.RollbackReleaseID {
		return domain.InvalidArgument("site.releases", "site release pointers are invalid")
	}
	return nil
}

func (r Release) Validate() error {
	return r.validate(true)
}

// ValidateCandidateRequest validates a candidate identity before the trusted
// store computes its immutable Workspace manifest digest.
func (r Release) ValidateCandidateRequest() error {
	if r.ManifestSHA256 != "" || r.State != ReleaseCandidate {
		return domain.InvalidArgument("site.release", "candidate request fields are invalid")
	}
	return r.validate(false)
}

func (r Release) validate(requireManifestDigest bool) error {
	if !ValidReleaseID(r.ID) || !ValidID(r.SiteID) || !domain.ValidWorkspaceID(r.WorkspaceID) ||
		r.WorkspaceRevision <= 0 || requireManifestDigest && !digestPattern.MatchString(r.ManifestSHA256) || r.ServicePort < 1024 || !domain.ValidPreviewTargetPort(r.ServicePort) {
		return domain.InvalidArgument("site.release", "site release identity is invalid")
	}
	if r.State != ReleaseCandidate && r.State != ReleaseHealthy && r.State != ReleaseFailed && r.State != ReleaseCancelled {
		return domain.InvalidArgument("site.release.state", "site release state is invalid")
	}
	if r.ReservationBytes < 0 || r.ArtifactFileCount < 0 || r.ArtifactBytes < 0 || r.MaterializedAt == nil && (r.ArtifactFileCount != 0 || r.ArtifactBytes != 0) ||
		r.MaterializedAt != nil && r.MaterializedAt.IsZero() || r.HealthCheckedAt != nil && (r.HealthCheckedAt.IsZero() || r.MaterializedAt == nil) ||
		r.State == ReleaseHealthy && (r.MaterializedAt == nil || r.HealthCheckedAt == nil) ||
		!ValidFailureCode(r.FailureCode) || r.FailureCode != "" && r.MaterializedAt != nil {
		return domain.InvalidArgument("site.release.materialization", "site release materialization is invalid")
	}
	if r.MaterializedAt != nil && r.ReservationBytes != r.ArtifactBytes ||
		r.MaterializedAt == nil && r.State != ReleaseCandidate && r.ReservationBytes != 0 {
		return domain.InvalidArgument("site.release.reservation", "site release storage reservation is invalid")
	}
	return nil
}

func ValidFailureCode(value FailureCode) bool {
	switch value {
	case "", FailureSourceChanged, FailureSourceUnavailable, FailureCapacityExceeded, FailureStorageUnavailable:
		return true
	default:
		return false
	}
}

func (s Site) PromoteCandidate(candidate Release) (Site, Release, error) {
	if err := s.Validate(); err != nil {
		return Site{}, Release{}, err
	}
	if err := candidate.Validate(); err != nil {
		return Site{}, Release{}, err
	}
	if s.State != StateActive || candidate.SiteID != s.ID || candidate.WorkspaceID != s.WorkspaceID ||
		candidate.State != ReleaseCandidate || candidate.MaterializedAt == nil || candidate.HealthCheckedAt == nil || candidate.ID == s.CurrentReleaseID || candidate.ID == s.RollbackReleaseID {
		return Site{}, Release{}, domain.NewError(domain.CodeConflict, "site candidate cannot be promoted")
	}
	next := s
	next.RollbackReleaseID = s.CurrentReleaseID
	next.CurrentReleaseID = candidate.ID
	candidate.State = ReleaseHealthy
	return next, candidate, nil
}

func (s Site) FailCandidate(candidate Release) (Site, Release, error) {
	if err := s.Validate(); err != nil {
		return Site{}, Release{}, err
	}
	if err := candidate.Validate(); err != nil {
		return Site{}, Release{}, err
	}
	if candidate.SiteID != s.ID || candidate.WorkspaceID != s.WorkspaceID || candidate.State != ReleaseCandidate {
		return Site{}, Release{}, domain.NewError(domain.CodeConflict, "site candidate cannot be failed")
	}
	candidate.State = ReleaseFailed
	if candidate.MaterializedAt == nil {
		candidate.ReservationBytes = 0
	}
	return s, candidate, nil
}

func (s Site) Rollback(target Release) (Site, error) {
	if err := s.Validate(); err != nil {
		return Site{}, err
	}
	if err := target.Validate(); err != nil {
		return Site{}, err
	}
	if s.State != StateActive || s.RollbackReleaseID == "" || target.ID != s.RollbackReleaseID ||
		target.SiteID != s.ID || target.WorkspaceID != s.WorkspaceID || target.State != ReleaseHealthy {
		return Site{}, domain.NewError(domain.CodeConflict, "site rollback target is not healthy and eligible")
	}
	next := s
	next.CurrentReleaseID, next.RollbackReleaseID = target.ID, s.CurrentReleaseID
	return next, nil
}

func (s Site) BeginDrain() (Site, error) {
	if err := s.Validate(); err != nil {
		return Site{}, err
	}
	if s.State == StateStopped {
		return Site{}, domain.NewError(domain.CodeConflict, "stopped site cannot enter drain")
	}
	s.State = StateDraining
	return s, nil
}

func (s Site) ConfirmStopped() (Site, error) {
	if err := s.Validate(); err != nil {
		return Site{}, err
	}
	if s.State != StateDraining {
		return Site{}, domain.NewError(domain.CodeConflict, "site must be draining before it can stop")
	}
	s.State = StateStopped
	return s, nil
}

func (o HostOccupancy) Occupied() bool { return len(o.SiteIDs) != 0 }
