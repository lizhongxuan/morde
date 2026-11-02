package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type adminQualityActivityStoreStub struct {
	activity AdminActivity
	err      error
	calls    int
	from     time.Time
	to       time.Time
}

func (s *adminQualityActivityStoreStub) GetAdminActivity(_ context.Context, from, to time.Time) (AdminActivity, error) {
	s.calls++
	s.from, s.to = from, to
	return s.activity, s.err
}

func TestAdminQualityActivityAuthorizesBoundsAndReportsUnavailableSources(t *testing.T) {
	store := &adminQualityActivityStoreStub{}
	service, err := NewAdminQualityActivityService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	user := admin
	user.Role = identity.RoleUser
	if _, err := service.Quality(context.Background(), user, 30); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Quality(non-admin) = %v", err)
	}
	if _, err := service.Activity(context.Background(), user, 30); domain.CodeOf(err) != domain.CodeForbidden || store.calls != 0 {
		t.Fatalf("Activity(non-admin) = %v calls=%d", err, store.calls)
	}
	if _, err := service.Places(context.Background(), user, 30); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Places(non-admin) = %v", err)
	}
	for _, days := range []int{0, 6, 14, 91} {
		if _, err := service.Quality(context.Background(), admin, days); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("Quality(days=%d) = %v", days, err)
		}
		if _, err := service.Activity(context.Background(), admin, days); domain.CodeOf(err) != domain.CodeInvalidArgument || store.calls != 0 {
			t.Fatalf("Activity(days=%d) = %v calls=%d", days, err, store.calls)
		}
		if _, err := service.Places(context.Background(), admin, days); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("Places(days=%d) = %v", days, err)
		}
	}
	quality, err := service.Quality(context.Background(), admin, 30)
	if err != nil || quality.Available || quality.Days != 30 || quality.UnavailableReason == "" || store.calls != 0 {
		t.Fatalf("Quality = %#v err=%v store calls=%d", quality, err, store.calls)
	}
	places, err := service.Places(context.Background(), admin, 90)
	if err != nil || places.Available || places.Days != 90 || places.UnavailableReason == "" || store.calls != 0 {
		t.Fatalf("Places = %#v err=%v store calls=%d", places, err, store.calls)
	}
}

func TestAdminActivityUsesCalendarWindowAndFillsEmptyDays(t *testing.T) {
	todayString := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")
	store := &adminQualityActivityStoreStub{activity: AdminActivity{
		ByDay:         []AdminActivityDay{{Date: "2000-01-01", HubOperations: 100}, {Date: todayString, TaskEvents: 2}},
		HubOperations: []AdminHubOperationCount{{Action: "files", Status: "succeeded", Count: 1}},
	}}
	service, err := NewAdminQualityActivityService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleSuperAdmin, Kind: identity.KindSession}
	activity, err := service.Activity(context.Background(), admin, 7)
	if err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 || activity.Days != 7 || activity.From.Location() != time.UTC || activity.From.Hour() != 0 || activity.To.Location() != time.UTC || len(activity.ByDay) != 7 {
		t.Fatalf("Activity window/result = %#v from=%s to=%s calls=%d", activity, activity.From, activity.To, store.calls)
	}
	for index, row := range activity.ByDay {
		if row.Date != activity.From.AddDate(0, 0, index).Format("2006-01-02") {
			t.Fatalf("activity date[%d] = %#v", index, row)
		}
		if row.Date == "2000-01-01" || row.HubOperations == 100 {
			t.Fatalf("out-of-range day survived normalization: %#v", row)
		}
	}
	if activity.ByDay[6].TaskEvents != 2 || len(activity.HubOperations) != 1 || activity.TaskEvents == nil || activity.AuditEvents == nil {
		t.Fatalf("Activity did not preserve summaries or empty slices: %#v", activity)
	}
}

func TestAdminActivityRequiresStore(t *testing.T) {
	if _, err := NewAdminQualityActivityService(nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("NewAdminQualityActivityService(nil) = %v", err)
	}
}

func TestAdminActivitySupportedWindowsAndEmptyAggregates(t *testing.T) {
	store := &adminQualityActivityStoreStub{}
	service, err := NewAdminQualityActivityService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	for _, days := range []int{7, 30, 90} {
		activity, err := service.Activity(context.Background(), admin, days)
		if err != nil {
			t.Fatalf("Activity(days=%d): %v", days, err)
		}
		if activity.Days != days || len(activity.ByDay) != days || len(activity.HubOperations) != 0 ||
			len(activity.TaskEvents) != 0 || len(activity.AuditEvents) != 0 {
			t.Fatalf("empty Activity(days=%d) = %#v", days, activity)
		}
	}
	if store.calls != 3 {
		t.Fatalf("store called %d times; want one call per supported window", store.calls)
	}
}
