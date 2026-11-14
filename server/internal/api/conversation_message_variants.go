package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ConversationMessageVariantService interface {
	CreateConversationMessageVariant(context.Context, identity.Principal, string, string, string, string,
		app.ConversationMessageVariantAction, int64, string, string) (app.ConversationMessageVariantOperation, error)
}

type createConversationMessageVariantRequest struct {
	Action          app.ConversationMessageVariantAction `json:"action"`
	ExpectedVersion int64                                `json:"expected_version"`
	Content         string                               `json:"content,omitempty"`
}

type conversationMessageVariantOperationResponse struct {
	OperationID        string                               `json:"operation_id"`
	VariantGroupID     string                               `json:"variant_group_id"`
	VariantVersion     int64                                `json:"variant_version"`
	Action             app.ConversationMessageVariantAction `json:"action"`
	State              string                               `json:"state"`
	TaskID             string                               `json:"task_id,omitempty"`
	UserMessageID      string                               `json:"user_message_id,omitempty"`
	AssistantMessageID string                               `json:"assistant_message_id,omitempty"`
}

func (h *api) createConversationMessageVariant(service ConversationMessageVariantService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !requiredMessageActor(c, principal) {
			return
		}
		instanceID, ok := requiredMessageServiceInstance(c)
		if !ok {
			return
		}
		projectID, conversationID := "", c.Param("id")
		if c.Param("conversation_id") != "" {
			projectID, conversationID = c.Param("id"), c.Param("conversation_id")
			if !project.ValidID(projectID) {
				writeError(c, domain.NewError(domain.CodeNotFound, "project not found"))
				return
			}
		}
		if !domain.ValidConversationID(conversationID) {
			writeError(c, domain.NewError(domain.CodeNotFound, "conversation not found"))
			return
		}
		messageID, ok := safeIDParam(c, "message_id", "message_id", domain.ValidMessageID)
		if !ok {
			return
		}
		var request createConversationMessageVariantRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		operation, err := service.CreateConversationMessageVariant(c.Request.Context(), principal, projectID, conversationID,
			messageID, instanceID, request.Action, request.ExpectedVersion, request.Content, key)
		if err != nil {
			writeError(c, err)
			return
		}
		if !domain.ValidOperationID(operation.OperationID) || !domain.ValidMessageID(operation.VariantGroupID) ||
			operation.VariantVersion < 2 || operation.Action != request.Action || operation.State == "" ||
			(operation.TaskID != "" && !domain.ValidTaskID(operation.TaskID)) ||
			(operation.UserMessageID != "" && !domain.ValidMessageID(operation.UserMessageID)) ||
			(operation.AssistantMessageID != "" && !domain.ValidMessageID(operation.AssistantMessageID)) {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		c.JSON(http.StatusAccepted, conversationMessageVariantOperationResponse{
			OperationID: operation.OperationID, VariantGroupID: operation.VariantGroupID, VariantVersion: operation.VariantVersion,
			Action: operation.Action, State: operation.State, TaskID: operation.TaskID,
			UserMessageID: operation.UserMessageID, AssistantMessageID: operation.AssistantMessageID,
		})
	}
}
