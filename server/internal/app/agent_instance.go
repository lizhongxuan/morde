package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
)

const MaxAgentLease = 30 * time.Second

type AgentInstanceFence struct {
	TaskID               string `json:"task_id"`
	OwnerID              string `json:"owner_id"`
	AgentInstanceID      string `json:"agent_instance_id"`
	AgentGeneration      int64  `json:"agent_generation"`
	AgentSessionID       string `json:"agent_session_id"`
	SupervisorID         string `json:"supervisor_id"`
	LeaseOwner           string `json:"lease_owner"`
	LeaseVersion         int64  `json:"lease_version"`
	AuthorizationVersion int64  `json:"authorization_version"`
	EngineVersion        string `json:"engine_version"`
	ProfileVersion       string `json:"profile_version"`
}

type AgentInstanceStart struct {
	Fence           AgentInstanceFence
	RuntimeFence    *AccountAgentRuntimeFence
	SupervisorNonce []byte
	LeaseDuration   time.Duration
}

func (r AgentInstanceStart) Validate() error {
	if err := validateAgentInstanceFence(r.Fence, false); err != nil {
		return err
	}
	if r.RuntimeFence != nil {
		if err := r.RuntimeFence.Validate(); err != nil || r.RuntimeFence.OwnerID != r.Fence.OwnerID {
			return domain.InvalidArgument("agent_instance", "account runtime fence is invalid")
		}
	}
	if len(r.SupervisorNonce) != 32 || bytes.Equal(r.SupervisorNonce, make([]byte, 32)) || !validAgentLease(r.LeaseDuration) {
		return domain.InvalidArgument("agent_instance", "supervisor nonce or lease duration is invalid")
	}
	return nil
}

func (f AgentInstanceFence) Validate() error {
	return validateAgentInstanceFence(f, true)
}

type AgentInstanceLease struct {
	Fence           AgentInstanceFence
	RuntimeFence    *AccountAgentRuntimeFence
	RuntimeSequence int64
	ExpiresAt       time.Time
}

type AgentGuardianStopIdentity struct {
	Fence       AgentInstanceFence
	NonceSHA256 string
}

type AgentStopReceipt struct {
	Fence           AgentInstanceFence `json:"fence"`
	SupervisorNonce []byte             `json:"supervisor_nonce"`
	TreeStopped     bool               `json:"tree_stopped"`
	Signature       []byte             `json:"signature"`
}

// AgentGuardianStopAttestation is a Guardian-signed proof that the exact
// fenced Agent container and its process tree were independently destroyed.
// It intentionally contains only the nonce digest, never the raw nonce.
type AgentGuardianStopAttestation struct {
	Fence         AgentInstanceFence `json:"fence"`
	ContainerID   string             `json:"container_id"`
	ContainerName string             `json:"container_name"`
	NonceSHA256   string             `json:"nonce_sha256"`
	ExpiresAt     time.Time          `json:"expires_at"`
	DestroyedAt   time.Time          `json:"destroyed_at"`
	ProofSHA256   string             `json:"proof_sha256"`
	Destroyed     bool               `json:"destroyed"`
	Signature     []byte             `json:"signature"`
}

type verifiedAgentStop struct {
	receipt AgentStopReceipt
	encoded []byte
}

// VerifiedAgentStop cannot be constructed with a valid receipt outside this package.
type VerifiedAgentStop struct{ value verifiedAgentStop }

func (v VerifiedAgentStop) Value() (AgentStopReceipt, []byte) {
	return v.value.receipt, append([]byte(nil), v.value.encoded...)
}

type verifiedAgentGuardianStop struct {
	attestation AgentGuardianStopAttestation
	encoded     []byte
}

// VerifiedAgentGuardianStop cannot be constructed with a valid attestation
// outside this package.
type VerifiedAgentGuardianStop struct{ value verifiedAgentGuardianStop }

func (v VerifiedAgentGuardianStop) Value() (AgentGuardianStopAttestation, []byte) {
	attestation := v.value.attestation
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	return attestation, append([]byte(nil), v.value.encoded...)
}

