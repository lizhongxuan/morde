package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"morde/server/internal/app"
	"morde/server/internal/config"
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

type builtinSearchToolClient struct {
	client *mcpconnector.BuiltinSearchClient
}

func newBuiltinSearchToolClient(cfg config.BuiltinSearch) (*builtinSearchToolClient, error) {
	client, err := mcpconnector.NewBuiltinSearchClient(cfg.NodePath, cfg.ScriptPath, cfg.SearxngURL, cfg.MaxConcurrent)
	if err != nil {
		return nil, err
	}
	return &builtinSearchToolClient{client: client}, nil
}

func (*builtinSearchToolClient) ListTools(context.Context, string, http.Header) ([]app.MCPConnectorToolDefinition, error) {
	return nil, errors.New("built-in search tools are registered locally")
}

func (client *builtinSearchToolClient) CallTool(ctx context.Context, endpoint string, headers http.Header,
	name string, arguments json.RawMessage) (app.MCPConnectorToolResult, error) {
	if endpoint != "" || len(headers) != 0 {
		return app.MCPConnectorToolResult{}, errors.New("built-in search does not accept caller endpoints or credentials")
	}
	result, err := client.client.CallTool(ctx, name, arguments)
	if err != nil {
		return app.MCPConnectorToolResult{}, err
	}
	return app.MCPConnectorToolResult{IsError: result.IsError, UnsupportedContent: result.UnsupportedContent,
		StructuredContent: result.StructuredContent, Text: result.Text}, nil
}

var _ app.MCPConnectorToolClient = (*builtinSearchToolClient)(nil)
