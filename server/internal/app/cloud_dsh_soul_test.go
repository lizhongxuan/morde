package app

import (
	"strings"
	"testing"
)

func TestMordeSoulInstructionsDoesNotDuplicateTheDSHSoul(t *testing.T) {
	base := strings.TrimSpace(mordeSoulFile)
	runtimeInstructions := "runtime preface\n\n" + base + "\n\nAuthorized task context"
	got := mordeSoulInstructions(runtimeInstructions)
	if strings.Count(got, base) != 1 || !strings.Contains(got, "runtime preface") ||
		!strings.Contains(got, "Authorized task context") || !strings.Contains(got, agentInstructions) {
		t.Fatalf("Morde SOUL or trusted task instructions were duplicated or dropped: %q", got)
	}
}

func TestMordeSoulInstructionsAddsCanonicalSoulWhenDSHDoesNot(t *testing.T) {
	got := mordeSoulInstructions("runtime preface")
	base := strings.TrimSpace(mordeSoulFile)
	if strings.Count(got, base) != 1 || !strings.Contains(got, "Additional trusted runtime instructions:\nruntime preface") ||
		!strings.Contains(got, agentInstructions) {
		t.Fatalf("fallback Morde SOUL or task policy is missing: %q", got)
	}
}
