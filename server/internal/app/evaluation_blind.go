package app

import (
	"context"
	"sort"
	"strconv"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

type BlindReviewBundle struct {
	Suite        evaluation.Suite
	Pack         evaluation.BlindPack
	Implementers []string
	Reviewers    []string
	Reviews      []evaluation.BlindReview
}

type BlindReviewerBundle struct {
	Pack    evaluation.BlindPack
	Reviews []evaluation.BlindReview
}

type BlindArbitrationReview struct {
	Reviewer  string
	Candidate string
	Cases     []evaluation.BlindCaseReview
}

type BlindArbitrationResult struct {
	Candidate string
	Reason    string
	Cases     []evaluation.BlindCaseReview
}

type BlindArbitrationBundle struct {
	Pack                        evaluation.BlindPack
	ExpectedReviewsPerCandidate int
	Reviews                     []BlindArbitrationReview
	Arbitrations                []BlindArbitrationResult
}

type BlindEvaluationPanelRecord struct {
	BlindReviewBundle
	Reviewers    []string
	Arbitrators  []string
	RunManifests []evaluation.RunManifest
	Reviews      []evaluation.BlindReview
	Arbitrations []evaluation.BlindArbitration
}

type BlindEvaluationStore interface {
	CreateBlindEvaluation(context.Context, evaluation.Suite, evaluation.BlindPack, evaluation.BlindMapping, []evaluation.RunManifest, string, []string, []string, []string) error
	GetBlindReviewBundle(context.Context, string, string) (BlindReviewBundle, error)
	GetBlindArbitrationBundle(context.Context, string, string) (BlindReviewBundle, error)
	GetBlindEvaluationPanel(context.Context, string) (BlindEvaluationPanelRecord, error)
	GetBlindEvaluationMapping(context.Context, string) (evaluation.BlindMapping, error)
	PutBlindReview(context.Context, evaluation.BlindReview) (evaluation.BlindReview, error)
	PutBlindArbitration(context.Context, evaluation.BlindArbitration) (evaluation.BlindArbitration, error)
}

type BlindEvaluationService struct {
	store BlindEvaluationStore
}

type BlindPanelStatus struct {
	PackID           string
	Ready            bool
	PendingReason    string
	ReviewCount      int
	ArbitrationCount int
}

func NewBlindEvaluationService(store BlindEvaluationStore) (*BlindEvaluationService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("blind_evaluation", "blind evaluation store is required")
	}
	return &BlindEvaluationService{store: store}, nil
}

func (s *BlindEvaluationService) Create(ctx context.Context, principal identity.Principal, suite evaluation.Suite, runs []evaluation.Run, implementers, reviewers, arbitrators []string) (evaluation.BlindPack, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return evaluation.BlindPack{}, err
	}
	if !validBlindParticipants(implementers, reviewers, arbitrators) {
		return evaluation.BlindPack{}, domain.InvalidArgument("blind_evaluation", "implementation and independent review rosters are invalid")
	}
	pack, mapping, err := evaluation.BuildBlindPack(suite, runs)
	if err != nil {
		return evaluation.BlindPack{}, domain.InvalidArgument("blind_evaluation", "blind evaluation inputs are invalid")
	}
	if err := evaluation.ValidateBlindMapping(suite, pack, mapping); err != nil {
		return evaluation.BlindPack{}, domain.NewError(domain.CodeInternal, "generated blind mapping is invalid")
	}
	manifests := make([]evaluation.RunManifest, len(runs))
	for index := range runs {
		manifests[index] = runs[index].Manifest
	}
	if err := s.store.CreateBlindEvaluation(ctx, suite, pack, mapping, manifests, principal.UserID, implementers, reviewers, arbitrators); err != nil {
		return evaluation.BlindPack{}, err
	}
	return pack, nil
}

