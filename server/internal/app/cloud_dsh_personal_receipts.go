package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

type CloudDSHPersonalReceiptRequest struct {
	Fence            AgentInstanceFence
	ThroughSequence  int64
	CheckpointDigest []byte
	Commit           CloudDSHPersonalMutationCommit
}

type CloudDSHPersonalReceiptStore interface {
	ConfirmCloudDSHPersonalMutationReceipt(context.Context, CloudDSHPersonalReceiptRequest) error
}

// CloudDSHGoalReceiptRequest identifies the original Goal call without loading
// mutable Goal content or putting that content in the Task event.
type CloudDSHGoalReceiptRequest struct {
	Fence            AgentInstanceFence
	ThroughSequence  int64
	CheckpointDigest []byte
	CallID           string
	ToolName         string
	ArgumentsSHA256  string
	GoalID           string
	BaseRevision     int64
}

type CloudDSHGoalReceipt struct {
	Mutation personal.Mutation
	Status   string
}

type CloudDSHGoalReceiptStore interface {
	ConfirmCloudDSHGoalMutationReceipt(context.Context, CloudDSHGoalReceiptRequest) (CloudDSHGoalReceipt, error)
}

// ValidateCloudDSHGoalReceiptEvent verifies only durable metadata; the caller
// must separately verify the personal mutation ledger and current Agent fence.
func ValidateCloudDSHGoalReceiptEvent(payload []byte, request CloudDSHGoalReceiptRequest) (CloudDSHGoalReceipt, error) {
	var event struct {
		Detail struct {
			Tool                 string `json:"tool"`
			CallID               string `json:"personal_call_id"`
			MutationID           string `json:"personal_mutation_id"`
			AgentSessionID       string `json:"agent_session_id"`
			AuthorizationVersion int64  `json:"authorization_version"`
			ObjectType           string `json:"object_type"`
			ObjectID             string `json:"object_id"`
			Revision             int64  `json:"revision"`
			RequestSHA256        string `json:"request_sha256"`
			FingerprintSHA256    string `json:"fingerprint_sha256"`
			GoalArgumentsSHA256  string `json:"goal_arguments_sha256"`
			GoalStatus           string `json:"goal_status"`
		} `json:"detail"`
	}
	invalid := func() (CloudDSHGoalReceipt, error) {
		return CloudDSHGoalReceipt{}, domain.NewError(domain.CodeInvalidState, "goal write Task receipt is invalid")
	}
	if len(payload) == 0 || len(payload) > 16<<10 || json.Unmarshal(payload, &event) != nil ||
		request.CallID == "" || len(request.CallID) > 256 || request.BaseRevision < 0 || request.BaseRevision == math.MaxInt64 ||
		personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, request.GoalID) != nil {
		return invalid()
	}
	switch request.ToolName {
	case cloudDSHGoalCreateToolName:
		if request.BaseRevision != 0 || request.GoalID != cloudDSHStablePersonalID("goal", request.Fence.TaskID,
			request.Fence.AgentSessionID, cloudDSHPersonalMutationID(request.Fence.TaskID, request.Fence.AgentSessionID, request.CallID), 0) {
			return invalid()
		}
	case cloudDSHGoalUpdateToolName, cloudDSHGoalDeleteToolName:
		if request.BaseRevision < 1 {
			return invalid()
		}
	default:
		return invalid()
	}
	detail := event.Detail
	argumentsHash, argumentsErr := hex.DecodeString(request.ArgumentsSHA256)
	requestHash, requestErr := hex.DecodeString(detail.RequestSHA256)
	mutationID := cloudDSHPersonalMutationID(request.Fence.TaskID, request.Fence.AgentSessionID, request.CallID)
	if argumentsErr != nil || len(argumentsHash) != sha256.Size || hex.EncodeToString(argumentsHash) != request.ArgumentsSHA256 ||
		requestErr != nil || len(requestHash) != sha256.Size || hex.EncodeToString(requestHash) != detail.RequestSHA256 ||
		detail.Tool != request.ToolName || detail.CallID != request.CallID || detail.MutationID != mutationID ||
		detail.AgentSessionID != request.Fence.AgentSessionID || detail.AuthorizationVersion != request.Fence.AuthorizationVersion ||
		detail.ObjectType != personal.ObjectTypeGoal || detail.ObjectID != request.GoalID || detail.Revision != request.BaseRevision+1 ||
		detail.GoalArgumentsSHA256 != request.ArgumentsSHA256 {
		return invalid()
	}
	if request.ToolName == cloudDSHGoalDeleteToolName {
		if detail.GoalStatus != "" {
			return invalid()
		}
	} else {
		switch detail.GoalStatus {
		case "active", "paused", "done", "cancelled":
		default:
			return invalid()
		}
	}
	mutation := personal.Mutation{OwnerID: request.Fence.OwnerID, MutationID: mutationID, ObjectType: personal.ObjectTypeGoal,
		ObjectID: request.GoalID, BaseRevision: request.BaseRevision, Delete: request.ToolName == cloudDSHGoalDeleteToolName,
		RequestHash: requestHash}
	commit := CloudDSHPersonalMutationCommit{TaskID: request.Fence.TaskID, OwnerID: request.Fence.OwnerID,
		AgentSessionID: request.Fence.AgentSessionID, AuthorizationVersion: request.Fence.AuthorizationVersion,
		CallID: request.CallID, Mutation: mutation, GoalToolName: request.ToolName,
		GoalArgumentsSHA256: request.ArgumentsSHA256, GoalStatus: detail.GoalStatus}
	if detail.FingerprintSHA256 != cloudDSHPersonalMutationFingerprint(commit) {
		return invalid()
	}
	return CloudDSHGoalReceipt{Mutation: mutation, Status: detail.GoalStatus}, nil
}

