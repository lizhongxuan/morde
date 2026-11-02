package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type ClientIssuedTokens struct {
	Client           identity.Token
	ClientCredential string
	Sync             *identity.Token
	SyncCredential   string
	Web              *identity.Token
	WebCredential    string
}

type ClientLoginResult struct {
	InstallationID       string
	ClientCredential     string
	ClientExpiresAt      time.Time
	SyncCredential       string
	SyncExpiresAt        time.Time
	WebSessionCredential string
	WebSessionExpiresAt  time.Time
	User                 UserSummary
}

type ClientWebSessionResult struct {
	Credential string
	ExpiresAt  time.Time
}

type ClientInfo struct {
	ID          string
	Platform    string
	DisplayName string
	ExpiresAt   time.Time
	Revoked     bool
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

const clientTokenTTL = 30 * 24 * time.Hour

var clientInstanceIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type ClientLoginAttempt struct {
	InstanceID    string
	Generation    int64
	LegacyAndroid bool
}

type HubRevocationCloser interface {
	CloseInstallation(string)
	CloseOwner(string)
}

type ClientLoginService struct {
	store                   ClientLoginStore
	credentials             *CredentialService
	workspaceSyncReconciler WorkspaceSyncNodeOwnerReconciler
	hub                     HubRevocationCloser
}

func NewClientLoginService(store ClientLoginStore, credentials *CredentialService, workspaceSyncReconciler WorkspaceSyncNodeOwnerReconciler, hubs ...HubRevocationCloser) (*ClientLoginService, error) {
	if store == nil || credentials == nil || len(hubs) > 1 {
		return nil, domain.InvalidArgument("dependencies", "client login dependencies are required")
	}
	var hub HubRevocationCloser
	if len(hubs) == 1 {
		hub = hubs[0]
	}
	return &ClientLoginService{store: store, credentials: credentials, workspaceSyncReconciler: workspaceSyncReconciler, hub: hub}, nil
}

func (s *ClientLoginService) Login(ctx context.Context, name, password, platform, displayName, renewalID, idempotencyKey, ip string, attempt ClientLoginAttempt) (ClientLoginResult, error) {
	displayName = strings.TrimSpace(displayName)
	if platform != "desktop" && platform != "android" && platform != "ios" {
		return ClientLoginResult{}, domain.InvalidArgument("platform", "unsupported client platform")
	}
	if displayName == "" || !utf8.ValidString(displayName) || len(displayName) > 100 {
		return ClientLoginResult{}, domain.InvalidArgument("display_name", "invalid client name")
	}
	if renewalID != "" && !domain.ValidClientInstallationID(renewalID) {
		return ClientLoginResult{}, domain.InvalidArgument("renewal_installation_id", "invalid installation id")
	}
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return ClientLoginResult{}, domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	if attempt.LegacyAndroid {
		if platform != "android" || attempt.InstanceID != "" || attempt.Generation != 0 {
			return ClientLoginResult{}, domain.InvalidArgument("client_login_attempt", "legacy login compatibility is Android-only")
		}
	} else if attempt.InstanceID == "" || attempt.Generation < 1 {
		return ClientLoginResult{}, domain.NewError(domain.CodeUnsupported, "client login requires the ordered_client_login protocol; update the client")
	} else if !clientInstanceIDPattern.MatchString(attempt.InstanceID) {
		return ClientLoginResult{}, domain.InvalidArgument("client_instance_id", "client instance id must be a UUID")
	}
	user, err := s.credentials.authenticatePassword(ctx, name, password, ip)
	if err != nil {
		return ClientLoginResult{}, err
	}
	if user.MustChangePassword {
		return ClientLoginResult{}, domain.NewError(domain.CodeForbidden, "password change is required before client login")
	}
	if err := s.credentials.ensureDefaultHost(ctx, user); err != nil {
		return ClientLoginResult{}, err
	}
	id, err := domain.NewClientInstallationID()
	if err != nil {
		return ClientLoginResult{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(clientTokenTTL)
	installation := identity.ClientInstallation{ID: id, OwnerID: user.ID, DisplayName: displayName, Platform: platform, ExpiresAt: expiresAt}
	keyHash := sha256.Sum256([]byte(idempotencyKey))
	requestMaterial := platform + "\x00" + displayName + "\x00" + renewalID
	if !attempt.LegacyAndroid {
		requestMaterial += "\x00" + strings.ToLower(attempt.InstanceID) + "\x00" + strconv.FormatInt(attempt.Generation, 10)
	}
	requestHash := sha256.Sum256([]byte(requestMaterial))
	value, issued, err := s.store.LoginClient(ctx, installation, strings.ToLower(attempt.InstanceID), attempt.Generation, attempt.LegacyAndroid, keyHash[:], requestHash[:], renewalID, ip, func(installationID string) (ClientIssuedTokens, error) {
		client, clientCredential, issueErr := newClientToken(user.ID, installationID, identity.KindClient, expiresAt)
		if issueErr != nil {
			return ClientIssuedTokens{}, issueErr
		}
		result := ClientIssuedTokens{Client: client, ClientCredential: clientCredential}
		if platform == "desktop" {
			syncToken, syncCredential, syncErr := newClientToken(user.ID, installationID, identity.KindDevice, expiresAt)
			if syncErr != nil {
				return ClientIssuedTokens{}, syncErr
			}
			webToken, webCredential, webErr := newClientWebToken(user.ID, installationID, s.webSessionExpiry(now, expiresAt))
			if webErr != nil {
				return ClientIssuedTokens{}, webErr
			}
			result.Sync, result.SyncCredential = &syncToken, syncCredential
			result.Web, result.WebCredential = &webToken, webCredential
		}
		return result, nil
	})
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredential) {
			return ClientLoginResult{}, domain.NewError(domain.CodeUnauthenticated, "invalid credentials")
		}
		return ClientLoginResult{}, err
	}
	if s.hub != nil {
		s.hub.CloseInstallation(value.ID)
	}
	result := ClientLoginResult{InstallationID: value.ID, ClientCredential: issued.ClientCredential, ClientExpiresAt: issued.Client.ExpiresAt,
		SyncCredential: issued.SyncCredential, WebSessionCredential: issued.WebCredential, User: userSummary(user, ResourceUsage{})}
	if issued.Sync != nil {
		result.SyncExpiresAt = issued.Sync.ExpiresAt
	}
	if issued.Web != nil {
		result.WebSessionExpiresAt = issued.Web.ExpiresAt
	}
	return result, nil
}

func (s *ClientLoginService) WebSession(ctx context.Context, principal identity.Principal) (ClientWebSessionResult, error) {
	if principal.Kind != identity.KindClient || !domain.ValidUserID(principal.UserID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return ClientWebSessionResult{}, domain.NewError(domain.CodeUnauthenticated, "client credential required")
	}
	installation, err := s.store.GetClientInstallation(ctx, principal.ClientInstallationID)
	if err != nil || installation.OwnerID != principal.UserID || (installation.Platform != "desktop" && installation.Platform != "ios") || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return ClientWebSessionResult{}, domain.NewError(domain.CodeUnauthenticated, "client installation is unavailable")
	}
	token, credential, err := newClientWebToken(principal.UserID, principal.ClientInstallationID, s.webSessionExpiry(time.Now().UTC(), installation.ExpiresAt))
	if err != nil {
		return ClientWebSessionResult{}, err
	}
	if err := s.store.IssueClientWebSession(ctx, principal, token); err != nil {
		return ClientWebSessionResult{}, err
	}
	return ClientWebSessionResult{Credential: credential, ExpiresAt: token.ExpiresAt}, nil
}

