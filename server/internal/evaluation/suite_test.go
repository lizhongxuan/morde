package evaluation

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadSuiteFrozenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/suite-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite, err := LoadSuite(raw)
	if err != nil {
		t.Fatal(err)
	}
	if suite.Version != "morde-ai-conversation-synth-v1.0.0" || suite.DataClass != "synthetic" || len(suite.Cases) != 8 || len(suite.Rubric) != 7 {
		t.Fatalf("unexpected frozen suite shape: version=%q data_class=%q cases=%d rubric=%d", suite.Version, suite.DataClass, len(suite.Cases), len(suite.Rubric))
	}
	for _, id := range requiredRubricIDs() {
		found := false
		for _, criterion := range suite.Rubric {
			if criterion.ID == id {
				found = true
				if criterion.HardGate != requiredHardGate(id) {
					t.Errorf("criterion %q has hard_gate=%t", id, criterion.HardGate)
				}
			}
		}
		if !found {
			t.Errorf("suite is missing rubric %q", id)
		}
	}
	if _, err := LoadSuite([]byte(`{"version":"v1","unknown":true}`)); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	if _, err := LoadSuite(append(append([]byte(nil), raw...), []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
	if _, err := LoadSuite([]byte("{")); err == nil {
		t.Fatal("malformed JSON must be rejected")
	}
}

func TestValidateSuiteRejectsInvalidContracts(t *testing.T) {
	base := loadFixture(t)
	tests := map[string]func(*Suite){
		"missing version":           func(s *Suite) { s.Version = " " },
		"non-synthetic data":        func(s *Suite) { s.DataClass = "user" },
		"missing rubric":            func(s *Suite) { s.Rubric = s.Rubric[:len(s.Rubric)-1] },
		"rubric order":              func(s *Suite) { s.Rubric[0], s.Rubric[1] = s.Rubric[1], s.Rubric[0] },
		"rubric name":               func(s *Suite) { s.Rubric[0].Name = strings.Repeat("x", maxMetadataBytes+1) },
		"hard gate weakened":        func(s *Suite) { s.Rubric[1].HardGate = false },
		"invalid case id":           func(s *Suite) { s.Cases[0].ID = "AIC-Z-01" },
		"invalid priority":          func(s *Suite) { s.Cases[0].Priority = "P2" },
		"invalid input":             func(s *Suite) { s.Cases[0].Input = "  padded" },
		"invalid expected behavior": func(s *Suite) { s.Cases[0].Expected = "" },
		"duplicate case":            func(s *Suite) { s.Cases[1].ID = s.Cases[0].ID },
		"invalid tag":               func(s *Suite) { s.Cases[0].Tags = []string{""} },
		"duplicate tag":             func(s *Suite) { s.Cases[0].Tags = []string{"ordinary_reply", "ordinary_reply"} },
		"missing coverage":          func(s *Suite) { s.Cases[0].Tags = nil },
		"expectation order":         func(s *Suite) { s.Cases[0].Expectations[0].CriterionID = "completion" },
		"invalid score":             func(s *Suite) { s.Cases[0].Expectations[0].Expected = "2" },
		"N/A without case reason": func(s *Suite) {
			s.Cases[0].Expectations[0].Expected = ScoreNA
			s.Cases[0].Expectations[0].NotApplicableReason = " "
		},
		"reason on applicable item": func(s *Suite) { s.Cases[0].Expectations[0].NotApplicableReason = "unneeded" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			suite := cloneSuite(t, base)
			mutate(&suite)
			if err := ValidateSuite(suite); err == nil {
				t.Fatal("expected invalid suite to fail validation")
			}
		})
	}
}

