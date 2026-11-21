package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func h01ProjectHistoryFilterTask(t *testing.T, f projectAttachmentFixture, messageIDs ...string) task.Task {
	t.Helper()
	ctx := context.Background()
	const leaseOwner = "h01-project-history-worker"
	const agentInstance = "h01-project-history-agent"
	const agentSession = "h01-project-history-session"
	taskID := mustID(t, domain.NewTaskID)
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	var through int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM conversation_messages WHERE id=ANY($1::text[])`, messageIDs).Scan(&through); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,project_id,initiator_id,conversation_id,model_id,objective,status,auth_kind,auth_token_id,auth_scopes,
 auth_version,auth_expires_at,budget_active_seconds,budget_tool_calls,budget_model_tokens,execution_location,
 lease_owner,lease_version,lease_expires_at,agent_instance_id,agent_generation,agent_session_id)
VALUES ($1,$2,$3,$4,$5,'p1234567','history lookup','running','session',$6,$7,1,$8,120,5,1000,'server',
 $9,1,clock_timestamp()+interval '5 minutes',$10,1,$11)`, taskID, f.owner.UserID, f.projectID, f.member.UserID,
		f.conversationID, token.ID, []string{string(identity.ScopeTaskRead)}, token.ExpiresAt, leaseOwner, agentInstance, agentSession); err != nil {
		t.Fatal(err)
	}
	return task.Task{ID: taskID, OwnerID: f.owner.UserID, ProjectID: f.projectID, InitiatorID: f.member.UserID,
		ConversationID: f.conversationID, ExecutionLocation: "server", Status: task.StatusRunning,
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID,
			Scopes: []string{string(identity.ScopeTaskRead)}, Version: 1, ExpiresAt: &token.ExpiresAt},
		Lease:           task.Lease{Owner: leaseOwner, Version: 1, ExpiresAt: ptrTime(time.Now().Add(5 * time.Minute))},
		AgentInstanceID: agentInstance, AgentGeneration: 1, AgentSessionID: agentSession,
		ConversationContext: &task.ConversationContext{OwnerID: f.owner.UserID, ProjectID: f.projectID, InitiatorID: f.member.UserID,
			ConversationID: f.conversationID, Scope: "project", ThroughSequence: through}}
}

func TestFilterTaskProjectConversationHistoryRequiresCurrentTaskAndMembership(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	own := projectHistoryInsertMessage(t, f, f.member.UserID, "", "", "member's applied message", conversation.RoleUser)
	other := projectHistoryInsertMessage(t, f, f.owner.UserID, "", "", "unverified other-member message", conversation.RoleUser)
	current := h01ProjectHistoryFilterTask(t, f, own, other)
	future := projectHistoryInsertMessage(t, f, f.member.UserID, "", "", "message after the Task snapshot", conversation.RoleUser)

	allowed, err := f.store.FilterTaskProjectConversationHistory(ctx, current, []string{own, other, future})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allowed[own]; !ok {
		t.Fatalf("current member's applied history was filtered: %v", allowed)
	}
	if _, ok := allowed[other]; ok {
		t.Fatalf("unverified other-member history was returned: %v", allowed)
	}
	if _, ok := allowed[future]; ok {
		t.Fatalf("message newer than the Task snapshot was returned: %v", allowed)
	}
	empty, err := f.store.FilterTaskProjectConversationHistory(ctx, current, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("authorization-only check returned %v, %v", empty, err)
	}

	stale := current
	stale.Authorization.Version++
	if _, err := f.store.FilterTaskProjectConversationHistory(ctx, stale, nil); err == nil {
		t.Fatal("history authorization accepted a stale Task authorization version")
	}
	outsider := current
	outsider.InitiatorID = f.outsider.UserID
	if _, err := f.store.FilterTaskProjectConversationHistory(ctx, outsider, nil); err == nil {
		t.Fatal("history authorization accepted a non-member actor")
	}

	projectValue, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveProjectMember(ctx, f.owner.UserID, f.projectID, f.member.UserID, projectValue.Version,
		"h01-project-history-revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FilterTaskProjectConversationHistory(ctx, current, nil); err == nil {
		t.Fatal("revoked project member retained history access")
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
