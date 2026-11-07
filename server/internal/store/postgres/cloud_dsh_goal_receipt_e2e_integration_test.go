package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type goalReceiptE2EModel struct {
	call  app.ModelToolCall
	calls int
}

func (*goalReceiptE2EModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (m *goalReceiptE2EModel) Complete(context.Context, app.ModelCall) (app.ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{m.call}, StopReason: "tool_calls",
			Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
	}
	return app.ModelResponse{Text: "已处理目标。", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
}

func TestCloudDSHGoalReceiptRealNodeRestart(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			f.authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl)}
			ctx := context.Background()
			principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
			personalData, err := app.NewPersonalMutationService(f.store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{MutationID: "goal-e2e-enable-memory",
				ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`)}); err != nil {
				t.Fatal(err)
			}
			goalID := "goal_e2e_seed"
			if operation != "create" {
				if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{MutationID: "goal-e2e-seed",
					ObjectType: personal.ObjectTypeGoal, ObjectID: goalID,
					Body: json.RawMessage(`{"title":"synthetic goal","description":"synthetic private body","status":"active","steps":[],"related_material_ids":[]}`)}); err != nil {
					t.Fatal(err)
				}
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
			initial := f.receive(t, "goal-receipt-"+operation, "处理一个合成目标", app.ConversationRoutingInput{})
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			first, found, err := f.store.ClaimNextTask(ctx, "goal-receipt-first", time.Minute)
			if err != nil || !found {
				t.Fatalf("claim original Task: found=%t err=%v", found, err)
			}
			model := &goalReceiptE2EModel{call: app.ModelToolCall{CallID: "goal_write_1", Name: "morde_goal_" + operation}}
			switch operation {
			case "create":
				model.call.Arguments = `{"title":"synthetic goal","description":"synthetic private body","status":"active","steps":[],"related_material_ids":[]}`
			case "update":
				model.call.Arguments = fmt.Sprintf(`{"goal_id":%q,"expected_revision":1,"title":null,"description":null,"status":"paused","steps":null}`, goalID)
			case "delete":
				model.call.Arguments = fmt.Sprintf(`{"goal_id":%q,"expected_revision":1}`, goalID)
			}
			var injected *failPersonalResultCheckpoint
			executor := revisionTestExecutorWithOptions(t, f.store, model, func(checkpoints worker.CloudDSHCheckpoints) worker.CloudDSHCheckpoints {
				injected = &failPersonalResultCheckpoint{CloudDSHCheckpoints: checkpoints}
				return injected
			}, nil, broker)
			if err := executor.ExecuteTask(ctx, first); err == nil || !injected.failed {
				t.Fatalf("expected interruption after goal write: failed=%t err=%v", injected.failed, err)
			}
			var event []byte
			if err := f.store.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND event_type='task.step'
AND payload #>> '{detail,tool}'=$2`, first.ID, model.call.Name).Scan(&event); err != nil {
				t.Fatalf("atomic goal event missing: %v", err)
			}
			var payload struct {
				Detail struct {
					ObjectID string `json:"object_id"`
				} `json:"detail"`
			}
			if json.Unmarshal(event, &payload) != nil || payload.Detail.ObjectID == "" ||
				strings.Contains(string(event), "synthetic goal") || strings.Contains(string(event), "synthetic private body") {
				t.Fatalf("invalid or unredacted goal event: %s", event)
			}
			goalID = payload.Detail.ObjectID
			object, err := f.store.GetPersonalObject(ctx, principal, personal.ObjectTypeGoal, goalID)
			if operation == "delete" {
				if domain.CodeOf(err) != domain.CodeNotFound {
					t.Fatalf("goal deletion did not commit: object=%+v err=%v", object, err)
				}
			} else {
				expectedRevision := int64(1)
				if operation == "update" {
					expectedRevision = 2
				}
				if err != nil || object.Revision != expectedRevision {
					t.Fatalf("goal write did not commit: object=%+v err=%v", object, err)
				}
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
				t.Fatal(err)
			}
			resumed, found, err := f.store.ClaimNextTask(ctx, "goal-receipt-recovery", time.Minute)
			if err != nil || !found || resumed.ID != first.ID || resumed.ConversationContext == nil {
				t.Fatalf("claim checkpoint: found=%t context=%+v err=%v", found, resumed.ConversationContext, err)
			}
			err = executor.ExecuteTask(ctx, resumed)
			if operation == "delete" {
				if err == nil || model.calls != 1 {
					t.Fatalf("deleted goal context was sent to the model: calls=%d err=%v", model.calls, err)
				}
			} else if !errors.Is(err, worker.ErrCloudDSHCompletionPending) || model.calls != 2 {
				t.Fatalf("committed goal write did not recover: calls=%d err=%v", model.calls, err)
			}
			details, err := f.store.GetTask(ctx, first.ID, f.ownerID)
			if err != nil || details.Task.Usage.ToolCalls != 1 {
				t.Fatalf("goal recovery duplicated usage: usage=%d err=%v", details.Task.Usage.ToolCalls, err)
			}
			if operation == "delete" && details.Task.Status != task.StatusWaitingUser {
				t.Fatalf("deleted goal recovery was not visibly paused: status=%s", details.Task.Status)
			}
		})
	}
}
