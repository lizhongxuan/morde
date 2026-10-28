package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHMCPToolPrefix      = "mcp_"
	maxCloudDSHToolDefinitions = 64
)

type cloudDSHMCPOutput struct {
	IsError            bool            `json:"is_error"`
	UnsupportedContent bool            `json:"unsupported_content,omitempty"`
	StructuredContent  json.RawMessage `json:"structured_content,omitempty"`
	Text               []string        `json:"text"`
}

func (b *CloudDSHToolBroker) currentServerTask(ctx context.Context, claimed task.Task) (TaskDetails, error) {
	details, err := (&Agent{store: b.store}).refreshTask(ctx, claimed)
	if err != nil {
		return TaskDetails{}, err
	}
	if details.Task.ExecutionLocation != "server" || details.Task.Status != task.StatusRunning {
		return TaskDetails{}, domain.NewError(domain.CodeInvalidState, "MCP connector tools require a running server task")
	}
	return details, nil
}

func (b *CloudDSHToolBroker) mcpDefinitions(ctx context.Context, claimed task.Task) ([]AgentToolDefinition, error) {
	if b.mcpClient == nil || b.mcpCredentials == nil {
		return nil, nil
	}
	grants, ok := b.store.(TaskMCPConnectorGrantStore)
	if !ok {
		return nil, domain.NewError(domain.CodeUnsupported, "task MCP connector grants are unavailable")
	}
	connectors, ok := b.store.(MCPConnectorStore)
	if !ok {
		return nil, domain.NewError(domain.CodeUnsupported, "MCP connector storage is unavailable")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return nil, err
	}
	grantsForTask, err := grants.ListTaskMCPConnectorGrants(ctx, details.Task.OwnerID, details.Task.ID)
	if err != nil {
		return nil, err
	}
	principal := identity.Principal{Kind: identity.Kind(details.Task.Authorization.Kind), Scopes: agentScopeSnapshot(details.Task.Authorization.Scopes)}
	result := make([]AgentToolDefinition, 0)
	for _, grant := range grantsForTask {
		if grant.RevokedAt != nil {
			continue
		}
		if !principal.HasScope(identity.ScopeMCPControl) {
			return nil, domain.NewError(domain.CodeForbidden, "task authorization does not allow MCP connector tools")
		}
		if grant.TaskID != details.Task.ID || grant.OwnerID != details.Task.OwnerID || !domain.ValidMCPConnectorID(grant.ConnectorID) ||
			grant.ConnectorRevision < 1 || grant.CredentialVersion < 0 || grant.GrantVersion < 1 {
			return nil, domain.NewError(domain.CodeInternal, "stored MCP connector grant is invalid")
		}
		connector, err := connectors.GetMCPConnector(ctx, grant.OwnerID, grant.ConnectorID)
		if err != nil {
			return nil, err
		}
		if !connector.Enabled || connector.Revision != grant.ConnectorRevision || connector.CredentialVersion != grant.CredentialVersion {
			return nil, domain.NewError(domain.CodeConflict, "MCP connector changed after this task was authorized")
		}
		headers, err := b.mcpCredentials.CredentialHeaders(ctx, connector)
		if err != nil {
			return nil, err
		}
		remoteTools, err := b.mcpClient.ListTools(ctx, connector.EndpointURL, headers)
		if err != nil {
			return nil, domain.NewError(domain.CodeUpstreamError, "MCP connector tools could not be loaded")
		}
		encoded, digest, err := canonicalMCPToolSchema(remoteTools)
		if err != nil {
			return nil, domain.NewError(domain.CodeUpstreamError, "MCP connector returned an invalid tool catalog")
		}
		if len(grant.ToolSchema) == 0 {
			grant, err = grants.SetTaskMCPConnectorToolSchema(ctx, grant.OwnerID, grant.TaskID, grant.ConnectorID, grant.GrantVersion, encoded, digest)
			if err != nil {
				return nil, err
			}
		} else if err := validateTaskMCPConnectorToolSchema(grant.ToolSchema, grant.ToolSchemaSHA256); err != nil ||
			!bytes.Equal(grant.ToolSchema, encoded) || !bytes.Equal(grant.ToolSchemaSHA256, digest) {
			return nil, domain.NewError(domain.CodeConflict, "MCP connector tool catalog changed after this task was authorized")
		}
		for _, tool := range remoteTools {
			alias := cloudDSHMCPToolName(grant.ConnectorID, tool.Name)
			result = append(result, AgentToolDefinition{
				ModelTool: ModelTool{
					Name: alias, Description: cloudDSHMCPToolDescription(connector.Name, tool), Parameters: append(json.RawMessage(nil), tool.InputSchema...),
				},
				RequiredScope: identity.ScopeMCPControl, MCPConnectorID: grant.ConnectorID, MCPRemoteToolName: tool.Name,
			})
		}
	}
	return result, nil
}

