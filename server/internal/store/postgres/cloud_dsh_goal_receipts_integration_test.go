package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestCloudDSHGoalReceiptRejectsTamperingRevocationAndPartialWrites(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	claimed := f.task
	claimed.AgentSessionID = f.delta.Fence.AgentSessionID
	claimed.AgentInstanceID = f.delta.Fence.AgentInstanceID
	claimed.AgentGeneration = f.delta.Fence.AgentGeneration
	claimed.AgentEngineVersion = f.delta.Fence.EngineVersion
	claimed.AgentProfileVersion = f.delta.Fence.ProfileVersion
	claimed.Authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET auth_scopes=$2 WHERE id=$1`, claimed.ID, claimed.Authorization.Scopes); err != nil {
		t.Fatal(err)
	}
	personalData, err := app.NewPersonalMutationService(f.route.store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(f.route.store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, f.route.store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := app.NewPersonalAttachmentService(f.route.store, f.route.store)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(f.route.store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(f.route.store, &app.RuntimeToolExecutor{}, nil, nil, capture,
		personalData, personalSync, goals, attachments, nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid := app.ModelToolCall{CallID: "goal-rollback", Name: "morde_goal_create", Arguments: `{"title":"synthetic goal","description":"","status":"active","steps":[],"related_material_ids":["missing_item"]}`}
	if _, err := broker.Execute(ctx, claimed, invalid); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("non-owned Goal material was accepted: %v", err)
	}
	var count int
	if err := f.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND payload #>> '{detail,personal_call_id}'=$2`,
		claimed.ID, invalid.CallID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed Goal write left a Task event: count=%d err=%v", count, err)
	}
	if err := f.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_mutation_receipts WHERE owner_id=$1 AND object_type='goal'`,
		claimed.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed Goal write left a mutation receipt: count=%d err=%v", count, err)
	}
	details, err := f.route.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.Usage.ToolCalls != 0 {
		t.Fatalf("failed Goal write consumed usage: usage=%d err=%v", details.Task.Usage.ToolCalls, err)
	}
	call := app.ModelToolCall{CallID: "goal-verified", Name: "morde_goal_create", Arguments: `{"title":"synthetic goal","description":"private body","status":"active","steps":[],"related_material_ids":[]}`}
	result, err := broker.Execute(ctx, claimed, call)
	if err != nil {
		t.Fatalf("commit authorized Goal: %v", err)
	}
	var output struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(result.Output), &output) != nil || output.ID == "" {
		t.Fatalf("invalid Goal result: %s", result.Output)
	}
	var digest []byte
	if err := f.route.store.pool.QueryRow(ctx, `SELECT checkpoint_digest FROM tasks WHERE id=$1`, claimed.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	modelFence := f.delta.Fence
	fence := app.AgentInstanceFence{TaskID: modelFence.TaskID, OwnerID: modelFence.OwnerID, InitiatorID: modelFence.InitiatorID,
		AgentInstanceID: modelFence.AgentInstanceID, AgentGeneration: modelFence.AgentGeneration, AgentSessionID: modelFence.AgentSessionID,
		SupervisorID: "stream-test-supervisor", LeaseOwner: modelFence.LeaseOwner, LeaseVersion: modelFence.LeaseVersion,
		AuthorizationVersion: modelFence.AuthorizationVersion, EngineVersion: modelFence.EngineVersion, ProfileVersion: modelFence.ProfileVersion}
	argumentsHash := sha256.Sum256([]byte(call.Arguments))
	request := app.CloudDSHGoalReceiptRequest{Fence: fence, ThroughSequence: modelFence.ThroughSequence, CheckpointDigest: digest,
		CallID: call.CallID, ToolName: call.Name, GoalID: output.ID, ArgumentsSHA256: hex.EncodeToString(argumentsHash[:])}
	receipt, err := f.route.store.ConfirmCloudDSHGoalMutationReceipt(ctx, request)
	if err != nil || receipt.Mutation.ObjectID != output.ID || receipt.Status != "active" {
		t.Fatalf("authorized Goal receipt was not confirmed: receipt=%+v err=%v", receipt, err)
	}
	for label, change := range map[string]func(*app.CloudDSHGoalReceiptRequest){
		"arguments": func(value *app.CloudDSHGoalReceiptRequest) {
			value.ArgumentsSHA256 = hex.EncodeToString(bytes.Repeat([]byte{2}, 32))
		},
		"call id": func(value *app.CloudDSHGoalReceiptRequest) { value.CallID = "different-call" },
		"goal id": func(value *app.CloudDSHGoalReceiptRequest) { value.GoalID = "different_goal" },
		"checkpoint": func(value *app.CloudDSHGoalReceiptRequest) {
			value.CheckpointDigest = bytes.Repeat([]byte{9}, 32)
		},
		"generation": func(value *app.CloudDSHGoalReceiptRequest) { value.Fence.AgentGeneration++ },
	} {
		t.Run(label, func(t *testing.T) {
			changed := request
			change(&changed)
			if _, err := f.route.store.ConfirmCloudDSHGoalMutationReceipt(ctx, changed); err == nil {
				t.Fatalf("changed %s was trusted", label)
			}
		})
	}
	var original []byte
	if err := f.route.store.pool.QueryRow(ctx, `SELECT payload FROM task_events WHERE task_id=$1 AND payload #>> '{detail,personal_call_id}'=$2`,
		claimed.ID, call.CallID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE task_events SET payload=jsonb_set(payload,'{detail,goal_status}', '"done"'::jsonb)
WHERE task_id=$1 AND payload #>> '{detail,personal_call_id}'=$2`, claimed.ID, call.CallID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.ConfirmCloudDSHGoalMutationReceipt(ctx, request); err == nil {
		t.Fatal("changed Goal status was trusted")
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE task_events SET payload=$3::jsonb WHERE task_id=$1 AND payload #>> '{detail,personal_call_id}'=$2`,
		claimed.ID, call.CallID, original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, claimed.Authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.ConfirmCloudDSHGoalMutationReceipt(ctx, request); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked credential was trusted: %v", err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, claimed.Authorization.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `DELETE FROM personal_mutation_receipts WHERE owner_id=$1 AND mutation_id=$2`,
		claimed.OwnerID, receipt.Mutation.MutationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.ConfirmCloudDSHGoalMutationReceipt(ctx, request); err == nil {
		t.Fatal("Goal event without mutation ledger was trusted")
	}
}
