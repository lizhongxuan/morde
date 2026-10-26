package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type userPreferencesResponse struct {
	AssistantName   string     `json:"assistant_name"`
	SelectedModelID *string    `json:"selected_model_id"`
	DefaultHostID   *string    `json:"default_host_id"`
	UpdatedAt       *time.Time `json:"updated_at"`
}

type saveUserPreferencesRequest struct {
	AssistantName   string `json:"assistant_name"`
	SelectedModelID string `json:"selected_model_id"`
}

type defaultHostRequest struct {
	HostID string `json:"host_id"`
}

type onboardingModelResponse struct {
	ID           string                    `json:"id"`
	Name         string                    `json:"name"`
	Capabilities onboardingModelCapability `json:"capabilities"`
}

type onboardingModelCapability struct {
	Text   bool `json:"text"`
	Tools  bool `json:"tools"`
	Vision bool `json:"vision"`
}

type onboardingComputerResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type onboardingStatusResponse struct {
	ModelStatus     app.ModelStatus              `json:"model_status"`
	Models          []onboardingModelResponse    `json:"models"`
	SelectedModelID *string                      `json:"selected_model_id"`
	ComputerStatus  app.ComputerStatus           `json:"computer_status"`
	Computers       []onboardingComputerResponse `json:"computers"`
	DefaultHostID   *string                      `json:"default_host_id"`
	CanSubmit       bool                         `json:"can_submit"`
	NextAction      string                       `json:"next_action"`
}

type defaultHostResponse struct {
	Created bool                        `json:"created"`
	Host    *onboardingComputerResponse `json:"host"`
	Status  onboardingStatusResponse    `json:"status"`
}

func (h *api) registerOnboarding(group *gin.RouterGroup) {
	group.GET("/me/preferences", h.getUserPreferences)
	group.PUT("/me/preferences", h.saveUserPreferences)
	group.GET("/onboarding/status", h.getOnboardingStatus)
	group.POST("/me/default-host", h.ensureDefaultHost)
}

func (h *api) getUserPreferences(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	if h.deps.Onboarding == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.Onboarding.Preferences(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserPreferencesResponse(value))
}

func (h *api) saveUserPreferences(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	var request saveUserPreferencesRequest
	if !decodeStrictModelJSON(c, &request) {
		return
	}
	if h.deps.Onboarding == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.Onboarding.SavePreferences(c.Request.Context(), principal, app.SaveUserPreferencesCommand{
		AssistantName: request.AssistantName, SelectedModelID: request.SelectedModelID,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserPreferencesResponse(value))
}

func (h *api) getOnboardingStatus(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	if h.deps.Onboarding == nil {
		serviceUnavailable(c)
		return
	}
	value, err := h.deps.Onboarding.Status(c.Request.Context(), principal)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toOnboardingStatusResponse(value))
}

func (h *api) ensureDefaultHost(c *gin.Context) {
	principal, ok := modelSession(c)
	if !ok {
		return
	}
	var request defaultHostRequest
	if !decodeStrictModelJSON(c, &request) {
		return
	}
	if request.HostID != "" && !domain.ValidHostID(request.HostID) {
		writeError(c, domain.InvalidArgument("host_id", "host id is invalid"))
		return
	}
	if h.deps.Onboarding == nil {
		serviceUnavailable(c)
		return
	}
	result, err := h.deps.Onboarding.EnsureDefaultHost(c.Request.Context(), principal, app.DefaultHostCommand{HostID: request.HostID})
	if err != nil {
		writeError(c, err)
		return
	}
	response := defaultHostResponse{Created: result.Created, Status: toOnboardingStatusResponse(result.Snapshot)}
	if result.Host != nil {
		response.Host = &onboardingComputerResponse{ID: result.Host.ID, Name: result.Host.Name, State: string(result.Host.State)}
	}
	c.JSON(http.StatusOK, response)
}

func toUserPreferencesResponse(value app.UserPreferences) userPreferencesResponse {
	return userPreferencesResponse{
		AssistantName: value.AssistantName, SelectedModelID: stringPointer(value.SelectedModelID),
		DefaultHostID: stringPointer(value.DefaultHostID), UpdatedAt: onboardingTimePointer(value.UpdatedAt),
	}
}

func toOnboardingStatusResponse(value app.OnboardingSnapshot) onboardingStatusResponse {
	models := make([]onboardingModelResponse, 0, len(value.AvailableModels))
	for _, model := range value.AvailableModels {
		models = append(models, onboardingModelResponse{
			ID: model.ID, Name: model.Name, Capabilities: onboardingModelCapability{
				Text: model.Capabilities.Text, Tools: model.Capabilities.Tools, Vision: model.Capabilities.Vision,
			},
		})
	}
	computers := make([]onboardingComputerResponse, 0, len(value.Computers))
	for _, computer := range value.Computers {
		computers = append(computers, onboardingComputerResponse{ID: computer.ID, Name: computer.Name, State: string(computer.State)})
	}
	return onboardingStatusResponse{
		ModelStatus: value.ModelStatus, Models: models, SelectedModelID: stringPointer(value.Preferences.SelectedModelID),
		ComputerStatus: value.ComputerStatus, Computers: computers, DefaultHostID: stringPointer(value.Preferences.DefaultHostID),
		CanSubmit: value.CanSubmit, NextAction: value.NextAction,
	}
}

func onboardingTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
