package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type ClientAuthorizationService struct {
	store      ClientAuthorizationStore
	workspaces WorkspaceStore
	limiter    loginLimiter
}

type ClientAuthorizationStart struct {
	DeviceCode string
	UserCode   string
	ExpiresAt  time.Time
	Interval   int
}

type ClientAuthorizationRequest struct {
	Platform              string
	DisplayName           string
	WantsWorkspaceSync    bool
	RenewalInstallationID string
	ExpiresAt             time.Time
}

type ClientAuthorizationStatus struct {
	State            string
	ExpiresAt        time.Time
	InstallationID   string
	ClientCredential string
	ClientExpiresAt  time.Time
	SyncCredential   string
	SyncExpiresAt    time.Time
	WorkspaceIDs     []string
}

type ClientInfo struct {
	ID           string
	Platform     string
	DisplayName  string
	WorkspaceIDs []string
	ExpiresAt    time.Time
	Revoked      bool
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

func NewClientAuthorizationService(store ClientAuthorizationStore, workspaces WorkspaceStore) (*ClientAuthorizationService, error) {
	if store == nil || workspaces == nil {
		return nil, domain.InvalidArgument("dependencies", "client authorization dependencies are required")
	}
	return &ClientAuthorizationService{store: store, workspaces: workspaces, limiter: loginLimiter{attempts: make(map[string]loginAttempt)}}, nil
}

func (s *ClientAuthorizationService) Start(ctx context.Context, platform, name string, sync bool, renewalID, ip string) (ClientAuthorizationStart, error) {
	name = strings.TrimSpace(name)
	if platform != "desktop" && platform != "android" && platform != "ios" {
		return ClientAuthorizationStart{}, domain.InvalidArgument("platform", "unsupported client platform")
	}
	if name == "" || !utf8.ValidString(name) || len(name) > 100 {
		return ClientAuthorizationStart{}, domain.InvalidArgument("display_name", "invalid client name")
	}
	if sync && platform != "desktop" {
		return ClientAuthorizationStart{}, domain.InvalidArgument("wants_workspace_sync", "workspace sync requires desktop")
	}
	if renewalID != "" && !domain.ValidClientInstallationID(renewalID) {
		return ClientAuthorizationStart{}, domain.InvalidArgument("renewal_installation_id", "invalid installation id")
	}
	if retry, ok := s.limiter.allow("client_start\x00"+ip, time.Now(), 10, time.Minute); !ok {
		err := domain.NewError(domain.CodeRateLimited, "client authorization rate limit reached")
		err.RetryAfter = retry
		return ClientAuthorizationStart{}, err
	}
	for range 5 {
		id, err := domain.NewTokenID()
		if err != nil {
			return ClientAuthorizationStart{}, err
		}
		code, secretHash, err := identity.NewCredential(identity.KindClient, id)
		if err != nil {
			return ClientAuthorizationStart{}, err
		}
		userCode, codeHash, err := newClientUserCode()
		if err != nil {
			return ClientAuthorizationStart{}, err
		}
		expiresAt := time.Now().UTC().Add(clientPairingTTL)
		err = s.store.CreateClientAuthorization(ctx, identity.ClientAuthorization{ID: id, DisplayName: name, Platform: platform, WantsWorkspaceSync: sync,
			RenewalInstallationID: renewalID, SecretHash: secretHash, UserCodeHash: codeHash, State: "pending", ExpiresAt: expiresAt})
		if err == nil {
			return ClientAuthorizationStart{DeviceCode: code, UserCode: userCode, ExpiresAt: expiresAt, Interval: 3}, nil
		}
		if !isUniqueViolation(err) {
			return ClientAuthorizationStart{}, err
		}
	}
	return ClientAuthorizationStart{}, domain.NewError(domain.CodeInternal, "unable to create client authorization")
}

func (s *ClientAuthorizationService) Poll(ctx context.Context, rawCode, ip string) (ClientAuthorizationStatus, error) {
	kind, id, secret, err := identity.ParseCredential(rawCode)
	if err != nil || kind != identity.KindClient {
		return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
	}
	if retry, ok := s.limiter.allow("client_poll\x00"+id+"\x00"+ip, time.Now(), 25, time.Minute); !ok {
		err := domain.NewError(domain.CodeRateLimited, "client authorization polling rate limit reached")
		err.RetryAfter = retry
		return ClientAuthorizationStatus{}, err
	}
	pairing, err := s.store.GetClientAuthorization(ctx, id)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
		}
		return ClientAuthorizationStatus{}, err
	}
	if !identity.VerifySecret(secret, pairing.SecretHash) {
		return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
	}
	if pairing.State == "denied" {
		return ClientAuthorizationStatus{State: "denied", ExpiresAt: pairing.ExpiresAt}, nil
	}
	if pairing.State == "claimed" {
		return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
	}
	if !pairing.ExpiresAt.After(time.Now()) {
		return ClientAuthorizationStatus{State: "expired", ExpiresAt: pairing.ExpiresAt}, nil
	}
	if pairing.State == "pending" {
		return ClientAuthorizationStatus{State: "authorization_pending", ExpiresAt: pairing.ExpiresAt}, nil
	}
	if pairing.State != "approved" || !domain.ValidClientInstallationID(pairing.InstallationID) || !domain.ValidUserID(pairing.OwnerID) {
		return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
	}
	installation, err := s.store.GetClientInstallation(ctx, pairing.InstallationID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
		}
		return ClientAuthorizationStatus{}, err
	}
	if installation.OwnerID != pairing.OwnerID || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return ClientAuthorizationStatus{}, identity.ErrInvalidCredential
	}
	expiresAt := installation.ExpiresAt
	clientToken, clientCredential, err := newClientToken(pairing.OwnerID, pairing.InstallationID, identity.KindClient, nil, expiresAt)
	if err != nil {
		return ClientAuthorizationStatus{}, err
	}
	var syncToken *identity.Token
	var syncCredential string
	if pairing.WantsWorkspaceSync {
		issued, credential, issueErr := newClientToken(pairing.OwnerID, pairing.InstallationID, identity.KindDevice, pairing.WorkspaceIDs, expiresAt)
		if issueErr != nil {
			return ClientAuthorizationStatus{}, issueErr
		}
		syncToken, syncCredential = &issued, credential
	}
	installation, err = s.store.ClaimClientAuthorization(ctx, id, pairing.SecretHash, clientToken, syncToken)
	if err != nil {
		return ClientAuthorizationStatus{}, err
	}
	return ClientAuthorizationStatus{State: "approved", ExpiresAt: pairing.ExpiresAt, InstallationID: installation.ID,
		ClientCredential: clientCredential, ClientExpiresAt: expiresAt, SyncCredential: syncCredential, SyncExpiresAt: expiresAt,
		WorkspaceIDs: append([]string{}, pairing.WorkspaceIDs...)}, nil
}

