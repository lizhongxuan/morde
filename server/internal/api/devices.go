package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type deviceService interface {
	List(context.Context, identity.Principal) ([]app.DeviceInfo, error)
	Rename(context.Context, identity.Principal, string, string) error
	Revoke(context.Context, identity.Principal, string, string) error
}

type deviceInfoResponse struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Platform    string     `json:"platform"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Revoked     bool       `json:"revoked"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Online      bool       `json:"online"`
}

type renameDeviceRequest struct {
	DisplayName string `json:"display_name"`
}

func registerDeviceRoutes(group *gin.RouterGroup, service deviceService) {
	group.GET("/devices", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		items, err := service.List(c.Request.Context(), principal)
		if err != nil {
			writeError(c, err)
			return
		}
		response := struct {
			Items []deviceInfoResponse `json:"items"`
		}{Items: make([]deviceInfoResponse, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, deviceInfoResponse{
				ID: item.ID, DisplayName: item.DisplayName, Platform: item.Platform, ExpiresAt: item.ExpiresAt,
				Revoked: item.Revoked, CreatedAt: item.CreatedAt, LastUsedAt: item.LastUsedAt, Online: item.Online,
			})
		}
		c.JSON(http.StatusOK, response)
	})
	group.PATCH("/devices/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidClientInstallationID)
		if !ok {
			return
		}
		var request renameDeviceRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if err := service.Rename(c.Request.Context(), principal, id, request.DisplayName); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	group.DELETE("/devices/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		id, ok := safeID(c, "id", domain.ValidClientInstallationID)
		if !ok {
			return
		}
		if err := service.Revoke(c.Request.Context(), principal, id, c.ClientIP()); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}
