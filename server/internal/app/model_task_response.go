package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const MaxTaskModelResponseCiphertext = 8 << 20

type TaskModelResponseRecord struct {
	AgentInstanceID       string
	AgentGeneration       int64
	ProviderEndpoint      string
	ProviderReportedModel string
	Ciphertext            []byte
	Digest                []byte
}

type TaskModelResponseStore interface {
	RecoverTaskModelResponse(context.Context, ModelCallReservation) (TaskModelResponseRecord, bool, error)
}

type taskModelResponseIdentity struct {
	Version              int    `json:"version"`
	OwnerID              string `json:"owner_id"`
	TaskOwnerID          string `json:"task_owner_id"`
	TaskID               string `json:"task_id"`
	CallID               string `json:"call_id"`
	DSHCallID            string `json:"dsh_call_id,omitempty"`
	TaskRevision         int64  `json:"task_revision,omitempty"`
	RequestDigest        string `json:"request_digest"`
	ModelID              string `json:"model_id"`
	ProviderModel        string `json:"provider_model"`
	ConnectionVersion    string `json:"connection_version"`
	ProjectID            string `json:"project_id"`
	PersonalModel        bool   `json:"personal_model"`
	AgentSessionID       string `json:"agent_session_id"`
	AgentRootSessionID   string `json:"agent_root_session_id"`
	AuthorizationVersion int64  `json:"authorization_version"`
	ThroughSequence      int64  `json:"through_sequence"`
	EngineVersion        string `json:"engine_version"`
	ProfileVersion       string `json:"profile_version"`
}

type taskModelResponseBody struct {
	Reasoning             string          `json:"reasoning"`
	Text                  string          `json:"text"`
	ToolCalls             []taskModelCall `json:"tool_calls"`
	Usage                 ModelUsage      `json:"usage"`
	StopReason            string          `json:"stop_reason"`
	ProviderEndpoint      string          `json:"provider_endpoint,omitempty"`
	ProviderReportedModel string          `json:"provider_reported_model,omitempty"`
}

type taskModelCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type taskModelResponseEnvelope struct {
	Identity        taskModelResponseIdentity `json:"identity"`
	AgentInstanceID string                    `json:"agent_instance_id"`
	AgentGeneration int64                     `json:"agent_generation"`
	Response        taskModelResponseBody     `json:"response"`
}

// validTaskModelTruncatedTextResponse accepts only a known, nonempty pure-text
// provider result terminated by the output-token limit. It is persisted for
// recovery/display as partial content, never as a successful DSH response.
func validTaskModelTruncatedTextResponse(response ModelResponse) bool {
	return response.StopReason == "length" && len(response.ToolCalls) == 0 &&
		response.Reasoning == "" && strings.TrimSpace(response.Text) != "" &&
		utf8.ValidString(response.Text) && len(response.Text) <= MaxDSHModelResponseTextBytes &&
		!strings.ContainsRune(response.Text, 0) && response.Usage.Known &&
		response.Usage.InputTokens > 0 && response.Usage.InputTokens <= maxDSHModelResponseTokenCount &&
		response.Usage.OutputTokens > 0 && response.Usage.OutputTokens <= maxDSHModelResponseTokenCount &&
		response.ProviderReportedModel != "" && ValidProviderReportedModel(response.ProviderReportedModel)
}

// ValidTaskModelTextTruncation reports whether Gateway marked a response as a
// verified, bounded pure-text Task truncation suitable for DSH max-tokens.
func ValidTaskModelTextTruncation(response ModelResponse) bool {
	return response.KnownTextTruncation && validTaskModelTruncatedTextResponse(response)
}

func validTaskModelTruncatedTextProviderResponse(response ModelResponse) bool {
	return response.StopReason == "length" && len(response.ToolCalls) == 0 &&
		utf8.ValidString(response.Reasoning) && len(response.Reasoning) <= MaxDSHModelResponseTextBytes &&
		!strings.ContainsRune(response.Reasoning, 0) && strings.TrimSpace(response.Text) != "" &&
		utf8.ValidString(response.Text) && len(response.Text) <= MaxDSHModelResponseTextBytes &&
		!strings.ContainsRune(response.Text, 0) && response.Usage.Known &&
		response.Usage.InputTokens > 0 && response.Usage.InputTokens <= maxDSHModelResponseTokenCount &&
		response.Usage.OutputTokens > 0 && response.Usage.OutputTokens <= maxDSHModelResponseTokenCount &&
		response.ProviderReportedModel != "" && ValidProviderReportedModel(response.ProviderReportedModel)
}

