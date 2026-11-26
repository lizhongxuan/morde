package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/evaluation"
)

// WorkflowTraceReader provides a read-only, source-bound view of one Task's
// persisted execution ledgers.
type WorkflowTraceReader interface {
	GetTaskWorkflowTrace(context.Context, string, string) (evaluation.WorkflowTrace, error)
}

func (h *api) registerAdminWorkflowTrace(group *gin.RouterGroup) {
	if h.deps.WorkflowTraces == nil {
		return
	}
	group.GET("/admin/users/:id/tasks/:task_id/workflow-trace", h.getAdminTaskWorkflowTrace)
}

func (h *api) getAdminTaskWorkflowTrace(c *gin.Context) {
	if _, ok := requireAdmin(c); !ok {
		return
	}
	trace, err := h.deps.WorkflowTraces.GetTaskWorkflowTrace(c.Request.Context(), c.Param("task_id"), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, trace)
}
