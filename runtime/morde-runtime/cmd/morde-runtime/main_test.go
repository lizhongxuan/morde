package main

import (
	"testing"

	"morde-runtime/runtimeactions"
)

func TestShellCapabilityUnavailableDiagnosticUsesStaticCategories(t *testing.T) {
	for _, test := range []struct {
		category runtimeactions.ShellFailureCategory
		want     string
	}{
		{runtimeactions.ShellFailureCgroup, "morde-runtime: shell capability unavailable reason=cgroup"},
		{runtimeactions.ShellFailureRequiredTool, "morde-runtime: shell capability unavailable reason=required_tool"},
		{runtimeactions.ShellFailureWorkspaceProbe, "morde-runtime: shell capability unavailable reason=workspace_or_probe"},
		{runtimeactions.ShellFailureOther, "morde-runtime: shell capability unavailable reason=other"},
		{runtimeactions.ShellFailureCategory("private/path"), "morde-runtime: shell capability unavailable reason=other"},
	} {
		if got := shellCapabilityUnavailableDiagnostic(test.category); got != test.want {
			t.Errorf("diagnostic(%q)=%q, want %q", test.category, got, test.want)
		}
	}
}
