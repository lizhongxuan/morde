package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectContentRoutesService interface {
	GetContent(context.Context, identity.Principal, string, project.ContentKind, string) (project.Content, error)
	MutateContent(context.Context, identity.Principal, string, app.MutateProjectContentCommand) (project.Content, bool, error)
	SearchContent(context.Context, identity.Principal, string, int) ([]project.ContentSearchResult, error)
	ListContentComments(context.Context, identity.Principal, string, project.ContentKind, string, int64, string) ([]project.ContentComment, error)
	MutateContentComment(context.Context, identity.Principal, string, app.MutateProjectContentCommentCommand) (project.ContentComment, bool, error)
}

type projectContentMutationRequest struct {
	ContentID    string          `json:"content_id"`
	BaseRevision *int64          `json:"base_revision"`
	Body         json.RawMessage `json:"body"`
}

type projectContentDeleteRequest struct {
	ContentID    string `json:"content_id"`
	BaseRevision *int64 `json:"base_revision"`
}

type projectContentResponse struct {
	ProjectID string              `json:"project_id"`
	Kind      project.ContentKind `json:"kind"`
	ID        string              `json:"id"`
	Revision  int64               `json:"revision"`
	Body      json.RawMessage     `json:"body"`
	UpdatedBy string              `json:"updated_by"`
}

type projectContentDeleteResponse struct {
	ProjectID string              `json:"project_id"`
	Kind      project.ContentKind `json:"kind"`
	ID        string              `json:"id"`
	Revision  int64               `json:"revision"`
	UpdatedBy string              `json:"updated_by"`
	Deleted   bool                `json:"deleted"`
}

type projectContentSearchResponse struct {
	Items []project.ContentSearchResult `json:"items"`
}

type projectContentCommentCreateRequest struct {
	ContentID       string `json:"content_id"`
	ContentRevision *int64 `json:"content_revision"`
	RowID           string `json:"row_id"`
	Body            string `json:"body"`
}

type projectContentCommentMutationRequest struct {
	BaseVersion *int64 `json:"base_version"`
	Body        string `json:"body"`
}

type projectContentCommentDeleteRequest struct {
	BaseVersion *int64 `json:"base_version"`
}

type projectContentCommentResponse struct {
	ProjectID       string              `json:"project_id"`
	Kind            project.ContentKind `json:"kind"`
	ContentID       string              `json:"content_id"`
	ContentRevision int64               `json:"content_revision"`
	RowID           string              `json:"row_id"`
	ID              string              `json:"id"`
	Revision        int64               `json:"revision"`
	Body            string              `json:"body,omitempty"`
	UpdatedBy       string              `json:"updated_by"`
	Deleted         bool                `json:"deleted,omitempty"`
}

