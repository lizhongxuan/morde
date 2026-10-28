package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

type accountRuntimeControlStub struct {
	runtime                app.AccountAgentRuntime
	candidates             []app.AccountAgentRuntimeStopIdentity
	pendingStopProofs      []app.AccountAgentRuntimeStopAttestation
	acknowledgedStopProofs []app.AccountAgentRuntimeStopAttestation
	requestStops           int
	stopProofs             int
	renewals               int
}

func (s *accountRuntimeControlStub) Start(_ context.Context, request app.AccountAgentRuntimeStart) (app.AccountAgentRuntime, bool, error) {
	digest := sha256.Sum256(request.SupervisorNonce)
	s.runtime = app.AccountAgentRuntime{
		Fence: app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: request.OwnerID,
			RuntimeID: request.RuntimeID, RuntimeGeneration: 1, LeaseOwner: request.LeaseOwner, LeaseVersion: 1, ExecutionEpoch: 1},
		State: app.AccountAgentRuntimeStarting, LeaseExpiresAt: time.Now().Add(request.LeaseDuration),
		MaxConcurrentTasks: request.MaxConcurrentTasks, NonceSHA256: hex.EncodeToString(digest[:]),
	}
	return s.runtime, false, nil
}

func (s *accountRuntimeControlStub) Ready(_ context.Context, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	if s.runtime.Fence != fence {
		return app.AccountAgentRuntime{}, errors.New("runtime fence mismatch")
	}
	s.runtime.State = app.AccountAgentRuntimeRunning
	return s.runtime, nil
}

func (s *accountRuntimeControlStub) Renew(_ context.Context, fence app.AccountAgentRuntimeFence, duration time.Duration) (app.AccountAgentRuntime, error) {
	if s.runtime.Fence != fence || s.runtime.State != app.AccountAgentRuntimeRunning {
		return app.AccountAgentRuntime{}, errors.New("runtime is not running")
	}
	s.renewals++
	s.runtime.LeaseExpiresAt = time.Now().Add(duration)
	return s.runtime, nil
}

func (s *accountRuntimeControlStub) Get(context.Context, string) (app.AccountAgentRuntime, bool, error) {
	return s.runtime, s.runtime.Fence.RuntimeID != "", nil
}

func (s *accountRuntimeControlStub) StopIdentity(context.Context, string) (app.AccountAgentRuntimeStopIdentity, error) {
	return app.AccountAgentRuntimeStopIdentity{Fence: s.runtime.Fence, NonceSHA256: s.runtime.NonceSHA256,
		ExpiresAt: s.runtime.LeaseExpiresAt}, nil
}

func (s *accountRuntimeControlStub) ListAccountAgentRuntimeStopCandidates(context.Context, int) ([]app.AccountAgentRuntimeStopIdentity, error) {
	candidates := append([]app.AccountAgentRuntimeStopIdentity(nil), s.candidates...)
	s.candidates = nil
	return candidates, nil
}

func (s *accountRuntimeControlStub) RequestStop(context.Context, app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	s.requestStops++
	s.runtime.State = app.AccountAgentRuntimeStopping
	return s.runtime, nil
}

func (s *accountRuntimeControlStub) ConfirmGuardianStopped(_ context.Context, attestation app.AccountAgentRuntimeStopAttestation,
	_ app.AccountAgentRuntimeStopVerifier) (bool, error) {
	s.stopProofs++
	s.runtime.State = app.AccountAgentRuntimeStopped
	s.pendingStopProofs = append(s.pendingStopProofs, attestation)
	return false, nil
}

func (s *accountRuntimeControlStub) ListPendingAccountAgentRuntimeStopAttestations(context.Context, int,
	app.AccountAgentRuntimeStopVerifier) ([]app.AccountAgentRuntimeStopAttestation, error) {
	return append([]app.AccountAgentRuntimeStopAttestation(nil), s.pendingStopProofs...), nil
}

func (s *accountRuntimeControlStub) MarkAccountAgentRuntimeStopAttestationAcknowledged(_ context.Context,
	attestation app.AccountAgentRuntimeStopAttestation, _ app.AccountAgentRuntimeStopVerifier) (bool, error) {
	s.acknowledgedStopProofs = append(s.acknowledgedStopProofs, attestation)
	for index, pending := range s.pendingStopProofs {
		if pending.Fence == attestation.Fence && pending.ProofSHA256 == attestation.ProofSHA256 {
			s.pendingStopProofs = append(s.pendingStopProofs[:index], s.pendingStopProofs[index+1:]...)
			break
		}
	}
	return false, nil
}

