package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type TokenService struct {
	hosts        HostStore
	tokens       TokenStore
	audit        AuditWriter
	sharing      HostPermissionAuthorizer
	apiKeyTTL    time.Duration
	apiKeyMaxTTL time.Duration
}

type APIKeyInfo struct {
	ID        string
	Key       string
	HostIDs   []string
	Scopes    []identity.Scope
	ExpiresAt time.Time
}

func NewTokenService(hosts HostStore, tokens TokenStore, writer AuditWriter, defaultAPIKeyTTL, maxAPIKeyTTL time.Duration, sharing ...HostPermissionAuthorizer) (*TokenService, error) {
	if hosts == nil || tokens == nil || writer == nil {
		return nil, errors.New("token service dependencies are required")
	}
	if len(sharing) > 1 {
		return nil, errors.New("at most one Host sharing authorizer is allowed")
	}
	if defaultAPIKeyTTL <= 0 || maxAPIKeyTTL < defaultAPIKeyTTL {
		return nil, errors.New("API key lifetimes are invalid")
	}
	service := &TokenService{hosts: hosts, tokens: tokens, audit: writer, apiKeyTTL: defaultAPIKeyTTL, apiKeyMaxTTL: maxAPIKeyTTL}
	if len(sharing) == 1 {
		service.sharing = sharing[0]
	}
	return service, nil
}

func (s *TokenService) ListAPIKeys(ctx context.Context, principal identity.Principal) ([]identity.Token, error) {
	if err := requireSession(principal); err != nil {
		return nil, err
	}
	values, err := s.tokens.ListTokensByOwner(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	result := make([]identity.Token, 0)
	for _, value := range values {
		if value.Kind == identity.KindAPIKey {
			result = append(result, value)
		}
	}
	return result, nil
}

func (s *TokenService) CreateAPIKey(ctx context.Context, principal identity.Principal, name string, hostIDs []string, scopes []identity.Scope, ttl time.Duration) (APIKeyInfo, error) {
	if err := requireSession(principal); err != nil {
		return APIKeyInfo{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return APIKeyInfo{}, domain.InvalidArgument("name", "API key name must contain 1 to 100 bytes")
	}
	hostIDs, err := normalizeHostIDs(hostIDs)
	if err != nil {
		return APIKeyInfo{}, err
	}
	scopes, err = normalizeScopes(scopes)
	if err != nil {
		return APIKeyInfo{}, err
	}
	if containsScope(scopes, identity.ScopeHostCreate) && len(hostIDs) >= 500 {
		return APIKeyInfo{}, domain.InvalidArgument("host_ids", "leave room for the Host created by this key")
	}
	for _, hostID := range hostIDs {
		value, err := s.hosts.GetHost(ctx, hostID)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				return APIKeyInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
			}
			return APIKeyInfo{}, err
		}
		if value.State == host.StateDeleted || value.DeletedAt != nil {
			return APIKeyInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
		}
		if value.OwnerID != principal.UserID {
			if s.sharing == nil {
				return APIKeyInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
			}
			if _, err := s.sharing.Authorize(ctx, hostID, principal.UserID, host.PermissionMCPControl); err != nil {
				if domain.CodeOf(err) == domain.CodeNotFound {
					return APIKeyInfo{}, domain.NewError(domain.CodeNotFound, "host not found")
				}
				return APIKeyInfo{}, err
			}
		}
	}
	if ttl == 0 {
		ttl = s.apiKeyTTL
	}
	if ttl <= 0 || ttl > s.apiKeyMaxTTL {
		return APIKeyInfo{}, domain.InvalidArgument("ttl_seconds", "API key ttl exceeds the allowed maximum")
	}
	expiresAt := time.Now().UTC().Add(ttl)
	value, credential, err := issueCredentialWithGrants(ctx, s.tokens, principal.UserID, identity.KindAPIKey, "", name, expiresAt, hostIDs, scopes)
	if err != nil {
		return APIKeyInfo{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionAPIKeyCreate,
		Target: value.ID,
		Detail: map[string]any{"host_ids": hostIDs, "scopes": scopes},
	}); err != nil {
		if revokeErr := s.tokens.RevokeToken(ctx, value.ID, principal.UserID); revokeErr != nil {
			return APIKeyInfo{}, fmt.Errorf("write API key audit: %w (revoke key: %w)", err, revokeErr)
		}
		return APIKeyInfo{}, fmt.Errorf("write API key audit: %w", err)
	}
	return APIKeyInfo{ID: value.ID, Key: credential, HostIDs: hostIDs, Scopes: scopes, ExpiresAt: expiresAt}, nil
}

func (s *TokenService) DeleteAPIKey(ctx context.Context, principal identity.Principal, tokenID string) error {
	if err := requireSession(principal); err != nil {
		return err
	}
	value, err := s.tokens.GetTokenByID(ctx, tokenID)
	if err != nil {
		return err
	}
	if value.Kind != identity.KindAPIKey || value.OwnerID != principal.UserID {
		return domain.NewError(domain.CodeNotFound, "API key not found")
	}
	if err := s.tokens.RevokeToken(ctx, value.ID, principal.UserID); err != nil {
		return err
	}
	return s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionAPIKeyDelete,
		Target: value.ID,
	})
}

func requireSession(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func normalizeHostIDs(values []string) ([]string, error) {
	if len(values) > 500 {
		return nil, domain.InvalidArgument("host_ids", "at most 500 Hosts may be selected")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !domain.ValidHostID(value) {
			return nil, domain.InvalidArgument("host_ids", "a Host id is invalid")
		}
		if _, exists := seen[value]; exists {
			return nil, domain.InvalidArgument("host_ids", "Host ids must be unique")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeScopes(values []identity.Scope) ([]identity.Scope, error) {
	seen := make(map[identity.Scope]struct{}, len(values))
	result := make([]identity.Scope, 0, len(values))
	for _, value := range values {
		if !identity.ValidScope(value) || value == identity.ScopeWorkspaceSync {
			return nil, domain.InvalidArgument("scopes", "a scope is invalid")
		}
		if _, exists := seen[value]; exists {
			return nil, domain.InvalidArgument("scopes", "scopes must be unique")
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}
