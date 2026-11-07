package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresConversationConcurrentTasksAndRevisionReceipts(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "concurrent-first", "first task"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := make([]app.TaskSubmission, 2)
	for i, key := range []string{"concurrent-new-a", "concurrent-new-b"} {
		inputs[i] = newTestTaskSubmission(t, owner.ID, "", key, key)
		inputs[i].Task.ConversationID = first.Conversation.ID
		inputs[i].Message.ConversationID = first.Conversation.ID
	}
	var results [2]app.TaskSubmissionResult
	var errs [2]error
	var wg sync.WaitGroup
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.SubmitTask(ctx, inputs[i])
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].Task.ID == results[1].Task.ID || results[0].Task.ID == first.Task.ID || results[1].Task.ID == first.Task.ID {
		t.Fatalf("parallel new tasks = %s, %s; errors = %v, %v", results[0].Task.ID, results[1].Task.ID, errs[0], errs[1])
	}
	page, err := store.ListConversationActiveTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.PageRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].EventSequence != 1 || page.NextCursor == "" {
		t.Fatalf("first active task page = %+v, err=%v", page, err)
	}
	seen := map[string]bool{page.Items[0].ID: true}
	for page.NextCursor != "" {
		page, err = store.ListConversationActiveTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.PageRequest{Limit: 1, Cursor: page.NextCursor})
		if err != nil || len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatalf("active task pagination = %+v, err=%v", page, err)
		}
		seen[page.Items[0].ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("active task count = %d, want 3", len(seen))
	}
	targets := []string{first.Task.ID, results[0].Task.ID}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	receipts := make([]app.ConversationMessageReceipt, 2)
	for i, key := range []string{"concurrent-revision-a", "concurrent-revision-b"} {
		receipts[i] = app.ConversationMessageReceipt{
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
				ConversationID: first.Conversation.ID, Role: conversation.RoleUser, Content: key},
			ServiceInstanceID: instanceID, ClientID: key, RequestHash: hashBytes(key),
			Input:         app.ConversationRoutingInput{ReferenceTaskID: targets[i]},
			Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		}
	}
	var messages [2]conversation.Message
	for i := range receipts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			messages[i], _, errs[i] = store.AcceptConversationMessage(ctx, receipts[i])
		}(i)
	}
	wg.Wait()
	for i := range receipts {
		if errs[i] != nil || messages[i].TaskID != "" || messages[i].DeliveryStatus != conversation.DeliveryPending {
			t.Fatalf("revision receipt target %s = %+v, err=%v", targets[i], messages[i], errs[i])
		}
	}
	for range receipts {
		claim, found, err := store.ClaimConversationRoute(ctx, "concurrent-revision-router", time.Minute)
		if err != nil || !found {
			t.Fatalf("claim revision receipt: found=%t err=%v", found, err)
		}
		snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		var candidate *app.ConversationRouteCandidate
		for index := range snapshot.Context.Candidates {
			if snapshot.Context.Candidates[index].TaskID == claim.Input.ReferenceTaskID {
				candidate = &snapshot.Context.Candidates[index]
			}
		}
		if candidate == nil {
			t.Fatalf("revision target missing from candidates: %+v", snapshot.Context.Candidates)
		}
		if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
			TaskID: candidate.TaskID, ExpectedRevision: candidate.Revision}); err != nil {
			t.Fatal(err)
		}
		var state, target string
		if err := store.pool.QueryRow(ctx, `SELECT r.state,i.task_id FROM conversation_message_routes r
JOIN task_revision_intents i ON i.message_id=r.message_id WHERE r.message_id=$1`, claim.Message.ID).Scan(&state, &target); err != nil ||
			state != "waiting_adjustment" || target != claim.Input.ReferenceTaskID {
			t.Fatalf("revision targeted wrong Task: state=%s target=%s err=%v", state, target, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',finished_at=clock_timestamp() WHERE id=$1`, targets[0]); err != nil {
		t.Fatal(err)
	}
	replay, replayed, err := store.AcceptConversationMessage(ctx, receipts[0])
	if err != nil || !replayed || replay.ID != messages[0].ID {
		t.Fatalf("terminal task idempotent replay = %+v, replay=%t err=%v", replay, replayed, err)
	}
	changed := receipts[0]
	changed.RequestHash = hashBytes("different payload")
	if _, _, err := store.AcceptConversationMessage(ctx, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed request under the same key: %v", err)
	}
	late := receivePendingRouteMessageForTest(t, ctx, store, owner.ID, first.Conversation.ID, targets[0], "late-revision", "late revision")
	lateClaim, found, err := store.ClaimConversationRoute(ctx, "late-revision-router", time.Minute)
	if err != nil || !found || lateClaim.Message.ID != late.ID {
		t.Fatalf("claim late receipt: found=%t err=%v", found, err)
	}
	lateSnapshot, err := store.LoadConversationRouteSnapshot(ctx, lateClaim)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyConversationRoute(ctx, lateClaim, lateSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: targets[0], ExpectedRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var lateState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM conversation_message_routes WHERE message_id=$1`, late.ID).Scan(&lateState); err != nil || lateState != "waiting_user" {
		t.Fatalf("late revision did not clarify terminal target: state=%s err=%v", lateState, err)
	}
	other := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	if _, err := store.ListConversationActiveTasks(ctx, other.ID, "", first.Conversation.ID, nil, app.PageRequest{Limit: 10}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-user active task list: %v", err)
	}
	foreign := receipts[1]
	foreign.Message.ID = mustID(t, domain.NewMessageID)
	foreign.Message.OwnerID, foreign.Message.ActorID = other.ID, other.ID
	foreign.ClientID, foreign.RequestHash = "foreign-revision", hashBytes("foreign-revision")
	foreign.Authorization.TokenID = testSessionTokenID(other.ID)
	if _, _, err := store.AcceptConversationMessage(ctx, foreign); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-user receipt: %v", err)
	}
}

func TestPostgresTaskServiceReplaysTerminalConversationMessage(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	principal := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: testSessionTokenID(owner.ID),
	}
	authorization := projectConversationTestAuthorization{}
	conversationService, err := app.NewConversationService(store, authorization)
	if err != nil {
		t.Fatal(err)
	}
	conversationValue, _, err := conversationService.Create(ctx, principal, app.CreateConversationCommand{
		Title: "Terminal replay", IdempotencyKey: "terminal-replay-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, authorization, testConfiguredModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command := app.AcceptConversationMessageCommand{
		ServiceInstanceID: instanceID, ClientMessageID: "terminal-replay-message", IdempotencyKey: "terminal-replay-message",
		ModelID: "p1234567", Content: "idempotent terminal replay",
	}
	first, replayed, err := tasks.AcceptConversationMessage(ctx, principal, conversationValue.ID, command)
	if err != nil || replayed || first.TaskID != "" {
		t.Fatalf("initial message receipt = %+v replay=%t err=%v", first, replayed, err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "terminal-replay-router", time.Minute)
	if err != nil || !found || claim.Message.ID != first.ID {
		t.Fatalf("claim terminal replay receipt: found=%t err=%v", found, err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, first.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed',finished_at=clock_timestamp() WHERE id=$1`, taskID); err != nil {
		t.Fatalf("complete submitted task: %v", err)
	}
	replay, replayed, err := tasks.AcceptConversationMessage(ctx, principal, conversationValue.ID, command)
	if err != nil || !replayed || replay.TaskID != taskID || replay.ID != first.ID {
		t.Fatalf("terminal receipt replay = %+v replay=%t err=%v", replay, replayed, err)
	}
	changed := command
	changed.Content = "different request with the same key"
	if _, _, err := tasks.AcceptConversationMessage(ctx, principal, conversationValue.ID, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed terminal request with same key: %v", err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM tasks WHERE conversation_id=$1`:                 1,
		`SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`: 1,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, conversationValue.ID).Scan(&count); err != nil {
			t.Fatalf("count persisted replay rows with %q: %v", query, err)
		}
		if count != want {
			t.Fatalf("persisted replay row count for %q = %d, want %d", query, count, want)
		}
	}
	var idempotencyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_routes WHERE actor_id=$1 AND client_message_id='terminal-replay-message'`, owner.ID).Scan(&idempotencyCount); err != nil || idempotencyCount != 1 {
		t.Fatalf("terminal replay idempotency row count = %d, err=%v; want one", idempotencyCount, err)
	}
}

func TestPostgresConcurrentNewConversationMessageReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, pool := newTemporaryStore(t)
	monitorConfig := pool.Config().Copy()
	monitorConfig.MaxConns = 8
	monitorPool, err := pgxpool.NewWithConfig(ctx, monitorConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL lock monitor pool: %v", err)
	}
	defer monitorPool.Close()

	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	initial, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "new-message-replay-base", "initial task"))
	if err != nil {
		t.Fatalf("create conversation fixture: %v", err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	first := app.ConversationMessageReceipt{Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
		ConversationID: initial.Conversation.ID, Role: conversation.RoleUser, Content: "same new instruction"},
		ServiceInstanceID: instanceID, ClientID: "new-message-concurrent-replay", RequestHash: hashBytes("same new instruction"),
		Input:         app.ConversationRoutingInput{ModelID: "p1234567", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}}
	second := first
	second.Message.ID = mustID(t, domain.NewMessageID)
	if first.Message.ID == second.Message.ID || len(first.RequestHash) != 32 {
		t.Fatal("concurrent receipt fixture must use distinct generated IDs and the same request hash")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, owner.ID); err != nil {
		t.Fatal(err)
	}

	const advisoryLockKey int64 = 824726211
	if _, err := pool.Exec(ctx, `CREATE FUNCTION wait_for_new_conversation_replay_test_release() RETURNS trigger AS $$
