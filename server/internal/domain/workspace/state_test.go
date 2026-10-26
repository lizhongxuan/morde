package workspace

import (
	"errors"
	"testing"

	"morde/server/internal/domain"
)

func TestValidateWorkspaceTransition(t *testing.T) {
	for _, tt := range []struct {
		from State
		to   State
	}{
		{StateReady, StateDeleting},
		{StateDeleting, StateDeleted},
	} {
		if err := ValidateTransition(tt.from, tt.to); err != nil {
			t.Fatalf("ValidateTransition(%q, %q) error = %v", tt.from, tt.to, err)
		}
	}
	for _, tt := range []struct {
		from State
		to   State
	}{
		{"", StateReady},
		{StateReady, StateReady},
		{StateReady, StateDeleted},
		{StateDeleted, StateReady},
		{"unknown", StateReady},
	} {
		err := ValidateTransition(tt.from, tt.to)
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeInvalidState {
			t.Fatalf("ValidateTransition(%q, %q) error = %#v, want invalid_state", tt.from, tt.to, err)
		}
		if got := de.Details["state"]; got != string(tt.from) {
			t.Fatalf("details.state = %#v, want %q", got, tt.from)
		}
	}
}
