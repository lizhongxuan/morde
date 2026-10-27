package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	SupervisorNonce []byte
	LeaseDuration   time.Duration
}

func (r AgentInstanceStart) Validate() error {
	if err := validateAgentInstanceFence(r.Fence, false); err != nil {
		return err
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
	Fence     AgentInstanceFence
	ExpiresAt time.Time
}

type AgentStopReceipt struct {
	Fence           AgentInstanceFence `json:"fence"`
	SupervisorNonce []byte             `json:"supervisor_nonce"`
	TreeStopped     bool               `json:"tree_stopped"`
	Signature       []byte             `json:"signature"`
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

type AgentInstanceStore interface {
	StartAgentInstance(context.Context, AgentInstanceStart) (AgentInstanceLease, bool, error)
	RenewAgentInstance(context.Context, AgentInstanceFence, time.Duration) (AgentInstanceLease, error)
	ConfirmAgentStopped(context.Context, VerifiedAgentStop) (bool, error)
}

// AgentStopAuthenticator verifies receipts issued by the separately trusted supervisor.
// Its key must never be made available to dsh or exposed through HTTP.
type AgentStopAuthenticator struct {
	supervisorID string
	key          []byte
}

func NewAgentStopAuthenticator(supervisorID string, key []byte) (*AgentStopAuthenticator, error) {
	if !agentCheckpointIDPattern.MatchString(supervisorID) || len(key) < 32 || bytes.Equal(key, make([]byte, len(key))) {
		return nil, domain.InvalidArgument("supervisor", "supervisor identity and a 32-byte key are required")
	}
	return &AgentStopAuthenticator{supervisorID: supervisorID, key: append([]byte(nil), key...)}, nil
}

type AgentInstanceService struct {
	store         AgentInstanceStore
	authenticator *AgentStopAuthenticator
}

func NewAgentInstanceService(store AgentInstanceStore, authenticator *AgentStopAuthenticator) (*AgentInstanceService, error) {
	if store == nil || authenticator == nil {
		return nil, domain.InvalidArgument("dependencies", "agent instance dependencies are required")
	}
	return &AgentInstanceService{store: store, authenticator: authenticator}, nil
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

// AgentStopReceiptSigningPayload defines the bytes the trusted supervisor signs.
func AgentStopReceiptSigningPayload(receipt AgentStopReceipt) ([]byte, error) {
	return agentStopSigningPayload(receipt)
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
