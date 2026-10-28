package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/mcpconnector"
)

type mcpConnectorOAuthClientStub struct {
	start          mcpconnector.ConnectorOAuthStart
	credentials    mcpconnector.ConnectorOAuthCredentials
	refreshed      mcpconnector.ConnectorOAuthCredentials
	beginCalls     int
	exchangeCalls  int
	refreshCalls   int
	callbackCode   string
	callbackIssuer string
}

func (client *mcpConnectorOAuthClientStub) Begin(_ context.Context, endpoint, redirect, clientID, clientSecret string, previous *mcpconnector.ConnectorOAuthCredentials) (mcpconnector.ConnectorOAuthStart, error) {
	client.beginCalls++
	if endpoint == "" || redirect == "" || clientID != "" || clientSecret != "" || previous != nil {
		return mcpconnector.ConnectorOAuthStart{}, domain.InvalidArgument("oauth", "unexpected OAuth begin arguments")
	}
	return client.start, nil
}

func (client *mcpConnectorOAuthClientStub) Exchange(_ context.Context, _ []byte, code, issuer string) (mcpconnector.ConnectorOAuthCredentials, error) {
	client.exchangeCalls++
	client.callbackCode, client.callbackIssuer = code, issuer
	return client.credentials, nil
}

func (client *mcpConnectorOAuthClientStub) Refresh(context.Context, mcpconnector.ConnectorOAuthCredentials) (mcpconnector.ConnectorOAuthCredentials, error) {
	client.refreshCalls++
	return client.refreshed, nil
}

func TestMCPConnectorOAuthCallbackEncryptsTokensAndRefreshesWithCAS(t *testing.T) {
	service, store, sealer := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{
		Name: "Notion", EndpointURL: "https://mcp.example.test/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	state := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	payload, err := json.Marshal(map[string]any{
		"verifier": "one-time-pkce-verifier", "issuer": "https://auth.example.test",
		"authorization_endpoint": "https://auth.example.test/authorize", "token_endpoint": "https://auth.example.test/token",
		"resource": "https://mcp.example.test/mcp", "registration": map[string]string{"client_id": "client", "auth_method": "none"},
		"redirect_uri": "https://portal.example.test/api/mcp/oauth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &mcpConnectorOAuthClientStub{
		start: mcpconnector.ConnectorOAuthStart{
			AuthorizationURL: "https://auth.example.test/authorize?state=" + state, State: state, Payload: payload,
		},
		credentials: mcpconnector.ConnectorOAuthCredentials{
			Issuer: "https://auth.example.test", AuthorizationEndpoint: "https://auth.example.test/authorize",
			TokenEndpoint: "https://auth.example.test/token", Resource: "https://mcp.example.test/mcp",
			Registration: mcpconnector.ConnectorOAuthRegistration{ClientID: "client", AuthMethod: "none"},
			AccessToken:  "access-before-refresh", RefreshToken: "refresh-before-refresh", ExpiresAt: time.Now().Add(5 * time.Second).UnixMilli(), ExpiresIn: 3600,
		},
		refreshed: mcpconnector.ConnectorOAuthCredentials{
			Issuer: "https://auth.example.test", AuthorizationEndpoint: "https://auth.example.test/authorize",
			TokenEndpoint: "https://auth.example.test/token", Resource: "https://mcp.example.test/mcp",
			Registration: mcpconnector.ConnectorOAuthRegistration{ClientID: "client", AuthMethod: "none"},
			AccessToken:  "rotated-access-token", RefreshToken: "rotated-refresh-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), ExpiresIn: 3600,
		},
	}
	service.oauthClient = client
	started, err := service.BeginOAuth(context.Background(), principal, created.ID, created.Revision, "", "")
	if err != nil || started.Kind != "authorize" || started.AuthorizationURL == "" || len(store.flows) != 1 {
		t.Fatalf("BeginOAuth() = %#v flows=%d, %v", started, len(store.flows), err)
	}
	var flow MCPConnectorOAuthFlow
	for _, flow = range store.flows {
		break
	}
	if bytes.Equal(flow.StateHash, []byte(state)) || bytes.Contains(flow.PayloadCipher, []byte("one-time-pkce-verifier")) ||
		!flow.ExpiresAt.After(time.Now()) || flow.ConnectorRevision != created.Revision {
		t.Fatal("OAuth state or PKCE verifier was not stored as an expiring hash and encrypted payload")
	}
	if err := service.CompleteOAuth(context.Background(), state, "authorization-code", "", "https://auth.example.test"); err != nil {
		t.Fatal(err)
	}
	if client.exchangeCalls != 1 || client.callbackCode != "authorization-code" || client.callbackIssuer != "https://auth.example.test" {
		t.Fatalf("OAuth exchange inputs = calls:%d code:%q issuer:%q", client.exchangeCalls, client.callbackCode, client.callbackIssuer)
	}
	stored := store.values[created.ID]
	if stored.CredentialVersion != 1 || stored.Revision != 2 || bytes.Contains(stored.CredentialCipher, []byte("rotated-access-token")) {
		t.Fatal("OAuth token set was not encrypted and versioned")
	}
	plain, err := sealer.openWithAAD(stored.CredentialCipher, mcpConnectorCredentialAAD(principal.UserID, created.ID, stored.CredentialVersion))
	if err != nil || !strings.Contains(string(plain), "access-before-refresh") || strings.Contains(string(plain), "rotated-access-token") {
		t.Fatalf("stored OAuth token bundle = %q, %v", plain, err)
	}
	headers, err := service.CredentialHeaders(context.Background(), stored)
	if err != nil || headers.Get("Authorization") != "Bearer rotated-access-token" || client.refreshCalls != 1 {
		t.Fatalf("CredentialHeaders() = %v, %v; refresh calls=%d", headers, err, client.refreshCalls)
	}
	refreshed := store.values[created.ID]
	if refreshed.CredentialVersion != stored.CredentialVersion || refreshed.Revision != stored.Revision {
		t.Fatalf("refresh changed grant-bound versions: before=%#v after=%#v", stored, refreshed)
	}
	plain, err = sealer.openWithAAD(refreshed.CredentialCipher, mcpConnectorCredentialAAD(principal.UserID, created.ID, refreshed.CredentialVersion))
	if err != nil || !strings.Contains(string(plain), "rotated-access-token") || !strings.Contains(string(plain), "rotated-refresh-token") {
		t.Fatalf("rotated OAuth token bundle = %q, %v", plain, err)
	}
	if err := service.CompleteOAuth(context.Background(), state, "authorization-code", "", "https://auth.example.test"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("OAuth state replay error = %v", err)
	}
	view, err := service.Get(context.Background(), principal, created.ID)
	encoded, marshalErr := json.Marshal(view)
	if err != nil || marshalErr != nil || strings.Contains(string(encoded), "rotated-access-token") || strings.Contains(string(encoded), "rotated-refresh-token") {
		t.Fatalf("connector view leaked OAuth tokens: %s, %v, %v", encoded, err, marshalErr)
	}
}

