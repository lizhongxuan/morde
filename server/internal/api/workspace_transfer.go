package api

import (
	"context"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type WorkspaceOwnershipTransferService interface {
	RequestWorkspaceTransfer(context.Context, identity.Principal, string, string, int64, string) (workspace.OwnershipTransfer, bool, error)
	GetWorkspaceTransfer(context.Context, identity.Principal, string, string) (workspace.OwnershipTransfer, error)
	AcceptWorkspaceTransfer(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error)
	CancelWorkspaceTransfer(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error)
}

type requestWorkspaceOwnershipTransfer struct {
	TargetUserID     string `json:"target_user_id"`
	ExpectedRevision *int64 `json:"expected_revision"`
}

func (h *api) registerWorkspaceOwnershipTransfers(group *gin.RouterGroup, service WorkspaceOwnershipTransferService) {
	group.POST("/workspaces/:id/ownership-transfers", h.requestWorkspaceOwnershipTransfer(service))
	group.GET("/workspaces/:id/ownership-transfers/:transfer_id", h.getWorkspaceOwnershipTransfer(service))
	group.POST("/workspaces/:id/ownership-transfers/:transfer_id/accept", h.acceptWorkspaceOwnershipTransfer(service))
	group.POST("/workspaces/:id/ownership-transfers/:transfer_id/cancel", h.cancelWorkspaceOwnershipTransfer(service))
}

func (h *api) requestWorkspaceOwnershipTransfer(service WorkspaceOwnershipTransferService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
		if !ok {
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		var request requestWorkspaceOwnershipTransfer
		if !decodeStrictJSON(c, &request) {
			return
		}
		if !domain.ValidUserID(request.TargetUserID) || request.ExpectedRevision == nil || *request.ExpectedRevision < 0 {
			writeError(c, domain.InvalidArgument("ownership_transfer", "workspace ownership transfer request is invalid"))
			return
		}
		transfer, replayed, err := service.RequestWorkspaceTransfer(c.Request.Context(), principal, workspaceID,
			request.TargetUserID, *request.ExpectedRevision, idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		c.JSON(status, transfer)
	}
}

func (h *api) getWorkspaceOwnershipTransfer(service WorkspaceOwnershipTransferService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, transferID, ok := workspaceOwnershipTransferPathIDs(c)
		if !ok {
			return
		}
		transfer, err := service.GetWorkspaceTransfer(c.Request.Context(), principal, workspaceID, transferID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, transfer)
	}
}

func (h *api) acceptWorkspaceOwnershipTransfer(service WorkspaceOwnershipTransferService) gin.HandlerFunc {
	return h.workspaceOwnershipTransferAction(service, func(ctx context.Context, principal identity.Principal,
		workspaceID, transferID, key string) (workspace.OwnershipTransfer, error) {
		return service.AcceptWorkspaceTransfer(ctx, principal, workspaceID, transferID, key)
	})
}

func (h *api) cancelWorkspaceOwnershipTransfer(service WorkspaceOwnershipTransferService) gin.HandlerFunc {
	return h.workspaceOwnershipTransferAction(service, func(ctx context.Context, principal identity.Principal,
		workspaceID, transferID, key string) (workspace.OwnershipTransfer, error) {
		return service.CancelWorkspaceTransfer(ctx, principal, workspaceID, transferID, key)
	})
}

func (h *api) workspaceOwnershipTransferAction(service WorkspaceOwnershipTransferService,
	action func(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		workspaceID, transferID, ok := workspaceOwnershipTransferPathIDs(c)
		if !ok {
			return
		}
		idempotencyKey, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if err != nil {
			writeDecodeError(c, err)
			return
		}
		if len(body) != 0 {
			writeError(c, domain.InvalidArgument("body", "request body must be empty"))
			return
		}
		transfer, err := action(c.Request.Context(), principal, workspaceID, transferID, idempotencyKey)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, transfer)
	}
}

func workspaceOwnershipTransferPathIDs(c *gin.Context) (string, string, bool) {
	workspaceID, ok := safeID(c, "id", domain.ValidWorkspaceID)
	if !ok {
		return "", "", false
	}
	transferID, ok := safeIDParam(c, "transfer_id", "transfer_id", workspace.ValidOwnershipTransferID)
	if !ok {
		return "", "", false
	}
	return workspaceID, transferID, true
}
