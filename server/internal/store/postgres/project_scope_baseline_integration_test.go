package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestProjectScopeMigrationBackfillsLegacyPersonalHistory(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	other := createTestUser(t, store, 4, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	conversationID := mustID(t, domain.NewConversationID)
	taskID := mustID(t, domain.NewTaskID)
	userMessageID := mustID(t, domain.NewMessageID)
	assistantMessageID := mustID(t, domain.NewMessageID)
	if _, err := provider.DownTo(ctx, 109); err != nil {
		t.Fatalf("roll back project scope migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, title)
VALUES ($1,$2,$3,'legacy personal conversation')`, conversationID, owner.ID, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind,
 budget_active_seconds, budget_tool_calls, budget_model_tokens)
VALUES ($1,$2,$3,$4,'model-test','legacy task','queued','session',60,10,1000)`, taskID, owner.ID, conversationID, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id, conversation_id, owner_id, role, content, task_id, sequence)
VALUES ($1,$2,$3,'user','legacy question',$4,1),($5,$2,$3,'assistant','legacy answer',$4,2)`,
		userMessageID, conversationID, owner.ID, taskID, assistantMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 110); err != nil {
		t.Fatalf("apply project scope migration: %v", err)
	}
	var conversationProjectID, taskProjectID pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT project_id FROM conversations WHERE id=$1`, conversationID).Scan(&conversationProjectID); err != nil || conversationProjectID.Valid {
		t.Fatalf("legacy conversation project scope = %#v, err=%v", conversationProjectID, err)
	}
	var initiatorID string
	if err := pool.QueryRow(ctx, `SELECT project_id, initiator_id FROM tasks WHERE id=$1`, taskID).Scan(&taskProjectID, &initiatorID); err != nil || taskProjectID.Valid || initiatorID != owner.ID {
		t.Fatalf("legacy task scope/initiator = %#v/%q, want NULL personal scope/%q; err=%v", taskProjectID, initiatorID, owner.ID, err)
	}
	var actorIDs []string
	rows, err := pool.Query(ctx, `SELECT actor_id FROM conversation_messages WHERE conversation_id=$1 ORDER BY sequence`, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var actorID string
		if err := rows.Scan(&actorID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		actorIDs = append(actorIDs, actorID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(actorIDs) != 2 || actorIDs[0] != owner.ID || actorIDs[1] != owner.ID {
		t.Fatalf("legacy message actors = %#v, want owner %q", actorIDs, owner.ID)
	}
	if _, err := store.GetConversation(ctx, conversationID, owner.ID); err != nil {
		t.Fatalf("owner cannot read preserved personal conversation: %v", err)
	}
	if _, err := store.GetConversation(ctx, conversationID, other.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other user conversation read = %v, want not found", err)
	}
	if _, err := store.GetTask(ctx, taskID, owner.ID); err != nil {
		t.Fatalf("owner cannot read preserved personal task: %v", err)
	}
	if _, err := store.GetTask(ctx, taskID, other.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other user task read = %v, want not found", err)
	}

	newConversationID := mustID(t, domain.NewConversationID)
	newTaskID := mustID(t, domain.NewTaskID)
	newMessageID := mustID(t, domain.NewMessageID)
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id,owner_id,host_id,title)
VALUES ($1,$2,$3,'current personal conversation')`, newConversationID, owner.ID, hostValue.ID); err != nil {
		t.Fatalf("insert current personal conversation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,host_id,model_id,objective,status,auth_kind,
 budget_active_seconds,budget_tool_calls,budget_model_tokens)
VALUES ($1,$2,$3,$4,'model-test','current personal task','queued','session',60,10,1000)`,
		newTaskID, owner.ID, newConversationID, hostValue.ID); err != nil {
		t.Fatalf("legacy task writer after scope migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,sequence)
VALUES ($1,$2,$3,'user','current personal message',$4,1)`, newMessageID, newConversationID, owner.ID, newTaskID); err != nil {
		t.Fatalf("legacy message writer after scope migration: %v", err)
	}
	var newInitiatorID, newActorID string
	if err := pool.QueryRow(ctx, `SELECT t.initiator_id,m.actor_id FROM tasks t
JOIN conversation_messages m ON m.task_id=t.id WHERE t.id=$1`, newTaskID).Scan(&newInitiatorID, &newActorID); err != nil ||
		newInitiatorID != owner.ID || newActorID != owner.ID {
		t.Fatalf("current personal task/message actors = %q/%q, want %q; err=%v", newInitiatorID, newActorID, owner.ID, err)
	}
}

func TestProjectScopeMigrationGuardsProjectMembersAndCompositeReferences(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 30_000, 60_000)
	member := createTestUser(t, store, 8, 30_000, 60_000)
	outsider := createTestUser(t, store, 8, 30_000, 60_000)
	projectID := createProjectScopeFixture(t, ctx, pool, owner.ID)
	otherProjectID := createProjectScopeFixture(t, ctx, pool, owner.ID)
	missingMemberProjectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	missingMemberDigest := sha256.Sum256([]byte(missingMemberProjectID))
	missingMemberTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingMemberTx.Exec(ctx, `INSERT INTO projects
(id,owner_id,name,create_idempotency_key,create_request_digest)
VALUES ($1,$2,'owner membership check','project-scope-' || $1,$3)`, missingMemberProjectID, outsider.ID, missingMemberDigest[:]); err != nil {
		_ = missingMemberTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := missingMemberTx.Commit(ctx); !isSQLState(err, "23503") {
		_ = missingMemberTx.Rollback(ctx)
		t.Fatalf("project without matching owner member commit error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_members (project_id,user_id,role)
VALUES ($1,$2,'member')`, projectID, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_members (project_id,user_id,role,version)
VALUES ($1,$2,'member',0)`, projectID, outsider.ID); !isSQLState(err, "23514") {
		t.Fatalf("zero project member version error = %v, want check violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_invitations
(id,project_id,inviter_id,invitee_id,role,state,version,create_idempotency_key,create_request_digest)
VALUES ($1,$2,$3,$4,'member','pending',0,'project-invite-version-001',$5)`,
		mustID(t, domain.NewPendingRequestID), projectID, owner.ID, outsider.ID, make([]byte, 32)); !isSQLState(err, "23514") {
		t.Fatalf("zero project invitation version error = %v, want check violation", err)
	}

	conversationID := mustID(t, domain.NewConversationID)
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id,owner_id,title,project_id)
VALUES ($1,$2,'forbidden project conversation',$3)`, mustID(t, domain.NewConversationID), outsider.ID, projectID); !isSQLState(err, "23503") {
		t.Fatalf("nonmember project conversation error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id,owner_id,title,project_id)
VALUES ($1,$2,'missing project conversation','j9999999')`, mustID(t, domain.NewConversationID), owner.ID); !isSQLState(err, "23503") {
		t.Fatalf("invalid project reference error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id,owner_id,title,project_id)
VALUES ($1,$2,'valid project conversation',$3)`, conversationID, owner.ID, projectID); err != nil {
		t.Fatalf("create project-scoped conversation: %v", err)
	}
	taskID := mustID(t, domain.NewTaskID)
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,model_id,objective,status,auth_kind,budget_active_seconds,
 budget_tool_calls,budget_model_tokens,project_id,initiator_id)
VALUES ($1,$2,$3,'model-test','mismatched project','queued','session',60,10,1000,$4,$2)`,
		mustID(t, domain.NewTaskID), owner.ID, conversationID, otherProjectID); !isSQLState(err, "23503") {
		t.Fatalf("mismatched task project reference error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,model_id,objective,status,auth_kind,budget_active_seconds,
 budget_tool_calls,budget_model_tokens,project_id,initiator_id)
VALUES ($1,$2,$3,'model-test','nonmember initiator','queued','session',60,10,1000,$4,$5)`,
		mustID(t, domain.NewTaskID), owner.ID, conversationID, projectID, outsider.ID); !isSQLState(err, "23503") {
		t.Fatalf("nonmember task initiator error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,model_id,objective,status,auth_kind,budget_active_seconds,
 budget_tool_calls,budget_model_tokens,project_id,initiator_id)
VALUES ($1,$2,$3,'model-test','valid project task','queued','session',60,10,1000,$4,$5)`,
		taskID, owner.ID, conversationID, projectID, member.ID); err != nil {
		t.Fatalf("create project-scoped task: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,sequence,actor_id)
VALUES ($1,$2,$3,'user','valid member message',$4,1,$5)`,
		mustID(t, domain.NewMessageID), conversationID, owner.ID, taskID, member.ID); err != nil {
		t.Fatalf("create project member message: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,sequence,actor_id)
VALUES ($1,$2,$3,'user','outsider message',$4,2,$5)`,
		mustID(t, domain.NewMessageID), conversationID, owner.ID, taskID, outsider.ID); !isSQLState(err, "23503") {
		t.Fatalf("nonmember message actor error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectID, member.ID); err != nil {
		t.Fatalf("revoke project member with existing history: %v", err)
	}
	var retainedMessages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND actor_id=$2`, taskID, member.ID).Scan(&retainedMessages); err != nil || retainedMessages != 1 {
		t.Fatalf("revoked member history count = %d, err=%v; want retained row", retainedMessages, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,role,content,task_id,sequence,actor_id)
VALUES ($1,$2,$3,'user','revoked member message',$4,2,$5)`,
		mustID(t, domain.NewMessageID), conversationID, owner.ID, taskID, member.ID); !isSQLState(err, "23503") {
		t.Fatalf("revoked project member message error = %v, want foreign key violation", err)
	}

	badVersionID := "j0000000"
	if _, err := pool.Exec(ctx, `INSERT INTO projects
(id,owner_id,name,version,create_idempotency_key,create_request_digest)
VALUES ($1,$2,'invalid version',0,'project-bad-version-001',$3)`, badVersionID, outsider.ID, make([]byte, 32)); !isSQLState(err, "23514") {
		t.Fatalf("zero project version error = %v, want check violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_content_revisions
(project_id,kind,content_id,revision,body,updated_by,idempotency_key,request_digest)
VALUES ($1,'memory','mem-1',0,'{}'::jsonb,$2,'project-content-version-001',$3)`, projectID, owner.ID, make([]byte, 32)); !isSQLState(err, "23514") {
		t.Fatalf("zero project content revision error = %v, want check violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_budgets
(project_id,version,currency,limit_micros,updated_by,idempotency_key,request_digest)
VALUES ($1,0,'USD',1000000,$2,'project-budget-version-001',$3)`, projectID, owner.ID, make([]byte, 32)); !isSQLState(err, "23514") {
		t.Fatalf("zero project budget version error = %v, want check violation", err)
	}

	otherOwner := createTestUser(t, store, 3, 10_000, 20_000)
	foreignHost := createTestHost(t, ctx, store, otherOwner.ID, app.Capacity{CPUMilli: 50_000, MemMiB: 100_000}, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO project_resources
(project_id,kind,resource_id,resource_owner_id,host_id,updated_by,version)
VALUES ($1,'host',$2,$3,$2,$4,0)`, projectID, foreignHost.ID, otherOwner.ID, owner.ID); !isSQLState(err, "23514") {
		t.Fatalf("zero project resource version error = %v, want check violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_resources
(project_id,kind,resource_id,resource_owner_id,host_id,updated_by,version)
VALUES ($1,'host',$2,$3,$2,$4,1)`, projectID, foreignHost.ID, owner.ID, owner.ID); !isSQLState(err, "23503") {
		t.Fatalf("resource with mismatched owner error = %v, want foreign key violation", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_resources
(project_id,kind,resource_id,resource_owner_id,host_id,updated_by,version)
VALUES ($1,'host',$2,$3,$2,$4,1)`, projectID, foreignHost.ID, otherOwner.ID, owner.ID); err != nil {
		t.Fatalf("resource with matching owner composite reference: %v", err)
	}
}

func createProjectScopeFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ownerID string) string {
	t.Helper()
	projectID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(projectID))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO projects
(id,owner_id,name,create_idempotency_key,create_request_digest)
VALUES ($1,$2,'scope test','project-scope-' || $1,$3)`, projectID, ownerID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_members (project_id,user_id,role)
VALUES ($1,$2,'owner')`, projectID, ownerID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return projectID
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
