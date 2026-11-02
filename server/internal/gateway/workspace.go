package gateway

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/httpserver"
)

type hostWorkspaceFileService interface {
	ListHostWorkspaceBindings(context.Context, string, string) ([]workspace.Binding, error)
	GetHostWorkspaceManifest(context.Context, string, string, string, int64, string, string, int32) (workspace.ManifestPage, error)
	StreamHostWorkspaceFile(context.Context, string, string, string, int64, string, func(workspace.File, []byte) error) (workspace.File, error)
}

type hostWorkspaceBindingsResponse struct {
	Items []workspaceBindingResponse `json:"items"`
}

func (g *gateway) listHostWorkspaces(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	principal := principalFrom(c)
	bindings, err := g.deps.WorkspaceFiles.ListHostWorkspaceBindings(c.Request.Context(), principal.UserID, c.GetString(hostIDKey))
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	items := make([]workspaceBindingResponse, 0, len(bindings))
	for _, binding := range bindings {
		if binding.HostID != c.GetString(hostIDKey) || binding.Workspace.ID == "" {
			httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "workspace binding is invalid"))
			return
		}
		items = append(items, hostWorkspaceBindingResponseFrom(binding))
	}
	c.JSON(http.StatusOK, hostWorkspaceBindingsResponse{Items: items})
}

func (g *gateway) getHostWorkspaceManifest(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	workspaceID := c.Param("workspace_id")
	query, err := parseWorkspaceManifestQuery(c.Request.URL.RawQuery)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	principal := principalFrom(c)
	page, err := g.deps.WorkspaceFiles.GetHostWorkspaceManifest(
		c.Request.Context(), principal.UserID, c.GetString(hostIDKey), workspaceID,
		query.revision, query.prefix, query.cursor, query.limit,
	)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	if page.WorkspaceID != workspaceID || page.Revision < 0 {
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "workspace manifest is invalid"))
		return
	}
	if page.Files == nil {
		page.Files = []workspace.File{}
	}
	if page.EmptyDirectories == nil {
		page.EmptyDirectories = []string{}
	}
	c.JSON(http.StatusOK, page)
}

func (g *gateway) downloadHostWorkspaceFile(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	workspaceID := c.Param("workspace_id")
	filePath := strings.TrimPrefix(c.Param("path"), "/")
	revision, err := parseWorkspaceFileRevision(c.Request.URL.RawQuery)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	principal := principalFrom(c)
	checkAccess := g.memberAuthorizationCheck(c)
	started := false
	write := func(file workspace.File, chunk []byte) error {
		if err := checkAccess(c.Request.Context()); err != nil {
			return err
		}
		if !started {
			filename := path.Base(file.Path)
			if filename == "." || filename == "/" || filename == "" {
				filename = "download"
			}
			c.Header("Content-Type", "application/octet-stream")
			c.Header("Content-Length", strconv.FormatInt(file.SizeBytes, 10))
			c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
			c.Status(http.StatusOK)
			started = true
		}
		if len(chunk) == 0 {
			return nil
		}
		written, err := c.Writer.Write(chunk)
		if err != nil {
			return err
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		return nil
	}
	_, err = g.deps.WorkspaceFiles.StreamHostWorkspaceFile(
		c.Request.Context(), principal.UserID, c.GetString(hostIDKey), workspaceID, revision, filePath, write,
	)
	if err != nil {
		if !started {
			httpserver.WriteError(c, err)
			return
		}
		c.Abort()
		return
	}
	if !started {
		httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "workspace file stream is empty"))
	}
}

type workspaceManifestQuery struct {
	revision int64
	prefix   string
	cursor   string
	limit    int32
}

func parseWorkspaceManifestQuery(raw string) (workspaceManifestQuery, error) {
	values, err := parseWorkspaceReadQuery(raw, "revision", "prefix", "cursor", "limit")
	if err != nil {
		return workspaceManifestQuery{}, err
	}
	result := workspaceManifestQuery{}
	value, ok := values["revision"]
	if !ok || len(value) != 1 {
		return workspaceManifestQuery{}, domain.InvalidArgument("revision", "workspace revision is required")
	}
	result.revision, err = strconv.ParseInt(value[0], 10, 64)
	if err != nil || result.revision < 0 {
		return workspaceManifestQuery{}, domain.InvalidArgument("revision", "workspace revision is invalid")
	}
	result.prefix = values.Get("prefix")
	result.cursor = values.Get("cursor")
	if value := values.Get("limit"); value != "" {
		limit, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil {
			return workspaceManifestQuery{}, domain.InvalidArgument("limit", "manifest page limit is invalid")
		}
		result.limit = int32(limit)
	}
	return result, nil
}

func parseWorkspaceFileRevision(raw string) (int64, error) {
	values, err := parseWorkspaceReadQuery(raw, "revision")
	if err != nil {
		return 0, err
	}
	value := values.Get("revision")
	if value == "" {
		return 0, domain.InvalidArgument("revision", "workspace revision is required")
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 {
		return 0, domain.InvalidArgument("revision", "workspace revision is invalid")
	}
	return revision, nil
}

func parseWorkspaceReadQuery(raw string, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, domain.InvalidArgument("query", "workspace query parameters are invalid")
	}
	allowedKeys := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedKeys[key] = struct{}{}
	}
	for key, items := range values {
		if _, ok := allowedKeys[key]; !ok || len(items) != 1 {
			return nil, domain.InvalidArgument("query", "workspace query parameters are invalid")
		}
	}
	return values, nil
}

func validHostWorkspaceReadRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet {
		return false
	}
	const prefix = "/__ch/api/workspaces/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(request.URL.Path, prefix), "/")
	if len(segments) == 2 && segments[1] == "manifest" && domain.ValidWorkspaceID(segments[0]) {
		_, err := parseWorkspaceManifestQuery(request.URL.RawQuery)
		return err == nil
	}
	if len(segments) >= 3 && segments[1] == "files" && domain.ValidWorkspaceID(segments[0]) {
		if _, err := parseWorkspaceFileRevision(request.URL.RawQuery); err != nil {
			return false
		}
		return workspace.ValidateRelativePath(strings.Join(segments[2:], "/")) == nil
	}
	return false
}

func hostWorkspaceBindingResponseFrom(binding workspace.Binding) workspaceBindingResponse {
	return workspaceBindingResponse{
		Workspace: workspaceRef{
			ID: binding.Workspace.ID, Name: binding.Workspace.Name, Revision: binding.Workspace.Revision,
			UsedBytes: binding.Workspace.UsedBytes, QuotaBytes: binding.Workspace.QuotaBytes, State: binding.Workspace.State,
		},
		Directory: binding.Directory, State: binding.State, BindingVersion: binding.BindingVersion,
		DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	}
}
