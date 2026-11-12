package api

import (
	"context"
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
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostListResponse struct {
	Items []hostResponse `json:"items"`
}

type hostResponse struct {
	ID                         string                     `json:"id"`
	Name                       string                     `json:"name"`
	OwnerID                    string                     `json:"owner_id"`
	TemplateID                 string                     `json:"template_id"`
	Isolation                  string                     `json:"isolation"`
	State                      host.State                 `json:"state"`
	StateReason                string                     `json:"state_reason"`
	RuntimeGeneration          int64                      `json:"runtime_generation"`
	WorkspaceRuntimeOnline     *bool                      `json:"workspace_runtime_online,omitempty"`
	WorkspaceRuntimeLastSeenAt *time.Time                 `json:"workspace_runtime_last_seen_at,omitempty"`
	ControlVersion             int64                      `json:"control_version"`
	ControlState               host.ControlState          `json:"control_state"`
	URL                        string                     `json:"url"`
	Workspaces                 []workspaceBindingResponse `json:"workspaces"`
	Resources                  resourcesResponse          `json:"resources"`
	ExpiresAt                  *time.Time                 `json:"expires_at"`
	LastActiveAt               *time.Time                 `json:"last_active_at"`
	CreatedAt                  time.Time                  `json:"created_at"`
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
	WorkspaceIDs json.RawMessage `json:"workspace_ids"`
	Workspace    json.RawMessage `json:"workspace"`
}

type releaseHostRequest struct {
	ConfirmProcessTermination bool `json:"confirm_process_termination"`
}

type issueHostEmbedTicketRequest struct {
	ConversationID string   `json:"conversation_id"`
	Target         string   `json:"target"`
	Targets        []string `json:"targets"`
}

type hostEmbedTicketResponse struct {
	ExchangeURL string    `json:"exchange_url"`
	Ticket      string    `json:"ticket"`
	EmbedNonce  string    `json:"embed_nonce"`
	Targets     []string  `json:"targets"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type hostEmbedTicketIssuer interface {
	IssueEmbedTicket(context.Context, identity.Principal, app.HostEmbedTicketCommand) (app.HostEmbedTicket, error)
}

func (h *api) registerHosts(group *gin.RouterGroup) {
	group.GET("/hosts", h.listHosts)
	group.GET("/shared-hosts", h.listSharedHosts)
	group.POST("/hosts", h.createHost)
	group.GET("/hosts/:id", h.getHost)
	group.GET("/hosts/:id/usage", h.getHostUsageReceipt)
	group.GET("/hosts/:id/workspace-bindings", h.listHostWorkspaceBindings)
	group.POST("/hosts/:id/workspace-bindings", h.bindHostWorkspace)
	group.POST("/hosts/:id/workspace-bindings/:workspace_id/retry-sync", h.retryHostWorkspaceSync)
	group.DELETE("/hosts/:id/workspace-bindings/:workspace_id", h.unbindHostWorkspace)
	group.DELETE("/hosts/:id", h.deleteHost)
	group.POST("/hosts/:id/release", h.releaseHost)
	group.POST("/hosts/:id/restore", h.restoreHost)
	group.POST("/hosts/:id/renew", h.renewHost)
	group.POST("/hosts/:id/enter", h.enterHost)
	group.POST("/hosts/:id/embed-tickets", h.issueHostEmbedTicket)
	group.GET("/hosts/:id/preview-auth", h.previewAuth)
	group.POST("/hosts/:id/observation-ticket", h.issueObservationTicket)
	group.POST("/hosts/:id/members", h.inviteHostMember)
	group.GET("/hosts/:id/members", h.listHostMembers)
	group.DELETE("/hosts/:id/members/:user_id", h.revokeHostMember)
}

func (h *api) issueHostEmbedTicket(c *gin.Context) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	if h.deps.HostSharing == nil || h.deps.Conversations == nil {
		writeError(c, domain.NewError(domain.CodeUnsupported, "embedded Host access is unavailable"))
		return
	}
	issuer, ok := h.deps.HostSharing.(hostEmbedTicketIssuer)
	if !ok {
		writeError(c, domain.NewError(domain.CodeUnsupported, "embedded Host access is unavailable"))
		return
	}
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	hostID, ok := safeID(c, "id", domain.ValidHostID)
	if !ok {
		return
	}
	if c.Request.URL.RawQuery != "" {
		writeError(c, domain.InvalidArgument("query", "embed ticket query parameters are not supported"))
		return
	}
	origin := h.options.HostScheme + "://" + h.options.PortalDomain
	if c.GetHeader("Origin") != origin {
		writeError(c, domain.NewError(domain.CodeForbidden, "request origin is not allowed"))
		return
	}
	if !domain.ValidServiceInstanceID(h.options.ServiceInstanceID) {
		writeError(c, domain.NewError(domain.CodeUnsupported, "embedded Host access is unavailable"))
		return
	}
	var request issueHostEmbedTicketRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if !domain.ValidConversationID(request.ConversationID) || !validHostEmbedTarget(request.Target) {
		writeError(c, domain.InvalidArgument("request", "embed ticket request is invalid"))
		return
	}
	conversationValue, err := h.deps.Conversations.Get(c.Request.Context(), principal, request.ConversationID)
	if err != nil {
		writeError(c, err)
		return
	}
	if conversationValue.ID != request.ConversationID || conversationValue.OwnerID != principal.UserID {
		writeError(c, domain.NewError(domain.CodeNotFound, "conversation not found"))
		return
	}
	issued, err := issuer.IssueEmbedTicket(c.Request.Context(), principal, app.HostEmbedTicketCommand{
		HostID: hostID, ConversationID: request.ConversationID, Target: request.Target, Targets: request.Targets,
		ServiceInstanceID: h.options.ServiceInstanceID, PortalOrigin: origin,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, hostEmbedTicketResponse{
		ExchangeURL: issued.ExchangeURL, Ticket: issued.Ticket, EmbedNonce: issued.EmbedNonce,
		Targets: issued.Targets, ExpiresAt: issued.ExpiresAt,
	})
}

func validHostEmbedTarget(target string) bool {
	switch target {
	case app.HostEmbedTargetBrowser, app.HostEmbedTargetTerminal, app.HostEmbedTargetWorkspace, app.HostEmbedTargetFiles, app.HostEmbedTargetVSCode:
		return true
	default:
		return false
	}
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
	var workspaceIDs []string
	if request.WorkspaceIDs != nil {
		if err := json.Unmarshal(request.WorkspaceIDs, &workspaceIDs); err != nil || workspaceIDs == nil || len(workspaceIDs) != 1 {
			writeError(c, domain.InvalidArgument("workspace_ids", "exactly one workspace must be bound to a host"))
			return
		}
		if !domain.ValidWorkspaceID(workspaceIDs[0]) {
			writeError(c, domain.InvalidArgument("workspace_ids", "invalid workspace id"))
			return
		}
	} else if principal.Kind != identity.KindSession {
		writeError(c, domain.NewError(domain.CodeForbidden, "portal session required to create a Workspace automatically"))
		return
	}
	command := app.CreateHostCommand{Name: request.Name, WorkspaceIDs: workspaceIDs, IdempotencyKey: keys[0]}
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
			binding.DesiredRevision < 0 || binding.AppliedRevision < 0 ||
			workspace.ValidateBindingSyncStatus(binding.SyncState, binding.SyncConflicts) != nil {
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
			SyncState: binding.SyncState, SyncConflicts: append([]string{}, binding.SyncConflicts...),
		})
	}
	var workspaceRuntimeOnline *bool
	if item.WorkspaceRuntimeLivenessKnown {
		online := item.WorkspaceRuntimeOnline
		workspaceRuntimeOnline = &online
	}
	return hostResponse{
		ID:                         item.ID,
		Name:                       item.Name,
		OwnerID:                    item.OwnerID,
		TemplateID:                 item.TemplateID,
		Isolation:                  item.RuntimeProfile,
		State:                      item.State,
		StateReason:                item.StateReason,
		RuntimeGeneration:          item.RuntimeGeneration,
		WorkspaceRuntimeOnline:     workspaceRuntimeOnline,
		WorkspaceRuntimeLastSeenAt: item.WorkspaceRuntimeLastSeenAt,
		ControlVersion:             item.ControlVersion,
		ControlState:               item.ControlState,
		URL:                        h.hostURL(item.ID),
		Workspaces:                 workspaces,
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
