package dshguardian

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/app"
)

const openSandboxContainerIDLabel = "opensandbox.io/id"

type TaskSandboxStopRequest struct {
	Guard TaskSandboxGuard `json:"guard"`
	Nonce []byte           `json:"nonce"`
}

// RegisterTaskSandbox records the exact running OpenSandbox container and
// independent process identity before the Task can use it.
func (s *Service) RegisterTaskSandbox(ctx context.Context, guard TaskSandboxGuard) error {
	if ctx == nil || ctx.Err() != nil || s == nil || s.taskSandboxSigningKey == nil {
		return errors.New("task Sandbox Guardian is unavailable")
	}
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	if err := s.processes.Ready(); err != nil {
		return fmt.Errorf("independent Task Sandbox process verification is unavailable: %w", err)
	}
	release, err := s.lockOperation(ctx, taskSandboxOperationKey(guard.LifecycleID))
	if err != nil {
		return err
	}
	defer release()
	return s.registerTaskSandboxLocked(ctx, guard)
}

// RegisterDiscoveredTaskSandbox resolves the container from its exact
// OpenSandbox identity label, rejects zero or ambiguous matches, then inspects
// and records that immutable container ID before returning it to the caller.
func (s *Service) RegisterDiscoveredTaskSandbox(ctx context.Context, request TaskSandboxGuard) (TaskSandboxGuard, error) {
	if ctx == nil || ctx.Err() != nil || s == nil || s.taskSandboxSigningKey == nil {
		return TaskSandboxGuard{}, errors.New("task Sandbox Guardian is unavailable")
	}
	if err := request.validateDiscoveryRequest(s.now()); err != nil {
		return TaskSandboxGuard{}, err
	}
	if err := s.processes.Ready(); err != nil {
		return TaskSandboxGuard{}, fmt.Errorf("independent Task Sandbox process verification is unavailable: %w", err)
	}
	release, err := s.lockOperation(ctx, taskSandboxOperationKey(request.LifecycleID))
	if err != nil {
		return TaskSandboxGuard{}, err
	}
	defer release()

	containers, err := s.docker.listByLabel(ctx, openSandboxContainerIDLabel, request.SandboxID)
	if err != nil {
		return TaskSandboxGuard{}, fmt.Errorf("discover OpenSandbox container for Task: %w", err)
	}
	if len(containers) != 1 || containers[0].Labels[openSandboxContainerIDLabel] != request.SandboxID ||
		!containerIDRE.MatchString(containers[0].ID) || len(containers[0].Names) != 1 ||
		containers[0].Names[0] != "/sandbox-"+request.SandboxID {
		return TaskSandboxGuard{}, errors.New("OpenSandbox container label does not resolve to one exact Task container")
	}
	guard := request
	guard.ContainerID = containers[0].ID
	guard.ContainerName = "sandbox-" + request.SandboxID
	if err := guard.validateNew(s.now()); err != nil {
		return TaskSandboxGuard{}, err
	}
	if err := s.registerTaskSandboxLocked(ctx, guard); err != nil {
		return TaskSandboxGuard{}, err
	}
	return guard, nil
}

func (s *Service) registerTaskSandboxLocked(ctx context.Context, guard TaskSandboxGuard) error {
	key := taskSandboxLeaseKey(guard.LifecycleID)
	if existing, ok := s.store.getTaskSandbox(key); ok {
		if existing.Guard.sameIdentity(guard) && !existing.Destroyed {
			return nil
		}
		return errors.New("task Sandbox lifecycle already has a different Guardian lease")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect Task Sandbox before registration: %w", err)
	}
	if err := validateTaskSandboxContainer(inspected, guard); err != nil {
		return err
	}
	proof, err := s.processes.Capture(guard.ContainerID, *inspected.State.PID)
	if err != nil || !containerCgroupMatches(proof.CgroupPath, guard.ContainerID) {
		return errors.New("cannot capture independent Task Sandbox process identity")
	}
	value := taskSandboxLease{Key: key, Guard: guard, Process: proof, Started: true}
	if err := s.store.createTaskSandbox(value); err != nil {
		return fmt.Errorf("persist Task Sandbox Guardian lease: %w", err)
	}
	return nil
}

