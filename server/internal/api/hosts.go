package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

type hostListResponse struct {
	Items []hostResponse `json:"items"`
}

type hostResponse struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name"`
	OwnerID           string                     `json:"owner_id"`
	TemplateID        string                     `json:"template_id"`
	Isolation         string                     `json:"isolation"`
	State             host.State                 `json:"state"`
	StateReason       string                     `json:"state_reason"`
	RuntimeGeneration int64                      `json:"runtime_generation"`
	ControlVersion    int64                      `json:"control_version"`
	ControlState      host.ControlState          `json:"control_state"`
	URL               string                     `json:"url"`
	Workspaces        []workspaceBindingResponse `json:"workspaces"`
	Resources         resourcesResponse          `json:"resources"`
	ExpiresAt         *time.Time                 `json:"expires_at"`
	LastActiveAt      *time.Time                 `json:"last_active_at"`
	CreatedAt         time.Time                  `json:"created_at"`
}

type workspaceRef struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Revision  int64           `json:"revision"`
	UsedBytes int64           `json:"used_bytes"`
	State     workspace.State `json:"state"`
}

type workspaceBindingResponse struct {
	Workspace       workspaceRef               `json:"workspace"`
	Directory       string                     `json:"directory"`
	State           workspace.BindingState     `json:"state"`
	BindingVersion  int64                      `json:"binding_version"`
	DesiredRevision int64                      `json:"desired_revision"`
	AppliedRevision int64                      `json:"applied_revision"`
	SyncState       workspace.BindingSyncState `json:"sync_state"`
	SyncConflicts   []string                   `json:"sync_conflicts"`
}

type resourcesResponse struct {
	CPUMilli      int `json:"cpu_milli"`
	MemMiB        int `json:"mem_mib"`
	CPULimitMilli int `json:"cpu_limit_milli"`
	MemLimitMiB   int `json:"mem_limit_mib"`
}

type createHostRequest struct {
	Name         string          `json:"name"`
	WorkspaceIDs []string        `json:"workspace_ids"`
	Workspace    json.RawMessage `json:"workspace"`
}

type releaseHostRequest struct {
	ConfirmProcessTermination bool `json:"confirm_process_termination"`
}

func (h *api) registerHosts(group *gin.RouterGroup) {
	group.GET("/hosts", h.listHosts)
	group.GET("/shared-hosts", h.listSharedHosts)
	group.POST("/hosts", h.createHost)
	group.GET("/hosts/:id", h.getHost)
	group.GET("/hosts/:id/workspace-bindings", h.listHostWorkspaceBindings)
	group.POST("/hosts/:id/workspace-bindings", h.bindHostWorkspace)
	group.POST("/hosts/:id/workspace-bindings/:workspace_id/retry-sync", h.retryHostWorkspaceSync)
	group.DELETE("/hosts/:id/workspace-bindings/:workspace_id", h.unbindHostWorkspace)
	group.DELETE("/hosts/:id", h.deleteHost)
	group.POST("/hosts/:id/release", h.releaseHost)
	group.POST("/hosts/:id/restore", h.restoreHost)
	group.POST("/hosts/:id/renew", h.renewHost)
	group.POST("/hosts/:id/enter", h.enterHost)
	group.GET("/hosts/:id/preview-auth", h.previewAuth)
	group.POST("/hosts/:id/observation-ticket", h.issueObservationTicket)
	group.POST("/hosts/:id/members", h.inviteHostMember)
	group.GET("/hosts/:id/members", h.listHostMembers)
	group.DELETE("/hosts/:id/members/:user_id", h.revokeHostMember)
}

func (h *api) releaseHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	var request releaseHostRequest
	if c.Request.Body != nil && c.Request.Body != http.NoBody &&
		(c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0) && !decodeJSON(c, &request) {
		return
	}
	item, err := h.deps.Hosts.Release(c.Request.Context(), principal, id, request.ConfirmProcessTermination)
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusAccepted, item)
}

func (h *api) restoreHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	item, err := h.deps.Hosts.Restore(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusAccepted, item)
}

