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
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

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

type cloudDSHMCPExecutionSource struct {
	connectorOwnerID    string
	connectorID         string
	connectorName       string
	connectorRevision   int64
	credentialVersion   int64
	grantVersion        int64
	projectID           string
	projectGrantVersion int64
	scope               identity.Scope
	endpoint            string
	remoteTool          string
	taskContextLimit    int
	client              MCPConnectorToolClient
	headers             func(context.Context) (http.Header, error)
}

func (b *CloudDSHToolBroker) currentServerTask(ctx context.Context, claimed task.Task) (TaskDetails, error) {
	details, err := b.currentTaskDetails(ctx, claimed)
	if err != nil {
		return TaskDetails{}, err
	}
	if details.Task.ExecutionLocation != "server" || details.Task.Status != task.StatusRunning {
		return TaskDetails{}, domain.NewError(domain.CodeInvalidState, "MCP connector tools require a running server task")
	}
	return details, nil
}

func canonicalMCPToolSchema(tools []MCPConnectorToolDefinition) ([]byte, []byte, error) {
	if len(tools) > 256 {
		return nil, nil, errors.New("MCP tool catalog exceeds its limit")
	}
	tools = append([]MCPConnectorToolDefinition(nil), tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	for index := range tools {
		tool := &tools[index]
		if tool.Name == "" || len(tool.Name) > 128 || !validUTF8(tool.Name) || strings.IndexFunc(tool.Name, unicode.IsControl) >= 0 || len(tool.Description) > 4096 || !validUTF8(tool.Description) ||
			!validAgentJSONObject(tool.InputSchema, 64<<10) || index > 0 && tools[index-1].Name == tool.Name {
			return nil, nil, errors.New("MCP tool definition is invalid")
		}
		decoder := json.NewDecoder(bytes.NewReader(tool.InputSchema))
		decoder.UseNumber()
		var schema any
		if decoder.Decode(&schema) != nil {
			return nil, nil, errors.New("MCP tool schema is invalid")
		}
		if object, ok := schema.(map[string]any); ok && object["type"] == "object" {
			if _, declared := object["additionalProperties"]; !declared {
				object["additionalProperties"] = false
			}
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

func (b *CloudDSHToolBroker) executeMCP(ctx context.Context, details TaskDetails, call ModelToolCall, arguments json.RawMessage,
	definition AgentToolDefinition, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	current := details.Task
	grantStore, grantsOK := b.store.(TaskMCPConnectorGrantStore)
	connectorStore, connectorsOK := b.store.(MCPConnectorStore)
	if !grantsOK || !connectorsOK {
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
	if grant == nil || grant.TaskID != current.ID || grant.OwnerID != current.OwnerID || !domain.ValidUserID(grant.ConnectorOwnerID) || grant.GrantVersion < 1 ||
		!bytes.Equal(grant.ToolSchemaSHA256, sha256Bytes(grant.ToolSchema)) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "MCP connector was not granted to this task")
	}
	if err := authorizeTaskMCPProjectConnector(ctx, b.store, current, *grant); err != nil {
		return AgentToolResult{}, err
	}
	connector, err := connectorStore.GetMCPConnector(ctx, grant.ConnectorOwnerID, grant.ConnectorID)
	if err != nil {
		return AgentToolResult{}, err
	}
	if !connector.Enabled || connector.Revision != grant.ConnectorRevision || connector.CredentialVersion != grant.CredentialVersion {
		return AgentToolResult{}, domain.NewError(domain.CodeConflict, "MCP connector changed after this task was authorized")
	}
	if _, err := definitionForPinnedMCPTool(grant.ToolSchema, grant.ConnectorID, call.Name, definition.MCPRemoteToolName); err != nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "MCP connector tool is not in this task's authorized catalog")
	}
	source := cloudDSHMCPExecutionSource{
		connectorOwnerID: grant.ConnectorOwnerID, connectorID: grant.ConnectorID, connectorName: connector.Name,
		connectorRevision: grant.ConnectorRevision, credentialVersion: grant.CredentialVersion,
		grantVersion: grant.GrantVersion, projectID: grant.ProjectID, projectGrantVersion: grant.ProjectGrantVersion,
		scope: identity.ScopeMCPControl, endpoint: connector.EndpointURL,
		remoteTool: definition.MCPRemoteToolName, client: b.mcpClient,
		headers: func(ctx context.Context) (http.Header, error) {
			return b.mcpCredentials.CredentialHeaders(ctx, connector)
		},
	}
	if result, handled, err := b.authorizeMCPCall(ctx, details, call, arguments, source); handled || err != nil {
		return result, err
	}
	return b.executeMCPSource(ctx, details, call, arguments, source, recovering, checkpointOutput)
}

func (b *CloudDSHToolBroker) authorizeMCPCall(ctx context.Context, details TaskDetails, call ModelToolCall,
	arguments json.RawMessage, source cloudDSHMCPExecutionSource) (AgentToolResult, bool, error) {
	current := details.Task
	hash, err := cloudDSHMCPApprovalHash(current, call, arguments, source)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	fence := cloudDSHMCPApprovalFence(source)
	request := AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: agentOperationKey(call.CallID),
		RequiredScope: source.scope, Fence: fence,
	}
	pending := details.PendingRequest
	sameApproval := sameMCPConnectorApproval(pending, current, call, hash, fence)
	if pending != nil && pending.Status == task.PendingRequestPending {
		result, err := b.pauseForExistingMCPApproval(ctx, details, pending)
		return result, true, err
	}
	if sameApproval {
		switch pending.Status {
		case task.PendingRequestApproved:
			if pending.Decision != task.PendingDecisionApprove {
				result, err := b.failMCPApprovalMismatch(ctx, current, call, details.Steps)
				return result, true, err
			}
			return AgentToolResult{}, false, nil
		case task.PendingRequestRejected:
			if pending.Decision != task.PendingDecisionReject {
				result, err := b.failMCPApprovalMismatch(ctx, current, call, details.Steps)
				return result, true, err
			}
			step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
				"External MCP action rejected", "The connected service was not called.")
			if err != nil {
				return AgentToolResult{}, true, err
			}
			result, err := b.rejectApprovedToolCall(ctx, current, step, cloneTaskUsage(current.Usage), request)
			return result, true, err
		default:
			result, err := b.failMCPApprovalMismatch(ctx, current, call, details.Steps)
			return result, true, err
		}
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Approve external MCP action: "+source.remoteTool, "Waiting for approval before calling the connected service.")
	if err != nil {
		return AgentToolResult{}, true, err
	}
	preparation := AgentToolPreparation{
		Status: AgentToolApprovalRequired, Approval: cloudDSHMCPApprovalRequest(source, arguments, hash), Fence: fence,
	}
	result, err := b.pauseForApproval(ctx, current, step, cloneTaskUsage(current.Usage), request, hash, preparation, pending)
	return result, true, err
}

