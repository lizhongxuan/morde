package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

const cloudDSHTaskPlanHistorySourceProofVersion = 1
const cloudDSHTaskPlanHistorySourceProofMACDomain = "morde/cloud-dsh/task-plan-history-source-proof/v1\x00"

// cloudDSHTaskPlanHistorySourceProof is signed before a historical user
// message reference is returned to DSH. It proves that the exact message ID
// and source digest appeared in an authorized search result for this Task.
// The plan tool still asks the Store to re-read and hash the message before it
// persists a canonical source receipt.
type cloudDSHTaskPlanHistorySourceProof struct {
	Version              int    `json:"version"`
	ServiceInstanceID    string `json:"service_instance_id"`
	TaskID               string `json:"task_id"`
	OwnerID              string `json:"owner_id"`
	InitiatorID          string `json:"initiator_id"`
	ProjectID            string `json:"project_id,omitempty"`
	ConversationID       string `json:"conversation_id"`
	AgentSessionID       string `json:"agent_session_id"`
	TaskRevision         int64  `json:"task_revision"`
	AuthorizationVersion int64  `json:"authorization_version"`
	ConversationScope    string `json:"conversation_scope"`
	ConversationSHA256   string `json:"conversation_sha256"`
	ThroughSequence      int64  `json:"through_sequence"`
	MessageID            string `json:"message_id"`
	MessageSequence      int64  `json:"message_sequence"`
	MessageContentSHA256 string `json:"message_content_sha256"`
}

// signCloudDSHTaskPlanHistorySource signs a user message match for use as a
// required-check source. History search may still return excerpts without a
// proof when the service signing key is unavailable; those excerpts cannot be
// promoted to a verified plan source.
func (b *CloudDSHToolBroker) signCloudDSHTaskPlanHistorySource(current task.Task,
	match cloudDSHCurrentHistoryMatch) (string, error) {
	if len(b.historyCursorKey) != sha256.Size || !domain.ValidServiceInstanceID(b.historyCursorInstanceID) {
		return "", nil
	}
	proof, ok := cloudDSHTaskPlanHistoryProofForMatch(b.historyCursorInstanceID, current, match)
	if !ok {
		return "", domain.NewError(domain.CodeInvalidState, "authorized history result cannot be bound to a Task source")
	}
	payload, err := json.Marshal(proof)
	if err != nil || len(payload) > task.PlanVerificationRequirementHistoryResultTokenMaxBytes {
		return "", domain.NewError(domain.CodeInternal, "authorized history result source proof could not be created")
	}
	mac := hmac.New(sha256.New, b.historyCursorKey)
	_, _ = mac.Write([]byte(cloudDSHTaskPlanHistorySourceProofMACDomain))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// resolveCloudDSHTaskPlanHistorySource verifies that a token was issued for a
// visible result of this Task's authorized, watermark-bounded history search.
// Store resolution remains authoritative for current content, ownership, role,
// delivery status and digest.
func (b *CloudDSHToolBroker) resolveCloudDSHTaskPlanHistorySource(ctx context.Context, current task.Task,
	token string) (task.PlanVerificationRequirementSource, error) {
	if ctx == nil {
		return task.PlanVerificationRequirementSource{}, domain.InvalidArgument("history source proof", "context is required")
	}
	if err := ctx.Err(); err != nil {
		return task.PlanVerificationRequirementSource{}, err
	}
	proof, err := b.verifyCloudDSHTaskPlanHistorySource(current, token)
	if err != nil {
		return task.PlanVerificationRequirementSource{}, err
	}
	return task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		MessageID: proof.MessageID, ContentSHA256: proof.MessageContentSHA256}, nil
}