func (s *BlindEvaluationService) ReviewerBundle(ctx context.Context, principal identity.Principal, packID string) (BlindReviewerBundle, error) {
	if err := validateBlindParticipantPrincipal(principal); err != nil {
		return BlindReviewerBundle{}, err
	}
	bundle, err := s.store.GetBlindReviewBundle(ctx, packID, principal.UserID)
	if err != nil {
		return BlindReviewerBundle{}, err
	}
	if err := evaluation.ValidateBlindPack(bundle.Suite, bundle.Pack); err != nil {
		return BlindReviewerBundle{}, domain.NewError(domain.CodeInternal, "stored blind evaluation is invalid")
	}
	for _, review := range bundle.Reviews {
		if review.ReviewerID != principal.UserID || evaluation.ValidateBlindReview(bundle.Suite, bundle.Pack, review, bundle.Implementers) != nil {
			return BlindReviewerBundle{}, domain.NewError(domain.CodeInternal, "stored blind review is invalid")
		}
	}
	return BlindReviewerBundle{Pack: bundle.Pack, Reviews: bundle.Reviews}, nil
}

func (s *BlindEvaluationService) ReviewPack(ctx context.Context, principal identity.Principal, packID string) (evaluation.BlindPack, error) {
	bundle, err := s.ReviewerBundle(ctx, principal, packID)
	return bundle.Pack, err
}

func (s *BlindEvaluationService) ArbitrationBundle(ctx context.Context, principal identity.Principal, packID string) (BlindArbitrationBundle, error) {
	if err := validateBlindParticipantPrincipal(principal); err != nil {
		return BlindArbitrationBundle{}, err
	}
	bundle, err := s.store.GetBlindArbitrationBundle(ctx, packID, principal.UserID)
	if err != nil {
		return BlindArbitrationBundle{}, err
	}
	if err := evaluation.ValidateBlindPack(bundle.Suite, bundle.Pack); err != nil {
		return BlindArbitrationBundle{}, domain.NewError(domain.CodeInternal, "stored blind evaluation is invalid")
	}
	reviewerIDs := make([]string, 0, len(bundle.Reviews))
	seen := make(map[string]struct{}, len(bundle.Reviews))
	for _, review := range bundle.Reviews {
		if err := evaluation.ValidateBlindReview(bundle.Suite, bundle.Pack, review, bundle.Implementers); err != nil {
			return BlindArbitrationBundle{}, domain.NewError(domain.CodeInternal, "stored blind review is invalid")
		}
		if _, exists := seen[review.ReviewerID]; !exists {
			seen[review.ReviewerID] = struct{}{}
			reviewerIDs = append(reviewerIDs, review.ReviewerID)
		}
	}
	sort.Strings(reviewerIDs)
	aliases := make(map[string]string, len(reviewerIDs))
	for index, reviewerID := range reviewerIDs {
		aliases[reviewerID] = "审阅者" + strconv.Itoa(index+1)
	}
	result := BlindArbitrationBundle{Pack: bundle.Pack, ExpectedReviewsPerCandidate: len(bundle.Reviewers),
		Reviews: make([]BlindArbitrationReview, 0, len(bundle.Reviews))}
	for _, review := range bundle.Reviews {
		result.Reviews = append(result.Reviews, BlindArbitrationReview{Reviewer: aliases[review.ReviewerID], Candidate: review.Candidate, Cases: review.Cases})
	}
	record, err := s.store.GetBlindEvaluationPanel(ctx, packID)
	if err != nil {
		return BlindArbitrationBundle{}, err
	}
	if !containsBlindParticipant(record.Arbitrators, principal.UserID) {
		return BlindArbitrationBundle{}, domain.NewError(domain.CodeInternal, "stored blind arbitrator assignment is invalid")
	}
	for _, arbitration := range record.Arbitrations {
		candidateReviews := make([]evaluation.BlindReview, 0, len(bundle.Reviewers))
		for _, review := range record.Reviews {
			if review.Candidate == arbitration.Candidate {
				candidateReviews = append(candidateReviews, review)
			}
		}
		if !containsBlindParticipant(record.Arbitrators, arbitration.ArbitratorID) ||
			evaluation.ValidateBlindArbitration(bundle.Suite, bundle.Pack, arbitration, candidateReviews, bundle.Implementers) != nil {
			return BlindArbitrationBundle{}, domain.NewError(domain.CodeInternal, "stored blind arbitration is invalid")
		}
		result.Arbitrations = append(result.Arbitrations, BlindArbitrationResult{Candidate: arbitration.Candidate,
			Reason: arbitration.Reason, Cases: arbitration.Cases})
	}
	return result, nil
}

