package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"morde/server/internal/domain/task"
)

func TestSelectedMCPHTTPBrokerApprovalReplayAndRevocation(t *testing.T) {
	var protocolRequests atomic.Int32
	var providerCalls atomic.Int32
	var receivedArguments []byte
	var receivedMu sync.Mutex

	provider := sdk.NewServer(&sdk.Implementation{Name: "morde-local-test-provider", Version: "1"}, nil)
	provider.AddTool(&sdk.Tool{
		Name: "lookup", Description: "Look up a synthetic record.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		providerCalls.Add(1)
		receivedMu.Lock()
		receivedArguments = append([]byte(nil), request.Params.Arguments...)
		receivedMu.Unlock()
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "synthetic record found"}}}, nil
	})
	providerHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, &sdk.StreamableHTTPOptions{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		protocolRequests.Add(1)
		providerHandler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	client := &selectedMCPStreamableHTTPTestClient{httpClient: server.Client()}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	connectorID := "k1234567"
	connector := store.values[connectorID]
	connector.EndpointURL = server.URL
	store.values[connectorID] = connector

	discovery, err := broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "mcp-http-discover-1", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"` + connectorID + `"}`,
	})
	if err != nil || discovery.Status != AgentToolSucceeded || protocolRequests.Load() == 0 || providerCalls.Load() != 0 {
		t.Fatalf("local MCP discovery = %+v err=%v protocol_requests=%d provider_calls=%d",
			discovery, err, protocolRequests.Load(), providerCalls.Load())
	}
	var catalog struct {
		ConnectorID string `json:"connector_id"`
		Tools       []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(discovery.Output), &catalog); err != nil || catalog.ConnectorID != connectorID || len(catalog.Tools) != 1 || catalog.Tools[0].Name != "lookup" {
		t.Fatalf("discovered local MCP catalog = %+v err=%v output=%q", catalog, err, discovery.Output)
	}

	arguments := `{"query":"local fixture"}`
	call := selectedMCPCallForTest(t, "mcp-http-call-1", connectorID, "lookup", arguments)
	waiting, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || waiting.Status != AgentToolNeedsUser || providerCalls.Load() != 0 || len(store.operations) != 0 ||
		store.details.PendingRequest == nil || store.details.PendingRequest.Status != task.PendingRequestPending {
		t.Fatalf("local MCP call before approval = %+v err=%v provider_calls=%d operations=%d pending=%+v",
			waiting, err, providerCalls.Load(), len(store.operations), store.details.PendingRequest)
	}

	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	completed, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || completed.Status != AgentToolSucceeded || providerCalls.Load() != 1 {
		t.Fatalf("approved local MCP call = %+v err=%v provider_calls=%d", completed, err, providerCalls.Load())
	}
	var result cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(completed.Output), &result); err != nil || result.IsError || len(result.Text) != 1 || result.Text[0] != "synthetic record found" {
		t.Fatalf("approved local MCP result = %+v err=%v output=%q", result, err, completed.Output)
	}
	receivedMu.Lock()
	gotArguments := append([]byte(nil), receivedArguments...)
	receivedMu.Unlock()
	if !bytes.Equal(gotArguments, []byte(arguments)) {
		t.Fatalf("provider arguments = %s, want %s", gotArguments, arguments)
	}

	replayed, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || replayed.Status != AgentToolSucceeded || replayed.Output != completed.Output || providerCalls.Load() != 1 {
		t.Fatalf("exact replay = %+v err=%v provider_calls=%d", replayed, err, providerCalls.Load())
	}

	revokedAt := time.Now().UTC()
	store.grants[0].RevokedAt = &revokedAt
	claimed = store.details.Task
	blocked, err := broker.Execute(context.Background(), claimed, selectedMCPCallForTest(
		t, "mcp-http-call-after-revoke", connectorID, "lookup", arguments))
	if err != nil || blocked.Status != AgentToolFailed || providerCalls.Load() != 1 || len(store.operations) != 1 {
		t.Fatalf("local MCP call after revocation = %+v err=%v provider_calls=%d operations=%d",
			blocked, err, providerCalls.Load(), len(store.operations))
	}
}

func TestGitHubMCPHTTPBrokerEnforcesReadonlyProfileAndDurableResults(t *testing.T) {
	const (
		connectorID = "k1234567"
		endpoint    = "https://api.githubcopilot.com/mcp/"
	)
	var protocolRequests atomic.Int32
	var searchCalls atomic.Int32
	var issueReadCalls atomic.Int32
	var createIssueCalls atomic.Int32
	var requestHeadersMu sync.Mutex
	var requestHeaders []http.Header

	provider := sdk.NewServer(&sdk.Implementation{Name: "morde-local-github-fixture", Version: "1"}, nil)
	provider.AddTool(&sdk.Tool{
		Name: "get_me", Description: "Read a synthetic profile.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "synthetic user"}}}, nil
	})
	provider.AddTool(&sdk.Tool{
		Name: "search_code", Description: "Search synthetic code.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		searchCalls.Add(1)
		if string(request.Params.Arguments) != `{"query":"repo:acme/demo bug"}` {
			return nil, errors.New("unexpected synthetic search arguments")
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "synthetic search result"}}}, nil
	})
	provider.AddTool(&sdk.Tool{
		Name: "issue_read", Description: "Read a synthetic issue.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"issue_number":{"type":"integer"}},"required":["owner","repo","issue_number"],"additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		issueReadCalls.Add(1)
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "synthetic issue is unavailable"}}}, nil
	})
	provider.AddTool(&sdk.Tool{
		Name: "create_issue", Description: "Write a synthetic issue.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		createIssueCalls.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "unexpected write"}}}, nil
	})
	providerHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, &sdk.StreamableHTTPOptions{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		protocolRequests.Add(1)
		requestHeadersMu.Lock()
		requestHeaders = append(requestHeaders, request.Header.Clone())
		requestHeadersMu.Unlock()
		providerHandler.ServeHTTP(writer, request)
	}))
	defer server.Close()

	client := &selectedMCPStreamableHTTPTestClient{
		httpClient:             server.Client(),
		requiredRemoteEndpoint: endpoint,
		localRouteEndpoint:     server.URL + "/mcp/",
	}
	broker, store, claimed := selectedMCPTestFixture(t, client)
	connector := store.values[connectorID]
	connector.EndpointURL = endpoint
	store.values[connectorID] = connector

	discovery, err := broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "github-http-discover", Name: cloudDSHMCPDiscoverToolName,
		Arguments: `{"connector_id":"` + connectorID + `"}`,
	})
	if err != nil || discovery.Status != AgentToolSucceeded || protocolRequests.Load() == 0 {
		t.Fatalf("local GitHub MCP discovery = %+v err=%v requests=%d", discovery, err, protocolRequests.Load())
	}
	var catalog struct {
		Tools []MCPConnectorToolDefinition `json:"tools"`
	}
	if err := json.Unmarshal([]byte(discovery.Output), &catalog); err != nil {
		t.Fatalf("decode local GitHub catalog: %v; output=%q", err, discovery.Output)
	}
	gotTools := make(map[string]bool, len(catalog.Tools))
	for _, tool := range catalog.Tools {
		gotTools[tool.Name] = true
	}
	if len(gotTools) != 3 || !gotTools["get_me"] || !gotTools["search_code"] || !gotTools["issue_read"] || gotTools["create_issue"] {
		t.Fatalf("GitHub catalog did not filter write tools: %+v", catalog.Tools)
	}

	writeCall := selectedMCPCallForTest(t, "github-http-write", connectorID, "create_issue", `{"title":"synthetic"}`)
	blockedWrite, err := broker.Execute(context.Background(), claimed, writeCall)
	if err != nil || blockedWrite.Status != AgentToolFailed || createIssueCalls.Load() != 0 || len(store.operations) != 0 {
		t.Fatalf("GitHub write tool was not rejected before remote execution: result=%+v err=%v writes=%d operations=%d",
			blockedWrite, err, createIssueCalls.Load(), len(store.operations))
	}

	searchCall := selectedMCPCallForTest(t, "github-http-search", connectorID, "search_code", `{"query":"repo:acme/demo bug"}`)
	waiting, err := broker.Execute(context.Background(), claimed, searchCall)
	if err != nil || waiting.Status != AgentToolNeedsUser || searchCalls.Load() != 0 {
		t.Fatalf("GitHub read call before exact approval = %+v err=%v calls=%d", waiting, err, searchCalls.Load())
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	claimed = store.details.Task
	searchResult, err := broker.Execute(context.Background(), claimed, searchCall)
	if err != nil || searchResult.Status != AgentToolSucceeded || searchCalls.Load() != 1 {
		t.Fatalf("approved GitHub search = %+v err=%v calls=%d", searchResult, err, searchCalls.Load())
	}
	var searchOutput cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(searchResult.Output), &searchOutput); err != nil || searchOutput.IsError || len(searchOutput.Text) != 1 || searchOutput.Text[0] != "synthetic search result" {
		t.Fatalf("GitHub search output = %+v err=%v raw=%q", searchOutput, err, searchResult.Output)
	}
	searchReplay, err := broker.Reconcile(context.Background(), claimed, searchCall, nil)
	if err != nil || searchReplay.Status != AgentToolSucceeded || searchReplay.Output != searchResult.Output || searchCalls.Load() != 1 {
		t.Fatalf("GitHub exact search replay = %+v err=%v calls=%d", searchReplay, err, searchCalls.Load())
	}

	issueCall := selectedMCPCallForTest(t, "github-http-issue", connectorID, "issue_read", `{"owner":"acme","repo":"demo","issue_number":42}`)
	issueApproval, err := broker.Execute(context.Background(), store.details.Task, issueCall)
	if err != nil || issueApproval.Status != AgentToolNeedsUser || issueReadCalls.Load() != 0 {
		t.Fatalf("GitHub issue read before approval = %+v err=%v calls=%d", issueApproval, err, issueReadCalls.Load())
	}
	resumeCloudDSHMCPApproval(t, store, task.PendingDecisionApprove)
	issueResult, err := broker.Execute(context.Background(), store.details.Task, issueCall)
	issueOperation := store.operations[agentOperationKey(issueCall.CallID)]
	if err != nil || issueResult.Status != AgentToolFailed || issueReadCalls.Load() != 1 || issueOperation.Status != task.OperationFailed {
		t.Fatalf("GitHub confirmed read failure = %+v err=%v calls=%d operation=%+v",
			issueResult, err, issueReadCalls.Load(), issueOperation)
	}
	issueReplay, err := broker.Reconcile(context.Background(), store.details.Task, issueCall, nil)
	if err != nil || issueReplay.Status != AgentToolFailed || issueReplay.Output != issueResult.Output || issueReadCalls.Load() != 1 {
		t.Fatalf("GitHub confirmed failure replay = %+v err=%v calls=%d", issueReplay, err, issueReadCalls.Load())
	}

	requestHeadersMu.Lock()
	observedHeaders := append([]http.Header(nil), requestHeaders...)
	requestHeadersMu.Unlock()
	if len(observedHeaders) == 0 {
		t.Fatal("local MCP server observed no protocol requests")
	}
	for index, headers := range observedHeaders {
		if headers.Get(githubMCPReadonlyHeader) != "true" || headers.Get(githubMCPToolsHeader) != githubMCPReadonlyTools {
			t.Fatalf("local GitHub protocol request %d missed server-enforced read-only headers: %v", index, headers)
		}
	}
}

// This adapter exercises MCP Streamable HTTP against the local fake server. It
// intentionally bypasses ConnectorClient endpoint/DNS policy; those guards have
// focused tests in the mcpconnector package.
type selectedMCPStreamableHTTPTestClient struct {
	httpClient             *http.Client
	requiredRemoteEndpoint string
	localRouteEndpoint     string
}

func (client *selectedMCPStreamableHTTPTestClient) connect(ctx context.Context, endpoint string, headers http.Header) (*sdk.ClientSession, func(), error) {
	if client.requiredRemoteEndpoint != "" {
		if endpoint != client.requiredRemoteEndpoint || client.localRouteEndpoint == "" {
			return nil, nil, errors.New("local MCP test route rejected an unexpected remote endpoint")
		}
		route, err := url.Parse(client.localRouteEndpoint)
		if err != nil || route.Scheme != "http" || route.Host == "" {
			return nil, nil, errors.New("local MCP test route is invalid")
		}
		endpoint = route.String()
	}
	httpClient := client.httpClient
	if len(headers) > 0 || client.localRouteEndpoint != "" {
		copy := *client.httpClient
		base := copy.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		copy.Transport = selectedMCPTestRoundTripper{base: base, allowedEndpoint: client.localRouteEndpoint, headers: headers.Clone()}
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		httpClient = &copy
	}
	transport := &sdk.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient,
		DisableStandaloneSSE: true, MaxRetries: -1,
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "morde-app-test-client", Version: "1"}, nil).
		Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	return session, func() { _ = session.Close() }, nil
}

type selectedMCPTestRoundTripper struct {
	base            http.RoundTripper
	allowedEndpoint string
	headers         http.Header
}

func (transport selectedMCPTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.allowedEndpoint != "" {
		allowed, err := url.Parse(transport.allowedEndpoint)
		if err != nil || request.URL.Scheme != allowed.Scheme || request.URL.Host != allowed.Host {
			return nil, errors.New("local MCP test transport blocked a non-local request")
		}
	}
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	for key, values := range transport.headers {
		cloned.Header.Del(key)
		for _, value := range values {
			cloned.Header.Add(key, value)
		}
	}
	return transport.base.RoundTrip(cloned)
}

func (client *selectedMCPStreamableHTTPTestClient) ListTools(ctx context.Context, endpoint string, headers http.Header) ([]MCPConnectorToolDefinition, error) {
	session, closeSession, err := client.connect(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	defer closeSession()
	response, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	definitions := make([]MCPConnectorToolDefinition, 0, len(response.Tools))
	for _, tool := range response.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, MCPConnectorToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: schema})
	}
	return definitions, nil
}

func (client *selectedMCPStreamableHTTPTestClient) CallTool(ctx context.Context, endpoint string, headers http.Header,
	name string, arguments json.RawMessage) (MCPConnectorToolResult, error) {
	var input any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return MCPConnectorToolResult{}, err
	}
	session, closeSession, err := client.connect(ctx, endpoint, headers)
	if err != nil {
		return MCPConnectorToolResult{}, err
	}
	defer closeSession()
	response, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		return MCPConnectorToolResult{}, err
	}
	result := MCPConnectorToolResult{IsError: response.IsError}
	if response.StructuredContent != nil {
		result.StructuredContent, err = json.Marshal(response.StructuredContent)
		if err != nil {
			return MCPConnectorToolResult{}, err
		}
	}
	for _, content := range response.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			result.Text = append(result.Text, text.Text)
		} else {
			result.UnsupportedContent = true
		}
	}
	return result, nil
}
