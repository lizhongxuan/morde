package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHPersonalUpdateToolName = "morde_personal_object_update"
	cloudDSHPersonalContextBytes   = 24 << 10
	cloudDSHPersonalContextItems   = 32
	cloudDSHPersonalContextPages   = 10
	cloudDSHProjectContextBytes    = 24 << 10
)

type cloudDSHPersonalSnapshotReader interface {
	GetSnapshot(context.Context, identity.Principal, string, int32) (PersonalSnapshotPage, error)
}

func cloudDSHPersonalUpdateDefinition() AgentToolDefinition {
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"object_type":   map[string]any{"type": "string", "enum": []string{personal.ObjectTypeProfile, personal.ObjectTypeMemory, personal.ObjectTypeSoul, personal.ObjectTypeGoal}},
			"object_id":     map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"base_revision": map[string]any{"type": "integer", "minimum": 0},
			"body":          map[string]any{"type": "object", "additionalProperties": true},
		},
		"required":             []string{"object_type", "object_id", "base_revision", "body"},
		"additionalProperties": false,
	})
	return AgentToolDefinition{ModelTool: ModelTool{
		Name:        cloudDSHPersonalUpdateToolName,
		Description: "Create or update this account's profile, memory, SOUL, or goal using the current revision. Never put credentials, connector secrets, or unrelated private data in the body.",
		Parameters:  parameters, Strict: true,
	}, RequiredScope: identity.ScopeTaskSubmit}
}

func cloudDSHPersonalContextEligible(value task.Task) bool {
	contextValue := value.ConversationContext
	if contextValue == nil || value.ProjectID != "" || value.InitiatorID != value.OwnerID ||
		contextValue.OwnerID != value.OwnerID || contextValue.InitiatorID != value.InitiatorID ||
		contextValue.ProjectID != "" || contextValue.ConversationID != value.ConversationID || contextValue.Scope != "personal" {
		return false
	}
	_, ok := cloudDSHPersonalPrincipal(value)
	return ok
}

func cloudDSHPersonalPrincipal(value task.Task) (identity.Principal, bool) {
	kind := identity.Kind(value.Authorization.Kind)
	if value.ProjectID != "" || value.InitiatorID != value.OwnerID ||
		(kind != identity.KindSession && kind != identity.KindClient) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidTokenID(value.Authorization.TokenID) || value.Authorization.Version < 1 ||
		value.Authorization.ExpiresAt == nil || !value.Authorization.ExpiresAt.After(time.Now()) {
		return identity.Principal{}, false
	}
	scopes := identity.Principal{Kind: kind, Scopes: agentScopeSnapshot(value.Authorization.Scopes)}
	if !scopes.HasScope(identity.ScopeTaskRead) || !scopes.HasScope(identity.ScopeTaskSubmit) {
		return identity.Principal{}, false
	}
	principal := identity.Principal{
		UserID: value.OwnerID, Role: identity.RoleUser, Kind: kind, TokenID: value.Authorization.TokenID,
		ClientInstallationID: value.Authorization.ClientInstallationID,
	}
	if kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return identity.Principal{}, false
	}
	return principal, true
}

