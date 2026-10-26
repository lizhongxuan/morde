package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type toolSpec struct {
	name        string
	description string
	schema      string
	required    []string
	properties  map[string]string
	defaults    map[string]any
	runtime     *app.RuntimeComputerToolDefinition
}

const browserGuidance = " Prefer browser_snapshot first; use screenshots only when you need to inspect visual appearance."
const idempotencyGuidance = " Require a unique idempotency_key; reuse the same key only when retrying the exact same request."
const hostCodexStatusToolName = "host_codex_status"

var tools = buildTools()

func buildTools() []toolSpec {
	result := []toolSpec{
		{
			name: "host_create", description: "Create a host and optionally bind existing workspaces to it. Returns immediately with its creating state. Supply a fresh idempotency_key for each new request and reuse it only when retrying that exact request.", required: []string{"template_id", "idempotency_key"},
			properties: map[string]string{"template_id": "string", "name": "string", "workspace_ids": "array", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"template_id":{"type":"string"},"name":{"type":"string"},"workspace_ids":{"type":"array","maxItems":64,"items":{"type":"string","pattern":"^w[a-z0-9]{7}$"}},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["template_id","idempotency_key"],"additionalProperties":false}`,
		},
		{name: "host_list", description: "List the hosts available to the authenticated user.", properties: map[string]string{}, schema: `{"type":"object","properties":{},"additionalProperties":false}`},
		{name: "host_get", description: "Get a host by ID, including its current state and URL.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
		{name: "host_delete", description: "Request host deletion. Its workspace bindings are removed; independent workspace data is retained.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
		{name: "host_renew", description: "Renew a host using its configured lifetime.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
		{
			name: "task_submit", description: "Submit a durable task for Morde to continue running after this MCP client disconnects. Supply a fresh idempotency_key for each new request and reuse it only when retrying that exact request.",
			required:   []string{"host_id", "model_id", "objective", "idempotency_key"},
			properties: map[string]string{"host_id": "string", "application_id": "string", "model_id": "string", "objective": "string", "conversation_id": "string", "budget": "object", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"host_id":{"type":"string"},"application_id":{"type":"string","enum":["wechat"]},"model_id":{"type":"string","minLength":1,"maxLength":256},"objective":{"type":"string","minLength":1,"maxLength":32768},"conversation_id":{"type":"string"},"budget":{"type":"object","properties":{"active_seconds":{"type":"integer","minimum":1,"maximum":1800},"tool_calls":{"type":"integer","minimum":1,"maximum":100},"model_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["active_seconds","tool_calls","model_tokens"],"additionalProperties":false},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["host_id","model_id","objective","idempotency_key"],"additionalProperties":false}`,
		},
		{
			name: "task_list", description: "List tasks visible to this API Key, limited to its current task:read scope and Host grants.",
			properties: map[string]string{"status": "string", "limit": "integer", "cursor": "string"},
			schema:     `{"type":"object","properties":{"status":{"type":"string","enum":["queued","running","waiting_user","pausing","paused","cancelling","completed","failed","cancelled"]},"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string","maxLength":1024}},"additionalProperties":false}`,
		},
		{
			name: "task_get", description: "Get a task status, bounded result and pending request. task_url opens the task page after Portal login.",
			required: []string{"task_id"}, properties: map[string]string{"task_id": "string"},
			schema: `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`,
		},
		{
			name: "task_pause", description: "Request a task pause at a safe execution boundary. expected_status prevents acting on a stale task state. Supply a fresh idempotency_key for each new request.",
			required:   []string{"task_id", "expected_status", "idempotency_key"},
			properties: map[string]string{"task_id": "string", "expected_status": "string", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"task_id":{"type":"string"},"expected_status":{"type":"string","enum":["queued","running"]},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["task_id","expected_status","idempotency_key"],"additionalProperties":false}`,
		},
		{
			name: "task_resume", description: "Resume a paused task. expected_status must be paused. Supply a fresh idempotency_key for each new request.",
			required:   []string{"task_id", "expected_status", "idempotency_key"},
			properties: map[string]string{"task_id": "string", "expected_status": "string", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"task_id":{"type":"string"},"expected_status":{"type":"string","enum":["paused"]},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["task_id","expected_status","idempotency_key"],"additionalProperties":false}`,
		},
		{
			name: "task_cancel", description: "Request a task cancellation and wait for safe runtime confirmation. expected_status prevents acting on a stale task state. Supply a fresh idempotency_key for each new request.",
			required:   []string{"task_id", "expected_status", "idempotency_key"},
			properties: map[string]string{"task_id": "string", "expected_status": "string", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"task_id":{"type":"string"},"expected_status":{"type":"string","enum":["queued","running","waiting_user","paused"]},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["task_id","expected_status","idempotency_key"],"additionalProperties":false}`,
		},
		{
			name: "task_respond", description: "Respond to one exact pending approval or clarification request version. Set either pending_id with decision, or clarification_id with answer. Supply a fresh idempotency_key for each new response.",
			required:   []string{"task_id", "version", "idempotency_key"},
			properties: map[string]string{"task_id": "string", "version": "integer", "pending_id": "string", "decision": "string", "clarification_id": "string", "answer": "string", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"task_id":{"type":"string"},"version":{"type":"integer","minimum":1},"pending_id":{"type":"string"},"decision":{"type":"string","enum":["approve","reject"]},"clarification_id":{"type":"string"},"answer":{"type":"string","minLength":1,"maxLength":16384},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["task_id","version","idempotency_key"],"additionalProperties":false}`,
		},
		{
			name:        hostCodexStatusToolName,
			description: "Read the authorized Host's Codex CLI version and login status. This read-only check does not run commands, reveal credentials, or verify model access or quota.",
			required:    []string{"host_id"},
			properties:  map[string]string{"host_id": "string"},
			schema:      `{"type":"object","properties":{"host_id":{"type":"string","pattern":"^h[a-z0-9]{7}$"}},"required":["host_id"],"additionalProperties":false}`,
		},
	}
	for _, definition := range app.RuntimeComputerTools() {
		result = append(result, runtimeToolSpec(definition))
	}
	result = append(result, toolSpec{
		name: "preview_url", description: "Build a private preview URL. Enter the host through the portal before opening it.", required: []string{"host_id", "port"},
		properties: map[string]string{"host_id": "string", "port": "integer"},
		schema:     `{"type":"object","properties":{"host_id":{"type":"string"},"port":{"type":"integer","minimum":1,"maximum":65535}},"required":["host_id","port"],"additionalProperties":false}`,
	})
	result = append(result, scheduleToolSpecs()...)
	return result
}

