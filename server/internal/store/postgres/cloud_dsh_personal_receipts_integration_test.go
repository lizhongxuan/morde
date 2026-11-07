package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestCloudDSHPersonalReceiptRequiresTaskCheckpointAndAuthorization(t *testing.T) {
	f := newCloudDSHStreamFixture(t)
	ctx := context.Background()
	claimed := f.task
	claimed.AgentSessionID = f.delta.Fence.AgentSessionID
	claimed.Authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET auth_scopes=$2 WHERE id=$1`, claimed.ID, claimed.Authorization.Scopes); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: claimed.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: f.route.authorization.TokenID}
	service, err := app.NewPersonalMutationService(f.route.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{MutationID: "receipt-enable-memory",
		ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory", Body: json.RawMessage(`{"enabled":true}`)}); err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"content":"synthetic remembered preference","category":"preference"}`)
	progress := cloudDSHPersonalMutationProgress(t, claimed, principal, service, "receipt-call-1", 0, body)
	progress.Steps[0].Sequence = 2
	var event map[string]any
	if err := json.Unmarshal(progress.Event.Payload, &event); err != nil {
		t.Fatal(err)
	}
	event["sequence"] = 2
	progress.Event.Payload, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatalf("commit personal write: %v", err)
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
	request := app.CloudDSHPersonalReceiptRequest{Fence: fence, ThroughSequence: modelFence.ThroughSequence,
		CheckpointDigest: digest, Commit: *progress.CloudDSHPersonalMutation}
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, request); err != nil {
		t.Fatalf("committed personal write was not confirmed: %v", err)
	}
	changed := cloudDSHPersonalMutationProgress(t, claimed, principal, service, "receipt-call-1", 0,
		json.RawMessage(`{"content":"different data","category":"preference"}`))
	forged := request
	forged.Commit = *changed.CloudDSHPersonalMutation
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, forged); err == nil {
		t.Fatal("same call ID with different arguments was trusted")
	}
	forged = request
	forged.CheckpointDigest = bytes.Repeat([]byte{9}, 32)
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, forged); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("changed checkpoint was trusted: %v", err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=1 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, request); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed Task revision was trusted: %v", err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=0 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, request); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked credential was trusted: %v", err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.route.store.pool.Exec(ctx, `DELETE FROM personal_mutation_receipts WHERE owner_id=$1 AND mutation_id=$2`, claimed.OwnerID, request.Commit.Mutation.MutationID); err != nil {
		t.Fatal(err)
	}
	if err := f.route.store.ConfirmCloudDSHPersonalMutationReceipt(ctx, request); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("missing mutation receipt was trusted: %v", err)
	}
}
