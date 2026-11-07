package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type scheduleExecutionAuthorizationStub struct {
	calls        int
	ownerID      string
	runID        string
	hostID       string
	scope        identity.Scope
	authorizeErr error
}

func (s *scheduleExecutionAuthorizationStub) AuthorizeScheduledTaskAction(_ context.Context, ownerID, runID, hostID string, scope identity.Scope) error {
	s.calls++
	s.ownerID, s.runID, s.hostID, s.scope = ownerID, runID, hostID, scope
	return s.authorizeErr
}

func TestScheduledExecutionAuthorizesRunIDAndRetainsScopes(t *testing.T) {
	for _, test := range []struct {
		name   string
		hostID string
		scopes []identity.Scope
		scope  identity.Scope
	}{
		{name: "hostless", scopes: identity.HostlessScheduleTaskExecutionScopes(), scope: identity.ScopeTaskSubmit},
		{name: "host-bound", hostID: "h1234567", scopes: identity.ScheduleTaskExecutionScopes(), scope: identity.ScopeComputerOperate},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, _, _ := newCredentialFixture(t)
			stub := &scheduleExecutionAuthorizationStub{}
			service.scheduledTasks = stub
			principal := identity.Principal{UserID: "u1234567", Kind: identity.KindScheduleTask, TokenID: "y1234567", Scopes: test.scopes}
			result, err := service.AuthorizeTaskExecution(context.Background(), principal, test.hostID, test.scope)
			if err != nil || result.Kind != string(identity.KindScheduleTask) || result.TokenID != principal.TokenID ||
				result.Version != 1 || result.ExpiresAt != nil || len(result.Scopes) != len(test.scopes) {
				t.Fatalf("scheduled authorization = %#v, %v", result, err)
			}
			if stub.calls != 1 || stub.ownerID != principal.UserID || stub.runID != principal.TokenID || stub.hostID != test.hostID || stub.scope != test.scope {
				t.Fatalf("schedule authorization received the wrong task identity: %#v", stub)
			}
		})
	}
}

func TestScheduledExecutionRejectsInvalidRunIDsAndRevocation(t *testing.T) {
	service, _, _, _, _, _ := newCredentialFixture(t)
	stub := &scheduleExecutionAuthorizationStub{}
	service.scheduledTasks = stub
	principal := identity.Principal{UserID: "u1234567", Kind: identity.KindScheduleTask, TokenID: "y1234567", Scopes: identity.HostlessScheduleTaskExecutionScopes()}
	for _, runID := range []string{"", "t1234567", "y123456", "y123456!"} {
		principal.TokenID = runID
		if _, err := service.AuthorizeTaskExecution(context.Background(), principal, "", identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("invalid run ID %q was accepted: %v", runID, err)
		}
	}
	if stub.calls != 0 {
		t.Fatalf("invalid run IDs reached the schedule authorizer %d times", stub.calls)
	}
	principal.TokenID = "y1234567"
	if _, err := service.AuthorizeTaskExecution(context.Background(), principal, "", identity.ScopeComputerOperate); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("hostless run gained computer scope: %v", err)
	}
	if stub.calls != 0 {
		t.Fatal("unauthorized scope reached the schedule authorizer")
	}
	stub.authorizeErr = domain.NewError(domain.CodeUnauthenticated, "scheduled grant was revoked")
	if _, err := service.AuthorizeTaskExecution(context.Background(), principal, "", identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked schedule authorization was accepted: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("revoked grant was not rechecked: calls=%d", stub.calls)
	}
}

func TestScheduledRunIDFixPreservesSessionAndClientCredentials(t *testing.T) {
	for _, kind := range []identity.Kind{identity.KindSession, identity.KindClient} {
		t.Run(string(kind), func(t *testing.T) {
			service, _, _, _, tokens, _ := newCredentialFixture(t)
			stub := &scheduleExecutionAuthorizationStub{}
			service.scheduledTasks = stub
			principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: kind, TokenID: "t1234567"}
			token := identity.Token{ID: principal.TokenID, OwnerID: principal.UserID, Kind: kind, ExpiresAt: time.Now().Add(time.Hour)}
			if kind == identity.KindClient {
				principal.ClientInstallationID = "i1234567"
				token.ClientInstallationID = principal.ClientInstallationID
				tokens.clientActive = true
			}
			tokens.values[token.ID] = token
			result, err := service.AuthorizeTaskExecution(context.Background(), principal, "", identity.ScopeTaskSubmit)
			if err != nil || result.TokenID != principal.TokenID || result.Kind != string(kind) || result.ExpiresAt == nil {
				t.Fatalf("normal authorization = %#v, %v", result, err)
			}
			now := time.Now()
			token.RevokedAt = &now
			tokens.values[token.ID] = token
			if _, err := service.AuthorizeTaskExecution(context.Background(), principal, "", identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("revoked normal credential was accepted: %v", err)
			}
			if stub.calls != 0 {
				t.Fatal("normal credential used the schedule authorizer")
			}
		})
	}
}
