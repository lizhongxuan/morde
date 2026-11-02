package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
)

type adminOverviewResponse struct {
	CheckedAt           time.Time      `json:"checked_at"`
	Health              adminHealth    `json:"health"`
	OnlineInstallations int            `json:"online_installations"`
	Limit               TotalsView     `json:"limit"`
	Allocated           TotalsView     `json:"allocated"`
	HostsByState        map[string]int `json:"hosts_by_state"`
	AgentsByState       map[string]int `json:"agents_by_state"`
}

type adminHealth struct {
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
}

type adminPageResponse[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

type adminInstallationResponse struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Platform    string     `json:"platform"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	Online      bool       `json:"online"`
}

type adminAuthorizationResponse struct {
	ID             string     `json:"id"`
	DisplayName    string     `json:"display_name"`
	Platform       string     `json:"platform"`
	State          string     `json:"state"`
	InstallationID string     `json:"installation_id"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	ClaimedAt      *time.Time `json:"claimed_at"`
}

type adminModelCallResponse struct {
	Source         string     `json:"source"`
	CallID         string     `json:"call_id"`
	ModelID        string     `json:"model_id"`
	ProviderModel  string     `json:"provider_model"`
	TaskID         string     `json:"task_id,omitempty"`
	ReservedTokens int64      `json:"reserved_tokens"`
	State          string     `json:"state"`
	InputTokens    *int64     `json:"input_tokens"`
	OutputTokens   *int64     `json:"output_tokens"`
	CreatedAt      time.Time  `json:"created_at"`
	SettledAt      *time.Time `json:"settled_at"`
}

type adminUsageResponse struct {
	Days    int                       `json:"days"`
	From    time.Time                 `json:"from"`
	To      time.Time                 `json:"to"`
	Summary adminUsageSummaryResponse `json:"summary"`
	ByDay   []adminUsageDayResponse   `json:"by_day"`
	ByModel []adminUsageModelResponse `json:"by_model"`
}

type adminUsageSummaryResponse struct {
	Calls          int64 `json:"calls"`
	SettledCalls   int64 `json:"settled_calls"`
	UnknownCalls   int64 `json:"unknown_calls"`
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	ActiveAccounts int64 `json:"active_accounts"`
}