// StopTaskSandbox signs only after the provider has removed the registered
// container and Guardian has independently verified its process tree is gone.
// The raw nonce is hashed and never persisted.
func (s *Service) StopTaskSandbox(ctx context.Context, request TaskSandboxStopRequest) (app.TaskSandboxGuardianStopAttestation, error) {
	if ctx == nil || ctx.Err() != nil || s == nil || s.taskSandboxSigningKey == nil {
		return app.TaskSandboxGuardianStopAttestation{}, errors.New("task Sandbox Guardian is unavailable")
	}
	lookupByLifecycle := request.Guard.ContainerID == "" && request.Guard.ContainerName == ""
	if lookupByLifecycle {
		if err := request.Guard.validateFence(); err != nil || request.Guard.ExpiresAt.IsZero() {
			return app.TaskSandboxGuardianStopAttestation{}, errors.New("task Sandbox stop lifecycle identity is invalid")
		}
	} else if err := request.Guard.validateIdentity(); err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, err
	}
	nonceSHA256, err := app.TaskSandboxGuardianNonceSHA256(request.Nonce)
	if err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, err
	}
	key := taskSandboxLeaseKey(request.Guard.LifecycleID)
	release, err := s.lockOperation(ctx, taskSandboxOperationKey(request.Guard.LifecycleID))
	if err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, err
	}
	defer release()
	value, ok := s.store.getTaskSandbox(key)
	identityMatches := ok && value.Guard.sameIdentity(request.Guard)
	if lookupByLifecycle {
		identityMatches = ok && value.Guard.sameLifecycleIdentity(request.Guard)
	}
	if !identityMatches {
		return app.TaskSandboxGuardianStopAttestation{}, errors.New("task Sandbox Guardian lease is unknown or stale")
	}
	if !value.Destroyed {
		if _, inspectErr := s.docker.inspect(ctx, value.Guard.ContainerID); inspectErr == nil {
			return app.TaskSandboxGuardianStopAttestation{}, errors.New("OpenSandbox container still exists")
		} else if !errors.Is(inspectErr, errContainerNotFound) {
			return app.TaskSandboxGuardianStopAttestation{}, fmt.Errorf("confirm Task Sandbox container absence: %w", inspectErr)
		}
		if err := s.confirmTaskSandboxProcessGone(ctx, &value); err != nil {
			return app.TaskSandboxGuardianStopAttestation{}, err
		}
		value.Destroyed = true
		value.DestroyedAt = s.now().UTC()
	}
	if value.NonceSHA256 != nonceSHA256 {
		value.NonceSHA256 = nonceSHA256
	}
	if err := s.store.replaceTaskSandbox(value); err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, fmt.Errorf("persist Task Sandbox destruction proof: %w", err)
	}
	return s.signTaskSandboxStop(value)
}

func validateTaskSandboxContainer(inspected containerInspect, guard TaskSandboxGuard) error {
	if inspected.ID != guard.ContainerID || inspected.Name != "/"+guard.ContainerName ||
		inspected.Config.Labels[openSandboxContainerIDLabel] != guard.SandboxID ||
		inspected.State.Running == nil || !*inspected.State.Running || inspected.State.PID == nil || *inspected.State.PID < 2 {
		return errors.New("OpenSandbox container ID, name, label, or process state does not match the Task fence")
	}
	if !taskSandboxLifecycleLabelsMatch(inspected.Config.Labels, guard) {
		return errors.New("OpenSandbox container metadata does not match the Task lifecycle fence")
	}
	return nil
}

func taskSandboxLifecycleLabelsMatch(labels map[string]string, guard TaskSandboxGuard) bool {
	metadata, err := guard.lifecycleMetadata()
	if err != nil {
		return false
	}
	for key, expected := range metadata {
		if labels[key] != expected {
			return false
		}
	}
	return true
}

func (s *Service) confirmTaskSandboxProcessGone(ctx context.Context, value *taskSandboxLease) error {
	value.UnprovenRunning = true
	if err := s.store.replaceTaskSandbox(*value); err != nil {
		return fmt.Errorf("persist unproven Task Sandbox process state: %w", err)
	}
	if err := s.processes.KillTree(value.Guard.ContainerID, value.Process); err != nil {
		return fmt.Errorf("stop independently identified Task Sandbox process: %w", err)
	}
	gone, err := s.processes.Gone(value.Guard.ContainerID, value.Process)
	if err != nil {
		return fmt.Errorf("verify Task Sandbox process exit: %w", err)
	}
	if !gone {
		return errors.New("task Sandbox process tree remains after container removal")
	}
	value.UnprovenRunning = false
	return nil
}