type accountRuntimeStopVerifierStub struct{}

func (accountRuntimeStopVerifierStub) VerifyAccountAgentRuntimeStop(context.Context, app.AccountAgentRuntimeStopAttestation) (app.VerifiedAccountAgentRuntimeStop, error) {
	return app.VerifiedAccountAgentRuntimeStop{}, nil
}

type accountRuntimeTaskStopperStub struct {
	pending      []app.AccountAgentTaskStopAttestation
	confirmed    []app.AccountAgentTaskStopAttestation
	acknowledged []app.AccountAgentTaskStopAttestation
}

func (s *accountRuntimeTaskStopperStub) ConfirmTaskStopped(_ context.Context, attestation app.AccountAgentTaskStopAttestation) (bool, error) {
	s.confirmed = append(s.confirmed, attestation)
	s.pending = append(s.pending, attestation)
	return false, nil
}

func (s *accountRuntimeTaskStopperStub) ListPendingAccountAgentTaskStopAttestations(context.Context, int) ([]app.AccountAgentTaskStopAttestation, error) {
	return append([]app.AccountAgentTaskStopAttestation(nil), s.pending...), nil
}

func (s *accountRuntimeTaskStopperStub) MarkAccountAgentTaskStopAttestationAcknowledged(_ context.Context,
	attestation app.AccountAgentTaskStopAttestation) (bool, error) {
	s.acknowledged = append(s.acknowledged, attestation)
	for index, pending := range s.pending {
		if pending.ProofSHA256 == attestation.ProofSHA256 && pending.TaskFence == attestation.TaskFence {
			s.pending = append(s.pending[:index], s.pending[index+1:]...)
			break
		}
	}
	return false, nil
}

type accountRuntimeSupervisorStub struct {
	mu         sync.Mutex
	admitted   bool
	recoverErr error
	stopCalls  int
	renewCalls int
}

func (s *accountRuntimeSupervisorStub) StartAccountRuntime(_ context.Context, fence app.AccountAgentRuntimeFence, _ []byte,
	_ time.Time) (CloudDSHAccountRuntimeProcess, error) {
	runtimeInput, portalInput := io.Pipe()
	portalOutput, runtimeOutput := io.Pipe()
	go func() {
		defer func() { _ = runtimeOutput.Close() }()
		reader := bufio.NewReader(runtimeInput)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(line, &frame) != nil {
				return
			}
			switch frame["type"] {
			case "runtime_start":
				_ = json.NewEncoder(runtimeOutput).Encode(map[string]any{"type": "runtime_ready", "version": 1,
					"server_instance_id": fence.ServiceInstanceID, "owner_id": fence.OwnerID, "runtime_id": fence.RuntimeID,
					"runtime_generation": fence.RuntimeGeneration, "max_concurrent_tasks": 2, "engine_version": cloudDSHEngineVersion})
			case "task_start":
				ready := map[string]any{"type": "task_process_ready", "server_instance_id": fence.ServiceInstanceID,
					"owner_id": fence.OwnerID, "runtime_id": fence.RuntimeID, "runtime_generation": fence.RuntimeGeneration,
					"runtime_sequence": frame["runtime_sequence"], "task_id": frame["task_id"], "agent_id": frame["agent_id"],
					"session_id": frame["session_id"], "generation": frame["generation"],
					"authorization_version": frame["authorization_version"], "container_pid": 91}
				_ = json.NewEncoder(runtimeOutput).Encode(ready)
			case "task_process_admit":
				s.mu.Lock()
				s.admitted = true
				s.mu.Unlock()
				started := map[string]any{"type": "task_started", "server_instance_id": fence.ServiceInstanceID,
					"owner_id": fence.OwnerID, "runtime_id": fence.RuntimeID, "runtime_generation": fence.RuntimeGeneration,
					"task_id": frame["task_id"], "agent_id": frame["agent_id"], "session_id": frame["session_id"],
					"generation": frame["generation"], "authorization_version": frame["authorization_version"]}
				_ = json.NewEncoder(runtimeOutput).Encode(started)
			}
		}
	}()
	return CloudDSHAccountRuntimeProcess{ContainerID: strings.Repeat("a", 64), ContainerName: "account-test",
		Input: portalInput, Output: portalOutput}, nil
}

