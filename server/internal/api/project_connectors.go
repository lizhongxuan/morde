package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectMCPConnectorRoutesService interface {
	List(context.Context, identity.Principal, string) ([]app.ProjectMCPConnector, error)
	Grant(context.Context, identity.Principal, string, string, int64) (app.ProjectMCPConnector, error)
	Revoke(context.Context, identity.Principal, string, string, int64) (app.ProjectMCPConnector, error)
}

type projectMCPConnectorResponse struct {
	ConnectorOwnerID  string     `json:"connector_owner_id"`
	ConnectorID       string     `json:"connector_id"`
	Name              string     `json:"name,omitempty"`
	Enabled           bool       `json:"enabled"`
	ConnectorRevision int64      `json:"connector_revision,omitempty"`
	GrantVersion      int64      `json:"grant_version"`
	Active            bool       `json:"active"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type projectMCPConnectorListResponse struct {
	Items []projectMCPConnectorResponse `json:"items"`
}

type projectMCPConnectorVersionRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
}

func RegisterProjectMCPConnectorRoutes(group *gin.RouterGroup, service projectMCPConnectorRoutesService) {
	group.GET("/projects/:id/mcp-connectors", func(c *gin.Context) {
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
		items, err := service.List(c.Request.Context(), principal, projectID)
		if err != nil {
			writeError(c, err)
			return
		}
		response := projectMCPConnectorListResponse{Items: make([]projectMCPConnectorResponse, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, toProjectMCPConnectorResponse(item))
		}
		c.JSON(http.StatusOK, response)
	})

	group.PUT("/projects/:id/mcp-connectors/:connector_id", func(c *gin.Context) {
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
		connectorID, ok := safeIDParam(c, "connector_id", "connector_id", func(value string) bool { return domain.ValidMCPConnectorID(value) })
		if !ok {
			return
		}
		var request projectMCPConnectorVersionRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project connector grant version is required"))
			return
		}
		value, err := service.Grant(c.Request.Context(), principal, projectID, connectorID, *request.ExpectedVersion)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectMCPConnectorResponse(value))
	})

	group.DELETE("/projects/:id/mcp-connectors/:connector_id", func(c *gin.Context) {
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
		connectorID, ok := safeIDParam(c, "connector_id", "connector_id", func(value string) bool { return domain.ValidMCPConnectorID(value) })
		if !ok {
			return
		}
		var request projectMCPConnectorVersionRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 {
			writeError(c, domain.InvalidArgument("expected_version", "a positive project connector grant version is required"))
			return
		}
		value, err := service.Revoke(c.Request.Context(), principal, projectID, connectorID, *request.ExpectedVersion)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectMCPConnectorResponse(value))
	})
}

func toProjectMCPConnectorResponse(value app.ProjectMCPConnector) projectMCPConnectorResponse {
	return projectMCPConnectorResponse{
		ConnectorOwnerID: value.Grant.ConnectorOwnerID, ConnectorID: value.Grant.ConnectorID,
		Name: value.Name, Enabled: value.Enabled, ConnectorRevision: value.ConnectorRevision,
		GrantVersion: value.Grant.Version, Active: value.Grant.Active, RevokedAt: value.Grant.RevokedAt,
		CreatedAt: value.Grant.CreatedAt, UpdatedAt: value.Grant.UpdatedAt,
	}
}
