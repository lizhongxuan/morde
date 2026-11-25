package app

import (
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestValidateConversationResultDelivery(t *testing.T) {
	source := func(id string, revision int64) ConversationTaskResultSource {
		return ConversationTaskResultSource{TaskID: id, TaskRevision: revision}
	}
	tests := []struct {
		name       string
		sources    []ConversationTaskResultSource
		states     []TaskValidationState
		wantErr    bool
		wantCode   domain.ErrorCode
		wantStage  string
		wantSubstr string
	}{
		{name: "empty batch publishes", sources: nil, states: nil},
		{name: "pass publishes", sources: []ConversationTaskResultSource{source("q1234567", 1)},
			states: []TaskValidationState{TaskValidationPass}},
		{name: "pass with notes publishes", sources: []ConversationTaskResultSource{source("q1234567", 2)},
			states: []TaskValidationState{TaskValidationPassNotes}},
		{name: "pending publishes with its label", sources: []ConversationTaskResultSource{source("q1234567", 1)},
			states: []TaskValidationState{TaskValidationPending}},
		{name: "mixed pass and pending publishes",
			sources: []ConversationTaskResultSource{source("q1234567", 1), source("q2345678", 1)},
			states:  []TaskValidationState{TaskValidationPass, TaskValidationPending}},
		{name: "single fail refuses", sources: []ConversationTaskResultSource{source("q1234567", 3)},
			states: []TaskValidationState{TaskValidationFail}, wantErr: true, wantCode: domain.CodeInvalidState,
			wantStage: "result_validation_gate", wantSubstr: "q1234567 r3"},
		{name: "fail among passing sources refuses the whole batch",
			sources: []ConversationTaskResultSource{source("q1234567", 1), source("q2345678", 2), source("q3456789", 1)},
			states:  []TaskValidationState{TaskValidationPass, TaskValidationFail, TaskValidationPending},
			wantErr: true, wantCode: domain.CodeInvalidState, wantStage: "result_validation_gate", wantSubstr: "q2345678 r2"},
		{name: "unknown state does not block",
			sources: []ConversationTaskResultSource{source("q1234567", 1)},
			states:  []TaskValidationState{TaskValidationState("weird")}},
		{name: "state count mismatch is invalid",
			sources: []ConversationTaskResultSource{source("q1234567", 1)},
			states:  []TaskValidationState{TaskValidationPass, TaskValidationPass},
			wantErr: true, wantCode: domain.CodeInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConversationResultDelivery(tc.sources, tc.states)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateConversationResultDelivery returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateConversationResultDelivery returned nil; want an error")
			}
			if domain.CodeOf(err) != tc.wantCode {
				t.Errorf("error code = %q; want %q (err=%v)", domain.CodeOf(err), tc.wantCode, err)
			}
			if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantSubstr)
			}
			if tc.wantStage != "" {
				var routeErr *domain.Error
				if !errors.As(err, &routeErr) {
					t.Fatalf("error is not a domain error: %T", err)
				}
				if stage, _ := routeErr.Details["stage"].(string); stage != tc.wantStage {
					t.Errorf("error stage = %q; want %q", stage, tc.wantStage)
				}
			}
		})
	}
}