func (s *accountRuntimeSupervisorStub) RenewAccountRuntime(context.Context, app.AccountAgentRuntimeFence, string, time.Time) error {
	s.mu.Lock()
	s.renewCalls++
	s.mu.Unlock()
	return nil
}

func TestAccountAgentRuntimeRenewsIdleSessionBeforeExpiry(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	session.renewMu.Lock()
	session.leaseExpiresAt = time.Now().Add(app.MaxAgentLease / 4)
	session.renewMu.Unlock()
	lifecycleCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := manager.Run(lifecycleCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("account Runtime lifecycle exited unexpectedly: %v", err)
	}
	if control.renewals != 1 || control.runtime.LeaseExpiresAt.Before(time.Now().Add(app.MaxAgentLease/2)) {
		t.Fatalf("idle account Runtime lease was not renewed: count=%d expires=%v", control.renewals, control.runtime.LeaseExpiresAt)
	}
	supervisor.mu.Lock()
	renewCalls := supervisor.renewCalls
	supervisor.mu.Unlock()
	if renewCalls != 1 || session.closed() {
		t.Fatalf("Guardian did not renew the idle Runtime or session closed: calls=%d closed=%t", renewCalls, session.closed())
	}
	if err := manager.renewIdleSessions(context.Background()); err != nil || control.renewals != 1 {
		t.Fatalf("healthy idle Runtime renewed too early: count=%d err=%v", control.renewals, err)
	}
}

func (s *accountRuntimeSupervisorStub) StopAccountRuntime(_ context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	s.mu.Lock()
	s.stopCalls++
	s.mu.Unlock()
	return app.AccountAgentRuntimeStopAttestation{Fence: fence, NonceSHA256: nonceSHA256,
		ProofSHA256: strings.Repeat("c", 64), Destroyed: true, DestroyedAt: time.Now()}, nil
}

func (s *accountRuntimeSupervisorStub) RecoverAccountRuntimeStopped(_ context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	s.mu.Lock()
	err := s.recoverErr
	s.mu.Unlock()
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	return app.AccountAgentRuntimeStopAttestation{Fence: fence, NonceSHA256: nonceSHA256,
		ProofSHA256: strings.Repeat("d", 64), Destroyed: true, DestroyedAt: time.Now()}, nil
}

type accountRuntimeGuardianStub struct {
	mu               sync.Mutex
	armed            bool
	started          bool
	acked            []app.AccountAgentTaskStopAttestation
	runtimeProofAcks []app.AccountAgentRuntimeStopAttestation
}

func (g *accountRuntimeGuardianStub) ArmTaskProcess(_ context.Context, guard dshguardian.TaskProcessGuard) error {
	if guard.ContainerPID != 91 || guard.RuntimeContainerID != strings.Repeat("a", 64) {
		return errors.New("unexpected Guardian task identity")
	}
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
	return nil
}

func (g *accountRuntimeGuardianStub) StartedTaskProcess(context.Context, dshguardian.TaskProcessGuard) error {
	g.mu.Lock()
	g.started = true
	g.mu.Unlock()
	return nil
}

func (g *accountRuntimeGuardianStub) RenewTaskProcess(context.Context, dshguardian.TaskProcessGuard) error {
	return nil
}

func (g *accountRuntimeGuardianStub) StopTaskProcess(_ context.Context, guard dshguardian.TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	return app.AccountAgentTaskStopAttestation{RuntimeFence: guard.RuntimeFence, TaskFence: guard.TaskFence,
		RuntimeContainerID: guard.RuntimeContainerID, ContainerPID: guard.ContainerPID, HostPID: 2,
		StartTimeTick: 3, CgroupPath: "/sys/fs/cgroup/account/task", NonceSHA256: guard.NonceSHA256,
		ExpiresAt: guard.ExpiresAt, DestroyedAt: time.Now(), ProofSHA256: strings.Repeat("b", 64), Destroyed: true}, nil
}

func (g *accountRuntimeGuardianStub) RecoverTaskProcessStopped(context.Context, app.AccountAgentRuntimeFence, app.AgentInstanceFence,
	string) (app.AccountAgentTaskStopAttestation, error) {
	return app.AccountAgentTaskStopAttestation{}, errors.New("unexpected recovery")
}

