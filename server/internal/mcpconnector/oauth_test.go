package mcpconnector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type connectorOAuthRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper connectorOAuthRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

func TestConnectorOAuthPKCEExchangeAndRefresh(t *testing.T) {
	var tokenRequests int
	client := &ConnectorOAuthClient{transport: connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
		var status int
		headers := make(http.Header)
		var body string
		switch request.URL.Host + request.URL.Path {
		case "mcp.example.test/mcp":
			status = http.StatusUnauthorized
			headers.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource", scope="read write"`)
		case "mcp.example.test/.well-known/oauth-protected-resource":
			status = http.StatusOK
			body = `{"resource":"https://mcp.example.test/mcp","authorization_servers":["https://auth.example.test/tenant"]}`
		case "auth.example.test/.well-known/oauth-authorization-server/tenant":
			status = http.StatusOK
			body = `{"issuer":"https://auth.example.test/tenant","authorization_endpoint":"https://auth.example.test/authorize","token_endpoint":"https://auth.example.test/token","registration_endpoint":"https://auth.example.test/register","code_challenge_methods_supported":["S256"]}`
		case "auth.example.test/register":
			status = http.StatusCreated
			body = `{"client_id":"registered-client","token_endpoint_auth_method":"none"}`
		case "auth.example.test/token":
			tokenRequests++
			if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Fatalf("token request content type = %q", request.Header.Get("Content-Type"))
			}
			form, err := url.ParseQuery(readOAuthRequestBody(t, request))
			if err != nil || form.Get("client_id") != "registered-client" || form.Get("resource") != "https://mcp.example.test/mcp" {
				t.Fatalf("token request form = %v, %v", form, err)
			}
			if form.Get("grant_type") == "authorization_code" {
				if form.Get("code") != "one-time-code" || form.Get("code_verifier") == "" {
					t.Fatalf("authorization-code form = %v", form)
				}
				body = `{"access_token":"access-one","refresh_token":"refresh-one","expires_in":3600,"scope":"read write"}`
			} else if form.Get("grant_type") == "refresh_token" {
				if form.Get("refresh_token") != "refresh-one" {
					t.Fatalf("refresh-token form = %v", form)
				}
				body = `{"access_token":"access-two","refresh_token":"refresh-two","expires_in":7200}`
			} else {
				t.Fatalf("unexpected grant type: %v", form)
			}
			status = http.StatusOK
		default:
			t.Fatalf("unexpected OAuth request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	start, err := client.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil)
	if err != nil || start.ClientRequired || start.AuthorizationURL == "" || start.State == "" {
		t.Fatalf("Begin() = %#v, %v", start, err)
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("response_type") != "code" || query.Get("client_id") != "registered-client" || query.Get("state") != start.State ||
		query.Get("redirect_uri") != "https://portal.example.test/api/mcp/oauth/callback" || query.Get("code_challenge_method") != "S256" ||
		query.Get("resource") != "https://mcp.example.test/mcp" || query.Get("scope") != "read write" {
		t.Fatalf("authorization query = %v", query)
	}
	var flow connectorOAuthFlow
	if err := json.Unmarshal(start.Payload, &flow); err != nil {
		t.Fatal(err)
	}
	if flow.Verifier == "" || flow.Registration.ClientID != "registered-client" {
		t.Fatalf("flow payload = %#v", flow)
	}
	credentials, err := client.Exchange(context.Background(), start.Payload, "one-time-code", "")
	if err != nil || credentials.AccessToken != "access-one" || credentials.RefreshToken != "refresh-one" || credentials.Registration.ClientID != "registered-client" {
		t.Fatalf("Exchange() = %#v, %v", credentials, err)
	}
	refreshed, err := client.Refresh(context.Background(), credentials)
	if err != nil || refreshed.AccessToken != "access-two" || refreshed.RefreshToken != "refresh-two" || refreshed.ExpiresIn != 7200 {
		t.Fatalf("Refresh() = %#v, %v", refreshed, err)
	}
	if tokenRequests != 2 {
		t.Fatalf("token requests = %d", tokenRequests)
	}
}

func TestConnectorOAuthRequiresManualClientAndRejectsUnsafeMetadata(t *testing.T) {
	manualClient := &ConnectorOAuthClient{transport: connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
		var status int
		var body string
		headers := make(http.Header)
		switch request.URL.Host + request.URL.Path {
		case "mcp.example.test/mcp":
			status = http.StatusUnauthorized
			headers.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.test/resource"`)
		case "mcp.example.test/resource":
			status, body = http.StatusOK, `{"authorization_servers":["https://auth.example.test"]}`
		case "auth.example.test/.well-known/oauth-authorization-server":
			status, body = http.StatusOK, `{"issuer":"https://auth.example.test","authorization_endpoint":"https://auth.example.test/authorize","token_endpoint":"https://auth.example.test/token"}`
		default:
			t.Fatalf("unexpected request: %s", request.URL)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	start, err := manualClient.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil)
	if err != nil || !start.ClientRequired || start.DeveloperURL != "https://auth.example.test" || start.RedirectURI == "" {
		t.Fatalf("manual-client result = %#v, %v", start, err)
	}
	if _, err := manualClient.Begin(context.Background(), "https://127.0.0.1/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil); err == nil {
		t.Fatal("accepted an IP-literal MCP endpoint")
	}
	if _, err := manualClient.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil); err != nil {
		t.Fatal(err)
	}
	unsafe := &ConnectorOAuthClient{transport: connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
		var status int
		var body string
		headers := make(http.Header)
		switch request.URL.Host + request.URL.Path {
		case "mcp.example.test/mcp":
			status = http.StatusUnauthorized
			headers.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.test/resource"`)
		case "mcp.example.test/resource":
			status, body = http.StatusOK, `{"authorization_servers":["https://auth.example.test"]}`
		case "auth.example.test/.well-known/oauth-authorization-server":
			status, body = http.StatusOK, `{"issuer":"https://auth.example.test","authorization_endpoint":"https://auth.example.test/authorize","token_endpoint":"https://127.0.0.1/token"}`
		default:
			t.Fatalf("unexpected request: %s", request.URL)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	if _, err := unsafe.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil); err == nil {
		t.Fatal("accepted a private IP OAuth token endpoint")
	}
}

func TestConnectorOAuthManualClientUsesAdvertisedAuthenticationMethod(t *testing.T) {
	tests := []struct {
		name          string
		metadataField string
		clientSecret  string
		wantMethod    string
		wantErr       bool
	}{
		{name: "omitted metadata defaults to basic", clientSecret: "client-secret", wantMethod: "client_secret_basic"},
		{name: "post only", metadataField: `,"token_endpoint_auth_methods_supported":["client_secret_post"]`, clientSecret: "client-secret", wantMethod: "client_secret_post"},
		{name: "public client", metadataField: `,"token_endpoint_auth_methods_supported":["none"]`, wantMethod: "none"},
		{name: "unsupported confidential method", metadataField: `,"token_endpoint_auth_methods_supported":["private_key_jwt"]`, clientSecret: "client-secret", wantErr: true},
		{name: "secret required when only confidential methods are supported", metadataField: `,"token_endpoint_auth_methods_supported":["client_secret_basic"]`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tokenRequests int
			client := &ConnectorOAuthClient{transport: connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
				status, body := http.StatusOK, ""
				headers := make(http.Header)
				switch request.URL.Host + request.URL.Path {
				case "mcp.example.test/mcp":
					status = http.StatusUnauthorized
					headers.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.test/resource"`)
				case "mcp.example.test/resource":
					body = `{"authorization_servers":["https://auth.example.test"]}`
				case "auth.example.test/.well-known/oauth-authorization-server":
					body = `{"issuer":"https://auth.example.test","authorization_endpoint":"https://auth.example.test/authorize","token_endpoint":"https://auth.example.test/token"` + test.metadataField + `}`
				case "auth.example.test/token":
					tokenRequests++
					form, err := url.ParseQuery(readOAuthRequestBody(t, request))
					if err != nil || form.Get("client_id") != "client-id" {
						t.Fatalf("token request form = %v, %v", form, err)
					}
					switch test.wantMethod {
					case "client_secret_basic":
						want := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape("client-id")+":"+url.QueryEscape(test.clientSecret)))
						if request.Header.Get("Authorization") != want || form.Has("client_secret") {
							t.Fatalf("Basic request authorization=%q form=%v", request.Header.Get("Authorization"), form)
						}
					case "client_secret_post":
						if request.Header.Get("Authorization") != "" || form.Get("client_secret") != test.clientSecret {
							t.Fatalf("POST request authorization=%q form=%v", request.Header.Get("Authorization"), form)
						}
					case "none":
						if request.Header.Get("Authorization") != "" || form.Has("client_secret") {
							t.Fatalf("public-client request authorization=%q form=%v", request.Header.Get("Authorization"), form)
						}
					}
					body = `{"access_token":"access-token","expires_in":3600}`
				default:
					t.Fatalf("unexpected OAuth request: %s", request.URL)
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})}

			start, err := client.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "client-id", test.clientSecret, nil)
			if test.wantErr {
				if err == nil {
					t.Fatal("Begin() accepted unsupported client authentication")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var flow connectorOAuthFlow
			if err := json.Unmarshal(start.Payload, &flow); err != nil || flow.Registration.AuthMethod != test.wantMethod {
				t.Fatalf("OAuth registration = %#v, %v", flow.Registration, err)
			}
			if _, err := client.Exchange(context.Background(), start.Payload, "authorization-code", ""); err != nil {
				t.Fatal(err)
			}
			if tokenRequests != 1 {
				t.Fatalf("token requests = %d, want 1", tokenRequests)
			}
		})
	}
}

func TestConnectorOAuthTokenRedirectIsRejected(t *testing.T) {
	client := &ConnectorOAuthClient{transport: connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host+request.URL.Path != "auth.example.test/token" {
			t.Fatalf("unexpected request: %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://elsewhere.example.test/token"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	payload, err := json.Marshal(connectorOAuthFlow{
		Verifier: "v", TokenEndpoint: "https://auth.example.test/token", RedirectURI: "https://portal.example.test/api/mcp/oauth/callback",
		Registration: ConnectorOAuthRegistration{ClientID: "id", AuthMethod: "none"}, Resource: "https://mcp.example.test/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exchange(context.Background(), payload, "code", ""); err == nil {
		t.Fatal("accepted a token endpoint redirect")
	}
}

func TestConnectorOAuthPublicHelpersValidateStateFlowAndBearerExpiry(t *testing.T) {
	client := NewConnectorOAuthClient()
	if client == nil {
		t.Fatal("NewConnectorOAuthClient() returned nil")
	}
	if _, err := client.Begin(context.Background(), "https://mcp.example.test/mcp", "http://portal.example.test/callback", "", "", nil); err == nil {
		t.Fatal("Begin() accepted an insecure callback")
	}

	start := ConnectorOAuthStart{State: "one-time-state"}
	digest, err := start.DigestState()
	if err != nil || digest != sha256.Sum256([]byte(start.State)) {
		t.Fatalf("DigestState() = %x, %v", digest, err)
	}
	if _, err := (ConnectorOAuthStart{}).DigestState(); err == nil {
		t.Fatal("DigestState() accepted an empty state")
	}

	now := time.Unix(1_800_000_000, 0)
	credentials := ConnectorOAuthCredentials{AccessToken: "access-token", ExpiresAt: now.Add(time.Minute).UnixMilli()}
	if got := credentials.AuthorizationHeader(now); got != "Bearer access-token" {
		t.Fatalf("AuthorizationHeader() = %q", got)
	}
	if got := credentials.AuthorizationHeader(now.Add(time.Minute)); got != "" {
		t.Fatalf("AuthorizationHeader() returned an expired token: %q", got)
	}
	credentials.AccessToken = ""
	if got := credentials.AuthorizationHeader(now); got != "" {
		t.Fatalf("AuthorizationHeader() returned an empty token: %q", got)
	}

	valid := json.RawMessage(`{"verifier":"v","token_endpoint":"https://auth.example.test/token","redirect_uri":"https://portal.example.test/callback","registration":{"client_id":"id"}}`)
	if err := ValidateConnectorOAuthFlowPayload(valid); err != nil {
		t.Fatalf("ValidateConnectorOAuthFlowPayload(valid) = %v", err)
	}
	for name, payload := range map[string][]byte{
		"empty":            nil,
		"malformed":        []byte(`{"`),
		"missing verifier": []byte(`{"token_endpoint":"https://auth.example.test/token","redirect_uri":"https://portal.example.test/callback","registration":{"client_id":"id"}}`),
		"missing client":   []byte(`{"verifier":"v","token_endpoint":"https://auth.example.test/token","redirect_uri":"https://portal.example.test/callback"}`),
		"unsafe endpoint":  []byte(`{"verifier":"v","token_endpoint":"http://auth.example.test/token","redirect_uri":"https://portal.example.test/callback","registration":{"client_id":"id"}}`),
		"oversized":        []byte(strings.Repeat(" ", connectorOAuthResponseLimit+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateConnectorOAuthFlowPayload(payload); err == nil {
				t.Fatal("ValidateConnectorOAuthFlowPayload() accepted invalid data")
			}
		})
	}
}

func TestConnectorOAuthRetriesRegistrationAndUsesClientSecretBasic(t *testing.T) {
	var registrationAttempts int
	var tokenRequests int
	client := NewConnectorOAuthClient()
	client.transport = connectorOAuthRoundTripper(func(request *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		headers := make(http.Header)
		switch request.URL.Host + request.URL.Path {
		case "mcp.example.test/mcp":
			status = http.StatusUnauthorized
			headers.Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource"`)
		case "mcp.example.test/.well-known/oauth-protected-resource":
			body = `{"authorization_servers":["https://auth.example.test"]}`
		case "auth.example.test/.well-known/oauth-authorization-server":
			body = `{"issuer":"https://auth.example.test","authorization_endpoint":"https://auth.example.test/authorize","token_endpoint":"https://auth.example.test/token","registration_endpoint":"https://auth.example.test/register","code_challenge_methods_supported":["S256"]}`
		case "auth.example.test/register":
			registrationAttempts++
			var registration map[string]any
			if err := json.NewDecoder(request.Body).Decode(&registration); err != nil {
				t.Fatal(err)
			}
			if registrationAttempts == 1 {
				if registration["token_endpoint_auth_method"] != "none" {
					t.Fatalf("initial registration method = %v", registration["token_endpoint_auth_method"])
				}
				status, body = http.StatusBadRequest, `{}`
			} else {
				if _, exists := registration["token_endpoint_auth_method"]; exists {
					t.Fatalf("fallback registration retained unsupported auth method: %v", registration)
				}
				status, body = http.StatusCreated, `{"client_id":"client-id","client_secret":"secret value"}`
			}
		case "auth.example.test/token":
			tokenRequests++
			wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape("client-id")+":"+url.QueryEscape("secret value")))
			if got := request.Header.Get("Authorization"); got != wantBasic {
				t.Fatalf("token authorization = %q, want client_secret_basic", got)
			}
			form, err := url.ParseQuery(readOAuthRequestBody(t, request))
			if err != nil || form.Has("client_secret") {
				t.Fatalf("token form exposed a client secret: %v, %v", form, err)
			}
			switch form.Get("grant_type") {
			case "authorization_code":
				body = `{"access_token":"access-one","refresh_token":"refresh-one","expires_in":3600,"scope":"read"}`
			case "refresh_token":
				if form.Get("refresh_token") != "refresh-one" || form.Get("scope") != "read" {
					t.Fatalf("refresh form = %v", form)
				}
				body = `{"access_token":"access-two","expires_in":7200}`
			default:
				t.Fatalf("unexpected token grant: %v", form)
			}
		default:
			t.Fatalf("unexpected OAuth request: %s", request.URL)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})

	start, err := client.Begin(context.Background(), "https://mcp.example.test/mcp", "https://portal.example.test/api/mcp/oauth/callback", "", "", nil)
	if err != nil || start.ClientRequired {
		t.Fatalf("Begin() = %#v, %v", start, err)
	}
	credentials, err := client.Exchange(context.Background(), start.Payload, "one-time-code", "")
	if err != nil || credentials.AccessToken != "access-one" || credentials.RefreshToken != "refresh-one" {
		t.Fatalf("Exchange() = %#v, %v", credentials, err)
	}
	refreshed, err := client.Refresh(context.Background(), credentials)
	if err != nil || refreshed.AccessToken != "access-two" || refreshed.RefreshToken != "refresh-one" || refreshed.Scope != "read" {
		t.Fatalf("Refresh() = %#v, %v", refreshed, err)
	}
	if registrationAttempts != 2 || tokenRequests != 2 {
		t.Fatalf("registration attempts=%d token requests=%d", registrationAttempts, tokenRequests)
	}
}

func readOAuthRequestBody(t *testing.T, request *http.Request) string {
	t.Helper()
	if request.Body == nil {
		return ""
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
