package dshguardian

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/app"
)

func (s *Service) ArmAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("account Runtime arm context is unavailable")
	}
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	if err := s.processes.Ready(); err != nil {
		return fmt.Errorf("independent process-tree verification is unavailable: %w", err)
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	if existing, ok := s.store.getRuntime(guard.ContainerID); ok {
		if existing.Guard.sameIdentity(guard) && existing.Guard.ExpiresAt.Equal(guard.ExpiresAt) && !existing.Destroyed {
			return nil
		}
		return errors.New("account Runtime lease identity already exists")
	}
	for _, existing := range s.store.allRuntimes() {
		if existing.Guard.Fence == guard.Fence {
			return errors.New("account Runtime fence already has a different container lease")
		}
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect account Runtime before arming: %w", err)
	}
	if err := validateAccountRuntimeContainer(inspected, guard); err != nil {
		return err
	}
	if inspected.State.Running == nil || inspected.State.PID == nil || *inspected.State.Running || *inspected.State.PID != 0 {
		return errors.New("guardian must arm an unstarted account Runtime container")
	}
	return s.store.createRuntime(accountRuntimeLease{Guard: guard})
}

func (s *Service) StartedAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("account Runtime start context is unavailable")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupAccountRuntime(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) {
		return errors.New("account Runtime lease is expired or destroyed")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect started account Runtime: %w", err)
	}
	if err := validateAccountRuntimeContainer(inspected, guard); err != nil {
		return err
	}
	if inspected.State.Running == nil || inspected.State.PID == nil || !*inspected.State.Running || *inspected.State.PID < 2 {
		return errors.New("account Runtime is not running")
	}
	proof, err := s.processes.Capture(guard.ContainerID, *inspected.State.PID)
	if err != nil {
		return fmt.Errorf("capture independent account Runtime process evidence: %w", err)
	}
	value.Process = proof
	value.Started = true
	value.UnprovenRunning = false
	if err := s.store.replaceRuntime(value); err != nil {
		return fmt.Errorf("persist account Runtime process evidence: %w", err)
	}
	return nil
}

