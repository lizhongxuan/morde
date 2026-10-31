package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type CredentialOptions struct {
	SessionTTL  time.Duration
	TicketTTL   time.Duration
	LoginLimit  int
	LoginWindow time.Duration
}

type ResourceUsage struct {
	Hosts    int
	CPUMilli int
	MemMiB   int
}

type HostResources struct {
	CPUMilli int
	MemMiB   int
}

type UserSummary struct {
	ID                   string
	Name                 string
	Role                 identity.Role
	Status               identity.AccountStatus
	MustChangePassword   bool
	CreatedAt            time.Time
	Quota                ResourceUsage
	DefaultHostResources HostResources
	Usage                ResourceUsage
}

type LoginResult struct {
	Credential string
	ExpiresAt  time.Time
	User       UserSummary
}

type CredentialService struct {
	users                   UserStore
	hosts                   HostStore
	tokens                  TokenStore
	audit                   AuditWriter
	options                 CredentialOptions
	dummyHash               []byte
	limiter                 loginLimiter
	defaultHosts            DefaultHostEnsurer
	workspaceSyncController WorkspaceSyncNodeOwnerReconciler
}

type DefaultHostEnsurer interface {
	EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error)
}

type loginAttempt struct {
	started time.Time
	count   int
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func NewCredentialService(users UserStore, hosts HostStore, tokens TokenStore, writer AuditWriter, options CredentialOptions, defaultHosts DefaultHostEnsurer, controllers ...WorkspaceSyncNodeOwnerReconciler) (*CredentialService, error) {
	if users == nil || hosts == nil || tokens == nil || writer == nil || defaultHosts == nil || len(controllers) > 1 {
		return nil, fmt.Errorf("credential service dependencies are required")
	}
	if options.SessionTTL <= 0 || options.TicketTTL <= 0 || options.LoginLimit <= 0 || options.LoginWindow <= 0 {
		return nil, fmt.Errorf("credential lifetimes and login limits must be positive")
	}
	dummyHash, err := bcrypt.GenerateFromPassword([]byte("unusable-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("prepare password verifier: %w", err)
	}
	var workspaceSyncController WorkspaceSyncNodeOwnerReconciler
	if len(controllers) == 1 {
		workspaceSyncController = controllers[0]
	}
	return &CredentialService{
		users: users, hosts: hosts, tokens: tokens, audit: writer,
		options: options, dummyHash: dummyHash,
		defaultHosts: defaultHosts, workspaceSyncController: workspaceSyncController,
		limiter: loginLimiter{attempts: make(map[string]loginAttempt)},
	}, nil
}

func (s *CredentialService) Login(ctx context.Context, name, password, ip string) (LoginResult, error) {
	user, err := s.authenticatePassword(ctx, name, password, ip)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.ensureDefaultHost(ctx, user); err != nil {
		return LoginResult{}, err
	}
	expires := time.Now().Add(s.options.SessionTTL)
	token, credential, err := issueCredential(ctx, s.tokens, user.ID, identity.KindSession, "", "session", expires)
	if err != nil {
		return LoginResult{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{ActorID: token.ID, ActorKind: audit.ActorSession, Action: audit.ActionLogin, IP: ip}); err != nil {
		if revokeErr := s.tokens.RevokeToken(ctx, token.ID, user.ID); revokeErr != nil {
			return LoginResult{}, fmt.Errorf("write login audit: %w (revoke session: %w)", err, revokeErr)
		}
		return LoginResult{}, fmt.Errorf("write login audit: %w", err)
	}
	return LoginResult{Credential: credential, ExpiresAt: expires, User: userSummary(user, ResourceUsage{})}, nil
}

func (s *CredentialService) ensureDefaultHost(ctx context.Context, user identity.User) error {
	_, _, err := s.defaultHosts.EnsureDefaultStopped(ctx, user)
	return err
}

func (s *CredentialService) authenticatePassword(ctx context.Context, name, password, ip string) (identity.User, error) {
	name = strings.TrimSpace(name)
	if name == "" || password == "" {
		return identity.User{}, domain.InvalidArgument("name", "name and password are required")
	}
	if retry, ok := s.limiter.allow(ip+"\x00"+strings.ToLower(name), time.Now(), s.options.LoginLimit, s.options.LoginWindow); !ok {
		err := domain.NewError(domain.CodeRateLimited, "login rate limit reached")
		err.RetryAfter = retry
		return identity.User{}, err
	}
	user, err := s.users.GetUserByName(ctx, name)
	validUser := err == nil
	hash := s.dummyHash
	if validUser {
		hash = []byte(user.PasswordHash)
	}
	passwordErr := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if !validUser || passwordErr != nil {
		actorID, actorKind := "", audit.ActorSystem
		if validUser {
			actorID, actorKind = user.ID, audit.ActorSession
		}
		if auditErr := s.audit.Write(ctx, audit.Entry{ActorID: actorID, ActorKind: actorKind, Action: audit.ActionLoginFailed, IP: ip}); auditErr != nil {
			return identity.User{}, fmt.Errorf("write failed login audit: %w", auditErr)
		}
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			return identity.User{}, err
		}
		return identity.User{}, domain.NewError(domain.CodeUnauthenticated, "invalid credentials")
	}
	if user.Status != identity.StatusApproved {
		if auditErr := s.audit.Write(ctx, audit.Entry{ActorID: user.ID, ActorKind: audit.ActorSession, Action: audit.ActionLoginFailed, IP: ip}); auditErr != nil {
			return identity.User{}, fmt.Errorf("write blocked login audit: %w", auditErr)
		}
		return identity.User{}, accountStatusError(user.Status)
	}
	if user.Role == identity.RoleSuperAdmin && user.MustChangePassword && password == "admin" && !isLoopbackIP(ip) {
		if auditErr := s.audit.Write(ctx, audit.Entry{ActorID: user.ID, ActorKind: audit.ActorSession, Action: audit.ActionLoginFailed, IP: ip}); auditErr != nil {
			return identity.User{}, fmt.Errorf("write blocked login audit: %w", auditErr)
		}
		return identity.User{}, domain.NewError(domain.CodeUnauthenticated, "invalid credentials")
	}
	return user, nil
}

func (s *CredentialService) Logout(ctx context.Context, principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "session required")
	}
	if err := s.tokens.RevokeToken(ctx, principal.TokenID, principal.UserID); err != nil {
		return err
	}
	return s.audit.Write(ctx, audit.Entry{ActorID: principal.TokenID, ActorKind: audit.ActorSession, Action: audit.ActionLogout})
}