func (b *CloudDSHToolBroker) InitialContext(ctx context.Context, claimed task.Task) (string, error) {
	if cloudDSHProjectContextEligible(claimed) {
		return b.initialProjectContext(ctx, claimed)
	}
	if b.personalSync == nil || b.goals == nil || !cloudDSHPersonalContextEligible(claimed) {
		return "", nil
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return "", err
	}
	current := details.Task
	if !cloudDSHPersonalContextEligible(current) || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
		return "", domain.NewError(domain.CodeForbidden, "personal Task context authorization changed")
	}
	principal, ok := cloudDSHPersonalPrincipal(current)
	if !ok {
		return "", domain.NewError(domain.CodeForbidden, "personal Task context is not authorized")
	}

	objects := make([]personal.Object, 0, cloudDSHPersonalContextItems)
	cursor := ""
	morePersonalObjects := false
	for pageNumber := 0; pageNumber < cloudDSHPersonalContextPages && len(objects) < cloudDSHPersonalContextItems; pageNumber++ {
		page, err := b.personalSync.GetSnapshot(ctx, principal, cursor, 100)
		if err != nil {
			return "", err
		}
		for _, object := range page.Items {
			if len(objects) == cloudDSHPersonalContextItems {
				morePersonalObjects = page.NextCursor != ""
				break
			}
			switch object.ObjectType {
			case personal.ObjectTypeProfile, personal.ObjectTypeMemory, personal.ObjectTypeSoul:
				objects = append(objects, object)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pageNumber == cloudDSHPersonalContextPages-1 {
			morePersonalObjects = true
		}
	}
	goals, err := b.goals.List(ctx, principal, PageRequest{Limit: 20})
	if err != nil {
		return "", err
	}
	objects = append(objects, goals.Items...)
	sort.SliceStable(objects, func(i, j int) bool {
		left, right := personalContextPriority(objects[i].ObjectType), personalContextPriority(objects[j].ObjectType)
		if left != right {
			return left < right
		}
		return objects[i].ObjectID < objects[j].ObjectID
	})

	var output strings.Builder
	output.WriteString("Authorized personal context for this account (bounded snapshot; it may be incomplete):\n")
	omitted := morePersonalObjects || goals.NextCursor != ""
	for _, object := range objects {
		if len(output.String()) >= cloudDSHPersonalContextBytes {
			omitted = true
			break
		}
		if object.Revision < 1 || !json.Valid(object.Body) || personal.ValidateObjectBody(object.ObjectType, object.ObjectID, object.Body) != nil {
			return "", errors.New("personal Task context contains an invalid object")
		}
		line, err := json.Marshal(struct {
			Type     string          `json:"type"`
			ID       string          `json:"id"`
			Revision int64           `json:"revision"`
			Body     json.RawMessage `json:"body"`
		}{object.ObjectType, object.ObjectID, object.Revision, object.Body})
		if err != nil {
			return "", err
		}
		if len(output.String())+len(line)+1 > cloudDSHPersonalContextBytes {
			omitted = true
			continue
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	if omitted {
		output.WriteString("Some personal objects were omitted because the snapshot or context size is bounded.\n")
	}
	if output.Len() == len("Authorized personal context for this account (bounded snapshot; it may be incomplete):\n") {
		return "", nil
	}
	if output.Len() > cloudDSHPersonalContextBytes+128 || !utf8.ValidString(output.String()) {
		return "", domain.NewError(domain.CodeResultTooLarge, "personal Task context exceeds its size limit")
	}
	return strings.TrimSpace(output.String()), nil
}

func cloudDSHProjectContextEligible(value task.Task) bool {
	contextValue := value.ConversationContext
	kind := identity.Kind(value.Authorization.Kind)
	if contextValue == nil || !project.ValidID(value.ProjectID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(value.InitiatorID) || !domain.ValidConversationID(value.ConversationID) ||
		contextValue.OwnerID != value.OwnerID || contextValue.InitiatorID != value.InitiatorID ||
		contextValue.ProjectID != value.ProjectID || contextValue.ConversationID != value.ConversationID || contextValue.Scope != "project" ||
		(kind != identity.KindSession && kind != identity.KindClient) || !domain.ValidTokenID(value.Authorization.TokenID) ||
		value.Authorization.Version < 1 || value.Authorization.ExpiresAt == nil || !value.Authorization.ExpiresAt.After(time.Now()) {
		return false
	}
	scopes := identity.Principal{Kind: kind, Scopes: agentScopeSnapshot(value.Authorization.Scopes)}
	if !scopes.HasScope(identity.ScopeTaskRead) || !scopes.HasScope(identity.ScopeTaskSubmit) {
		return false
	}
	return kind != identity.KindClient || domain.ValidClientInstallationID(value.Authorization.ClientInstallationID)
}

func cloudDSHProjectPrincipal(value task.Task) (identity.Principal, bool) {
	if !cloudDSHProjectContextEligible(value) {
		return identity.Principal{}, false
	}
	kind := identity.Kind(value.Authorization.Kind)
	return identity.Principal{
		UserID: value.InitiatorID, Role: identity.RoleUser, Kind: kind, TokenID: value.Authorization.TokenID,
		ClientInstallationID: value.Authorization.ClientInstallationID,
	}, true
}

func (b *CloudDSHToolBroker) initialProjectContext(ctx context.Context, claimed task.Task) (string, error) {
	if b.projectContext == nil {
		return "", nil
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return "", err
	}
	current := details.Task
	if !cloudDSHProjectContextEligible(current) || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
		return "", domain.NewError(domain.CodeForbidden, "project Task context authorization changed")
	}
	principal, ok := cloudDSHProjectPrincipal(current)
	if !ok {
		return "", domain.NewError(domain.CodeForbidden, "project Task context is not authorized")
	}
	objects, omitted, err := b.projectContext.AgentContext(ctx, principal, current)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	output.WriteString("Authorized project context for this project (bounded snapshot; it may be incomplete):\n")
	for _, object := range objects {
		if object.Revision < 1 || !json.Valid(object.Body) || project.ValidateContentBody(object.Kind, object.ID, object.Body) != nil {
			return "", errors.New("project Task context contains an invalid object")
		}
		line, err := json.Marshal(struct {
			Type     project.ContentKind `json:"type"`
			ID       string              `json:"id"`
			Revision int64               `json:"revision"`
			Body     json.RawMessage     `json:"body"`
		}{object.Kind, object.ID, object.Revision, object.Body})
		if err != nil {
			return "", err
		}
		if output.Len()+len(line)+1 > cloudDSHProjectContextBytes {
			omitted = true
			continue
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	if omitted {
		output.WriteString("Some project objects were omitted because the snapshot or context size is bounded.\n")
	}
	if output.Len() == len("Authorized project context for this project (bounded snapshot; it may be incomplete):\n") {
		return "", nil
	}
	if output.Len() > cloudDSHProjectContextBytes+128 || !utf8.ValidString(output.String()) {
		return "", domain.NewError(domain.CodeResultTooLarge, "project Task context exceeds its size limit")
	}
	return strings.TrimSpace(output.String()), nil
}

func personalContextPriority(objectType string) int {
	switch objectType {
	case personal.ObjectTypeProfile:
		return 0
	case personal.ObjectTypeMemory:
		return 1
	case personal.ObjectTypeSoul:
		return 2
	default:
		return 3
	}
}

type cloudDSHPersonalUpdateArguments struct {
	ObjectType   string          `json:"object_type"`
	ObjectID     string          `json:"object_id"`
	BaseRevision int64           `json:"base_revision"`
	Body         json.RawMessage `json:"body"`
}

func (b *CloudDSHToolBroker) executePersonalUpdate(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if b.personalData == nil || ctx == nil || len(call.Arguments) == 0 || len(call.Arguments) > maxAgentToolArgsBytes ||
		!validAgentJSONObject([]byte(call.Arguments), maxAgentToolArgsBytes) {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "personal data arguments are invalid")
	}
	var arguments cloudDSHPersonalUpdateArguments
	if decodeStrictRuntimeResult([]byte(call.Arguments), &arguments) != nil || len(arguments.Body) == 0 ||
		!validAgentJSONObject(arguments.Body, personal.MaxMutationBodyBytes) {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "personal data arguments are invalid")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if !cloudDSHPersonalContextEligible(current) || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) ||
		(current.AgentSessionID != "" && claimed.AgentSessionID != "" && current.AgentSessionID != claimed.AgentSessionID) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "personal data is not authorized for this Task")
	}
	principal, ok := cloudDSHPersonalPrincipal(current)
	if !ok {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "personal data is not authorized for this Task")
	}
	mutationID := cloudDSHPersonalMutationID(current.ID, current.AgentSessionID, call.CallID)
	mutation, err := b.personalData.PreparePersonalMutation(principal, PersonalMutationCommand{
		MutationID: mutationID, ObjectType: arguments.ObjectType, ObjectID: arguments.ObjectID,
		BaseRevision: arguments.BaseRevision, Body: arguments.Body,
	})
	if err != nil {
		return AgentToolResult{}, err
	}
	if mutation.BaseRevision == math.MaxInt64 {
		return AgentToolResult{}, domain.InvalidArgument("base_revision", "personal object revision is out of range")
	}
	outputBytes, err := json.Marshal(struct {
		ObjectType string `json:"object_type"`
		ObjectID   string `json:"object_id"`
		Revision   int64  `json:"revision"`
	}{mutation.ObjectType, mutation.ObjectID, mutation.BaseRevision + 1})
	if err != nil || len(outputBytes) > maxAgentToolOutputBytes {
		return AgentToolResult{}, errors.New("personal mutation receipt is invalid")
	}
	output := string(outputBytes)
	if recovering && checkpointOutput != nil && *checkpointOutput != output {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed personal mutation result changed")
	}
	commit := CloudDSHPersonalMutationCommit{
		TaskID: current.ID, OwnerID: current.OwnerID, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version, CallID: call.CallID, Mutation: mutation,
	}
	commit.FingerprintSHA256 = cloudDSHPersonalMutationFingerprint(commit)
	usage := cloneTaskUsage(current.Usage)
	if usage.ToolCalls == math.MaxInt64 || usage.ToolCalls >= current.Budget.ToolCalls {
		return AgentToolResult{}, domain.NewError(domain.CodeQuotaExceeded, "task tool call budget is exhausted")
	}
	usage.ToolCalls++
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepSucceeded,
		"Update personal data", "Saved an authorized personal object with revision checking.")
	if err != nil {
		return AgentToolResult{}, err
	}
	detail := map[string]any{
		"tool": cloudDSHPersonalUpdateToolName, "personal_call_id": commit.CallID,
		"personal_mutation_id": mutation.MutationID, "agent_session_id": commit.AgentSessionID,
		"authorization_version": commit.AuthorizationVersion, "object_type": mutation.ObjectType,
		"object_id": mutation.ObjectID, "revision": mutation.BaseRevision + 1,
		"request_sha256": hex.EncodeToString(mutation.RequestHash), "fingerprint_sha256": commit.FingerprintSHA256,
	}
	if err := b.commitTaskPersonalMutation(ctx, current, step, usage, detail, commit); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: output}, nil
}

