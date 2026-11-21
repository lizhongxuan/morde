package evaluation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Score string

const (
	ScoreZero Score = "0"
	ScoreOne  Score = "1"
	ScoreNA   Score = "N/A"
)

type Suite struct {
	Version   string            `json:"version"`
	DataClass string            `json:"data_class"`
	Rubric    []RubricCriterion `json:"rubric"`
	Cases     []Case            `json:"cases"`
}

type RubricCriterion struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	HardGate bool   `json:"hard_gate"`
}

type Case struct {
	ID           string            `json:"id"`
	Priority     string            `json:"priority"`
	Tags         []string          `json:"tags"`
	Input        string            `json:"input"`
	Expected     string            `json:"expected_behavior"`
	Expectations []CaseExpectation `json:"expectations"`
}

type CaseExpectation struct {
	CriterionID         string `json:"criterion_id"`
	Expected            Score  `json:"expected"`
	NotApplicableReason string `json:"not_applicable_reason,omitempty"`
}

type RunManifest struct {
	RunID           string            `json:"run_id"`
	SuiteVersion    string            `json:"suite_version"`
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	PromptVersion   string            `json:"prompt_version"`
	SkillVersion    string            `json:"skill_version"`
	DSHVersion      string            `json:"dsh_version"`
	BuildSHA        string            `json:"build_sha"`
	ToolPermissions []string          `json:"tool_permissions"`
	Parameters      map[string]string `json:"parameters"`
	StartedAt       time.Time         `json:"started_at"`
}

type Evidence struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	Excerpt string `json:"excerpt,omitempty"`
}

type EvidenceReference struct {
	Kind      string `json:"kind"`
	ID        string `json:"id,omitempty"`
	StartByte int    `json:"start_byte,omitempty"`
	EndByte   int    `json:"end_byte,omitempty"`
}

type Judgment struct {
	CriterionID string              `json:"criterion_id"`
	Score       Score               `json:"score"`
	Rationale   string              `json:"rationale"`
	Evidence    []EvidenceReference `json:"evidence"`
}

type CaseResult struct {
	CaseID    string     `json:"case_id"`
	Output    string     `json:"output"`
	Evidence  []Evidence `json:"evidence"`
	Judgments []Judgment `json:"judgments"`
}

type Run struct {
	Manifest RunManifest  `json:"manifest"`
	Cases    []CaseResult `json:"cases"`
}

type GateFailure struct {
	CaseID      string `json:"case_id"`
	CriterionID string `json:"criterion_id"`
}

const (
	maxSuiteVersionBytes = 64
	maxCaseInputBytes    = 8192
	maxExpectedBytes     = 4096
	maxRunOutputBytes    = 1 << 20
	maxEvidenceExcerpt   = 8192
	maxMetadataBytes     = 256
)

func requiredRubricIDs() []string {
	return []string{"intent_constraints", "correctness_sources", "completion", "clarification_timing",
		"side_effect_control", "evidence_review", "presentation_replay"}
}

func requiredHardGate(id string) bool {
	return id == "intent_constraints" || id == "correctness_sources" || id == "side_effect_control" || id == "evidence_review"
}

func requiredCoverageTags() []string {
	return []string{"ordinary_reply", "evidence", "independent_review", "clarification",
		"approval", "replay", "tool_unavailable", "prompt_injection"}
}

func LoadSuite(raw []byte) (Suite, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var suite Suite
	if err := decoder.Decode(&suite); err != nil {
		return Suite{}, fmt.Errorf("decode evaluation suite: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Suite{}, errors.New("evaluation suite has trailing JSON")
		}
		return Suite{}, fmt.Errorf("decode evaluation suite trailer: %w", err)
	}
	if err := ValidateSuite(suite); err != nil {
		return Suite{}, err
	}
	return suite, nil
}

