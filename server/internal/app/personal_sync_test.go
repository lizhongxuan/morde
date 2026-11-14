package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalSyncStoreStub struct {
	snapshotCalls  int
	snapshotCursor *personal.SnapshotCursor
	snapshotLimit  int32
	snapshot       personal.SnapshotBatch
	changesCalls   int
	after          int64
	through        *int64
	changeLimit    int32
	changes        personal.ChangePage
	historyCalls   int
	historyObject  string
	historyCursor  *personal.MemoryHistoryCursor
	historyLimit   int32
	historyBatch   personal.MemoryHistoryBatch
	historyErr     error
}

func (s *personalSyncStoreStub) GetPersonalSnapshotBatch(_ context.Context, _ identity.Principal, cursor *personal.SnapshotCursor, limit int32) (personal.SnapshotBatch, error) {
	s.snapshotCalls++
	s.snapshotCursor, s.snapshotLimit = cursor, limit
	return s.snapshot, nil
}

func (s *personalSyncStoreStub) GetPersonalChanges(_ context.Context, _ identity.Principal, after int64, through *int64, limit int32) (personal.ChangePage, error) {
	s.changesCalls++
	s.after, s.through, s.changeLimit = after, through, limit
	return s.changes, nil
}

func (s *personalSyncStoreStub) GetPersonalMemoryHistoryBatch(_ context.Context, _ identity.Principal, objectID string,
	cursor *personal.MemoryHistoryCursor, limit int32) (personal.MemoryHistoryBatch, error) {
	s.historyCalls++
	s.historyObject, s.historyCursor, s.historyLimit = objectID, cursor, limit
	return s.historyBatch, s.historyErr
}

