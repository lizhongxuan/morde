package evaluation

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The P0 smoke table (deploy/single/smoke-cases.tsv) maps each fixed case to
// a runnable check. This test guards the mapping: a renamed test must fail
// here instead of letting smoke.sh report a stale check as passed (or as
// "no tests to run"). The checker itself is test-only; the table is a deploy
// artifact, not a runtime input.

type smokeCaseRow struct {
	id          string
	level       string
	description string
	check       string
	needs       string
}

var smokeFixedCases = []string{"T01", "T04", "T06", "T10", "T13", "T19", "T22", "T26", "T36", "T43"}

func parseSmokeCases(content string) ([]smokeCaseRow, error) {
	var rows []smokeCaseRow
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, fmt.Errorf("smoke case line has %d fields, want 5: %q", len(fields), line)
		}
		rows = append(rows, smokeCaseRow{id: fields[0], level: fields[1], description: fields[2], check: fields[3], needs: fields[4]})
	}
	return rows, nil
}

// checkSmokeCases verifies the table shape and that every go-test mapping
// names at least one real test in its package. testNames lists the test
// function names of a package (e.g. "internal/app").
func checkSmokeCases(rows []smokeCaseRow, testNames func(pkg string) ([]string, error)) error {
	if len(rows) != len(smokeFixedCases) {
		return fmt.Errorf("smoke table has %d cases, want %d", len(rows), len(smokeFixedCases))
	}
	seen := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		if row.id != smokeFixedCases[index] {
			return fmt.Errorf("smoke case %d is %q, want %q in fixed order", index, row.id, smokeFixedCases[index])
		}
		if _, duplicate := seen[row.id]; duplicate {
			return fmt.Errorf("smoke case %q appears twice", row.id)
		}
		seen[row.id] = struct{}{}
		switch row.level {
		case "S+P", "P", "P+D", "P+S", "S+D":
		default:
			return fmt.Errorf("smoke case %q has unknown level %q", row.id, row.level)
		}
		if row.description == "" {
			return fmt.Errorf("smoke case %q has no description", row.id)
		}
		switch row.needs {
		case "none", "CH_TEST_DATABASE_URL", "real-pc":
		default:
			return fmt.Errorf("smoke case %q has unknown needs %q", row.id, row.needs)
		}
		switch {
		case strings.HasPrefix(row.check, "go-test:"):
			rest := strings.TrimPrefix(row.check, "go-test:")
			pkg, regex, found := strings.Cut(rest, ":")
			if !found || pkg == "" || regex == "" {
				return fmt.Errorf("smoke case %q has malformed go-test check %q", row.id, row.check)
			}
			// Fail-closed (P3-3): the two exception reasons are
			// auto-detected environment limits, so the table must not
			// claim them for anything else. A database exception is
			// only for a PostgreSQL integration test, and a real-PC
			// exception is only for a manual check — a go-test that
			// "needs a real PC" would silently skip running.
			switch row.needs {
			case "CH_TEST_DATABASE_URL":
				if pkg != "internal/store/postgres" {
					return fmt.Errorf("smoke case %q claims a database exception for non-PostgreSQL package %q", row.id, pkg)
				}
			case "real-pc":
				return fmt.Errorf("smoke case %q claims a real-PC exception for a go-test check", row.id)
			}
			pattern, err := regexp.Compile("^(?:" + regex + ")$")
			if err != nil {
				return fmt.Errorf("smoke case %q has invalid test regex %q: %w", row.id, regex, err)
			}
			names, err := testNames(pkg)
			if err != nil {
				return fmt.Errorf("smoke case %q: %w", row.id, err)
			}
			matched := false
			for _, name := range names {
				if pattern.MatchString(name) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("smoke case %q maps to no real test: %q", row.id, row.check)
			}
		case strings.HasPrefix(row.check, "manual:"):
			if strings.TrimSpace(strings.TrimPrefix(row.check, "manual:")) == "" {
				return fmt.Errorf("smoke case %q has a manual check without a reason", row.id)
			}
			// A manual check can never produce evidence in an automated
			// gate, so it is an exception only when it needs a real PC.
			if row.needs != "real-pc" {
				return fmt.Errorf("smoke case %q has a manual check without real-pc needs", row.id)
			}
		default:
			return fmt.Errorf("smoke case %q has unknown check kind %q", row.id, row.check)
		}
	}
	return nil
}

var smokeTestFuncPattern = regexp.MustCompile(`^func (Test\w+)\(`)