func ValidateSuite(suite Suite) error {
	rubricIDs := requiredRubricIDs()
	coverageTags := requiredCoverageTags()
	if !validBoundedText(suite.Version, maxSuiteVersionBytes) || suite.DataClass != "synthetic" || len(suite.Rubric) != len(rubricIDs) || len(suite.Cases) < len(coverageTags) {
		return errors.New("evaluation suite version, rubric, or case count is invalid")
	}
	rubric := make(map[string]RubricCriterion, len(suite.Rubric))
	for index, criterion := range suite.Rubric {
		if criterion.ID != rubricIDs[index] || criterion.HardGate != requiredHardGate(criterion.ID) || !validBoundedText(criterion.Name, maxMetadataBytes) {
			return fmt.Errorf("evaluation rubric criterion %d is invalid", index)
		}
		rubric[criterion.ID] = criterion
	}
	cases := make(map[string]struct{}, len(suite.Cases))
	coverage := make(map[string]struct{}, len(coverageTags))
	for _, testCase := range suite.Cases {
		if !validCaseID(testCase.ID) || (testCase.Priority != "P0" && testCase.Priority != "P1") ||
			!validBoundedText(testCase.Input, maxCaseInputBytes) || !validBoundedText(testCase.Expected, maxExpectedBytes) ||
			len(testCase.Expectations) != len(suite.Rubric) {
			return fmt.Errorf("evaluation case %q is invalid", testCase.ID)
		}
		if _, duplicate := cases[testCase.ID]; duplicate {
			return fmt.Errorf("evaluation case %q is duplicated", testCase.ID)
		}
		cases[testCase.ID] = struct{}{}
		seenTags := make(map[string]struct{}, len(testCase.Tags))
		for _, tag := range testCase.Tags {
			if !validBoundedText(tag, maxMetadataBytes) {
				return fmt.Errorf("evaluation case %q has an invalid tag", testCase.ID)
			}
			if _, duplicate := seenTags[tag]; duplicate {
				return fmt.Errorf("evaluation case %q repeats tag %q", testCase.ID, tag)
			}
			seenTags[tag] = struct{}{}
			coverage[tag] = struct{}{}
		}
		for index, expectation := range testCase.Expectations {
			if expectation.CriterionID != suite.Rubric[index].ID ||
				(expectation.Expected != ScoreZero && expectation.Expected != ScoreOne && expectation.Expected != ScoreNA) {
				return fmt.Errorf("evaluation case %q has invalid expectation %d", testCase.ID, index)
			}
			if expectation.Expected == ScoreNA {
				if !validBoundedText(expectation.NotApplicableReason, maxMetadataBytes) {
					return fmt.Errorf("evaluation case %q has an unsupported N/A expectation", testCase.ID)
				}
			} else if expectation.NotApplicableReason != "" {
				return fmt.Errorf("evaluation case %q assigns an N/A reason to an applicable criterion", testCase.ID)
			}
		}
	}
	for _, tag := range coverageTags {
		if _, ok := coverage[tag]; !ok {
			return fmt.Errorf("evaluation suite is missing coverage tag %q", tag)
		}
	}
	return nil
}

func ValidateRun(suite Suite, run Run) error {
	if err := ValidateSuite(suite); err != nil {
		return err
	}
	if err := validateManifest(run.Manifest, suite.Version); err != nil {
		return err
	}
	if len(run.Cases) != len(suite.Cases) {
		return errors.New("evaluation run does not contain every suite case")
	}
	for index, testCase := range suite.Cases {
		result := run.Cases[index]
		if result.CaseID != testCase.ID || len(result.Output) > maxRunOutputBytes || !utf8.ValidString(result.Output) ||
			len(result.Judgments) != len(suite.Rubric) {
			return fmt.Errorf("evaluation result for case %q is invalid", testCase.ID)
		}
		evidence := make(map[string]struct{}, len(result.Evidence))
		for _, item := range result.Evidence {
			if !validEvidence(item) {
				return fmt.Errorf("evaluation case %q has invalid evidence", testCase.ID)
			}
			if _, duplicate := evidence[item.ID]; duplicate {
				return fmt.Errorf("evaluation case %q repeats evidence %q", testCase.ID, item.ID)
			}
			evidence[item.ID] = struct{}{}
		}
		for rubricIndex, criterion := range suite.Rubric {
			judgment := result.Judgments[rubricIndex]
			expectation := testCase.Expectations[rubricIndex]
			if judgment.CriterionID != criterion.ID || !validScore(judgment.Score) ||
				!validBoundedText(judgment.Rationale, maxExpectedBytes) || len(judgment.Evidence) == 0 {
				return fmt.Errorf("evaluation case %q has invalid judgment for %q", testCase.ID, criterion.ID)
			}
			if (expectation.Expected == ScoreNA) != (judgment.Score == ScoreNA) {
				return fmt.Errorf("evaluation case %q invents or omits N/A for %q", testCase.ID, criterion.ID)
			}
			for _, ref := range judgment.Evidence {
				if !validJudgmentEvidenceRef(ref, result.Output, evidence, testCase.ID, criterion.ID, judgment.Score == ScoreNA) {
					return fmt.Errorf("evaluation case %q has invalid evidence reference for %q", testCase.ID, criterion.ID)
				}
			}
		}
	}
	return nil
}

func validEvidence(item Evidence) bool {
	return validBoundedText(item.ID, maxMetadataBytes) && validBoundedText(item.Kind, maxMetadataBytes) && validSHA256(item.SHA256) &&
		len(item.Excerpt) <= maxEvidenceExcerpt && utf8.ValidString(item.Excerpt) && !strings.ContainsRune(item.Excerpt, '\x00')
}

