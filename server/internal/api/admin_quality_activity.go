package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
)

func (h *api) registerAdminQualityActivity(group *gin.RouterGroup) {
	group.GET("/admin/quality", h.getAdminQuality)
	group.GET("/admin/activity", h.getAdminActivity)
	group.GET("/admin/places", h.getAdminPlaces)
}

func (h *api) getAdminQuality(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminQualityActivity == nil {
		serviceUnavailable(c)
		return
	}
	days, ok := adminQualityActivityDays(c)
	if !ok {
		return
	}
	quality, err := h.deps.AdminQualityActivity.Quality(c.Request.Context(), principal, days)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, quality)
}

func (h *api) getAdminActivity(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminQualityActivity == nil {
		serviceUnavailable(c)
		return
	}
	days, ok := adminQualityActivityDays(c)
	if !ok {
		return
	}
	activity, err := h.deps.AdminQualityActivity.Activity(c.Request.Context(), principal, days)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, activity)
}

func (h *api) getAdminPlaces(c *gin.Context) {
	principal, ok := requireAdmin(c)
	if !ok {
		return
	}
	if h.deps.AdminQualityActivity == nil {
		serviceUnavailable(c)
		return
	}
	days, ok := adminQualityActivityDays(c)
	if !ok {
		return
	}
	places, err := h.deps.AdminQualityActivity.Places(c.Request.Context(), principal, days)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, places)
}

func adminQualityActivityDays(c *gin.Context) (int, bool) {
	days := 7
	if values, exists := c.Request.URL.Query()["days"]; exists {
		if len(values) != 1 {
			writeError(c, domain.InvalidArgument("days", "days must appear once"))
			return 0, false
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || (parsed != 7 && parsed != 30 && parsed != 90) {
			writeError(c, domain.InvalidArgument("days", "days must be 7, 30, or 90"))
			return 0, false
		}
		days = parsed
	}
	return days, true
}
