package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type adminOperationsStoreStub struct {
	code         string
	invites      AdminInvitePage
	sites        AdminSitePage
	nudge        AdminNudgePolicy
	update       UpdateAdminNudgeCommand
	codeAttempts int
}

func (s *adminOperationsStoreStub) GetOrCreateUserInviteCode(_ context.Context, _ string, code string) (string, error) {
	s.codeAttempts++
	if s.code != "" {
		return s.code, nil
	}
	return code, nil
}

func (s *adminOperationsStoreStub) ListAdminInvites(_ context.Context, filter AdminInviteFilter) (AdminInvitePage, error) {
	page := s.invites
	page.Limit, page.Offset = filter.Limit, filter.Offset
	return page, nil
}

func (s *adminOperationsStoreStub) ListAdminSites(_ context.Context, limit, offset int) (AdminSitePage, error) {
	page := s.sites
	page.Limit, page.Offset = limit, offset
	return page, nil
}

func (s *adminOperationsStoreStub) GetAdminNudgePolicy(context.Context) (AdminNudgePolicy, error) {
	return s.nudge, nil
}

func (s *adminOperationsStoreStub) UpdateAdminNudgePolicy(_ context.Context, _ identity.Principal, command UpdateAdminNudgeCommand) (AdminNudgePolicy, error) {
	s.update = command
	return AdminNudgePolicy{Enabled: command.Enabled, URL: command.URL, TextZH: command.TextZH, TextEN: command.TextEN,
		Version: command.ExpectedVersion + 1, UpdatedAt: timePointer(time.Now().UTC())}, nil
}

func TestAdminOperationsPolicyAndInvitationCode(t *testing.T) {
	store := &adminOperationsStoreStub{nudge: AdminNudgePolicy{Version: 1}}
	service, err := NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	settings, err := service.Settings(context.Background(), admin)
	if err != nil || !settings.RegistrationReviewRequired || !settings.AutomaticNewUserModelBudget || settings.AutomaticInvitationReward ||
		settings.AutomaticContributionReward || settings.DefaultPersonalDataQuotaBytes != 5<<30 || settings.DefaultPersonalAttachmentBytes != 300<<20 {
		t.Fatalf("fixed settings = %+v, %v", settings, err)
	}
	code, err := service.InviteCode(context.Background(), identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleUser})
	if err != nil || len(code) != 32 || store.codeAttempts != 1 {
		t.Fatalf("invite code = %q attempts=%d err=%v", code, store.codeAttempts, err)
	}
	for _, denied := range []identity.Principal{
		{}, {UserID: "u1234567", Kind: identity.KindClient, Role: identity.RoleUser},
		{UserID: "u1234567", Kind: identity.KindSession, Role: identity.RoleUser, ClientInstallationID: "i1234567"},
	} {
		if _, err := service.InviteCode(context.Background(), denied); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("invalid invite-code principal %#v: %v", denied, err)
		}
	}
}

func TestAdminOperationsInvitesSitesAndNudgeValidation(t *testing.T) {
	now := time.Now().UTC()
	store := &adminOperationsStoreStub{
		invites: AdminInvitePage{Items: []AdminInvite{{InviterID: "u1234567", InviterName: "inviter", InviteeID: "u7654321",
			InviteeName: "invitee", InviteeStatus: identity.StatusPending, CreatedAt: now, InviterInvitees: 1}}, Total: 1, TotalInviters: 1},
		sites: AdminSitePage{Items: []AdminSite{{ID: "site_0123456789abcdef", OwnerID: "u1234567", OwnerName: "owner", HostID: "h1234567",
			State: "active", CreatedAt: now, ReleaseCount: 2, HealthyReleaseCount: 1}}, Summary: AdminSiteSummary{Sites: 1, ActiveSites: 1, Releases: 2, HealthyReleases: 1}, Total: 1},
		nudge: AdminNudgePolicy{Version: 1},
	}
	service, err := NewAdminOperationsService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	page, err := service.ListInvites(context.Background(), admin, AdminInviteFilter{Search: " invitee ", Limit: 25})
	if err != nil || page.Total != 1 || page.TotalInviters != 1 || page.Items[0].InviteeID != "u7654321" {
		t.Fatalf("invitation page = %+v, %v", page, err)
	}
	sites, err := service.ListSites(context.Background(), admin, 25, 0)
	if err != nil || sites.Summary.Releases != 2 || sites.Items[0].HealthyReleaseCount != 1 {
		t.Fatalf("site page = %+v, %v", sites, err)
	}
	if _, err := service.ListSites(context.Background(), identity.Principal{UserID: admin.UserID, TokenID: admin.TokenID, Kind: identity.KindClient, Role: identity.RoleAdmin}, 25, 0); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client token read admin sites: %v", err)
	}
	public, err := service.PublicNudgePolicy(context.Background())
	if err != nil || public.Enabled || public.URL != "" || public.TextZH != "" || public.TextEN != "" {
		t.Fatalf("default public nudge = %+v, %v", public, err)
	}
	updated, err := service.UpdateNudgePolicy(context.Background(), admin, UpdateAdminNudgeCommand{
		ExpectedVersion: 1, Enabled: true, URL: "https://github.com/morde/example",
		TextZH: "支持项目", TextEN: "Support the project", IP: "192.0.2.1",
	})
	if err != nil || !updated.Enabled || updated.Version != 2 || store.update.ExpectedVersion != 1 {
		t.Fatalf("update nudge = %+v command=%+v err=%v", updated, store.update, err)
	}
	for _, url := range []string{"http://github.com/morde/example", "https://github.com.example.test/x", "https://github.com/", "https://github.com/x?q=1"} {
		if _, err := service.UpdateNudgePolicy(context.Background(), admin, UpdateAdminNudgeCommand{
			ExpectedVersion: 1, Enabled: true, URL: url, TextEN: "ask",
		}); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("invalid nudge URL %q: %v", url, err)
		}
	}
	if _, err := service.UpdateNudgePolicy(context.Background(), admin, UpdateAdminNudgeCommand{
		ExpectedVersion: 1, Enabled: true, URL: "https://github.com/morde/example", TextZH: strings.Repeat("字", 241),
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("oversized nudge text: %v", err)
	}
}
