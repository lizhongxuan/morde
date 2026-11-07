package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHMCPDiscoverToolName = "mcp_discover"
	cloudDSHMCPCallToolName     = "mcp_call"
)

// TaskMCPSelectionStore resolves user selections only under the current Task
// and Agent lease. A grant or schema must never outlive its source revision.
type TaskMCPSelectionStore interface {
	ListSelectedTaskMCPConnectors(context.Context, task.Task) ([]string, error)
	GrantSelectedTaskMCPConnector(context.Context, task.Task, string) (TaskMCPConnectorGrant, error)
	PinSelectedTaskMCPSchema(context.Context, task.Task, string, int64, []byte, []byte) (TaskMCPConnectorGrant, error)
}

func (b *CloudDSHToolBroker) selectedMCPDefinitions(claimed task.Task) []AgentToolDefinition {
	if b.mcpClient == nil || b.mcpCredentials == nil {
		return nil
	}
	if _, ok := b.store.(TaskMCPSelectionStore); !ok {
		return nil
	}
	principal := identity.Principal{Kind: identity.Kind(claimed.Authorization.Kind), Scopes: agentScopeSnapshot(claimed.Authorization.Scopes)}
	if !principal.HasScope(identity.ScopeMCPControl) {
		return nil
	}
	return []AgentToolDefinition{
		{ModelTool: ModelTool{Name: cloudDSHMCPDiscoverToolName,
			Description: "List the user's selected MCP connectors, or discover the tools on one selected connector when needed.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"connector_id":{"type":"string"}},"additionalProperties":false}`)},
			RequiredScope: identity.ScopeMCPControl},
		{ModelTool: ModelTool{Name: cloudDSHMCPCallToolName,
			Description: "Call a previously discovered tool on a selected MCP connector using its published input schema.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"connector_id":{"type":"string"},"tool_name":{"type":"string"},"arguments":{"type":"object"}},"required":["connector_id","tool_name","arguments"],"additionalProperties":false}`)},
			RequiredScope: identity.ScopeMCPControl},
	}
}

type selectedMCPRequest struct {
	ConnectorID string          `json:"connector_id"`
	ToolName    string          `json:"tool_name"`
	Arguments   json.RawMessage `json:"arguments"`
}

func selectedMCPFailure(ctx context.Context, err error) (AgentToolResult, error) {
	if ctx.Err() != nil {
		return AgentToolResult{}, ctx.Err()
	}
	if domain.CodeOf(err) == domain.CodeUnauthenticated {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolFailed, Output: "所选 MCP 连接器暂不可用或权限已变化，请检查连接后再试。"}, nil
}

func (b *CloudDSHToolBroker) executeSelectedMCP(ctx context.Context, claimed task.Task, call ModelToolCall,
	recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	selections, ok := b.store.(TaskMCPSelectionStore)
	if !ok || b.mcpClient == nil || b.mcpCredentials == nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 连接器服务暂不可用。"}, nil
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	raw, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具参数无效。"}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request selectedMCPRequest
	if decoder.Decode(&request) != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具参数无效。"}, nil
	}
	if call.Name == cloudDSHMCPDiscoverToolName && request.ConnectorID == "" && request.ToolName == "" && len(request.Arguments) == 0 {
		ids, err := selections.ListSelectedTaskMCPConnectors(ctx, details.Task)
		if err != nil {
			return selectedMCPFailure(ctx, err)
		}
		output, err := json.Marshal(struct {
			ConnectorIDs []string `json:"connector_ids"`
		}{ConnectorIDs: ids})
		if err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolSucceeded, Output: string(output)}, nil
	}
	if !domain.ValidMCPConnectorID(request.ConnectorID) {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 连接器标识无效。"}, nil
	}
	if call.Name == cloudDSHMCPDiscoverToolName && (request.ToolName != "" || len(request.Arguments) != 0) ||
		call.Name == cloudDSHMCPCallToolName && (request.ToolName == "" || len(request.ToolName) > 128 ||
			!validUTF8(request.ToolName) || len(request.Arguments) == 0) ||
		call.Name != cloudDSHMCPDiscoverToolName && call.Name != cloudDSHMCPCallToolName {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具参数无效。"}, nil
	}
	grant, err := selections.GrantSelectedTaskMCPConnector(ctx, details.Task, request.ConnectorID)
	if err != nil {
		return selectedMCPFailure(ctx, err)
	}
	if call.Name == cloudDSHMCPDiscoverToolName {
		return b.discoverSelectedMCP(ctx, details.Task, selections, grant)
	}
	arguments, err := validateAgentToolArguments(string(request.Arguments))
	if err != nil || validateTaskMCPConnectorToolSchema(grant.ToolSchema, grant.ToolSchemaSHA256) != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "请先发现该连接器的工具，并提供有效参数。"}, nil
	}
	var tools []MCPConnectorToolDefinition
	if json.Unmarshal(grant.ToolSchema, &tools) != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具目录无效，请重新检查连接。"}, nil
	}
	var selected *MCPConnectorToolDefinition
	for index := range tools {
		if tools[index].Name == request.ToolName {
			selected = &tools[index]
			break
		}
	}
	if selected == nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "该工具不在已发现的连接器目录中。"}, nil
	}
	if err := validateSelectedMCPArguments(selected.InputSchema, arguments); err != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具参数与已发现的输入要求不符。"}, nil
	}
	alias := cloudDSHMCPToolName(request.ConnectorID, selected.Name)
	definition := AgentToolDefinition{ModelTool: ModelTool{Name: alias, Parameters: selected.InputSchema},
		RequiredScope: identity.ScopeMCPControl, MCPConnectorID: request.ConnectorID, MCPRemoteToolName: selected.Name}
	call.Name, call.Arguments = alias, string(arguments)
	result, err := b.executeMCP(ctx, details, call, arguments, definition, recovering, checkpointOutput)
	if err != nil && !recovering && (domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeNotFound ||
		domain.CodeOf(err) == domain.CodeConflict || domain.CodeOf(err) == domain.CodeUnsupported) {
		if operations, ok := b.store.(MCPConnectorOperationStore); ok {
			_, lookupErr := operations.GetMCPConnectorOperation(ctx, details.Task.OwnerID, details.Task.ID, request.ConnectorID, agentOperationKey(call.CallID))
			if domain.CodeOf(lookupErr) == domain.CodeNotFound {
				return selectedMCPFailure(ctx, err)
			}
		}
	}
	return result, err
}

