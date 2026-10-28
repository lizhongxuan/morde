package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"morde/server/internal/domain"
)

type AccountAgentRuntimeState string

const (
	AccountAgentRuntimeStarting AccountAgentRuntimeState = "starting"
	AccountAgentRuntimeRunning  AccountAgentRuntimeState = "running"
	AccountAgentRuntimeStopping AccountAgentRuntimeState = "stopping"
	AccountAgentRuntimeStopped  AccountAgentRuntimeState = "stopped"
	AccountAgentRuntimeFailed   AccountAgentRuntimeState = "failed"
	maxAccountAgentRuntimeTasks                          = 16
)

const MaxAccountAgentRuntimeStopCandidates = 100

var serviceInstanceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidAccountAgentRuntimeOwnerID(ownerID string) bool { return domain.ValidUserID(ownerID) }

type AccountAgentRuntimeFence struct {
	ServiceInstanceID string `json:"service_instance_id"`
	OwnerID           string `json:"owner_id"`
	RuntimeID         string `json:"runtime_id"`
	RuntimeGeneration int64  `json:"runtime_generation"`
	LeaseOwner        string `json:"lease_owner"`
	LeaseVersion      int64  `json:"lease_version"`
	ExecutionEpoch    int64  `json:"execution_epoch"`
}

func (f AccountAgentRuntimeFence) Validate() error {
	if !serviceInstanceIDPattern.MatchString(f.ServiceInstanceID) || !domain.ValidUserID(f.OwnerID) ||
		!agentCheckpointIDPattern.MatchString(f.RuntimeID) || f.RuntimeGeneration < 1 ||
		invalidCheckpointLabel(f.LeaseOwner, 255) || f.LeaseVersion < 1 || f.ExecutionEpoch < 1 {
		return domain.InvalidArgument("account_runtime", "account runtime fence is invalid")
	}
	return nil
}

type AccountAgentRuntimeStart struct {
	OwnerID            string
	RuntimeID          string
	LeaseOwner         string
	SupervisorNonce    []byte
	MaxConcurrentTasks int
	LeaseDuration      time.Duration
}

func (r AccountAgentRuntimeStart) Validate() error {
	if !domain.ValidUserID(r.OwnerID) || !agentCheckpointIDPattern.MatchString(r.RuntimeID) ||
		invalidCheckpointLabel(r.LeaseOwner, 255) || len(r.SupervisorNonce) != 32 || bytes.Equal(r.SupervisorNonce, make([]byte, 32)) ||
		r.MaxConcurrentTasks < 1 || r.MaxConcurrentTasks > maxAccountAgentRuntimeTasks ||
		!validAgentLease(r.LeaseDuration) {
		return domain.InvalidArgument("account_runtime", "account runtime start is invalid")
	}
	return nil
}

type AccountAgentRuntime struct {
	Fence                         AccountAgentRuntimeFence
	State                         AccountAgentRuntimeState
	LeaseExpiresAt                time.Time
	MaxConcurrentTasks            int
	RuntimeSequence               int64
	ControlVersion                int64
	NonceSHA256                   string
	GuardianStopAttestationDigest []byte
	UpdatedAt                     time.Time
}

type AccountAgentRuntimeStopIdentity struct {
	Fence       AccountAgentRuntimeFence
	NonceSHA256 string
	ExpiresAt   time.Time
}

// AccountAgentRuntimeStopAttestation is a Guardian-signed proof that the
// exact account Runtime container was destroyed. The raw start nonce is never
// included; only its digest is carried across the Guardian boundary.
type AccountAgentRuntimeStopAttestation struct {
	Fence         AccountAgentRuntimeFence `json:"fence"`
	ContainerID   string                   `json:"container_id"`
	ContainerName string                   `json:"container_name"`
	NonceSHA256   string                   `json:"nonce_sha256"`
	ExpiresAt     time.Time                `json:"expires_at"`
	DestroyedAt   time.Time                `json:"destroyed_at"`
	ProofSHA256   string                   `json:"proof_sha256"`
	Destroyed     bool                     `json:"destroyed"`
	Signature     []byte                   `json:"signature"`
}

