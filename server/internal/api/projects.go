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

type projectRoutesService interface {
	Create(context.Context, identity.Principal, app.CreateProjectCommand) (project.Project, bool, error)
	List(context.Context, identity.Principal, app.PageRequest) (app.ProjectPage, error)
	Get(context.Context, identity.Principal, string) (project.Project, error)
	ListMembers(context.Context, identity.Principal, string, app.PageRequest) (app.ProjectMemberPage, error)
	Invite(context.Context, identity.Principal, string, app.InviteProjectMemberCommand) (project.Invitation, bool, error)
	ListInvitations(context.Context, identity.Principal, app.PageRequest) (app.ProjectInvitationPage, error)
	AcceptInvitation(context.Context, identity.Principal, string, string) (project.Member, bool, error)
	RevokeInvitation(context.Context, identity.Principal, string, string, int64, string) error
	UpdateMemberRole(context.Context, identity.Principal, string, string, app.UpdateProjectMemberCommand) (project.Member, error)
	RemoveMember(context.Context, identity.Principal, string, string, int64, string) error
	TransferOwner(context.Context, identity.Principal, string, string, int64, string) (project.Project, error)
	ListResources(context.Context, identity.Principal, string, app.PageRequest) (app.ProjectResourcePage, error)
	AddResource(context.Context, identity.Principal, string, project.ResourceKind, string, int64, string) (project.Resource, bool, error)
	RemoveResource(context.Context, identity.Principal, string, project.ResourceKind, string, int64, string) error
	GetWorkspaceManifest(context.Context, identity.Principal, string, string, int64, string, string, int32) (app.ProjectWorkspaceFilePage, error)
}

type projectCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type projectResponse struct {
	ID          string `json:"id"`
	OwnerID     string `json:"owner_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     int64  `json:"version"`
}

type projectPageResponse struct {
	Items      []projectResponse `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

type projectMemberResponse struct {
	ProjectID string       `json:"project_id"`
	UserID    string       `json:"user_id"`
	Role      project.Role `json:"role"`
	Version   int64        `json:"version"`
}

type projectMemberPageResponse struct {
	Items      []projectMemberResponse `json:"items"`
	NextCursor *string                 `json:"next_cursor"`
}

type projectInvitationResponse struct {
	ID        string                  `json:"id"`
	ProjectID string                  `json:"project_id"`
	InviterID string                  `json:"inviter_id"`
	InviteeID string                  `json:"invitee_id"`
	Role      project.Role            `json:"role"`
	State     project.InvitationState `json:"state"`
	Version   int64                   `json:"version"`
}

type projectInvitationPageResponse struct {
	Items      []projectInvitationResponse `json:"items"`
	NextCursor *string                     `json:"next_cursor"`
}

type projectMemberMutationRequest struct {
	ExpectedVersion *int64       `json:"expected_version"`
	Role            project.Role `json:"role"`
}

type projectInviteRequest struct {
	UserID          string       `json:"user_id"`
	Role            project.Role `json:"role"`
	ExpectedVersion *int64       `json:"expected_version"`
}

type projectInvitationMutationRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
}

