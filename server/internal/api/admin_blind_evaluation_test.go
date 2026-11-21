package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

type blindEvaluationManagerStub struct {
	principal identity.Principal
	pack      evaluation.BlindPack
	create    int
	review    int
	arbitrate int
}

func (s *blindEvaluationManagerStub) Create(_ context.Context, principal identity.Principal, _ evaluation.Suite, _ []evaluation.Run, _, _, _ []string) (evaluation.BlindPack, error) {
	s.principal, s.create = principal, s.create+1
	return s.pack, nil
}
func (s *blindEvaluationManagerStub) ReviewerBundle(_ context.Context, principal identity.Principal, _ string) (app.BlindReviewerBundle, error) {
	s.principal = principal
	return app.BlindReviewerBundle{Pack: s.pack, Reviews: []evaluation.BlindReview{{PackID: s.pack.ID, Candidate: "blind-a", ReviewerID: testUserID,
		SubmittedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), Cases: []evaluation.BlindCaseReview{{CaseID: "case-1"}}}}}, nil
}
func (s *blindEvaluationManagerStub) SubmitReview(_ context.Context, principal identity.Principal, _, candidate string, _ []evaluation.BlindCaseReview) (evaluation.BlindReview, error) {
	s.principal, s.review = principal, s.review+1
	return evaluation.BlindReview{Candidate: candidate, SubmittedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}, nil
}
func (s *blindEvaluationManagerStub) ArbitrationBundle(_ context.Context, principal identity.Principal, _ string) (app.BlindArbitrationBundle, error) {
	s.principal = principal
	return app.BlindArbitrationBundle{Pack: s.pack, ExpectedReviewsPerCandidate: 2, Reviews: []app.BlindArbitrationReview{{Reviewer: "审阅者1", Candidate: "blind-a",
		Cases: []evaluation.BlindCaseReview{{CaseID: "case-1"}}}},
		Arbitrations: []app.BlindArbitrationResult{{Candidate: "blind-b", Reason: "evidence supports this resolution", Cases: []evaluation.BlindCaseReview{{CaseID: "case-1"}}}}}, nil
}
func (s *blindEvaluationManagerStub) SubmitArbitration(_ context.Context, principal identity.Principal, _, candidate, _ string, _ []evaluation.BlindCaseReview) (evaluation.BlindArbitration, error) {
	s.principal, s.arbitrate = principal, s.arbitrate+1
	return evaluation.BlindArbitration{Candidate: candidate, ResolvedAt: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}, nil
}
func (s *blindEvaluationManagerStub) Mapping(_ context.Context, principal identity.Principal, packID string) (evaluation.BlindMapping, error) {
	s.principal = principal
	return evaluation.BlindMapping{PackID: packID, Assignments: []evaluation.BlindAssignment{{Label: "blind-a", RunID: "private-run-id"}}}, nil
}
func (s *blindEvaluationManagerStub) RunManifests(_ context.Context, principal identity.Principal, _ string) ([]evaluation.RunManifest, error) {
	s.principal = principal
	return []evaluation.RunManifest{{RunID: "private-run-id", Provider: "private-provider", Model: "private-model"}}, nil
}
func (s *blindEvaluationManagerStub) Panel(_ context.Context, principal identity.Principal, packID string) (app.BlindPanelStatus, error) {
	s.principal = principal
	return app.BlindPanelStatus{PackID: packID, Ready: false, PendingReason: "pending disagreement", ReviewCount: 1}, nil
}

