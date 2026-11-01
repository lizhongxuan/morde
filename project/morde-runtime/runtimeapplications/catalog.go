package runtimeapplications

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"morde-runtime/contract"
)

type applicationManifest struct {
	id                          string
	pkg                         debPackage
	downloadTempPrefix          string
	installTimeout              time.Duration
	restoreTimeout              time.Duration
	maxHistory                  int
	maxLaunches                 int
	launchTimeout               time.Duration
	stopTimeout                 time.Duration
	prepareLaunch               func(context.Context) (*exec.Cmd, uint32, uint32, string)
	launchArchitectureSupported func() bool
}

type CatalogOptions struct {
	StoreDir         string
	ExpectedOwnerUID uint32
	LaunchDomain     applicationProcessDomain
}

// Catalog owns one isolated installer per compiled application manifest.
type Catalog struct {
	mu         sync.Mutex
	installers map[string]*Installer
}

var applicationCatalog = [...]applicationManifest{wechatManifest}

func OpenCatalog(options CatalogOptions) (*Catalog, error) {
	catalog := &Catalog{installers: make(map[string]*Installer, len(applicationCatalog))}
	for _, manifest := range applicationCatalog {
		installer, err := openInstaller(installerOptions{
			applicationID: manifest.id, storeDir: options.StoreDir,
			expectedOwnerUID: options.ExpectedOwnerUID, launchDomain: options.LaunchDomain,
		})
		if err != nil {
			_ = catalog.Close()
			return nil, err
		}
		catalog.installers[manifest.id] = installer
	}
	return catalog, nil
}

func (c *Catalog) Statuses() []contract.ApplicationStatusItem {
	items := make([]contract.ApplicationStatusItem, 0, len(applicationCatalog))
	for _, manifest := range applicationCatalog {
		if installer := c.installers[manifest.id]; installer != nil {
			items = append(items, installer.Status())
		}
	}
	return items
}

func (c *Catalog) Install(applicationID, requestID string) (contract.ApplicationInstallOperation, error) {
	installer, ok := c.installers[applicationID]
	if !ok {
		return contract.ApplicationInstallOperation{}, ErrUnsupportedApplication
	}
	return installer.Install(applicationID, requestID)
}

func (c *Catalog) Launch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	installer, ok := c.installers[applicationID]
	if !ok {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, other := range c.installers {
		if id == applicationID {
			continue
		}
		other.mu.Lock()
		if other.storageFailed {
			other.mu.Unlock()
			return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
		}
		used := other.findLaunchLocked(launchID) != nil
		active := false
		for _, entry := range other.launchJournal.Operations {
			if launchPending(entry.Operation.Status) {
				active = true
				break
			}
		}
		other.mu.Unlock()
		if used || active {
			return contract.ApplicationLaunchOperation{}, ErrLaunchInProgress
		}
	}
	return installer.Launch(applicationID, fence, launchID)
}

func (c *Catalog) QueryLaunch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	installer, ok := c.installers[applicationID]
	if !ok {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	return installer.QueryLaunch(applicationID, fence, launchID)
}

func (c *Catalog) StopLaunch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	installer, ok := c.installers[applicationID]
	if !ok {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	return installer.StopLaunch(applicationID, fence, launchID)
}

func (c *Catalog) Start(ctx context.Context) error {
	for _, manifest := range applicationCatalog {
		if installer := c.installers[manifest.id]; installer != nil {
			if err := installer.Start(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) Restore(ctx context.Context) error {
	for _, manifest := range applicationCatalog {
		if installer := c.installers[manifest.id]; installer != nil {
			if err := installer.Restore(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) ReconcileLaunches(fence contract.HostFence) error {
	for _, manifest := range applicationCatalog {
		if installer := c.installers[manifest.id]; installer != nil {
			if err := installer.ReconcileLaunches(fence); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) Close() error {
	var firstErr error
	for _, manifest := range applicationCatalog {
		if installer := c.installers[manifest.id]; installer != nil {
			if err := installer.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func ApplicationIDs() []string {
	ids := make([]string, 0, len(applicationCatalog))
	for _, manifest := range applicationCatalog {
		ids = append(ids, manifest.id)
	}
	return ids
}

func SupportsApplication(id string) bool {
	_, ok := manifestFor(id)
	return ok
}

func manifestFor(id string) (applicationManifest, bool) {
	for _, manifest := range applicationCatalog {
		if manifest.id == id {
			return manifest, true
		}
	}
	return applicationManifest{}, false
}

type installerOptions struct {
	applicationID    string
	storeDir         string
	expectedOwnerUID uint32
	launchDomain     applicationProcessDomain
}
