package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresAgentCheckpointFencingAndIdempotency(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "checkpoint-login-1", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	submission := newTestTaskSubmission(t, owner.ID, "", "checkpoint-task-1", "do work")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "worker-1", 20*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t value=%+v err=%v", ok, claimed, err)
	}
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-1", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: owner.ID, AgentInstanceID: "agent-1",
			AgentSessionID: "session-1", SupervisorID: "supervisor-1", LeaseOwner: "worker-1",
			LeaseVersion: claimed.Lease.Version, AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: nonce, LeaseDuration: 10 * time.Second,
	})
	if err != nil || replayed || started.Fence.AgentGeneration != 1 {
		t.Fatalf("start durable agent instance: lease=%+v replayed=%t err=%v", started, replayed, err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: owner.ID, AgentInstanceID: started.Fence.AgentInstanceID,
		AgentGeneration: started.Fence.AgentGeneration, AgentSessionID: started.Fence.AgentSessionID,
		AuthorizationVersion: started.Fence.AuthorizationVersion, LeaseOwner: started.Fence.LeaseOwner,
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion}
	makeCommand := func(from, through int64, body string) app.AgentCheckpointCommand {
		payload := []byte(fmt.Sprintf(`{"header":{"id":"session-1"},"from_seq":%d,"events":[{"seq":%d,"text":%q}]}`, from, from, body))
		digest := sha256.Sum256(payload)
		return app.AgentCheckpointCommand{Fence: fence, From: from, Through: through, Digest: digest[:], Payload: payload}
	}
	first := makeCommand(0, 0, "private content")
	type result struct {
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			replayed, err := service.Save(ctx, first)
			results <- result{replayed: replayed, err: err}
		}()
	}
	workers.Wait()
	close(results)
	createdCount, replayCount := 0, 0
	for value := range results {
		if value.err != nil {
			t.Fatalf("concurrent checkpoint: %v", value.err)
		}
		if value.replayed {
			replayCount++
		} else {
			createdCount++
		}
	}
	if createdCount != 1 || replayCount != 1 {
		t.Fatalf("new/replayed checkpoints = %d/%d", createdCount, replayCount)
	}
	if _, err := service.Save(ctx, makeCommand(0, 0, "changed")); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed replay = %v", err)
	}
	if _, err := service.Save(ctx, makeCommand(2, 2, "gap")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("sequence gap = %v", err)
	}
	stale := makeCommand(1, 1, "stale")
	stale.Fence.AgentGeneration++
	if _, err := service.Save(ctx, stale); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale agent accepted = %v", err)
	}
	if err := clients.Revoke(ctx, principal, principal.ClientInstallationID, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(ctx, makeCommand(1, 1, "after revoke")); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation accepted = %v", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_checkpoints WHERE task_id=$1`, claimed.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted checkpoint count = %d, %v", count, err)
	}
}

