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
	page        record.Page
	content     record.Content
	attachment  record.AttachmentContent
	settings    app.RecordAdminSettings
	access      app.RecordAdminAccess
	tombstone   record.DeleteTombstone
	err         error
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
func (s *recordAccessAPITestService) List(_ context.Context, _ identity.Principal, ownerID string, query record.Query) (record.Page, error) {
	s.listCalls++
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
func (s *recordAccessAPITestService) Delete(_ context.Context, _ identity.Principal, ownerID, recordID string) (record.DeleteTombstone, error) {
	s.deleteCalls++
	s.ownerID, s.recordID = ownerID, recordID
	return s.tombstone, s.err
}

func recordAdminAPIRouter(t *testing.T, role identity.Role, consents *recordConsentAPITestService, access *recordAccessAPITestService) *gin.Engine {
	t.Helper()
	deps := newServices().deps()
	deps.Verifier = verifierStub{role: role}
	deps.RecordConsents = consents
	deps.RecordAccess = access
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
