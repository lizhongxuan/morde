package contract

import (
	"strings"
	"testing"
	"time"
)

func TestApplicationWireValidationAcceptsSafeFutureIDs(t *testing.T) {
	now := time.Now().UTC()
	install := ApplicationInstallOperation{
		ID: "app_0123456789abcdef01234567", ApplicationID: "qq", Status: OperationAccepted,
		CreatedAt: now, UpdatedAt: now,
	}
	if !ValidApplicationInstallOperation(install) {
		t.Fatal("valid future application install operation rejected")
	}
	launch := ApplicationLaunchOperation{
		ID: "launch_qq_01", ApplicationID: "qq", Status: ApplicationLaunchRunning,
		CreatedAt: now, UpdatedAt: now,
	}
	if !ValidApplicationLaunchOperation(launch) {
		t.Fatal("valid future application launch operation rejected")
	}
	status := ApplicationStatusItem{ID: "qq", Installed: true, Version: "3.2.34", LoginStatus: ApplicationLoginNotApplicable}
	if !ValidApplicationStatusItem(status) {
		t.Fatal("valid future application status rejected")
	}

	for _, id := range []string{"../qq", "QQ", "qq_1", "qq--app", "qq-", "9qq", ""} {
		if validApplicationID(id) {
			t.Fatalf("unsafe application ID accepted: %q", id)
		}
	}
	install.ApplicationID = "../qq"
	if ValidApplicationInstallOperation(install) {
		t.Fatal("unsafe application ID accepted in install operation")
	}
	launch.ApplicationID = "../qq"
	if ValidApplicationLaunchOperation(launch) {
		t.Fatal("unsafe application ID accepted in launch operation")
	}
}

func TestApplicationStatusInstallRequestIdentityIsPairedAndValidated(t *testing.T) {
	now := time.Now().UTC()
	item := ApplicationStatusItem{
		ID: "wechat", LoginStatus: ApplicationLoginNotApplicable,
		InstallOperation: &ApplicationInstallOperation{
			ID: "app_1234", ApplicationID: "wechat", Status: OperationAccepted,
			CreatedAt: now, UpdatedAt: now,
		},
		InstallRequestID: "install-wechat-01",
	}
	if !ValidApplicationStatusItem(item) {
		t.Fatal("valid install request identity was rejected")
	}
	for _, requestID := range []string{"", "bad key", "../escape", "-bad", strings.Repeat("a", 129)} {
		item.InstallRequestID = requestID
		if ValidApplicationStatusItem(item) {
			t.Fatalf("invalid install request identity was accepted: %q", requestID)
		}
	}
	item.InstallOperation = nil
	item.InstallRequestID = "install-wechat-01"
	if ValidApplicationStatusItem(item) {
		t.Fatal("request ID without an install operation was accepted")
	}
}

func TestWechatStatusRequiresUnknownLoginWhenInstalled(t *testing.T) {
	installed := ApplicationStatusItem{ID: "wechat", Installed: true, Version: "4.1.13.26", LoginStatus: ApplicationLoginUnknown}
	if !ValidApplicationStatusItem(installed) {
		t.Fatal("installed WeChat with unprobed login state was rejected")
	}
	installed.LoginStatus = ApplicationLoginNotApplicable
	if ValidApplicationStatusItem(installed) {
		t.Fatal("installed WeChat with not_applicable login state was accepted")
	}

	uninstalled := ApplicationStatusItem{ID: "wechat", LoginStatus: ApplicationLoginNotApplicable}
	if !ValidApplicationStatusItem(uninstalled) {
		t.Fatal("uninstalled WeChat was rejected")
	}
	uninstalled.LoginStatus = ApplicationLoginUnknown
	if ValidApplicationStatusItem(uninstalled) {
		t.Fatal("uninstalled WeChat with unknown login state was accepted")
	}
}
