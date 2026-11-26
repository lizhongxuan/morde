package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskRequirementManifestConfirmationIsImmutableAndRevisionBound(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "requirement-confirmation-waiting", false)
	current, eventSequence, digest := commitRequirementManifestPlan(t, fixture, 1, "requirement-confirmation-plan-1")
	if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE tasks SET status='waiting_user',waiting_reason='awaiting confirmation' WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}

	command := taskRequirementConfirmationCommand(current.ID, fixture.route.ownerID, *current.CurrentRevision, 1, eventSequence, digest, "confirm-requirement-plan-1")
	confirmed, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), command)
	if err != nil || confirmed.TaskID != current.ID || confirmed.TaskRevision != *current.CurrentRevision ||
		confirmed.PlanRevision != 1 || confirmed.PlanEventSequence != eventSequence || confirmed.ManifestDigest != digest ||
		confirmed.ConfirmedByUserID != fixture.route.ownerID || confirmed.ConfirmedByPrincipalKind != string(identity.KindSession) ||
		confirmed.TaskAuthorizationVersion != current.Authorization.Version || confirmed.ConfirmedAt.IsZero() {
		t.Fatalf("confirm current waiting-user manifest: value=%+v err=%v", confirmed, err)
	}
	readTx, err := fixture.route.store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer readTx.Rollback(context.Background())
	projected, found, err := readTaskRequirementManifestConfirmationForPlan(context.Background(), readTx,
		fixture.route.ownerID, current.ID, *current.CurrentRevision, 1, eventSequence,
		current.Authorization.Version, digest)
	if err != nil || !found || projected != confirmed {
		t.Fatalf("read exact confirmation projection: projection=%+v found=%v err=%v; want stored fact", projected, found, err)
	}
	projected, found, err = readTaskRequirementManifestConfirmationForPlan(context.Background(), readTx,
		fixture.route.ownerID, current.ID, *current.CurrentRevision, 1, eventSequence,
		current.Authorization.Version+1, digest)
	if err != nil || found || projected != (app.TaskRequirementManifestConfirmation{}) {
		t.Fatalf("stale authorization version returned confirmation: projection=%+v found=%v err=%v", projected, found, err)
	}
	replayed, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), command)
	if err != nil || replayed != confirmed {
		t.Fatalf("replay exact confirmation: first=%+v replay=%+v err=%v", confirmed, replayed, err)
	}
	newKey := command
	newKey.IdempotencyKey = "confirm-requirement-plan-1-second-key"
	if _, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), newKey); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second key confirmed an already-confirmed plan: %v", err)
	}
	wrongDigest := command
	wrongDigest.ManifestDigest = strings.Repeat("0", 64)
	wrongDigest.IdempotencyKey = "confirm-requirement-plan-1-wrong-digest"
	if _, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), wrongDigest); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("model/client digest mismatch was accepted: %v", err)
	}
	wrongSequence := command
	wrongSequence.PlanEventSequence++
	wrongSequence.IdempotencyKey = "confirm-requirement-plan-1-wrong-sequence"
	if _, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), wrongSequence); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale event sequence was accepted: %v", err)
	}

	if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE tasks SET status='running',waiting_reason='' WHERE id=$1`, current.ID); err != nil {
		t.Fatal(err)
	}
	_, nextSequence, _ := commitRequirementManifestPlan(t, fixture, 2, "requirement-confirmation-plan-2")
	if _, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), command); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("old plan revision remained confirmable after plan update (latest=%d): %v", nextSequence, err)
	}
	var stored int
	if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_requirement_manifest_confirmations WHERE task_id=$1`, current.ID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("rejected confirmations changed durable fact count=%d err=%v; want one", stored, err)
	}
}

