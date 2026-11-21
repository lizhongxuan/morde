package evaluation

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

type BlindPack struct {
	ID           string            `json:"id"`
	SuiteVersion string            `json:"suite_version"`
	Rubric       []RubricCriterion `json:"rubric"`
	Cases        []BlindCase       `json:"cases"`
}

type BlindCase struct {
	CaseID        string               `json:"case_id"`
	Priority      string               `json:"priority"`
	Tags          []string             `json:"tags"`
	Input         string               `json:"input"`
	Expected      string               `json:"expected_behavior"`
	Applicability []BlindApplicability `json:"applicability"`
	Candidates    []BlindCandidate     `json:"candidates"`
}

type BlindApplicability struct {
	CriterionID string `json:"criterion_id"`
	Applicable  bool   `json:"applicable"`
	Reason      string `json:"not_applicable_reason,omitempty"`
}

type BlindCandidate struct {
	Label    string     `json:"label"`
	Output   string     `json:"output"`
	Evidence []Evidence `json:"evidence"`
}

type BlindMapping struct {
	PackID      string            `json:"pack_id"`
	Assignments []BlindAssignment `json:"assignments"`
}

type BlindAssignment struct {
	Label     string `json:"label"`
	RunID     string `json:"run_id"`
	Positions []int  `json:"positions"`
}

type BlindCaseReview struct {
	CaseID    string     `json:"case_id"`
	Judgments []Judgment `json:"judgments"`
}

type BlindReview struct {
	PackID      string            `json:"pack_id"`
	Candidate   string            `json:"candidate"`
	ReviewerID  string            `json:"reviewer_id"`
	SubmittedAt time.Time         `json:"submitted_at"`
	Cases       []BlindCaseReview `json:"cases"`
}

type BlindArbitration struct {
	PackID       string            `json:"pack_id"`
	Candidate    string            `json:"candidate"`
	ArbitratorID string            `json:"arbitrator_id"`
	Reason       string            `json:"reason"`
	ResolvedAt   time.Time         `json:"resolved_at"`
	Cases        []BlindCaseReview `json:"cases"`
}

func BuildBlindPack(suite Suite, runs []Run) (BlindPack, BlindMapping, error) {
	if err := ValidateSuite(suite); err != nil {
		return BlindPack{}, BlindMapping{}, err
	}
	if len(runs) < 2 {
		return BlindPack{}, BlindMapping{}, errors.New("blind evaluation requires at least two candidate runs")
	}
	packID, err := randomHex(16)
	if err != nil {
		return BlindPack{}, BlindMapping{}, fmt.Errorf("generate blind pack ID: %w", err)
	}
	pack := BlindPack{ID: packID, SuiteVersion: suite.Version, Rubric: append([]RubricCriterion(nil), suite.Rubric...)}
	mapping := BlindMapping{PackID: packID, Assignments: make([]BlindAssignment, len(runs))}
	labelsByRunID := make(map[string]string, len(runs))
	runIndexes := make(map[string]int, len(runs))
	usedLabels := make(map[string]struct{}, len(runs))
	for index, run := range runs {
		if err := ValidateRun(suite, run); err != nil {
			return BlindPack{}, BlindMapping{}, fmt.Errorf("validate candidate run %d: %w", index, err)
		}
		if _, duplicate := runIndexes[run.Manifest.RunID]; duplicate {
			return BlindPack{}, BlindMapping{}, errors.New("blind evaluation repeats a candidate run")
		}
		runIndexes[run.Manifest.RunID] = index
		label, err := randomHex(8)
		if err != nil {
			return BlindPack{}, BlindMapping{}, fmt.Errorf("generate blind candidate label: %w", err)
		}
		label = "blind-" + label
		if _, duplicate := usedLabels[label]; duplicate {
			return BlindPack{}, BlindMapping{}, errors.New("generated duplicate blind candidate label")
		}
		usedLabels[label] = struct{}{}
		labelsByRunID[run.Manifest.RunID] = label
		mapping.Assignments[index] = BlindAssignment{Label: label, RunID: run.Manifest.RunID, Positions: make([]int, len(suite.Cases))}
	}
	for caseIndex, testCase := range suite.Cases {
		candidateRuns := make([]int, len(runs))
		for index := range candidateRuns {
			candidateRuns[index] = index
		}
		if err := shuffleBlindCandidates(candidateRuns); err != nil {
			return BlindPack{}, BlindMapping{}, fmt.Errorf("randomize blind candidate order: %w", err)
		}
		blindCase := BlindCase{CaseID: testCase.ID, Priority: testCase.Priority, Tags: append([]string(nil), testCase.Tags...), Input: testCase.Input,
			Expected: testCase.Expected, Applicability: blindApplicability(testCase.Expectations)}
		for position, runIndex := range candidateRuns {
			run := runs[runIndex]
			result := run.Cases[caseIndex]
			blindCase.Candidates = append(blindCase.Candidates, BlindCandidate{
				Label: labelsByRunID[run.Manifest.RunID], Output: result.Output,
				Evidence: append([]Evidence(nil), result.Evidence...),
			})
			mapping.Assignments[runIndexes[run.Manifest.RunID]].Positions[caseIndex] = position
		}
		pack.Cases = append(pack.Cases, blindCase)
	}
	return pack, mapping, nil
}

