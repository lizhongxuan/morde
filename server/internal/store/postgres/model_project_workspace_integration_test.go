package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type projectWorkspaceModelFixture struct {
	project     projectAttachmentFixture
	workspaceID string
	hostID      string
	version     int64
	fence       app.ModelTaskFence
	reservation app.ModelCallReservation
	gateway     *app.ModelGatewayService
	provider    *revisionProviderTest
	call        app.ModelCall
}

func newProjectWorkspaceModelFixture(t *testing.T, bound bool) projectWorkspaceModelFixture {
	t.Helper()
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	workspace := createTestWorkspace(t, ctx, f.store, f.owner.UserID, "synthetic shared Workspace")
	hostValue := createTestHost(t, ctx, f.store, f.owner.UserID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspace.ID})
	if _, err := f.store.pool.Exec(ctx, `UPDATE hosts SET state='running' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`, hostValue.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpsertHostMember(ctx, host.Member{HostID: hostValue.ID, OwnerID: f.owner.UserID, UserID: f.member.UserID,
		Permissions: []host.Permission{host.PermissionWorkspaceFileRead}}); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	resource, _, err := f.store.AddProjectResource(ctx, f.owner.UserID, project.Resource{ProjectID: f.projectID,
		Kind: project.ResourceWorkspace, ResourceID: workspace.ID, Version: 1}, current.Version, "project-model-workspace-resource")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',$1,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	claim := projectHistoryClaim(t, f, "project-model-workspace-001")
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "project-model-workspace-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim project model Task: found=%t err=%v", found, err)
	}
	digest := sha256.Sum256([]byte("synthetic project Workspace checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000,agent_instance_id='agent-1',agent_generation=1,
agent_session_id='session-1',agent_engine_version='engine',agent_profile_version='profile',checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, taskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if bound {
		// Persist a synthetic bound snapshot; actual selection/binding is covered
		// by the project Workspace integration tests.
		if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			if err := pinProjectTaskWorkspaceGrant(ctx, tx, taskID, f.projectID, f.member.UserID, f.owner.UserID, workspace.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE tasks SET workspace_id=$2,workspace_owner_id=$3,
workspace_project_resource_version=$4,input_revision=$5,workspace_checkpoint_revision=$5,context_digest=$6 WHERE id=$1`,
				taskID, workspace.ID, f.owner.UserID, resource.Version, workspace.Revision, digest[:])
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-1',0,$3)`, taskID, f.owner.UserID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-1',1,'session-1','engine','profile','project-model-supervisor',$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, f.owner.UserID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
VALUES ($1,$2,'agent-1',1,'session-1','session-1',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, f.owner.UserID, digest[:], claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: f.owner.UserID, InitiatorID: f.member.UserID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var connectionVersion time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	provider := &revisionProviderTest{started: make(chan struct{}), release: make(chan struct{})}
	gateway, err := app.NewModelGatewayService(f.store, revisionModelResolverTest{version: connectionVersion}, provider)
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256([]byte("synthetic project Workspace model request"))
	reservation := app.ModelCallReservation{Source: app.ModelCallTask, Task: &fence, OwnerID: f.member.UserID, ProjectID: f.projectID,
		PersonalModel: true, ModelID: "p1234567", ProviderModel: "test", ConnectionVersion: connectionVersion,
		CallID: callID, RequestDigest: requestHash[:], ReservedTokens: 100}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"synthetic"}]}],"tools":[],"max_tokens":16}`)}
	return projectWorkspaceModelFixture{project: f, workspaceID: workspace.ID, hostID: hostValue.ID, version: resource.Version,
		fence: fence, reservation: reservation, gateway: gateway, provider: provider, call: call}
}

func TestProjectModelWorkspaceFenceReservationAndDispatch(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, true)
	ctx := context.Background()
	if err := f.project.store.ReserveModelCall(ctx, f.reservation); err != nil {
		t.Fatalf("authorized shared Workspace could not reserve model call: %v", err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE project_resources SET active=false WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		f.project.projectID, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := f.project.store.BeginModelCallDispatch(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("removed project Workspace allowed dispatch: %v", err)
	}
	if err := f.project.store.AbandonModelCallReservation(ctx, f.reservation); err != nil {
		t.Fatalf("failed dispatch could not release its reservation: %v", err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE project_resources SET active=true,version=version+1 WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		f.project.projectID, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := f.project.store.ReserveModelCall(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("re-added Workspace revived stale model context: %v", err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE project_resources SET version=$3 WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		f.project.projectID, f.workspaceID, f.version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE host_members SET revoked_at=clock_timestamp() WHERE host_id=$1 AND owner_id=$2 AND user_id=$3`,
		f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
		t.Fatal(err)
	}
	if err := f.project.store.ReserveModelCall(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked independent Workspace grant allowed model reservation: %v", err)
	}
}

