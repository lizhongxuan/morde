package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func TestCapabilitiesArePublicAndOnlyAdvertiseAssembledServices(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.ClientLogin = &clientLoginHTTPStub{}
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("capabilities = %d %s", response.Code, response.Body)
	}
	var result capabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ServiceInstanceID != options.ServiceInstanceID || result.APIVersion != "1" || result.DefaultMaxRequestBytes != options.APIMaxBodyBytes {
		t.Fatalf("capability identity or limits = %+v", result)
	}
	if result.HubVersion != nil || result.EventVersion != nil || result.MinimumClientVersion != nil {
		t.Fatalf("unimplemented protocols must not be claimed: %+v", result)
	}
	for _, feature := range []string{"password_login", "client_login", "client_installations"} {
		if !slices.Contains(result.SupportedFeatures, feature) {
			t.Fatalf("missing %s in %v", feature, result.SupportedFeatures)
		}
	}
	for _, feature := range []string{"available_models", "cloud_tasks", "hub", "personal_sync"} {
		if slices.Contains(result.SupportedFeatures, feature) {
			t.Fatalf("unassembled or unimplemented feature %s was advertised", feature)
		}
	}
}

func TestCapabilitiesAdvertiseCloudTasksOnlyWithExecutionControl(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.Tasks = &taskServiceStub{}
	deps.ExecutionControl = &executionControlServiceStub{}
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
	var result capabilitiesResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || !slices.Contains(result.SupportedFeatures, "cloud_tasks") {
		t.Fatalf("enabled task capabilities = %d %s; want cloud_tasks", response.Code, response.Body)
	}
}

func TestCapabilitiesFailClosedWithoutValidInstanceIdentity(t *testing.T) {
	for _, id := range []string{"", "cloudhost", "00000000-0000-0000-0000-000000000000"} {
		t.Run(id, func(t *testing.T) {
			options := validOptions()
			options.ServiceInstanceID = id
			router, err := New(newServices().deps(), options)
			if err != nil {
				t.Fatal(err)
			}
			response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("invalid service identity returned %d %s", response.Code, response.Body)
			}
		})
	}
}