func ValidateBlindPack(suite Suite, pack BlindPack) error {
	return validateBlindPack(suite, pack)
}

func ValidateBlindMapping(suite Suite, pack BlindPack, mapping BlindMapping) error {
	if err := validateBlindPack(suite, pack); err != nil {
		return err
	}
	if mapping.PackID != pack.ID || len(mapping.Assignments) != len(pack.Cases[0].Candidates) {
		return errors.New("blind mapping header is invalid")
	}
	seenLabels := make(map[string]struct{}, len(mapping.Assignments))
	seenRuns := make(map[string]struct{}, len(mapping.Assignments))
	for _, assignment := range mapping.Assignments {
		if !validBoundedText(assignment.Label, maxMetadataBytes) || !validBoundedText(assignment.RunID, maxMetadataBytes) || len(assignment.Positions) != len(pack.Cases) {
			return errors.New("blind mapping assignment is invalid")
		}
		if _, duplicate := seenLabels[assignment.Label]; duplicate {
			return errors.New("blind mapping repeats a candidate label")
		}
		if _, duplicate := seenRuns[assignment.RunID]; duplicate {
			return errors.New("blind mapping repeats a candidate run")
		}
		seenLabels[assignment.Label] = struct{}{}
		seenRuns[assignment.RunID] = struct{}{}
		for caseIndex, position := range assignment.Positions {
			if position < 0 || position >= len(pack.Cases[caseIndex].Candidates) || pack.Cases[caseIndex].Candidates[position].Label != assignment.Label {
				return fmt.Errorf("blind mapping position is invalid for case %q", pack.Cases[caseIndex].CaseID)
			}
		}
	}
	return nil
}

func ValidateBlindReview(suite Suite, pack BlindPack, review BlindReview, implementers []string) error {
	if err := validateBlindPack(suite, pack); err != nil {
		return err
	}
	if !validImplementationRoster(implementers) {
		return errors.New("blind review implementation roster is missing or invalid")
	}
	if review.PackID != pack.ID || !validBoundedText(review.Candidate, maxMetadataBytes) ||
		!validBoundedText(review.ReviewerID, maxMetadataBytes) || review.SubmittedAt.IsZero() ||
		isImplementer(review.ReviewerID, implementers) {
		return errors.New("blind review identity or pack reference is invalid")
	}
	candidateExists := false
	for _, candidate := range pack.Cases[0].Candidates {
		if candidate.Label == review.Candidate {
			candidateExists = true
			break
		}
	}
	if !candidateExists {
		return errors.New("blind review candidate is not in the pack")
	}
	return validateBlindCaseReviews(suite, pack, review.Candidate, review.Cases)
}

