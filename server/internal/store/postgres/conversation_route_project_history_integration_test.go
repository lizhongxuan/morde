package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func projectHistoryClaim(t *testing.T, f projectAttachmentFixture, clientID string) app.ConversationRouteClaim {
	t.Helper()
	return projectHistoryClaimWithReference(t, f, clientID, "", "synthetic follow up")
}

func projectHistoryClaimWithReference(t *testing.T, f projectAttachmentFixture, clientID, referenceID, content string) app.ConversationRouteClaim {
	t.Helper()
	ctx := context.Background()
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: digest[:],
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567", ReferenceTaskID: referenceID,
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "project-history-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim project message: id=%q found=%t err=%v", claim.Message.ID, found, err)
	}
	return claim
}

func projectHistoryInsertMessage(t *testing.T, f projectAttachmentFixture, actorID, taskID, source, content string, role conversation.Role) string {
	t.Helper()
	messageID := mustID(t, domain.NewMessageID)
	ctx := context.Background()
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: f.owner.UserID, ActorID: actorID,
			ConversationID: f.conversationID, TaskID: taskID, SourceKind: source, Content: content, Role: role})
	}); err != nil {
		t.Fatal(err)
	}
	return messageID
}

func projectHistoryAssert(t *testing.T, history []app.ConversationRouteHistory, included, excluded []string) {
	t.Helper()
	got := make(map[string]bool, len(history))
	for _, item := range history {
		got[item.MessageID] = true
	}
	for _, id := range included {
		if !got[id] {
			t.Errorf("authorized message %s missing from project history: %+v", id, history)
		}
	}
	for _, id := range excluded {
		if got[id] {
			t.Errorf("unverified message %s leaked into project history: %+v", id, history)
		}
	}
}