func (b *CloudDSHToolBroker) pauseForExistingMCPApproval(ctx context.Context, details TaskDetails, pending *task.PendingRequest) (AgentToolResult, error) {
	current := details.Task
	step, err := newAgentStep(details.Task.ID, maxStepSequence(details.Steps)+1, "tool", task.StepWaitingUser,
		"Approval required: "+pending.ToolName, pending.Content)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := b.commitProgress(ctx, current, step, cloneTaskUsage(current.Usage), task.StatusWaitingUser,
		"approval_required", "task.waiting_user", AgentApprovalRequest{Object: pending.Object, Content: pending.Content, Impact: pending.Impact}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser}, nil
}

func cloudDSHMCPApprovalHash(current task.Task, call ModelToolCall, arguments json.RawMessage, source cloudDSHMCPExecutionSource) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		TaskID              string          `json:"task_id"`
		OwnerID             string          `json:"owner_id"`
		CallID              string          `json:"call_id"`
		ToolAlias           string          `json:"tool_alias"`
		ConnectorOwnerID    string          `json:"connector_owner_id"`
		ConnectorID         string          `json:"connector_id"`
		ConnectorRevision   int64           `json:"connector_revision"`
		CredentialVersion   int64           `json:"credential_version"`
		GrantVersion        int64           `json:"grant_version"`
		ProjectID           string          `json:"project_id"`
		ProjectGrantVersion int64           `json:"project_grant_version"`
		RemoteTool          string          `json:"remote_tool"`
		Arguments           json.RawMessage `json:"arguments"`
	}{
		TaskID: current.ID, OwnerID: current.OwnerID, CallID: call.CallID, ToolAlias: call.Name,
		ConnectorOwnerID: source.connectorOwnerID, ConnectorID: source.connectorID,
		ConnectorRevision: source.connectorRevision, CredentialVersion: source.credentialVersion,
		GrantVersion: source.grantVersion, ProjectID: source.projectID, ProjectGrantVersion: source.projectGrantVersion,
		RemoteTool: source.remoteTool, Arguments: arguments,
	})
	if err != nil {
		return nil, fmt.Errorf("encode exact MCP approval request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func cloudDSHMCPApprovalFence(source cloudDSHMCPExecutionSource) AgentHostFence {
	// Reuse the pending-request fence slots for the connector revision and task grant version.
	// The approval hash also binds connector ownership, credential/project grants, tool, and arguments.
	return AgentHostFence{
		RuntimeGeneration: source.connectorRevision, RuntimeInstanceID: "mcp-connector:" + source.connectorID,
		ControlVersion: source.grantVersion, AIAllowed: true,
	}
}

func cloudDSHMCPApprovalRequest(source cloudDSHMCPExecutionSource, arguments json.RawMessage, requestHash []byte) AgentApprovalRequest {
	argumentPreview := cloudDSHMCPApprovalArgumentPreview(arguments)
	return AgentApprovalRequest{
		Object:  fmt.Sprintf("External MCP connector %s (%s)", source.connectorName, source.connectorID),
		Content: fmt.Sprintf("Run tool %q with arguments %s? Request fingerprint: %s", source.remoteTool, argumentPreview, hex.EncodeToString(requestHash)),
		Impact:  "This may read or change data in the connected external service. Approval is limited to this task, connector version, tool, and exact arguments.",
	}
}

func cloudDSHMCPApprovalArgumentPreview(arguments json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "unavailable"
	}
	hasRedactions := redactMCPApprovalArguments(value)
	if object, ok := value.(map[string]any); ok {
		return cloudDSHMCPApprovalObjectPreview(object, hasRedactions)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "unavailable"
	}
	const previewLimit = 240
	preview := string(encoded)
	if hasRedactions {
		preview = "Sensitive values: [redacted]; arguments: " + preview
	}
	if utf8.RuneCountInString(preview) > previewLimit {
		return truncateAgentRunes(preview, previewLimit-1) + "…"
	}
	return preview
}

