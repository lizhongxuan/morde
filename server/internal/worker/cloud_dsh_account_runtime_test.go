package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/task"
	"morde/server/internal/dshguardian"
)

func TestCloudDSHRuntimeStartupErrorKeepsSafeStage(t *testing.T) {
	wrapped := errors.Join(fmt.Errorf("startup failed: %w", cloudDSHStartupFailure(
		"guardian_arm", "guardian could not arm the account Agent Runtime", errors.New("private provider detail"))), errors.New("cleanup pending"))
	var staged interface{ RuntimeStartupStage() string }
	if !errors.As(wrapped, &staged) || staged.RuntimeStartupStage() != "guardian_arm" {
		t.Fatalf("startup stage was not preserved through wrapping: %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), "private provider detail") {
		t.Fatal("safe startup error exposed its underlying detail")
	}
}

type accountRuntimeControlStub struct {
	runtime                app.AccountAgentRuntime
	candidates             []app.AccountAgentRuntimeStopIdentity
	pendingStopProofs      []app.AccountAgentRuntimeStopAttestation
	acknowledgedStopProofs []app.AccountAgentRuntimeStopAttestation
	requestStops           int
	stopProofs             int
	failureMarks           int
	renewals               int
}

func (s *accountRuntimeControlStub) Start(_ context.Context, request app.AccountAgentRuntimeStart) (app.AccountAgentRuntime, bool, error) {
	digest := sha256.Sum256(request.SupervisorNonce)
	s.runtime = app.AccountAgentRuntime{
		Fence: app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", ConversationID: request.ConversationID, OwnerID: request.OwnerID,
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

func (s *accountRuntimeControlStub) Fail(_ context.Context, fence app.AccountAgentRuntimeFence, reason execution.RuntimeFailureReason) error {
	if s.runtime.Fence != fence {
		return errors.New("runtime fence mismatch")
	}
	if s.runtime.State == app.AccountAgentRuntimeStarting || s.runtime.State == app.AccountAgentRuntimeRunning {
		s.runtime.State = app.AccountAgentRuntimeFailed
		s.runtime.FailureReason = reason
		s.failureMarks++
	}
	return nil
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
	if !attestation.StartAborted {
		s.pendingStopProofs = append(s.pendingStopProofs, attestation)
	}
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

type ownerScopedAccountRuntimeControlStub struct {
	*accountRuntimeControlStub
	mu          sync.Mutex
	runtimes    map[string]app.AccountAgentRuntime
	startCounts map[string]int
}

func (s *ownerScopedAccountRuntimeControlStub) Start(ctx context.Context, request app.AccountAgentRuntimeStart) (app.AccountAgentRuntime, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime, replayed, err := s.accountRuntimeControlStub.Start(ctx, request)
	if err == nil {
		s.runtimes[request.ConversationID] = runtime
		if s.startCounts == nil {
			s.startCounts = make(map[string]int)
		}
		s.startCounts[request.ConversationID]++
	}
	return runtime, replayed, err
}

func (s *ownerScopedAccountRuntimeControlStub) Ready(ctx context.Context, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[fence.ConversationID]
	runtime, err := s.accountRuntimeControlStub.Ready(ctx, fence)
	if err == nil {
		s.runtimes[fence.ConversationID] = runtime
	}
	return runtime, err
}

func (s *ownerScopedAccountRuntimeControlStub) Fail(ctx context.Context, fence app.AccountAgentRuntimeFence, reason execution.RuntimeFailureReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[fence.ConversationID]
	if err := s.accountRuntimeControlStub.Fail(ctx, fence, reason); err != nil {
		return err
	}
	s.runtimes[fence.ConversationID] = s.runtime
	return nil
}

func (s *ownerScopedAccountRuntimeControlStub) Renew(ctx context.Context, fence app.AccountAgentRuntimeFence, duration time.Duration) (app.AccountAgentRuntime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[fence.ConversationID]
	runtime, err := s.accountRuntimeControlStub.Renew(ctx, fence, duration)
	if err == nil {
		s.runtimes[fence.ConversationID] = runtime
	}
	return runtime, err
}

func (s *ownerScopedAccountRuntimeControlStub) Get(_ context.Context, conversationID string) (app.AccountAgentRuntime, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtime, found := s.runtimes[conversationID]
	return runtime, found, nil
}

func (s *ownerScopedAccountRuntimeControlStub) StopIdentity(ctx context.Context, conversationID string) (app.AccountAgentRuntimeStopIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[conversationID]
	return s.accountRuntimeControlStub.StopIdentity(ctx, conversationID)
}

func (s *ownerScopedAccountRuntimeControlStub) RequestStop(ctx context.Context, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[fence.ConversationID]
	runtime, err := s.accountRuntimeControlStub.RequestStop(ctx, fence)
	if err != nil {
		return app.AccountAgentRuntime{}, err
	}
	s.runtimes[fence.ConversationID] = s.runtime
	return runtime, nil
}

func (s *ownerScopedAccountRuntimeControlStub) ConfirmGuardianStopped(ctx context.Context, attestation app.AccountAgentRuntimeStopAttestation,
	verifier app.AccountAgentRuntimeStopVerifier) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = s.runtimes[attestation.Fence.ConversationID]
	if _, err := s.accountRuntimeControlStub.ConfirmGuardianStopped(ctx, attestation, verifier); err != nil {
		return false, err
	}
	s.runtimes[attestation.Fence.ConversationID] = s.runtime
	return false, nil
}

type accountRuntimeStopVerifierStub struct{}

func (accountRuntimeStopVerifierStub) VerifyAccountAgentRuntimeStop(context.Context, app.AccountAgentRuntimeStopAttestation) (app.VerifiedAccountAgentRuntimeStop, error) {
	return app.VerifiedAccountAgentRuntimeStop{}, nil
}

type accountRuntimeTaskStopperStub struct {
	pending          []app.AccountAgentTaskStopAttestation
	confirmed        []app.AccountAgentTaskStopAttestation
	acknowledged     []app.AccountAgentTaskStopAttestation
	recoveryIdentity app.AccountAgentTaskStopRecoveryIdentity
	recoveryReady    bool
	recoveryErr      error
	confirmErr       error
}

func (s *accountRuntimeTaskStopperStub) ConfirmTaskStopped(_ context.Context, attestation app.AccountAgentTaskStopAttestation) (bool, error) {
	if s.confirmErr != nil {
		return false, s.confirmErr
	}
	s.confirmed = append(s.confirmed, attestation)
	s.pending = append(s.pending, attestation)
	return false, nil
}

func (s *accountRuntimeTaskStopperStub) ExpiredAccountAgentTaskStopRecoveryIdentity(_ context.Context,
	_ app.TaskStopClaim) (app.AccountAgentTaskStopRecoveryIdentity, bool, error) {
	if s.recoveryErr != nil {
		return app.AccountAgentTaskStopRecoveryIdentity{}, false, s.recoveryErr
	}
	return s.recoveryIdentity, s.recoveryReady, nil
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
	mu             sync.Mutex
	admitted       bool
	recoverErr     error
	recoverAborted bool
	incomplete     bool
	stopCalls      int
	renewCalls     int
}

type blockingAccountRuntimeSupervisor struct {
	*accountRuntimeSupervisorStub
	mu           sync.Mutex
	startGates   map[string]<-chan struct{}
	renewGates   map[string]<-chan struct{}
	stopGates    map[string]<-chan struct{}
	startEntered chan string
	renewEntered chan string
	stopEntered  chan string
}

func (s *blockingAccountRuntimeSupervisor) wait(ctx context.Context, conversationID string,
	gates map[string]<-chan struct{}, entered chan string) error {
	s.mu.Lock()
	gate := gates[conversationID]
	s.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case entered <- conversationID:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingAccountRuntimeSupervisor) StartAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonce []byte, expiresAt time.Time) (CloudDSHAccountRuntimeProcess, error) {
	if err := s.wait(ctx, fence.ConversationID, s.startGates, s.startEntered); err != nil {
		return CloudDSHAccountRuntimeProcess{}, err
	}
	return s.accountRuntimeSupervisorStub.StartAccountRuntime(ctx, fence, nonce, expiresAt)
}

func (s *blockingAccountRuntimeSupervisor) RenewAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string, expiresAt time.Time) error {
	if err := s.wait(ctx, fence.ConversationID, s.renewGates, s.renewEntered); err != nil {
		return err
	}
	return s.accountRuntimeSupervisorStub.RenewAccountRuntime(ctx, fence, nonceSHA256, expiresAt)
}

func (s *blockingAccountRuntimeSupervisor) RecoverAccountRuntimeStopped(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	if err := s.wait(ctx, fence.ConversationID, s.stopGates, s.stopEntered); err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	return s.accountRuntimeSupervisorStub.RecoverAccountRuntimeStopped(ctx, fence, nonceSHA256)
}

func (s *accountRuntimeSupervisorStub) StartAccountRuntime(_ context.Context, fence app.AccountAgentRuntimeFence, _ []byte,
	_ time.Time) (CloudDSHAccountRuntimeProcess, error) {
	if s.incomplete {
		return CloudDSHAccountRuntimeProcess{}, nil
	}
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
					"runtime_generation": fence.RuntimeGeneration, "max_concurrent_tasks": frame["max_concurrent_tasks"], "engine_version": cloudDSHEngineVersion})
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