func TestProjectRouteHistoryUsesProvenCompletedMemberResults(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	aggregation := resultAggregationFixture{store: f.store, ownerID: f.owner.UserID,
		conversation: conversation.Conversation{ID: f.conversationID, OwnerID: f.owner.UserID, ProjectID: f.projectID}}
	first := aggregation.createTask(t, f.owner.UserID, "first shared question")
	second := aggregation.createTask(t, f.owner.UserID, "second shared question")
	if !aggregation.complete(t, &first, "first shared answer") || !aggregation.complete(t, &second, "second shared answer") {
		t.Fatal("completed results were not queued")
	}
	var batchID string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches
WHERE conversation_id=$1 AND state='published'`, f.conversationID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}

	stream := aggregation.createTask(t, f.owner.UserID, "shared streamed question")
	streamID := projectHistoryInsertMessage(t, f, f.owner.UserID, stream.ID, "", "shared streamed answer", conversation.RoleAssistant)
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status='completed',result='shared streamed answer',event_seq=1 WHERE id=$1`, stream.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,1,'task.result','{}'::jsonb,0,sha256(convert_to('shared streamed answer','UTF8')))`, stream.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE conversation_messages SET output_status='completed',output_task_revision=0,output_result_event_sequence=1 WHERE id=$1`, streamID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pending := aggregation.createTask(t, f.owner.UserID, "other member pending task")
	unverified := aggregation.createTask(t, f.owner.UserID, "completed without result proof")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='completed',result='unverified' WHERE id=$1`, unverified.ID); err != nil {
		t.Fatal(err)
	}
	unverifiedID := projectHistoryInsertMessage(t, f, f.owner.UserID, unverified.ID, "", "unverified answer", conversation.RoleAssistant)
	importID := projectHistoryInsertMessage(t, f, f.owner.UserID, "", "history_import", "private imported text", conversation.RoleUser)
	unknownID := projectHistoryInsertMessage(t, f, f.owner.UserID, stream.ID, "", "delivery unknown", conversation.RoleUser)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='unknown' WHERE id=$1`, unknownID); err != nil {
		t.Fatal(err)
	}
	memberTask := aggregation.createTask(t, f.member.UserID, "own pending task")
	claim := projectHistoryClaim(t, f, "project-history-001")
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	projectHistoryAssert(t, snapshot.Context.History,
		[]string{first.UserMessageID, second.UserMessageID, batchID, stream.UserMessageID, streamID, memberTask.UserMessageID},
		[]string{pending.UserMessageID, unverified.UserMessageID, unverifiedID, importID, unknownID})
	if len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != memberTask.ID {
		t.Fatalf("other members' tasks became control candidates: %+v", snapshot.Context.Candidates)
	}

	snapshot.Context.ModelID = "p1234567"
	fence := app.ConversationRouteModelFence{Claim: claim, Context: snapshot.Context}
	checkFence := func() error {
		return f.store.WithTx(ctx, func(tx pgx.Tx) error {
			return checkConversationRouteModelFence(ctx, tx, fence, "p1234567")
		})
	}
	if err := checkFence(); err != nil {
		t.Fatalf("model rejected verified shared history: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET content='tampered batch' WHERE id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	if err := checkFence(); err == nil {
		t.Fatal("model accepted stale, tampered batch evidence")
	}
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	projectHistoryAssert(t, snapshot.Context.History, []string{stream.UserMessageID, streamID}, []string{first.UserMessageID, second.UserMessageID, batchID})
	snapshot.Context.ModelID = "p1234567"
	fence.Context = snapshot.Context
	if err := checkFence(); err != nil {
		t.Fatalf("model rejected remaining proven stream: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET content='tampered stream' WHERE id=$1`, streamID); err != nil {
		t.Fatal(err)
	}
	if err := checkFence(); err == nil {
		t.Fatal("model accepted stale, tampered stream evidence")
	}
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	projectHistoryAssert(t, snapshot.Context.History, nil, []string{stream.UserMessageID, streamID})
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: pending.ID, ExpectedRevision: 0,
	}); err != nil {
		t.Fatal(err)
	}
	var state, taskStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,t.status FROM conversation_message_routes r JOIN tasks t ON t.id=$2 WHERE r.message_id=$1`,
		claim.Message.ID, pending.ID).Scan(&state, &taskStatus); err != nil || state != "waiting_user" || taskStatus != "queued" {
		t.Fatalf("another member's task was controlled: route=%s task=%s err=%v", state, taskStatus, err)
	}

	claim = projectHistoryClaim(t, f, "project-history-002")
	value, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveProjectMember(ctx, f.owner.UserID, f.projectID, f.member.UserID, value.Version, "project-history-revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.LoadConversationRouteSnapshot(ctx, claim); err == nil {
		t.Fatal("revoked project member could still read the route snapshot")
	}
}

func TestPersonalRouteHistoryRemainsActorIsolated(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	other := createPreRetirementTestUser(t, f.store, 2, 2000, 4096)
	foreignID := mustID(t, domain.NewMessageID)
	err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: foreignID, OwnerID: f.ownerID, ActorID: other.ID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "foreign synthetic message"})
	})
	if err == nil {
		t.Fatal("personal conversation accepted a different actor")
	}
	claim := f.receive(t, "personal-history-001", "own question", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	projectHistoryAssert(t, snapshot.Context.History, nil, []string{foreignID})
}

func TestProjectTaskHistoryFiltersUnappliedAndUnprovenMessages(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	aggregation := resultAggregationFixture{store: f.store, ownerID: f.owner.UserID,
		conversation: conversation.Conversation{ID: f.conversationID, OwnerID: f.owner.UserID, ProjectID: f.projectID}}
	completed := aggregation.createTask(t, f.owner.UserID, "shared completed question")
	if !aggregation.complete(t, &completed, "shared completed answer") {
		t.Fatal("completed result was not queued")
	}
	var batchID string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id FROM conversation_result_batches
WHERE conversation_id=$1 AND state='published'`, f.conversationID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	importID := projectHistoryInsertMessage(t, f, f.owner.UserID, completed.ID, "history_import", "private imported history", conversation.RoleUser)
	ownPending := projectHistoryInsertMessage(t, f, f.member.UserID, "", "", "own pending delivery", conversation.RoleUser)
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending' WHERE id=$1`, ownPending); err != nil {
		t.Fatal(err)
	}
	claim := projectHistoryClaim(t, f, "project-history-filter-001")
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, claim.Message.ID); err != nil {
		t.Fatal(err)
	}
	input := []task.ConversationContextMessage{{ID: completed.UserMessageID}, {ID: batchID}, {ID: importID},
		{ID: ownPending}, {ID: claim.Message.ID}}
	filter := func() []task.ConversationContextMessage {
		t.Helper()
		var result []task.ConversationContextMessage
		err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			result, err = filterConversationRouteTaskHistory(ctx, tx, task.Task{OwnerID: f.owner.UserID, ProjectID: f.projectID,
				ConversationID: f.conversationID, InitiatorID: f.member.UserID}, input)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertFilter := func(result []task.ConversationContextMessage, present, absent []string) {
		t.Helper()
		got := make(map[string]bool, len(result))
		for _, item := range result {
			got[item.ID] = true
		}
		for _, id := range present {
			if !got[id] {
				t.Errorf("verified message %s excluded from task history", id)
			}
		}
		for _, id := range absent {
			if got[id] {
				t.Errorf("unapplied or unproven message %s entered task history", id)
			}
		}
	}
	assertFilter(filter(), []string{completed.UserMessageID, batchID}, []string{importID, ownPending, claim.Message.ID})
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET output_status='interrupted',output_task_revision=0 WHERE id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	assertFilter(filter(), nil, []string{completed.UserMessageID, batchID, importID, ownPending, claim.Message.ID})
}

