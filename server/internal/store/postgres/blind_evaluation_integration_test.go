package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

func TestBlindEvaluationPersistsReviewsWithoutExposingMapping(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestUser(t, store, 1, 1000, 1024)
	implementer := createTestUser(t, store, 1, 1000, 1024)
	reviewerA := createTestUser(t, store, 1, 1000, 1024)
	reviewerB := createTestUser(t, store, 1, 1000, 1024)
	arbitrator := createTestUser(t, store, 1, 1000, 1024)
	unassigned := createTestUser(t, store, 1, 1000, 1024)

	service, err := app.NewBlindEvaluationService(store)
	if err != nil {
		t.Fatal(err)
	}
	suite := loadBlindEvaluationSuite(t)
	if _, err := service.Create(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), suite,
		[]evaluation.Run{makeBlindEvaluationRun(suite, "run-candidate-000"), makeBlindEvaluationRun(suite, "run-candidate-005")},
		[]string{implementer.ID}, []string{reviewerA.ID, reviewerB.ID}, []string{arbitrator.ID}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-admin creation error = %v, want forbidden", err)
	}
	pack, err := service.Create(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), suite,
		[]evaluation.Run{makeBlindEvaluationRun(suite, "run-candidate-001"), makeBlindEvaluationRun(suite, "run-candidate-002")},
		[]string{implementer.ID}, []string{reviewerA.ID, reviewerB.ID}, []string{arbitrator.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Cases) == 0 || len(pack.Cases[0].Candidates) != 2 {
		t.Fatalf("blind pack = %#v", pack)
	}
	blindJSON, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{"provider-private-marker", "model-private-marker", "run-candidate-001", "run-candidate-002"} {
		if strings.Contains(string(blindJSON), privateValue) {
			t.Fatalf("review pack leaked private run metadata %q", privateValue)
		}
	}
	if _, err := service.RunManifests(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("reviewer run-manifest access error = %v, want forbidden", err)
	}
	manifests, err := service.RunManifests(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), pack.ID)
	if err != nil || len(manifests) != 2 || manifests[0].Provider != "provider-private-marker" || manifests[0].Model != "model-private-marker" {
		t.Fatalf("admin run manifests = %#v, %v", manifests, err)
	}
	if _, err := service.ReviewPack(ctx, blindEvaluationPrincipal(unassigned.ID, identity.RoleUser), pack.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unassigned reviewer error = %v, want not_found", err)
	}
	if _, err := service.ReviewPack(ctx, blindEvaluationPrincipal(implementer.ID, identity.RoleUser), pack.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("implementer review access error = %v, want not_found", err)
	}
	if _, err := service.Mapping(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("reviewer mapping access error = %v, want forbidden", err)
	}
	mapping, err := service.Mapping(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), pack.ID)
	if err != nil || evaluation.ValidateBlindMapping(suite, pack, mapping) != nil {
		t.Fatalf("admin mapping = %#v, error = %v", mapping, err)
	}

	labels := []string{pack.Cases[0].Candidates[0].Label, pack.Cases[0].Candidates[1].Label}
	for _, label := range labels {
		casesA := makeBlindReviewCases(suite, pack, label, false)
		first, err := service.SubmitReview(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID, label, casesA)
		if err != nil {
			t.Fatalf("submit reviewer A for %s: %v", label, err)
		}
		repeated, err := service.SubmitReview(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID, label, casesA)
		if err != nil || !repeated.SubmittedAt.Equal(first.SubmittedAt) {
			t.Fatalf("same review retry = %#v, %v; want original receipt", repeated, err)
		}
		changed := makeBlindReviewCases(suite, pack, label, false)
		changed[0].Judgments[0].Rationale = "a changed but still valid rationale"
		if _, err := service.SubmitReview(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID, label, changed); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("changed review retry error = %v, want conflict", err)
		}
	}
	status, err := service.Panel(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), pack.ID)
	if err != nil || status.Ready || status.PendingReason == "" {
		t.Fatalf("incomplete panel status = %#v, %v; want pending", status, err)
	}
	if _, err := service.SubmitArbitration(ctx, blindEvaluationPrincipal(arbitrator.ID, identity.RoleUser), pack.ID,
		labels[0], "resolve the cited output disagreement", makeBlindReviewCases(suite, pack, labels[0], false)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("early arbitration error = %v, want invalid_state", err)
	}
	for candidateIndex, label := range labels {
		casesB := makeBlindReviewCases(suite, pack, label, candidateIndex == 0)
		if _, err := service.SubmitReview(ctx, blindEvaluationPrincipal(reviewerB.ID, identity.RoleUser), pack.ID, label, casesB); err != nil {
			t.Fatalf("submit reviewer B for %s: %v", label, err)
		}
	}
	reviewerBundle, err := service.ReviewerBundle(ctx, blindEvaluationPrincipal(reviewerA.ID, identity.RoleUser), pack.ID)
	if err != nil || len(reviewerBundle.Reviews) != len(labels) {
		t.Fatalf("reviewer resume bundle = %#v, %v", reviewerBundle, err)
	}
	for _, review := range reviewerBundle.Reviews {
		if review.ReviewerID != reviewerA.ID {
			t.Fatalf("reviewer bundle exposed another reviewer: %#v", review)
		}
	}
	arbitrationBundle, err := service.ArbitrationBundle(ctx, blindEvaluationPrincipal(arbitrator.ID, identity.RoleUser), pack.ID)
	if err != nil || arbitrationBundle.ExpectedReviewsPerCandidate != 2 || len(arbitrationBundle.Reviews) != len(labels)*2 {
		t.Fatalf("arbitration bundle = %#v, %v", arbitrationBundle, err)
	}
	for _, review := range arbitrationBundle.Reviews {
		if review.Reviewer == reviewerA.ID || review.Reviewer == reviewerB.ID || review.Reviewer == "" {
			t.Fatalf("arbitration bundle leaked reviewer identity: %#v", review)
		}
	}

	status, err = service.Panel(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), pack.ID)
	if err != nil || status.Ready || status.PendingReason == "" {
		t.Fatalf("unresolved panel status = %#v, %v; want pending", status, err)
	}
	if _, err := service.SubmitArbitration(ctx, blindEvaluationPrincipal(arbitrator.ID, identity.RoleUser), pack.ID,
		labels[0], "resolve the cited output disagreement", makeBlindReviewCases(suite, pack, labels[0], false)); err != nil {
		t.Fatalf("submit arbitration: %v", err)
	}
	arbitrationBundle, err = service.ArbitrationBundle(ctx, blindEvaluationPrincipal(arbitrator.ID, identity.RoleUser), pack.ID)
	if err != nil || len(arbitrationBundle.Arbitrations) != 1 || arbitrationBundle.Arbitrations[0].Candidate != labels[0] {
		t.Fatalf("arbitration bundle after reload = %#v, %v", arbitrationBundle, err)
	}
	status, err = service.Panel(ctx, blindEvaluationPrincipal(admin.ID, identity.RoleAdmin), pack.ID)
	if err != nil || !status.Ready || status.ReviewCount != 4 || status.ArbitrationCount != 1 {
		t.Fatalf("resolved panel status = %#v, %v; want ready with persisted records", status, err)
	}
	panel, err := store.GetBlindEvaluationPanel(ctx, pack.ID)
	if err != nil || len(panel.Reviews) != 4 {
		t.Fatalf("stored review panel = %#v, %v", panel, err)
	}
	for _, review := range panel.Reviews {
		if review.ReviewerID != reviewerA.ID && review.ReviewerID != reviewerB.ID {
			t.Fatalf("reviewer identity was not taken from the assigned session: %#v", review)
		}
	}

	var mappingRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM blind_evaluation_mappings WHERE pack_id=$1`, pack.ID).Scan(&mappingRows); err != nil || mappingRows != 1 {
		t.Fatalf("stored mapping rows = %d, error = %v", mappingRows, err)
	}
}

func TestBlindEvaluationCreationRollsBackPartialRoster(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	admin := createTestUser(t, store, 1, 1000, 1024)
	implementer := createTestUser(t, store, 1, 1000, 1024)
	reviewer := createTestUser(t, store, 1, 1000, 1024)
	arbitrator := createTestUser(t, store, 1, 1000, 1024)
	suite := loadBlindEvaluationSuite(t)
	runs := []evaluation.Run{makeBlindEvaluationRun(suite, "run-candidate-003"), makeBlindEvaluationRun(suite, "run-candidate-004")}
	pack, mapping, err := evaluation.BuildBlindPack(suite, runs)
	if err != nil {
		t.Fatal(err)
	}
	manifests := []evaluation.RunManifest{runs[0].Manifest, runs[1].Manifest}
	err = store.CreateBlindEvaluation(ctx, suite, pack, mapping, manifests, admin.ID, []string{implementer.ID},
		[]string{reviewer.ID, "u99999999"}, []string{arbitrator.ID})
	if err == nil {
		t.Fatal("creation with missing assigned account unexpectedly succeeded")
	}
	var packs, mappings, participants int
	if err := store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM blind_evaluation_packs WHERE pack_id=$1),
 (SELECT count(*) FROM blind_evaluation_mappings WHERE pack_id=$1),
 (SELECT count(*) FROM blind_evaluation_participants WHERE pack_id=$1)`, pack.ID).Scan(&packs, &mappings, &participants); err != nil {
		t.Fatal(err)
	}
	if packs != 0 || mappings != 0 || participants != 0 {
		t.Fatalf("partial blind evaluation remains after rollback: packs=%d mappings=%d participants=%d", packs, mappings, participants)
	}
}

