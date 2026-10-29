//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	identitydomain "morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/dshguardian"
	"morde/server/internal/storage/workspacefs"
)

type guardianRuntimeFrame struct {
	Type            string `json:"type"`
	TaskID          string `json:"task_id"`
	ContainerPID    int    `json:"container_pid"`
	RuntimeSequence int64  `json:"runtime_sequence"`
	ID              string `json:"id"`
	ThroughSeq      int64  `json:"through_seq"`
	SHA256          string `json:"sha256"`
}

type f01ImmediateCloudDSHModel struct{}

func (f01ImmediateCloudDSHModel) Complete(context.Context, app.ModelCall) (app.ModelResponse, error) {
	return app.ModelResponse{Text: "The persistent cloud task completed.", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
}

type f01ObservedTaskExecutor struct {
	inner TaskExecutor
	errs  chan<- error
}

func (e f01ObservedTaskExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	err := e.inner.ExecuteTask(ctx, value)
	if err != nil {
		select {
		case e.errs <- err:
		default:
		}
	}
	return err
}

func TestRealDockerGuardianProcess(t *testing.T) {
	if os.Getenv("MORDE_REAL_DOCKER_GUARDIAN_HELPER") != "1" {
		t.Skip("Guardian subprocess helper")
	}
	key, err := dshguardian.LoadAttestationPrivateKey(os.Getenv("MORDE_REAL_DOCKER_GUARDIAN_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := dshguardian.Open(os.Getenv("MORDE_REAL_DOCKER_GUARDIAN_STATE"), "/var/run/docker.sock", key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if err := service.Serve(ctx, os.Getenv("MORDE_REAL_DOCKER_GUARDIAN_SOCKET"), 0o600, 100*time.Millisecond,
		func(err error) { t.Errorf("Guardian expiry sweep: %v", err) }); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func startRealDockerGuardianProcess(t *testing.T, stateDir, keyPath, socketPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealDockerGuardianProcess$")
	cmd.Env = append(os.Environ(), "MORDE_REAL_DOCKER_GUARDIAN_HELPER=1",
		"MORDE_REAL_DOCKER_GUARDIAN_STATE="+stateDir, "MORDE_REAL_DOCKER_GUARDIAN_KEY="+keyPath,
		"MORDE_REAL_DOCKER_GUARDIAN_SOCKET="+socketPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Guardian subprocess did not open its Unix socket")
	return nil
}

func TestRealDockerAccountRuntimeTaskIsolationAndGuardianCrashRestart(t *testing.T) {
	image := os.Getenv("MORDE_REAL_DOCKER_RUNTIME_IMAGE")
	if image == "" {
		t.Skip("set MORDE_REAL_DOCKER_RUNTIME_IMAGE to a local pinned Runtime image")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	guardianDir, err := os.MkdirTemp("/tmp", "dg-real-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(guardianDir) })
	stateDir := filepath.Join(guardianDir, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(guardianDir, "attestation.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(guardianDir, "guardian.sock")
	guardianProcess := startRealDockerGuardianProcess(t, stateDir, keyPath, socketPath)
	guardianInterface, err := NewUnixSocketDockerCloudDSHGuardian(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	guardian := guardianInterface.(*unixSocketDockerCloudDSHGuardian)
	docker, err := NewDockerCloudDSHSupervisor(DockerCloudDSHSupervisorOptions{
		DockerHost: "unix:///var/run/docker.sock", Image: image, NodePath: "/usr/local/bin/node",
		ScriptPath: "/opt/morde-cloud-runtime/bin.mjs", SupervisorID: "supervisor-test",
		SigningKey: bytes.Repeat([]byte{5}, 32), Guardian: guardian,
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
		OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 1,
		LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	encodedFence, err := json.Marshal(fence)
	if err != nil {
		t.Fatal(err)
	}
	nonceHash := sha256.Sum256(nonce)
	identity := sha256.Sum256(append(append(encodedFence, 0), nonceHash[:]...))
	name := "morde-account-runtime-" + hex.EncodeToString(identity[:])
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = docker.requestJSON(cleanup, http.MethodDelete, "/containers/"+name+"?force=1&v=0", nil, http.StatusNoContent, nil)
	})
	supervisor := &dockerCloudDSHAccountRuntimeSupervisor{docker: docker, guardian: guardian}
	process, err := supervisor.StartAccountRuntime(ctx, fence, nonce, time.Now().Add(12*time.Second))
	if err != nil {
		t.Fatalf("start account Runtime with real Docker and Guardian: %v", err)
	}
	defer func() { _ = process.Input.Close(); _ = process.Output.Close() }()
	frames := make(chan cloudDSHRead, 32)
	go readCloudDSHFrames(ctx, process.Output, frames)
	send := func(frame any) {
		t.Helper()
		if err := writeCloudDSHFrame(ctx, process.Input, frame); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"type": "runtime_start", "version": 1, "server_instance_id": fence.ServiceInstanceID,
		"owner_id": fence.OwnerID, "runtime_id": fence.RuntimeID, "runtime_generation": fence.RuntimeGeneration,
		"max_concurrent_tasks": 2})
	waitFrame := func() guardianRuntimeFrame {
		t.Helper()
		select {
		case item := <-frames:
			if item.err != nil {
				t.Fatal(item.err)
			}
			var frame guardianRuntimeFrame
			if err := json.Unmarshal(item.frame, &frame); err != nil {
				t.Fatal(err)
			}
			return frame
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return guardianRuntimeFrame{}
	}
	if frame := waitFrame(); frame.Type != "runtime_ready" {
		t.Fatalf("account Runtime did not become ready: %s", frame.Type)
	}
	taskIDs := []string{"q1234567", "q2345678"}
	labels := map[string]string{taskIDs[0]: "1", taskIDs[1]: "2"}
	tasks := map[string]dshguardian.TaskProcessGuard{}
	for sequence, id := range taskIDs {
		label := labels[id]
		send(map[string]any{"type": "task_start", "version": 1, "server_instance_id": fence.ServiceInstanceID,
			"owner_id": fence.OwnerID, "runtime_id": fence.RuntimeID, "runtime_generation": fence.RuntimeGeneration,
			"task_id": id, "agent_id": "agent-" + label, "generation": 1, "authorization_version": 1,
			"session_id": "session-" + label, "runtime_sequence": sequence + 1,
			"model": "container-smoke-model", "tools": []string{}, "prompt": "wait for model response"})
	}
	models := map[string]bool{}
	for len(models) != 2 {
		frame := waitFrame()
		switch frame.Type {
		case "task_process_ready":
			if frame.ContainerPID < 2 || tasks[frame.TaskID].ContainerPID != 0 {
				t.Fatalf("invalid Task child process frame: %+v", frame)
			}
			label := labels[frame.TaskID]
			if label == "" {
				t.Fatalf("unexpected Task child process frame: %+v", frame)
			}
			taskFence := app.AgentInstanceFence{TaskID: frame.TaskID, OwnerID: fence.OwnerID,
				AgentInstanceID: "agent-" + label, AgentGeneration: 1,
				AgentSessionID: "session-" + label, SupervisorID: "supervisor-test",
				LeaseOwner: "worker-runtime", LeaseVersion: 1, AuthorizationVersion: 1,
				EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
			taskNonce := sha256.Sum256([]byte(frame.TaskID))
			guard := dshguardian.TaskProcessGuard{RuntimeFence: fence, TaskFence: taskFence,
				RuntimeContainerID: process.ContainerID, ContainerPID: frame.ContainerPID,
				NonceSHA256: hex.EncodeToString(taskNonce[:]), ExpiresAt: time.Now().Add(8 * time.Second)}
			if err := guardian.ArmTaskProcess(ctx, guard); err != nil {
				t.Fatalf("arm real Task %s cgroup: %v", frame.TaskID, err)
			}
			if err := guardian.StartedTaskProcess(ctx, guard); err != nil {
				t.Fatalf("verify real Task %s cgroup: %v", frame.TaskID, err)
			}
			tasks[frame.TaskID] = guard
			send(map[string]any{"type": "task_process_admit", "server_instance_id": fence.ServiceInstanceID,
				"runtime_id": fence.RuntimeID, "runtime_generation": fence.RuntimeGeneration,
				"runtime_sequence": frame.RuntimeSequence, "task_id": frame.TaskID, "owner_id": fence.OwnerID,
				"agent_id": "agent-" + label, "generation": 1, "authorization_version": 1,
				"session_id": "session-" + label})
		case "checkpoint":
			label := labels[frame.TaskID]
			if label == "" {
				t.Fatalf("unexpected Task checkpoint frame: %+v", frame)
			}
			send(map[string]any{"type": "checkpoint_ack", "task_id": frame.TaskID, "owner_id": fence.OwnerID,
				"agent_id": "agent-" + label, "generation": 1, "authorization_version": 1,
				"session_id": "session-" + label, "id": frame.ID, "ok": true,
				"through_seq": frame.ThroughSeq, "sha256": frame.SHA256})
		case "model":
			models[frame.TaskID] = true
		case "runtime_ready", "task_started":
		case "protocol_error", "task_terminated", "failed":
			t.Fatalf("account Runtime rejected Task flow: %s (%s)", frame.Type, frame.TaskID)
		default:
			t.Logf("waiting for Task model requests: %s", frame.Type)
		}
	}
	stoppedA, err := guardian.StopTaskProcess(ctx, tasks[taskIDs[0]])
	if err != nil || !stoppedA.Destroyed {
		t.Fatalf("stop Task A without stopping Runtime: proof=%+v err=%v", stoppedA, err)
	}
	if err := guardian.RenewTaskProcess(ctx, tasks[taskIDs[1]]); err != nil {
		t.Fatalf("Task B did not survive Task A stop: %v", err)
	}
	if err := guardianProcess.Process.Kill(); err != nil {
		t.Fatalf("crash Guardian test process: %v", err)
	}
	if err := guardianProcess.Wait(); err == nil {
		t.Fatal("Guardian crash test process exited normally")
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("Guardian crash did not leave its socket: info=%v err=%v", info, err)
	}
	guardianProcess = startRealDockerGuardianProcess(t, stateDir, keyPath, socketPath)
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for docker.requireAbsent(ctx, process.ContainerID) != nil {
		select {
		case <-ctx.Done():
			t.Fatalf("Guardian did not remove expired Runtime: %v", ctx.Err())
		case <-poll.C:
		}
	}
	stoppedB, err := guardian.RecoverTaskProcessStopped(ctx, fence, tasks[taskIDs[1]].TaskFence, tasks[taskIDs[1]].NonceSHA256)
	if err != nil || !stoppedB.Destroyed {
		t.Fatalf("recover Task B stop after Guardian restart: proof=%+v err=%v", stoppedB, err)
	}
	for _, proof := range []struct {
		id    string
		proof app.AccountAgentTaskStopAttestation
	}{{taskIDs[0], stoppedA}, {taskIDs[1], stoppedB}} {
		guard := tasks[proof.id]
		if err := guardian.AcknowledgeTaskProcessStopped(ctx, fence, guard.TaskFence, guard.NonceSHA256, proof.proof.ProofSHA256); err != nil {
			t.Fatalf("acknowledge %s Task proof: %v", proof.id, err)
		}
	}
	proof, err := guardian.RecoverAccountRuntimeStopped(ctx, fence, hex.EncodeToString(nonceHash[:]))
	if err != nil || !proof.Destroyed || proof.StartAborted || proof.ContainerID != process.ContainerID {
		t.Fatalf("recover real Runtime stop: proof=%+v err=%v", proof, err)
	}
	if err := guardian.AcknowledgeAccountRuntimeStopped(ctx, fence, proof.NonceSHA256, proof.ProofSHA256); err != nil {
		t.Fatalf("acknowledge real Runtime stop proof: %v", err)
	}
	t.Run("postgres-manager", func(t *testing.T) {
		store, pool := newF01TemporaryStore(t)
		owner := createF01User(t, ctx, store)
		if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
			t.Fatal(err)
		}
		control, err := app.NewAccountAgentRuntimeService(store)
		if err != nil {
			t.Fatal(err)
		}
		stopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		accountSupervisor, err := NewDockerCloudDSHAccountRuntimeSupervisor(docker)
		if err != nil {
			t.Fatal(err)
		}
		supervisorAuth, err := app.NewAgentStopAuthenticator("supervisor-test", bytes.Repeat([]byte{5}, 32))
		if err != nil {
			t.Fatal(err)
		}
		agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator("supervisor-test", key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := app.NewAuditService(store)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
			SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
		}, f01DefaultHosts{})
		if err != nil {
			t.Fatal(err)
		}
		principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
		if err != nil || principal.UserID != owner.ID || principal.Kind != identitydomain.KindSession {
			t.Fatalf("verify real Task owner: principal=%+v err=%v", principal, err)
		}
		taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
		if err != nil {
			t.Fatal(err)
		}
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: accountSupervisor, Guardian: guardian,
			StopVerifier: stopVerifier, TaskStopper: instances, LeaseOwner: "worker-runtime", MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		firstCtx, cancelFirst := context.WithTimeout(ctx, 20*time.Second)
		session, err := manager.Acquire(firstCtx, owner.ID)
		if err != nil {
			cancelFirst()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancelFirst()
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			_, _ = control.RequestStop(cleanup, session.runtime.Fence)
			_ = manager.reapStopCandidates(cleanup)
			_ = session.process.Input.Close()
			_ = session.process.Output.Close()
			_ = docker.requestJSON(cleanup, http.MethodDelete, "/containers/"+session.process.ContainerID+"?force=1&v=0", nil, http.StatusNoContent, nil)
		})
		prepareTask := func(taskCtx context.Context, sequence int64) (app.AgentInstanceLease, []byte) {
			t.Helper()
			submitted, err := taskService.Submit(taskCtx, principal, app.SubmitTaskCommand{
				ModelID: "p1234567", Objective: "Answer with one sentence.",
				Budget:         task.Budget{ActiveSeconds: 60, ToolCalls: 5, ModelTokens: 1000},
				IdempotencyKey: fmt.Sprintf("f01-real-account-task-%d", sequence),
			})
			if err != nil {
				t.Fatalf("submit real account Task %d: %v", sequence, err)
			}
			claimed, found, err := store.ClaimNextTaskWithOwnerLimit(taskCtx, "worker-runtime-task", 25*time.Second, 2)
			if err != nil || !found || claimed.ID != submitted.Task.ID {
				t.Fatalf("claim real account Task %d: claimed=%+v found=%t err=%v", sequence, claimed, found, err)
			}
			nonce := bytes.Repeat([]byte{byte(sequence)}, 32)
			lease, replayed, err := instances.Start(taskCtx, app.AgentInstanceStart{
				Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: owner.ID,
					AgentInstanceID: fmt.Sprintf("agent-%d", sequence), AgentSessionID: fmt.Sprintf("session-%d", sequence),
					SupervisorID: "supervisor-test", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
					AuthorizationVersion: claimed.Authorization.Version, EngineVersion: cloudDSHEngineVersion,
					ProfileVersion: cloudDSHProfileVersion},
				RuntimeFence: &session.runtime.Fence, SupervisorNonce: nonce, LeaseDuration: 20 * time.Second,
			})
			if err != nil || replayed || lease.RuntimeSequence != sequence {
				t.Fatalf("start persisted account Task %d: lease=%+v replayed=%t err=%v", sequence, lease, replayed, err)
			}
			return lease, nonce
		}
		leaseA, nonceA := prepareTask(firstCtx, 1)
		secondCtx, cancelSecond := context.WithTimeout(ctx, 20*time.Second)
		defer cancelSecond()
		reused, err := manager.Acquire(secondCtx, owner.ID)
		if err != nil || reused != session {
			t.Fatalf("second Docker Task did not reuse the PostgreSQL account Runtime: reused=%t err=%v", reused == session, err)
		}
		modelRequestedA, modelRequestedB := make(chan struct{}, 1), make(chan struct{}, 1)
		modelGateA, modelGateB := make(chan struct{}), make(chan struct{})
		doneA, doneB := make(chan error, 1), make(chan error, 1)
		go func() {
			doneA <- f01RunNodeAccountTaskControlled(firstCtx, manager, session, leaseA, nonceA, modelRequestedA, modelGateA)
		}()
		select {
		case <-modelRequestedA:
		case err := <-doneA:
			t.Fatalf("Task A stopped before reaching its model call: %v", err)
		case <-ctx.Done():
			t.Fatalf("PostgreSQL-backed Task A did not reach its model call: %v", ctx.Err())
		}
		leaseB, nonceB := prepareTask(secondCtx, 2)
		go func() {
			doneB <- f01RunNodeAccountTaskControlled(secondCtx, manager, session, leaseB, nonceB, modelRequestedB, modelGateB)
		}()
		requestedB := (<-chan struct{})(modelRequestedB)
		for requestedB != nil {
			select {
			case <-requestedB:
				requestedB = nil
			case err := <-doneB:
				if err == nil {
					t.Fatal("Task B settled before reaching its model call")
				}
				t.Fatalf("Task B stopped before reaching its model call: %v", err)
			case <-ctx.Done():
				t.Fatalf("both PostgreSQL-backed Tasks did not reach their model call: %v", ctx.Err())
			}
		}
		cancelFirst()
		select {
		case err := <-doneA:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Task A returned %v, want context cancellation", err)
			}
		case <-ctx.Done():
			t.Fatalf("Task A did not observe cancellation: %v", ctx.Err())
		}
		stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := manager.StopTask(stopCtx, session, leaseA.Fence); err != nil {
			stopCancel()
			t.Fatalf("stop canceled Task A with Guardian proof: %v", err)
		}
		stopCancel()
		close(modelGateB)
		select {
		case err := <-doneB:
			if err != nil {
				t.Fatalf("Task B did not complete after Task A stopped: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("Task B did not continue after Task A stopped: %v", ctx.Err())
		}
		current, found, err := control.Get(ctx, owner.ID)
		var provedTasks int
		if queryErr := pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_instances
WHERE owner_id=$1 AND state='stopped' AND guardian_stop_attestation IS NOT NULL
 AND guardian_stop_acknowledged_at IS NOT NULL`, owner.ID).Scan(&provedTasks); queryErr != nil {
			t.Fatal(queryErr)
		}
		if err != nil || !found || current.State != app.AccountAgentRuntimeRunning || current.Fence != session.runtime.Fence ||
			provedTasks != 2 || session.closed() {
			t.Fatalf("real Docker/PostgreSQL Task proof or Runtime state is invalid: state=%+v found=%t proved=%d closed=%t err=%v",
				current, found, provedTasks, session.closed(), err)
		}
		if _, err := control.RequestStop(ctx, session.runtime.Fence); err != nil {
			t.Fatal(err)
		}
		if err := manager.reapStopCandidates(ctx); err != nil {
			t.Fatal(err)
		}
		stopped, found, err := control.Get(ctx, owner.ID)
		var runtimeProofAcknowledged bool
		if queryErr := pool.QueryRow(ctx, `SELECT guardian_stop_attestation IS NOT NULL AND guardian_stop_acknowledged_at IS NOT NULL
FROM account_agent_runtimes WHERE service_instance_id=$1::uuid AND owner_id=$2`,
			session.runtime.Fence.ServiceInstanceID, owner.ID).Scan(&runtimeProofAcknowledged); queryErr != nil {
			t.Fatal(queryErr)
		}
		if err != nil || !found || stopped.State != app.AccountAgentRuntimeStopped || !runtimeProofAcknowledged {
			t.Fatalf("real Guardian account Runtime stop proof was not persisted and acknowledged: state=%+v found=%t ack=%t err=%v",
				stopped, found, runtimeProofAcknowledged, err)
		}
	})
	t.Run("portal-task-worker", func(t *testing.T) {
		store, pool := newF01TemporaryStore(t)
		owner := createF01User(t, ctx, store)
		if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
			t.Fatal(err)
		}
		control, err := app.NewAccountAgentRuntimeService(store)
		if err != nil {
			t.Fatal(err)
		}
		accountSupervisor, err := NewDockerCloudDSHAccountRuntimeSupervisor(docker)
		if err != nil {
			t.Fatal(err)
		}
		stopVerifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		supervisorAuth, err := app.NewAgentStopAuthenticator("supervisor-test", bytes.Repeat([]byte{5}, 32))
		if err != nil {
			t.Fatal(err)
		}
		agentGuardianAuth, err := app.NewAgentGuardianStopAuthenticator("supervisor-test", key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		taskGuardianAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		instances, err := app.NewAgentInstanceServiceWithGuardianStops(store, supervisorAuth, agentGuardianAuth, taskGuardianAuth)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := app.NewAuditService(store)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
			SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
		}, f01DefaultHosts{})
		if err != nil {
			t.Fatal(err)
		}
		taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
		if err != nil {
			t.Fatal(err)
		}
		sealer, err := app.NewSealer(bytes.Repeat([]byte{9}, 32))
		if err != nil {
			t.Fatal(err)
		}
		runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = blocks.Close() })
		syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
		if err != nil {
			t.Fatal(err)
		}
		toolBroker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: accountSupervisor, Guardian: guardian,
			StopVerifier: stopVerifier, TaskStopper: instances, LeaseOwner: "portal-worker-runtime", MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		var session *cloudDSHAccountRuntimeSession
		runtimeStopped := false
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if session == nil {
				manager.mu.Lock()
				session = manager.sessions[owner.ID]
				manager.mu.Unlock()
			}
			if session == nil {
				return
			}
			if !runtimeStopped {
				_, _ = control.RequestStop(cleanup, session.runtime.Fence)
				_ = manager.reapStopCandidates(cleanup)
			}
			_ = session.process.Input.Close()
			_ = session.process.Output.Close()
			_ = docker.requestJSON(cleanup, http.MethodDelete, "/containers/"+session.process.ContainerID+"?force=1&v=0", nil, http.StatusNoContent, nil)
		})
		checkpoints, err := app.NewAgentCheckpointService(store, sealer)
		if err != nil {
			t.Fatal(err)
		}
		children, err := app.NewAgentChildSessionService(store)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
			NodePath: "/usr/local/bin/node", ScriptPath: "/opt/morde-cloud-runtime/bin.mjs",
			SupervisorID: "supervisor-test", Supervisor: docker, AccountRuntimes: manager,
			Instances: instances, Checkpoints: checkpoints, Models: f01ImmediateCloudDSHModel{},
			Tools: toolBroker, Children: children, Completion: toolBroker,
		})
		if err != nil {
			t.Fatal(err)
		}
		executionErrors := make(chan error, 1)
		backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
			Store: store, Executor: f01ObservedTaskExecutor{inner: executor, errs: executionErrors},
			WorkerID: "f01-portal-task-worker", LeaseDuration: 30 * time.Second,
			PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2,
			Logger: zap.NewNop(),
		})
		if err != nil {
			t.Fatal(err)
		}
		router, err := api.New(api.Deps{
			Hosts: f01UnusedHostService{}, TLSAuthorizer: f01UnusedTLSAuthorizer{}, Verifier: credentials,
			Auth: f01UnusedAuth{}, Tasks: taskService, WorkspaceSync: syncService, MCPDisabled: true,
		}, api.Options{
			Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
			SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test",
			HostSuffix: "hosts.example.test", PreviewPortMin: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		portal := httptest.NewServer(router)
		t.Cleanup(portal.Close)
		cookie := createF01Session(t, ctx, store, owner.ID)
		postCtx, cancelPost := context.WithCancel(ctx)
		body, err := json.Marshal(map[string]any{
			"model_id": "p1234567", "objective": "Answer with one sentence.",
			"budget": map[string]int64{"active_seconds": 60, "tool_calls": 5, "model_tokens": 1000},
		})
		if err != nil {
			cancelPost()
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(postCtx, http.MethodPost, portal.URL+"/api/tasks", bytes.NewReader(body))
		if err != nil {
			cancelPost()
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: "ch_portal", Value: cookie})
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", portal.URL)
		request.Header.Set("Idempotency-Key", "f01-real-runtime-worker-001")
		response, err := portal.Client().Do(request)
		if err != nil {
			cancelPost()
			t.Fatal(err)
		}
		var accepted struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
			_ = response.Body.Close()
			cancelPost()
			t.Fatal(err)
		}
		_ = response.Body.Close()
		cancelPost()
		if response.StatusCode != http.StatusAccepted || accepted.ID == "" || accepted.Status != string(task.StatusQueued) {
			t.Fatalf("Portal Task submission = status %d, response %+v", response.StatusCode, accepted)
		}

		workerCtx, cancelWorker := context.WithCancel(context.Background())
		workerDone := make(chan error, 1)
		go func() { workerDone <- backgroundWorker.Run(workerCtx) }()
		workerStopped := false
		defer func() {
			cancelWorker()
			if !workerStopped {
				select {
				case runErr := <-workerDone:
					if !errors.Is(runErr, context.Canceled) {
						t.Errorf("TaskWorker.Run() = %v, want context cancellation during cleanup", runErr)
					}
				case <-time.After(10 * time.Second):
					t.Error("TaskWorker did not stop")
				}
			}
		}()
		deadline := time.Now().Add(30 * time.Second)
		var completed task.Task
		var lastGetErr error
		for time.Now().Before(deadline) {
			select {
			case executionErr := <-executionErrors:
				t.Fatalf("Portal TaskWorker execution stopped: %v", executionErr)
			default:
			}
			current, getErr := store.GetTask(ctx, accepted.ID, owner.ID)
			lastGetErr = getErr
			if getErr == nil {
				completed = current.Task
				if completed.Status == task.StatusCompleted {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if completed.ID == "" || completed.Result != "The persistent cloud task completed." ||
			completed.FileDeliveryIntent != task.FileDeliveryNone || completed.AgentInstanceID == "" {
			t.Fatalf("Portal-submitted Task did not complete through the persistent Runtime after disconnect: task=%+v last_get_error=%v", completed, lastGetErr)
		}
		cancelWorker()
		select {
		case runErr := <-workerDone:
			if !errors.Is(runErr, context.Canceled) {
				t.Fatalf("TaskWorker.Run() = %v, want context cancellation", runErr)
			}
			workerStopped = true
		case <-time.After(10 * time.Second):
			t.Fatal("TaskWorker did not stop after completing the Task")
		}
		current, found, err := control.Get(ctx, owner.ID)
		if err != nil || !found || current.State != app.AccountAgentRuntimeRunning {
			t.Fatalf("Portal Task closed the account Runtime: runtime=%+v found=%t err=%v", current, found, err)
		}
		var taskProofs int
		if queryErr := pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND state='stopped' AND guardian_stop_attestation IS NOT NULL
 AND guardian_stop_acknowledged_at IS NOT NULL`, accepted.ID, owner.ID).Scan(&taskProofs); queryErr != nil {
			t.Fatal(queryErr)
		}
		if taskProofs != 1 {
			t.Fatalf("Portal Task Guardian stop proof acknowledgements = %d, want 1", taskProofs)
		}
		if session, err = manager.Acquire(ctx, owner.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := control.RequestStop(ctx, session.runtime.Fence); err != nil {
			t.Fatal(err)
		}
		if err := manager.reapStopCandidates(ctx); err != nil {
			t.Fatal(err)
		}
		stopped, found, err := control.Get(ctx, owner.ID)
		if err != nil || !found || stopped.State != app.AccountAgentRuntimeStopped {
			t.Fatalf("Portal account Runtime did not stop with Guardian proof: runtime=%+v found=%t err=%v", stopped, found, err)
		}
		runtimeStopped = true
	})
	if err := guardianProcess.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop Guardian test process: %v", err)
	}
	if err := guardianProcess.Wait(); err != nil {
		t.Fatalf("Guardian test process did not stop cleanly: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Guardian socket remained after clean shutdown: %v", err)
	}
}
