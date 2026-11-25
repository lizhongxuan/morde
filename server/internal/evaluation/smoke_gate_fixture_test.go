package evaluation

import "testing"

// Smoke-gate fixtures (P3-3). These tests exist only so the fail-closed
// smoke gate has a real "no evidence" case to detect: they always skip.
// TestSmokeGateFixtureTopLevelSkipped skips outright, and
// TestSmokeGateFixtureSubtestsSkipped skips every subtest while the parent
// itself passes — so only a leaf-aware verdict parser can tell the case
// produced no evidence. They never fail a package run; they only show up
// as SKIP in verbose output.

func TestSmokeGateFixtureTopLevelSkipped(t *testing.T) {
	t.Skip("smoke gate fixture: a skipped test is not evidence")
}

func TestSmokeGateFixtureSubtestsSkipped(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Skip("smoke gate fixture: a skipped subtest is not evidence")
		})
	}
}
