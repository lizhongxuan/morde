package app

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type hostSharingService struct {
	hosts      HostStore
	users      UserStore
	sharing    HostSharingStore
	tokens     TokenStore
	audit      AuditWriter
	scheme     string
	hostSuffix string
	ticketTTL  time.Duration
	logger     *zap.Logger
	countAudit func()
}

func NewHostSharingService(hosts HostStore, users UserStore, sharing HostSharingStore, tokens TokenStore, writer AuditWriter,
	scheme, hostSuffix string, ticketTTL time.Duration, logger *zap.Logger, countAudit func()) (HostSharingService, error) {
	if hosts == nil || users == nil || sharing == nil || tokens == nil || writer == nil || logger == nil || countAudit == nil ||
		(scheme != "http" && scheme != "https") || hostSuffix == "" || ticketTTL <= 0 {
		return nil, fmt.Errorf("host sharing dependencies and settings are required")
	}
	return &hostSharingService{hosts: hosts, users: users, sharing: sharing, tokens: tokens, audit: writer,
		scheme: scheme, hostSuffix: hostSuffix, ticketTTL: ticketTTL, logger: logger, countAudit: countAudit}, nil
}

func (s *hostSharingService) Invite(ctx context.Context, principal identity.Principal, hostID, userID string, permissions []host.Permission) (host.Member, error) {
	hostValue, err := s.ownedHost(ctx, principal, hostID)
	if err != nil {
		return host.Member{}, err
	}
	if !domain.ValidUserID(userID) || userID == principal.UserID {
		return host.Member{}, domain.InvalidArgument("user_id", "member user id is invalid")
	}
	permissions, err = normalizeHostMemberPermissions(permissions)
	if err != nil {
		return host.Member{}, err
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil || user.Disabled {
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			return host.Member{}, err
		}
		return host.Member{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	value := host.Member{HostID: hostID, OwnerID: hostValue.OwnerID, UserID: userID, Permissions: permissions}
	value, err = s.sharing.UpsertHostMember(ctx, value)
	if err != nil {
		return host.Member{}, err
	}
	s.writeAudit(ctx, audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionHostMemberInvite,
		Target: hostID, Detail: map[string]any{"member_user_id": userID, "permissions": permissions}})
	return value, nil
}

func normalizeHostMemberPermissions(permissions []host.Permission) ([]host.Permission, error) {
	var desktopView, desktopControl, terminalControl, mcpControl bool
	for _, permission := range permissions {
		switch permission {
		case host.PermissionDesktopView:
			desktopView = true
		case host.PermissionDesktopControl:
			desktopControl = true
		case host.PermissionTerminalControl:
			terminalControl = true
		case host.PermissionMCPControl:
			mcpControl = true
		default:
			return nil, domain.InvalidArgument("permissions", "host member permission is unsupported")
		}
	}
	if desktopControl {
		desktopView = true
	}
	if !desktopView && !desktopControl && !terminalControl && !mcpControl {
		return nil, domain.InvalidArgument("permissions", "at least one host member permission is required")
	}
	result := make([]host.Permission, 0, 4)
	if desktopView {
		result = append(result, host.PermissionDesktopView)
	}
	if desktopControl {
		result = append(result, host.PermissionDesktopControl)
	}
	if terminalControl {
		result = append(result, host.PermissionTerminalControl)
	}
	if mcpControl {
		result = append(result, host.PermissionMCPControl)
	}
	return result, nil
}

func (s *hostSharingService) ListMembers(ctx context.Context, principal identity.Principal, hostID string) ([]host.Member, error) {
	if _, err := s.ownedHost(ctx, principal, hostID); err != nil {
		return nil, err
	}
	return s.sharing.ListHostMembers(ctx, hostID, principal.UserID)
}

func (s *hostSharingService) Revoke(ctx context.Context, principal identity.Principal, hostID, userID string) error {
	if _, err := s.ownedHost(ctx, principal, hostID); err != nil {
		return err
	}
	if !domain.ValidUserID(userID) {
		return domain.NewError(domain.CodeNotFound, "host member not found")
	}
	if err := s.sharing.RevokeHostMember(ctx, hostID, principal.UserID, userID); err != nil {
		return err
	}
	s.writeAudit(ctx, audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionHostMemberRevoke,
		Target: hostID, Detail: map[string]any{"member_user_id": userID}})
	return nil
}

