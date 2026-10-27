package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type ModelUsageAdmin interface {
	ListUnknown(context.Context, identity.Principal, string, int) ([]app.UnknownModelCall, error)
	Reconcile(context.Context, identity.Principal, app.ModelUsageReconcileCommand) (bool, error)
	Budget(context.Context, identity.Principal, string) (app.ModelAccountBudget, error)
	SetBudget(context.Context, identity.Principal, app.ModelAccountBudgetUpdate) (app.ModelAccountBudget, error)
}

type unknownModelCallResponse struct {
	OwnerID        string              `json:"owner_id"`
	Source         app.ModelCallSource `json:"source"`
	CallID         string              `json:"call_id"`
	ModelID        string              `json:"model_id"`
	ProviderModel  string              `json:"provider_model"`
	TaskID         string              `json:"task_id,omitempty"`
	ReservedTokens int64               `json:"reserved_tokens"`
	CreatedAt      time.Time           `json:"created_at"`
	UnknownAt      time.Time           `json:"unknown_at"`
}

type modelUsageReconcileRequest struct {
	OwnerID      string              `json:"owner_id"`
	Source       app.ModelCallSource `json:"source"`
	CallID       string              `json:"call_id"`
	InputTokens  *int64              `json:"input_tokens"`
	OutputTokens *int64              `json:"output_tokens"`
	Evidence     string              `json:"evidence"`
}

type modelAccountBudgetUpdateRequest struct {
	TokenLimit    *int64 `json:"token_limit"`
	MaxConcurrent *int32 `json:"max_concurrent"`
}

type modelAccountBudgetResponse struct {
	OwnerID       string `json:"owner_id"`
	Configured    bool   `json:"configured"`
	TokenLimit    int64  `json:"token_limit"`
	MaxConcurrent int32  `json:"max_concurrent"`
	UsedTokens    int64  `json:"used_tokens"`
	InFlight      int32  `json:"in_flight"`
	HasUnknown    bool   `json:"has_unknown"`
}

func (h *api) registerAdminModelUsage(group *gin.RouterGroup) {
	if h.deps.ModelUsageAdmin == nil {
		return
	}
	group.GET("/admin/model-usage/unknown", h.listUnknownModelUsage)
	group.POST("/admin/model-usage/reconcile", h.reconcileModelUsage)
	group.GET("/admin/users/:id/model-usage-budget", h.getModelAccountBudget)
	group.PUT("/admin/users/:id/model-usage-budget", h.setModelAccountBudget)
}

func (h *api) getModelAccountBudget(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	budget, err := h.deps.ModelUsageAdmin.Budget(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toModelAccountBudgetResponse(budget))
}

func (h *api) setModelAccountBudget(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request modelAccountBudgetUpdateRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.TokenLimit == nil || request.MaxConcurrent == nil {
		writeError(c, domain.InvalidArgument("model_budget", "token_limit and max_concurrent are required"))
		return
	}
	budget, err := h.deps.ModelUsageAdmin.SetBudget(c.Request.Context(), principal, app.ModelAccountBudgetUpdate{
		OwnerID: c.Param("id"), TokenLimit: *request.TokenLimit, MaxConcurrent: *request.MaxConcurrent, IP: c.ClientIP(),
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toModelAccountBudgetResponse(budget))
}

func toModelAccountBudgetResponse(value app.ModelAccountBudget) modelAccountBudgetResponse {
	return modelAccountBudgetResponse{
		OwnerID: value.OwnerID, Configured: value.Configured, TokenLimit: value.TokenLimit,
		MaxConcurrent: value.MaxConcurrent, UsedTokens: value.UsedTokens, InFlight: value.InFlight, HasUnknown: value.HasUnknown,
	}
}

func (h *api) listUnknownModelUsage(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	query := c.Request.URL.Query()
	ownerValues, limitValues := query["owner_id"], query["limit"]
	if len(ownerValues) > 1 || len(limitValues) > 1 {
		writeError(c, domain.InvalidArgument("query", "query parameters must appear once"))
		return
	}
	ownerID, limit := "", 0
	if len(ownerValues) == 1 {
		ownerID = ownerValues[0]
		if ownerID == "" {
			writeError(c, domain.InvalidArgument("owner_id", "invalid owner"))
			return
		}
	}
	if len(limitValues) == 1 {
		parsed, err := strconv.Atoi(limitValues[0])
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(c, domain.InvalidArgument("limit", "limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	items, err := h.deps.ModelUsageAdmin.ListUnknown(c.Request.Context(), principal, ownerID, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	response := make([]unknownModelCallResponse, 0, len(items))
	for _, item := range items {
		response = append(response, unknownModelCallResponse{
			OwnerID: item.OwnerID, Source: item.Source, CallID: item.CallID, ModelID: item.ModelID,
			ProviderModel: item.ProviderModel, TaskID: item.TaskID, ReservedTokens: item.ReservedTokens,
			CreatedAt: item.CreatedAt, UnknownAt: item.UnknownAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": response})
}

func (h *api) reconcileModelUsage(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	var request modelUsageReconcileRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.InputTokens == nil || request.OutputTokens == nil {
		writeError(c, domain.InvalidArgument("model_usage", "verified input and output counts are required"))
		return
	}
	replayed, err := h.deps.ModelUsageAdmin.Reconcile(c.Request.Context(), principal, app.ModelUsageReconcileCommand{
		OwnerID: request.OwnerID, Source: request.Source, CallID: request.CallID, InputTokens: *request.InputTokens,
		OutputTokens: *request.OutputTokens, Evidence: request.Evidence, IP: c.ClientIP(),
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"replayed": replayed})
}
