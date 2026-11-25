package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The live weekly suite (design proposal §16.4) is deliberately separate
// from the 8-item synthetic contract layer (suite.go): it is run by hand
// on a real PC — or, once built, by the Inspect AI runner — and scored
// separately. Its case IDs are the PC-CHAT numbers and the SPEC IDs from
// the design, its priorities follow the source documents (including P2),
// and its data is real execution, not synthetic — so it cannot load
// through LoadSuite, whose validator only accepts AIC-* IDs, P0/P1, and
// data_class "synthetic". Nothing here may mark a case passed: passing
// can only be decided by a real execution, recorded in the weekly report,
// never in the suite file or the status file.

const liveSuiteCaseCount = 44

// LiveCase is one item of the weekly suite. Source names where the case
// comes from: the PC-CHAT cases condense pc-chat-merged-candidate.md one
// by one; the SPEC cases are new, written from known failure modes, and
// their Source says so.
type LiveCase struct {
	ID       string `json:"id"`
	Priority string `json:"priority"`
	Group    string `json:"group"`
	Input    string `json:"input"`
	Expected string `json:"expected_behavior"`
	Source   string `json:"source"`
}

// LiveSuite is the weekly suite file
// (testdata/suite-live-v1.json): 44 cases, no rubric, no scores.
type LiveSuite struct {
	Version string     `json:"version"`
	Cases   []LiveCase `json:"cases"`
}

// liveSuiteCaseIDs is the fixed list, in order: PC-CHAT 001–024, then the
// 20 specialist cases grouped by failure mode (classification failure,
// error recovery, approval, cost, multi-round merge).
func liveSuiteCaseIDs() []string {
	ids := make([]string, 0, liveSuiteCaseCount)
	for n := 1; n <= 24; n++ {
		ids = append(ids, fmt.Sprintf("PC-CHAT-%03d", n))
	}
	for _, group := range []struct {
		prefix string
		count  int
	}{
		{"SPEC-CLF", 5},
		{"SPEC-ERR", 5},
		{"SPEC-APR", 4},
		{"SPEC-COST", 3},
		{"SPEC-MRG", 3},
	} {
		for n := 1; n <= group.count; n++ {
			ids = append(ids, fmt.Sprintf("%s-%02d", group.prefix, n))
		}
	}
	return ids
}

// liveCaseGroup is the group a case ID belongs to. For PC-CHAT it follows
// the sections of pc-chat-merged-candidate.md; for SPEC it follows the ID
// prefix.
func liveCaseGroup(id string) (string, bool) {
	if strings.HasPrefix(id, "PC-CHAT-") {
		n, err := strconv.Atoi(strings.TrimPrefix(id, "PC-CHAT-"))
		if err != nil {
			return "", false
		}
		switch {
		case n >= 1 && n <= 7:
			return "basic", true
		case n >= 8 && n <= 13:
			return "complex-tasks", true
		case n >= 14 && n <= 16:
			return "session-history", true
		case n >= 17 && n <= 19:
			return "approval", true
		case n >= 20 && n <= 22:
			return "event-workspace", true
		case n >= 23 && n <= 24:
			return "cost-usage", true
		}
		return "", false
	}
	switch {
	case strings.HasPrefix(id, "SPEC-CLF-"):
		return "classification-failure", true
	case strings.HasPrefix(id, "SPEC-ERR-"):
		return "error-recovery", true
	case strings.HasPrefix(id, "SPEC-APR-"):
		return "approval", true
	case strings.HasPrefix(id, "SPEC-COST-"):
		return "cost", true
	case strings.HasPrefix(id, "SPEC-MRG-"):
		return "multi-round-merge", true
	}
	return "", false
}

func LoadLiveSuite(raw []byte) (LiveSuite, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var suite LiveSuite
	if err := decoder.Decode(&suite); err != nil {
		return LiveSuite{}, fmt.Errorf("decode live evaluation suite: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return LiveSuite{}, errors.New("live evaluation suite has trailing JSON")
		}
		return LiveSuite{}, fmt.Errorf("decode live evaluation suite trailer: %w", err)
	}
	if err := ValidateLiveSuite(suite); err != nil {
		return LiveSuite{}, err
	}
	return suite, nil
}

func ValidateLiveSuite(suite LiveSuite) error {
	wantIDs := liveSuiteCaseIDs()
	if !validBoundedText(suite.Version, maxSuiteVersionBytes) || len(suite.Cases) != len(wantIDs) {
		return errors.New("live evaluation suite version or case count is invalid")
	}
	for index, testCase := range suite.Cases {
		wantID := wantIDs[index]
		if testCase.ID != wantID {
			return fmt.Errorf("live evaluation case %d is %q, want %q in fixed order", index, testCase.ID, wantID)
		}
		wantGroup, ok := liveCaseGroup(testCase.ID)
		if !ok || testCase.Group != wantGroup {
			return fmt.Errorf("live evaluation case %q has group %q, want %q", testCase.ID, testCase.Group, wantGroup)
		}
		if testCase.Priority != "P0" && testCase.Priority != "P1" && testCase.Priority != "P2" ||
			!validBoundedText(testCase.Input, maxCaseInputBytes) ||
			!validBoundedText(testCase.Expected, maxExpectedBytes) ||
			!validBoundedText(testCase.Source, maxMetadataBytes) {
			return fmt.Errorf("live evaluation case %q is invalid", testCase.ID)
		}
	}
	return nil
}
