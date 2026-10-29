package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

type f01NodeAccountSupervisor struct {
	accountRuntimeSupervisorStub
	node   string
	script string
	cmd    *exec.Cmd
}

type f01RecoveringNodeAccountSupervisor struct {
	*f01NodeAccountSupervisor
	recovery *signedRuntimeRecoverySupervisor
}

func (s *f01RecoveringNodeAccountSupervisor) RecoverAccountRuntimeStopped(ctx context.Context,
	fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return s.recovery.RecoverAccountRuntimeStopped(ctx, fence, nonceSHA256)
}

func (s *f01NodeAccountSupervisor) StartAccountRuntime(_ context.Context, _ app.AccountAgentRuntimeFence,
	_ []byte, _ time.Time) (CloudDSHAccountRuntimeProcess, error) {
	command := exec.Command(s.node, s.script)
	command.Dir = filepath.Dir(s.script)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"}
	input, err := command.StdinPipe()
	if err != nil {
		return CloudDSHAccountRuntimeProcess{}, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return CloudDSHAccountRuntimeProcess{}, err
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return CloudDSHAccountRuntimeProcess{}, err
	}
	s.cmd = command
	return CloudDSHAccountRuntimeProcess{ContainerID: strings.Repeat("a", 64), ContainerName: "f01-node-account",
		Input: input, Output: output}, nil
}

type f01NodeTaskGuardian struct{ accountRuntimeGuardianStub }

type f01SignedNodeTaskGuardian struct {
	f01NodeTaskGuardian
	key ed25519.PrivateKey
}

func (g *f01NodeTaskGuardian) ArmTaskProcess(_ context.Context, guard dshguardian.TaskProcessGuard) error {
	if guard.ContainerPID < 2 || guard.RuntimeContainerID != strings.Repeat("a", 64) {
		return errors.New("Node Task process identity is invalid")
	}
	return nil
}

func (g *f01SignedNodeTaskGuardian) StopTaskProcess(ctx context.Context,
	guard dshguardian.TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	attestation, err := g.f01NodeTaskGuardian.StopTaskProcess(ctx, guard)
	if err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	payload, err := app.AccountAgentTaskStopAttestationSigningPayload(attestation)
	if err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	attestation.Signature = ed25519.Sign(g.key, payload)
	return attestation, nil
}

func TestAccountRuntimePostgresAndRealNodeContinueAfterFirstTaskContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &f01NodeAccountSupervisor{node: node, script: script}
	manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
		Control: control, Supervisor: supervisor, Guardian: &f01NodeTaskGuardian{},
		StopVerifier: accountRuntimeStopVerifierStub{}, TaskStopper: &accountRuntimeTaskStopperStub{},
		LeaseOwner: "f01-node-runtime", MaxTasks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithTimeout(ctx, 10*time.Second)
	session, err := manager.Acquire(firstCtx, owner.ID)
	if err != nil {
		cancelFirst()
		t.Fatal(err)
	}
	defer func() {
		_ = session.process.Input.Close()
		_ = session.process.Output.Close()
		_ = supervisor.cmd.Process.Kill()
		_ = supervisor.cmd.Wait()
	}()
	f01RunNodeAccountTask(t, firstCtx, manager, session, owner.ID, 1)
	cancelFirst()
	secondCtx, cancelSecond := context.WithTimeout(ctx, 10*time.Second)
	defer cancelSecond()
	reused, err := manager.Acquire(secondCtx, owner.ID)
	if err != nil || reused != session {
		t.Fatalf("second Task did not reuse the account Runtime: reused=%t err=%v", reused == session, err)
	}
	f01RunNodeAccountTask(t, secondCtx, manager, session, owner.ID, 2)
	current, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || current.State != app.AccountAgentRuntimeRunning || current.Fence != session.runtime.Fence || session.closed() {
		t.Fatalf("account Runtime did not remain running after sequential Tasks: runtime=%+v found=%t closed=%t err=%v",
			current, found, session.closed(), err)
	}
}

