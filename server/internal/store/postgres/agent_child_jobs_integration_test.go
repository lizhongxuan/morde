package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresBackgroundAgentJobsListAndCancelFence(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clients.Login(ctx, owner.Name, "client-password-123", "ios", "phone", "", "background-job-login", "127.0.0.1")
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
	submission := newTestTaskSubmission(t, owner.ID, "", "background-job-task", "inspect workspace")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "worker-1", 20*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: claimed=%t id=%q err=%v", ok, claimed.ID, err)
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
	nonce := make([]byte, 32)
	nonce[0] = 1
	lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: owner.ID, AgentInstanceID: "agent-job-1",
			AgentSessionID: "session-job-root", SupervisorID: "supervisor-1", LeaseOwner: "worker-1",
			LeaseVersion: claimed.Lease.Version, AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: nonce, LeaseDuration: 10 * time.Second,
	})
	if err != nil || replayed || lease.Fence.AgentGeneration != 1 {
		t.Fatalf("start Agent: lease=%#v replayed=%t err=%v", lease, replayed, err)
	}
	service, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	create := func(spawnKey string, background bool) app.AgentChildSession {
		t.Helper()
		child, replayed, err := service.Create(ctx, app.AgentChildSessionCommand{
			Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: spawnKey,
			Arguments: []byte(`{"title":"Collect notes","objective":"read the workspace"}`),
			Title:     "Collect notes", Summary: "safe child task", AllowedTools: []string{"workspace_read"}, Background: background,
		})
		if err != nil || replayed || child.Step.Status != task.StepRunning || child.Background != background {
			t.Fatalf("create child background=%t: child=%#v replayed=%t err=%v", background, child, replayed, err)
		}
		return child
	}
	job := create("job_spawn", true)
	ordinary := create("ordinary_spawn", false)
	replayedJob, replayed, err := service.Create(ctx, app.AgentChildSessionCommand{
		Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: "job_spawn",
		Arguments: []byte(`{"title":"Collect notes","objective":"read the workspace"}`),
		Title:     "Collect notes", Summary: "safe child task", AllowedTools: []string{"workspace_read"}, Background: true,
	})
	if err != nil || !replayed || replayedJob.SessionID != job.SessionID || replayedJob.SpawnKey != job.SpawnKey {
		t.Fatalf("replay background job: job=%#v replayed=%t err=%v", replayedJob, replayed, err)
	}
	if _, _, err := service.Create(ctx, app.AgentChildSessionCommand{
		Fence: lease.Fence, ParentSessionID: lease.Fence.AgentSessionID, SpawnKey: "job_spawn",
		Arguments: []byte(`{"title":"Collect notes","objective":"read the workspace"}`),
		Title:     "Collect notes", Summary: "safe child task", AllowedTools: []string{"workspace_read"}, Background: false,
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("background job replay changed mode: %v", err)
	}
	jobs, err := service.List(ctx, lease.Fence, lease.Fence.AgentSessionID)
	if err != nil || len(jobs) != 1 || jobs[0].SessionID != job.SessionID || !jobs[0].Background || jobs[0].CancelRequested {
		t.Fatalf("list background jobs: jobs=%#v err=%v", jobs, err)
	}
	if _, err := service.List(ctx, lease.Fence, ordinary.SessionID); err == nil {
		t.Fatal("non-root session listed background jobs")
	}
	stale := lease.Fence
	stale.AgentGeneration++
	if _, err := service.List(ctx, stale, stale.AgentSessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale generation listed background jobs: %v", err)
	}
	wrongOwner := lease.Fence
	wrongOwner.OwnerID = "u7654321"
	if _, err := service.List(ctx, wrongOwner, wrongOwner.AgentSessionID); err == nil {
		t.Fatalf("other owner listed background jobs: %v", err)
	}
	wrongTask := lease.Fence
	wrongTask.TaskID = "q7654321"
	if _, err := service.List(ctx, wrongTask, wrongTask.AgentSessionID); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("other Task listed background jobs: %v", err)
	}

	cancel := app.AgentChildSessionCancelCommand{Fence: lease.Fence, ActorSessionID: lease.Fence.AgentSessionID, SessionID: job.SessionID}
	requested, replayed, err := service.RequestCancel(ctx, cancel)
	if err != nil || replayed || !requested.Background || !requested.CancelRequested || requested.Step.Status != task.StepRunning {
		t.Fatalf("request job cancellation: job=%#v replayed=%t err=%v", requested, replayed, err)
	}
	requested, replayed, err = service.RequestCancel(ctx, cancel)
	if err != nil || !replayed || !requested.CancelRequested || requested.Step.Status != task.StepRunning {
		t.Fatalf("replay job cancellation: job=%#v replayed=%t err=%v", requested, replayed, err)
	}
	var cancelEvents int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND payload->>'phase'='subagent_cancel_requested'`, claimed.ID).Scan(&cancelEvents); err != nil || cancelEvents != 1 {
		t.Fatalf("cancel request events=%d err=%v", cancelEvents, err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: stale, ActorSessionID: stale.AgentSessionID, SessionID: job.SessionID}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale generation canceled background job: %v", err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: wrongOwner, ActorSessionID: wrongOwner.AgentSessionID, SessionID: job.SessionID}); err == nil {
		t.Fatalf("other owner canceled background job: %v", err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: wrongTask, ActorSessionID: wrongTask.AgentSessionID, SessionID: job.SessionID}); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("other Task canceled background job: %v", err)
	}
	if _, _, err := service.RequestCancel(ctx, app.AgentChildSessionCancelCommand{Fence: lease.Fence, ActorSessionID: lease.Fence.AgentSessionID, SessionID: ordinary.SessionID}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("ordinary child session was canceled as a job: %v", err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"started"}]}`, job.SessionID))
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{TaskID: claimed.ID,
		OwnerID: owner.ID, AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: job.SessionID, AgentRootSessionID: lease.Fence.AgentSessionID,
		AuthorizationVersion: lease.Fence.AuthorizationVersion, LeaseOwner: lease.Fence.LeaseOwner,
		LeaseVersion: lease.Fence.LeaseVersion, EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion},
		From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("save child checkpoint: %v", err)
	}
	if _, _, err := service.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: lease.Fence,
		ActorSessionID: lease.Fence.AgentSessionID, SessionID: job.SessionID, Status: task.StepSucceeded,
		CheckpointThrough: 0, CheckpointDigest: digest[:], Summary: "completed after cancel request"}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("background job succeeded after a persisted cancel request: %v", err)
	}
	settled, replayed, err := service.Settle(ctx, app.AgentChildSessionSettleCommand{Fence: lease.Fence,
		ActorSessionID: lease.Fence.AgentSessionID, SessionID: job.SessionID, Status: task.StepFailed,
		CheckpointThrough: 0, CheckpointDigest: digest[:], Summary: "cancelled after stop confirmation"})
	if err != nil || replayed || settled.Step.Status != task.StepFailed || !settled.CancelRequested {
		t.Fatalf("settle cancelled job: job=%#v replayed=%t err=%v", settled, replayed, err)
	}
	settled, replayed, err = service.RequestCancel(ctx, cancel)
	if err != nil || !replayed || settled.Step.Status != task.StepFailed || !settled.CancelRequested {
		t.Fatalf("replay cancellation after terminal settlement: job=%#v replayed=%t err=%v", settled, replayed, err)
	}
}
