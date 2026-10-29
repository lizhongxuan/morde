package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

type CloudDSHAccountRuntimeControl interface {
	Start(context.Context, app.AccountAgentRuntimeStart) (app.AccountAgentRuntime, bool, error)
	Ready(context.Context, app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error)
	Renew(context.Context, app.AccountAgentRuntimeFence, time.Duration) (app.AccountAgentRuntime, error)
	Get(context.Context, string) (app.AccountAgentRuntime, bool, error)
	StopIdentity(context.Context, string) (app.AccountAgentRuntimeStopIdentity, error)
	ListAccountAgentRuntimeStopCandidates(context.Context, int) ([]app.AccountAgentRuntimeStopIdentity, error)
	ListPendingAccountAgentRuntimeStopAttestations(context.Context, int, app.AccountAgentRuntimeStopVerifier) ([]app.AccountAgentRuntimeStopAttestation, error)
	MarkAccountAgentRuntimeStopAttestationAcknowledged(context.Context, app.AccountAgentRuntimeStopAttestation,
		app.AccountAgentRuntimeStopVerifier) (bool, error)
	RequestStop(context.Context, app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error)
	ConfirmGuardianStopped(context.Context, app.AccountAgentRuntimeStopAttestation, app.AccountAgentRuntimeStopVerifier) (bool, error)
}

type CloudDSHAccountRuntimeProcess struct {
	ContainerID   string
	ContainerName string
	Input         io.WriteCloser
	Output        io.ReadCloser
}

type CloudDSHAccountRuntimeSupervisor interface {
	StartAccountRuntime(context.Context, app.AccountAgentRuntimeFence, []byte, time.Time) (CloudDSHAccountRuntimeProcess, error)
	RenewAccountRuntime(context.Context, app.AccountAgentRuntimeFence, string, time.Time) error
	StopAccountRuntime(context.Context, app.AccountAgentRuntimeFence, string) (app.AccountAgentRuntimeStopAttestation, error)
	RecoverAccountRuntimeStopped(context.Context, app.AccountAgentRuntimeFence, string) (app.AccountAgentRuntimeStopAttestation, error)
}

type CloudDSHAccountRuntimeTaskGuardian interface {
	ArmTaskProcess(context.Context, dshguardian.TaskProcessGuard) error
	StartedTaskProcess(context.Context, dshguardian.TaskProcessGuard) error
	RenewTaskProcess(context.Context, dshguardian.TaskProcessGuard) error
	StopTaskProcess(context.Context, dshguardian.TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error)
	RecoverTaskProcessStopped(context.Context, app.AccountAgentRuntimeFence, app.AgentInstanceFence, string) (app.AccountAgentTaskStopAttestation, error)
	AcknowledgeTaskProcessStopped(context.Context, app.AccountAgentRuntimeFence, app.AgentInstanceFence, string, string) error
	AcknowledgeAccountRuntimeStopped(context.Context, app.AccountAgentRuntimeFence, string, string) error
}

type CloudDSHTaskStopConfirmer interface {
	ConfirmTaskStopped(context.Context, app.AccountAgentTaskStopAttestation) (bool, error)
	ListPendingAccountAgentTaskStopAttestations(context.Context, int) ([]app.AccountAgentTaskStopAttestation, error)
	MarkAccountAgentTaskStopAttestationAcknowledged(context.Context, app.AccountAgentTaskStopAttestation) (bool, error)
}

type CloudDSHAccountRuntimeManagerOptions struct {
	Control      CloudDSHAccountRuntimeControl
	Supervisor   CloudDSHAccountRuntimeSupervisor
	Guardian     CloudDSHAccountRuntimeTaskGuardian
	StopVerifier app.AccountAgentRuntimeStopVerifier
	TaskStopper  CloudDSHTaskStopConfirmer
	LeaseOwner   string
	MaxTasks     int
}

type CloudDSHAccountRuntimeManager struct {
	options  CloudDSHAccountRuntimeManagerOptions
	mu       sync.Mutex
	sessions map[string]*cloudDSHAccountRuntimeSession
}

func NewCloudDSHAccountRuntimeManager(options CloudDSHAccountRuntimeManagerOptions) (*CloudDSHAccountRuntimeManager, error) {
	if options.Control == nil || options.Supervisor == nil || options.Guardian == nil || options.StopVerifier == nil ||
		options.TaskStopper == nil || options.LeaseOwner == "" || options.MaxTasks < 1 || options.MaxTasks > 16 {
		return nil, errors.New("account Agent Runtime dependencies and quota are required")
	}
	return &CloudDSHAccountRuntimeManager{options: options, sessions: make(map[string]*cloudDSHAccountRuntimeSession)}, nil
}

