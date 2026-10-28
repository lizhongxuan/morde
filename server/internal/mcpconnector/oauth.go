package mcpconnector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	connectorOAuthResponseLimit = 32 << 10
	ConnectorOAuthFlowTTL       = 10 * time.Minute
	connectorOAuthClientName    = "Morde"
	connectorOAuthProtocol      = "2025-06-18"
)

var oauthChallengeField = regexp.MustCompile(`(?i)([a-z_]+)="((?:\\.|[^"\\])*)"`)

type ConnectorOAuthClient struct {
	transport http.RoundTripper
}

type ConnectorOAuthRegistration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthMethod   string `json:"auth_method"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

type ConnectorOAuthCredentials struct {
	Issuer                string                     `json:"issuer"`
	AuthorizationEndpoint string                     `json:"authorization_endpoint"`
	TokenEndpoint         string                     `json:"token_endpoint"`
	Resource              string                     `json:"resource"`
	Scope                 string                     `json:"scope,omitempty"`
	Registration          ConnectorOAuthRegistration `json:"registration"`
	AccessToken           string                     `json:"access_token"`
	RefreshToken          string                     `json:"refresh_token,omitempty"`
	ExpiresAt             int64                      `json:"expires_at,omitempty"`
	ExpiresIn             int64                      `json:"expires_in,omitempty"`
}

type ConnectorOAuthStart struct {
	AuthorizationURL string
	State            string
	Payload          []byte
	ClientRequired   bool
	DeveloperURL     string
	RedirectURI      string
}

type connectorOAuthFlow struct {
	Verifier              string                     `json:"verifier"`
	Issuer                string                     `json:"issuer"`
	AuthorizationEndpoint string                     `json:"authorization_endpoint"`
	TokenEndpoint         string                     `json:"token_endpoint"`
	Resource              string                     `json:"resource"`
	Scope                 string                     `json:"scope,omitempty"`
	Registration          ConnectorOAuthRegistration `json:"registration"`
	RedirectURI           string                     `json:"redirect_uri"`
}

type connectorOAuthMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	TokenEndpointAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type connectorOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

type oauthRegistrationResponse struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret"`
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	ClientSecretExpiresAt   int64  `json:"client_secret_expires_at"`
	ClientIDExpiresAt       int64  `json:"client_id_expires_at"`
}

func NewConnectorOAuthClient() *ConnectorOAuthClient { return &ConnectorOAuthClient{} }