func validTaskModelTruncatedTextRequest(request ModelRequest) bool {
	if request.JSONMode || request.RequiredTool != "" || len(request.Tools) != 0 || len(request.Input) == 0 {
		return false
	}
	for _, input := range request.Input {
		if input.Type != "message" || len(input.Images) != 0 || input.CallID != "" || input.Name != "" ||
			input.Arguments != "" || input.Output != "" {
			return false
		}
		for _, part := range input.Parts {
			if part.Type != "text" || part.Image != nil {
				return false
			}
		}
	}
	return true
}

func validTaskModelTruncatedTextForRequest(request ModelRequest, response ModelResponse) bool {
	return validTaskModelTruncatedTextRequest(request) && validTaskModelTruncatedTextProviderResponse(response)
}

func taskModelResponseAAD(reservation ModelCallReservation) (taskModelResponseIdentity, []byte, error) {
	adaptiveBudget := reservation.BudgetPlanID != "" || reservation.BudgetCallID != "" || reservation.BudgetRequestDigest != ""
	if adaptiveBudget && (!idempotencyKeyPattern.MatchString(reservation.BudgetPlanID) ||
		!idempotencyKeyPattern.MatchString(reservation.BudgetCallID) ||
		!modelBudgetDigestPattern.MatchString(reservation.BudgetRequestDigest) || reservation.DSHCallID == "") {
		return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response budget identity is invalid")
	}
	if reservation.Source != ModelCallTask || reservation.Task == nil || reservation.OwnerID != reservation.Task.InitiatorID ||
		!domain.ValidUserID(reservation.OwnerID) || !domain.ValidUserID(reservation.Task.OwnerID) || !domain.ValidTaskID(reservation.Task.TaskID) ||
		!domain.ValidModelConnectionID(reservation.ModelID) || taskValidationModelIdentityRouteKey(reservation.ProviderEndpoint) == "" || len(reservation.RequestDigest) != sha256.Size ||
		len(reservation.ProviderModel) == 0 || len(reservation.ProviderModel) > 256 || reservation.ConnectionVersion.IsZero() ||
		reservation.Task.AgentGeneration < 1 || reservation.Task.AgentInstanceID == "" || len(reservation.Task.AgentInstanceID) > 128 ||
		!dshModelIdentityPattern.MatchString(reservation.Task.AgentSessionID) ||
		!dshModelIdentityPattern.MatchString(reservation.Task.AgentRootSessionID) ||
		reservation.Task.AuthorizationVersion < 1 || reservation.Task.TaskRevision < 0 || reservation.Task.ThroughSequence < 0 ||
		!idempotencyKeyPattern.MatchString(reservation.CallID) || reservation.Task.EngineVersion == "" ||
		reservation.Task.ProfileVersion == "" {
		return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response identity is invalid")
	}
	dshCallID := reservation.DSHCallID
	if dshCallID == "" {
		dshCallID = reservation.CallID
	}
	failedCallID, err := DSHModelCallID(reservation.Task.TaskID, reservation.Task.AgentSessionID, reservation.Task.ThroughSequence)
	if err != nil {
		return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response call identity is invalid")
	}
	recoveryDSHCallID, dshRecoveryErr := TaskModelRecoveryDSHCallID(reservation.Task.TaskID, reservation.Task.TaskRevision,
		failedCallID, reservation.Task.ThroughSequence)
	recoveryLedgerCallID, ledgerRecoveryErr := TaskModelRecoveryCallID(reservation.Task.TaskID, reservation.Task.TaskRevision,
		failedCallID, reservation.Task.ThroughSequence)
	if reservation.Task.RecoveryCallID == "" {
		if adaptiveBudget {
			if dshCallID != failedCallID || !strings.HasPrefix(reservation.CallID, "mdl_") ||
				reservation.Task.RecoveryDSHCallID != "" {
				return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response call identity is invalid")
			}
		} else if dshCallID != failedCallID || reservation.CallID != failedCallID || reservation.Task.RecoveryDSHCallID != "" {
			return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response call identity is invalid")
		}
	} else if dshRecoveryErr != nil || ledgerRecoveryErr != nil || dshCallID != recoveryDSHCallID ||
		reservation.CallID != recoveryLedgerCallID || reservation.Task.RecoveryDSHCallID != recoveryDSHCallID ||
		reservation.Task.RecoveryCallID != recoveryLedgerCallID || reservation.CallID == dshCallID {
		return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response call identity is invalid")
	}
	identity := taskModelResponseIdentity{Version: 1, OwnerID: reservation.OwnerID, TaskOwnerID: reservation.Task.OwnerID,
		TaskID: reservation.Task.TaskID, CallID: reservation.CallID, RequestDigest: fmt.Sprintf("%x", reservation.RequestDigest),
		ModelID: reservation.ModelID, ProviderModel: reservation.ProviderModel,
		ConnectionVersion: reservation.ConnectionVersion.UTC().Format(time.RFC3339Nano),
		ProjectID:         reservation.ProjectID, PersonalModel: reservation.PersonalModel,
		AgentSessionID: reservation.Task.AgentSessionID, AgentRootSessionID: reservation.Task.AgentRootSessionID,
		AuthorizationVersion: reservation.Task.AuthorizationVersion, ThroughSequence: reservation.Task.ThroughSequence,
		EngineVersion: reservation.Task.EngineVersion, ProfileVersion: reservation.Task.ProfileVersion}
	if reservation.Task.RecoveryCallID != "" || adaptiveBudget && dshCallID != reservation.CallID {
		identity.DSHCallID = dshCallID
	}
	if reservation.Task.RecoveryCallID != "" {
		identity.TaskRevision = reservation.Task.TaskRevision
	}
	aad, err := json.Marshal(identity)
	if err != nil {
		return taskModelResponseIdentity{}, nil, domain.NewError(domain.CodeInternal, "task model response identity could not be encoded")
	}
	return identity, append([]byte("morde-task-model-response-v1\n"), aad...), nil
}