type AgentInstanceStore interface {
	StartAgentInstance(context.Context, AgentInstanceStart) (AgentInstanceLease, bool, error)
	RenewAgentInstance(context.Context, AgentInstanceFence, time.Duration) (AgentInstanceLease, error)
	ConfirmAgentStopped(context.Context, VerifiedAgentStop) (bool, error)
}

type AgentInstanceTaskStopStore interface {
	ConfirmAccountAgentTaskStopped(context.Context, VerifiedAccountAgentTaskStop) (bool, error)
}

type AccountAgentTaskStopProofAcknowledgementStore interface {
	ListPendingAccountAgentTaskStopAttestations(context.Context, int) ([]AccountAgentTaskStopAttestation, error)
	MarkAccountAgentTaskStopAttestationAcknowledged(context.Context, VerifiedAccountAgentTaskStop) (bool, error)
}

const MaxPendingAccountAgentTaskStopAttestations = 100

type AgentGuardianStopRecoveryStore interface {
	ActiveAgentGuardianStopIdentity(context.Context, string, string, string, int64) (AgentGuardianStopIdentity, error)
}

// AgentStopAuthenticator verifies receipts issued by the separately trusted supervisor.
// Its key must never be made available to dsh or exposed through HTTP.
type AgentStopAuthenticator struct {
	supervisorID string
	key          []byte
}

// AgentGuardianStopAuthenticator verifies a public key that is separate from
// the Portal-to-supervisor HMAC key.
type AgentGuardianStopAuthenticator struct {
	supervisorID string
	publicKey    ed25519.PublicKey
}

func NewAgentStopAuthenticator(supervisorID string, key []byte) (*AgentStopAuthenticator, error) {
	if !agentCheckpointIDPattern.MatchString(supervisorID) || len(key) < 32 || bytes.Equal(key, make([]byte, len(key))) {
		return nil, domain.InvalidArgument("supervisor", "supervisor identity and a 32-byte key are required")
	}
	return &AgentStopAuthenticator{supervisorID: supervisorID, key: append([]byte(nil), key...)}, nil
}

