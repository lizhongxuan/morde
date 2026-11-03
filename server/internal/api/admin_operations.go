package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type adminSettingsResponse struct {
	RegistrationReviewRequired     bool                       `json:"registration_review_required"`
	AutomaticNewUserModelBudget    bool                       `json:"automatic_new_user_model_budget"`
	AutomaticInvitationReward      bool                       `json:"automatic_invitation_reward"`
	AutomaticContributionReward    bool                       `json:"automatic_contribution_reward"`
	DefaultPersonalDataQuotaBytes  int64                      `json:"default_personal_data_quota_bytes"`
	DefaultPersonalAttachmentBytes int64                      `json:"default_personal_attachment_bytes"`
	DefaultModelBudget             modelBudgetDefaultResponse `json:"default_model_budget"`
}

type modelBudgetDefaultRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	TokenLimit      *int64 `json:"token_limit"`
	MaxConcurrent   *int32 `json:"max_concurrent"`
}

type modelBudgetDefaultResponse struct {
	Configured    bool  `json:"configured"`
	TokenLimit    int64 `json:"token_limit"`
	MaxConcurrent int32 `json:"max_concurrent"`
	Version       int64 `json:"version"`
}

type adminInviteResponse struct {
	InviterID       string    `json:"inviter_id"`
	InviterName     string    `json:"inviter_name"`
	InviteeID       string    `json:"invitee_id"`
	InviteeName     string    `json:"invitee_name"`
	InviteeStatus   string    `json:"invitee_status"`
	CreatedAt       time.Time `json:"created_at"`
	InviterInvitees int64     `json:"inviter_invitees"`
}

type adminInvitePageResponse struct {
	Items         []adminInviteResponse `json:"items"`
	Total         int64                 `json:"total"`
	TotalInviters int64                 `json:"total_inviters"`
	Limit         int                   `json:"limit"`
	Offset        int                   `json:"offset"`
}

type adminSiteResponse struct {
	ID                  string     `json:"id"`
	OwnerID             string     `json:"owner_id"`
	OwnerName           string     `json:"owner_name"`
	HostID              string     `json:"host_id"`
	State               string     `json:"state"`
	CreatedAt           time.Time  `json:"created_at"`
	ReleaseCount        int64      `json:"release_count"`
	HealthyReleaseCount int64      `json:"healthy_release_count"`
	LatestReleaseAt     *time.Time `json:"latest_release_at"`
}

type adminSiteStatsResponse struct {
	Sites           int64 `json:"sites"`
	ActiveSites     int64 `json:"active_sites"`
	Releases        int64 `json:"releases"`
	HealthyReleases int64 `json:"healthy_releases"`
}

type adminSiteIntegrationsResponse struct {
	Demo        string `json:"demo"`
	GitHubStats string `json:"github_stats"`
	Traffic     string `json:"traffic"`
}

type adminSitePageResponse struct {
	Items        []adminSiteResponse           `json:"items"`
	Summary      adminSiteStatsResponse        `json:"summary"`
	Integrations adminSiteIntegrationsResponse `json:"integrations"`
	Total        int64                         `json:"total"`
	Limit        int                           `json:"limit"`
	Offset       int                           `json:"offset"`
}

