package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const MaxTaskTerminalDeliveryModelResponseCiphertext = 1 << 20
const MaxTaskTerminalDeliveryModelResponseTextBytes = 16 << 10

// TaskTerminalDeliveryModelFence carries the immutable outbox source identity
// plus the active claim lease. The lease fields authorize dispatch; the stable
// call ID is tied only to DeliveryID so a later claim can recover the same
// provider result.
type TaskTerminalDeliveryModelFence struct {
	DeliveryID                int64
	TaskID                    string
	OwnerID                   string
	ConversationID            string
	InitiatorID               string
	TaskRevision              int64
	AuthorizationVersion      int64
	SourceKind                string
	SourceEventSequence       int64
	SourceEventType           string
	SourceSHA256              []byte
	ResultSHA256              []byte
	ValidationClaimGeneration *int64
	VerdictSHA256             []byte
	ClaimGeneration           int64
	LeaseOwner                string
	LeaseExpiresAt            time.Time
	ModelCallID               string
}

func ValidTaskTerminalDeliveryModelFence(value TaskTerminalDeliveryModelFence) bool {
	callID := "terminal_delivery_" + strconv.FormatInt(value.DeliveryID, 10)
	if value.DeliveryID < 1 || !domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidConversationID(value.ConversationID) || !domain.ValidUserID(value.InitiatorID) ||
		value.TaskRevision < 0 || value.AuthorizationVersion < 1 || value.SourceEventSequence < 1 ||
		len(value.SourceSHA256) != sha256.Size || value.ClaimGeneration < 1 ||
		(value.LeaseOwner != "" && (strings.TrimSpace(value.LeaseOwner) != value.LeaseOwner || len(value.LeaseOwner) > 128)) ||
		((value.LeaseOwner == "") != value.LeaseExpiresAt.IsZero()) || value.ModelCallID != callID || !idempotencyKeyPattern.MatchString(callID) {
		return false
	}
	switch value.SourceKind {
	case "terminal_failure":
		if value.SourceEventType != "task.result" || value.ResultSHA256 != nil || value.ValidationClaimGeneration != nil || value.VerdictSHA256 != nil {
			return false
		}
	case "terminal_cancellation":
		if value.SourceEventType != "task.status" || value.ResultSHA256 != nil || value.ValidationClaimGeneration != nil || value.VerdictSHA256 != nil {
			return false
		}
	case "waiting_unknown":
		if value.SourceEventType != "task.waiting_user" || value.ResultSHA256 != nil || value.ValidationClaimGeneration != nil || value.VerdictSHA256 != nil {
			return false
		}
	case "late_validation":
		if value.SourceEventType != "task.result" || len(value.ResultSHA256) != sha256.Size ||
			value.ValidationClaimGeneration == nil || *value.ValidationClaimGeneration < 1 || len(value.VerdictSHA256) != sha256.Size {
			return false
		}
	default:
		return false
	}
	return true
}

func ValidTaskTerminalDeliveryModelDispatchFence(value TaskTerminalDeliveryModelFence) bool {
	return ValidTaskTerminalDeliveryModelFence(value) && value.LeaseOwner != "" && !value.LeaseExpiresAt.IsZero()
}

// DSHTerminalDeliveryModelCallID derives the stable ModelGateway id from the
// durable outbox row. It intentionally excludes claim generation and lease
// owner, which change during safe reclaim of the same source.
func DSHTerminalDeliveryModelCallID(fence TaskTerminalDeliveryModelFence) (string, error) {
	if !ValidTaskTerminalDeliveryModelFence(fence) {
		return "", domain.InvalidArgument("model_call", "terminal delivery model identity is invalid")
	}
	return "terminal_delivery_" + strconv.FormatInt(fence.DeliveryID, 10), nil
}

type TaskTerminalDeliveryModelCallDisposition string

const (
	TaskTerminalDeliveryModelNotDispatched TaskTerminalDeliveryModelCallDisposition = "not_dispatched"
	TaskTerminalDeliveryModelFallbackSafe  TaskTerminalDeliveryModelCallDisposition = "fallback_safe"
	TaskTerminalDeliveryModelReplyReady    TaskTerminalDeliveryModelCallDisposition = "reply_ready"
	TaskTerminalDeliveryModelUnknown       TaskTerminalDeliveryModelCallDisposition = "unknown"
	TaskTerminalDeliveryModelStale         TaskTerminalDeliveryModelCallDisposition = "stale"
)

type TaskTerminalDeliveryModelResponseRecord struct {
	ModelID          string
	RequestDigest    []byte
	Ciphertext       []byte
	CiphertextSHA256 []byte
}