// AccountAgentTaskStopAttestation proves that one Task process was destroyed
// inside the current account Runtime container. It is a different proof scope
// from destroying the account Runtime itself.
type AccountAgentTaskStopAttestation struct {
	RuntimeFence       AccountAgentRuntimeFence `json:"runtime_fence"`
	TaskFence          AgentInstanceFence       `json:"task_fence"`
	RuntimeContainerID string                   `json:"runtime_container_id"`
	ContainerPID       int                      `json:"container_pid"`
	HostPID            int                      `json:"host_pid"`
	StartTimeTick      uint64                   `json:"start_time_tick"`
	CgroupPath         string                   `json:"cgroup_path"`
	NonceSHA256        string                   `json:"nonce_sha256"`
	ExpiresAt          time.Time                `json:"expires_at"`
	DestroyedAt        time.Time                `json:"destroyed_at"`
	ProofSHA256        string                   `json:"proof_sha256"`
	Destroyed          bool                     `json:"destroyed"`
	Signature          []byte                   `json:"signature"`
}

type verifiedAccountAgentRuntimeStop struct {
	attestation AccountAgentRuntimeStopAttestation
	encoded     []byte
}

// VerifiedAccountAgentRuntimeStop can only be created by an app verifier.
type VerifiedAccountAgentRuntimeStop struct {
	value verifiedAccountAgentRuntimeStop
}

func (v VerifiedAccountAgentRuntimeStop) Value() (AccountAgentRuntimeStopAttestation, []byte) {
	attestation := v.value.attestation
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	return attestation, append([]byte(nil), v.value.encoded...)
}

type verifiedAccountAgentTaskStop struct {
	attestation AccountAgentTaskStopAttestation
	encoded     []byte
}

// VerifiedAccountAgentTaskStop can only be created by an app verifier.
type VerifiedAccountAgentTaskStop struct{ value verifiedAccountAgentTaskStop }

func (v VerifiedAccountAgentTaskStop) Value() (AccountAgentTaskStopAttestation, []byte) {
	attestation := v.value.attestation
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	return attestation, append([]byte(nil), v.value.encoded...)
}

// AccountAgentRuntimeGuardianStopAuthenticator verifies the public key of the
// Guardian, which is separate from supervisor credentials.
type AccountAgentRuntimeGuardianStopAuthenticator struct{ publicKey ed25519.PublicKey }

// AccountAgentTaskGuardianStopAuthenticator verifies the separate proof that
// one Task process has left its account Runtime container.
type AccountAgentTaskGuardianStopAuthenticator struct{ publicKey ed25519.PublicKey }

type AccountAgentTaskStopVerifier interface {
	VerifyAccountAgentTaskStop(context.Context, AccountAgentTaskStopAttestation) (VerifiedAccountAgentTaskStop, error)
}

type AccountAgentRuntimeStopVerifier interface {
	VerifyAccountAgentRuntimeStop(context.Context, AccountAgentRuntimeStopAttestation) (VerifiedAccountAgentRuntimeStop, error)
}

const MaxPendingAccountAgentRuntimeStopAttestations = 100

type AccountAgentRuntimeStore interface {
	StartAccountAgentRuntime(context.Context, AccountAgentRuntimeStart) (AccountAgentRuntime, bool, error)
	MarkAccountAgentRuntimeReady(context.Context, AccountAgentRuntimeFence) (AccountAgentRuntime, error)
	RenewAccountAgentRuntime(context.Context, AccountAgentRuntimeFence, time.Duration) (AccountAgentRuntime, error)
	GetAccountAgentRuntime(context.Context, string) (AccountAgentRuntime, bool, error)
	AccountAgentRuntimeStopIdentity(context.Context, string) (AccountAgentRuntimeStopIdentity, error)
	ListAccountAgentRuntimeStopCandidates(context.Context, int) ([]AccountAgentRuntimeStopIdentity, error)
	RequestAccountAgentRuntimeStop(context.Context, AccountAgentRuntimeFence) (AccountAgentRuntime, error)
	ConfirmAccountAgentRuntimeStopped(context.Context, VerifiedAccountAgentRuntimeStop) (bool, error)
}