func decodeCloudDSHPersonalUpdateArguments(raw string) (cloudDSHPersonalUpdateArguments, error) {
	var arguments cloudDSHPersonalUpdateArguments
	if len(raw) == 0 || len(raw) > maxAgentToolArgsBytes || !validAgentJSONObject([]byte(raw), maxAgentToolArgsBytes) ||
		decodeStrictRuntimeResult([]byte(raw), &arguments) != nil || len(arguments.Body) == 0 {
		return arguments, domain.InvalidArgument("tool_call.arguments", "personal data arguments are invalid")
	}
	var body string
	if json.Unmarshal(arguments.Body, &body) == nil {
		arguments.Body = json.RawMessage(body)
	}
	if !validAgentJSONObject(arguments.Body, personal.MaxMutationBodyBytes) {
		return arguments, domain.InvalidArgument("tool_call.arguments", "personal data arguments are invalid")
	}
	return arguments, nil
}

// ConfirmPersonalContextWrite is read-only. It accepts a checkpointed call
// only when its exact mutation and Task event were durably committed under the
// original authorization and the currently recovered Agent lease is valid.
func (b *CloudDSHToolBroker) ConfirmPersonalContextWrite(ctx context.Context, claimed task.Task, fence AgentInstanceFence,
	through int64, digest []byte, call ModelToolCall) (CloudDSHMemoryVersion, string, error) {
	if b == nil {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "personal write receipt is unavailable")
	}
	if ctx == nil || b.personalData == nil || call.CallID == "" ||
		fence.TaskID != claimed.ID || fence.OwnerID != claimed.OwnerID || fence.InitiatorID != claimed.InitiatorID ||
		fence.AgentSessionID != claimed.AgentSessionID || through < 0 || len(digest) != 32 {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "personal write receipt is unavailable")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil || !cloudDSHPersonalContextEligible(details.Task) || !sameCloudDSHAuthorization(details.Task.Authorization, claimed.Authorization) ||
		details.Task.AgentSessionID != fence.AgentSessionID {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeUnauthenticated, "personal write authorization changed")
	}
	principal, ok := cloudDSHPersonalPrincipal(details.Task)
	if !ok {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeUnauthenticated, "personal write authorization changed")
	}
	if call.Name == cloudDSHGoalCreateToolName || call.Name == cloudDSHGoalUpdateToolName || call.Name == cloudDSHGoalDeleteToolName {
		return b.confirmGoalContextWrite(ctx, fence, through, digest, call)
	}
	store, ok := b.store.(CloudDSHPersonalReceiptStore)
	if !ok || call.Name != cloudDSHPersonalUpdateToolName {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "personal write receipt is unavailable")
	}
	arguments, err := decodeCloudDSHPersonalUpdateArguments(call.Arguments)
	if err != nil {
		return CloudDSHMemoryVersion{}, "", err
	}
	switch arguments.ObjectType {
	case personal.ObjectTypeMemory, personal.ObjectTypeProfile, personal.ObjectTypeSoul, personal.ObjectTypeGoal:
	default:
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "personal context write is unsupported")
	}
	mutation, err := b.personalData.PreparePersonalMutation(principal, PersonalMutationCommand{
		MutationID: cloudDSHPersonalMutationID(claimed.ID, fence.AgentSessionID, call.CallID), ObjectType: arguments.ObjectType,
		ObjectID: arguments.ObjectID, BaseRevision: arguments.BaseRevision, Body: arguments.Body,
	})
	if err != nil {
		return CloudDSHMemoryVersion{}, "", err
	}
	commit := CloudDSHPersonalMutationCommit{TaskID: claimed.ID, OwnerID: claimed.OwnerID, AgentSessionID: fence.AgentSessionID,
		AuthorizationVersion: fence.AuthorizationVersion, CallID: call.CallID, Mutation: mutation}
	commit.FingerprintSHA256 = cloudDSHPersonalMutationFingerprint(commit)
	if !ValidCloudDSHPersonalMutationCommit(commit) {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "personal write receipt is invalid")
	}
	request := CloudDSHPersonalReceiptRequest{Fence: fence, ThroughSequence: through,
		CheckpointDigest: append([]byte(nil), digest...), Commit: commit}
	if err := store.ConfirmCloudDSHPersonalMutationReceipt(ctx, request); err != nil {
		return CloudDSHMemoryVersion{}, "", err
	}
	version := CloudDSHMemoryVersion{ObjectType: mutation.ObjectType, ID: mutation.ObjectID, Revision: mutation.BaseRevision + 1}
	output, err := json.Marshal(struct {
		ObjectType string `json:"object_type"`
		ObjectID   string `json:"object_id"`
		Revision   int64  `json:"revision"`
	}{version.ObjectType, version.ID, version.Revision})
	if err != nil {
		return CloudDSHMemoryVersion{}, "", err
	}
	return version, string(output), nil
}

