package host

import (
	"errors"
	"testing"

	"morde/server/internal/domain"
)

func TestValidateTransition(t *testing.T) {
	tests := []struct {
		from State
		to   State
	}{
		{StateCreating, StateRunning},
		{StateCreating, StateStopped},
		{StateCreating, StateFailed},
		{StateCreating, StateDeleting},
		{StateRunning, StateFailed},
		{StateRunning, StateStopping},
		{StateRunning, StateDeleting},
		{StateStopping, StateStopped},
		{StateStopping, StateDeleting},
		{StateStopped, StateCreating},
		{StateStopped, StateDeleting},
		{StateFailed, StateDeleting},
		{StateDeleting, StateDeleted},
	}
	for _, tt := range tests {
		t.Run(string(tt.from)+"_to_"+string(tt.to), func(t *testing.T) {
			if err := ValidateTransition(tt.from, tt.to); err != nil {
				t.Fatalf("ValidateTransition(%q, %q) error = %v", tt.from, tt.to, err)
			}
		})
	}
}

func TestValidateTransitionRejectsIllegalEdge(t *testing.T) {
	for _, tt := range []struct {
		from State
		to   State
	}{
		{"", StateCreating},
		{StateDeleted, StateRunning},
		{"unknown", StateRunning},
	} {
		t.Run(string(tt.from)+"_to_"+string(tt.to), func(t *testing.T) {
			err := ValidateTransition(tt.from, tt.to)
			var de *domain.Error
			if !errors.As(err, &de) || de.Code != domain.CodeInvalidState {
				t.Fatalf("ValidateTransition(%q, %q) error = %#v, want invalid_state", tt.from, tt.to, err)
			}
			if got := de.Details["state"]; got != string(tt.from) {
				t.Fatalf("details.state = %#v, want %q", got, tt.from)
			}
		})
	}
}
