package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalDataQuotaStoreStub struct {
	quota         PersonalDataQuota
	getCalls      int
	setCalls      int
	reserveCalls  int
	releaseCalls  int
	cleanupCalls  int
	principal     identity.Principal
	ownerID       string
	expected      int64
	quotaBytes    int64
	maxAttachment int64
	uploadID      string
	sizeBytes     int64
	reservationID string
	cleanupNow    time.Time
	cleanupLimit  int32
	cleanupCount  int32
	getErr        error
	setErr        error
	reserveErr    error
	releaseErr    error
	cleanupErr    error
}

func (s *personalDataQuotaStoreStub) GetAdminPersonalDataQuota(_ context.Context, principal identity.Principal, ownerID string) (PersonalDataQuota, error) {
	s.getCalls++
	s.principal, s.ownerID = principal, ownerID
	return s.quota, s.getErr
}

func (s *personalDataQuotaStoreStub) SetAdminPersonalDataQuota(_ context.Context, principal identity.Principal, ownerID string, expectedVersion, quotaBytes, maxAttachmentBytes int64) (PersonalDataQuota, error) {
	s.setCalls++
	s.principal, s.ownerID = principal, ownerID
	s.expected, s.quotaBytes, s.maxAttachment = expectedVersion, quotaBytes, maxAttachmentBytes
	return s.quota, s.setErr
}

func (s *personalDataQuotaStoreStub) ReservePersonalAttachment(_ context.Context, principal identity.Principal, uploadID string, sizeBytes int64) (string, error) {
	s.reserveCalls++
	s.principal, s.uploadID, s.sizeBytes = principal, uploadID, sizeBytes
	if s.reservationID == "" {
		s.reservationID = "pq_0123456789abcdef0123456789abcdef"
	}
	return s.reservationID, s.reserveErr
}

func (s *personalDataQuotaStoreStub) ReleasePersonalAttachmentReservation(_ context.Context, ownerID, reservationID string) error {
	s.releaseCalls++
	s.ownerID, s.reservationID = ownerID, reservationID
	return s.releaseErr
}

func (s *personalDataQuotaStoreStub) CleanupExpiredPersonalDataQuotaReservations(_ context.Context, now time.Time, limit int32) (int32, error) {
	s.cleanupCalls++
	s.cleanupNow, s.cleanupLimit = now, limit
	return s.cleanupCount, s.cleanupErr
}

func personalDataQuotaAdmin() identity.Principal {
	return identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
}

func TestPersonalDataQuotaAdminGetSetAuthorizationAndValidation(t *testing.T) {
	store := &personalDataQuotaStoreStub{quota: PersonalDataQuota{
		OwnerID: "u7654321", UsedBytes: 10, ReservedBytes: 4, QuotaBytes: DefaultPersonalDataQuotaBytes,
		MaxAttachmentBytes: DefaultPersonalDataMaxAttachmentBytes, Version: 2,
	}}
	service, err := NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := personalDataQuotaAdmin()
	got, err := service.GetAdminPersonalDataQuota(ctx, admin, "u7654321")
	if err != nil || got != store.quota || store.getCalls != 1 || !reflect.DeepEqual(store.principal, admin) || store.ownerID != "u7654321" {
		t.Fatalf("get quota = %+v, err=%v, store=%+v", got, err, store)
	}
	updated, err := service.SetAdminPersonalDataQuota(ctx, admin, "u7654321", 2, 2048, 512)
	if err != nil || updated != store.quota || store.setCalls != 1 || store.expected != 2 || store.quotaBytes != 2048 || store.maxAttachment != 512 {
		t.Fatalf("set quota = %+v, err=%v, store=%+v", updated, err, store)
	}

	beforeGet, beforeSet := store.getCalls, store.setCalls
	for _, denied := range []identity.Principal{
		{UserID: admin.UserID, TokenID: admin.TokenID, Kind: identity.KindSession, Role: identity.RoleUser},
		{UserID: admin.UserID, TokenID: admin.TokenID, Kind: identity.KindClient, Role: identity.RoleAdmin, ClientInstallationID: "i1234567"},
		{Kind: identity.KindSystem, Role: identity.RoleAdmin},
		{UserID: admin.UserID, TokenID: admin.TokenID, Kind: identity.KindSession, Role: identity.RoleAdmin, HostScope: "h1234567"},
	} {
		if _, err := service.GetAdminPersonalDataQuota(ctx, denied, "u7654321"); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("GET accepted non-admin principal %+v: %v", denied, err)
		}
		if _, err := service.SetAdminPersonalDataQuota(ctx, denied, "u7654321", 2, 2048, 512); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("PATCH accepted non-admin principal %+v: %v", denied, err)
		}
	}
	if store.getCalls != beforeGet || store.setCalls != beforeSet {
		t.Fatalf("unauthorized admin calls reached store: GET %d→%d PATCH %d→%d", beforeGet, store.getCalls, beforeSet, store.setCalls)
	}
	for _, values := range []struct {
		owner      string
		expected   int64
		quota      int64
		attachment int64
	}{
		{owner: "bad", expected: 1, quota: 1, attachment: 1},
		{owner: "u7654321", expected: 0, quota: 1, attachment: 1},
		{owner: "u7654321", expected: 1, quota: 0, attachment: 1},
		{owner: "u7654321", expected: 1, quota: 1, attachment: 0},
		{owner: "u7654321", expected: 1, quota: 1, attachment: personal.MaxAttachmentBytes + 1},
	} {
		if _, err := service.SetAdminPersonalDataQuota(ctx, admin, values.owner, values.expected, values.quota, values.attachment); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("invalid quota limits accepted: %+v, err=%v", values, err)
		}
	}
	if store.setCalls != beforeSet {
		t.Fatalf("invalid quota limits reached store %d times", store.setCalls-beforeSet)
	}
}