func (b *CloudDSHToolBroker) confirmGoalContextWrite(ctx context.Context, fence AgentInstanceFence, through int64,
	digest []byte, call ModelToolCall) (CloudDSHMemoryVersion, string, error) {
	store, ok := b.store.(CloudDSHGoalReceiptStore)
	raw, err := validateAgentToolArguments(call.Arguments)
	if !ok || err != nil {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write receipt is unavailable")
	}
	request := CloudDSHGoalReceiptRequest{Fence: fence, ThroughSequence: through, CheckpointDigest: append([]byte(nil), digest...),
		CallID: call.CallID, ToolName: call.Name}
	argumentsHash := sha256.Sum256(raw)
	request.ArgumentsSHA256 = hex.EncodeToString(argumentsHash[:])
	var create cloudDSHGoalCreateArguments
	var update cloudDSHGoalUpdateArguments
	var remove cloudDSHGoalDeleteArguments
	switch call.Name {
	case cloudDSHGoalCreateToolName:
		if decodeStrictRuntimeResult(raw, &create) != nil || len(create.Steps) > 100 || len(create.RelatedMaterialIDs) > 100 {
			return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write arguments are invalid")
		}
		mutationID := cloudDSHPersonalMutationID(fence.TaskID, fence.AgentSessionID, call.CallID)
		request.GoalID = cloudDSHStablePersonalID("goal", fence.TaskID, fence.AgentSessionID, mutationID, 0)
	case cloudDSHGoalUpdateToolName:
		if decodeStrictRuntimeResult(raw, &update) != nil || update.ExpectedRevision < 1 || update.ExpectedRevision >= 1<<53-1 {
			return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write arguments are invalid")
		}
		request.GoalID, request.BaseRevision = update.GoalID, update.ExpectedRevision
	case cloudDSHGoalDeleteToolName:
		if decodeStrictRuntimeResult(raw, &remove) != nil || remove.ExpectedRevision < 1 || remove.ExpectedRevision >= 1<<53-1 {
			return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write arguments are invalid")
		}
		request.GoalID, request.BaseRevision = remove.GoalID, remove.ExpectedRevision
	default:
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write is unsupported")
	}
	receipt, err := store.ConfirmCloudDSHGoalMutationReceipt(ctx, request)
	if err != nil {
		return CloudDSHMemoryVersion{}, "", err
	}
	if receipt.Mutation.OwnerID != fence.OwnerID || receipt.Mutation.ObjectType != personal.ObjectTypeGoal ||
		receipt.Mutation.ObjectID != request.GoalID || receipt.Mutation.BaseRevision != request.BaseRevision ||
		receipt.Mutation.MutationID != cloudDSHPersonalMutationID(fence.TaskID, fence.AgentSessionID, call.CallID) ||
		receipt.Mutation.Delete != (call.Name == cloudDSHGoalDeleteToolName) {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write receipt changed")
	}
	version := CloudDSHMemoryVersion{ObjectType: personal.ObjectTypeGoal, ID: request.GoalID, Revision: request.BaseRevision + 1}
	var result any
	switch call.Name {
	case cloudDSHGoalCreateToolName:
		if receipt.Status != create.Status {
			return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write result changed")
		}
		result = map[string]any{"id": version.ID, "revision": version.Revision, "title": create.Title,
			"status": receipt.Status, "step_count": len(create.Steps)}
	case cloudDSHGoalUpdateToolName:
		if update.Status != nil && *update.Status != receipt.Status {
			return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write result changed")
		}
		result = map[string]any{"id": version.ID, "revision": version.Revision, "status": receipt.Status}
	case cloudDSHGoalDeleteToolName:
		result = map[string]any{"id": version.ID, "revision": version.Revision, "deleted": true}
	}
	output, err := json.Marshal(result)
	if err != nil || len(output) > maxAgentToolOutputBytes {
		return CloudDSHMemoryVersion{}, "", domain.NewError(domain.CodeInvalidState, "goal write result is invalid")
	}
	return version, string(output), nil
}