type adminUsageDayResponse struct {
	Date           string `json:"date"`
	Calls          int64  `json:"calls"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	ActiveAccounts int64  `json:"active_accounts"`
}

type adminUsageModelResponse struct {
	ModelID      string `json:"model_id"`
	Calls        int64  `json:"calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

type adminStatsResponse struct {
	Users                    adminUserStatsResponse       `json:"users"`
	Platforms                []adminPlatformStatsResponse `json:"platforms"`
	Cohorts                  []adminCohortStatsResponse   `json:"cohorts"`
	VersionTrackingAvailable bool                         `json:"version_tracking_available"`
}

type adminUserStatsResponse struct {
	Total    int64 `json:"total"`
	Approved int64 `json:"approved"`
	Pending  int64 `json:"pending"`
	Rejected int64 `json:"rejected"`
	Disabled int64 `json:"disabled"`
}

type adminPlatformStatsResponse struct {
	Platform            string `json:"platform"`
	Installations       int64  `json:"installations"`
	ActiveInstallations int64  `json:"active_installations"`
	Users               int64  `json:"users"`
}

type adminCohortStatsResponse struct {
	Week             string `json:"week"`
	Signups          int64  `json:"signups"`
	WithInstallation int64  `json:"with_installation"`
}

func (h *api) registerAdminConsole(group *gin.RouterGroup) {
	group.GET("/admin/overview", h.getAdminOverview)
	group.GET("/admin/model-connections", h.listAdminModelConnections)
	group.GET("/admin/usage", h.getAdminUsage)
	group.GET("/admin/stats", h.getAdminStats)
	group.GET("/admin/users/:id", h.getAdminUser)
	group.GET("/admin/users/:id/installations", h.listAdminUserInstallations)
	group.GET("/admin/users/:id/authorizations", h.listAdminUserAuthorizations)
	group.GET("/admin/users/:id/model-usage/ledger", h.listAdminUserModelCalls)
}

func (h *api) getAdminOverview(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.Admin == nil || h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	capacity, err := h.deps.Admin.Capacity(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	if !validTotals(capacity.Limit) || !validTotals(capacity.Allocated) {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	agents, err := h.deps.AdminConsole.AgentStates(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminOverviewResponse{
		CheckedAt: time.Now().UTC(), Limit: capacity.Limit, Allocated: capacity.Allocated,
		HostsByState: make(map[string]int), AgentsByState: make(map[string]int),
	}
	for state, count := range capacity.HostsByState {
		if count < 0 {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		response.HostsByState[string(state)] = count
	}
	for state, count := range agents {
		if count < 0 || int64(int(count)) != count {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		response.AgentsByState[state] = int(count)
	}
	if presence, ok := h.deps.Hub.(HubPresenceReader); ok {
		response.OnlineInstallations = presence.OnlineInstallationCount()
	}
	response.Health.Ready = h.deps.Readiness != nil && h.deps.Readiness.CheckReady(c.Request.Context()) == nil
	if !response.Health.Ready {
		response.Health.Problem = "一个或多个平台依赖尚未就绪。"
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) listAdminModelConnections(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.ModelConnections.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := modelConnectionListResponse{Items: make([]modelConnectionResponse, 0, len(items))}
	for _, item := range items {
		if !item.Shared {
			continue
		}
		view, err := toModelConnectionResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminUsage(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	days := 7
	if values, exists := c.Request.URL.Query()["days"]; exists {
		if len(values) != 1 {
			writeError(c, domain.InvalidArgument("days", "days must appear once"))
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil {
			writeError(c, domain.InvalidArgument("days", "days must be 7, 30, or 90"))
			return
		}
		days = parsed
	}
	if days != 7 && days != 30 && days != 90 {
		writeError(c, domain.InvalidArgument("days", "days must be 7, 30, or 90"))
		return
	}
	usage, err := h.deps.AdminConsole.Usage(c.Request.Context(), principal, days)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminUsageResponse{Days: usage.Days, From: usage.From, To: usage.To,
		Summary: adminUsageSummaryResponse{Calls: usage.Summary.Calls, SettledCalls: usage.Summary.SettledCalls,
			UnknownCalls: usage.Summary.UnknownCalls, InputTokens: usage.Summary.InputTokens,
			OutputTokens: usage.Summary.OutputTokens, ActiveAccounts: usage.Summary.ActiveAccounts},
		ByDay: make([]adminUsageDayResponse, 0, len(usage.ByDay)), ByModel: make([]adminUsageModelResponse, 0, len(usage.ByModel)),
	}
	for _, item := range usage.ByDay {
		response.ByDay = append(response.ByDay, adminUsageDayResponse{Date: item.Date, Calls: item.Calls,
			InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, ActiveAccounts: item.ActiveAccounts})
	}
	for _, item := range usage.ByModel {
		response.ByModel = append(response.ByModel, adminUsageModelResponse{ModelID: item.ModelID, Calls: item.Calls,
			InputTokens: item.InputTokens, OutputTokens: item.OutputTokens})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminStats(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	stats, err := h.deps.AdminConsole.Stats(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminStatsResponse{
		Users: adminUserStatsResponse{Total: stats.Users.Total, Approved: stats.Users.Approved, Pending: stats.Users.Pending,
			Rejected: stats.Users.Rejected, Disabled: stats.Users.Disabled},
		Platforms:                make([]adminPlatformStatsResponse, 0, len(stats.Platforms)),
		Cohorts:                  make([]adminCohortStatsResponse, 0, len(stats.Cohorts)),
		VersionTrackingAvailable: stats.VersionTrackingAvailable,
	}
	for _, item := range stats.Platforms {
		response.Platforms = append(response.Platforms, adminPlatformStatsResponse{Platform: item.Platform,
			Installations: item.Installations, ActiveInstallations: item.ActiveInstallations, Users: item.Users})
	}
	for _, item := range stats.Cohorts {
		response.Cohorts = append(response.Cohorts, adminCohortStatsResponse{Week: item.Week, Signups: item.Signups, WithInstallation: item.WithInstallation})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getAdminUser(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidUserID)
	if !ok {
		return
	}
	service, ok := h.deps.Admin.(AdminUserDetailService)
	if !ok {
		serviceUnavailable(c)
		return
	}
	item, err := service.GetUser(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toAdminUserResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) listAdminUserInstallations(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidUserID)
	if !ok {
		return
	}
	limit, offset, ok := adminPageQuery(c)
	if !ok {
		return
	}
	if h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	page, err := h.deps.AdminConsole.Installations(c.Request.Context(), principal, id, limit, offset)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]adminInstallationResponse, 0, len(page.Items))
	presence, hasPresence := h.deps.Hub.(HubPresenceReader)
	for _, item := range page.Items {
		items = append(items, adminInstallationResponse{ID: item.ID, DisplayName: item.DisplayName, Platform: item.Platform,
			CreatedAt: item.CreatedAt, LastUsedAt: item.LastUsedAt, ExpiresAt: item.ExpiresAt, RevokedAt: item.RevokedAt,
			Online: hasPresence && presence.InstallationOnline(id, item.ID)})
	}
	c.JSON(http.StatusOK, adminPageResponse[adminInstallationResponse]{Items: items, Total: page.Total, Limit: page.Limit, Offset: page.Offset})
}

func (h *api) listAdminUserAuthorizations(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidUserID)
	if !ok {
		return
	}
	limit, offset, ok := adminPageQuery(c)
	if !ok {
		return
	}
	if h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	page, err := h.deps.AdminConsole.Authorizations(c.Request.Context(), principal, id, limit, offset)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]adminAuthorizationResponse, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, adminAuthorizationResponse{ID: item.ID, DisplayName: item.DisplayName, Platform: item.Platform,
			State: item.State, InstallationID: item.InstallationID, CreatedAt: item.CreatedAt, ExpiresAt: item.ExpiresAt, ClaimedAt: item.ClaimedAt})
	}
	c.JSON(http.StatusOK, adminPageResponse[adminAuthorizationResponse]{Items: items, Total: page.Total, Limit: page.Limit, Offset: page.Offset})
}

func (h *api) listAdminUserModelCalls(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidUserID)
	if !ok {
		return
	}
	limit, offset, ok := adminPageQuery(c)
	if !ok {
		return
	}
	if h.deps.AdminConsole == nil {
		serviceUnavailable(c)
		return
	}
	page, err := h.deps.AdminConsole.ModelCalls(c.Request.Context(), principal, id, limit, offset)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]adminModelCallResponse, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, adminModelCallResponse{Source: item.Source, CallID: item.CallID, ModelID: item.ModelID,
			ProviderModel: item.ProviderModel, TaskID: item.TaskID, ReservedTokens: item.ReservedTokens, State: item.State,
			InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, CreatedAt: item.CreatedAt, SettledAt: item.SettledAt})
	}
	c.JSON(http.StatusOK, adminPageResponse[adminModelCallResponse]{Items: items, Total: page.Total, Limit: page.Limit, Offset: page.Offset})
}

func adminPageQuery(c *gin.Context) (int, int, bool) {
	return adminPageQueryWithDefault(c, 25)
}

func adminPageQueryWithDefault(c *gin.Context, defaultLimit int) (int, int, bool) {
	query := c.Request.URL.Query()
	limit, offset := defaultLimit, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		values, exists := query[name]
		if !exists {
			continue
		}
		if len(values) != 1 {
			writeError(c, domain.InvalidArgument(name, name+" must appear once"))
			return 0, 0, false
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil {
			writeError(c, domain.InvalidArgument(name, "invalid page value"))
			return 0, 0, false
		}
		*target = parsed
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		writeError(c, domain.InvalidArgument("page", "limit must be 1-100 and offset must be 0-1000000"))
		return 0, 0, false
	}
	return limit, offset, true
}
