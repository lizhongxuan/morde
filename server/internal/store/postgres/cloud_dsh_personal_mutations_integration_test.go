package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if _, err := mutationService.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "enable-memory-1", ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory",
		Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
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
	var sourceID, sourceTaskID, sourceConversationID string
	var sourceSequence int64
	var sourceDigest []byte
	if err := store.pool.QueryRow(ctx, `SELECT source_id,source_task_id,source_conversation_id,source_sequence,source_sha256
FROM personal_memory_sources WHERE owner_id=$1 AND object_id='global' AND object_revision=1 AND source_kind='conversation_message'`, owner.ID).
		Scan(&sourceID, &sourceTaskID, &sourceConversationID, &sourceSequence, &sourceDigest); err != nil {
		t.Fatalf("read Task memory provenance: %v", err)
	}
	wantSourceDigest := sha256.Sum256([]byte(created.Message.Content))
	if sourceID != created.Message.ID || sourceTaskID != created.Task.ID || sourceConversationID != created.Task.ConversationID ||
		sourceSequence != created.Message.Sequence || !strings.EqualFold(hex.EncodeToString(sourceDigest), hex.EncodeToString(wantSourceDigest[:])) {
		t.Fatalf("personal memory provenance = id=%q task=%q conversation=%q sequence=%d digest=%x", sourceID, sourceTaskID,
			sourceConversationID, sourceSequence, sourceDigest)
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
	if _, err := mutationService.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "disable-memory-1", ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory",
		BaseRevision: 1, Body: json.RawMessage(`{"enabled":false}`),
	}); err != nil {
		t.Fatal(err)
	}
	if replayed, err := store.CommitTaskProgress(ctx, progress); err != nil || replayed.Sequence != first.Sequence {
		t.Fatalf("committed mutation did not replay after withdrawal: %+v, %v", replayed, err)
	}
	disabled := cloudDSHPersonalMutationProgress(t, claimed, principal, mutationService, "call_personal_write_disabled", 1,
		json.RawMessage(`{"content":"must-not-write-disabled"}`))
	if _, err := store.CommitTaskProgress(ctx, disabled); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("disabled memory mutation error = %v, want forbidden", err)
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

func TestPostgresDeletedPersonalMemorySuppressesMatchingAIHistoryOnlyAndCannotRollBack(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	deletedText := "我喜欢喝乌龙茶"
	deletedConversation, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "deleted-memory-history", "我的偏好是："+deletedText))
	if err != nil {
		t.Fatal(err)
	}
	unrelatedConversation, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "unrelated-memory-history", "乌龙茶适合用 90 度左右的水冲泡"))
	if err != nil {
		t.Fatal(err)
	}
	otherOwnerConversation, err := store.SubmitTask(ctx, newTestTaskSubmission(t, otherOwner.ID, "", "other-owner-history", "我的偏好是："+deletedText))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	mutations, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutations.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "remember-history-1", ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000001",
		Body: json.RawMessage(`{"content":"` + deletedText + `","category":"preference"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mutations.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "forget-history-1", ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000001", BaseRevision: 1, Delete: true,
	}); err != nil {
		t.Fatal(err)
	}

	deletedIDs, err := store.ListSuppressedPersonalHistoryMessageIDs(ctx, owner.ID, []string{
		deletedConversation.Message.ID, unrelatedConversation.Message.ID, otherOwnerConversation.Message.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deletedIDs[deletedConversation.Message.ID]; !ok {
		t.Fatalf("matching personal history message was not suppressed: %+v", deletedIDs)
	}
	if _, ok := deletedIDs[unrelatedConversation.Message.ID]; ok {
		t.Fatalf("unrelated history message was suppressed: %+v", deletedIDs)
	}
	if _, ok := deletedIDs[otherOwnerConversation.Message.ID]; ok {
		t.Fatalf("another owner's history message was suppressed: %+v", deletedIDs)
	}

	visible, err := store.ListMessages(ctx, owner.ID, deletedConversation.Conversation.ID, app.PageRequest{Limit: 10})
	if err != nil || len(visible.Items) != 1 || !strings.Contains(visible.Items[0].Content, deletedText) {
		t.Fatalf("personal memory deletion changed user-visible conversation history: messages=%+v err=%v", visible.Items, err)
	}

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 179); err == nil || !strings.Contains(err.Error(), "cannot roll back personal memory source provenance") {
		t.Fatalf("memory source provenance rollback = %v", err)
	}
	deletedIDs, err = store.ListSuppressedPersonalHistoryMessageIDs(ctx, owner.ID, []string{deletedConversation.Message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deletedIDs[deletedConversation.Message.ID]; !ok {
		t.Fatalf("history suppression tombstone was lost after refused rollback: %+v", deletedIDs)
	}
}

func TestPersonalHistorySuppressionRollbackGuard(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "history-suppression-rollback", "deleted preference"))
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO personal_history_suppressions(service_instance_id,owner_id,message_id)
VALUES ($1::uuid,$2,$3)`, serviceInstanceID, owner.ID, created.Message.ID); err != nil {
		t.Fatal(err)
	}
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 168); err == nil || !strings.Contains(err.Error(), "cannot roll back personal history suppression") {
		t.Fatalf("history suppression rollback with deletion tombstones error = %v", err)
	}
}

func TestPostgresCloudDSHMemoryWriteCannotRaceConsentWithdrawal(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "memory-concurrent-task", "remember preference")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "memory-concurrent-worker", 20*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	claimed.AgentSessionID = "session_memory_race"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_session_id=$2 WHERE id=$1`, claimed.ID, claimed.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	service, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{MutationID: "memory-race-enable", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	disable, err := service.PreparePersonalMutation(principal, app.PersonalMutationCommand{MutationID: "memory-race-disable", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", BaseRevision: 1, Body: json.RawMessage(`{"enabled":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := applyPersonalMutationInTx(ctx, tx, principal, disable, "", ""); err != nil {
		t.Fatal(err)
	}
	progress := cloudDSHPersonalMutationProgress(t, claimed, principal, service, "memory_racing_write", 0, json.RawMessage(`{"content":"should not be written"}`))
	done := make(chan error, 1)
	go func() { _, err := store.CommitTaskProgress(ctx, progress); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("write was not serialized behind withdrawal: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("write after withdrawal = %v", err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("memory appeared after withdrawal: %v", err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := broker.PausePersonalMemoryRecovery(ctx, claimed); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "dsh_recovery_unresolved" ||
		len(details.Steps) != 1 || !strings.Contains(details.Steps[0].Summary, "请重新发送消息") {
		t.Fatalf("memory pause was not durable or duplicated: %+v, %v", details, err)
	}
	if next, found, err := store.ClaimNextTask(ctx, "other-memory-worker", 20*time.Second); err != nil || found {
		t.Fatalf("paused memory task was reclaimed: %+v found=%t err=%v", next, found, err)
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
