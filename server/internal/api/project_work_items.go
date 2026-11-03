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

type projectWorkItemRoutesService interface {
	List(context.Context, identity.Principal, string, app.PageRequest) (app.ProjectWorkItemPage, error)
	Get(context.Context, identity.Principal, string, string) (project.WorkItem, error)
	Create(context.Context, identity.Principal, string, app.CreateProjectWorkItemCommand) (project.WorkItem, bool, error)
	Assign(context.Context, identity.Principal, string, string, string, int64, string) (project.WorkItem, error)
	Block(context.Context, identity.Principal, string, string, string, int64, string) (project.WorkItem, error)
	Resume(context.Context, identity.Principal, string, string, int64, string) (project.WorkItem, error)
	LinkTask(context.Context, identity.Principal, string, string, string, int64, string) (project.WorkItem, error)
	Deliver(context.Context, identity.Principal, string, string, project.WorkItemDelivery, int64, string) (project.WorkItem, error)
}

type projectWorkItemResponse struct {
	ID                 string                           `json:"id"`
	ProjectID          string                           `json:"project_id"`
	Title              string                           `json:"title"`
	Description        string                           `json:"description"`
	AcceptanceCriteria []string                         `json:"acceptance_criteria"`
	AssigneeID         string                           `json:"assignee_id"`
	State              project.WorkItemState            `json:"state"`
	BlockedReason      string                           `json:"blocked_reason"`
	TaskIDs            []string                         `json:"task_ids"`
	Delivery           *projectWorkItemDeliveryResponse `json:"delivery,omitempty"`
	Version            int64                            `json:"version"`
}

type projectWorkItemDeliveryResponse struct {
	TaskID      string `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	Revision    int64  `json:"revision"`
}

type projectWorkItemPageResponse struct {
	Items      []projectWorkItemResponse `json:"items"`
	NextCursor *string                   `json:"next_cursor"`
}

type createProjectWorkItemRequest struct {
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	AssigneeID         string   `json:"assignee_id"`
}

type assignProjectWorkItemRequest struct {
	AssigneeID      string `json:"assignee_id"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type blockProjectWorkItemRequest struct {
	BlockedReason   string `json:"blocked_reason"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type projectWorkItemVersionRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
}

type linkProjectWorkItemTaskRequest struct {
	TaskID          string `json:"task_id"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type deliverProjectWorkItemRequest struct {
	TaskID          string `json:"task_id"`
	WorkspaceID     string `json:"workspace_id"`
	Revision        int64  `json:"revision"`
	ExpectedVersion *int64 `json:"expected_version"`
}

// RegisterProjectWorkItemRoutes exposes the project work-item handlers for W08b to mount.
func RegisterProjectWorkItemRoutes(group *gin.RouterGroup, service projectWorkItemRoutesService) {
	group = group.Group("")
	group.Use(func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			c.Abort()
			return
		}
		c.Next()
	})
	group.GET("/projects/:id/work-items", func(c *gin.Context) {
		principal, projectID, ok := projectWorkItemPrincipalAndID(c)
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
		items := make([]projectWorkItemResponse, 0, len(result.Items))
		for _, item := range result.Items {
			items = append(items, projectWorkItemToResponse(item))
		}
		c.JSON(http.StatusOK, projectWorkItemPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/projects/:id/work-items", func(c *gin.Context) {
		principal, projectID, ok := projectWorkItemPrincipalAndID(c)
		if !ok {
			return
		}
		var request createProjectWorkItemRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, replay, err := service.Create(c.Request.Context(), principal, projectID, app.CreateProjectWorkItemCommand{
			Title: request.Title, Description: request.Description, AcceptanceCriteria: request.AcceptanceCriteria,
			AssigneeID: request.AssigneeID, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, projectWorkItemToResponse(item))
	})
	group.GET("/projects/:id/work-items/:work_item_id", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		item, err := service.Get(c.Request.Context(), principal, projectID, itemID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
	group.PATCH("/projects/:id/work-items/:work_item_id", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		var request assignProjectWorkItemRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "work item version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, err := service.Assign(c.Request.Context(), principal, projectID, itemID, request.AssigneeID, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
	group.POST("/projects/:id/work-items/:work_item_id/block", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		var request blockProjectWorkItemRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "work item version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, err := service.Block(c.Request.Context(), principal, projectID, itemID, request.BlockedReason, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
	group.POST("/projects/:id/work-items/:work_item_id/resume", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		var request projectWorkItemVersionRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "work item version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, err := service.Resume(c.Request.Context(), principal, projectID, itemID, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
	group.POST("/projects/:id/work-items/:work_item_id/tasks", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		var request linkProjectWorkItemTaskRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "work item version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, err := service.LinkTask(c.Request.Context(), principal, projectID, itemID, request.TaskID, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
	group.POST("/projects/:id/work-items/:work_item_id/delivery", func(c *gin.Context) {
		principal, projectID, itemID, ok := projectWorkItemPrincipalProjectAndItemID(c)
		if !ok {
			return
		}
		var request deliverProjectWorkItemRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "work item version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		item, err := service.Deliver(c.Request.Context(), principal, projectID, itemID, project.WorkItemDelivery{
			TaskID: request.TaskID, WorkspaceID: request.WorkspaceID, Revision: request.Revision,
		}, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectWorkItemToResponse(item))
	})
}

func projectWorkItemPrincipalAndID(c *gin.Context) (identity.Principal, string, bool) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return identity.Principal{}, "", false
	}
	projectID, ok := safeID(c, "project_id", project.ValidID)
	if !ok {
		return identity.Principal{}, "", false
	}
	return principal, projectID, true
}

func projectWorkItemPrincipalProjectAndItemID(c *gin.Context) (identity.Principal, string, string, bool) {
	principal, projectID, ok := projectWorkItemPrincipalAndID(c)
	if !ok {
		return identity.Principal{}, "", "", false
	}
	itemID, ok := safeID(c, "work_item_id", project.ValidID)
	if !ok {
		return identity.Principal{}, "", "", false
	}
	return principal, projectID, itemID, true
}

func projectWorkItemToResponse(value project.WorkItem) projectWorkItemResponse {
	response := projectWorkItemResponse{
		ID: value.ID, ProjectID: value.ProjectID, Title: value.Title, Description: value.Description,
		AcceptanceCriteria: append([]string{}, value.AcceptanceCriteria...), AssigneeID: value.AssigneeID,
		State: value.State, BlockedReason: value.BlockedReason, TaskIDs: append([]string{}, value.TaskIDs...), Version: value.Version,
	}
	if value.Delivery != nil {
		response.Delivery = &projectWorkItemDeliveryResponse{
			TaskID: value.Delivery.TaskID, WorkspaceID: value.Delivery.WorkspaceID, Revision: value.Delivery.Revision,
		}
	}
	return response
}
