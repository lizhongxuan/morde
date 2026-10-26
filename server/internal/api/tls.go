package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
)

func (h *api) authorizeTLS(c *gin.Context) {
	name := strings.ToLower(strings.TrimSuffix(c.Query("domain"), "."))
	if name == strings.ToLower(h.options.PortalDomain) {
		c.Status(http.StatusNoContent)
		return
	}

	suffix := "." + strings.ToLower(strings.TrimSuffix(h.options.HostSuffix, "."))
	label, ok := strings.CutSuffix(name, suffix)
	if !ok || strings.Contains(label, ".") {
		c.Status(http.StatusForbidden)
		return
	}
	hostID := label
	if portText, candidateID, preview := strings.Cut(label, "-"); preview {
		port, err := strconv.Atoi(portText)
		if err != nil || !domain.ValidPreviewPort(portText) || !domain.ValidPreviewTargetPort(port) || port < h.options.PreviewPortMin || previewPortDenied(port, h.options.PreviewPortDeny) {
			c.Status(http.StatusForbidden)
			return
		}
		hostID = candidateID
	}
	if !domain.ValidHostID(hostID) || !h.deps.TLSAuthorizer.AuthorizeTLS(c.Request.Context(), hostID) {
		c.Status(http.StatusForbidden)
		return
	}
	c.Status(http.StatusNoContent)
}

func previewPortDenied(port int, denied []int) bool {
	for _, value := range denied {
		if port == value {
			return true
		}
	}
	return false
}