func ValidateBlindArbitration(suite Suite, pack BlindPack, arbitration BlindArbitration, reviews []BlindReview, implementers []string) error {
	if err := validateBlindPack(suite, pack); err != nil {
		return err
	}
	if !validImplementationRoster(implementers) || len(reviews) < 2 {
		return errors.New("blind arbitration requires an implementation roster and two reviews")
	}
	seenReviewers := make(map[string]struct{}, len(reviews))
	for _, review := range reviews {
		if err := ValidateBlindReview(suite, pack, review, implementers); err != nil {
			return err
		}
		if review.Candidate != arbitration.Candidate {
			return errors.New("blind arbitration reviews do not match candidate")
		}
		if _, duplicate := seenReviewers[review.ReviewerID]; duplicate {
			return errors.New("blind arbitration repeats a reviewer")
		}
		seenReviewers[review.ReviewerID] = struct{}{}
	}
	disagreement := false
	for index := 1; index < len(reviews); index++ {
		disagreement = disagreement || blindReviewsDisagree(reviews[0], reviews[index])
	}
	if !disagreement {
		return errors.New("blind arbitration requires a scoring disagreement")
	}
	return validateBlindArbitration(suite, pack, arbitration, arbitration.Candidate, reviews, implementers)
}

func ValidateBlindPanel(suite Suite, pack BlindPack, reviews []BlindReview, arbitrations []BlindArbitration, implementers []string) error {
	if err := validateBlindPack(suite, pack); err != nil {
		return err
	}
	if !validImplementationRoster(implementers) {
		return errors.New("blind panel implementation roster is missing or invalid")
	}
	reviewsByCandidate := make(map[string][]BlindReview)
	for _, review := range reviews {
		if err := ValidateBlindReview(suite, pack, review, implementers); err != nil {
			return err
		}
		for _, previous := range reviewsByCandidate[review.Candidate] {
			if previous.ReviewerID == review.ReviewerID {
				return errors.New("blind candidate has duplicate reviewer")
			}
		}
		reviewsByCandidate[review.Candidate] = append(reviewsByCandidate[review.Candidate], review)
	}
	arbitrationByCandidate := make(map[string]BlindArbitration, len(arbitrations))
	for _, arbitration := range arbitrations {
		if _, duplicate := arbitrationByCandidate[arbitration.Candidate]; duplicate {
			return errors.New("blind candidate has duplicate arbitration")
		}
		arbitrationByCandidate[arbitration.Candidate] = arbitration
	}
	for _, candidate := range pack.Cases[0].Candidates {
		candidateReviews := reviewsByCandidate[candidate.Label]
		if len(candidateReviews) < 2 {
			return fmt.Errorf("blind candidate %q has fewer than two independent reviews", candidate.Label)
		}
		disagreement := false
		for index := 1; index < len(candidateReviews); index++ {
			disagreement = disagreement || blindReviewsDisagree(candidateReviews[0], candidateReviews[index])
		}
		arbitration, hasArbitration := arbitrationByCandidate[candidate.Label]
		if !disagreement && hasArbitration {
			return errors.New("blind arbitration exists without a scoring disagreement")
		}
		if disagreement && !hasArbitration {
			return fmt.Errorf("blind candidate %q has an unresolved scoring disagreement", candidate.Label)
		}
		if disagreement {
			if err := validateBlindArbitration(suite, pack, arbitration, candidate.Label, reviews, implementers); err != nil {
				return err
			}
		}
	}
	for label := range arbitrationByCandidate {
		if !blindCandidateExists(pack, label) {
			return errors.New("blind arbitration candidate is not in the pack")
		}
	}
	return nil
}