func SealTaskModelResponse(sealer *Sealer, reservation ModelCallReservation, response ModelResponse) (TaskModelResponseRecord, error) {
	identity, aad, err := taskModelResponseAAD(reservation)
	if err != nil {
		return TaskModelResponseRecord{}, err
	}
	if sealer == nil || (!ValidDSHModelResponse(response) && !validTaskModelTruncatedTextResponse(response)) {
		return TaskModelResponseRecord{}, domain.InvalidArgument("task_model_response", "task model response is invalid")
	}
	body := taskModelResponseBody{Reasoning: response.Reasoning, Text: response.Text, Usage: response.Usage,
		StopReason: response.StopReason, ProviderEndpoint: reservation.ProviderEndpoint,
		ProviderReportedModel: response.ProviderReportedModel}
	if len(response.ToolCalls) != 0 {
		body.ToolCalls = make([]taskModelCall, len(response.ToolCalls))
		for index, call := range response.ToolCalls {
			if call.TaskFence != nil {
				return TaskModelResponseRecord{}, domain.InvalidArgument("task_model_response", "task model response contains an invalid tool fence")
			}
			body.ToolCalls[index] = taskModelCall{CallID: call.CallID, Name: call.Name, Arguments: call.Arguments}
		}
	}
	plain, err := json.Marshal(taskModelResponseEnvelope{Identity: identity,
		AgentInstanceID: reservation.Task.AgentInstanceID, AgentGeneration: reservation.Task.AgentGeneration, Response: body})
	if err != nil || len(plain) > MaxTaskModelResponseCiphertext {
		return TaskModelResponseRecord{}, domain.NewError(domain.CodeResultTooLarge, "task model response is too large to store")
	}
	ciphertext, err := sealer.sealWithAAD(plain, aad)
	if err != nil {
		return TaskModelResponseRecord{}, err
	}
	digest := sha256.Sum256(plain)
	return TaskModelResponseRecord{AgentInstanceID: reservation.Task.AgentInstanceID,
		AgentGeneration: reservation.Task.AgentGeneration, ProviderEndpoint: reservation.ProviderEndpoint,
		ProviderReportedModel: response.ProviderReportedModel, Ciphertext: ciphertext, Digest: append([]byte(nil), digest[:]...)}, nil
}