func TestBlindEvaluationRoutesSeparateReviewerAndAdminData(t *testing.T) {
	pack := evaluation.BlindPack{ID: "1234567890abcdef1234567890abcdef", SuiteVersion: "suite-v1"}
	manager := &blindEvaluationManagerStub{pack: pack}
	adminServices := newServices()
	adminServices.verifier.role = identity.RoleAdmin
	adminDeps := adminServices.deps()
	adminDeps.BlindEvaluations = manager
	adminRouter, err := New(adminDeps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"

	created := requestWithCookieBody(adminRouter, http.MethodPost, "/api/admin/evaluation-batches",
		`{"suite":{"data_class":"synthetic"},"runs":[],"implementers":["u1234567"],"reviewers":["u1234567"],"arbitrators":["u1234567"]}`, cookie, origin)
	if created.Code != http.StatusCreated || manager.create != 1 || manager.principal.Role != identity.RoleAdmin {
		t.Fatalf("create blind pack = %d %s create=%d principal=%+v", created.Code, created.Body, manager.create, manager.principal)
	}

	panel := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/evaluation-batches/"+pack.ID, cookie, origin)
	if panel.Code != http.StatusOK || !strings.Contains(panel.Body.String(), `"ready":false`) {
		t.Fatalf("admin panel = %d %s", panel.Code, panel.Body)
	}
	mapping := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/evaluation-batches/"+pack.ID+"/mapping", cookie, origin)
	if mapping.Code != http.StatusOK || !strings.Contains(mapping.Body.String(), "private-run-id") {
		t.Fatalf("admin mapping = %d %s", mapping.Code, mapping.Body)
	}
	manifests := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/evaluation-batches/"+pack.ID+"/manifests", cookie, origin)
	if manifests.Code != http.StatusOK || !strings.Contains(manifests.Body.String(), "private-model") {
		t.Fatalf("admin manifests = %d %s", manifests.Code, manifests.Body)
	}

	userServices := newServices()
	userDeps := userServices.deps()
	userDeps.BlindEvaluations = manager
	userRouter, err := New(userDeps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	reviewerBundle := requestWithCookie(userRouter, http.MethodGet, "/api/evaluation-batches/"+pack.ID+"/review", cookie, origin)
	if reviewerBundle.Code != http.StatusOK || strings.Contains(reviewerBundle.Body.String(), `"reviewer_id"`) ||
		strings.Contains(reviewerBundle.Body.String(), "private-model") {
		t.Fatalf("reviewer bundle leaked private identity or model: %d %s", reviewerBundle.Code, reviewerBundle.Body)
	}
	arbitrationBundle := requestWithCookie(userRouter, http.MethodGet, "/api/evaluation-batches/"+pack.ID+"/arbitration", cookie, origin)
	if arbitrationBundle.Code != http.StatusOK || !strings.Contains(arbitrationBundle.Body.String(), "审阅者1") ||
		!strings.Contains(arbitrationBundle.Body.String(), "evidence supports this resolution") ||
		strings.Contains(arbitrationBundle.Body.String(), testUserID) || strings.Contains(arbitrationBundle.Body.String(), "private-model") ||
		strings.Contains(arbitrationBundle.Body.String(), "arbitrator_id") {
		t.Fatalf("arbitration bundle was not anonymized: %d %s", arbitrationBundle.Code, arbitrationBundle.Body)
	}
	if denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/evaluation-batches/"+pack.ID+"/mapping", cookie, origin); denied.Code != http.StatusForbidden {
		t.Fatalf("non-admin mapping status = %d %s; want 403", denied.Code, denied.Body)
	}
}

func TestBlindEvaluationSubmissionsUseAuthenticatedIdentity(t *testing.T) {
	services := newServices()
	manager := &blindEvaluationManagerStub{pack: evaluation.BlindPack{ID: "1234567890abcdef1234567890abcdef"}}
	deps := services.deps()
	deps.BlindEvaluations = manager
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	origin := "https://portal.example.test"
	path := "/api/evaluation-batches/" + manager.pack.ID + "/reviews"
	spoof := requestWithCookieBody(router, http.MethodPost, path,
		`{"candidate":"blind-a","reviewer_id":"u7654321","cases":[]}`, cookie, origin)
	if spoof.Code != http.StatusBadRequest || manager.review != 0 {
		t.Fatalf("reviewer identity spoof = %d %s calls=%d", spoof.Code, spoof.Body, manager.review)
	}
	valid := requestWithCookieBody(router, http.MethodPost, path,
		`{"candidate":"blind-a","cases":[{"case_id":"case-1","judgments":[]}]}`, cookie, origin)
	if valid.Code != http.StatusOK || manager.review != 1 || manager.principal.UserID != testUserID || manager.principal.Kind != identity.KindSession {
		t.Fatalf("review submission = %d %s calls=%d principal=%+v", valid.Code, valid.Body, manager.review, manager.principal)
	}
	arbitration := requestWithCookieBody(router, http.MethodPost, "/api/evaluation-batches/"+manager.pack.ID+"/arbitrations",
		`{"candidate":"blind-a","reason":"independent evidence review","cases":[{"case_id":"case-1","judgments":[]}]}`, cookie, origin)
	if arbitration.Code != http.StatusOK || manager.arbitrate != 1 || manager.principal.UserID != testUserID {
		t.Fatalf("arbitration submission = %d %s calls=%d principal=%+v", arbitration.Code, arbitration.Body, manager.arbitrate, manager.principal)
	}
}
