package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type observationTaskStoreStub struct {
	*taskStoreStub
	records map[string]TaskWorkbenchObservationRecord
	host    host.Host
}

func (s *observationTaskStoreStub) GetHost(_ context.Context, id string) (host.Host, error) {
	if id != s.host.ID {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return s.host, nil
}

func (s *observationTaskStoreStub) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	if id != "w1234567" {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return workspace.Workspace{ID: id, OwnerID: s.host.OwnerID, State: workspace.StateReady}, nil
}

func (s *observationTaskStoreStub) CreateTaskWorkbenchObservationTicket(_ context.Context, value TaskWorkbenchObservationRecord) error {
	if len(value.TicketHash) != sha256.Size || len(value.Nonce) != sha256.Size {
		return errors.New("raw observation credential was persisted")
	}
	s.records[hex.EncodeToString(value.TicketHash)] = value
	return nil
}

func (s *observationTaskStoreStub) GetTaskWorkbenchObservationTicket(_ context.Context, hash []byte) (TaskWorkbenchObservationRecord, error) {
	value, ok := s.records[hex.EncodeToString(hash)]
	if !ok || value.UsedAt != nil || !value.ExpiresAt.After(time.Now()) {
		return TaskWorkbenchObservationRecord{}, domain.NewError(domain.CodeNotFound, "ticket not found")
	}
	return value, nil
}

func (s *observationTaskStoreStub) ConsumeTaskWorkbenchObservationTicket(_ context.Context, ticketHash, sessionHash []byte, generation int64, sessionExpiresAt time.Time) (TaskWorkbenchObservationRecord, error) {
	key := hex.EncodeToString(ticketHash)
	value, ok := s.records[key]
	if !ok || value.UsedAt != nil || value.RuntimeGeneration != generation {
		return TaskWorkbenchObservationRecord{}, domain.NewError(domain.CodeNotFound, "ticket not found")
	}
	now := time.Now().UTC()
	value.SessionHash = append([]byte(nil), sessionHash...)
	value.UsedAt = &now
	value.SessionExpiresAt = sessionExpiresAt
	s.records[key] = value
	return value, nil
}

func (s *observationTaskStoreStub) GetTaskWorkbenchObservationSession(_ context.Context, sessionHash []byte, now time.Time) (TaskWorkbenchObservationRecord, error) {
	for _, value := range s.records {
		if len(value.SessionHash) == sha256.Size && hex.EncodeToString(value.SessionHash) == hex.EncodeToString(sessionHash) &&
			value.UsedAt != nil && value.SessionExpiresAt.After(now) {
			return value, nil
		}
	}
	return TaskWorkbenchObservationRecord{}, domain.NewError(domain.CodeNotFound, "session not found")
}

type observationTaskAuthorizer struct {
	revoked bool
}

func (a *observationTaskAuthorizer) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, tokenID string, _ string, _ []identity.Scope, scope identity.Scope) error {
	if a.revoked || tokenID != "t1234567" || scope != identity.ScopeTaskRead {
		return identity.ErrInvalidCredential
	}
	return nil
}

func (a *observationTaskAuthorizer) AuthorizeTaskExecution(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if err := a.AuthorizeTaskAction(ctx, principal.UserID, principal.Kind, principal.TokenID, hostID, principal.Scopes, scope); err != nil {
		return task.Authorization{}, err
	}
	return task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID}, nil
}

