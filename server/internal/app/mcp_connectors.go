package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/mcpconnector"
)

const mcpConnectorCredentialMaxBytes = 32 << 10

type MCPConnectorRecord struct {
	ID                string
	OwnerID           string
	Name              string
	EndpointURL       string
	Enabled           bool
	CredentialCipher  []byte
	CredentialVersion int64
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type MCPConnectorStore interface {
	CreateMCPConnector(context.Context, MCPConnectorRecord, audit.Entry) (MCPConnectorRecord, error)
	ListMCPConnectors(context.Context, string) ([]MCPConnectorRecord, error)
	GetMCPConnector(context.Context, string, string) (MCPConnectorRecord, error)
	UpdateMCPConnector(context.Context, string, MCPConnectorRecord, int64, audit.Entry) (MCPConnectorRecord, error)
	DeleteMCPConnector(context.Context, string, string, int64, audit.Entry) error
}

type MCPConnectorOAuthFlow struct {
	ID                string
	OwnerID           string
	ConnectorID       string
	ConnectorRevision int64
	StateHash         []byte
	PayloadCipher     []byte
	ExpiresAt         time.Time
}

type MCPConnectorOAuthStore interface {
	CreateMCPConnectorOAuthFlow(context.Context, MCPConnectorOAuthFlow) error
	ConsumeMCPConnectorOAuthFlow(context.Context, []byte) (MCPConnectorOAuthFlow, error)
	ReplaceMCPConnectorOAuthCredential(context.Context, string, string, int64, []byte, []byte) (bool, error)
}

type connectorOAuthClient interface {
	Begin(context.Context, string, string, string, string, *mcpconnector.ConnectorOAuthCredentials) (mcpconnector.ConnectorOAuthStart, error)
	Exchange(context.Context, []byte, string, string) (mcpconnector.ConnectorOAuthCredentials, error)
	Refresh(context.Context, mcpconnector.ConnectorOAuthCredentials) (mcpconnector.ConnectorOAuthCredentials, error)
}

type MCPConnectorView struct {
	ID                string
	Name              string
	EndpointURL       string
	Enabled           bool
	HasCredential     bool
	CredentialVersion int64
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type CreateMCPConnectorCommand struct {
	Name           string
	EndpointURL    string
	CredentialJSON []byte
}

type UpdateMCPConnectorCommand struct {
	Name             string
	EndpointURL      string
	ExpectedRevision int64
}

type MCPConnectorRevisionCommand struct {
	ExpectedRevision int64
}

type RotateMCPConnectorCredentialCommand struct {
	CredentialJSON   []byte
	ExpectedRevision int64
}

type MCPConnectorService struct {
	store       MCPConnectorStore
	oauthStore  MCPConnectorOAuthStore
	sealer      *Sealer
	oauthClient connectorOAuthClient
	redirectURI string
}

func NewMCPConnectorService(store MCPConnectorStore, sealer *Sealer, redirectURI string) (*MCPConnectorService, error) {
	oauthStore, ok := store.(MCPConnectorOAuthStore)
	if store == nil || sealer == nil || !ok || !validMCPConnectorOAuthRedirect(redirectURI) {
		return nil, domain.NewError(domain.CodeInvalidArgument, "MCP connector dependencies are required")
	}
	return &MCPConnectorService{
		store: store, oauthStore: oauthStore, sealer: sealer,
		oauthClient: mcpconnector.NewConnectorOAuthClient(), redirectURI: redirectURI,
	}, nil
}

func (s *MCPConnectorService) List(ctx context.Context, principal identity.Principal) ([]MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return nil, err
	}
	records, err := s.store.ListMCPConnectors(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	views := make([]MCPConnectorView, 0, len(records))
	for _, record := range records {
		views = append(views, mcpConnectorView(record))
	}
	return views, nil
}

func (s *MCPConnectorService) Get(ctx context.Context, principal identity.Principal, id string) (MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorView{}, err
	}
	if !domain.ValidMCPConnectorID(id) {
		return MCPConnectorView{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	record, err := s.store.GetMCPConnector(ctx, principal.UserID, id)
	if err != nil {
		return MCPConnectorView{}, err
	}
	return mcpConnectorView(record), nil
}

func (s *MCPConnectorService) Create(ctx context.Context, principal identity.Principal, command CreateMCPConnectorCommand) (MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorView{}, err
	}
	name, endpoint, err := validateMCPConnectorMetadata(command.Name, command.EndpointURL)
	if err != nil {
		return MCPConnectorView{}, err
	}
	if err := validateMCPConnectorCredential(command.CredentialJSON, true); err != nil {
		return MCPConnectorView{}, err
	}
	id, err := domain.NewMCPConnectorID()
	if err != nil {
		return MCPConnectorView{}, fmt.Errorf("generate MCP connector ID: %w", err)
	}
	var cipher []byte
	var credentialVersion int64
	if len(command.CredentialJSON) > 0 {
		credentialVersion = 1
		cipher, err = s.sealer.sealWithAAD(command.CredentialJSON, mcpConnectorCredentialAAD(principal.UserID, id, credentialVersion))
		if err != nil {
			return MCPConnectorView{}, fmt.Errorf("encrypt MCP connector credential: %w", err)
		}
	}
	entry := mcpConnectorAudit(principal.UserID, audit.ActionMCPConnectorCreate, id, map[string]any{
		"revision": int64(1), "has_credential": len(cipher) > 0,
	})
	created, err := s.store.CreateMCPConnector(ctx, MCPConnectorRecord{
		ID: id, OwnerID: principal.UserID, Name: name, EndpointURL: endpoint,
		CredentialCipher: cipher, CredentialVersion: credentialVersion,
	}, entry)
	if err != nil {
		return MCPConnectorView{}, err
	}
	return mcpConnectorView(created), nil
}

func (s *MCPConnectorService) Update(ctx context.Context, principal identity.Principal, id string, command UpdateMCPConnectorCommand) (MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorView{}, err
	}
	if !domain.ValidMCPConnectorID(id) {
		return MCPConnectorView{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if command.ExpectedRevision < 1 {
		return MCPConnectorView{}, domain.InvalidArgument("expected_revision", "a positive connector revision is required")
	}
	name, endpoint, err := validateMCPConnectorMetadata(command.Name, command.EndpointURL)
	if err != nil {
		return MCPConnectorView{}, err
	}
	current, err := s.store.GetMCPConnector(ctx, principal.UserID, id)
	if err != nil {
		return MCPConnectorView{}, err
	}
	endpointChanged := current.EndpointURL != endpoint
	if endpointChanged {
		if current.CredentialVersion == int64(^uint64(0)>>1) {
			return MCPConnectorView{}, domain.NewError(domain.CodeConflict, "MCP connector credential version is exhausted")
		}
		current.CredentialCipher = nil
		current.CredentialVersion++
	}
	current.Name, current.EndpointURL = name, endpoint
	detail := map[string]any{"expected_revision": command.ExpectedRevision}
	if endpointChanged {
		detail["credential_invalidated"] = true
	}
	entry := mcpConnectorAudit(principal.UserID, audit.ActionMCPConnectorUpdate, id, detail)
	updated, err := s.store.UpdateMCPConnector(ctx, principal.UserID, current, command.ExpectedRevision, entry)
	if err != nil {
		return MCPConnectorView{}, err
	}
	return mcpConnectorView(updated), nil
}

func (s *MCPConnectorService) RotateCredential(ctx context.Context, principal identity.Principal, id string, command RotateMCPConnectorCredentialCommand) (MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorView{}, err
	}
	if !domain.ValidMCPConnectorID(id) {
		return MCPConnectorView{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if command.ExpectedRevision < 1 {
		return MCPConnectorView{}, domain.InvalidArgument("expected_revision", "a positive connector revision is required")
	}
	if err := validateMCPConnectorCredential(command.CredentialJSON, false); err != nil {
		return MCPConnectorView{}, err
	}
	current, err := s.store.GetMCPConnector(ctx, principal.UserID, id)
	if err != nil {
		return MCPConnectorView{}, err
	}
	if current.CredentialVersion == int64(^uint64(0)>>1) {
		return MCPConnectorView{}, domain.NewError(domain.CodeConflict, "MCP connector credential version is exhausted")
	}
	current.CredentialVersion++
	current.CredentialCipher, err = s.sealer.sealWithAAD(command.CredentialJSON,
		mcpConnectorCredentialAAD(principal.UserID, id, current.CredentialVersion))
	if err != nil {
		return MCPConnectorView{}, fmt.Errorf("encrypt MCP connector credential: %w", err)
	}
	entry := mcpConnectorAudit(principal.UserID, audit.ActionMCPConnectorCredentialRotate, id, map[string]any{
		"credential_version": current.CredentialVersion, "expected_revision": command.ExpectedRevision,
	})
	updated, err := s.store.UpdateMCPConnector(ctx, principal.UserID, current, command.ExpectedRevision, entry)
	if err != nil {
		return MCPConnectorView{}, err
	}
	return mcpConnectorView(updated), nil
}

func (s *MCPConnectorService) SetEnabled(ctx context.Context, principal identity.Principal, id string, enabled bool, command MCPConnectorRevisionCommand) (MCPConnectorView, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorView{}, err
	}
	if !domain.ValidMCPConnectorID(id) {
		return MCPConnectorView{}, domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if command.ExpectedRevision < 1 {
		return MCPConnectorView{}, domain.InvalidArgument("expected_revision", "a positive connector revision is required")
	}
	current, err := s.store.GetMCPConnector(ctx, principal.UserID, id)
	if err != nil {
		return MCPConnectorView{}, err
	}
	if current.Revision != command.ExpectedRevision {
		return MCPConnectorView{}, domain.NewError(domain.CodeConflict, "MCP connector changed before update")
	}
	if current.Enabled == enabled {
		return mcpConnectorView(current), nil
	}
	current.Enabled = enabled
	action := audit.ActionMCPConnectorDisable
	if enabled {
		action = audit.ActionMCPConnectorEnable
	}
	entry := mcpConnectorAudit(principal.UserID, action, id, map[string]any{
		"enabled": enabled, "expected_revision": command.ExpectedRevision,
	})
	updated, err := s.store.UpdateMCPConnector(ctx, principal.UserID, current, command.ExpectedRevision, entry)
	if err != nil {
		return MCPConnectorView{}, err
	}
	return mcpConnectorView(updated), nil
}

func (s *MCPConnectorService) Delete(ctx context.Context, principal identity.Principal, id string, command MCPConnectorRevisionCommand) error {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidMCPConnectorID(id) {
		return domain.NewError(domain.CodeNotFound, "MCP connector not found")
	}
	if command.ExpectedRevision < 1 {
		return domain.InvalidArgument("expected_revision", "a positive connector revision is required")
	}
	entry := mcpConnectorAudit(principal.UserID, audit.ActionMCPConnectorDelete, id, map[string]any{
		"expected_revision": command.ExpectedRevision,
	})
	return s.store.DeleteMCPConnector(ctx, principal.UserID, id, command.ExpectedRevision, entry)
}

func validateMCPConnectorPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" ||
		principal.ReplicaWorkspaceID != "" || len(principal.HostIDs) != 0 || len(principal.WorkspaceIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func validateMCPConnectorMetadata(name, endpoint string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", "", domain.InvalidArgument("name", "connector name is required and must be within limits")
	}
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || len(endpoint) > 2048 || !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || strings.Contains(endpoint, "#") || strings.Contains(parsed.EscapedPath(), "\\") {
		return "", "", domain.InvalidArgument("endpoint_url", "connector endpoint must be an HTTPS URL without credentials, query, or fragment")
	}
	port := parsed.Port()
	host := strings.ToLower(parsed.Hostname())
	if (port != "" && port != "443") || !strings.EqualFold(parsed.Host, host) && !strings.EqualFold(parsed.Host, host+":443") ||
		net.ParseIP(host) != nil || !validAllowedModelHost(host) {
		return "", "", domain.InvalidArgument("endpoint_url", "connector endpoint must use a public DNS hostname on port 443")
	}
	for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
		decoded, decodeErr := url.PathUnescape(segment)
		if decodeErr != nil || decoded == "." || decoded == ".." || strings.Contains(decoded, "\\") {
			return "", "", domain.InvalidArgument("endpoint_url", "connector endpoint path is invalid")
		}
	}
	parsed.Scheme, parsed.Host = "https", host
	return name, parsed.String(), nil
}

func validateMCPConnectorCredential(value []byte, optional bool) error {
	if len(value) == 0 && optional {
		return nil
	}
	if len(value) == 0 || len(value) > mcpConnectorCredentialMaxBytes {
		return domain.InvalidArgument("credential", "connector credential must be a JSON object within the size limit")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return domain.InvalidArgument("credential", "connector credential must be a JSON object within the size limit")
	}
	return nil
}

func mcpConnectorCredentialAAD(ownerID, connectorID string, credentialVersion int64) []byte {
	return []byte("morde:mcp-connector-credential:v1\x00" + ownerID + "\x00" + connectorID + "\x00" + strconv.FormatInt(credentialVersion, 10))
}

func mcpConnectorAudit(ownerID string, action audit.Action, connectorID string, detail map[string]any) audit.Entry {
	return audit.Entry{ActorID: ownerID, ActorKind: audit.ActorSession, Action: action, Target: connectorID, Detail: detail}
}

func mcpConnectorView(record MCPConnectorRecord) MCPConnectorView {
	return MCPConnectorView{
		ID: record.ID, Name: record.Name, EndpointURL: record.EndpointURL, Enabled: record.Enabled,
		HasCredential: len(record.CredentialCipher) > 0, CredentialVersion: record.CredentialVersion,
		Revision: record.Revision, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}
