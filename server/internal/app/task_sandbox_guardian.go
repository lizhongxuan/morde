package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"morde/server/internal/domain"
)

// TaskSandboxGuardianStopAttestation proves that OpenSandbox has completed its
// cleanup and the exact Docker container for one Task sandbox generation is
// absent. It is deliberately distinct from account Runtime and DSH process
// stop proofs.
type TaskSandboxGuardianStopAttestation struct {
	LifecycleID          string    `json:"lifecycle_id"`
	TaskID               string    `json:"task_id"`
	OwnerID              string    `json:"owner_id"`
	InitiatorID          string    `json:"initiator_id"`
	LeaseOwner           string    `json:"lease_owner"`
	LeaseVersion         int64     `json:"lease_version"`
	AuthorizationVersion int64     `json:"authorization_version"`
	AgentInstanceID      string    `json:"agent_instance_id"`
	AgentGeneration      int64     `json:"agent_generation"`
	SandboxID            string    `json:"sandbox_id"`
	SandboxGeneration    int64     `json:"sandbox_generation"`
	ContainerID          string    `json:"container_id"`
	ContainerName        string    `json:"container_name"`
	NonceSHA256          string    `json:"nonce_sha256"`
	ExpiresAt            time.Time `json:"expires_at"`
	DestroyedAt          time.Time `json:"destroyed_at"`
	ProofSHA256          string    `json:"proof_sha256"`
	Destroyed            bool      `json:"destroyed"`
	Signature            []byte    `json:"signature"`
}

type verifiedTaskSandboxGuardianStop struct {
	attestation TaskSandboxGuardianStopAttestation
	encoded     []byte
}

// VerifiedTaskSandboxGuardianStop can only be created by this package's
// signature verifier.
type VerifiedTaskSandboxGuardianStop struct {
	value verifiedTaskSandboxGuardianStop
}

func (v VerifiedTaskSandboxGuardianStop) Value() (TaskSandboxGuardianStopAttestation, []byte) {
	attestation := v.value.attestation
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	return attestation, append([]byte(nil), v.value.encoded...)
}

// TaskSandboxGuardianStopAuthenticator verifies the dedicated Task Sandbox
// Guardian key. It must not share a key with DSH process or account Runtime
// attestations.
type TaskSandboxGuardianStopAuthenticator struct{ publicKey ed25519.PublicKey }