func loadBlindEvaluationSuite(t *testing.T) evaluation.Suite {
	t.Helper()
	raw, err := os.ReadFile("../../evaluation/testdata/suite-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite, err := evaluation.LoadSuite(raw)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func makeBlindEvaluationRun(suite evaluation.Suite, runID string) evaluation.Run {
	build := sha256.Sum256([]byte("blind-evaluation-synthetic-build"))
	run := evaluation.Run{Manifest: evaluation.RunManifest{RunID: runID, SuiteVersion: suite.Version, Provider: "provider-private-marker",
		Model: "model-private-marker", PromptVersion: "prompt-private-marker", SkillVersion: "skill-private-marker", DSHVersion: "dsh-private-marker",
		BuildSHA: hex.EncodeToString(build[:]), ToolPermissions: []string{}, Parameters: map[string]string{}, StartedAt: time.Now().UTC()}}
	for _, testCase := range suite.Cases {
		result := evaluation.CaseResult{CaseID: testCase.ID, Output: "synthetic candidate output",
			Evidence: []evaluation.Evidence{{ID: "evidence-1", Kind: "synthetic_test", SHA256: hex.EncodeToString(build[:]), Excerpt: "synthetic evidence"}}}
		for _, expectation := range testCase.Expectations {
			judgment := evaluation.Judgment{CriterionID: expectation.CriterionID, Score: evaluation.ScoreOne, Rationale: "synthetic baseline evidence supports result"}
			if expectation.Expected == evaluation.ScoreNA {
				judgment.Score = evaluation.ScoreNA
				judgment.Evidence = []evaluation.EvidenceReference{{Kind: "suite_na_reason", ID: testCase.ID + "/" + expectation.CriterionID}}
			} else {
				judgment.Evidence = []evaluation.EvidenceReference{{Kind: "output_span", StartByte: 0, EndByte: len(result.Output)}}
			}
			result.Judgments = append(result.Judgments, judgment)
		}
		run.Cases = append(run.Cases, result)
	}
	return run
}

func makeBlindReviewCases(suite evaluation.Suite, pack evaluation.BlindPack, candidate string, disagree bool) []evaluation.BlindCaseReview {
	cases := make([]evaluation.BlindCaseReview, len(suite.Cases))
	changed := false
	for caseIndex, testCase := range suite.Cases {
		var selected *evaluation.BlindCandidate
		for index := range pack.Cases[caseIndex].Candidates {
			if pack.Cases[caseIndex].Candidates[index].Label == candidate {
				selected = &pack.Cases[caseIndex].Candidates[index]
				break
			}
		}
		caseReview := evaluation.BlindCaseReview{CaseID: testCase.ID}
		for _, expectation := range testCase.Expectations {
			judgment := evaluation.Judgment{CriterionID: expectation.CriterionID, Score: expectation.Expected,
				Rationale: "judgment supported by cited candidate output"}
			if disagree && !changed && judgment.Score != evaluation.ScoreNA {
				if judgment.Score == evaluation.ScoreOne {
					judgment.Score = evaluation.ScoreZero
				} else {
					judgment.Score = evaluation.ScoreOne
				}
				changed = true
			}
			if judgment.Score == evaluation.ScoreNA {
				judgment.Evidence = []evaluation.EvidenceReference{{Kind: "suite_na_reason", ID: testCase.ID + "/" + expectation.CriterionID}}
			} else {
				judgment.Evidence = []evaluation.EvidenceReference{{Kind: "output_span", StartByte: 0, EndByte: len(selected.Output)}}
			}
			caseReview.Judgments = append(caseReview.Judgments, judgment)
		}
		cases[caseIndex] = caseReview
	}
	return cases
}

func blindEvaluationPrincipal(userID string, role identity.Role) identity.Principal {
	return identity.Principal{UserID: userID, Role: role, Kind: identity.KindSession, TokenID: "t1234567"}
}
