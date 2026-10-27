package postgres

import (
	"context"
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
