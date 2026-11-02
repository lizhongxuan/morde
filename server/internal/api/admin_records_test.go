package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type recordConsentAPITestService struct {
	principal    identity.Principal
	setCalls     int
	quotaCalls   int
	quotaVersion int64
	consent      record.Consent
	err          error
}

func (s *recordConsentAPITestService) List(_ context.Context, principal identity.Principal) ([]record.Consent, error) {
	s.principal = principal
	return []record.Consent{s.consent}, s.err
}
func (s *recordConsentAPITestService) Set(_ context.Context, principal identity.Principal, purpose record.Purpose, enabled bool) (record.Consent, error) {
	s.principal = principal
	s.setCalls++
	s.consent.Purpose, s.consent.Enabled = purpose, enabled
	return s.consent, s.err
}
func (*recordConsentAPITestService) Quota(context.Context, identity.Principal, string) (record.Quota, error) {
	return record.Quota{}, nil
}
func (s *recordConsentAPITestService) SetQuota(_ context.Context, principal identity.Principal, ownerID string, expectedVersion int64, retentionDays int32, limitBytes int64) (record.Quota, error) {
	s.principal = principal
	s.quotaCalls++
	s.quotaVersion = expectedVersion
	return record.Quota{OwnerID: ownerID, RetentionDays: retentionDays, LimitBytes: limitBytes, Version: expectedVersion + 1}, s.err
}

type recordAccessAPITestService struct {
	listCalls   int
	readCalls   int
	exportCalls int
	deleteCalls int
	grantCalls  int
	query       record.Query
	ownerID     string
	recordID    string
	principal   identity.Principal
	page        record.Page
	content     record.Content
	attachment  record.AttachmentContent
	settings    app.RecordAdminSettings
	access      app.RecordAdminAccess
	tombstone   record.DeleteTombstone
	err         error
}

type recordCaptureStatusAPITestService struct {
	calls     int
	principal identity.Principal
	query     record.CaptureStatusQuery
	page      record.CaptureStatusPage
	err       error
}

func (s *recordCaptureStatusAPITestService) Statuses(_ context.Context, principal identity.Principal, query record.CaptureStatusQuery) (record.CaptureStatusPage, error) {
	s.calls++
	s.principal, s.query = principal, query
	return s.page, s.err
}

func (s *recordAccessAPITestService) SetGrant(_ context.Context, _ identity.Principal, ownerID, recordAdminID string, capability record.AdminCapability, granted bool) (record.AdminGrant, error) {
	s.grantCalls++
	return record.AdminGrant{OwnerID: ownerID, AdminID: recordAdminID, Capability: capability, Granted: granted}, s.err
}
func (s *recordAccessAPITestService) AdminSettings(context.Context, identity.Principal, string) (app.RecordAdminSettings, error) {
	return s.settings, s.err
}
func (s *recordAccessAPITestService) AccessSettings(context.Context, identity.Principal, string) (app.RecordAdminAccess, error) {
	return s.access, s.err
}
func (s *recordAccessAPITestService) List(_ context.Context, principal identity.Principal, ownerID string, query record.Query) (record.Page, error) {
	s.listCalls++
	s.principal = principal
	s.ownerID, s.query = ownerID, query
	return s.page, s.err
}
func (s *recordAccessAPITestService) Read(_ context.Context, _ identity.Principal, ownerID, recordID string) (record.Content, error) {
	s.readCalls++
	s.ownerID, s.recordID = ownerID, recordID
	return s.content, s.err
}
func (s *recordAccessAPITestService) Export(_ context.Context, _ identity.Principal, ownerID, recordID string) (record.Content, error) {
	s.exportCalls++
	s.ownerID, s.recordID = ownerID, recordID
	return s.content, s.err
}
func (*recordAccessAPITestService) ListAttachments(context.Context, identity.Principal, string, string) ([]record.AttachmentMetadata, error) {
	return []record.AttachmentMetadata{}, nil
}
func (s *recordAccessAPITestService) ReadAttachment(context.Context, identity.Principal, string, string, string) (record.AttachmentContent, error) {
	return s.attachment, s.err
}
func (s *recordAccessAPITestService) Delete(_ context.Context, principal identity.Principal, ownerID, recordID string) (record.DeleteTombstone, error) {
	s.deleteCalls++
	s.principal = principal
	s.ownerID, s.recordID = ownerID, recordID
	return s.tombstone, s.err
}