func canonicalMCPToolSchema(tools []MCPConnectorToolDefinition) ([]byte, []byte, error) {
	if len(tools) > 256 {
		return nil, nil, errors.New("MCP tool catalog exceeds its limit")
	}
	tools = append([]MCPConnectorToolDefinition(nil), tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for index := range tools {
		tool := &tools[index]
		if tool.Name == "" || len(tool.Name) > 128 || !validUTF8(tool.Name) || len(tool.Description) > 4096 || !validUTF8(tool.Description) ||
			!validAgentJSONObject(tool.InputSchema, 64<<10) || index > 0 && tools[index-1].Name == tool.Name {
			return nil, nil, errors.New("MCP tool definition is invalid")
		}
		decoder := json.NewDecoder(bytes.NewReader(tool.InputSchema))
		decoder.UseNumber()
		var schema any
		if decoder.Decode(&schema) != nil {
			return nil, nil, errors.New("MCP tool schema is invalid")
		}
		canonical, err := json.Marshal(schema)
		if err != nil {
			return nil, nil, err
		}
		tool.InputSchema = canonical
	}
	encoded, err := json.Marshal(tools)
	if err != nil || len(encoded) == 0 || len(encoded) > maxTaskMCPConnectorToolSchema {
		return nil, nil, errors.New("MCP tool catalog exceeds its storage limit")
	}
	digest := sha256.Sum256(encoded)
	return encoded, digest[:], nil
}

func cloudDSHMCPToolName(connectorID, remoteName string) string {
	digest := sha256.Sum256([]byte(remoteName))
	return cloudDSHMCPToolPrefix + connectorID + "_" + hex.EncodeToString(digest[:6])
}

func cloudDSHMCPToolDescription(connectorName string, tool MCPConnectorToolDefinition) string {
	description := "MCP connector " + connectorName + ": " + tool.Name
	if strings.TrimSpace(tool.Description) != "" {
		description += ". " + strings.TrimSpace(tool.Description)
	}
	if len(description) > 4096 {
		description = description[:4096]
		for !validUTF8(description) {
			description = description[:len(description)-1]
		}
	}
	return description
}

func (b *CloudDSHToolBroker) executeMCP(ctx context.Context, details TaskDetails, call ModelToolCall, arguments json.RawMessage,
	definition AgentToolDefinition, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	current := details.Task
	grantStore, grantsOK := b.store.(TaskMCPConnectorGrantStore)
	connectorStore, connectorsOK := b.store.(MCPConnectorStore)
	operationStore, operationsOK := b.store.(MCPConnectorOperationStore)
	if !grantsOK || !connectorsOK || !operationsOK {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "MCP connector execution storage is unavailable")
	}
	principal := identity.Principal{Kind: identity.Kind(current.Authorization.Kind), Scopes: agentScopeSnapshot(current.Authorization.Scopes)}
	if !principal.HasScope(identity.ScopeMCPControl) || definition.RequiredScope != identity.ScopeMCPControl ||
		!domain.ValidMCPConnectorID(definition.MCPConnectorID) || definition.MCPRemoteToolName == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "task authorization does not allow this MCP connector tool")
	}
	grants, err := grantStore.ListTaskMCPConnectorGrants(ctx, current.OwnerID, current.ID)
	if err != nil {
		return AgentToolResult{}, err
	}
	var grant *TaskMCPConnectorGrant
	for index := range grants {
		if grants[index].ConnectorID == definition.MCPConnectorID && grants[index].RevokedAt == nil {
			grant = &grants[index]
			break
		}
	}
	if grant == nil || grant.TaskID != current.ID || grant.OwnerID != current.OwnerID || grant.GrantVersion < 1 ||
		!bytes.Equal(grant.ToolSchemaSHA256, sha256Bytes(grant.ToolSchema)) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "MCP connector was not granted to this task")
	}
	connector, err := connectorStore.GetMCPConnector(ctx, current.OwnerID, grant.ConnectorID)
	if err != nil {
		return AgentToolResult{}, err
	}
	if !connector.Enabled || connector.Revision != grant.ConnectorRevision || connector.CredentialVersion != grant.CredentialVersion {
		return AgentToolResult{}, domain.NewError(domain.CodeConflict, "MCP connector changed after this task was authorized")
	}
	if _, err := definitionForPinnedMCPTool(grant.ToolSchema, grant.ConnectorID, call.Name, definition.MCPRemoteToolName); err != nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "MCP connector tool is not in this task's authorized catalog")
	}
	arguments, err = validateAgentToolArguments(string(arguments))
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	key := agentOperationKey(call.CallID)
	operation, lookupErr := operationStore.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, grant.ConnectorID, key)
	hasOperation := lookupErr == nil
	if lookupErr != nil && domain.CodeOf(lookupErr) != domain.CodeNotFound {
		return AgentToolResult{}, lookupErr
	}
	if hasOperation && (operation.TaskID != current.ID || operation.OwnerID != current.OwnerID || operation.ConnectorID != grant.ConnectorID ||
		operation.Key != key || operation.ToolName != call.Name || !bytes.Equal(operation.RequestHash, hash[:]) ||
		operation.ConnectorRevision != grant.ConnectorRevision || operation.CredentialVersion != grant.CredentialVersion || operation.GrantVersion != grant.GrantVersion) {
		step, stepErr := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepUnknown,
			"Review MCP connector recovery", "The prior MCP connector operation does not match this exact request.")
		if stepErr != nil {
			return AgentToolResult{}, stepErr
		}
		return b.pauseMCPUnknown(ctx, current, step, current.Usage, key, "The prior MCP connector operation does not match this exact request.")
	}
	if !hasOperation && recovering && checkpointOutput != nil {
		step, stepErr := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepUnknown,
			"Review MCP connector recovery", "A checkpointed MCP result has no matching operation ledger entry.")
		if stepErr != nil {
			return AgentToolResult{}, stepErr
		}
		return b.pauseMCPUnknown(ctx, current, step, current.Usage, key, "A checkpointed MCP result has no matching operation ledger entry.")
	}
	usage := cloneTaskUsage(current.Usage)
	if err := checkRuntimeNonToolBudget(current); err != nil {
		return AgentToolResult{}, err
	}
	if usage.ToolCalls < 0 || usage.ToolCalls > current.Budget.ToolCalls {
		return AgentToolResult{}, domain.NewError(domain.CodeQuotaExceeded, "task execution budget is exhausted")
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Run cloud dsh MCP tool: "+call.Name, "Checking the task grant, connector version, and call ledger.")
	if err != nil {
		return AgentToolResult{}, err
	}
	if !hasOperation && !recovering {
		if usage.ToolCalls >= current.Budget.ToolCalls {
			step.Status, step.Summary = task.StepWaitingUser, "The task tool call budget is exhausted."
			if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
				return AgentToolResult{}, err
			}
			return AgentToolResult{Status: AgentToolNeedsUser}, nil
		}
		usage.ToolCalls++
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "operation_key": key, "phase": "prepare", "scope": identity.ScopeMCPControl,
		}); err != nil {
			return AgentToolResult{}, err
		}
		current.Usage = usage
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate MCP connector operation ID: %w", err)
	}
	operation, replayed, err := operationStore.PrepareMCPConnectorOperation(ctx, MCPConnectorOperation{
		ID: operationID, TaskID: current.ID, OwnerID: current.OwnerID, ConnectorID: grant.ConnectorID, Key: key,
		ToolName: call.Name, ConnectorRevision: grant.ConnectorRevision, CredentialVersion: grant.CredentialVersion,
		GrantVersion: grant.GrantVersion, LeaseVersion: current.Lease.Version, RequestHash: hash[:], Status: task.OperationPrepared,
	}, current.Lease.Owner)
	if err != nil {
		return AgentToolResult{}, err
	}
	if recovering && hasOperation && !replayed {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector operation disappeared during recovery.")
	}
	if operation.ID == "" || operation.TaskID != current.ID || operation.ConnectorID != grant.ConnectorID || operation.Key != key ||
		operation.Status == task.OperationPrepared && operation.LeaseVersion != current.Lease.Version || !bytes.Equal(operation.RequestHash, hash[:]) {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The stored MCP connector operation does not match this request.")
	}
	if replayed {
		switch operation.Status {
		case task.OperationSucceeded, task.OperationFailed:
			result, err := cloudDSHMCPResult(operation.Status, operation.Result)
			if err != nil {
				return b.pauseMCPUnknown(ctx, current, step, usage, key, "The stored MCP connector result is invalid.")
			}
			if recovering && checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
				return b.pauseMCPUnknown(ctx, current, step, usage, key, "The checkpointed MCP result differs from the exact ledger result.")
			}
			if result.Status == AgentToolNeedsUser {
				return b.pauseMCPUnsupportedResult(ctx, current, step, usage, key, operation.Status)
			}
			return b.commitMCPResult(ctx, current, step, usage, call.Name, result)
		case task.OperationUnknown, task.OperationCancelled:
			return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector operation cannot be replayed safely.")
		case task.OperationDispatched:
			if err := b.markMCPUnknown(ctx, current, operation, "MCP call was interrupted before its result was committed."); err != nil {
				return AgentToolResult{}, err
			}
			return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result is unknown; the action will not be replayed.")
		case task.OperationPrepared:
		default:
			return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector operation has an unsupported ledger state.")
		}
	}
	if recovering && checkpointOutput != nil {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The checkpointed MCP result is not backed by a terminal ledger entry.")
	}
	if operation.Status != task.OperationPrepared {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector operation is not safe to dispatch.")
	}
	headers, err := b.mcpCredentials.CredentialHeaders(ctx, connector)
	if err != nil {
		return AgentToolResult{}, err
	}
	if _, err := operationStore.MarkMCPConnectorOperationDispatched(ctx, operation, current.Lease.Owner); err != nil {
		if latest, getErr := operationStore.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, grant.ConnectorID, key); getErr == nil &&
			(latest.Status == task.OperationCancelled || latest.Status == task.OperationUnknown) {
			return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant changed before dispatch.")
		}
		return AgentToolResult{}, err
	}
	remoteResult, callErr := b.mcpClient.CallTool(ctx, connector.EndpointURL, headers, definition.MCPRemoteToolName, arguments)
	if callErr != nil {
		if latest, getErr := operationStore.GetMCPConnectorOperation(context.WithoutCancel(ctx), current.OwnerID, current.ID, grant.ConnectorID, key); getErr == nil &&
			latest.CancelRequested {
			if markErr := b.markMCPUnknown(ctx, current, operation, "MCP call ended after connector revocation."); markErr != nil {
				return AgentToolResult{}, markErr
			}
			return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant or task was revoked while the MCP call was running.")
		}
		if markErr := b.markMCPUnknown(ctx, current, operation, "MCP call outcome could not be confirmed."); markErr != nil {
			return AgentToolResult{}, errors.Join(callErr, markErr)
		}
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result is unknown; the action will not be replayed.")
	}
	output, err := encodeCloudDSHMCPOutput(remoteResult)
	if err != nil {
		if markErr := b.markMCPUnknown(ctx, current, operation, "MCP result exceeded the durable output limit."); markErr != nil {
			return AgentToolResult{}, errors.Join(err, markErr)
		}
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result exceeded its durable output limit.")
	}
	operationStatus := task.OperationSucceeded
	resultStatus := AgentToolSucceeded
	if remoteResult.IsError {
		operationStatus, resultStatus = task.OperationFailed, AgentToolFailed
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	completed, completeErr := operationStore.CompleteMCPConnectorOperation(persistCtx, operation, current.Lease.Owner, operationStatus, output)
	cancel()
	if completeErr != nil {
		latest, getErr := operationStore.GetMCPConnectorOperation(context.WithoutCancel(ctx), current.OwnerID, current.ID, grant.ConnectorID, key)
		if getErr == nil && latest.CancelRequested {
			if markErr := b.markMCPUnknown(ctx, current, operation, "MCP result was discarded after connector revocation."); markErr != nil {
				return AgentToolResult{}, errors.Join(completeErr, markErr)
			}
			return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant or task was revoked while the MCP call was running.")
		}
		return AgentToolResult{}, completeErr
	}
	if completed.Status != operationStatus || completed.Result != output {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result could not be verified in the operation ledger.")
	}
	if remoteResult.UnsupportedContent {
		return b.pauseMCPUnsupportedResult(ctx, current, step, usage, key, operationStatus)
	}
	if ctx.Err() != nil {
		return AgentToolResult{Status: AgentToolUnknown}, ctx.Err()
	}
	return b.commitMCPResult(ctx, current, step, usage, call.Name, AgentToolResult{Status: resultStatus, Output: output})
}

