package contract

import (
	"testing"
	"time"
)

func TestApplicationLaunchContractUsesLaunchIDAndClosedErrorSet(t *testing.T) {
	now := time.Now().UTC()
	fence := HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "runtime-instance", ControlVersion: 2}
	request := ApplicationLaunchRequest{ProtocolVersion: ProtocolVersion, HostFence: fence, LaunchID: "launch_wechat_01"}
	if !ValidApplicationLaunchRequest(request) {
		t.Fatal("valid launch request rejected")
	}
	operation := ApplicationLaunchOperation{
		ID: request.LaunchID, ApplicationID: "wechat", Status: ApplicationLaunchStarting,
		CreatedAt: now, UpdatedAt: now,
	}
	response := ApplicationLaunchResponse{ProtocolVersion: ProtocolVersion, HostFence: fence, Operation: operation}
	if !ValidApplicationLaunchResponse(response) {
		t.Fatal("valid launch response rejected")
	}

	for _, invalid := range []ApplicationLaunchOperation{
		{ID: "app_0123456789abcdef01234567", ApplicationID: "wechat", Status: ApplicationLaunchStarting, CreatedAt: now, UpdatedAt: now},
		{ID: request.LaunchID, ApplicationID: "wechat", Status: ApplicationLaunchFailed, ErrorCode: "runtime_unavailable", CreatedAt: now, UpdatedAt: now},
		{ID: request.LaunchID, ApplicationID: "wechat", Status: ApplicationLaunchStopped, ErrorCode: "stop_unconfirmed", CreatedAt: now, UpdatedAt: now},
		{ID: request.LaunchID, ApplicationID: "wechat", Status: ApplicationLaunchFailed, ErrorCode: "stop_unconfirmed", CreatedAt: now, UpdatedAt: now},
	} {
		if ValidApplicationLaunchOperation(invalid) {
			t.Fatalf("invalid launch operation accepted: %#v", invalid)
		}
	}
	for _, launchID := range []string{"wechat_01", "launch_", "launch space", "launch_中文", "launch_" + string(make([]byte, 122))} {
		request.LaunchID = launchID
		if ValidApplicationLaunchRequest(request) {
			t.Fatalf("invalid launch id accepted: %q", launchID)
		}
	}
}