func NewTaskSandboxGuardianStopAuthenticator(publicKey ed25519.PublicKey) (*TaskSandboxGuardianStopAuthenticator, error) {
	if len(publicKey) != ed25519.PublicKeySize || bytesAreZero(publicKey) {
		return nil, domain.InvalidArgument("guardian", "Task Sandbox Guardian Ed25519 public key is required")
	}
	return &TaskSandboxGuardianStopAuthenticator{publicKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func (a *TaskSandboxGuardianStopAuthenticator) VerifyTaskSandboxStop(ctx context.Context,
	attestation TaskSandboxGuardianStopAttestation) (VerifiedTaskSandboxGuardianStop, error) {
	if ctx == nil || ctx.Err() != nil || a == nil || len(a.publicKey) != ed25519.PublicKeySize {
		return VerifiedTaskSandboxGuardianStop{}, domain.InvalidArgument("attestation", "Task Sandbox Guardian verification context is unavailable")
	}
	if err := validateTaskSandboxGuardianStopAttestation(attestation); err != nil {
		return VerifiedTaskSandboxGuardianStop{}, err
	}
	payload, err := TaskSandboxGuardianStopAttestationSigningPayload(attestation)
	if err != nil || !ed25519.Verify(a.publicKey, payload, attestation.Signature) {
		return VerifiedTaskSandboxGuardianStop{}, domain.NewError(domain.CodeUnauthenticated, "Task Sandbox Guardian signature is invalid")
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return VerifiedTaskSandboxGuardianStop{}, errors.New("encode verified Task Sandbox Guardian proof")
	}
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	return VerifiedTaskSandboxGuardianStop{value: verifiedTaskSandboxGuardianStop{attestation: attestation, encoded: encoded}}, nil
}

// TaskSandboxLifecycleStopStore is the narrow persistence contract needed to
// accept a Guardian proof. Lifecycle and call creation remain owned by their
// existing store methods.
type TaskSandboxLifecycleStopStore interface {
	GetTaskSandboxLifecycle(context.Context, string, string, int64) (TaskSandboxLifecycle, error)
	SetTaskSandboxGuardianStopChallenge(context.Context, TaskSandboxLifecycle, string) (TaskSandboxLifecycle, error)
	TerminateTaskSandbox(context.Context, TaskSandboxLifecycle, []byte) (TaskSandboxLifecycle, error)
}

type TaskSandboxLifecycleService struct {
	store         TaskSandboxLifecycleStopStore
	authenticator *TaskSandboxGuardianStopAuthenticator
}

func NewTaskSandboxLifecycleService(store TaskSandboxLifecycleStopStore,
	authenticator *TaskSandboxGuardianStopAuthenticator) (*TaskSandboxLifecycleService, error) {
	if store == nil || authenticator == nil {
		return nil, domain.InvalidArgument("dependencies", "Task Sandbox lifecycle stop dependencies are required")
	}
	return &TaskSandboxLifecycleService{store: store, authenticator: authenticator}, nil
}

// ConfirmGuardianStopped records only a signed proof for the exact persisted
// Task fence and sandbox generation. Replaying the same signed proof is
// idempotent at the store boundary; a different proof conflicts there.
func (s *TaskSandboxLifecycleService) ConfirmGuardianStopped(ctx context.Context,
	attestation TaskSandboxGuardianStopAttestation) (TaskSandboxLifecycle, error) {
	if ctx == nil || s == nil || s.store == nil || s.authenticator == nil {
		return TaskSandboxLifecycle{}, domain.InvalidArgument("dependencies", "Task Sandbox lifecycle stop service is unavailable")
	}
	verified, err := s.authenticator.VerifyTaskSandboxStop(ctx, attestation)
	if err != nil {
		return TaskSandboxLifecycle{}, err
	}
	lifecycle, err := s.store.GetTaskSandboxLifecycle(ctx, attestation.OwnerID, attestation.TaskID, attestation.SandboxGeneration)
	if err != nil {
		return TaskSandboxLifecycle{}, err
	}
	if !taskSandboxLifecycleMatchesGuardianStop(lifecycle, attestation) {
		return TaskSandboxLifecycle{}, domain.NewError(domain.CodeWorkspaceConflict, "Task Sandbox Guardian proof does not match the persisted lifecycle")
	}
	_, evidence := verified.Value()
	return s.store.TerminateTaskSandbox(ctx, lifecycle, evidence)
}

// PrepareGuardianStopChallenge persists a fresh digest before the raw nonce is
// sent to Guardian. If the caller crashes, it can rotate the digest and retry;
// proofs for an abandoned challenge will no longer match the lifecycle.
func (s *TaskSandboxLifecycleService) PrepareGuardianStopChallenge(ctx context.Context, ownerID, taskID string,
	generation int64) (TaskSandboxLifecycle, []byte, error) {
	if ctx == nil || s == nil || s.store == nil {
		return TaskSandboxLifecycle{}, nil, domain.InvalidArgument("dependencies", "Task Sandbox stop challenge service is unavailable")
	}
	lifecycle, err := s.store.GetTaskSandboxLifecycle(ctx, ownerID, taskID, generation)
	if err != nil {
		return TaskSandboxLifecycle{}, nil, err
	}
	if lifecycle.Status != TaskSandboxStopping && lifecycle.Status != TaskSandboxUnknown {
		return TaskSandboxLifecycle{}, nil, domain.NewError(domain.CodeConflict, "Task Sandbox must be stopping before Guardian challenge")
	}
	nonce, digest, err := NewTaskSandboxGuardianNonce()
	if err != nil {
		return TaskSandboxLifecycle{}, nil, err
	}
	updated, err := s.store.SetTaskSandboxGuardianStopChallenge(ctx, lifecycle, digest)
	if err != nil {
		clear(nonce)
		return TaskSandboxLifecycle{}, nil, err
	}
	return updated, nonce, nil
}

// NewTaskSandboxGuardianNonce creates a high-entropy one-time challenge. The
// caller persists only the returned digest and passes the raw nonce to the
// dedicated Guardian. Callers should clear the raw bytes after use.
func NewTaskSandboxGuardianNonce() ([]byte, string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", errors.New("generate Task Sandbox Guardian nonce")
	}
	digest := sha256.Sum256(nonce)
	return nonce, hex.EncodeToString(digest[:]), nil
}

func TaskSandboxGuardianNonceSHA256(nonce []byte) (string, error) {
	if len(nonce) != 32 || bytesAreZero(nonce) {
		return "", domain.InvalidArgument("guardian_nonce", "Task Sandbox Guardian nonce is invalid")
	}
	digest := sha256.Sum256(nonce)
	return hex.EncodeToString(digest[:]), nil
}

func TaskSandboxGuardianStopAttestationSigningPayload(attestation TaskSandboxGuardianStopAttestation) ([]byte, error) {
	return json.Marshal(struct {
		Purpose              string    `json:"purpose"`
		LifecycleID          string    `json:"lifecycle_id"`
		TaskID               string    `json:"task_id"`
		OwnerID              string    `json:"owner_id"`
		InitiatorID          string    `json:"initiator_id"`
		LeaseOwner           string    `json:"lease_owner"`
		LeaseVersion         int64     `json:"lease_version"`
		AuthorizationVersion int64     `json:"authorization_version"`
		AgentInstanceID      string    `json:"agent_instance_id"`
		AgentGeneration      int64     `json:"agent_generation"`
		SandboxID            string    `json:"sandbox_id"`
		SandboxGeneration    int64     `json:"sandbox_generation"`
		ContainerID          string    `json:"container_id"`
		ContainerName        string    `json:"container_name"`
		NonceSHA256          string    `json:"nonce_sha256"`
		ExpiresAt            time.Time `json:"expires_at"`
		DestroyedAt          time.Time `json:"destroyed_at"`
		ProofSHA256          string    `json:"proof_sha256"`
		Destroyed            bool      `json:"destroyed"`
	}{
		Purpose: "morde.task-sandbox-guardian-stop.v1", LifecycleID: attestation.LifecycleID,
		TaskID: attestation.TaskID, OwnerID: attestation.OwnerID, InitiatorID: attestation.InitiatorID,
		LeaseOwner: attestation.LeaseOwner, LeaseVersion: attestation.LeaseVersion,
		AuthorizationVersion: attestation.AuthorizationVersion, AgentInstanceID: attestation.AgentInstanceID,
		AgentGeneration: attestation.AgentGeneration, SandboxID: attestation.SandboxID,
		SandboxGeneration: attestation.SandboxGeneration, ContainerID: attestation.ContainerID,
		ContainerName: attestation.ContainerName, NonceSHA256: attestation.NonceSHA256,
		ExpiresAt: attestation.ExpiresAt, DestroyedAt: attestation.DestroyedAt,
		ProofSHA256: attestation.ProofSHA256, Destroyed: attestation.Destroyed,
	})
}

func validateTaskSandboxGuardianStopAttestation(value TaskSandboxGuardianStopAttestation) error {
	if !domain.ValidOperationID(value.LifecycleID) || !domain.ValidTaskID(value.TaskID) ||
		!domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) || value.OwnerID != value.InitiatorID ||
		invalidCheckpointLabel(value.LeaseOwner, 255) || value.LeaseVersion < 1 || value.AuthorizationVersion < 1 ||
		!agentCheckpointIDPattern.MatchString(value.AgentInstanceID) || value.AgentGeneration < 1 ||
		!validTaskSandboxGuardianSandboxID(value.SandboxID) || value.SandboxGeneration < 1 ||
		!isLowerHex(value.ContainerID) || len(value.ContainerID) != sha256.Size*2 ||
		value.ContainerName != "sandbox-"+value.SandboxID ||
		!isLowerHex(value.NonceSHA256) || len(value.NonceSHA256) != sha256.Size*2 ||
		value.ExpiresAt.IsZero() || value.DestroyedAt.IsZero() || !value.Destroyed ||
		!isLowerHex(value.ProofSHA256) || len(value.ProofSHA256) != sha256.Size*2 ||
		len(value.Signature) != ed25519.SignatureSize {
		return domain.InvalidArgument("attestation", "Task Sandbox Guardian stop attestation is invalid")
	}
	return nil
}

func validTaskSandboxGuardianSandboxID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func taskSandboxLifecycleMatchesGuardianStop(lifecycle TaskSandboxLifecycle, value TaskSandboxGuardianStopAttestation) bool {
	return lifecycle.ID == value.LifecycleID && lifecycle.TaskID == value.TaskID && lifecycle.OwnerID == value.OwnerID &&
		lifecycle.InitiatorID == value.InitiatorID && lifecycle.LeaseOwner == value.LeaseOwner &&
		lifecycle.LeaseVersion == value.LeaseVersion && lifecycle.AuthorizationVersion == value.AuthorizationVersion &&
		lifecycle.AgentInstanceID == value.AgentInstanceID && lifecycle.AgentGeneration == value.AgentGeneration &&
		lifecycle.NonceSHA256 == value.NonceSHA256 && lifecycle.SandboxID == value.SandboxID && lifecycle.SandboxGeneration == value.SandboxGeneration &&
		lifecycle.ExpiresAt != nil && lifecycle.ExpiresAt.Equal(value.ExpiresAt) &&
		(lifecycle.Status == TaskSandboxStopping || lifecycle.Status == TaskSandboxUnknown || lifecycle.Status == TaskSandboxTerminated)
}

func bytesAreZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}