func OpenTaskModelResponse(sealer *Sealer, record TaskModelResponseRecord, reservation ModelCallReservation) (ModelResponse, error) {
	identity, aad, err := taskModelResponseAAD(reservation)
	if err != nil || sealer == nil || record.AgentInstanceID == "" || len(record.AgentInstanceID) > 128 ||
		record.AgentGeneration < 1 || record.AgentGeneration > reservation.Task.AgentGeneration ||
		record.AgentGeneration == reservation.Task.AgentGeneration && record.AgentInstanceID != reservation.Task.AgentInstanceID ||
		len(record.Ciphertext) == 0 || len(record.Ciphertext) > MaxTaskModelResponseCiphertext || len(record.Digest) != sha256.Size {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response identity is invalid")
	}
	if record.ProviderEndpoint != "" && record.ProviderEndpoint != reservation.ProviderEndpoint ||
		record.ProviderReportedModel != "" && !ValidProviderReportedModel(record.ProviderReportedModel) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response provider identity is invalid")
	}
	plain, err := sealer.openWithAAD(record.Ciphertext, aad)
	if err != nil {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response could not be opened")
	}
	digest := sha256.Sum256(plain)
	if !bytes.Equal(digest[:], record.Digest) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response digest changed")
	}
	var envelope taskModelResponseEnvelope
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Identity != identity || envelope.AgentInstanceID != record.AgentInstanceID ||
		envelope.AgentGeneration != record.AgentGeneration || !utf8.ValidString(envelope.Response.Reasoning) ||
		!utf8.ValidString(envelope.Response.Text) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response identity changed")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response has trailing data")
	}
	response := ModelResponse{Reasoning: envelope.Response.Reasoning, Text: envelope.Response.Text,
		Usage: envelope.Response.Usage, StopReason: envelope.Response.StopReason,
		ProviderReportedModel: envelope.Response.ProviderReportedModel}
	if len(envelope.Response.ToolCalls) != 0 {
		response.ToolCalls = make([]ModelToolCall, len(envelope.Response.ToolCalls))
		for index, call := range envelope.Response.ToolCalls {
			response.ToolCalls[index] = ModelToolCall{CallID: call.CallID, Name: call.Name, Arguments: call.Arguments}
		}
	}
	if !ValidDSHModelResponse(response) && !validTaskModelTruncatedTextResponse(response) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response is invalid")
	}
	if record.ProviderReportedModel != "" && record.ProviderReportedModel != response.ProviderReportedModel ||
		record.ProviderEndpoint != "" && envelope.Response.ProviderEndpoint != record.ProviderEndpoint {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response provider identity changed")
	}
	return response, nil
}

func validateTaskModelResponse(request ModelRequest, response ModelResponse) error {
	if !ValidDSHModelResponse(response) {
		return invalidTaskModelResponseError(response, "model provider returned an invalid task response")
	}
	for _, call := range response.ToolCalls {
		if !validAgentJSONObject([]byte(call.Arguments), maxAgentToolArgsBytes) {
			return invalidTaskModelResponseError(response, "model provider returned an invalid tool call")
		}
		declared := false
		for _, tool := range request.Tools {
			declared = declared || tool.Name == call.Name
		}
		if !declared || request.RequiredTool != "" && call.Name != request.RequiredTool {
			return invalidTaskModelResponseError(response, "model provider returned a tool call not allowed by the request")
		}
	}
	return nil
}

func invalidTaskModelResponseError(response ModelResponse, message string) *domain.Error {
	reason := ModelErrorInvalidResponse
	switch {
	case validTaskModelTruncatedTextProviderResponse(response):
		reason = ModelErrorTruncatedResponse
	case response.ProviderReportedModel != "" && ValidProviderReportedModel(response.ProviderReportedModel) &&
		response.Usage.Known && response.StopReason == "content_filter":
		reason = ModelErrorContentFiltered
	case response.ProviderReportedModel != "" && ValidProviderReportedModel(response.ProviderReportedModel) && response.StopReason == "stop" &&
		len(response.ToolCalls) == 0 && response.Text == "" && response.Reasoning == "" && response.Usage.Known &&
		response.Usage.InputTokens > 0 && response.Usage.InputTokens <= maxDSHModelResponseTokenCount && response.Usage.OutputTokens == 0:
		reason = ModelErrorEmptyResponse
	}
	err := domain.NewError(domain.CodeUpstreamError, message)
	err.Details["reason"] = string(reason)
	return err
}