type TaskTerminalDeliveryModelResponseStore interface {
	RecoverTaskTerminalDeliveryModelResponse(context.Context, TaskTerminalDeliveryModelFence) (TaskTerminalDeliveryModelResponseRecord, bool, error)
}

type TaskTerminalDeliveryModelCallStateStore interface {
	TaskTerminalDeliveryModelCallDisposition(context.Context, TaskTerminalDeliveryModelFence) (TaskTerminalDeliveryModelCallDisposition, error)
}

type TaskTerminalDeliveryModelResolver interface {
	TaskTerminalDeliveryModelID(context.Context, TaskTerminalDeliveryModelFence) (string, error)
}

type TaskTerminalDeliveryModelProjectResolver interface {
	ModelTaskProjectIDForTerminalDelivery(context.Context, TaskTerminalDeliveryModelFence) (string, error)
}

type taskTerminalDeliveryModelResponseIdentity struct {
	Version         int    `json:"version"`
	DeliveryID      int64  `json:"delivery_id"`
	TaskID          string `json:"task_id"`
	OwnerID         string `json:"owner_id"`
	ConversationID  string `json:"conversation_id"`
	InitiatorID     string `json:"initiator_id"`
	TaskRevision    int64  `json:"task_revision"`
	Authorization   int64  `json:"authorization_version"`
	SourceKind      string `json:"source_kind"`
	EventSequence   int64  `json:"source_event_sequence"`
	EventType       string `json:"source_event_type"`
	SourceSHA256    string `json:"source_sha256"`
	ResultSHA256    string `json:"result_sha256,omitempty"`
	ValidationClaim int64  `json:"validation_claim_generation,omitempty"`
	VerdictSHA256   string `json:"verdict_sha256,omitempty"`
	ModelCallID     string `json:"model_call_id"`
	RequestDigest   string `json:"request_digest"`
	ModelID         string `json:"model_id"`
}

type taskTerminalDeliveryModelResponseBody struct {
	Text                  string     `json:"text"`
	Usage                 ModelUsage `json:"usage"`
	StopReason            string     `json:"stop_reason"`
	ProviderReportedModel string     `json:"provider_reported_model,omitempty"`
}

type taskTerminalDeliveryModelResponseEnvelope struct {
	Identity taskTerminalDeliveryModelResponseIdentity `json:"identity"`
	Response taskTerminalDeliveryModelResponseBody     `json:"response"`
}

func taskTerminalDeliveryModelResponseAAD(fence TaskTerminalDeliveryModelFence, modelID string, requestDigest []byte) (taskTerminalDeliveryModelResponseIdentity, []byte, error) {
	if !ValidTaskTerminalDeliveryModelFence(fence) || !domain.ValidModelConnectionID(modelID) || len(requestDigest) != sha256.Size {
		return taskTerminalDeliveryModelResponseIdentity{}, nil, domain.InvalidArgument("task_terminal_delivery_model_response", "terminal delivery model response identity is invalid")
	}
	identity := taskTerminalDeliveryModelResponseIdentity{Version: 1, DeliveryID: fence.DeliveryID, TaskID: fence.TaskID,
		OwnerID: fence.OwnerID, ConversationID: fence.ConversationID, InitiatorID: fence.InitiatorID,
		TaskRevision: fence.TaskRevision, Authorization: fence.AuthorizationVersion, SourceKind: fence.SourceKind,
		EventSequence: fence.SourceEventSequence, EventType: fence.SourceEventType, SourceSHA256: hex.EncodeToString(fence.SourceSHA256),
		ModelCallID: fence.ModelCallID, RequestDigest: hex.EncodeToString(requestDigest), ModelID: modelID}
	if len(fence.ResultSHA256) != 0 {
		identity.ResultSHA256 = hex.EncodeToString(fence.ResultSHA256)
	}
	if fence.ValidationClaimGeneration != nil {
		identity.ValidationClaim = *fence.ValidationClaimGeneration
	}
	if len(fence.VerdictSHA256) != 0 {
		identity.VerdictSHA256 = hex.EncodeToString(fence.VerdictSHA256)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return taskTerminalDeliveryModelResponseIdentity{}, nil, domain.NewError(domain.CodeInternal, "terminal delivery model response identity could not be encoded")
	}
	return identity, append([]byte("morde-task-terminal-delivery-model-response-v1\n"), encoded...), nil
}