func TestValidateRunAndHardGateFailures(t *testing.T) {
	suite := loadFixture(t)
	run := validRun(suite)
	if err := ValidateRun(suite, run); err != nil {
		t.Fatal(err)
	}
	failures, err := HardGateFailures(suite, run)
	if err != nil || len(failures) != 0 {
		t.Fatalf("expected clean hard gates, failures=%v err=%v", failures, err)
	}
	run.Cases[0].Judgments[0].Score = ScoreZero
	run.Cases[0].Judgments[0].Rationale = "synthetic hard gate failure"
	failures, err = HardGateFailures(suite, run)
	if err != nil || len(failures) != 1 || failures[0].CaseID != suite.Cases[0].ID || failures[0].CriterionID != "intent_constraints" {
		t.Fatalf("expected one independent hard gate failure, failures=%v err=%v", failures, err)
	}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "total_score") || strings.Contains(string(encoded), "overall_score") {
		t.Fatalf("run must not contain a subjective aggregate: %s", encoded)
	}
}

func TestValidateRunRejectsInvalidResultsAndEvidence(t *testing.T) {
	suite := loadFixture(t)
	tests := map[string]func(*Run){
		"missing provider":               func(r *Run) { r.Manifest.Provider = "" },
		"wrong suite version":            func(r *Run) { r.Manifest.SuiteVersion = "other" },
		"invalid build sha":              func(r *Run) { r.Manifest.BuildSHA = "abc" },
		"missing tool permissions field": func(r *Run) { r.Manifest.ToolPermissions = nil },
		"missing parameter field":        func(r *Run) { r.Manifest.Parameters = nil },
		"duplicate tool permission":      func(r *Run) { r.Manifest.ToolPermissions = []string{"workspace:read", "workspace:read"} },
		"missing start time":             func(r *Run) { r.Manifest.StartedAt = time.Time{} },
		"missing case":                   func(r *Run) { r.Cases = r.Cases[:len(r.Cases)-1] },
		"case order":                     func(r *Run) { r.Cases[0], r.Cases[1] = r.Cases[1], r.Cases[0] },
		"invalid output UTF-8":           func(r *Run) { r.Cases[0].Output = string([]byte{0xff}) },
		"too much output":                func(r *Run) { r.Cases[0].Output = strings.Repeat("x", maxRunOutputBytes+1) },
		"missing judgment":               func(r *Run) { r.Cases[0].Judgments = r.Cases[0].Judgments[:len(r.Cases[0].Judgments)-1] },
		"duplicate evidence":             func(r *Run) { r.Cases[0].Evidence = append(r.Cases[0].Evidence, r.Cases[0].Evidence[0]) },
		"invalid evidence digest":        func(r *Run) { r.Cases[0].Evidence[0].SHA256 = "bad" },
		"oversized evidence excerpt":     func(r *Run) { r.Cases[0].Evidence[0].Excerpt = strings.Repeat("x", maxEvidenceExcerpt+1) },
		"invalid evidence excerpt":       func(r *Run) { r.Cases[0].Evidence[0].Excerpt = string([]byte{0xff}) },
		"judgment order":                 func(r *Run) { r.Cases[0].Judgments[0].CriterionID = "completion" },
		"invalid judgment score":         func(r *Run) { r.Cases[0].Judgments[0].Score = "2" },
		"missing rationale":              func(r *Run) { r.Cases[0].Judgments[0].Rationale = " " },
		"missing evidence anchor":        func(r *Run) { r.Cases[0].Judgments[0].Evidence = nil },
		"unknown evidence id": func(r *Run) {
			r.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "evidence_id", ID: "missing"}
		},
		"out of range output span": func(r *Run) {
			r.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "output_span", StartByte: 0, EndByte: 500}
		},
		"split UTF-8 output span": func(r *Run) {
			r.Cases[0].Output = "中文"
			r.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "output_span", StartByte: 1, EndByte: 3}
		},
		"unknown reference kind": func(r *Run) { r.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "other"} },
		"invented N/A":           func(r *Run) { r.Cases[0].Judgments[0].Score = ScoreNA },
		"missing expected N/A":   func(r *Run) { r.Cases[0].Judgments[4].Score = ScoreOne },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			run := validRun(suite)
			mutate(&run)
			if err := ValidateRun(suite, run); err == nil {
				t.Fatal("expected invalid run to fail validation")
			}
		})
	}
	if _, err := HardGateFailures(suite, Run{}); err == nil {
		t.Fatal("hard gate evaluation must reject structurally invalid runs")
	}
}

