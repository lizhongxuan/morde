package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
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
			name: "host_create", description: "Create a host with the account's current default resources. Supply a fresh idempotency_key for each new request and reuse it only when retrying that exact request.", required: []string{"idempotency_key"},
			properties: map[string]string{"name": "string", "workspace_ids": "array", "idempotency_key": "string"},
			schema:     `{"type":"object","properties":{"name":{"type":"string"},"workspace_ids":{"type":"array","maxItems":64,"items":{"type":"string","pattern":"^w[a-z0-9]{7}$"}},"idempotency_key":{"type":"string","pattern":"^[A-Za-z0-9._~-]{8,128}$"}},"required":["idempotency_key"],"additionalProperties":false}`,
		},
		{name: "host_list", description: "List the hosts available to the authenticated user.", properties: map[string]string{}, schema: `{"type":"object","properties":{},"additionalProperties":false}`},
		{name: "host_get", description: "Get a host by ID, including its current state and URL.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
		{name: "host_delete", description: "Request host deletion. Its workspace bindings are removed; independent workspace data is retained.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
		{name: "host_renew", description: "Renew a host using its configured lifetime.", required: []string{"host_id"}, properties: map[string]string{"host_id": "string"}, schema: `{"type":"object","properties":{"host_id":{"type":"string"}},"required":["host_id"],"additionalProperties":false}`},
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