func (s *CredentialService) ChangePassword(ctx context.Context, principal identity.Principal, currentPassword, newPassword string) error {
	if principal.Kind != identity.KindSession || principal.Role != identity.RoleSuperAdmin ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeForbidden, "only the super administrator can change this password")
	}
	user, err := s.users.GetUserByID(ctx, principal.UserID)
	if err != nil || user.Name != "admin" || user.Role != identity.RoleSuperAdmin || user.Status != identity.StatusApproved {
		return domain.NewError(domain.CodeForbidden, "only the super administrator can change this password")
	}
	if currentPassword == "" || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)) != nil {
		return domain.NewError(domain.CodeUnauthenticated, "current password is incorrect")
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = string(hash)
	user.MustChangePassword = false
	var disableRelay func(context.Context) error
	if s.workspaceSyncController != nil {
		disableRelay = func(gateCtx context.Context) error {
			return s.workspaceSyncController.DisableWorkspaceSyncNodeRelay(gateCtx, user.ID)
		}
	}
	_, err = s.users.UpdateUserWithAudit(ctx, user.ID, user.Status, user, audit.Entry{
		ActorID: user.ID, ActorKind: audit.ActorSession, Action: audit.ActionUserUpdate,
		Target: user.ID, Detail: map[string]any{"password_changed": true},
	}, disableRelay)
	if err != nil {
		return err
	}
	if s.workspaceSyncController != nil {
		if err := s.workspaceSyncController.ReconcileWorkspaceSyncNodeOwner(ctx, user.ID); err != nil {
			return domain.NewError(domain.CodeUpstreamError, "workspace sync node could not confirm account credential revocation")
		}
	}
	return nil
}

