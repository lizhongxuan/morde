package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type adminUserListResponse struct {
	Items []adminUserResponse `json:"items"`
}

type adminUserResponse struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Role      identity.Role          `json:"role"`
	Status    identity.AccountStatus `json:"status"`
	Quota     TotalsView             `json:"quota"`
	Usage     TotalsView             `json:"usage"`
	CreatedAt time.Time              `json:"created_at"`
}

type capacityResponse struct {
	Limit        TotalsView     `json:"limit"`
	Allocated    TotalsView     `json:"allocated"`
	HostsByState map[string]int `json:"hosts_by_state"`
}

type auditListResponse struct {
	Items []auditResponse `json:"items"`
}

type auditResponse struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorID   string          `json:"actor_id"`
	ActorKind audit.ActorKind `json:"actor_kind"`
	Action    audit.Action    `json:"action"`
	Target    string          `json:"target"`
	Detail    map[string]any  `json:"detail"`
	IP        string          `json:"ip"`
}

func (h *api) registerAdmin(group *gin.RouterGroup) {
	group.GET("/admin/hosts", h.listAllHosts)
	group.GET("/admin/workspaces", h.listAdminWorkspaces)
	group.GET("/admin/workspaces/:id", h.getAdminWorkspace)
	group.GET("/admin/users", h.listUsers)
	group.PATCH("/admin/users/:id", h.updateUser)
	group.GET("/admin/capacity", h.getCapacity)
	group.GET("/admin/audit", h.listAudit)
}

