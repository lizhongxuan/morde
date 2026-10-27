package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type TokenService struct {
	tokens       TokenStore
	audit        AuditWriter
	apiKeyTTL    time.Duration
	apiKeyMaxTTL time.Duration
}

type APIKeyInfo struct {
	ID        string
	Key       string
	ExpiresAt time.Time
}

func NewTokenService(tokens TokenStore, writer AuditWriter, defaultAPIKeyTTL, maxAPIKeyTTL time.Duration) (*TokenService, error) {
	if tokens == nil || writer == nil {
		return nil, errors.New("token service dependencies are required")
	}
	if defaultAPIKeyTTL <= 0 || maxAPIKeyTTL < defaultAPIKeyTTL {
		return nil, errors.New("API key lifetimes are invalid")
	}
	return &TokenService{tokens: tokens, audit: writer, apiKeyTTL: defaultAPIKeyTTL, apiKeyMaxTTL: maxAPIKeyTTL}, nil
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

func (s *TokenService) CreateAPIKey(ctx context.Context, principal identity.Principal, name string, ttl time.Duration) (APIKeyInfo, error) {
	if err := requireSession(principal); err != nil {
		return APIKeyInfo{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return APIKeyInfo{}, domain.InvalidArgument("name", "API key name must contain 1 to 100 bytes")
	}
	if ttl == 0 {
		ttl = s.apiKeyTTL
	}
	if ttl <= 0 || ttl > s.apiKeyMaxTTL {
		return APIKeyInfo{}, domain.InvalidArgument("ttl_seconds", "API key ttl exceeds the allowed maximum")
	}
	expiresAt := time.Now().UTC().Add(ttl)
	value, credential, err := issueCredentialWithGrants(ctx, s.tokens, principal.UserID, identity.KindAPIKey, "", name, expiresAt, nil, nil)
	if err != nil {
		return APIKeyInfo{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionAPIKeyCreate,
		Target: value.ID,
		Detail: map[string]any{"authorization": "account_current"},
	}); err != nil {
		if revokeErr := s.tokens.RevokeToken(ctx, value.ID, principal.UserID); revokeErr != nil {
			return APIKeyInfo{}, fmt.Errorf("write API key audit: %w (revoke key: %w)", err, revokeErr)
		}
		return APIKeyInfo{}, fmt.Errorf("write API key audit: %w", err)
	}
	return APIKeyInfo{ID: value.ID, Key: credential, ExpiresAt: expiresAt}, nil
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