func TestPostgresTaskRequirementManifestConfirmationAllowsCompletedAndFailsClosedAfterRevocation(t *testing.T) {
	t.Run("completed current plan", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "requirement-confirmation-completed", false)
		current, sequence, digest := commitRequirementManifestPlan(t, fixture, 1, "requirement-completed-plan-1")
		if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE tasks SET status='completed',lease_owner='',lease_expires_at=NULL,finished_at=clock_timestamp() WHERE id=$1`, current.ID); err != nil {
			t.Fatal(err)
		}
		command := taskRequirementConfirmationCommand(current.ID, fixture.route.ownerID, *current.CurrentRevision, 1, sequence, digest, "confirm-completed-plan-1")
		confirmed, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), command)
		if err != nil || confirmed.PlanEventSequence != sequence || confirmed.ManifestDigest != digest {
			t.Fatalf("confirm completed Task manifest: value=%+v err=%v", confirmed, err)
		}
	})

	t.Run("revoked current task authorization", func(t *testing.T) {
		fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "requirement-confirmation-revoked", false)
		current, sequence, digest := commitRequirementManifestPlan(t, fixture, 1, "requirement-revoked-plan-1")
		if _, err := fixture.route.store.pool.Exec(context.Background(), `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, current.Authorization.TokenID); err != nil {
			t.Fatal(err)
		}
		command := taskRequirementConfirmationCommand(current.ID, fixture.route.ownerID, *current.CurrentRevision, 1, sequence, digest, "confirm-revoked-plan-1")
		if _, err := fixture.route.store.ConfirmTaskRequirementManifest(context.Background(), command); domain.CodeOf(err) != domain.CodeUnauthenticated {
			t.Fatalf("revoked Task authorization confirmation error=%v; want unauthenticated", err)
		}
		var stored int
		if err := fixture.route.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM task_requirement_manifest_confirmations WHERE task_id=$1`, current.ID).Scan(&stored); err != nil || stored != 0 {
			t.Fatalf("revoked confirmation persisted rows=%d err=%v; want zero", stored, err)
		}
	})
}

func taskRequirementConfirmationCommand(taskID, actorID string, taskRevision, planRevision, eventSequence int64, digest, idempotencyKey string) app.TaskRequirementManifestConfirmationCommand {
	return app.TaskRequirementManifestConfirmationCommand{TaskID: taskID, ActorUserID: actorID,
		ActorPrincipalKind: string(identity.KindSession), TaskRevision: taskRevision, PlanRevision: planRevision,
		PlanEventSequence: eventSequence, ManifestDigest: digest, IdempotencyKey: idempotencyKey}
}

func commitRequirementManifestPlan(t *testing.T, fixture s03AgentTaskFixture, planRevision int64, callID string) (task.Task, int64, string) {
	t.Helper()
	ctx := context.Background()
	current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load current Task before manifest plan commit: task=%+v err=%v", current.Task, err)
	}
	var messageID, content string
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT id,content FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='user' AND source_kind IN ('task','conversation') AND delivery_status='applied'
ORDER BY sequence ASC,id ASC LIMIT 1`, current.Task.ID, current.Task.OwnerID, current.Task.ConversationID).Scan(&messageID, &content); err != nil {
		t.Fatalf("read applied requirement source for test manifest: %v", err)
	}
	sourceDigest := sha256.Sum256([]byte(content))
	obligation := task.PlanVerificationObligation{Ref: "required-tests", Statement: "Run the full test suite",
		Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeFullSuite, CoveragePaths: []string{"."},
		Source: task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			MessageID: messageID, ContentSHA256: hex.EncodeToString(sourceDigest[:])}}
	canonicalID, err := obligation.CanonicalID()
	if err != nil {
		t.Fatalf("canonicalize requirement obligation: %v", err)
	}
	obligation.ID, obligation.Ref = task.PlanVerificationRequirementID(canonicalID), ""
	plan := task.Plan{TaskRevision: *current.Task.CurrentRevision, Revision: planRevision, Goal: "Prepare verified result",
		Scope: "Run requested checks", VerificationMethod: "Run the user-requested full test suite", CostEstimateState: "unknown",
		RequirementManifest: &task.PlanVerificationRequirementManifest{Version: 1, Obligations: []task.PlanVerificationObligation{obligation}},
		Steps: []task.PlanStep{{ID: "verify", Title: "Run full test suite", Status: task.PlanStepPending,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &task.PlanVerificationCheck{
				Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired, Command: "go test ./...",
				ScopeMode: task.PlanVerificationScopeFullSuite, CoveragePaths: []string{"."},
				RequirementIDs: []task.PlanVerificationRequirementID{obligation.ID},
			}}},
	}
	digest, err := plan.RequirementManifestDigest()
	if err != nil {
		t.Fatalf("compute candidate requirement digest: %v", err)
	}
	plan.RequirementManifest.Digest = digest
	if err := plan.Validate(); err != nil {
		t.Fatalf("validate candidate requirement plan: %v", err)
	}
	var stepSequence int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&stepSequence); err != nil {
		t.Fatal(err)
	}
	progress := postgresCloudDSHTaskPlanProgress(t, current.Task, fixture.fence, plan, callID, mustID(t, domain.NewStepID), stepSequence)
	event, err := fixture.route.store.CommitTaskProgress(ctx, progress)
	if err != nil || event.Type != "task.plan" {
		t.Fatalf("commit candidate requirement plan: event=%+v err=%v", event, err)
	}
	return current.Task, event.Sequence, digest
}
