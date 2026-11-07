package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type personalReceiptE2EModel struct{ calls int }

func (*personalReceiptE2EModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (m *personalReceiptE2EModel) Complete(context.Context, app.ModelCall) (app.ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "remember_1", Name: "morde_personal_object_update",
			Arguments: `{"object_type":"memory","object_id":"global","base_revision":0,"body":"{\"content\":\"a synthetic preference\"}"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	}
	return app.ModelResponse{Text: "已记录。", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
}

type failPersonalResultCheckpoint struct {
	worker.CloudDSHCheckpoints
	failed bool
}

func (f *failPersonalResultCheckpoint) Save(ctx context.Context, command app.AgentCheckpointCommand) (bool, error) {
	if !f.failed && bytes.Contains(command.Payload, []byte(`"tool/result"`)) {
		f.failed = true
		return false, errors.New("synthetic checkpoint interruption")
	}
	return f.CloudDSHCheckpoints.Save(ctx, command)
}

func TestCloudDSHPersonalReceiptRealNodeRestartDoesNotRepeatMutation(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	f.authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl)}
	ctx := context.Background()
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	personalData, err := app.NewPersonalMutationService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{MutationID: "personal-e2e-enable-memory",
		ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(f.store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, f.store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := app.NewPersonalAttachmentService(f.store, f.store)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(f.store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(f.store, &app.RuntimeToolExecutor{}, nil, nil, capture,
		personalData, personalSync, goals, attachments, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial := f.receive(t, "personal-receipt-e2e-initial", "记住我的偏好", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	first, found, err := f.store.ClaimNextTask(ctx, "personal-receipt-first", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim original Task: found=%t err=%v", found, err)
	}
	model := &personalReceiptE2EModel{}
	var injected *failPersonalResultCheckpoint
	executor := revisionTestExecutorWithOptions(t, f.store, model, func(checkpoints worker.CloudDSHCheckpoints) worker.CloudDSHCheckpoints {
		injected = &failPersonalResultCheckpoint{CloudDSHCheckpoints: checkpoints}
		return injected
	}, nil, broker)
	if err := executor.ExecuteTask(ctx, first); err == nil || !injected.failed {
		t.Fatalf("expected interruption after personal write: failed=%t err=%v", injected.failed, err)
	}
	object, err := f.store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || object.Revision != 1 {
		t.Fatalf("personal write did not commit: object=%+v err=%v", object, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	resumed, found, err := f.store.ClaimNextTask(ctx, "personal-receipt-recovery", time.Minute)
	if err != nil || !found || resumed.ID != first.ID || resumed.ConversationContext == nil {
		t.Fatalf("claim saved checkpoint: found=%t context=%+v err=%v", found, resumed.ConversationContext, err)
	}
	if err := executor.ExecuteTask(ctx, resumed); !errors.Is(err, worker.ErrCloudDSHCompletionPending) {
		t.Fatalf("recover committed personal write: %v", err)
	}
	object, err = f.store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, "global")
	if err != nil || object.Revision != 1 {
		t.Fatalf("personal write was repeated: object=%+v err=%v", object, err)
	}
	details, err := f.store.GetTask(ctx, first.ID, f.ownerID)
	if err != nil || details.Task.Usage.ToolCalls != 1 || model.calls != 2 {
		t.Fatalf("recovery duplicated usage or model work: usage=%d calls=%d err=%v", details.Task.Usage.ToolCalls, model.calls, err)
	}
}