func TestProjectRouteDoesNotTargetUnrecoverableTask(t *testing.T) {
	for _, status := range []string{"pausing", "waiting_user"} {
		t.Run(status, func(t *testing.T) {
			f := newProjectAttachmentFixture(t)
			ctx := context.Background()
			if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, f.member.UserID); err != nil {
				t.Fatal(err)
			}
			aggregation := resultAggregationFixture{store: f.store, ownerID: f.owner.UserID,
				conversation: conversation.Conversation{ID: f.conversationID, OwnerID: f.owner.UserID, ProjectID: f.projectID}}
			old := aggregation.createTask(t, f.member.UserID, "unrecoverable request")
			other := aggregation.createTask(t, f.member.UserID, "another active request")
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status=$2,waiting_reason='project_recovery_unavailable' WHERE id=$1`, old.ID, status); err != nil {
				t.Fatal(err)
			}
			claim := projectHistoryClaimWithReference(t, f, "project-unrecoverable-001", old.ID, "停止任务")
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			projectHistoryAssert(t, snapshot.Context.History, []string{old.UserMessageID}, nil)
			if !snapshot.Context.ReferenceUnavailable || snapshot.Context.ReferenceTaskID != old.ID ||
				len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != other.ID {
				t.Fatalf("unrecoverable Task remained a route target: %+v", snapshot.Context.Candidates)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				return checkConversationRouteModelFence(ctx, tx, app.ConversationRouteModelFence{Claim: claim, Context: snapshot.Context}, "p1234567")
			}); err != nil {
				t.Fatalf("model fence rejected the verified unavailable reference: %v", err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET waiting_reason='approval_required' WHERE id=$1`, old.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				return checkConversationRouteModelFence(ctx, tx, app.ConversationRouteModelFence{Claim: claim, Context: snapshot.Context}, "p1234567")
			}); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("model fence accepted a changed reference state: %v", err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET waiting_reason='project_recovery_unavailable' WHERE id=$1`, old.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
				Action: app.ConversationRouteUpdate, TaskID: old.ID, ExpectedRevision: 0,
			}); err != nil {
				t.Fatal(err)
			}
			var routeState, oldStatus, reason, reply string
			if err := f.store.pool.QueryRow(ctx, `SELECT r.state,t.status,t.waiting_reason,
 (SELECT content FROM conversation_messages WHERE conversation_id=t.conversation_id AND role='assistant' ORDER BY sequence DESC LIMIT 1)
FROM conversation_message_routes r JOIN tasks t ON t.id=$2 WHERE r.message_id=$1`, claim.Message.ID, old.ID).
				Scan(&routeState, &oldStatus, &reason, &reply); err != nil || routeState != "waiting_user" || oldStatus != status ||
				reason != "project_recovery_unavailable" || reply != app.UnavailableRouteReferenceMessage {
				t.Fatalf("unrecoverable Task was changed: route=%s task=%s reason=%s reply=%q err=%v", routeState, oldStatus, reason, reply, err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='completed' WHERE id=$1`, other.ID); err != nil {
				t.Fatal(err)
			}
			claim = projectHistoryClaimWithReference(t, f, "project-unrecoverable-002", old.ID, "停止任务")
			snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil || !snapshot.Context.ReferenceUnavailable {
				t.Fatalf("unavailable Task reference was not detected: %+v err=%v", snapshot.Context, err)
			}
			if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var controlTask string
			if err := f.store.pool.QueryRow(ctx, `SELECT r.state,COALESCE(m.task_id,'') FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).Scan(&routeState, &controlTask); err != nil ||
				routeState != "waiting_user" || controlTask != "" {
				t.Fatalf("standalone stop was converted into a new Task: route=%s task=%s err=%v", routeState, controlTask, err)
			}
			claim = projectHistoryClaimWithReference(t, f, "project-unrecoverable-003", old.ID, "重新整理今年的财报")
			snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil || !snapshot.Context.ReferenceUnavailable || len(snapshot.Context.Candidates) != 0 {
				t.Fatalf("resend still targeted the old Task: candidates=%+v err=%v", snapshot.Context.Candidates, err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var replacement string
			if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&replacement); err != nil ||
				replacement == "" || replacement == old.ID {
				t.Fatalf("resend did not create a new Task: task=%s err=%v", replacement, err)
			}
			foreign := aggregation.createTask(t, f.owner.UserID, "another member's unrecoverable Task")
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='waiting_user',waiting_reason='project_recovery_unavailable' WHERE id=$1`, foreign.ID); err != nil {
				t.Fatal(err)
			}
			claim = projectHistoryClaimWithReference(t, f, "project-unrecoverable-004", foreign.ID, "查看任务状态")
			snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil || snapshot.Context.ReferenceUnavailable {
				t.Fatalf("other member's Task recovery state was exposed: %+v err=%v", snapshot.Context, err)
			}
		})
	}
}
