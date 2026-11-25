package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func loadLiveFixture(t *testing.T) LiveSuite {
	t.Helper()
	raw, err := os.ReadFile("testdata/suite-live-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	suite, err := LoadLiveSuite(raw)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func cloneLiveSuite(t *testing.T, suite LiveSuite) LiveSuite {
	t.Helper()
	encoded, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	var clone LiveSuite
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

// The live suite list is frozen: 44 IDs in fixed order, with the group and
// priority each case carries in its source document. Any addition, removal,
// reorder, or regrouping must fail here first.
func TestLoadLiveSuite(t *testing.T) {
	suite := loadLiveFixture(t)
	if suite.Version != "morde-pc-chat-live-v1.0.0" {
		t.Fatalf("live suite version = %q, want %q", suite.Version, "morde-pc-chat-live-v1.0.0")
	}
	ids := make([]string, 0, len(suite.Cases))
	for _, testCase := range suite.Cases {
		ids = append(ids, testCase.ID)
	}
	if !reflect.DeepEqual(ids, liveSuiteCaseIDs()) {
		t.Fatalf("live suite IDs do not match the fixed 44-item list:\n got: %v\nwant: %v", ids, liveSuiteCaseIDs())
	}
	spot := map[string]struct{ priority, group string }{
		"PC-CHAT-001":  {"P0", "basic"},
		"PC-CHAT-008":  {"P0", "complex-tasks"},
		"PC-CHAT-015":  {"P2", "session-history"},
		"PC-CHAT-017":  {"P0", "approval"},
		"PC-CHAT-020":  {"P1", "event-workspace"},
		"PC-CHAT-023":  {"P1", "cost-usage"},
		"SPEC-CLF-01":  {"P0", "classification-failure"},
		"SPEC-ERR-01":  {"P0", "error-recovery"},
		"SPEC-APR-01":  {"P0", "approval"},
		"SPEC-COST-01": {"P0", "cost"},
		"SPEC-MRG-01":  {"P0", "multi-round-merge"},
	}
	for _, testCase := range suite.Cases {
		want, ok := spot[testCase.ID]
		if !ok {
			continue
		}
		if testCase.Priority != want.priority || testCase.Group != want.group {
			t.Errorf("live case %q priority/group = %q/%q, want %q/%q",
				testCase.ID, testCase.Priority, testCase.Group, want.priority, want.group)
		}
		if testCase.Source == "" {
			t.Errorf("live case %q has no source", testCase.ID)
		}
	}
}

func TestLoadLiveSuiteRejectsInvalidSuites(t *testing.T) {
	base := loadLiveFixture(t)
	raw, err := os.ReadFile("testdata/suite-live-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func() ([]byte, error){
		"duplicate case": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[1].ID = suite.Cases[0].ID
			return json.Marshal(suite)
		},
		"wrong order": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[0], suite.Cases[1] = suite.Cases[1], suite.Cases[0]
			return json.Marshal(suite)
		},
		"wrong group": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[0].Group = "approval"
			return json.Marshal(suite)
		},
		"invalid priority": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[0].Priority = "P3"
			return json.Marshal(suite)
		},
		"empty input": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[0].Input = " "
			return json.Marshal(suite)
		},
		"missing source": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases[0].Source = ""
			return json.Marshal(suite)
		},
		"missing case": func() ([]byte, error) {
			suite := cloneLiveSuite(t, base)
			suite.Cases = suite.Cases[:len(suite.Cases)-1]
			return json.Marshal(suite)
		},
		"unknown field": func() ([]byte, error) {
			return bytes.Replace(raw, []byte(`"cases": [`), []byte(`"extra": 1, "cases": [`), 1), nil
		},
		"trailing JSON": func() ([]byte, error) {
			return append(append([]byte(nil), raw...), []byte(` {}`)...), nil
		},
		"malformed JSON": func() ([]byte, error) {
			return []byte("{"), nil
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			encoded, err := build()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadLiveSuite(encoded); err == nil {
				t.Fatalf("LoadLiveSuite() accepted an invalid live suite (%s)", name)
			}
		})
	}
}

// The status file is the only place that says whether each item can be run
// by hand right now. It must list all 44 items in suite order, mark each
// manual-runnable or blocked with a reason, and never mark any passed.
func TestLiveSuiteStatus(t *testing.T) {
	suite := loadLiveFixture(t)
	raw, err := os.ReadFile("live-suite-status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, need := range []string{"0/44", "Inspect AI", "Judge key"} {
		if !strings.Contains(text, need) {
			t.Fatalf("live suite status does not record the weekly auto-run state %q", need)
		}
	}
	type statusRow struct {
		id, status, reason string
	}
	var rows []statusRow
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "| PC-CHAT-") && !strings.HasPrefix(line, "| SPEC-") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 5 {
			t.Fatalf("live suite status row has %d fields, want 5: %q", len(fields), line)
		}
		rows = append(rows, statusRow{
			id:     strings.TrimSpace(fields[1]),
			status: strings.TrimSpace(fields[2]),
			reason: strings.TrimSpace(fields[3]),
		})
	}
	if len(rows) != len(suite.Cases) {
		t.Fatalf("live suite status has %d rows, want %d", len(rows), len(suite.Cases))
	}
	counts := map[string]int{}
	for index, row := range rows {
		if row.id != suite.Cases[index].ID {
			t.Fatalf("live suite status row %d is %q, want %q in suite order", index, row.id, suite.Cases[index].ID)
		}
		switch row.status {
		case "manual-runnable", "blocked":
			counts[row.status]++
		default:
			t.Fatalf("live suite status row %q has status %q, want manual-runnable or blocked (never passed)", row.id, row.status)
		}
		if row.reason == "" {
			t.Fatalf("live suite status row %q has no reason", row.id)
		}
	}
	if counts["manual-runnable"] != 36 || counts["blocked"] != 8 {
		t.Fatalf("live suite status counts = manual-runnable %d / blocked %d, want 36 / 8",
			counts["manual-runnable"], counts["blocked"])
	}
}

// The live suite and the synthetic contract layer are separate artifacts:
// the live file must not load as a synthetic suite, and the two versions
// must differ, so a run of one can never be scored as the other.
func TestLiveSuiteIsSeparateFromSyntheticSuite(t *testing.T) {
	raw, err := os.ReadFile("testdata/suite-live-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(raw); err == nil {
		t.Fatal("live suite file must not load as a synthetic suite")
	}
	synthetic := loadFixture(t)
	live := loadLiveFixture(t)
	if synthetic.Version == live.Version {
		t.Fatalf("live and synthetic suites share version %q", live.Version)
	}
	if len(synthetic.Cases) == len(live.Cases) {
		t.Fatalf("live and synthetic suites share case count %d", len(live.Cases))
	}
}