func definitionForPinnedMCPTool(raw []byte, connectorID, alias, remoteName string) (MCPConnectorToolDefinition, error) {
	if len(raw) == 0 || len(raw) > maxTaskMCPConnectorToolSchema {
		return MCPConnectorToolDefinition{}, errors.New("MCP tool catalog is missing")
	}
	var tools []MCPConnectorToolDefinition
	if err := json.Unmarshal(raw, &tools); err != nil {
		return MCPConnectorToolDefinition{}, errors.New("MCP tool catalog is invalid")
	}
	for _, tool := range tools {
		if cloudDSHMCPToolName(connectorID, tool.Name) == alias && tool.Name == remoteName {
			return tool, nil
		}
	}
	return MCPConnectorToolDefinition{}, errors.New("MCP tool is not pinned")
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func encodeCloudDSHMCPOutput(result MCPConnectorToolResult) (string, error) {
	if len(result.StructuredContent) > 64<<10 || len(result.StructuredContent) != 0 && !validCloudDSHMCPJSON(result.StructuredContent) || len(result.Text) > 256 {
		return "", errors.New("MCP tool output is invalid")
	}
	total := 0
	for _, text := range result.Text {
		if len(text) > 64<<10 || !validUTF8(text) {
			return "", errors.New("MCP tool text output is invalid")
		}
		total += len(text)
	}
	total += len(result.StructuredContent)
	if total > maxAgentToolOutputBytes-1024 {
		return "", errors.New("MCP tool output exceeds its durable limit")
	}
	encoded, err := json.Marshal(cloudDSHMCPOutput{
		IsError: result.IsError, UnsupportedContent: result.UnsupportedContent,
		StructuredContent: append(json.RawMessage(nil), result.StructuredContent...), Text: append([]string(nil), result.Text...),
	})
	if err != nil || len(encoded) > maxAgentToolOutputBytes {
		return "", errors.New("MCP tool output exceeds its durable limit")
	}
	return string(encoded), nil
}

func cloudDSHMCPResult(status task.OperationStatus, output string) (AgentToolResult, error) {
	if (status != task.OperationSucceeded && status != task.OperationFailed) || len(output) == 0 || len(output) > maxAgentToolOutputBytes {
		return AgentToolResult{}, errors.New("MCP operation result is not terminal")
	}
	var value cloudDSHMCPOutput
	if err := json.Unmarshal([]byte(output), &value); err != nil || len(value.Text) > 256 ||
		len(value.StructuredContent) > 64<<10 || len(value.StructuredContent) != 0 && !validCloudDSHMCPJSON(value.StructuredContent) {
		return AgentToolResult{}, errors.New("MCP operation result is invalid")
	}
	for _, text := range value.Text {
		if len(text) > 64<<10 || !validUTF8(text) {
			return AgentToolResult{}, errors.New("MCP operation result is invalid")
		}
	}
	if (status == task.OperationFailed) != value.IsError {
		return AgentToolResult{}, errors.New("MCP operation result status is inconsistent")
	}
	if value.UnsupportedContent {
		return AgentToolResult{Status: AgentToolNeedsUser, Output: output}, nil
	}
	toolStatus := AgentToolSucceeded
	if status == task.OperationFailed {
		toolStatus = AgentToolFailed
	}
	return AgentToolResult{Status: toolStatus, Output: output}, nil
}

func (b *CloudDSHToolBroker) pauseMCPUnsupportedResult(ctx context.Context, current task.Task, step task.Step, usage task.Usage,
	key string, operationStatus task.OperationStatus) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepWaitingUser, "The MCP call completed, but its result contains content this task cannot safely process."
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "mcp_result_unsupported", "task.waiting_user", map[string]any{
		"operation_key": key, "operation_status": operationStatus,
	})
	return AgentToolResult{Status: AgentToolNeedsUser}, err
}

