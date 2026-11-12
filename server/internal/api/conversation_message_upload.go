package api

import (
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

// A full 16 KiB chunk is base64-encoded in JSON (about 21.4 KiB).
const conversationMessageChunkJSONLimit = 24 << 10

type initializeMessageUploadRequest struct {
	TotalBytes              int                `json:"total_bytes"`
	TotalChunks             int                `json:"total_chunks"`
	SHA256                  string             `json:"sha256"`
	PreviousClientMessageID string             `json:"previous_client_message_id"`
	ReferenceTaskID         string             `json:"reference_task_id"`
	HostID                  string             `json:"host_id"`
	WorkspaceID             string             `json:"workspace_id"`
	ChangeTaskTarget        bool               `json:"change_task_target,omitempty"`
	MCPConnectorIDs         []string           `json:"mcp_connector_ids"`
	ClientAttachmentIDs     []string           `json:"client_attachment_ids"`
	ApplicationID           string             `json:"application_id"`
	ModelID                 string             `json:"model_id"`
	Budget                  *taskBudgetRequest `json:"budget"`
}

type messageUploadChunkRequest struct {
	ContentBase64 string `json:"content_base64"`
	SHA256        string `json:"sha256"`
}

type messageUploadResponse struct {
	ClientMessageID   string `json:"client_message_id"`
	State             string `json:"state"`
	TotalBytes        int    `json:"total_bytes"`
	TotalChunks       int    `json:"total_chunks"`
	ReceivedSequences []int  `json:"received_sequences"`
	Message           any    `json:"message,omitempty"`
}

type messageUploadScope struct {
	Principal      identity.Principal
	ProjectID      string
	ConversationID string
	ClientID       string
	InstanceID     string
}

func readMessageUploadScope(c *gin.Context) (messageUploadScope, bool) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return messageUploadScope{}, false
	}
	if !requiredMessageActor(c, principal) {
		return messageUploadScope{}, false
	}
	projectID, conversationID := "", c.Param("id")
	if c.Param("conversation_id") != "" {
		projectID, conversationID = c.Param("id"), c.Param("conversation_id")
		if !project.ValidID(projectID) {
			writeError(c, domain.NewError(domain.CodeNotFound, "project not found"))
			return messageUploadScope{}, false
		}
	}
	if !domain.ValidConversationID(conversationID) {
		writeError(c, domain.NewError(domain.CodeNotFound, "conversation not found"))
		return messageUploadScope{}, false
	}
	instanceID, ok := requiredMessageServiceInstance(c)
	if !ok {
		return messageUploadScope{}, false
	}
	return messageUploadScope{Principal: principal, ProjectID: projectID, ConversationID: conversationID,
		ClientID: c.Param("client_message_id"), InstanceID: instanceID}, true
}

func messageUploadView(value app.ConversationMessageUpload, projectID string) (messageUploadResponse, error) {
	result := messageUploadResponse{ClientMessageID: value.ClientMessageID, State: value.State,
		TotalBytes: value.TotalBytes, TotalChunks: value.TotalChunks, ReceivedSequences: append([]int{}, value.ReceivedSequences...)}
	if value.Message == nil {
		return result, nil
	}
	if projectID != "" {
		message, err := toProjectConversationMessageResponse(*value.Message)
		if err != nil {
			return messageUploadResponse{}, err
		}
		result.Message = message
	} else {
		message, err := toMessageResponse(*value.Message)
		if err != nil {
			return messageUploadResponse{}, err
		}
		result.Message = message
	}
	return result, nil
}

func writeAcceptedUploadedMessage(c *gin.Context, value conversation.Message, projectID string) {
	if projectID != "" {
		message, err := toProjectConversationMessageResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, projectConversationMessageResponseEnvelope{Message: message})
		return
	}
	message, err := toMessageResponse(value)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, conversationMessageResponseEnvelope{Message: message})
}

