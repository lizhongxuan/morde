package evaluation

import (
	"reflect"
	"testing"
)

func TestSummarizeRunReportsCountsWithoutTreatingNAAsPass(t *testing.T) {
	suite := loadFixture(t)
	suite.Cases[1].Priority = "P1"
	run := validRun(suite)
	run.Manifest.ToolPermissions = []string{"workspace:read", "mcp:read"}

	const hardGateCriterion = "intent_constraints"
	hardGateIndex := rubricIndex(t, suite, hardGateCriterion)
	run.Cases[0].Judgments[hardGateIndex].Score = ScoreZero
	run.Cases[0].Judgments[hardGateIndex].Rationale = "synthetic hard gate failure"

	summary, err := SummarizeRun(suite, run)
	if err != nil {
		t.Fatal(err)
	}
	if summary.DataClass != "synthetic" || summary.Samples != (PrioritySampleCounts{P0: 7, P1: 1, Total: 8}) {
		t.Fatalf("unexpected sample counts: data_class=%q samples=%+v", summary.DataClass, summary.Samples)
	}

	gateScores := summary.Criteria[hardGateIndex].Scores
	if gateScores.ScoreZero != 1 || gateScores.ScoreOne != len(suite.Cases)-1 || gateScores.NotApplicable != 0 ||
		gateScores.ApplicableDenominator != len(suite.Cases) || gateScores.TotalDenominator != len(suite.Cases) {
		t.Fatalf("unexpected hard-gate score distribution: %+v", gateScores)
	}
	if len(summary.P0HardGateFailures) != 1 || summary.P0HardGateFailures[0] != (GateFailure{CaseID: suite.Cases[0].ID, CriterionID: hardGateCriterion}) {
		t.Fatalf("unexpected P0 hard-gate failures: %+v", summary.P0HardGateFailures)
	}

	sideEffectIndex := rubricIndex(t, suite, "side_effect_control")
	naCount := 0
	for _, testCase := range suite.Cases {
		if testCase.Expectations[sideEffectIndex].Expected == ScoreNA {
			naCount++
		}
	}
	distribution := summary.Criteria[sideEffectIndex].Scores
	if distribution.NotApplicable != naCount || distribution.ApplicableDenominator != len(suite.Cases)-naCount ||
		distribution.TotalDenominator != len(suite.Cases) || distribution.ScoreOne+distribution.ScoreZero != distribution.ApplicableDenominator {
		t.Fatalf("N/A scores must be reported separately from the applicable denominator: %+v N/A cases=%d", distribution, naCount)
	}

	ordinaryReply := tagSummary(t, summary, "ordinary_reply")
	if ordinaryReply.CaseCount != 1 {
		t.Fatalf("unexpected ordinary_reply case count: %d", ordinaryReply.CaseCount)
	}
	ordinarySideEffect := ordinaryReply.Criteria[sideEffectIndex].Scores
	if ordinarySideEffect.NotApplicable != 1 || ordinarySideEffect.ApplicableDenominator != 0 || ordinarySideEffect.TotalDenominator != 1 {
		t.Fatalf("tag summary counted N/A as applicable: %+v", ordinarySideEffect)
	}
	ordinaryGate := ordinaryReply.Criteria[hardGateIndex].Scores
	if ordinaryGate.ScoreZero != 1 || ordinaryGate.ScoreOne != 0 || ordinaryGate.TotalDenominator != ordinaryReply.CaseCount {
		t.Fatalf("tag summary did not include the tagged case score: %+v", ordinaryGate)
	}
	for _, tag := range summary.Tags {
		for _, criterion := range tag.Criteria {
			if criterion.Scores.TotalDenominator != tag.CaseCount {
				t.Fatalf("tag %q criterion %q denominator=%d, want %d", tag.Tag, criterion.ID, criterion.Scores.TotalDenominator, tag.CaseCount)
			}
		}
	}

	provenance := summary.Provenance
	if provenance.Provider != run.Manifest.Provider || provenance.Model != run.Manifest.Model ||
		provenance.PromptVersion != run.Manifest.PromptVersion || provenance.SkillVersion != run.Manifest.SkillVersion ||
		provenance.DSHVersion != run.Manifest.DSHVersion || provenance.BuildSHA != run.Manifest.BuildSHA ||
		!reflect.DeepEqual(provenance.ToolPermissions, []string{"mcp:read", "workspace:read"}) {
		t.Fatalf("run provenance was not preserved: %+v", provenance)
	}
	if run.Manifest.ToolPermissions[0] != "workspace:read" {
		t.Fatal("summary sorted permissions by mutating the source run")
	}
	if summary.Metrics.Latency != "not_recorded" || summary.Metrics.ActualCost != "not_recorded" {
		t.Fatalf("unavailable latency and actual cost must be explicit: %+v", summary.Metrics)
	}
}

func TestSummarizeRunRejectsInvalidOrIncompleteRun(t *testing.T) {
	suite := loadFixture(t)
	tests := map[string]func(*Run){
		"incomplete cases": func(run *Run) { run.Cases = run.Cases[:len(run.Cases)-1] },
		"invalid judgment evidence": func(run *Run) {
			run.Cases[0].Judgments[0].Evidence[0] = EvidenceReference{Kind: "evidence_id", ID: "missing"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			run := validRun(suite)
			mutate(&run)
			summary, err := SummarizeRun(suite, run)
			if err == nil {
				t.Fatal("expected invalid or incomplete run to be rejected")
			}
			if !reflect.DeepEqual(summary, RunSummary{}) {
				t.Fatalf("invalid run must not produce a partial report: %+v", summary)
			}
		})
	}
	invalidSuite := cloneSuite(t, suite)
	invalidSuite.Cases[0].Tags = []string{""}
	summary, err := SummarizeRun(invalidSuite, validRun(suite))
	if err == nil {
		t.Fatal("expected invalid suite to be rejected")
	}
	if !reflect.DeepEqual(summary, RunSummary{}) {
		t.Fatalf("invalid suite must not produce a partial report: %+v", summary)
	}
}

func rubricIndex(t *testing.T, suite Suite, id string) int {
	t.Helper()
	for index, criterion := range suite.Rubric {
		if criterion.ID == id {
			return index
		}
	}
	t.Fatalf("fixture is missing rubric %q", id)
	return -1
}

func tagSummary(t *testing.T, summary RunSummary, tag string) TagSummary {
	t.Helper()
	for _, candidate := range summary.Tags {
		if candidate.Tag == tag {
			return candidate
		}
	}
	t.Fatalf("summary is missing tag %q", tag)
	return TagSummary{}
}