func recordAdminAPIRouter(t *testing.T, role identity.Role, consents *recordConsentAPITestService, access *recordAccessAPITestService,
	captureStatuses ...RecordCaptureStatusService) *gin.Engine {
	t.Helper()
	deps := newServices().deps()
	deps.Verifier = verifierStub{role: role}
	deps.RecordConsents = consents
	deps.RecordAccess = access
	if len(captureStatuses) > 0 {
		deps.RecordCaptureStatus = captureStatuses[0]
	}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestRecordConsentRoutesAuthorizeAndValidateRequest(t *testing.T) {
	service := &recordConsentAPITestService{consent: record.Consent{OwnerID: "u1234567", Version: 2}}
	router := recordAdminAPIRouter(t, identity.RoleUser, service, nil)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	bad := requestWithCookieBody(router, http.MethodPut, "/api/me/record-consents", `{"purpose":"other","enabled":true}`, cookie, "https://portal.example.test")
	if bad.Code != http.StatusBadRequest || service.setCalls != 0 {
		t.Fatalf("invalid consent update = %d %s calls=%d", bad.Code, bad.Body, service.setCalls)
	}
	updated := requestWithCookieBody(router, http.MethodPut, "/api/me/record-consents", `{"purpose":"full_record","enabled":false}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || service.setCalls != 1 || service.principal.UserID != testUserID || !strings.Contains(updated.Body.String(), `"enabled":false`) {
		t.Fatalf("consent update = %d %s service=%+v", updated.Code, updated.Body, service)
	}
}

func TestRecordCaptureStatusRouteListsOwnerScopedMetadataOnly(t *testing.T) {
	service := &recordCaptureStatusAPITestService{page: record.CaptureStatusPage{
		Items: []record.CaptureStatus{{
			ID: "j1234567", TaskID: "q1234567", Purpose: record.PurposeFullRecord, Kind: record.ItemTurn,
			State: record.QueueQueued, CapturedAt: time.Date(2026, 11, 2, 1, 2, 3, 0, time.UTC),
			ExpiresAt: time.Date(2026, 12, 2, 1, 2, 3, 0, time.UTC),
		}}, NextBefore: "j7654321",
	}}
	router := recordAdminAPIRouter(t, identity.RoleUser, nil, nil, service)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/me/record-captures?limit=7&before_id=j7654321", cookie, "https://portal.example.test")
	if response.Code != http.StatusOK || service.calls != 1 || service.principal.UserID != testUserID ||
		service.query.Limit != 7 || service.query.BeforeID != "j7654321" {
		t.Fatalf("capture status page = %d %s service=%+v", response.Code, response.Body, service)
	}
	for _, expected := range []string{`"id":"j1234567"`, `"task_id":"q1234567"`, `"state":"queued"`, `"failure_code":""`, `"next_before":"j7654321"`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("capture status response missing %s: %s", expected, response.Body)
		}
	}
	if strings.Contains(response.Body.String(), "payload") || strings.Contains(response.Body.String(), "attachment") ||
		strings.Contains(response.Body.String(), "lease") || strings.Contains(response.Body.String(), "owner_id") {
		t.Fatalf("capture status response leaked queue internals: %s", response.Body)
	}
	clientCredential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	clientResponse := request(router, http.MethodGet, "/api/me/record-captures", "", "Bearer "+clientCredential, "")
	if clientResponse.Code != http.StatusOK || service.calls != 2 || service.principal.Kind != identity.KindClient ||
		service.principal.ClientInstallationID != "i1234567" {
		t.Fatalf("client capture status page = %d %s principal=%#v calls=%d", clientResponse.Code, clientResponse.Body, service.principal, service.calls)
	}

	badCursor := requestWithCookie(router, http.MethodGet, "/api/me/record-captures?before_id=v1234567", cookie, "https://portal.example.test")
	if badCursor.Code != http.StatusBadRequest || service.calls != 2 {
		t.Fatalf("invalid capture status cursor = %d %s calls=%d", badCursor.Code, badCursor.Body, service.calls)
	}
}

func TestAdminRecordRoutesValidateFilterAndRequireAdmin(t *testing.T) {
	access := &recordAccessAPITestService{}
	adminRouter := recordAdminAPIRouter(t, identity.RoleAdmin, nil, access)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	badFilter := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/users/u1234567/records?purpose=unknown", cookie, "")
	if badFilter.Code != http.StatusBadRequest || access.listCalls != 0 {
		t.Fatalf("invalid record filter = %d %s calls=%d", badFilter.Code, badFilter.Body, access.listCalls)
	}
	access.page = record.Page{Items: []record.RecordMetadata{{ID: "v1234567", OwnerID: "u1234567", Purpose: record.PurposeTrainingContribution, Kind: record.ItemSample}}, NextBefore: "v1234567"}
	listed := requestWithCookie(adminRouter, http.MethodGet, "/api/admin/users/u1234567/records?purpose=training_contribution&limit=10", cookie, "")
	if listed.Code != http.StatusOK || access.listCalls != 1 || access.ownerID != "u1234567" || access.query.Purpose != record.PurposeTrainingContribution || access.query.Limit != 10 || !strings.Contains(listed.Body.String(), `"next_before":"v1234567"`) {
		t.Fatalf("record list = %d %s service=%+v", listed.Code, listed.Body, access)
	}
	userRouter := recordAdminAPIRouter(t, identity.RoleUser, nil, access)
	denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/users/u1234567/records", cookie, "")
	if denied.Code != http.StatusForbidden || access.listCalls != 1 {
		t.Fatalf("non-admin record list = %d %s calls=%d", denied.Code, denied.Body, access.listCalls)
	}
}

func TestMyRecordRoutesExposeOnlyOwnerMetadataAndRequestOwnerDeletion(t *testing.T) {
	access := &recordAccessAPITestService{page: record.Page{
		Items:      []record.RecordMetadata{{ID: "v1234567", OwnerID: testUserID, Purpose: record.PurposeFullRecord, Kind: record.ItemTurn}},
		NextBefore: "v1234567",
	}, tombstone: record.DeleteTombstone{
		ID: "l1234567", OwnerID: testUserID, RecordID: "v1234567", State: record.DeletePending,
		RequestedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	}}
	router := recordAdminAPIRouter(t, identity.RoleUser, nil, access)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	listed := requestWithCookie(router, http.MethodGet, "/api/me/records?purpose=full_record&limit=10", cookie, "")
	if listed.Code != http.StatusOK || access.listCalls != 1 || access.ownerID != "" || access.principal.UserID != testUserID ||
		access.query.Purpose != record.PurposeFullRecord || access.query.Limit != 10 ||
		!strings.Contains(listed.Body.String(), `"next_before":"v1234567"`) || strings.Contains(listed.Body.String(), `"payload"`) {
		t.Fatalf("my record metadata = %d %s service=%+v", listed.Code, listed.Body, access)
	}

	badID := requestWithCookie(router, http.MethodDelete, "/api/me/records/nope", cookie, "https://portal.example.test")
	if badID.Code != http.StatusBadRequest || access.deleteCalls != 0 {
		t.Fatalf("invalid owner record delete = %d %s calls=%d", badID.Code, badID.Body, access.deleteCalls)
	}
	deleted := requestWithCookie(router, http.MethodDelete, "/api/me/records/v1234567", cookie, "https://portal.example.test")
	if deleted.Code != http.StatusAccepted || access.deleteCalls != 1 || access.ownerID != testUserID ||
		access.recordID != "v1234567" || access.principal.UserID != testUserID ||
		!strings.Contains(deleted.Body.String(), `"state":"pending"`) {
		t.Fatalf("my record delete = %d %s service=%+v", deleted.Code, deleted.Body, access)
	}
}

func TestAdminRecordQuotaRequiresExpectedVersion(t *testing.T) {
	service := &recordConsentAPITestService{}
	router := recordAdminAPIRouter(t, identity.RoleSuperAdmin, service, nil)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	missing := requestWithCookieBody(router, http.MethodPut, "/api/admin/users/u1234567/record-quota", `{"retention_days":30,"limit_bytes":1000}`, cookie, "https://portal.example.test")
	if missing.Code != http.StatusBadRequest || service.quotaCalls != 0 {
		t.Fatalf("quota without version = %d %s calls=%d", missing.Code, missing.Body, service.quotaCalls)
	}
	updated := requestWithCookieBody(router, http.MethodPut, "/api/admin/users/u1234567/record-quota", `{"expected_version":4,"retention_days":30,"limit_bytes":1000}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || service.quotaCalls != 1 || service.quotaVersion != 4 || !strings.Contains(updated.Body.String(), `"version":5`) {
		t.Fatalf("quota with version = %d %s service=%+v", updated.Code, updated.Body, service)
	}
}

func TestRecordSettingsAndAccessExposeOnlyTheirIntendedScope(t *testing.T) {
	adminAccess := &recordAccessAPITestService{access: app.RecordAdminAccess{
		Consents: []record.Consent{{OwnerID: "u1234567", Purpose: record.PurposeFullRecord}},
		Grants: []record.AdminGrant{
			{OwnerID: "u1234567", AdminID: testUserID, Capability: record.CapabilityView, Granted: true},
			{OwnerID: "u1234567", AdminID: testUserID, Capability: record.CapabilityExport, Granted: false},
		},
	}}
	router := recordAdminAPIRouter(t, identity.RoleAdmin, nil, adminAccess)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	access := requestWithCookie(router, http.MethodGet, "/api/admin/users/u1234567/record-access", cookie, "")
	if access.Code != http.StatusOK || !strings.Contains(access.Body.String(), `"capabilities":["view"]`) || strings.Contains(access.Body.String(), `"admin_id"`) || strings.Contains(access.Body.String(), `"granted_by"`) {
		t.Fatalf("record access projection = %d %s", access.Code, access.Body)
	}
	adminAccess.settings = app.RecordAdminSettings{Consents: []record.Consent{}, Grants: []record.AdminGrant{}}
	superRouter := recordAdminAPIRouter(t, identity.RoleSuperAdmin, nil, adminAccess)
	settings := requestWithCookie(superRouter, http.MethodGet, "/api/admin/users/u1234567/record-settings", cookie, "")
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"owner_id":"u1234567"`) {
		t.Fatalf("record settings response = %d %s", settings.Code, settings.Body)
	}
}

func TestAdminRecordExportAndDeleteResponses(t *testing.T) {
	access := &recordAccessAPITestService{
		content:   record.Content{Metadata: record.RecordMetadata{ID: "v1234567", OwnerID: "u1234567", Purpose: record.PurposeFullRecord, Kind: record.ItemTurn}, Payload: []byte(`{"messages":["synthetic"]}`)},
		tombstone: record.DeleteTombstone{ID: "l1234567", OwnerID: "u1234567", RecordID: "v1234567", State: record.DeletePending, RequestedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	router := recordAdminAPIRouter(t, identity.RoleAdmin, nil, access)
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	exported := requestWithCookie(router, http.MethodGet, "/api/admin/users/u1234567/records/v1234567/export", cookie, "")
	if exported.Code != http.StatusOK || access.exportCalls != 1 || exported.Header().Get("Content-Disposition") != `attachment; filename=record-v1234567.json` || exported.Body.String() != `{"messages":["synthetic"]}` {
		t.Fatalf("record export = %d %q %s calls=%d", exported.Code, exported.Header().Get("Content-Disposition"), exported.Body, access.exportCalls)
	}
	deleted := requestWithCookie(router, http.MethodDelete, "/api/admin/users/u1234567/records/v1234567", cookie, "https://portal.example.test")
	if deleted.Code != http.StatusAccepted || access.deleteCalls != 1 || !strings.Contains(deleted.Body.String(), `"state":"pending"`) || !strings.Contains(deleted.Body.String(), `"id":"l1234567"`) {
		t.Fatalf("record delete = %d %s calls=%d", deleted.Code, deleted.Body, access.deleteCalls)
	}
}

func TestRecordExportRejectsInvalidJSONAndSecretLikeData(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("not-json"),
		[]byte(`{"password":"synthetic"}`),
		[]byte(`{"text":"Bearer abcdefghijklmnopqrstuvwxyz0123456789"}`),
	} {
		if _, ok := safeRecordJSON(payload); ok {
			t.Errorf("safeRecordJSON accepted %q", payload)
		}
	}
	if _, ok := safeRecordJSON([]byte(`{"messages":["hello"]}`)); !ok {
		t.Fatal("safeRecordJSON rejected synthetic user content")
	}
}
