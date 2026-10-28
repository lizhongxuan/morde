package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/mcpconnector"
)

type MCPConnectorOAuthStart struct {
	Kind             string
	AuthorizationURL string
	DeveloperURL     string
	RedirectURI      string
}

type mcpConnectorCredentialBundle struct {
	Kind    string                                  `json:"kind"`
	Headers map[string]string                       `json:"headers,omitempty"`
	OAuth   *mcpconnector.ConnectorOAuthCredentials `json:"oauth,omitempty"`
}

func validMCPConnectorOAuthRedirect(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Hostname() == "" || parsed.Path != "/api/mcp/oauth/callback" || parsed.Port() != "" && parsed.Port() != "443" ||
		net.ParseIP(parsed.Hostname()) != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, parsed.Hostname()) || strings.EqualFold(parsed.Host, parsed.Hostname()+":443")
}

func (s *MCPConnectorService) BeginOAuth(ctx context.Context, principal identity.Principal, connectorID string, expectedRevision int64, clientID, clientSecret string) (MCPConnectorOAuthStart, error) {
	if err := validateMCPConnectorPrincipal(principal); err != nil {
		return MCPConnectorOAuthStart{}, err
	}
	if !domain.ValidMCPConnectorID(connectorID) || expectedRevision < 1 {
		return MCPConnectorOAuthStart{}, domain.InvalidArgument("connector", "connector ID and positive revision are required")
	}
	record, err := s.store.GetMCPConnector(ctx, principal.UserID, connectorID)
	if err != nil {
		return MCPConnectorOAuthStart{}, err
	}
	if record.Revision != expectedRevision {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeConflict, "MCP connector changed before OAuth authorization")
	}
	var previous *mcpconnector.ConnectorOAuthCredentials
	if len(record.CredentialCipher) > 0 {
		bundle, decodeErr := s.openMCPConnectorCredential(record)
		if decodeErr != nil {
			return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInternal, "MCP connector credential is unavailable")
		}
		previous = bundle.OAuth
	}
	start, err := s.oauthClient.Begin(ctx, record.EndpointURL, s.redirectURI, clientID, clientSecret, previous)
	if err != nil {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInvalidArgument, "MCP server OAuth authorization could not be started")
	}
	if start.ClientRequired {
		return MCPConnectorOAuthStart{Kind: "client_required", DeveloperURL: start.DeveloperURL, RedirectURI: start.RedirectURI}, nil
	}
	if start.AuthorizationURL == "" || start.State == "" || len(start.Payload) == 0 || len(start.Payload) > 32<<10 {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInternal, "MCP server OAuth authorization could not be prepared")
	}
	flowID, err := randomMCPConnectorOAuthFlowID()
	if err != nil {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInternal, "MCP server OAuth authorization could not be prepared")
	}
	stateHash, err := (&mcpconnector.ConnectorOAuthStart{State: start.State}).DigestState()
	if err != nil {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInternal, "MCP server OAuth authorization could not be prepared")
	}
	payloadCipher, err := s.sealer.sealWithAAD(start.Payload, mcpConnectorOAuthFlowAAD(principal.UserID, connectorID, flowID))
	if err != nil {
		return MCPConnectorOAuthStart{}, domain.NewError(domain.CodeInternal, "MCP server OAuth authorization could not be prepared")
	}
	err = s.oauthStore.CreateMCPConnectorOAuthFlow(ctx, MCPConnectorOAuthFlow{
		ID: flowID, OwnerID: principal.UserID, ConnectorID: connectorID, ConnectorRevision: expectedRevision,
		StateHash: stateHash[:], PayloadCipher: payloadCipher, ExpiresAt: time.Now().Add(mcpconnector.ConnectorOAuthFlowTTL),
	})
	if err != nil {
		return MCPConnectorOAuthStart{}, err
	}
	return MCPConnectorOAuthStart{Kind: "authorize", AuthorizationURL: start.AuthorizationURL, RedirectURI: s.redirectURI}, nil
}

