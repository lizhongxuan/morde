package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalDataService interface {
	GetPersonalObject(context.Context, identity.Principal, string, string) (personal.Object, error)
	ApplyPersonalMutation(context.Context, identity.Principal, app.PersonalMutationCommand) (personal.MutationReceipt, error)
}

type personalMutationRequest struct {
	BaseRevision *int64          `json:"base_revision"`
	Body         json.RawMessage `json:"body"`
}

type personalObjectResponse struct {
	ObjectType string          `json:"object_type"`
	ObjectID   string          `json:"object_id"`
	Revision   int64           `json:"revision"`
	Body       json.RawMessage `json:"body"`
}

type personalMutationReceiptResponse struct {
	ObjectType            string `json:"object_type"`
	ObjectID              string `json:"object_id"`
	ResultRevision        int64  `json:"result_revision"`
	AccountChangeSequence int64  `json:"account_change_sequence"`
	Replay                bool   `json:"replay"`
}

func registerPersonalDataRoutes(group *gin.RouterGroup, service personalDataService) {
	group.GET("/personal-data/:type/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		object, err := service.GetPersonalObject(c.Request.Context(), principal, c.Param("type"), c.Param("id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalObjectResponse{
			ObjectType: object.ObjectType, ObjectID: object.ObjectID, Revision: object.Revision, Body: object.Body,
		})
	})
	group.PUT("/personal-data/:type/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		var request personalMutationRequest
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
		receipt, err := service.ApplyPersonalMutation(c.Request.Context(), principal, app.PersonalMutationCommand{
			MutationID: mutationID, ObjectType: c.Param("type"), ObjectID: c.Param("id"),
			BaseRevision: *request.BaseRevision, Body: request.Body,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, personalMutationReceiptResponse{
			ObjectType: receipt.ObjectType, ObjectID: receipt.ObjectID, ResultRevision: receipt.ResultRevision,
			AccountChangeSequence: receipt.AccountChangeSequence, Replay: receipt.Replay,
		})
	})
	group.DELETE("/personal-data/:type/:id", func(c *gin.Context) {
		if service == nil {
			serviceUnavailable(c)
			return
		}
		principal, ok := principalFrom(c)
		if !ok {
			unauthenticated(c)
			return
		}
		if !optionalMessageActor(c, principal) {
			return
		}
		var request personalMutationRequest
		if !decodeStrictJSON(c, &request) {
			return
		}
		if request.BaseRevision == nil {
			writeError(c, invalidPersonalMutationRequest("base_revision"))
			return
		}
		if len(request.Body) != 0 {
			writeError(c, invalidPersonalMutationRequest("body"))
			return
		}
		mutationID, ok := requiredIdempotencyKey(c)
		if !ok {
			return
		}
		receipt, err := service.ApplyPersonalMutation(c.Request.Context(), principal, app.PersonalMutationCommand{
			MutationID: mutationID, ObjectType: c.Param("type"), ObjectID: c.Param("id"),
			BaseRevision: *request.BaseRevision, Delete: true,
		})
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

func invalidPersonalMutationRequest(field string) *domain.Error {
	return domain.InvalidArgument(field, "personal mutation request is invalid")
}
