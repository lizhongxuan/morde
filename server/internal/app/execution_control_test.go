package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

type executionControlStoreStub struct {
	control execution.Control
	err     error
	call    string
	record  IdempotencyRecord
	epoch   int64
}

func (s *executionControlStoreStub) GetExecutionControl(context.Context, string) (execution.Control, error) {
	s.call = "status"
	return s.control, s.err
}

func (s *executionControlStoreStub) StopAllExecution(_ context.Context, record IdempotencyRecord, epoch int64) (execution.Control, error) {
	s.call, s.record, s.epoch = "stop", record, epoch
	return s.control, s.err
}

func (s *executionControlStoreStub) EnableExecution(_ context.Context, record IdempotencyRecord, epoch int64) (execution.Control, error) {
	s.call, s.record, s.epoch = "enable", record, epoch
	return s.control, s.err
}

type executionControlAuditStub struct {
	entries []audit.Entry
	err     error
}

func (s *executionControlAuditStub) Write(_ context.Context, entry audit.Entry) error {
	s.entries = append(s.entries, entry)
	return s.err
}

func TestExecutionControlRequiresPortalSessionAndAuditsChanges(t *testing.T) {
	store := &executionControlStoreStub{control: execution.Control{
		OwnerID: "u1234567", State: execution.StateDisabled, Epoch: 4, PendingTaskCount: 0, UpdatedAt: time.Now().UTC(),
	}}
	writer := &executionControlAuditStub{}
	service, err := NewExecutionControlService(store, writer)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	result, err := service.Status(context.Background(), principal)
	if err != nil || result.Epoch != 4 || store.call != "status" {
		t.Fatalf("Status() = %#v, call=%q, err=%v", result, store.call, err)
	}

	keyPrincipal := principal
	keyPrincipal.Kind = identity.KindAPIKey
	if _, err := service.Status(context.Background(), keyPrincipal); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API Key status error = %v", err)
	}
	if _, err := service.StopAll(context.Background(), keyPrincipal, 4, "stop-control-key-01"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API Key stop-all error = %v", err)
	}
	if store.call != "status" {
		t.Fatalf("API Key reached store: call=%q", store.call)
	}

	store.control = execution.Control{OwnerID: principal.UserID, State: execution.StateStopping, Epoch: 5}
	result, err = service.StopAll(context.Background(), principal, 4, "stop-control-key-01")
	if err != nil || result.State != execution.StateStopping || store.call != "stop" || store.epoch != 4 {
		t.Fatalf("StopAll() = %#v, store=%#v, err=%v", result, store, err)
	}
	if store.record.OwnerID != principal.UserID || store.record.Operation != "execution.stop_all" ||
		store.record.ResourceID != principal.UserID || store.record.RelatedID != "pending" || store.record.Key != "stop-control-key-01" {
		t.Fatalf("stop idempotency record = %#v", store.record)
	}
	if len(writer.entries) != 1 || writer.entries[0].ActorKind != audit.ActorSession || writer.entries[0].Action != audit.ActionExecutionControl || writer.entries[0].Target != principal.UserID {
		t.Fatalf("execution control audit = %#v", writer.entries)
	}

	store.control = execution.Control{OwnerID: principal.UserID, State: execution.StateEnabled, Epoch: 6}
	result, err = service.Enable(context.Background(), principal, 5, "enable-control-key-01")
	if err != nil || result.State != execution.StateEnabled || store.call != "enable" || store.epoch != 5 || len(writer.entries) != 2 {
		t.Fatalf("Enable() = %#v, store=%#v, audit=%#v, err=%v", result, store, writer.entries, err)
	}
	if _, err := service.Enable(context.Background(), principal, 0, "enable-control-key-02"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("zero epoch enable error = %v", err)
	}
}

func TestExecutionControlDoesNotAuditFailedMutation(t *testing.T) {
	store := &executionControlStoreStub{err: domain.NewError(domain.CodeConflict, "epoch mismatch")}
	writer := &executionControlAuditStub{}
	service, err := NewExecutionControlService(store, writer)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.StopAll(context.Background(), principal, 1, "stop-control-key-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("StopAll() error = %v", err)
	}
	if len(writer.entries) != 0 {
		t.Fatalf("failed mutation wrote audit entries: %#v", writer.entries)
	}

	writer.err = errors.New("audit unavailable")
	store.err = nil
	store.control = execution.Control{OwnerID: principal.UserID, State: execution.StateDisabled, Epoch: 2}
	if _, err := service.StopAll(context.Background(), principal, 1, "stop-control-key-02"); err == nil {
		t.Fatal("audit failure did not fail closed")
	}
	if len(writer.entries) != 1 {
		t.Fatalf("audit attempt count = %d", len(writer.entries))
	}
}
