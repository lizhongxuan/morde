package app

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
)

type CodexStore interface {
	ConnectCodexProfile(context.Context, identity.Principal, codex.ProfileConnectionRequest) (codex.ProfileConnection, error)
	RevokeCodexProfile(context.Context, identity.Principal, string) error
	DiscoverCodexSession(context.Context, identity.Principal, codex.Discover) (codex.Session, error)
	ListCodexSessions(context.Context, string, string, string, int32) (codex.SessionPage, error)
	GetCodexEvents(context.Context, string, string, []codex.Scope, int64, int32) (codex.EventPage, error)
	WriteAudit(context.Context, audit.Entry) error
	CreateCodexCommand(context.Context, string, string, string, string, codex.CommandInput, []byte) (codex.Command, error)
	ClaimCodexCommand(context.Context, string, string) ([]codex.Command, error)
	ApplyCodexCommandResult(context.Context, string, string, string, codex.CommandResult) (codex.Command, error)
	AppendCodexEvents(context.Context, string, string, string, codex.EventBatch) (codex.EventBatchAck, error)
	DeleteCodexSession(context.Context, string, string) error
}

// CodexProfileStatusStore holds profile-level discovery and history progress.
// It is a narrow optional port so existing consumers that only mirror sessions
// do not need to provide status reporting behavior.
type CodexProfileStatusStore interface {
	UpdateCodexProfileSessionDiscovery(context.Context, identity.Principal, string, codex.ProfileSessionDiscoveryUpdate) (codex.ProfileConnection, error)
	ListCodexProfileConnections(context.Context, identity.Principal, string) ([]codex.ProfileConnection, error)
}

type CodexService struct{ store CodexStore }

var codexResultCodePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{0,128}$`)

func NewCodexService(store CodexStore) (*CodexService, error) {
	if store == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "Codex service store is required")
	}
	return &CodexService{store: store}, nil
}

func (s *CodexService) Connect(ctx context.Context, principal identity.Principal, request codex.ProfileConnectionRequest) (codex.ProfileConnection, error) {
	if err := validateCodexClient(principal); err != nil {
		return codex.ProfileConnection{}, err
	}
	if err := request.Validate(); err != nil {
		return codex.ProfileConnection{}, domain.InvalidArgument("connection", err.Error())
	}
	return s.store.ConnectCodexProfile(ctx, principal, request)
}

func (s *CodexService) Revoke(ctx context.Context, principal identity.Principal, profileKey string) error {
	if err := validateCodexClient(principal); err != nil {
		return err
	}
	if !codex.ValidProfileKey(profileKey) {
		return domain.InvalidArgument("profile_key", "Codex profile key is invalid")
	}
	return s.store.RevokeCodexProfile(ctx, principal, profileKey)
}

func (s *CodexService) UpdateSessionDiscovery(ctx context.Context, principal identity.Principal, profileKey string,
	update codex.ProfileSessionDiscoveryUpdate) (codex.ProfileConnection, error) {
	if err := validateCodexClient(principal); err != nil {
		return codex.ProfileConnection{}, err
	}
	if !codex.ValidProfileKey(profileKey) {
		return codex.ProfileConnection{}, domain.InvalidArgument("profile_key", "Codex profile key is invalid")
	}
	if err := update.Validate(); err != nil {
		return codex.ProfileConnection{}, domain.InvalidArgument("session_discovery", err.Error())
	}
	store, ok := s.store.(CodexProfileStatusStore)
	if !ok {
		return codex.ProfileConnection{}, domain.NewError(domain.CodeInternal, "Codex profile status store is unavailable")
	}
	return store.UpdateCodexProfileSessionDiscovery(ctx, principal, profileKey, update)
}

func (s *CodexService) ListProfileConnections(ctx context.Context, principal identity.Principal,
	installationID string) ([]codex.ProfileConnection, error) {
	if err := validateCodexUser(principal); err != nil {
		return nil, err
	}
	if !domain.ValidClientInstallationID(installationID) {
		return nil, domain.InvalidArgument("installation_id", "client installation id is invalid")
	}
	store, ok := s.store.(CodexProfileStatusStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "Codex profile status store is unavailable")
	}
	return store.ListCodexProfileConnections(ctx, principal, installationID)
}

func (s *CodexService) Discover(ctx context.Context, principal identity.Principal, request codex.Discover) (codex.Session, error) {
	if err := validateCodexClient(principal); err != nil {
		return codex.Session{}, err
	}
	if err := request.Validate(); err != nil {
		return codex.Session{}, domain.InvalidArgument("discover", err.Error())
	}
	return s.store.DiscoverCodexSession(ctx, principal, request)
}

func (s *CodexService) List(ctx context.Context, principal identity.Principal, installationID, cursor string, limit int32) (codex.SessionPage, error) {
	if err := validateCodexUser(principal); err != nil {
		return codex.SessionPage{}, err
	}
	if !domain.ValidClientInstallationID(installationID) || limit < 1 || limit > codex.MaxSessionPageSize ||
		(cursor != "" && !domain.ValidCodexSessionID(cursor)) {
		return codex.SessionPage{}, domain.InvalidArgument("page", "Codex session page request is invalid")
	}
	page, err := s.store.ListCodexSessions(ctx, principal.UserID, installationID, cursor, limit)
	if err != nil {
		return codex.SessionPage{}, err
	}
	if err := s.store.WriteAudit(ctx, codexReadAuditEntry(principal, audit.ActionCodexSessionListRead, installationID,
		map[string]any{"cursor": cursor, "row_count": len(page.Items)})); err != nil {
		return codex.SessionPage{}, fmt.Errorf("audit Codex session list read: %w", err)
	}
	return page, nil
}

func (s *CodexService) Events(ctx context.Context, principal identity.Principal, sessionID string, afterSequence int64, limit int32) (codex.EventPage, error) {
	page, err := s.events(ctx, principal, sessionID, []codex.Scope{codex.ScopeRead}, afterSequence, limit)
	if err != nil {
		return codex.EventPage{}, err
	}
	if len(page.Items) > 0 {
		if err := s.store.WriteAudit(ctx, codexReadAuditEntry(principal, audit.ActionCodexEventPageRead, sessionID,
			map[string]any{"after_sequence": afterSequence, "row_count": len(page.Items)})); err != nil {
			return codex.EventPage{}, fmt.Errorf("audit Codex history page read: %w", err)
		}
	}
	return page, nil
}

func (s *CodexService) LiveEvents(ctx context.Context, principal identity.Principal, sessionID string, afterSequence int64, limit int32) (codex.EventPage, error) {
	page, err := s.events(ctx, principal, sessionID, []codex.Scope{codex.ScopeRead, codex.ScopeLive}, afterSequence, limit)
	if err != nil {
		return codex.EventPage{}, err
	}
	if len(page.Items) > 0 {
		firstSequence := page.Items[0].Sequence
		lastSequence := page.Items[len(page.Items)-1].Sequence
		if err := s.store.WriteAudit(ctx, codexReadAuditEntry(principal, audit.ActionCodexEventStreamRead, sessionID,
			map[string]any{"after_sequence": afterSequence, "first_sequence": firstSequence,
				"last_sequence": lastSequence, "row_count": len(page.Items)})); err != nil {
			return codex.EventPage{}, fmt.Errorf("audit Codex event stream batch: %w", err)
		}
	}
	return page, nil
}

func (s *CodexService) events(ctx context.Context, principal identity.Principal, sessionID string, requiredScopes []codex.Scope, afterSequence int64, limit int32) (codex.EventPage, error) {
	if err := validateCodexUser(principal); err != nil {
		return codex.EventPage{}, err
	}
	if !codex.ValidateSessionID(sessionID) || afterSequence < 0 || limit < 1 || limit > codex.MaxEventPageSize {
		return codex.EventPage{}, domain.InvalidArgument("page", "Codex event page request is invalid")
	}
	return s.store.GetCodexEvents(ctx, principal.UserID, sessionID, requiredScopes, afterSequence, limit)
}

func (s *CodexService) CreateCommand(ctx context.Context, principal identity.Principal, sessionID, idempotencyKey string, input codex.CommandInput) (codex.Command, error) {
	if err := validateCodexUser(principal); err != nil {
		return codex.Command{}, err
	}
	if !codex.ValidateSessionID(sessionID) {
		return codex.Command{}, domain.NewError(domain.CodeNotFound, "Codex session not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return codex.Command{}, err
	}
	if err := input.Validate(); err != nil {
		return codex.Command{}, domain.InvalidArgument("command", err.Error())
	}
	hash, err := hashIdempotencyRequest(input)
	if err != nil {
		return codex.Command{}, fmt.Errorf("hash Codex command: %w", err)
	}
	installationID := ""
	if principal.Kind == identity.KindClient {
		installationID = principal.ClientInstallationID
	}
	return s.store.CreateCodexCommand(ctx, principal.UserID, installationID, sessionID, idempotencyKey, input, hash)
}

func (s *CodexService) Claim(ctx context.Context, principal identity.Principal) ([]codex.Command, error) {
	if err := validateCodexClient(principal); err != nil {
		return nil, err
	}
	return s.store.ClaimCodexCommand(ctx, principal.UserID, principal.ClientInstallationID)
}

func (s *CodexService) Result(ctx context.Context, principal identity.Principal, commandID string, result codex.CommandResult) (codex.Command, error) {
	if err := validateCodexClient(principal); err != nil {
		return codex.Command{}, err
	}
	if !codex.ValidateCommandID(commandID) || !result.Status.ValidResult() ||
		len(result.ProviderRequestID) > 512 || strings.ContainsAny(result.ProviderRequestID, "\x00\r\n") ||
		!codexResultCodePattern.MatchString(result.ResultCode) {
		return codex.Command{}, domain.InvalidArgument("result", "Codex command result is invalid")
	}
	return s.store.ApplyCodexCommandResult(ctx, principal.UserID, principal.ClientInstallationID, commandID, result)
}

func (s *CodexService) AppendEvents(ctx context.Context, principal identity.Principal, sessionID string, batch codex.EventBatch) (codex.EventBatchAck, error) {
	if err := validateCodexClient(principal); err != nil {
		return codex.EventBatchAck{}, err
	}
	if !codex.ValidateSessionID(sessionID) {
		return codex.EventBatchAck{}, domain.NewError(domain.CodeNotFound, "Codex session not found")
	}
	if _, err := batch.Validate(); err != nil {
		return codex.EventBatchAck{}, domain.InvalidArgument("events", err.Error())
	}
	return s.store.AppendCodexEvents(ctx, principal.UserID, principal.ClientInstallationID, sessionID, batch)
}

func (s *CodexService) Delete(ctx context.Context, principal identity.Principal, sessionID string) error {
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "an owner session is required to delete a Codex mirror")
	}
	if err := validateCodexUser(principal); err != nil {
		return err
	}
	if !codex.ValidateSessionID(sessionID) {
		return domain.NewError(domain.CodeNotFound, "Codex session not found")
	}
	return s.store.DeleteCodexSession(ctx, principal.UserID, sessionID)
}

func validateCodexUser(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeForbidden, "Codex session or client credential required")
	}
	if principal.Kind == identity.KindClient {
		return validateCodexClient(principal)
	}
	return nil
}

func validateCodexClient(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		principal.Kind != identity.KindClient || !domain.ValidClientInstallationID(principal.ClientInstallationID) ||
		len(principal.Scopes) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.HostIDs) != 0 || principal.HostScope != "" {
		return domain.NewError(domain.CodeForbidden, "active Codex client installation is required")
	}
	return nil
}

func codexReadAuditEntry(principal identity.Principal, action audit.Action, target string, detail map[string]any) audit.Entry {
	actorKind := audit.ActorSession
	auditDetail := make(map[string]any, len(detail)+1)
	for key, value := range detail {
		auditDetail[key] = value
	}
	if principal.Kind == identity.KindClient {
		actorKind = audit.ActorClient
		auditDetail["actor_installation_id"] = principal.ClientInstallationID
	}
	return audit.Entry{ActorID: principal.UserID, ActorKind: actorKind, Action: action, Target: target, Detail: auditDetail}
}
