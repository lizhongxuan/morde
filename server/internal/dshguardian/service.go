package dshguardian

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"morde/server/internal/app"
)

type processInspector interface {
	Ready() error
	Capture(containerID string, pid int) (processProof, error)
	KillTree(containerID string, proof processProof) error
	Gone(containerID string, proof processProof) (bool, error)
}

type Service struct {
	store      *leaseStore
	docker     dockerAPI
	processes  processInspector
	signingKey ed25519.PrivateKey
	now        func() time.Time
	operationM sync.Mutex
}

// Open creates a guardian using a private on-disk lease directory and a Docker
// Engine Unix socket. It never starts or creates containers.
func Open(stateDir, dockerSocket string, signingKey ed25519.PrivateKey) (*Service, error) {
	if !validPrivateKey(signingKey) {
		return nil, errors.New("guardian requires a valid Ed25519 attestation private key")
	}
	store, err := openLeaseStore(stateDir)
	if err != nil {
		return nil, err
	}
	docker, err := newDockerEngine(dockerSocket)
	if err != nil {
		return nil, err
	}
	processes := newProcessInspector()
	if err := processes.Ready(); err != nil {
		return nil, fmt.Errorf("initialize independent process-tree verification: %w", err)
	}
	return &Service{store: store, docker: docker, processes: processes, signingKey: append(ed25519.PrivateKey(nil), signingKey...), now: time.Now}, nil
}

func validPrivateKey(key ed25519.PrivateKey) bool {
	return len(key) == ed25519.PrivateKeySize && bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize]))
}