// RegisterProjectContentRoutes exposes project-scoped memory, goal, and document handlers for W08b.
func RegisterProjectContentRoutes(group *gin.RouterGroup, service projectContentRoutesService) {
	group.GET("/project-content/search", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		query, exists := c.Request.URL.Query()["q"]
		if !exists || len(query) != 1 {
			writeError(c, domain.InvalidArgument("q", "project content search query is required"))
			return
		}
		if err := project.ValidateContentSearchQuery(query[0]); err != nil {
			writeError(c, err)
			return
		}
		limit := 20
		if values, exists := c.Request.URL.Query()["limit"]; exists {
			if len(values) != 1 {
				writeError(c, domain.InvalidArgument("limit", "project content search limit is invalid"))
				return
			}
			parsed, err := strconv.Atoi(values[0])
			if err != nil {
				writeError(c, domain.InvalidArgument("limit", "project content search limit is invalid"))
				return
			}
			limit = parsed
		}
		if limit < 1 || limit > project.MaxContentSearchResults {
			writeError(c, domain.InvalidArgument("limit", "project content search limit is invalid"))
			return
		}
		items, err := service.SearchContent(c.Request.Context(), principal, query[0], limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectContentSearchResponse{Items: items})
	})
	group.GET("/projects/:id/content/:kind", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		kind, ok := projectContentPath(c)
		if !ok {
			return
		}
		contentID, ok := projectContentIDFromQuery(c)
		if !ok {
			return
		}
		value, err := service.GetContent(c.Request.Context(), principal, projectID, kind, contentID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectContentResponse(value))
	})
	group.PUT("/projects/:id/content/:kind", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		kind, ok := projectContentPath(c)
		if !ok {
			return
		}
		var request projectContentMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil {
			writeError(c, domain.InvalidArgument("base_revision", "project content revision is required"))
			return
		}
		if !project.ValidContentID(request.ContentID) {
			writeError(c, domain.InvalidArgument("content_id", "project content id is invalid"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.MutateContent(c.Request.Context(), principal, projectID, app.MutateProjectContentCommand{
			Kind: kind, ContentID: request.ContentID, BaseRevision: *request.BaseRevision,
			Body: request.Body, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusOK
		if !replay && *request.BaseRevision == 0 {
			status = http.StatusCreated
		}
		c.JSON(status, toProjectContentResponse(value))
	})
	group.DELETE("/projects/:id/content/:kind", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		kind, ok := projectContentPath(c)
		if !ok {
			return
		}
		var request projectContentDeleteRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil {
			writeError(c, domain.InvalidArgument("base_revision", "project content revision is required"))
			return
		}
		if !project.ValidContentID(request.ContentID) {
			writeError(c, domain.InvalidArgument("content_id", "project content id is invalid"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, _, err := service.MutateContent(c.Request.Context(), principal, projectID, app.MutateProjectContentCommand{
			Kind: kind, ContentID: request.ContentID, BaseRevision: *request.BaseRevision,
			Delete: true, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, projectContentDeleteResponse{
			ProjectID: value.ProjectID, Kind: value.Kind, ID: value.ID, Revision: value.Revision,
			UpdatedBy: value.UpdatedBy, Deleted: true,
		})
	})
	group.GET("/projects/:id/content/:kind/comments", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, kind, contentID, contentRevision, rowID, ok := projectContentCommentTarget(c)
		if !ok {
			return
		}
		items, err := service.ListContentComments(c.Request.Context(), principal, projectID, kind, contentID, contentRevision, rowID)
		if err != nil {
			writeError(c, err)
			return
		}
		responses := make([]projectContentCommentResponse, 0, len(items))
		for _, item := range items {
			responses = append(responses, toProjectContentCommentResponse(item))
		}
		c.JSON(http.StatusOK, projectContentCommentListResponse{Items: responses})
	})
	group.POST("/projects/:id/content/:kind/comments", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, kind, ok := projectContentProjectAndKind(c)
		if !ok {
			return
		}
		var request projectContentCommentCreateRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ContentRevision == nil || !project.ValidContentID(request.ContentID) || !project.ValidContentID(request.RowID) {
			writeError(c, domain.InvalidArgument("comment", "project content comment target is invalid"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, replay, err := service.MutateContentComment(c.Request.Context(), principal, projectID, app.MutateProjectContentCommentCommand{
			Kind: kind, ContentID: request.ContentID, ContentRevision: *request.ContentRevision, RowID: request.RowID,
			Body: request.Body, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, toProjectContentCommentResponse(value))
	})
	group.PUT("/projects/:id/content/:kind/comments/:comment_id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, kind, contentID, contentRevision, rowID, ok := projectContentCommentTarget(c)
		if !ok {
			return
		}
		commentID := c.Param("comment_id")
		if !project.ValidCommentID(commentID) {
			writeError(c, domain.InvalidArgument("comment_id", "project content comment id is invalid"))
			return
		}
		var request projectContentCommentMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseVersion == nil {
			writeError(c, domain.InvalidArgument("base_version", "project content comment version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, _, err := service.MutateContentComment(c.Request.Context(), principal, projectID, app.MutateProjectContentCommentCommand{
			Kind: kind, ContentID: contentID, ContentRevision: contentRevision, RowID: rowID, CommentID: commentID,
			BaseVersion: *request.BaseVersion, Body: request.Body, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectContentCommentResponse(value))
	})
	group.DELETE("/projects/:id/content/:kind/comments/:comment_id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, kind, contentID, contentRevision, rowID, ok := projectContentCommentTarget(c)
		if !ok {
			return
		}
		commentID := c.Param("comment_id")
		if !project.ValidCommentID(commentID) {
			writeError(c, domain.InvalidArgument("comment_id", "project content comment id is invalid"))
			return
		}
		var request projectContentCommentDeleteRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseVersion == nil {
			writeError(c, domain.InvalidArgument("base_version", "project content comment version is required"))
			return
		}
		key, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, _, err := service.MutateContentComment(c.Request.Context(), principal, projectID, app.MutateProjectContentCommentCommand{
			Kind: kind, ContentID: contentID, ContentRevision: contentRevision, RowID: rowID, CommentID: commentID,
			BaseVersion: *request.BaseVersion, Delete: true, IdempotencyKey: key,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectContentCommentResponse(value))
	})
}

type projectContentCommentListResponse struct {
	Items []projectContentCommentResponse `json:"items"`
}

func projectContentProjectAndKind(c *gin.Context) (string, project.ContentKind, bool) {
	projectID, ok := safeID(c, "project_id", project.ValidID)
	if !ok {
		return "", "", false
	}
	kind, ok := projectContentPath(c)
	if !ok || !project.ValidMuseContentKind(kind) {
		if ok {
			writeError(c, domain.InvalidArgument("kind", "project Muse content kind is invalid"))
		}
		return "", "", false
	}
	return projectID, kind, true
}

func projectContentCommentTarget(c *gin.Context) (string, project.ContentKind, string, int64, string, bool) {
	projectID, kind, ok := projectContentProjectAndKind(c)
	if !ok {
		return "", "", "", 0, "", false
	}
	query := c.Request.URL.Query()
	contentIDValues, contentExists := query["content_id"]
	revisionValues, revisionExists := query["content_revision"]
	rowIDValues, rowExists := query["row_id"]
	if !contentExists || len(contentIDValues) != 1 || !project.ValidContentID(contentIDValues[0]) ||
		!revisionExists || len(revisionValues) != 1 || !rowExists || len(rowIDValues) != 1 || !project.ValidContentID(rowIDValues[0]) {
		writeError(c, domain.InvalidArgument("comment_target", "project content comment target is invalid"))
		return "", "", "", 0, "", false
	}
	revision, err := strconv.ParseInt(revisionValues[0], 10, 64)
	if err != nil || revision < 1 {
		writeError(c, domain.InvalidArgument("content_revision", "project content revision is invalid"))
		return "", "", "", 0, "", false
	}
	return projectID, kind, contentIDValues[0], revision, rowIDValues[0], true
}

func projectContentPath(c *gin.Context) (project.ContentKind, bool) {
	kind := project.ContentKind(c.Param("kind"))
	if !project.ValidContentKind(kind) {
		writeError(c, domain.InvalidArgument("kind", "project content kind is invalid"))
		return "", false
	}
	return kind, true
}

func projectContentIDFromQuery(c *gin.Context) (string, bool) {
	values, exists := c.Request.URL.Query()["content_id"]
	if !exists || len(values) != 1 || !project.ValidContentID(values[0]) {
		writeError(c, domain.InvalidArgument("content_id", "project content id is invalid"))
		return "", false
	}
	return values[0], true
}

func toProjectContentResponse(value project.Content) projectContentResponse {
	return projectContentResponse{
		ProjectID: value.ProjectID, Kind: value.Kind, ID: value.ID, Revision: value.Revision,
		Body: append(json.RawMessage(nil), value.Body...), UpdatedBy: value.UpdatedBy,
	}
}

func toProjectContentCommentResponse(value project.ContentComment) projectContentCommentResponse {
	return projectContentCommentResponse{
		ProjectID: value.ProjectID, Kind: value.Kind, ContentID: value.ContentID, ContentRevision: value.ContentRevision,
		RowID: value.RowID, ID: value.ID, Revision: value.Revision, Body: value.Body, UpdatedBy: value.UpdatedBy, Deleted: value.Deleted,
	}
}
