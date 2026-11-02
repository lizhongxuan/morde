package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type adminControlsStoreStub struct {
	controls  AdminControls
	rule      AdminControlRule
	eval      AdminControlEvaluation
	notify    AdminControlNotifyTest
	list      int
	create    int
	update    int
	delete    int
	evaluate  int
	test      int
	principal identity.Principal
	threshold int64
	note      string
	command   UpdateAdminControlRuleCommand
	id        int64
	version   int64
}

func (s *adminControlsStoreStub) ListAdminControls(context.Context) (AdminControls, error) {
	s.list++
	return s.controls, nil
}

func (s *adminControlsStoreStub) CreateAdminControlRule(_ context.Context, p identity.Principal, threshold int64, note string) (AdminControlRule, error) {
	s.create++
	s.principal, s.threshold, s.note = p, threshold, note
	return s.rule, nil
}

func (s *adminControlsStoreStub) UpdateAdminControlRule(_ context.Context, p identity.Principal, command UpdateAdminControlRuleCommand) (AdminControlRule, error) {
	s.update++
	s.principal, s.command = p, command
	return s.rule, nil
}

func (s *adminControlsStoreStub) DeleteAdminControlRule(_ context.Context, p identity.Principal, id, version int64) error {
	s.delete++
	s.principal, s.id, s.version = p, id, version
	return nil
}

func (s *adminControlsStoreStub) EvaluateAdminControlRules(_ context.Context, p identity.Principal) (AdminControlEvaluation, error) {
	s.evaluate++
	s.principal = p
	return s.eval, nil
}

func (s *adminControlsStoreStub) CreateAdminControlNotifyTest(_ context.Context, p identity.Principal) (AdminControlNotifyTest, error) {
	s.test++
	s.principal = p
	return s.notify, nil
}

func TestAdminControlsAuthorizeValidateAndExposeOnlyNotifyRules(t *testing.T) {
	now := time.Now().UTC()
	rule := AdminControlRule{ID: 1, Threshold: 20, Enabled: true, Note: "review", CreatedAt: now, Version: 2}
	notificationID := int64(1)
	store := &adminControlsStoreStub{
		controls: AdminControls{Rules: []AdminControlRule{rule}, AccountsTotal: 20, NotifyConfigured: true,
			Notifications: []AdminControlNotification{{Kind: "rule", RuleID: &notificationID, AccountsTotal: 20, CreatedAt: now}}},
		rule: rule,
		eval: AdminControlEvaluation{AccountsTotal: 20, Fired: []AdminControlRule{{
			ID: 1, Threshold: 20, Enabled: true, Note: "review", CreatedAt: now, LastFiredAt: &now, FiredAccounts: 20, Version: 2,
		}}},
		notify: AdminControlNotifyTest{Sent: true, Configured: true, CreatedAt: now},
	}
	service, err := NewAdminControlsService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}

	listed, err := service.List(context.Background(), admin)
	if err != nil || listed.AccountsTotal != 20 || len(listed.Rules) != 1 || !listed.NotifyConfigured {
		t.Fatalf("List = %#v err=%v", listed, err)
	}
	created, err := service.Create(context.Background(), admin, 20, " review ")
	if err != nil || created != rule || store.threshold != 20 || store.note != "review" || store.principal.UserID != admin.UserID {
		t.Fatalf("Create = %#v err=%v store=%+v", created, err, store)
	}
	threshold, enabled, note := int64(25), false, "hold"
	updated, err := service.Update(context.Background(), admin, UpdateAdminControlRuleCommand{
		ID: 1, ExpectedVersion: 2, Threshold: &threshold, Enabled: &enabled, Note: &note, Rearm: true,
	})
	if err != nil || updated != rule || store.command.ExpectedVersion != 2 || *store.command.Note != "hold" || !store.command.Rearm {
		t.Fatalf("Update = %#v err=%v command=%+v", updated, err, store.command)
	}
	if err := service.Delete(context.Background(), admin, 1, 2); err != nil || store.id != 1 || store.version != 2 {
		t.Fatalf("Delete = %v id=%d version=%d", err, store.id, store.version)
	}
	evaluation, err := service.Evaluate(context.Background(), admin)
	if err != nil || evaluation.AccountsTotal != 20 || len(evaluation.Fired) != 1 {
		t.Fatalf("Evaluate = %#v err=%v", evaluation, err)
	}
	notify, err := service.NotifyTest(context.Background(), admin)
	if err != nil || !notify.Sent || !notify.Configured {
		t.Fatalf("NotifyTest = %#v err=%v", notify, err)
	}
	if store.list != 1 || store.create != 1 || store.update != 1 || store.delete != 1 || store.evaluate != 1 || store.test != 1 {
		t.Fatalf("store calls = %+v", store)
	}
}