func TestAccountAgentRuntimeRejectsIncompleteSupervisorProcess(t *testing.T) {
	control := &accountRuntimeControlStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: &accountRuntimeSupervisorStub{incomplete: true},
		Guardian: &accountRuntimeGuardianStub{}, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "worker-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), "u1234567", "c1234567"); err == nil || !strings.Contains(err.Error(), "incomplete pipes") {
		t.Fatalf("incomplete supervisor process error = %v", err)
	}
	if control.runtime.State != app.AccountAgentRuntimeFailed || control.failureMarks != 1 {
		t.Fatalf("failed startup state=%q marks=%d, want failed/1", control.runtime.State, control.failureMarks)
	}
}

func TestAccountAgentRuntimeSessionsAreIsolatedByConversation(t *testing.T) {
	control := &ownerScopedAccountRuntimeControlStub{
		accountRuntimeControlStub: &accountRuntimeControlStub{}, runtimes: make(map[string]app.AccountAgentRuntime),
	}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: &accountRuntimeSupervisorStub{}, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
	if err != nil {
		t.Fatal(err)
	}
	third, err := manager.Acquire(context.Background(), "u1234567", "c1111111")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.runtime.Fence.OwnerID == second.runtime.Fence.OwnerID ||
		first == third || first.runtime.Fence.ConversationID == third.runtime.Fence.ConversationID ||
		manager.sessions[first.runtime.Fence.ConversationID] != first || manager.sessions[second.runtime.Fence.ConversationID] != second ||
		manager.sessions[third.runtime.Fence.ConversationID] != third {
		t.Fatalf("account Runtime sessions were shared: first=%p/%+v second=%p/%+v sessions=%v",
			first, first.runtime.Fence, second, second.runtime.Fence, manager.sessions)
	}
	reused, err := manager.Acquire(context.Background(), first.runtime.Fence.OwnerID, first.runtime.Fence.ConversationID)
	if err != nil || reused != first {
		t.Fatalf("acquire for first account did not reuse its own Runtime: reused=%t err=%v", reused == first, err)
	}
	if _, err := manager.Acquire(context.Background(), "u7654321", first.runtime.Fence.ConversationID); err == nil {
		t.Fatal("conversation Runtime was reused for a different owner")
	}
	firstState, firstFound, err := control.Get(context.Background(), first.runtime.Fence.ConversationID)
	if err != nil || !firstFound || firstState.Fence != first.runtime.Fence {
		t.Fatalf("first account Runtime state crossed accounts: runtime=%+v found=%t err=%v", firstState, firstFound, err)
	}
	secondState, secondFound, err := control.Get(context.Background(), second.runtime.Fence.ConversationID)
	if err != nil || !secondFound || secondState.Fence != second.runtime.Fence {
		t.Fatalf("second account Runtime state crossed accounts: runtime=%+v found=%t err=%v", secondState, secondFound, err)
	}
	_ = first.process.Input.Close()
	_ = first.process.Output.Close()
	_ = second.process.Input.Close()
	_ = second.process.Output.Close()
	_ = third.process.Input.Close()
	_ = third.process.Output.Close()
}

func newOwnerScopedRuntimeManager(t *testing.T, supervisor CloudDSHAccountRuntimeSupervisor) (*CloudDSHAccountRuntimeManager, *ownerScopedAccountRuntimeControlStub) {
	t.Helper()
	control := &ownerScopedAccountRuntimeControlStub{accountRuntimeControlStub: &accountRuntimeControlStub{},
		runtimes: make(map[string]app.AccountAgentRuntime)}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "worker-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, control
}

type accountRuntimeAcquireResult struct {
	session *cloudDSHAccountRuntimeSession
	err     error
}

func waitAccountRuntimeEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for account Runtime operation")
		return ""
	}
}

func TestAccountAgentRuntimeColdStartsDoNotBlockOtherConversations(t *testing.T) {
	startGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseStart := func() { releaseOnce.Do(func() { close(startGate) }) }
	defer releaseStart()
	supervisor := &blockingAccountRuntimeSupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{},
		startGates: map[string]<-chan struct{}{"c1234567": startGate}, startEntered: make(chan string, 4)}
	manager, control := newOwnerScopedRuntimeManager(t, supervisor)

	first := make(chan accountRuntimeAcquireResult, 1)
	go func() {
		session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
		first <- accountRuntimeAcquireResult{session: session, err: err}
	}()
	if got := waitAccountRuntimeEvent(t, supervisor.startEntered); got != "c1234567" {
		t.Fatalf("blocked start conversation = %q", got)
	}
	second := make(chan accountRuntimeAcquireResult, 1)
	go func() {
		session, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
		second <- accountRuntimeAcquireResult{session: session, err: err}
	}()
	var secondResult accountRuntimeAcquireResult
	select {
	case secondResult = <-second:
	case <-time.After(2 * time.Second):
		t.Error("a blocked conversation startup prevented another conversation from becoming ready")
	}
	releaseStart()
	firstResult := <-first
	if firstResult.err != nil {
		t.Fatalf("first conversation acquire: %v", firstResult.err)
	}
	if secondResult.session == nil {
		secondResult = <-second
	}
	if secondResult.err != nil || secondResult.session == nil || firstResult.session == secondResult.session {
		t.Fatalf("independent conversation Runtime was not created: first=%p second=%p err=%v",
			firstResult.session, secondResult.session, secondResult.err)
	}
	control.mu.Lock()
	startsFirst, startsSecond := control.startCounts["c1234567"], control.startCounts["c7654321"]
	control.mu.Unlock()
	if startsFirst != 1 || startsSecond != 1 {
		t.Fatalf("Runtime starts first=%d second=%d, want 1/1", startsFirst, startsSecond)
	}
	for _, session := range []*cloudDSHAccountRuntimeSession{firstResult.session, secondResult.session} {
		_ = session.process.Input.Close()
		_ = session.process.Output.Close()
	}
}