func TestPostgresAgentCheckpointPagesRequireCurrentRecoveredLease(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "checkpoint-page-login", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	submission := newTestTaskSubmission(t, owner.ID, "", "checkpoint-page-task", "do work")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	firstTask, ok, err := store.ClaimNextTask(ctx, "worker-1", 20*time.Second)
	if err != nil || !ok || firstTask.ID != created.Task.ID {
		t.Fatalf("first claim = %#v, %v, %v", firstTask, ok, err)
	}
	key := make([]byte, 32)
	key[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	start := func(current task.Task, instanceID string, nonceByte byte) app.AgentInstanceLease {
		t.Helper()
		nonce := make([]byte, 32)
		nonce[0] = nonceByte
		lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
			TaskID: current.ID, OwnerID: owner.ID, AgentInstanceID: instanceID, AgentSessionID: "session-page-1",
			SupervisorID: "supervisor-1", LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
		}, SupervisorNonce: nonce, LeaseDuration: 10 * time.Second})
		if err != nil || replayed {
			t.Fatalf("start %s = %#v replayed=%v err=%v", instanceID, lease, replayed, err)
		}
		return lease
	}
	firstLease := start(firstTask, "agent-page-1", 1)
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	checkpointFence := func(lease app.AgentInstanceLease) app.AgentCheckpointFence {
		return app.AgentCheckpointFence{TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID,
			AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
			AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	}
	makeCommand := func(lease app.AgentInstanceLease, seq int64, content string) app.AgentCheckpointCommand {
		payload := []byte(fmt.Sprintf(`{"header":{"id":"session-page-1"},"from_seq":%d,"events":[{"seq":%d,"text":%q}]}`, seq, seq, content))
		digest := sha256.Sum256(payload)
		return app.AgentCheckpointCommand{Fence: checkpointFence(lease), From: seq, Through: seq, Digest: digest[:], Payload: payload}
	}
	page0 := makeCommand(firstLease, 0, "first checkpoint")
	page1 := makeCommand(firstLease, 1, "tail checkpoint")
	if _, err := checkpoints.Save(ctx, page0); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.Save(ctx, page1); err != nil {
		t.Fatal(err)
	}
	childSessions, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	spawn := app.AgentChildSessionCommand{Fence: firstLease.Fence, ParentSessionID: firstLease.Fence.AgentSessionID,
		SpawnKey: "call_spawn_1", Arguments: []byte(`{"instruction":"inspect one file"}`), Title: "Inspect a file", Summary: "Read-only child task",
		AllowedTools: []string{"file_read", "workspace_search"}}
	childSession, replayed, err := childSessions.Create(ctx, spawn)
	if err != nil || replayed {
		t.Fatalf("create child session = %#v replayed=%t err=%v", childSession, replayed, err)
	}
	childReplay, replayed, err := childSessions.Create(ctx, spawn)
	if err != nil || !replayed || childReplay.SessionID != childSession.SessionID || childReplay.Step.ID != childSession.Step.ID ||
		!sameAgentToolNames(childSession.AllowedTools, spawn.AllowedTools) || !sameAgentToolNames(childReplay.AllowedTools, spawn.AllowedTools) {
		t.Fatalf("replay child session = %#v replayed=%t err=%v", childReplay, replayed, err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: firstTask.ID, LeaseOwner: firstTask.Lease.Owner,
		LeaseVersion: firstTask.Lease.Version, Status: task.StatusCompleted, Result: "done",
		Steps: []task.Step{{ID: childSession.Step.ID, TaskID: firstTask.ID, Sequence: childSession.Step.Sequence,
			Kind: "subagent", Status: task.StepSucceeded, Title: childSession.Step.Title, Summary: "forged completion"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"done"}`)}}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("parent Task changed its child Session step: %v", err)
	}
	childDetails, err := store.GetTask(ctx, firstTask.ID, owner.ID)
	if err != nil || childDetails.Task.Status != task.StatusRunning || len(childDetails.Steps) == 0 ||
		childDetails.Steps[len(childDetails.Steps)-1].ID != childSession.Step.ID ||
		childDetails.Steps[len(childDetails.Steps)-1].Status != task.StepRunning {
		t.Fatalf("root progress changed child Session state: task=%#v steps=%#v err=%v", childDetails.Task, childDetails.Steps, err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: firstTask.ID, LeaseOwner: firstTask.Lease.Owner,
		LeaseVersion: firstTask.Lease.Version, Status: task.StatusCompleted, Result: "done",
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"done"}`)}}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("parent Task completed with a running child Session: %v", err)
	}
	changedSpawn := spawn
	changedSpawn.Arguments = []byte(`{"instruction":"inspect another file"}`)
	if _, _, err := childSessions.Create(ctx, changedSpawn); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed child spawn accepted: %v", err)
	}
	childSessionID := childSession.SessionID
	if _, err := childSessions.Get(ctx, firstLease.Fence, "session_other", childSessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("unrelated Agent Session read a child Session: %v", err)
	}
	childPayload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"child checkpoint"}]}`, childSessionID))
	childDigest := sha256.Sum256(childPayload)
	childFence := checkpointFence(firstLease)
	childFence.AgentSessionID, childFence.AgentRootSessionID = childSessionID, firstLease.Fence.AgentSessionID
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: childFence, From: 0, Through: 0,
		Digest: childDigest[:], Payload: childPayload}); err != nil {
		t.Fatalf("save child checkpoint: %v", err)
	}
	var rootThrough int64
	if err := store.pool.QueryRow(ctx, `SELECT checkpoint_through_seq FROM tasks WHERE id=$1`, firstTask.ID).Scan(&rootThrough); err != nil || rootThrough != 1 {
		t.Fatalf("child checkpoint advanced root head to %d: %v", rootThrough, err)
	}
	firstNonce := make([]byte, 32)
	firstNonce[0] = 1
	stop := app.AgentStopReceipt{Fence: firstLease.Fence, SupervisorNonce: firstNonce, TreeStopped: true}
	stopPayload, err := app.AgentStopReceiptSigningPayload(stop)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(stopPayload)
	stop.Signature = mac.Sum(nil)
	if _, err := instances.ConfirmStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, firstTask.ID); err != nil {
		t.Fatal(err)
	}
	secondTask, ok, err := store.ClaimNextTask(ctx, "worker-2", 20*time.Second)
	if err != nil || !ok || secondTask.ID != firstTask.ID || secondTask.CheckpointThroughSequence != 1 {
		t.Fatalf("recovery claim = %#v, %v, %v", secondTask, ok, err)
	}
	secondLease := start(secondTask, "agent-page-2", 2)
	read := func(from int64) app.AgentCheckpointPage {
		t.Helper()
		page, err := checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{Fence: secondLease.Fence,
			From: from, Through: 1, ThroughDigest: page1.Digest})
		if err != nil {
			t.Fatalf("read checkpoint page %d: %v", from, err)
		}
		return page
	}
	if page := read(0); page.From != 0 || page.Through != 0 || !bytes.Equal(page.Payload, page0.Payload) {
		t.Fatalf("first checkpoint page = %#v", page)
	}
	if page := read(1); page.From != 1 || page.Through != 1 || !bytes.Equal(page.Payload, page1.Payload) {
		t.Fatalf("tail checkpoint page = %#v", page)
	}
	childPage, err := checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{Fence: secondLease.Fence,
		SessionID: childSessionID, From: 0, Through: 0, ThroughDigest: childDigest[:]})
	if err != nil || childPage.From != 0 || childPage.Through != 0 || !bytes.Equal(childPage.Payload, childPayload) {
		t.Fatalf("restored child checkpoint = %#v, %v", childPage, err)
	}
	recoveredChild, err := childSessions.Get(ctx, secondLease.Fence, secondLease.Fence.AgentSessionID, childSessionID)
	if err != nil || recoveredChild.Step.Status != task.StepRunning || recoveredChild.CheckpointThrough != 0 ||
		!bytes.Equal(recoveredChild.CheckpointDigest, childDigest[:]) {
		t.Fatalf("read recovered child session = %#v, %v", recoveredChild, err)
	}
	wrongDigest := sha256.Sum256([]byte("different checkpoint"))
	if _, _, err := childSessions.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: secondLease.Fence, ActorSessionID: secondLease.Fence.AgentSessionID, SessionID: childSessionID,
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: wrongDigest[:], Summary: "must not settle",
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("child session settled with an uncommitted checkpoint head: %v", err)
	}
	settledChild, replayed, err := childSessions.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: secondLease.Fence, ActorSessionID: secondLease.Fence.AgentSessionID, SessionID: childSessionID,
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: childDigest[:], Summary: "Child work finished.",
	})
	if err != nil || replayed || settledChild.Step.Status != task.StepSucceeded || settledChild.Step.Summary != "Child work finished." {
		t.Fatalf("settle recovered child session = %#v replayed=%t err=%v", settledChild, replayed, err)
	}
	if _, replayed, err := childSessions.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: secondLease.Fence, ActorSessionID: secondLease.Fence.AgentSessionID, SessionID: childSessionID,
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: childDigest[:], Summary: "Child work finished.",
	}); err != nil || !replayed {
		t.Fatalf("replay child settlement = replayed %t err=%v", replayed, err)
	}
	if _, _, err := childSessions.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: secondLease.Fence, ActorSessionID: secondLease.Fence.AgentSessionID, SessionID: childSessionID,
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: childDigest[:], Summary: "Changed result.",
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed child settlement summary error = %v", err)
	}
	if _, _, err := childSessions.Settle(ctx, app.AgentChildSessionSettleCommand{
		Fence: firstLease.Fence, ActorSessionID: firstLease.Fence.AgentSessionID, SessionID: childSessionID,
		Status: task.StepFailed, CheckpointThrough: 0, CheckpointDigest: childDigest[:], Summary: "stale worker",
	}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale Agent generation settled child Session: %v", err)
	}
	if _, err := checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{Fence: firstLease.Fence,
		From: 0, Through: 1, ThroughDigest: page1.Digest}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("old agent lease read checkpoint = %v", err)
	}
	if _, err := checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{Fence: secondLease.Fence,
		From: 0, Through: 1, ThroughDigest: []byte("bad")}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid pinned digest = %v", err)
	}
	cancelledChild, replayed, err := childSessions.Create(ctx, app.AgentChildSessionCommand{Fence: secondLease.Fence,
		ParentSessionID: secondLease.Fence.AgentSessionID, SpawnKey: "call_spawn_2",
		Arguments: []byte(`{"instruction":"cancel child"}`), Title: "Cancelable child", Summary: "Will be stopped with the parent."})
	if err != nil || replayed {
		t.Fatalf("create child for cancellation settlement = %#v replayed=%t err=%v", cancelledChild, replayed, err)
	}
	for index := 0; index < 14; index++ {
		_, replayed, err := childSessions.Create(ctx, app.AgentChildSessionCommand{Fence: secondLease.Fence,
			ParentSessionID: secondLease.Fence.AgentSessionID, SpawnKey: fmt.Sprintf("call_spawn_fill_%d", index),
			Arguments: []byte(fmt.Sprintf(`{"instruction":"fill child slot %d"}`, index)), Title: fmt.Sprintf("Child %d", index)})
		if err != nil || replayed {
			t.Fatalf("create bounded child session %d: replayed=%t err=%v", index, replayed, err)
		}
	}
	if _, _, err := childSessions.Create(ctx, app.AgentChildSessionCommand{Fence: secondLease.Fence,
		ParentSessionID: secondLease.Fence.AgentSessionID, SpawnKey: "call_spawn_over_limit",
		Arguments: []byte(`{"instruction":"exceed child limit"}`), Title: "Over limit"}); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("Task accepted more than its child Agent limit: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='cancelling' WHERE id=$1`, firstTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.ConfirmStopped(ctx, stop); err != nil {
		t.Fatalf("replay stale generation stop receipt: %v", err)
	}
	staleStopDetails, err := store.GetTask(ctx, firstTask.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	childStillRunning := false
	for _, step := range staleStopDetails.Steps {
		if step.ID == cancelledChild.Step.ID {
			childStillRunning = step.Status == task.StepRunning
			break
		}
	}
	if !childStillRunning {
		t.Fatalf("stale stop receipt settled or lost current-generation child step: %#v", staleStopDetails.Steps)
	}
	secondNonce := make([]byte, 32)
	secondNonce[0] = 2
	secondStop := app.AgentStopReceipt{Fence: secondLease.Fence, SupervisorNonce: secondNonce, TreeStopped: true}
	secondStopPayload, err := app.AgentStopReceiptSigningPayload(secondStop)
	if err != nil {
		t.Fatal(err)
	}
	secondMAC := hmac.New(sha256.New, key)
	_, _ = secondMAC.Write(secondStopPayload)
	secondStop.Signature = secondMAC.Sum(nil)
	if _, err := instances.ConfirmStopped(ctx, secondStop); err != nil {
		t.Fatalf("confirm cancelled Agent tree stop: %v", err)
	}
	cancelledDetails, err := store.GetTask(ctx, firstTask.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cancelledStep *task.Step
	for i := range cancelledDetails.Steps {
		if cancelledDetails.Steps[i].ID == cancelledChild.Step.ID {
			cancelledStep = &cancelledDetails.Steps[i]
			break
		}
	}
	if cancelledStep == nil || cancelledStep.Status != task.StepUnknown {
		t.Fatalf("verified parent stop did not settle child step as unknown: %#v", cancelledStep)
	}
}