func registerConversationMessageUploads(group *gin.RouterGroup, service TaskService) {
	for _, base := range []string{
		"/conversations/:id/message-uploads/:client_message_id",
		"/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id",
	} {
		group.PUT(base, func(c *gin.Context) {
			scope, ok := readMessageUploadScope(c)
			if !ok {
				return
			}
			key, ok := requiredIdempotencyKey(c)
			if !ok {
				return
			}
			var request initializeMessageUploadRequest
			if !decodeStrictTaskJSON(c, &request) {
				return
			}
			budget, ok := requestBudget(c, request.Budget)
			if !ok {
				return
			}
			value, err := service.InitializeConversationMessageUpload(c.Request.Context(), scope.Principal, scope.ConversationID,
				app.InitializeConversationMessageUploadCommand{AcceptConversationMessageCommand: app.AcceptConversationMessageCommand{
					ProjectID: scope.ProjectID, ServiceInstanceID: scope.InstanceID, ClientMessageID: scope.ClientID, IdempotencyKey: key,
					PreviousClientMessageID: request.PreviousClientMessageID, ReferenceTaskID: request.ReferenceTaskID,
					HostID: request.HostID, WorkspaceID: request.WorkspaceID, ChangeTaskTarget: request.ChangeTaskTarget, MCPConnectorIDs: request.MCPConnectorIDs,
					ClientAttachmentIDs: request.ClientAttachmentIDs, ApplicationID: request.ApplicationID, ModelID: request.ModelID, Budget: budget,
				}, TotalBytes: request.TotalBytes, TotalChunks: request.TotalChunks, SHA256: request.SHA256})
			if err != nil {
				writeError(c, err)
				return
			}
			response, err := messageUploadView(value, scope.ProjectID)
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusOK, response)
		})
		group.PUT(base+"/chunks/:sequence", func(c *gin.Context) {
			scope, ok := readMessageUploadScope(c)
			if !ok {
				return
			}
			sequence, err := strconv.Atoi(c.Param("sequence"))
			if err != nil || sequence < 0 || strconv.Itoa(sequence) != c.Param("sequence") {
				writeError(c, domain.InvalidArgument("sequence", "message chunk sequence is invalid"))
				return
			}
			var request messageUploadChunkRequest
			if !decodeStrictTaskJSON(c, &request) {
				return
			}
			content, err := base64.StdEncoding.Strict().DecodeString(request.ContentBase64)
			if err != nil {
				writeError(c, domain.InvalidArgument("content_base64", "message chunk encoding is invalid"))
				return
			}
			if err := service.PutConversationMessageChunk(c.Request.Context(), scope.Principal, scope.ProjectID, scope.ConversationID,
				scope.ClientID, scope.InstanceID, sequence, content, request.SHA256); err != nil {
				writeError(c, err)
				return
			}
			c.Status(http.StatusNoContent)
		})
		group.GET(base, func(c *gin.Context) {
			scope, ok := readMessageUploadScope(c)
			if !ok {
				return
			}
			value, err := service.GetConversationMessageUpload(c.Request.Context(), scope.Principal, scope.ProjectID,
				scope.ConversationID, scope.ClientID, scope.InstanceID)
			if err != nil {
				writeError(c, err)
				return
			}
			response, err := messageUploadView(value, scope.ProjectID)
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(http.StatusOK, response)
		})
		group.POST(base+"/commit", func(c *gin.Context) {
			scope, ok := readMessageUploadScope(c)
			if !ok {
				return
			}
			key, ok := requiredIdempotencyKey(c)
			if !ok {
				return
			}
			value, err := service.CommitConversationMessageUpload(c.Request.Context(), scope.Principal, scope.ProjectID,
				scope.ConversationID, scope.ClientID, scope.InstanceID, key)
			if err != nil {
				writeError(c, err)
				return
			}
			writeAcceptedUploadedMessage(c, value, scope.ProjectID)
		})
	}
}