type AccountAgentRuntimeStopAcknowledgementStore interface {
	ListPendingAccountAgentRuntimeStopAttestations(context.Context, int) ([]AccountAgentRuntimeStopAttestation, error)
	MarkAccountAgentRuntimeStopAttestationAcknowledged(context.Context, VerifiedAccountAgentRuntimeStop) (bool, error)
}

type AccountAgentRuntimeService struct{ store AccountAgentRuntimeStore }

func NewAccountAgentRuntimeService(store AccountAgentRuntimeStore) (*AccountAgentRuntimeService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("dependencies", "account runtime store is required")
	}
	return &AccountAgentRuntimeService{store: store}, nil
}

func NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey ed25519.PublicKey) (*AccountAgentRuntimeGuardianStopAuthenticator, error) {
	if len(publicKey) != ed25519.PublicKeySize || bytes.Equal(publicKey, make([]byte, ed25519.PublicKeySize)) {
		return nil, domain.InvalidArgument("guardian", "Guardian Ed25519 public key is required")
	}
	return &AccountAgentRuntimeGuardianStopAuthenticator{publicKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func NewAccountAgentTaskGuardianStopAuthenticator(publicKey ed25519.PublicKey) (*AccountAgentTaskGuardianStopAuthenticator, error) {
	if len(publicKey) != ed25519.PublicKeySize || bytes.Equal(publicKey, make([]byte, ed25519.PublicKeySize)) {
		return nil, domain.InvalidArgument("guardian", "Guardian Ed25519 public key is required")
	}
	return &AccountAgentTaskGuardianStopAuthenticator{publicKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func (a *AccountAgentRuntimeGuardianStopAuthenticator) VerifyAccountAgentRuntimeStop(ctx context.Context,
	attestation AccountAgentRuntimeStopAttestation) (VerifiedAccountAgentRuntimeStop, error) {
	if ctx == nil || a == nil || len(a.publicKey) != ed25519.PublicKeySize {
		return VerifiedAccountAgentRuntimeStop{}, domain.InvalidArgument("guardian", "Guardian verifier is unavailable")
	}
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	if err := validateAccountAgentRuntimeStopAttestation(attestation); err != nil {
		return VerifiedAccountAgentRuntimeStop{}, domain.NewError(domain.CodeUnauthenticated, "Guardian Runtime stop attestation is invalid")
	}
	payload, err := AccountAgentRuntimeStopAttestationSigningPayload(attestation)
	if err != nil {
		return VerifiedAccountAgentRuntimeStop{}, err
	}
	if !ed25519.Verify(a.publicKey, payload, attestation.Signature) {
		return VerifiedAccountAgentRuntimeStop{}, domain.NewError(domain.CodeUnauthenticated, "Guardian Runtime stop attestation is invalid")
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return VerifiedAccountAgentRuntimeStop{}, fmt.Errorf("encode Guardian Runtime stop attestation: %w", err)
	}
	return VerifiedAccountAgentRuntimeStop{value: verifiedAccountAgentRuntimeStop{attestation: attestation, encoded: encoded}}, nil
}

func (a *AccountAgentTaskGuardianStopAuthenticator) VerifyAccountAgentTaskStop(ctx context.Context,
	attestation AccountAgentTaskStopAttestation) (VerifiedAccountAgentTaskStop, error) {
	if ctx == nil || a == nil || len(a.publicKey) != ed25519.PublicKeySize {
		return VerifiedAccountAgentTaskStop{}, domain.InvalidArgument("guardian", "Guardian verifier is unavailable")
	}
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	if err := validateAccountAgentTaskStopAttestation(attestation); err != nil {
		return VerifiedAccountAgentTaskStop{}, domain.NewError(domain.CodeUnauthenticated, "Guardian Task stop attestation is invalid")
	}
	payload, err := AccountAgentTaskStopAttestationSigningPayload(attestation)
	if err != nil {
		return VerifiedAccountAgentTaskStop{}, err
	}
	if !ed25519.Verify(a.publicKey, payload, attestation.Signature) {
		return VerifiedAccountAgentTaskStop{}, domain.NewError(domain.CodeUnauthenticated, "Guardian Task stop attestation is invalid")
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return VerifiedAccountAgentTaskStop{}, fmt.Errorf("encode Guardian Task stop attestation: %w", err)
	}
	return VerifiedAccountAgentTaskStop{value: verifiedAccountAgentTaskStop{attestation: attestation, encoded: encoded}}, nil
}

func (s *AccountAgentRuntimeService) Get(ctx context.Context, ownerID string) (AccountAgentRuntime, bool, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return AccountAgentRuntime{}, false, domain.InvalidArgument("account_runtime", "account Runtime query is invalid")
	}
	return s.store.GetAccountAgentRuntime(ctx, ownerID)
}

func (s *AccountAgentRuntimeService) StopIdentity(ctx context.Context, ownerID string) (AccountAgentRuntimeStopIdentity, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) {
		return AccountAgentRuntimeStopIdentity{}, domain.InvalidArgument("account_runtime", "account Runtime recovery identity is invalid")
	}
	return s.store.AccountAgentRuntimeStopIdentity(ctx, ownerID)
}

func (s *AccountAgentRuntimeService) ListAccountAgentRuntimeStopCandidates(ctx context.Context, limit int) ([]AccountAgentRuntimeStopIdentity, error) {
	if ctx == nil || limit < 1 || limit > MaxAccountAgentRuntimeStopCandidates {
		return nil, domain.InvalidArgument("account_runtime", "account Runtime stop candidate query is invalid")
	}
	return s.store.ListAccountAgentRuntimeStopCandidates(ctx, limit)
}

func (s *AccountAgentRuntimeService) RequestStop(ctx context.Context, fence AccountAgentRuntimeFence) (AccountAgentRuntime, error) {
	if ctx == nil {
		return AccountAgentRuntime{}, domain.InvalidArgument("context", "account Runtime context is required")
	}
	if err := fence.Validate(); err != nil {
		return AccountAgentRuntime{}, err
	}
	return s.store.RequestAccountAgentRuntimeStop(ctx, fence)
}

func (s *AccountAgentRuntimeService) ConfirmGuardianStopped(ctx context.Context, attestation AccountAgentRuntimeStopAttestation,
	authenticator AccountAgentRuntimeStopVerifier) (bool, error) {
	if ctx == nil || authenticator == nil {
		return false, domain.InvalidArgument("guardian", "Guardian verifier is required")
	}
	verified, err := authenticator.VerifyAccountAgentRuntimeStop(ctx, attestation)
	if err != nil {
		return false, err
	}
	return s.store.ConfirmAccountAgentRuntimeStopped(ctx, verified)
}

func (s *AccountAgentRuntimeService) ListPendingAccountAgentRuntimeStopAttestations(ctx context.Context, limit int,
	authenticator AccountAgentRuntimeStopVerifier) ([]AccountAgentRuntimeStopAttestation, error) {
	if ctx == nil || limit < 1 || limit > MaxPendingAccountAgentRuntimeStopAttestations || authenticator == nil {
		return nil, domain.InvalidArgument("attestation", "pending Guardian Runtime stop query is invalid")
	}
	store, ok := s.store.(AccountAgentRuntimeStopAcknowledgementStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInvalidState, "Runtime stop acknowledgement store is unavailable")
	}
	attestations, err := store.ListPendingAccountAgentRuntimeStopAttestations(ctx, limit)
	if err != nil {
		return nil, err
	}
	for _, attestation := range attestations {
		if _, err := authenticator.VerifyAccountAgentRuntimeStop(ctx, attestation); err != nil {
			return nil, domain.NewError(domain.CodeInvalidState, "stored Guardian Runtime stop attestation is invalid")
		}
	}
	return attestations, nil
}

func (s *AccountAgentRuntimeService) MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx context.Context,
	attestation AccountAgentRuntimeStopAttestation, authenticator AccountAgentRuntimeStopVerifier) (bool, error) {
	if ctx == nil || authenticator == nil {
		return false, domain.InvalidArgument("guardian", "Guardian Runtime stop verifier is required")
	}
	store, ok := s.store.(AccountAgentRuntimeStopAcknowledgementStore)
	if !ok {
		return false, domain.NewError(domain.CodeInvalidState, "Runtime stop acknowledgement store is unavailable")
	}
	verified, err := authenticator.VerifyAccountAgentRuntimeStop(ctx, attestation)
	if err != nil {
		return false, err
	}
	return store.MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx, verified)
}