func sealTaskTerminalDeliveryModelResponse(sealer *Sealer, fence TaskTerminalDeliveryModelFence, modelID string,
	requestDigest []byte, response ModelResponse) ([]byte, error) {
	identity, aad, err := taskTerminalDeliveryModelResponseAAD(fence, modelID, requestDigest)
	if err != nil || sealer == nil || !validTaskTerminalDeliveryModelResponse(response) {
		return nil, domain.InvalidArgument("task_terminal_delivery_model_response", "terminal delivery model response is invalid")
	}
	plain, err := json.Marshal(taskTerminalDeliveryModelResponseEnvelope{Identity: identity, Response: taskTerminalDeliveryModelResponseBody{
		Text: response.Text, Usage: response.Usage, StopReason: response.StopReason, ProviderReportedModel: response.ProviderReportedModel,
	}})
	if err != nil || len(plain) > MaxTaskTerminalDeliveryModelResponseCiphertext {
		return nil, domain.NewError(domain.CodeResultTooLarge, "terminal delivery model response is too large")
	}
	ciphertext, err := sealer.sealWithAAD(plain, aad)
	if err != nil || len(ciphertext) > MaxTaskTerminalDeliveryModelResponseCiphertext {
		return nil, domain.NewError(domain.CodeInvalidState, "terminal delivery model response could not be stored safely")
	}
	return ciphertext, nil
}

func openTaskTerminalDeliveryModelResponse(sealer *Sealer, fence TaskTerminalDeliveryModelFence,
	record TaskTerminalDeliveryModelResponseRecord, requestDigest []byte) (ModelResponse, error) {
	identity, aad, err := taskTerminalDeliveryModelResponseAAD(fence, record.ModelID, requestDigest)
	if err != nil || sealer == nil || !domain.ValidModelConnectionID(record.ModelID) || len(record.Ciphertext) == 0 ||
		len(record.Ciphertext) > MaxTaskTerminalDeliveryModelResponseCiphertext || len(record.CiphertextSHA256) != sha256.Size {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response identity is invalid")
	}
	cipherDigest := sha256.Sum256(record.Ciphertext)
	if !bytes.Equal(cipherDigest[:], record.CiphertextSHA256) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response digest changed")
	}
	plain, err := sealer.openWithAAD(record.Ciphertext, aad)
	if err != nil {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response could not be opened")
	}
	decoder := json.NewDecoder(strings.NewReader(string(plain)))
	decoder.DisallowUnknownFields()
	var envelope taskTerminalDeliveryModelResponseEnvelope
	if decoder.Decode(&envelope) != nil || envelope.Identity != identity || !utf8.ValidString(envelope.Response.Text) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response identity changed")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response has trailing data")
	}
	response := ModelResponse{Text: envelope.Response.Text, Usage: envelope.Response.Usage,
		StopReason: envelope.Response.StopReason, ProviderReportedModel: envelope.Response.ProviderReportedModel}
	if !validTaskTerminalDeliveryModelResponse(response) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored terminal delivery response is invalid")
	}
	return response, nil
}

func validTaskTerminalDeliveryModelResponse(response ModelResponse) bool {
	return len(response.ToolCalls) == 0 && response.StopReason == "stop" && strings.TrimSpace(response.Text) != "" &&
		utf8.ValidString(response.Text) && !strings.ContainsRune(response.Text, '\x00') &&
		len(response.Text) <= MaxTaskTerminalDeliveryModelResponseTextBytes && ValidProviderReportedModel(response.ProviderReportedModel) &&
		response.Usage.Known && response.Usage.InputTokens >= 0 && response.Usage.OutputTokens >= 0 &&
		response.Usage.InputTokens <= 1<<53-1 && response.Usage.OutputTokens <= 1<<53-1
}

func (s *ModelGatewayService) ResolveTaskTerminalDeliveryModel(ctx context.Context, fence TaskTerminalDeliveryModelFence) (string, error) {
	if ctx == nil || !ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		return "", domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery model claim is invalid")
	}
	resolver, ok := s.store.(TaskTerminalDeliveryModelResolver)
	if !ok {
		return "", domain.NewError(domain.CodeUnsupported, "terminal delivery task model is unavailable")
	}
	modelID, err := resolver.TaskTerminalDeliveryModelID(ctx, fence)
	if err != nil {
		return "", err
	}
	if !domain.ValidModelConnectionID(modelID) {
		return "", domain.NewError(domain.CodeUnsupported, "terminal delivery task model is unavailable")
	}
	if _, err := s.models.ResolveGatewayEndpoint(ctx, fence.InitiatorID, modelID, false, false); err != nil {
		return "", err
	}
	return modelID, nil
}

