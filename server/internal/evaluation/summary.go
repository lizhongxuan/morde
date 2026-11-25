package evaluation

import (
	"fmt"
	"sort"
	"time"
)

// ScoreDistribution reports observed scores and both useful denominators.
// N/A entries are counted but never included in ApplicableDenominator.
type ScoreDistribution struct {
	ScoreZero             int `json:"score_0"`
	ScoreOne              int `json:"score_1"`
	NotApplicable         int `json:"score_na"`
	ApplicableDenominator int `json:"applicable_denominator"`
	TotalDenominator      int `json:"total_denominator"`
}

// CriterionSummary is the score distribution for one rubric criterion.
type CriterionSummary struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	HardGate bool              `json:"hard_gate"`
	Scores   ScoreDistribution `json:"scores"`
}

// TagSummary reports criterion score distributions for cases with one tag.
type TagSummary struct {
	Tag       string             `json:"tag"`
	CaseCount int                `json:"case_count"`
	Criteria  []CriterionSummary `json:"criteria"`
}

// PrioritySampleCounts reports the run's P0/P1 sample sizes.
type PrioritySampleCounts struct {
	P0    int `json:"p0"`
	P1    int `json:"p1"`
	Total int `json:"total"`
}

// RunProvenance records the configuration needed to identify the evaluated run.
type RunProvenance struct {
	RunID           string    `json:"run_id"`
	SuiteVersion    string    `json:"suite_version"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	PromptVersion   string    `json:"prompt_version"`
	SkillVersion    string    `json:"skill_version"`
	DSHVersion      string    `json:"dsh_version"`
	BuildSHA        string    `json:"build_sha"`
	ToolPermissions []string  `json:"tool_permissions"`
	StartedAt       time.Time `json:"started_at"`
}

// MetricAvailability makes missing measurements explicit rather than
// estimating them from arbitrary run parameters.
type MetricAvailability struct {
	Latency    string `json:"latency"`
	ActualCost string `json:"actual_cost"`
}

// RunSummary contains deterministic descriptive counts, not an overall score
// or pass threshold. Every summary is derived from a complete, validated run.
type RunSummary struct {
	DataClass          string               `json:"data_class"`
	Samples            PrioritySampleCounts `json:"samples"`
	Criteria           []CriterionSummary   `json:"criteria"`
	Tags               []TagSummary         `json:"tags"`
	P0HardGateFailures []GateFailure        `json:"p0_hard_gate_failures"`
	Provenance         RunProvenance        `json:"provenance"`
	Metrics            MetricAvailability   `json:"metrics"`
}

// SummarizeRun returns counts for a complete evaluation run. It validates both
// the suite and the full run before producing any report; callers cannot use a
// partial run to create a plausible-looking summary.
func SummarizeRun(suite Suite, run Run) (RunSummary, error) {
	if err := ValidateRun(suite, run); err != nil {
		return RunSummary{}, fmt.Errorf("validate evaluation run before summary: %w", err)
	}

	summary := RunSummary{
		DataClass:          suite.DataClass,
		Samples:            PrioritySampleCounts{Total: len(suite.Cases)},
		Criteria:           make([]CriterionSummary, len(suite.Rubric)),
		P0HardGateFailures: make([]GateFailure, 0),
		Provenance: RunProvenance{
			RunID: run.Manifest.RunID, SuiteVersion: run.Manifest.SuiteVersion,
			Provider: run.Manifest.Provider, Model: run.Manifest.Model,
			PromptVersion: run.Manifest.PromptVersion, SkillVersion: run.Manifest.SkillVersion,
			DSHVersion: run.Manifest.DSHVersion, BuildSHA: run.Manifest.BuildSHA,
			ToolPermissions: append([]string(nil), run.Manifest.ToolPermissions...),
			StartedAt:       run.Manifest.StartedAt,
		},
		Metrics: MetricAvailability{Latency: "not_recorded", ActualCost: "not_recorded"},
	}
	sort.Strings(summary.Provenance.ToolPermissions)

	for rubricIndex, criterion := range suite.Rubric {
		summary.Criteria[rubricIndex] = CriterionSummary{ID: criterion.ID, Name: criterion.Name, HardGate: criterion.HardGate}
	}

	tagCaseCounts := make(map[string]int)
	tagScores := make(map[string][]ScoreDistribution)
	for caseIndex, testCase := range suite.Cases {
		result := run.Cases[caseIndex]
		if testCase.Priority == "P0" {
			summary.Samples.P0++
		} else {
			summary.Samples.P1++
		}

		for _, tag := range testCase.Tags {
			tagCaseCounts[tag]++
			distributions, exists := tagScores[tag]
			if !exists {
				distributions = make([]ScoreDistribution, len(suite.Rubric))
				tagScores[tag] = distributions
			}
			for rubricIndex, judgment := range result.Judgments {
				distributions[rubricIndex].add(judgment.Score)
			}
		}

		for rubricIndex, judgment := range result.Judgments {
			summary.Criteria[rubricIndex].Scores.add(judgment.Score)
			criterion := suite.Rubric[rubricIndex]
			expectation := testCase.Expectations[rubricIndex]
			if testCase.Priority == "P0" && criterion.HardGate && expectation.Expected == ScoreOne && judgment.Score == ScoreZero {
				summary.P0HardGateFailures = append(summary.P0HardGateFailures, GateFailure{CaseID: testCase.ID, CriterionID: criterion.ID})
			}
		}
	}

	tags := make([]string, 0, len(tagScores))
	for tag := range tagScores {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	summary.Tags = make([]TagSummary, 0, len(tags))
	for _, tag := range tags {
		criteria := make([]CriterionSummary, len(suite.Rubric))
		for rubricIndex, criterion := range suite.Rubric {
			criteria[rubricIndex] = CriterionSummary{
				ID: criterion.ID, Name: criterion.Name, HardGate: criterion.HardGate,
				Scores: tagScores[tag][rubricIndex],
			}
		}
		summary.Tags = append(summary.Tags, TagSummary{Tag: tag, CaseCount: tagCaseCounts[tag], Criteria: criteria})
	}

	return summary, nil
}

func (distribution *ScoreDistribution) add(score Score) {
	distribution.TotalDenominator++
	switch score {
	case ScoreZero:
		distribution.ScoreZero++
		distribution.ApplicableDenominator++
	case ScoreOne:
		distribution.ScoreOne++
		distribution.ApplicableDenominator++
	case ScoreNA:
		distribution.NotApplicable++
	}
}
