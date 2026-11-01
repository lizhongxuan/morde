package runtimeapplications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"morde-runtime/contract"
)

func (i *Installer) Launch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	if applicationID != i.manifest.id {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	request := contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: launchID}
	if !contract.ValidApplicationLaunchRequest(request) {
		return contract.ApplicationLaunchOperation{}, ErrInvalidLaunchRequest
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.storageFailed {
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	if i.launchContext == nil {
		i.launchContext, i.launchCancel = context.WithCancel(context.Background())
	}
	if entry := i.findLaunchLocked(launchID); entry != nil {
		if entry.HostFence.HostID != fence.HostID || entry.HostFence.RuntimeGeneration != fence.RuntimeGeneration {
			return contract.ApplicationLaunchOperation{}, ErrLaunchFenceMismatch
		}
		return entry.Operation, nil
	}
	for _, entry := range i.launchJournal.Operations {
		if launchPending(entry.Operation.Status) {
			return contract.ApplicationLaunchOperation{}, ErrLaunchInProgress
		}
	}
	if len(i.launchJournal.Operations) >= i.manifest.maxLaunches {
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	now := i.now().UTC()
	entry := storedApplicationLaunch{LaunchID: launchID, HostFence: fence, Operation: contract.ApplicationLaunchOperation{
		ID: launchID, ApplicationID: i.manifest.id, Status: contract.ApplicationLaunchStarting, CreatedAt: now, UpdatedAt: now,
	}}
	i.launchJournal.Operations = append(i.launchJournal.Operations, entry)
	if err := i.persistLaunchesLocked(); err != nil {
		i.launchJournal.Operations = i.launchJournal.Operations[:len(i.launchJournal.Operations)-1]
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	if !i.launchArchitectureSupported() {
		if err := i.updateLaunchLocked(launchID, contract.ApplicationLaunchFailed, "architecture_unsupported"); err != nil {
			return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
		}
		return i.findLaunchLocked(launchID).Operation, nil
	}
	if i.launchDomain == nil || i.launchDomain.Prepare(launchID) != nil {
		if err := i.updateLaunchLocked(launchID, contract.ApplicationLaunchFailed, "cgroup_unavailable"); err != nil {
			return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
		}
		return i.findLaunchLocked(launchID).Operation, nil
	}
	i.launchWorkers.Add(1)
	ctx := i.launchContext
	if ctx == nil {
		ctx = context.Background()
	}
	go i.runLaunch(ctx, launchID)
	return entry.Operation, nil
}

func (i *Installer) QueryLaunch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	if applicationID != i.manifest.id {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	if !validApplicationLaunchID(launchID) {
		return contract.ApplicationLaunchOperation{}, ErrInvalidLaunchRequest
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.storageFailed {
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	entry := i.findLaunchLocked(launchID)
	if entry == nil {
		return contract.ApplicationLaunchOperation{}, ErrLaunchNotFound
	}
	if entry.HostFence.HostID != fence.HostID || entry.HostFence.RuntimeGeneration != fence.RuntimeGeneration {
		return contract.ApplicationLaunchOperation{}, ErrLaunchFenceMismatch
	}
	return entry.Operation, nil
}

func (i *Installer) StopLaunch(applicationID string, fence contract.HostFence, launchID string) (contract.ApplicationLaunchOperation, error) {
	if applicationID != i.manifest.id {
		return contract.ApplicationLaunchOperation{}, ErrUnsupportedApplication
	}
	if !validApplicationLaunchID(launchID) {
		return contract.ApplicationLaunchOperation{}, ErrInvalidLaunchRequest
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.storageFailed {
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	entry := i.findLaunchLocked(launchID)
	if entry == nil {
		return contract.ApplicationLaunchOperation{}, ErrLaunchNotFound
	}
	if entry.HostFence.HostID != fence.HostID || entry.HostFence.RuntimeGeneration != fence.RuntimeGeneration {
		return contract.ApplicationLaunchOperation{}, ErrLaunchFenceMismatch
	}
	if !launchPending(entry.Operation.Status) {
		return entry.Operation, nil
	}
	if entry.Operation.Status == contract.ApplicationLaunchStopping {
		return entry.Operation, nil
	}
	if err := i.updateLaunchLocked(launchID, contract.ApplicationLaunchStopping, ""); err != nil {
		return contract.ApplicationLaunchOperation{}, errApplicationStoreUnavailable
	}
	i.launchWorkers.Add(1)
	go i.runStop(launchID)
	return i.findLaunchLocked(launchID).Operation, nil
}

// ReconcileLaunches runs before HTTP serving. It only examines cgroups derived
// from durable launch IDs and leaves every uncertain outcome as unknown.
func (i *Installer) ReconcileLaunches(current contract.HostFence) error {
	if !validLaunchFence(current) {
		return ErrInvalidLaunchRequest
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.storageFailed || i.closed {
		return errApplicationStoreUnavailable
	}
	for index := range i.launchJournal.Operations {
		entry := &i.launchJournal.Operations[index]
		if !launchPending(entry.Operation.Status) {
			continue
		}
		if entry.HostFence.HostID != current.HostID || entry.HostFence.RuntimeGeneration != current.RuntimeGeneration {
			if entry.HostFence.HostID == current.HostID && entry.HostFence.RuntimeGeneration < current.RuntimeGeneration {
				// A new Host generation runs in a replacement Sandbox; its old processes cannot survive.
				if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchExited, ""); err != nil {
					return errApplicationStoreUnavailable
				}
				continue
			}
			errorCode := ""
			if entry.Operation.Status == contract.ApplicationLaunchStopping || entry.Operation.ErrorCode == "stop_unconfirmed" {
				errorCode = "stop_unconfirmed"
			}
			if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, errorCode); err != nil {
				return errApplicationStoreUnavailable
			}
			continue
		}
		if i.launchDomain == nil {
			errorCode := ""
			if entry.Operation.Status == contract.ApplicationLaunchStopping || entry.Operation.ErrorCode == "stop_unconfirmed" {
				errorCode = "stop_unconfirmed"
			}
			if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, errorCode); err != nil {
				return errApplicationStoreUnavailable
			}
			continue
		}
		populated, err := i.launchDomain.Populated(entry.LaunchID)
		if err != nil {
			errorCode := ""
			if entry.Operation.Status == contract.ApplicationLaunchStopping || entry.Operation.ErrorCode == "stop_unconfirmed" {
				errorCode = "stop_unconfirmed"
			}
			if updateErr := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, errorCode); updateErr != nil {
				return errApplicationStoreUnavailable
			}
			continue
		}
		switch entry.Operation.Status {
		case contract.ApplicationLaunchStarting:
			if populated {
				if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchRunning, ""); err != nil {
					return errApplicationStoreUnavailable
				}
			} else if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, ""); err != nil {
				return errApplicationStoreUnavailable
			}
		case contract.ApplicationLaunchRunning:
			status := contract.ApplicationLaunchExited
			if populated {
				status = contract.ApplicationLaunchRunning
			}
			if err := i.updateLaunchLocked(entry.LaunchID, status, ""); err != nil {
				return errApplicationStoreUnavailable
			}
			if status == contract.ApplicationLaunchExited {
				_ = i.launchDomain.RemoveEmpty(entry.LaunchID)
			}
		case contract.ApplicationLaunchStopping:
			if populated {
				ctx, cancel := context.WithTimeout(context.Background(), i.manifest.stopTimeout)
				err = i.launchDomain.Stop(ctx, entry.LaunchID)
				cancel()
				if err == nil {
					populated, err = i.launchDomain.Populated(entry.LaunchID)
				}
			}
			if err != nil || populated {
				if updateErr := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, "stop_unconfirmed"); updateErr != nil {
					return errApplicationStoreUnavailable
				}
			} else {
				if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchStopped, ""); err != nil {
					return errApplicationStoreUnavailable
				}
				_ = i.launchDomain.RemoveEmpty(entry.LaunchID)
			}
		case contract.ApplicationLaunchUnknown:
			if entry.Operation.ErrorCode == "stop_unconfirmed" {
				if populated {
					ctx, cancel := context.WithTimeout(context.Background(), i.manifest.stopTimeout)
					err = i.launchDomain.Stop(ctx, entry.LaunchID)
					cancel()
					if err == nil {
						populated, err = i.launchDomain.Populated(entry.LaunchID)
					}
				}
				if err != nil || populated {
					if updateErr := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchUnknown, "stop_unconfirmed"); updateErr != nil {
						return errApplicationStoreUnavailable
					}
				} else {
					if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchStopped, ""); err != nil {
						return errApplicationStoreUnavailable
					}
					_ = i.launchDomain.RemoveEmpty(entry.LaunchID)
				}
			} else if populated {
				if err := i.updateLaunchLocked(entry.LaunchID, contract.ApplicationLaunchRunning, ""); err != nil {
					return errApplicationStoreUnavailable
				}
			}
		}
	}
	return nil
}