func NewAgentGuardianStopAuthenticator(supervisorID string, publicKey ed25519.PublicKey) (*AgentGuardianStopAuthenticator, error) {
	if !agentCheckpointIDPattern.MatchString(supervisorID) || len(publicKey) != ed25519.PublicKeySize || bytes.Equal(publicKey, make([]byte, ed25519.PublicKeySize)) {
		return nil, domain.InvalidArgument("guardian", "guardian identity and Ed25519 public key are required")
	}
	return &AgentGuardianStopAuthenticator{supervisorID: supervisorID, publicKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

type AgentInstanceService struct {
	store                            AgentInstanceStore
	authenticator                    *AgentStopAuthenticator
	guardianAuthenticator            *AgentGuardianStopAuthenticator
	accountTaskGuardianAuthenticator *AccountAgentTaskGuardianStopAuthenticator
}

func NewAgentInstanceService(store AgentInstanceStore, authenticator *AgentStopAuthenticator) (*AgentInstanceService, error) {
	if store == nil || authenticator == nil {
		return nil, domain.InvalidArgument("dependencies", "agent instance dependencies are required")
	}
	return &AgentInstanceService{store: store, authenticator: authenticator}, nil
}

func NewAgentInstanceServiceWithGuardianStop(store AgentInstanceStore, supervisor *AgentStopAuthenticator, guardian *AgentGuardianStopAuthenticator) (*AgentInstanceService, error) {
	service, err := NewAgentInstanceService(store, supervisor)
	if err != nil {
		return nil, err
	}
	if guardian == nil || guardian.supervisorID != supervisor.supervisorID ||
		bytes.Equal(guardian.publicKey, supervisor.key) || bytes.Equal(guardian.publicKey, supervisor.key[:ed25519.PublicKeySize]) {
		return nil, domain.InvalidArgument("guardian", "guardian key must be distinct and match the supervisor identity")
	}
	service.guardianAuthenticator = guardian
	return service, nil
}

func NewAgentInstanceServiceWithGuardianStops(store AgentInstanceStore, supervisor *AgentStopAuthenticator,
	agentGuardian *AgentGuardianStopAuthenticator, accountTaskGuardian *AccountAgentTaskGuardianStopAuthenticator) (*AgentInstanceService, error) {
	service, err := NewAgentInstanceServiceWithGuardianStop(store, supervisor, agentGuardian)
	if err != nil {
		return nil, err
	}
	if accountTaskGuardian == nil || !bytes.Equal(accountTaskGuardian.publicKey, agentGuardian.publicKey) {
		return nil, domain.InvalidArgument("guardian", "account Task and Agent proofs must use the configured Guardian key")
	}
	service.accountTaskGuardianAuthenticator = accountTaskGuardian
	return service, nil
}

func (s *AgentInstanceService) Start(ctx context.Context, request AgentInstanceStart) (AgentInstanceLease, bool, error) {
	if err := request.Validate(); err != nil {
		return AgentInstanceLease{}, false, err
	}
	if request.Fence.SupervisorID != s.authenticator.supervisorID {
		return AgentInstanceLease{}, false, domain.InvalidArgument("agent_instance", "supervisor identity is invalid")
	}
	request.SupervisorNonce = append([]byte(nil), request.SupervisorNonce...)
	return s.store.StartAgentInstance(ctx, request)
}

func (s *AgentInstanceService) Renew(ctx context.Context, fence AgentInstanceFence, duration time.Duration) (AgentInstanceLease, error) {
	if err := validateAgentInstanceFence(fence, true); err != nil {
		return AgentInstanceLease{}, err
	}
	if fence.SupervisorID != s.authenticator.supervisorID || !validAgentLease(duration) {
		return AgentInstanceLease{}, domain.InvalidArgument("agent_instance", "supervisor or lease duration is invalid")
	}
	return s.store.RenewAgentInstance(ctx, fence, duration)
}

func (s *AgentInstanceService) ConfirmStopped(ctx context.Context, receipt AgentStopReceipt) (bool, error) {
	receipt.SupervisorNonce = append([]byte(nil), receipt.SupervisorNonce...)
	receipt.Signature = append([]byte(nil), receipt.Signature...)
	if err := validateAgentInstanceFence(receipt.Fence, true); err != nil {
		return false, err
	}
	if receipt.Fence.SupervisorID != s.authenticator.supervisorID || len(receipt.SupervisorNonce) != 32 ||
		bytes.Equal(receipt.SupervisorNonce, make([]byte, 32)) || !receipt.TreeStopped || len(receipt.Signature) != sha256.Size {
		return false, domain.NewError(domain.CodeUnauthenticated, "supervisor stop receipt is invalid")
	}
	payload, err := agentStopSigningPayload(receipt)
	if err != nil {
		return false, err
	}
	mac := hmac.New(sha256.New, s.authenticator.key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), receipt.Signature) {
		return false, domain.NewError(domain.CodeUnauthenticated, "supervisor stop receipt is invalid")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, fmt.Errorf("encode supervisor stop receipt: %w", err)
	}
	return s.store.ConfirmAgentStopped(ctx, VerifiedAgentStop{value: verifiedAgentStop{receipt: receipt, encoded: encoded}})
}

func (s *AgentInstanceService) ConfirmGuardianStopped(ctx context.Context, attestation AgentGuardianStopAttestation) (bool, error) {
	if s.guardianAuthenticator == nil {
		return false, domain.NewError(domain.CodeInvalidState, "guardian stop verification is unavailable")
	}
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	if err := validateAgentInstanceFence(attestation.Fence, true); err != nil {
		return false, err
	}
	if attestation.Fence.SupervisorID != s.guardianAuthenticator.supervisorID || !attestation.Destroyed ||
		len(attestation.ContainerID) != 64 || !isLowerHex(attestation.ContainerID) ||
		!strings.HasPrefix(attestation.ContainerName, "morde-dsh-") || len(attestation.ContainerName) != len("morde-dsh-")+64 ||
		!isLowerHex(strings.TrimPrefix(attestation.ContainerName, "morde-dsh-")) ||
		!isLowerHex(attestation.NonceSHA256) || len(attestation.NonceSHA256) != sha256.Size*2 ||
		!isLowerHex(attestation.ProofSHA256) || len(attestation.ProofSHA256) != sha256.Size*2 ||
		attestation.ExpiresAt.IsZero() || attestation.DestroyedAt.IsZero() || len(attestation.Signature) != ed25519.SignatureSize {
		return false, domain.NewError(domain.CodeUnauthenticated, "guardian stop attestation is invalid")
	}
	payload, err := AgentGuardianStopAttestationSigningPayload(attestation)
	if err != nil {
		return false, err
	}
	if !ed25519.Verify(s.guardianAuthenticator.publicKey, payload, attestation.Signature) {
		return false, domain.NewError(domain.CodeUnauthenticated, "guardian stop attestation is invalid")
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return false, fmt.Errorf("encode guardian stop attestation: %w", err)
	}
	store, ok := s.store.(interface {
		ConfirmAgentGuardianStopped(context.Context, VerifiedAgentGuardianStop) (bool, error)
	})
	if !ok {
		return false, domain.NewError(domain.CodeInvalidState, "guardian stop persistence is unavailable")
	}
	return store.ConfirmAgentGuardianStopped(ctx, VerifiedAgentGuardianStop{value: verifiedAgentGuardianStop{attestation: attestation, encoded: encoded}})
}

func (s *AgentInstanceService) ConfirmTaskStopped(ctx context.Context, attestation AccountAgentTaskStopAttestation) (bool, error) {
	if ctx == nil || s.accountTaskGuardianAuthenticator == nil {
		return false, domain.NewError(domain.CodeInvalidState, "account Task Guardian stop verification is unavailable")
	}
	attestation.Signature = append([]byte(nil), attestation.Signature...)
	verified, err := s.accountTaskGuardianAuthenticator.VerifyAccountAgentTaskStop(ctx, attestation)
	if err != nil {
		return false, err
	}
	store, ok := s.store.(AgentInstanceTaskStopStore)
	if !ok {
		return false, domain.NewError(domain.CodeInvalidState, "account Task stop persistence is unavailable")
	}
	return store.ConfirmAccountAgentTaskStopped(ctx, verified)
}

func (s *AgentInstanceService) ListPendingAccountAgentTaskStopAttestations(ctx context.Context, limit int) ([]AccountAgentTaskStopAttestation, error) {
	if ctx == nil || s.accountTaskGuardianAuthenticator == nil || limit < 1 || limit > MaxPendingAccountAgentTaskStopAttestations {
		return nil, domain.InvalidArgument("attestation", "pending Guardian Task stop query is invalid")
	}
	store, ok := s.store.(AccountAgentTaskStopProofAcknowledgementStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInvalidState, "Guardian Task stop acknowledgement store is unavailable")
	}
	attestations, err := store.ListPendingAccountAgentTaskStopAttestations(ctx, limit)
	if err != nil {
		return nil, err
	}
	for _, attestation := range attestations {
		if _, err := s.accountTaskGuardianAuthenticator.VerifyAccountAgentTaskStop(ctx, attestation); err != nil {
			return nil, domain.NewError(domain.CodeInternal, "stored Guardian Task stop attestation is invalid")
		}
	}
	return attestations, nil
}