func (s *AccountAgentRuntimeService) Start(ctx context.Context, request AccountAgentRuntimeStart) (AccountAgentRuntime, bool, error) {
	if ctx == nil {
		return AccountAgentRuntime{}, false, domain.InvalidArgument("context", "account runtime context is required")
	}
	if err := request.Validate(); err != nil {
		return AccountAgentRuntime{}, false, err
	}
	request.SupervisorNonce = append([]byte(nil), request.SupervisorNonce...)
	return s.store.StartAccountAgentRuntime(ctx, request)
}

func (s *AccountAgentRuntimeService) Ready(ctx context.Context, fence AccountAgentRuntimeFence) (AccountAgentRuntime, error) {
	if ctx == nil {
		return AccountAgentRuntime{}, domain.InvalidArgument("context", "account runtime context is required")
	}
	if err := fence.Validate(); err != nil {
		return AccountAgentRuntime{}, err
	}
	return s.store.MarkAccountAgentRuntimeReady(ctx, fence)
}

func (s *AccountAgentRuntimeService) Renew(ctx context.Context, fence AccountAgentRuntimeFence, duration time.Duration) (AccountAgentRuntime, error) {
	if ctx == nil {
		return AccountAgentRuntime{}, domain.InvalidArgument("context", "account runtime context is required")
	}
	if err := fence.Validate(); err != nil || !validAgentLease(duration) {
		return AccountAgentRuntime{}, domain.InvalidArgument("account_runtime", "account runtime fence or lease duration is invalid")
	}
	return s.store.RenewAccountAgentRuntime(ctx, fence, duration)
}

