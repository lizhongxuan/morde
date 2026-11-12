package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

const (
	HostEmbedTargetBrowser   = "browser"
	HostEmbedTargetTerminal  = "terminal"
	HostEmbedTargetWorkspace = "workspace"
	HostEmbedTargetFiles     = "files"
	HostEmbedTargetVSCode    = "vscode"
	hostEmbedTicketTTL       = time.Minute
	hostEmbedTicketBytes     = 32
)

// HostEmbedTicketCommand contains the server-verified context for one embedded
// Workbench entry. The Portal Origin, service instance, user, token, and
// installation identity must come from trusted server context, never the body.
type HostEmbedTicketCommand struct {
	HostID            string
	ConversationID    string
	Target            string
	Targets           []string
	ServiceInstanceID string
	PortalOrigin      string
}

type HostEmbedTicket struct {
	ExchangeURL string    `json:"exchange_url"`
	Ticket      string    `json:"ticket"`
	EmbedNonce  string    `json:"embed_nonce"`
	Targets     []string  `json:"targets"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type HostEmbedTargetGrant struct {
	Target            string          `json:"target"`
	PermissionCeiling host.Permission `json:"permission_ceiling"`
}

// HostEmbedTicketRecord is the hash-only persisted binding for a one-time
// ticket. Ticket is intentionally absent; only its SHA-256 hash is stored.
type HostEmbedTicketRecord struct {
	TicketHash           []byte
	HostID               string
	ConversationID       string
	ActorID              string
	PortalTokenID        string
	ClientInstallationID string
	ServiceInstanceID    string
	PortalOrigin         string
	InitialTarget        string
	TargetGrants         []HostEmbedTargetGrant
	EmbedNonce           string
	RuntimeGeneration    int64
	ExpiresAt            time.Time
	UsedAt               *time.Time
}

type hostEmbedTicketStore interface {
	CreateHostEmbedTicket(context.Context, HostEmbedTicketRecord) error
	ConsumeHostEmbedTicket(context.Context, []byte, string, string, string, string) (HostEmbedTicketRecord, error)
}

// HostEmbedTicketExchanger is the narrow application capability Gateway will
// use in P08. Consuming a ticket checks its host, service instance, exact
// Origin, live Portal/client credentials, live Host generation, and current
// target permission in one database statement. P08 must still establish its
// separate embedded session and apply per-request authorization.
type HostEmbedTicketExchanger interface {
	ConsumeEmbedTicket(context.Context, string, string, string, string, string) (HostEmbedTicketRecord, error)
}

func (s *hostSharingService) IssueEmbedTicket(ctx context.Context, principal identity.Principal, command HostEmbedTicketCommand) (HostEmbedTicket, error) {
	if s == nil || !validEmbedPrincipal(principal) || !domain.ValidHostID(command.HostID) ||
		!domain.ValidConversationID(command.ConversationID) || !domain.ValidServiceInstanceID(command.ServiceInstanceID) ||
		!validHostEmbedTarget(command.Target) || !validEmbedOrigin(command.PortalOrigin, s.scheme) {
		return HostEmbedTicket{}, domain.NewError(domain.CodeForbidden, "authorized Portal session required")
	}
	targets, err := normalizeHostEmbedTargets(command.Targets, command.Target)
	if err != nil {
		return HostEmbedTicket{}, err
	}
	if principal.Kind == identity.KindClient && (command.Target != HostEmbedTargetBrowser || len(targets) != 1) {
		return HostEmbedTicket{}, domain.NewError(domain.CodeForbidden, "native clients may only request browser observation")
	}
	store, ok := s.sharing.(hostEmbedTicketStore)
	if !ok {
		return HostEmbedTicket{}, domain.NewError(domain.CodeUnsupported, "embedded Host access is unavailable")
	}

	access, err := s.sharing.GetHostActorAccess(ctx, command.HostID, principal.UserID)
	if err != nil {
		return HostEmbedTicket{}, err
	}
	if access.Disabled || access.OwnerID == "" {
		return HostEmbedTicket{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	owner := access.OwnerID == principal.UserID
	grants := make([]HostEmbedTargetGrant, 0, len(targets))
	authorizedTargets := make([]string, 0, len(targets))
	for _, target := range targets {
		grant, allowed := embedTargetGrant(target, owner, access.Permissions)
		if !allowed || principal.Kind == identity.KindClient && target != HostEmbedTargetBrowser {
			continue
		}
		if principal.Kind == identity.KindClient {
			grant.PermissionCeiling = host.PermissionDesktopView
		}
		grants = append(grants, grant)
		authorizedTargets = append(authorizedTargets, target)
	}
	if !containsHostEmbedTarget(authorizedTargets, command.Target) {
		return HostEmbedTicket{}, domain.NewError(domain.CodeNotFound, "host not found")
	}

	hostValue, err := s.hosts.GetHost(ctx, command.HostID)
	if err != nil {
		return HostEmbedTicket{}, err
	}
	if hostValue.OwnerID != access.OwnerID || hostValue.State == host.StateDeleted || hostValue.DeletedAt != nil {
		return HostEmbedTicket{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if hostValue.State != host.StateRunning {
		return HostEmbedTicket{}, invalidHostState(hostValue.State)
	}
	if hostValue.RuntimeGeneration < 1 {
		return HostEmbedTicket{}, domain.NewError(domain.CodeConflict, "host runtime is not ready")
	}

	rawTicketBytes := make([]byte, hostEmbedTicketBytes)
	if _, err := rand.Read(rawTicketBytes); err != nil {
		return HostEmbedTicket{}, fmt.Errorf("generate Host embed ticket: %w", err)
	}
	rawTicket := base64.RawURLEncoding.EncodeToString(rawTicketBytes)
	ticketHash := sha256.Sum256([]byte(rawTicket))
	var nonceBytes [24]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return HostEmbedTicket{}, fmt.Errorf("generate Host embed nonce: %w", err)
	}
	embedNonce := base64.RawURLEncoding.EncodeToString(nonceBytes[:])
	ttl := s.ticketTTL
	if ttl > hostEmbedTicketTTL {
		ttl = hostEmbedTicketTTL
	}
	expiresAt := time.Now().UTC().Add(ttl)
	record := HostEmbedTicketRecord{
		TicketHash: ticketHash[:], HostID: command.HostID, ConversationID: command.ConversationID,
		ActorID: principal.UserID, PortalTokenID: principal.TokenID, ClientInstallationID: principal.ClientInstallationID,
		ServiceInstanceID: command.ServiceInstanceID, PortalOrigin: command.PortalOrigin, InitialTarget: command.Target,
		TargetGrants: grants, EmbedNonce: embedNonce, RuntimeGeneration: hostValue.RuntimeGeneration, ExpiresAt: expiresAt,
	}
	if err := store.CreateHostEmbedTicket(ctx, record); err != nil {
		return HostEmbedTicket{}, err
	}

	exchangeURL := url.URL{Scheme: s.scheme, Host: hostURLHost(hostValue.ID, s.hostSuffix), Path: "/__ch/embed-auth"}
	s.writeAudit(ctx, audit.Entry{ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind),
		Action: audit.ActionHostEnter, Target: command.HostID,
		Detail: map[string]any{"mode": "embed", "target": command.Target}})
	return HostEmbedTicket{ExchangeURL: exchangeURL.String(), Ticket: rawTicket, EmbedNonce: embedNonce, Targets: authorizedTargets, ExpiresAt: expiresAt}, nil
}

func (s *hostSharingService) ConsumeEmbedTicket(ctx context.Context, rawTicket, embedNonce, hostID, serviceInstanceID, portalOrigin string) (HostEmbedTicketRecord, error) {
	if s == nil || rawTicket == "" || len(rawTicket) != base64.RawURLEncoding.EncodedLen(hostEmbedTicketBytes) ||
		!validHostEmbedNonce(embedNonce) ||
		!domain.ValidHostID(hostID) || !domain.ValidServiceInstanceID(serviceInstanceID) || !validEmbedOrigin(portalOrigin, s.scheme) {
		return HostEmbedTicketRecord{}, domain.NewError(domain.CodeNotFound, "Host embed ticket not found")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawTicket)
	if err != nil || len(decoded) != hostEmbedTicketBytes {
		return HostEmbedTicketRecord{}, domain.NewError(domain.CodeNotFound, "Host embed ticket not found")
	}
	store, ok := s.sharing.(hostEmbedTicketStore)
	if !ok {
		return HostEmbedTicketRecord{}, domain.NewError(domain.CodeUnsupported, "embedded Host access is unavailable")
	}
	ticketHash := sha256.Sum256([]byte(rawTicket))
	record, err := store.ConsumeHostEmbedTicket(ctx, ticketHash[:], hostID, serviceInstanceID, portalOrigin, embedNonce)
	if err != nil || record.EmbedNonce != embedNonce {
		return HostEmbedTicketRecord{}, domain.NewError(domain.CodeNotFound, "Host embed ticket not found")
	}
	return record, nil
}

func validEmbedPrincipal(principal identity.Principal) bool {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return false
	}
	switch principal.Kind {
	case identity.KindSession:
		return principal.ClientInstallationID == "" || domain.ValidClientInstallationID(principal.ClientInstallationID)
	case identity.KindClient:
		return domain.ValidClientInstallationID(principal.ClientInstallationID)
	default:
		return false
	}
}

func validHostEmbedTarget(target string) bool {
	switch target {
	case HostEmbedTargetBrowser, HostEmbedTargetTerminal, HostEmbedTargetWorkspace, HostEmbedTargetFiles, HostEmbedTargetVSCode:
		return true
	default:
		return false
	}
}

func normalizeHostEmbedTargets(targets []string, initial string) ([]string, error) {
	if len(targets) == 0 {
		targets = []string{initial}
	}
	if len(targets) > 5 {
		return nil, domain.InvalidArgument("targets", "embedded Host target list is invalid")
	}
	unique := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if !validHostEmbedTarget(target) {
			return nil, domain.InvalidArgument("targets", "embedded Host target list is invalid")
		}
		if _, exists := seen[target]; !exists {
			seen[target] = struct{}{}
			unique = append(unique, target)
		}
	}
	if !containsHostEmbedTarget(unique, initial) {
		return nil, domain.InvalidArgument("target", "initial embedded Host target must be requested")
	}
	return unique, nil
}

func containsHostEmbedTarget(targets []string, target string) bool {
	for _, value := range targets {
		if value == target {
			return true
		}
	}
	return false
}

func embedTargetGrant(target string, owner bool, permissions []host.Permission) (HostEmbedTargetGrant, bool) {
	grant := HostEmbedTargetGrant{Target: target}
	switch target {
	case HostEmbedTargetBrowser:
		if owner || hasHostEmbedPermission(permissions, host.PermissionDesktopControl) {
			grant.PermissionCeiling = host.PermissionDesktopControl
			return grant, true
		}
		if hasHostEmbedPermission(permissions, host.PermissionDesktopView) {
			grant.PermissionCeiling = host.PermissionDesktopView
			return grant, true
		}
	case HostEmbedTargetTerminal:
		if owner || hasHostEmbedPermission(permissions, host.PermissionTerminalControl) && hasHostEmbedPermission(permissions, host.PermissionDesktopControl) {
			grant.PermissionCeiling = host.PermissionTerminalControl
			return grant, true
		}
	case HostEmbedTargetWorkspace:
		if owner || hasHostEmbedPermission(permissions, host.PermissionWorkspaceFileRead) {
			grant.PermissionCeiling = host.PermissionWorkspaceFileRead
			return grant, true
		}
	case HostEmbedTargetFiles, HostEmbedTargetVSCode:
		if owner {
			grant.PermissionCeiling = host.PermissionDesktopControl
			return grant, true
		}
	}
	return HostEmbedTargetGrant{}, false
}

func hasHostEmbedPermission(permissions []host.Permission, wanted host.Permission) bool {
	for _, permission := range permissions {
		if permission == wanted || wanted == host.PermissionDesktopView && permission == host.PermissionDesktopControl {
			return true
		}
	}
	return false
}

func validHostEmbedNonce(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(24) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 24
}

func validEmbedOrigin(value, scheme string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (scheme == "http" || scheme == "https") && parsed.Scheme == scheme && parsed.Host != "" &&
		parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Opaque == "" && parsed.String() == value
}

var _ HostEmbedTicketExchanger = (*hostSharingService)(nil)