func cloudDSHMCPApprovalObjectPreview(value map[string]any, hasRedactions bool) string {
	const (
		previewLimit    = 240
		fieldValueLimit = 32
	)
	keyFieldsLimit := 190
	if hasRedactions {
		keyFieldsLimit = 165
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type keyField struct {
		name     string
		value    any
		priority int
	}
	fields := make([]keyField, 0, len(keys))
	other := make(map[string]any, len(value))
	for _, key := range keys {
		if priority, ok := cloudDSHMCPApprovalFieldPriority(key); ok {
			fields = append(fields, keyField{name: key, value: value[key], priority: priority})
			continue
		}
		other[key] = value[key]
	}
	if len(fields) == 0 {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "unavailable"
		}
		if hasRedactions {
			encoded = append([]byte("Sensitive values: [redacted]; arguments: "), encoded...)
		}
		if utf8.RuneCount(encoded) > previewLimit {
			return truncateAgentRunes(string(encoded), previewLimit-1) + "…"
		}
		return string(encoded)
	}
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].priority < fields[j].priority })

	var summary strings.Builder
	summary.WriteString("Key fields: {")
	for _, field := range fields {
		encodedValue, err := json.Marshal(field.value)
		if err != nil {
			continue
		}
		valuePreview := string(encodedValue)
		if utf8.RuneCount(encodedValue) > fieldValueLimit {
			if text, ok := field.value.(string); ok {
				text = truncateAgentRunes(text, fieldValueLimit-3) + "…"
				encodedValue, _ = json.Marshal(text)
				valuePreview = string(encodedValue)
			} else {
				valuePreview = `"` + truncateAgentRunes(valuePreview, fieldValueLimit-2) + "…\""
			}
		}
		separator := ""
		if summary.Len() > len("Key fields: {") {
			separator = ", "
		}
		entry := separator + `"` + field.name + `":` + valuePreview
		remaining := keyFieldsLimit - utf8.RuneCountInString(summary.String()) - 1
		if utf8.RuneCountInString(entry) > remaining {
			valueBudget := remaining - utf8.RuneCountInString(separator) - utf8.RuneCountInString(field.name) - 3
			if valueBudget < 2 {
				break
			}
			valuePreview = `"` + truncateAgentRunes(valuePreview, valueBudget-1) + "…\""
			entry = separator + `"` + field.name + `":` + valuePreview
		}
		if utf8.RuneCountInString(entry) > remaining {
			break
		}
		summary.WriteString(entry)
	}
	summary.WriteByte('}')
	preview := summary.String()
	if hasRedactions {
		preview += "; sensitive values: [redacted]"
	}
	if len(other) > 0 {
		encodedOther, err := json.Marshal(other)
		if err == nil {
			const otherArgsLabel = "; other arguments: "
			remaining := previewLimit - utf8.RuneCountInString(preview) - utf8.RuneCountInString(otherArgsLabel)
			if remaining > 0 {
				preview += otherArgsLabel + truncateAgentRunes(string(encodedOther), remaining)
			}
		}
	}
	if utf8.RuneCountInString(preview) > previewLimit {
		return truncateAgentRunes(preview, previewLimit-1) + "…"
	}
	return preview
}

