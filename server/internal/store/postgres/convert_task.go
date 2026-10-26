package postgres

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func conversationDomain(row sqlcgen.Conversation) conversation.Conversation {
	return conversation.Conversation{
		ID: row.ID, OwnerID: row.OwnerID, HostID: row.HostID, Title: row.Title,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func messageDomain(row sqlcgen.ConversationMessage) conversation.Message {
	return conversation.Message{
		ID: row.ID, ConversationID: row.ConversationID, OwnerID: row.OwnerID,
		Role: conversation.Role(row.Role), Content: row.Content, TaskID: row.TaskID,
		CreatedAt: row.CreatedAt.Time,
	}
}

func taskDomain(row sqlcgen.Task) task.Task {
	return task.Task{
		ID: row.ID, OwnerID: row.OwnerID, ConversationID: row.ConversationID, HostID: row.HostID,
		ApplicationID: row.ApplicationID, ModelID: row.ModelID, Objective: row.Objective, Status: task.Status(row.Status),
		Authorization: task.Authorization{
			Kind: row.AuthKind, TokenID: row.AuthTokenID,
			ClientInstallationID: row.AuthClientInstallationID.String, Scopes: row.AuthScopes,
		},
		Budget:      task.Budget{ActiveSeconds: int64(row.BudgetActiveSeconds), ToolCalls: int64(row.BudgetToolCalls), ModelTokens: int64(row.BudgetModelTokens)},
		Usage:       task.Usage{ActiveSeconds: row.UsageActiveSeconds, ToolCalls: row.UsageToolCalls, ModelTokens: int64Pointer(row.UsageModelTokens)},
		Lease:       task.Lease{Owner: row.LeaseOwner, Version: row.LeaseVersion, ExpiresAt: timePointer(row.LeaseExpiresAt)},
		CurrentStep: row.CurrentStep, Result: row.Result, WaitingReason: row.WaitingReason,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time, FinishedAt: timePointer(row.FinishedAt),
	}
}

func taskStepDomain(row sqlcgen.TaskStep) task.Step {
	return task.Step{
		ID: row.ID, TaskID: row.TaskID, Sequence: row.Sequence, Kind: row.Kind,
		Status: task.StepStatus(row.Status), Title: row.Title, Summary: row.Summary, CreatedAt: row.CreatedAt.Time,
	}
}

func taskBatchItemDomain(row sqlcgen.TaskBatchItem) task.BatchItem {
	return task.BatchItem{
		ID: row.ID, TaskID: row.TaskID, Key: row.ItemKey, Input: row.Input,
		Status: task.BatchItemStatus(row.Status), Attempt: row.Attempt, Result: row.Result, Error: row.Error,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func taskArtifactDomain(row sqlcgen.TaskArtifact) task.Artifact {
	var batchItemID string
	if row.BatchItemID.Valid {
		batchItemID = row.BatchItemID.String
	}
	var batchAttempt int32
	if row.BatchAttempt.Valid {
		batchAttempt = row.BatchAttempt.Int32
	}
	return task.Artifact{
		ID: row.ID, TaskID: row.TaskID, StepID: row.StepID, BatchItemID: batchItemID,
		BatchAttempt: batchAttempt, Label: row.Label, Path: row.Path, MediaType: row.MediaType,
		CreatedAt: row.CreatedAt.Time,
	}
}

func taskEventDomain(row sqlcgen.TaskEvent) task.Event {
	return task.Event{
		TaskID: row.TaskID, Sequence: row.Sequence, Type: row.EventType,
		Payload: row.Payload, CreatedAt: row.CreatedAt.Time,
	}
}

func taskOperationDomain(row sqlcgen.TaskOperation) task.Operation {
	return task.Operation{
		ID: row.ID, TaskID: row.TaskID, Key: row.OperationKey, RequestHash: row.RequestHash,
		HostID: row.HostID, WorkspaceID: row.WorkspaceID, BindingVersion: row.BindingVersion,
		RuntimeGeneration: row.RuntimeGeneration, RuntimeInstanceID: row.RuntimeInstanceID,
		ControlVersion: row.ControlVersion, LeaseVersion: row.LeaseVersion,
		RuntimeLeaseVersion: row.RuntimeLeaseVersion, Status: task.OperationStatus(row.Status),
		ExternalID: row.ExternalID, Result: row.Result, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func eventPayload(event task.Event) ([]byte, error) {
	if len(event.Payload) == 0 || !json.Valid(event.Payload) {
		return nil, invalidTaskField("event.payload", "event payload must be valid JSON")
	}
	return event.Payload, nil
}

func int64Pointer(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

func taskUsageTokens(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func taskTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