func (s *AgentInstanceService) MarkAccountAgentTaskStopAttestationAcknowledged(ctx context.Context,
	attestation AccountAgentTaskStopAttestation) (bool, error) {
	if ctx == nil || s.accountTaskGuardianAuthenticator == nil {
		return false, domain.InvalidArgument("attestation", "Guardian Task stop acknowledgement is invalid")
	}
	verified, err := s.accountTaskGuardianAuthenticator.VerifyAccountAgentTaskStop(ctx, attestation)
	if err != nil {
		return false, err
	}
	store, ok := s.store.(AccountAgentTaskStopProofAcknowledgementStore)
	if !ok {
		return false, domain.NewError(domain.CodeInvalidState, "Guardian Task stop acknowledgement store is unavailable")
	}
	return store.MarkAccountAgentTaskStopAttestationAcknowledged(ctx, verified)
}

func (s *AgentInstanceService) GuardianStopIdentity(ctx context.Context, taskID, ownerID, agentInstanceID string, generation int64) (AgentGuardianStopIdentity, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) ||
		!agentCheckpointIDPattern.MatchString(agentInstanceID) || generation < 1 {
		return AgentGuardianStopIdentity{}, domain.InvalidArgument("agent_instance", "current Agent recovery identity is invalid")
	}
	store, ok := s.store.(AgentGuardianStopRecoveryStore)
	if !ok {
		return AgentGuardianStopIdentity{}, domain.NewError(domain.CodeInvalidState, "Guardian recovery lookup is unavailable")
	}
	identity, err := store.ActiveAgentGuardianStopIdentity(ctx, taskID, ownerID, agentInstanceID, generation)
	if err != nil {
		return AgentGuardianStopIdentity{}, err
	}
	if identity.Fence.TaskID != taskID || identity.Fence.OwnerID != ownerID || identity.Fence.AgentInstanceID != agentInstanceID ||
		identity.Fence.AgentGeneration != generation || !isLowerHex(identity.NonceSHA256) || len(identity.NonceSHA256) != sha256.Size*2 {
		return AgentGuardianStopIdentity{}, domain.NewError(domain.CodeInternal, "agent recovery identity is inconsistent")
	}
	return identity, nil
}