func validateBlindPack(suite Suite, pack BlindPack) error {
	if err := ValidateSuite(suite); err != nil {
		return err
	}
	if !validBlindPackID(pack.ID) || pack.SuiteVersion != suite.Version || len(pack.Rubric) != len(suite.Rubric) || len(pack.Cases) != len(suite.Cases) {
		return errors.New("blind pack header is invalid")
	}
	for index, criterion := range suite.Rubric {
		if pack.Rubric[index] != criterion {
			return errors.New("blind pack rubric does not match the frozen suite")
		}
	}
	labels := make(map[string]struct{})
	for caseIndex, testCase := range suite.Cases {
		blindCase := pack.Cases[caseIndex]
		if blindCase.CaseID != testCase.ID || blindCase.Priority != testCase.Priority || blindCase.Input != testCase.Input || blindCase.Expected != testCase.Expected ||
			len(blindCase.Applicability) != len(testCase.Expectations) || len(blindCase.Candidates) < 2 || len(blindCase.Tags) != len(testCase.Tags) {
			return fmt.Errorf("blind pack case %q is invalid", testCase.ID)
		}
		for tagIndex, tag := range testCase.Tags {
			if blindCase.Tags[tagIndex] != tag {
				return fmt.Errorf("blind pack case %q has modified tags", testCase.ID)
			}
		}
		for expectationIndex, expectation := range testCase.Expectations {
			applicability := blindCase.Applicability[expectationIndex]
			if applicability.CriterionID != expectation.CriterionID || applicability.Applicable == (expectation.Expected == ScoreNA) ||
				(expectation.Expected == ScoreNA && applicability.Reason != expectation.NotApplicableReason) ||
				(expectation.Expected != ScoreNA && applicability.Reason != "") {
				return fmt.Errorf("blind pack case %q has modified applicability", testCase.ID)
			}
		}
		caseLabels := make(map[string]struct{}, len(blindCase.Candidates))
		for _, candidate := range blindCase.Candidates {
			if !validBlindLabel(candidate.Label) || len(candidate.Output) > maxRunOutputBytes || !utf8.ValidString(candidate.Output) {
				return fmt.Errorf("blind pack case %q has an invalid candidate", testCase.ID)
			}
			if _, duplicate := caseLabels[candidate.Label]; duplicate {
				return fmt.Errorf("blind pack case %q repeats candidate %q", testCase.ID, candidate.Label)
			}
			caseLabels[candidate.Label] = struct{}{}
			if caseIndex == 0 {
				labels[candidate.Label] = struct{}{}
			}
			if err := validateEvidence(candidate.Evidence, testCase.ID); err != nil {
				return err
			}
		}
		if len(caseLabels) != len(labels) {
			return fmt.Errorf("blind pack case %q candidate set changed", testCase.ID)
		}
		for label := range labels {
			if _, ok := caseLabels[label]; !ok {
				return fmt.Errorf("blind pack case %q is missing candidate %q", testCase.ID, label)
			}
		}
	}
	return nil
}

func validateBlindCaseReviews(suite Suite, pack BlindPack, candidateLabel string, cases []BlindCaseReview) error {
	if len(cases) != len(suite.Cases) {
		return errors.New("blind review does not cover every suite case")
	}
	for caseIndex, testCase := range suite.Cases {
		caseReview := cases[caseIndex]
		candidate := findBlindCandidate(pack.Cases[caseIndex], candidateLabel)
		if candidate == nil || caseReview.CaseID != testCase.ID || len(caseReview.Judgments) != len(suite.Rubric) {
			return fmt.Errorf("blind review result for case %q is invalid", testCase.ID)
		}
		evidenceIDs := make(map[string]struct{}, len(candidate.Evidence))
		for _, evidence := range candidate.Evidence {
			evidenceIDs[evidence.ID] = struct{}{}
		}
		for rubricIndex, judgment := range caseReview.Judgments {
			expectation := testCase.Expectations[rubricIndex]
			if judgment.CriterionID != suite.Rubric[rubricIndex].ID || !validScore(judgment.Score) ||
				!validBoundedText(judgment.Rationale, maxExpectedBytes) || len(judgment.Evidence) == 0 ||
				(expectation.Expected == ScoreNA) != (judgment.Score == ScoreNA) {
				return fmt.Errorf("blind review has invalid judgment for %q/%q", testCase.ID, judgment.CriterionID)
			}
			for _, reference := range judgment.Evidence {
				if !validJudgmentEvidenceRef(reference, candidate.Output, evidenceIDs, testCase.ID, judgment.CriterionID, judgment.Score == ScoreNA) {
					return fmt.Errorf("blind review has invalid evidence for %q/%q", testCase.ID, judgment.CriterionID)
				}
			}
		}
	}
	return nil
}

