package dshguardian

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestStopTaskInOneAccountRuntimeDoesNotBlockSiblingRuntimeRenewal(t *testing.T) {
	now := time.Now().UTC()
	runtimeA := testAccountRuntimeGuard(now)
	runtimeB := testAccountRuntimeGuard(now)
	runtimeB.Fence.ConversationID = "c2345678"
	runtimeB.Fence.RuntimeID = "runtime-2"
	runtimeB.NonceSHA256 = testNonceHash("runtime-b")
	runtimeB.ContainerID = strings.Repeat("d", 64)
	runtimeB.ContainerName = expectedAccountRuntimeContainerName(runtimeB.Fence, runtimeB.NonceSHA256)
	taskA := testTaskProcessGuard(runtimeA, now)
	taskB := testTaskProcessGuard(runtimeB, now)
	taskB.TaskFence.TaskID = "q7654321"
	taskB.TaskFence.AgentInstanceID = "agent-2"
	taskB.TaskFence.AgentSessionID = "session-2"
	taskB.NonceSHA256 = testNonceHash("task-b")
	taskB.ContainerPID = 6

	runtimeProofA := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtimeA.ContainerID + ".scope"}
	runtimeProofB := processProof{PID: 322, StartTimeTick: 89, CgroupPath: "/system.slice/docker-" + runtimeB.ContainerID + ".scope"}
	taskKeyA, taskKeyB := taskProcessKey(taskA), taskProcessKey(taskB)
	processes := &concurrentRuntimeProcessInspector{
		runtimeProofs: map[string]processProof{runtimeA.ContainerID: runtimeProofA, runtimeB.ContainerID: runtimeProofB},
		taskProofs: map[string]taskProcessProof{
			taskKeyA: {ContainerPID: taskA.ContainerPID, HostPID: 1001, StartTimeTick: 2001,
				CgroupPath: taskCgroupPath(runtimeProofA.CgroupPath, taskKeyA), NamespaceInode: 77},
			taskKeyB: {ContainerPID: taskB.ContainerPID, HostPID: 1002, StartTimeTick: 2002,
				CgroupPath: taskCgroupPath(runtimeProofB.CgroupPath, taskKeyB), NamespaceInode: 78},
		},
		stopKey: taskKeyA, stopEntered: make(chan struct{}), stopRelease: make(chan struct{}), gone: map[string]bool{},
	}
	docker := newConcurrentRuntimeDocker(runtimeA, runtimeB)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	service := newService(store, docker, processes, func() time.Time { return now })
	for i, runtime := range []AccountRuntimeGuard{runtimeA, runtimeB} {
		if err := service.ArmAccountRuntime(context.Background(), runtime); err != nil {
			t.Fatalf("arm Runtime %d: %v", i, err)
		}
		pid := runtimeProofA.PID
		if i == 1 {
			pid = runtimeProofB.PID
		}
		docker.setRunning(runtime.ContainerID, pid)
		if err := service.StartedAccountRuntime(context.Background(), runtime); err != nil {
			t.Fatalf("start Runtime %d: %v", i, err)
		}
	}
	for i, guard := range []TaskProcessGuard{taskA, taskB} {
		if err := service.ArmTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("arm Task %d: %v", i, err)
		}
		if err := service.StartedTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("start Task %d: %v", i, err)
		}
	}

	stopDone := make(chan error, 1)
	go func() {
		_, err := service.StopTaskProcess(context.Background(), taskA)
		stopDone <- err
	}()
	select {
	case <-processes.stopEntered:
	case err := <-stopDone:
		t.Fatalf("Task A stop finished before entering the controlled stop: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Task A stop did not enter the controlled stop")
	}
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(processes.stopRelease) }) }
	t.Cleanup(releaseStop)

	renewedA := taskA
	renewedA.ExpiresAt = now.Add(30 * time.Second)
	sameRuntimeCtx, cancelSameRuntime := context.WithTimeout(context.Background(), 50*time.Millisecond)
	sameRuntimeDone := make(chan error, 1)
	go func() { sameRuntimeDone <- service.RenewTaskProcess(sameRuntimeCtx, renewedA) }()
	select {
	case err := <-sameRuntimeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			releaseStop()
			if stopErr := <-stopDone; stopErr != nil {
				t.Errorf("stop Task A while cleaning up same-Runtime renewal: %v", stopErr)
			}
			t.Fatalf("same-Runtime renewal did not respect its lock wait deadline: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		cancelSameRuntime()
		releaseStop()
		stopErr := <-stopDone
		renewErr := <-sameRuntimeDone
		t.Fatalf("same-Runtime renewal ignored its lock wait deadline; stop=%v renew=%v", stopErr, renewErr)
	}
	cancelSameRuntime()

	renewedB := taskB
	renewedB.ExpiresAt = now.Add(30 * time.Second)
	renewStarted := time.Now()
	renewDone := make(chan error, 1)
	go func() { renewDone <- service.RenewTaskProcess(context.Background(), renewedB) }()
	select {
	case err := <-renewDone:
		if err != nil {
			releaseStop()
			if stopErr := <-stopDone; stopErr != nil {
				t.Errorf("stop Task A while cleaning up unrelated renewal: %v", stopErr)
			}
			t.Fatalf("Task B lease renewal failed while Task A was stopping: %v", err)
		}
		if elapsed := time.Since(renewStarted); elapsed > 250*time.Millisecond {
			releaseStop()
			if stopErr := <-stopDone; stopErr != nil {
				t.Errorf("stop Task A while cleaning up delayed sibling renewal: %v", stopErr)
			}
			t.Fatalf("Task B renewal waited %s for an unrelated Runtime stop", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		releaseStop()
		stopErr := <-stopDone
		renewErr := <-renewDone
		t.Fatalf("Task B renewal was blocked by Task A stop for at least 250ms; stop=%v renew=%v", stopErr, renewErr)
	}
	releaseStop()
	if err := <-stopDone; err != nil {
		t.Fatalf("stop Task A after sibling renewal: %v", err)
	}
}

func TestStopTaskInAccountRuntimeDoesNotBlockSiblingTaskRenewal(t *testing.T) {
	now := time.Now().UTC()
	runtime := testAccountRuntimeGuard(now)
	taskA := testTaskProcessGuard(runtime, now)
	taskB := testTaskProcessGuard(runtime, now)
	taskB.TaskFence.TaskID = "q7654321"
	taskB.TaskFence.AgentInstanceID = "agent-2"
	taskB.TaskFence.AgentSessionID = "session-2"
	taskB.NonceSHA256 = testNonceHash("task-b")
	taskB.ContainerPID = 6

	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtime.ContainerID + ".scope"}
	taskKeyA, taskKeyB := taskProcessKey(taskA), taskProcessKey(taskB)
	processes := &concurrentRuntimeProcessInspector{
		runtimeProofs: map[string]processProof{runtime.ContainerID: runtimeProof},
		taskProofs: map[string]taskProcessProof{
			taskKeyA: {ContainerPID: taskA.ContainerPID, HostPID: 1001, StartTimeTick: 2001,
				CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskKeyA), NamespaceInode: 77},
			taskKeyB: {ContainerPID: taskB.ContainerPID, HostPID: 1002, StartTimeTick: 2002,
				CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskKeyB), NamespaceInode: 78},
		},
		stopKey: taskKeyA, stopEntered: make(chan struct{}), stopRelease: make(chan struct{}), gone: map[string]bool{},
	}
	docker := newConcurrentRuntimeDocker(runtime)
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtime); err != nil {
		t.Fatalf("arm account Runtime: %v", err)
	}
	docker.setRunning(runtime.ContainerID, runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtime); err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	for name, guard := range map[string]TaskProcessGuard{"A": taskA, "B": taskB} {
		if err := service.ArmTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("arm Task %s: %v", name, err)
		}
		if err := service.StartedTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("start Task %s: %v", name, err)
		}
	}

	stopDone := make(chan error, 1)
	stopComplete := make(chan struct{})
	go func() {
		_, err := service.StopTaskProcess(context.Background(), taskA)
		stopDone <- err
		close(stopComplete)
	}()
	select {
	case <-processes.stopEntered:
	case err := <-stopDone:
		t.Fatalf("Task A stop finished before reaching the cgroup barrier: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Task A stop did not reach the cgroup barrier")
	}
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(processes.stopRelease) }) }
	t.Cleanup(func() {
		releaseStop()
		select {
		case <-stopComplete:
		case <-time.After(time.Second):
			t.Error("Task A stop did not finish after releasing the cgroup barrier")
		}
	})

	updatedTaskB := taskB
	updatedTaskB.ExpiresAt = now.Add(25 * time.Second)
	updatedTaskA := taskA
	updatedTaskA.ExpiresAt = now.Add(25 * time.Second)
	sameTaskRenewStarted := make(chan struct{})
	sameTaskRenewDone := make(chan error, 1)
	go func() {
		close(sameTaskRenewStarted)
		sameTaskRenewDone <- service.RenewTaskProcess(context.Background(), updatedTaskA)
	}()
	<-sameTaskRenewStarted
	renewStarted := make(chan struct{})
	renewDone := make(chan error, 1)
	go func() {
		close(renewStarted)
		renewDone <- service.RenewTaskProcess(context.Background(), updatedTaskB)
	}()
	<-renewStarted
	select {
	case err := <-renewDone:
		if err != nil {
			releaseStop()
			stopErr := <-stopDone
			t.Fatalf("sibling Task B renewal failed while Task A remained at the cgroup barrier: renew=%v stop=%v", err, stopErr)
		}
	case <-time.After(250 * time.Millisecond):
		releaseStop()
		stopErr := <-stopDone
		renewErr := <-renewDone
		sameTaskErr := <-sameTaskRenewDone
		t.Fatalf("sibling Task B renewal waited for Task A stop to unlock the shared Runtime: stop=%v renew=%v same_task_renew=%v", stopErr, renewErr, sameTaskErr)
	}
	select {
	case err := <-sameTaskRenewDone:
		t.Fatalf("same Task renewal was not serialized behind its stop: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case err := <-stopDone:
		t.Fatalf("Task A stop crossed its cgroup barrier before release: %v", err)
	default:
	}

	storedA, ok := store.getTask(taskKeyA)
	if !ok || storedA.Destroyed || storedA.Guard.TaskFence != taskA.TaskFence || storedA.Process.ContainerPID != taskA.ContainerPID {
		t.Fatalf("Task A changed before its cgroup barrier was released: %#v found=%t", storedA, ok)
	}
	storedB, ok := store.getTask(taskKeyB)
	if !ok || storedB.Destroyed || storedB.Guard.TaskFence != taskB.TaskFence || storedB.Guard.ExpiresAt != updatedTaskB.ExpiresAt ||
		storedB.Process.ContainerPID != taskB.ContainerPID || storedB.Process.HostPID != 1002 || storedB.Process.CgroupPath != taskCgroupPath(runtimeProof.CgroupPath, taskKeyB) {
		t.Fatalf("Task B renewal changed its fence or process identity: %#v found=%t", storedB, ok)
	}
	processes.mu.Lock()
	taskBGone := processes.gone[taskKeyB]
	processes.mu.Unlock()
	if taskBGone {
		t.Fatal("Task A stop removed the sibling Task B process")
	}

	releaseStop()
	if err := <-stopDone; err != nil {
		t.Fatalf("stop Task A after sibling renewal: %v", err)
	}
	if err := <-sameTaskRenewDone; err == nil {
		t.Fatal("same Task renewal succeeded after its process was stopped")
	}
	storedA, ok = store.getTask(taskKeyA)
	if !ok || !storedA.Destroyed || storedA.Guard.TaskFence != taskA.TaskFence {
		t.Fatalf("Task A stop proof was not isolated to Task A: %#v found=%t", storedA, ok)
	}
	storedB, ok = store.getTask(taskKeyB)
	if !ok || storedB.Destroyed || storedB.Guard.TaskFence != taskB.TaskFence || storedB.Guard.ExpiresAt != updatedTaskB.ExpiresAt {
		t.Fatalf("Task A stop changed Task B after the barrier was released: %#v found=%t", storedB, ok)
	}
}

