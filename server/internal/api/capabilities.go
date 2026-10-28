package api

import (
	"context"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/identity"
)

var serviceInstanceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type capabilitiesResponse struct {
	ServiceInstanceID      string   `json:"service_instance_id"`
	APIVersion             string   `json:"api_version"`
	HubVersion             *string  `json:"hub_version"`
	EventVersion           *string  `json:"event_version"`
	MinimumClientVersion   *string  `json:"minimum_client_version"`
	SupportedFeatures      []string `json:"supported_features"`
	DefaultMaxRequestBytes int64    `json:"default_max_request_bytes"`
}

func (h *api) capabilities(c *gin.Context) {
	// A missing identity cannot safely partition client data across servers.
	if !serviceInstanceIDPattern.MatchString(h.options.ServiceInstanceID) || h.options.ServiceInstanceID == "00000000-0000-0000-0000-000000000000" {
		serviceUnavailable(c)
		return
	}
	features := []string{"password_login"}
	_, clientVerifier := h.deps.Verifier.(interface {
		VerifyClient(context.Context, string) (identity.Principal, error)
	})
	if h.deps.ClientLogin != nil && clientVerifier {
		features = append(features, "client_login", "client_installations")
	}
	if h.deps.ModelConnections != nil && clientVerifier {
		features = append(features, "available_models")
	}
	if h.deps.ModelGateway != nil && clientVerifier {
		features = append(features, "model_gateway")
	}
	if h.deps.ExecutionControl != nil && h.deps.Tasks != nil {
		features = append(features, "cloud_tasks")
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, capabilitiesResponse{
		ServiceInstanceID: h.options.ServiceInstanceID,
		APIVersion:        "1", SupportedFeatures: features, DefaultMaxRequestBytes: h.options.APIMaxBodyBytes,
	})
}