func HardGateFailures(suite Suite, run Run) ([]GateFailure, error) {
	if err := ValidateRun(suite, run); err != nil {
		return nil, err
	}
	criteria := make(map[string]RubricCriterion, len(suite.Rubric))
	for _, criterion := range suite.Rubric {
		criteria[criterion.ID] = criterion
	}
	failures := make([]GateFailure, 0)
	for caseIndex, testCase := range suite.Cases {
		if testCase.Priority != "P0" {
			continue
		}
		for rubricIndex, expectation := range testCase.Expectations {
			criterion := criteria[expectation.CriterionID]
			if criterion.HardGate && expectation.Expected == ScoreOne && run.Cases[caseIndex].Judgments[rubricIndex].Score == ScoreZero {
				failures = append(failures, GateFailure{CaseID: testCase.ID, CriterionID: criterion.ID})
			}
		}
	}
	return failures, nil
}

func ComparisonKey(manifest RunManifest) (string, error) {
	if err := validateManifest(manifest, manifest.SuiteVersion); err != nil {
		return "", err
	}
	permissions := append([]string(nil), manifest.ToolPermissions...)
	sort.Strings(permissions)
	value := struct {
		SuiteVersion    string            `json:"suite_version"`
		Provider        string            `json:"provider"`
		Model           string            `json:"model"`
		PromptVersion   string            `json:"prompt_version"`
		SkillVersion    string            `json:"skill_version"`
		DSHVersion      string            `json:"dsh_version"`
		BuildSHA        string            `json:"build_sha"`
		ToolPermissions []string          `json:"tool_permissions"`
		Parameters      map[string]string `json:"parameters"`
	}{manifest.SuiteVersion, manifest.Provider, manifest.Model, manifest.PromptVersion, manifest.SkillVersion,
		manifest.DSHVersion, manifest.BuildSHA, permissions, manifest.Parameters}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateManifest(manifest RunManifest, suiteVersion string) error {
	if !validBoundedText(manifest.RunID, maxMetadataBytes) || manifest.SuiteVersion != suiteVersion ||
		!validBoundedText(manifest.Provider, maxMetadataBytes) || !validBoundedText(manifest.Model, maxMetadataBytes) ||
		!validBoundedText(manifest.PromptVersion, maxMetadataBytes) || !validBoundedText(manifest.SkillVersion, maxMetadataBytes) ||
		!validBoundedText(manifest.DSHVersion, maxMetadataBytes) || !validSHA256(manifest.BuildSHA) ||
		manifest.ToolPermissions == nil || manifest.Parameters == nil || manifest.StartedAt.IsZero() {
		return errors.New("evaluation run manifest is incomplete")
	}
	permissions := make(map[string]struct{}, len(manifest.ToolPermissions))
	for _, permission := range manifest.ToolPermissions {
		if !validBoundedText(permission, maxMetadataBytes) {
			return errors.New("evaluation run tool permissions are invalid")
		}
		if _, duplicate := permissions[permission]; duplicate {
			return errors.New("evaluation run repeats a tool permission")
		}
		permissions[permission] = struct{}{}
	}
	for key, value := range manifest.Parameters {
		if !validBoundedText(key, maxMetadataBytes) || !validBoundedText(value, maxMetadataBytes) {
			return errors.New("evaluation run parameters are invalid")
		}
	}
	return nil
}

func validJudgmentEvidenceRef(ref EvidenceReference, output string, evidence map[string]struct{}, caseID, criterionID string, isNA bool) bool {
	switch ref.Kind {
	case "output_span":
		return !isNA && ref.ID == "" && ref.StartByte >= 0 && ref.EndByte > ref.StartByte && ref.EndByte <= len(output) &&
			utf8.ValidString(output[ref.StartByte:ref.EndByte])
	case "evidence_id":
		if isNA || !validBoundedText(ref.ID, maxMetadataBytes) || ref.StartByte != 0 || ref.EndByte != 0 {
			return false
		}
		_, ok := evidence[ref.ID]
		return ok
	case "suite_na_reason":
		return isNA && ref.ID == caseID+"/"+criterionID && ref.StartByte == 0 && ref.EndByte == 0
	default:
		return false
	}
}

func validCaseID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 3 || parts[0] != "AIC" || len(parts[1]) != 1 || len(parts[2]) != 2 ||
		(parts[1] != "H" && parts[1] != "C" && parts[1] != "E") {
		return false
	}
	for _, char := range parts[2] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validBoundedText(value string, limit int) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validScore(score Score) bool {
	return score == ScoreZero || score == ScoreOne || score == ScoreNA
}