func runtimeToolSpec(definition app.RuntimeComputerToolDefinition) toolSpec {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		panic("invalid shared runtime tool schema: " + definition.Name)
	}
	if schema.Properties == nil {
		schema.Properties = make(map[string]json.RawMessage)
	}
	schema.Properties["host_id"] = json.RawMessage(`{"type":"string"}`)
	schema.Properties["idempotency_key"] = json.RawMessage(`{"type":"string","minLength":8,"maxLength":128}`)
	schema.Required = append(schema.Required, "host_id", "idempotency_key")
	encoded, err := json.Marshal(map[string]any{
		"type": "object", "properties": schema.Properties,
		"required": schema.Required, "additionalProperties": false,
	})
	if err != nil {
		panic("could not encode shared runtime tool schema: " + definition.Name)
	}
	properties := make(map[string]string, len(schema.Properties))
	for name, raw := range schema.Properties {
		var value struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || value.Type == "" {
			panic("invalid shared runtime tool property: " + definition.Name + "." + name)
		}
		properties[name] = value.Type
	}
	runtimeDefinition := definition
	runtimeDefinition.Parameters = append(json.RawMessage(nil), definition.Parameters...)
	description := definition.Description + idempotencyGuidance
	if strings.HasPrefix(definition.Name, "browser_") {
		description += browserGuidance
	}
	return toolSpec{
		name: definition.Name, description: description, schema: string(encoded),
		required: schema.Required, properties: properties, runtime: &runtimeDefinition,
	}
}