func TestProjectModelWorkspacePermitDoesNotDeadlockBehindRevocation(t *testing.T) {
	for _, subject := range []string{"account", "credential"} {
		t.Run(subject, func(t *testing.T) {
			f := newProjectWorkspaceModelFixture(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			instanceID, err := f.project.store.ServiceInstanceID(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Attach the synthetic Agent to this service so the real Workspace
			// guard can issue its short-lived in-process authorization permit.
			if _, err := f.project.store.pool.Exec(ctx, `UPDATE task_agent_instances SET service_instance_id=$2::uuid,
account_runtime_id='synthetic-runtime',account_runtime_generation=1,runtime_sequence=1
WHERE task_id=$1 AND agent_generation=1`, f.fence.TaskID, instanceID); err != nil {
				t.Fatal(err)
			}
			details, err := f.project.store.GetTask(ctx, f.fence.TaskID, f.project.member.UserID)
			if err != nil {
				t.Fatal(err)
			}
			entered, runNested, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			guardDone, nestedDone := make(chan error, 1), make(chan error, 1)
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			go func() {
				guardDone <- f.project.store.WithProjectTaskWorkspaceAccess(ctx, details.Task, false, func(guardedCtx context.Context) error {
					close(entered)
					select {
					case <-runNested:
					case <-release:
						return context.Canceled
					case <-guardedCtx.Done():
						return guardedCtx.Err()
					}
					err := f.project.store.ReserveModelCall(guardedCtx, f.reservation)
					if err == nil {
						err = f.project.store.BeginModelCallDispatch(guardedCtx, f.reservation)
					}
					nestedDone <- err
					if err != nil {
						return err
					}
					select {
					case <-release:
						return nil
					case <-guardedCtx.Done():
						return guardedCtx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case err := <-guardDone:
				t.Fatalf("Workspace guard did not start: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			type revocationStart struct {
				pid int32
				err error
			}
			started, revoked := make(chan revocationStart, 1), make(chan error, 1)
			go func() {
				conn, err := f.project.store.pool.Acquire(ctx)
				if err != nil {
					started <- revocationStart{err: err}
					return
				}
				defer conn.Release()
				var pid int32
				if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					started <- revocationStart{err: err}
					return
				}
				started <- revocationStart{pid: pid}
				if subject == "account" {
					_, err = conn.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, f.project.member.UserID)
				} else {
					_, err = conn.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, details.Task.Authorization.TokenID)
				}
				revoked <- err
			}()
			start := <-started
			if start.err != nil {
				t.Fatal(start.err)
			}
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				if err := f.project.store.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, start.pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-revoked:
					t.Fatalf("%s revocation passed a live Workspace permit: %v", subject, err)
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatalf("%s revocation did not queue: %v", subject, ctx.Err())
				}
			}
			close(runNested)
			select {
			case err := <-nestedDone:
				if err != nil {
					t.Fatalf("model call inside Workspace permit failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatalf("model call deadlocked behind queued %s revocation: %v", subject, ctx.Err())
			}
			select {
			case err := <-revoked:
				t.Fatalf("%s revocation passed before the Workspace permit ended: %v", subject, err)
			default:
			}
			close(release)
			released = true
			if err := <-guardDone; err != nil {
				t.Fatalf("Workspace guard did not finish: %v", err)
			}
			if err := <-revoked; err != nil {
				t.Fatalf("%s revocation did not finish: %v", subject, err)
			}
			if err := f.project.store.ValidateTaskModelDelivery(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("model output remained deliverable after %s revocation: %v", subject, err)
			}
			if err := f.project.store.SettleModelCall(ctx, app.ModelCallSettlement{Source: app.ModelCallTask,
				OwnerID: f.reservation.OwnerID, CallID: f.reservation.CallID,
				Usage: app.ModelUsage{InputTokens: 8, OutputTokens: 3, Known: true}}); err != nil {
				t.Fatalf("authorized in-flight usage was not settled: %v", err)
			}
		})
	}
}

func TestProjectWorkspaceModelPermitRequiresOriginalAgentAndLease(t *testing.T) {
	permit := &workspaceMutationPermit{taskID: "task", taskOwnerID: "owner", actorID: "actor", leaseOwner: "worker", leaseVersion: 3,
		agentID: "agent", agentGeneration: 2, agentSessionID: "root", authorization: task.Authorization{Version: 4}}
	permit.active.Store(true)
	ctx := context.WithValue(context.Background(), workspaceMutationPermitKey{}, permit)
	valid := app.ModelTaskFence{TaskID: "task", OwnerID: "owner", InitiatorID: "actor", LeaseOwner: "worker", LeaseVersion: 3,
		AgentInstanceID: "agent", AgentGeneration: 2, AgentSessionID: "child", AgentRootSessionID: "root", AuthorizationVersion: 4}
	if got, err := projectWorkspaceModelPermit(ctx, valid); err != nil || got != permit {
		t.Fatalf("matching child Agent lost its Workspace permit: %v", err)
	}
	for name, mutate := range map[string]func(*app.ModelTaskFence){
		"owner":      func(value *app.ModelTaskFence) { value.OwnerID = "other" },
		"lease":      func(value *app.ModelTaskFence) { value.LeaseVersion++ },
		"agent":      func(value *app.ModelTaskFence) { value.AgentInstanceID = "other" },
		"generation": func(value *app.ModelTaskFence) { value.AgentGeneration++ },
		"root":       func(value *app.ModelTaskFence) { value.AgentRootSessionID = "other" },
		"credential": func(value *app.ModelTaskFence) { value.AuthorizationVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			mutate(&value)
			if _, err := projectWorkspaceModelPermit(ctx, value); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("changed %s reused another Agent's Workspace permit: %v", name, err)
			}
		})
	}
	permit.active.Store(false)
	if got, err := projectWorkspaceModelPermit(ctx, valid); err != nil || got != nil {
		t.Fatalf("expired Workspace permit was reused: permit=%v err=%v", got, err)
	}
}

func TestProjectModelWorkspaceGrantCannotBeRevivedByRegrantOrRebinding(t *testing.T) {
	for _, change := range []string{"regrant", "rebind"} {
		t.Run(change, func(t *testing.T) {
			f := newProjectWorkspaceModelFixture(t, true)
			ctx := context.Background()
			if err := f.project.store.ReserveModelCall(ctx, f.reservation); err != nil {
				t.Fatalf("reserve against the original Workspace grant: %v", err)
			}
			switch change {
			case "regrant":
				if err := f.project.store.RevokeHostMember(ctx, f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
					t.Fatalf("revoke original Workspace grant: %v", err)
				}
				if _, err := f.project.store.UpsertHostMember(ctx, host.Member{HostID: f.hostID, OwnerID: f.project.owner.UserID,
					UserID: f.project.member.UserID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead}}); err != nil {
					t.Fatalf("grant new Workspace access: %v", err)
				}
			case "rebind":
				if _, err := f.project.store.pool.Exec(ctx, `UPDATE workspace_bindings SET binding_version=binding_version+1
WHERE host_id=$1 AND workspace_id=$2`, f.hostID, f.workspaceID); err != nil {
					t.Fatalf("advance Workspace binding version: %v", err)
				}
			}
			if err := f.project.store.BeginModelCallDispatch(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("%s revived an old Workspace grant for model dispatch: %v", change, err)
			}
			if err := f.project.store.AbandonModelCallReservation(ctx, f.reservation); err != nil {
				t.Fatalf("release reservation after stale Workspace grant: %v", err)
			}
		})
	}
}

