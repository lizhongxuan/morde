package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

func TestPostgresCloudDSHPersonalHistoryReadCommitFencesConsent(t *testing.T) {
	fixture := newAgentLifecycleFixture(t)
	store := fixture.store
	ctx := context.Background()
	lease, _, err := fixture.service.Start(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task: task=%+v err=%v", current.Task, err)
	}
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-consent-enable-1", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-memory-create-before-read", ObjectType: personal.ObjectTypeMemory,
		ObjectID: "global", Body: json.RawMessage(`{"content":"Synthetic memory phrase"}`),
	}); err != nil {
		t.Fatal(err)
	}
	initialSnapshot, err := store.GetPersonalSnapshotBatch(ctx, fixture.principal, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	unfenced := cloudDSHPersonalHistoryReadProgress(t, current.Task, lease.Fence,
		"call_history_read_unfenced", 1, initialSnapshot.Watermark, 1)
	unfenced.CloudDSHPersonalHistoryRead = nil
	if _, err := store.CommitTaskProgress(ctx, unfenced); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unfenced successful history result commit = %v, want invalid argument", err)
	}
	if _, err := store.CommitTaskProgress(ctx, cloudDSHPersonalHistoryReadProgress(t, current.Task, lease.Fence,
		"call_history_read_valid", 1, initialSnapshot.Watermark, 1)); err != nil {
		t.Fatalf("commit history read while consent is current: %v", err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-memory-delete-after-read", ObjectType: personal.ObjectTypeMemory,
		ObjectID: "global", BaseRevision: 1, Delete: true,
	}); err != nil {
		t.Fatal(err)
	}
	changedSnapshot, err := store.GetPersonalSnapshotBatch(ctx, fixture.principal, nil, 1)
	if err != nil || changedSnapshot.Watermark <= initialSnapshot.Watermark {
		t.Fatalf("memory deletion did not advance the personal change watermark: initial=%d current=%d err=%v",
			initialSnapshot.Watermark, changedSnapshot.Watermark, err)
	}
	if _, err := store.CommitTaskProgress(ctx, cloudDSHPersonalHistoryReadProgress(t, current.Task, lease.Fence,
		"call_history_read_stale_personal_data", 1, initialSnapshot.Watermark, 2)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("history read after personal data changed = %v, want invalid state", err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-consent-disable-1", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", BaseRevision: 1, Body: json.RawMessage(`{"enabled":false}`),
	}); err != nil {
		t.Fatal(err)
	}
	disabledSnapshot, err := store.GetPersonalSnapshotBatch(ctx, fixture.principal, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, cloudDSHPersonalHistoryReadProgress(t, stale.Task, lease.Fence,
		"call_history_read_stale", 1, disabledSnapshot.Watermark, 2)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("history read after consent revision changed = %v, want invalid state", err)
	}

	if _, err := personalData.ApplyPersonalMutation(ctx, fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-consent-enable-2", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", BaseRevision: 2, Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	raceSnapshot, err := store.GetPersonalSnapshotBatch(ctx, fixture.principal, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	disable, err := personalData.PreparePersonalMutation(fixture.principal, app.PersonalMutationCommand{
		MutationID: "history-consent-disable-race", ObjectType: personal.ObjectTypeSkillSetting,
		ObjectID: "builtin_memory", BaseRevision: 3, Body: json.RawMessage(`{"enabled":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := applyPersonalMutationInTx(ctx, tx, fixture.principal, disable, "", ""); err != nil {
		t.Fatal(err)
	}
	racing, err := store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	progress := cloudDSHPersonalHistoryReadProgress(t, racing.Task, lease.Fence,
		"call_history_read_race", 3, raceSnapshot.Watermark, 2)
	done := make(chan error, 1)
	go func() { _, err := store.CommitTaskProgress(ctx, progress); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("history result commit was not serialized behind consent withdrawal: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("history result commit after consent withdrawal = %v, want invalid state", err)
	}
	details, err := store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || details.Task.EventSequence != racing.Task.EventSequence || len(details.Steps) != 1 {
		t.Fatalf("withdrawn history result was persisted: task=%+v steps=%d err=%v", details.Task, len(details.Steps), err)
	}
}

func cloudDSHPersonalHistoryReadProgress(t *testing.T, current task.Task, fence app.AgentInstanceFence,
	callID string, settingRevision, personalChangeWatermark, sequence int64) app.TaskProgress {
	t.Helper()
	if current.CurrentRevision == nil {
		t.Fatal("Task current revision is unavailable")
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	toolName := "morde_recent_conversation_summary"
	usage := current.Usage
	usage.ToolCalls++
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: sequence, Kind: "tool", Status: task.StepSucceeded,
		Title: "Summarize prior personal history", Summary: "Read authorized personal conversation excerpts."}
	callDigest := sha256.Sum256([]byte(toolName + ":" + callID))
	detail := map[string]any{"tool": toolName, "call_id": callID, "authorization_version": fence.AuthorizationVersion,
		"memory_setting_revision": settingRevision, "personal_change_watermark": personalChangeWatermark}
	payload, err := json.Marshal(map[string]any{
		"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind, "status": step.Status,
		"title": step.Title, "summary": step.Summary, "usage_model_tokens_known": false,
		"usage_active_seconds": usage.ActiveSeconds, "usage_tool_calls": usage.ToolCalls, "detail": detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app.TaskProgress{
		TaskID: current.ID, LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
		Event: task.Event{TaskID: current.ID, Type: "task.step", Payload: payload},
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: fence.AgentSessionID, AgentInstanceID: fence.AgentInstanceID,
			AgentGeneration: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion,
			TaskRevision: *current.CurrentRevision, CallKey: callID, ToolName: toolName,
			RequestDigest: callDigest[:], Phase: string(task.StepSucceeded), DetailState: "not_recorded"},
		CloudDSHPersonalHistoryRead: &app.CloudDSHPersonalHistoryReadCommit{SettingRevision: settingRevision,
			PersonalChangeWatermark: personalChangeWatermark, CallID: callID, ToolName: toolName, AgentSessionID: fence.AgentSessionID,
			AuthorizationVersion: fence.AuthorizationVersion, TaskRevision: *current.CurrentRevision},
	}
}
