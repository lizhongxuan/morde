package runtimehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestApplicationLaunchRoutesUseExactFenceAndIdempotentID(t *testing.T) {
	httpHandler, manager, credential := newHTTPHandler(t, 1)
	launcher := &fakeApplicationLauncher{}
	httpHandler.(*handler).launcher = launcher
	fence := currentFence(t, manager, 1)
	requestBody := contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: "launch_wechat_01"}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}

	response := postJSON(httpHandler, credential, "/runtime/v1/applications/wechat/launch", string(encoded))
	if response.Code != http.StatusAccepted || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("launch response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var launch contract.ApplicationLaunchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.Operation.ID != requestBody.LaunchID || launch.HostFence != fence || launcher.launches != 1 {
		t.Fatalf("launch identity/fence mismatch: response=%#v launches=%d", launch, launcher.launches)
	}
	if launcher.applicationID != "wechat" {
		t.Fatalf("launch application ID = %q", launcher.applicationID)
	}

	query := fmt.Sprintf("/runtime/v1/applications/wechat/launches/%s?protocol_version=%s&host_id=%s&runtime_generation=1&runtime_instance_id=%s&control_version=%d",
		requestBody.LaunchID, contract.ProtocolVersion, fence.HostID, fence.RuntimeInstanceID, fence.ControlVersion)
	get := httptest.NewRequest(http.MethodGet, query, nil)
	get.Header.Set("Authorization", "Bearer "+credential)
	get.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	getResponse := httptest.NewRecorder()
	httpHandler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || getResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("query response = %d headers=%v body=%s", getResponse.Code, getResponse.Header(), getResponse.Body.String())
	}
	var queried contract.ApplicationLaunchResponse
	if err := json.Unmarshal(getResponse.Body.Bytes(), &queried); err != nil {
		t.Fatal(err)
	}
	if queried.Operation.ID != requestBody.LaunchID || queried.Operation.Status != contract.ApplicationLaunchRunning || launcher.queries != 1 {
		t.Fatalf("query identity/status mismatch: %#v calls=%d", queried, launcher.queries)
	}

	response = postJSON(httpHandler, credential, "/runtime/v1/applications/wechat/launches/launch_wechat_01/stop", string(encoded))
	if response.Code != http.StatusAccepted || launcher.stops != 1 {
		t.Fatalf("stop response = %d body=%s calls=%d", response.Code, response.Body.String(), launcher.stops)
	}
}

func TestApplicationLaunchRejectsStaleFenceAndPathBodyMismatch(t *testing.T) {
	httpHandler, manager, credential := newHTTPHandler(t, 1)
	launcher := &fakeApplicationLauncher{}
	httpHandler.(*handler).launcher = launcher
	fence := currentFence(t, manager, 1)
	stale := fence
	stale.ControlVersion++
	body, _ := json.Marshal(contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: stale, LaunchID: "launch_wechat_02"})
	response := postJSON(httpHandler, credential, "/runtime/v1/applications/wechat/launch", string(body))
	assertError(t, response, http.StatusConflict, "fence_rejected")
	if launcher.launches != 0 {
		t.Fatalf("stale fence dispatched launch %d times", launcher.launches)
	}

	wrongBody, _ := json.Marshal(contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: "launch_other_02"})
	response = postJSON(httpHandler, credential, "/runtime/v1/applications/wechat/launches/launch_wechat_02/stop", string(wrongBody))
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	if launcher.stops != 0 {
		t.Fatalf("mismatched stop path dispatched %d times", launcher.stops)
	}

	wrongProtocol := "/runtime/v1/applications/wechat/launches/launch_wechat_02?protocol_version=2.0&host_id=h1234567&runtime_generation=1&runtime_instance_id=" + fence.RuntimeInstanceID + "&control_version=0"
	get := httptest.NewRequest(http.MethodGet, wrongProtocol, nil)
	get.Header.Set("Authorization", "Bearer "+credential)
	get.Header.Set(contract.ProtocolHeader, contract.ProtocolVersion)
	getResponse := httptest.NewRecorder()
	httpHandler.ServeHTTP(getResponse, get)
	assertError(t, getResponse, http.StatusConflict, "protocol_incompatible")

	validBody, _ := json.Marshal(contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: "launch_unverified_01"})
	response = postJSON(httpHandler, credential, "/runtime/v1/applications/qq/launch", string(validBody))
	assertError(t, response, http.StatusConflict, "capability_unavailable")
	if launcher.launches != 0 {
		t.Fatalf("unverified app reached launcher: %d calls", launcher.launches)
	}
}

type fakeApplicationLauncher struct {
	applicationID string
	launches      int
	queries       int
	stops         int
}

func (f *fakeApplicationLauncher) Launch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	f.applicationID = applicationID
	f.launches++
	return fakeLaunchOperation(launchID, contract.ApplicationLaunchStarting), nil
}

func (f *fakeApplicationLauncher) QueryLaunch(applicationID string, _ contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	f.applicationID = applicationID
	f.queries++
	return fakeLaunchOperation(launchID, contract.ApplicationLaunchRunning), nil
}

func (f *fakeApplicationLauncher) StopLaunch(applicationID string, _ contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	f.applicationID = applicationID
	f.stops++
	return fakeLaunchOperation(launchID, contract.ApplicationLaunchStopping), nil
}

func fakeLaunchOperation(launchID string, status contract.ApplicationLaunchStatus) contract.ApplicationLaunchOperation {
	now := time.Now().UTC()
	return contract.ApplicationLaunchOperation{ID: launchID, ApplicationID: "wechat", Status: status, CreatedAt: now, UpdatedAt: now}
}

var _ applicationLauncher = (*fakeApplicationLauncher)(nil)