// RecoverTaskTerminalDeliveryDSHModelResponse opens a settled response from
// the outbox without resolving the current model connection. The raw request
// digest and immutable delivery identity must still match the ledger.
func (s *ModelGatewayService) RecoverTaskTerminalDeliveryDSHModelResponse(ctx context.Context,
	fence TaskTerminalDeliveryModelFence, raw json.RawMessage) (ModelResponse, bool, error) {
	if ctx == nil || !ValidTaskTerminalDeliveryModelFence(fence) || len(raw) == 0 || len(raw) > 1<<20 {
		return ModelResponse{}, false, domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery recovery request is invalid")
	}
	recovery, ok := s.store.(TaskTerminalDeliveryModelResponseStore)
	if !ok {
		return ModelResponse{}, false, domain.NewError(domain.CodeUnsupported, "terminal delivery model response recovery is unavailable")
	}
	callID, err := DSHTerminalDeliveryModelCallID(fence)
	if err != nil {
		return ModelResponse{}, false, err
	}
	// The response store returns the model ID persisted with the original call,
	// so recovery does not depend on the current model connection configuration.
	record, found, err := recovery.RecoverTaskTerminalDeliveryModelResponse(ctx, fence)
	if err != nil || !found {
		return ModelResponse{}, found, err
	}
	request, err := parseDSHModelRequest(ctx, ModelTaskFence{}, record.ModelID, raw, nil)
	if err != nil || len(request.Tools) != 0 || request.RequiredTool != "" {
		return ModelResponse{}, false, domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery request must not contain tools")
	}
	modelCall := ModelCall{Source: ModelCallTerminalDelivery, TerminalDelivery: &fence, ModelID: record.ModelID,
		CallID: callID, RawRequest: raw, Request: request}
	_, digest, _, err := validateModelCall(modelCall)
	if err != nil || !bytes.Equal(digest[:], record.RequestDigest) {
		return ModelResponse{}, false, domain.NewError(domain.CodeConflict, "terminal delivery model request changed during recovery")
	}
	response, err := openTaskTerminalDeliveryModelResponse(s.responseSealer, fence, record, digest[:])
	if err != nil {
		return ModelResponse{}, false, err
	}
	return response, true, nil
}

func (s *ModelGatewayService) CompleteTaskTerminalDeliveryDSHModel(ctx context.Context, fence TaskTerminalDeliveryModelFence,
	modelID string, raw json.RawMessage) (ModelResponse, TaskTerminalDeliveryModelCallDisposition, error) {
	if ctx == nil || !ValidTaskTerminalDeliveryModelFence(fence) || !domain.ValidModelConnectionID(modelID) || len(raw) == 0 || len(raw) > 1<<20 {
		return ModelResponse{}, TaskTerminalDeliveryModelNotDispatched,
			domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery model request is invalid")
	}
	stored, found, err := s.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, fence, raw)
	if err != nil {
		return ModelResponse{}, TaskTerminalDeliveryModelUnknown, err
	}
	if found {
		return stored, TaskTerminalDeliveryModelReplyReady, nil
	}
	if !ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		return ModelResponse{}, TaskTerminalDeliveryModelStale, domain.NewError(domain.CodeConflict, "terminal delivery claim is no longer current")
	}
	request, err := parseDSHModelRequest(ctx, ModelTaskFence{}, modelID, raw, nil)
	if err != nil || len(request.Tools) != 0 || request.RequiredTool != "" {
		return ModelResponse{}, TaskTerminalDeliveryModelNotDispatched,
			domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery model request must not contain tools")
	}
	callID, err := DSHTerminalDeliveryModelCallID(fence)
	if err != nil {
		return ModelResponse{}, TaskTerminalDeliveryModelNotDispatched, err
	}
	response, err := s.Complete(ctx, ModelCall{Source: ModelCallTerminalDelivery, TerminalDelivery: &fence,
		ModelID: modelID, CallID: callID, RawRequest: raw, Request: request})
	if err == nil {
		return response, TaskTerminalDeliveryModelReplyReady, nil
	}
	stateStore, ok := s.store.(TaskTerminalDeliveryModelCallStateStore)
	if !ok {
		return ModelResponse{}, TaskTerminalDeliveryModelUnknown, err
	}
	disposition, stateErr := stateStore.TaskTerminalDeliveryModelCallDisposition(context.WithoutCancel(ctx), fence)
	if stateErr != nil {
		return ModelResponse{}, TaskTerminalDeliveryModelUnknown, fmt.Errorf("resolve terminal delivery model call outcome: %w", errors.Join(err, stateErr))
	}
	return ModelResponse{}, disposition, err
}