func (i *Installer) runLaunch(ctx context.Context, launchID string) {
	defer i.launchWorkers.Done()
	fail := func(code string) { i.failLaunchIfStarting(launchID, code) }
	architectureSupported := i.launchArchitectureSupported
	if architectureSupported == nil {
		architectureSupported = i.manifest.launchArchitectureSupported
	}
	if !architectureSupported() {
		fail("architecture_unsupported")
		return
	}
	installed := i.installedCheck
	if installed == nil {
		installed = func() bool { return i.installedVersion() == i.manifest.pkg.version }
	}
	if !installed() {
		fail("not_installed")
		return
	}
	if i.launchDomain == nil {
		fail("cgroup_unavailable")
		return
	}
	if i.prepareLaunch == nil {
		fail("launch_failed")
		return
	}
	prepareContext, cancel := context.WithTimeout(ctx, i.manifest.launchTimeout)
	command, uid, gid, errorCode := i.prepareLaunch(prepareContext)
	cancel()
	if errorCode != "" || command == nil {
		if errorCode == "" {
			errorCode = "launch_failed"
		}
		fail(errorCode)
		return
	}
	if ctx.Err() != nil {
		fail("launch_failed")
		return
	}
	i.mu.Lock()
	entry := i.findLaunchLocked(launchID)
	if entry == nil || entry.Operation.Status != contract.ApplicationLaunchStarting || i.closed {
		i.mu.Unlock()
		return
	}
	if err := i.launchDomain.Start(launchID, command, uid, gid); err != nil {
		populated, stateErr := i.launchDomain.Populated(launchID)
		if stateErr != nil || populated {
			_ = i.updateLaunchLocked(launchID, contract.ApplicationLaunchUnknown, "")
			i.mu.Unlock()
			return
		}
		updateErr := i.updateLaunchLocked(launchID, contract.ApplicationLaunchFailed, "cgroup_unavailable")
		i.mu.Unlock()
		if updateErr == nil {
			_ = i.launchDomain.RemoveEmpty(launchID)
		}
		return
	}
	populated, stateErr := i.launchDomain.Populated(launchID)
	status := contract.ApplicationLaunchRunning
	if stateErr != nil {
		status = contract.ApplicationLaunchUnknown
	} else if !populated {
		status = contract.ApplicationLaunchUnknown
	}
	_ = i.updateLaunchLocked(launchID, status, "")
	i.mu.Unlock()
	i.launchWatchers.Add(1)
	go i.watchLaunch(ctx, launchID, command)
}

