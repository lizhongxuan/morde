package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

func TestGetHostUsageReceiptRequiresHostReadAndReturnsSummary(t *testing.T) {
	services := newServices()
	usage := &hostUsageReceiptStub{receipt: host.UsageReceipt{
		HostID: testHostID, RuntimeGeneration: 1, Status: host.UsageIntervalMeasured,
		Samples: 3, CPUSeconds: 1.5, EstimatedMemoryMiBSeconds: 512,
		CalculationMethod: host.UsageCalculationMethod, PriceStatus: "unknown", PriceUnknownReason: "host_rate_unconfigured",
	}}
	deps := services.deps()
	deps.HostUsage = usage
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/hosts/"+testHostID+"/usage", cookie, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var value hostUsageReceiptResponse
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode usage receipt: %v", err)
	}
	if value.HostID != testHostID || value.RuntimeGeneration != 1 || value.Status != host.UsageIntervalMeasured ||
		value.Samples != 3 || value.CPUSeconds != 1.5 || value.EstimatedMemoryMiBSeconds != 512 ||
		value.PriceStatus != "unknown" || value.PriceUnknownReason != "host_rate_unconfigured" ||
		usage.calls != 1 || usage.hostID != testHostID || usage.generation != 1 ||
		response.Header().Get("Cache-Control") != "no-store" || response.Body.String() == "" {
		t.Fatalf("usage response/call = %#v / %#v", value, usage)
	}
	for _, excluded := range []string{"observations", "intervals", "project_id", "raw_payload", "container_id"} {
		if containsJSONKey(response.Body.Bytes(), excluded) {
			t.Fatalf("usage receipt leaked %q: %s", excluded, response.Body)
		}
	}
	assertError(t, request(router, http.MethodGet, "/api/hosts/"+testHostID+"/usage", "", "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestGetHostUsageReceiptDoesNotReadAfterHostAuthorizationFails(t *testing.T) {
	services := newServices()
	services.hosts.err = domain.NewError(domain.CodeNotFound, "host not found")
	usage := &hostUsageReceiptStub{}
	deps := services.deps()
	deps.HostUsage = usage
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/hosts/"+testHostID+"/usage", cookie, "")
	if response.Code != http.StatusNotFound || usage.calls != 0 {
		t.Fatalf("unauthorized usage response = %d %s, calls=%d", response.Code, response.Body, usage.calls)
	}
}

func TestGetProjectHostUsageReceiptReturnsScopedSummary(t *testing.T) {
	services := newServices()
	usage := &hostUsageReceiptStub{projectReceipt: app.ProjectHostUsageReceipt{
		ProjectID: "j1234567", HostID: testHostID, Status: host.UsageIntervalMeasured,
		Samples: 2, CPUSeconds: 1.25, EstimatedMemoryMiBSeconds: 256,
		CalculationMethod: host.UsageCalculationMethod, PriceStatus: "unknown", PriceUnknownReason: "host_rate_unconfigured",
	}}
	deps := services.deps()
	deps.HostUsage = usage
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/projects/j1234567/hosts/"+testHostID+"/usage", cookie, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var value projectHostUsageReceiptResponse
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode project usage receipt: %v", err)
	}
	if value.ProjectID != "j1234567" || value.HostID != testHostID || value.Samples != 2 || value.CPUSeconds != 1.25 ||
		value.EstimatedMemoryMiBSeconds != 256 || value.PriceStatus != "unknown" ||
		usage.projectCalls != 1 || usage.projectID != "j1234567" || usage.hostID != testHostID ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("project usage response/call = %#v / %#v", value, usage)
	}
	for _, excluded := range []string{"observations", "intervals", "workspace_id", "project_resource_version", "raw_payload", "container_id"} {
		if containsJSONKey(response.Body.Bytes(), excluded) {
			t.Fatalf("project usage receipt leaked %q: %s", excluded, response.Body)
		}
	}
}

func TestGetProjectHostUsageReceiptPropagatesMembershipDenial(t *testing.T) {
	services := newServices()
	usage := &hostUsageReceiptStub{projectErr: domain.NewError(domain.CodeForbidden, "project role is insufficient")}
	deps := services.deps()
	deps.HostUsage = usage
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookie(router, http.MethodGet, "/api/projects/j1234567/hosts/"+testHostID+"/usage", cookie, "")
	if response.Code != http.StatusForbidden || usage.projectCalls != 1 {
		t.Fatalf("project usage denial = %d %s calls=%d", response.Code, response.Body, usage.projectCalls)
	}
}

type hostUsageReceiptStub struct {
	receipt        host.UsageReceipt
	err            error
	calls          int
	hostID         string
	generation     int64
	projectReceipt app.ProjectHostUsageReceipt
	projectErr     error
	projectCalls   int
	projectID      string
}

func (s *hostUsageReceiptStub) Receipt(_ context.Context, hostID string, generation int64) (host.UsageReceipt, error) {
	s.calls++
	s.hostID, s.generation = hostID, generation
	return s.receipt, s.err
}

func (s *hostUsageReceiptStub) ProjectReceipt(_ context.Context, _ identity.Principal, projectID, hostID string) (app.ProjectHostUsageReceipt, error) {
	s.projectCalls++
	s.projectID, s.hostID = projectID, hostID
	return s.projectReceipt, s.projectErr
}

func containsJSONKey(data []byte, key string) bool {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return false
	}
	_, exists := value[key]
	return exists
}
