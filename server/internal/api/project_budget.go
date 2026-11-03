package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectBudgetRoutesService interface {
	GetBudget(context.Context, identity.Principal, string) (app.ProjectBudgetView, error)
	SetBudget(context.Context, identity.Principal, string, app.SetProjectBudgetCommand) (app.ProjectBudgetView, error)
	ListLedger(context.Context, identity.Principal, string, app.PageRequest) (app.ProjectLedgerPage, error)
}

type projectBudgetResponse struct {
	Configured     bool   `json:"configured"`
	Currency       string `json:"currency"`
	LimitMicros    int64  `json:"limit_micros"`
	Version        int64  `json:"version"`
	SpentMicros    int64  `json:"spent_micros"`
	ReservedMicros int64  `json:"reserved_micros"`
	HasUnknown     bool   `json:"has_unknown"`
}

type setProjectBudgetRequest struct {
	Currency        string `json:"currency"`
	LimitMicros     int64  `json:"limit_micros"`
	ExpectedVersion *int64 `json:"expected_version"`
}

type projectUsageResponse struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type projectLedgerEntryResponse struct {
	CallID         string                  `json:"call_id"`
	ActorID        string                  `json:"actor_id"`
	TaskID         string                  `json:"task_id,omitempty"`
	Source         app.ModelCallSource     `json:"source"`
	ModelID        string                  `json:"model_id"`
	RateVersion    int64                   `json:"rate_version"`
	Currency       string                  `json:"currency"`
	Status         app.ProjectLedgerStatus `json:"status"`
	ReservedUsage  projectUsageResponse    `json:"reserved_usage"`
	ActualUsage    *projectUsageResponse   `json:"actual_usage,omitempty"`
	ReservedMicros int64                   `json:"reserved_micros"`
	ActualMicros   *int64                  `json:"actual_micros,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
}

type projectLedgerPageResponse struct {
	Items      []projectLedgerEntryResponse `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
}

// RegisterProjectBudgetRoutes exposes the budget handlers for W08b to mount with the shared API contract.
func RegisterProjectBudgetRoutes(group *gin.RouterGroup, service projectBudgetRoutesService) {
	group.GET("/projects/:id/budget", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		value, err := service.GetBudget(c.Request.Context(), principal, projectID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectBudgetResponse(value))
	})
	group.PUT("/projects/:id/budget", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		var request setProjectBudgetRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.ExpectedVersion == nil {
			writeError(c, domain.InvalidArgument("expected_version", "project budget version is required"))
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		value, err := service.SetBudget(c.Request.Context(), principal, projectID, app.SetProjectBudgetCommand{
			Currency: request.Currency, LimitMicros: request.LimitMicros,
			ExpectedVersion: *request.ExpectedVersion, IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, toProjectBudgetResponse(value))
	})
	group.GET("/projects/:id/budget/ledger", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "project_id", project.ValidID)
		if !ok {
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListLedger(c.Request.Context(), principal, projectID, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]projectLedgerEntryResponse, 0, len(result.Items))
		for _, value := range result.Items {
			items = append(items, toProjectLedgerEntryResponse(value))
		}
		c.JSON(http.StatusOK, projectLedgerPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
}

func toProjectBudgetResponse(value app.ProjectBudgetView) projectBudgetResponse {
	return projectBudgetResponse{
		Configured: value.Configured, Currency: value.Budget.Currency, LimitMicros: value.Budget.LimitMicros,
		Version: value.Budget.Version, SpentMicros: value.SpentMicros, ReservedMicros: value.ReservedMicros,
		HasUnknown: value.HasUnknown,
	}
}

func toProjectLedgerEntryResponse(value app.ProjectLedgerEntry) projectLedgerEntryResponse {
	response := projectLedgerEntryResponse{
		CallID: value.CallID, ActorID: value.ActorID, TaskID: value.TaskID, Source: value.Source,
		ModelID: value.ModelID, RateVersion: value.RateVersion, Currency: value.Currency, Status: value.Status,
		ReservedUsage:  projectUsageResponse{InputTokens: value.ReservedUsage.InputTokens, OutputTokens: value.ReservedUsage.OutputTokens},
		ReservedMicros: value.ReservedMicros, ActualMicros: value.ActualMicros, CreatedAt: value.CreatedAt,
	}
	if value.ActualUsage != nil {
		response.ActualUsage = &projectUsageResponse{InputTokens: value.ActualUsage.InputTokens, OutputTokens: value.ActualUsage.OutputTokens}
	}
	return response
}