func bindSyntheticSharedHostModelFixture(t *testing.T, f projectWorkspaceModelFixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.project.store.UpsertHostMember(ctx, host.Member{HostID: f.hostID, OwnerID: f.project.owner.UserID,
		UserID: f.project.member.UserID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead, host.PermissionDesktopControl}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='synthetic-model-host' WHERE id=$1`, f.hostID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1`, f.fence.TaskID, f.hostID); err != nil {
		t.Fatal(err)
	}
	err := f.project.store.WithTx(ctx, func(tx pgx.Tx) error {
		target, err := lockAgentHostTarget(ctx, tx, f.fence.TaskID, f.fence.OwnerID)
		if err != nil {
			return err
		}
		captured, err := captureAgentHostFence(ctx, tx, agentTaskLock{HostID: f.hostID, InitiatorID: f.fence.InitiatorID}, target)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_instances SET host_id=$3,host_owner_id=$4,host_actor_id=$5,
host_permission=$6,host_authorization_kind=$7,host_runtime_generation=$8,host_runtime_instance_id=$9,
host_control_version=$10,host_grant_fingerprint=$11 WHERE task_id=$1 AND agent_generation=$2`,
			f.fence.TaskID, f.fence.AgentGeneration, captured.HostID, captured.OwnerID, captured.ActorID, captured.Permission,
			captured.AuthorizationKind, captured.RuntimeGeneration, captured.RuntimeInstanceID, captured.ControlVersion, captured.GrantFingerprint)
		return err
	})
	if err != nil {
		t.Fatalf("capture synthetic shared Host grant: %v", err)
	}
}

func TestProjectModelSharedHostOfflineStillCompletes(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, false)
	bindSyntheticSharedHostModelFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE hosts SET state='stopped',control_state='user' WHERE id=$1`, f.hostID); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		response app.ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := f.gateway.Complete(ctx, f.call)
		done <- outcome{response: response, err: err}
	}()
	select {
	case <-f.provider.started:
		close(f.provider.release)
	case result := <-done:
		t.Fatalf("stopped shared Host blocked model dispatch: response=%+v err=%v", result.response, result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result := <-done
	if result.err != nil || result.response.Text != "old result" {
		t.Fatalf("stopped shared Host blocked model delivery: response=%+v err=%v", result.response, result.err)
	}
}

func TestProjectModelSharedHostGrantSurvivesStopButNotRegrant(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, false)
	bindSyntheticSharedHostModelFixture(t, f)
	ctx := context.Background()
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, f.hostID); err != nil {
		t.Fatal(err)
	}
	if err := f.project.store.ReserveModelCall(ctx, f.reservation); err != nil {
		t.Fatalf("stopped shared Host blocked a text model call: %v", err)
	}
	if err := f.project.store.RevokeHostMember(ctx, f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.project.store.UpsertHostMember(ctx, host.Member{HostID: f.hostID, OwnerID: f.project.owner.UserID,
		UserID: f.project.member.UserID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead, host.PermissionDesktopControl}}); err != nil {
		t.Fatal(err)
	}
	if err := f.project.store.BeginModelCallDispatch(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("regranted shared Host revived the old Agent model context: %v", err)
	}
	if err := f.project.store.AbandonModelCallReservation(ctx, f.reservation); err != nil {
		t.Fatalf("release reservation after Host grant changed: %v", err)
	}
}

