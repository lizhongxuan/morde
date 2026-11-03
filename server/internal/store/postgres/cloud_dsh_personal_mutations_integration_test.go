package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

func TestPostgresCloudDSHPersonalMutationIsAtomicFencedAndRedacted(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "personal-data-task", "remember a preference")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "personal-data-worker", 20_000_000_000)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", ok, claimed, err)
	}
	claimed.AgentSessionID = "session_personal_data_1"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_session_id=$2 WHERE id=$1`, claimed.ID, claimed.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	mutationService, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"content":"private-personal-body-7f9c","category":"preference"}`)
	progress := cloudDSHPersonalMutationProgress(t, claimed, principal, mutationService, "call_personal_write_1", 0, body)
	initialSequence := claimed.EventSequence

	first, err := store.CommitTaskProgress(ctx, progress)
	if err != nil {
		t.Fatalf("commit personal mutation: %v", err)
	}
	if first.Sequence != initialSequence+1 {
		t.Fatalf("personal mutation event sequence = %d, want %d", first.Sequence, initialSequence+1)
	}
	stored, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || stored.Revision != 1 || !strings.Contains(string(stored.Body), "private-personal-body-7f9c") {
		t.Fatalf("personal object = %+v, err=%v", stored, err)
	}
	if strings.Contains(string(first.Payload), "private-personal-body-7f9c") || strings.Contains(string(first.Payload), "secret") {
		t.Fatalf("personal Task audit event leaked object content: %s", first.Payload)
	}

	// Replaying the same dsh call must return the original event without charging twice.
	replayed, err := store.CommitTaskProgress(ctx, progress)
	if err != nil || replayed.Sequence != first.Sequence {
		t.Fatalf("personal mutation replay event=%+v, first=%+v, err=%v", replayed, first, err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Usage.ToolCalls != 1 || details.Task.EventSequence != first.Sequence || len(details.Steps) != 1 {
		t.Fatalf("replay charged Task progress twice: task=%+v steps=%d err=%v", details.Task, len(details.Steps), err)
	}

	stale := cloudDSHPersonalMutationProgress(t, claimed, principal, mutationService, "call_personal_write_stale", 0,
		json.RawMessage(`{"content":"stale-body","category":"preference"}`))
	if _, err := store.CommitTaskProgress(ctx, stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale personal revision error = %v, want conflict", err)
	}
	stored, err = store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || stored.Revision != 1 || !strings.Contains(string(stored.Body), "private-personal-body-7f9c") {
		t.Fatalf("stale write changed the personal object: %+v, err=%v", stored, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	revoked := cloudDSHPersonalMutationProgress(t, claimed, principal, mutationService, "call_personal_write_revoked", 1,
		json.RawMessage(`{"content":"must-not-write","category":"preference"}`))
	if _, err := store.CommitTaskProgress(ctx, revoked); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked personal mutation error = %v, want unauthenticated", err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global"); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked session still reads personal data: %v", err)
	}
}

func cloudDSHPersonalMutationProgress(t *testing.T, claimed task.Task, principal identity.Principal, service *app.PersonalMutationService,
	callID string, baseRevision int64, body json.RawMessage) app.TaskProgress {
	t.Helper()
	mutationID := postgresCloudDSHPersonalMutationID(claimed.ID, claimed.AgentSessionID, callID)
	mutation, err := service.PreparePersonalMutation(principal, app.PersonalMutationCommand{
		MutationID: mutationID, ObjectType: personal.ObjectTypeMemory, ObjectID: "global", BaseRevision: baseRevision, Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	commit := app.CloudDSHPersonalMutationCommit{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, AgentSessionID: claimed.AgentSessionID,
		AuthorizationVersion: claimed.Authorization.Version, CallID: callID, Mutation: mutation,
	}
	commit.FingerprintSHA256 = postgresCloudDSHPersonalMutationFingerprint(commit)
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	step := task.Step{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Update personal data", Summary: "Saved an authorized personal object with revision checking."}
	usage := claimed.Usage
	usage.ToolCalls++
	detail := map[string]any{
		"tool": "morde_personal_object_update", "personal_call_id": callID,
		"personal_mutation_id": mutation.MutationID, "agent_session_id": commit.AgentSessionID,
		"authorization_version": commit.AuthorizationVersion, "object_type": mutation.ObjectType,
		"object_id": mutation.ObjectID, "revision": mutation.BaseRevision + 1,
		"request_sha256": hex.EncodeToString(mutation.RequestHash), "fingerprint_sha256": commit.FingerprintSHA256,
	}
	payload, err := json.Marshal(map[string]any{
		"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind, "status": step.Status,
		"title": step.Title, "summary": step.Summary, "usage_model_tokens_known": false,
		"usage_active_seconds": usage.ActiveSeconds, "usage_tool_calls": usage.ToolCalls, "detail": detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
		Event: task.Event{TaskID: claimed.ID, Type: "task.step", Payload: payload}, CloudDSHPersonalMutation: &commit,
	}
}

func postgresCloudDSHPersonalMutationID(taskID, sessionID, callID string) string {
	digest := sha256.Sum256([]byte("morde-personal-mutation-v1\n" + taskID + "\n" + sessionID + "\n" + callID))
	return "dsh_" + hex.EncodeToString(digest[:])
}

func postgresCloudDSHPersonalMutationFingerprint(value app.CloudDSHPersonalMutationCommit) string {
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
