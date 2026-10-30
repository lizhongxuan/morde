package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestIDs(t *testing.T) {
	for _, tc := range []struct {
		newID func() (string, error)
		valid func(string) bool
	}{
		{NewHostID, ValidHostID},
		{NewWorkspaceID, ValidWorkspaceID},
		{NewUserID, ValidUserID},
		{NewTokenID, ValidTokenID},
		{NewConversationID, ValidConversationID},
		{NewTaskID, ValidTaskID},
		{NewMessageID, ValidMessageID},
		{NewStepID, ValidStepID},
		{NewOperationID, ValidOperationID},
		{NewBatchItemID, ValidBatchItemID},
		{NewArtifactID, ValidArtifactID},
		{NewPendingRequestID, ValidPendingRequestID},
		{NewModelConnectionID, ValidModelConnectionID},
		{NewMCPConnectorID, ValidMCPConnectorID},
		{NewWorkspaceSyncDeviceBindingID, ValidWorkspaceSyncDeviceBindingID},
		{NewWorkspaceSyncDeviceChallengeID, ValidWorkspaceSyncDeviceChallengeID},
		{NewWorkspaceSyncDeviceIdentityID, ValidWorkspaceSyncDeviceIdentityID},
	} {
		id, err := tc.newID()
		if err != nil || !tc.valid(id) {
			t.Fatalf("generated invalid id %q: %v", id, err)
		}
	}
}

func TestIDValidationAndRetry(t *testing.T) {
	if !ValidTemplateID("std-2c4g") || ValidTemplateID("UPPER") || ValidTemplateID("a_") {
		t.Fatal("template id validation failed")
	}
	for _, hostID := range []string{"h0000000", "h1234567", "hzzzzzzz"} {
		workspaceRoot := WorkspaceRootVolumeName(hostID)
		if workspaceRoot != "workspaces-"+hostID || len(workspaceRoot) != len("workspaces-")+8 {
			t.Errorf("workspace root volume name for %q = %q", hostID, workspaceRoot)
		}
		name := RuntimeVolumeName(hostID)
		if !ValidHostID(hostID) || name != "runtime-"+hostID || len(name) != len("runtime-")+8 {
			t.Errorf("runtime volume name for %q = %q", hostID, name)
		}
		profileName := ProfileVolumeName(hostID)
		if profileName != "profile-"+hostID || len(profileName) != len("profile-")+8 {
			t.Errorf("profile volume name for %q = %q", hostID, profileName)
		}
		appsName := HostAppsVolumeName(hostID)
		if appsName != "apps-"+hostID || len(appsName) != len("apps-")+8 {
			t.Errorf("Host apps volume name for %q = %q", hostID, appsName)
		}
	}
	if RuntimeVolumeName("h1234567") == RuntimeVolumeName("h1234568") {
		t.Fatal("different hosts share a runtime volume name")
	}
	if ProfileVolumeName("h1234567") == ProfileVolumeName("h1234568") {
		t.Fatal("different hosts share a profile volume name")
	}
	if HostAppsVolumeName("h1234567") == HostAppsVolumeName("h1234568") {
		t.Fatal("different hosts share an apps volume name")
	}
	for _, port := range []string{"1", "8080", "65535"} {
		if !ValidPreviewPort(port) {
			t.Errorf("expected valid port %q", port)
		}
	}
	for _, port := range []string{"", "0", "01", "65536", "1a"} {
		if ValidPreviewPort(port) {
			t.Errorf("expected invalid port %q", port)
		}
	}
	for _, port := range []int{8080, 5900, 6080, 8090, 8091, 8100, 9222, 0, 65536} {
		if ValidPreviewTargetPort(port) {
			t.Errorf("expected reserved or out-of-range preview target %d to be rejected", port)
		}
	}
	for _, port := range []int{1024, 3000, 5173, 65535} {
		if !ValidPreviewTargetPort(port) {
			t.Errorf("expected user preview target %d to be allowed", port)
		}
	}

	calls := 0
	value, err := CreateWithIDRetry(func() (string, error) { calls++; return fmt.Sprint(calls), nil }, func(id string) (int, error) {
		if id != "3" {
			return 0, errors.New("collision")
		}
		return 7, nil
	}, func(err error) bool { return err.Error() == "collision" })
	if err != nil || value != 7 || calls != 3 {
		t.Fatalf("retry result = %d, %v, calls=%d", value, err, calls)
	}

	_, err = CreateWithIDRetry(func() (string, error) { return "id", nil }, func(string) (int, error) {
		return 0, errors.New("collision")
	}, func(err error) bool { return err.Error() == "collision" })
	if !errors.Is(err, ErrIDAttemptsExhausted) {
		t.Fatalf("expected exhausted retries, got %v", err)
	}
}
