package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const MaxTaskModelResponseCiphertext = 8 << 20

type TaskModelResponseRecord struct {
	AgentInstanceID string
	AgentGeneration int64
	Ciphertext      []byte
	Digest          []byte
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
	Reasoning  string          `json:"reasoning"`
	Text       string          `json:"text"`
	ToolCalls  []taskModelCall `json:"tool_calls"`
	Usage      ModelUsage      `json:"usage"`
	StopReason string          `json:"stop_reason"`
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

func taskModelResponseAAD(reservation ModelCallReservation) (taskModelResponseIdentity, []byte, error) {
	if reservation.Source != ModelCallTask || reservation.Task == nil || reservation.OwnerID != reservation.Task.InitiatorID ||
		!domain.ValidUserID(reservation.OwnerID) || !domain.ValidUserID(reservation.Task.OwnerID) || !domain.ValidTaskID(reservation.Task.TaskID) ||
		!domain.ValidModelConnectionID(reservation.ModelID) || len(reservation.RequestDigest) != sha256.Size ||
		len(reservation.ProviderModel) == 0 || len(reservation.ProviderModel) > 256 || reservation.ConnectionVersion.IsZero() ||
		reservation.Task.AgentGeneration < 1 || reservation.Task.AgentInstanceID == "" || len(reservation.Task.AgentInstanceID) > 128 ||
		!dshModelIdentityPattern.MatchString(reservation.Task.AgentSessionID) ||
		!dshModelIdentityPattern.MatchString(reservation.Task.AgentRootSessionID) ||
		reservation.Task.AuthorizationVersion < 1 || reservation.Task.ThroughSequence < 0 ||
		!idempotencyKeyPattern.MatchString(reservation.CallID) || reservation.Task.EngineVersion == "" ||
		reservation.Task.ProfileVersion == "" {
		return taskModelResponseIdentity{}, nil, domain.InvalidArgument("task_model_response", "task model response identity is invalid")
	}
	expectedCallID, err := DSHModelCallID(reservation.Task.TaskID, reservation.Task.AgentSessionID, reservation.Task.ThroughSequence)
	if err != nil || expectedCallID != reservation.CallID {
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
	if sealer == nil || !ValidDSHModelResponse(response) {
		return TaskModelResponseRecord{}, domain.InvalidArgument("task_model_response", "task model response is invalid")
	}
	body := taskModelResponseBody{Reasoning: response.Reasoning, Text: response.Text, Usage: response.Usage, StopReason: response.StopReason}
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
		AgentGeneration: reservation.Task.AgentGeneration, Ciphertext: ciphertext, Digest: append([]byte(nil), digest[:]...)}, nil
}

func OpenTaskModelResponse(sealer *Sealer, record TaskModelResponseRecord, reservation ModelCallReservation) (ModelResponse, error) {
	identity, aad, err := taskModelResponseAAD(reservation)
	if err != nil || sealer == nil || record.AgentInstanceID == "" || len(record.AgentInstanceID) > 128 ||
		record.AgentGeneration < 1 || record.AgentGeneration > reservation.Task.AgentGeneration ||
		record.AgentGeneration == reservation.Task.AgentGeneration && record.AgentInstanceID != reservation.Task.AgentInstanceID ||
		len(record.Ciphertext) == 0 || len(record.Ciphertext) > MaxTaskModelResponseCiphertext || len(record.Digest) != sha256.Size {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response identity is invalid")
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
		Usage: envelope.Response.Usage, StopReason: envelope.Response.StopReason}
	if len(envelope.Response.ToolCalls) != 0 {
		response.ToolCalls = make([]ModelToolCall, len(envelope.Response.ToolCalls))
		for index, call := range envelope.Response.ToolCalls {
			response.ToolCalls[index] = ModelToolCall{CallID: call.CallID, Name: call.Name, Arguments: call.Arguments}
		}
	}
	if !ValidDSHModelResponse(response) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored task model response is invalid")
	}
	return response, nil
}

func validateTaskModelResponse(request ModelRequest, response ModelResponse) error {
	if !ValidDSHModelResponse(response) {
		return domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid task response")
	}
	for _, call := range response.ToolCalls {
		if !validAgentJSONObject([]byte(call.Arguments), maxAgentToolArgsBytes) {
			return domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid tool call")
		}
		declared := false
		for _, tool := range request.Tools {
			declared = declared || tool.Name == call.Name
		}
		if !declared || request.RequiredTool != "" && call.Name != request.RequiredTool {
			return domain.NewError(domain.CodeUpstreamError, "model provider returned a tool call not allowed by the request")
		}
	}
	return nil
}
