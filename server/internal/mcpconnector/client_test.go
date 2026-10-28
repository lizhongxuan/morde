package mcpconnector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConnectorClientListsAndCallsToolsWithoutLeakingHeaders(t *testing.T) {
	var mu sync.Mutex
	var authHeaders []string
	server := sdk.NewServer(&sdk.Implementation{Name: "connector-test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{
		Name: "search", Description: "Search records",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{
			Content:           []sdk.Content{&sdk.TextContent{Text: "done"}},
			StructuredContent: map[string]any{"count": 1},
		}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{})
	wrapped := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, request.Header.Get("Authorization"))
		mu.Unlock()
		handler.ServeHTTP(writer, request)
	})
	client := &ConnectorClient{transport: inProcessConnectorTransport{handler: wrapped}}
	endpoint := "https://connector.example.test/mcp"
	headers := http.Header{"Authorization": []string{"Bearer hidden-test-token"}}

	tools, err := client.ListTools(context.Background(), endpoint, headers)
	if err != nil || len(tools) != 1 || tools[0].Name != "search" || !jsonObject(tools[0].InputSchema) {
		t.Fatalf("ListTools() = %#v, %v", tools, err)
	}
	result, err := client.CallTool(context.Background(), endpoint, headers, "search", json.RawMessage(`{"query":"hello"}`))
	if err != nil || result.IsError || len(result.Text) != 1 || result.Text[0] != "done" || string(result.StructuredContent) != `{"count":1}` {
		t.Fatalf("CallTool() = %#v, %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) < 4 {
		t.Fatalf("expected auth on each MCP request, got %d requests", len(authHeaders))
	}
	for _, value := range authHeaders {
		if value != "Bearer hidden-test-token" {
			t.Fatalf("credential header was not scoped to the MCP request: %q", value)
		}
	}
}

func TestConnectorClientRejectsUnsafeEndpointAndCredentialHeaders(t *testing.T) {
	for _, endpoint := range []string{
		"http://connector.example.test/mcp", "https://127.0.0.1/mcp", "https://user:pass@connector.example.test/mcp",
		"https://connector.example.test/mcp?token=x", "https://connector.example.test/mcp#fragment",
		"https://connector.example.test:8443/mcp", "https://bad.-label.example/mcp", "https://double..label.example/mcp",
	} {
		if _, err := validConnectorEndpoint(endpoint); err == nil {
			t.Errorf("validConnectorEndpoint(%q) succeeded", endpoint)
		}
	}
	for _, headers := range []http.Header{
		{"Cookie": []string{"session=secret"}},
		{"Authorization": []string{"Bearer a\r\nb"}},
		{"Host": []string{"other.example.test"}},
		{"Authorization": []string{"one", "two"}},
		{"Bad\nInjected": []string{"value"}},
	} {
		if err := validateConnectorHeaders(headers); err == nil {
			t.Errorf("validateConnectorHeaders(%v) succeeded", headers)
		}
	}
}