func validCloudDSHMCPJSON(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 64<<10 || !validUTF8(string(raw)) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	tokens := 0
	if err := scanAgentJSONValue(decoder, 0, &tokens); err != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func (b *CloudDSHToolBroker) commitMCPResult(ctx context.Context, current task.Task, step task.Step, usage task.Usage, name string, result AgentToolResult) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepSucceeded, "The cloud dsh MCP tool completed and its result was saved."
	if result.Status == AgentToolFailed {
		step.Status, step.Summary = task.StepFailed, "The MCP connector reported a definitive tool error."
	}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": name, "operation_status": result.Status, "output": result.Output,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) pauseMCPUnknown(ctx context.Context, current task.Task, step task.Step, usage task.Usage, key, summary string) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "mcp_result_unknown", "task.waiting_user", map[string]any{
		"operation_key": key, "operation_status": task.OperationUnknown,
	})
	return AgentToolResult{Status: AgentToolUnknown}, err
}

func (b *CloudDSHToolBroker) pauseMCPNeedsUser(ctx context.Context, current task.Task, step task.Step, usage task.Usage, key, summary string) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "mcp_connector_revoked", "task.waiting_user", map[string]any{
		"operation_key": key, "operation_status": task.OperationCancelled,
	})
	return AgentToolResult{Status: AgentToolNeedsUser}, err
}

func (b *CloudDSHToolBroker) markMCPUnknown(ctx context.Context, current task.Task, operation MCPConnectorOperation, reason string) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	store, ok := b.store.(MCPConnectorOperationStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "MCP connector operation storage is unavailable")
	}
	latest, err := store.GetMCPConnectorOperation(persistCtx, current.OwnerID, current.ID, operation.ConnectorID, operation.Key)
	if err == nil && (latest.Status == task.OperationUnknown || latest.Status == task.OperationCancelled) {
		return nil
	}
	if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
		return err
	}
	return store.MarkMCPConnectorOperationUnknown(persistCtx, operation, current.Lease.Owner, reason)
}
