package app

import (
	"context"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

// AcceptConversationMessageCommand describes user input before any Task exists.
type AcceptConversationMessageCommand struct {
	ProjectID               string
	ServiceInstanceID       string
	ClientMessageID         string
	PreviousClientMessageID string
	ReferenceTaskID         string
	Content                 string
	ModelID                 string
	HostID                  string
	WorkspaceID             string
	ApplicationID           string
	MCPConnectorIDs         []string
	ClientAttachmentIDs     []string
	Budget                  task.Budget
	IdempotencyKey          string
}

// ConversationRoutingInput contains only the immutable options needed after receipt.
type ConversationRoutingInput struct {
	ProjectID               string      `json:"project_id,omitempty"`
	PreviousClientMessageID string      `json:"previous_client_message_id,omitempty"`
	ReferenceTaskID         string      `json:"reference_task_id,omitempty"`
	ModelID                 string      `json:"model_id,omitempty"`
	HostID                  string      `json:"host_id,omitempty"`
	WorkspaceID             string      `json:"workspace_id,omitempty"`
	ApplicationID           string      `json:"application_id,omitempty"`
	MCPConnectorIDs         []string    `json:"mcp_connector_ids,omitempty"`
	ClientAttachmentIDs     []string    `json:"client_attachment_ids,omitempty"`
	Budget                  task.Budget `json:"budget"`
}

type ConversationMessageReceipt struct {
	Message           conversation.Message
	ServiceInstanceID string
	ClientID          string
	Input             ConversationRoutingInput
	RequestHash       []byte
	Authorization     task.Authorization
}

type ConversationMessageReceiptStore interface {
	AcceptConversationMessage(context.Context, ConversationMessageReceipt) (conversation.Message, bool, error)
}

// prepareConversationMessage verifies identity and immutable routing options without checking tool availability.
func (s *TaskService) prepareConversationMessage(ctx context.Context, principal identity.Principal, conversationID string, command AcceptConversationMessageCommand) (ConversationMessageReceipt, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return ConversationMessageReceipt{}, err
	}
	if !domain.ValidConversationID(conversationID) {
		return ConversationMessageReceipt{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if !domain.ValidServiceInstanceID(command.ServiceInstanceID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
	}
	if !idempotencyKeyPattern.MatchString(command.ClientMessageID) || command.IdempotencyKey != command.ClientMessageID {
		return ConversationMessageReceipt{}, domain.InvalidArgument("client_message_id", "client message id and idempotency key must match")
	}
	if command.PreviousClientMessageID != "" && (!idempotencyKeyPattern.MatchString(command.PreviousClientMessageID) || command.PreviousClientMessageID == command.ClientMessageID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("previous_client_message_id", "previous client message id is invalid")
	}
	if command.ReferenceTaskID != "" && !domain.ValidTaskID(command.ReferenceTaskID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("reference_task_id", "reference task id is invalid")
	}
	if command.HostID != "" && !domain.ValidHostID(command.HostID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	if command.WorkspaceID != "" && !domain.ValidWorkspaceID(command.WorkspaceID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("workspace_id", "workspace id is invalid")
	}
	if command.ApplicationID != "" && !ValidHostApplicationID(command.ApplicationID) {
		return ConversationMessageReceipt{}, domain.InvalidArgument("application_id", "application is not supported")
	}
	command.ModelID = strings.TrimSpace(command.ModelID)
	if len(command.ModelID) > 256 || !utf8.ValidString(command.ModelID) || strings.ContainsRune(command.ModelID, '\x00') {
		return ConversationMessageReceipt{}, domain.InvalidArgument("model_id", "model id is invalid")
	}
	if len(command.ClientAttachmentIDs) > conversation.MaxHistoryImportAttachments {
		return ConversationMessageReceipt{}, domain.InvalidArgument("client_attachment_ids", "message attachment count exceeds the limit")
	}
	seen := make(map[string]struct{}, len(command.ClientAttachmentIDs))
	for _, id := range command.ClientAttachmentIDs {
		if !idempotencyKeyPattern.MatchString(id) {
			return ConversationMessageReceipt{}, domain.InvalidArgument("client_attachment_ids", "attachment id is invalid")
		}
		if _, exists := seen[id]; exists {
			return ConversationMessageReceipt{}, domain.InvalidArgument("client_attachment_ids", "attachment ids must be unique")
		}
		seen[id] = struct{}{}
	}
	connectors, err := normalizeTaskMCPConnectorIDs(command.MCPConnectorIDs)
	if err != nil {
		return ConversationMessageReceipt{}, err
	}
	budget, err := normalizedBudget(command.Budget)
	if err != nil {
		return ConversationMessageReceipt{}, err
	}
	ownerID := principal.UserID
	if command.ProjectID != "" {
		if !project.ValidID(command.ProjectID) || s.projectAccess == nil {
			return ConversationMessageReceipt{}, domain.NewError(domain.CodeNotFound, "project not found")
		}
		if _, err := s.projectAccess.RequireAccess(ctx, principal, command.ProjectID, project.RoleMember); err != nil {
			return ConversationMessageReceipt{}, err
		}
		value, err := s.store.GetProjectConversation(ctx, principal.UserID, command.ProjectID, conversationID)
		if err != nil {
			return ConversationMessageReceipt{}, err
		}
		ownerID = value.OwnerID
	} else if _, err := s.store.GetConversation(ctx, conversationID, principal.UserID); err != nil {
		return ConversationMessageReceipt{}, err
	}
	// Host and Workspace authorization is intentionally deferred until a tool uses them.
	authorization, err := s.authorizeTask(ctx, principal, "", identity.ScopeTaskSubmit)
	if err != nil {
		return ConversationMessageReceipt{}, err
	}
	input := ConversationRoutingInput{
		ProjectID: command.ProjectID, PreviousClientMessageID: command.PreviousClientMessageID,
		ReferenceTaskID: command.ReferenceTaskID, ModelID: command.ModelID, HostID: command.HostID,
		WorkspaceID: command.WorkspaceID, ApplicationID: command.ApplicationID, MCPConnectorIDs: connectors,
		ClientAttachmentIDs: append([]string(nil), command.ClientAttachmentIDs...), Budget: budget,
	}
	return ConversationMessageReceipt{
		Message: conversation.Message{ConversationID: conversationID, OwnerID: ownerID, ActorID: principal.UserID,
			Role: conversation.RoleUser, DeliveryStatus: conversation.DeliveryPending},
		ServiceInstanceID: command.ServiceInstanceID, ClientID: command.ClientMessageID, Input: input, Authorization: authorization,
	}, nil
}

// ConversationReceiptHash binds the complete text and immutable routing options to one receipt.
func ConversationReceiptHash(conversationID, content string, input ConversationRoutingInput) ([]byte, error) {
	return hashIdempotencyRequest(struct {
		ConversationID string                   `json:"conversation_id"`
		Content        string                   `json:"content"`
		Input          ConversationRoutingInput `json:"input"`
	}{conversationID, content, input})
}

// AcceptConversationMessage records the message before checking model or tool availability.
func (s *TaskService) AcceptConversationMessage(ctx context.Context, principal identity.Principal, conversationID string, command AcceptConversationMessageCommand) (conversation.Message, bool, error) {
	if !utf8.ValidString(command.Content) || strings.ContainsRune(command.Content, '\x00') || utf8.RuneCountInString(command.Content) > 32768 ||
		(strings.TrimSpace(command.Content) == "" && len(command.ClientAttachmentIDs) == 0) {
		return conversation.Message{}, false, domain.InvalidArgument("content", "message text or an attachment is required")
	}
	receipt, err := s.prepareConversationMessage(ctx, principal, conversationID, command)
	if err != nil {
		return conversation.Message{}, false, err
	}
	requestHash, err := ConversationReceiptHash(conversationID, command.Content, receipt.Input)
	if err != nil {
		return conversation.Message{}, false, err
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		return conversation.Message{}, false, err
	}
	store, ok := s.store.(ConversationMessageReceiptStore)
	if !ok {
		return conversation.Message{}, false, domain.NewError(domain.CodeUnsupported, "conversation receipt storage is unavailable")
	}
	receipt.Message.ID = messageID
	receipt.Message.Content = command.Content
	receipt.RequestHash = requestHash
	return store.AcceptConversationMessage(ctx, receipt)
}

func (s *TaskService) AcceptProjectMessage(ctx context.Context, principal identity.Principal, projectID, conversationID string, command AcceptConversationMessageCommand) (conversation.Message, bool, error) {
	if command.ProjectID != "" && command.ProjectID != projectID {
		return conversation.Message{}, false, domain.InvalidArgument("project_id", "project does not match the conversation scope")
	}
	command.ProjectID = projectID
	return s.AcceptConversationMessage(ctx, principal, conversationID, command)
}
