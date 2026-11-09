package runtimeapplications

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"morde-runtime/contract"
)

func TestLaunchIsDurableIdempotentAndStoppedOnlyAfterEmptyReceipt(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	fence := launchTestFence()

	first, err := installer.Launch(wechatID, fence, "launch_wechat_01")
	if err != nil || first.ID != "launch_wechat_01" || first.Status != contract.ApplicationLaunchStarting {
		t.Fatalf("launch = %#v, err=%v", first, err)
	}
	if got := waitLaunch(t, installer, first.ID, contract.ApplicationLaunchRunning); got.ID != first.ID {
		t.Fatalf("running operation = %#v", got)
	}
	duplicate, err := installer.Launch(wechatID, fence, first.ID)
	if err != nil || duplicate.ID != first.ID {
		t.Fatalf("idempotent launch = %#v, err=%v", duplicate, err)
	}
	if domain.startCount() != 1 {
		t.Fatalf("duplicate launch dispatched %d starts", domain.startCount())
	}
	if _, err := installer.Launch(wechatID, fence, "launch_wechat_02"); !errors.Is(err, ErrLaunchInProgress) {
		t.Fatalf("overlapping launch error = %v", err)
	}

	stopping, err := installer.StopLaunch(wechatID, fence, first.ID)
	if err != nil || stopping.Status != contract.ApplicationLaunchStopping {
		t.Fatalf("stop = %#v, err=%v", stopping, err)
	}
	stopped := waitLaunch(t, installer, first.ID, contract.ApplicationLaunchStopped)
	if stopped.ErrorCode != "" {
		t.Fatalf("stopped operation has error: %#v", stopped)
	}
	populated, err := domain.Populated(first.ID)
	if err != nil || populated {
		t.Fatalf("stopped without cgroup empty receipt: populated=%v err=%v", populated, err)
	}
	if domain.stopCount() != 1 {
		t.Fatalf("stop calls = %d, want one", domain.stopCount())
	}
	if domain.removeCount() != 1 {
		t.Fatalf("empty cgroup cleanup calls = %d, want one", domain.removeCount())
	}
}

func TestCloseCancelsAndWaitsForLaunchWatcherWithoutUpdatingJournal(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	domain.blockAtCall = 2 // launch start check completes; block the watcher's first poll.
	domain.blockEntered = make(chan struct{})
	domain.blockRelease = make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(domain.blockRelease)
		}
	}()
	configureLaunchTest(installer, domain)
	if err := installer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	operation, err := installer.Launch(wechatID, launchTestFence(), "launch_close_01")
	if err != nil || operation.Status != contract.ApplicationLaunchStarting {
		t.Fatalf("launch = %#v, err=%v", operation, err)
	}
	select {
	case <-domain.blockEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch watcher did not enter its poll")
	}
	if got := waitLaunch(t, installer, operation.ID, contract.ApplicationLaunchRunning); got.Status != contract.ApplicationLaunchRunning {
		t.Fatalf("launch before close = %#v", got)
	}
	before, err := os.ReadFile(installer.launchPath)
	if err != nil {
		t.Fatal(err)
	}

	closeDone := make(chan struct{})
	go func() {
		_ = installer.Close()
		close(closeDone)
	}()
	select {
	case <-installer.launchContext.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the launch context")
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while the launch watcher was still polling")
	default:
	}
	secondCloseDone := make(chan struct{})
	go func() {
		_ = installer.Close()
		close(secondCloseDone)
	}()
	select {
	case <-secondCloseDone:
		t.Fatal("concurrent Close returned before the launch watcher exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(domain.blockRelease)
	released = true
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for the cancelled launch watcher to exit")
	}
	select {
	case <-secondCloseDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent Close did not wait for the cancelled launch watcher to exit")
	}

	after, err := os.ReadFile(installer.launchPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("launch journal changed after Close began:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestRepeatedStartKeepsLaunchWatcherActive(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	domain.blockAtCall = 2 // launch start check completes; block the watcher's first poll.
	domain.blockEntered = make(chan struct{})
	domain.blockRelease = make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(domain.blockRelease)
		}
	}()
	configureLaunchTest(installer, domain)
	if err := installer.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	watchContextDone := installer.launchContext.Done()

	operation, err := installer.Launch(wechatID, launchTestFence(), "launch_repeat_start_01")
	if err != nil || operation.Status != contract.ApplicationLaunchStarting {
		t.Fatalf("launch = %#v, err=%v", operation, err)
	}
	select {
	case <-domain.blockEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch watcher did not enter its poll")
	}
	if got := waitLaunch(t, installer, operation.ID, contract.ApplicationLaunchRunning); got.Status != contract.ApplicationLaunchRunning {
		t.Fatalf("launch before repeated Start = %#v", got)
	}

	if err := installer.Start(context.Background()); err != nil {
		t.Fatalf("repeated Start error = %v", err)
	}
	select {
	case <-watchContextDone:
		t.Fatal("repeated Start cancelled the active launch watcher")
	default:
	}
	close(domain.blockRelease)
	released = true
	domain.setPopulated(operation.ID, false)
	if got := waitLaunch(t, installer, operation.ID, contract.ApplicationLaunchExited); got.Status != contract.ApplicationLaunchExited {
		t.Fatalf("launch after repeated Start = %#v", got)
	}
}

