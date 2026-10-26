package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/site"
)

type createSiteRequest struct {
	HostID      string `json:"host_id"`
	WorkspaceID string `json:"workspace_id"`
}

type createSiteCandidateRequest struct {
	WorkspaceRevision int64 `json:"workspace_revision"`
	ServicePort       int   `json:"service_port"`
}

type siteResponse struct {
	ID          string    `json:"id"`
	HostID      string    `json:"host_id"`
	WorkspaceID string    `json:"workspace_id"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type siteReleaseResponse struct {
	ID                string            `json:"id"`
	SiteID            string            `json:"site_id"`
	WorkspaceID       string            `json:"workspace_id"`
	WorkspaceRevision int64             `json:"workspace_revision"`
	ManifestSHA256    string            `json:"manifest_sha256"`
	ServicePort       int               `json:"service_port"`
	State             site.ReleaseState `json:"state"`
	ArtifactFileCount int64             `json:"artifact_file_count"`
	ArtifactBytes     int64             `json:"artifact_bytes"`
	MaterializedAt    *time.Time        `json:"materialized_at,omitempty"`
	FailureCode       site.FailureCode  `json:"failure_code,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type sitePageResponse struct {
	Items      []siteResponse `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

type siteReleasePageResponse struct {
	Items      []siteReleaseResponse `json:"items"`
	NextCursor *string               `json:"next_cursor"`
}

func (h *api) registerSites(group *gin.RouterGroup) {
	group.GET("/sites", h.listSites)
	group.POST("/sites", h.createSite)
	group.GET("/sites/:id/releases", h.listSiteReleases)
	group.POST("/sites/:id/releases", h.createSiteCandidate)
	group.POST("/sites/:id/releases/:release_id/materialize", h.materializeSiteRelease)
	group.GET("/sites/:id/releases/:release_id", h.getSiteRelease)
}

func (h *api) listSites(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	page, ok := sitePageRequest(c)
	if !ok {
		return
	}
	result, err := h.deps.Sites.List(c.Request.Context(), principal, page)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]siteResponse, 0, len(result.Items))
	for _, value := range result.Items {
		items = append(items, siteView(value))
	}
	c.JSON(http.StatusOK, sitePageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
}

func (h *api) listSiteReleases(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	siteID, ok := safeID(c, "site_id", func(value string) bool { return site.ValidID(site.ID(value)) })
	if !ok {
		return
	}
	page, ok := sitePageRequest(c)
	if !ok {
		return
	}
	result, err := h.deps.Sites.ListReleases(c.Request.Context(), principal, site.ID(siteID), page)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]siteReleaseResponse, 0, len(result.Items))
	for _, value := range result.Items {
		items = append(items, siteReleaseView(value))
	}
	c.JSON(http.StatusOK, siteReleasePageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
}

func (h *api) createSite(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	key, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	var request createSiteRequest
	if !decodeJSON(c, &request) {
		return
	}
	if !domain.ValidHostID(request.HostID) {
		writeError(c, domain.InvalidArgument("host_id", "invalid host_id"))
		return
	}
	if !domain.ValidWorkspaceID(request.WorkspaceID) {
		writeError(c, domain.InvalidArgument("workspace_id", "invalid workspace_id"))
		return
	}
	value, err := h.deps.Sites.Create(c.Request.Context(), principal, request.HostID, request.WorkspaceID, key)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, siteView(value))
}

func siteView(value site.Site) siteResponse {
	return siteResponse{ID: string(value.ID), HostID: value.HostID, WorkspaceID: value.WorkspaceID, State: string(value.State), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
}

func (h *api) createSiteCandidate(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	siteID, ok := safeID(c, "site_id", func(value string) bool { return site.ValidID(site.ID(value)) })
	if !ok {
		return
	}
	key, ok := requiredIdempotencyKey(c)
	if !ok {
		return
	}
	var request createSiteCandidateRequest
	if !decodeJSON(c, &request) {
		return
	}
	if request.ServicePort < h.options.PreviewPortMin || !domain.ValidPreviewTargetPort(request.ServicePort) || previewPortDenied(request.ServicePort, h.options.PreviewPortDeny) {
		writeError(c, domain.InvalidArgument("service_port", "service port is not allowed"))
		return
	}
	value, err := h.deps.Sites.CreateCandidate(c.Request.Context(), principal, site.ID(siteID), request.WorkspaceRevision, request.ServicePort, key)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, siteReleaseView(value))
}

func (h *api) materializeSiteRelease(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	siteID, releaseID, ok := siteReleaseIDs(c)
	if !ok {
		return
	}
	value, err := h.deps.Sites.Materialize(c.Request.Context(), principal, siteID, releaseID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, siteReleaseView(value))
}

func (h *api) getSiteRelease(c *gin.Context) {
	if h.deps.Sites == nil {
		serviceUnavailable(c)
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	siteID, releaseID, ok := siteReleaseIDs(c)
	if !ok {
		return
	}
	value, err := h.deps.Sites.GetRelease(c.Request.Context(), principal, siteID, releaseID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, siteReleaseView(value))
}

func siteReleaseIDs(c *gin.Context) (site.ID, site.ReleaseID, bool) {
	siteID, ok := safeID(c, "site_id", func(value string) bool { return site.ValidID(site.ID(value)) })
	if !ok {
		return "", "", false
	}
	releaseID, ok := safeIDParam(c, "release_id", "release_id", func(value string) bool { return site.ValidReleaseID(site.ReleaseID(value)) })
	if !ok {
		return "", "", false
	}
	return site.ID(siteID), site.ReleaseID(releaseID), true
}

func siteReleaseView(value site.Release) siteReleaseResponse {
	return siteReleaseResponse{
		ID: string(value.ID), SiteID: string(value.SiteID), WorkspaceID: value.WorkspaceID,
		WorkspaceRevision: value.WorkspaceRevision, ManifestSHA256: value.ManifestSHA256, ServicePort: value.ServicePort,
		State: value.State, ArtifactFileCount: value.ArtifactFileCount, ArtifactBytes: value.ArtifactBytes,
		MaterializedAt: value.MaterializedAt, FailureCode: value.FailureCode, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func sitePageRequest(c *gin.Context) (app.PageRequest, bool) {
	page, ok := requestPage(c)
	if !ok {
		return app.PageRequest{}, false
	}
	if _, exists := c.Request.URL.Query()["cursor"]; exists && page.Cursor == "" {
		writeError(c, domain.InvalidArgument("cursor", "page cursor is invalid"))
		return app.PageRequest{}, false
	}
	return page, true
}

func requiredIdempotencyKey(c *gin.Context) (string, bool) {
	keys := c.Request.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeError(c, domain.InvalidArgument("Idempotency-Key", "exactly one idempotency key is required"))
		return "", false
	}
	return keys[0], true
}