func containsBlindParticipant(participants []string, userID string) bool {
	for _, participant := range participants {
		if participant == userID {
			return true
		}
	}
	return false
}

func (s *BlindEvaluationService) SubmitReview(ctx context.Context, principal identity.Principal, packID, candidate string, cases []evaluation.BlindCaseReview) (evaluation.BlindReview, error) {
	if err := validateBlindParticipantPrincipal(principal); err != nil {
		return evaluation.BlindReview{}, err
	}
	bundle, err := s.store.GetBlindReviewBundle(ctx, packID, principal.UserID)
	if err != nil {
		return evaluation.BlindReview{}, err
	}
	review := evaluation.BlindReview{PackID: packID, Candidate: candidate, ReviewerID: principal.UserID, SubmittedAt: time.Now().UTC(), Cases: cases}
	if err := evaluation.ValidateBlindReview(bundle.Suite, bundle.Pack, review, bundle.Implementers); err != nil {
		return evaluation.BlindReview{}, domain.InvalidArgument("blind_review", "blind review is incomplete or invalid")
	}
	return s.store.PutBlindReview(ctx, review)
}

func (s *BlindEvaluationService) SubmitArbitration(ctx context.Context, principal identity.Principal, packID, candidate, reason string, cases []evaluation.BlindCaseReview) (evaluation.BlindArbitration, error) {
	if err := validateBlindParticipantPrincipal(principal); err != nil {
		return evaluation.BlindArbitration{}, err
	}
	bundle, err := s.store.GetBlindArbitrationBundle(ctx, packID, principal.UserID)
	if err != nil {
		return evaluation.BlindArbitration{}, err
	}
	record, err := s.store.GetBlindEvaluationPanel(ctx, packID)
	if err != nil {
		return evaluation.BlindArbitration{}, err
	}
	candidateReviews := make([]evaluation.BlindReview, 0, 2)
	for _, review := range record.Reviews {
		if review.Candidate == candidate {
			candidateReviews = append(candidateReviews, review)
		}
	}
	if len(candidateReviews) != len(record.Reviewers) {
		return evaluation.BlindArbitration{}, domain.NewError(domain.CodeInvalidState, "all assigned reviews must be submitted before arbitration")
	}
	arbitration := evaluation.BlindArbitration{PackID: packID, Candidate: candidate, ArbitratorID: principal.UserID,
		Reason: reason, ResolvedAt: time.Now().UTC(), Cases: cases}
	if err := evaluation.ValidateBlindArbitration(bundle.Suite, bundle.Pack, arbitration, candidateReviews, bundle.Implementers); err != nil {
		return evaluation.BlindArbitration{}, domain.InvalidArgument("blind_arbitration", "blind arbitration is incomplete or invalid")
	}
	return s.store.PutBlindArbitration(ctx, arbitration)
}

func (s *BlindEvaluationService) Mapping(ctx context.Context, principal identity.Principal, packID string) (evaluation.BlindMapping, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return evaluation.BlindMapping{}, err
	}
	mapping, err := s.store.GetBlindEvaluationMapping(ctx, packID)
	if err != nil {
		return evaluation.BlindMapping{}, err
	}
	record, err := s.store.GetBlindEvaluationPanel(ctx, packID)
	if err != nil {
		return evaluation.BlindMapping{}, err
	}
	if err := evaluation.ValidateBlindMapping(record.Suite, record.Pack, mapping); err != nil {
		return evaluation.BlindMapping{}, domain.NewError(domain.CodeInternal, "stored blind evaluation mapping is invalid")
	}
	return mapping, nil
}

