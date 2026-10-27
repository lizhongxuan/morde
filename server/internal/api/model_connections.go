package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelConnectionListResponse struct {
	Items []modelConnectionResponse `json:"items"`
}

type modelConnectionResponse struct {
	ID           string                      `json:"id"`
	Name         string                      `json:"name"`
	BaseURL      string                      `json:"base_url"`
	ModelName    string                      `json:"model_name"`
	Shared       bool                        `json:"shared"`
	HasAPIKey    bool                        `json:"has_api_key"`
	Capabilities modelConnectionCapabilities `json:"capabilities"`
	CheckedAt    *time.Time                  `json:"checked_at"`
	CreatedAt    time.Time                   `json:"created_at"`
	UpdatedAt    time.Time                   `json:"updated_at"`
}

type modelConnectionCapabilities struct {
	Text   bool `json:"text"`
	Tools  bool `json:"tools"`
	Vision bool `json:"vision"`
}

type createModelConnectionRequest struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	ModelName string `json:"model_name"`
	APIKey    string `json:"api_key"`
	Shared    bool   `json:"shared"`
}

type updateModelConnectionRequest struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	ModelName string `json:"model_name"`
	APIKey    string `json:"api_key"`
}

func (h *api) registerModelConnections(group *gin.RouterGroup) {
	group.GET("/available-models", h.listAvailableModels)
	group.GET("/model-connections", h.listModelConnections)
	group.POST("/model-connections", h.createModelConnection)
	group.GET("/model-connections/:id", h.getModelConnection)
	group.PUT("/model-connections/:id", h.updateModelConnection)
	group.DELETE("/model-connections/:id", h.deleteModelConnection)
	group.POST("/model-connections/:id/check", h.checkModelConnection)
}

func (h *api) listAvailableModels(c *gin.Context) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return
	}
	if principal.Kind != identity.KindClient && principal.Kind != identity.KindSession {
		writeError(c, domain.NewError(domain.CodeForbidden, "user credential required"))
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.ModelConnections.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	type model struct {
		ID           string                      `json:"id"`
		Name         string                      `json:"name"`
		ModelName    string                      `json:"model_name"`
		Capabilities modelConnectionCapabilities `json:"capabilities"`
	}
	result := make([]model, 0, len(items))
	for _, item := range items {
		if item.CheckedAt == nil || !item.Capabilities.Text {
			continue
		}
		result = append(result, model{ID: item.ID, Name: item.Name, ModelName: item.ModelName,
			Capabilities: modelConnectionCapabilities{Text: item.Capabilities.Text, Tools: item.Capabilities.Tools, Vision: item.Capabilities.Vision}})
	}
	c.JSON(http.StatusOK, gin.H{"items": result})
}

func (h *api) listModelConnections(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	items, err := h.deps.ModelConnections.List(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	response := modelConnectionListResponse{Items: make([]modelConnectionResponse, 0, len(items))}
	for _, item := range items {
		view, err := toModelConnectionResponse(item)
		if err != nil {
			writeError(c, err)
			return
		}
		response.Items = append(response.Items, view)
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) getModelConnection(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidModelConnectionID)
	if !ok {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.ModelConnections.Get(c.Request.Context(), principal, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toModelConnectionResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) createModelConnection(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	var request createModelConnectionRequest
	if !decodeStrictModelJSON(c, &request) {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.ModelConnections.Create(c.Request.Context(), principal, app.ModelConnectionCommand{
		Name: request.Name, BaseURL: request.BaseURL, ModelName: request.ModelName, APIKey: request.APIKey, Shared: request.Shared,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toModelConnectionResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, response)
}

func (h *api) updateModelConnection(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidModelConnectionID)
	if !ok {
		return
	}
	var request updateModelConnectionRequest
	if !decodeStrictModelJSON(c, &request) {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	item, err := h.deps.ModelConnections.Update(c.Request.Context(), principal, id, app.ModelConnectionCommand{
		Name: request.Name, BaseURL: request.BaseURL, ModelName: request.ModelName, APIKey: request.APIKey,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response, err := toModelConnectionResponse(item)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (h *api) deleteModelConnection(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidModelConnectionID)
	if !ok {
		return
	}
	if h.deps.ModelConnections == nil {
		serviceUnavailable(c)
		return
	}
	if err := h.deps.ModelConnections.Delete(c.Request.Context(), principal, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *api) checkModelConnection(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	id, ok := safeID(c, "id", domain.ValidModelConnectionID)
	if !ok {
		return
	}
	if h.deps.ModelGateway == nil {
		serviceUnavailable(c)
		return
	}
	request := struct {
		Capability string `json:"capability"`
	}{Capability: "all"}
	if c.Request.ContentLength != 0 && !decodeStrictModelJSON(c, &request) {
		return
	}
	capabilities, err := h.deps.ModelGateway.CheckModelCapability(c.Request.Context(), principal, id, c.GetHeader("Idempotency-Key"), request.Capability)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, modelConnectionCapabilities{
		Text: capabilities.Text, Tools: capabilities.Tools, Vision: capabilities.Vision,
	})
}

func modelSession(c *gin.Context) (identity.Principal, bool) {
	principal, ok := principalFrom(c)
	if !ok {
		unauthenticated(c)
		return identity.Principal{}, false
	}
	if principal.Kind != identity.KindSession {
		writeError(c, domain.NewError(domain.CodeForbidden, "portal session required"))
		return identity.Principal{}, false
	}
	return principal, true
}

func toModelConnectionResponse(item app.ModelConnectionView) (modelConnectionResponse, error) {
	if !domain.ValidModelConnectionID(item.ID) || item.Name == "" || item.BaseURL == "" || item.ModelName == "" || !item.HasAPIKey ||
		item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() || (item.CheckedAt == nil && (item.Capabilities.Text || item.Capabilities.Tools || item.Capabilities.Vision)) {
		return modelConnectionResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return modelConnectionResponse{
		ID: item.ID, Name: item.Name, BaseURL: item.BaseURL, ModelName: item.ModelName, Shared: item.Shared, HasAPIKey: item.HasAPIKey,
		Capabilities: modelConnectionCapabilities{Text: item.Capabilities.Text, Tools: item.Capabilities.Tools, Vision: item.Capabilities.Vision},
		CheckedAt:    item.CheckedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

func decodeStrictModelJSON(c *gin.Context, target any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeDecodeError(c, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		writeDecodeError(c, err)
		return false
	}
	return true
}