func (m *CloudDSHAccountRuntimeManager) Acquire(ctx context.Context, ownerID string) (*cloudDSHAccountRuntimeSession, error) {
	if ctx == nil || !app.ValidAccountAgentRuntimeOwnerID(ownerID) {
		return nil, errors.New("account Agent Runtime owner is invalid")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if session := m.sessions[ownerID]; session != nil && !session.closed() {
		if err := m.renewAccountRuntime(ctx, session); err != nil {
			session.fail(err)
			return nil, err
		}
		return session, nil
	}
	delete(m.sessions, ownerID)
	current, found, err := m.options.Control.Get(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if found && current.State != app.AccountAgentRuntimeStopped {
		if current.State != app.AccountAgentRuntimeStopping && time.Until(current.LeaseExpiresAt) > 0 {
			return nil, errors.New("account Agent Runtime lease is active in another worker")
		}
		if err := m.stopUnattachedRuntime(ctx, current.Fence); err != nil {
			return nil, err
		}
	}
	runtimeID, err := randomCloudDSHID("runtime_")
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err := randRead(nonce); err != nil {
		return nil, err
	}
	runtime, replayed, err := m.options.Control.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: ownerID, RuntimeID: runtimeID,
		LeaseOwner: m.options.LeaseOwner, SupervisorNonce: nonce, MaxConcurrentTasks: m.options.MaxTasks, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed || runtime.State != app.AccountAgentRuntimeStarting || !validCloudDSHLease(runtime.LeaseExpiresAt) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("account Agent Runtime start was not newly confirmed")
	}
	process, err := m.options.Supervisor.StartAccountRuntime(ctx, runtime.Fence, nonce, runtime.LeaseExpiresAt)
	if err != nil {
		return nil, errors.New("account Agent Runtime container could not start")
	}
	if process.Input == nil || process.Output == nil || process.ContainerID == "" || process.ContainerName == "" {
		if process.Input != nil {
			_ = process.Input.Close()
		}
		if process.Output != nil {
			_ = process.Output.Close()
		}
		return nil, errors.New("account Agent Runtime supervisor returned incomplete pipes")
	}
	frames := make(chan cloudDSHRead, 32)
	// The account Runtime outlives the Task that first acquires it.
	go readCloudDSHFrames(context.Background(), process.Output, frames)
	start := map[string]any{"type": "runtime_start", "version": 1, "server_instance_id": runtime.Fence.ServiceInstanceID,
		"owner_id": runtime.Fence.OwnerID, "runtime_id": runtime.Fence.RuntimeID,
		"runtime_generation": runtime.Fence.RuntimeGeneration, "max_concurrent_tasks": runtime.MaxConcurrentTasks}
	if err := writeCloudDSHFrame(ctx, process.Input, start); err != nil {
		_ = process.Input.Close()
		_ = process.Output.Close()
		return nil, errors.New("account Agent Runtime handshake could not start")
	}
	select {
	case <-ctx.Done():
		_ = process.Input.Close()
		_ = process.Output.Close()
		return nil, ctx.Err()
	case item := <-frames:
		if item.err != nil || !validAccountRuntimeReady(item.frame, runtime) {
			_ = process.Input.Close()
			_ = process.Output.Close()
			return nil, errors.New("account Agent Runtime handshake is invalid")
		}
	}
	ready, err := m.options.Control.Ready(ctx, runtime.Fence)
	if err != nil || ready.Fence != runtime.Fence || ready.State != app.AccountAgentRuntimeRunning {
		_ = process.Input.Close()
		_ = process.Output.Close()
		return nil, errors.New("account Agent Runtime ready state was not persisted")
	}
	session := &cloudDSHAccountRuntimeSession{manager: m, runtime: ready, process: process, frames: frames,
		leaseExpiresAt: ready.LeaseExpiresAt, tasks: make(map[app.AgentInstanceFence]*cloudDSHAccountTask),
		stopped: make(map[cloudDSHIdentity]struct{}),
		input:   &cloudDSHRuntimeInput{target: process.Input}}
	go session.route()
	m.sessions[ownerID] = session
	return session, nil
}

func validAccountRuntimeReady(raw []byte, runtime app.AccountAgentRuntime) bool {
	if !uniqueCloudDSHJSON(raw) {
		return false
	}
	var frame struct {
		Type       string `json:"type"`
		Version    int    `json:"version"`
		ServerID   string `json:"server_instance_id"`
		OwnerID    string `json:"owner_id"`
		RuntimeID  string `json:"runtime_id"`
		Generation int64  `json:"runtime_generation"`
		MaxTasks   int    `json:"max_concurrent_tasks"`
		Engine     string `json:"engine_version"`
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || len(keys) != 8 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil {
		return false
	}
	return frame.Type == "runtime_ready" && frame.Version == 1 && frame.ServerID == runtime.Fence.ServiceInstanceID &&
		frame.OwnerID == runtime.Fence.OwnerID && frame.RuntimeID == runtime.Fence.RuntimeID &&
		frame.Generation == runtime.Fence.RuntimeGeneration && frame.MaxTasks == runtime.MaxConcurrentTasks && frame.Engine == cloudDSHEngineVersion
}

func (m *CloudDSHAccountRuntimeManager) stopUnattachedRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence) error {
	current, found, err := m.options.Control.Get(ctx, fence.OwnerID)
	if err != nil || !found || current.Fence != fence {
		return errors.New("account Agent Runtime stop state is unavailable")
	}
	if current.State == app.AccountAgentRuntimeStopped {
		return nil
	}
	identity, err := m.options.Control.StopIdentity(ctx, fence.OwnerID)
	if err != nil || identity.Fence != fence || identity.NonceSHA256 == "" {
		return errors.New("stale account Agent Runtime stop identity is unavailable")
	}
	if current.State != app.AccountAgentRuntimeStopping && time.Until(current.LeaseExpiresAt) > 0 {
		return errors.New("account Agent Runtime lease is still active")
	}
	attestation, err := m.options.Supervisor.RecoverAccountRuntimeStopped(ctx, fence, identity.NonceSHA256)
	if err != nil {
		if current.State != app.AccountAgentRuntimeStopping {
			if _, err = m.options.Control.RequestStop(ctx, fence); err != nil {
				return errors.New("stale account Agent Runtime stop intent could not be persisted")
			}
		}
		attestation, err = m.options.Supervisor.StopAccountRuntime(ctx, fence, identity.NonceSHA256)
	}
	if err != nil {
		return errors.New("stale account Agent Runtime destruction is unconfirmed")
	}
	if _, err := m.options.Control.ConfirmGuardianStopped(ctx, attestation, m.options.StopVerifier); err != nil {
		return errors.New("stale account Agent Runtime stop proof was not persisted")
	}
	if err := m.acknowledgeAccountRuntimeStopProof(ctx, attestation); err != nil {
		return err
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("account Agent Runtime lifecycle context is required")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := m.reapStopCandidates(ctx); err != nil {
			return err
		}
		if err := m.renewIdleSessions(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *CloudDSHAccountRuntimeManager) renewIdleSessions(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ownerID, session := range m.sessions {
		session.mu.Lock()
		idle := session.failed == nil && len(session.tasks) == 0
		session.mu.Unlock()
		if !idle {
			continue
		}
		session.renewMu.Lock()
		expiresAt := session.leaseExpiresAt
		session.renewMu.Unlock()
		if time.Until(expiresAt) > app.MaxAgentLease/3 {
			continue
		}
		if err := m.renewAccountRuntime(ctx, session); err != nil {
			session.fail(err)
			return fmt.Errorf("renew idle account Agent Runtime for owner %s: %w", ownerID, err)
		}
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) reapStopCandidates(ctx context.Context) error {
	if err := m.reapTaskStopProofAcknowledgements(ctx); err != nil {
		return err
	}
	if err := m.reapAccountRuntimeStopProofAcknowledgements(ctx); err != nil {
		return err
	}
	candidates, err := m.options.Control.ListAccountAgentRuntimeStopCandidates(ctx, app.MaxAccountAgentRuntimeStopCandidates)
	if err != nil {
		return fmt.Errorf("list account Agent Runtime stop candidates: %w", err)
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		session := m.sessions[candidate.Fence.OwnerID]
		if session != nil {
			session.mu.Lock()
			activeTasks := len(session.tasks) > 0
			session.mu.Unlock()
			if activeTasks {
				m.mu.Unlock()
				continue
			}
		}
		if err := m.stopUnattachedRuntime(ctx, candidate.Fence); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("stop account Agent Runtime %s: %w", candidate.Fence.OwnerID, err)
		}
		delete(m.sessions, candidate.Fence.OwnerID)
		if session != nil {
			session.fail(errors.New("account Agent Runtime was stopped by lifecycle recovery"))
			_ = session.process.Input.Close()
			_ = session.process.Output.Close()
		}
		m.mu.Unlock()
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) reapTaskStopProofAcknowledgements(ctx context.Context) error {
	attestations, err := m.options.TaskStopper.ListPendingAccountAgentTaskStopAttestations(ctx, app.MaxAccountAgentRuntimeStopCandidates)
	if err != nil {
		return fmt.Errorf("list pending Guardian Task proof acknowledgements: %w", err)
	}
	for _, attestation := range attestations {
		if err := m.acknowledgeTaskStopProof(ctx, attestation); err != nil {
			return err
		}
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) acknowledgeTaskStopProof(ctx context.Context, attestation app.AccountAgentTaskStopAttestation) error {
	if err := m.options.Guardian.AcknowledgeTaskProcessStopped(ctx, attestation.RuntimeFence,
		attestation.TaskFence, attestation.NonceSHA256, attestation.ProofSHA256); err != nil {
		return fmt.Errorf("acknowledge Guardian Task stop proof for task %s: %w", attestation.TaskFence.TaskID, err)
	}
	if _, err := m.options.TaskStopper.MarkAccountAgentTaskStopAttestationAcknowledged(ctx, attestation); err != nil {
		return fmt.Errorf("persist Guardian Task stop proof acknowledgement for task %s: %w", attestation.TaskFence.TaskID, err)
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) reapAccountRuntimeStopProofAcknowledgements(ctx context.Context) error {
	attestations, err := m.options.Control.ListPendingAccountAgentRuntimeStopAttestations(ctx,
		app.MaxAccountAgentRuntimeStopCandidates, m.options.StopVerifier)
	if err != nil {
		return fmt.Errorf("list pending Guardian account Runtime proof acknowledgements: %w", err)
	}
	for _, attestation := range attestations {
		if err := m.acknowledgeAccountRuntimeStopProof(ctx, attestation); err != nil {
			return err
		}
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) acknowledgeAccountRuntimeStopProof(ctx context.Context,
	attestation app.AccountAgentRuntimeStopAttestation) error {
	if attestation.StartAborted {
		return nil
	}
	if err := m.options.Guardian.AcknowledgeAccountRuntimeStopped(ctx, attestation.Fence,
		attestation.NonceSHA256, attestation.ProofSHA256); err != nil {
		return fmt.Errorf("acknowledge Guardian account Runtime proof for owner %s: %w", attestation.Fence.OwnerID, err)
	}
	if _, err := m.options.Control.MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx, attestation,
		m.options.StopVerifier); err != nil {
		return fmt.Errorf("persist Guardian account Runtime proof acknowledgement for owner %s: %w", attestation.Fence.OwnerID, err)
	}
	return nil
}

func (m *CloudDSHAccountRuntimeManager) renewAccountRuntime(ctx context.Context, session *cloudDSHAccountRuntimeSession) error {
	session.renewMu.Lock()
	defer session.renewMu.Unlock()
	lease, err := m.options.Control.Renew(ctx, session.runtime.Fence, app.MaxAgentLease)
	if err != nil || lease.Fence != session.runtime.Fence || !validCloudDSHLease(lease.LeaseExpiresAt) {
		return errors.New("account Agent Runtime lease renewal failed")
	}
	if err := m.options.Supervisor.RenewAccountRuntime(ctx, lease.Fence, session.runtime.NonceSHA256, lease.LeaseExpiresAt); err != nil {
		return errors.New("account Agent Runtime Guardian renewal failed")
	}
	session.leaseExpiresAt = lease.LeaseExpiresAt
	return nil
}

type cloudDSHAccountRuntimeSession struct {
	manager        *CloudDSHAccountRuntimeManager
	runtime        app.AccountAgentRuntime
	leaseExpiresAt time.Time
	process        CloudDSHAccountRuntimeProcess
	frames         <-chan cloudDSHRead
	input          *cloudDSHRuntimeInput
	mu             sync.Mutex
	startMu        sync.Mutex
	renewMu        sync.Mutex
	tasks          map[app.AgentInstanceFence]*cloudDSHAccountTask
	stopped        map[cloudDSHIdentity]struct{}
	failed         error
}

func (s *cloudDSHAccountRuntimeSession) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed != nil
}

func (s *cloudDSHAccountRuntimeSession) fail(err error) {
	if err == nil {
		err = errors.New("account Agent Runtime stream ended")
	}
	s.mu.Lock()
	if s.failed == nil {
		s.failed = err
	}
	tasks := make([]*cloudDSHAccountTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}
	s.mu.Unlock()
	for _, task := range tasks {
		task.fail(err)
	}
}

func (s *cloudDSHAccountRuntimeSession) route() {
	for item := range s.frames {
		if item.err != nil {
			s.fail(item.err)
			return
		}
		if err := s.routeFrame(item.frame); err != nil {
			s.fail(err)
			return
		}
	}
	s.fail(errors.New("account Agent Runtime output closed"))
}

func (s *cloudDSHAccountRuntimeSession) routeFrame(raw []byte) error {
	var frame map[string]json.RawMessage
	if !uniqueCloudDSHJSON(raw) || json.Unmarshal(raw, &frame) != nil || frame == nil {
		return errors.New("account Agent Runtime frame is invalid")
	}
	var kind string
	if json.Unmarshal(frame["type"], &kind) != nil {
		return errors.New("account Agent Runtime frame type is missing")
	}
	if kind == "runtime_stopped" || kind == "protocol_error" {
		var code string
		if len(frame["code"]) <= 64 {
			_ = json.Unmarshal(frame["code"], &code)
		}
		if code != "" {
			return fmt.Errorf("account Agent Runtime stopped: %s (%s)", kind, code)
		}
		return fmt.Errorf("account Agent Runtime stopped: %s", kind)
	}
	if err := s.validateRuntimeIdentity(frame); err != nil {
		return err
	}
	var identity cloudDSHIdentity
	if json.Unmarshal(raw, &identity) != nil || !cloudDSHIdentityValid(identity) {
		return errors.New("account Agent Runtime Task fence is invalid")
	}
	s.mu.Lock()
	var task *cloudDSHAccountTask
	for fence, candidate := range s.tasks {
		if fence.TaskID == identity.TaskID {
			task = candidate
			break
		}
	}
	if task == nil {
		for stopped := range s.stopped {
			if stopped.TaskID == identity.TaskID && stopped.OwnerID == identity.OwnerID && stopped.AgentID == identity.AgentID &&
				stopped.SessionID == identity.SessionID && stopped.Generation == identity.Generation &&
				stopped.AuthorizationVersion == identity.AuthorizationVersion {
				if kind == "settled" || kind == "failed" || kind == "task_terminated" {
					delete(s.stopped, stopped)
				}
				s.mu.Unlock()
				return nil
			}
		}
	} else if task.fence.OwnerID != identity.OwnerID || task.fence.AgentInstanceID != identity.AgentID ||
		task.fence.AgentGeneration != identity.Generation || task.fence.AuthorizationVersion != identity.AuthorizationVersion ||
		((kind == "task_process_ready" || kind == "task_started" || kind == "task_terminated" || kind == "settled" || kind == "failed") &&
			task.fence.AgentSessionID != identity.SessionID) {
		s.mu.Unlock()
		return errors.New("account Agent Runtime frame names an unknown Task")
	}
	if task != nil && (kind == "settled" || kind == "failed" || kind == "task_terminated") {
		task.terminal = true
	}
	s.mu.Unlock()
	if task == nil {
		return errors.New("account Agent Runtime frame names an unknown Task")
	}
	switch kind {
	case "task_process_ready":
		return s.admitTaskProcess(task, frame)
	case "task_started":
		select {
		case task.started <- nil:
		default:
		}
		return nil
	case "task_terminated":
		var reason string
		if len(frame["reason"]) <= 128 {
			_ = json.Unmarshal(frame["reason"], &reason)
		}
		if reason != "" {
			task.fail(errors.New("account Agent Runtime Task process exited without a result: " + reason))
		} else {
			task.fail(errors.New("account Agent Runtime Task process exited without a result"))
		}
		return nil
	}
	for _, key := range []string{"server_instance_id", "runtime_id", "runtime_generation", "runtime_sequence"} {
		delete(frame, key)
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return errors.New("account Agent Runtime Task frame could not be decoded")
	}
	select {
	case task.events <- cloudDSHRead{frame: encoded}:
		return nil
	case <-task.ctx.Done():
		task.fail(task.ctx.Err())
		return nil
	}
}

func (s *cloudDSHAccountRuntimeSession) validateRuntimeIdentity(frame map[string]json.RawMessage) error {
	var serverID, ownerID, runtimeID string
	var generation int64
	if json.Unmarshal(frame["server_instance_id"], &serverID) != nil || json.Unmarshal(frame["owner_id"], &ownerID) != nil ||
		json.Unmarshal(frame["runtime_id"], &runtimeID) != nil || json.Unmarshal(frame["runtime_generation"], &generation) != nil ||
		serverID != s.runtime.Fence.ServiceInstanceID || ownerID != s.runtime.Fence.OwnerID || runtimeID != s.runtime.Fence.RuntimeID || generation != s.runtime.Fence.RuntimeGeneration {
		return errors.New("account Agent Runtime frame identity does not match")
	}
	return nil
}

func cloudDSHIdentityValid(identity cloudDSHIdentity) bool {
	return cloudDSHID.MatchString(identity.TaskID) && cloudDSHID.MatchString(identity.OwnerID) &&
		cloudDSHID.MatchString(identity.AgentID) && cloudDSHID.MatchString(identity.SessionID) &&
		identity.Generation > 0 && identity.AuthorizationVersion > 0
}

func (s *cloudDSHAccountRuntimeSession) admitTaskProcess(task *cloudDSHAccountTask, frame map[string]json.RawMessage) error {
	var pid int
	var sequence int64
	if json.Unmarshal(frame["container_pid"], &pid) != nil || pid < 1 || json.Unmarshal(frame["runtime_sequence"], &sequence) != nil || sequence != task.sequence {
		return errors.New("account Agent Runtime Task process identity is invalid")
	}
	guard := dshguardian.TaskProcessGuard{RuntimeFence: s.runtime.Fence, TaskFence: task.fence,
		RuntimeContainerID: s.process.ContainerID, ContainerPID: pid, NonceSHA256: task.nonceSHA256, ExpiresAt: task.expiresAt}
	if err := s.manager.options.Guardian.ArmTaskProcess(task.ctx, guard); err != nil {
		task.fail(errors.New("guardian could not isolate the task process"))
		return nil
	}
	if err := s.manager.options.Guardian.StartedTaskProcess(task.ctx, guard); err != nil {
		task.guard = &guard
		task.fail(errors.New("guardian could not verify the isolated task process"))
		return nil
	}
	task.mu.Lock()
	task.guard = &guard
	task.mu.Unlock()
	admit := map[string]any{"type": "task_process_admit", "server_instance_id": s.runtime.Fence.ServiceInstanceID,
		"runtime_id": s.runtime.Fence.RuntimeID, "runtime_generation": s.runtime.Fence.RuntimeGeneration,
		"runtime_sequence": task.sequence, "task_id": task.fence.TaskID, "owner_id": task.fence.OwnerID,
		"agent_id": task.fence.AgentInstanceID, "session_id": task.fence.AgentSessionID,
		"generation": task.fence.AgentGeneration, "authorization_version": task.fence.AuthorizationVersion}
	if err := s.input.WriteFrame(task.ctx, admit); err != nil {
		task.fail(errors.New("account Agent Runtime could not admit the isolated Task process"))
	}
	return nil
}

func (s *cloudDSHAccountRuntimeSession) StartTask(ctx context.Context, fence app.AgentInstanceFence, nonce []byte, lease app.AgentInstanceLease) (io.WriteCloser, io.ReadCloser, error) {
	if ctx == nil || fence.Validate() != nil || len(nonce) != 32 || lease.Fence != fence || lease.RuntimeFence == nil ||
		*lease.RuntimeFence != s.runtime.Fence || lease.RuntimeSequence < 1 || !validCloudDSHLease(lease.ExpiresAt) {
		return nil, nil, errors.New("account Agent Runtime Task lease is invalid")
	}
	nonceHash := sha256.Sum256(nonce)
	task := &cloudDSHAccountTask{ctx: ctx, fence: fence, sequence: lease.RuntimeSequence,
		nonceSHA256: hex.EncodeToString(nonceHash[:]), expiresAt: lease.ExpiresAt,
		events: make(chan cloudDSHRead, 32), started: make(chan error, 1)}
	s.mu.Lock()
	if s.failed != nil || s.tasks[fence] != nil {
		s.mu.Unlock()
		return nil, nil, errors.New("account Agent Runtime is unavailable for this Task")
	}
	s.tasks[fence] = task
	s.mu.Unlock()
	ready, _ := json.Marshal(map[string]any{"type": "ready", "version": 1, "engine": "dsh", "engine_version": cloudDSHEngineVersion})
	task.events <- cloudDSHRead{frame: ready}
	pipes := &cloudDSHAccountTaskPipes{session: s, task: task, ctx: ctx}
	return pipes, pipes, nil
}

func (s *cloudDSHAccountRuntimeSession) renewTask(ctx context.Context, task *cloudDSHAccountTask, expires time.Time) error {
	if err := s.manager.renewAccountRuntime(ctx, s); err != nil {
		return err
	}
	task.mu.Lock()
	guard := task.guard
	if guard != nil {
		updated := *guard
		updated.ExpiresAt = expires
		guard = &updated
		task.guard = guard
	}
	task.expiresAt = expires
	task.mu.Unlock()
	if guard != nil {
		if err := s.manager.options.Guardian.RenewTaskProcess(ctx, *guard); err != nil {
			return errors.New("guardian task process lease renewal failed")
		}
	}
	return nil
}

func (s *cloudDSHAccountRuntimeSession) stopTask(ctx context.Context, task *cloudDSHAccountTask) error {
	task.mu.Lock()
	guard := task.guard
	task.mu.Unlock()
	if guard == nil {
		if _, err := s.manager.options.Control.RequestStop(ctx, s.runtime.Fence); err != nil {
			return errors.New("unarmed account Task stop intent could not be persisted")
		}
		if err := s.manager.stopUnattachedRuntime(ctx, s.runtime.Fence); err != nil {
			return errors.New("unarmed account Task could not be safely retired")
		}
		s.mu.Lock()
		delete(s.tasks, task.fence)
		s.mu.Unlock()
		s.fail(errors.New("account Agent Runtime was stopped after an unarmed Task failed to start"))
		return nil
	}
	attestation, err := s.manager.options.Guardian.StopTaskProcess(ctx, *guard)
	if err != nil {
		attestation, err = s.manager.options.Guardian.RecoverTaskProcessStopped(ctx, guard.RuntimeFence, guard.TaskFence, guard.NonceSHA256)
	}
	if err != nil {
		return errors.New("guardian task process stop is unconfirmed")
	}
	if _, err := s.manager.options.TaskStopper.ConfirmTaskStopped(ctx, attestation); err != nil {
		return errors.New("guardian task process stop proof was not persisted")
	}
	if err := s.manager.acknowledgeTaskStopProof(ctx, attestation); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.tasks, task.fence)
	if !task.terminal {
		s.stopped[cloudDSHIdentity{TaskID: task.fence.TaskID, OwnerID: task.fence.OwnerID,
			AgentID: task.fence.AgentInstanceID, SessionID: task.fence.AgentSessionID,
			Generation: task.fence.AgentGeneration, AuthorizationVersion: task.fence.AuthorizationVersion}] = struct{}{}
	}
	stopRequested := len(s.tasks) == 0
	s.mu.Unlock()
	if stopRequested || s.closed() {
		// A crashed shared Runtime recovers after lease expiry; RequestStop would cancel every owner Task.
		current, found, getErr := s.manager.options.Control.Get(ctx, s.runtime.Fence.OwnerID)
		if getErr == nil && found && current.State != app.AccountAgentRuntimeStopped && current.State == app.AccountAgentRuntimeStopping {
			return s.manager.stopUnattachedRuntime(ctx, s.runtime.Fence)
		}
	}
	return nil
}

type cloudDSHAccountTask struct {
	ctx         context.Context
	fence       app.AgentInstanceFence
	sequence    int64
	nonceSHA256 string
	expiresAt   time.Time
	mu          sync.Mutex
	guard       *dshguardian.TaskProcessGuard
	events      chan cloudDSHRead
	started     chan error
	terminal    bool // Protected by the account Runtime session mutex.
	failure     error
}

func (t *cloudDSHAccountTask) fail(err error) {
	t.mu.Lock()
	if t.failure == nil {
		t.failure = err
		select {
		case t.events <- cloudDSHRead{err: err}:
		default:
		}
		select {
		case t.started <- err:
		default:
		}
	}
	t.mu.Unlock()
}

type cloudDSHAccountTaskPipes struct {
	session *cloudDSHAccountRuntimeSession
	task    *cloudDSHAccountTask
	ctx     context.Context
	mu      sync.Mutex
	buffer  []byte
	readMu  sync.Mutex
	pending []byte
}

func (p *cloudDSHAccountTaskPipes) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffer = append(p.buffer, data...)
	for index := bytes.IndexByte(p.buffer, '\n'); index >= 0; index = bytes.IndexByte(p.buffer, '\n') {
		frame := append([]byte(nil), p.buffer[:index]...)
		p.buffer = p.buffer[index+1:]
		if err := p.send(frame); err != nil {
			return len(data), err
		}
	}
	return len(data), nil
}

func (p *cloudDSHAccountTaskPipes) send(raw []byte) error {
	if !uniqueCloudDSHJSON(raw) {
		return errors.New("account Task frame is invalid")
	}
	var frame map[string]any
	if json.Unmarshal(raw, &frame) != nil || frame == nil {
		return errors.New("account Task frame is invalid")
	}
	var identity cloudDSHIdentity
	if json.Unmarshal(raw, &identity) != nil || !cloudDSHIdentityValid(identity) ||
		identity.TaskID != p.task.fence.TaskID || identity.OwnerID != p.task.fence.OwnerID ||
		identity.AgentID != p.task.fence.AgentInstanceID || identity.Generation != p.task.fence.AgentGeneration ||
		identity.AuthorizationVersion != p.task.fence.AuthorizationVersion {
		return errors.New("account Task frame fence is invalid")
	}
	if identity.SessionID == p.task.fence.AgentSessionID && frame["type"] == "start" {
		frame["type"] = "task_start"
		frame["server_instance_id"] = p.session.runtime.Fence.ServiceInstanceID
		frame["runtime_id"] = p.session.runtime.Fence.RuntimeID
		frame["runtime_generation"] = p.session.runtime.Fence.RuntimeGeneration
		frame["runtime_sequence"] = p.task.sequence
		if err := p.session.input.WriteFrame(p.ctx, frame); err != nil {
			return err
		}
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case err := <-p.task.started:
			return err
		}
	}
	frame["server_instance_id"] = p.session.runtime.Fence.ServiceInstanceID
	frame["runtime_id"] = p.session.runtime.Fence.RuntimeID
	frame["runtime_generation"] = p.session.runtime.Fence.RuntimeGeneration
	return p.session.input.WriteFrame(p.ctx, frame)
}

func (p *cloudDSHAccountTaskPipes) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	p.readMu.Lock()
	defer p.readMu.Unlock()
	for len(p.pending) == 0 {
		select {
		case <-p.ctx.Done():
			return 0, p.ctx.Err()
		case item := <-p.task.events:
			if item.err != nil {
				return 0, item.err
			}
			p.pending = append(append(p.pending[:0], item.frame...), '\n')
		}
	}
	n := copy(data, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *cloudDSHAccountTaskPipes) Close() error { return nil }

type cloudDSHRuntimeInput struct {
	mu     sync.Mutex
	target io.WriteCloser
}

func (w *cloudDSHRuntimeInput) WriteFrame(ctx context.Context, frame any) error {
	return writeCloudDSHFrame(ctx, w, frame)
}

func (w *cloudDSHRuntimeInput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.target.Write(data)
}

func (w *cloudDSHRuntimeInput) Close() error { return nil }

func (m *CloudDSHAccountRuntimeManager) RenewTask(ctx context.Context, session *cloudDSHAccountRuntimeSession, fence app.AgentInstanceFence, expires time.Time) error {
	session.mu.Lock()
	task := session.tasks[fence]
	session.mu.Unlock()
	if task == nil {
		return errors.New("account Agent Runtime Task is not active")
	}
	return session.renewTask(ctx, task, expires)
}

func (m *CloudDSHAccountRuntimeManager) StopTask(ctx context.Context, session *cloudDSHAccountRuntimeSession, fence app.AgentInstanceFence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session.mu.Lock()
	task := session.tasks[fence]
	session.mu.Unlock()
	if task == nil {
		return nil
	}
	return session.stopTask(ctx, task)
}

func randRead(value []byte) (int, error) { return rand.Read(value) }