func (h *api) listHosts(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	items, err := h.deps.Hosts.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := hostListResponse{Items: make([]hostResponse, 0, len(items))}
	for _, item := range items {
		view, err := h.hostView(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) createHost(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	keys := c.Request.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeError(c, domain.InvalidArgument("Idempotency-Key", "exactly one idempotency key is required"))
		return
	}
	var request createHostRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.Workspace != nil {
		writeError(c, domain.InvalidArgument("workspace", "workspace is not a supported host creation field"))
		return
	}
	if len(request.WorkspaceIDs) > workspace.MaxBindingsPerHost {
		writeError(c, domain.InvalidArgument("workspace_ids", "at most 64 workspaces may be bound to a host"))
		return
	}
	seenWorkspaceIDs := make(map[string]struct{}, len(request.WorkspaceIDs))
	for _, workspaceID := range request.WorkspaceIDs {
		if !domain.ValidWorkspaceID(workspaceID) {
			writeError(c, domain.InvalidArgument("workspace_ids", "invalid workspace id"))
			return
		}
		if _, exists := seenWorkspaceIDs[workspaceID]; exists {
			writeError(c, domain.InvalidArgument("workspace_ids", "workspace ids must be unique"))
			return
		}
		seenWorkspaceIDs[workspaceID] = struct{}{}
	}
	command := app.CreateHostCommand{Name: request.Name, WorkspaceIDs: request.WorkspaceIDs, IdempotencyKey: keys[0]}
	created, err := h.deps.Hosts.Create(c.Request.Context(), principal, command)
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusCreated, created)
}

func (h *api) getHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	item, err := h.deps.Hosts.Get(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusOK, item)
}

