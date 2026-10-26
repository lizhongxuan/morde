package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

func TestNewIDAndValidID(t *testing.T) {
	id, err := NewID()
	if err != nil || !ValidID(id) {
		t.Fatalf("NewID() = %q, %v", id, err)
	}
	for _, invalid := range []string{"", "n123456", "N1234567", "n123456!"} {
		if ValidID(invalid) {
			t.Errorf("ValidID(%q) = true", invalid)
		}
	}
}

func TestDeriveEventEffectUsesFixedSummaryAndOpaqueReference(t *testing.T) {
	created := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	id := "n1234567"
	tests := []struct {
		name    string
		type_   string
		status  task.Status
		payload string
		kind    Kind
		ref     string
	}{
		{name: "completed", type_: "task.result", status: task.StatusCompleted, payload: `{"result":"private task text"}`, kind: KindCompleted},
		{name: "failed", type_: "task.result", status: task.StatusFailed, payload: `{"detail":"private task text"}`, kind: KindFailed},
		{name: "approval", type_: "task.waiting_user", status: task.StatusWaitingUser, payload: `{"detail":{"pending_request_id":"r1234567","pending_request_version":2,"pending_request_kind":"approval","approval":{"content":"private approval"}}}`, kind: KindApprovalRequired, ref: "r1234567"},
		{name: "clarification", type_: "task.waiting_user", status: task.StatusWaitingUser, payload: `{"detail":{"clarification_id":"r7654321","clarification_version":3,"question":"private question"}}`, kind: KindClarification, ref: "r7654321"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effect, err := DeriveEventEffect(TaskEvent{
				OwnerID: "u1234567", TaskID: "q1234567", Sequence: 8, Type: test.type_, Payload: []byte(test.payload),
				TaskStatus: test.status, NotificationEnabled: true, CreatedAt: created,
			}, id)
			if err != nil || effect.Notification == nil {
				t.Fatalf("DeriveEventEffect() = %#v, %v", effect, err)
			}
			value := effect.Notification
			if value.Kind != test.kind || value.RequestID != test.ref || value.EventSequence != 8 || value.OwnerID != "u1234567" {
				t.Fatalf("notification = %#v", value)
			}
			if strings.Contains(value.Summary, "private") || strings.Contains(value.Summary, "question") || strings.Contains(value.Summary, "approval") {
				t.Fatalf("summary exposes event content: %q", value.Summary)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "question") {
				t.Fatalf("serialized notification exposes event content: %s", encoded)
			}
			if err := Validate(*value); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestDeriveEventEffectClosesPendingAndSkipsNonTerminalResult(t *testing.T) {
	base := TaskEvent{OwnerID: "u1234567", TaskID: "q1234567", Sequence: 2, Payload: []byte(`{}`), NotificationEnabled: true, CreatedAt: time.Now()}
	base.Type, base.TaskStatus = "task.response", task.StatusQueued
	effect, err := DeriveEventEffect(base, "")
	if err != nil || !effect.ClosePending || effect.Notification != nil {
		t.Fatalf("response effect = %#v, %v", effect, err)
	}
	base.Type, base.TaskStatus = "task.result", task.StatusRunning
	effect, err = DeriveEventEffect(base, "")
	if err != nil || effect.Notification != nil {
		t.Fatalf("non-terminal result effect = %#v, %v", effect, err)
	}
	base.NotificationEnabled = false
	base.TaskStatus = task.StatusCompleted
	effect, err = DeriveEventEffect(base, "")
	if err != nil || !effect.ClosePending || effect.Notification != nil {
		t.Fatalf("unsubscribed completed result effect = %#v, %v", effect, err)
	}
	base.Type, base.TaskStatus = "task.waiting_user", task.StatusWaitingUser
	effect, err = DeriveEventEffect(base, "")
	if err != nil || effect.ClosePending || effect.Notification != nil {
		t.Fatalf("unsubscribed waiting event effect = %#v, %v", effect, err)
	}
}

func TestValidateRejectsActionableHandledAndRequestlessApproval(t *testing.T) {
	value := Notification{
		ID: "n1234567", OwnerID: "u1234567", TaskID: "q1234567", EventSequence: 2,
		Kind: KindApprovalRequired, Summary: "任务等待审批", RequestID: "r1234567", RequestVersion: 1,
		Actionable: true, CreatedAt: time.Now(),
	}
	if err := Validate(value); err != nil {
		t.Fatal(err)
	}
	handled := time.Now()
	value.HandledAt = &handled
	if err := Validate(value); err == nil {
		t.Fatal("actionable handled notification was accepted")
	}
	value.Actionable = false
	value.RequestID = ""
	if err := Validate(value); err == nil {
		t.Fatal("approval notification without an exact request reference was accepted")
	}
}
