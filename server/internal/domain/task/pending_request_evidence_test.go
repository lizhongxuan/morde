package task

import "testing"

func TestValidApprovalTriggerRule(t *testing.T) {
	for _, value := range []string{
		"",
		ApprovalTriggerHostSensitiveTool,
		ApprovalTriggerExternalMCPTool,
		ApprovalTriggerWorkspaceLiveWrite,
		ApprovalTriggerMediaPublish,
	} {
		if !ValidApprovalTriggerRule(value) {
			t.Fatalf("ValidApprovalTriggerRule(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"bogus", "HOST_SENSITIVE_TOOL", "host_sensitive_tool ", " step"} {
		if ValidApprovalTriggerRule(value) {
			t.Fatalf("ValidApprovalTriggerRule(%q) = true, want false", value)
		}
	}
}

func TestEvidencePointerForStep(t *testing.T) {
	pointer, ok := EvidencePointerForStep("s1234567")
	if !ok || pointer != "step:s1234567" {
		t.Fatalf("EvidencePointerForStep(s1234567) = (%q, %v), want (step:s1234567, true)", pointer, ok)
	}
	for _, id := range []string{"", "x1234567", "s12345", "s12345678", "sABCDEFG"} {
		if pointer, ok := EvidencePointerForStep(id); ok || pointer != "" {
			t.Fatalf("EvidencePointerForStep(%q) = (%q, %v), want (\"\", false)", id, pointer, ok)
		}
	}
}
