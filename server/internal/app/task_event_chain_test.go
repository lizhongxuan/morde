package app

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func taskEventChainEvent(sequence int64, eventType, payload string) task.Event {
	return task.Event{TaskID: "q1234567", Sequence: sequence, Type: eventType, Payload: []byte(payload)}
}

func taskEventChainKinds(chain TaskEventChain) []task.EventKind {
	kinds := make([]task.EventKind, 0, len(chain.Entries))
	for _, entry := range chain.Entries {
		kinds = append(kinds, entry.Kind)
	}
	return kinds
}

func TestReplayTaskEventChainCompleteFixTask(t *testing.T) {
	events := []task.Event{
		taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainEvent(2, "task.plan", `{}`),
		taskEventChainEvent(3, "task.status", `{"status":"running"}`),
		taskEventChainEvent(4, "task.step", `{"status":"succeeded"}`),
		taskEventChainEvent(5, "task.step", `{"status":"failed"}`),
		taskEventChainEvent(6, "task.step", `{"status":"succeeded"}`),
		taskEventChainEvent(7, "task.step", `{"status":"succeeded"}`),
		taskEventChainEvent(8, "task.result", `{"status":"completed"}`),
	}
	chain := ReplayTaskEventChain("q1234567", events, TaskEventChainFacts{ValidationState: TaskValidationPass, Delivered: true})
	if !chain.Valid {
		t.Fatalf("complete fix chain must be valid, reasons: %v", chain.InvalidReasons)
	}
	wantKinds := []task.EventKind{task.EventKindCreated, task.EventKindSplit, task.EventKindDispatched,
		task.EventKindExecution, task.EventKindFailure, task.EventKindRecovery, task.EventKindExecution,
		task.EventKindExecution, task.EventKindVerification, task.EventKindDelivery}
	gotKinds := taskEventChainKinds(chain)
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("chain kinds = %v, want %v", gotKinds, wantKinds)
	}
	for index := range wantKinds {
		if gotKinds[index] != wantKinds[index] {
			t.Fatalf("chain kind %d = %q, want %q (all: %v)", index, gotKinds[index], wantKinds[index], gotKinds)
		}
	}
	if len(chain.UnmappedSequences) != 1 || chain.UnmappedSequences[0] != 8 {
		t.Fatalf("unmapped sequences = %v, want [8]", chain.UnmappedSequences)
	}
	verification := chain.Entries[8]
	if verification.Kind != task.EventKindVerification || verification.ValidationState != TaskValidationPass || verification.Sequence != 8 {
		t.Fatalf("verification entry = %+v, want kind verification with PASS at sequence 8", verification)
	}
	delivery := chain.Entries[9]
	if delivery.Kind != task.EventKindDelivery || !delivery.Delivered || delivery.Sequence != 8 {
		t.Fatalf("delivery entry = %+v, want kind delivery at sequence 8", delivery)
	}
	recovery := chain.Entries[5]
	if recovery.Kind != task.EventKindRecovery || recovery.Sequence != 6 || recovery.RawType != "task.step" {
		t.Fatalf("recovery entry = %+v, want kind recovery at sequence 6 from task.step", recovery)
	}
}