func TestAccountRuntimeStopWaitsForTaskOperations(t *testing.T) {
	now := time.Now().UTC()
	runtime := testAccountRuntimeGuard(now)
	taskA := testTaskProcessGuard(runtime, now)
	taskB := testTaskProcessGuard(runtime, now)
	taskB.TaskFence.TaskID = "q7654321"
	taskB.TaskFence.AgentInstanceID = "agent-2"
	taskB.TaskFence.AgentSessionID = "session-2"
	taskB.NonceSHA256 = testNonceHash("task-b")
	taskB.ContainerPID = 6

	runtimeProof := processProof{PID: 321, StartTimeTick: 88, CgroupPath: "/system.slice/docker-" + runtime.ContainerID + ".scope"}
	taskKeyA, taskKeyB := taskProcessKey(taskA), taskProcessKey(taskB)
	processes := &concurrentRuntimeProcessInspector{
		runtimeProofs: map[string]processProof{runtime.ContainerID: runtimeProof},
		taskProofs: map[string]taskProcessProof{
			taskKeyA: {ContainerPID: taskA.ContainerPID, HostPID: 1001, StartTimeTick: 2001,
				CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskKeyA), NamespaceInode: 77},
			taskKeyB: {ContainerPID: taskB.ContainerPID, HostPID: 1002, StartTimeTick: 2002,
				CgroupPath: taskCgroupPath(runtimeProof.CgroupPath, taskKeyB), NamespaceInode: 78},
		},
		allowRuntimeDestruction: true, attachEntered: make(chan struct{}), attachRelease: make(chan struct{}), gone: map[string]bool{},
	}
	docker := newConcurrentRuntimeDocker(runtime)
	docker.runtimeRemoved = make(chan struct{})
	store, err := openLeaseStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	service := newService(store, docker, processes, func() time.Time { return now })
	if err := service.ArmAccountRuntime(context.Background(), runtime); err != nil {
		t.Fatalf("arm account Runtime: %v", err)
	}
	docker.setRunning(runtime.ContainerID, runtimeProof.PID)
	if err := service.StartedAccountRuntime(context.Background(), runtime); err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	for name, guard := range map[string]TaskProcessGuard{"A": taskA, "B": taskB} {
		if err := service.ArmTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("arm Task %s: %v", name, err)
		}
		if err := service.StartedTaskProcess(context.Background(), guard); err != nil {
			t.Fatalf("start Task %s: %v", name, err)
		}
	}
	processes.mu.Lock()
	processes.holdAttachKey = taskKeyB
	processes.mu.Unlock()

	updatedTaskB := taskB
	updatedTaskB.ExpiresAt = now.Add(25 * time.Second)
	renewDone := make(chan error, 1)
	renewComplete := make(chan struct{})
	go func() {
		renewDone <- service.RenewTaskProcess(context.Background(), updatedTaskB)
		close(renewComplete)
	}()
	select {
	case <-processes.attachEntered:
	case <-time.After(time.Second):
		t.Fatal("Task B renewal did not reach its process-identity barrier")
	}

	type runtimeStopResult struct {
		attestation app.AccountAgentRuntimeStopAttestation
		err         error
	}
	stopStarted := make(chan struct{})
	stopDone := make(chan runtimeStopResult, 1)
	stopComplete := make(chan struct{})
	go func() {
		close(stopStarted)
		attestation, err := service.StopAccountRuntime(context.Background(), runtime.Fence, runtime.NonceSHA256)
		stopDone <- runtimeStopResult{attestation: attestation, err: err}
		close(stopComplete)
	}()
	<-stopStarted
	var releaseOnce sync.Once
	releaseTaskOperation := func() { releaseOnce.Do(func() { close(processes.attachRelease) }) }
	t.Cleanup(func() {
		releaseTaskOperation()
		for _, complete := range []<-chan struct{}{renewComplete, stopComplete} {
			select {
			case <-complete:
			case <-time.After(time.Second):
				t.Error("pending Runtime/Task operation did not complete after releasing the barrier")
			}
		}
	})
	select {
	case <-docker.runtimeRemoved:
		releaseTaskOperation()
		<-renewComplete
		<-stopComplete
		t.Fatal("whole Runtime was destroyed while a Task operation still held its shared fence")
	case <-time.After(100 * time.Millisecond):
	}

	releaseTaskOperation()
	if err := <-renewDone; err != nil {
		t.Fatalf("Task B lease renewal failed before Runtime stop: %v", err)
	}
	result := <-stopDone
	if result.err != nil || !result.attestation.Destroyed || result.attestation.Fence != runtime.Fence {
		t.Fatalf("Runtime stop after Task operation: proof=%+v err=%v", result.attestation, result.err)
	}
	verifyRuntimeAttestation(t, result.attestation)
	select {
	case <-docker.runtimeRemoved:
	case <-time.After(time.Second):
		t.Fatal("Runtime stop did not remove the whole Runtime after Task operations ended")
	}
	storedB, ok := store.getTask(taskKeyB)
	if !ok || !storedB.Destroyed || storedB.Guard.TaskFence != taskB.TaskFence || storedB.Guard.ExpiresAt != updatedTaskB.ExpiresAt ||
		storedB.Process.ContainerPID != taskB.ContainerPID || storedB.Process.HostPID != 1002 || storedB.Process.CgroupPath != taskCgroupPath(runtimeProof.CgroupPath, taskKeyB) {
		t.Fatalf("Runtime destruction changed Task B's renewed fence or process evidence: %#v found=%t", storedB, ok)
	}
}