func (s *MCPConnectorService) CompleteOAuth(ctx context.Context, state, code, providerError, issuer string) error {
	if len(state) != 43 || len(code) > 8192 || len(providerError) > 128 || len(issuer) > 4096 {
		return domain.NewError(domain.CodeNotFound, "MCP OAuth authorization expired or invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(decoded) != 32 {
		return domain.NewError(domain.CodeNotFound, "MCP OAuth authorization expired or invalid")
	}
	stateHash := sha256.Sum256([]byte(state))
	flow, err := s.oauthStore.ConsumeMCPConnectorOAuthFlow(ctx, stateHash[:])
	if err != nil {
		return err
	}
	if providerError != "" {
		return domain.NewError(domain.CodeForbidden, "MCP OAuth authorization was not granted")
	}
	if code == "" {
		return domain.NewError(domain.CodeInvalidArgument, "MCP OAuth authorization code is missing")
	}
	payload, err := s.sealer.openWithAAD(flow.PayloadCipher, mcpConnectorOAuthFlowAAD(flow.OwnerID, flow.ConnectorID, flow.ID))
	if err != nil || mcpconnector.ValidateConnectorOAuthFlowPayload(payload) != nil {
		return domain.NewError(domain.CodeInternal, "MCP OAuth authorization state is invalid")
	}
	authorized, err := s.store.GetMCPConnector(ctx, flow.OwnerID, flow.ConnectorID)
	if err != nil {
		return err
	}
	if authorized.ID != flow.ConnectorID || authorized.OwnerID != flow.OwnerID || authorized.Revision != flow.ConnectorRevision {
		return domain.NewError(domain.CodeConflict, "MCP connector changed during OAuth authorization")
	}
	credentials, err := s.oauthClient.Exchange(ctx, payload, code, issuer)
	if err != nil {
		return domain.NewError(domain.CodeInvalidState, "MCP OAuth token exchange failed; restart authorization")
	}
	record, err := s.store.GetMCPConnector(ctx, flow.OwnerID, flow.ConnectorID)
	if err != nil {
		return err
	}
	if record.ID != flow.ConnectorID || record.OwnerID != flow.OwnerID || record.Revision != flow.ConnectorRevision {
		return domain.NewError(domain.CodeConflict, "MCP connector changed during OAuth authorization")
	}
	if record.CredentialVersion == int64(^uint64(0)>>1) {
		return domain.NewError(domain.CodeConflict, "MCP connector credential version is exhausted")
	}
	record.CredentialVersion++
	credentialJSON, err := json.Marshal(mcpConnectorCredentialBundle{Kind: "oauth", OAuth: &credentials})
	if err != nil {
		return domain.NewError(domain.CodeInternal, "MCP OAuth credentials could not be stored")
	}
	record.CredentialCipher, err = s.sealer.sealWithAAD(credentialJSON,
		mcpConnectorCredentialAAD(flow.OwnerID, flow.ConnectorID, record.CredentialVersion))
	if err != nil {
		return domain.NewError(domain.CodeInternal, "MCP OAuth credentials could not be stored")
	}
	entry := mcpConnectorAudit(flow.OwnerID, audit.ActionMCPConnectorCredentialRotate, flow.ConnectorID, map[string]any{
		"credential_version": record.CredentialVersion, "auth_type": "oauth",
	})
	if _, err := s.store.UpdateMCPConnector(ctx, flow.OwnerID, record, flow.ConnectorRevision, entry); err != nil {
		return err
	}
	return nil
}

func (s *MCPConnectorService) CredentialHeaders(ctx context.Context, record MCPConnectorRecord) (http.Header, error) {
	headers := make(http.Header)
	if !domain.ValidMCPConnectorID(record.ID) || !domain.ValidUserID(record.OwnerID) || record.CredentialVersion < 0 {
		return nil, domain.InvalidArgument("connector", "MCP connector credential context is invalid")
	}
	if len(record.CredentialCipher) == 0 {
		return headers, nil
	}
	bundle, err := s.openMCPConnectorCredential(record)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "MCP connector credential is unavailable")
	}
	if bundle.Kind == "oauth" && bundle.OAuth != nil {
		credentials := *bundle.OAuth
		if credentials.ExpiresAt > 0 && time.Now().Add(time.Minute).UnixMilli() >= credentials.ExpiresAt {
			if credentials.RefreshToken == "" {
				return nil, domain.NewError(domain.CodeForbidden, "MCP connector needs to be authorized again")
			}
			refreshed, refreshErr := s.oauthClient.Refresh(ctx, credentials)
			if refreshErr != nil {
				current, getErr := s.store.GetMCPConnector(ctx, record.OwnerID, record.ID)
				if getErr != nil || current.Revision != record.Revision || current.CredentialVersion != record.CredentialVersion || bytes.Equal(current.CredentialCipher, record.CredentialCipher) {
					return nil, domain.NewError(domain.CodeForbidden, "MCP connector needs to be authorized again")
				}
				bundle, err = s.openMCPConnectorCredential(current)
				if err != nil || bundle.OAuth == nil || bundle.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() >= bundle.OAuth.ExpiresAt {
					return nil, domain.NewError(domain.CodeConflict, "MCP connector token refresh must be retried")
				}
				credentials = *bundle.OAuth
			} else {
				credentialJSON, marshalErr := json.Marshal(mcpConnectorCredentialBundle{Kind: "oauth", OAuth: &refreshed})
				if marshalErr != nil {
					return nil, domain.NewError(domain.CodeInternal, "MCP connector credential is unavailable")
				}
				nextCipher, sealErr := s.sealer.sealWithAAD(credentialJSON,
					mcpConnectorCredentialAAD(record.OwnerID, record.ID, record.CredentialVersion))
				if sealErr != nil {
					return nil, domain.NewError(domain.CodeInternal, "MCP connector credential is unavailable")
				}
				saved, saveErr := s.oauthStore.ReplaceMCPConnectorOAuthCredential(ctx, record.OwnerID, record.ID,
					record.CredentialVersion, record.CredentialCipher, nextCipher)
				if saveErr != nil {
					return nil, saveErr
				}
				if !saved {
					current, getErr := s.store.GetMCPConnector(ctx, record.OwnerID, record.ID)
					if getErr != nil || current.Revision != record.Revision || current.CredentialVersion != record.CredentialVersion {
						return nil, domain.NewError(domain.CodeConflict, "MCP connector changed during token refresh")
					}
					bundle, err = s.openMCPConnectorCredential(current)
					if err != nil || bundle.OAuth == nil || bundle.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() >= bundle.OAuth.ExpiresAt {
						return nil, domain.NewError(domain.CodeConflict, "MCP connector token refresh must be retried")
					}
					credentials = *bundle.OAuth
				} else {
					credentials = refreshed
				}
			}
		}
		authorization := credentials.AuthorizationHeader(time.Now())
		if authorization == "" {
			return nil, domain.NewError(domain.CodeForbidden, "MCP connector needs to be authorized again")
		}
		headers.Set("Authorization", authorization)
		return headers, nil
	}
	credentialHeaders := bundle.Headers
	if bundle.Kind == "none" {
		return headers, nil
	}
	if len(credentialHeaders) > 4 {
		return nil, domain.NewError(domain.CodeInvalidArgument, "MCP connector credential headers exceed their limit")
	}
	total := 0
	for name, value := range credentialHeaders {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if canonical == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, domain.NewError(domain.CodeInvalidArgument, "MCP connector credential headers are invalid")
		}
		total += len(name) + len(value)
		if total > 8192 {
			return nil, domain.NewError(domain.CodeInvalidArgument, "MCP connector credential headers exceed their limit")
		}
		headers.Set(canonical, value)
	}
	return headers, nil
}

