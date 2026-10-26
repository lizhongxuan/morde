package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *api) healthz(c *gin.Context) {
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("ok"))
}

func (h *api) readyz(c *gin.Context) {
	if h.deps.Readiness == nil || h.deps.Readiness.CheckReady(c.Request.Context()) != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Status(http.StatusOK)
}
