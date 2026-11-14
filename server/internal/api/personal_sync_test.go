package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalSyncServiceStub struct {
	snapshotPrincipal identity.Principal
	snapshotCursor    string
	snapshotLimit     int32
	snapshotPage      app.PersonalSnapshotPage
	changePrincipal   identity.Principal
	afterSequence     int64
	throughSequence   *int64
	changeLimit       int32
	changePage        personal.ChangePage
	historyPrincipal  identity.Principal
	historyType       string
	historyObjectID   string
	historyCursor     string
	historyLimit      int32
	historyPage       app.PersonalMemoryHistoryPage
	historyCalls      int
	snapshotCalls     int
	changeCalls       int
}

func (s *personalSyncServiceStub) GetSnapshot(_ context.Context, principal identity.Principal, cursor string, limit int32) (app.PersonalSnapshotPage, error) {
	s.snapshotCalls++
	s.snapshotPrincipal, s.snapshotCursor, s.snapshotLimit = principal, cursor, limit
	return s.snapshotPage, nil
}

func (s *personalSyncServiceStub) GetChanges(_ context.Context, principal identity.Principal, after int64, through *int64, limit int32) (personal.ChangePage, error) {
	s.changeCalls++
	s.changePrincipal, s.afterSequence, s.throughSequence, s.changeLimit = principal, after, through, limit
	return s.changePage, nil
}

func (s *personalSyncServiceStub) GetMemoryHistory(_ context.Context, principal identity.Principal, objectType, objectID, cursor string, limit int32) (app.PersonalMemoryHistoryPage, error) {
	s.historyCalls++
	s.historyPrincipal, s.historyType, s.historyObjectID, s.historyCursor, s.historyLimit = principal, objectType, objectID, cursor, limit
	return s.historyPage, nil
}

func TestPersonalSyncRoutesUseClientCredentialAndKeepFixedWatermarks(t *testing.T) {
	services := newServices()
	syncService := &personalSyncServiceStub{
		snapshotPage: app.PersonalSnapshotPage{
			Watermark: 42,
			Items: []personal.Object{{
				ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Revision: 4,
				Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
			}},
			NextCursor: "signed-next-page",
		},
		changePage: personal.ChangePage{
			Items: []personal.Change{{ChangeSequence: 19, ObjectType: personal.ObjectTypeProfile,
				ObjectID: personal.ProfileObjectID, Revision: 5, Operation: "upsert", MutationID: "profile-write-001"}},
			ThroughSequence: 24, NextAfterSequence: 19, HasMore: true,
		},
	}
	deps := services.deps()
	deps.PersonalData = &personalDataServiceStub{}
	deps.PersonalSync = syncService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	authorization := "Bearer " + credential

	response := request(router, http.MethodGet, "/api/sync/snapshot?cursor=prior-page&limit=2", "", authorization, "")
	var snapshot personalSnapshotResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil ||
		syncService.snapshotCalls != 1 || syncService.snapshotPrincipal.Kind != identity.KindClient ||
		syncService.snapshotPrincipal.UserID != testUserID || syncService.snapshotCursor != "prior-page" || syncService.snapshotLimit != 2 ||
		snapshot.Watermark != 42 || len(snapshot.Items) != 1 || snapshot.Items[0].Revision != 4 ||
		string(snapshot.Items[0].Body) != `{"name":"Muse","avatar":"dragon"}` || snapshot.NextCursor == nil || *snapshot.NextCursor != "signed-next-page" {
		t.Fatalf("snapshot = %d %+v calls=%d cursor=%q limit=%d body=%s", response.Code, snapshot,
			syncService.snapshotCalls, syncService.snapshotCursor, syncService.snapshotLimit, response.Body)
	}

	response = request(router, http.MethodGet, "/api/sync/changes?after_sequence=18&through_sequence=24&limit=1", "", authorization, "")
	var changes personalChangePageResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &changes) != nil ||
		syncService.changeCalls != 1 || syncService.changePrincipal.Kind != identity.KindClient ||
		syncService.changePrincipal.UserID != testUserID || syncService.afterSequence != 18 ||
		syncService.throughSequence == nil || *syncService.throughSequence != 24 || syncService.changeLimit != 1 ||
		changes.ThroughSequence != 24 || changes.NextAfterSequence != 19 || !changes.HasMore || len(changes.Items) != 1 ||
		changes.Items[0].ChangeSequence != 19 || changes.Items[0].MutationID != "profile-write-001" {
		t.Fatalf("changes = %d %+v calls=%d after=%d through=%v limit=%d body=%s", response.Code, changes,
			syncService.changeCalls, syncService.afterSequence, syncService.throughSequence, syncService.changeLimit, response.Body)
	}
}