func TestAccountRuntimePostgresAndRealNodeRecoverAfterManagerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin-account.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	guardian := &f01NodeTaskGuardian{}
	makeSupervisor := func() *f01RecoveringNodeAccountSupervisor {
		return &f01RecoveringNodeAccountSupervisor{
			f01NodeAccountSupervisor: &f01NodeAccountSupervisor{node: node, script: script},
			recovery:                 &signedRuntimeRecoverySupervisor{accountRuntimeSupervisorStub: &accountRuntimeSupervisorStub{}, key: privateKey},
		}
	}
	makeManager := func(supervisor CloudDSHAccountRuntimeSupervisor, leaseOwner string) *CloudDSHAccountRuntimeManager {
		t.Helper()
		manager, err := NewCloudDSHAccountRuntimeManager(CloudDSHAccountRuntimeManagerOptions{
			Control: control, Supervisor: supervisor, Guardian: guardian, StopVerifier: verifier,
			TaskStopper: &accountRuntimeTaskStopperStub{}, LeaseOwner: leaseOwner, MaxTasks: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	oldSupervisor := makeSupervisor()
	defer func() {
		if command := oldSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	oldManager := makeManager(oldSupervisor, "f01-node-runtime-old")
	oldSession, err := oldManager.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldFence := oldSession.runtime.Fence
	if err := oldSupervisor.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("simulate Portal Runtime process loss: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- oldSupervisor.cmd.Wait() }()
	select {
	case waitErr := <-waitDone:
		if waitErr == nil || oldSupervisor.cmd.ProcessState == nil || oldSupervisor.cmd.ProcessState.ExitCode() != -1 {
			t.Fatalf("wait for old account Agent Runtime: %v", waitErr)
		}
		oldSupervisor.cmd = nil
	case <-ctx.Done():
		t.Fatalf("old Node account Runtime did not exit: %v", ctx.Err())
	}
	for deadline := time.Now().Add(time.Second); !oldSession.closed() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !oldSession.closed() {
		t.Fatal("old Manager did not observe the account Runtime process exit")
	}
	newSupervisor := makeSupervisor()
	defer func() {
		if command := newSupervisor.cmd; command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	newManager := makeManager(newSupervisor, "f01-node-runtime-new")
	if _, err := newManager.Acquire(ctx, owner.ID); err == nil {
		t.Fatal("new Manager replaced a Runtime before its lease expired")
	}
	if _, err := pool.Exec(ctx, `UPDATE account_agent_runtimes SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`, oldFence.ServiceInstanceID, owner.ID, oldFence.RuntimeID); err != nil {
		t.Fatal(err)
	}
	newSession, err := newManager.Acquire(ctx, owner.ID)
	if err != nil {
		t.Fatalf("new Manager failed to recover the stopped Runtime and start Node: %v", err)
	}
	defer func() { _ = newSession.process.Input.Close(); _ = newSession.process.Output.Close() }()
	current, found, err := control.Get(ctx, owner.ID)
	if err != nil || !found || current.State != app.AccountAgentRuntimeRunning ||
		current.Fence.RuntimeGeneration != oldFence.RuntimeGeneration+1 || current.Fence.RuntimeID == oldFence.RuntimeID {
		t.Fatalf("recovered account Runtime=%+v found=%t err=%v", current, found, err)
	}
	if _, err := control.Renew(ctx, oldFence, app.MaxAgentLease); err == nil {
		t.Fatal("old Runtime fence renewed after Manager recovery")
	}
	var oldRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM account_agent_runtimes WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3`,
		oldFence.ServiceInstanceID, owner.ID, oldFence.RuntimeID).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("old Runtime row after Guardian ACK count=%d err=%v", oldRows, err)
	}
	guardian.mu.Lock()
	proofAcks := append([]app.AccountAgentRuntimeStopAttestation(nil), guardian.runtimeProofAcks...)
	guardian.mu.Unlock()
	if len(proofAcks) != 1 || proofAcks[0].Fence != oldFence {
		t.Fatalf("old Runtime Guardian proof ACKs=%#v", proofAcks)
	}
	f01RunNodeAccountTask(t, ctx, newManager, newSession, owner.ID, 1)
}

func f01RunNodeAccountTask(t *testing.T, ctx context.Context, manager *CloudDSHAccountRuntimeManager,
	session *cloudDSHAccountRuntimeSession, ownerID string, sequence int64) {
	t.Helper()
	fence := app.AgentInstanceFence{TaskID: fmt.Sprintf("q%07d", sequence), OwnerID: ownerID,
		AgentInstanceID: fmt.Sprintf("agent-%d", sequence), AgentSessionID: fmt.Sprintf("session-%d", sequence),
		AgentGeneration: 1, SupervisorID: "f01-node-supervisor", LeaseOwner: "f01-node-task", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	lease := app.AgentInstanceLease{Fence: fence, RuntimeFence: &session.runtime.Fence, RuntimeSequence: sequence,
		ExpiresAt: time.Now().Add(app.MaxAgentLease)}
	f01RunNodeAccountTaskWithLease(t, ctx, manager, session, lease, bytes.Repeat([]byte{byte(sequence)}, 32))
}

func f01RunNodeAccountTaskWithLease(t *testing.T, ctx context.Context, manager *CloudDSHAccountRuntimeManager,
	session *cloudDSHAccountRuntimeSession, lease app.AgentInstanceLease, nonce []byte) {
	t.Helper()
	if err := f01RunNodeAccountTaskControlled(ctx, manager, session, lease, nonce, nil, nil); err != nil {
		t.Fatalf("run Task %d through real Node: %v", lease.RuntimeSequence, err)
	}
}

func f01RunNodeAccountTaskControlled(ctx context.Context, manager *CloudDSHAccountRuntimeManager,
	session *cloudDSHAccountRuntimeSession, lease app.AgentInstanceLease, nonce []byte,
	modelRequested chan<- struct{}, modelGate <-chan struct{}) error {
	fence := lease.Fence
	sequence := lease.RuntimeSequence
	input, output, err := session.StartTask(ctx, fence, nonce, lease)
	if err != nil {
		return fmt.Errorf("start account Task: %w", err)
	}
	defer func() { _ = input.Close(); _ = output.Close() }()
	reader := bufio.NewReader(output)
	var ready struct {
		Type string `json:"type"`
	}
	if err := f01ReadNodeAccountFrame(reader, &ready); err != nil {
		return fmt.Errorf("read Task readiness: %w", err)
	}
	if ready.Type != "ready" {
		return fmt.Errorf("Task readiness = %q", ready.Type)
	}
	identity := map[string]any{"task_id": fence.TaskID, "owner_id": fence.OwnerID, "agent_id": fence.AgentInstanceID,
		"session_id": fence.AgentSessionID, "generation": fence.AgentGeneration, "authorization_version": fence.AuthorizationVersion}
	start := map[string]any{"type": "start", "version": 1, "model": "f01-local-model", "tools": []any{}, "prompt": "Answer with one sentence."}
	for key, value := range identity {
		start[key] = value
	}
	if err := writeCloudDSHFrame(ctx, input, start); err != nil {
		return fmt.Errorf("send Task start: %w", err)
	}
	models := 0
	for {
		var frame struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			ThroughSeq int64  `json:"through_seq"`
			SHA256     string `json:"sha256"`
			State      string `json:"state"`
		}
		if err := f01ReadNodeAccountFrame(reader, &frame); err != nil {
			return fmt.Errorf("read Task frame: %w", err)
		}
		switch frame.Type {
		case "checkpoint":
			ack := map[string]any{"type": "checkpoint_ack", "id": frame.ID, "ok": true,
				"through_seq": frame.ThroughSeq, "sha256": frame.SHA256}
			for key, value := range identity {
				ack[key] = value
			}
			if err := writeCloudDSHFrame(ctx, input, ack); err != nil {
				return fmt.Errorf("acknowledge Task checkpoint: %w", err)
			}
		case "model":
			models++
			if modelRequested != nil {
				select {
				case modelRequested <- struct{}{}:
				default:
				}
			}
			if modelGate != nil {
				select {
				case <-modelGate:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			chunks := []any{
				map[string]any{"type": "block-start", "index": 0, "blockType": "text"},
				map[string]any{"type": "text-delta", "index": 0, "text": "Completed."},
				map[string]any{"type": "block-end", "index": 0, "block": map[string]any{"type": "text", "text": "Completed."}},
				map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": 2, "outputTokens": 1}},
				map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}},
			}
			for index, chunk := range chunks {
				response := map[string]any{"type": "model_chunk", "id": frame.ID, "index": index, "chunk": chunk}
				for key, value := range identity {
					response[key] = value
				}
				if err := writeCloudDSHFrame(ctx, input, response); err != nil {
					return fmt.Errorf("write model response chunk: %w", err)
				}
			}
			end := map[string]any{"type": "model_end", "id": frame.ID, "ok": true}
			for key, value := range identity {
				end[key] = value
			}
			if err := writeCloudDSHFrame(ctx, input, end); err != nil {
				return fmt.Errorf("finish model response: %w", err)
			}
		case "settled":
			if frame.State != "completed_candidate" || models != 1 {
				return fmt.Errorf("Task %d settled with state %q and %d model calls", sequence, frame.State, models)
			}
			if err := manager.StopTask(ctx, session, fence); err != nil {
				return fmt.Errorf("stop settled Task: %w", err)
			}
			return nil
		default:
			return fmt.Errorf("unexpected Task %d Node frame: %s", sequence, frame.Type)
		}
	}
}

func f01ReadNodeAccountFrame(reader *bufio.Reader, target any) error {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, target)
}