func (client *ConnectorOAuthClient) Begin(ctx context.Context, connectorURL, redirectURI, clientID, clientSecret string, previous *ConnectorOAuthCredentials) (ConnectorOAuthStart, error) {
	if ctx == nil || !validOAuthURI(redirectURI, false) || len(clientID) > 2048 || len(clientSecret) > 4096 || clientID == "" && clientSecret != "" ||
		strings.ContainsAny(clientID+clientSecret, "\r\n\x00") {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth request is invalid")
	}
	endpoint, err := validOAuthURIValue(connectorURL, false)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth connector endpoint is invalid")
	}
	probe := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + connectorOAuthProtocol + `","capabilities":{},"clientInfo":{"name":"morde-cloud","version":"1"}}}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(probe)))
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth probe failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", connectorOAuthProtocol)
	response, err := client.do(request)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth probe failed")
	}
	challenge := response.Header.Get("WWW-Authenticate")
	status := response.StatusCode
	_ = response.Body.Close()
	if status >= 200 && status < 300 {
		return ConnectorOAuthStart{}, errors.New("MCP server does not require OAuth")
	}
	if status != http.StatusUnauthorized {
		return ConnectorOAuthStart{}, errors.New("MCP server does not advertise OAuth authorization")
	}
	fields := parseOAuthChallenge(challenge)
	resourceMeta, err := client.discoverProtectedResource(ctx, endpoint, fields)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP server OAuth metadata is unavailable")
	}
	issuer := endpoint.Scheme + "://" + endpoint.Host
	if len(resourceMeta.AuthorizationServers) > 0 {
		issuer = resourceMeta.AuthorizationServers[0]
	}
	authorizationServer, err := client.discoverAuthorizationServer(ctx, issuer)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP authorization server metadata is unavailable")
	}
	if len(authorizationServer.CodeChallengeMethodsSupported) > 0 && !containsOAuthValue(authorizationServer.CodeChallengeMethodsSupported, "S256") {
		return ConnectorOAuthStart{}, errors.New("MCP authorization server does not support PKCE S256")
	}
	scope := fields["scope"]
	if scope == "" && len(resourceMeta.ScopesSupported) > 0 {
		scope = strings.Join(resourceMeta.ScopesSupported, " ")
	}
	resource := resourceMeta.Resource
	if resource == "" {
		resource = connectorURL
	}
	if len(scope) > 2048 || !validOAuthResource(resource) {
		return ConnectorOAuthStart{}, errors.New("MCP protected resource metadata is invalid")
	}
	redirect := redirectURI
	var registration ConnectorOAuthRegistration
	if clientID != "" {
		method, methodErr := manualOAuthAuthMethod(authorizationServer.TokenEndpointAuthMethods, clientSecret != "")
		if methodErr != nil {
			return ConnectorOAuthStart{}, errors.New("MCP authorization server does not support the provided client credentials")
		}
		registration = ConnectorOAuthRegistration{ClientID: clientID, ClientSecret: clientSecret, AuthMethod: method}
	} else if previous != nil && previous.Issuer == authorizationServer.Issuer && previous.Registration.ClientID != "" &&
		(previous.Registration.ExpiresAt == 0 || previous.Registration.ExpiresAt > time.Now().Unix()) {
		registration = previous.Registration
	} else if authorizationServer.RegistrationEndpoint != "" {
		registration, err = client.register(ctx, authorizationServer.RegistrationEndpoint, redirect, scope)
		if err != nil {
			return ConnectorOAuthStart{}, errors.New("MCP authorization server refused client registration")
		}
	} else {
		return ConnectorOAuthStart{ClientRequired: true, DeveloperURL: authorizationServer.Issuer, RedirectURI: redirect}, nil
	}
	state, err := randomOAuthValue(32)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth state generation failed")
	}
	verifier, err := randomOAuthValue(32)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth verifier generation failed")
	}
	pkceDigest := sha256.Sum256([]byte(verifier))
	flow := connectorOAuthFlow{
		Verifier: verifier, Issuer: authorizationServer.Issuer,
		AuthorizationEndpoint: authorizationServer.AuthorizationEndpoint, TokenEndpoint: authorizationServer.TokenEndpoint,
		Resource: resource, Scope: scope, Registration: registration, RedirectURI: redirect,
	}
	payload, err := json.Marshal(flow)
	if err != nil {
		return ConnectorOAuthStart{}, errors.New("MCP OAuth flow could not be prepared")
	}
	authorize, _ := url.Parse(authorizationServer.AuthorizationEndpoint)
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", registration.ClientID)
	query.Set("redirect_uri", redirect)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(pkceDigest[:]))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	query.Set("resource", resource)
	if scope != "" {
		query.Set("scope", scope)
	}
	authorize.RawQuery = query.Encode()
	return ConnectorOAuthStart{AuthorizationURL: authorize.String(), State: state, Payload: payload, RedirectURI: redirect}, nil
}

func (client *ConnectorOAuthClient) Exchange(ctx context.Context, payload []byte, code, callbackIssuer string) (ConnectorOAuthCredentials, error) {
	if ctx == nil || code == "" || len(code) > 8192 || strings.ContainsAny(code, "\r\n\x00") {
		return ConnectorOAuthCredentials{}, errors.New("MCP OAuth callback is invalid")
	}
	var flow connectorOAuthFlow
	if err := json.Unmarshal(payload, &flow); err != nil || flow.Verifier == "" || flow.Registration.ClientID == "" ||
		!validOAuthURI(flow.TokenEndpoint, false) || !validOAuthURI(flow.RedirectURI, false) {
		return ConnectorOAuthCredentials{}, errors.New("MCP OAuth flow data is invalid")
	}
	if callbackIssuer != "" && callbackIssuer != flow.Issuer {
		return ConnectorOAuthCredentials{}, errors.New("MCP OAuth callback issuer does not match")
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {flow.RedirectURI},
		"client_id": {flow.Registration.ClientID}, "code_verifier": {flow.Verifier}, "resource": {flow.Resource},
	}
	tokens, err := client.tokenRequest(ctx, flow.TokenEndpoint, form, flow.Registration)
	if err != nil {
		return ConnectorOAuthCredentials{}, err
	}
	return ConnectorOAuthCredentials{
		Issuer: flow.Issuer, AuthorizationEndpoint: flow.AuthorizationEndpoint, TokenEndpoint: flow.TokenEndpoint,
		Resource: flow.Resource, Scope: firstNonEmpty(tokens.Scope, flow.Scope), Registration: flow.Registration,
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: expiresAt(tokens.ExpiresIn), ExpiresIn: tokens.ExpiresIn,
	}, nil
}

func (client *ConnectorOAuthClient) Refresh(ctx context.Context, credentials ConnectorOAuthCredentials) (ConnectorOAuthCredentials, error) {
	if ctx == nil || credentials.RefreshToken == "" || !validOAuthURI(credentials.TokenEndpoint, false) || credentials.Registration.ClientID == "" {
		return ConnectorOAuthCredentials{}, errors.New("MCP OAuth refresh data is invalid")
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {credentials.RefreshToken},
		"client_id": {credentials.Registration.ClientID}, "resource": {credentials.Resource},
	}
	if credentials.Scope != "" {
		form.Set("scope", credentials.Scope)
	}
	tokens, err := client.tokenRequest(ctx, credentials.TokenEndpoint, form, credentials.Registration)
	if err != nil {
		return ConnectorOAuthCredentials{}, err
	}
	credentials.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		credentials.RefreshToken = tokens.RefreshToken
	}
	credentials.ExpiresAt = expiresAt(tokens.ExpiresIn)
	credentials.ExpiresIn = tokens.ExpiresIn
	if tokens.Scope != "" {
		credentials.Scope = tokens.Scope
	}
	return credentials, nil
}

func (client *ConnectorOAuthClient) discoverProtectedResource(ctx context.Context, connector *url.URL, challenge map[string]string) (protectedResourceMetadata, error) {
	path := strings.TrimSuffix(connector.EscapedPath(), "/")
	if path == "/" {
		path = ""
	}
	origin := connector.Scheme + "://" + connector.Host
	candidates := []string{challenge["resource_metadata"], challenge["resource_metadata_uri"]}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+path, origin+"/.well-known/oauth-protected-resource")
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		resolved, err := connector.Parse(raw)
		if err != nil || !validOAuthURI(resolved.String(), false) {
			continue
		}
		var metadata protectedResourceMetadata
		if client.getJSON(ctx, resolved.String(), &metadata) == nil && len(metadata.AuthorizationServers) > 0 && len(metadata.AuthorizationServers) <= 8 {
			return metadata, nil
		}
	}
	return protectedResourceMetadata{}, errors.New("protected resource metadata not found")
}

func (client *ConnectorOAuthClient) discoverAuthorizationServer(ctx context.Context, rawIssuer string) (connectorOAuthMetadata, error) {
	issuer, err := validOAuthURIValue(rawIssuer, false)
	if err != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return connectorOAuthMetadata{}, errors.New("invalid OAuth issuer")
	}
	path := strings.TrimSuffix(issuer.EscapedPath(), "/")
	if path == "/" {
		path = ""
	}
	origin := issuer.Scheme + "://" + issuer.Host
	candidates := []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
	}
	if path != "" {
		candidates = append(candidates, origin+path+"/.well-known/openid-configuration", origin+path+"/.well-known/oauth-authorization-server")
	}
	for _, candidate := range candidates {
		var metadata connectorOAuthMetadata
		if err := client.getJSON(ctx, candidate, &metadata); err != nil || metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" {
			continue
		}
		if metadata.Issuer != "" && metadata.Issuer != issuer.String() {
			continue
		}
		metadata.Issuer = issuer.String()
		if !validOAuthURI(metadata.AuthorizationEndpoint, true) || !validOAuthURI(metadata.TokenEndpoint, false) ||
			metadata.RegistrationEndpoint != "" && !validOAuthURI(metadata.RegistrationEndpoint, false) {
			return connectorOAuthMetadata{}, errors.New("OAuth metadata contains an unsafe endpoint")
		}
		return metadata, nil
	}
	return connectorOAuthMetadata{}, errors.New("OAuth metadata not found")
}

func (client *ConnectorOAuthClient) register(ctx context.Context, endpoint, redirectURI, scope string) (ConnectorOAuthRegistration, error) {
	body := map[string]any{
		"client_name": connectorOAuthClientName, "client_uri": oauthClientURI(redirectURI), "redirect_uris": []string{redirectURI},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if scope != "" {
		body["scope"] = scope
	}
	encoded, _ := json.Marshal(body)
	response, err := client.request(ctx, http.MethodPost, endpoint, "application/json", encoded)
	if err != nil {
		return ConnectorOAuthRegistration{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		delete(body, "token_endpoint_auth_method")
		encoded, _ = json.Marshal(body)
		response, err = client.request(ctx, http.MethodPost, endpoint, "application/json", encoded)
		if err != nil {
			return ConnectorOAuthRegistration{}, err
		}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ConnectorOAuthRegistration{}, errors.New("OAuth client registration failed")
	}
	var registration oauthRegistrationResponse
	if err := decodeLimitedJSON(response.Body, &registration); err != nil || registration.ClientID == "" || len(registration.ClientID) > 2048 || len(registration.ClientSecret) > 4096 {
		return ConnectorOAuthRegistration{}, errors.New("OAuth client registration response is invalid")
	}
	method := registration.TokenEndpointAuthMethod
	if method == "" {
		// RFC 7591 defaults an omitted token_endpoint_auth_method to client_secret_basic.
		method = "client_secret_basic"
	}
	if method != "none" && method != "client_secret_post" && method != "client_secret_basic" {
		return ConnectorOAuthRegistration{}, errors.New("OAuth client uses an unsupported token authentication method")
	}
	if method != "none" && registration.ClientSecret == "" {
		return ConnectorOAuthRegistration{}, errors.New("OAuth client authentication method requires a client secret")
	}
	expires := registration.ClientIDExpiresAt
	if registration.ClientSecretExpiresAt > 0 && (expires == 0 || registration.ClientSecretExpiresAt < expires) {
		expires = registration.ClientSecretExpiresAt
	}
	return ConnectorOAuthRegistration{ClientID: registration.ClientID, ClientSecret: registration.ClientSecret, AuthMethod: method, ExpiresAt: expires}, nil
}

func (client *ConnectorOAuthClient) tokenRequest(ctx context.Context, endpoint string, form url.Values, registration ConnectorOAuthRegistration) (connectorOAuthTokenResponse, error) {
	if registration.ClientID == "" || registration.AuthMethod != "none" && registration.AuthMethod != "client_secret_post" && registration.AuthMethod != "client_secret_basic" {
		return connectorOAuthTokenResponse{}, errors.New("OAuth client credentials are invalid")
	}
	headers := make(http.Header)
	if registration.ClientSecret != "" {
		switch registration.AuthMethod {
		case "client_secret_basic":
			clientID := url.QueryEscape(registration.ClientID)
			clientSecret := url.QueryEscape(registration.ClientSecret)
			headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":"+clientSecret)))
		case "client_secret_post":
			form.Set("client_secret", registration.ClientSecret)
		}
	}
	response, err := client.requestWithHeaders(ctx, http.MethodPost, endpoint, "application/x-www-form-urlencoded", []byte(form.Encode()), headers)
	if err != nil {
		return connectorOAuthTokenResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var tokens connectorOAuthTokenResponse
	if response.StatusCode < 200 || response.StatusCode >= 300 || decodeLimitedJSON(response.Body, &tokens) != nil ||
		tokens.AccessToken == "" || len(tokens.AccessToken) > 16384 || len(tokens.RefreshToken) > 16384 || tokens.ExpiresIn < 0 || tokens.ExpiresIn > 315360000 {
		return connectorOAuthTokenResponse{}, errors.New("OAuth token endpoint rejected the request or returned an invalid token")
	}
	return tokens, nil
}

func (client *ConnectorOAuthClient) getJSON(ctx context.Context, endpoint string, value any) error {
	response, err := client.request(ctx, http.MethodGet, endpoint, "application/json", nil)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("OAuth metadata request failed")
	}
	return decodeLimitedJSON(response.Body, value)
}

func (client *ConnectorOAuthClient) request(ctx context.Context, method, endpoint, contentType string, body []byte) (*http.Response, error) {
	return client.requestWithHeaders(ctx, method, endpoint, contentType, body, nil)
}

func (client *ConnectorOAuthClient) requestWithHeaders(ctx context.Context, method, endpoint, contentType string, body []byte, headers http.Header) (*http.Response, error) {
	if ctx == nil || !validOAuthURI(endpoint, false) {
		return nil, errors.New("OAuth endpoint is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return client.do(request)
}

func (client *ConnectorOAuthClient) do(request *http.Request) (*http.Response, error) {
	if client.transport != nil {
		return (&http.Client{Transport: client.transport, Timeout: 15 * time.Second, CheckRedirect: noOAuthRedirect}).Do(request)
	}
	u, err := validOAuthURIValue(request.URL.String(), true)
	if err != nil {
		return nil, errors.New("OAuth endpoint is invalid")
	}
	httpClient := &http.Client{Transport: newConnectorTransport(strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))), Timeout: 15 * time.Second, CheckRedirect: noOAuthRedirect}
	return httpClient.Do(request)
}

func noOAuthRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func validOAuthURI(raw string, allowQuery bool) bool {
	u, err := validOAuthURIValue(raw, allowQuery)
	return err == nil && u.Scheme == "https"
}

func validOAuthURIValue(raw string, allowQuery bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" ||
		u.Port() != "" && u.Port() != "443" || !allowQuery && u.RawQuery != "" {
		return nil, errors.New("invalid OAuth URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != u.Hostname() || !validConnectorDNSName(host) || net.ParseIP(host) != nil {
		return nil, errors.New("invalid OAuth host")
	}
	u.Scheme, u.Host = "https", host
	return u, nil
}

func validOAuthResource(raw string) bool {
	if !strings.HasPrefix(raw, "https://") || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && validOAuthURI(raw, true) && u.User == nil && u.Fragment == "" && u.Opaque == ""
}

func parseOAuthChallenge(header string) map[string]string {
	result := make(map[string]string)
	for _, match := range oauthChallengeField.FindAllStringSubmatch(header, -1) {
		value := strings.ReplaceAll(strings.ReplaceAll(match[2], `\"`, `"`), `\\`, `\`)
		result[strings.ToLower(match[1])] = value
	}
	return result
}

func decodeLimitedJSON(reader io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(reader, connectorOAuthResponseLimit+1))
	if err != nil || len(data) > connectorOAuthResponseLimit || !json.Valid(data) {
		return errors.New("OAuth response is invalid or too large")
	}
	return json.Unmarshal(data, value)
}

func randomOAuthValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func oauthClientURI(redirectURI string) string {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func expiresAt(expiresIn int64) int64 {
	if expiresIn <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func containsOAuthValue(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func manualOAuthAuthMethod(supported []string, hasSecret bool) (string, error) {
	if hasSecret {
		// RFC 8414 defaults an omitted token_endpoint_auth_methods_supported to client_secret_basic.
		if len(supported) == 0 || containsOAuthValue(supported, "client_secret_basic") {
			return "client_secret_basic", nil
		}
		if containsOAuthValue(supported, "client_secret_post") {
			return "client_secret_post", nil
		}
	} else if containsOAuthValue(supported, "none") {
		return "none", nil
	}
	return "", errors.New("OAuth server does not support the provided client authentication")
}

func (start ConnectorOAuthStart) DigestState() ([32]byte, error) {
	if start.State == "" {
		return [32]byte{}, errors.New("OAuth state is missing")
	}
	return sha256.Sum256([]byte(start.State)), nil
}

func (credentials ConnectorOAuthCredentials) AuthorizationHeader(now time.Time) string {
	if credentials.AccessToken == "" || credentials.ExpiresAt > 0 && now.UnixMilli() >= credentials.ExpiresAt {
		return ""
	}
	return "Bearer " + credentials.AccessToken
}

func ValidateConnectorOAuthFlowPayload(payload []byte) error {
	var flow connectorOAuthFlow
	if len(payload) == 0 || len(payload) > connectorOAuthResponseLimit || json.Unmarshal(payload, &flow) != nil || flow.Verifier == "" ||
		flow.Registration.ClientID == "" || !validOAuthURI(flow.TokenEndpoint, false) || !validOAuthURI(flow.RedirectURI, false) {
		return fmt.Errorf("MCP OAuth flow payload is invalid")
	}
	return nil
}
