package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/personal"
)

type adminPersonalDataQuotaResponse struct {
	UsedBytes                 int64 `json:"used_bytes"`
	ReservedBytes             int64 `json:"reserved_bytes"`
	QuotaBytes                int64 `json:"quota_bytes"`
	MaxAttachmentBytes        int64 `json:"max_attachment_bytes"`
	Version                   int64 `json:"version"`
	DefaultQuotaBytes         int64 `json:"default_quota_bytes"`
	DefaultMaxAttachmentBytes int64 `json:"default_max_attachment_bytes"`
}

type updateAdminPersonalDataQuotaRequest struct {
	ExpectedVersion    *int64 `json:"expected_version"`
	QuotaBytes         *int64 `json:"quota_bytes"`
	MaxAttachmentBytes *int64 `json:"max_attachment_bytes"`
}

func (h *api) registerAdminPersonalDataQuota(group *gin.RouterGroup) {
	group.GET("/admin/users/:id/personal-data-quota", h.getAdminPersonalDataQuota)
	group.PATCH("/admin/users/:id/personal-data-quota", h.updateAdminPersonalDataQuota)
}

func (h *api) getAdminPersonalDataQuota(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.PersonalDataQuota == nil {
		serviceUnavailable(c)
		return
	}
	quota, err := h.deps.PersonalDataQuota.GetAdminPersonalDataQuota(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := adminPersonalDataQuotaView(quota)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) updateAdminPersonalDataQuota(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.PersonalDataQuota == nil || h.deps.AdminAudit == nil {
		serviceUnavailable(c)
		return
	}
	var request updateAdminPersonalDataQuotaRequest
	if !decodeStrictJSON(c, &request) {
		return
	}
	if request.ExpectedVersion == nil || request.QuotaBytes == nil || request.MaxAttachmentBytes == nil || *request.ExpectedVersion < 1 {
		writeError(c, domain.InvalidArgument("personal_data_quota", "quota version and limits are required"))
		return
	}
	ownerID := c.Param("id")
	before, err := h.deps.PersonalDataQuota.GetAdminPersonalDataQuota(c.Request.Context(), principal, ownerID)
	if err != nil {
		writeError(c, err)
		return
	}
	updated, err := h.deps.PersonalDataQuota.SetAdminPersonalDataQuota(c.Request.Context(), principal, ownerID,
		*request.ExpectedVersion, *request.QuotaBytes, *request.MaxAttachmentBytes)
	if err != nil {
		writeError(c, err)
		return
	}
	if err := h.deps.AdminAudit.Write(c.Request.Context(), audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorSession, Action: audit.ActionPersonalDataQuotaUpdate,
		Target: ownerID, IP: c.ClientIP(), Detail: map[string]any{
			"quota_bytes_before": before.QuotaBytes, "quota_bytes_after": updated.QuotaBytes,
			"max_attachment_bytes_before": before.MaxAttachmentBytes, "max_attachment_bytes_after": updated.MaxAttachmentBytes,
			"expected_version": *request.ExpectedVersion, "version": updated.Version,
		},
	}); err != nil {
		writeError(c, err)
		return
	}
	response, err := adminPersonalDataQuotaView(updated)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func adminPersonalDataQuotaView(value app.PersonalDataQuota) (adminPersonalDataQuotaResponse, error) {
	if value.UsedBytes < 0 || value.ReservedBytes < 0 || value.QuotaBytes < 1 || value.MaxAttachmentBytes < 1 || value.Version < 1 {
		return adminPersonalDataQuotaResponse{}, domain.NewError(domain.CodeInternal, "personal data quota is invalid")
	}
	return adminPersonalDataQuotaResponse{
		UsedBytes: value.UsedBytes, ReservedBytes: value.ReservedBytes, QuotaBytes: value.QuotaBytes,
		MaxAttachmentBytes: value.MaxAttachmentBytes, Version: value.Version,
		DefaultQuotaBytes: app.DefaultPersonalDataQuotaBytes, DefaultMaxAttachmentBytes: personal.DefaultMaxAttachmentBytes,
	}, nil
}