func (b *CloudDSHToolBroker) commitTaskPersonalMutation(ctx context.Context, current task.Task, step task.Step, usage task.Usage,
	detail any, commit CloudDSHPersonalMutationCommit) error {
	payload, err := agentStepEvent(step, usage, detail)
	if err != nil {
		return err
	}
	_, err = b.store.CommitTaskProgress(ctx, TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Result: current.Result, Usage: usage,
		Steps: []task.Step{step}, Event: task.Event{TaskID: current.ID, Type: "task.step", Payload: payload},
		CloudDSHPersonalMutation: &commit,
	})
	return err
}

func cloudDSHPersonalMutationID(taskID, sessionID, callID string) string {
	digest := sha256.Sum256([]byte("morde-personal-mutation-v1\n" + taskID + "\n" + sessionID + "\n" + callID))
	return "dsh_" + hex.EncodeToString(digest[:])
}

func cloudDSHPersonalMutationFingerprint(value CloudDSHPersonalMutationCommit) string {
	input, _ := json.Marshal(struct {
		TaskID               string `json:"task_id"`
		OwnerID              string `json:"owner_id"`
		AgentSessionID       string `json:"agent_session_id"`
		AuthorizationVersion int64  `json:"authorization_version"`
		CallID               string `json:"call_id"`
		MutationID           string `json:"mutation_id"`
		RequestSHA256        string `json:"request_sha256"`
	}{value.TaskID, value.OwnerID, value.AgentSessionID, value.AuthorizationVersion, value.CallID,
		value.Mutation.MutationID, hex.EncodeToString(value.Mutation.RequestHash)})
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

func ValidCloudDSHPersonalMutationCommit(value CloudDSHPersonalMutationCommit) bool {
	if !domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) || value.Mutation.OwnerID != value.OwnerID ||
		value.AgentSessionID == "" || len(value.AgentSessionID) > 128 || !utf8.ValidString(value.AgentSessionID) ||
		value.AuthorizationVersion < 1 || value.CallID == "" || len(value.CallID) > 256 || !utf8.ValidString(value.CallID) ||
		value.Mutation.Delete || value.Mutation.MutationID != cloudDSHPersonalMutationID(value.TaskID, value.AgentSessionID, value.CallID) ||
		personal.ValidateMutation(value.Mutation) != nil || len(value.Mutation.RequestHash) != sha256.Size || value.Mutation.BaseRevision == math.MaxInt64 {
		return false
	}
	hash, err := personal.HashMutationContent(value.Mutation)
	return err == nil && hex.EncodeToString(hash) == hex.EncodeToString(value.Mutation.RequestHash) &&
		value.FingerprintSHA256 == cloudDSHPersonalMutationFingerprint(value)
}

func ValidateCloudDSHPersonalMutationEvent(payload []byte, value CloudDSHPersonalMutationCommit) error {
	if len(payload) == 0 || len(payload) > 16<<10 || !ValidCloudDSHPersonalMutationCommit(value) {
		return domain.NewError(domain.CodeInvalidState, "personal mutation event is invalid")
	}
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
		} `json:"detail"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Detail.Tool != cloudDSHPersonalUpdateToolName ||
		event.Detail.CallID != value.CallID || event.Detail.MutationID != value.Mutation.MutationID ||
		event.Detail.AgentSessionID != value.AgentSessionID || event.Detail.AuthorizationVersion != value.AuthorizationVersion ||
		event.Detail.ObjectType != value.Mutation.ObjectType || event.Detail.ObjectID != value.Mutation.ObjectID ||
		event.Detail.Revision != value.Mutation.BaseRevision+1 ||
		event.Detail.RequestSHA256 != hex.EncodeToString(value.Mutation.RequestHash) ||
		event.Detail.FingerprintSHA256 != value.FingerprintSHA256 {
		return domain.NewError(domain.CodeInvalidState, "personal mutation event fingerprint changed")
	}
	return nil
}