func TestPersonalDataQuotaRejectsInvalidStoreReceipt(t *testing.T) {
	store := &personalDataQuotaStoreStub{quota: PersonalDataQuota{OwnerID: "u7654321", QuotaBytes: 1, MaxAttachmentBytes: 1, Version: 1}}
	service, err := NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	store.quota.OwnerID = "u0000000"
	if _, err := service.GetAdminPersonalDataQuota(context.Background(), personalDataQuotaAdmin(), "u7654321"); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid GET receipt error = %v", err)
	}
	store.quota.OwnerID = "u7654321"
	store.quota.ReservedBytes = -1
	if _, err := service.SetAdminPersonalDataQuota(context.Background(), personalDataQuotaAdmin(), "u7654321", 1, 4, 2); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid PATCH receipt error = %v", err)
	}
}

func TestPersonalDataQuotaAttachmentReserveReleaseAndCleanup(t *testing.T) {
	store := &personalDataQuotaStoreStub{cleanupCount: 2}
	service, err := NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []identity.Principal{
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"},
	} {
		id, err := service.ReservePersonalAttachment(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef", personal.MaxAttachmentBytes)
		if err != nil || id != store.reservationID || store.principal.UserID != principal.UserID || store.principal.Kind != principal.Kind ||
			store.principal.TokenID != principal.TokenID || store.uploadID != "pu_0123456789abcdef0123456789abcdef" || store.sizeBytes != personal.MaxAttachmentBytes {
			t.Fatalf("reserve for %s = %q, err=%v, store=%+v", principal.Kind, id, err, store)
		}
	}
	before := store.reserveCalls
	for _, principal := range []identity.Principal{
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567"},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567"},
		{UserID: "invalid", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
	} {
		if _, err := service.ReservePersonalAttachment(context.Background(), principal, "pu_0123456789abcdef0123456789abcdef", 1); err == nil {
			t.Errorf("reserve accepted principal %+v", principal)
		}
	}
	for _, input := range []struct {
		uploadID string
		size     int64
	}{
		{uploadID: "bad", size: 1},
		{uploadID: "pu_0123456789abcdef0123456789abcdef", size: -1},
		{uploadID: "pu_0123456789abcdef0123456789abcdef", size: personal.MaxAttachmentBytes + 1},
	} {
		if _, err := service.ReservePersonalAttachment(context.Background(), identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}, input.uploadID, input.size); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("invalid reservation accepted %+v: %v", input, err)
		}
	}
	if store.reserveCalls != before {
		t.Fatalf("invalid reservation calls reached store: %d→%d", before, store.reserveCalls)
	}
	if err := service.ReleasePersonalAttachmentReservation(context.Background(), "u1234567", "pq_0123456789abcdef0123456789abcdef"); err != nil || store.releaseCalls != 1 || store.ownerID != "u1234567" {
		t.Fatalf("release = %v, store=%+v", err, store)
	}
	if err := service.ReleasePersonalAttachmentReservation(context.Background(), "bad", "pq_0123456789abcdef0123456789abcdef"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.releaseCalls != 1 {
		t.Fatalf("invalid release = %v, calls=%d", err, store.releaseCalls)
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	count, err := service.CleanupExpiredPersonalDataQuotaReservations(context.Background(), now, 10)
	if err != nil || count != 2 || store.cleanupCalls != 1 || !store.cleanupNow.Equal(now.UTC()) || store.cleanupLimit != 10 {
		t.Fatalf("cleanup count=%d err=%v store=%+v", count, err, store)
	}
	store.cleanupErr = errors.New("database unavailable")
	if _, err := service.CleanupExpiredPersonalDataQuotaReservations(context.Background(), now, 10); err == nil {
		t.Fatal("cleanup swallowed store error")
	}
	if _, err := service.CleanupExpiredPersonalDataQuotaReservations(context.Background(), time.Time{}, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("zero cleanup time error = %v", err)
	}
	if _, err := service.CleanupExpiredPersonalDataQuotaReservations(context.Background(), now, maxPersonalDataQuotaCleanupLimit+1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("oversized cleanup limit error = %v", err)
	}
}

func TestNewPersonalDataQuotaServiceRequiresStore(t *testing.T) {
	if _, err := NewPersonalDataQuotaService(nil); err == nil {
		t.Fatal("quota service accepted a missing store")
	}
}