func TestProjectModelSharedHostRevocationSerializesWithReservation(t *testing.T) {
	for _, revocationFirst := range []bool{true, false} {
		name := "reservation_first"
		if revocationFirst {
			name = "revocation_first"
		}
		t.Run(name, func(t *testing.T) {
			f := newProjectWorkspaceModelFixture(t, false)
			bindSyntheticSharedHostModelFixture(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tx, err := f.project.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			var blockerPID int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			if revocationFirst {
				var owner string
				if err := tx.QueryRow(ctx, `SELECT owner_id FROM owner_execution_controls WHERE owner_id=$1 FOR UPDATE`, f.project.owner.UserID).Scan(&owner); err != nil {
					t.Fatal(err)
				}
				if err := tx.QueryRow(ctx, `SELECT id FROM hosts WHERE id=$1 FOR UPDATE`, f.hostID).Scan(new(string)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `UPDATE host_members SET revoked_at=clock_timestamp() WHERE host_id=$1 AND owner_id=$2 AND user_id=$3`,
					f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.QueryRow(ctx, `SELECT id FROM hosts WHERE id=$1 FOR UPDATE`, f.hostID).Scan(new(string)); err != nil {
				t.Fatal(err)
			}
			reserved := make(chan error, 1)
			go func() { reserved <- f.project.store.ReserveModelCall(ctx, f.reservation) }()
			modelPID := waitForMCPBlockedTransaction(t, ctx, f.project.store, blockerPID)
			var revoked chan error
			if !revocationFirst {
				revoked = make(chan error, 1)
				go func() {
					revoked <- f.project.store.RevokeHostMember(ctx, f.hostID, f.project.owner.UserID, f.project.member.UserID)
				}()
				waitForMCPBlockedTransaction(t, ctx, f.project.store, modelPID)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-reserved:
				if revocationFirst && domain.CodeOf(err) != domain.CodeUnauthenticated || !revocationFirst && err != nil {
					t.Fatalf("reservation after %s = %v", name, err)
				}
			case <-ctx.Done():
				t.Fatalf("reservation deadlocked after %s: %v", name, ctx.Err())
			}
			if revocationFirst {
				return
			}
			select {
			case err := <-revoked:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatalf("Host revocation deadlocked after reservation: %v", ctx.Err())
			}
			if err := f.project.store.BeginModelCallDispatch(ctx, f.reservation); domain.CodeOf(err) != domain.CodeUnauthenticated {
				t.Fatalf("Host revocation was not rechecked before dispatch: %v", err)
			}
			if err := f.project.store.AbandonModelCallReservation(ctx, f.reservation); err != nil {
				t.Fatalf("release pre-revocation reservation: %v", err)
			}
		})
	}
}

func TestProjectModelSharedHostRevocationFencesDeliveryButSettlesUsage(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, false)
	bindSyntheticSharedHostModelFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type outcome struct {
		response app.ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := f.gateway.Complete(ctx, f.call)
		done <- outcome{response: response, err: err}
	}()
	select {
	case <-f.provider.started:
	case result := <-done:
		t.Fatalf("authorized shared Host model call did not dispatch: response=%+v err=%v", result.response, result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := f.project.store.RevokeHostMember(ctx, f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
		t.Fatal(err)
	}
	close(f.provider.release)
	result := <-done
	if domain.CodeOf(result.err) != domain.CodeUnauthenticated || result.response.Text != "" {
		t.Fatalf("model output escaped after Host grant revocation: response=%+v err=%v", result.response, result.err)
	}
	var state string
	var input, output int64
	if err := f.project.store.pool.QueryRow(ctx, `SELECT state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, f.project.member.UserID, f.call.CallID).Scan(&state, &input, &output); err != nil ||
		state != "settled" || input != 8 || output != 3 || f.provider.calls.Load() != 1 {
		t.Fatalf("shared Host in-flight usage was not settled: state=%s usage=%d/%d calls=%d err=%v", state, input, output, f.provider.calls.Load(), err)
	}
}

func TestProjectModelWorkspaceRevocationFencesDeliveryButSettlesUsage(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type outcome struct {
		response app.ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := f.gateway.Complete(ctx, f.call)
		done <- outcome{response: response, err: err}
	}()
	select {
	case <-f.provider.started:
	case result := <-done:
		t.Fatalf("authorized model call did not dispatch: %+v err=%v", result.response, result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE host_members SET revoked_at=clock_timestamp() WHERE host_id=$1 AND owner_id=$2 AND user_id=$3`,
		f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
		t.Fatal(err)
	}
	close(f.provider.release)
	result := <-done
	if domain.CodeOf(result.err) != domain.CodeUnauthenticated || result.response.Text != "" {
		t.Fatalf("model output escaped after Workspace revocation: response=%+v err=%v", result.response, result.err)
	}
	var state string
	var input, output int64
	if err := f.project.store.pool.QueryRow(ctx, `SELECT state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, f.project.member.UserID, f.call.CallID).Scan(&state, &input, &output); err != nil ||
		state != "settled" || input != 8 || output != 3 || f.provider.calls.Load() != 1 {
		t.Fatalf("in-flight usage was not settled: state=%s usage=%d/%d calls=%d err=%v", state, input, output, f.provider.calls.Load(), err)
	}
}

func TestProjectModelWithoutWorkspaceDoesNotDependOnProjectWorkspace(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, false)
	ctx := context.Background()
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE project_resources SET active=false WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		f.project.projectID, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := f.gateway.Complete(ctx, f.call)
		if err == nil && response.Text != "old result" {
			err = domain.NewError(domain.CodeInvalidState, "unexpected synthetic model response")
		}
		done <- err
	}()
	select {
	case <-f.provider.started:
		close(f.provider.release)
	case err := <-done:
		t.Fatalf("unbound project Task was blocked by Workspace revocation: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("unbound project model call did not start")
	}
	if err := <-done; err != nil {
		t.Fatalf("unbound project model call failed: %v", err)
	}
}

func TestProjectRevisionSettlementRequiresCurrentWorkspaceGrant(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, true)
	ctx := context.Background()
	claim := projectHistoryClaimWithReference(t, f.project, "project-model-workspace-revision", f.fence.TaskID, "change the report scope")
	snapshot, err := f.project.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("project revision candidate: %+v err=%v", snapshot.Context.Candidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.project.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: f.fence.TaskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.project.store.CheckRevisionSettlement(ctx, f.fence); err != nil || !proven {
		t.Fatalf("authorized project revision settlement not proven: proven=%t err=%v", proven, err)
	}
	if _, err := f.project.store.pool.Exec(ctx, `UPDATE project_resources SET active=false WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`,
		f.project.projectID, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.project.store.CheckRevisionSettlement(ctx, f.fence); err != nil || proven {
		t.Fatalf("revision settlement continued after Workspace removal: proven=%t err=%v", proven, err)
	}
}

func TestProjectRevisionSettlementRequiresOriginalSharedHostGrant(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, false)
	bindSyntheticSharedHostModelFixture(t, f)
	ctx := context.Background()
	claim := projectHistoryClaimWithReference(t, f.project, "project-model-host-revision", f.fence.TaskID, "change the report scope")
	snapshot, err := f.project.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("shared Host revision candidate: %+v err=%v", snapshot.Context.Candidates, err)
	}
	if err := f.project.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: f.fence.TaskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.project.store.CheckRevisionSettlement(ctx, f.fence); err != nil || !proven {
		t.Fatalf("authorized shared Host revision settlement not proven: proven=%t err=%v", proven, err)
	}
	if err := f.project.store.RevokeHostMember(ctx, f.hostID, f.project.owner.UserID, f.project.member.UserID); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.project.store.CheckRevisionSettlement(ctx, f.fence); err != nil || proven {
		t.Fatalf("revision settlement continued after shared Host revocation: proven=%t err=%v", proven, err)
	}
}

func TestProjectRevisionSettlementCancelsProviderAfterWorkspaceRevocation(t *testing.T) {
	f := newProjectWorkspaceModelFixture(t, true)
	f.provider.aborted = make(chan struct{})
	lifecycle, stopService := context.WithCancel(context.Background())
	defer stopService()
	explicit, stopExplicit := context.WithCancel(context.Background())
	defer stopExplicit()
	taskCtx, cancelTask := context.WithCancelCause(context.Background())
	defer cancelTask(context.Canceled)
	ctx := app.WithModelExplicitCancellation(app.WithModelTaskLifecycle(taskCtx, lifecycle), explicit)
	type outcome struct {
		response app.ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := f.gateway.Complete(ctx, f.call)
		done <- outcome{response: response, err: err}
	}()
	select {
	case <-f.provider.started:
	case result := <-done:
		t.Fatalf("project model provider did not start: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("project model provider did not start")
	}
	claim := projectHistoryClaimWithReference(t, f.project, "project-model-workspace-cancel", f.fence.TaskID, "change the report scope")
	snapshot, err := f.project.store.LoadConversationRouteSnapshot(context.Background(), claim)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("project revision candidate: %+v err=%v", snapshot.Context.Candidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.project.store.ApplyConversationRoute(context.Background(), claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: f.fence.TaskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.project.store.CheckRevisionSettlement(context.Background(), f.fence); err != nil || !proven {
		t.Fatalf("authorized project revision settlement not proven: proven=%t err=%v", proven, err)
	}
	cancelTask(app.ErrTaskRevisionInterrupted)
	select {
	case result := <-done:
		t.Fatalf("valid revision prematurely cancelled the provider: %v", result.err)
	case <-time.After(250 * time.Millisecond):
	}
	if _, err := f.project.store.pool.Exec(context.Background(), `UPDATE project_resources SET active=false
WHERE project_id=$1 AND kind='workspace' AND resource_id=$2`, f.project.projectID, f.workspaceID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.provider.aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("provider remained active after Workspace revocation")
	}
	result := <-done
	if !errors.Is(result.err, context.Canceled) || result.response.Text != "" || f.provider.calls.Load() != 1 {
		t.Fatalf("revoked revision returned an old result: response=%+v err=%v calls=%d", result.response, result.err, f.provider.calls.Load())
	}
	var state string
	if err := f.project.store.pool.QueryRow(context.Background(), `SELECT state FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, f.project.member.UserID, f.call.CallID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("unconfirmed provider usage was not preserved: state=%s err=%v", state, err)
	}
}