func (s *Service) destroyExpiredTaskSandbox(ctx context.Context, value *taskSandboxLease) error {
	inspected, err := s.docker.inspect(ctx, value.Guard.ContainerID)
	if err != nil && !errors.Is(err, errContainerNotFound) {
		return fmt.Errorf("inspect expired Task Sandbox: %w", err)
	}
	if err == nil {
		if inspected.ID != value.Guard.ContainerID || inspected.Name != "/"+value.Guard.ContainerName ||
			inspected.Config.Labels[openSandboxContainerIDLabel] != value.Guard.SandboxID || inspected.State.Running == nil || inspected.State.PID == nil ||
			!taskSandboxLifecycleLabelsMatch(inspected.Config.Labels, value.Guard) {
			return errors.New("expired OpenSandbox container identity is not proven")
		}
		if *inspected.State.Running {
			proof, captureErr := s.processes.Capture(value.Guard.ContainerID, *inspected.State.PID)
			if captureErr != nil || proof.CgroupPath != value.Process.CgroupPath {
				value.UnprovenRunning = true
				if persistErr := s.store.replaceTaskSandbox(*value); persistErr != nil {
					return fmt.Errorf("persist changed Task Sandbox process identity: %w", persistErr)
				}
			} else {
				value.Process = proof
				if persistErr := s.store.replaceTaskSandbox(*value); persistErr != nil {
					return fmt.Errorf("persist expired Task Sandbox process evidence: %w", persistErr)
				}
			}
		}
		if !value.UnprovenRunning {
			if killErr := s.processes.KillTree(value.Guard.ContainerID, value.Process); killErr != nil {
				value.UnprovenRunning = true
				if persistErr := s.store.replaceTaskSandbox(*value); persistErr != nil {
					return errors.Join(killErr, fmt.Errorf("persist unproven expired Task Sandbox process state: %w", persistErr))
				}
			}
		}
		if *inspected.State.Running {
			if err := s.docker.kill(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errDockerConflict) {
				return fmt.Errorf("kill expired OpenSandbox container: %w", err)
			}
			if err := s.docker.wait(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
				return fmt.Errorf("wait for expired OpenSandbox process: %w", err)
			}
		}
		if err := s.docker.remove(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
			return fmt.Errorf("remove expired OpenSandbox container: %w", err)
		}
	}
	if _, err := s.docker.inspect(ctx, value.Guard.ContainerID); err == nil {
		return errors.New("expired OpenSandbox container remains after removal")
	} else if !errors.Is(err, errContainerNotFound) {
		return fmt.Errorf("confirm expired OpenSandbox absence: %w", err)
	}
	if err := s.confirmTaskSandboxProcessGone(ctx, value); err != nil {
		return err
	}
	value.Destroyed = true
	value.DestroyedAt = s.now().UTC()
	if err := s.store.replaceTaskSandbox(*value); err != nil {
		return fmt.Errorf("persist expired Task Sandbox destruction: %w", err)
	}
	return nil
}

func (s *Service) signTaskSandboxStop(value taskSandboxLease) (app.TaskSandboxGuardianStopAttestation, error) {
	if !value.Destroyed || value.DestroyedAt.IsZero() || value.UnprovenRunning || value.NonceSHA256 == "" {
		return app.TaskSandboxGuardianStopAttestation{}, errors.New("task Sandbox destruction proof is not durable")
	}
	proofBytes, err := json.Marshal(value)
	if err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, fmt.Errorf("encode Task Sandbox destruction proof: %w", err)
	}
	proofDigest := sha256.Sum256(proofBytes)
	attestation := app.TaskSandboxGuardianStopAttestation{
		LifecycleID: value.Guard.LifecycleID, TaskID: value.Guard.TaskID, OwnerID: value.Guard.OwnerID,
		InitiatorID: value.Guard.InitiatorID, LeaseOwner: value.Guard.LeaseOwner, LeaseVersion: value.Guard.LeaseVersion,
		AuthorizationVersion: value.Guard.AuthorizationVersion, AgentInstanceID: value.Guard.AgentInstanceID,
		AgentGeneration: value.Guard.AgentGeneration, SandboxID: value.Guard.SandboxID,
		SandboxGeneration: value.Guard.SandboxGeneration, ContainerID: value.Guard.ContainerID,
		ContainerName: value.Guard.ContainerName, NonceSHA256: value.NonceSHA256, ExpiresAt: value.Guard.ExpiresAt,
		DestroyedAt: value.DestroyedAt, ProofSHA256: hex.EncodeToString(proofDigest[:]), Destroyed: true,
	}
	payload, err := app.TaskSandboxGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		return app.TaskSandboxGuardianStopAttestation{}, err
	}
	attestation.Signature = ed25519.Sign(s.taskSandboxSigningKey, payload)
	return attestation, nil
}