func (g *accountRuntimeGuardianStub) AcknowledgeTaskProcessStopped(_ context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256, proofSHA256 string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if runtimeFence.OwnerID != taskFence.OwnerID || nonceSHA256 == "" || proofSHA256 == "" {
		return errors.New("invalid Task stop acknowledgement")
	}
	g.acked = append(g.acked, app.AccountAgentTaskStopAttestation{RuntimeFence: runtimeFence, TaskFence: taskFence,
		NonceSHA256: nonceSHA256, ProofSHA256: proofSHA256})
	return nil
}

func (g *accountRuntimeGuardianStub) AcknowledgeAccountRuntimeStopped(_ context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256, proofSHA256 string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if nonceSHA256 == "" || proofSHA256 == "" {
		return errors.New("invalid Runtime stop acknowledgement")
	}
	g.runtimeProofAcks = append(g.runtimeProofAcks, app.AccountAgentRuntimeStopAttestation{Fence: fence,
		NonceSHA256: nonceSHA256, ProofSHA256: proofSHA256})
	return nil
}

func TestAccountAgentRuntimeTaskStartWaitsForGuardianAdmission(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{}
	guardian := &accountRuntimeGuardianStub{}
	taskStopper := &accountRuntimeTaskStopperStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: guardian, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: taskStopper, LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	nonce := []byte(strings.Repeat("n", 32))
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &session.runtime.Fence, RuntimeSequence: 1,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	input, output, err := session.StartTask(context.Background(), fence, nonce, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = output.Close() }()
	ready, err := bufio.NewReader(output).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read Task ready frame: %v", err)
	}
	readyFrame, err := decodeCloudDSHFrame(ready[:len(ready)-1])
	if err != nil || readyFrame.Type != "ready" || readyFrame.Version != 1 || readyFrame.Engine != "dsh" || readyFrame.EngineVer != cloudDSHEngineVersion {
		t.Fatalf("Task ready frame = %q, %v", ready, err)
	}
	start := map[string]any{"type": "start", "version": 1, "task_id": fence.TaskID, "owner_id": fence.OwnerID,
		"agent_id": fence.AgentInstanceID, "session_id": fence.AgentSessionID, "generation": fence.AgentGeneration,
		"authorization_version": fence.AuthorizationVersion, "model": "test-model", "tools": []any{}, "prompt": "do work"}
	if err := writeCloudDSHFrame(context.Background(), input, start); err != nil {
		t.Fatal(err)
	}
	guardian.mu.Lock()
	armed, started := guardian.armed, guardian.started
	guardian.mu.Unlock()
	supervisor.mu.Lock()
	admitted := supervisor.admitted
	supervisor.mu.Unlock()
	if !armed || !started || !admitted {
		t.Fatalf("guardian armed=%t started=%t runtime admitted=%t", armed, started, admitted)
	}
	if err := manager.StopTask(context.Background(), session, fence); err != nil {
		t.Fatal(err)
	}
	if len(taskStopper.confirmed) != 1 || len(taskStopper.acknowledged) != 1 || len(guardian.acked) != 1 {
		t.Fatalf("Task proof confirm=%d DB ack=%d Guardian ack=%d, want 1/1/1", len(taskStopper.confirmed),
			len(taskStopper.acknowledged), len(guardian.acked))
	}
}

func TestAccountAgentRuntimeLifecycleReapsIdleStoppingRuntime(t *testing.T) {
	control := &accountRuntimeControlStub{}
	guardian := &accountRuntimeGuardianStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: &accountRuntimeSupervisorStub{}, Guardian: guardian,
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	control.runtime.State = app.AccountAgentRuntimeStopping
	control.candidates = []app.AccountAgentRuntimeStopIdentity{{Fence: session.runtime.Fence,
		NonceSHA256: session.runtime.NonceSHA256, ExpiresAt: session.runtime.LeaseExpiresAt}}
	if err := manager.reapStopCandidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if control.runtime.State != app.AccountAgentRuntimeStopped || control.stopProofs != 1 {
		t.Fatalf("Runtime state=%q stop proofs=%d, want stopped with one proof", control.runtime.State, control.stopProofs)
	}
	if len(control.acknowledgedStopProofs) != 1 || len(guardian.runtimeProofAcks) != 1 || len(control.pendingStopProofs) != 0 {
		t.Fatalf("Runtime proof acknowledgements DB=%d Guardian=%d pending=%d, want 1/1/0",
			len(control.acknowledgedStopProofs), len(guardian.runtimeProofAcks), len(control.pendingStopProofs))
	}
	if manager.sessions[session.runtime.Fence.OwnerID] != nil {
		t.Fatal("reaped Runtime session remains cached")
	}
}