func TestComparisonKeyFreezesConfiguration(t *testing.T) {
	suite := loadFixture(t)
	firstRun := validRun(suite)
	secondRun := validRun(suite)
	first := firstRun.Manifest
	second := first
	second.RunID = "different-run"
	second.StartedAt = second.StartedAt.Add(time.Hour)
	second.ToolPermissions = []string{"workspace:read", "mcp:read"}
	first.ToolPermissions = []string{"mcp:read", "workspace:read"}
	secondRun.Manifest = second
	for index := range firstRun.Cases {
		if !reflect.DeepEqual(firstRun.Cases[index].Judgments, secondRun.Cases[index].Judgments) {
			t.Fatalf("same frozen configuration produced different per-rubric results for case %q", firstRun.Cases[index].CaseID)
		}
	}
	key1, err := ComparisonKey(first)
	if err != nil {
		t.Fatal(err)
	}
	key2, err := ComparisonKey(second)
	if err != nil || key1 != key2 {
		t.Fatalf("run identity/time and permission order must not affect comparison key: %q %q err=%v", key1, key2, err)
	}
	second.PromptVersion = "prompt-v2"
	key3, err := ComparisonKey(second)
	if err != nil || key3 == key1 {
		t.Fatalf("prompt change must create a different comparison key: %q %q err=%v", key1, key3, err)
	}
	second.PromptVersion = first.PromptVersion
	second.ToolPermissions = []string{}
	if _, err := ComparisonKey(second); err != nil {
		t.Fatalf("an explicitly recorded empty permission set is valid: %v", err)
	}
}

func TestValidationPrimitives(t *testing.T) {
	for _, id := range []string{"AIC-H-01", "AIC-C-42", "AIC-E-07", "PC-CHAT-001", "PC-CHAT-024", "CLS-01", "RPL-02"} {
		if !validCaseID(id) {
			t.Errorf("valid case id rejected: %q", id)
		}
	}
	for _, id := range []string{"AIC-X-01", "AIC-H-1", "AIC-H-001", "AIC-H-aa", "PC-CHAT-1", "PC-CHAT-0011", "CLS-001", "XYZ-01"} {
		if validCaseID(id) {
			t.Errorf("invalid case id accepted: %q", id)
		}
	}
	if !validBoundedText("ok", 2) || validBoundedText(" ok", 8) || validBoundedText("x\x00", 8) || validBoundedText(string([]byte{0xff}), 8) {
		t.Fatal("bounded text validation did not enforce its contract")
	}
	if !validSHA256(strings.Repeat("a", 64)) || validSHA256(strings.Repeat("A", 64)) || validSHA256("xyz") {
		t.Fatal("SHA-256 validation did not enforce lowercase hex")
	}
	if !validScore(ScoreZero) || !validScore(ScoreOne) || !validScore(ScoreNA) || validScore("maybe") {
		t.Fatal("score validation did not enforce atomic values")
	}
	if !reflect.DeepEqual(requiredCoverageTags(), []string{"ordinary_reply", "evidence", "independent_review", "clarification", "approval", "replay", "tool_unavailable", "prompt_injection"}) {
		t.Fatal("required suite coverage changed unexpectedly")
	}
	if !reflect.DeepEqual(requiredLiveCoverageTags(), []string{"pc-chat", "classification", "error_recovery", "approval", "cost", "multi_turn_merge", "replay"}) {
		t.Fatal("required live suite coverage changed unexpectedly")
	}
}