BEGIN
  IF NEW.client_message_id = 'new-message-concurrent-replay' THEN
    PERFORM pg_advisory_xact_lock(824726211);
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create new-message replay barrier: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER wait_for_new_conversation_replay_test_release
BEFORE INSERT ON conversation_message_routes FOR EACH ROW EXECUTE FUNCTION wait_for_new_conversation_replay_test_release()`); err != nil {
		t.Fatalf("install new-message replay barrier: %v", err)
	}
	advisoryConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire PostgreSQL advisory lock connection: %v", err)
	}
	defer func() {
		_, _ = advisoryConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		advisoryConn.Release()
	}()
	if _, err := advisoryConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		t.Fatalf("hold new-message replay lock: %v", err)
	}

	type receiptResult struct {
		value    conversation.Message
		replayed bool
		err      error
	}
	firstResult, secondResult := make(chan receiptResult, 1), make(chan receiptResult, 1)
	go func() {
		value, replayed, err := store.AcceptConversationMessage(ctx, first)
		firstResult <- receiptResult{value: value, replayed: replayed, err: err}
	}()
	waitForPostgresLockWait(t, ctx, monitorPool, "%INSERT INTO conversation_message_routes%", 1)
	go func() {
		value, replayed, err := store.AcceptConversationMessage(ctx, second)
		secondResult <- receiptResult{value: value, replayed: replayed, err: err}
	}()
	waitForPostgresLockWait(t, ctx, monitorPool, "%FROM conversations%FOR UPDATE%", 1)

	var unlocked bool
	if err := advisoryConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release new-message replay lock: unlocked=%t err=%v", unlocked, err)
	}
	var created, replayed receiptResult
	select {
	case created = <-firstResult:
	case <-ctx.Done():
		t.Fatalf("first new-message submission did not finish: %v", ctx.Err())
	}
	select {
	case replayed = <-secondResult:
	case <-ctx.Done():
		t.Fatalf("concurrent new-message replay did not finish: %v", ctx.Err())
	}
	if created.err != nil || created.replayed || created.value.ID != first.Message.ID || created.value.TaskID != "" {
		t.Fatalf("first new-message receipt = message %s replay=%t err=%v", created.value.ID, created.replayed, created.err)
	}
	if replayed.err != nil || !replayed.replayed || replayed.value.ID != first.Message.ID || replayed.value.ID == second.Message.ID {
		t.Fatalf("concurrent new-message replay = message %s replay=%t err=%v", replayed.value.ID, replayed.replayed, replayed.err)
	}
	var taskCount, messageCount, idempotencyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id IN ($1,$2)`,
		first.Message.ID, second.Message.ID).Scan(&messageCount); err != nil || messageCount != 1 {
		t.Fatalf("same-key new message count=%d err=%v, want one", messageCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_routes WHERE actor_id=$1 AND client_message_id=$2`,
		owner.ID, first.ClientID).Scan(&idempotencyCount); err != nil || idempotencyCount != 1 {
		t.Fatalf("same-key new idempotency record count=%d err=%v, want one", idempotencyCount, err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "new-message-replay-router", time.Minute)
	if err != nil || !found || claim.Message.ID != first.Message.ID {
		t.Fatalf("claim idempotent message: found=%t err=%v", found, err)
	}
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, initial.Conversation.ID).Scan(&taskCount); err != nil || taskCount != 2 {
		t.Fatalf("same-key receipt created the wrong number of Tasks: count=%d err=%v", taskCount, err)
	}
}

func TestPostgresProjectConversationConcurrentNewTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, pool := newTemporaryStore(t)
	owner := createPreRetirementTestUser(t, store, 4, 20000, 40000)
	member := createPreRetirementTestUser(t, store, 4, 20000, 40000)
	outsider := createTestUser(t, store, 4, 20000, 40000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Parallel chat", "Concurrent project messages", "project-conversation-parallel")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue,
		project.RoleMember, projectValue.Version, "project-conversation-parallel-invite", "project-conversation-parallel-accept"); err != nil {
		t.Fatalf("accept project member: %v", err)
	}
	projects, err := app.NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	tasks, err := app.NewTaskService(store, authorization, authorization, projects)
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewProjectConversationService(store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	principal := func(userID string) identity.Principal {
		return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
	}
	shared, replayed, err := conversations.Create(ctx, principal(owner.ID), projectValue.ID, app.CreateProjectConversationCommand{
		Title: "Concurrent", IdempotencyKey: "project-conversation-parallel-create",
	})
	if err != nil || replayed {
		t.Fatalf("create project conversation: value=%+v replayed=%t err=%v", shared, replayed, err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	type receiptResult struct {
		actorID  string
		value    conversation.Message
		replayed bool
		err      error
	}
	requests := []struct {
		actorID string
		key     string
		content string
	}{{owner.ID, "project-parallel-owner-message", "from project owner"},
		{member.ID, "project-parallel-member-message", "from project member"}}
	expectedContent := make(map[string]string, len(requests))
	for _, request := range requests {
		expectedContent[request.actorID] = request.content
	}
	start := make(chan struct{})
	results := make(chan receiptResult, len(requests))
	var workers sync.WaitGroup
	for _, request := range requests {
		request := request
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			value, replayed, err := conversations.AcceptMessage(ctx, principal(request.actorID), projectValue.ID, shared.ID,
				app.AcceptConversationMessageCommand{ServiceInstanceID: instanceID, ClientMessageID: request.key,
					IdempotencyKey: request.key, ModelID: "p1234567", Content: request.content})
			results <- receiptResult{actorID: request.actorID, value: value, replayed: replayed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	seenMessages := make(map[string]string, len(requests))
	seenActors := make(map[string]bool, len(requests))
	for result := range results {
		if result.err != nil || result.replayed || result.value.OwnerID != owner.ID || result.value.ActorID != result.actorID ||
			result.value.TaskID != "" || result.value.ConversationID != shared.ID || result.value.Content != expectedContent[result.actorID] {
			t.Fatalf("parallel project receipt actor=%s message=%+v replay=%t err=%v", result.actorID,
				result.value, result.replayed, result.err)
		}
		if _, exists := seenMessages[result.value.ID]; exists || seenActors[result.actorID] {
			t.Fatalf("project message or actor was duplicated: message=%s actor=%s", result.value.ID, result.actorID)
		}
		seenMessages[result.value.ID], seenActors[result.actorID] = result.actorID, true
	}
	if len(seenMessages) != 2 || len(seenActors) != 2 {
		t.Fatalf("parallel project receipts returned messages=%d actors=%d, want two each", len(seenMessages), len(seenActors))
	}
	seenTasks := make(map[string]bool, len(requests))
	for range requests {
		claim, found, err := store.ClaimConversationRoute(ctx, "parallel-project-router", time.Minute)
		if err != nil || !found || seenMessages[claim.Message.ID] != claim.Message.ActorID {
			t.Fatalf("claim parallel project receipt: id=%s actor=%s found=%t err=%v", claim.Message.ID, claim.Message.ActorID, found, err)
		}
		snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatal(err)
		}
		var taskID string
		if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		details, err := store.GetTask(ctx, taskID, claim.Message.ActorID)
		if err != nil || seenTasks[taskID] || details.Task.ProjectID != projectValue.ID ||
			details.Task.OwnerID != owner.ID || details.Task.InitiatorID != claim.Message.ActorID {
			t.Fatalf("routed project task for %s = %+v err=%v", claim.Message.ActorID, details.Task, err)
		}
		seenTasks[taskID] = true
	}
	if len(seenTasks) != 2 {
		t.Fatalf("parallel project messages created %d tasks, want two", len(seenTasks))
	}
	for _, actorID := range []string{owner.ID, member.ID} {
		page, err := conversations.ListTasks(ctx, principal(actorID), projectValue.ID, shared.ID, app.PageRequest{Limit: 10})
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("active project tasks for %s = %d err=%v, want two", actorID, len(page.Items), err)
		}
	}
	if _, err := conversations.ListTasks(ctx, principal(outsider.ID), projectValue.ID, shared.ID, app.PageRequest{Limit: 10}); err == nil {
		t.Fatal("outsider read active project tasks")
	}
}

func TestPostgresConversationSnapshotExcludesOtherActiveTaskAfterItFinishes(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	history, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "history-task", "remember cedar"))
	if err != nil {
		t.Fatal(err)
	}
	seedCompletedAssistantContextMessage(t, store, history, "cedar remembered")
	create := func(key, objective string) app.TaskSubmissionResult {
		t.Helper()
		input := newTestTaskSubmission(t, owner.ID, "", key, objective)
		input.Task.ConversationID, input.Message.ConversationID = history.Conversation.ID, history.Conversation.ID
		result, err := store.SubmitTask(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	other := create("inflight-task", "unrelated active instruction")
	current := create("current-task", "current instruction")
	manifest := readTaskConversationContextManifest(t, ctx, store, current.Task.ID)
	if manifest.MessageCount != 2 || len(manifest.MessageIDs) != 2 {
		t.Fatalf("snapshot should contain only completed history: %+v", manifest)
	}
	seedCompletedAssistantContextMessage(t, store, other, "unrelated task finished")
	claimed, found, err := store.ClaimNextTask(ctx, "parallel-history-worker", time.Minute)
	if err != nil || !found || claimed.ID != current.Task.ID || claimed.ConversationContext == nil || len(claimed.ConversationContext.Messages) != 2 {
		t.Fatalf("claim after sibling completed: task=%s found=%t context=%+v err=%v", claimed.ID, found, claimed.ConversationContext, err)
	}
	if claimed.ConversationContext.Messages[0].Content != "remember cedar" || claimed.ConversationContext.Messages[1].Content != "cedar remembered" {
		t.Fatalf("another active task leaked into context: %+v", claimed.ConversationContext.Messages)
	}
}

func TestPostgresMissingSnapshotSafelyStopsTaskWithoutBlockingQueue(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	old, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "old-snapshot-task", "old task content"))
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, old.Conversation.ID, old.Task.ID, "pending old message", nil)
	if _, err := pool.Exec(ctx, `UPDATE task_events
SET payload=jsonb_set(payload,'{conversation_context}',(payload->'conversation_context')-'message_ids')
WHERE task_id=$1 AND sequence=1`, old.Task.ID); err != nil {
		t.Fatal(err)
	}
	next, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "snapshot-next-task", "next task content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ClaimNextTask(ctx, "snapshot-recovery-worker", time.Minute); err != nil || found {
		t.Fatalf("old task should enter safe stop: found=%t err=%v", found, err)
	}
	details, err := store.GetTask(ctx, old.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCancelling || !strings.Contains(details.Task.Result, "无法恢复") || details.StopRequest == nil {
		t.Fatalf("old task did not retain a visible reason and safe-stop request: %+v err=%v", details, err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, old.Conversation.ID, app.PageRequest{Limit: 10})
	if err != nil || len(messages.Items) != 2 || messages.Items[0].Content != "pending old message" ||
		messages.Items[0].DeliveryStatus != conversation.DeliveryUnknown || messages.Items[1].Content != "old task content" {
		t.Fatalf("old user message was lost: %+v err=%v", messages, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "snapshot-recovery-worker", time.Minute)
	if err != nil || !found || claimed.ID != next.Task.ID {
		t.Fatalf("unrelated task remained blocked: id=%s found=%t err=%v", claimed.ID, found, err)
	}
	stop, found, err := store.ClaimNextTaskStop(ctx, "snapshot-stop-worker", time.Minute)
	if err != nil || !found || stop.Task.ID != old.Task.ID {
		t.Fatalf("claim safe stop: task=%s found=%t err=%v", stop.Task.ID, found, err)
	}
	stopped, err := store.CompleteTaskStop(ctx, old.Task.ID, stop.Request.ID, "snapshot-stop-worker", stop.Request.LeaseVersion)
	if err != nil || stopped.Status != task.StatusCancelled || !strings.Contains(stopped.Result, "无法恢复") {
		t.Fatalf("complete safe stop: status=%s result=%q err=%v", stopped.Status, stopped.Result, err)
	}
}

func TestPostgresConcurrentConversationMigrationPreservesExistingTaskAndGuardsRollback(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 136); err != nil {
		t.Fatal(err)
	}
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	old, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "migration-old-task", "preserve old work"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 137); err != nil {
		t.Fatalf("upgrade existing conversation: %v", err)
	}
	input := newTestTaskSubmission(t, owner.ID, "", "migration-new-task", "new parallel work")
	input.Task.ConversationID, input.Message.ConversationID = old.Conversation.ID, old.Conversation.ID
	newTask, err := store.SubmitTask(ctx, input)
	if err != nil || newTask.Task.ID == old.Task.ID {
		t.Fatalf("new task after migration = %s err=%v", newTask.Task.ID, err)
	}
	if _, err := provider.DownTo(ctx, 136); err == nil {
		t.Fatal("rollback accepted two active tasks in the same conversation")
	}
	var active, messages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND status='queued'`, old.Conversation.ID).Scan(&active); err != nil || active != 2 {
		t.Fatalf("failed rollback changed tasks: active=%d err=%v", active, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`, old.Conversation.ID).Scan(&messages); err != nil || messages != 2 {
		t.Fatalf("failed rollback changed messages: count=%d err=%v", messages, err)
	}
}