func TestAccountAgentRuntimeConcurrentAcquireSharesOneStartup(t *testing.T) {
	supervisor := &blockingAccountRuntimeSupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}}
	manager, control := newOwnerScopedRuntimeManager(t, supervisor)
	start := make(chan struct{})
	results := make(chan accountRuntimeAcquireResult, 2)
	for range 2 {
		go func() {
			<-start
			session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
			results <- accountRuntimeAcquireResult{session: session, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.session == nil || first.session != second.session {
		t.Fatalf("same-conversation acquire did not share Runtime: first=%p/%v second=%p/%v",
			first.session, first.err, second.session, second.err)
	}
	control.mu.Lock()
	starts := control.startCounts["c1234567"]
	control.mu.Unlock()
	if starts != 1 {
		t.Fatalf("same conversation started %d Runtimes, want 1", starts)
	}
	_ = first.session.process.Input.Close()
	_ = first.session.process.Output.Close()
}

func TestAccountAgentRuntimeRunsMultipleUsersAndConversationsConcurrently(t *testing.T) {
	supervisor := &blockingAccountRuntimeSupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}}
	manager, control := newOwnerScopedRuntimeManager(t, supervisor)
	requests := []struct{ owner, conversation string }{
		{owner: "u1234567", conversation: "c1111111"},
		{owner: "u1234567", conversation: "c2222222"},
		{owner: "u7654321", conversation: "c3333333"},
		{owner: "u7654321", conversation: "c4444444"},
	}
	start := make(chan struct{})
	type result struct {
		conversation string
		session      *cloudDSHAccountRuntimeSession
		err          error
	}
	results := make(chan result, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			session, err := manager.Acquire(context.Background(), request.owner, request.conversation)
			results <- result{conversation: request.conversation, session: session, err: err}
		}()
	}
	close(start)
	seen := make(map[*cloudDSHAccountRuntimeSession]bool, len(requests))
	for range requests {
		value := <-results
		if value.err != nil || value.session == nil || seen[value.session] {
			t.Fatalf("concurrent conversation %s did not get an isolated Runtime: %p, %v", value.conversation, value.session, value.err)
		}
		seen[value.session] = true
	}
	manager.mu.Lock()
	sessions := make(map[string]*cloudDSHAccountRuntimeSession, len(manager.sessions))
	for conversation, session := range manager.sessions {
		sessions[conversation] = session
	}
	manager.mu.Unlock()
	control.mu.Lock()
	starts := make(map[string]int, len(control.startCounts))
	for conversation, count := range control.startCounts {
		starts[conversation] = count
	}
	control.mu.Unlock()
	for _, request := range requests {
		session := sessions[request.conversation]
		if starts[request.conversation] != 1 || session == nil || session.runtime.Fence.OwnerID != request.owner {
			t.Fatalf("Runtime mapping for %s/%s was not isolated: starts=%d session=%p", request.owner, request.conversation,
				starts[request.conversation], session)
		}
		_ = session.process.Input.Close()
		_ = session.process.Output.Close()
	}
}

func TestAccountAgentRuntimeRenewalDoesNotBlockOtherAcquire(t *testing.T) {
	renewGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseRenew := func() { releaseOnce.Do(func() { close(renewGate) }) }
	defer releaseRenew()
	supervisor := &blockingAccountRuntimeSupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{},
		renewGates: map[string]<-chan struct{}{"c1234567": renewGate}, renewEntered: make(chan string, 4)}
	manager, _ := newOwnerScopedRuntimeManager(t, supervisor)
	first, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = first.process.Input.Close()
		_ = first.process.Output.Close()
		_ = second.process.Input.Close()
		_ = second.process.Output.Close()
	}()
	first.renewMu.Lock()
	first.leaseExpiresAt = time.Now().Add(app.MaxAgentLease / 4)
	first.renewMu.Unlock()

	renewed := make(chan error, 1)
	go func() { renewed <- manager.renewIdleSessions(context.Background()) }()
	if got := waitAccountRuntimeEvent(t, supervisor.renewEntered); got != "c1234567" {
		t.Fatalf("blocked renewal conversation = %q", got)
	}
	acquired := make(chan accountRuntimeAcquireResult, 1)
	go func() {
		session, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
		acquired <- accountRuntimeAcquireResult{session: session, err: err}
	}()
	var result accountRuntimeAcquireResult
	select {
	case result = <-acquired:
	case <-time.After(2 * time.Second):
		t.Error("a blocked Runtime renewal prevented another conversation acquire")
	}
	releaseRenew()
	if result.session == nil {
		result = <-acquired
	}
	if result.err != nil || result.session != second {
		t.Fatalf("other conversation acquire = %p/%v, want %p", result.session, result.err, second)
	}
	if err := <-renewed; err != nil {
		t.Fatalf("idle Runtime renewal: %v", err)
	}
}

func TestAccountAgentRuntimeStopRecoveryDoesNotBlockOtherAcquire(t *testing.T) {
	stopGate := make(chan struct{})
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(stopGate) }) }
	defer releaseStop()
	supervisor := &blockingAccountRuntimeSupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{},
		stopGates: map[string]<-chan struct{}{"c1234567": stopGate}, stopEntered: make(chan string, 4)}
	manager, control := newOwnerScopedRuntimeManager(t, supervisor)
	first, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = second.process.Input.Close()
		_ = second.process.Output.Close()
	}()
	control.mu.Lock()
	stopping := control.runtimes["c1234567"]
	stopping.State = app.AccountAgentRuntimeStopping
	control.runtimes["c1234567"] = stopping
	control.candidates = []app.AccountAgentRuntimeStopIdentity{{
		Fence: first.runtime.Fence, NonceSHA256: first.runtime.NonceSHA256, ExpiresAt: first.runtime.LeaseExpiresAt}}
	control.mu.Unlock()

	stopped := make(chan error, 1)
	go func() { stopped <- manager.reapStopCandidates(context.Background()) }()
	if got := waitAccountRuntimeEvent(t, supervisor.stopEntered); got != "c1234567" {
		t.Fatalf("blocked stop recovery conversation = %q", got)
	}
	acquired := make(chan accountRuntimeAcquireResult, 1)
	go func() {
		session, err := manager.Acquire(context.Background(), "u7654321", "c7654321")
		acquired <- accountRuntimeAcquireResult{session: session, err: err}
	}()
	var result accountRuntimeAcquireResult
	select {
	case result = <-acquired:
	case <-time.After(2 * time.Second):
		t.Error("a blocked Runtime stop recovery prevented another conversation acquire")
	}
	releaseStop()
	if result.session == nil {
		result = <-acquired
	}
	if result.err != nil || result.session != second {
		t.Fatalf("other conversation acquire = %p/%v, want %p", result.session, result.err, second)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Runtime stop recovery: %v", err)
	}
	manager.mu.Lock()
	firstCached, secondCached := manager.sessions["c1234567"], manager.sessions["c7654321"]
	manager.mu.Unlock()
	if firstCached != nil || secondCached != second {
		t.Fatalf("stop recovery session cache first=%p second=%p, want nil/%p", firstCached, secondCached, second)
	}
	_ = first.process.Output.Close()
}