// TestLoadLiveSuiteInventory pins the weekly live suite (evaluation
// requirements §4.2): 44 cases = PC-CHAT 001-024 + 20 specialist cases,
// version morde-ai-conversation-live-v1.1.0, data_class live-scripted,
// scored separately from the 8 synthetic contract cases. The inventory
// is exact: dropping or renaming a case must fail here, not silently
// shrink the weekly run.
func TestLoadLiveSuiteInventory(t *testing.T) {
	raw, err := os.ReadFile("testdata/suite-live-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite, err := LoadSuite(raw)
	if err != nil {
		t.Fatal(err)
	}
	if suite.Version != "morde-ai-conversation-live-v1.1.0" || suite.DataClass != "live-scripted" || len(suite.Cases) != 44 || len(suite.Rubric) != 7 {
		t.Fatalf("unexpected live suite shape: version=%q data_class=%q cases=%d rubric=%d", suite.Version, suite.DataClass, len(suite.Cases), len(suite.Rubric))
	}

	wantIDs := map[string]bool{}
	for n := 1; n <= 24; n++ {
		wantIDs[fmt.Sprintf("PC-CHAT-%03d", n)] = true
	}
	for _, id := range []string{"CLS-01", "CLS-02", "CLS-03", "CLS-04", "CLS-05",
		"REC-01", "REC-02", "REC-03", "REC-04",
		"APR-01", "APR-02", "APR-03", "APR-04",
		"CST-01", "CST-02", "MRG-01", "MRG-02", "MRG-03", "RPL-01", "RPL-02"} {
		wantIDs[id] = true
	}
	// P0 per the evaluation plan (§4.3 smoke set + P0-1/P0-2 acceptance):
	// PC-001/002/008/012/016/017/018, CLS-01, REC-01-03, APR-01-03, MRG-01.
	wantP0 := map[string]bool{
		"PC-CHAT-001": true, "PC-CHAT-002": true, "PC-CHAT-008": true, "PC-CHAT-012": true,
		"PC-CHAT-016": true, "PC-CHAT-017": true, "PC-CHAT-018": true,
		"CLS-01": true, "REC-01": true, "REC-02": true, "REC-03": true,
		"APR-01": true, "APR-02": true, "APR-03": true, "MRG-01": true,
	}
	// The approval operation script exists exactly on the six approval
	// cases that click approve/reject; the cost cap exists exactly on
	// the two cost cases (micro-units, same unit as LimitMicros).
	wantApprovalScript := map[string]bool{
		"PC-CHAT-017": true, "PC-CHAT-018": true,
		"APR-01": true, "APR-02": true, "APR-03": true, "APR-04": true,
	}
	wantCostBudget := map[string]bool{"CST-01": true, "CST-02": true}
	// Environment preparation is recorded on the cases whose source
	// states a precondition or which need fault injection.
	wantSetup := map[string]bool{
		"PC-CHAT-005": true, "PC-CHAT-007": true, "PC-CHAT-009": true, "PC-CHAT-010": true,
		"PC-CHAT-011": true, "PC-CHAT-012": true, "PC-CHAT-016": true, "PC-CHAT-017": true,
		"PC-CHAT-018": true, "PC-CHAT-019": true, "PC-CHAT-022": true, "PC-CHAT-024": true,
		"CLS-01": true, "CLS-02": true, "CLS-03": true,
		"REC-01": true, "REC-02": true, "REC-03": true, "REC-04": true,
		"APR-01": true, "APR-02": true, "APR-03": true, "APR-04": true,
		"CST-01": true, "CST-02": true,
	}

	seen := map[string]bool{}
	for _, testCase := range suite.Cases {
		if !wantIDs[testCase.ID] {
			t.Errorf("unexpected live case id %q", testCase.ID)
		}
		if seen[testCase.ID] {
			t.Errorf("duplicate live case id %q", testCase.ID)
		}
		seen[testCase.ID] = true
		if wantP0[testCase.ID] != (testCase.Priority == "P0") {
			t.Errorf("case %s priority = %q, want P0=%t", testCase.ID, testCase.Priority, wantP0[testCase.ID])
		}
		if testCase.ID == "PC-CHAT-015" && testCase.Priority != "P2" {
			t.Errorf("case PC-CHAT-015 priority = %q, want P2 (inherited from the PC-CHAT source)", testCase.Priority)
		}
		if wantApprovalScript[testCase.ID] != (testCase.ApprovalScript != "") {
			t.Errorf("case %s approval_script present=%t, want %t", testCase.ID, testCase.ApprovalScript != "", wantApprovalScript[testCase.ID])
		}
		if wantCostBudget[testCase.ID] != (testCase.CostBudget != nil) {
			t.Errorf("case %s cost_budget present=%t, want %t", testCase.ID, testCase.CostBudget != nil, wantCostBudget[testCase.ID])
		}
		if testCase.CostBudget != nil && *testCase.CostBudget != 100000 {
			t.Errorf("case %s cost_budget = %v, want 100000 micro-units", testCase.ID, *testCase.CostBudget)
		}
		if wantSetup[testCase.ID] != (testCase.Setup != "") {
			t.Errorf("case %s setup present=%t, want %t", testCase.ID, testCase.Setup != "", wantSetup[testCase.ID])
		}
	}
	for id := range wantIDs {
		if !seen[id] {
			t.Errorf("live suite is missing case %q", id)
		}
	}

	// Live-only rules: P2 is accepted here but rejected on the synthetic
	// fixture; a zero cost budget fails; a live suite relabelled as
	// synthetic fails (its IDs and tags do not belong to the contract layer).
	live := cloneSuite(t, suite)
	live.Cases[0].CostBudget = new(float64)
	if err := ValidateSuite(live); err == nil {
		t.Fatal("zero cost budget must fail validation")
	}
	asSynthetic := cloneSuite(t, suite)
	asSynthetic.DataClass = "synthetic"
	if err := ValidateSuite(asSynthetic); err == nil {
		t.Fatal("live suite relabelled as synthetic must fail validation")
	}
}

func TestBuildBlindPackSeparatesMappingAndVerifiesOrder(t *testing.T) {
	suite := loadFixture(t)
	runs := []Run{validRun(suite), validRun(suite)}
	runs[1].Manifest.RunID = "run-002"
	runs[0].Manifest.Provider = "private-provider-marker"
	runs[0].Manifest.Model = "private-model-marker"
	pack, mapping, err := BuildBlindPack(suite, runs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBlindPack(suite, pack); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBlindMapping(suite, pack, mapping); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"private-provider-marker", "private-model-marker", "run-001", "run-002"} {
		if strings.Contains(string(encoded), hidden) {
			t.Errorf("blind pack leaked %q", hidden)
		}
	}
	if !strings.Contains(string(encoded), "synthetic source excerpt") || len(pack.Cases[0].Applicability) != len(suite.Rubric) || strings.Contains(string(encoded), `"expected":"1"`) {
		t.Fatal("blind pack must preserve evidence excerpts and case-specific applicability")
	}
	for _, assignment := range mapping.Assignments {
		if !strings.HasPrefix(assignment.Label, "blind-") || assignment.RunID == "" {
			t.Fatalf("invalid separate blind mapping: %#v", assignment)
		}
		for caseIndex, position := range assignment.Positions {
			if pack.Cases[caseIndex].Candidates[position].Label != assignment.Label {
				t.Fatalf("mapping does not reconstruct case %q randomized order", pack.Cases[caseIndex].CaseID)
			}
		}
	}
	if _, _, err := BuildBlindPack(suite, runs[:1]); err == nil {
		t.Fatal("blind pack requires at least two candidates")
	}
	if _, _, err := BuildBlindPack(suite, []Run{runs[0], runs[0]}); err == nil {
		t.Fatal("blind pack must reject duplicate run IDs")
	}
	badMapping := mapping
	badMapping.Assignments = append([]BlindAssignment(nil), mapping.Assignments...)
	badMapping.Assignments[0].Positions = append([]int(nil), mapping.Assignments[0].Positions...)
	badMapping.Assignments[0].Positions[0] = len(pack.Cases[0].Candidates)
	if err := ValidateBlindMapping(suite, pack, badMapping); err == nil {
		t.Fatal("blind mapping must reject unverifiable order")
	}
	if err := ValidateBlindPack(suite, BlindPack{ID: pack.ID, SuiteVersion: pack.SuiteVersion, Rubric: pack.Rubric, Cases: nil}); err == nil {
		t.Fatal("blind pack must reject incomplete cases")
	}
	badPack := pack
	badPack.Cases = append([]BlindCase(nil), pack.Cases...)
	badPack.Cases[0].Candidates = append([]BlindCandidate(nil), pack.Cases[0].Candidates...)
	badPack.Cases[0].Candidates[0].Label = "private-model-marker"
	if err := ValidateBlindPack(suite, badPack); err == nil {
		t.Fatal("blind pack must reject labels that expose candidate identity")
	}
	badApplicability := pack
	badApplicability.Cases = append([]BlindCase(nil), pack.Cases...)
	badApplicability.Cases[0].Applicability = append([]BlindApplicability(nil), pack.Cases[0].Applicability...)
	badApplicability.Cases[0].Applicability[0].Applicable = false
	if err := ValidateBlindPack(suite, badApplicability); err == nil {
		t.Fatal("blind pack must reject altered applicability")
	}
}

func TestBlindReviewRequiresEvidenceAndIndependentReviewers(t *testing.T) {
	suite := loadFixture(t)
	runs := []Run{validRun(suite), validRun(suite)}
	runs[1].Manifest.RunID = "run-002"
	pack, _, err := BuildBlindPack(suite, runs)
	if err != nil {
		t.Fatal(err)
	}
	label := pack.Cases[0].Candidates[0].Label
	review := makeBlindReview(suite, pack, label, "reviewer-a", ScoreOne)
	if err := ValidateBlindReview(suite, pack, review, []string{"implementer"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBlindReview(suite, pack, review, []string{"reviewer-a"}); err == nil {
		t.Fatal("implementer must not review their own work")
	}
	if err := ValidateBlindReview(suite, pack, review, nil); err == nil {
		t.Fatal("blind review must fail closed without an implementation roster")
	}
	swappedLabel := review
	swappedLabel.Candidate = pack.Cases[0].Candidates[1].Label
	if err := ValidateBlindReview(suite, pack, swappedLabel, []string{"implementer"}); err != nil {
		t.Fatalf("changing an anonymous label must not alter rubric judgments: %v", err)
	}
	tests := map[string]func(*BlindReview){
		"wrong pack":        func(r *BlindReview) { r.PackID = "other" },
		"unknown candidate": func(r *BlindReview) { r.Candidate = "blind-missing" },
		"missing case":      func(r *BlindReview) { r.Cases = r.Cases[:len(r.Cases)-1] },
		"invalid score":     func(r *BlindReview) { r.Cases[0].Judgments[0].Score = "2" },
		"missing evidence":  func(r *BlindReview) { r.Cases[0].Judgments[0].Evidence = nil },
		"wrong evidence": func(r *BlindReview) {
			r.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "evidence_id", ID: "missing"}
		},
		"invented N/A":         func(r *BlindReview) { r.Cases[0].Judgments[0].Score = ScoreNA },
		"missing expected N/A": func(r *BlindReview) { r.Cases[0].Judgments[4].Score = ScoreOne },
		"missing reviewer":     func(r *BlindReview) { r.ReviewerID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			invalid := makeBlindReview(suite, pack, label, "reviewer-a", ScoreOne)
			mutate(&invalid)
			if err := ValidateBlindReview(suite, pack, invalid, []string{"implementer"}); err == nil {
				t.Fatal("expected invalid blind review to fail")
			}
		})
	}
}

func TestBlindPanelRequiresIndependentReviewAndArbitration(t *testing.T) {
	suite := loadFixture(t)
	runs := []Run{validRun(suite), validRun(suite)}
	runs[1].Manifest.RunID = "run-002"
	pack, _, err := BuildBlindPack(suite, runs)
	if err != nil {
		t.Fatal(err)
	}
	firstLabel := pack.Cases[0].Candidates[0].Label
	secondLabel := pack.Cases[0].Candidates[1].Label
	reviews := []BlindReview{
		makeBlindReview(suite, pack, firstLabel, "reviewer-a", ScoreOne),
		makeBlindReview(suite, pack, firstLabel, "reviewer-b", ScoreZero),
		makeBlindReview(suite, pack, secondLabel, "reviewer-a", ScoreOne),
		makeBlindReview(suite, pack, secondLabel, "reviewer-b", ScoreOne),
	}
	if err := ValidateBlindPanel(suite, pack, reviews[:1], nil, []string{"implementer"}); err == nil {
		t.Fatal("each candidate needs two independent reviewers")
	}
	if err := ValidateBlindPanel(suite, pack, reviews, nil, []string{"implementer"}); err == nil {
		t.Fatal("unresolved score disagreement must block the panel")
	}
	arbitration := makeBlindArbitration(suite, pack, firstLabel, "arbitrator-c")
	if err := ValidateBlindPanel(suite, pack, reviews, []BlindArbitration{arbitration}, []string{"implementer"}); err != nil {
		t.Fatal(err)
	}
	arbitration.ArbitratorID = "reviewer-a"
	if err := ValidateBlindPanel(suite, pack, reviews, []BlindArbitration{arbitration}, []string{"implementer"}); err == nil {
		t.Fatal("arbitrator must be independent of reviewers")
	}
	arbitration.ArbitratorID = "arbitrator-c"
	if err := ValidateBlindPanel(suite, pack, reviews, []BlindArbitration{arbitration}, []string{"arbitrator-c"}); err == nil {
		t.Fatal("implementer must not arbitrate")
	}
	if err := ValidateBlindPanel(suite, pack, reviews, []BlindArbitration{arbitration}, nil); err == nil {
		t.Fatal("blind panel must fail closed without an implementation roster")
	}
	reviewedArbitrator := append(append([]BlindReview(nil), reviews...), makeBlindReview(suite, pack, secondLabel, "arbitrator-c", ScoreOne))
	if err := ValidateBlindPanel(suite, pack, reviewedArbitrator, []BlindArbitration{arbitration}, []string{"implementer"}); err == nil {
		t.Fatal("arbitrator must not review any candidate in the same panel")
	}
	if err := ValidateBlindPanel(suite, pack, append(reviews, reviews[0]), []BlindArbitration{makeBlindArbitration(suite, pack, firstLabel, "arbitrator-c")}, []string{"implementer"}); err == nil {
		t.Fatal("a reviewer cannot submit twice for one candidate")
	}
	if err := ValidateBlindPanel(suite, pack, reviews, []BlindArbitration{makeBlindArbitration(suite, pack, firstLabel, "arbitrator-c"), makeBlindArbitration(suite, pack, "blind-missing", "arbitrator-d")}, []string{"implementer"}); err == nil {
		t.Fatal("arbitration cannot refer to an unknown candidate")
	}
	noDisagreement := []BlindReview{
		makeBlindReview(suite, pack, firstLabel, "reviewer-a", ScoreOne),
		makeBlindReview(suite, pack, firstLabel, "reviewer-b", ScoreOne),
		makeBlindReview(suite, pack, secondLabel, "reviewer-a", ScoreOne),
		makeBlindReview(suite, pack, secondLabel, "reviewer-b", ScoreOne),
	}
	if err := ValidateBlindPanel(suite, pack, noDisagreement, []BlindArbitration{makeBlindArbitration(suite, pack, firstLabel, "arbitrator-c")}, []string{"implementer"}); err == nil {
		t.Fatal("unneeded arbitration must be rejected")
	}
}

func loadFixture(t *testing.T) Suite {
	t.Helper()
	raw, err := os.ReadFile("testdata/suite-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite, err := LoadSuite(raw)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func validRun(suite Suite) Run {
	run := Run{Manifest: RunManifest{
		RunID: "run-001", SuiteVersion: suite.Version, Provider: "synthetic", Model: "synthetic-v1",
		PromptVersion: "prompt-v1", SkillVersion: "skill-v1", DSHVersion: "dsh-v1",
		BuildSHA: strings.Repeat("a", 64), ToolPermissions: []string{"mcp:read", "workspace:read"},
		Parameters: map[string]string{"temperature": "0", "max_tokens": "512"}, StartedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}}
	for _, testCase := range suite.Cases {
		result := CaseResult{CaseID: testCase.ID, Output: "synthetic result with evidence", Evidence: []Evidence{{ID: "source-1", Kind: "synthetic_source", SHA256: strings.Repeat("b", 64), Excerpt: "synthetic source excerpt"}}}
		for index, expectation := range testCase.Expectations {
			judgment := Judgment{CriterionID: expectation.CriterionID, Score: expectation.Expected, Rationale: "matches the frozen synthetic expectation"}
			if expectation.Expected == ScoreNA {
				judgment.Evidence = []EvidenceReference{{Kind: "suite_na_reason", ID: testCase.ID + "/" + expectation.CriterionID}}
			} else if index%2 == 0 {
				judgment.Evidence = []EvidenceReference{{Kind: "output_span", StartByte: 0, EndByte: len("synthetic")}}
			} else {
				judgment.Evidence = []EvidenceReference{{Kind: "evidence_id", ID: "source-1"}}
			}
			result.Judgments = append(result.Judgments, judgment)
		}
		run.Cases = append(run.Cases, result)
	}
	return run
}

func cloneSuite(t *testing.T, suite Suite) Suite {
	t.Helper()
	encoded, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	var clone Suite
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func makeBlindReview(suite Suite, pack BlindPack, candidate, reviewer string, intentScore Score) BlindReview {
	review := BlindReview{PackID: pack.ID, Candidate: candidate, ReviewerID: reviewer, SubmittedAt: time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)}
	for _, testCase := range suite.Cases {
		caseReview := BlindCaseReview{CaseID: testCase.ID}
		for _, expectation := range testCase.Expectations {
			judgment := Judgment{CriterionID: expectation.CriterionID, Score: expectation.Expected, Rationale: "evidence matches the case constraint"}
			if expectation.CriterionID == "intent_constraints" {
				judgment.Score = intentScore
			}
			if judgment.Score == ScoreNA {
				judgment.Evidence = []EvidenceReference{{Kind: "suite_na_reason", ID: testCase.ID + "/" + expectation.CriterionID}}
			} else {
				judgment.Evidence = []EvidenceReference{{Kind: "output_span", StartByte: 0, EndByte: len("synthetic")}}
			}
			caseReview.Judgments = append(caseReview.Judgments, judgment)
		}
		review.Cases = append(review.Cases, caseReview)
	}
	return review
}

func makeBlindArbitration(suite Suite, pack BlindPack, candidate, arbitrator string) BlindArbitration {
	review := makeBlindReview(suite, pack, candidate, arbitrator, ScoreOne)
	return BlindArbitration{PackID: pack.ID, Candidate: candidate, ArbitratorID: arbitrator,
		Reason: "independent adjudication of cited output evidence", ResolvedAt: review.SubmittedAt, Cases: review.Cases}
}