func TestLauncherRejectsApplicationOutsideItsManifest(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	fence := launchTestFence()
	if _, err := installer.Launch("qq", fence, "launch_qq_01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified app launch error = %v", err)
	}
	if _, err := installer.QueryLaunch("qq", fence, "launch_qq_01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified app query error = %v", err)
	}
	if _, err := installer.StopLaunch("qq", fence, "launch_qq_01"); !errors.Is(err, ErrUnsupportedApplication) {
		t.Fatalf("unverified app stop error = %v", err)
	}
	if domain.startCount() != 0 {
		t.Fatalf("unverified app started a process: %d", domain.startCount())
	}
}

func TestStopDuringLauncherPreflightPreventsDispatch(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	entered, release := make(chan struct{}), make(chan struct{})
	installer.prepareLaunch = func(context.Context) (*exec.Cmd, uint32, uint32, string) {
		close(entered)
		<-release
		return exec.Command("/fixed/test-launcher"), 1000, 1000, ""
	}
	fence := launchTestFence()
	if _, err := installer.Launch(wechatID, fence, "launch_race_01"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("launcher preflight did not start")
	}
	if _, err := installer.StopLaunch(wechatID, fence, "launch_race_01"); err != nil {
		t.Fatal(err)
	}
	if got := waitLaunch(t, installer, "launch_race_01", contract.ApplicationLaunchStopped); got.Status != contract.ApplicationLaunchStopped {
		t.Fatalf("stop-before-start = %#v", got)
	}
	close(release)
	time.Sleep(20 * time.Millisecond)
	if domain.startCount() != 0 {
		t.Fatalf("launch dispatched after stop was accepted: starts=%d", domain.startCount())
	}
}

func TestLaunchRestartReconciliationNeverRedispatchesUnknownStart(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	fence := launchTestFence()
	now := time.Now().UTC()
	installer.mu.Lock()
	installer.launchJournal.Operations = append(installer.launchJournal.Operations, storedApplicationLaunch{
		LaunchID: "launch_uncertain_01", HostFence: fence,
		Operation: contract.ApplicationLaunchOperation{ID: "launch_uncertain_01", ApplicationID: wechatID,
			Status: contract.ApplicationLaunchStarting, CreatedAt: now, UpdatedAt: now},
	})
	if err := installer.persistLaunchesLocked(); err != nil {
		installer.mu.Unlock()
		t.Fatal(err)
	}
	storeDir := filepath.Dir(installer.dir)
	installer.mu.Unlock()
	if err := installer.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid()), launchDomain: domain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	configureLaunchTest(restarted, domain)
	if err := restarted.ReconcileLaunches(fence); err != nil {
		t.Fatal(err)
	}
	operation, err := restarted.QueryLaunch(wechatID, fence, "launch_uncertain_01")
	if err != nil || operation.Status != contract.ApplicationLaunchUnknown {
		t.Fatalf("reconciled uncertain launch = %#v, err=%v", operation, err)
	}
	if _, err := restarted.Launch(wechatID, fence, operation.ID); err != nil {
		t.Fatalf("idempotent uncertain launch query returned err=%v", err)
	}
	if domain.startCount() != 0 {
		t.Fatalf("restart redispatched uncertain launch %d times", domain.startCount())
	}
	// Simulate a registered, already-empty cgroup. A missing cgroup after a
	// restart remains unconfirmed and must not be reported as stopped.
	if err := domain.Prepare(operation.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.StopLaunch(wechatID, fence, operation.ID); err != nil || got.Status != contract.ApplicationLaunchStopping {
		t.Fatalf("explicit stop to resolve unknown = %#v, err=%v", got, err)
	}
	if got := waitLaunch(t, restarted, operation.ID, contract.ApplicationLaunchStopped); got.ErrorCode != "" {
		t.Fatalf("empty cgroup stop did not confirm stopped: %#v", got)
	}
}