func TestConnectorClientConstructorAndRejectsInvalidRequests(t *testing.T) {
	client := NewConnectorClient()
	if client == nil {
		t.Fatal("NewConnectorClient() returned nil")
	}
	if _, err := client.ListTools(context.Background(), "http://connector.example.test/mcp", nil); err == nil {
		t.Fatal("ListTools() accepted an unsafe endpoint")
	}
	for _, test := range []struct {
		name      string
		tool      string
		arguments json.RawMessage
	}{
		{name: "missing tool name", arguments: json.RawMessage(`{}`)},
		{name: "non-object arguments", tool: "search", arguments: json.RawMessage(`[]`)},
		{name: "malformed arguments", tool: "search", arguments: json.RawMessage(`{"`)},
		{name: "oversized arguments", tool: "search", arguments: json.RawMessage(`{"value":"` + strings.Repeat("x", 32<<10) + `"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.CallTool(context.Background(), "https://connector.example.test/mcp", nil, test.tool, test.arguments); err == nil {
				t.Fatal("CallTool() accepted invalid input")
			}
		})
	}
}

func TestConnectorClientDefaultTransportRejectsUnapprovedDialWithoutDNS(t *testing.T) {
	transport, ok := newConnectorTransport("connector.example.test").(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatal("newConnectorTransport() did not create a dialer")
	}
	for _, address := range []string{"missing-port", "other.example.test:443", "connector.example.test:8443"} {
		if _, err := transport.DialContext(context.Background(), "tcp", address); err == nil {
			t.Errorf("default connector dialer accepted %q", address)
		}
	}
	local, ok := newConnectorTransport("localhost").(*http.Transport)
	if !ok || local.DialContext == nil {
		t.Fatal("newConnectorTransport() did not create a local-resolving dialer")
	}
	if _, err := local.DialContext(context.Background(), "tcp", "localhost:443"); err == nil {
		t.Fatal("default connector dialer accepted loopback DNS results")
	}
}

func TestConnectorClientRejectsToolListingOverLimit(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "connector-test", Version: "1"}, nil)
	for i := 0; i <= connectorToolLimit; i++ {
		name := "tool-" + strconv.Itoa(i)
		server.AddTool(&sdk.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{})
	client := &ConnectorClient{transport: inProcessConnectorTransport{handler: handler}}
	if _, err := client.ListTools(context.Background(), "https://connector.example.test/mcp", nil); err == nil {
		t.Fatal("ListTools() accepted more than its tool limit")
	}
}

func TestConnectorLimitedBodyStopsBeforeReturningOverLimitBytes(t *testing.T) {
	body := connectorLimitedBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", connectorResponseLimit+1)))}
	value, err := io.ReadAll(&body)
	if err == nil || len(value) == 0 || len(value) > connectorResponseLimit {
		t.Fatalf("limited body read length=%d err=%v, want a capped read and error", len(value), err)
	}
}

func TestPublicConnectorIPRejectsNonPublicRanges(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "192.0.2.1", "198.51.100.1", "224.0.0.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::a00:1"} {
		if address := netip.MustParseAddr(raw); publicConnectorIP(address) {
			t.Errorf("publicConnectorIP(%s) = true", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if address := netip.MustParseAddr(raw); !publicConnectorIP(address) {
			t.Errorf("publicConnectorIP(%s) = false", raw)
		}
	}
}

func TestConnectorClientCapsResponsesAndDoesNotFollowRedirects(t *testing.T) {
	for _, response := range []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), ContentLength: connectorResponseLimit + 1},
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://127.0.0.1/secret"}}, Body: io.NopCloser(strings.NewReader("")), ContentLength: 0},
	} {
		transport := &singleResponseConnectorTransport{response: response}
		client := &ConnectorClient{transport: transport}
		if _, err := client.ListTools(context.Background(), "https://connector.example.test/mcp", nil); err == nil {
			t.Fatal("ListTools() accepted an oversized or redirected response")
		}
		if transport.calls == 0 {
			t.Fatal("no MCP request was made")
		}
		for _, target := range transport.targets {
			if target != "https://connector.example.test/mcp" {
				t.Fatalf("MCP client followed a redirect to %q", target)
			}
		}
	}
}

func TestConnectorClientRecordsUnsupportedMCPResultContentWithoutLosingCallOutcome(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "connector-test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{MIMEType: "image/png", Data: []byte("image")}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{})
	client := &ConnectorClient{transport: inProcessConnectorTransport{handler: handler}}
	result, err := client.CallTool(context.Background(), "https://connector.example.test/mcp", nil, "search", json.RawMessage(`{}`))
	if err != nil || !result.UnsupportedContent {
		t.Fatalf("CallTool() = %#v, %v, want confirmed result marked unsupported", result, err)
	}
}

func TestConnectorClientCapsCombinedMCPResult(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "connector-test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: strings.Repeat("a", 40<<10)},
			&sdk.TextContent{Text: strings.Repeat("b", 40<<10)},
		}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{})
	client := &ConnectorClient{transport: inProcessConnectorTransport{handler: handler}}
	result, err := client.CallTool(context.Background(), "https://connector.example.test/mcp", nil, "search", json.RawMessage(`{}`))
	if err != nil || !result.UnsupportedContent || len(result.Text) != 0 {
		t.Fatalf("CallTool() = %#v, %v, want confirmed bounded unsupported result", result, err)
	}
}

func TestConnectorClientRejectsUnknownToolAndOversizedStructuredResult(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "connector-test", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "large-result", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: map[string]any{"value": strings.Repeat("x", 64<<10)}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{})
	client := &ConnectorClient{transport: inProcessConnectorTransport{handler: handler}}
	if _, err := client.CallTool(context.Background(), "https://connector.example.test/mcp", nil, "missing", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool() accepted an unknown tool")
	}
	result, err := client.CallTool(context.Background(), "https://connector.example.test/mcp", nil, "large-result", json.RawMessage(`{}`))
	if err != nil || !result.UnsupportedContent || len(result.Text) != 0 {
		t.Fatalf("CallTool() = %#v, %v, want confirmed unsupported structured result", result, err)
	}
}

type inProcessConnectorTransport struct{ handler http.Handler }

func (transport inProcessConnectorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

type singleResponseConnectorTransport struct {
	response *http.Response
	calls    int
	targets  []string
}

func (transport *singleResponseConnectorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	transport.targets = append(transport.targets, request.URL.String())
	return transport.response, nil
}
