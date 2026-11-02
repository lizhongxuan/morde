package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type AdminPage[T any] struct {
	Items  []T
	Total  int64
	Limit  int
	Offset int
}

type AdminInstallation struct {
	ID          string
	DisplayName string
	Platform    string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

type AdminAuthorization struct {
	ID             string
	DisplayName    string
	Platform       string
	State          string
	InstallationID string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ClaimedAt      *time.Time
}

type AdminModelCall struct {
	Source         string
	CallID         string
	ModelID        string
	ProviderModel  string
	TaskID         string
	ReservedTokens int64
	State          string
	InputTokens    *int64
	OutputTokens   *int64
	CreatedAt      time.Time
	SettledAt      *time.Time
}

type AdminUsageSummary struct {
	Calls          int64
	SettledCalls   int64
	UnknownCalls   int64
	InputTokens    int64
	OutputTokens   int64
	ActiveAccounts int64
}

type AdminUsageDay struct {
	Date           string
	Calls          int64
	InputTokens    int64
	OutputTokens   int64
	ActiveAccounts int64
}

type AdminUsageModel struct {
	ModelID      string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}

type AdminUsage struct {
	Days    int
	From    time.Time
	To      time.Time
	Summary AdminUsageSummary
	ByDay   []AdminUsageDay
	ByModel []AdminUsageModel
}

type AdminUserCounts struct {
	Total    int64
	Approved int64
	Pending  int64
	Rejected int64
	Disabled int64
}

type AdminPlatformStats struct {
	Platform            string
	Installations       int64
	ActiveInstallations int64
	Users               int64
}

type AdminCohortStats struct {
	Week             string
	Signups          int64
	WithInstallation int64
}

type AdminStats struct {
	Users                    AdminUserCounts
	Platforms                []AdminPlatformStats
	Cohorts                  []AdminCohortStats
	VersionTrackingAvailable bool
}

type AdminConsoleStore interface {
	ListAdminAgentStates(context.Context) (map[string]int64, error)
	ListAdminInstallations(context.Context, string, int, int) (AdminPage[AdminInstallation], error)
	ListAdminAuthorizations(context.Context, string, int, int) (AdminPage[AdminAuthorization], error)
	ListAdminModelCalls(context.Context, string, int, int) (AdminPage[AdminModelCall], error)
	GetAdminUsage(context.Context, time.Time, time.Time) (AdminUsage, error)
	GetAdminStats(context.Context, time.Time) (AdminStats, error)
}

type AdminConsoleService struct {
	store AdminConsoleStore
}

func NewAdminConsoleService(store AdminConsoleStore) (*AdminConsoleService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("admin_console", "admin console store is required")
	}
	return &AdminConsoleService{store: store}, nil
}

func (s *AdminConsoleService) AgentStates(ctx context.Context, principal identity.Principal) (map[string]int64, error) {
	if err := authorizeAdmin(principal); err != nil {
		return nil, err
	}
	return s.store.ListAdminAgentStates(ctx)
}

func (s *AdminConsoleService) Installations(ctx context.Context, principal identity.Principal, ownerID string, limit, offset int) (AdminPage[AdminInstallation], error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminPage[AdminInstallation]{}, err
	}
	if !domain.ValidUserID(ownerID) {
		return AdminPage[AdminInstallation]{}, domain.InvalidArgument("owner_id", "invalid user id")
	}
	limit, offset, err := normalizeAdminPage(limit, offset)
	if err != nil {
		return AdminPage[AdminInstallation]{}, err
	}
	return s.store.ListAdminInstallations(ctx, ownerID, limit, offset)
}

func (s *AdminConsoleService) Authorizations(ctx context.Context, principal identity.Principal, ownerID string, limit, offset int) (AdminPage[AdminAuthorization], error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminPage[AdminAuthorization]{}, err
	}
	if !domain.ValidUserID(ownerID) {
		return AdminPage[AdminAuthorization]{}, domain.InvalidArgument("owner_id", "invalid user id")
	}
	limit, offset, err := normalizeAdminPage(limit, offset)
	if err != nil {
		return AdminPage[AdminAuthorization]{}, err
	}
	return s.store.ListAdminAuthorizations(ctx, ownerID, limit, offset)
}

func (s *AdminConsoleService) ModelCalls(ctx context.Context, principal identity.Principal, ownerID string, limit, offset int) (AdminPage[AdminModelCall], error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminPage[AdminModelCall]{}, err
	}
	if !domain.ValidUserID(ownerID) {
		return AdminPage[AdminModelCall]{}, domain.InvalidArgument("owner_id", "invalid user id")
	}
	limit, offset, err := normalizeAdminPage(limit, offset)
	if err != nil {
		return AdminPage[AdminModelCall]{}, err
	}
	return s.store.ListAdminModelCalls(ctx, ownerID, limit, offset)
}

func (s *AdminConsoleService) Usage(ctx context.Context, principal identity.Principal, days int) (AdminUsage, error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminUsage{}, err
	}
	if days != 7 && days != 30 && days != 90 {
		return AdminUsage{}, domain.InvalidArgument("days", "days must be 7, 30, or 90")
	}
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -days)
	usage, err := s.store.GetAdminUsage(ctx, from, to)
	if err != nil {
		return AdminUsage{}, err
	}
	usage.Days, usage.From, usage.To = days, from, to
	return usage, nil
}

func (s *AdminConsoleService) Stats(ctx context.Context, principal identity.Principal) (AdminStats, error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminStats{}, err
	}
	return s.store.GetAdminStats(ctx, time.Now().UTC())
}

func normalizeAdminPage(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return 0, 0, domain.InvalidArgument("page", "limit must be between 1 and 100 and offset must be between 0 and 1000000")
	}
	return limit, offset, nil
}
