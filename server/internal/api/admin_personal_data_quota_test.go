package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type adminPersonalDataQuotaServiceStub struct {
	quota     app.PersonalDataQuota
	getCalls  int
	setCalls  int
	ownerID   string
	setErr    error
	expected  int64
	requested app.PersonalDataQuota
}

func (s *adminPersonalDataQuotaServiceStub) GetAdminPersonalDataQuota(_ context.Context, _ identity.Principal, ownerID string) (app.PersonalDataQuota, error) {
	s.getCalls++
	s.ownerID = ownerID
	return s.quota, nil
}

func (s *adminPersonalDataQuotaServiceStub) SetAdminPersonalDataQuota(_ context.Context, _ identity.Principal, ownerID string, expectedVersion, quotaBytes, maxAttachmentBytes int64) (app.PersonalDataQuota, error) {
	s.setCalls++
	s.ownerID, s.expected = ownerID, expectedVersion
	if s.setErr != nil {
		return app.PersonalDataQuota{}, s.setErr
	}
	s.quota = app.PersonalDataQuota{
		UsedBytes: s.quota.UsedBytes, ReservedBytes: s.quota.ReservedBytes,
		QuotaBytes: quotaBytes, MaxAttachmentBytes: maxAttachmentBytes, Version: expectedVersion + 1,
	}
	s.requested = s.quota
	return s.quota, nil
}

func TestAdminPersonalDataQuotaAPIReadsUpdatesAuditsAndRejectsUnauthorized(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	quotaService := &adminPersonalDataQuotaServiceStub{quota: app.PersonalDataQuota{
		UsedBytes: 1024, ReservedBytes: 128, QuotaBytes: app.DefaultPersonalDataQuotaBytes,
		MaxAttachmentBytes: 300 << 20, Version: 1,
	}}
	writer := &adminAuditWriterStub{}
	deps := services.deps()
	deps.PersonalDataQuota, deps.AdminAudit = quotaService, writer
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	read := requestWithCookie(router, http.MethodGet, "/api/admin/users/"+testUserID+"/personal-data-quota", cookie, "")
	if read.Code != http.StatusOK || quotaService.getCalls != 1 || quotaService.ownerID != testUserID {
		t.Fatalf("GET quota = %d %s calls=%d owner=%s", read.Code, read.Body, quotaService.getCalls, quotaService.ownerID)
	}
	var got adminPersonalDataQuotaResponse
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil || got.UsedBytes != 1024 || got.ReservedBytes != 128 ||
		got.DefaultQuotaBytes != app.DefaultPersonalDataQuotaBytes || got.DefaultMaxAttachmentBytes != 300<<20 {
		t.Fatalf("GET quota response = %+v err=%v", got, err)
	}
	updated := requestWithCookieBody(router, http.MethodPatch, "/api/admin/users/"+testUserID+"/personal-data-quota",
		`{"expected_version":1,"quota_bytes":2048,"max_attachment_bytes":524288}`, cookie, "https://portal.example.test")
	if updated.Code != http.StatusOK || quotaService.setCalls != 1 || quotaService.expected != 1 ||
		quotaService.requested.QuotaBytes != 2048 || quotaService.requested.MaxAttachmentBytes != 524288 || len(writer.entries) != 1 {
		t.Fatalf("PATCH quota = %d %s set=%d expected=%d requested=%+v audit=%+v", updated.Code, updated.Body,
			quotaService.setCalls, quotaService.expected, quotaService.requested, writer.entries)
	}
	if writer.entries[0].Action != audit.ActionPersonalDataQuotaUpdate || writer.entries[0].ActorKind != audit.ActorSession ||
		writer.entries[0].Target != testUserID || writer.entries[0].Detail["quota_bytes_before"] != int64(app.DefaultPersonalDataQuotaBytes) {
		t.Fatalf("quota update audit = %+v", writer.entries[0])
	}
	var response adminPersonalDataQuotaResponse
	if err := json.Unmarshal(updated.Body.Bytes(), &response); err != nil || response.QuotaBytes != 2048 || response.Version != 2 {
		t.Fatalf("PATCH quota response = %+v err=%v", response, err)
	}
	invalid := requestWithCookieBody(router, http.MethodPatch, "/api/admin/users/"+testUserID+"/personal-data-quota",
		`{"quota_bytes":2048,"max_attachment_bytes":524288}`, cookie, "https://portal.example.test")
	if invalid.Code != http.StatusBadRequest || quotaService.setCalls != 1 {
		t.Fatalf("PATCH without expected version = %d %s calls=%d", invalid.Code, invalid.Body, quotaService.setCalls)
	}

	services.verifier.role = identity.RoleUser
	userDeps := services.deps()
	userDeps.PersonalDataQuota = quotaService
	userRouter, err := New(userDeps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	getCallsBeforeDenied := quotaService.getCalls
	denied := requestWithCookie(userRouter, http.MethodGet, "/api/admin/users/"+testUserID+"/personal-data-quota", cookie, "")
	if denied.Code != http.StatusForbidden || quotaService.getCalls != getCallsBeforeDenied {
		t.Fatalf("non-admin quota read = %d %s calls=%d", denied.Code, denied.Body, quotaService.getCalls)
	}
}

func TestAdminPersonalDataQuotaAPIMapsQuotaConflict(t *testing.T) {
	services := newServices()
	services.verifier.role = identity.RoleAdmin
	quotaService := &adminPersonalDataQuotaServiceStub{
		quota:  app.PersonalDataQuota{UsedBytes: 0, ReservedBytes: 0, QuotaBytes: 1, MaxAttachmentBytes: 1, Version: 3},
		setErr: domain.NewError(domain.CodeConflict, "personal data quota version changed"),
	}
	deps := services.deps()
	deps.PersonalDataQuota = quotaService
	deps.AdminAudit = &adminAuditWriterStub{}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBody(router, http.MethodPatch, "/api/admin/users/"+testUserID+"/personal-data-quota",
		`{"expected_version":2,"quota_bytes":2,"max_attachment_bytes":2}`, cookie, "https://portal.example.test")
	assertError(t, response, http.StatusConflict, "conflict")
	if quotaService.setCalls != 1 {
		t.Fatalf("conflicting PATCH calls=%d", quotaService.setCalls)
	}
}