func (b *CloudDSHToolBroker) verifyCloudDSHTaskPlanHistorySource(current task.Task,
	token string) (cloudDSHTaskPlanHistorySourceProof, error) {
	if len(token) == 0 || len(token) > task.PlanVerificationRequirementHistoryResultTokenMaxBytes ||
		len(b.historyCursorKey) != sha256.Size || !domain.ValidServiceInstanceID(b.historyCursorInstanceID) {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is unavailable")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > task.PlanVerificationRequirementHistoryResultTokenMaxBytes ||
		base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is invalid")
	}
	mac := hmac.New(sha256.New, b.historyCursorKey)
	_, _ = mac.Write([]byte(cloudDSHTaskPlanHistorySourceProofMACDomain))
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is invalid")
	}
	var proof cloudDSHTaskPlanHistorySourceProof
	if decodeStrictRuntimeResult(payload, &proof) != nil {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof is invalid")
	}
	canonical, err := json.Marshal(proof)
	if err != nil || !hmac.Equal(canonical, payload) || !cloudDSHTaskPlanHistoryProofMatchesTask(b.historyCursorInstanceID, current, proof) {
		return cloudDSHTaskPlanHistorySourceProof{}, domain.NewError(domain.CodeInvalidArgument, "history source proof does not belong to this Task")
	}
	return proof, nil
}

func cloudDSHTaskPlanHistoryProofForMatch(serviceInstanceID string, current task.Task,
	match cloudDSHCurrentHistoryMatch) (cloudDSHTaskPlanHistorySourceProof, bool) {
	if match.Role != conversation.RoleUser || !domain.ValidMessageID(match.MessageID) || match.Sequence < 1 ||
		!cloudDSHValidSHA256Hex(match.ContentSHA256) || !cloudDSHTaskPlanHistoryCurrentTaskValid(serviceInstanceID, current) ||
		match.Sequence > current.ConversationContext.ThroughSequence {
		return cloudDSHTaskPlanHistorySourceProof{}, false
	}
	return cloudDSHTaskPlanHistorySourceProof{
		Version: cloudDSHTaskPlanHistorySourceProofVersion, ServiceInstanceID: serviceInstanceID,
		TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		ProjectID: current.ProjectID, ConversationID: current.ConversationID, AgentSessionID: current.AgentSessionID,
		TaskRevision: *current.CurrentRevision, AuthorizationVersion: current.Authorization.Version,
		ConversationScope:  current.ConversationContext.Scope,
		ConversationSHA256: hex.EncodeToString(current.ConversationContext.Digest),
		ThroughSequence:    current.ConversationContext.ThroughSequence, MessageID: match.MessageID,
		MessageSequence: match.Sequence, MessageContentSHA256: match.ContentSHA256,
	}, true
}

func cloudDSHTaskPlanHistoryProofMatchesTask(serviceInstanceID string, current task.Task,
	proof cloudDSHTaskPlanHistorySourceProof) bool {
	if proof.Version != cloudDSHTaskPlanHistorySourceProofVersion || proof.ServiceInstanceID != serviceInstanceID ||
		proof.MessageSequence < 1 || !domain.ValidMessageID(proof.MessageID) ||
		!cloudDSHValidSHA256Hex(proof.MessageContentSHA256) || !cloudDSHTaskPlanHistoryCurrentTaskValid(serviceInstanceID, current) {
		return false
	}
	return proof.TaskID == current.ID && proof.OwnerID == current.OwnerID && proof.InitiatorID == current.InitiatorID &&
		proof.ProjectID == current.ProjectID && proof.ConversationID == current.ConversationID &&
		proof.AgentSessionID == current.AgentSessionID && proof.TaskRevision == *current.CurrentRevision &&
		proof.AuthorizationVersion == current.Authorization.Version && proof.ConversationScope == current.ConversationContext.Scope &&
		proof.ConversationSHA256 == hex.EncodeToString(current.ConversationContext.Digest) &&
		proof.ThroughSequence == current.ConversationContext.ThroughSequence &&
		proof.MessageSequence <= proof.ThroughSequence
}

func cloudDSHTaskPlanHistoryCurrentTaskValid(serviceInstanceID string, current task.Task) bool {
	if !domain.ValidServiceInstanceID(serviceInstanceID) || !domain.ValidTaskID(current.ID) ||
		!domain.ValidUserID(current.OwnerID) || !domain.ValidUserID(current.InitiatorID) ||
		!domain.ValidConversationID(current.ConversationID) || current.Status != task.StatusRunning ||
		current.CurrentRevision == nil || *current.CurrentRevision < 0 || current.AgentSessionID == "" ||
		current.Authorization.Version < 1 || !cloudDSHCurrentConversationHistoryEligible(current) ||
		len(current.ConversationContext.Digest) != sha256.Size {
		return false
	}
	if current.Authorization.ExpiresAt != nil && !current.Authorization.ExpiresAt.After(time.Now()) {
		return false
	}
	return true
}