func (i *Installer) watchLaunch(ctx context.Context, launchID string, command *exec.Cmd) {
	defer i.launchWatchers.Done()
	waitDone := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(waitDone)
	}()
	select {
	case <-ctx.Done():
		return
	case <-waitDone:
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		populated, err := i.launchDomain.Populated(launchID)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			i.finishLaunchUnlessStopping(launchID, contract.ApplicationLaunchUnknown, "")
			return
		}
		if !populated {
			if i.finishLaunchUnlessStopping(launchID, contract.ApplicationLaunchExited, "") {
				_ = i.launchDomain.RemoveEmpty(launchID)
			}
			return
		}
		i.finishLaunchUnlessStopping(launchID, contract.ApplicationLaunchRunning, "")
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (i *Installer) runStop(launchID string) {
	defer i.launchWorkers.Done()
	if i.launchDomain == nil {
		_ = i.finishLaunch(launchID, contract.ApplicationLaunchUnknown, "stop_unconfirmed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), i.manifest.stopTimeout)
	err := i.launchDomain.Stop(ctx, launchID)
	cancel()
	populated, stateErr := i.launchDomain.Populated(launchID)
	if err != nil || stateErr != nil || populated {
		_ = i.finishLaunch(launchID, contract.ApplicationLaunchUnknown, "stop_unconfirmed")
		return
	}
	if i.finishLaunch(launchID, contract.ApplicationLaunchStopped, "") == nil {
		_ = i.launchDomain.RemoveEmpty(launchID)
	}
}

func (i *Installer) finishLaunch(launchID string, status contract.ApplicationLaunchStatus, code string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.updateLaunchLocked(launchID, status, code)
}

func (i *Installer) failLaunchIfStarting(launchID, code string) {
	i.mu.Lock()
	entry := i.findLaunchLocked(launchID)
	if entry == nil || entry.Operation.Status != contract.ApplicationLaunchStarting {
		i.mu.Unlock()
		return
	}
	err := i.updateLaunchLocked(launchID, contract.ApplicationLaunchFailed, code)
	i.mu.Unlock()
	if err == nil && i.launchDomain != nil {
		_ = i.launchDomain.RemoveEmpty(launchID)
	}
}

func (i *Installer) finishLaunchUnlessStopping(launchID string, status contract.ApplicationLaunchStatus, code string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	entry := i.findLaunchLocked(launchID)
	if i.closed || entry == nil || entry.Operation.Status == contract.ApplicationLaunchStopping ||
		entry.Operation.Status == contract.ApplicationLaunchStopped || entry.Operation.Status == contract.ApplicationLaunchFailed {
		return false
	}
	return i.updateLaunchLocked(launchID, status, code) == nil
}

func (i *Installer) updateLaunchLocked(launchID string, status contract.ApplicationLaunchStatus, code string) error {
	if i.storageFailed {
		return errStore
	}
	entry := i.findLaunchLocked(launchID)
	if entry == nil {
		return errStore
	}
	previous := entry.Operation
	entry.Operation.Status = status
	entry.Operation.ErrorCode = code
	entry.Operation.UpdatedAt = i.now().UTC()
	if !contract.ValidApplicationLaunchOperation(entry.Operation) {
		entry.Operation = previous
		return errStore
	}
	if err := i.persistLaunchesLocked(); err != nil {
		entry.Operation = previous
		return err
	}
	return nil
}

func (i *Installer) findLaunchLocked(launchID string) *storedApplicationLaunch {
	for index := range i.launchJournal.Operations {
		if i.launchJournal.Operations[index].LaunchID == launchID {
			return &i.launchJournal.Operations[index]
		}
	}
	return nil
}

func (i *Installer) persistLaunchesLocked() error {
	if i.storageFailed {
		return errStore
	}
	data, err := json.Marshal(i.launchJournal)
	if err != nil || len(data) > 4<<20 {
		i.storageFailed = true
		return errStore
	}
	temp, err := os.CreateTemp(i.dir, ".launches-state-*")
	if err != nil {
		i.storageFailed = true
		return errStore
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		i.storageFailed = true
		return errStore
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		i.storageFailed = true
		return errStore
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		i.storageFailed = true
		return errStore
	}
	if err := temp.Close(); err != nil {
		i.storageFailed = true
		return errStore
	}
	if err := os.Rename(name, i.launchPath); err != nil || syncDirectory(i.dir) != nil {
		i.storageFailed = true
		return errStore
	}
	return nil
}

func (i *Installer) loadLaunchJournal() error {
	info, err := os.Lstat(i.launchPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 ||
		fileUID(info) != i.ownerUID || info.Size() > 4<<20 {
		return errors.New("application launch journal is unsafe")
	}
	data, err := os.ReadFile(i.launchPath)
	if err != nil {
		return errors.New("application launch journal is unavailable")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var journal applicationLaunchJournal
	if err := decoder.Decode(&journal); err != nil || journal.Version != 1 || len(journal.Operations) > i.manifest.maxLaunches {
		return errors.New("application launch journal is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("application launch journal is invalid")
	}
	seenLaunchIDs := make(map[string]struct{}, len(journal.Operations))
	seenOperationIDs := make(map[string]struct{}, len(journal.Operations))
	for _, entry := range journal.Operations {
		if !validApplicationLaunchID(entry.LaunchID) || entry.Operation.ID != entry.LaunchID || !validLaunchFence(entry.HostFence) || !contract.ValidApplicationLaunchOperation(entry.Operation) || entry.Operation.ApplicationID != i.manifest.id {
			return errors.New("application launch journal is invalid")
		}
		if _, exists := seenLaunchIDs[entry.LaunchID]; exists {
			return errors.New("application launch journal is invalid")
		}
		if _, exists := seenOperationIDs[entry.Operation.ID]; exists {
			return errors.New("application launch journal is invalid")
		}
		seenLaunchIDs[entry.LaunchID] = struct{}{}
		seenOperationIDs[entry.Operation.ID] = struct{}{}
	}
	i.launchJournal = journal
	return nil
}

func validLaunchFence(fence contract.HostFence) bool {
	return contract.ValidApplicationLaunchRequest(contract.ApplicationLaunchRequest{
		ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: "launch_validation",
	})
}

func validApplicationLaunchID(value string) bool {
	if len(value) < len("launch_")+1 || len(value) > 128 || !strings.HasPrefix(value, "launch_") {
		return false
	}
	if !isAlphaNumeric(value[len("launch_")]) {
		return false
	}
	for index := len("launch_"); index < len(value); index++ {
		character := value[index]
		if !isAlphaNumeric(character) && !strings.ContainsRune("._:-", rune(character)) {
			return false
		}
	}
	return true
}

func launchPending(status contract.ApplicationLaunchStatus) bool {
	switch status {
	case contract.ApplicationLaunchStarting, contract.ApplicationLaunchRunning, contract.ApplicationLaunchStopping, contract.ApplicationLaunchUnknown:
		return true
	default:
		return false
	}
}

var (
	ErrInvalidLaunchRequest = errors.New("invalid application launch request")
	ErrLaunchInProgress     = errors.New("another application launch is active or uncertain")
	ErrLaunchNotFound       = errors.New("application launch not found")
	ErrLaunchFenceMismatch  = errors.New("application launch belongs to another Host generation")
)
