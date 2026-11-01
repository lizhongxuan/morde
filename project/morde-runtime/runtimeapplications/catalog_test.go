package runtimeapplications

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestCatalogExposesOnlyVerifiedApplication(t *testing.T) {
	if !SupportsApplication(WeChatApplicationID) {
		t.Fatal("verified WeChat manifest is missing from the catalog")
	}
	for _, id := range []string{"qq", "", "codex", "wechat ", "WECHAT"} {
		if SupportsApplication(id) {
			t.Fatalf("unsupported application %q was exposed", id)
		}
	}
	storeDir := filepath.Join(t.TempDir(), "apps")
	if err := os.Mkdir(storeDir, 0755); err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenCatalog(CatalogOptions{StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if items := catalog.Statuses(); len(items) != 1 || items[0].ID != WeChatApplicationID {
		t.Fatalf("catalog statuses = %#v", items)
	}
	if _, err := catalog.Install("qq", "req-qq-01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified install error = %v", err)
	}
	if _, err := catalog.Launch("qq", contract.HostFence{}, "launch_qq_01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified launch error = %v", err)
	}
}

func TestCatalogPreventsCrossApplicationLaunchIDReuse(t *testing.T) {
	launchID := "launch_shared_01"
	now := time.Now().UTC()
	other := &Installer{launchJournal: applicationLaunchJournal{Operations: []storedApplicationLaunch{{
		LaunchID:  launchID,
		Operation: contract.ApplicationLaunchOperation{ID: launchID, ApplicationID: "other-app", Status: contract.ApplicationLaunchStopped, CreatedAt: now, UpdatedAt: now},
	}}}}
	catalog := &Catalog{installers: map[string]*Installer{
		WeChatApplicationID: {},
		"other-app":         other,
	}}
	if _, err := catalog.Launch(WeChatApplicationID, contract.HostFence{}, launchID); !errors.Is(err, ErrLaunchInProgress) {
		t.Fatalf("cross-application launch ID reuse error = %v", err)
	}
}

func TestCatalogBlocksLaunchWhileAnotherApplicationMayBeRunning(t *testing.T) {
	for _, status := range []contract.ApplicationLaunchStatus{
		contract.ApplicationLaunchStarting,
		contract.ApplicationLaunchRunning,
		contract.ApplicationLaunchStopping,
		contract.ApplicationLaunchUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			now := time.Now().UTC()
			other := &Installer{launchJournal: applicationLaunchJournal{Operations: []storedApplicationLaunch{{
				LaunchID: "launch_other_01",
				Operation: contract.ApplicationLaunchOperation{
					ID: "launch_other_01", ApplicationID: "other-app", Status: status, CreatedAt: now, UpdatedAt: now,
				},
			}}}}
			catalog := &Catalog{installers: map[string]*Installer{
				WeChatApplicationID: catalogLaunchInstaller(t),
				"other-app":         other,
			}}
			if _, err := catalog.Launch(WeChatApplicationID, launchTestFence(), "launch_wechat_01"); !errors.Is(err, ErrLaunchInProgress) {
				t.Fatalf("launch with another application's %s journal error = %v", status, err)
			}
		})
	}
}

func TestCatalogAllowsLaunchAfterOtherApplicationStopped(t *testing.T) {
	now := time.Now().UTC()
	other := &Installer{launchJournal: applicationLaunchJournal{Operations: []storedApplicationLaunch{{
		LaunchID: "launch_other_01",
		Operation: contract.ApplicationLaunchOperation{
			ID: "launch_other_01", ApplicationID: "other-app", Status: contract.ApplicationLaunchStopped, CreatedAt: now, UpdatedAt: now,
		},
	}}}}
	installer := catalogLaunchInstaller(t)
	catalog := &Catalog{installers: map[string]*Installer{
		WeChatApplicationID: installer,
		"other-app":         other,
	}}

	operation, err := catalog.Launch(WeChatApplicationID, launchTestFence(), "launch_wechat_01")
	if err != nil || operation.Status != contract.ApplicationLaunchFailed || operation.ErrorCode != "architecture_unsupported" {
		t.Fatalf("launch after stopped application = %#v, err=%v", operation, err)
	}
}

func TestCatalogFailsClosedWhenOtherApplicationJournalIsUnavailable(t *testing.T) {
	catalog := &Catalog{installers: map[string]*Installer{
		WeChatApplicationID: catalogLaunchInstaller(t),
		"other-app":         {storageFailed: true},
	}}
	if _, err := catalog.Launch(WeChatApplicationID, launchTestFence(), "launch_wechat_01"); !errors.Is(err, errApplicationStoreUnavailable) {
		t.Fatalf("launch with an unavailable other-application journal error = %v", err)
	}
}

func catalogLaunchInstaller(t *testing.T) *Installer {
	t.Helper()
	dir := t.TempDir()
	return &Installer{
		dir: dir, launchPath: filepath.Join(dir, "launches.json"), manifest: wechatManifest, now: time.Now,
		launchJournal:               applicationLaunchJournal{Version: 1, Operations: []storedApplicationLaunch{}},
		launchArchitectureSupported: func() bool { return false },
	}
}