func cloudDSHMCPApprovalFieldPriority(key string) (int, bool) {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
	switch {
	case strings.Contains(key, "amount"):
		return 0, true
	case strings.Contains(key, "currency"):
		return 1, true
	case strings.Contains(key, "project"):
		return 2, true
	case strings.Contains(key, "item") || strings.Contains(key, "product"):
		return 3, true
	case strings.Contains(key, "merchant") || strings.Contains(key, "vendor") || strings.Contains(key, "seller"):
		return 4, true
	case strings.Contains(key, "quantity") || key == "qty":
		return 5, true
	case strings.Contains(key, "total") || strings.Contains(key, "price"):
		return 6, true
	case strings.Contains(key, "booking") || strings.Contains(key, "reservation") || strings.Contains(key, "order"):
		return 7, true
	case strings.Contains(key, "checkin") || strings.Contains(key, "checkout") || strings.Contains(key, "date") || strings.Contains(key, "time"):
		return 8, true
	case key == "to" || strings.Contains(key, "recipient"):
		return 9, true
	case strings.Contains(key, "subject") || key == "title":
		return 10, true
	default:
		return 0, false
	}
}

func redactMCPApprovalArguments(value any) bool {
	redacted := false
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			if sensitiveMCPApprovalKey(key) {
				item[key] = "[redacted]"
				redacted = true
				continue
			}
			redacted = redactMCPApprovalArguments(nested) || redacted
		}
	case []any:
		for _, nested := range item {
			redacted = redactMCPApprovalArguments(nested) || redacted
		}
	}
	return redacted
}

func sensitiveMCPApprovalKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") ||
		strings.Contains(key, "authorization") || strings.Contains(key, "apikey") || key == "cookie" ||
		key == "card" || key == "creditcard" || key == "debitcard" || key == "pan" || key == "ccnumber" ||
		strings.Contains(key, "cardnumber") || strings.Contains(key, "carddetails") || strings.Contains(key, "cardinfo") ||
		strings.Contains(key, "cardcvv") || strings.Contains(key, "cardcvc") ||
		strings.Contains(key, "cvv") || strings.Contains(key, "cvc") || strings.Contains(key, "securitycode") ||
		strings.Contains(key, "cardverification") || strings.Contains(key, "paymentmethod") ||
		strings.Contains(key, "paymentcredential")
}

func sameMCPConnectorApproval(pending *task.PendingRequest, current task.Task, call ModelToolCall, requestHash []byte, fence AgentHostFence) bool {
	return pending != nil && pending.TaskID == current.ID && pending.Kind == "approval" && pending.CallID == call.CallID &&
		pending.ToolName == call.Name && bytes.Equal(pending.RequestHash, requestHash) && samePendingApprovalFence(pending, fence)
}