type adminNudgePolicyResponse struct {
	Enabled   bool       `json:"enabled"`
	URL       string     `json:"url"`
	TextZH    string     `json:"text_zh"`
	TextEN    string     `json:"text_en"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type updateAdminNudgePolicyRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	Enabled         *bool  `json:"enabled"`
	URL             string `json:"url"`
	TextZH          string `json:"text_zh"`
	TextEN          string `json:"text_en"`
}

type inviteCodeResponse struct {
	Code string `json:"code"`
}

func (h *api) registerAdminOperations(group *gin.RouterGroup) {
	if h.deps.AdminOperations != nil {
		group.GET("/me/invite-code", h.getInviteCode)
		group.GET("/admin/settings", h.getAdminSettings)
		group.GET("/admin/invites", h.listAdminInvites)
		group.GET("/admin/sites", h.listAdminSites)
		group.GET("/admin/nudges", h.getAdminNudgePolicy)
		group.PUT("/admin/nudges", h.updateAdminNudgePolicy)
	}
	if h.deps.AdminModelBudgetDefaults != nil {
		group.PUT("/admin/settings/model-budget-default", h.updateAdminModelBudgetDefault)
	}
}

func (h *api) getInviteCode(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	code, err := h.deps.AdminOperations.InviteCode(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, inviteCodeResponse{Code: code})
}

func (h *api) getAdminSettings(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	value, err := h.deps.AdminOperations.Settings(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	if h.deps.AdminModelBudgetDefaults == nil {
		serviceUnavailable(c)
		return
	}
	modelBudget, err := h.deps.AdminModelBudgetDefaults.Get(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminSettingsResponse{
		RegistrationReviewRequired:     value.RegistrationReviewRequired,
		AutomaticNewUserModelBudget:    value.AutomaticNewUserModelBudget,
		AutomaticInvitationReward:      value.AutomaticInvitationReward,
		AutomaticContributionReward:    value.AutomaticContributionReward,
		DefaultPersonalDataQuotaBytes:  value.DefaultPersonalDataQuotaBytes,
		DefaultPersonalAttachmentBytes: value.DefaultPersonalAttachmentBytes,
		DefaultModelBudget:             toModelBudgetDefaultResponse(modelBudget),
	})
}

func (h *api) updateAdminModelBudgetDefault(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request modelBudgetDefaultRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || request.TokenLimit == nil || request.MaxConcurrent == nil {
		writeError(c, domain.InvalidArgument("model_budget_default", "expected_version, token_limit and max_concurrent are required"))
		return
	}
	value, err := h.deps.AdminModelBudgetDefaults.Update(c.Request.Context(), principal, app.UpdateModelBudgetDefaultCommand{
		ExpectedVersion: *request.ExpectedVersion,
		TokenLimit:      *request.TokenLimit,
		MaxConcurrent:   *request.MaxConcurrent,
	}, c.ClientIP())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toModelBudgetDefaultResponse(value))
}

func toModelBudgetDefaultResponse(value app.ModelBudgetDefault) modelBudgetDefaultResponse {
	return modelBudgetDefaultResponse{
		Configured: value.Configured, TokenLimit: value.TokenLimit,
		MaxConcurrent: value.MaxConcurrent, Version: value.Version,
	}
}

func (h *api) listAdminInvites(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	limit, offset, ok := adminPageQuery(c)
	if !ok {
		return
	}
	query := c.Request.URL.Query()
	searchValues := query["search"]
	if len(searchValues) > 1 {
		writeError(c, domain.InvalidArgument("search", "search must appear once"))
		return
	}
	search := ""
	if len(searchValues) == 1 {
		search = searchValues[0]
	}
	result, err := h.deps.AdminOperations.ListInvites(c.Request.Context(), principal, app.AdminInviteFilter{Search: search, Limit: limit, Offset: offset})
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminInvitePageResponse{Items: make([]adminInviteResponse, 0, len(result.Items)), Total: result.Total,
		TotalInviters: result.TotalInviters, Limit: result.Limit, Offset: result.Offset}
	for _, item := range result.Items {
		response.Items = append(response.Items, adminInviteResponse{
			InviterID: item.InviterID, InviterName: item.InviterName, InviteeID: item.InviteeID,
			InviteeName: item.InviteeName, InviteeStatus: string(item.InviteeStatus), CreatedAt: item.CreatedAt,
			InviterInvitees: item.InviterInvitees,
		})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) listAdminSites(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	limit, offset, ok := adminPageQuery(c)
	if !ok {
		return
	}
	result, err := h.deps.AdminOperations.ListSites(c.Request.Context(), principal, limit, offset)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminSitePageResponse{
		Items: make([]adminSiteResponse, 0, len(result.Items)),
		Summary: adminSiteStatsResponse{Sites: result.Summary.Sites, ActiveSites: result.Summary.ActiveSites,
			Releases: result.Summary.Releases, HealthyReleases: result.Summary.HealthyReleases},
		Integrations: adminSiteIntegrationsResponse{Demo: "not_configured", GitHubStats: "not_configured", Traffic: "not_collected"},
		Total:        result.Total, Limit: result.Limit, Offset: result.Offset,
	}
	for _, item := range result.Items {
		response.Items = append(response.Items, adminSiteResponse{
			ID: item.ID, OwnerID: item.OwnerID, OwnerName: item.OwnerName, HostID: item.HostID,
			State: item.State, CreatedAt: item.CreatedAt, ReleaseCount: item.ReleaseCount,
			HealthyReleaseCount: item.HealthyReleaseCount, LatestReleaseAt: item.LatestReleaseAt,
		})
	}
	c.JSON(http.StatusOK, response)
}
func (h *api) getAdminNudgePolicy(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	value, err := h.deps.AdminOperations.NudgePolicy(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminNudgePolicyView(value))
}

func (h *api) updateAdminNudgePolicy(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request updateAdminNudgePolicyRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 || request.Enabled == nil {
		writeError(c, domain.InvalidArgument("nudge_policy", "expected_version and enabled are required"))
		return
	}
	value, err := h.deps.AdminOperations.UpdateNudgePolicy(c.Request.Context(), principal, app.UpdateAdminNudgeCommand{
		ExpectedVersion: *request.ExpectedVersion, Enabled: *request.Enabled, URL: request.URL,
		TextZH: request.TextZH, TextEN: request.TextEN, IP: c.ClientIP(),
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminNudgePolicyView(value))
}

func (h *api) getPublicNudges(c *gin.Context) {
	value, err := h.deps.AdminOperations.PublicNudgePolicy(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.JSON(http.StatusOK, adminNudgePolicyView(value))
}

func adminNudgePolicyView(value app.AdminNudgePolicy) adminNudgePolicyResponse {
	return adminNudgePolicyResponse{Enabled: value.Enabled, URL: value.URL, TextZH: value.TextZH,
		TextEN: value.TextEN, Version: value.Version, UpdatedAt: value.UpdatedAt}
}
