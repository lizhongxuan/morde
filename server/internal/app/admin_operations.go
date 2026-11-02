package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const (
	maxAdminInviteSearch = 128
	maxAdminNudgeText    = 240
)

type AdminSettings struct {
	RegistrationReviewRequired     bool
	AutomaticNewUserModelBudget    bool
	AutomaticInvitationReward      bool
	AutomaticContributionReward    bool
	DefaultPersonalDataQuotaBytes  int64
	DefaultPersonalAttachmentBytes int64
}

type AdminInvite struct {
	InviterID       string
	InviterName     string
	InviteeID       string
	InviteeName     string
	InviteeStatus   identity.AccountStatus
	CreatedAt       time.Time
	InviterInvitees int64
}

type AdminInvitePage struct {
	Items         []AdminInvite
	Total         int64
	TotalInviters int64
	Limit         int
	Offset        int
}

type AdminInviteFilter struct {
	Search string
	Limit  int
	Offset int
}

type AdminSite struct {
	ID                  string
	OwnerID             string
	OwnerName           string
	HostID              string
	State               string
	CreatedAt           time.Time
	ReleaseCount        int64
	HealthyReleaseCount int64
	LatestReleaseAt     *time.Time
}

type AdminSiteSummary struct {
	Sites           int64
	ActiveSites     int64
	Releases        int64
	HealthyReleases int64
}

type AdminSitePage struct {
	Items   []AdminSite
	Summary AdminSiteSummary
	Total   int64
	Limit   int
	Offset  int
}

type AdminNudgePolicy struct {
	Enabled   bool
	URL       string
	TextZH    string
	TextEN    string
	Version   int64
	UpdatedAt *time.Time
}

type UpdateAdminNudgeCommand struct {
	ExpectedVersion int64
	Enabled         bool
	URL             string
	TextZH          string
	TextEN          string
	IP              string
}

type AdminOperationsStore interface {
	GetOrCreateUserInviteCode(context.Context, string, string) (string, error)
	ListAdminInvites(context.Context, AdminInviteFilter) (AdminInvitePage, error)
	ListAdminSites(context.Context, int, int) (AdminSitePage, error)
	GetAdminNudgePolicy(context.Context) (AdminNudgePolicy, error)
	UpdateAdminNudgePolicy(context.Context, identity.Principal, UpdateAdminNudgeCommand) (AdminNudgePolicy, error)
}

type AdminOperationsService struct {
	store AdminOperationsStore
}

func NewAdminOperationsService(store AdminOperationsStore) (*AdminOperationsService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("admin_operations", "admin operations store is required")
	}
	return &AdminOperationsService{store: store}, nil
}

func (s *AdminOperationsService) Settings(ctx context.Context, principal identity.Principal) (AdminSettings, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminSettings{}, err
	}
	return AdminSettings{
		RegistrationReviewRequired:     true,
		AutomaticNewUserModelBudget:    false,
		AutomaticInvitationReward:      false,
		AutomaticContributionReward:    false,
		DefaultPersonalDataQuotaBytes:  DefaultPersonalDataQuotaBytes,
		DefaultPersonalAttachmentBytes: DefaultPersonalDataMaxAttachmentBytes,
	}, nil
}

func (s *AdminOperationsService) InviteCode(ctx context.Context, principal identity.Principal) (string, error) {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || principal.ClientInstallationID != "" {
		return "", domain.NewError(domain.CodeForbidden, "portal session required")
	}
	for range 3 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", domain.NewError(domain.CodeInternal, "invitation code is unavailable")
		}
		code, err := s.store.GetOrCreateUserInviteCode(ctx, principal.UserID, hex.EncodeToString(random[:]))
		if domain.CodeOf(err) == domain.CodeConflict {
			continue
		}
		if err != nil {
			return "", err
		}
		if !validInvitationCode(code) {
			return "", domain.NewError(domain.CodeInternal, "stored invitation code is invalid")
		}
		return code, nil
	}
	return "", domain.NewError(domain.CodeConflict, "invitation code could not be created")
}

func (s *AdminOperationsService) ListInvites(ctx context.Context, principal identity.Principal, filter AdminInviteFilter) (AdminInvitePage, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminInvitePage{}, err
	}
	filter.Search = strings.TrimSpace(filter.Search)
	if len(filter.Search) > maxAdminInviteSearch || strings.ContainsAny(filter.Search, "\r\n\x00") {
		return AdminInvitePage{}, domain.InvalidArgument("search", "invitation search is invalid")
	}
	limit, offset, err := normalizeAdminPage(filter.Limit, filter.Offset)
	if err != nil {
		return AdminInvitePage{}, err
	}
	page, err := s.store.ListAdminInvites(ctx, AdminInviteFilter{Search: filter.Search, Limit: limit, Offset: offset})
	if err != nil {
		return AdminInvitePage{}, err
	}
	if page.Limit != limit || page.Offset != offset || page.Total < 0 || page.TotalInviters < 0 || len(page.Items) > limit {
		return AdminInvitePage{}, domain.NewError(domain.CodeInternal, "admin invitation receipt is invalid")
	}
	for _, item := range page.Items {
		if !domain.ValidUserID(item.InviterID) || !domain.ValidUserID(item.InviteeID) || item.InviterID == item.InviteeID ||
			item.InviterName == "" || item.InviteeName == "" || !item.InviteeStatus.Valid() || item.CreatedAt.IsZero() || item.InviterInvitees < 1 {
			return AdminInvitePage{}, domain.NewError(domain.CodeInternal, "admin invitation receipt is invalid")
		}
	}
	return page, nil
}