func TestAccountAgentRuntimeLifecycleReplaysPersistedStopProofAcknowledgements(t *testing.T) {
	runtimeFence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
		OwnerID: "u1234567", RuntimeID: "runtime_1", RuntimeGeneration: 1, LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
	runtimeProof := app.AccountAgentRuntimeStopAttestation{Fence: runtimeFence, NonceSHA256: strings.Repeat("a", 64),
		ProofSHA256: strings.Repeat("b", 64), Destroyed: true, DestroyedAt: time.Now()}
	control := &accountRuntimeControlStub{pendingStopProofs: []app.AccountAgentRuntimeStopAttestation{runtimeProof}}
	taskFence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	taskProof := app.AccountAgentTaskStopAttestation{RuntimeFence: runtimeFence, TaskFence: taskFence,
		NonceSHA256: strings.Repeat("c", 64), ProofSHA256: strings.Repeat("d", 64), Destroyed: true}
	taskStopper := &accountRuntimeTaskStopperStub{pending: []app.AccountAgentTaskStopAttestation{taskProof}}
	guardian := &accountRuntimeGuardianStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: &accountRuntimeSupervisorStub{}, Guardian: guardian, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: taskStopper, LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reapStopCandidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(control.acknowledgedStopProofs) != 1 || len(control.pendingStopProofs) != 0 ||
		len(taskStopper.acknowledged) != 1 || len(taskStopper.pending) != 0 ||
		len(guardian.runtimeProofAcks) != 1 || len(guardian.acked) != 1 {
		t.Fatalf("replayed stop proof ACKs: Runtime DB=%d pending=%d Guardian=%d; Task DB=%d pending=%d Guardian=%d",
			len(control.acknowledgedStopProofs), len(control.pendingStopProofs), len(guardian.runtimeProofAcks),
			len(taskStopper.acknowledged), len(taskStopper.pending), len(guardian.acked))
	}
}

func TestAccountAgentRuntimeLifecycleKeepsRuntimeWithActiveTasks(t *testing.T) {
	control := &accountRuntimeControlStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: &accountRuntimeSupervisorStub{}, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent-1",
		AgentSessionID: "session-1", AgentGeneration: 1, AuthorizationVersion: 1}
	session.tasks[fence] = &cloudDSHAccountTask{fence: fence}
	control.runtime.State = app.AccountAgentRuntimeStopping
	control.candidates = []app.AccountAgentRuntimeStopIdentity{{Fence: session.runtime.Fence,
		NonceSHA256: session.runtime.NonceSHA256, ExpiresAt: session.runtime.LeaseExpiresAt}}
	if err := manager.reapStopCandidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if control.runtime.State != app.AccountAgentRuntimeStopping || control.stopProofs != 0 {
		t.Fatalf("Runtime state=%q stop proofs=%d, want stopping with no proof", control.runtime.State, control.stopProofs)
	}
}

func TestAccountAgentRuntimeLifecyclePersistsStopBeforeReapingExpiredRuntime(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{recoverErr: errors.New("guardian has not stopped the Runtime")}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Acquire(context.Background(), "u1234567")
	if err != nil {
		t.Fatal(err)
	}
	control.runtime.LeaseExpiresAt = time.Now().Add(-time.Second)
	control.candidates = []app.AccountAgentRuntimeStopIdentity{{Fence: session.runtime.Fence,
		NonceSHA256: session.runtime.NonceSHA256, ExpiresAt: control.runtime.LeaseExpiresAt}}
	if err := manager.reapStopCandidates(context.Background()); err != nil {
		t.Fatal(err)
	}
	supervisor.mu.Lock()
	stopCalls := supervisor.stopCalls
	supervisor.mu.Unlock()
	if control.requestStops != 1 || stopCalls != 1 || control.stopProofs != 1 || control.runtime.State != app.AccountAgentRuntimeStopped {
		t.Fatalf("stop intents=%d Guardian stops=%d proofs=%d Runtime state=%q, want 1/1/1/stopped",
			control.requestStops, stopCalls, control.stopProofs, control.runtime.State)
	}
}
