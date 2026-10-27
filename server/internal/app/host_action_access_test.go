package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskAuthorizerStub struct {
	err        error
	hostErrors map[string]error
	calls      int
	lastHost   string
	lastScope  identity.Scope
}

func (s *taskAuthorizerStub) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, _ string, hostID string, _ []identity.Scope, scope identity.Scope) error {
	s.calls++
	s.lastHost, s.lastScope = hostID, scope
	if err, ok := s.hostErrors[hostID]; ok {
		return err
	}
	return s.err
}

func (s *taskAuthorizerStub) AuthorizeTaskExecution(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if principal.Kind == identity.KindAPIKey {
		if !principal.HasScope(scope) {
			return task.Authorization{}, domain.NewError(domain.CodeForbidden, "task scope is required")
		}
		if !principal.AllowsHost(hostID) {
			return task.Authorization{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
	}
	if err := s.AuthorizeTaskAction(ctx, principal.UserID, principal.Kind, principal.TokenID, hostID, principal.Scopes, scope); err != nil {
		return task.Authorization{}, err
	}
	authorization := authorizationSnapshot(principal)
	authorization.Version = 1
	expiresAt := time.Now().Add(time.Hour)
	authorization.ExpiresAt = &expiresAt
	return authorization, nil
}

func taskTestPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}