func AccountAgentRuntimeStopAttestationSigningPayload(attestation AccountAgentRuntimeStopAttestation) ([]byte, error) {
	return json.Marshal(struct {
		Purpose       string                   `json:"purpose"`
		Fence         AccountAgentRuntimeFence `json:"fence"`
		ContainerID   string                   `json:"container_id"`
		ContainerName string                   `json:"container_name"`
		NonceSHA256   string                   `json:"nonce_sha256"`
		ExpiresAt     time.Time                `json:"expires_at"`
		DestroyedAt   time.Time                `json:"destroyed_at"`
		ProofSHA256   string                   `json:"proof_sha256"`
		Destroyed     bool                     `json:"destroyed"`
	}{
		Purpose: "morde.account-agent-runtime-guardian-stop.v1", Fence: attestation.Fence,
		ContainerID: attestation.ContainerID, ContainerName: attestation.ContainerName,
		NonceSHA256: attestation.NonceSHA256, ExpiresAt: attestation.ExpiresAt,
		DestroyedAt: attestation.DestroyedAt, ProofSHA256: attestation.ProofSHA256, Destroyed: attestation.Destroyed,
	})
}

func AccountAgentTaskStopAttestationSigningPayload(attestation AccountAgentTaskStopAttestation) ([]byte, error) {
	return json.Marshal(struct {
		Purpose            string                   `json:"purpose"`
		RuntimeFence       AccountAgentRuntimeFence `json:"runtime_fence"`
		TaskFence          AgentInstanceFence       `json:"task_fence"`
		RuntimeContainerID string                   `json:"runtime_container_id"`
		ContainerPID       int                      `json:"container_pid"`
		HostPID            int                      `json:"host_pid"`
		StartTimeTick      uint64                   `json:"start_time_tick"`
		CgroupPath         string                   `json:"cgroup_path"`
		NonceSHA256        string                   `json:"nonce_sha256"`
		ExpiresAt          time.Time                `json:"expires_at"`
		DestroyedAt        time.Time                `json:"destroyed_at"`
		ProofSHA256        string                   `json:"proof_sha256"`
		Destroyed          bool                     `json:"destroyed"`
	}{
		Purpose: "morde.account-agent-task-guardian-stop.v1", RuntimeFence: attestation.RuntimeFence,
		TaskFence: attestation.TaskFence, RuntimeContainerID: attestation.RuntimeContainerID,
		ContainerPID: attestation.ContainerPID, HostPID: attestation.HostPID, StartTimeTick: attestation.StartTimeTick,
		CgroupPath: attestation.CgroupPath, NonceSHA256: attestation.NonceSHA256, ExpiresAt: attestation.ExpiresAt,
		DestroyedAt: attestation.DestroyedAt, ProofSHA256: attestation.ProofSHA256, Destroyed: attestation.Destroyed,
	})
}