func smokePackageTestNames(pkg string) ([]string, error) {
	// Package paths are server-relative ("internal/app"); the test runs in
	// server/internal/evaluation, so the directory is one level up.
	dir := filepath.Join("..", strings.TrimPrefix(pkg, "internal/"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read package %q: %w", pkg, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", entry.Name(), err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if match := smokeTestFuncPattern.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
				names = append(names, match[1])
			}
		}
	}
	return names, nil
}

func TestSmokeCasesTableMatchesRealTests(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/single/smoke-cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseSmokeCases(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSmokeCases(rows, smokePackageTestNames); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeCasesTableCheckerRejectsBadRows(t *testing.T) {
	valid := "T01\tS+P\tdesc\tgo-test:internal/app:TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\tnone\n" +
		"T04\tP\tdesc\tgo-test:internal/app:TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\tnone\n" +
		"T06\tS+P\tdesc\tgo-test:internal/worker:TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting\tnone\n" +
		"T10\tP+D\tdesc\tmanual:needs real PC + real model/DSH\treal-pc\n" +
		"T13\tP\tdesc\tgo-test:internal/app:TestTaskStepDetailIsBoundToItsIdentityAndTruncatedSafely\tnone\n" +
		"T19\tP+S\tdesc\tgo-test:internal/api:TestTaskApprovalResponseCarriesEvidenceFields\tnone\n" +
		"T22\tP+D\tdesc\tgo-test:internal/app:TestRuntimeToolExecutorVerifyRequiresCommittedWorkspaceBlockReadback\tnone\n" +
		"T26\tS+P\tdesc\tgo-test:internal/store/postgres:TestConversationRouteSteersOnlySafeRunningTaskSupplement\tCH_TEST_DATABASE_URL\n" +
		"T36\tS+D\tdesc\tgo-test:internal/store/postgres:TestPostgresTaskAgentRequestReconciliationPersistsUntilExactResolution\tCH_TEST_DATABASE_URL\n" +
		"T43\tP+D\tdesc\tmanual:needs real PC + real model/DSH\treal-pc\n"
	names := func(pkg string) ([]string, error) {
		return []string{
			"TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks",
			"TestConversationRouteWorkerClassifiesFollowUpWhilePredecessorRouting",
			"TestTaskStepDetailIsBoundToItsIdentityAndTruncatedSafely",
			"TestTaskApprovalResponseCarriesEvidenceFields",
			"TestRuntimeToolExecutorVerifyRequiresCommittedWorkspaceBlockReadback",
			"TestConversationRouteSteersOnlySafeRunningTaskSupplement",
			"TestPostgresTaskAgentRequestReconciliationPersistsUntilExactResolution",
		}, nil
	}
	mustParse := func(content string) []smokeCaseRow {
		t.Helper()
		rows, err := parseSmokeCases(content)
		if err != nil {
			t.Fatalf("parse synthetic smoke table: %v", err)
		}
		return rows
	}
	for name, content := range map[string]string{
		"renamed test matches nothing": strings.Replace(valid,
			"TestRuntimeToolExecutorVerifyRequiresCommittedWorkspaceBlockReadback", "TestRenamedAway", 1),
		"unknown check kind": strings.Replace(valid,
			"go-test:internal/app:TestTaskStepDetailIsBoundToItsIdentityAndTruncatedSafely", "http://example.invalid/T13", 1),
		"duplicate case":        strings.Replace(valid, "T04\tP", "T01\tP", 1),
		"unknown case id":       strings.Replace(valid, "T43\tP+D", "T99\tP+D", 1),
		"unknown needs":         strings.Replace(valid, "CH_TEST_DATABASE_URL", "a-real-database", 1),
		"manual without reason": strings.Replace(valid, "manual:needs real PC + real model/DSH", "manual: ", 1),
		"manual without real-pc needs": strings.Replace(valid,
			"manual:needs real PC + real model/DSH\treal-pc", "manual:needs real PC + real model/DSH\tnone", 1),
		"database needs on a non-PostgreSQL package": strings.Replace(valid,
			"TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\tnone",
			"TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\tCH_TEST_DATABASE_URL", 1),
		"go-test check with real-pc needs": strings.Replace(valid,
			"TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\tnone",
			"TestExplicitSearchFallbackAvoidsQuestionsNegationAndActiveTasks\treal-pc", 1),
		"missing case": strings.Replace(valid, "T43\tP+D\tdesc\tmanual:needs real PC + real model/DSH\treal-pc\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkSmokeCases(mustParse(content), names); err == nil {
				t.Fatalf("checkSmokeCases() accepted a bad table (%s)", name)
			}
		})
	}
	if err := checkSmokeCases(mustParse(valid), names); err != nil {
		t.Fatalf("checkSmokeCases() rejected the valid synthetic table: %v", err)
	}
}