func newClientToken(ownerID, installationID string, kind identity.Kind, workspaceIDs []string, expires time.Time) (identity.Token, string, error) {
	id, err := domain.NewTokenID()
	if err != nil {
		return identity.Token{}, "", err
	}
	credential, hash, err := identity.NewCredential(kind, id)
	if err != nil {
		return identity.Token{}, "", err
	}
	token := identity.Token{ID: id, OwnerID: ownerID, ClientInstallationID: installationID, Kind: kind, SecretHash: hash, ExpiresAt: expires, CreatedAt: time.Now().UTC()}
	if kind == identity.KindDevice {
		token.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
		token.WorkspaceIDs = append([]string{}, workspaceIDs...)
	}
	return token, credential, nil
}

func (s *ClientAuthorizationService) Inspect(ctx context.Context, principal identity.Principal, userCode, ip string) (ClientAuthorizationRequest, error) {
	if err := requireSession(principal); err != nil {
		return ClientAuthorizationRequest{}, err
	}
	pairing, err := s.pendingByUserCode(ctx, userCode, ip)
	if err != nil {
		return ClientAuthorizationRequest{}, err
	}
	return ClientAuthorizationRequest{Platform: pairing.Platform, DisplayName: pairing.DisplayName, WantsWorkspaceSync: pairing.WantsWorkspaceSync,
		RenewalInstallationID: pairing.RenewalInstallationID, ExpiresAt: pairing.ExpiresAt}, nil
}

func (s *ClientAuthorizationService) Approve(ctx context.Context, principal identity.Principal, userCode string, workspaceIDs []string, ip string) (ClientInfo, error) {
	if err := requireSession(principal); err != nil {
		return ClientInfo{}, err
	}
	pairing, err := s.pendingByUserCode(ctx, userCode, ip)
	if err != nil {
		return ClientInfo{}, err
	}
	if !pairing.WantsWorkspaceSync && len(workspaceIDs) > 0 {
		return ClientInfo{}, domain.InvalidArgument("workspace_ids", "workspace sync was not requested")
	}
	workspaceIDs, err = s.validateWorkspaces(ctx, principal.UserID, workspaceIDs)
	if err != nil {
		return ClientInfo{}, err
	}
	_, codeHash, err := normalizeClientUserCode(userCode)
	if err != nil {
		return ClientInfo{}, domain.InvalidArgument("user_code", "authorization code is invalid")
	}
	id, err := domain.NewClientInstallationID()
	if err != nil {
		return ClientInfo{}, err
	}
	installation, err := s.store.ApproveClientAuthorization(ctx, codeHash, principal.UserID, workspaceIDs, id, ip, time.Now().UTC().Add(clientTokenTTL))
	if err != nil {
		return ClientInfo{}, err
	}
	return clientInfo(installation), nil
}