func TestOperationLockerPreservesWriterFairnessAndCancellation(t *testing.T) {
	var locker operationLocker
	initialReaderRelease, err := locker.acquireMode(context.Background(), "runtime-fence", operationLockShared)
	if err != nil {
		t.Fatal(err)
	}
	var writerReleaseOnce, laterReaderReleaseOnce sync.Once
	writerRelease := make(chan struct{})
	laterReaderRelease := make(chan struct{})
	t.Cleanup(func() {
		initialReaderRelease()
		writerReleaseOnce.Do(func() { close(writerRelease) })
		laterReaderReleaseOnce.Do(func() { close(laterReaderRelease) })
	})

	writerAcquired := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		release, err := locker.acquireMode(context.Background(), "runtime-fence", operationLockExclusive)
		if err != nil {
			writerDone <- err
			return
		}
		close(writerAcquired)
		<-writerRelease
		release()
		writerDone <- nil
	}()
	waitForOperationQueue(t, &locker, "runtime-fence", 1)

	laterReaderAcquired := make(chan struct{})
	laterReaderDone := make(chan error, 1)
	go func() {
		release, err := locker.acquireMode(context.Background(), "runtime-fence", operationLockShared)
		if err != nil {
			laterReaderDone <- err
			return
		}
		close(laterReaderAcquired)
		<-laterReaderRelease
		release()
		laterReaderDone <- nil
	}()
	waitForOperationQueue(t, &locker, "runtime-fence", 2)
	locker.mu.Lock()
	queue := locker.locks["runtime-fence"].queue
	writerFirst := len(queue) >= 2 && queue[0].mode == operationLockExclusive && queue[1].mode == operationLockShared
	locker.mu.Unlock()
	if !writerFirst {
		t.Fatal("operation queue did not preserve Runtime writer order")
	}

	initialReaderRelease()
	select {
	case <-writerAcquired:
	case <-time.After(time.Second):
		t.Fatal("queued Runtime writer did not acquire after the initial reader released")
	}
	select {
	case <-laterReaderAcquired:
		t.Fatal("later Runtime reader bypassed a queued writer")
	default:
	}
	writerReleaseOnce.Do(func() { close(writerRelease) })
	select {
	case <-laterReaderAcquired:
	case <-time.After(time.Second):
		t.Fatal("later Runtime reader did not acquire after the writer released")
	}
	laterReaderReleaseOnce.Do(func() { close(laterReaderRelease) })
	if err := <-writerDone; err != nil {
		t.Fatalf("Runtime writer: %v", err)
	}
	if err := <-laterReaderDone; err != nil {
		t.Fatalf("later Runtime reader: %v", err)
	}

	exclusiveRelease, err := locker.acquireMode(context.Background(), "cancel-fence", operationLockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	var cancelLockReleaseOnce sync.Once
	releaseCancelLock := func() { cancelLockReleaseOnce.Do(exclusiveRelease) }
	t.Cleanup(releaseCancelLock)
	ctx, cancel := context.WithCancel(context.Background())
	cancelStarted := make(chan struct{})
	cancelDone := make(chan error, 1)
	go func() {
		close(cancelStarted)
		_, err := locker.acquireMode(ctx, "cancel-fence", operationLockShared)
		cancelDone <- err
	}()
	<-cancelStarted
	waitForOperationQueue(t, &locker, "cancel-fence", 1)
	cancel()
	if err := <-cancelDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued operation cancellation = %v, want context.Canceled", err)
	}
	releaseCancelLock()
	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), time.Second)
	defer acquireCancel()
	release, err := locker.acquireMode(acquireCtx, "cancel-fence", operationLockExclusive)
	if err != nil {
		t.Fatalf("operation after canceled waiter: %v", err)
	}
	release()
}

