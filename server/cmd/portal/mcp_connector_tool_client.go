package main

import (
	"context"
	"encoding/json"
	"net/http"

	"morde/server/internal/app"
	"morde/server/internal/mcpconnector"
)

type mcpConnectorToolClient struct{ client *mcpconnector.ConnectorClient }

func newMCPConnectorToolClient() *mcpConnectorToolClient {
	return &mcpConnectorToolClient{client: mcpconnector.NewConnectorClient()}
}

func (client *mcpConnectorToolClient) ListTools(ctx context.Context, endpoint string, headers http.Header) ([]app.MCPConnectorToolDefinition, error) {
	tools, err := client.client.ListTools(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	result := make([]app.MCPConnectorToolDefinition, 0, len(tools))
	for _, tool := range tools {
		result = append(result, app.MCPConnectorToolDefinition{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return result, nil
}

func (client *mcpConnectorToolClient) CallTool(ctx context.Context, endpoint string, headers http.Header, name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	result, err := client.client.CallTool(ctx, endpoint, headers, name, arguments)
	if err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	return app.MCPConnectorToolResult{
		IsError: result.IsError, UnsupportedContent: result.UnsupportedContent,
		StructuredContent: result.StructuredContent, Text: result.Text,
	}, nil
}

var _ app.MCPConnectorToolClient = (*mcpConnectorToolClient)(nil)