func (s *ClientLoginService) Logout(ctx context.Context, principal identity.Principal, ip string) error {
	if principal.Kind != identity.KindClient || !domain.ValidUserID(principal.UserID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "client credential required")
	}
	if err := s.store.RevokeClientInstallation(ctx, principal.ClientInstallationID, principal.UserID, ip, audit.ActorClient,
		func(gateCtx context.Context) error { return s.disableWorkspaceSyncRelay(gateCtx, principal.UserID) }); err != nil {
		return err
	}
	if s.hub != nil {
		s.hub.CloseInstallation(principal.ClientInstallationID)
	}
	return s.reconcileWorkspaceSyncAfterInstallationRevocation(ctx, principal.UserID)
}

func (s *ClientLoginService) LogoutWebSession(ctx context.Context, principal identity.Principal, ip string) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "client Web session required")
	}
	if err := s.store.RevokeClientInstallation(ctx, principal.ClientInstallationID, principal.UserID, ip, audit.ActorSession,
		func(gateCtx context.Context) error { return s.disableWorkspaceSyncRelay(gateCtx, principal.UserID) }); err != nil {
		return err
	}
	if s.hub != nil {
		s.hub.CloseInstallation(principal.ClientInstallationID)
	}
	return s.reconcileWorkspaceSyncAfterInstallationRevocation(ctx, principal.UserID)
}