type coordinatorRuntimeWriteCounter struct {
	writes int
}

func (w *coordinatorRuntimeWriteCounter) Write(value []byte) (int, error) {
	w.writes++
	return len(value), nil
}

func (w *coordinatorRuntimeWriteCounter) Close() error { return nil }

func TestAccountAgentRuntimeRejectsCoordinatorFromDifferentRuntimeSession(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		mutateClaim func(*app.ConversationRouteClaim)
		mutateFence func(*app.AccountAgentRuntimeFence)
	}{
		{
			name: "conversation",
			mutateClaim: func(claim *app.ConversationRouteClaim) {
				claim.Message.ConversationID = "c7654321"
			},
		},
		{
			name: "owner",
			mutateClaim: func(claim *app.ConversationRouteClaim) {
				claim.Message.OwnerID = "u7654321"
			},
		},
		{
			name: "service instance",
			mutateFence: func(fence *app.AccountAgentRuntimeFence) {
				fence.ServiceInstanceID = "00000000-0000-4000-8000-000000000002"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			claim, value := coordinatorTestClaimAndContext()
			if testCase.mutateClaim != nil {
				testCase.mutateClaim(&claim)
			}
			identity := cloudDSHCoordinatorIdentity(claim, value)
			runtimeFence := app.AccountAgentRuntimeFence{ServiceInstanceID: claim.ServiceInstanceID,
				ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime_123",
				RuntimeGeneration: 1, LeaseOwner: "route-worker", LeaseVersion: 1, ExecutionEpoch: 1}
			if testCase.mutateFence != nil {
				testCase.mutateFence(&runtimeFence)
			}
			writer := &coordinatorRuntimeWriteCounter{}
			session := &cloudDSHAccountRuntimeSession{
				runtime: app.AccountAgentRuntime{Fence: runtimeFence, MaxConcurrentTasks: 1},
				input:   &cloudDSHRuntimeInput{target: writer}, coordinators: make(map[string]*cloudDSHAccountCoordinator),
				coordinatorSlots: make(chan struct{}, 1),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			_, err := session.StartCoordinator(ctx, claim, identity, map[string]any{
				"type": "coordinator_start", "context_role": "coordinator", "conversation_id": identity.ConversationID,
				"message_id": identity.MessageID, "owner_id": identity.OwnerID, "actor_id": identity.ActorID,
				"agent_id": identity.AgentID, "generation": identity.Generation,
				"authorization_version": identity.AuthorizationVersion, "session_id": identity.SessionID,
				"purpose": identity.Purpose, "operation_id": identity.OperationID,
				"model": "p1234567", "tools": []any{}, "system_prompt": "route", "prompt": "hello",
			})
			if err == nil {
				t.Fatal("coordinator from a different Runtime fence was accepted")
			}
			if writer.writes != 0 || len(session.coordinators) != 0 || session.coordinatorSequence != 0 {
				t.Fatalf("rejected coordinator crossed Runtime boundary: writes=%d coordinators=%d sequence=%d",
					writer.writes, len(session.coordinators), session.coordinatorSequence)
			}
		})
	}
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
	session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
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
	aborted := s.recoverAborted
	s.mu.Unlock()
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	return app.AccountAgentRuntimeStopAttestation{Fence: fence, NonceSHA256: nonceSHA256,
		ProofSHA256: strings.Repeat("d", 64), Destroyed: true, StartAborted: aborted, DestroyedAt: time.Now()}, nil
}