type projectOwnerTransferRequest struct {
	UserID          string `json:"user_id"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type projectResourceRequest struct {
	Kind            project.ResourceKind `json:"kind"`
	ResourceID      string               `json:"resource_id"`
	ExpectedVersion *int64               `json:"expected_version"`
}

type projectResourceResponse struct {
	ProjectID  string               `json:"project_id"`
	Kind       project.ResourceKind `json:"kind"`
	ResourceID string               `json:"resource_id"`
	Version    int64                `json:"version"`
}

type projectResourcePageResponse struct {
	Items      []projectResourceResponse `json:"items"`
	NextCursor *string                   `json:"next_cursor"`
}

// RegisterProjectRoutes exposes the project handlers for W08b to mount after
// the shared OpenAPI contract and dependency wiring are ready.
func RegisterProjectRoutes(group *gin.RouterGroup, service projectRoutesService) {
	group = group.Group("")
	group.Use(func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			c.Abort()
			return
		}
		c.Next()
	})
	group.GET("/projects", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, projectToResponse(value))
		}
		c.JSON(http.StatusOK, projectPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/projects", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectCreateRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.Create(c.Request.Context(), principal, app.CreateProjectCommand{
			Name: request.Name, Description: request.Description, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, projectToResponse(value))
	})
	group.GET("/projects/:id", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		value, err := service.Get(c.Request.Context(), principal, c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectToResponse(value))
	})
	group.GET("/projects/:id/members", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListMembers(c.Request.Context(), principal, c.Param("id"), page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectMemberResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, memberToResponse(value))
		}
		c.JSON(http.StatusOK, projectMemberPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/projects/:id/members", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectInviteRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.Invite(c.Request.Context(), principal, c.Param("id"), app.InviteProjectMemberCommand{
			UserID: request.UserID, Role: request.Role, ExpectedVersion: *request.ExpectedVersion, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, invitationToResponse(value))
	})
	group.PATCH("/projects/:id/members/:user_id", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectMemberMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, err := service.UpdateMemberRole(c.Request.Context(), principal, c.Param("id"), c.Param("user_id"), app.UpdateProjectMemberCommand{
			Role: request.Role, ExpectedVersion: *request.ExpectedVersion, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, memberToResponse(value))
	})
	group.DELETE("/projects/:id/members/:user_id", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectMemberMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if err := service.RemoveMember(c.Request.Context(), principal, c.Param("id"), c.Param("user_id"), *request.ExpectedVersion, key); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	group.GET("/me/project-invitations", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListInvitations(c.Request.Context(), principal, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectInvitationResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, invitationToResponse(value))
		}
		c.JSON(http.StatusOK, projectInvitationPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.DELETE("/projects/:id/invitations/:invitation_id", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectInvitationMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "invitation version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if err := service.RevokeInvitation(c.Request.Context(), principal, c.Param("id"), c.Param("invitation_id"), *request.ExpectedVersion, key); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	group.POST("/project-invitations/:id/accept", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.AcceptInvitation(c.Request.Context(), principal, c.Param("id"), key)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, memberToResponse(value))
	})
	group.POST("/projects/:id/owner-transfer", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectOwnerTransferRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, err := service.TransferOwner(c.Request.Context(), principal, c.Param("id"), request.UserID, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectToResponse(value))
	})
	group.GET("/projects/:id/resources", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListResources(c.Request.Context(), principal, c.Param("id"), page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectResourceResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, resourceToResponse(value))
		}
		c.JSON(http.StatusOK, projectResourcePageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.GET("/projects/:id/workspaces/:workspace_id/manifest", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		workspaceID, ok := safeIDParam(c, "workspace_id", "workspace_id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		revision, err := optionalWorkspaceSyncInt(c, "revision", 0, 0, int64(^uint64(0)>>1))
		if err != nil {
			writeError(c, err)
			return
		}
		prefix, ok := workspaceSyncQueryText(c, "prefix", 1024, false)
		if !ok {
			return
		}
		cursor, ok := workspaceSyncQueryText(c, "cursor", 512, false)
		if !ok {
			return
		}
		limit, err := optionalWorkspaceSyncInt(c, "limit", 500, 1, 1000)
		if err != nil {
			writeError(c, err)
			return
		}
		result, err := service.GetWorkspaceManifest(c.Request.Context(), principal, projectID, workspaceID, revision, prefix, cursor, int32(limit))
		if err != nil {
			writeError(c, err)
			return
		}
		if result.Files == nil {
			result.Files = []app.ProjectWorkspaceFile{}
		}
		if result.EmptyDirectories == nil {
			result.EmptyDirectories = []string{}
		}
		c.JSON(http.StatusOK, result)
	})
	group.POST("/projects/:id/resources", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectResourceRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.AddResource(c.Request.Context(), principal, c.Param("id"), request.Kind, request.ResourceID, *request.ExpectedVersion, key)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, resourceToResponse(value))
	})
	group.DELETE("/projects/:id/resources", func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request projectResourceRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		if err := service.RemoveResource(c.Request.Context(), principal, c.Param("id"), request.Kind, request.ResourceID, *request.ExpectedVersion, key); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func projectToResponse(value project.Project) projectResponse {
	return projectResponse{ID: value.ID, OwnerID: value.OwnerID, Name: value.Name, Description: value.Description, Version: value.Version}
}

func memberToResponse(value project.Member) projectMemberResponse {
	return projectMemberResponse{ProjectID: value.ProjectID, UserID: value.UserID, Role: value.Role, Version: value.Version}
}

func invitationToResponse(value project.Invitation) projectInvitationResponse {
	return projectInvitationResponse{
		ID: value.ID, ProjectID: value.ProjectID, InviterID: value.InviterID, InviteeID: value.InviteeID,
		Role: value.Role, State: value.State, Version: value.Version,
	}
}

func resourceToResponse(value project.Resource) projectResourceResponse {
	return projectResourceResponse{ProjectID: value.ProjectID, Kind: value.Kind, ResourceID: value.ResourceID, Version: value.Version}
}