func (b *CloudDSHToolBroker) failMCPApprovalMismatch(ctx context.Context, current task.Task, call ModelToolCall, steps []task.Step) (AgentToolResult, error) {
	step, err := newAgentStep(current.ID, maxStepSequence(steps)+1, "tool", task.StepFailed,
		"MCP approval no longer matches", "The external connector call changed after approval; it was not executed.")
	if err != nil {
		return AgentToolResult{}, err
	}
	result := AgentToolResult{Status: AgentToolFailed, Output: "The saved approval does not match this exact external MCP action. It was not executed."}
	if err := b.commitProgress(ctx, current, step, cloneTaskUsage(current.Usage), task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "operation_status": result.Status, "approval_decision": "mismatch",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) executeMCPSource(ctx context.Context, details TaskDetails, call ModelToolCall, arguments json.RawMessage,
	source cloudDSHMCPExecutionSource, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	current := details.Task
	actorID := taskActorID(current)
	operationStore, ok := b.store.(MCPConnectorOperationStore)
	if !ok || source.client == nil || source.headers == nil {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "MCP connector execution storage is unavailable")
	}
	var err error
	arguments, err = validateAgentToolArguments(string(arguments))
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	key := agentOperationKey(call.CallID)
	operation, lookupErr := operationStore.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, source.connectorID, key)
	hasOperation := lookupErr == nil
	if lookupErr != nil && domain.CodeOf(lookupErr) != domain.CodeNotFound {
		return AgentToolResult{}, lookupErr
	}
	if hasOperation && (operation.TaskID != current.ID || operation.OwnerID != current.OwnerID || operation.ConnectorID != source.connectorID ||
		operation.Key != key || operation.ToolName != call.Name || !bytes.Equal(operation.RequestHash, hash[:]) ||
		operation.ConnectorRevision != source.connectorRevision || operation.CredentialVersion != source.credentialVersion || operation.GrantVersion != source.grantVersion ||
		!sameMCPConnectorOperationTaskSnapshot(operation, current)) {
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
			"tool": call.Name, "operation_key": key, "phase": "prepare", "scope": source.scope,
		}); err != nil {
			return AgentToolResult{}, err
		}
		current.Usage = usage
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate MCP connector operation ID: %w", err)
	}
	preparedFence := MCPConnectorOperation{
		ID: operationID, TaskID: current.ID, OwnerID: current.OwnerID, ConnectorID: source.connectorID, Key: key,
		ToolName: call.Name, ConnectorRevision: source.connectorRevision, CredentialVersion: source.credentialVersion,
		GrantVersion: source.grantVersion, LeaseVersion: current.Lease.Version, HostID: current.HostID,
		WorkspaceID: current.WorkspaceID, InputRevision: current.InputRevision,
		WorkspaceCheckpointRevision: current.WorkspaceCheckpointRevision,
		RequestHash:                 hash[:], Status: task.OperationPrepared,
	}
	if hasOperation {
		// A replay keeps the checkpoint captured by the original operation. The
		// store compares it with the current Task and never refreshes this fence.
		preparedFence.BindingVersion = operation.BindingVersion
	} else if current.WorkspaceID != "" {
		binding, err := operationStore.GetWorkspaceBinding(ctx, current.HostID, current.WorkspaceID)
		if err != nil {
			return AgentToolResult{}, err
		}
		if binding.OwnerID != actorID || binding.HostID != current.HostID || binding.Workspace.ID != current.WorkspaceID || binding.BindingVersion <= 0 {
			return AgentToolResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Host Workspace binding changed before MCP operation preparation")
		}
		preparedFence.BindingVersion = binding.BindingVersion
	}
	operation, replayed, err := operationStore.PrepareMCPConnectorOperation(ctx, preparedFence, current.Lease.Owner)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseMCPWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return AgentToolResult{}, err
	}
	if recovering && hasOperation && !replayed {
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector operation disappeared during recovery.")
	}
	if operation.ID == "" || operation.TaskID != current.ID || operation.ConnectorID != source.connectorID || operation.Key != key ||
		operation.Status == task.OperationPrepared && operation.LeaseVersion != current.Lease.Version || !bytes.Equal(operation.RequestHash, hash[:]) ||
		!sameMCPConnectorOperationTaskSnapshot(operation, current) || operation.BindingVersion != preparedFence.BindingVersion {
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
	headers, err := source.headers(ctx)
	if err != nil {
		return AgentToolResult{}, err
	}
	if _, err := operationStore.MarkMCPConnectorOperationDispatched(ctx, operation, current.Lease.Owner); err != nil {
		if latest, getErr := operationStore.GetMCPConnectorOperation(ctx, current.OwnerID, current.ID, source.connectorID, key); getErr == nil &&
			(latest.Status == task.OperationCancelled || latest.Status == task.OperationUnknown) {
			return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant changed before dispatch.", latest.Status)
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseMCPWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return AgentToolResult{}, err
	}
	remoteResult, callErr := source.client.CallTool(ctx, source.endpoint, headers, source.remoteTool, arguments)
	if callErr != nil {
		if latest, getErr := operationStore.GetMCPConnectorOperation(context.WithoutCancel(ctx), current.OwnerID, current.ID, source.connectorID, key); getErr == nil &&
			latest.CancelRequested {
			if markErr := b.markMCPUnknown(ctx, current, operation, "MCP call ended after connector revocation."); markErr != nil {
				return AgentToolResult{}, markErr
			}
			return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant or task was revoked while the MCP call was running.", task.OperationUnknown)
		}
		unknownReason := "MCP call outcome could not be confirmed."
		unknownSummary := "The MCP connector result is unknown; the action will not be replayed."
		builtinErrorCategory := ""
		if source.connectorID == BuiltinSearchMCPConnectorID {
			builtinErrorCategory = cloudDSHBuiltinSearchErrorCategory(ctx, callErr)
			unknownReason = "MCP call outcome could not be confirmed (" + builtinErrorCategory + ")."
			unknownSummary = "The MCP connector result is unknown (" + builtinErrorCategory + "); the action will not be replayed."
		}
		if markErr := b.markMCPUnknown(ctx, current, operation, unknownReason); markErr != nil {
			if source.connectorID == BuiltinSearchMCPConnectorID {
				return AgentToolResult{}, fmt.Errorf("built-in web tool outcome could not be recorded (%s): %w", builtinErrorCategory, markErr)
			}
			return AgentToolResult{}, errors.Join(callErr, markErr)
		}
		return b.pauseMCPUnknown(ctx, current, step, usage, key, unknownSummary)
	}
	if source.connectorID == BuiltinSearchMCPConnectorID && remoteResult.IsError {
		remoteResult = cloudDSHBuiltinSearchSafeErrorResult()
	}
	output, err := encodeCloudDSHMCPOutput(remoteResult)
	if err != nil {
		if markErr := b.markMCPUnknown(ctx, current, operation, "MCP result exceeded the durable output limit."); markErr != nil {
			return AgentToolResult{}, errors.Join(err, markErr)
		}
		return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result exceeded its durable output limit.")
	}
	if source.taskContextLimit > 0 {
		output, err = boundCloudDSHMCPContextOutput(output, source.taskContextLimit)
		if err != nil {
			if markErr := b.markMCPUnknown(ctx, current, operation, "MCP result could not be bounded for task context."); markErr != nil {
				return AgentToolResult{}, errors.Join(err, markErr)
			}
			return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP connector result could not be bounded for task context.")
		}
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
		latest, getErr := operationStore.GetMCPConnectorOperation(context.WithoutCancel(ctx), current.OwnerID, current.ID, source.connectorID, key)
		if getErr == nil && latest.Status == operationStatus && latest.Result == output {
			// The commit may have succeeded even if its confirmation was lost.
			completed = latest
		} else {
			if getErr == nil && latest.Status == task.OperationDispatched {
				unknownReason := "MCP result could not be committed after the connector returned."
				if latest.CancelRequested {
					unknownReason = "MCP result was discarded after connector authorization was revoked."
				}
				if markErr := b.markMCPUnknown(ctx, current, operation, unknownReason); markErr != nil {
					return AgentToolResult{}, errors.Join(completeErr, markErr)
				}
				if latest.CancelRequested {
					return b.pauseMCPNeedsUser(ctx, current, step, usage, key, "The connector grant or task was revoked while the MCP call was running.", task.OperationUnknown)
				}
			}
			return b.pauseMCPUnknown(ctx, current, step, usage, key, "The MCP call returned, but its result could not be committed safely.")
		}
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

func boundCloudDSHMCPContextOutput(output string, limit int) (string, error) {
	if !utf8.ValidString(output) || limit < 256 {
		return "", errors.New("MCP task context limit is invalid")
	}
	if len(output) <= limit {
		return output, nil
	}
	var value cloudDSHMCPOutput
	if json.Unmarshal([]byte(output), &value) != nil || len(value.Text) > 256 {
		return "", errors.New("MCP task context output is invalid")
	}
	var content strings.Builder
	for _, text := range value.Text {
		if !utf8.ValidString(text) {
			return "", errors.New("MCP task context text is invalid")
		}
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(text)
	}
	if len(value.StructuredContent) > 0 {
		if content.Len() > 0 {
			content.WriteByte('\n')
		}
		content.Write(value.StructuredContent)
	}
	body := content.String()
	runes := []rune(body)
	const marker = "\n[page content truncated for task context]"
	low, high := 0, len(runes)
	best := ""
	for low <= high {
		mid := (low + high) / 2
		candidate, err := json.Marshal(cloudDSHMCPOutput{IsError: value.IsError, UnsupportedContent: value.UnsupportedContent, Text: []string{string(runes[:mid]) + marker}})
		if err != nil {
			return "", errors.New("MCP task context output cannot be encoded")
		}
		if len(candidate) <= limit {
			best = string(candidate)
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best == "" {
		return "", errors.New("MCP task context limit is too small")
	}
	return best, nil
}

func taskActorID(value task.Task) string {
	if value.InitiatorID != "" {
		return value.InitiatorID
	}
	return value.OwnerID
}

func authorizeTaskMCPProjectConnector(ctx context.Context, store any, current task.Task, grant TaskMCPConnectorGrant) error {
	if current.ProjectID == "" {
		if grant.ProjectID != "" || grant.ProjectGrantVersion != 0 || grant.ConnectorOwnerID != grant.OwnerID {
			return domain.NewError(domain.CodeForbidden, "personal MCP connector grant has an invalid project scope")
		}
		return nil
	}
	if grant.ProjectID != current.ProjectID || grant.ProjectGrantVersion < 1 {
		return domain.NewError(domain.CodeForbidden, "MCP connector was not granted to this project")
	}
	authorizer, ok := store.(ProjectMCPConnectorAuthorizationStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "project MCP connector authorization is unavailable")
	}
	return authorizer.AuthorizeProjectMCPConnector(ctx, current.ProjectID, taskActorID(current), grant.ConnectorOwnerID,
		grant.ConnectorID, grant.ProjectGrantVersion)
}

func sameMCPConnectorOperationTaskSnapshot(operation MCPConnectorOperation, current task.Task) bool {
	return operation.HostID == current.HostID && operation.WorkspaceID == current.WorkspaceID &&
		sameTaskInputRevision(operation.InputRevision, current.InputRevision) &&
		sameTaskInputRevision(operation.WorkspaceCheckpointRevision, current.WorkspaceCheckpointRevision)
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
		"tool": name, "operation_status": result.Status,
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

func (b *CloudDSHToolBroker) pauseMCPNeedsUser(ctx context.Context, current task.Task, step task.Step, usage task.Usage, key, summary string,
	operationStatus task.OperationStatus) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "mcp_connector_revoked", "task.waiting_user", map[string]any{
		"operation_key": key, "operation_status": operationStatus,
	})
	return AgentToolResult{Status: AgentToolNeedsUser}, err
}

func (b *CloudDSHToolBroker) pauseMCPWorkspaceInputChange(ctx context.Context, current task.Task, step task.Step, usage task.Usage, tool string) (AgentToolResult, error) {
	message := "Workspace changed after this task was submitted. Submit a new task to use the updated files."
	step.Status, step.Summary = task.StepWaitingUser, message
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "workspace_input_changed", "task.waiting_user", map[string]any{
		"tool": tool, "message": message,
	})
	return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "workspace_input_changed", Output: message}, err
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
