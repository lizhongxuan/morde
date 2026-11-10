package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskK2Projection(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	createTask := func(key string) app.TaskSubmissionResult {
		t.Helper()
		submission := newTestTaskSubmission(t, owner.ID, "", key, "K2 projection "+key)
		submission.Task.ConversationID = conversationValue.ID
		submission.Message.ConversationID = conversationValue.ID
		created, err := store.SubmitTask(ctx, submission)
		if err != nil {
			t.Fatalf("create Task %q: %v", key, err)
		}
		return created
	}
	readProjection := func(taskID string, revision int64) ([]app.Dependency, []app.ChildJob, *app.ResultReview) {
		t.Helper()
		var dependencies []app.Dependency
		var childJobs []app.ChildJob
		var review *app.ResultReview
		if err := store.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
			var err error
			dependencies, childJobs, review, err = loadTaskK2Projection(ctx, tx, taskID, revision)
			return err
		}); err != nil {
			t.Fatalf("load K2 projection: %v", err)
		}
		return dependencies, childJobs, review
	}

	completed := createTask("k2-dependency-complete")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',current_revision=0,desired_revision=0,result='completed dependency result' WHERE id=$1`, completed.Task.ID); err != nil {
		t.Fatal(err)
	}
	stale := createTask("k2-dependency-stale")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',current_revision=1,desired_revision=1,result='stale dependency result' WHERE id=$1`, stale.Task.ID); err != nil {
		t.Fatal(err)
	}
	failed := createTask("k2-dependency-failed")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='failed',current_revision=0,desired_revision=0,result='failed dependency result' WHERE id=$1`, failed.Task.ID); err != nil {
		t.Fatal(err)
	}
	emptyResult := createTask("k2-dependency-empty-result")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',current_revision=0,desired_revision=0,result='' WHERE id=$1`, emptyResult.Task.ID); err != nil {
		t.Fatal(err)
	}
	parent := createTask("k2-projection-parent")
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=2,desired_revision=2 WHERE id=$1`, parent.Task.ID); err != nil {
		t.Fatal(err)
	}

	dependencies, childJobs, review := readProjection(parent.Task.ID, 2)
	if dependencies == nil || len(dependencies) != 0 || childJobs == nil || len(childJobs) != 0 || review != nil {
		t.Fatalf("empty K2 projection = dependencies %#v, child jobs %#v, review %#v", dependencies, childJobs, review)
	}
	for _, dependency := range []struct {
		taskID           string
		requiredRevision int64
	}{{completed.Task.ID, 0}, {stale.Task.ID, 0}, {failed.Task.ID, 0}, {emptyResult.Task.ID, 0}} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_dependencies(task_id,owner_id,conversation_id,dependency_task_id,required_revision)
VALUES ($1,$2,$3,$4,$5)`, parent.Task.ID, owner.ID, conversationValue.ID, dependency.taskID, dependency.requiredRevision); err != nil {
			t.Fatalf("insert dependency %s: %v", dependency.taskID, err)
		}
	}

	rootSessionID := "root-k2-session"
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,task_revision)
VALUES ($1,$2,$3,2)`, parent.Task.ID, owner.ID, rootSessionID); err != nil {
		t.Fatalf("insert root Agent Session: %v", err)
	}
	childValues := []struct {
		sessionID string
		stepID    string
		sequence  int64
		title     string
		status    task.StepStatus
		revision  int64
	}{
		{sessionID: "child-k2-running", stepID: mustID(t, domain.NewStepID), sequence: 1,
			title: "Current revision running", status: task.StepRunning, revision: 2},
		{sessionID: "child-k2-failed", stepID: mustID(t, domain.NewStepID), sequence: 2,
			title: "Current revision failed", status: task.StepFailed, revision: 2},
		{sessionID: "child-k2-stale", stepID: mustID(t, domain.NewStepID), sequence: 3,
			title: "Older revision", status: task.StepSucceeded, revision: 1},
	}
	requestHash := sha256.Sum256([]byte("k2-child-request"))
	for _, child := range childValues {
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title)
VALUES ($1,$2,$3,'subagent',$4,$5)`, child.stepID, parent.Task.ID, child.sequence, child.status, child.title); err != nil {
			t.Fatalf("insert TaskStep %s: %v", child.stepID, err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,parent_session_id,step_id,spawn_key,request_hash,task_revision)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, parent.Task.ID, owner.ID, child.sessionID, rootSessionID,
			child.stepID, child.sessionID, requestHash[:], child.revision); err != nil {
			t.Fatalf("insert child Agent Session %s: %v", child.sessionID, err)
		}
	}

	var serviceInstanceID string
	if err := store.pool.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&serviceInstanceID); err != nil {
		t.Fatal(err)
	}
	batchID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	resultContent := "raw child result"
	contentHash := sha256.Sum256([]byte(resultContent))
	insertBatchAndSource := func(messageID string, revision, eventSequence int64, batchState, sourceState string) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_result_batches