func validateBlindArbitration(suite Suite, pack BlindPack, arbitration BlindArbitration, candidate string, reviews []BlindReview, implementers []string) error {
	if arbitration.PackID != pack.ID || arbitration.Candidate != candidate || !validBoundedText(arbitration.ArbitratorID, maxMetadataBytes) ||
		!validBoundedText(arbitration.Reason, maxExpectedBytes) || arbitration.ResolvedAt.IsZero() || isImplementer(arbitration.ArbitratorID, implementers) {
		return errors.New("blind arbitration is invalid")
	}
	for _, review := range reviews {
		if review.ReviewerID == arbitration.ArbitratorID {
			return errors.New("blind arbitrator must be independent of reviewers")
		}
	}
	return validateBlindCaseReviews(suite, pack, candidate, arbitration.Cases)
}

func validateEvidence(evidenceItems []Evidence, caseID string) error {
	seen := make(map[string]struct{}, len(evidenceItems))
	for _, evidence := range evidenceItems {
		if !validEvidence(evidence) {
			return fmt.Errorf("blind pack case %q has invalid evidence", caseID)
		}
		if _, duplicate := seen[evidence.ID]; duplicate {
			return fmt.Errorf("blind pack case %q repeats evidence %q", caseID, evidence.ID)
		}
		seen[evidence.ID] = struct{}{}
	}
	return nil
}

func blindApplicability(expectations []CaseExpectation) []BlindApplicability {
	result := make([]BlindApplicability, len(expectations))
	for index, expectation := range expectations {
		result[index] = BlindApplicability{CriterionID: expectation.CriterionID, Applicable: expectation.Expected != ScoreNA}
		if expectation.Expected == ScoreNA {
			result[index].Reason = expectation.NotApplicableReason
		}
	}
	return result
}

func blindReviewsDisagree(first, second BlindReview) bool {
	for caseIndex := range first.Cases {
		for rubricIndex := range first.Cases[caseIndex].Judgments {
			if first.Cases[caseIndex].Judgments[rubricIndex].Score != second.Cases[caseIndex].Judgments[rubricIndex].Score {
				return true
			}
		}
	}
	return false
}

func findBlindCandidate(testCase BlindCase, label string) *BlindCandidate {
	for index := range testCase.Candidates {
		if testCase.Candidates[index].Label == label {
			return &testCase.Candidates[index]
		}
	}
	return nil
}

func blindCandidateExists(pack BlindPack, label string) bool {
	if len(pack.Cases) == 0 {
		return false
	}
	return findBlindCandidate(pack.Cases[0], label) != nil
}

func isImplementer(reviewer string, implementers []string) bool {
	for _, implementer := range implementers {
		if reviewer == implementer {
			return true
		}
	}
	return false
}

func validImplementationRoster(implementers []string) bool {
	if len(implementers) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(implementers))
	for _, implementer := range implementers {
		if !validBoundedText(implementer, maxMetadataBytes) {
			return false
		}
		if _, duplicate := seen[implementer]; duplicate {
			return false
		}
		seen[implementer] = struct{}{}
	}
	return true
}

func validBlindPackID(value string) bool {
	return validLowerHex(value, 16)
}

func validBlindLabel(value string) bool {
	return strings.HasPrefix(value, "blind-") && validLowerHex(strings.TrimPrefix(value, "blind-"), 8)
}

func validLowerHex(value string, byteCount int) bool {
	if len(value) != byteCount*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == byteCount
}

func randomHex(byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, err := cryptorand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func shuffleBlindCandidates(candidates []int) error {
	for index := len(candidates) - 1; index > 0; index-- {
		limit := big.NewInt(int64(index + 1))
		swapIndex, err := cryptorand.Int(cryptorand.Reader, limit)
		if err != nil {
			return err
		}
		other := int(swapIndex.Int64())
		candidates[index], candidates[other] = candidates[other], candidates[index]
	}
	return nil
}
