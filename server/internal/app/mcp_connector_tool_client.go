package app

import (
	"context"
	"encoding/json"
	"net/http"
)

type MCPConnectorToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type MCPConnectorToolResult struct {
	IsError            bool
	UnsupportedContent bool
	StructuredContent  json.RawMessage
	Text               []string
}

// MCPConnectorToolClient is the app port for the server-side Streamable HTTP adapter.
type MCPConnectorToolClient interface {
	ListTools(context.Context, string, http.Header) ([]MCPConnectorToolDefinition, error)
	CallTool(context.Context, string, http.Header, string, json.RawMessage) (MCPConnectorToolResult, error)
}

type MCPConnectorCredentialProvider interface {
	CredentialHeaders(context.Context, MCPConnectorRecord) (http.Header, error)
}