func TestPersonalSyncSnapshotSignsAndBindsNextPageCursor(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	store := &personalSyncStoreStub{snapshot: personal.SnapshotBatch{
		ServiceInstanceID: "instance-a", Watermark: 14,
		Items: []personal.Object{{ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
			Revision: 3, Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`)}},
		AfterObjectType: personal.ObjectTypeProfile, AfterObjectID: personal.ProfileObjectID, HasMore: true,
	}}
	service, err := NewPersonalSyncService(store, key)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	page, err := service.GetSnapshot(context.Background(), principal, "", 0)
	if err != nil || page.Watermark != 14 || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("GetSnapshot() = %+v, %v", page, err)
	}
	if store.snapshotCalls != 1 || store.snapshotLimit != personalSyncDefaultLimit || store.snapshotCursor != nil {
		t.Fatalf("store snapshot call = calls:%d cursor:%+v limit:%d", store.snapshotCalls, store.snapshotCursor, store.snapshotLimit)
	}
	cursor, err := personal.DecodeSnapshotCursor(page.NextCursor, key)
	if err != nil || cursor.OwnerID != principal.UserID || cursor.ServiceInstanceID != "instance-a" ||
		cursor.ObjectType != personal.SnapshotScopeObjectType || cursor.ObjectID != personal.SnapshotScopeObjectID || cursor.Watermark != 14 ||
		cursor.AfterObjectType != personal.ObjectTypeProfile || cursor.AfterObjectID != personal.ProfileObjectID {
		t.Fatalf("decoded snapshot cursor = %+v, %v", cursor, err)
	}

	other := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	if _, err := service.GetSnapshot(context.Background(), other, page.NextCursor, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-account cursor error = %v, want invalid_argument", err)
	}
	if store.snapshotCalls != 1 {
		t.Fatalf("cross-account cursor reached store; calls=%d", store.snapshotCalls)
	}
	tamperedLast := "A"
	if page.NextCursor[len(page.NextCursor)-1] == 'A' {
		tamperedLast = "B"
	}
	tampered := page.NextCursor[:len(page.NextCursor)-1] + tamperedLast
	if _, err := service.GetSnapshot(context.Background(), principal, tampered, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("tampered cursor error = %v, want invalid_argument", err)
	}

	store.snapshot = personal.SnapshotBatch{ServiceInstanceID: "instance-a", Watermark: 14, Items: []personal.Object{}}
	secondPage, err := service.GetSnapshot(context.Background(), principal, page.NextCursor, 10)
	if err != nil || secondPage.Watermark != 14 || len(secondPage.Items) != 0 || secondPage.NextCursor != "" ||
		store.snapshotCursor == nil || store.snapshotCursor.AfterObjectType != personal.ObjectTypeProfile ||
		store.snapshotCursor.AfterObjectID != personal.ProfileObjectID {
		t.Fatalf("snapshot continuation = %+v cursor=%+v err=%v", secondPage, store.snapshotCursor, err)
	}
	for _, mismatch := range []personal.SnapshotBatch{
		{ServiceInstanceID: "instance-b", Watermark: 14, Items: []personal.Object{}},
		{ServiceInstanceID: "instance-a", Watermark: 15, Items: []personal.Object{}},
	} {
		store.snapshot = mismatch
		if _, err := service.GetSnapshot(context.Background(), principal, page.NextCursor, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("mismatched continuation batch %+v error = %v, want invalid_argument", mismatch, err)
		}
	}
}

func TestPersonalSyncChangesFixesThroughSequenceAndValidatesBounds(t *testing.T) {
	store := &personalSyncStoreStub{changes: personal.ChangePage{
		Items: []personal.Change{{ChangeSequence: 12, ObjectType: personal.ObjectTypeProfile,
			ObjectID: personal.ProfileObjectID, Revision: 4, Operation: "upsert", MutationID: "mutation-123"}},
		ThroughSequence: 20, NextAfterSequence: 20,
	}}
	service, err := NewPersonalSyncService(store, []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	through := int64(20)
	page, err := service.GetChanges(context.Background(), principal, 8, &through, 25)
	if err != nil || !reflect.DeepEqual(page, store.changes) {
		t.Fatalf("GetChanges() = %+v, %v; want %+v", page, err, store.changes)
	}
	if store.changesCalls != 1 || store.after != 8 || store.through == nil || *store.through != through || store.changeLimit != 25 {
		t.Fatalf("store changes call = calls:%d after:%d through:%v limit:%d", store.changesCalls, store.after, store.through, store.changeLimit)
	}
	if _, err := service.GetChanges(context.Background(), principal, -1, nil, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("negative cursor error = %v, want invalid_argument", err)
	}
	invalidThrough := int64(7)
	if _, err := service.GetChanges(context.Background(), principal, 8, &invalidThrough, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("through-before-after error = %v, want invalid_argument", err)
	}
	if store.changesCalls != 1 {
		t.Fatalf("invalid cursor reached store; calls=%d", store.changesCalls)
	}
}

func TestNewPersonalSyncServiceRequiresDataKey(t *testing.T) {
	if _, err := NewPersonalSyncService(&personalSyncStoreStub{}, []byte("short")); err == nil {
		t.Fatal("NewPersonalSyncService accepted a non-32-byte data key")
	}
}

func TestPersonalMemoryHistorySignsCursorAndBindsRequest(t *testing.T) {
	key := []byte(strings.Repeat("m", 32))
	changedAt := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	store := &personalSyncStoreStub{historyBatch: personal.MemoryHistoryBatch{
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", HasMore: true,
		Items: []personal.MemoryRevision{{Revision: 4, ChangedAt: changedAt, Body: json.RawMessage(`{"content":"prior"}`),
			Restorable: true, SourceStatus: personal.MemoryHistorySourceAvailable, Sources: []personal.MemoryHistorySource{{
				Kind: "mutation_receipt", ID: "mutation-123",
			}}}},
	}}
	service, err := NewPersonalSyncService(store, key)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	page, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeMemory, "global", "", 0)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" || store.historyCalls != 1 || store.historyObject != "global" ||
		store.historyLimit != personalSyncDefaultLimit || store.historyCursor != nil {
		t.Fatalf("GetMemoryHistory() = %+v, err=%v store=%+v", page, err, store)
	}
	cursor, err := personal.DecodeMemoryHistoryCursor(page.NextCursor, key)
	if err != nil || cursor.OwnerID != principal.UserID || cursor.ServiceInstanceID != store.historyBatch.ServiceInstanceID ||
		cursor.ObjectType != personal.ObjectTypeMemory || cursor.ObjectID != "global" || cursor.AfterRevision != 4 {
		t.Fatalf("decoded memory history cursor = %+v, err=%v", cursor, err)
	}

	store.historyBatch = personal.MemoryHistoryBatch{ServiceInstanceID: cursor.ServiceInstanceID, Items: []personal.MemoryRevision{}}
	if _, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeMemory, "global", page.NextCursor, 5); err != nil ||
		store.historyCursor == nil || store.historyCursor.AfterRevision != 4 || store.historyLimit != 5 {
		t.Fatalf("history continuation = cursor %+v limit %d err %v", store.historyCursor, store.historyLimit, err)
	}

	other := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	if _, err := service.GetMemoryHistory(context.Background(), other, personal.ObjectTypeMemory, "global", page.NextCursor, 5); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-owner history cursor error = %v, want invalid_argument", err)
	}
	if _, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeMemory, "user", page.NextCursor, 5); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-object history cursor error = %v, want invalid_argument", err)
	}
	if _, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeProfile, "default", "", 5); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("profile history error = %v, want invalid_argument", err)
	}
	if _, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeMemory, "global", "", 101); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("out-of-range history limit error = %v, want invalid_argument", err)
	}
	apiKeyPrincipal := principal
	apiKeyPrincipal.Kind = identity.KindAPIKey
	if _, err := service.GetMemoryHistory(context.Background(), apiKeyPrincipal, personal.ObjectTypeMemory, "global", "", 10); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("API Key history authorization error = %v, want unauthenticated", err)
	}
	if store.historyCalls != 2 {
		t.Fatalf("invalid history request reached store; calls=%d", store.historyCalls)
	}
}

func TestPersonalMemoryHistoryRejectsInvalidProvenanceAndBodyCombinations(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	for _, revision := range []personal.MemoryRevision{
		{Revision: 1, ChangedAt: time.Now(), Body: json.RawMessage(`{"content":"x"}`), Restorable: true,
			SourceStatus: personal.MemoryHistorySourceUnavailable, Sources: []personal.MemoryHistorySource{}},
		{Revision: 1, ChangedAt: time.Now(), Restorable: true,
			SourceStatus: personal.MemoryHistorySourceUnrecorded, Sources: []personal.MemoryHistorySource{}},
		{Revision: 1, ChangedAt: time.Now(), Body: json.RawMessage(`{"content":"x"}`), Restorable: true,
			SourceStatus: personal.MemoryHistorySourceAvailable, Sources: nil},
	} {
		store := &personalSyncStoreStub{historyBatch: personal.MemoryHistoryBatch{
			ServiceInstanceID: "00000000-0000-4000-8000-000000000001", Items: []personal.MemoryRevision{revision},
		}}
		service, err := NewPersonalSyncService(store, []byte(strings.Repeat("k", 32)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.GetMemoryHistory(context.Background(), principal, personal.ObjectTypeMemory, "global", "", 10); domain.CodeOf(err) != domain.CodeInternal {
			t.Errorf("invalid history revision %+v error = %v, want internal", revision, err)
		}
	}
}
