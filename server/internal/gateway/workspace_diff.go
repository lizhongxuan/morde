package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
)

type hostWorkspaceDiffService interface {
	GetHostDiff(ctx context.Context, actorID, hostID, workspaceID string, fromRevision, toRevision int64) (app.WorkspaceDiff, error)
}

func (g *gateway) getHostWorkspaceDiff(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) != 2 || len(query["from_revision"]) != 1 || len(query["to_revision"]) != 1 {
		httpserver.WriteError(c, domain.InvalidArgument("revision", "from_revision and to_revision are required"))
		return
	}
	fromRevision, fromErr := strconv.ParseInt(query.Get("from_revision"), 10, 64)
	toRevision, toErr := strconv.ParseInt(query.Get("to_revision"), 10, 64)
	if fromErr != nil || toErr != nil || fromRevision < 0 || toRevision < 0 {
		httpserver.WriteError(c, domain.InvalidArgument("revision", "Workspace diff revisions are invalid"))
		return
	}
	workspaceID := c.Param("workspace_id")
	if !domain.ValidWorkspaceID(workspaceID) {
		writeNotFound(c)
		return
	}
	principal := principalFrom(c)
	ref := hostRefFrom(c)
	if principal.Kind != identity.KindHostCookie || principal.UserID != ref.OwnerID || ref.ID != c.GetString(hostIDKey) {
		writeNotFound(c)
		return
	}
	service, ok := g.deps.WorkspaceFiles.(hostWorkspaceDiffService)
	if !ok {
		httpserver.WriteError(c, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace diff is unavailable"))
		return
	}
	value, err := service.GetHostDiff(c.Request.Context(), principal.UserID, c.GetString(hostIDKey), workspaceID, fromRevision, toRevision)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}