(message_id,service_instance_id,owner_id,actor_id,conversation_id,state,created_at,deadline)
VALUES ($1,$2,$3,$3,$4,$5,now(),now()+interval '1 second')`, messageID, serviceInstanceID, owner.ID,
			conversationValue.ID, batchState); err != nil {
			t.Fatalf("insert result batch %s: %v", messageID, err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_result_sources
(batch_message_id,owner_id,actor_id,conversation_id,user_message_id,task_id,task_revision,event_sequence,turn_id,content,source_order,state,payload_sha256,authorization_version)
VALUES ($1,$2,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,$11,1)`, messageID, owner.ID, conversationValue.ID,
			parent.Message.ID, parent.Task.ID, revision, eventSequence, turnID, resultContent, sourceState, contentHash[:]); err != nil {
			t.Fatalf("insert result source for batch %s: %v", messageID, err)
		}
	}
	insertBatchAndSource(batchID, 2, 4, "pending", "pending")

	dependencies, childJobs, review = readProjection(parent.Task.ID, 2)
	if len(dependencies) != 4 || len(childJobs) != 2 || review == nil {
		t.Fatalf("K2 projection counts = dependencies %d, child jobs %d, review %#v", len(dependencies), len(childJobs), review)
	}
	dependencyByID := make(map[string]app.Dependency, len(dependencies))
	for _, dependency := range dependencies {
		dependencyByID[dependency.TaskID] = dependency
	}
	if dependency, ok := dependencyByID[completed.Task.ID]; !ok || dependency.TaskStatus != task.StatusCompleted ||
		dependency.RequiredRevision != 0 || !dependency.Satisfied {
		t.Fatalf("satisfied dependency = %#v, found=%t", dependency, ok)
	}
	if dependency, ok := dependencyByID[stale.Task.ID]; !ok || dependency.TaskStatus != task.StatusCompleted || dependency.Satisfied {
		t.Fatalf("outdated dependency = %#v, found=%t", dependency, ok)
	}
	if dependency, ok := dependencyByID[failed.Task.ID]; !ok || dependency.TaskStatus != task.StatusFailed || dependency.Satisfied {
		t.Fatalf("failed dependency = %#v, found=%t", dependency, ok)
	}
	if dependency, ok := dependencyByID[emptyResult.Task.ID]; !ok || dependency.TaskStatus != task.StatusCompleted || dependency.Satisfied {
		t.Fatalf("empty-result dependency = %#v, found=%t", dependency, ok)
	}
	if childJobs[0].JobID != childValues[0].sessionID || childJobs[0].TaskStepID != childValues[0].stepID ||
		childJobs[0].Title != childValues[0].title || childJobs[0].TaskRevision != 2 || childJobs[0].Status != task.StepRunning ||
		childJobs[1].JobID != childValues[1].sessionID || childJobs[1].TaskStepID != childValues[1].stepID ||
		childJobs[1].Title != childValues[1].title || childJobs[1].TaskRevision != 2 || childJobs[1].Status != task.StepFailed {
		t.Fatalf("current revision child jobs = %#v", childJobs)
	}
	if review.State != "pending" || review.SourceTaskID != parent.Task.ID || review.SourceTaskRevision != 2 ||
		review.SourceEventSequence != 4 || review.ReplyMessageID != nil {
		t.Fatalf("pending result review = %#v", review)
	}

	assertListedProjection := func(label string, page app.TaskPage, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s Task summaries: %v", label, err)
		}
		foundTask := false
		for _, listed := range page.Items {
			if listed.ID == parent.Task.ID {
				foundTask = true
				break
			}
		}
		projection, ok := page.K2Projections[parent.Task.ID]
		if !foundTask || !ok || len(projection.Dependencies) != 4 || len(projection.ChildJobs) != 2 || projection.ResultReview == nil ||
			projection.ResultReview.State != "pending" || projection.ResultReview.SourceTaskID != parent.Task.ID ||
			projection.ResultReview.SourceTaskRevision != 2 || projection.ResultReview.SourceEventSequence != 4 {
			t.Fatalf("%s K2 summary for parent: listed=%t projection=%#v", label, foundTask, projection)
		}
	}
	listedTasks, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	assertListedProjection("all tasks", listedTasks, err)
	activeTasks, err := store.ListConversationActiveTasks(ctx, owner.ID, "", conversationValue.ID, nil, app.PageRequest{Limit: 20})
	assertListedProjection("active conversation", activeTasks, err)

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: batchID, OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: conversationValue.ID, Role: conversation.RoleAssistant, TaskID: parent.Task.ID, Content: "reviewed result"})
	}); err != nil {
		t.Fatalf("insert published assistant reply: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_result_batches SET state='published',published_message_id=message_id WHERE message_id=$1`, batchID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_result_sources SET state='published' WHERE task_id=$1 AND task_revision=2`, parent.Task.ID); err != nil {
		t.Fatal(err)
	}
	_, _, review = readProjection(parent.Task.ID, 2)
	if review == nil || review.State != "published" || review.ReplyMessageID == nil || *review.ReplyMessageID != batchID {
		t.Fatalf("published result review = %#v", review)
	}
	_, _, review = readProjection(parent.Task.ID, 3)
	if review != nil {
		t.Fatalf("older revision review leaked into a new revision = %#v", review)
	}

	supersededBatchID := mustID(t, domain.NewMessageID)
	insertBatchAndSource(supersededBatchID, 3, 5, "superseded", "superseded")
	_, _, review = readProjection(parent.Task.ID, 3)
	if review != nil {
		t.Fatalf("latest superseded result review = %#v, want nil", review)
	}

	_, olderRevisionChildren, _ := readProjection(parent.Task.ID, 1)
	if len(olderRevisionChildren) != 1 || olderRevisionChildren[0].JobID != childValues[2].sessionID ||
		olderRevisionChildren[0].TaskRevision != 1 || olderRevisionChildren[0].Status != task.StepSucceeded {
		t.Fatalf("older revision child projection = %#v", olderRevisionChildren)
	}

	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "K2 dependency scope", "Task initiator boundary", "k2-dependency-scope-project")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"k2-dependency-scope-invite", "k2-dependency-scope-accept"); err != nil {
		t.Fatalf("add project dependency initiator: %v", err)
	}
	projectConversation, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: mustID(t, domain.NewConversationID), OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "K2 dependency scope",
	}, testIdempotency(owner.ID, "project_conversation.create", "k2-dependency-scope-conversation"))
	if err != nil {
		t.Fatalf("create project dependency conversation: %v", err)
	}
	submitProjectTask := func(actorID, key string) app.TaskSubmissionResult {
		t.Helper()
		created, err := submitIndependentProjectTaskForTest(t, ctx, store, owner.ID, actorID, projectValue.ID,
			projectConversation.ID, "project-test-model", "K2 dependency scope "+key, key, nil)
		if err != nil {
			t.Fatalf("create project Task %q: %v", key, err)
		}
		return created
	}
	scopeParent := submitProjectTask(owner.ID, "k2-dependency-scope-parent")
	sameInitiator := submitProjectTask(owner.ID, "k2-dependency-scope-same-initiator")
	differentInitiator := submitProjectTask(member.ID, "k2-dependency-scope-other-initiator")
	for _, dependencyID := range []string{sameInitiator.Task.ID, differentInitiator.Task.ID} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_dependencies(task_id,owner_id,conversation_id,dependency_task_id,required_revision)
VALUES ($1,$2,$3,$4,0)`, scopeParent.Task.ID, owner.ID, projectConversation.ID, dependencyID); err != nil {
			t.Fatalf("insert scoped dependency %s: %v", dependencyID, err)
		}
	}
	scopedDependencies, _, _ := readProjection(scopeParent.Task.ID, 0)
	if len(scopedDependencies) != 1 || scopedDependencies[0].TaskID != sameInitiator.Task.ID {
		t.Fatalf("dependency projection crossed initiator scope: %#v", scopedDependencies)
	}
}