func (s *ClientAuthorizationService) Deny(ctx context.Context, principal identity.Principal, userCode, ip string) error {
	if err := requireSession(principal); err != nil {
		return err
	}
	_, err := s.pendingByUserCode(ctx, userCode, ip)
	if err != nil {
		return err
	}
	_, codeHash, err := normalizeClientUserCode(userCode)
	if err != nil {
		return domain.InvalidArgument("user_code", "authorization code is invalid")
	}
	return s.store.DenyClientAuthorization(ctx, codeHash, principal.UserID, ip)
}

func (s *ClientAuthorizationService) List(ctx context.Context, principal identity.Principal) ([]ClientInfo, error) {
	if err := requireSession(principal); err != nil {
		return nil, err
	}
	values, err := s.store.ListClientInstallations(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	items := make([]ClientInfo, 0, len(values))
	for _, v := range values {
		items = append(items, clientInfo(v))
	}
	return items, nil
}

func (s *ClientAuthorizationService) Revoke(ctx context.Context, principal identity.Principal, id, ip string) error {
	if err := requireSession(principal); err != nil {
		return err
	}
	if !domain.ValidClientInstallationID(id) {
		return domain.InvalidArgument("client_id", "invalid client id")
	}
	return s.store.RevokeClientInstallation(ctx, id, principal.UserID, ip)
}

func (s *ClientAuthorizationService) pendingByUserCode(ctx context.Context, userCode, ip string) (identity.ClientAuthorization, error) {
	if retry, ok := s.limiter.allow("client_approve\x00"+ip, time.Now(), 10, time.Minute); !ok {
		err := domain.NewError(domain.CodeRateLimited, "client authorization rate limit reached")
		err.RetryAfter = retry
		return identity.ClientAuthorization{}, err
	}
	_, hash, err := normalizeClientUserCode(userCode)
	if err != nil {
		return identity.ClientAuthorization{}, domain.InvalidArgument("user_code", "authorization code is invalid")
	}
	pairing, err := s.store.GetClientAuthorizationByCode(ctx, hash)
	if err != nil {
		return identity.ClientAuthorization{}, err
	}
	if pairing.State != "pending" || !pairing.ExpiresAt.After(time.Now()) {
		return identity.ClientAuthorization{}, domain.NewError(domain.CodeConflict, "client authorization is no longer pending")
	}
	return pairing, nil
}

func (s *ClientAuthorizationService) validateWorkspaces(ctx context.Context, ownerID string, ids []string) ([]string, error) {
	if len(ids) > 500 {
		return nil, domain.InvalidArgument("workspace_ids", "too many workspaces")
	}
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if !domain.ValidWorkspaceID(id) {
			return nil, domain.InvalidArgument("workspace_ids", "invalid workspace id")
		}
		if _, exists := seen[id]; exists {
			return nil, domain.InvalidArgument("workspace_ids", "duplicate workspace id")
		}
		seen[id] = struct{}{}
		value, err := s.workspaces.GetWorkspace(ctx, id)
		if err != nil || value.OwnerID != ownerID || value.State != workspace.StateReady || value.DeletedAt != nil {
			return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		result = append(result, id)
	}
	return result, nil
}

func clientInfo(value identity.ClientInstallation) ClientInfo {
	return ClientInfo{ID: value.ID, Platform: value.Platform, DisplayName: value.DisplayName, WorkspaceIDs: append([]string{}, value.WorkspaceIDs...),
		ExpiresAt: value.ExpiresAt, Revoked: value.RevokedAt != nil, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt}
}

const (
	clientCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	clientCodeLength   = 8
	clientPairingTTL   = 10 * time.Minute
	clientTokenTTL     = 30 * 24 * time.Hour
)

func normalizeClientUserCode(value string) (string, []byte, error) {
	value = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(value)))
	if len(value) != clientCodeLength {
		return "", nil, identity.ErrInvalidCredential
	}
	for _, char := range value {
		if !strings.ContainsRune(clientCodeAlphabet, char) {
			return "", nil, identity.ErrInvalidCredential
		}
	}
	hash := sha256.Sum256([]byte(value))
	return value, hash[:], nil
}

func newClientUserCode() (string, []byte, error) {
	var value strings.Builder
	value.Grow(clientCodeLength)
	limit := big.NewInt(int64(len(clientCodeAlphabet)))
	for range clientCodeLength {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", nil, err
		}
		value.WriteByte(clientCodeAlphabet[index.Int64()])
	}
	code := value.String()
	hash := sha256.Sum256([]byte(code))
	return code, hash[:], nil
}