func (s *CredentialService) CurrentUser(ctx context.Context, principal identity.Principal) (UserSummary, error) {
	if err := validateUserPrincipal(principal); err != nil {
		return UserSummary{}, err
	}
	user, err := s.users.GetUserByID(ctx, principal.UserID)
	if err != nil || user.Status != identity.StatusApproved {
		return UserSummary{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	hosts, err := s.hosts.ListHostsByOwner(ctx, user.ID)
	if err != nil {
		return UserSummary{}, err
	}
	usage := allocatedUsage(hosts)
	return userSummary(user, usage), nil
}

func (s *CredentialService) VerifySession(ctx context.Context, raw string) (identity.Principal, error) {
	return s.verify(ctx, raw, identity.KindSession)
}

func (s *CredentialService) VerifyAPIKey(ctx context.Context, raw string) (identity.Principal, error) {
	return s.verify(ctx, raw, identity.KindAPIKey)
}

func (s *CredentialService) VerifyDevice(ctx context.Context, raw string) (identity.Principal, error) {
	return s.verify(ctx, raw, identity.KindDevice)
}

func (s *CredentialService) VerifyClient(ctx context.Context, raw string) (identity.Principal, error) {
	return s.verify(ctx, raw, identity.KindClient)
}

type TaskAuthorizer interface {
	AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error
}

func (s *CredentialService) AuthorizeTaskAction(ctx context.Context, ownerID string, kind identity.Kind, tokenID, hostID string, approvedScopes []identity.Scope, required identity.Scope) error {
	return s.authorizeHostAction(ctx, ownerID, ownerID, kind, tokenID, hostID, approvedScopes, required)
}

func (s *CredentialService) AuthorizeTaskExecution(ctx context.Context, principal identity.Principal, hostID string, required identity.Scope) (task.Authorization, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return task.Authorization{}, err
	}
	if !principal.HasScope(required) || (principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) {
		return task.Authorization{}, domain.NewError(domain.CodeForbidden, "task execution is not authorized")
	}
	if hostID != "" {
		if _, err := authorizeTaskHost(ctx, s, principal, hostID, required); err != nil {
			return task.Authorization{}, err
		}
	}
	user, err := s.users.GetUserByID(ctx, principal.UserID)
	if err != nil || user.Status != identity.StatusApproved {
		return task.Authorization{}, domain.NewError(domain.CodeUnauthenticated, "task owner is unavailable")
	}
	token, err := s.tokens.GetTokenByID(ctx, principal.TokenID)
	if err != nil || token.OwnerID != principal.UserID || token.Kind != principal.Kind || token.RevokedAt != nil ||
		!token.ExpiresAt.After(time.Now()) || token.ClientInstallationID != principal.ClientInstallationID {
		return task.Authorization{}, domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
	if token.ClientInstallationID != "" && !s.clientInstallationActive(ctx, token.ClientInstallationID, principal.UserID) {
		return task.Authorization{}, domain.NewError(domain.CodeUnauthenticated, "task installation is unavailable")
	}
	authorization := authorizationSnapshot(principal)
	authorization.Version = 1
	expiresAt := token.ExpiresAt.UTC()
	authorization.ExpiresAt = &expiresAt
	return authorization, nil
}

func (s *CredentialService) AuthorizeMCPRuntimeAction(ctx context.Context, hostOwnerID, actorID, tokenID, hostID string, approvedScopes []identity.Scope, required identity.Scope) error {
	return s.authorizeHostAction(ctx, hostOwnerID, actorID, identity.KindAPIKey, tokenID, hostID, approvedScopes, required)
}

func (s *CredentialService) authorizeHostAction(ctx context.Context, hostOwnerID, actorID string, kind identity.Kind, tokenID, hostID string, approvedScopes []identity.Scope, required identity.Scope) error {
	if !domain.ValidUserID(hostOwnerID) || !domain.ValidUserID(actorID) || !domain.ValidHostID(hostID) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	user, err := s.users.GetUserByID(ctx, actorID)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeNotFound {
			return fmt.Errorf("load task owner: %w", err)
		}
		return domain.NewError(domain.CodeUnauthenticated, "task owner is unavailable")
	}
	if user.Status != identity.StatusApproved {
		return domain.NewError(domain.CodeUnauthenticated, "task owner is unavailable")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeNotFound {
			return fmt.Errorf("load task Host: %w", err)
		}
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.OwnerID != hostOwnerID || value.State == host.StateDeleted || value.DeletedAt != nil {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	switch kind {
	case identity.KindSession:
		if actorID != hostOwnerID {
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		return nil
	case identity.KindClient:
		if actorID != hostOwnerID || !domain.ValidTokenID(tokenID) || !clientTaskScope(required) {
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		token, err := s.tokens.GetTokenByID(ctx, tokenID)
		if err != nil || token.Kind != identity.KindClient || token.OwnerID != actorID ||
			token.ClientInstallationID == "" || token.RevokedAt != nil || !token.ExpiresAt.After(time.Now()) ||
			!s.clientInstallationActive(ctx, token.ClientInstallationID, actorID) {
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		return nil
	case identity.KindAPIKey:
		if !domain.ValidTokenID(tokenID) || (required != identity.ScopeComputerObserve && required != identity.ScopeComputerOperate) {
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		token, err := s.tokens.GetTokenByID(ctx, tokenID)
		if err != nil {
			if domain.CodeOf(err) != domain.CodeNotFound {
				return fmt.Errorf("load task authorization: %w", err)
			}
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		if token.Kind != identity.KindAPIKey || token.OwnerID != actorID || token.RevokedAt != nil || !token.ExpiresAt.After(time.Now()) || len(token.HostIDs) != 0 || len(token.Scopes) != 0 {
			return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
		}
		return nil
	default:
		return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
}

func clientTaskScope(required identity.Scope) bool {
	switch required {
	case identity.ScopeTaskRead, identity.ScopeTaskSubmit, identity.ScopeTaskControl, identity.ScopeScheduleRead,
		identity.ScopeComputerObserve, identity.ScopeComputerOperate:
		return true
	default:
		return false
	}
}

func (s *CredentialService) ConsumeTicket(ctx context.Context, raw string) (Ticket, error) {
	kind, tokenID, secret, err := identity.ParseCredential(raw)
	if err != nil || kind != identity.KindTicket {
		return Ticket{}, identity.ErrInvalidCredential
	}
	token, err := s.tokens.GetTokenByID(ctx, tokenID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return Ticket{}, identity.ErrInvalidCredential
		}
		return Ticket{}, err
	}
	if !validToken(token, kind, secret, time.Now()) || token.HostID == "" ||
		(token.PreviewPort != 0 && !domain.ValidPreviewTargetPort(token.PreviewPort)) {
		return Ticket{}, identity.ErrInvalidCredential
	}
	ticket, err := s.tokens.ConsumeTicket(ctx, tokenID)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidCredential) {
			return Ticket{}, identity.ErrInvalidCredential
		}
		return Ticket{}, err
	}
	actorUserID := token.ActorUserID
	if actorUserID == "" {
		actorUserID = token.OwnerID
	}
	if ticket.TokenID != token.ID || ticket.UserID != token.OwnerID || ticket.ActorUserID != actorUserID || ticket.HostID != token.HostID || ticket.PreviewPort != token.PreviewPort || ticket.ClientInstallationID != token.ClientInstallationID {
		return Ticket{}, identity.ErrInvalidCredential
	}
	if token.ClientInstallationID != "" && !s.clientInstallationActive(ctx, token.ClientInstallationID, actorUserID) {
		return Ticket{}, identity.ErrInvalidCredential
	}
	return ticket, nil
}

func (s *CredentialService) verify(ctx context.Context, raw string, want identity.Kind) (identity.Principal, error) {
	kind, tokenID, secret, err := identity.ParseCredential(raw)
	if err != nil || kind != want {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	token, err := s.tokens.GetTokenByID(ctx, tokenID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return identity.Principal{}, identity.ErrInvalidCredential
		}
		return identity.Principal{}, err
	}
	if !validToken(token, want, secret, time.Now()) {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	if want == identity.KindAPIKey && (len(token.HostIDs) != 0 || len(token.Scopes) != 0 || len(token.WorkspaceIDs) != 0) {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	if want == identity.KindDevice && (token.ClientInstallationID == "" || !validDeviceToken(token)) {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	if want == identity.KindClient && (token.ClientInstallationID == "" || len(token.Scopes) != 0 || len(token.HostIDs) != 0 || len(token.WorkspaceIDs) != 0) {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	if (want == identity.KindClient || want == identity.KindDevice || want == identity.KindSession && token.ClientInstallationID != "") &&
		!s.clientInstallationActive(ctx, token.ClientInstallationID, token.OwnerID) {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	user, err := s.users.GetUserByID(ctx, token.OwnerID)
	if err != nil || user.Status != identity.StatusApproved {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	role := user.Role
	if want != identity.KindSession || token.ClientInstallationID != "" {
		role = identity.RoleUser
	}
	principal := identity.Principal{
		UserID: user.ID, Role: role, MustChangePassword: user.MustChangePassword && want == identity.KindSession,
		Kind: want, TokenID: token.ID, ClientInstallationID: token.ClientInstallationID,
		HostIDs: append([]string(nil), token.HostIDs...), WorkspaceIDs: append([]string(nil), token.WorkspaceIDs...),
		Scopes: append([]identity.Scope(nil), token.Scopes...),
	}
	return principal, nil
}

func (s *CredentialService) clientInstallationActive(ctx context.Context, id, ownerID string) bool {
	checker, ok := s.tokens.(interface {
		ActiveClientInstallation(context.Context, string, string) (bool, error)
	})
	if !ok {
		return false
	}
	active, err := checker.ActiveClientInstallation(ctx, id, ownerID)
	return err == nil && active
}

func (s *CredentialService) ClientInstallationActive(ctx context.Context, id, ownerID string) (bool, error) {
	checker, ok := s.tokens.(interface {
		ActiveClientInstallation(context.Context, string, string) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return checker.ActiveClientInstallation(ctx, id, ownerID)
}

func validDeviceToken(token identity.Token) bool {
	return token.HostID == "" && len(token.HostIDs) == 0 && len(token.Scopes) == 1 &&
		token.Scopes[0] == identity.ScopeWorkspaceSync && len(token.WorkspaceIDs) == 0
}

func userSummary(user identity.User, usage ResourceUsage) UserSummary {
	return UserSummary{
		ID: user.ID, Name: user.Name, Role: user.Role, Status: user.Status,
		MustChangePassword: user.MustChangePassword, CreatedAt: user.CreatedAt,
		Quota:                ResourceUsage{Hosts: user.QuotaHosts, CPUMilli: user.QuotaCPUMilli, MemMiB: user.QuotaMemMiB},
		DefaultHostResources: HostResources{CPUMilli: user.DefaultHostCPUMilli, MemMiB: user.DefaultHostMemMiB},
		Usage:                usage,
	}
}

func accountStatusError(status identity.AccountStatus) error {
	switch status {
	case identity.StatusPending:
		return domain.NewError(domain.CodeAccountPending, "account is awaiting administrator review")
	case identity.StatusRejected:
		return domain.NewError(domain.CodeAccountRejected, "account review was not approved")
	case identity.StatusDisabled:
		return domain.NewError(domain.CodeAccountDisabled, "account is disabled")
	default:
		return domain.NewError(domain.CodeUnauthenticated, "invalid credentials")
	}
}

func isLoopbackIP(value string) bool {
	address := net.ParseIP(value)
	return address != nil && address.IsLoopback()
}

func validateUserPrincipal(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindAPIKey && principal.Kind != identity.KindClient) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}

func validToken(token identity.Token, kind identity.Kind, secret string, now time.Time) bool {
	return token.Kind == kind && token.RevokedAt == nil && token.UsedAt == nil && token.ExpiresAt.After(now) &&
		identity.VerifySecret(secret, token.SecretHash)
}

func (l *loginLimiter) allow(key string, now time.Time, limit int, window time.Duration) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for candidate, attempt := range l.attempts {
		if !now.Before(attempt.started.Add(window)) {
			delete(l.attempts, candidate)
		}
	}
	attempt := l.attempts[key]
	if attempt.count == 0 || !now.Before(attempt.started.Add(window)) {
		attempt = loginAttempt{started: now}
	}
	if attempt.count >= limit {
		return attempt.started.Add(window).Sub(now), false
	}
	attempt.count++
	l.attempts[key] = attempt
	return 0, true
}

func issueCredential(ctx context.Context, store TokenStore, ownerID string, kind identity.Kind, hostID, name string, expiresAt time.Time) (identity.Token, string, error) {
	return issueCredentialWithGrants(ctx, store, ownerID, kind, hostID, name, expiresAt, nil, nil)
}

func issueClientTicket(ctx context.Context, store TokenStore, ownerID, actorID, hostID, installationID string, expiresAt time.Time) (identity.Token, string, error) {
	return issueInstallationTicket(ctx, store, ownerID, actorID, hostID, installationID, expiresAt, 0)
}

func issueInstallationTicket(ctx context.Context, store TokenStore, ownerID, actorID, hostID, installationID string, expiresAt time.Time, previewPort int) (identity.Token, string, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidUserID(actorID) || !domain.ValidHostID(hostID) ||
		!domain.ValidClientInstallationID(installationID) || !expiresAt.After(time.Now()) ||
		(previewPort != 0 && !domain.ValidPreviewTargetPort(previewPort)) {
		return identity.Token{}, "", domain.InvalidArgument("ticket", "client ticket fields are invalid")
	}
	type issued struct {
		token      identity.Token
		credential string
	}
	result, err := domain.CreateWithIDRetry(domain.NewTokenID, func(id string) (issued, error) {
		credential, hash, err := identity.NewCredential(identity.KindTicket, id)
		if err != nil {
			return issued{}, err
		}
		actor := ""
		if actorID != ownerID {
			actor = actorID
		}
		token, err := store.CreateToken(ctx, identity.Token{ID: id, OwnerID: ownerID, ActorUserID: actor, Kind: identity.KindTicket,
			HostID: hostID, PreviewPort: previewPort, ClientInstallationID: installationID, SecretHash: hash, ExpiresAt: expiresAt})
		if err != nil {
			return issued{}, err
		}
		return issued{token: token, credential: credential}, nil
	}, isUniqueViolation)
	return result.token, result.credential, err
}

func issueCredentialWithGrants(ctx context.Context, store TokenStore, ownerID string, kind identity.Kind, hostID, name string, expiresAt time.Time, hostIDs []string, scopes []identity.Scope) (identity.Token, string, error) {
	return issueCredentialWithPreviewPort(ctx, store, ownerID, kind, hostID, name, expiresAt, hostIDs, scopes, 0)
}

func issueCredentialWithPreviewPort(ctx context.Context, store TokenStore, ownerID string, kind identity.Kind, hostID, name string, expiresAt time.Time, hostIDs []string, scopes []identity.Scope, previewPort int) (identity.Token, string, error) {
	return issueCredentialWithActorAndPreviewPort(ctx, store, ownerID, "", kind, hostID, name, expiresAt, hostIDs, scopes, previewPort)
}

func issueCredentialWithActor(ctx context.Context, store TokenStore, ownerID, actorUserID string, kind identity.Kind, hostID string, expiresAt time.Time) (identity.Token, string, error) {
	if kind != identity.KindTicket || !domain.ValidUserID(actorUserID) {
		return identity.Token{}, "", domain.InvalidArgument("actor_user_id", "ticket actor is invalid")
	}
	return issueCredentialWithActorAndPreviewPort(ctx, store, ownerID, actorUserID, kind, hostID, "", expiresAt, nil, nil, 0)
}

func issueCredentialWithActorAndPreviewPort(ctx context.Context, store TokenStore, ownerID, actorUserID string, kind identity.Kind, hostID, name string, expiresAt time.Time, hostIDs []string, scopes []identity.Scope, previewPort int) (identity.Token, string, error) {
	if store == nil || !domain.ValidUserID(ownerID) || !expiresAt.After(time.Now()) {
		return identity.Token{}, "", domain.InvalidArgument("credential", "credential fields are invalid")
	}
	if kind == identity.KindTicket && !domain.ValidHostID(hostID) {
		return identity.Token{}, "", domain.InvalidArgument("host_id", "host id is invalid")
	}
	if (kind != identity.KindTicket && previewPort != 0) || (previewPort != 0 && !domain.ValidPreviewTargetPort(previewPort)) {
		return identity.Token{}, "", domain.InvalidArgument("preview_port", "preview port is invalid")
	}
	type issued struct {
		token      identity.Token
		credential string
	}
	result, err := domain.CreateWithIDRetry(
		domain.NewTokenID,
		func(id string) (issued, error) {
			credential, secretHash, err := identity.NewCredential(kind, id)
			if err != nil {
				return issued{}, err
			}
			token, err := store.CreateToken(ctx, identity.Token{
				ID: id, OwnerID: ownerID, ActorUserID: actorUserID, Kind: kind, HostID: hostID, PreviewPort: previewPort, Name: name,
				HostIDs: append([]string(nil), hostIDs...), Scopes: append([]identity.Scope(nil), scopes...),
				SecretHash: secretHash, ExpiresAt: expiresAt,
			})
			if err != nil {
				return issued{}, err
			}
			return issued{token: token, credential: credential}, nil
		},
		isUniqueViolation,
	)
	if err != nil {
		return identity.Token{}, "", fmt.Errorf("issue credential: %w", err)
	}
	return result.token, result.credential, nil
}

func isUniqueViolation(err error) bool {
	var sqlState interface{ SQLState() string }
	return errors.As(err, &sqlState) && sqlState.SQLState() == "23505"
}
