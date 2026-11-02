package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalDataQuotaUserService interface {
	GetPersonalDataQuota(context.Context, identity.Principal) (app.PersonalDataQuota, error)
}

type personalDataQuotaUserResponse struct {
	OwnerID            string `json:"owner_id"`
	UsedBytes          int64  `json:"used_bytes"`
	ReservedBytes      int64  `json:"reserved_bytes"`
	QuotaBytes         int64  `json:"quota_bytes"`
	MaxAttachmentBytes int64  `json:"max_attachment_bytes"`
	RemainingBytes     int64  `json:"remaining_bytes"`
	Version            int64  `json:"version"`
}

// RegisterPersonalDataQuotaRoute adds GET /quota to the /api/personal-data group.
func RegisterPersonalDataQuotaRoute(group *gin.RouterGroup, service personalDataQuotaUserService) {
	group.GET("/quota", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		value, err := service.GetPersonalDataQuota(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response, err := personalDataQuotaUserView(value, principal.UserID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	})
}

func personalDataQuotaUserView(value app.PersonalDataQuota, ownerID string) (personalDataQuotaUserResponse, error) {
	if value.OwnerID != ownerID || !domain.ValidUserID(value.OwnerID) || value.UsedBytes < 0 || value.ReservedBytes < 0 ||
		value.QuotaBytes < 1 || value.MaxAttachmentBytes < 1 || value.MaxAttachmentBytes > personal.MaxAttachmentBytes || value.Version < 1 {
		return personalDataQuotaUserResponse{}, domain.NewError(domain.CodeInternal, "personal data quota is invalid")
	}
	remaining := int64(0)
	if value.UsedBytes < value.QuotaBytes {
		available := value.QuotaBytes - value.UsedBytes
		if value.ReservedBytes < available {
			remaining = available - value.ReservedBytes
		}
	}
	return personalDataQuotaUserResponse{
		OwnerID: value.OwnerID, UsedBytes: value.UsedBytes, ReservedBytes: value.ReservedBytes,
		QuotaBytes: value.QuotaBytes, MaxAttachmentBytes: value.MaxAttachmentBytes,
		RemainingBytes: remaining, Version: value.Version,
	}, nil
}