func TestAdminControlsRejectsUnauthorizedAndInvalidMutationsBeforeStore(t *testing.T) {
	store := &adminControlsStoreStub{}
	service, err := NewAdminControlsService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	for _, denied := range []identity.Principal{
		{UserID: admin.UserID, TokenID: admin.TokenID, Role: identity.RoleUser, Kind: identity.KindSession},
		{UserID: admin.UserID, TokenID: admin.TokenID, Role: identity.RoleAdmin, Kind: identity.KindAPIKey},
		{UserID: admin.UserID, TokenID: admin.TokenID, Role: identity.RoleAdmin, Kind: identity.KindSession, ClientInstallationID: "i1234567"},
		{UserID: admin.UserID, TokenID: admin.TokenID, Role: identity.RoleAdmin, Kind: identity.KindSession, HostScope: "h1234567"},
	} {
		if _, err := service.List(context.Background(), denied); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("List accepted principal %+v: %v", denied, err)
		}
		if _, err := service.Create(context.Background(), denied, 10, ""); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("Create accepted principal %+v: %v", denied, err)
		}
		if _, err := service.Evaluate(context.Background(), denied); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("Evaluate accepted principal %+v: %v", denied, err)
		}
		if _, err := service.NotifyTest(context.Background(), denied); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("NotifyTest accepted principal %+v: %v", denied, err)
		}
	}
	for _, tc := range []struct {
		threshold int64
		note      string
	}{
		{threshold: 0}, {threshold: 1, note: string(make([]byte, 257))}, {threshold: 1, note: "bad\nvalue"},
	} {
		if _, err := service.Create(context.Background(), admin, tc.threshold, tc.note); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Create accepted threshold=%d note len=%d: %v", tc.threshold, len(tc.note), err)
		}
	}
	for _, command := range []UpdateAdminControlRuleCommand{
		{ID: 1, ExpectedVersion: 2},
		{ID: 0, ExpectedVersion: 2, Enabled: new(bool)},
		{ID: 1, ExpectedVersion: 0, Enabled: new(bool)},
		{ID: 1, ExpectedVersion: 2, Threshold: func() *int64 { value := int64(0); return &value }()},
		{ID: 1, ExpectedVersion: 2, Note: func() *string { value := "bad\nvalue"; return &value }()},
	} {
		if _, err := service.Update(context.Background(), admin, command); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Update accepted command %+v: %v", command, err)
		}
	}
	for _, values := range [][2]int64{{0, 1}, {1, 0}} {
		if err := service.Delete(context.Background(), admin, values[0], values[1]); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("Delete accepted id/version %v: %v", values, err)
		}
	}
	if store.list+store.create+store.update+store.delete+store.evaluate+store.test != 0 {
		t.Fatalf("invalid requests reached store: %+v", store)
	}
}

func TestAdminControlsRejectInvalidReceipts(t *testing.T) {
	service, err := NewAdminControlsService(&adminControlsStoreStub{controls: AdminControls{
		Rules: []AdminControlRule{}, Notifications: []AdminControlNotification{}, NotifyConfigured: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	if _, err := service.List(context.Background(), admin); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("List accepted absent internal receiver state: %v", err)
	}
}
