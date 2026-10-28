package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const (
	maxTaskMCPConnectorGrants     = 16
	maxTaskMCPConnectorToolSchema = 1 << 20
)

type TaskMCPConnectorGrant struct {
	TaskID            string
	OwnerID           string
	ConnectorID       string
	ConnectorRevision int64
	CredentialVersion int64
	GrantVersion      int64
	ToolSchema        []byte
	ToolSchemaSHA256  []byte
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TaskMCPConnectorGrantStore persists owner-scoped connector grants independently
// from Host-bound task_operations.
type TaskMCPConnectorGrantStore interface {
	ListTaskMCPConnectorGrants(context.Context, string, string) ([]TaskMCPConnectorGrant, error)
	SetTaskMCPConnectorToolSchema(context.Context, string, string, string, int64, []byte, []byte) (TaskMCPConnectorGrant, error)
	RevokeTaskMCPConnectorGrant(context.Context, string, string, string, int64) (TaskMCPConnectorGrant, error)
}

func normalizeTaskMCPConnectorIDs(values []string) ([]string, error) {
	if len(values) > maxTaskMCPConnectorGrants {
		return nil, domain.InvalidArgument("mcp_connector_ids", "too many MCP connectors were selected")
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	for index, id := range result {
		if !domain.ValidMCPConnectorID(id) || index > 0 && result[index-1] == id {
			return nil, domain.InvalidArgument("mcp_connector_ids", "MCP connector IDs must be valid and unique")
		}
	}
	return result, nil
}

func validateTaskMCPConnectorToolSchema(schema, digest []byte) error {
	if len(schema) == 0 || len(schema) > maxTaskMCPConnectorToolSchema || !json.Valid(schema) || len(digest) != sha256.Size {
		return domain.InvalidArgument("tool_schema", "MCP connector tool schema is invalid or exceeds its limit")
	}
	expected := sha256.Sum256(schema)
	if !bytes.Equal(expected[:], digest) {
		return domain.InvalidArgument("tool_schema", "MCP connector tool schema digest is invalid")
	}
	return nil
}

func (s *TaskService) RevokeMCPConnectorGrant(ctx context.Context, principal identity.Principal, taskID, connectorID string, expectedGrantVersion int64) (TaskMCPConnectorGrant, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskMCPConnectorGrant{}, err
	}
	if !domain.ValidTaskID(taskID) {
		return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if !domain.ValidMCPConnectorID(connectorID) || expectedGrantVersion < 1 {
		return TaskMCPConnectorGrant{}, domain.InvalidArgument("grant", "connector ID and positive grant version are required")
	}
	grantStore, ok := s.store.(TaskMCPConnectorGrantStore)
	if !ok {
		return TaskMCPConnectorGrant{}, domain.NewError(domain.CodeUnsupported, "task MCP connector grants are unavailable")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskMCPConnectorGrant{}, hideTaskAccessError(err)
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return TaskMCPConnectorGrant{}, hideTaskAccessError(err)
	}
	return grantStore.RevokeTaskMCPConnectorGrant(ctx, principal.UserID, taskID, connectorID, expectedGrantVersion)
}