// AgentStopReceiptSigningPayload defines the bytes the trusted supervisor signs.
func AgentStopReceiptSigningPayload(receipt AgentStopReceipt) ([]byte, error) {
	return agentStopSigningPayload(receipt)
}

// AgentGuardianStopAttestationSigningPayload defines the exact Ed25519-signed
// Guardian proof payload. The signature itself is excluded.
func AgentGuardianStopAttestationSigningPayload(attestation AgentGuardianStopAttestation) ([]byte, error) {
	value, err := json.Marshal(struct {
		Purpose       string             `json:"purpose"`
		Fence         AgentInstanceFence `json:"fence"`
		ContainerID   string             `json:"container_id"`
		ContainerName string             `json:"container_name"`
		NonceSHA256   string             `json:"nonce_sha256"`
		ExpiresAt     time.Time          `json:"expires_at"`
		DestroyedAt   time.Time          `json:"destroyed_at"`
		ProofSHA256   string             `json:"proof_sha256"`
		Destroyed     bool               `json:"destroyed"`
	}{
		Purpose: "morde.agent-guardian-stop.v1", Fence: attestation.Fence,
		ContainerID: attestation.ContainerID, ContainerName: attestation.ContainerName,
		NonceSHA256: attestation.NonceSHA256, ExpiresAt: attestation.ExpiresAt,
		DestroyedAt: attestation.DestroyedAt, ProofSHA256: attestation.ProofSHA256, Destroyed: attestation.Destroyed,
	})
	if err != nil {
		return nil, fmt.Errorf("encode guardian stop attestation payload: %w", err)
	}
	return value, nil
}

func agentStopSigningPayload(receipt AgentStopReceipt) ([]byte, error) {
	value, err := json.Marshal(struct {
		Purpose         string             `json:"purpose"`
		Fence           AgentInstanceFence `json:"fence"`
		SupervisorNonce []byte             `json:"supervisor_nonce"`
		TreeStopped     bool               `json:"tree_stopped"`
	}{Purpose: "morde.agent-stop.v1", Fence: receipt.Fence, SupervisorNonce: receipt.SupervisorNonce, TreeStopped: receipt.TreeStopped})
	if err != nil {
		return nil, fmt.Errorf("encode supervisor stop receipt payload: %w", err)
	}
	return value, nil
}

func isLowerHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func validateAgentInstanceFence(fence AgentInstanceFence, existing bool) error {
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) ||
		!agentCheckpointIDPattern.MatchString(fence.AgentInstanceID) || !agentCheckpointIDPattern.MatchString(fence.AgentSessionID) ||
		!agentCheckpointIDPattern.MatchString(fence.SupervisorID) || invalidCheckpointLabel(fence.LeaseOwner, 255) ||
		fence.LeaseVersion < 1 || fence.AuthorizationVersion < 1 || invalidCheckpointLabel(fence.EngineVersion, 128) ||
		invalidCheckpointLabel(fence.ProfileVersion, 128) || (existing && fence.AgentGeneration < 1) || (!existing && fence.AgentGeneration != 0) {
		return domain.InvalidArgument("agent_instance", "agent instance fields are invalid")
	}
	return nil
}

func validAgentLease(duration time.Duration) bool {
	return duration >= time.Millisecond && duration <= MaxAgentLease
}