func waitForOperationQueue(t *testing.T, locker *operationLocker, key string, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		locker.mu.Lock()
		lock := locker.locks[key]
		queued := lock != nil && len(lock.queue) >= count
		locker.mu.Unlock()
		if queued {
			return
		}
		goruntime.Gosched()
	}
	t.Fatalf("operation lock %q did not queue %d waiters", key, count)
}

type concurrentRuntimeDocker struct {
	mu             sync.Mutex
	values         map[string]containerInspect
	runtimeRemoved chan struct{}
	removeOnce     sync.Once
}

func newConcurrentRuntimeDocker(guards ...AccountRuntimeGuard) *concurrentRuntimeDocker {
	d := &concurrentRuntimeDocker{values: make(map[string]containerInspect, len(guards))}
	for _, guard := range guards {
		d.values[guard.ContainerID] = newFakeAccountRuntimeDocker(guard, false, 0).value
	}
	return d
}

func (d *concurrentRuntimeDocker) inspect(ctx context.Context, id string) (containerInspect, error) {
	if err := ctx.Err(); err != nil {
		return containerInspect{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.values[id]
	if !ok {
		return containerInspect{}, errContainerNotFound
	}
	return value, nil
}

func (*concurrentRuntimeDocker) listByLabel(context.Context, string, string) ([]containerSummary, error) {
	return nil, errors.New("unexpected container discovery")
}
func (d *concurrentRuntimeDocker) kill(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.values[id]
	if !ok {
		return errContainerNotFound
	}
	if value.State.Running != nil {
		*value.State.Running = false
	}
	if value.State.PID != nil {
		*value.State.PID = 0
	}
	d.values[id] = value
	return nil
}
func (d *concurrentRuntimeDocker) wait(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.values[id]; !ok {
		return errContainerNotFound
	}
	return nil
}
func (d *concurrentRuntimeDocker) remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.values[id]; !ok {
		return errContainerNotFound
	}
	delete(d.values, id)
	if d.runtimeRemoved != nil {
		d.removeOnce.Do(func() { close(d.runtimeRemoved) })
	}
	return nil
}

func (d *concurrentRuntimeDocker) setRunning(id string, pid int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value := d.values[id]
	*value.State.Running = true
	*value.State.PID = pid
	d.values[id] = value
}

type concurrentRuntimeProcessInspector struct {
	mu                      sync.Mutex
	runtimeProofs           map[string]processProof
	taskProofs              map[string]taskProcessProof
	stopKey                 string
	stopEntered             chan struct{}
	stopRelease             chan struct{}
	gone                    map[string]bool
	allowRuntimeDestruction bool
	runtimeTreeGone         bool
	holdAttachKey           string
	attachEntered           chan struct{}
	attachRelease           chan struct{}
}

func (*concurrentRuntimeProcessInspector) Ready() error { return nil }
func (p *concurrentRuntimeProcessInspector) Capture(containerID string, pid int) (processProof, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	proof, ok := p.runtimeProofs[containerID]
	if !ok || proof.PID != pid {
		return processProof{}, fmt.Errorf("runtime process proof mismatch for %q", containerID)
	}
	return proof, nil
}
func (p *concurrentRuntimeProcessInspector) KillTree(string, processProof) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowRuntimeDestruction {
		return errors.New("unexpected Runtime kill")
	}
	p.runtimeTreeGone = true
	return nil
}
func (p *concurrentRuntimeProcessInspector) Gone(string, processProof) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.allowRuntimeDestruction {
		return false, errors.New("unexpected Runtime check")
	}
	return p.runtimeTreeGone, nil
}
func (p *concurrentRuntimeProcessInspector) AttachTaskProcess(containerID string, runtime processProof, containerPID int, key string) (taskProcessProof, error) {
	p.mu.Lock()
	proof, ok := p.taskProofs[key]
	runtimeProof, runtimeFound := p.runtimeProofs[containerID]
	hold := key == p.holdAttachKey && p.attachEntered != nil && p.attachRelease != nil
	entered, release := p.attachEntered, p.attachRelease
	p.mu.Unlock()
	if hold {
		close(entered)
		<-release
	}
	if !ok || !runtimeFound || proof.ContainerPID != containerPID || runtime.CgroupPath != runtimeProof.CgroupPath ||
		!cgroupPathWithin(runtime.CgroupPath, proof.CgroupPath) {
		return taskProcessProof{}, errors.New("Task process proof mismatch")
	}
	return proof, nil
}
func (p *concurrentRuntimeProcessInspector) KillTaskProcess(_ string, _ processProof, _ taskProcessProof, key string) error {
	if key == p.stopKey {
		close(p.stopEntered)
		<-p.stopRelease
	}
	p.mu.Lock()
	p.gone[key] = true
	p.mu.Unlock()
	return nil
}
func (p *concurrentRuntimeProcessInspector) TaskProcessGone(_ string, _ processProof, _ taskProcessProof, key string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gone[key], nil
}
func (p *concurrentRuntimeProcessInspector) RemoveTaskCgroup(_ string, _ processProof, _ taskProcessProof, _ string) error {
	return nil
}

var _ dockerAPI = (*concurrentRuntimeDocker)(nil)
var _ taskProcessInspector = (*concurrentRuntimeProcessInspector)(nil)
var _ processInspector = (*concurrentRuntimeProcessInspector)(nil)
