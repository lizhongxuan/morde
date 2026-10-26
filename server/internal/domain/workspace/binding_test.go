package workspace

import (
	"errors"
	"testing"

	"morde/server/internal/domain"
)

func TestValidateBindingTransition(t *testing.T) {
	valid := [][2]BindingState{
		{BindingPreparing, BindingActive},
		{BindingPreparing, BindingUnbinding},
		{BindingPreparing, BindingFailed},
		{BindingActive, BindingUnbinding},
		{BindingActive, BindingFailed},
		{BindingUnbinding, BindingDetached},
		{BindingUnbinding, BindingFailed},
		{BindingDetached, BindingPreparing},
		{BindingFailed, BindingPreparing},
		{BindingFailed, BindingUnbinding},
		{BindingFailed, BindingDetached},
	}
	for _, edge := range valid {
		if err := ValidateBindingTransition(edge[0], edge[1]); err != nil {
			t.Errorf("ValidateBindingTransition(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	if err := ValidateBindingTransition(BindingDetached, BindingActive); err == nil {
		t.Fatal("detached binding must prepare before it becomes active")
	} else {
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeInvalidState {
			t.Fatalf("invalid transition error = %#v, want invalid_state", err)
		}
	}
}

func TestValidateBindingSyncStatus(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     BindingSyncState
		conflicts []string
		valid     bool
	}{
		{name: "healthy", state: BindingSyncHealthy, valid: true},
		{name: "conflict", state: BindingSyncConflict, conflicts: []string{"src/main.go"}, valid: true},
		{name: "conflict without path details", state: BindingSyncConflict, valid: true},
		{name: "unknown state", state: "paused"},
		{name: "healthy with paths", state: BindingSyncHealthy, conflicts: []string{"src/main.go"}},
		{name: "traversal", state: BindingSyncConflict, conflicts: []string{"../secret"}},
		{name: "duplicate", state: BindingSyncConflict, conflicts: []string{"src/main.go", "src/main.go"}},
		{name: "too many", state: BindingSyncConflict, conflicts: make([]string, MaxBindingSyncConflictPaths+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBindingSyncStatus(test.state, test.conflicts)
			if test.valid && err != nil {
				t.Fatalf("ValidateBindingSyncStatus() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ValidateBindingSyncStatus() accepted invalid status")
			}
		})
	}
}