func (s *Service) RenewAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	if ctx == nil || ctx.Err() != nil || guard.validateNew(s.now()) != nil {
		return errors.New("account Runtime renewal identity or context is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	return s.renewAccountRuntimeLocked(ctx, guard)
}

func (s *Service) LookupAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (AccountRuntimeGuard, error) {
	if ctx == nil || ctx.Err() != nil || fence.Validate() != nil || !nonceHashRE.MatchString(nonceSHA256) {
		return AccountRuntimeGuard{}, errors.New("account Runtime lookup identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, ok, err := s.store.findRuntime(fence, nonceSHA256)
	if err != nil || !ok {
		return AccountRuntimeGuard{}, errors.New("guardian lease for the exact account Runtime fence is unknown")
	}
	if _, err := s.activeAccountRuntime(ctx, fence, value.Guard.ContainerID); err != nil {
		return AccountRuntimeGuard{}, err
	}
	return value.Guard, nil
}

func (s *Service) RenewAccountRuntimeByIdentity(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string, expiresAt time.Time) error {
	if ctx == nil || ctx.Err() != nil || fence.Validate() != nil || !nonceHashRE.MatchString(nonceSHA256) {
		return errors.New("account Runtime renewal identity or context is invalid")
	}
	guard := AccountRuntimeGuard{Fence: fence, NonceSHA256: nonceSHA256, ExpiresAt: expiresAt}
	remaining := expiresAt.Sub(s.now())
	if remaining <= 0 || remaining > app.MaxAgentLease {
		return errors.New("account Runtime lease expiry is outside the allowed window")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, ok, err := s.store.findRuntime(fence, nonceSHA256)
	if err != nil || !ok {
		return errors.New("guardian lease for the exact account Runtime fence is unknown")
	}
	guard.ContainerID = value.Guard.ContainerID
	guard.ContainerName = value.Guard.ContainerName
	return s.renewAccountRuntimeLocked(ctx, guard)
}

func (s *Service) renewAccountRuntimeLocked(ctx context.Context, guard AccountRuntimeGuard) error {
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	value, err := s.lookupAccountRuntimeIdentity(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) || guard.ExpiresAt.Before(value.Guard.ExpiresAt) {
		return errors.New("account Runtime lease cannot be renewed")
	}
	if !value.Started || value.UnprovenRunning {
		return errors.New("account Runtime process identity is unavailable for renewal")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect account Runtime before renewal: %w", err)
	}
	if err := validateAccountRuntimeContainer(inspected, guard); err != nil {
		return err
	}
	if inspected.State.Running == nil || inspected.State.PID == nil || !*inspected.State.Running || *inspected.State.PID != value.Process.PID {
		return errors.New("account Runtime process changed before renewal")
	}
	proof, err := s.processes.Capture(guard.ContainerID, value.Process.PID)
	if err != nil || proof != value.Process {
		return errors.New("account Runtime host process identity changed before renewal")
	}
	value.Guard = guard
	if err := s.store.replaceRuntime(value); err != nil {
		return fmt.Errorf("persist account Runtime lease renewal: %w", err)
	}
	return nil
}

func (s *Service) StopAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return s.accountRuntimeStopAttestation(ctx, fence, nonceSHA256)
}

func (s *Service) RecoverAccountRuntimeStopped(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return s.accountRuntimeStopAttestation(ctx, fence, nonceSHA256)
}

func (s *Service) AcknowledgeAccountRuntimeStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	nonceSHA256, proofSHA256 string) error {
	if ctx == nil || ctx.Err() != nil || runtimeFence.Validate() != nil ||
		!nonceHashRE.MatchString(nonceSHA256) || !nonceHashRE.MatchString(proofSHA256) {
		return errors.New("account Runtime proof acknowledgement identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	return s.store.acknowledgeAccountRuntimeProof(runtimeFence, nonceSHA256, proofSHA256)
}

func (s *Service) accountRuntimeStopAttestation(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	if ctx == nil || ctx.Err() != nil || fence.Validate() != nil || !nonceHashRE.MatchString(nonceSHA256) {
		return app.AccountAgentRuntimeStopAttestation{}, errors.New("account Runtime stop identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, ok, err := s.store.findRuntime(fence, nonceSHA256)
	if err != nil || !ok {
		return app.AccountAgentRuntimeStopAttestation{}, errors.New("guardian lease for the exact account Runtime fence is unknown")
	}
	// A whole-container proof remains valid if one Task proof cannot be
	// recovered; that Task lease retains its own explicit uncertainty marker.
	_ = s.stopRuntimeTasks(ctx, value)
	if err := s.destroyAccountRuntime(ctx, &value); err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, fmt.Errorf("guardian could not verify account Runtime destruction: %w", err)
	}
	current, ok := s.store.getRuntime(value.Guard.ContainerID)
	if !ok || current.Guard != value.Guard || !current.Destroyed || current.DestroyedAt.IsZero() || current.UnprovenRunning {
		return app.AccountAgentRuntimeStopAttestation{}, errors.New("account Runtime destruction proof was not durably recorded")
	}
	proofBytes, err := json.Marshal(current)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, fmt.Errorf("encode durable account Runtime proof: %w", err)
	}
	proofDigest := sha256.Sum256(proofBytes)
	attestation := app.AccountAgentRuntimeStopAttestation{
		Fence: current.Guard.Fence, ContainerID: current.Guard.ContainerID, ContainerName: current.Guard.ContainerName,
		NonceSHA256: current.Guard.NonceSHA256, ExpiresAt: current.Guard.ExpiresAt, DestroyedAt: current.DestroyedAt,
		ProofSHA256: hex.EncodeToString(proofDigest[:]), Destroyed: true,
	}
	payload, err := app.AccountAgentRuntimeStopAttestationSigningPayload(attestation)
	if err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	attestation.Signature = ed25519.Sign(s.signingKey, payload)
	return attestation, nil
}

func (s *Service) ArmTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("task process arm context is unavailable")
	}
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	processes, ok := s.processes.(taskProcessInspector)
	if !ok {
		return errors.New("independent task cgroup isolation is unavailable")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	runtimeLease, err := s.activeAccountRuntime(ctx, guard.RuntimeFence, guard.RuntimeContainerID)
	if err != nil {
		return err
	}
	for _, existing := range s.store.allTasks() {
		if existing.Guard.TaskFence == guard.TaskFence && existing.Key != taskProcessKey(guard) {
			return errors.New("AgentInstance fence already has a different Task process lease")
		}
	}
	if existing, exists := s.store.getTask(taskProcessKey(guard)); exists {
		if !existing.Guard.sameIdentity(guard) || existing.Guard.ExpiresAt.After(guard.ExpiresAt) || existing.Destroyed {
			return errors.New("task process lease identity already exists")
		}
		if existing.Guard.ExpiresAt.Equal(guard.ExpiresAt) && existing.Armed {
			proof, err := processes.AttachTaskProcess(guard.RuntimeContainerID, runtimeLease.Process, guard.ContainerPID, existing.Key)
			if err != nil || proof != existing.Process {
				return errors.New("task process host identity changed after arming")
			}
			return nil
		}
	}
	key := taskProcessKey(guard)
	value, exists := s.store.getTask(key)
	if !exists {
		value = taskProcessLease{Guard: guard, Key: key}
		if err := s.store.createTask(value); err != nil {
			return fmt.Errorf("persist Task process lease before cgroup attachment: %w", err)
		}
	} else {
		value.Guard = guard
		if err := s.store.replaceTask(value); err != nil {
			return fmt.Errorf("persist Task process lease renewal before cgroup attachment: %w", err)
		}
	}
	proof, err := processes.AttachTaskProcess(guard.RuntimeContainerID, runtimeLease.Process, guard.ContainerPID, key)
	if err != nil {
		return fmt.Errorf("resolve and attach exact Task process: %w", err)
	}
	value.Process = proof
	value.Armed = true
	value.UnprovenRunning = false
	if err := s.store.replaceTask(value); err != nil {
		return fmt.Errorf("persist exact Task process identity: %w", err)
	}
	return nil
}

func (s *Service) StartedTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("task process start context is unavailable")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupTaskProcess(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) || !value.Armed {
		return errors.New("task process lease is expired, unarmed, or destroyed")
	}
	runtimeLease, err := s.activeAccountRuntime(ctx, guard.RuntimeFence, guard.RuntimeContainerID)
	if err != nil {
		return err
	}
	processes, ok := s.processes.(taskProcessInspector)
	if !ok {
		return errors.New("independent task cgroup isolation is unavailable")
	}
	proof, err := processes.AttachTaskProcess(guard.RuntimeContainerID, runtimeLease.Process, guard.ContainerPID, value.Key)
	if err != nil || proof != value.Process {
		return errors.New("task process host identity changed after arming")
	}
	value.Started = true
	if err := s.store.replaceTask(value); err != nil {
		return fmt.Errorf("persist Task process start state: %w", err)
	}
	return nil
}

func (s *Service) RenewTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	if ctx == nil || ctx.Err() != nil || guard.validateNew(s.now()) != nil {
		return errors.New("task process renewal identity or context is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupTaskProcessIdentity(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) || guard.ExpiresAt.Before(value.Guard.ExpiresAt) {
		return errors.New("task process lease cannot be renewed")
	}
	runtimeLease, err := s.activeAccountRuntime(ctx, guard.RuntimeFence, guard.RuntimeContainerID)
	if err != nil {
		return err
	}
	processes, ok := s.processes.(taskProcessInspector)
	if !ok {
		return errors.New("independent task cgroup isolation is unavailable")
	}
	proof, err := processes.AttachTaskProcess(guard.RuntimeContainerID, runtimeLease.Process, guard.ContainerPID, value.Key)
	if err != nil || proof != value.Process {
		return errors.New("task process host identity changed before renewal")
	}
	value.Guard = guard
	if err := s.store.replaceTask(value); err != nil {
		return fmt.Errorf("persist Task process lease renewal: %w", err)
	}
	return nil
}

func (s *Service) StopTaskProcess(ctx context.Context, guard TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	if ctx == nil || ctx.Err() != nil {
		return app.AccountAgentTaskStopAttestation{}, errors.New("task process stop context is unavailable")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupTaskProcess(guard)
	if err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	if err := s.destroyTaskProcess(ctx, &value); err != nil {
		return app.AccountAgentTaskStopAttestation{}, fmt.Errorf("guardian could not verify Task cgroup destruction: %w", err)
	}
	return s.signTaskProcessStop(value)
}

func (s *Service) RecoverTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence, taskFence app.AgentInstanceFence, nonceSHA256 string) (app.AccountAgentTaskStopAttestation, error) {
	if ctx == nil || ctx.Err() != nil || runtimeFence.Validate() != nil || taskFence.Validate() != nil || !nonceHashRE.MatchString(nonceSHA256) {
		return app.AccountAgentTaskStopAttestation{}, errors.New("task process recovery identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, ok, err := s.store.findTask(runtimeFence, taskFence, nonceSHA256)
	if err != nil || !ok {
		return app.AccountAgentTaskStopAttestation{}, errors.New("guardian lease for the exact account Runtime and Task fences is unknown")
	}
	if err := s.destroyTaskProcess(ctx, &value); err != nil {
		return app.AccountAgentTaskStopAttestation{}, fmt.Errorf("guardian could not verify recovered Task cgroup destruction: %w", err)
	}
	return s.signTaskProcessStop(value)
}

func (s *Service) AcknowledgeTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256, proofSHA256 string) error {
	if ctx == nil || ctx.Err() != nil || runtimeFence.Validate() != nil || taskFence.Validate() != nil ||
		runtimeFence.OwnerID != taskFence.OwnerID || !nonceHashRE.MatchString(nonceSHA256) || !nonceHashRE.MatchString(proofSHA256) {
		return errors.New("task process proof acknowledgement identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	return s.store.acknowledgeTaskProcessProof(runtimeFence, taskFence, nonceSHA256, proofSHA256)
}

func (s *Service) signTaskProcessStop(value taskProcessLease) (app.AccountAgentTaskStopAttestation, error) {
	current, ok := s.store.getTask(value.Key)
	if !ok || current.Guard != value.Guard || !current.Destroyed || current.DestroyedAt.IsZero() || current.UnprovenRunning {
		return app.AccountAgentTaskStopAttestation{}, errors.New("task cgroup destruction proof was not durably recorded")
	}
	proofDigest, err := taskProcessLeaseProofDigest(current)
	if err != nil {
		return app.AccountAgentTaskStopAttestation{}, fmt.Errorf("encode durable Task process proof: %w", err)
	}
	attestation := app.AccountAgentTaskStopAttestation{
		RuntimeFence: current.Guard.RuntimeFence, TaskFence: current.Guard.TaskFence, RuntimeContainerID: current.Guard.RuntimeContainerID,
		ContainerPID: current.Process.ContainerPID, HostPID: current.Process.HostPID, StartTimeTick: current.Process.StartTimeTick,
		CgroupPath: current.Process.CgroupPath, NonceSHA256: current.Guard.NonceSHA256, ExpiresAt: current.Guard.ExpiresAt,
		DestroyedAt: current.DestroyedAt, ProofSHA256: proofDigest, Destroyed: true,
	}
	payload, err := app.AccountAgentTaskStopAttestationSigningPayload(attestation)
	if err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	attestation.Signature = ed25519.Sign(s.signingKey, payload)
	return attestation, nil
}

func taskProcessLeaseProofDigest(value taskProcessLease) (string, error) {
	proofBytes, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	proofDigest := sha256.Sum256(proofBytes)
	return hex.EncodeToString(proofDigest[:]), nil
}

func (s *Service) destroyTaskProcess(ctx context.Context, value *taskProcessLease) error {
	if value.Destroyed {
		return nil
	}
	if !value.Armed || value.Process.HostPID < 2 {
		return errors.New("task process was never independently armed")
	}
	runtimeLease, err := s.runtimeForTask(value.Guard)
	if err != nil {
		return err
	}
	processes, ok := s.processes.(taskProcessInspector)
	if !ok {
		return errors.New("independent task cgroup isolation is unavailable")
	}
	if err := processes.KillTaskProcess(value.Guard.RuntimeContainerID, runtimeLease.Process, value.Process, value.Key); err != nil {
		value.UnprovenRunning = true
		_ = s.store.replaceTask(*value)
		return err
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		gone, err := processes.TaskProcessGone(value.Guard.RuntimeContainerID, runtimeLease.Process, value.Process, value.Key)
		if err != nil {
			value.UnprovenRunning = true
			_ = s.store.replaceTask(*value)
			return err
		}
		if gone {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("task cgroup remained populated after cgroup.kill")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := processes.RemoveTaskCgroup(value.Guard.RuntimeContainerID, runtimeLease.Process, value.Process, value.Key); err != nil {
		value.UnprovenRunning = true
		_ = s.store.replaceTask(*value)
		return err
	}
	gone, err := processes.TaskProcessGone(value.Guard.RuntimeContainerID, runtimeLease.Process, value.Process, value.Key)
	if err != nil || !gone {
		if err == nil {
			err = errors.New("task process or cgroup identity remains after removal")
		}
		value.UnprovenRunning = true
		_ = s.store.replaceTask(*value)
		return err
	}
	value.UnprovenRunning = false
	value.Destroyed = true
	if value.DestroyedAt.IsZero() {
		value.DestroyedAt = s.now().UTC()
	}
	if err := s.store.replaceTask(*value); err != nil {
		return fmt.Errorf("persist Task cgroup destruction proof: %w", err)
	}
	return nil
}

func (s *Service) stopRuntimeTasks(ctx context.Context, runtimeValue accountRuntimeLease) error {
	var firstErr error
	for _, value := range s.store.allTasks() {
		if value.Guard.RuntimeFence != runtimeValue.Guard.Fence || value.Guard.RuntimeContainerID != runtimeValue.Guard.ContainerID || value.Destroyed {
			continue
		}
		current, ok := s.store.getTask(value.Key)
		if !ok || current.Destroyed {
			continue
		}
		if err := s.destroyTaskProcess(ctx, &current); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) destroyAccountRuntime(ctx context.Context, value *accountRuntimeLease) error {
	inspected, err := s.docker.inspect(ctx, value.Guard.ContainerID)
	switch {
	case errors.Is(err, errContainerNotFound):
		if value.Started {
			if err := s.processes.KillTree(value.Guard.ContainerID, value.Process); err != nil {
				value.UnprovenRunning = true
				if persistErr := s.store.replaceRuntime(*value); persistErr != nil {
					return fmt.Errorf("persist unproven account Runtime process tree: %w", persistErr)
				}
			} else if value.UnprovenRunning {
				value.UnprovenRunning = false
				if err := s.store.replaceRuntime(*value); err != nil {
					return fmt.Errorf("persist recovered account Runtime process evidence: %w", err)
				}
			}
		}
	case err != nil:
		return fmt.Errorf("inspect guarded account Runtime: %w", err)
	default:
		if err := validateAccountRuntimeContainer(inspected, value.Guard); err != nil {
			return err
		}
		if inspected.State.Running == nil || inspected.State.PID == nil {
			return errors.New("docker inspection omitted account Runtime process state")
		}
		if *inspected.State.Running {
			if *inspected.State.PID < 2 {
				return errors.New("running account Runtime has no host PID")
			}
			if !value.Started || value.Process.PID != *inspected.State.PID || value.UnprovenRunning {
				proof, captureErr := s.processes.Capture(value.Guard.ContainerID, *inspected.State.PID)
				if captureErr == nil && value.Started && proof.CgroupPath != value.Process.CgroupPath {
					captureErr = errors.New("account Runtime container moved to a different cgroup")
				}
				if captureErr != nil {
					value.UnprovenRunning = true
					if persistErr := s.store.replaceRuntime(*value); persistErr != nil {
						return fmt.Errorf("persist unproven account Runtime process state: %w", persistErr)
					}
				} else {
					value.Process = proof
					value.Started = true
					value.UnprovenRunning = false
					if err := s.store.replaceRuntime(*value); err != nil {
						return fmt.Errorf("persist expired account Runtime process evidence: %w", err)
					}
				}
			}
		}
		if value.Started && !value.UnprovenRunning {
			if err := s.processes.KillTree(value.Guard.ContainerID, value.Process); err != nil {
				value.UnprovenRunning = true
				if persistErr := s.store.replaceRuntime(*value); persistErr != nil {
					return fmt.Errorf("persist unproven account Runtime process tree: %w", persistErr)
				}
			}
		}
		if *inspected.State.Running {
			if err := s.docker.kill(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errDockerConflict) {
				return fmt.Errorf("kill account Runtime container: %w", err)
			}
			if err := s.docker.wait(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
				return fmt.Errorf("wait for account Runtime process exit: %w", err)
			}
		}
		if err := s.docker.remove(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
			return fmt.Errorf("remove account Runtime container: %w", err)
		}
	}
	remaining, err := s.docker.inspect(ctx, value.Guard.ContainerID)
	if err == nil {
		if err := validateAccountRuntimeContainer(remaining, value.Guard); err != nil {
			return err
		}
		return errors.New("docker still reports the guarded account Runtime")
	}
	if !errors.Is(err, errContainerNotFound) {
		return fmt.Errorf("confirm account Runtime container absence: %w", err)
	}
	if value.UnprovenRunning {
		return errors.New("account Runtime was stopped without sufficient process identity evidence")
	}
	gone, err := s.processes.Gone(value.Guard.ContainerID, value.Process)
	if err != nil {
		return fmt.Errorf("verify account Runtime host process tree: %w", err)
	}
	if !gone {
		return errors.New("account Runtime cgroup or host process is still present")
	}
	value.Destroyed = true
	if value.DestroyedAt.IsZero() {
		value.DestroyedAt = s.now().UTC()
	}
	if err := s.store.replaceRuntime(*value); err != nil {
		return fmt.Errorf("persist account Runtime destruction proof: %w", err)
	}
	return nil
}

func (s *Service) activeAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, containerID string) (accountRuntimeLease, error) {
	value, ok := s.store.getRuntime(containerID)
	if !ok || value.Guard.Fence != fence || !value.Started || value.Destroyed || value.UnprovenRunning || !value.Guard.ExpiresAt.After(s.now()) {
		return accountRuntimeLease{}, errors.New("active account Runtime lease is unavailable")
	}
	inspected, err := s.docker.inspect(ctx, containerID)
	if err != nil || validateAccountRuntimeContainer(inspected, value.Guard) != nil || inspected.State.Running == nil || !*inspected.State.Running || inspected.State.PID == nil || *inspected.State.PID != value.Process.PID {
		return accountRuntimeLease{}, errors.New("account Runtime container identity is unavailable")
	}
	proof, err := s.processes.Capture(containerID, value.Process.PID)
	if err != nil || proof != value.Process {
		return accountRuntimeLease{}, errors.New("account Runtime host process identity changed")
	}
	return value, nil
}

func (s *Service) runtimeForTask(guard TaskProcessGuard) (accountRuntimeLease, error) {
	value, ok := s.store.getRuntime(guard.RuntimeContainerID)
	if !ok || value.Guard.Fence != guard.RuntimeFence || value.Destroyed || value.UnprovenRunning {
		return accountRuntimeLease{}, errors.New("task process account Runtime lease is unavailable")
	}
	return value, nil
}

func (s *Service) lookupAccountRuntime(guard AccountRuntimeGuard) (accountRuntimeLease, error) {
	value, err := s.lookupAccountRuntimeIdentity(guard)
	if err != nil || !value.Guard.ExpiresAt.Equal(guard.ExpiresAt) {
		return accountRuntimeLease{}, errors.New("account Runtime lease is unknown or stale")
	}
	return value, nil
}

func (s *Service) lookupAccountRuntimeIdentity(guard AccountRuntimeGuard) (accountRuntimeLease, error) {
	value, ok := s.store.getRuntime(guard.ContainerID)
	if !ok || !value.Guard.sameIdentity(guard) {
		return accountRuntimeLease{}, errors.New("account Runtime lease identity is unknown")
	}
	return value, nil
}

func (s *Service) lookupTaskProcess(guard TaskProcessGuard) (taskProcessLease, error) {
	value, err := s.lookupTaskProcessIdentity(guard)
	if err != nil || !value.Guard.ExpiresAt.Equal(guard.ExpiresAt) {
		return taskProcessLease{}, errors.New("task process lease is unknown or stale")
	}
	return value, nil
}

func (s *Service) lookupTaskProcessIdentity(guard TaskProcessGuard) (taskProcessLease, error) {
	value, ok := s.store.getTask(taskProcessKey(guard))
	if !ok || !value.Guard.sameIdentity(guard) {
		return taskProcessLease{}, errors.New("task process lease identity is unknown")
	}
	return value, nil
}

func validateAccountRuntimeContainer(inspected containerInspect, guard AccountRuntimeGuard) error {
	encodedFence, err := json.Marshal(guard.Fence)
	if err != nil {
		return errors.New("cannot encode account Runtime fence")
	}
	if inspected.State.Running == nil || inspected.State.PID == nil || inspected.ID != guard.ContainerID || inspected.Name != "/"+guard.ContainerName ||
		inspected.Config.Labels["morde.account-agent-runtime.fence"] != string(encodedFence) ||
		inspected.Config.Labels["morde.account-agent-runtime.nonce-sha256"] != guard.NonceSHA256 {
		return errors.New("docker container ID, name, or account Runtime labels do not match the lease")
	}
	return nil
}
