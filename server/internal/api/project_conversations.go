package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectConversationRoutesService interface {
	List(context.Context, identity.Principal, string, app.PageRequest) (app.ConversationPage, error)
	Create(context.Context, identity.Principal, string, app.CreateProjectConversationCommand) (conversation.Conversation, bool, error)
	Get(context.Context, identity.Principal, string, string) (conversation.Conversation, error)
	ListMessages(context.Context, identity.Principal, string, string, app.PageRequest) (app.MessagePage, error)
	AcceptMessage(context.Context, identity.Principal, string, string, app.AcceptConversationMessageCommand) (conversation.Message, bool, error)
	ListTasks(context.Context, identity.Principal, string, string, app.PageRequest) (app.TaskPage, error)
}

type projectConversationResponse struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	OwnerID   string    `json:"owner_id"`
	Title     string    `json:"title"`
	ModelID   string    `json:"model_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type projectConversationPageResponse struct {
	Items      []projectConversationResponse `json:"items"`
	NextCursor *string                       `json:"next_cursor"`
}

type createProjectConversationRequest struct {
	Title string `json:"title"`
}

type projectConversationMessageResponse struct {
	messageResponse
	OwnerID string `json:"owner_id"`
	ActorID string `json:"actor_id"`
}

type projectConversationMessagePageResponse struct {
	Items      []projectConversationMessageResponse `json:"items"`
	NextCursor *string                              `json:"next_cursor"`
}

type projectConversationMessageResponseEnvelope struct {
	Message projectConversationMessageResponse `json:"message"`
}

type acceptedProjectConversationMessageResponse struct {
	Message  projectConversationMessageResponse `json:"message"`
	Replayed bool                               `json:"replayed"`
}

// RegisterProjectConversationRoutes exposes the shared conversation endpoints
// for project members. Mount these beside RegisterProjectRoutes.
func RegisterProjectConversationRoutes(group *gin.RouterGroup, service projectConversationRoutesService) {
	group = group.Group("")
	group.Use(func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			c.Abort()
			return
		}
		c.Next()
	})
	group.GET("/projects/:id/conversations", func(c *gin.Context) {
		principal, projectID, ok := projectConversationPrincipalAndID(c)
		if !ok {
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, projectID, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectConversationResponse, 0, len(result.Items))
		for _, value := range result.Items {
			item, err := toProjectConversationResponse(value)
			if err != nil {
				writeError(c, err)
				return
			}
			items = append(items, item)
		}
		c.JSON(http.StatusOK, projectConversationPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/projects/:id/conversations", func(c *gin.Context) {
		principal, projectID, ok := projectConversationPrincipalAndID(c)
		if !ok {
			return
		}
		instanceID, ok := optionalOutboxCreationScope(c, principal)
		if !ok {
			return
		}
		var request createProjectConversationRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.Create(c.Request.Context(), principal, projectID,
			app.CreateProjectConversationCommand{Title: request.Title, IdempotencyKey: key, ServiceInstanceID: instanceID})
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toProjectConversationResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, response)
	})
	group.GET("/projects/:id/conversations/:conversation_id", func(c *gin.Context) {
		principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
		if !ok {
			return
		}
		value, err := service.Get(c.Request.Context(), principal, projectID, conversationID)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := toProjectConversationResponse(value)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	})
	group.GET("/projects/:id/conversations/:conversation_id/messages", func(c *gin.Context) {
		principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
		if !ok {
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		var result app.MessagePage
		var err error
		if clientID := c.Query("client_message_id"); clientID != "" {
			if !requiredMessageActor(c, principal) {
				return
			}
			instanceID, ok := requiredMessageServiceInstance(c)
			if !ok {
				return
			}
			lookup, ok := service.(interface {
				FindMessageByClientID(context.Context, identity.Principal, string, string, string, string) (app.MessagePage, error)
			})
			if !ok {
				serviceUnavailable(c)
				return
			}
			result, err = lookup.FindMessageByClientID(c.Request.Context(), principal, projectID, conversationID, clientID, instanceID)
		} else {
			result, err = service.ListMessages(c.Request.Context(), principal, projectID, conversationID, page)
		}
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectConversationMessageResponse, 0, len(result.Items))
		for _, value := range result.Items {
			item, err := toProjectConversationMessageResponse(value)
			if err != nil {
				writeError(c, err)
				return
			}
			items = append(items, item)
		}
		c.JSON(http.StatusOK, projectConversationMessagePageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/projects/:id/conversations/:conversation_id/messages", acceptProjectConversationMessage(service))
	group.GET("/projects/:id/conversations/:conversation_id/tasks", func(c *gin.Context) {
		principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
		if !ok {
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListTasks(c.Request.Context(), principal, projectID, conversationID, page)
		if err != nil {
			writeError(c, err)
			return
		}
		writeConversationActiveTasks(c, result)
	})
}

func acceptProjectConversationMessage(service projectConversationRoutesService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, projectID, conversationID, ok := projectConversationPrincipalAndConversationID(c)
		if !ok {
			return
		}
		if !requiredMessageActor(c, principal) {
			return
		}
		var request conversationMessageRequest
		if !decodeStrictTaskJSON(c, &request) {
			return
		}
		instanceID, ok := requiredMessageServiceInstance(c)
		if !ok {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if request.ClientMessageID == "" || request.ClientMessageID != key {
			writeError(c, domain.InvalidArgument("client_message_id", "client message id and idempotency key must match"))
			return
		}
		budget, ok := requestBudget(c, request.Budget)
		if !ok {
			return
		}
		message, replayed, err := service.AcceptMessage(c.Request.Context(), principal, projectID, conversationID, app.AcceptConversationMessageCommand{
			ProjectID: projectID, ServiceInstanceID: instanceID, ClientMessageID: request.ClientMessageID,
			PreviousClientMessageID: request.PreviousClientMessageID, ReferenceTaskID: request.ReferenceTaskID,
			HostID: request.HostID, WorkspaceID: request.WorkspaceID, ChangeTaskTarget: request.ChangeTaskTarget,
			MCPConnectorIDs:     request.MCPConnectorIDs,
			ClientAttachmentIDs: request.ClientAttachmentIDs, ApplicationID: request.ApplicationID,
			ModelID: request.ModelID, TimeZone: request.TimeZone, Content: request.Content, Budget: budget, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		message.ClientMessageID = request.ClientMessageID
		view, err := toProjectConversationMessageResponse(message)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, acceptedProjectConversationMessageResponse{Message: view, Replayed: replayed})
	}
}

func projectConversationPrincipalAndID(c *gin.Context) (identity.Principal, string, bool) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return identity.Principal{}, "", false
	}
	projectID, ok := safeID(c, "id", project.ValidID)
	if !ok {
		return identity.Principal{}, "", false
	}
	return principal, projectID, true
}

func projectConversationPrincipalAndConversationID(c *gin.Context) (identity.Principal, string, string, bool) {
	principal, projectID, ok := projectConversationPrincipalAndID(c)
	if !ok {
		return identity.Principal{}, "", "", false
	}
	conversationID, ok := safeIDParam(c, "conversation_id", "conversation_id", domain.ValidConversationID)
	if !ok {
		return identity.Principal{}, "", "", false
	}
	return principal, projectID, conversationID, true
}

func toProjectConversationResponse(value conversation.Conversation) (projectConversationResponse, error) {
	if !domain.ValidConversationID(value.ID) || !project.ValidID(value.ProjectID) || !domain.ValidUserID(value.OwnerID) ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return projectConversationResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return projectConversationResponse{ID: value.ID, ProjectID: value.ProjectID, OwnerID: value.OwnerID,
		Title: value.Title, ModelID: value.ModelID, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}, nil
}

func toProjectConversationMessageResponse(value conversation.Message) (projectConversationMessageResponse, error) {
	if !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.ActorID) {
		return projectConversationMessageResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	base, err := toScopedMessageResponse(value, true)
	if err != nil {
		return projectConversationMessageResponse{}, err
	}
	return projectConversationMessageResponse{messageResponse: base, OwnerID: value.OwnerID, ActorID: value.ActorID}, nil
}
