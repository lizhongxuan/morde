package postgres

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

func TestPostgresConversationRuntimeIdentityAndIsolation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	for _, id := range []string{"c1234567", "c7654321", "c1111111"} {
		if _, err := pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ($1,$2,'runtime test')`, id, owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	service, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	request := func(conversationID, runtimeID string, nonce byte) app.AccountAgentRuntimeStart {
		value := app.AccountAgentRuntimeStart{OwnerID: owner.ID, ConversationID: conversationID, RuntimeID: runtimeID,
			LeaseOwner: "runtime-worker", SupervisorNonce: make([]byte, 32), MaxConcurrentTasks: 2, LeaseDuration: 20 * time.Second}
		value.SupervisorNonce[0] = nonce
		return value
	}
	first, replayed, err := service.Start(ctx, request("c1234567", "runtime-first", 1))
	if err != nil || replayed {
		t.Fatalf("start first conversation Runtime: runtime=%+v replayed=%t err=%v", first, replayed, err)
	}
	first, err = service.Ready(ctx, first.Fence)
	if err != nil {
		t.Fatal(err)
	}
	second, replayed, err := service.Start(ctx, request("c7654321", "runtime-second", 2))
	if err != nil || replayed || second.Fence.RuntimeID == first.Fence.RuntimeID {
		t.Fatalf("start second conversation Runtime: runtime=%+v replayed=%t err=%v", second, replayed, err)
	}
	second, err = service.Ready(ctx, second.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RequestStop(ctx, first.Fence); err != nil {
		t.Fatalf("request stop for first conversation: %v", err)
	}
	if _, err := service.Renew(ctx, second.Fence, 15*time.Second); err != nil {
		t.Fatalf("stopping one conversation affected another: %v", err)
	}
	control, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != execution.StateEnabled {
		t.Fatalf("stopping a conversation changed the owner execution gate: control=%+v err=%v", control, err)
	}
	foreign := request("c1111111", "runtime-foreign", 3)
	foreign.OwnerID = other.ID
	if _, _, err := service.Start(ctx, foreign); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("other owner started the conversation Runtime: %v", err)
	}

	var group sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, _, startErr := service.Start(ctx, request("c1111111", fmt.Sprintf("runtime-race-%d", index), byte(4+index)))
			errs <- startErr
		}(i)
	}
	group.Wait()
	close(errs)
	started, conflicts := 0, 0
	for startErr := range errs {
		if startErr == nil {
			started++
		} else if domain.CodeOf(startErr) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent start result: %v", startErr)
		}
	}
	if started != 1 || conflicts != 1 {
		t.Fatalf("concurrent conversation Runtime starts: started=%d conflicts=%d", started, conflicts)
	}
}

func TestPostgresConversationRuntimeKeepsProjectInitiatorAuthorization(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Runtime project", "Shared work", "runtime-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"runtime-invite-01", "runtime-accept-01"); err != nil {
		t.Fatal(err)
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
	conversation, _, err := conversations.Create(ctx, principal(owner.ID), projectValue.ID,
		app.CreateProjectConversationCommand{Title: "Shared Runtime", IdempotencyKey: "runtime-conversation-01"})
	if err != nil {
		t.Fatal(err)
	}
	for index, actorID := range []string{member.ID, owner.ID} {
		_, err := submitIndependentProjectTaskForTest(t, ctx, store, owner.ID, actorID, projectValue.ID, conversation.ID,
			"project-test-model", "Review shared work", fmt.Sprintf("runtime-task-%d", index), nil)
		if err != nil {
			t.Fatalf("submit project Task for %s: %v", actorID, err)
		}
	}
	runtimeService, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: owner.ID, ConversationID: conversation.ID,
		RuntimeID: "project-runtime", LeaseOwner: "project-runtime-worker", SupervisorNonce: bytes.Repeat([]byte{7}, 32),
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = runtimeService.Ready(ctx, runtime.Fence)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{8}, 32)
	authenticator, err := app.NewAgentStopAuthenticator("project-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	leases := make(map[string]app.AgentInstanceLease)
	for index := 0; index < 2; index++ {
		claimed, found, err := store.ClaimNextTaskWithOwnerLimit(ctx, fmt.Sprintf("project-worker-%d", index), 25*time.Second, 2)
		if err != nil || !found {
			t.Fatalf("claim project Task: found=%t err=%v", found, err)
		}
		fence := app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: fmt.Sprintf("project-agent-%d", index), AgentSessionID: fmt.Sprintf("project-session-%d", index),
			SupervisorID: "project-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"}
		lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: fence, RuntimeFence: &runtime.Fence,
			SupervisorNonce: bytes.Repeat([]byte{byte(index + 1)}, 32), LeaseDuration: 20 * time.Second})
		if err != nil || replayed || lease.RuntimeFence == nil || *lease.RuntimeFence != runtime.Fence {
			t.Fatalf("start project Task for %s: lease=%+v replayed=%t err=%v", claimed.InitiatorID, lease, replayed, err)
		}
		leases[claimed.InitiatorID] = lease
	}
	if _, ok := leases[member.ID]; !ok {
		t.Fatal("member Task was not started in the shared Runtime")
	}
	if _, ok := leases[owner.ID]; !ok {
		t.Fatal("owner Task was not started in the shared Runtime")
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "runtime-member-remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.Renew(ctx, leases[member.ID].Fence, 15*time.Second); err == nil {
		t.Fatal("revoked member renewed a Task through the shared Runtime")
	}
	if _, err := instances.Renew(ctx, leases[owner.ID].Fence, 15*time.Second); err != nil {
		t.Fatalf("member revocation affected the owner's sibling Task: %v", err)
	}
}

func TestPostgresConversationRuntimeMigrationRequiresStoppedLegacyRuntime(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 144); err != nil {
		t.Fatalf("restore account Runtime schema: %v", err)
	}
	serviceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO account_agent_runtimes
(service_instance_id,owner_id,runtime_id,runtime_generation,state,lease_owner,lease_version,execution_epoch,
 lease_expires_at,max_concurrent_tasks,supervisor_nonce_sha256)
VALUES ($1::uuid,$2,'legacy-runtime',1,'running','worker',1,1,clock_timestamp()+interval '1 minute',1,repeat('a',64))`, serviceID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 145); err == nil {
		t.Fatal("migration accepted a live account Runtime")
	}
	_, err = pool.Exec(ctx, `UPDATE account_agent_runtimes SET state='stopped',lease_expires_at=NULL,
guardian_stop_attestation=convert_to('verified-legacy-proof','UTF8'),
guardian_stop_attestation_digest=sha256(convert_to('verified-legacy-proof','UTF8')),
guardian_stop_acknowledged_at=clock_timestamp(),stopped_at=clock_timestamp() WHERE runtime_id='legacy-runtime'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 145); err != nil {
		t.Fatalf("migrate stopped legacy Runtime: %v", err)
	}
	var conversationID *string
	var proof []byte
	if err := pool.QueryRow(ctx, `SELECT conversation_id,guardian_stop_attestation FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND runtime_id='legacy-runtime'`, serviceID).Scan(&conversationID, &proof); err != nil {
		t.Fatal(err)
	}
	if conversationID != nil || string(proof) != "verified-legacy-proof" {
		t.Fatalf("migration changed historical Runtime proof: conversation=%v proof=%q", conversationID, proof)
	}
	_, err = pool.Exec(ctx, `INSERT INTO account_agent_runtimes
(service_instance_id,owner_id,runtime_id,runtime_generation,state,lease_owner,lease_version,execution_epoch,
 lease_expires_at,max_concurrent_tasks,supervisor_nonce_sha256)
VALUES ($1::uuid,$2,'legacy-restart',1,'running','worker',1,1,clock_timestamp()+interval '1 minute',1,repeat('b',64))`, serviceID, owner.ID)
	if err == nil {
		t.Fatal("old account-scoped Runtime started after migration")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations(id,owner_id,title) VALUES ('c1234567',$1,'runtime test')`, owner.ID); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 7
	if _, _, err := service.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: owner.ID, ConversationID: "c1234567",
		RuntimeID: "conversation-runtime", LeaseOwner: "worker", SupervisorNonce: nonce, MaxConcurrentTasks: 1,
		LeaseDuration: 20 * time.Second}); err != nil {
		t.Fatalf("start new conversation Runtime: %v", err)
	}
	if _, err := provider.DownTo(ctx, 144); err == nil {
		t.Fatal("rollback discarded a conversation Runtime")
	}
}
