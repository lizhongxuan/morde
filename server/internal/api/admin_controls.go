package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type adminControlsResponse struct {
	Rules            []adminControlRuleResponse         `json:"rules"`
	AccountsTotal    int64                              `json:"accounts_total"`
	NextThreshold    *int64                             `json:"next_threshold"`
	NotifyConfigured bool                               `json:"notify_configured"`
	Notifications    []adminControlNotificationResponse `json:"notifications"`
}

type adminControlRuleResponse struct {
	ID            int64      `json:"id"`
	Threshold     int64      `json:"threshold"`
	Enabled       bool       `json:"enabled"`
	Note          string     `json:"note"`
	CreatedAt     time.Time  `json:"created_at"`
	LastFiredAt   *time.Time `json:"last_fired_at"`
	FiredAccounts int64      `json:"fired_accounts"`
	Version       int64      `json:"version"`
}

type adminControlNotificationResponse struct {
	Kind          string    `json:"kind"`
	RuleID        *int64    `json:"rule_id"`
	AccountsTotal int64     `json:"accounts_total"`
	CreatedAt     time.Time `json:"created_at"`
}

type adminControlRuleRequest struct {
	Threshold *int64 `json:"threshold"`
	Note      string `json:"note"`
}

type adminControlRuleUpdateRequest struct {
	ExpectedVersion *int64  `json:"expected_version"`
	Threshold       *int64  `json:"threshold"`
	Enabled         *bool   `json:"enabled"`
	Note            *string `json:"note"`
	Rearm           bool    `json:"rearm"`
}

type adminControlEvaluationResponse struct {
	AccountsTotal int64                      `json:"accounts_total"`
	Fired         []adminControlRuleResponse `json:"fired"`
}

type adminControlNotifyTestResponse struct {
	Sent       bool      `json:"sent"`
	Configured bool      `json:"configured"`
	CreatedAt  time.Time `json:"created_at"`
}

func (h *api) registerAdminControls(group *gin.RouterGroup) {
	group.GET("/admin/controls", h.listAdminControls)
	group.POST("/admin/controls/rules", h.createAdminControlRule)
	group.PUT("/admin/controls/rules/:id", h.updateAdminControlRule)
	group.DELETE("/admin/controls/rules/:id", h.deleteAdminControlRule)
	group.POST("/admin/controls/evaluate", h.evaluateAdminControls)
	group.POST("/admin/controls/notify-test", h.adminControlNotifyTest)
}

func (h *api) listAdminControls(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.AdminControls.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminControlsResponse{
		Rules: make([]adminControlRuleResponse, 0, len(value.Rules)), AccountsTotal: value.AccountsTotal,
		NextThreshold: value.NextThreshold, NotifyConfigured: value.NotifyConfigured,
		Notifications: make([]adminControlNotificationResponse, 0, len(value.Notifications)),
	}
	if value.AccountsTotal < 0 {
		writeError(c, domain.NewError(domain.CodeInternal, "admin controls receipt is invalid"))
		return
	}
	for _, rule := range value.Rules {
		response.Rules = append(response.Rules, adminControlRuleView(rule))
	}
	for _, notification := range value.Notifications {
		if notification.AccountsTotal < 0 || notification.CreatedAt.IsZero() {
			writeError(c, domain.NewError(domain.CodeInternal, "admin notification receipt is invalid"))
			return
		}
		response.Notifications = append(response.Notifications, adminControlNotificationResponse{
			Kind: notification.Kind, RuleID: notification.RuleID, AccountsTotal: notification.AccountsTotal, CreatedAt: notification.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) createAdminControlRule(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	var request adminControlRuleRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.Threshold == nil || *request.Threshold < 1 {
		writeError(c, domain.InvalidArgument("threshold", "threshold must be positive"))
		return
	}
	value, err := h.deps.AdminControls.Create(c.Request.Context(), principal, *request.Threshold, request.Note)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, adminControlRuleView(value))
}

func (h *api) updateAdminControlRule(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	id, err := adminControlRuleID(c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	var request adminControlRuleUpdateRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || *request.ExpectedVersion < 1 ||
		(request.Threshold == nil && request.Enabled == nil && request.Note == nil && !request.Rearm) {
		writeError(c, domain.InvalidArgument("rule", "expected version and a rule change are required"))
		return
	}
	value, err := h.deps.AdminControls.Update(c.Request.Context(), principal, app.UpdateAdminControlRuleCommand{
		ID: id, ExpectedVersion: *request.ExpectedVersion, Threshold: request.Threshold,
		Enabled: request.Enabled, Note: request.Note, Rearm: request.Rearm,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminControlRuleView(value))
}

func (h *api) deleteAdminControlRule(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	id, err := adminControlRuleID(c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	values, exists := c.Request.URL.Query()["expected_version"]
	if !exists || len(values) != 1 {
		writeError(c, domain.InvalidArgument("expected_version", "expected_version must appear once"))
		return
	}
	expectedVersion, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || expectedVersion < 1 {
		writeError(c, domain.InvalidArgument("expected_version", "expected_version must be positive"))
		return
	}
	if err := h.deps.AdminControls.Delete(c.Request.Context(), principal, id, expectedVersion); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) evaluateAdminControls(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.AdminControls.Evaluate(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := adminControlEvaluationResponse{AccountsTotal: value.AccountsTotal, Fired: make([]adminControlRuleResponse, 0, len(value.Fired))}
	if value.AccountsTotal < 0 {
		writeError(c, domain.NewError(domain.CodeInternal, "admin controls receipt is invalid"))
		return
	}
	for _, rule := range value.Fired {
		response.Fired = append(response.Fired, adminControlRuleView(rule))
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) adminControlNotifyTest(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminControls == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.AdminControls.NotifyTest(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminControlNotifyTestResponse{Sent: value.Sent, Configured: value.Configured, CreatedAt: value.CreatedAt})
}

func adminControlRuleID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, domain.InvalidArgument("rule_id", "rule id must be positive")
	}
	return id, nil
}

func adminControlRuleView(value app.AdminControlRule) adminControlRuleResponse {
	return adminControlRuleResponse{
		ID: value.ID, Threshold: value.Threshold, Enabled: value.Enabled, Note: value.Note,
		CreatedAt: value.CreatedAt, LastFiredAt: value.LastFiredAt, FiredAccounts: value.FiredAccounts, Version: value.Version,
	}
}
