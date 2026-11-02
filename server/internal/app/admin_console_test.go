package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type adminConsoleStoreStub struct {
	installations int
	usage         int
	from          time.Time
	to            time.Time
}

func (s *adminConsoleStoreStub) ListAdminAgentStates(context.Context) (map[string]int64, error) {
	return nil, nil
}
func (s *adminConsoleStoreStub) ListAdminInstallations(_ context.Context, _ string, limit, offset int) (AdminPage[AdminInstallation], error) {
	s.installations++
	return AdminPage[AdminInstallation]{Limit: limit, Offset: offset}, nil
}
func (s *adminConsoleStoreStub) ListAdminAuthorizations(context.Context, string, int, int) (AdminPage[AdminAuthorization], error) {
	return AdminPage[AdminAuthorization]{}, nil
}
func (s *adminConsoleStoreStub) ListAdminModelCalls(context.Context, string, int, int) (AdminPage[AdminModelCall], error) {
	return AdminPage[AdminModelCall]{}, nil
}
func (s *adminConsoleStoreStub) GetAdminUsage(_ context.Context, from, to time.Time) (AdminUsage, error) {
	s.usage++
	s.from, s.to = from, to
	return AdminUsage{}, nil
}
func (s *adminConsoleStoreStub) GetAdminStats(context.Context, time.Time) (AdminStats, error) {
	return AdminStats{}, nil
}

func TestAdminConsoleServiceAuthorizesAndBoundsQueries(t *testing.T) {
	store := &adminConsoleStoreStub{}
	service, err := NewAdminConsoleService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	if page, err := service.Installations(context.Background(), admin, "u1234567", 0, 5); err != nil || page.Limit != 25 || page.Offset != 5 || store.installations != 1 {
		t.Fatalf("Installations(default page) = %#v, %v calls=%d", page, err, store.installations)
	}
	if _, err := service.Installations(context.Background(), admin, "bad-id", 10, 0); domain.CodeOf(err) != domain.CodeInvalidArgument || store.installations != 1 {
		t.Fatalf("Installations(invalid ID) = %v calls=%d", err, store.installations)
	}
	if _, err := service.Installations(context.Background(), admin, "u1234567", 101, 0); domain.CodeOf(err) != domain.CodeInvalidArgument || store.installations != 1 {
		t.Fatalf("Installations(invalid limit) = %v calls=%d", err, store.installations)
	}
	user := admin
	user.Role = identity.RoleUser
	if _, err := service.Usage(context.Background(), user, 30); domain.CodeOf(err) != domain.CodeForbidden || store.usage != 0 {
		t.Fatalf("Usage(user) = %v calls=%d", err, store.usage)
	}
	if _, err := service.Usage(context.Background(), admin, 31); domain.CodeOf(err) != domain.CodeInvalidArgument || store.usage != 0 {
		t.Fatalf("Usage(invalid period) = %v calls=%d", err, store.usage)
	}
	if usage, err := service.Usage(context.Background(), admin, 7); err != nil || usage.Days != 7 || store.usage != 1 || store.to.Sub(store.from) < 7*24*time.Hour-time.Second {
		t.Fatalf("Usage(admin) = %#v, %v calls=%d window=%s", usage, err, store.usage, store.to.Sub(store.from))
	}
}