func (s *ClientLoginService) List(ctx context.Context, principal identity.Principal) ([]ClientInfo, error) {
	if err := validateClientAccountPrincipal(principal); err != nil {
		return nil, err
	}
	values, err := s.store.ListClientInstallations(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	items := make([]ClientInfo, 0, len(values))
	for _, value := range values {
		items = append(items, ClientInfo{ID: value.ID, Platform: value.Platform, DisplayName: value.DisplayName,
			ExpiresAt: value.ExpiresAt, Revoked: value.RevokedAt != nil, CreatedAt: value.CreatedAt, LastUsedAt: value.LastUsedAt})
	}
	return items, nil
}

func (s *ClientLoginService) Revoke(ctx context.Context, principal identity.Principal, id, ip string) error {
	if err := validateClientAccountPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidClientInstallationID(id) {
		return domain.InvalidArgument("client_id", "invalid client id")
	}
	if err := s.store.RevokeClientInstallation(ctx, id, principal.UserID, ip, audit.ActorKind(principal.Kind),
		func(gateCtx context.Context) error { return s.disableWorkspaceSyncRelay(gateCtx, principal.UserID) }); err != nil {
		return err
	}
	if s.hub != nil {
		s.hub.CloseInstallation(id)
	}
	return s.reconcileWorkspaceSyncAfterInstallationRevocation(ctx, principal.UserID)
}

func (s *ClientLoginService) disableWorkspaceSyncRelay(ctx context.Context, ownerID string) error {
	if s.workspaceSyncReconciler != nil {
		return s.workspaceSyncReconciler.DisableWorkspaceSyncNodeRelay(ctx, ownerID)
	}
	return nil
}

func (s *ClientLoginService) reconcileWorkspaceSyncAfterInstallationRevocation(ctx context.Context, ownerID string) error {
	if s.workspaceSyncReconciler != nil {
		if err := s.workspaceSyncReconciler.ReconcileWorkspaceSyncNodeOwner(ctx, ownerID); err != nil {
			return domain.NewError(domain.CodeUpstreamError, "workspace sync node could not confirm installation revocation")
		}
	}
	return nil
}

func validateClientAccountPrincipal(principal identity.Principal) error {
	if principal.Kind == identity.KindSession {
		return requireSession(principal)
	}
	if principal.Kind != identity.KindClient || !domain.ValidTokenID(principal.TokenID) || len(principal.WorkspaceIDs) != 0 {
		return domain.NewError(domain.CodeForbidden, "user session or client credential required")
	}
	return validatePrincipal(principal)
}

func (s *ClientLoginService) webSessionExpiry(now, clientExpiry time.Time) time.Time {
	expires := now.Add(s.credentials.options.SessionTTL)
	if expires.After(now.Add(time.Hour)) {
		expires = now.Add(time.Hour)
	}
	if expires.After(clientExpiry) {
		expires = clientExpiry
	}
	return expires
}

func newClientWebToken(ownerID, installationID string, expiresAt time.Time) (identity.Token, string, error) {
	id, err := domain.NewTokenID()
	if err != nil {
		return identity.Token{}, "", err
	}
	credential, secretHash, err := identity.NewCredential(identity.KindSession, id)
	if err != nil {
		return identity.Token{}, "", err
	}
	return identity.Token{ID: id, OwnerID: ownerID, ClientInstallationID: installationID, Kind: identity.KindSession,
		Name: "client web session", SecretHash: secretHash, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}, credential, nil
}

func newClientToken(ownerID, installationID string, kind identity.Kind, expiresAt time.Time) (identity.Token, string, error) {
	id, err := domain.NewTokenID()
	if err != nil {
		return identity.Token{}, "", err
	}
	credential, hash, err := identity.NewCredential(kind, id)
	if err != nil {
		return identity.Token{}, "", err
	}
	token := identity.Token{ID: id, OwnerID: ownerID, ClientInstallationID: installationID, Kind: kind,
		SecretHash: hash, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	if kind == identity.KindDevice {
		token.Scopes = []identity.Scope{identity.ScopeWorkspaceSync}
	}
	return token, credential, nil
}
