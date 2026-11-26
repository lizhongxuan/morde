package app

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type passwordResetUserStore struct {
	*credentialUsers
	events *[]string
	entry  audit.Entry
}

func (s *passwordResetUserStore) ResetUserPasswordWithAudit(ctx context.Context, actorID string, expectedStatus identity.AccountStatus,
	userID, passwordHash string, entry audit.Entry, disableRelay func(context.Context) error) error {
	if disableRelay != nil {
		if err := disableRelay(ctx); err != nil {
			return err
		}
	}
	user, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if user.Status != expectedStatus {
		return domain.NewError(domain.CodeConflict, "account status changed")
	}
	user.PasswordHash = passwordHash
	user.MustChangePassword = true
	s.values[userID] = user
	s.entry = entry
	*s.events = append(*s.events, "commit:"+userID)
	return nil
}

func TestSuperAdminCanResetAnyAccountPasswordAndForceChange(t *testing.T) {
	actor := identity.User{ID: "u7654321", Name: "root-admin", Role: identity.RoleSuperAdmin,
		Status: identity.StatusApproved, QuotaHosts: 1, QuotaCPUMilli: 1000, QuotaMemMiB: 2048,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048}
	users := &credentialUsers{values: map[string]identity.User{
		actor.ID:   actor,
		"u1234567": {ID: "u1234567", Name: "pending-user", Role: identity.RoleUser, Status: identity.StatusPending},
		"u2345678": {ID: "u2345678", Name: "other-admin", Role: identity.RoleAdmin, Status: identity.StatusDisabled},
		"u3456789": {ID: "u3456789", Name: "other-super", Role: identity.RoleSuperAdmin, Status: identity.StatusApproved},
	}}
	events := []string{}
	store := &passwordResetUserStore{credentialUsers: users, events: &events}
	service, err := NewUserServiceWithHub(store, &credentialHosts{}, &clientRevocationHub{events: &events},
		&clientRevocationWorkspaceSync{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: actor.ID, Role: identity.RoleSuperAdmin, Kind: identity.KindSession, TokenID: "t7654321"}

	targets := []struct {
		id     string
		status identity.AccountStatus
	}{
		{id: "u1234567", status: identity.StatusPending},
		{id: "u2345678", status: identity.StatusDisabled},
		{id: "u3456789", status: identity.StatusApproved},
		{id: actor.ID, status: identity.StatusApproved},
	}
	for _, target := range targets {
		id, expectedStatus := target.id, target.status
		events = events[:0]
		if err := service.ResetPassword(context.Background(), principal, id, expectedStatus, "replacement-password"); err != nil {
			t.Fatalf("ResetPassword(%s) error = %v", id, err)
		}
		updated := users.values[id]
		if !updated.MustChangePassword || bcrypt.CompareHashAndPassword([]byte(updated.PasswordHash), []byte("replacement-password")) != nil {
			t.Fatalf("ResetPassword(%s) did not store a forced-change password hash: %#v", id, updated)
		}
		if store.entry.ActorID != actor.ID || store.entry.Target != id || store.entry.Action != audit.ActionUserUpdate ||
			store.entry.Detail["password_reset"] != true || store.entry.Detail["must_change_password"] != true {
			t.Fatalf("ResetPassword(%s) audit = %#v", id, store.entry)
		}
		if got, want := strings.Join(events, ","), "disable:"+id+",commit:"+id+",hub-owner-close:"+id+",reconcile:"+id; got != want {
			t.Fatalf("ResetPassword(%s) event order = %q, want %q", id, got, want)
		}
	}
}