func (h *api) listAllHosts(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
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

func (h *api) listUsers(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.Admin == nil {
		serviceUnavailable(c)
		return
	}
	var status *identity.AccountStatus
	if values, exists := c.Request.URL.Query()["status"]; exists {
		if len(values) != 1 {
			writeError(c, domain.InvalidArgument("status", "status must appear once"))
			return
		}
		value := identity.AccountStatus(values[0])
		if !value.Valid() {
			writeError(c, domain.InvalidArgument("status", "invalid account status"))
			return
		}
		status = &value
	}
	items, err := h.deps.Admin.ListUsers(c.Request.Context(), principal, status)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminUserListResponse{Items: make([]adminUserResponse, 0, len(items))}
	for _, item := range items {
		view, err := toAdminUserResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) updateUser(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidUserID)
	if !ok {
		return
	}
	if h.deps.Admin == nil {
		serviceUnavailable(c)
		return
	}
	var raw map[string]json.RawMessage
	if !decodeJSON(c, &raw) {
		return
	}
	var command UpdateAdminUserCommand
	for name := range raw {
		if name != "expected_status" && name != "status" && name != "role" && name != "quota" {
			writeError(c, domain.InvalidArgument("body", "contains an unsupported user field"))
			return
		}
	}
	if data, exists := raw["expected_status"]; exists {
		if err := json.Unmarshal(data, &command.ExpectedStatus); err != nil || !command.ExpectedStatus.Valid() {
			writeError(c, domain.InvalidArgument("expected_status", "invalid account status"))
			return
		}
	} else {
		writeError(c, domain.InvalidArgument("expected_status", "expected_status is required"))
		return
	}
	if data, exists := raw["status"]; exists {
		var value identity.AccountStatus
		if err := json.Unmarshal(data, &value); err != nil || !value.Valid() {
			writeError(c, domain.InvalidArgument("status", "invalid account status"))
			return
		}
		command.Status = &value
	}
	if data, exists := raw["role"]; exists {
		var value identity.Role
		if err := json.Unmarshal(data, &value); err != nil || !validRole(value) {
			writeError(c, domain.InvalidArgument("role", "invalid role"))
			return
		}
		command.Role = &value
	}
	if data, exists := raw["quota"]; exists {
		var value *TotalsView
		if err := json.Unmarshal(data, &value); err != nil || value == nil || !validTotals(*value) {
			writeError(c, domain.InvalidArgument("quota", "invalid quota"))
			return
		}
		command.Quota = value
	}
	if command.Role == nil && command.Status == nil && command.Quota == nil {
		writeError(c, domain.InvalidArgument("body", "at least one user field is required"))
		return
	}
	item, err := h.deps.Admin.UpdateUser(c.Request.Context(), principal, id, command)
	if err != nil {
		writeError(c, err)
		return
	}
	view, err := toAdminUserResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *api) getCapacity(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.Admin == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.Admin.Capacity(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	if !validTotals(item.Limit) || !validTotals(item.Allocated) {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	states := make(map[string]int, len(item.HostsByState))
	for state, count := range item.HostsByState {
		if !validHostState(state) || count < 0 {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		states[string(state)] = count
	}
	c.JSON(http.StatusOK, capacityResponse{Limit: item.Limit, Allocated: item.Allocated, HostsByState: states})
}

func (h *api) listAudit(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.Admin == nil {
		serviceUnavailable(c)
		return
	}
	query, err := auditQuery(c)
	if err != nil {
		writeError(c, err)
		return
	}
	items, err := h.deps.Admin.ListAudit(c.Request.Context(), principal, query)
	if err != nil {
		writeError(c, err)
		return
	}
	response := auditListResponse{Items: make([]auditResponse, 0, len(items))}
	for _, item := range items {
		if !item.Entry.Action.Valid() || !validActorKind(item.Entry.ActorKind) {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		response.Items = append(response.Items, auditResponse{
			ID: item.ID, At: item.At, ActorID: item.Entry.ActorID, ActorKind: item.Entry.ActorKind,
			Action: item.Entry.Action, Target: item.Entry.Target, Detail: sanitizeAuditDetail(item.Entry.Detail), IP: item.Entry.IP,
		})
	}
	c.JSON(http.StatusOK, response)
}

func auditQuery(c *gin.Context) (AuditQuery, error) {
	query := AuditQuery{Limit: 100}
	for name, target := range map[string]*string{"actor": &query.Actor, "action": &query.Action, "target": &query.Target} {
		values, exists := c.Request.URL.Query()[name]
		if !exists {
			continue
		}
		if len(values) != 1 {
			return AuditQuery{}, domain.InvalidArgument(name, name+" must appear once")
		}
		*target = values[0]
	}
	if values, exists := c.Request.URL.Query()["since"]; exists {
		if len(values) != 1 {
			return AuditQuery{}, domain.InvalidArgument("since", "since must appear once")
		}
		parsed, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			return AuditQuery{}, domain.InvalidArgument("since", "since must be an RFC3339 date-time")
		}
		query.Since = &parsed
	}
	if values, exists := c.Request.URL.Query()["limit"]; exists {
		if len(values) != 1 {
			return AuditQuery{}, domain.InvalidArgument("limit", "limit must appear once")
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 500 {
			return AuditQuery{}, domain.InvalidArgument("limit", "limit must be between 1 and 500")
		}
		query.Limit = parsed
	}
	return query, nil
}

func toAdminUserResponse(item AdminUserRecord) (adminUserResponse, error) {
	if !domain.ValidUserID(item.ID) || !validStoredRole(item.Role) || !item.Status.Valid() || !validTotals(item.Quota) || !validTotals(item.Usage) {
		return adminUserResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return adminUserResponse(item), nil
}

func requireAdmin(c *gin.Context) (identity.Principal, bool) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return identity.Principal{}, false
	}
	if !principal.Role.IsAdmin() {
		writeError(c, domain.NewError(domain.CodeForbidden, "forbidden"))
		c.Abort()
		return identity.Principal{}, false
	}
	return principal, true
}

func validRole(role identity.Role) bool {
	return role == identity.RoleUser || role == identity.RoleAdmin
}

func validStoredRole(role identity.Role) bool {
	return validRole(role) || role == identity.RoleSuperAdmin
}

func validTotals(value TotalsView) bool {
	return value.Hosts >= 0 && value.CPUMilli >= 0 && value.MemMiB >= 0
}

func validHostState(state host.State) bool {
	return state == host.StateCreating || state == host.StateRunning || state == host.StateStopping || state == host.StateStopped || state == host.StateFailed || state == host.StateDeleting || state == host.StateDeleted
}

func validActorKind(kind audit.ActorKind) bool {
	switch kind {
	case audit.ActorSession, audit.ActorAPIKey, audit.ActorClient, audit.ActorHostCookie, audit.ActorSystem:
		return true
	default:
		return false
	}
}

func sanitizeAuditDetail(detail map[string]any) map[string]any {
	safe := make(map[string]any, len(detail))
	for key, value := range detail {
		if sensitiveAuditKey(key) {
			continue
		}
		safe[key] = sanitizeAuditValue(value)
	}
	return safe
}

func sanitizeAuditValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return sanitizeAuditDetail(value)
	case map[string]string:
		safe := make(map[string]string, len(value))
		for key, item := range value {
			if !sensitiveAuditKey(key) {
				safe[key] = item
			}
		}
		return safe
	case []any:
		safe := make([]any, len(value))
		for index, item := range value {
			safe[index] = sanitizeAuditValue(item)
		}
		return safe
	case []map[string]any:
		safe := make([]map[string]any, len(value))
		for index, item := range value {
			safe[index] = sanitizeAuditDetail(item)
		}
		return safe
	default:
		return value
	}
}

func sensitiveAuditKey(key string) bool {
	lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "cookie") || strings.Contains(lower, "authorization")
}
