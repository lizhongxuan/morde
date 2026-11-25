package app

import (
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestValidateConversationResultDelivery(t *testing.T) {
	source := func(taskID string) ConversationTaskResultSource {
		return ConversationTaskResultSource{TaskID: taskID, TaskRevision: 1}
	}
	cases := []struct {
		name    string
		sources []ConversationTaskResultSource
		states  []TaskValidationState
		wantErr bool
	}{
		{"all pass", []ConversationTaskResultSource{source("q1234567")}, []TaskValidationState{TaskValidationPass}, false},
		{"pass with notes", []ConversationTaskResultSource{source("q1234567")}, []TaskValidationState{TaskValidationPassNotes}, false},
		{"pending does not block", []ConversationTaskResultSource{source("q1234567")}, []TaskValidationState{TaskValidationPending}, false},
		{"mixed pass and pending", []ConversationTaskResultSource{source("q1234567"), source("q7654321")},
			[]TaskValidationState{TaskValidationPass, TaskValidationPending}, false},
		{"single fail refuses", []ConversationTaskResultSource{source("q1234567")}, []TaskValidationState{TaskValidationFail}, true},
		{"fail among pass and pending refuses the whole batch", []ConversationTaskResultSource{source("q1234567"), source("q7654321"), source("q1111111")},
			[]TaskValidationState{TaskValidationPass, TaskValidationFail, TaskValidationPending}, true},
		{"empty batch passes vacuously", nil, nil, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := ValidateConversationResultDelivery(item.sources, item.states)
			if item.wantErr && err == nil {
				t.Fatalf("delivery was allowed for %s", item.name)
			}
			if !item.wantErr && err != nil {
				t.Fatalf("delivery refused for %s: %v", item.name, err)
			}
			if !item.wantErr {
				return
			}
			if domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("error code = %s, want invalid_state", domain.CodeOf(err))
			}
			var routeErr *domain.Error
			if ok := errors.As(err, &routeErr); !ok {
				t.Fatalf("error is not a domain error: %T", err)
			}
			if routeErr.Details["stage"] != "result_validation_gate" {
				t.Fatalf("stage detail = %v, want result_validation_gate", routeErr.Details["stage"])
			}
			failedIDs, _ := routeErr.Details["failed_task_ids"].(string)
			for index, state := range item.states {
				taskID := item.sources[index].TaskID
				if state == TaskValidationFail && !strings.Contains(failedIDs, taskID) {
					t.Fatalf("failed_task_ids = %q, want it to name %s", failedIDs, taskID)
				}
				if state != TaskValidationFail && strings.Contains(failedIDs, taskID) {
					t.Fatalf("failed_task_ids = %q, want it not to name passing task %s", failedIDs, taskID)
				}
			}
		})
	}
}

func TestValidateConversationResultDeliveryRejectsBadInput(t *testing.T) {
	source := ConversationTaskResultSource{TaskID: "q1234567", TaskRevision: 1}
	if err := ValidateConversationResultDelivery([]ConversationTaskResultSource{source},
		[]TaskValidationState{TaskValidationPass, TaskValidationPending}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched sources and states error = %v, want invalid_argument", err)
	}
	if err := ValidateConversationResultDelivery([]ConversationTaskResultSource{source},
		[]TaskValidationState{"MAYBE"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid validation state error = %v, want invalid_argument", err)
	}
}