func (s *BlindEvaluationService) RunManifests(ctx context.Context, principal identity.Principal, packID string) ([]evaluation.RunManifest, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return nil, err
	}
	record, err := s.store.GetBlindEvaluationPanel(ctx, packID)
	if err != nil {
		return nil, err
	}
	mapping, err := s.store.GetBlindEvaluationMapping(ctx, packID)
	if err != nil {
		return nil, err
	}
	if err := evaluation.ValidateBlindMapping(record.Suite, record.Pack, mapping); err != nil {
		return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation mapping is invalid")
	}
	manifestsByRun := make(map[string]evaluation.RunManifest, len(record.RunManifests))
	for _, manifest := range record.RunManifests {
		if manifest.SuiteVersion != record.Suite.Version {
			return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation manifest has the wrong suite")
		}
		if _, err := evaluation.ComparisonKey(manifest); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation manifest is invalid")
		}
		if _, duplicate := manifestsByRun[manifest.RunID]; duplicate {
			return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation manifests contain duplicate runs")
		}
		manifestsByRun[manifest.RunID] = manifest
	}
	if len(manifestsByRun) != len(mapping.Assignments) {
		return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation manifests do not match candidates")
	}
	for _, assignment := range mapping.Assignments {
		if _, ok := manifestsByRun[assignment.RunID]; !ok {
			return nil, domain.NewError(domain.CodeInternal, "stored blind evaluation manifest is missing")
		}
	}
	result := make([]evaluation.RunManifest, 0, len(mapping.Assignments))
	for _, assignment := range mapping.Assignments {
		result = append(result, manifestsByRun[assignment.RunID])
	}
	return result, nil
}

func (s *BlindEvaluationService) Panel(ctx context.Context, principal identity.Principal, packID string) (BlindPanelStatus, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return BlindPanelStatus{}, err
	}
	record, err := s.store.GetBlindEvaluationPanel(ctx, packID)
	if err != nil {
		return BlindPanelStatus{}, err
	}
	status := BlindPanelStatus{PackID: packID, ReviewCount: len(record.Reviews), ArbitrationCount: len(record.Arbitrations)}
	if !blindPanelHasAllAssignedReviews(record) {
		status.PendingReason = "assigned reviewers have not completed every candidate"
		return status, nil
	}
	if err := evaluation.ValidateBlindPanel(record.Suite, record.Pack, record.Reviews, record.Arbitrations, record.Implementers); err != nil {
		status.PendingReason = err.Error()
		return status, nil
	}
	status.Ready = true
	return status, nil
}

func blindPanelHasAllAssignedReviews(record BlindEvaluationPanelRecord) bool {
	if len(record.Reviewers) < 2 || len(record.Pack.Cases) == 0 || len(record.Pack.Cases[0].Candidates) < 2 {
		return false
	}
	assigned := make(map[string]struct{}, len(record.Reviewers))
	for _, reviewer := range record.Reviewers {
		assigned[reviewer] = struct{}{}
	}
	seen := make(map[string]struct{}, len(record.Reviews))
	for _, review := range record.Reviews {
		if _, ok := assigned[review.ReviewerID]; !ok {
			return false
		}
		key := review.Candidate + "\x00" + review.ReviewerID
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	for _, candidate := range record.Pack.Cases[0].Candidates {
		for reviewer := range assigned {
			if _, ok := seen[candidate.Label+"\x00"+reviewer]; !ok {
				return false
			}
		}
	}
	return len(seen) == len(record.Reviewers)*len(record.Pack.Cases[0].Candidates)
}

func validateBlindParticipantPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || principal.ClientInstallationID != "" {
		return domain.NewError(domain.CodeForbidden, "authenticated user session required")
	}
	return nil
}

func validBlindParticipants(implementers, reviewers, arbitrators []string) bool {
	sets := make([]map[string]struct{}, 3)
	values := [][]string{implementers, reviewers, arbitrators}
	minimums := []int{1, 2, 1}
	for index, roster := range values {
		if len(roster) < minimums[index] {
			return false
		}
		sets[index] = make(map[string]struct{}, len(roster))
		for _, userID := range roster {
			if !domain.ValidUserID(userID) {
				return false
			}
			if _, duplicate := sets[index][userID]; duplicate {
				return false
			}
			sets[index][userID] = struct{}{}
		}
	}
	for index, set := range sets {
		for other := index + 1; other < len(sets); other++ {
			for userID := range set {
				if _, overlap := sets[other][userID]; overlap {
					return false
				}
			}
		}
	}
	return true
}