// LoadAttestationPrivateKey reads a raw Ed25519 private key from a protected
// local file. It never creates or repairs signing material.
func LoadAttestationPrivateKey(path string) (ed25519.PrivateKey, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("guardian attestation key path must be a clean absolute path")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != ed25519.PrivateKeySize ||
		before.Mode().Perm()&0o077 != 0 || before.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("guardian attestation key must be a protected regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open guardian attestation key: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 || opened.Mode().Perm()&0o400 == 0 || !os.SameFile(before, opened) {
		return nil, errors.New("guardian attestation key changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, ed25519.PrivateKeySize+1))
	if err != nil || len(data) != ed25519.PrivateKeySize {
		return nil, errors.New("guardian attestation key has an invalid length")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || after.Mode().Perm()&0o077 != 0 || after.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("guardian attestation key changed while reading")
	}
	key := ed25519.PrivateKey(data)
	if !validPrivateKey(key) {
		return nil, errors.New("guardian attestation key is invalid")
	}
	return append(ed25519.PrivateKey(nil), key...), nil
}

// Arm durably records an expiry before the supervisor starts the container.
func (s *Service) Arm(ctx context.Context, guard Guard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("guardian arm context is unavailable")
	}
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	if err := s.processes.Ready(); err != nil {
		return fmt.Errorf("independent process-tree verification is unavailable: %w", err)
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	if existing, ok := s.store.get(guard.ContainerID); ok {
		if existing.Guard.sameIdentity(guard) && existing.Guard.ExpiresAt.Equal(guard.ExpiresAt) && !existing.Destroyed {
			return nil
		}
		return errors.New("guardian lease identity already exists")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect dsh container before arming: %w", err)
	}
	if err := validateContainerIdentity(inspected, guard); err != nil {
		return err
	}
	if inspected.State.Running == nil || inspected.State.PID == nil {
		return errors.New("docker inspection omitted process state")
	}
	if *inspected.State.Running || *inspected.State.PID != 0 {
		return errors.New("guardian must arm an unstarted dsh container")
	}
	return s.store.create(lease{Guard: guard})
}

// Started binds a running container to its host PID and cgroup before returning.
func (s *Service) Started(ctx context.Context, guard Guard, pid int) error {
	if ctx == nil || ctx.Err() != nil || pid < 1 {
		return errors.New("guardian started identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupGuard(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) {
		return errors.New("guardian lease is expired or destroyed")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect started dsh container: %w", err)
	}
	if err := validateContainerIdentity(inspected, guard); err != nil {
		return err
	}
	if inspected.State.Running == nil || inspected.State.PID == nil || !*inspected.State.Running || *inspected.State.PID != pid {
		return errors.New("guardian PID does not match the running dsh container")
	}
	proof, err := s.processes.Capture(guard.ContainerID, pid)
	if err != nil {
		return fmt.Errorf("capture independent dsh process evidence: %w", err)
	}
	value.Process = proof
	value.Started = true
	value.UnprovenRunning = false
	if err := s.store.replace(value); err != nil {
		return fmt.Errorf("persist dsh process evidence: %w", err)
	}
	return nil
}

// Renew persists the new deadline before acknowledging renewal.
func (s *Service) Renew(ctx context.Context, guard Guard) error {
	if ctx == nil || ctx.Err() != nil {
		return errors.New("guardian renewal context is unavailable")
	}
	if err := guard.validateNew(s.now()); err != nil {
		return err
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupGuardIdentity(guard)
	if err != nil {
		return err
	}
	if value.Destroyed || !value.Guard.ExpiresAt.After(s.now()) || guard.ExpiresAt.Before(value.Guard.ExpiresAt) {
		return errors.New("guardian lease cannot be renewed")
	}
	inspected, err := s.docker.inspect(ctx, guard.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect dsh container before renewal: %w", err)
	}
	if err := validateContainerIdentity(inspected, guard); err != nil {
		return err
	}
	value.Guard = guard
	if err := s.store.replace(value); err != nil {
		return fmt.Errorf("persist dsh lease renewal: %w", err)
	}
	return nil
}

// ConfirmDestroyed repeats identity checks and process-exit verification. A
// true result is returned only after the Docker object is absent and the host
// process evidence is independently gone.
func (s *Service) ConfirmDestroyed(ctx context.Context, guard Guard) (bool, error) {
	if ctx == nil || ctx.Err() != nil {
		return false, errors.New("guardian confirmation context is unavailable")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, err := s.lookupGuard(guard)
	if err != nil {
		return false, err
	}
	if err := s.destroyAndVerify(ctx, &value); err != nil {
		return false, err
	}
	return true, nil
}

// RecoverStopped locates one exact persisted lease, stops/rechecks it through
// the independent Docker and cgroup inspectors, and signs the durable proof.
func (s *Service) RecoverStopped(ctx context.Context, fence app.AgentInstanceFence, nonceSHA256 string) (app.AgentGuardianStopAttestation, error) {
	if ctx == nil || ctx.Err() != nil || fence.Validate() != nil || fence.SupervisorID == "" || !nonceHashRE.MatchString(nonceSHA256) {
		return app.AgentGuardianStopAttestation{}, errors.New("guardian recovery identity is invalid")
	}
	s.operationM.Lock()
	defer s.operationM.Unlock()
	value, ok, err := s.store.find(fence, nonceSHA256)
	if err != nil || !ok {
		return app.AgentGuardianStopAttestation{}, errors.New("guardian lease for the exact Agent fence is unknown")
	}
	if err := s.destroyAndVerify(ctx, &value); err != nil {
		return app.AgentGuardianStopAttestation{}, fmt.Errorf("guardian could not verify recovered Agent destruction: %w", err)
	}
	current, ok := s.store.get(value.Guard.ContainerID)
	if !ok || current.Guard != value.Guard || !current.Destroyed || current.DestroyedAt.IsZero() || current.UnprovenRunning {
		return app.AgentGuardianStopAttestation{}, errors.New("guardian destruction proof was not durably recorded")
	}
	proofBytes, err := json.Marshal(current)
	if err != nil {
		return app.AgentGuardianStopAttestation{}, fmt.Errorf("encode durable guardian proof: %w", err)
	}
	proofDigest := sha256.Sum256(proofBytes)
	attestation := app.AgentGuardianStopAttestation{
		Fence: current.Guard.Fence, ContainerID: current.Guard.ContainerID, ContainerName: current.Guard.ContainerName,
		NonceSHA256: current.Guard.NonceSHA256, ExpiresAt: current.Guard.ExpiresAt, DestroyedAt: current.DestroyedAt,
		ProofSHA256: hex.EncodeToString(proofDigest[:]), Destroyed: true,
	}
	payload, err := app.AgentGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		return app.AgentGuardianStopAttestation{}, err
	}
	attestation.Signature = ed25519.Sign(s.signingKey, payload)
	return attestation, nil
}

func (s *Service) lookupGuard(guard Guard) (lease, error) {
	value, ok := s.store.get(guard.ContainerID)
	if !ok || !value.Guard.sameIdentity(guard) || !value.Guard.ExpiresAt.Equal(guard.ExpiresAt) {
		return lease{}, errors.New("guardian lease is unknown or stale")
	}
	return value, nil
}

func (s *Service) lookupGuardIdentity(guard Guard) (lease, error) {
	value, ok := s.store.get(guard.ContainerID)
	if !ok || !value.Guard.sameIdentity(guard) {
		return lease{}, errors.New("guardian lease identity is unknown")
	}
	return value, nil
}

func validateContainerIdentity(inspected containerInspect, guard Guard) error {
	encodedFence, err := json.Marshal(guard.Fence)
	if err != nil {
		return errors.New("cannot encode dsh fence")
	}
	if inspected.State.Running == nil || inspected.State.PID == nil ||
		inspected.ID != guard.ContainerID || inspected.Name != "/"+guard.ContainerName ||
		inspected.Config.Labels["morde.dsh.fence"] != string(encodedFence) ||
		inspected.Config.Labels["morde.dsh.nonce-sha256"] != guard.NonceSHA256 {
		return errors.New("docker container ID, name, or DSH labels do not match the lease")
	}
	return nil
}

func (s *Service) destroyAndVerify(ctx context.Context, value *lease) error {
	inspected, err := s.docker.inspect(ctx, value.Guard.ContainerID)
	switch {
	case errors.Is(err, errContainerNotFound):
		if value.Started {
			if err := s.processes.KillTree(value.Guard.ContainerID, value.Process); err != nil {
				value.UnprovenRunning = true
				if persistErr := s.store.replace(*value); persistErr != nil {
					return fmt.Errorf("persist unproven dsh process tree: %w", persistErr)
				}
			} else if value.UnprovenRunning {
				value.UnprovenRunning = false
				if err := s.store.replace(*value); err != nil {
					return fmt.Errorf("persist recovered dsh process evidence: %w", err)
				}
			}
		}
	case err != nil:
		return fmt.Errorf("inspect guarded dsh container: %w", err)
	default:
		if err := validateContainerIdentity(inspected, value.Guard); err != nil {
			return err
		}
		if inspected.State.Running == nil || inspected.State.PID == nil {
			return errors.New("docker inspection omitted process state")
		}
		if *inspected.State.Running {
			if *inspected.State.PID < 1 {
				return errors.New("running dsh container has no host PID")
			}
			if !value.Started || value.Process.PID != *inspected.State.PID || value.UnprovenRunning {
				proof, err := s.processes.Capture(value.Guard.ContainerID, *inspected.State.PID)
				if err == nil && value.Started && proof.CgroupPath != value.Process.CgroupPath {
					err = errors.New("guarded dsh container moved to a different cgroup")
				}
				if err != nil {
					// Persist uncertainty before stopping so a restart can never
					// turn an unverified running tree into a success receipt.
					value.UnprovenRunning = true
					if persistErr := s.store.replace(*value); persistErr != nil {
						return fmt.Errorf("persist unproven dsh process state: %w", persistErr)
					}
				} else {
					value.Process = proof
					value.Started = true
					value.UnprovenRunning = false
					if err := s.store.replace(*value); err != nil {
						return fmt.Errorf("persist expired dsh process evidence: %w", err)
					}
				}
			}
		} else if *inspected.State.PID != 0 {
			return errors.New("stopped dsh container still reports a host PID")
		}
		if value.Started && !value.UnprovenRunning {
			if err := s.processes.KillTree(value.Guard.ContainerID, value.Process); err != nil {
				value.UnprovenRunning = true
				if persistErr := s.store.replace(*value); persistErr != nil {
					return fmt.Errorf("persist unproven dsh process tree: %w", persistErr)
				}
			} else if value.UnprovenRunning {
				value.UnprovenRunning = false
				if err := s.store.replace(*value); err != nil {
					return fmt.Errorf("persist recovered dsh process evidence: %w", err)
				}
			}
		}
		if *inspected.State.Running {
			if err := s.docker.kill(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errDockerConflict) {
				return fmt.Errorf("kill expired dsh container: %w", err)
			}
			if err := s.docker.wait(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
				return fmt.Errorf("wait for dsh process exit: %w", err)
			}
		}
		if err := s.docker.remove(ctx, value.Guard.ContainerID); err != nil && !errors.Is(err, errContainerNotFound) {
			return fmt.Errorf("remove stopped dsh container: %w", err)
		}
	}

	remaining, err := s.docker.inspect(ctx, value.Guard.ContainerID)
	if err == nil {
		if err := validateContainerIdentity(remaining, value.Guard); err != nil {
			return err
		}
		return errors.New("docker still reports the guarded dsh container")
	}
	if !errors.Is(err, errContainerNotFound) {
		return fmt.Errorf("confirm dsh container absence: %w", err)
	}
	if value.UnprovenRunning {
		return errors.New("dsh process tree was stopped without sufficient pre-stop identity evidence")
	}
	gone, err := s.processes.Gone(value.Guard.ContainerID, value.Process)
	if err != nil {
		return fmt.Errorf("verify host dsh process tree: %w", err)
	}
	if !gone {
		return errors.New("host cgroup or DSH process is still present")
	}
	value.Destroyed = true
	if value.DestroyedAt.IsZero() {
		value.DestroyedAt = s.now().UTC()
	}
	if err := s.store.replace(*value); err != nil {
		return fmt.Errorf("persist dsh destruction proof: %w", err)
	}
	return nil
}

// Run sweeps immediately and then at the supplied interval. It keeps retrying
// failed leases; onError is called at most once per lease per sweep.
func (s *Service) Run(ctx context.Context, interval time.Duration, onError func(error)) error {
	if ctx == nil || interval <= 0 {
		return errors.New("guardian sweep context and interval are required")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.sweep(ctx, onError)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) sweep(ctx context.Context, onError func(error)) {
	for _, value := range s.store.allTasks() {
		if ctx.Err() != nil {
			return
		}
		if value.Destroyed || value.Guard.ExpiresAt.After(s.now()) {
			continue
		}
		s.operationM.Lock()
		current, ok := s.store.getTask(value.Key)
		if !ok || current.Guard != value.Guard || current.Destroyed || current.Guard.ExpiresAt.After(s.now()) {
			s.operationM.Unlock()
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := s.destroyTaskProcess(opCtx, &current)
		cancel()
		s.operationM.Unlock()
		if err != nil && onError != nil {
			onError(fmt.Errorf("reconcile expired Task process lease %s: %w", value.Key, err))
		}
	}
	for _, value := range s.store.allRuntimes() {
		if ctx.Err() != nil {
			return
		}
		if value.Destroyed || value.Guard.ExpiresAt.After(s.now()) {
			continue
		}
		s.operationM.Lock()
		current, ok := s.store.getRuntime(value.Guard.ContainerID)
		if !ok || current.Guard != value.Guard || current.Destroyed || current.Guard.ExpiresAt.After(s.now()) {
			s.operationM.Unlock()
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		taskStopErr := s.stopRuntimeTasks(opCtx, current)
		err := s.destroyAccountRuntime(opCtx, &current)
		if taskStopErr != nil {
			err = errors.Join(err, fmt.Errorf("one or more Task cgroups remain unproven: %w", taskStopErr))
		}
		cancel()
		s.operationM.Unlock()
		if err != nil && onError != nil {
			onError(fmt.Errorf("reconcile expired account Runtime lease %s: %w", value.Guard.ContainerID, err))
		}
	}
	for _, value := range s.store.all() {
		if ctx.Err() != nil {
			return
		}
		if value.Destroyed || value.Guard.ExpiresAt.After(s.now()) {
			continue
		}
		s.operationM.Lock()
		current, ok := s.store.get(value.Guard.ContainerID)
		if !ok || current.Guard != value.Guard || current.Destroyed || current.Guard.ExpiresAt.After(s.now()) {
			s.operationM.Unlock()
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := s.destroyAndVerify(opCtx, &current)
		cancel()
		s.operationM.Unlock()
		if err != nil && onError != nil {
			onError(fmt.Errorf("reconcile expired dsh lease %s: %w", value.Guard.ContainerID, err))
		}
	}
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}
