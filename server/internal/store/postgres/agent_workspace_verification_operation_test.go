package postgres

import (
	"testing"

	"morde/server/internal/domain/task"
)

func TestIsTaskWorkspaceVerificationOperation(t *testing.T) {
	tests := []struct {
		name           string
		key            string
		workspaceID    string
		bindingVersion int64
		want           bool
	}{
		{
			name:           "current v2 key",
			key:            "agent:verify:v2:s1234567:w1234567:3",
			workspaceID:    "w1234567",
			bindingVersion: 3,
			want:           true,
		},
		{
			name:           "persisted legacy key",
			key:            "agent:verify:s1234567:w1234567:3",
			workspaceID:    "w1234567",
			bindingVersion: 3,
			want:           true,
		},
		{
			name:           "unsupported key version",
			key:            "agent:verify:v3:s1234567:w1234567:3",
			workspaceID:    "w1234567",
			bindingVersion: 3,
		},
		{
			name:           "invalid step id",
			key:            "agent:verify:v2:step:w1234567:3",
			workspaceID:    "w1234567",
			bindingVersion: 3,
		},
		{
			name:           "invalid workspace id",
			key:            "agent:verify:v2:s1234567:workspace:3",
			workspaceID:    "workspace",
			bindingVersion: 3,
		},
		{
			name:           "workspace does not match key",
			key:            "agent:verify:v2:s1234567:w1234567:3",
			workspaceID:    "w7654321",
			bindingVersion: 3,
		},
		{
			name:           "binding version does not match key",
			key:            "agent:verify:v2:s1234567:w1234567:3",
			workspaceID:    "w1234567",
			bindingVersion: 4,
		},
		{
			name:           "non-positive key binding version",
			key:            "agent:verify:v2:s1234567:w1234567:0",
			workspaceID:    "w1234567",
			bindingVersion: 0,
		},
		{
			name:           "malformed key binding version",
			key:            "agent:verify:v2:s1234567:w1234567:3x",
			workspaceID:    "w1234567",
			bindingVersion: 3,
		},
		{
			name:           "extra key segment",
			key:            "agent:verify:v2:s1234567:w1234567:3:extra",
			workspaceID:    "w1234567",
			bindingVersion: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := task.Operation{
				Key:            test.key,
				WorkspaceID:    test.workspaceID,
				BindingVersion: test.bindingVersion,
			}
			if got := isTaskWorkspaceVerificationOperation(operation); got != test.want {
				t.Fatalf("isTaskWorkspaceVerificationOperation() = %t, want %t", got, test.want)
			}
		})
	}
}