func (s *MCPConnectorService) openMCPConnectorCredential(record MCPConnectorRecord) (mcpConnectorCredentialBundle, error) {
	plain, err := s.sealer.openWithAAD(record.CredentialCipher,
		mcpConnectorCredentialAAD(record.OwnerID, record.ID, record.CredentialVersion))
	if err != nil {
		return mcpConnectorCredentialBundle{}, err
	}
	var bundle mcpConnectorCredentialBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return mcpConnectorCredentialBundle{}, errors.New("MCP connector credential format is invalid")
	}
	if bundle.Kind == "" && bundle.Headers == nil {
		var flat map[string]string
		if err := json.Unmarshal(plain, &flat); err != nil || flat == nil {
			return mcpConnectorCredentialBundle{}, errors.New("MCP connector credential format is invalid")
		}
		bundle = mcpConnectorCredentialBundle{Kind: "headers", Headers: flat}
	}
	if bundle.Kind == "oauth" && bundle.OAuth == nil || bundle.Kind != "" && bundle.Kind != "oauth" && bundle.Kind != "headers" && bundle.Kind != "none" {
		return mcpConnectorCredentialBundle{}, errors.New("MCP connector credential format is invalid")
	}
	return bundle, nil
}

func randomMCPConnectorOAuthFlowID() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "o" + base64.RawURLEncoding.EncodeToString(value), nil
}

func mcpConnectorOAuthFlowAAD(ownerID, connectorID, flowID string) []byte {
	return []byte("morde:mcp-connector-oauth-flow:v1\x00" + ownerID + "\x00" + connectorID + "\x00" + flowID)
}

var _ connectorOAuthClient = (*mcpconnector.ConnectorOAuthClient)(nil)