type accountRuntimeGuardianStub struct {
	mu               sync.Mutex
	armed            bool
	started          bool
	acked            []app.AccountAgentTaskStopAttestation
	runtimeProofAcks []app.AccountAgentRuntimeStopAttestation
	recovered        *app.AccountAgentTaskStopAttestation
	recoveryErr      error
	recoveryCalls    int
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

func (g *accountRuntimeGuardianStub) RecoverTaskProcessStopped(_ context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256 string) (app.AccountAgentTaskStopAttestation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recoveryCalls++
	if g.recoveryErr != nil {
		return app.AccountAgentTaskStopAttestation{}, g.recoveryErr
	}
	if g.recovered == nil {
		return app.AccountAgentTaskStopAttestation{}, errors.New("unexpected recovery")
	}
	attestation := *g.recovered
	if attestation.RuntimeFence != runtimeFence || attestation.TaskFence != taskFence || attestation.NonceSHA256 != nonceSHA256 {
		return app.AccountAgentTaskStopAttestation{}, errors.New("recovery identity mismatch")
	}
	return attestation, nil
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

func TestAccountRuntimeReconcilesOnlyExactExpiredTaskStopAndAcknowledgesProof(t *testing.T) {
	now := time.Now().UTC()
	runtimeFence := app.AccountAgentRuntimeFence{ServiceInstanceID: "12345678-1234-1234-1234-123456789abc",
		ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 2,
		LeaseOwner: "runtime-worker", LeaseVersion: 5, ExecutionEpoch: 3}
	taskFence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		AgentInstanceID: "agent-2", AgentGeneration: 2, AgentSessionID: "session-2", SupervisorID: "supervisor-1",
		LeaseOwner: "task-worker", LeaseVersion: 4, AuthorizationVersion: 3, EngineVersion: "engine-v1", ProfileVersion: "profile-v1"}
	identity := app.AccountAgentTaskStopRecoveryIdentity{RuntimeFence: runtimeFence, TaskFence: taskFence,
		AgentLeaseExpiresAt: now.Add(-2 * time.Minute), NonceSHA256: strings.Repeat("a", 64)}
	attestation := app.AccountAgentTaskStopAttestation{RuntimeFence: runtimeFence, TaskFence: taskFence,
		RuntimeContainerID: strings.Repeat("b", 64), ContainerPID: 7, HostPID: 8, StartTimeTick: 9,
		CgroupPath: "/sys/fs/cgroup/morde-task", NonceSHA256: identity.NonceSHA256,
		ExpiresAt: now.Add(-time.Second), DestroyedAt: now, ProofSHA256: strings.Repeat("c", 64), Destroyed: true}
	guardian := &accountRuntimeGuardianStub{recovered: &attestation}
	stopper := &accountRuntimeTaskStopperStub{recoveryIdentity: identity, recoveryReady: true}
	supervisor := &accountRuntimeSupervisorStub{}
	control := &accountRuntimeControlStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: stopper, LeaseOwner: "runtime-worker", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := app.TaskStopClaim{Task: task.Task{ID: taskFence.TaskID, OwnerID: taskFence.OwnerID, Status: task.StatusCancelling},
		Request: task.StopRequest{ID: "stop-request-1", TaskID: taskFence.TaskID, Action: task.ActionCancel, Status: task.StopRequestPending}}
	if err := manager.ReconcileExpiredTaskAgentStop(context.Background(), claim); err != nil {
		t.Fatalf("reconcile exact expired Task stop: %v", err)
	}
	guardian.mu.Lock()
	recoveryCalls, acked := guardian.recoveryCalls, append([]app.AccountAgentTaskStopAttestation(nil), guardian.acked...)
	guardian.mu.Unlock()
	if recoveryCalls != 1 || len(stopper.confirmed) != 1 || len(stopper.acknowledged) != 1 || len(acked) != 1 ||
		stopper.confirmed[0].TaskFence != taskFence || stopper.acknowledged[0].ProofSHA256 != attestation.ProofSHA256 ||
		acked[0].ProofSHA256 != attestation.ProofSHA256 || supervisor.stopCalls != 0 || control.stopProofs != 0 {
		t.Fatalf("Task recovery did not follow proof/ACK path or affected shared Runtime: recover=%d confirmed=%d persistedAck=%d guardianAck=%d runtimeStops=%d runtimeProofs=%d",
			recoveryCalls, len(stopper.confirmed), len(stopper.acknowledged), len(acked), supervisor.stopCalls, control.stopProofs)
	}
}

func TestAccountRuntimeTaskStopRecoveryFailsClosedAndRetries(t *testing.T) {
	now := time.Now().UTC()
	runtimeFence := app.AccountAgentRuntimeFence{ServiceInstanceID: "12345678-1234-1234-1234-123456789abc",
		ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime-1", RuntimeGeneration: 1,
		LeaseOwner: "runtime-worker", LeaseVersion: 1, ExecutionEpoch: 1}
	taskFence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", SupervisorID: "supervisor-1",
		LeaseOwner: "task-worker", LeaseVersion: 1, AuthorizationVersion: 1, EngineVersion: "engine-v1", ProfileVersion: "profile-v1"}
	identity := app.AccountAgentTaskStopRecoveryIdentity{RuntimeFence: runtimeFence, TaskFence: taskFence,
		AgentLeaseExpiresAt: now.Add(-time.Second), NonceSHA256: strings.Repeat("a", 64)}
	stopper := &accountRuntimeTaskStopperStub{recoveryIdentity: identity, recoveryReady: true}
	guardian := &accountRuntimeGuardianStub{recoveryErr: errors.New("Guardian unavailable")}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: &accountRuntimeControlStub{}, Supervisor: &accountRuntimeSupervisorStub{}, Guardian: guardian,
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: stopper, LeaseOwner: "runtime-worker", MaxTasks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := app.TaskStopClaim{Task: task.Task{ID: taskFence.TaskID, OwnerID: taskFence.OwnerID, Status: task.StatusPausing},
		Request: task.StopRequest{ID: "stop-request-1", TaskID: taskFence.TaskID, Action: task.ActionPause, Status: task.StopRequestPending}}
	if err := manager.ReconcileExpiredTaskAgentStop(context.Background(), claim); err == nil {
		t.Fatal("Guardian unavailability did not defer the stop claim")
	}
	if len(stopper.confirmed) != 0 || len(stopper.acknowledged) != 0 {
		t.Fatalf("unavailable Guardian reached proof persistence or ACK: confirmed=%d acked=%d", len(stopper.confirmed), len(stopper.acknowledged))
	}

	stopper.recoveryReady = false
	guardian.recoveryErr = nil
	if err := manager.ReconcileExpiredTaskAgentStop(context.Background(), claim); err != nil {
		t.Fatalf("non-ready identity should defer to DB completion/retry without failing: %v", err)
	}
	guardian.mu.Lock()
	recoveryCalls := guardian.recoveryCalls
	guardian.mu.Unlock()
	if recoveryCalls != 1 || len(stopper.confirmed) != 0 {
		t.Fatalf("non-ready stop identity invoked Guardian: recover=%d confirmed=%d", recoveryCalls, len(stopper.confirmed))
	}

	futureProof := app.AccountAgentTaskStopAttestation{RuntimeFence: runtimeFence, TaskFence: taskFence,
		RuntimeContainerID: strings.Repeat("b", 64), ContainerPID: 7, HostPID: 8, StartTimeTick: 9,
		CgroupPath: "/sys/fs/cgroup/morde-task", NonceSHA256: identity.NonceSHA256,
		ExpiresAt: now.Add(time.Minute), DestroyedAt: now, ProofSHA256: strings.Repeat("d", 64), Destroyed: true}
	guardian.mu.Lock()
	guardian.recovered = &futureProof
	guardian.mu.Unlock()
	stopper.recoveryReady = true
	if err := manager.ReconcileExpiredTaskAgentStop(context.Background(), claim); err == nil {
		t.Fatal("accepted a Guardian Task stop proof with an unexpired Task lease")
	}
	if len(stopper.confirmed) != 0 || len(stopper.acknowledged) != 0 {
		t.Fatalf("unexpired Guardian proof reached persistence or ACK: confirmed=%d acked=%d", len(stopper.confirmed), len(stopper.acknowledged))
	}
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
	acquireCtx, cancelAcquire := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelAcquire()
	session, err := manager.Acquire(acquireCtx, "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	nonce := []byte(strings.Repeat("n", 32))
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &session.runtime.Fence, RuntimeSequence: 1,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	input, output, err := session.StartTask(acquireCtx, fence, nonce, lease)
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
	if err := writeCloudDSHFrame(acquireCtx, input, start); err != nil {
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
	if err := manager.StopTask(acquireCtx, session, fence); err != nil {
		t.Fatal(err)
	}
	cancelAcquire()
	if len(taskStopper.confirmed) != 1 || len(taskStopper.acknowledged) != 1 || len(guardian.acked) != 1 {
		t.Fatalf("Task proof confirm=%d DB ack=%d Guardian ack=%d, want 1/1/1", len(taskStopper.confirmed),
			len(taskStopper.acknowledged), len(guardian.acked))
	}
	routeTaskFrame := func(kind string, target app.AgentInstanceFence) error {
		raw, err := json.Marshal(map[string]any{"type": kind, "server_instance_id": session.runtime.Fence.ServiceInstanceID,
			"runtime_id": session.runtime.Fence.RuntimeID, "runtime_generation": session.runtime.Fence.RuntimeGeneration,
			"task_id": target.TaskID, "owner_id": target.OwnerID, "agent_id": target.AgentInstanceID,
			"session_id": target.AgentSessionID, "generation": target.AgentGeneration,
			"authorization_version": target.AuthorizationVersion})
		if err != nil {
			return err
		}
		return session.routeFrame(raw)
	}
	wrongSession := fence
	wrongSession.AgentSessionID = "other-session"
	if err := routeTaskFrame("task_terminated", wrongSession); err == nil {
		t.Fatal("stopped Task frame with a different session was accepted")
	}
	if err := routeTaskFrame("task_terminated", fence); err != nil || session.closed() {
		t.Fatalf("late stopped Task frame ended sibling Runtime: err=%v closed=%t", err, session.closed())
	}
	unknown := fence
	unknown.TaskID = "q9999999"
	if err := routeTaskFrame("task_terminated", unknown); err == nil {
		t.Fatal("unknown Task frame was accepted without a stop proof")
	}
	if err := session.routeFrame([]byte(`{"type":"failed","code":"runtime_unavailable"}`)); err == nil {
		t.Fatal("unfenced Runtime failure was accepted as a Task failure")
	}
	for sequence := int64(2); sequence <= 8; sequence++ {
		taskCtx, cancelTask := context.WithTimeout(context.Background(), 2*time.Second)
		nextFence := fence
		nextFence.TaskID = fmt.Sprintf("q%07d", sequence)
		nextFence.AgentInstanceID = fmt.Sprintf("agent-%d", sequence)
		nextFence.AgentSessionID = fmt.Sprintf("session-%d", sequence)
		nextLease := lease
		nextLease.Fence, nextLease.RuntimeSequence = nextFence, sequence
		input, output, err := session.StartTask(taskCtx, nextFence, nonce, nextLease)
		if err != nil {
			cancelTask()
			t.Fatal(err)
		}
		if _, err := bufio.NewReader(output).ReadBytes('\n'); err != nil {
			cancelTask()
			t.Fatalf("read Task %d ready frame: %v", sequence, err)
		}
		start["task_id"], start["agent_id"], start["session_id"] = nextFence.TaskID, nextFence.AgentInstanceID, nextFence.AgentSessionID
		if err := writeCloudDSHFrame(taskCtx, input, start); err != nil {
			cancelTask()
			t.Fatalf("start Task %d after initial Task stopped: %v", sequence, err)
		}
		if sequence == 2 {
			if err := routeTaskFrame("failed", nextFence); err != nil || session.closed() {
				cancelTask()
				t.Fatalf("Task failure ended sibling Runtime: err=%v closed=%t", err, session.closed())
			}
			var failed cloudDSHFrame
			if err := json.NewDecoder(output).Decode(&failed); err != nil || failed.Type != "failed" {
				cancelTask()
				t.Fatalf("Task failure was not routed to its Task: type=%q err=%v", failed.Type, err)
			}
		}
		stopCtx := taskCtx
		if sequence == 3 {
			session.mu.Lock()
			active := session.tasks[nextFence]
			session.mu.Unlock()
			for len(active.events) < cap(active.events) {
				active.events <- cloudDSHRead{frame: []byte(`{}`)}
			}
			cancelTask()
			if err := routeTaskFrame("checkpoint", nextFence); err != nil || session.closed() {
				t.Fatalf("canceled Task frame ended sibling Runtime: err=%v closed=%t", err, session.closed())
			}
			stopCtx = context.Background()
		}
		if err := manager.StopTask(stopCtx, session, nextFence); err != nil {
			cancelTask()
			t.Fatalf("stop Task %d: %v", sequence, err)
		}
		_ = input.Close()
		_ = output.Close()
		cancelTask()
	}
	if session.closed() || len(taskStopper.confirmed) != 8 {
		t.Fatalf("account Runtime ended after a Task context was canceled: closed=%t stop proofs=%d", session.closed(), len(taskStopper.confirmed))
	}
}

func TestAccountAgentRuntimeUnarmedTaskStopDoesNotStopSibling(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{recoverErr: errors.New("Runtime is still running")}
	guardian := &accountRuntimeGuardianStub{}
	taskStopper := &accountRuntimeTaskStopperStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: guardian, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: taskStopper, LeaseOwner: "worker-runtime", MaxTasks: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.Acquire(ctx, "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	nonce := []byte(strings.Repeat("n", 32))
	startTask := func(taskID, agentID, sessionID string, sequence int64, admitted bool) app.AgentInstanceFence {
		t.Helper()
		fence := app.AgentInstanceFence{TaskID: taskID, OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: agentID, AgentGeneration: 1,
			AgentSessionID: sessionID, SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
			AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
		lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &session.runtime.Fence, RuntimeSequence: sequence,
			ExpiresAt: time.Now().Add(app.MaxAgentLease)}
		input, output, err := session.StartTask(ctx, fence, nonce, lease)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
		if _, err := bufio.NewReader(output).ReadBytes('\n'); err != nil {
			t.Fatalf("read Task %s ready frame: %v", taskID, err)
		}
		if admitted {
			start := map[string]any{"type": "start", "version": 1, "task_id": fence.TaskID, "owner_id": fence.OwnerID,
				"agent_id": fence.AgentInstanceID, "session_id": fence.AgentSessionID, "generation": fence.AgentGeneration,
				"authorization_version": fence.AuthorizationVersion, "model": "test-model", "tools": []any{}, "prompt": "do work"}
			if err := writeCloudDSHFrame(ctx, input, start); err != nil {
				t.Fatalf("admit Task %s: %v", taskID, err)
			}
		}
		return fence
	}
	sibling := startTask("q1234567", "agent-armed", "session-armed", 1, true)
	siblingTwo := startTask("q2345679", "agent-armed-2", "session-armed-2", 2, true)
	unarmed := startTask("q2345678", "agent-unarmed", "session-unarmed", 3, false)
	if err := manager.StopTask(ctx, session, unarmed); err == nil {
		t.Fatal("unarmed Task stop succeeded while a sibling Task was active")
	}
	if control.requestStops != 0 || supervisor.stopCalls != 0 || session.closed() {
		t.Fatalf("unarmed stop affected shared Runtime: stop requests=%d Runtime stops=%d closed=%t",
			control.requestStops, supervisor.stopCalls, session.closed())
	}
	if err := manager.StopTask(ctx, session, sibling); err != nil {
		t.Fatalf("stop guarded sibling Task: %v", err)
	}
	if control.requestStops != 0 || supervisor.stopCalls != 0 || session.closed() {
		t.Fatalf("stopping sibling retired shared Runtime: requests=%d stops=%d closed=%t",
			control.requestStops, supervisor.stopCalls, session.closed())
	}
	session.mu.Lock()
	_, secondSiblingActive := session.tasks[siblingTwo]
	_, unarmedActive := session.tasks[unarmed]
	session.mu.Unlock()
	if !secondSiblingActive || !unarmedActive {
		t.Fatalf("stopping one Task removed siblings: guarded=%t unarmed=%t", secondSiblingActive, unarmedActive)
	}
	if err := manager.StopTask(ctx, session, siblingTwo); err != nil {
		t.Fatalf("stop second guarded sibling Task: %v", err)
	}
	if control.requestStops != 0 || supervisor.stopCalls != 0 || session.closed() {
		t.Fatalf("stopping second sibling retired shared Runtime: requests=%d stops=%d closed=%t",
			control.requestStops, supervisor.stopCalls, session.closed())
	}
	if err := manager.StopTask(ctx, session, unarmed); err != nil {
		t.Fatalf("retire last unarmed Task after sibling stopped: %v", err)
	}
	if control.runtime.State != app.AccountAgentRuntimeStopped || control.requestStops != 0 || supervisor.stopCalls != 1 || len(taskStopper.confirmed) != 2 {
		t.Fatalf("final Runtime state=%q requests=%d Runtime stops=%d Task proofs=%d",
			control.runtime.State, control.requestStops, supervisor.stopCalls, len(taskStopper.confirmed))
	}
}

func TestAccountAgentRuntimeUnarmedTaskStopProtectsActiveCoordinator(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{}, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		AgentInstanceID: "agent-b", AgentGeneration: 1, AgentSessionID: "session-b", AuthorizationVersion: 1}
	task := &cloudDSHAccountTask{fence: fence}
	session := &cloudDSHAccountRuntimeSession{manager: manager, tasks: map[app.AgentInstanceFence]*cloudDSHAccountTask{fence: task},
		coordinators: map[string]*cloudDSHAccountCoordinator{"session-a": {}}}

	if err := manager.StopTask(context.Background(), session, fence); err == nil ||
		!strings.Contains(err.Error(), "shared Runtime work is active") {
		t.Fatalf("unarmed Task stop with an active coordinator = %v, want an unconfirmed stop", err)
	}
	if session.stopping || session.tasks[fence] != task || session.coordinators["session-a"] == nil ||
		control.failureMarks != 0 || supervisor.stopCalls != 0 {
		t.Fatalf("unarmed Task stop affected active coordinator: stopping=%t task=%t coordinator=%t failures=%d runtimeStops=%d",
			session.stopping, session.tasks[fence] == task, session.coordinators["session-a"] != nil,
			control.failureMarks, supervisor.stopCalls)
	}
}

func TestAccountAgentRuntimeStopLastTaskPreservesActiveCoordinator(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: &accountRuntimeGuardianStub{}, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.Acquire(ctx, "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567",
		AgentInstanceID: "agent-b", AgentGeneration: 1, AgentSessionID: "session-b", SupervisorID: "dsh-supervisor",
		LeaseOwner: "task-worker", LeaseVersion: 1, AuthorizationVersion: 1,
		EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &session.runtime.Fence, RuntimeSequence: 1,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	input, output, err := session.StartTask(ctx, fence, []byte(strings.Repeat("n", 32)), lease)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = output.Close() }()
	if _, err := bufio.NewReader(output).ReadBytes('\n'); err != nil {
		t.Fatalf("read Task ready frame: %v", err)
	}
	start := map[string]any{"type": "start", "version": 1, "task_id": fence.TaskID, "owner_id": fence.OwnerID,
		"agent_id": fence.AgentInstanceID, "session_id": fence.AgentSessionID, "generation": fence.AgentGeneration,
		"authorization_version": fence.AuthorizationVersion, "model": "test-model", "tools": []any{}, "prompt": "do work"}
	if err := writeCloudDSHFrame(ctx, input, start); err != nil {
		t.Fatalf("start Task B: %v", err)
	}
	coordinator := &cloudDSHAccountCoordinator{}
	session.mu.Lock()
	session.coordinators["session-a"] = coordinator
	session.mu.Unlock()

	// A stop candidate can coexist with a live coordinator after Task B stops.
	control.runtime.State = app.AccountAgentRuntimeStopping
	if err := manager.StopTask(ctx, session, fence); err != nil {
		t.Fatalf("stop Task B while coordinator A is active: %v", err)
	}
	control.candidates = []app.AccountAgentRuntimeStopIdentity{{Fence: session.runtime.Fence,
		NonceSHA256: session.runtime.NonceSHA256, ExpiresAt: session.runtime.LeaseExpiresAt}}
	if err := manager.reapStopCandidates(ctx); err != nil {
		t.Fatalf("reap stop candidates with coordinator A active: %v", err)
	}
	supervisor.mu.Lock()
	runtimeStops := supervisor.stopCalls
	supervisor.mu.Unlock()
	session.mu.Lock()
	coordinatorStillActive := session.coordinators["session-a"] == coordinator
	stopping := session.stopping
	session.mu.Unlock()
	if control.runtime.State != app.AccountAgentRuntimeStopping || control.stopProofs != 0 || runtimeStops != 0 ||
		!coordinatorStillActive || stopping || session.closed() {
		t.Fatalf("Task B stop affected coordinator A: runtime=%q stopProofs=%d runtimeStops=%d coordinator=%t stopping=%t closed=%t",
			control.runtime.State, control.stopProofs, runtimeStops, coordinatorStillActive, stopping, session.closed())
	}
}

func TestAccountAgentRuntimeInterleavedTaskFramesStayOnTheirOwnPipes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := app.AccountAgentRuntime{Fence: app.AccountAgentRuntimeFence{
		ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", ConversationID: "c1234567", OwnerID: "u1234567",
		RuntimeID: "runtime_1", RuntimeGeneration: 1,
	}, MaxConcurrentTasks: 2}
	session := &cloudDSHAccountRuntimeSession{runtime: runtime, tasks: make(map[app.AgentInstanceFence]*cloudDSHAccountTask)}
	startTask := func(taskID, agentID, sessionID string, sequence int64) (app.AgentInstanceFence, io.WriteCloser, *bufio.Reader) {
		t.Helper()
		fence := app.AgentInstanceFence{TaskID: taskID, OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: agentID,
			AgentGeneration: 1, AgentSessionID: sessionID, SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
			AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
		lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &runtime.Fence, RuntimeSequence: sequence, ExpiresAt: time.Now().Add(app.MaxAgentLease)}
		input, output, err := session.StartTask(ctx, fence, []byte(strings.Repeat("n", 32)), lease)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(output)
		ready, err := reader.ReadBytes('\n')
		if err != nil || !strings.Contains(string(ready), `"type":"ready"`) {
			t.Fatalf("Task %s ready frame = %q, %v", taskID, ready, err)
		}
		return fence, input, reader
	}
	foreignOwnerFence := app.AgentInstanceFence{TaskID: "q9999999", OwnerID: "u9999999", InitiatorID: "u9999999", AgentInstanceID: "agent-foreign",
		AgentGeneration: 1, AgentSessionID: "session-foreign", SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	foreignOwnerLease := app.AgentInstanceLease{Fence: foreignOwnerFence, RuntimeFence: &runtime.Fence, RuntimeSequence: 99,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	if _, _, err := session.StartTask(ctx, foreignOwnerFence, []byte(strings.Repeat("n", 32)), foreignOwnerLease); err == nil ||
		!strings.Contains(err.Error(), "owner does not match") {
		t.Fatalf("Task from a different owner was not rejected: %v", err)
	}
	if len(session.tasks) != 0 {
		t.Fatalf("rejected foreign Task changed active Task count to %d", len(session.tasks))
	}
	fenceA, inputA, readerA := startTask("q1234567", "agent-1", "session-1", 1)
	fenceB, _, readerB := startTask("q2345678", "agent-2", "session-2", 2)
	overLimitFence := app.AgentInstanceFence{TaskID: "q3456789", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent-3",
		AgentGeneration: 1, AgentSessionID: "session-3", SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	overLimitLease := app.AgentInstanceLease{Fence: overLimitFence, RuntimeFence: &runtime.Fence, RuntimeSequence: 3,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	if _, _, err := session.StartTask(ctx, overLimitFence, []byte(strings.Repeat("n", 32)), overLimitLease); err == nil ||
		!strings.Contains(err.Error(), "concurrency limit") {
		t.Fatalf("third Task was not rejected at Runtime concurrency limit: %v", err)
	}
	if len(session.tasks) != 2 {
		t.Fatalf("rejected Task changed active Task count to %d, want 2", len(session.tasks))
	}

	frame := func(fence app.AgentInstanceFence, sequence int64, kind, payload string) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{"type": kind, "server_instance_id": runtime.Fence.ServiceInstanceID,
			"runtime_id": runtime.Fence.RuntimeID, "runtime_generation": runtime.Fence.RuntimeGeneration,
			"runtime_sequence": sequence, "task_id": fence.TaskID, "owner_id": fence.OwnerID,
			"agent_id": fence.AgentInstanceID, "session_id": fence.AgentSessionID, "generation": fence.AgentGeneration,
			"authorization_version": fence.AuthorizationVersion, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	// Reject a frame carrying Task B's identity if it arrives on Task A's input pipe.
	spoofed, err := json.Marshal(map[string]any{"type": "checkpoint", "task_id": fenceB.TaskID, "owner_id": fenceB.OwnerID,
		"agent_id": fenceB.AgentInstanceID, "session_id": fenceB.AgentSessionID, "generation": fenceB.AgentGeneration,
		"authorization_version": fenceB.AuthorizationVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inputA.Write(append(spoofed, '\n')); err == nil {
		t.Fatal("Task A input pipe accepted Task B identity")
	}

	// Deliver interleaved frames concurrently and make sure each Task reads only its own payload.
	frames := [][]byte{frame(fenceB, 2, "checkpoint", "private-to-B"), frame(fenceA, 1, "model", "private-to-A")}
	var wait sync.WaitGroup
	errs := make(chan error, len(frames))
	for _, data := range frames {
		data := data
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- session.routeFrame(data)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	readPayload := func(reader *bufio.Reader, taskID, payload string) {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read Task %s event: %v", taskID, err)
		}
		var got map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(line), &got); err != nil {
			t.Fatalf("decode Task %s event %q: %v", taskID, line, err)
		}
		if got["task_id"] != taskID || got["payload"] != payload {
			t.Fatalf("Task %s received another Task's frame: %#v", taskID, got)
		}
	}
	readPayload(readerA, fenceA.TaskID, "private-to-A")
	readPayload(readerB, fenceB.TaskID, "private-to-B")
	if len(session.tasks[fenceA].events) != 0 || len(session.tasks[fenceB].events) != 0 {
		t.Fatal("a Task pipe retained another Task's unread frame")
	}
}

func TestFailedAccountAgentRuntimeCanBeGuardianStoppedBeforeLeaseExpiry(t *testing.T) {
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
	session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.process.Input.Close(); _ = session.process.Output.Close() }()
	session.fail(errors.New("account Agent Runtime protocol failed"))
	if !control.runtime.LeaseExpiresAt.After(time.Now()) {
		t.Fatal("test requires an active account Runtime lease")
	}
	if err := manager.stopUnattachedRuntime(context.Background(), session.runtime.Fence); err != nil {
		t.Fatalf("Guardian recovery could not stop a failed Runtime: %v", err)
	}
	if control.runtime.State != app.AccountAgentRuntimeStopped || control.requestStops != 0 || control.stopProofs != 1 {
		t.Fatalf("failed Runtime cleanup changed owner execution: state=%s owner_stop_requests=%d guardian_proofs=%d",
			control.runtime.State, control.requestStops, control.stopProofs)
	}
}

func TestAccountAgentRuntimeBackpressureDoesNotBlockSibling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := app.AccountAgentRuntime{Fence: app.AccountAgentRuntimeFence{
		ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", ConversationID: "c1234567", OwnerID: "u1234567",
		RuntimeID: "runtime_1", RuntimeGeneration: 1,
	}, MaxConcurrentTasks: 2}
	session := &cloudDSHAccountRuntimeSession{runtime: runtime, tasks: make(map[app.AgentInstanceFence]*cloudDSHAccountTask)}
	startTask := func(taskID, agentID, sessionID string, sequence int64) (app.AgentInstanceFence, io.ReadCloser) {
		t.Helper()
		fence := app.AgentInstanceFence{TaskID: taskID, OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: agentID,
			AgentGeneration: 1, AgentSessionID: sessionID, SupervisorID: "dsh-supervisor", LeaseOwner: "task-worker", LeaseVersion: 1,
			AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
		lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &runtime.Fence, RuntimeSequence: sequence, ExpiresAt: time.Now().Add(app.MaxAgentLease)}
		_, output, err := session.StartTask(ctx, fence, []byte(strings.Repeat("n", 32)), lease)
		if err != nil {
			t.Fatal(err)
		}
		return fence, output
	}
	stalled, stalledOutput := startTask("q1234567", "agent-1", "session-1", 1)
	sibling, siblingOutput := startTask("q2345678", "agent-2", "session-2", 2)
	stalledTask := session.tasks[stalled]
	for len(stalledTask.events) < cap(stalledTask.events) {
		stalledTask.events <- cloudDSHRead{frame: []byte(`{}`)}
	}
	frame := func(fence app.AgentInstanceFence) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{"type": "checkpoint", "server_instance_id": runtime.Fence.ServiceInstanceID,
			"runtime_id": runtime.Fence.RuntimeID, "runtime_generation": runtime.Fence.RuntimeGeneration,
			"task_id": fence.TaskID, "owner_id": fence.OwnerID, "agent_id": fence.AgentInstanceID,
			"session_id": fence.AgentSessionID, "generation": fence.AgentGeneration, "authorization_version": fence.AuthorizationVersion})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	done := make(chan error, 1)
	stalledFrame := frame(stalled)
	go func() { done <- session.routeFrame(stalledFrame) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stalled Task stopped the shared Runtime: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a full Task event queue blocked routing for the shared Runtime")
	}
	if _, err := stalledOutput.Read(make([]byte, 64)); err == nil {
		t.Fatal("the stalled Task did not report its event-queue failure")
	}
	if err := session.routeFrame(frame(sibling)); err != nil || session.closed() {
		t.Fatalf("sibling Task could not receive its event: err=%v closed=%t", err, session.closed())
	}
	reader := bufio.NewReader(siblingOutput)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadBytes('\n')
	if err != nil || !strings.Contains(string(got), `"task_id":"q2345678"`) {
		t.Fatalf("sibling received wrong event: %s, %v", got, err)
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
	session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
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
	if manager.sessions[session.runtime.Fence.ConversationID] != nil {
		t.Fatal("reaped Runtime session remains cached")
	}
}

func TestAbortedAccountRuntimeStartDoesNotAcknowledgeMissingGuardianLease(t *testing.T) {
	control := &accountRuntimeControlStub{}
	supervisor := &accountRuntimeSupervisorStub{recoverAborted: true}
	guardian := &accountRuntimeGuardianStub{}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{Control: control,
		Supervisor: supervisor, Guardian: guardian, StopVerifier: accountRuntimeStopVerifierStub{},
		TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: "worker-runtime", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = control.Start(context.Background(), app.AccountAgentRuntimeStart{OwnerID: "u1234567", ConversationID: "c1234567",
		RuntimeID: "aborted-runtime", LeaseOwner: "worker-runtime", SupervisorNonce: []byte(strings.Repeat("n", 32)),
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	control.runtime.LeaseExpiresAt = time.Now().Add(-time.Second)
	if err := manager.stopUnattachedRuntime(context.Background(), control.runtime.Fence); err != nil {
		t.Fatal(err)
	}
	if control.runtime.State != app.AccountAgentRuntimeStopped || control.stopProofs != 1 ||
		len(control.pendingStopProofs) != 0 || len(control.acknowledgedStopProofs) != 0 || len(guardian.runtimeProofAcks) != 0 {
		t.Fatalf("aborted start recovery state=%q proofs=%d pending=%d DB ACK=%d Guardian ACK=%d",
			control.runtime.State, control.stopProofs, len(control.pendingStopProofs), len(control.acknowledgedStopProofs), len(guardian.runtimeProofAcks))
	}
}

func TestAccountAgentRuntimeLifecycleReplaysPersistedStopProofAcknowledgements(t *testing.T) {
	runtimeFence := app.AccountAgentRuntimeFence{ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000",
		ConversationID: "c1234567", OwnerID: "u1234567", RuntimeID: "runtime_1", RuntimeGeneration: 1, LeaseOwner: "worker-runtime", LeaseVersion: 1, ExecutionEpoch: 1}
	runtimeProof := app.AccountAgentRuntimeStopAttestation{Fence: runtimeFence, NonceSHA256: strings.Repeat("a", 64),
		ProofSHA256: strings.Repeat("b", 64), Destroyed: true, DestroyedAt: time.Now()}
	control := &accountRuntimeControlStub{pendingStopProofs: []app.AccountAgentRuntimeStopAttestation{runtimeProof}}
	taskFence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent-1", AgentGeneration: 1,
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
	session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent-1",
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
	session, err := manager.Acquire(context.Background(), "u1234567", "c1234567")
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
