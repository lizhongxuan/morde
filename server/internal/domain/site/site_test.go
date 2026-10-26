package site

import (
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func testSite() Site {
	return Site{
		ID: ID("site_0123456789abcdef"), OwnerID: "u1234567", HostID: "h1234567", WorkspaceID: "w1234567", State: StateActive,
	}
}

func testRelease(id string, siteValue Site, state ReleaseState) Release {
	return Release{
		ID: ReleaseID(id), SiteID: siteValue.ID, WorkspaceID: siteValue.WorkspaceID,
		WorkspaceRevision: 4, ManifestSHA256: strings.Repeat("a", 64), ServicePort: 3000, State: state,
	}
}

func TestIDsAndValidation(t *testing.T) {
	siteID, err := NewID()
	if err != nil || !ValidID(siteID) {
		t.Fatalf("NewID() = %q, %v", siteID, err)
	}
	releaseID, err := NewReleaseID()
	if err != nil || !ValidReleaseID(releaseID) {
		t.Fatalf("NewReleaseID() = %q, %v", releaseID, err)
	}
	if ValidID(ID("rel_0123456789abcdef")) || ValidReleaseID(ReleaseID("site_0123456789abcdef")) {
		t.Fatal("site and release identity formats must remain distinct")
	}

	value := testSite()
	if err := value.Validate(); err != nil {
		t.Fatalf("valid site: %v", err)
	}
	value.CurrentReleaseID, value.RollbackReleaseID = "rel_0123456789abcdef", "rel_0123456789abcdef"
	if value.Validate() == nil {
		t.Fatal("equal current and rollback pointers were accepted")
	}

	value = testSite()
	invalid := testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.ManifestSHA256 = "BAD"
	if invalid.Validate() == nil {
		t.Fatal("invalid manifest digest was accepted")
	}
	candidateRequest := testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	candidateRequest.ManifestSHA256 = ""
	if err := candidateRequest.ValidateCandidateRequest(); err != nil {
		t.Fatalf("candidate request before server digest calculation: %v", err)
	}
	if candidateRequest.Validate() == nil {
		t.Fatal("persisted release without a server-computed digest was accepted")
	}
	candidateRequest.ManifestSHA256 = strings.Repeat("a", 64)
	if candidateRequest.ValidateCandidateRequest() == nil {
		t.Fatal("candidate request with a client-provided digest was accepted")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.ServicePort = 0
	if invalid.Validate() == nil {
		t.Fatal("invalid service port was accepted")
	}
	for _, port := range []int{22, 80, 1023, 8080, 5900, 6080, 8090, 8091, 8100, 9222} {
		invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
		invalid.ServicePort = port
		if err := invalid.Validate(); err == nil {
			t.Errorf("non-routable Host service port %d was accepted", port)
		}
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.WorkspaceRevision = 0
	if invalid.Validate() == nil {
		t.Fatal("revision zero was accepted as a publishable revision")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.ArtifactBytes = -1
	if invalid.Validate() == nil {
		t.Fatal("negative materialized artifact size was accepted")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.ReservationBytes = -1
	if invalid.Validate() == nil {
		t.Fatal("negative storage reservation was accepted")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.FailureCode = FailureCode("raw-error-text")
	if invalid.Validate() == nil {
		t.Fatal("unapproved materialization failure code was accepted")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseCandidate)
	invalid.FailureCode = FailureStorageUnavailable
	now := time.Now().UTC()
	invalid.MaterializedAt = &now
	if invalid.Validate() == nil {
		t.Fatal("materialized release retained a failure code")
	}
	invalid = testRelease("rel_0123456789abcdef", value, ReleaseHealthy)
	if invalid.Validate() == nil {
		t.Fatal("healthy release without materialization and health-check evidence was accepted")
	}
}

func TestCandidatePromotionFailureAndRollback(t *testing.T) {
	siteValue := testSite()
	first := testRelease("rel_0123456789abcdef", siteValue, ReleaseCandidate)
	if _, _, err := siteValue.PromoteCandidate(first); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promotion without a materialized artifact error = %v", err)
	}
	materializedAt := time.Now().UTC()
	first.MaterializedAt = &materializedAt
	if _, _, err := siteValue.PromoteCandidate(first); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promotion without a health check error = %v", err)
	}
	first.HealthCheckedAt = &materializedAt
	promotedSite, promotedFirst, err := siteValue.PromoteCandidate(first)
	if err != nil || promotedSite.CurrentReleaseID != first.ID || promotedSite.RollbackReleaseID != "" || promotedFirst.State != ReleaseHealthy {
		t.Fatalf("first promotion = %#v %#v, %v", promotedSite, promotedFirst, err)
	}

	second := testRelease("rel_fedcba9876543210", promotedSite, ReleaseCandidate)
	failedSite, failedSecond, err := promotedSite.FailCandidate(second)
	if err != nil || failedSite.CurrentReleaseID != first.ID || failedSite.RollbackReleaseID != "" || failedSecond.State != ReleaseFailed {
		t.Fatalf("candidate failure changed current release: %#v %#v, %v", failedSite, failedSecond, err)
	}

	second.ID = "rel_fedcba9876543210"
	second.State = ReleaseCandidate
	second.MaterializedAt = &materializedAt
	second.HealthCheckedAt = &materializedAt
	promotedAgain, healthySecond, err := promotedSite.PromoteCandidate(second)
	if err != nil || promotedAgain.CurrentReleaseID != second.ID || promotedAgain.RollbackReleaseID != first.ID || healthySecond.State != ReleaseHealthy {
		t.Fatalf("second promotion = %#v %#v, %v", promotedAgain, healthySecond, err)
	}

	rolledBack, err := promotedAgain.Rollback(promotedFirst)
	if err != nil || rolledBack.CurrentReleaseID != first.ID || rolledBack.RollbackReleaseID != second.ID {
		t.Fatalf("rollback = %#v, %v", rolledBack, err)
	}
	if _, err := promotedAgain.Rollback(healthySecond); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("rollback to current instead of rollback target error = %v", err)
	}
	wrongSite := promotedFirst
	wrongSite.SiteID = ID("site_1111111111111111")
	if _, err := promotedAgain.Rollback(wrongSite); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("rollback to another site's release error = %v", err)
	}
}

func TestDrainTransitions(t *testing.T) {
	siteValue := testSite()
	draining, err := siteValue.BeginDrain()
	if err != nil || draining.State != StateDraining {
		t.Fatalf("BeginDrain() = %#v, %v", draining, err)
	}
	stopped, err := draining.ConfirmStopped()
	if err != nil || stopped.State != StateStopped {
		t.Fatalf("ConfirmStopped() = %#v, %v", stopped, err)
	}
	if _, err := siteValue.ConfirmStopped(); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stopping an active site error = %v", err)
	}
	if _, err := stopped.BeginDrain(); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("draining a stopped site error = %v", err)
	}
	if !(HostOccupancy{HostID: siteValue.HostID, SiteIDs: []ID{siteValue.ID}, DrainInProgress: true}).Occupied() {
		t.Fatal("non-stopped site was not considered occupied")
	}
}

func TestCandidateStateMustBelongToActiveSite(t *testing.T) {
	siteValue := testSite()
	candidate := testRelease("rel_0123456789abcdef", siteValue, ReleaseFailed)
	if _, _, err := siteValue.PromoteCandidate(candidate); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promoting failed candidate error = %v", err)
	}
	other := siteValue
	other.ID = ID("site_1111111111111111")
	if _, _, err := other.FailCandidate(testRelease("rel_0123456789abcdef", siteValue, ReleaseCandidate)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("failing another site's candidate error = %v", err)
	}
	other.State = StateDraining
	if _, _, err := other.PromoteCandidate(testRelease("rel_fedcba9876543210", other, ReleaseCandidate)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("promoting while draining error = %v", err)
	}
}
