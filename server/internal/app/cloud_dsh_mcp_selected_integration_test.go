package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// This adapter exercises MCP Streamable HTTP against the local fake server. It
// intentionally bypasses ConnectorClient endpoint/DNS policy; those guards have
// focused tests in the mcpconnector package.
type selectedMCPStreamableHTTPTestClient struct {
	httpClient *http.Client
}

func (client *selectedMCPStreamableHTTPTestClient) connect(ctx context.Context, endpoint string) (*sdk.ClientSession, func(), error) {
	transport := &sdk.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: client.httpClient,
		DisableStandaloneSSE: true, MaxRetries: -1,
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "morde-app-test-client", Version: "1"}, nil).
		Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	return session, func() { _ = session.Close() }, nil
}

func (client *selectedMCPStreamableHTTPTestClient) ListTools(ctx context.Context, endpoint string, _ http.Header) ([]MCPConnectorToolDefinition, error) {
	session, closeSession, err := client.connect(ctx, endpoint)
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

func (client *selectedMCPStreamableHTTPTestClient) CallTool(ctx context.Context, endpoint string, _ http.Header,
	name string, arguments json.RawMessage) (MCPConnectorToolResult, error) {
	var input any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return MCPConnectorToolResult{}, err
	}
	session, closeSession, err := client.connect(ctx, endpoint)
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
