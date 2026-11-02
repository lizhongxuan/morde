package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type goalRoutesService interface {
	List(context.Context, identity.Principal, app.PageRequest) (app.GoalPage, error)
	Get(context.Context, identity.Principal, string) (personal.Object, error)
	Create(context.Context, identity.Principal, string, json.RawMessage, string) (personal.MutationReceipt, error)
	Patch(context.Context, identity.Principal, string, int64, json.RawMessage, string) (personal.MutationReceipt, error)
	Delete(context.Context, identity.Principal, string, int64, string) (personal.MutationReceipt, error)
}

type goalCreateRequest struct {
	ID   string          `json:"id"`
	Body json.RawMessage `json:"body"`
}

type goalPatchRequest struct {
	BaseRevision *int64          `json:"base_revision"`
	Body         json.RawMessage `json:"body"`
}

type goalDeleteRequest struct {
	BaseRevision *int64 `json:"base_revision"`
}

type goalPageResponse struct {
	Items      []personalObjectResponse `json:"items"`
	NextCursor *string                  `json:"next_cursor"`
}

func registerGoalRoutes(group *gin.RouterGroup, service goalRoutesService) {
	group.GET("/goals", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		page, ok := sitePageRequest(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), principal, page)
		if err != nil {
			writeError(c, err)
			return
		}
		items := make([]personalObjectResponse, 0, len(result.Items))
		for _, object := range result.Items {
			items = append(items, personalObjectResponse{
				ObjectType: object.ObjectType, ObjectID: object.ObjectID, Revision: object.Revision, Body: object.Body,
			})
		}
		c.JSON(http.StatusOK, goalPageResponse{Items: items, NextCursor: cursorPointer(result.NextCursor)})
	})
	group.POST("/goals", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request goalCreateRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if err := personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, request.ID); err != nil {
			writeError(c, err)
			return
		}
		if len(request.Body) == 0 {
			writeError(c, invalidPersonalMutationRequest("body"))
			return
		}
		mutationID, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.Create(c.Request.Context(), principal, request.ID, request.Body, mutationID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, personalMutationReceiptResponse{
			ObjectType: receipt.ObjectType, ObjectID: receipt.ObjectID, ResultRevision: receipt.ResultRevision,
			AccountChangeSequence: receipt.AccountChangeSequence, Replay: receipt.Replay,
		})
	})
	group.GET("/goals/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		object, err := service.Get(c.Request.Context(), principal, c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalObjectResponse{
			ObjectType: object.ObjectType, ObjectID: object.ObjectID, Revision: object.Revision, Body: object.Body,
		})
	})
	group.PATCH("/goals/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request goalPatchRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil || len(request.Body) == 0 {
			field := "base_revision"
			if request.BaseRevision != nil {
				field = "body"
			}
			writeError(c, invalidPersonalMutationRequest(field))
			return
		}
		mutationID, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.Patch(c.Request.Context(), principal, c.Param("id"), *request.BaseRevision, request.Body, mutationID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalMutationReceiptResponse{
			ObjectType: receipt.ObjectType, ObjectID: receipt.ObjectID, ResultRevision: receipt.ResultRevision,
			AccountChangeSequence: receipt.AccountChangeSequence, Replay: receipt.Replay,
		})
	})
	group.DELETE("/goals/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		var request goalDeleteRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil {
			writeError(c, invalidPersonalMutationRequest("base_revision"))
			return
		}
		mutationID, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.Delete(c.Request.Context(), principal, c.Param("id"), *request.BaseRevision, mutationID)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalMutationReceiptResponse{
			ObjectType: receipt.ObjectType, ObjectID: receipt.ObjectID, ResultRevision: receipt.ResultRevision,
			AccountChangeSequence: receipt.AccountChangeSequence, Replay: receipt.Replay,
		})
	})
}