func (h *api) listHostWorkspaceBindings(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	item, err := h.deps.Hosts.Get(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	view, err := h.hostView(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, struct {
		Items []workspaceBindingResponse `json:"items"`
	}{Items: view.Workspaces})
}

func (h *api) bindHostWorkspace(c *gin.Context) {
	principal, _ := principalFrom(c)
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	var request struct {
		WorkspaceID            string `json:"workspace_id"`
		ExpectedRevision       *int64 `json:"expected_revision"`
		ExpectedBindingVersion *int64 `json:"expected_binding_version"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeDecodeError(c, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("body", "request body must contain exactly one value"))
		return
	}
	if !domain.ValidWorkspaceID(request.WorkspaceID) || request.ExpectedBindingVersion == nil || *request.ExpectedBindingVersion < 0 ||
		(request.ExpectedRevision != nil && *request.ExpectedRevision < 0) {
		writeError(c, domain.InvalidArgument("expected_binding_version", "workspace binding request is invalid"))
		return
	}
	key, ok := workspaceSyncIdempotencyKey(c)
	if !ok {
		return
	}
	binding, err := h.deps.Hosts.BindWorkspace(c.Request.Context(), principal, hostID, request.WorkspaceID,
		request.ExpectedRevision, *request.ExpectedBindingVersion, key)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toHostWorkspaceBindingResponse(binding)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, response)
}

func (h *api) unbindHostWorkspace(c *gin.Context) {
	principal, _ := principalFrom(c)
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	workspaceID, ok := safeIDParam(c, "workspace_id", "workspace_id", domain.ValidWorkspaceID)
	if !ok {
		return
	}
	var request struct {
		ExpectedBindingVersion int64 `json:"expected_binding_version"`
		WritersStopped         bool  `json:"writers_stopped"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeDecodeError(c, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("body", "request body must contain exactly one value"))
		return
	}
	if request.ExpectedBindingVersion < 1 {
		writeError(c, domain.InvalidArgument("expected_binding_version", "workspace binding version is required"))
		return
	}
	if !request.WritersStopped {
		writeError(c, domain.InvalidArgument("writers_stopped", "confirm workspace writers are stopped before unbinding"))
		return
	}
	key, ok := workspaceSyncIdempotencyKey(c)
	if !ok {
		return
	}
	binding, err := h.deps.Hosts.UnbindWorkspace(c.Request.Context(), principal, hostID, workspaceID,
		request.ExpectedBindingVersion, request.WritersStopped, key)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toHostWorkspaceBindingResponse(binding)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (h *api) retryHostWorkspaceSync(c *gin.Context) {
	principal, _ := principalFrom(c)
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	workspaceID, ok := safeIDParam(c, "workspace_id", "workspace_id", domain.ValidWorkspaceID)
	if !ok {
		return
	}
	var request struct {
		ExpectedBindingVersion int64 `json:"expected_binding_version"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeDecodeError(c, err)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, domain.InvalidArgument("body", "request body must contain exactly one value"))
		return
	}
	if request.ExpectedBindingVersion < 1 {
		writeError(c, domain.InvalidArgument("expected_binding_version", "workspace binding version is required"))
		return
	}
	binding, err := h.deps.Hosts.RetryWorkspaceSync(c.Request.Context(), principal, hostID, workspaceID, request.ExpectedBindingVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toHostWorkspaceBindingResponse(binding)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func toHostWorkspaceBindingResponse(binding workspace.Binding) (workspaceBindingResponse, error) {
	value := binding.Workspace
	if !domain.ValidHostID(binding.HostID) || !domain.ValidUserID(binding.OwnerID) || !domain.ValidWorkspaceID(value.ID) ||
		value.OwnerID != binding.OwnerID || value.Name == "" || value.Revision < 0 || value.UsedBytes < 0 || !validWorkspaceState(value.State) ||
		!domain.ValidWorkspaceID(binding.Directory) || !validWorkspaceBindingState(binding.State) || binding.BindingVersion < 1 ||
		binding.DesiredRevision < 0 || binding.AppliedRevision < 0 ||
		workspace.ValidateBindingSyncStatus(binding.SyncState, binding.SyncConflicts) != nil {
		return workspaceBindingResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return workspaceBindingResponse{
		Workspace: workspaceRef{ID: value.ID, Name: value.Name, Revision: value.Revision, UsedBytes: value.UsedBytes, State: value.State},
		Directory: binding.Directory, State: binding.State, BindingVersion: binding.BindingVersion,
		DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
	}, nil
}

func (h *api) deleteHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	if _, exists := c.Request.URL.Query()["keepWorkspace"]; exists {
		writeError(c, domain.InvalidArgument("keepWorkspace", "workspace retention is not a host deletion option"))
		return
	}
	item, err := h.deps.Hosts.Delete(c.Request.Context(), principal, app.DeleteHostCommand{HostID: id})
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusAccepted, item)
}

func (h *api) renewHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	item, err := h.deps.Hosts.Renew(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeHost(c, http.StatusOK, item)
}

func (h *api) enterHost(c *gin.Context) {
	principal, _ := principalFrom(c)
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	info, err := h.deps.Hosts.IssueTicket(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	if info.URL == "" || !info.ExpiresAt.After(time.Now()) {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	c.JSON(http.StatusOK, struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}{URL: info.URL, ExpiresAt: info.ExpiresAt})
}

func (h *api) previewAuth(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	id, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	portText := c.Query("port")
	port, err := strconv.Atoi(portText)
	if err != nil || !domain.ValidPreviewPort(portText) || !domain.ValidPreviewTargetPort(port) ||
		port < h.options.PreviewPortMin || previewPortDenied(port, h.options.PreviewPortDeny) {
		writeError(c, domain.NewError(domain.CodeNotFound, "not found"))
		return
	}
	info, err := h.deps.Hosts.IssuePreviewTicket(c.Request.Context(), principal, id, port)
	if err != nil {
		writeError(c, err)
		return
	}
	target, err := url.Parse(info.URL)
	expectedHost := fmt.Sprintf("%d-%s.%s", port, id, strings.TrimPrefix(h.options.HostSuffix, "."))
	if err != nil || target.Scheme != h.options.HostScheme || target.Host != expectedHost || target.Path != "/__ch/auth" ||
		target.User != nil || target.Fragment != "" || target.Query().Get("t") == "" || !info.ExpiresAt.After(time.Now()) {
		writeError(c, domain.NewError(domain.CodeInternal, "preview ticket unavailable"))
		return
	}
	query := target.Query()
	query.Set("next", c.Query("next"))
	target.RawQuery = query.Encode()
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, target.String())
}

func (h *api) writeHost(c *gin.Context, status int, item host.Host) {
	view, err := h.hostView(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(status, view)
}

func (h *api) hostView(item host.Host) (hostResponse, error) {
	if !domain.ValidHostID(item.ID) || !domain.ValidUserID(item.OwnerID) || !domain.ValidTemplateID(item.TemplateID) || !validHostState(item.State) ||
		item.RuntimeGeneration < 0 || item.ControlVersion < 0 || !validControlState(item.ControlState) ||
		item.CPUMilli < 0 || item.MemMiB < 0 || item.CPULimitMilli < 0 || item.MemLimitMiB < 0 || len(item.Workspaces) > workspace.MaxBindingsPerHost {
		return hostResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	workspaces := make([]workspaceBindingResponse, 0, len(item.Workspaces))
	seenWorkspaceIDs := make(map[string]struct{}, len(item.Workspaces))
	seenDirectories := make(map[string]struct{}, len(item.Workspaces))
	for _, binding := range item.Workspaces {
		workspaceItem := binding.Workspace
		if !domain.ValidWorkspaceID(workspaceItem.ID) || !domain.ValidUserID(workspaceItem.OwnerID) || binding.HostID != item.ID || binding.OwnerID != item.OwnerID ||
			workspaceItem.Name == "" || workspaceItem.Revision < 0 || workspaceItem.UsedBytes < 0 || !validWorkspaceState(workspaceItem.State) ||
			!domain.ValidWorkspaceID(binding.Directory) || !validWorkspaceBindingState(binding.State) || binding.BindingVersion < 1 ||
			binding.DesiredRevision < 0 || binding.AppliedRevision < 0 {
			return hostResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if _, exists := seenWorkspaceIDs[workspaceItem.ID]; exists {
			return hostResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if _, exists := seenDirectories[binding.Directory]; exists {
			return hostResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		seenWorkspaceIDs[workspaceItem.ID] = struct{}{}
		seenDirectories[binding.Directory] = struct{}{}
		workspaces = append(workspaces, workspaceBindingResponse{
			Workspace: workspaceRef{
				ID: workspaceItem.ID, Name: workspaceItem.Name, Revision: workspaceItem.Revision,
				UsedBytes: workspaceItem.UsedBytes, State: workspaceItem.State,
			},
			Directory: binding.Directory, State: binding.State, BindingVersion: binding.BindingVersion,
			DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		})
	}
	return hostResponse{
		ID:                item.ID,
		Name:              item.Name,
		OwnerID:           item.OwnerID,
		TemplateID:        item.TemplateID,
		Isolation:         item.RuntimeProfile,
		State:             item.State,
		StateReason:       item.StateReason,
		RuntimeGeneration: item.RuntimeGeneration,
		ControlVersion:    item.ControlVersion,
		ControlState:      item.ControlState,
		URL:               h.hostURL(item.ID),
		Workspaces:        workspaces,
		Resources: resourcesResponse{
			CPUMilli:      item.CPUMilli,
			MemMiB:        item.MemMiB,
			CPULimitMilli: item.CPULimitMilli,
			MemLimitMiB:   item.MemLimitMiB,
		},
		ExpiresAt:    item.ExpiresAt,
		LastActiveAt: item.LastActiveAt,
		CreatedAt:    item.CreatedAt,
	}, nil
}

func validWorkspaceBindingState(state workspace.BindingState) bool {
	switch state {
	case workspace.BindingPreparing, workspace.BindingActive, workspace.BindingUnbinding, workspace.BindingDetached, workspace.BindingFailed:
		return true
	default:
		return false
	}
}

func validControlState(state host.ControlState) bool {
	switch state {
	case host.ControlAI, host.ControlTakingOver, host.ControlUser, host.ControlReturning:
		return true
	default:
		return false
	}
}

func (h *api) hostURL(id string) string {
	return h.options.HostScheme + "://" + id + "." + h.options.HostSuffix
}