func (b *CloudDSHToolBroker) discoverSelectedMCP(ctx context.Context, value task.Task, selections TaskMCPSelectionStore,
	grant TaskMCPConnectorGrant) (AgentToolResult, error) {
	connectors, ok := b.store.(MCPConnectorStore)
	if !ok {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 连接器服务暂不可用。"}, nil
	}
	if err := authorizeTaskMCPProjectConnector(ctx, b.store, value, grant); err != nil {
		return selectedMCPFailure(ctx, err)
	}
	connector, err := connectors.GetMCPConnector(ctx, grant.ConnectorOwnerID, grant.ConnectorID)
	if err != nil || !connector.Enabled || connector.Revision != grant.ConnectorRevision || connector.CredentialVersion != grant.CredentialVersion {
		return selectedMCPFailure(ctx, err)
	}
	headers, err := b.mcpCredentials.CredentialHeaders(ctx, connector)
	if err != nil {
		return selectedMCPFailure(ctx, err)
	}
	current, err := selections.GrantSelectedTaskMCPConnector(ctx, value, grant.ConnectorID)
	if err != nil || current.GrantVersion != grant.GrantVersion || current.ConnectorRevision != grant.ConnectorRevision ||
		current.CredentialVersion != grant.CredentialVersion {
		return selectedMCPFailure(ctx, err)
	}
	tools, err := b.mcpClient.ListTools(ctx, connector.EndpointURL, headers)
	if err != nil {
		return selectedMCPFailure(ctx, err)
	}
	encoded, digest, err := canonicalMCPToolSchema(tools)
	if err != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 连接器返回的工具目录无效。"}, nil
	}
	var canonicalTools []MCPConnectorToolDefinition
	if json.Unmarshal(encoded, &canonicalTools) != nil {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 连接器返回的工具目录无效。"}, nil
	}
	for _, tool := range canonicalTools {
		if _, err := resolveSelectedMCPSchema(tool.InputSchema); err != nil {
			return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具目录包含不受支持的输入要求。"}, nil
		}
	}
	if _, err := selections.PinSelectedTaskMCPSchema(ctx, value, grant.ConnectorID, grant.GrantVersion, encoded, digest); err != nil {
		return selectedMCPFailure(ctx, err)
	}
	output, err := json.Marshal(struct {
		ConnectorID string          `json:"connector_id"`
		Tools       json.RawMessage `json:"tools"`
	}{ConnectorID: grant.ConnectorID, Tools: encoded})
	if err != nil || len(output) > maxAgentToolOutputBytes {
		return AgentToolResult{Status: AgentToolFailed, Output: "MCP 工具目录过大，无法在当前任务中使用。"}, nil
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(output)}, nil
}

func validateSelectedMCPArguments(rawSchema, arguments []byte) error {
	if !validAgentJSONObject(arguments, maxAgentToolArgsBytes) {
		return errors.New("MCP arguments are invalid")
	}
	resolved, err := resolveSelectedMCPSchema(rawSchema)
	if err != nil {
		return err
	}
	var input any
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	return resolved.Validate(input)
}

func resolveSelectedMCPSchema(rawSchema []byte) (*jsonschema.Resolved, error) {
	if !validAgentJSONObject(rawSchema, maxAgentToolSchemaBytes) {
		return nil, errors.New("MCP schema is invalid")
	}
	var value any
	if err := json.Unmarshal(rawSchema, &value); err != nil || !localMCPReferences(value) {
		return nil, errors.New("MCP schema has a non-local reference")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return nil, err
	}
	return schema.Resolve(nil)
}

func localMCPReferences(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return false
				}
			}
			if !localMCPReferences(child) {
				return false
			}
		}
	case []any:
		for _, child := range item {
			if !localMCPReferences(child) {
				return false
			}
		}
	}
	return true
}