func (s *AdminOperationsService) ListSites(ctx context.Context, principal identity.Principal, limit, offset int) (AdminSitePage, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminSitePage{}, err
	}
	limit, offset, err := normalizeAdminPage(limit, offset)
	if err != nil {
		return AdminSitePage{}, err
	}
	page, err := s.store.ListAdminSites(ctx, limit, offset)
	if err != nil {
		return AdminSitePage{}, err
	}
	if page.Limit != limit || page.Offset != offset || page.Total < 0 || page.Summary.Sites < 0 || page.Summary.ActiveSites < 0 ||
		page.Summary.Releases < 0 || page.Summary.HealthyReleases < 0 || page.Summary.ActiveSites > page.Summary.Sites ||
		page.Summary.HealthyReleases > page.Summary.Releases || int64(len(page.Items)) > page.Total {
		return AdminSitePage{}, domain.NewError(domain.CodeInternal, "admin site receipt is invalid")
	}
	for _, item := range page.Items {
		if item.ID == "" || !domain.ValidUserID(item.OwnerID) || !domain.ValidHostID(item.HostID) || item.OwnerName == "" ||
			(item.State != "active" && item.State != "draining" && item.State != "stopped") || item.CreatedAt.IsZero() ||
			item.ReleaseCount < 0 || item.HealthyReleaseCount < 0 || item.HealthyReleaseCount > item.ReleaseCount {
			return AdminSitePage{}, domain.NewError(domain.CodeInternal, "admin site receipt is invalid")
		}
	}
	return page, nil
}

func (s *AdminOperationsService) NudgePolicy(ctx context.Context, principal identity.Principal) (AdminNudgePolicy, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminNudgePolicy{}, err
	}
	value, err := s.store.GetAdminNudgePolicy(ctx)
	if err != nil {
		return AdminNudgePolicy{}, err
	}
	if !validAdminNudgePolicy(value) {
		return AdminNudgePolicy{}, domain.NewError(domain.CodeInternal, "admin nudge policy is invalid")
	}
	return value, nil
}

func (s *AdminOperationsService) PublicNudgePolicy(ctx context.Context) (AdminNudgePolicy, error) {
	value, err := s.store.GetAdminNudgePolicy(ctx)
	if err != nil {
		return AdminNudgePolicy{}, err
	}
	if !validAdminNudgePolicy(value) {
		return AdminNudgePolicy{}, domain.NewError(domain.CodeInternal, "nudge policy is invalid")
	}
	if !value.Enabled {
		value.URL, value.TextZH, value.TextEN = "", "", ""
	}
	return value, nil
}

func (s *AdminOperationsService) UpdateNudgePolicy(ctx context.Context, principal identity.Principal, command UpdateAdminNudgeCommand) (AdminNudgePolicy, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminNudgePolicy{}, err
	}
	if command.ExpectedVersion < 1 || len(command.IP) > 64 {
		return AdminNudgePolicy{}, domain.InvalidArgument("nudge_policy", "nudge version is invalid")
	}
	command.URL = strings.TrimSpace(command.URL)
	command.TextZH = strings.TrimSpace(command.TextZH)
	command.TextEN = strings.TrimSpace(command.TextEN)
	if !validNudgeFields(command.Enabled, command.URL, command.TextZH, command.TextEN) {
		return AdminNudgePolicy{}, domain.InvalidArgument("nudge_policy", "enabled nudges need a GitHub URL and message; text is limited to 240 characters")
	}
	value, err := s.store.UpdateAdminNudgePolicy(ctx, principal, command)
	if err != nil {
		return AdminNudgePolicy{}, err
	}
	if !validAdminNudgePolicy(value) {
		return AdminNudgePolicy{}, domain.NewError(domain.CodeInternal, "admin nudge policy is invalid")
	}
	return value, nil
}

func validAdminNudgePolicy(value AdminNudgePolicy) bool {
	return value.Version >= 1 && validNudgeFields(value.Enabled, value.URL, value.TextZH, value.TextEN) &&
		((value.UpdatedAt == nil && value.Version == 1) || (value.UpdatedAt != nil && !value.UpdatedAt.IsZero()))
}

func validNudgeFields(enabled bool, rawURL, textZH, textEN string) bool {
	if !utf8.ValidString(textZH) || !utf8.ValidString(textEN) || utf8.RuneCountInString(textZH) > maxAdminNudgeText || utf8.RuneCountInString(textEN) > maxAdminNudgeText {
		return false
	}
	for _, text := range []string{textZH, textEN} {
		if strings.ContainsAny(text, "\r\n\x00") {
			return false
		}
	}
	if !enabled && rawURL == "" && textZH == "" && textEN == "" {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "github.com" && parsed.Hostname() != "www.github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Trim(parsed.Path, "/") == "" {
		return !enabled && rawURL == "" && textZH == "" && textEN == ""
	}
	return !enabled || textZH != "" || textEN != ""
}

func validInvitationCode(code string) bool {
	if len(code) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(code)
	return err == nil && len(decoded) == 16 && strings.ToLower(code) == code
}
