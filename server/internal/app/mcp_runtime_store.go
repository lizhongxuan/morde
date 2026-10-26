package app

import (
	"context"
	"encoding/json"
	"time"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// MCPRuntimeOperationKey identifies one client-requested tool action within an
// API Key, authenticated MCP protocol session, and Host. The idempotency key is
// required because the MCP SDK does not expose the JSON-RPC request ID to tool
// handlers.
type MCPRuntimeOperationKey struct {
	OwnerID        string
	ActorUserID    string
	APIKeyID       string
	SessionID      string
	HostID         string
	IdempotencyKey string
}

// MCPRuntimeOperation is the durable bridge between an MCP call and the
// Host's operation journal. Arguments and result contain tool data, never
// credentials.
type MCPRuntimeOperation struct {
	Key            MCPRuntimeOperationKey
	RuntimeTaskID  string
	OperationID    string
	RequestHash    []byte
	Capability     string
	Action         string
	Arguments      json.RawMessage
	WorkspaceID    string
	BindingVersion int64
	Fence          AgentHostFence
	LeaseVersion   int64
	LeaseExpiresAt time.Time
	Status         task.OperationStatus
	Result         json.RawMessage
}

type BeginMCPRuntimeOperation struct {
	Key                    MCPRuntimeOperationKey
	NewRuntimeTaskID       string
	NewOperationID         string
	RequestHash            []byte
	Capability             string
	Action                 string
	Arguments              json.RawMessage
	WorkspaceID            string
	BindingVersion         int64
	Fence                  AgentHostFence
	ExpectedExecutionEpoch int64
	LeaseDuration          time.Duration
}

// MCPRuntimeOperationStore persists MCP sessions and operations separately
// from Agent task rows while applying the same owner and Host fences.
type MCPRuntimeOperationStore interface {
	BeginMCPRuntimeOperation(context.Context, BeginMCPRuntimeOperation) (MCPRuntimeOperation, bool, error)
	MarkMCPRuntimeOperationDispatched(context.Context, MCPRuntimeOperationKey, string, AgentHostFence, int64) (MCPRuntimeOperation, error)
	RenewMCPRuntimeOperationLease(context.Context, MCPRuntimeOperationKey, string, AgentHostFence, int64, int64, time.Duration) (MCPRuntimeOperation, error)
	SettleMCPRuntimeOperation(context.Context, MCPRuntimeOperationKey, string, task.OperationStatus, json.RawMessage) (MCPRuntimeOperation, error)
	GetMCPRuntimeOperation(context.Context, MCPRuntimeOperationKey) (MCPRuntimeOperation, error)
	ListActiveMCPRuntimeTaskIDs(context.Context, string, string, AgentHostFence) ([]string, error)
	ListOwnerActiveMCPRuntimeOperations(context.Context, string) ([]MCPRuntimeOperation, error)
}

// MCPRuntimeCancellationStore exposes only durable stop recovery operations.
// The Owner stop gate itself remains Portal-session-only and is persisted by
// the PostgreSQL execution-control transaction.
type MCPRuntimeCancellationStore interface {
	ListMCPRuntimeOperationsPendingCancellation(context.Context, int) ([]MCPRuntimeOperation, error)
	SettleMCPRuntimeOperationNotFound(context.Context, MCPRuntimeOperationKey, string) (MCPRuntimeOperation, error)
}

// RuntimeComputerToolDefinition exposes the exact operation contract used by
// both Agent and MCP callers.
type RuntimeComputerToolDefinition struct {
	Name        string
	Capability  string
	Action      string
	Scope       identity.Scope
	Description string
	Parameters  json.RawMessage
}