func TestPersonalSyncChangesRejectInvalidSequencesBeforeServiceCall(t *testing.T) {
	services := newServices()
	syncService := &personalSyncServiceStub{}
	deps := services.deps()
	deps.PersonalData = &personalDataServiceStub{}
	deps.PersonalSync = syncService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	authorization := "Bearer " + credential
	for _, path := range []string{
		"/api/sync/changes",
		"/api/sync/changes?after_sequence=1&after_sequence=2",
		"/api/sync/changes?after_sequence=-1",
		"/api/sync/changes?after_sequence=1&through_sequence=-1",
		"/api/sync/changes?after_sequence=1&through_sequence=2&through_sequence=3",
		"/api/sync/changes?after_sequence=1&limit=101",
	} {
		response := request(router, http.MethodGet, path, "", authorization, "")
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid changes query %q = %d %s; want 400", path, response.Code, response.Body)
		}
	}
	if syncService.changeCalls != 0 {
		t.Fatalf("invalid queries reached service %d times", syncService.changeCalls)
	}
}

func TestPersonalMemoryHistoryRouteAllowsClientWithBoundPaging(t *testing.T) {
	services := newServices()
	syncService := &personalSyncServiceStub{historyPage: app.PersonalMemoryHistoryPage{
		Items: []personal.MemoryRevision{
			{Revision: 4, ChangedAt: time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC),
				Body: json.RawMessage(`{"content":"memory"}`), Restorable: true,
				SourceStatus: personal.MemoryHistorySourceAvailable, Sources: []personal.MemoryHistorySource{{Kind: "mutation_receipt", ID: "mutation-123"}}},
			{Revision: 3, ChangedAt: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC),
				Restorable: false, SourceStatus: personal.MemoryHistorySourceUnrecorded, Sources: []personal.MemoryHistorySource{}},
		},
		NextCursor: "next-signed-cursor",
	}}
	deps := services.deps()
	deps.PersonalSync = syncService
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/personal-data/memory/global/history?cursor=previous-page&limit=1", "", "Bearer "+credential, "")
	var page personalMemoryHistoryPageResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || syncService.historyCalls != 1 ||
		syncService.historyPrincipal.Kind != identity.KindClient || syncService.historyPrincipal.UserID != testUserID ||
		syncService.historyType != personal.ObjectTypeMemory || syncService.historyObjectID != "global" ||
		syncService.historyCursor != "previous-page" || syncService.historyLimit != 1 || len(page.Items) != 2 ||
		page.Items[0].Revision != 4 || page.Items[0].SourceStatus != personal.MemoryHistorySourceAvailable ||
		page.Items[1].Revision != 3 || string(page.Items[1].Body) != "null" || page.Items[1].Restorable || !strings.Contains(response.Body.String(), `"body":null`) ||
		page.NextCursor == nil || *page.NextCursor != "next-signed-cursor" {
		t.Fatalf("memory history = %d %+v calls=%d owner=%s type=%s id=%s cursor=%s limit=%d body=%s", response.Code, page,
			syncService.historyCalls, syncService.historyPrincipal.UserID, syncService.historyType, syncService.historyObjectID,
			syncService.historyCursor, syncService.historyLimit, response.Body)
	}
	for _, path := range []string{
		"/api/personal-data/memory/global/history?limit=0",
		"/api/personal-data/memory/global/history?limit=101",
		"/api/personal-data/memory/global/history?cursor=a&cursor=b",
	} {
		bad := request(router, http.MethodGet, path, "", "Bearer "+credential, "")
		if bad.Code != http.StatusBadRequest {
			t.Errorf("invalid history query %s = %d %s; want 400", path, bad.Code, bad.Body)
		}
	}
	if !clientCredentialRoute(http.MethodGet, "/api/personal-data/:type/:id/history") ||
		clientCredentialRoute(http.MethodPut, "/api/personal-data/:type/:id/history") {
		t.Fatal("client credential route allowlist does not contain only memory history GET")
	}
	unauthorized := request(router, http.MethodGet, "/api/personal-data/memory/global/history", "", "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated history request = %d %s; want 401", unauthorized.Code, unauthorized.Body)
	}
}