func TestReplayTaskEventChainTerminalFailureIsValid(t *testing.T) {
	events := []task.Event{
		taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainEvent(2, "task.step", `{"status":"succeeded"}`),
		taskEventChainEvent(3, "task.step", `{"status":"failed"}`),
	}
	chain := ReplayTaskEventChain("q1234567", events, TaskEventChainFacts{})
	if !chain.Valid {
		t.Fatalf("terminal failure chain must be valid, reasons: %v", chain.InvalidReasons)
	}
	wantKinds := []task.EventKind{task.EventKindCreated, task.EventKindExecution, task.EventKindFailure}
	if gotKinds := taskEventChainKinds(chain); len(gotKinds) != len(wantKinds) {
		t.Fatalf("chain kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestReplayTaskEventChainDeliveredWithoutPassingVerificationIsInvalid(t *testing.T) {
	events := []task.Event{
		taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainEvent(2, "task.result", `{"status":"completed"}`),
	}
	for _, facts := range []TaskEventChainFacts{
		{ValidationState: TaskValidationFail, Delivered: true},
		{ValidationState: TaskValidationPending, Delivered: true},
		{Delivered: true},
	} {
		chain := ReplayTaskEventChain("q1234567", events, facts)
		if chain.Valid {
			t.Fatalf("delivered chain with facts %+v must be invalid", facts)
		}
		found := false
		for _, reason := range chain.InvalidReasons {
			if reason == "delivered without passing verification" {
				found = true
			}
		}
		if !found {
			t.Fatalf("chain reasons = %v, want delivered without passing verification", chain.InvalidReasons)
		}
		for _, entry := range chain.Entries {
			if entry.Kind == task.EventKindDelivery {
				t.Fatalf("chain must not contain a delivery entry, got %+v", entry)
			}
		}
	}
}

func TestReplayTaskEventChainValidationWithoutResultEventIsInvalid(t *testing.T) {
	events := []task.Event{
		taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainEvent(2, "task.step", `{"status":"succeeded"}`),
	}
	chain := ReplayTaskEventChain("q1234567", events, TaskEventChainFacts{ValidationState: TaskValidationPass})
	if chain.Valid {
		t.Fatalf("validation without result event must be invalid")
	}
	found := false
	for _, reason := range chain.InvalidReasons {
		if reason == "validation without result event" {
			found = true
		}
	}
	if !found {
		t.Fatalf("chain reasons = %v, want validation without result event", chain.InvalidReasons)
	}
}

func TestReplayTaskEventChainStructuralRules(t *testing.T) {
	cases := []struct {
		name   string
		events []task.Event
		facts  TaskEventChainFacts
		reason string
	}{
		{
			name:   "empty events",
			events: nil,
			reason: "task has no events",
		},
		{
			name: "does not start with created",
			events: []task.Event{
				taskEventChainEvent(1, "task.plan", `{}`),
				taskEventChainEvent(2, "task.step", `{"status":"succeeded"}`),
			},
			reason: "chain does not start with created",
		},
		{
			name: "sequence does not increase",
			events: []task.Event{
				taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
				taskEventChainEvent(1, "task.step", `{"status":"succeeded"}`),
			},
			reason: "event sequence 1 does not increase",
		},
		{
			name: "unknown validation state",
			events: []task.Event{
				taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
				taskEventChainEvent(2, "task.result", `{"status":"completed"}`),
			},
			facts:  TaskEventChainFacts{ValidationState: "bogus"},
			reason: `validation state "bogus" is unknown`,
		},
		{
			name: "no mappable events",
			events: []task.Event{
				taskEventChainEvent(1, "task.message", `{}`),
				taskEventChainEvent(2, "task.result", `{"status":"completed"}`),
			},
			reason: "task has no mappable events",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chain := ReplayTaskEventChain("q1234567", tc.events, tc.facts)
			if chain.Valid {
				t.Fatalf("chain must be invalid")
			}
			found := false
			for _, reason := range chain.InvalidReasons {
				if reason == tc.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("chain reasons = %v, want %q", chain.InvalidReasons, tc.reason)
			}
			if len(chain.Entries) > 0 && chain.Entries[0].Kind == task.EventKindCreated {
				// created first is fine; other rules still apply
			}
		})
	}
}

type taskEventChainStoreStub struct {
	events []task.Event
	facts  TaskEventChainFacts
	err    error
}

func (s *taskEventChainStoreStub) ListTaskEvents(context.Context, string, string, int64, int32) ([]task.Event, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.events, nil
}

func (s *taskEventChainStoreStub) TaskEventChainFacts(context.Context, string, string) (TaskEventChainFacts, error) {
	if s.err != nil {
		return TaskEventChainFacts{}, s.err
	}
	return s.facts, nil
}

func TestTaskEventChainServiceReplay(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567"}
	service, err := NewTaskEventChainService(&taskEventChainStoreStub{
		events: []task.Event{
			taskEventChainEvent(1, "task.status", `{"status":"queued"}`),
			taskEventChainEvent(2, "task.result", `{"status":"completed"}`),
		},
		facts: TaskEventChainFacts{ValidationState: TaskValidationPassNotes, Delivered: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := service.Replay(context.Background(), principal, "q1234567")
	if err != nil {
		t.Fatal(err)
	}
	if !chain.Valid {
		t.Fatalf("service replay must be valid, reasons: %v", chain.InvalidReasons)
	}
	if gotKinds := taskEventChainKinds(chain); len(gotKinds) != 3 ||
		gotKinds[0] != task.EventKindCreated || gotKinds[1] != task.EventKindVerification || gotKinds[2] != task.EventKindDelivery {
		t.Fatalf("service replay kinds = %v, want [created verification delivery]", gotKinds)
	}
}

func TestTaskEventChainServiceRejectsInvalidInputAndStoreErrors(t *testing.T) {
	service, err := NewTaskEventChainService(&taskEventChainStoreStub{
		events: []task.Event{taskEventChainEvent(1, "task.status", `{"status":"queued"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTaskEventChainService(nil); err == nil {
		t.Fatalf("nil store must be rejected")
	}
	for _, taskID := range []string{"", "bad id"} {
		if _, err := service.Replay(context.Background(), identity.Principal{UserID: "u1234567"}, taskID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("Replay with task id %q must return not found, got %v", taskID, err)
		}
	}
	if _, err := service.Replay(context.Background(), identity.Principal{}, "q1234567"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Replay with empty principal must return not found, got %v", err)
	}
	failing, err := NewTaskEventChainService(&taskEventChainStoreStub{err: errors.New("store down")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Replay(context.Background(), identity.Principal{UserID: "u1234567"}, "q1234567"); err == nil {
		t.Fatalf("store error must propagate")
	}
}