func TestNewRuntimeGenerationReconcilesOldLaunchAndAllowsRelaunch(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	t.Cleanup(func() { _ = installer.Close() })

	oldFence := launchTestFence()
	currentFence := oldFence
	currentFence.RuntimeGeneration++
	currentFence.RuntimeInstanceID = "runtime-2"
	previous := contract.ApplicationLaunchOperation{
		ID: "launch_previous_generation", ApplicationID: wechatID,
		Status: contract.ApplicationLaunchRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installer.mu.Lock()
	installer.launchJournal.Operations = append(installer.launchJournal.Operations, storedApplicationLaunch{
		LaunchID: previous.ID, HostFence: oldFence, Operation: previous,
	})
	if err := installer.persistLaunchesLocked(); err != nil {
		installer.mu.Unlock()
		t.Fatal(err)
	}
	installer.mu.Unlock()

	if err := installer.ReconcileLaunches(currentFence); err != nil {
		t.Fatal(err)
	}
	retired, err := installer.QueryLaunch(wechatID, oldFence, previous.ID)
	if err != nil || retired.Status != contract.ApplicationLaunchExited {
		t.Fatalf("old-generation launch = %#v, err=%v; want exited", retired, err)
	}
	if _, err := installer.Launch(wechatID, currentFence, "launch_current_generation"); err != nil {
		t.Fatalf("new generation launch remained blocked by stale launch: %v", err)
	}
}

func TestFutureRuntimeGenerationLaunchRemainsUnknown(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	configureLaunchTest(installer, newLaunchDomainFake())
	t.Cleanup(func() { _ = installer.Close() })

	currentFence := launchTestFence()
	futureFence := currentFence
	futureFence.RuntimeGeneration++
	operation := contract.ApplicationLaunchOperation{
		ID: "launch_future_generation", ApplicationID: wechatID,
		Status: contract.ApplicationLaunchRunning, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	installer.mu.Lock()
	installer.launchJournal.Operations = append(installer.launchJournal.Operations, storedApplicationLaunch{
		LaunchID: operation.ID, HostFence: futureFence, Operation: operation,
	})
	if err := installer.persistLaunchesLocked(); err != nil {
		installer.mu.Unlock()
		t.Fatal(err)
	}
	installer.mu.Unlock()

	if err := installer.ReconcileLaunches(currentFence); err != nil {
		t.Fatal(err)
	}
	uncertain, err := installer.QueryLaunch(wechatID, futureFence, operation.ID)
	if err != nil || uncertain.Status != contract.ApplicationLaunchUnknown {
		t.Fatalf("future-generation launch = %#v, err=%v; want unknown", uncertain, err)
	}
	if _, err := installer.Launch(wechatID, currentFence, "launch_current_generation"); !errors.Is(err, ErrLaunchInProgress) {
		t.Fatalf("launch was not blocked by future-generation uncertainty: %v", err)
	}
}

func TestStopUnconfirmedStaysUnknownUntilReconciliationProvesEmpty(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	domain := newLaunchDomainFake()
	configureLaunchTest(installer, domain)
	domain.setPopulated("launch_stuck_01", true)
	domain.stopErr = errors.New("cannot verify stop")
	now := time.Now().UTC()
	installer.mu.Lock()
	installer.launchJournal.Operations = append(installer.launchJournal.Operations, storedApplicationLaunch{
		LaunchID: "launch_stuck_01", HostFence: launchTestFence(),
		Operation: contract.ApplicationLaunchOperation{ID: "launch_stuck_01", ApplicationID: wechatID,
			Status: contract.ApplicationLaunchStopping, CreatedAt: now, UpdatedAt: now},
	})
	if err := installer.persistLaunchesLocked(); err != nil {
		installer.mu.Unlock()
		t.Fatal(err)
	}
	installer.mu.Unlock()
	if err := installer.ReconcileLaunches(launchTestFence()); err != nil {
		t.Fatal(err)
	}
	operation, err := installer.QueryLaunch(wechatID, launchTestFence(), "launch_stuck_01")
	if err != nil || operation.Status != contract.ApplicationLaunchUnknown || operation.ErrorCode != "stop_unconfirmed" {
		t.Fatalf("unconfirmed stop = %#v, err=%v", operation, err)
	}
	populated, err := domain.Populated("launch_stuck_01")
	if err != nil || !populated {
		t.Fatalf("test domain unexpectedly empty: populated=%v err=%v", populated, err)
	}
}

func TestLaunchJournalRejectsMismatchedOperationIdentity(t *testing.T) {
	installer, _, _ := newTestInstaller(t, []byte("fake package"))
	installer.mu.Lock()
	installer.launchJournal.Operations = append(installer.launchJournal.Operations, storedApplicationLaunch{
		LaunchID: "launch_record_01", HostFence: launchTestFence(),
		Operation: contract.ApplicationLaunchOperation{ID: "launch_other_01", ApplicationID: wechatID,
			Status: contract.ApplicationLaunchUnknown, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	})
	if err := installer.persistLaunchesLocked(); err != nil {
		installer.mu.Unlock()
		t.Fatal(err)
	}
	storeDir := filepath.Dir(installer.dir)
	installer.mu.Unlock()
	if err := installer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openInstaller(installerOptions{applicationID: wechatID, storeDir: storeDir, expectedOwnerUID: uint32(os.Geteuid())}); err == nil {
		t.Fatal("mismatched launch operation identity was accepted")
	}
}

func configureLaunchTest(installer *Installer, domain *launchDomainFake) {
	installer.launchDomain = domain
	installer.installedCheck = func() bool { return true }
	installer.launchArchitectureSupported = func() bool { return true }
	installer.prepareLaunch = func(context.Context) (*exec.Cmd, uint32, uint32, string) {
		return exec.Command("/fixed/test-launcher"), 1000, 1000, ""
	}
}

func launchTestFence() contract.HostFence {
	return contract.HostFence{HostID: "h1234567", RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 0}
}

func waitLaunch(t *testing.T, installer *Installer, launchID string, status contract.ApplicationLaunchStatus) contract.ApplicationLaunchOperation {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := installer.QueryLaunch(wechatID, launchTestFence(), launchID)
		if err == nil && operation.Status == status {
			return operation
		}
		time.Sleep(5 * time.Millisecond)
	}
	operation, err := installer.QueryLaunch(wechatID, launchTestFence(), launchID)
	t.Fatalf("launch %q did not reach %s: operation=%#v err=%v", launchID, status, operation, err)
	return contract.ApplicationLaunchOperation{}
}

type launchDomainFake struct {
	mu             sync.Mutex
	populated      map[string]bool
	starts         int
	stops          int
	removes        int
	populatedCalls int
	blockAtCall    int
	blockEntered   chan struct{}
	blockRelease   chan struct{}
	stopErr        error
	population     error
}

func newLaunchDomainFake() *launchDomainFake {
	return &launchDomainFake{populated: make(map[string]bool)}
}

func (d *launchDomainFake) Prepare(launchID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.populated[launchID]; exists {
		return errors.New("cgroup already exists")
	}
	d.populated[launchID] = false
	return nil
}

func (d *launchDomainFake) Start(launchID string, _ *exec.Cmd, _, _ uint32) error {
	d.mu.Lock()
	if _, exists := d.populated[launchID]; !exists || d.populated[launchID] {
		d.mu.Unlock()
		return errors.New("cgroup was not prepared or was populated")
	}
	d.starts++
	d.populated[launchID] = true
	d.mu.Unlock()
	return nil
}

func (d *launchDomainFake) Populated(launchID string) (bool, error) {
	d.mu.Lock()
	d.populatedCalls++
	populated, exists := d.populated[launchID]
	block := d.blockAtCall == d.populatedCalls
	if block {
		close(d.blockEntered)
	}
	populationErr := d.population
	d.mu.Unlock()
	if block {
		<-d.blockRelease
	}
	if !exists {
		return false, errors.New("cgroup receipt unavailable")
	}
	return populated, populationErr
}

func (d *launchDomainFake) Stop(_ context.Context, launchID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stops++
	if _, exists := d.populated[launchID]; !exists {
		return errors.New("cgroup receipt unavailable")
	}
	if d.stopErr != nil {
		return d.stopErr
	}
	d.populated[launchID] = false
	return nil
}

func (d *launchDomainFake) RemoveEmpty(launchID string) error {
	populated, err := d.Populated(launchID)
	if err != nil || populated {
		return errors.New("cgroup is populated")
	}
	d.mu.Lock()
	d.removes++
	d.mu.Unlock()
	return nil
}

func (d *launchDomainFake) setPopulated(launchID string, populated bool) {
	d.mu.Lock()
	d.populated[launchID] = populated
	d.mu.Unlock()
}

func (d *launchDomainFake) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.starts
}

func (d *launchDomainFake) stopCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stops
}

func (d *launchDomainFake) removeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removes
}