func validateToolArguments(spec toolSpec, raw json.RawMessage) (map[string]any, error) {
	args := make(map[string]any)
	if len(raw) != 0 {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&args); err != nil || args == nil {
			return nil, domain.InvalidArgument("arguments", "arguments must be a JSON object")
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, domain.InvalidArgument("arguments", "arguments must contain one JSON object")
		}
	}
	for name, value := range spec.defaults {
		if _, ok := args[name]; !ok {
			args[name] = value
		}
	}
	for _, name := range spec.required {
		if _, ok := args[name]; !ok {
			return nil, domain.InvalidArgument(name, fmt.Sprintf("%s is required", name))
		}
	}
	for name, value := range args {
		want, ok := spec.properties[name]
		if !ok {
			return nil, domain.InvalidArgument(name, "unexpected argument")
		}
		if !matchesJSONType(value, want) {
			return nil, domain.InvalidArgument(name, "argument has the wrong type")
		}
	}
	if spec.name == "host_create" {
		if !app.ValidMCPIdempotencyKey(stringArg(args, "idempotency_key")) {
			return nil, domain.InvalidArgument("idempotency_key", "a unique idempotency key of 8 to 128 safe characters is required")
		}
		if ids, ok := args["workspace_ids"].([]any); ok {
			if len(ids) > workspace.MaxBindingsPerHost {
				return nil, domain.InvalidArgument("workspace_ids", "at most 64 workspaces may be bound to a host")
			}
			seen := make(map[string]struct{}, len(ids))
			for _, value := range ids {
				id, ok := value.(string)
				if !ok || !domain.ValidWorkspaceID(id) {
					return nil, domain.InvalidArgument("workspace_ids", "workspace_ids must contain valid workspace IDs")
				}
				if _, exists := seen[id]; exists {
					return nil, domain.InvalidArgument("workspace_ids", "workspace_ids must not contain duplicates")
				}
				seen[id] = struct{}{}
			}
		}
	}
	for _, name := range []string{"timeout", "start_line", "end_line", "port"} {
		if rawValue, ok := args[name]; ok {
			value, _ := intArg(map[string]any{name: rawValue}, name)
			if value < 0 || name == "port" && (value < 1 || value > 65535) || (name == "timeout" || name == "start_line" || name == "end_line") && value == 0 {
				return nil, domain.InvalidArgument(name, "argument is outside its allowed range")
			}
		}
	}
	if start, ok := intArg(args, "start_line"); ok {
		if end, hasEnd := intArg(args, "end_line"); hasEnd && end < start {
			return nil, domain.InvalidArgument("end_line", "end_line must be greater than or equal to start_line")
		}
	}
	if spec.name == "task_submit" {
		if appID := stringArg(args, "application_id"); appID != "" && !app.ValidHostApplicationID(appID) {
			return nil, domain.InvalidArgument("application_id", "application is not supported")
		}
		if budget, ok := args["budget"]; ok {
			if _, err := taskBudgetArg(budget); err != nil {
				return nil, err
			}
		}
	}
	if spec.name == "task_list" {
		if limit, ok := intArg(args, "limit"); ok && (limit < 1 || limit > 100) {
			return nil, domain.InvalidArgument("limit", "page limit is invalid")
		}
		if len(stringArg(args, "cursor")) > 1024 {
			return nil, domain.InvalidArgument("cursor", "page cursor is invalid")
		}
	}
	if spec.name == "task_respond" {
		pending := hasArg(args, "pending_id") || hasArg(args, "decision")
		clarification := hasArg(args, "clarification_id") || hasArg(args, "answer")
		if pending == clarification || pending && (strings.TrimSpace(stringArg(args, "pending_id")) == "" || stringArg(args, "decision") == "") ||
			clarification && (strings.TrimSpace(stringArg(args, "clarification_id")) == "" || strings.TrimSpace(stringArg(args, "answer")) == "") {
			return nil, domain.InvalidArgument("request", "respond to one pending approval or clarification request")
		}
	}
	if spec.runtime != nil {
		if !app.ValidMCPIdempotencyKey(stringArg(args, "idempotency_key")) {
			return nil, domain.InvalidArgument("idempotency_key", "a unique idempotency key of 8 to 128 safe characters is required")
		}
		if _, exists := args["host_id"]; !exists || strings.TrimSpace(stringArg(args, "host_id")) == "" {
			return nil, domain.InvalidArgument("host_id", "host_id is required")
		}
	}
	return args, nil
}

func taskBudgetArg(value any) (task.Budget, error) {
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 3 {
		return task.Budget{}, domain.InvalidArgument("budget", "budget must include active_seconds, tool_calls, and model_tokens")
	}
	values := make([]int64, 3)
	for index, name := range []string{"active_seconds", "tool_calls", "model_tokens"} {
		raw, exists := fields[name]
		if !exists {
			return task.Budget{}, domain.InvalidArgument("budget", "budget must include active_seconds, tool_calls, and model_tokens")
		}
		number, ok := raw.(json.Number)
		if !ok {
			return task.Budget{}, domain.InvalidArgument("budget", "budget fields must be integers")
		}
		parsed, err := number.Int64()
		if err != nil {
			return task.Budget{}, domain.InvalidArgument("budget", "budget fields must be integers")
		}
		values[index] = parsed
	}
	return task.Budget{ActiveSeconds: values[0], ToolCalls: values[1], ModelTokens: values[2]}, nil
}

func hasArg(args map[string]any, name string) bool { _, ok := args[name]; return ok }

func matchesJSONType(value any, want string) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		_, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := value.(json.Number).Int64()
		return err == nil
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "nullable-object":
		if value == nil {
			return true
		}
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	default:
		return false
	}
}

func toolByName(name string) toolSpec {
	for _, spec := range tools {
		if spec.name == name {
			return spec
		}
	}
	panic("missing test tool: " + name)
}