func TestMCPConnectorOAuthProviderDenialConsumesState(t *testing.T) {
	service, store, _ := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{Name: "Linear", EndpointURL: "https://mcp.example.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	state := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	payload, _ := json.Marshal(map[string]any{
		"verifier": "verifier", "issuer": "https://auth.example.test", "token_endpoint": "https://auth.example.test/token",
		"registration": map[string]string{"client_id": "client", "auth_method": "none"}, "redirect_uri": "https://portal.example.test/api/mcp/oauth/callback",
	})
	service.oauthClient = &mcpConnectorOAuthClientStub{start: mcpconnector.ConnectorOAuthStart{
		AuthorizationURL: "https://auth.example.test/authorize?state=" + state, State: state, Payload: payload,
	}}
	if _, err := service.BeginOAuth(context.Background(), principal, created.ID, created.Revision, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteOAuth(context.Background(), state, "", "access_denied", "https://auth.example.test"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("OAuth denial error = %v", err)
	}
	if len(store.flows) != 0 || store.values[created.ID].CredentialVersion != 0 {
		t.Fatal("denied OAuth state was reusable or credentials were written")
	}
}

func TestMCPConnectorOAuthStaleConnectorDoesNotExchangeCode(t *testing.T) {
	service, store, _ := mcpConnectorTestService(t)
	principal := mcpConnectorPrincipal("u1234567")
	created, err := service.Create(context.Background(), principal, CreateMCPConnectorCommand{
		Name: "Notion", EndpointURL: "https://mcp.example.test/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	state := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	payload, err := json.Marshal(map[string]any{
		"verifier": "one-time-pkce-verifier", "issuer": "https://auth.example.test",
		"authorization_endpoint": "https://auth.example.test/authorize", "token_endpoint": "https://auth.example.test/token",
		"resource": "https://mcp.example.test/mcp", "registration": map[string]string{"client_id": "client", "auth_method": "none"},
		"redirect_uri": "https://portal.example.test/api/mcp/oauth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &mcpConnectorOAuthClientStub{start: mcpconnector.ConnectorOAuthStart{
		AuthorizationURL: "https://auth.example.test/authorize?state=" + state, State: state, Payload: payload,
	}}
	service.oauthClient = client
	if _, err := service.BeginOAuth(context.Background(), principal, created.ID, created.Revision, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(context.Background(), principal, created.ID, UpdateMCPConnectorCommand{
		Name: "Notion renamed", EndpointURL: created.EndpointURL, ExpectedRevision: created.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteOAuth(context.Background(), state, "authorization-code", "", "https://auth.example.test"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale OAuth callback error = %v", err)
	}
	if client.exchangeCalls != 0 || store.values[created.ID].CredentialVersion != 0 || len(store.values[created.ID].CredentialCipher) != 0 || len(store.flows) != 0 {
		t.Fatalf("stale callback exchanged or stored credentials: exchanges=%d connector=%#v remaining_flows=%d",
			client.exchangeCalls, store.values[created.ID], len(store.flows))
	}
}