func TestTaskWorkbenchObservationTicketIsSingleUseAndProjectsOnlySafeReadData(t *testing.T) {
	now := time.Now().UTC()
	artifactDigest := sha256.Sum256([]byte("verified artifact"))
	store := &observationTaskStoreStub{
		taskStoreStub: &taskStoreStub{details: TaskDetails{
			Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", WorkspaceID: "w1234567", Status: task.StatusRunning, EventSequence: 5, UpdatedAt: now},
			Artifacts: []task.Artifact{
				{ID: "a1234567", TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 3, SizeBytes: 17, SHA256: hex.EncodeToString(artifactDigest[:]), Label: "verified", Path: "secret/path.txt", MediaType: "text/plain", CreatedAt: now},
				{ID: "a7654321", TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 3, SizeBytes: 17, SHA256: "unverified", Label: "skip", Path: "private", CreatedAt: now},
			},
		}, eventPage: &TaskEventPage{HostID: "h1234567", Status: task.StatusRunning, CurrentSequence: 5,
			Items: []task.Event{{TaskID: "q1234567", Sequence: 5, Type: "task.status", Payload: []byte(`{"private":"payload"}`), CreatedAt: now}}}},
		records: map[string]TaskWorkbenchObservationRecord{},
		host:    host.Host{ID: "h1234567", OwnerID: "u1234567", State: host.StateRunning, RuntimeGeneration: 9},
	}
	authorizer := &observationTaskAuthorizer{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	ticket, err := service.IssueWorkbenchObservationTicket(context.Background(), principal, "q1234567")
	if err != nil || ticket.TaskID != "q1234567" || ticket.HostID != "h1234567" || !ticket.ExpiresAt.After(time.Now()) {
		t.Fatalf("IssueWorkbenchObservationTicket() = %#v, %v", ticket, err)
	}
	record := store.records[hex.EncodeToString(observationHash(ticket.Ticket))]
	if err := service.revalidateObservationTask(context.Background(), record); err != nil {
		t.Fatalf("pre-exchange revalidation: %v", err)
	}
	if record.TaskID != ticket.TaskID || record.ActorID != principal.UserID || record.RuntimeGeneration != 9 || record.Scope != "task:read" {
		t.Fatalf("stored ticket binding = %#v", record)
	}
	if len(ticket.Ticket) == 0 || string(record.TicketHash) == ticket.Ticket {
		t.Fatal("ticket credential was not kept hash-only")
	}
	if _, _, err := service.ConsumeWorkbenchObservationTicket(context.Background(), ticket.Ticket, "h7654321", 9); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-Host ticket exchange error = %v, want not found", err)
	}
	if _, _, err := service.ConsumeWorkbenchObservationTicket(context.Background(), ticket.Ticket, ticket.HostID, 8); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-generation ticket exchange error = %v, want not found", err)
	}
	cookie, session, err := service.ConsumeWorkbenchObservationTicket(context.Background(), ticket.Ticket, ticket.HostID, 9)
	if err != nil || cookie == "" || session.TaskID != ticket.TaskID || session.RuntimeGeneration != 9 {
		t.Fatalf("ConsumeWorkbenchObservationTicket() = %q, %#v, %v", cookie, session, err)
	}
	if _, _, err := service.ConsumeWorkbenchObservationTicket(context.Background(), ticket.Ticket, ticket.HostID, 9); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("replayed ticket error = %v, want not found", err)
	}
	snapshot, err := service.GetWorkbenchObservationSnapshot(context.Background(), cookie, ticket.HostID, 9)
	if err != nil || snapshot.Status != task.StatusRunning || len(snapshot.Artifacts) != 1 {
		t.Fatalf("observation snapshot = %#v, %v", snapshot, err)
	}
	encoded, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{"objective", "result", "secret/path.txt", "private"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("observation snapshot leaked %q: %s", forbidden, encoded)
		}
	}
	page, err := service.ListWorkbenchObservationEvents(context.Background(), cookie, ticket.HostID, 9, 4, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 5 || page.Items[0].Type != "task.status" {
		t.Fatalf("observation events = %#v, %v", page, err)
	}
	encoded, _ = json.Marshal(page)
	if bytes.Contains(encoded, []byte("private")) {
		t.Fatalf("observation event leaked its payload: %s", encoded)
	}
	sessionRecordKey := hex.EncodeToString(record.TicketHash)
	sessionRecord := store.records[sessionRecordKey]
	sessionRecord.SessionExpiresAt = time.Now().UTC().Add(-time.Second)
	store.records[sessionRecordKey] = sessionRecord
	if _, err := service.ValidateWorkbenchObservationSession(context.Background(), cookie, ticket.HostID, 9); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired observation session error = %v, want not found", err)
	}
	sessionRecord.SessionExpiresAt = time.Now().UTC().Add(time.Minute)
	store.records[sessionRecordKey] = sessionRecord
	authorizer.revoked = true
	if _, err := service.GetWorkbenchObservationSnapshot(context.Background(), cookie, ticket.HostID, 9); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked observation session error = %v, want not found", err)
	}
	authorizer.revoked = false
	expiredTicket, err := service.IssueWorkbenchObservationTicket(context.Background(), principal, "q1234567")
	if err != nil {
		t.Fatal(err)
	}
	expiredHash := hex.EncodeToString(observationHash(expiredTicket.Ticket))
	expiredRecord := store.records[expiredHash]
	expiredRecord.ExpiresAt = time.Now().UTC().Add(-time.Second)
	store.records[expiredHash] = expiredRecord
	if _, _, err := service.ConsumeWorkbenchObservationTicket(context.Background(), expiredTicket.Ticket, expiredTicket.HostID, 9); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired observation ticket error = %v, want not found", err)
	}
}
