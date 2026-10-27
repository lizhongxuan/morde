package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"morde/server/internal/app"
)

func TestOnlineResourcesRequestAndReadback(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/sandboxes/sandbox-1/resources" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			var body struct {
				ResourceLimits         liveResourceLimits `json:"resourceLimits"`
				ExpectedResourceLimits liveResourceLimits `json:"expectedResourceLimits"`
				ExpectedContainerID    string             `json:"expectedContainerId"`
				ExpectedStartedAt      string             `json:"expectedStartedAt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.ResourceLimits.CPU != "1500m" || body.ResourceLimits.Memory != "3072Mi" ||
				body.ExpectedResourceLimits.CPU != "1000m" || body.ExpectedResourceLimits.Memory != "2048Mi" ||
				body.ExpectedContainerID != "container-1" || body.ExpectedStartedAt != "start-1" {
				t.Errorf("update request = %#v", body)
			}
			_, _ = w.Write([]byte(`{"resourceLimits":{"cpu":"1500m","memory":"3072Mi"},"containerId":"container-1","startedAt":"start-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"resourceLimits":{"cpu":"1000m","memory":"2048Mi"},"containerId":"container-1","startedAt":"start-1"}`))
	}))
	defer server.Close()
	client := newTestClient(server.URL, nil)
	client.checkNetworkPolicy = func(context.Context, string, string) error { return nil }
	before, err := client.ReadResources(context.Background(), "sandbox-1")
	if err != nil || before.Resources != (app.HostResources{CPUMilli: 1000, MemMiB: 2048}) {
		t.Fatalf("before = %#v, %v", before, err)
	}
	after, err := client.UpdateResources(context.Background(), "sandbox-1", before, app.HostResources{CPUMilli: 1500, MemMiB: 3072})
	if err != nil || after.Resources != (app.HostResources{CPUMilli: 1500, MemMiB: 3072}) || requests != 2 {
		t.Fatalf("after = %#v, err=%v, requests=%d", after, err, requests)
	}
}

func TestOnlineResourceUpdateFailureIsNotRetried(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newTestClient(server.URL, nil)
	client.checkNetworkPolicy = func(context.Context, string, string) error { return nil }
	_, err := client.UpdateResources(context.Background(), "sandbox-1", app.SandboxResourceStatus{
		Resources: app.HostResources{CPUMilli: 1000, MemMiB: 2048}, ContainerID: "container-1", StartedAt: "start-1",
	}, app.HostResources{CPUMilli: 1500, MemMiB: 3072})
	if !errors.Is(err, app.ErrSandboxUnavailable) || requests != 1 {
		t.Fatalf("error=%v requests=%d", err, requests)
	}
}