func (s *hostSharingService) ListSharedHosts(ctx context.Context, principal identity.Principal) ([]host.SharedHost, error) {
	if err := requireHostSharingViewer(principal); err != nil {
		return nil, err
	}
	return s.sharing.ListSharedHosts(ctx, principal.UserID)
}

func (s *hostSharingService) IssueObservationTicket(ctx context.Context, principal identity.Principal, hostID string) (EnterInfo, error) {
	return s.issueHostTicket(ctx, principal, hostID, host.PermissionDesktopView, "desktop_observe", "")
}

func (s *hostSharingService) IssueTerminalTicket(ctx context.Context, principal identity.Principal, hostID string) (EnterInfo, error) {
	return s.issueHostTicket(ctx, principal, hostID, host.PermissionTerminalControl, "terminal_control", "/__ch/terminal/")
}

func (s *hostSharingService) issueHostTicket(ctx context.Context, principal identity.Principal, hostID string, permission host.Permission, mode, next string) (EnterInfo, error) {
	if err := requireHostSharingViewer(principal); err != nil {
		return EnterInfo{}, err
	}
	if principal.Kind == identity.KindClient && permission != host.PermissionDesktopView {
		return EnterInfo{}, domain.NewError(domain.CodeForbidden, "client ticket permission is unavailable")
	}
	owner, err := s.Authorize(ctx, hostID, principal.UserID, permission)
	if err != nil {
		return EnterInfo{}, err
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return EnterInfo{}, err
	}
	if value.State != host.StateRunning {
		return EnterInfo{}, invalidHostState(value.State)
	}
	expiresAt := time.Now().UTC().Add(s.ticketTTL)
	var credential string
	if principal.Kind == identity.KindClient {
		_, credential, err = issueClientTicket(ctx, s.tokens, value.OwnerID, principal.UserID, value.ID, principal.ClientInstallationID, expiresAt)
	} else if owner {
		_, credential, err = issueCredentialWithPreviewPort(ctx, s.tokens, value.OwnerID, identity.KindTicket, value.ID, "", expiresAt, nil, nil, 0)
	} else {
		_, credential, err = issueCredentialWithActor(ctx, s.tokens, value.OwnerID, principal.UserID, identity.KindTicket, value.ID, expiresAt)
	}
	if err != nil {
		return EnterInfo{}, err
	}
	query := url.Values{"t": []string{credential}}
	if next != "" {
		query.Set("next", next)
	}
	enterURL := url.URL{Scheme: s.scheme, Host: hostURLHost(value.ID, s.hostSuffix), Path: "/__ch/auth", RawQuery: query.Encode()}
	s.writeAudit(ctx, audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostEnter,
		Target: hostID, Detail: map[string]any{"mode": mode}})
	return EnterInfo{URL: enterURL.String(), ExpiresAt: expiresAt}, nil
}

// Authorize is the single dynamic Host membership check used by the Gateway.
// The boolean identifies the owner for read-only desktop protocol filtering.
func (s *hostSharingService) Authorize(ctx context.Context, hostID, actorID string, permission host.Permission) (bool, error) {
	if !domain.ValidHostID(hostID) || !domain.ValidUserID(actorID) || !permission.Valid() {
		return false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	access, err := s.sharing.GetHostActorAccess(ctx, hostID, actorID)
	if err != nil {
		return false, err
	}
	if access.Disabled {
		return false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if access.OwnerID == actorID {
		return true, nil
	}
	for _, granted := range access.Permissions {
		if granted == permission || permission == host.PermissionDesktopView && granted == host.PermissionDesktopControl {
			return false, nil
		}
	}
	return false, domain.NewError(domain.CodeNotFound, "host not found")
}

func (s *hostSharingService) ownedHost(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	if err := requireHostSharingSession(principal); err != nil {
		return host.Host{}, err
	}
	if !domain.ValidHostID(hostID) {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if value.OwnerID != principal.UserID || value.State == host.StateDeleted || value.DeletedAt != nil {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return value, nil
}

func requireHostSharingSession(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func requireHostSharingViewer(principal identity.Principal) error {
	if principal.Kind == identity.KindClient && domain.ValidUserID(principal.UserID) &&
		domain.ValidTokenID(principal.TokenID) && domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return nil
	}
	return requireHostSharingSession(principal)
}

func (s *hostSharingService) writeAudit(ctx context.Context, entry audit.Entry) {
	if err := s.audit.Write(ctx, entry); err != nil {
		s.logger.Error("audit write failed", zap.String("action", string(entry.Action)), zap.Error(err))
		s.countAudit()
	}
}

var _ HostSharingService = (*hostSharingService)(nil)
