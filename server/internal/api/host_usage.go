package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type HostUsageReceiptService interface {
	Receipt(context.Context, string, int64) (host.UsageReceipt, error)
	ProjectReceipt(context.Context, identity.Principal, string, string) (app.ProjectHostUsageReceipt, error)
}

type hostUsageReceiptResponse struct {
	HostID                    string                  `json:"host_id"`
	RuntimeGeneration         int64                   `json:"runtime_generation"`
	Status                    host.UsageIntervalState `json:"status"`
	Samples                   int64                   `json:"samples"`
	UnknownIntervals          int64                   `json:"unknown_intervals"`
	UnknownReason             host.UsageUnknownReason `json:"unknown_reason,omitempty"`
	CalculationMethod         string                  `json:"calculation_method"`
	CPUSeconds                float64                 `json:"cpu_seconds"`
	EstimatedMemoryMiBSeconds float64                 `json:"estimated_memory_mib_seconds"`
	PriceStatus               string                  `json:"price_status"`
	PriceUnknownReason        string                  `json:"price_unknown_reason"`
}

type projectHostUsageReceiptResponse struct {
	ProjectID                 string                  `json:"project_id"`
	HostID                    string                  `json:"host_id"`
	Status                    host.UsageIntervalState `json:"status"`
	Samples                   int64                   `json:"samples"`
	UnknownIntervals          int64                   `json:"unknown_intervals"`
	UnknownReason             host.UsageUnknownReason `json:"unknown_reason,omitempty"`
	CalculationMethod         string                  `json:"calculation_method"`
	CPUSeconds                float64                 `json:"cpu_seconds"`
	EstimatedMemoryMiBSeconds float64                 `json:"estimated_memory_mib_seconds"`
	PriceStatus               string                  `json:"price_status"`
	PriceUnknownReason        string                  `json:"price_unknown_reason"`
}

func RegisterProjectHostUsageRoutes(group *gin.RouterGroup, service HostUsageReceiptService) {
	group.GET("/projects/:id/hosts/:host_id/usage", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		projectID, ok := safeID(c, "id", project.ValidID)
		if !ok {
			return
		}
		hostID, ok := safeIDParam(c, "host_id", "host_id", domain.ValidHostID)
		if !ok {
			return
		}
		receipt, err := service.ProjectReceipt(c.Request.Context(), principal, projectID, hostID)
		if err != nil {
			writeError(c, err)
			return
		}
		if receipt.ProjectID != projectID || receipt.HostID != hostID || receipt.Samples < 0 ||
			receipt.UnknownIntervals < 0 || receipt.UnknownIntervals > receipt.Samples ||
			!host.UsageMeasured(receipt.CPUSeconds, receipt.EstimatedMemoryMiBSeconds) ||
			!validHostUsageIntervalState(receipt.Status) || receipt.CalculationMethod != host.UsageCalculationMethod ||
			receipt.PriceStatus != "unknown" || receipt.PriceUnknownReason == "" {
			writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, projectHostUsageReceiptResponse{
			ProjectID: receipt.ProjectID, HostID: receipt.HostID, Status: receipt.Status,
			Samples: receipt.Samples, UnknownIntervals: receipt.UnknownIntervals, UnknownReason: receipt.UnknownReason,
			CalculationMethod: receipt.CalculationMethod, CPUSeconds: receipt.CPUSeconds,
			EstimatedMemoryMiBSeconds: receipt.EstimatedMemoryMiBSeconds,
			PriceStatus:               receipt.PriceStatus, PriceUnknownReason: receipt.PriceUnknownReason,
		})
	})
}

func (h *api) getHostUsageReceipt(c *gin.Context) {
	if h.deps.HostUsage == nil {
		serviceUnavailable(c)
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
	value, err := h.deps.Hosts.Get(c.Request.Context(), principal, hostID)
	if err != nil {
		writeError(c, err)
		return
	}
	if value.RuntimeGeneration < 1 {
		writeError(c, domain.NewError(domain.CodeHostNotReady, "Host usage generation is unavailable"))
		return
	}
	receipt, err := h.deps.HostUsage.Receipt(c.Request.Context(), hostID, value.RuntimeGeneration)
	if err != nil {
		writeError(c, err)
		return
	}
	if receipt.HostID != hostID || receipt.RuntimeGeneration != value.RuntimeGeneration || receipt.Samples < 0 ||
		receipt.UnknownIntervals < 0 || !host.UsageMeasured(receipt.CPUSeconds, receipt.EstimatedMemoryMiBSeconds) ||
		!validHostUsageIntervalState(receipt.Status) || receipt.CalculationMethod != host.UsageCalculationMethod ||
		receipt.PriceStatus != "unknown" || receipt.PriceUnknownReason == "" {
		writeError(c, domain.NewError(domain.CodeInternal, "internal error"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, hostUsageReceiptResponse{
		HostID: receipt.HostID, RuntimeGeneration: receipt.RuntimeGeneration, Status: receipt.Status,
		Samples: receipt.Samples, UnknownIntervals: receipt.UnknownIntervals, UnknownReason: receipt.UnknownReason,
		CalculationMethod: receipt.CalculationMethod, CPUSeconds: receipt.CPUSeconds,
		EstimatedMemoryMiBSeconds: receipt.EstimatedMemoryMiBSeconds,
		PriceStatus:               receipt.PriceStatus, PriceUnknownReason: receipt.PriceUnknownReason,
	})
}

func validHostUsageIntervalState(value host.UsageIntervalState) bool {
	switch value {
	case host.UsageIntervalUnknown, host.UsageIntervalMeasured, host.UsageIntervalPartial:
		return true
	default:
		return false
	}
}