func validateAccountAgentRuntimeStopAttestation(attestation AccountAgentRuntimeStopAttestation) error {
	if err := attestation.Fence.Validate(); err != nil || !attestation.Destroyed || !isLowerHex(attestation.ContainerID) ||
		len(attestation.ContainerID) != sha256.Size*2 || !isLowerHex(attestation.NonceSHA256) ||
		len(attestation.NonceSHA256) != sha256.Size*2 || !isLowerHex(attestation.ProofSHA256) ||
		len(attestation.ProofSHA256) != sha256.Size*2 || attestation.ContainerName != expectedAccountAgentRuntimeContainerName(attestation.Fence, attestation.NonceSHA256) ||
		attestation.ExpiresAt.IsZero() || attestation.DestroyedAt.IsZero() || len(attestation.Signature) != ed25519.SignatureSize {
		return domain.InvalidArgument("attestation", "account Runtime stop attestation is invalid")
	}
	return nil
}

func validateAccountAgentTaskStopAttestation(attestation AccountAgentTaskStopAttestation) error {
	if err := attestation.RuntimeFence.Validate(); err != nil || attestation.TaskFence.Validate() != nil ||
		attestation.TaskFence.OwnerID != attestation.RuntimeFence.OwnerID || !attestation.Destroyed ||
		!isLowerHex(attestation.RuntimeContainerID) || len(attestation.RuntimeContainerID) != sha256.Size*2 ||
		attestation.ContainerPID < 1 || attestation.HostPID < 1 || attestation.StartTimeTick == 0 ||
		!validCgroupPath(attestation.CgroupPath) || !isLowerHex(attestation.NonceSHA256) ||
		len(attestation.NonceSHA256) != sha256.Size*2 || !isLowerHex(attestation.ProofSHA256) ||
		len(attestation.ProofSHA256) != sha256.Size*2 || attestation.ExpiresAt.IsZero() ||
		attestation.DestroyedAt.IsZero() || len(attestation.Signature) != ed25519.SignatureSize {
		return domain.InvalidArgument("attestation", "account Task stop attestation is invalid")
	}
	return nil
}

func expectedAccountAgentRuntimeContainerName(fence AccountAgentRuntimeFence, nonceSHA256 string) string {
	encoded, err := json.Marshal(fence)
	if err != nil {
		return ""
	}
	nonce, err := hex.DecodeString(nonceSHA256)
	if err != nil {
		return ""
	}
	identity := sha256.Sum256(append(append(encoded, 0), nonce...))
	return "morde-account-runtime-" + hex.EncodeToString(identity[:])
}

func validCgroupPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && strings.TrimSpace(path) == path &&
		!strings.ContainsAny(path, "\x00\r\n")
}
