package app

import (
	"context"
	"time"

	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type MCPConnectorOperation struct {
	ID                          string
	TaskID                      string
	OwnerID                     string
	ConnectorID                 string
	Key                         string
	ToolName                    string
	ConnectorRevision           int64
	CredentialVersion           int64
	GrantVersion                int64
	LeaseVersion                int64
	HostID                      string
	WorkspaceID                 string
	BindingVersion              int64
	InputRevision               *int64
	WorkspaceCheckpointRevision *int64
	RequestHash                 []byte
	Status                      task.OperationStatus
	CancelRequested             bool
	Result                      string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type MCPConnectorWorkspaceBindingStore interface {
	GetWorkspaceBinding(context.Context, string, string) (workspace.Binding, error)
}

type MCPConnectorOperationStore interface {
	MCPConnectorWorkspaceBindingStore
	PrepareMCPConnectorOperation(context.Context, MCPConnectorOperation, string) (MCPConnectorOperation, bool, error)
	MarkMCPConnectorOperationDispatched(context.Context, MCPConnectorOperation, string) (MCPConnectorOperation, error)
	CompleteMCPConnectorOperation(context.Context, MCPConnectorOperation, string, task.OperationStatus, string) (MCPConnectorOperation, error)
	MarkMCPConnectorOperationUnknown(context.Context, MCPConnectorOperation, string, string) error
	GetMCPConnectorOperation(context.Context, string, string, string, string) (MCPConnectorOperation, error)
}
