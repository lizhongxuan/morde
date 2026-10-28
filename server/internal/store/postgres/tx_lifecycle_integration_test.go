package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestHostComputeReleaseAndConcurrentRestore(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "restore-check")
	created, _, err := store.CreateHost(ctx, testCreateHostParams(t, owner.ID, capacity, []string{workspaceValue.ID}))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-release-generation-1"
	if err := store.SetSandboxID(ctx, created.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, created.ID, 1, 0, "", "runtime-instance-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, created.ID, workspaceValue.ID, 1, workspace.BindingPreparing, workspace.BindingActive, 0, 0); err != nil {
		t.Fatal(err)
	}
	workspaceBefore, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}

	stopping, changed, err := store.BeginHostRelease(ctx, created.ID, false)
	if err != nil || !changed || stopping.State != host.StateStopping {
		t.Fatalf("BeginHostRelease() = %#v, %t, %v", stopping, changed, err)
	}
	if _, err := store.CompleteHostRelease(ctx, created.ID, created.RuntimeGeneration+1); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("CompleteHostRelease(stale generation) error = %v", err)
	}
	stopped, err := store.CompleteHostRelease(ctx, created.ID, created.RuntimeGeneration)
	if err != nil || stopped.State != host.StateStopped || stopped.ID != created.ID || stopped.OwnerID != created.OwnerID ||
		len(stopped.Workspaces) != 1 || stopped.Workspaces[0].Workspace.ID != workspaceValue.ID ||
		stopped.Workspaces[0].State != workspace.BindingActive || stopped.RuntimeGeneration != 1 || stopped.SandboxID != "" || stopped.RuntimeInstanceID != "" {
		t.Fatalf("CompleteHostRelease() = %#v, %v", stopped, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET quota_hosts=0 WHERE id=$1`, owner.ID); err != nil {
		t.Fatalf("set former host-count limit before restore: %v", err)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter != workspaceBefore {
		t.Fatalf("workspace changed during compute release: %#v, err=%v; before %#v", workspaceAfter, err, workspaceBefore)
	}
	var generation int64
	var sandboxIsNull, runtimeInstanceIsNull bool
	if err := pool.QueryRow(ctx, `SELECT runtime_generation, sandbox_id IS NULL, runtime_instance_id IS NULL FROM hosts WHERE id=$1`, created.ID).
		Scan(&generation, &sandboxIsNull, &runtimeInstanceIsNull); err != nil || generation != 1 || !sandboxIsNull || !runtimeInstanceIsNull {
		t.Fatalf("stored Host after release: generation=%d sandbox_null=%t runtime_instance_null=%t err=%v", generation, sandboxIsNull, runtimeInstanceIsNull, err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var changedCount int
	var failures []error
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, changed, err := store.BeginHostRestore(ctx, app.RestoreHostParams{
				HostID: created.ID, RuntimeCredentialEnc: []byte(fmt.Sprintf("runtime-credential-%d", i)),
				ExpiresAt: time.Now().UTC().Add(time.Hour), Capacity: capacity,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			if value.State != host.StateCreating || value.StateReason != "restore_pending" || value.RuntimeGeneration != 2 ||
				value.ID != created.ID || value.OwnerID != created.OwnerID || len(value.Workspaces) != 1 || value.Workspaces[0].Workspace.ID != workspaceValue.ID {
				failures = append(failures, fmt.Errorf("restore returned unexpected Host: %#v", value))
			}
			if changed {
				changedCount++
			}
		}(i)
	}
	wg.Wait()
	if len(failures) != 0 || changedCount != 1 {
		t.Fatalf("concurrent restore: changed=%d failures=%v", changedCount, failures)
	}

	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetHostStateReason(ctx, created.ID, host.StateCreating, 2, "restore_unknown"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateDeleting, ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delete uncertain restore error = %v", err)
	}
	final, err := store.GetHost(ctx, created.ID)
	if err != nil || final.State != host.StateCreating || final.StateReason != "restore_unknown" || final.RuntimeGeneration != 2 || final.CreateAttemptedAt == nil {
		t.Fatalf("Host after refused uncertain-restore delete = %#v, %v", final, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='stopped', create_attempted_at=NULL WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateStopped, host.StateDeleting, ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delete with persisted restore_unknown reason but no timestamp error = %v", err)
	}
	workspaceFinal, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceFinal != workspaceBefore {
		t.Fatalf("workspace changed after refused delete: %#v, err=%v; before %#v", workspaceFinal, err, workspaceBefore)
	}
}

func TestBeginHostReleaseChecksControlTasksAndActiveSessions(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}
	created := createTestHost(t, ctx, store, owner.ID, capacity, nil)
	if err := store.MarkCreateAttempt(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, created.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_state='user' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, created.ID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release while user-controlled = %#v, %t, %v", value, changed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_state='ai' WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}

	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id) VALUES ($1,$2,$3)`, conversationID, owner.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO tasks (id, owner_id, conversation_id, host_id, model_id, objective, status, auth_kind,
                   budget_active_seconds, budget_tool_calls, budget_model_tokens)
VALUES ($1,$2,$3,$4,'model-test','active work','queued','session',60,10,1000)`, taskID, owner.ID, conversationID, created.ID); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, created.ID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release with active task = %#v, %t, %v", value, changed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='paused' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO task_operations (id, task_id, operation_key, request_hash, host_id,
                             runtime_generation, control_version, lease_version, status)
VALUES ('o1234567',$1,'release-unknown',$2,$3,$4,$5,1,'unknown')`,
		taskID, make([]byte, 32), created.ID, created.RuntimeGeneration, created.ControlVersion); err != nil {
		t.Fatal(err)
	}
	_, changed, err := store.BeginHostRelease(ctx, created.ID, false)
	var operationConflict *domain.Error
	if changed || domain.CodeOf(err) != domain.CodeConflict || !errors.As(err, &operationConflict) ||
		operationConflict.Details["task_id"] != taskID {
		t.Fatalf("release with paused task's unknown operation: changed=%t err=%v", changed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_operations SET status='failed' WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=now() WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	lease := app.HostActivityLease{
		HostID: created.ID, LeaseID: "active-release-test", RuntimeGeneration: created.RuntimeGeneration,
		ControlVersion: created.ControlVersion, Kind: app.HostActivityLeaseKindPreview, TTL: 30 * time.Second,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, created.ID, false); err != nil || !changed || value.State != host.StateStopping {
		t.Fatalf("release with active session = %#v, %t, %v", value, changed, err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, created.ID, created.RuntimeGeneration); err != nil || !active {
		t.Fatalf("stopping Host did not retain its active session lease: active=%t, err=%v", active, err)
	}
	if err := store.AcquireHostActivityLease(ctx, lease); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("activity lease acquired after Host entered stopping: %v", err)
	}
	if err := store.ReleaseHostActivityLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if active, err := store.HasActiveHostActivityLease(ctx, created.ID, created.RuntimeGeneration); err != nil || active {
		t.Fatalf("released session remained active: active=%t, err=%v", active, err)
	}
}

func TestBeginHostReleaseBlocksUncommittedHostWorkspaceSync(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "release-uncommitted")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID})
	if err := store.MarkCreateAttempt(ctx, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, hostValue.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	binding, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, 1,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision)
	if err != nil {
		t.Fatal(err)
	}
	file := syncTestFile("pending.txt", []byte("Host has an unpublished change"))
	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", hostValue.ID, hostValue.RuntimeGeneration, workspaceValue.ID, binding.BindingVersion)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "release-uncommitted:create", session.ID)); err != nil {
		t.Fatal(err)
	}
	_, changed, err := store.BeginHostRelease(ctx, hostValue.ID, true)
	var scoped *domain.Error
	if changed || domain.CodeOf(err) != domain.CodeConflict || !errors.As(err, &scoped) ||
		scoped.Details["workspace_id"] != workspaceValue.ID {
		t.Fatalf("release during Host upload: changed=%t err=%v", changed, err)
	}
	if current, getErr := store.GetHost(ctx, hostValue.ID); getErr != nil || current.State != host.StateRunning {
		t.Fatalf("Host after rejected upload release = %#v, %v", current, getErr)
	}
	committed, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "release-uncommitted:commit", session.ID))
	if err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("commit Host upload = %#v, %v", committed, err)
	}
	updatedWorkspace, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	deletion := newTestSyncSession(t, updatedWorkspace, owner.ID, nil, nil, []string{file.Path})
	deletion.ReplicaID = session.ReplicaID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, deletion, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "release-uncommitted:delete", deletion.ID)); err != nil {
		t.Fatal(err)
	}
	_, changed, err = store.BeginHostRelease(ctx, hostValue.ID, true)
	if changed || domain.CodeOf(err) != domain.CodeConflict || !errors.As(err, &scoped) ||
		scoped.Details["workspace_id"] != workspaceValue.ID {
		t.Fatalf("release during Host deletion sync: changed=%t err=%v", changed, err)
	}
	if deleted, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, deletion.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "release-uncommitted:delete-commit", deletion.ID)); err != nil || deleted.State != workspace.SyncCompleted {
		t.Fatalf("commit Host deletion = %#v, %v", deleted, err)
	}
	if _, err := store.SetWorkspaceBindingSyncStatus(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingSyncConflict, []string{file.Path}); err != nil {
		t.Fatal(err)
	}
	_, changed, err = store.BeginHostRelease(ctx, hostValue.ID, true)
	if changed || domain.CodeOf(err) != domain.CodeConflict || !errors.As(err, &scoped) ||
		scoped.Details["workspace_id"] != workspaceValue.ID {
		t.Fatalf("release during Host sync conflict: changed=%t err=%v", changed, err)
	}
	if _, err := store.SetWorkspaceBindingSyncStatus(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingSyncHealthy, nil); err != nil {
		t.Fatal(err)
	}
	stopping, changed, err := store.BeginHostRelease(ctx, hostValue.ID, true)
	if err != nil || !changed || stopping.State != host.StateStopping {
		t.Fatalf("release after Host upload and conflict settle = %#v, changed=%t, err=%v", stopping, changed, err)
	}
}

func TestHostSyncSessionCreationSerializesWithRelease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "release-sync-race")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{workspaceValue.ID})
	if err := store.MarkCreateAttempt(ctx, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, hostValue.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	binding, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, 1,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision)
	if err != nil {
		t.Fatal(err)
	}
	file := syncTestFile("raced.txt", []byte("save before release"))
	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", hostValue.ID, hostValue.RuntimeGeneration, workspaceValue.ID, binding.BindingVersion)
	blocks := syncTestBlocks(session.UploadFiles)
	if err := seedTestStoredWorkspaceBlockUsage(ctx, store, workspaceValue.ID, blocks[0].Chunk); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wait sync.WaitGroup
	var sessionErr, releaseErr error
	var released host.Host
	var changed bool
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, _, sessionErr = store.CreateWorkspaceSyncSession(ctx, session, blocks, nil, syncTestOwnerQuotaBytes,
			syncTestIdempotency(owner.ID, "workspace.sync.create", "release-sync-race:create", session.ID))
	}()
	go func() {
		defer wait.Done()
		<-start
		released, changed, releaseErr = store.BeginHostRelease(ctx, hostValue.ID, true)
	}()
	close(start)
	wait.Wait()
	switch {
	case sessionErr == nil:
		if changed || domain.CodeOf(releaseErr) != domain.CodeConflict {
			t.Fatalf("Host released after upload session was created: Host=%#v changed=%t err=%v", released, changed, releaseErr)
		}
		current, err := store.GetHost(ctx, hostValue.ID)
		if err != nil || current.State != host.StateRunning {
			t.Fatalf("Host after upload won release race = %#v, %v", current, err)
		}
	case releaseErr == nil && changed:
		if released.State != host.StateStopping || domain.CodeOf(sessionErr) != domain.CodeInvalidState {
			t.Fatalf("upload session created after release: Host=%#v session err=%v", released, sessionErr)
		}
	default:
		t.Fatalf("unexpected release/upload race: Host=%#v changed=%t release err=%v session err=%v", released, changed, releaseErr, sessionErr)
	}
}

func TestBeginHostReleaseRequiresConfirmationForCurrentTerminalSessions(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 40_000, 80_000)
	tests := []struct {
		name             string
		state            string
		generationOffset int64
		wantConflict     bool
	}{
		{name: "creating", state: "creating", wantConflict: true},
		{name: "ready", state: "ready", wantConflict: true},
		{name: "unknown", state: "unknown", wantConflict: true},
		{name: "interrupted", state: "interrupted"},
		{name: "abandoned", state: "abandoned"},
		{name: "previous generation", state: "ready", generationOffset: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
			if tt.generationOffset < 0 {
				if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_generation=2 WHERE id=$1`, hostValue.ID); err != nil {
					t.Fatal(err)
				}
				hostValue.RuntimeGeneration = 2
			}
			attemptID, err := domain.NewOperationID()
			if err != nil {
				t.Fatal(err)
			}
			sessionGeneration := hostValue.RuntimeGeneration + tt.generationOffset
			aioSessionID := ""
			if tt.state == "ready" {
				aioSessionID = "aio-session-123"
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO host_terminal_sessions
  (host_id, owner_id, actor_id, runtime_generation, control_version, attempt_id, aio_session_id, state)
VALUES ($1,$2,$2,$3,$4,$5,NULLIF($6,''),$7)`,
				hostValue.ID, owner.ID, sessionGeneration, hostValue.ControlVersion, attemptID, aioSessionID, tt.state); err != nil {
				t.Fatalf("insert terminal session: %v", err)
			}

			value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false)
			if tt.wantConflict {
				if domain.CodeOf(err) != domain.CodeConflict || changed {
					t.Fatalf("release without confirmation = %#v changed=%t err=%v; want conflict", value, changed, err)
				}
				stillRunning, getErr := store.GetHost(ctx, hostValue.ID)
				if getErr != nil || stillRunning.State != host.StateRunning {
					t.Fatalf("Host after refused release = %#v, %v", stillRunning, getErr)
				}
				value, changed, err = store.BeginHostRelease(ctx, hostValue.ID, true)
			}
			if err != nil || !changed || value.State != host.StateStopping {
				t.Fatalf("release after confirmation/past session = %#v changed=%t err=%v", value, changed, err)
			}
		})
	}
}

func TestBeginHostReleaseRequiresConfirmationForApplicationLaunchesAndMarksThemStopped(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 9, 40_000, 80_000)
	for _, status := range []string{"starting", "running", "stopping", "unknown"} {
		t.Run(status, func(t *testing.T) {
			hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
			if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-app-launch"); err != nil {
				t.Fatal(err)
			}
			hostValue.RuntimeInstanceID = "runtime-app-launch"
			launchID := "launch_" + status + "01"
			if _, err := pool.Exec(ctx, `
INSERT INTO host_application_launches
  (host_id, owner_id, launch_id, application_id, runtime_generation, runtime_instance_id, control_version, status)
VALUES ($1,$2,$3,'wechat',$4,$5,$6,$7)`,
				hostValue.ID, owner.ID, launchID, hostValue.RuntimeGeneration, hostValue.RuntimeInstanceID, hostValue.ControlVersion, status); err != nil {
				t.Fatalf("insert application launch: %v", err)
			}
			value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false)
			if domain.CodeOf(err) != domain.CodeConflict || changed {
				t.Fatalf("release without explicit confirmation = %#v, changed=%t, err=%v", value, changed, err)
			}
			stillRunning, err := store.GetHost(ctx, hostValue.ID)
			if err != nil || stillRunning.State != host.StateRunning {
				t.Fatalf("release without explicit confirmation changed Host state = %#v, err=%v", stillRunning, err)
			}
			value, changed, err = store.BeginHostRelease(ctx, hostValue.ID, true)
			if err != nil || !changed || value.State != host.StateStopping {
				t.Fatalf("confirmed release = %#v, changed=%t, err=%v", value, changed, err)
			}
			value, err = store.CompleteHostRelease(ctx, hostValue.ID, hostValue.RuntimeGeneration)
			if err != nil || value.State != host.StateStopped {
				t.Fatalf("complete release = %#v, err=%v", value, err)
			}
			var storedStatus string
			if err := pool.QueryRow(ctx, `SELECT status FROM host_application_launches WHERE host_id=$1 AND launch_id=$2`, hostValue.ID, launchID).Scan(&storedStatus); err != nil || storedStatus != "stopped" {
				t.Fatalf("application status after confirmed sandbox deletion = %q, err=%v", storedStatus, err)
			}
		})
	}
}

func TestCompleteHostReleaseInterruptsOnlyCurrentActiveTerminalSessions(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 11, 40_000, 80_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_generation=2 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatalf("prepare previous terminal-session generation: %v", err)
	}
	hostValue.RuntimeGeneration = 2
	tests := []struct {
		name         string
		actorID      string
		generation   int64
		state        string
		aioSessionID string
		wantState    string
		wantSession  string
	}{
		{name: "creating", actorID: owner.ID, generation: hostValue.RuntimeGeneration, state: "creating", wantState: "interrupted"},
		{name: "ready", actorID: createTestUser(t, store, 12, 40_000, 80_000).ID, generation: hostValue.RuntimeGeneration, state: "ready", aioSessionID: "aio-session-ready", wantState: "interrupted"},
		{name: "unknown", actorID: createTestUser(t, store, 13, 40_000, 80_000).ID, generation: hostValue.RuntimeGeneration, state: "unknown", wantState: "interrupted"},
		{name: "already interrupted", actorID: createTestUser(t, store, 14, 40_000, 80_000).ID, generation: hostValue.RuntimeGeneration, state: "interrupted", wantState: "interrupted"},
		{name: "abandoned", actorID: createTestUser(t, store, 15, 40_000, 80_000).ID, generation: hostValue.RuntimeGeneration, state: "abandoned", wantState: "abandoned"},
		{name: "previous generation", actorID: createTestUser(t, store, 16, 40_000, 80_000).ID, generation: hostValue.RuntimeGeneration - 1, state: "ready", aioSessionID: "aio-session-old", wantState: "ready", wantSession: "aio-session-old"},
	}
	for i, tt := range tests {
		attemptID := fmt.Sprintf("o%07d", i+1)
		if _, err := pool.Exec(ctx, `
INSERT INTO host_terminal_sessions
  (host_id, owner_id, actor_id, runtime_generation, control_version, attempt_id, aio_session_id, state)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`,
			hostValue.ID, owner.ID, tt.actorID, tt.generation, hostValue.ControlVersion, attemptID, tt.aioSessionID, tt.state); err != nil {
			t.Fatalf("insert %s terminal session: %v", tt.name, err)
		}
	}
	if _, _, err := store.BeginHostRelease(ctx, hostValue.ID, true); err != nil {
		t.Fatalf("begin Host release: %v", err)
	}
	if _, err := store.CompleteHostRelease(ctx, hostValue.ID, hostValue.RuntimeGeneration); err != nil {
		t.Fatalf("complete Host release: %v", err)
	}
	for i, tt := range tests {
		attemptID := fmt.Sprintf("o%07d", i+1)
		var state, aioSessionID string
		if err := pool.QueryRow(ctx, `
SELECT state, COALESCE(aio_session_id, '')
FROM host_terminal_sessions WHERE host_id=$1 AND actor_id=$2 AND attempt_id=$3`, hostValue.ID, tt.actorID, attemptID).
			Scan(&state, &aioSessionID); err != nil {
			t.Fatalf("read %s terminal session: %v", tt.name, err)
		}
		if state != tt.wantState || aioSessionID != tt.wantSession {
			t.Errorf("%s session after release = state %q, id %q; want state %q, id %q", tt.name, state, aioSessionID, tt.wantState, tt.wantSession)
		}
	}
}

func TestApplicationLaunchReservationIsHostFencedAndIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 40_000, 80_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-app-launch"); err != nil {
		t.Fatal(err)
	}
	hostValue.RuntimeInstanceID = "runtime-app-launch"
	request := app.ApplicationLaunch{
		HostID: hostValue.ID, OwnerID: owner.ID, ID: "launch_replay01", ApplicationID: app.WechatApplicationID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, Status: app.ApplicationLaunchStarting,
	}
	first, created, err := store.ReserveApplicationLaunch(ctx, request)
	if err != nil || !created || first.Status != app.ApplicationLaunchStarting || first.CreatedAt.IsZero() {
		t.Fatalf("first reservation = %#v, created=%t, err=%v", first, created, err)
	}
	latest, err := store.GetLatestApplicationLaunch(ctx, hostValue.ID, owner.ID, app.WechatApplicationID)
	if err != nil || latest.ID != request.ID {
		t.Fatalf("latest application launch by app ID = %#v, err=%v", latest, err)
	}
	loaded, err := store.GetApplicationLaunch(ctx, hostValue.ID, owner.ID, app.WechatApplicationID, request.ID)
	if err != nil || loaded.ID != request.ID {
		t.Fatalf("specific application launch by app ID = %#v, err=%v", loaded, err)
	}
	replay, created, err := store.ReserveApplicationLaunch(ctx, request)
	if err != nil || created || replay.ID != first.ID || replay.CreatedAt != first.CreatedAt {
		t.Fatalf("same-ID reservation replay = %#v, created=%t, err=%v", replay, created, err)
	}
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, hostValue.RuntimeInstanceID, "runtime-app-launch-restarted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_version=control_version+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	changedRuntimeFence := request
	changedRuntimeFence.RuntimeInstanceID = "runtime-app-launch-restarted"
	changedRuntimeFence.ControlVersion++
	replayAfterRestart, created, err := store.ReserveApplicationLaunch(ctx, changedRuntimeFence)
	if err != nil || created || replayAfterRestart.ID != first.ID || replayAfterRestart.RuntimeInstanceID != request.RuntimeInstanceID ||
		replayAfterRestart.ControlVersion != request.ControlVersion {
		t.Fatalf("same-ID replay after Runtime restart lost original fence = %#v created=%t err=%v", replayAfterRestart, created, err)
	}
	proven := first
	proven.Status = app.ApplicationLaunchRunning
	updated, err := store.UpdateApplicationLaunch(ctx, proven)
	if err != nil || updated.Status != app.ApplicationLaunchRunning || updated.RuntimeInstanceID != request.RuntimeInstanceID || updated.ControlVersion != request.ControlVersion {
		t.Fatalf("Runtime proof after restart/control version change = %#v err=%v", updated, err)
	}
	secondID := request
	secondID.ID = "launch_parallel01"
	if _, created, err := store.ReserveApplicationLaunch(ctx, secondID); domain.CodeOf(err) != domain.CodeConflict || created {
		t.Fatalf("different launch while first is running = created %t err %v", created, err)
	}
	stopped := updated
	stopped.Status = app.ApplicationLaunchStopped
	if _, err := store.UpdateApplicationLaunch(ctx, stopped); err != nil {
		t.Fatalf("mark first launch stopped: %v", err)
	}
	if _, _, err := store.ReserveApplicationLaunch(ctx, app.ApplicationLaunch{
		HostID: hostValue.ID, OwnerID: owner.ID, ID: "launch_stale001", ApplicationID: app.WechatApplicationID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: hostValue.RuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion + 1, Status: app.ApplicationLaunchStarting,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new reservation with a stale fence error = %v", err)
	}
	secondID.RuntimeInstanceID, secondID.ControlVersion = changedRuntimeFence.RuntimeInstanceID, changedRuntimeFence.ControlVersion
	if _, created, err := store.ReserveApplicationLaunch(ctx, secondID); err != nil || !created {
		t.Fatalf("new launch after prior operation stopped = created %t err %v", created, err)
	}
	wrongGeneration := request
	wrongGeneration.RuntimeGeneration++
	if _, _, err := store.ReserveApplicationLaunch(ctx, wrongGeneration); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("same-ID reservation with a different Host generation error = %v", err)
	}
}

func TestApplicationLaunchReservationSerializesWithHostRelease(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 200_000, 400_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-app-launch-race"); err != nil {
		t.Fatal(err)
	}
	request := app.ApplicationLaunch{
		HostID: hostValue.ID, OwnerID: owner.ID, ID: "launch_race-check1", ApplicationID: app.WechatApplicationID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "runtime-app-launch-race",
		ControlVersion: hostValue.ControlVersion, Status: app.ApplicationLaunchStarting,
	}
	start := make(chan struct{})
	reserveResult := make(chan struct {
		value   app.ApplicationLaunch
		created bool
		err     error
	}, 1)
	releaseResult := make(chan struct {
		value   host.Host
		changed bool
		err     error
	}, 1)
	go func() {
		<-start
		value, created, err := store.ReserveApplicationLaunch(ctx, request)
		reserveResult <- struct {
			value   app.ApplicationLaunch
			created bool
			err     error
		}{value, created, err}
	}()
	go func() {
		<-start
		value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false)
		releaseResult <- struct {
			value   host.Host
			changed bool
			err     error
		}{value, changed, err}
	}()
	close(start)
	reserved := <-reserveResult
	released := <-releaseResult
	current, err := store.GetHost(ctx, hostValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.err == nil {
		if !reserved.created {
			t.Fatalf("launch reservation was not new: %#v", reserved)
		}
		if domain.CodeOf(released.err) != domain.CodeConflict || released.changed || released.value.State != host.StateRunning || current.State != host.StateRunning {
			t.Fatalf("release crossed an active launch reservation: release=%#v, current=%#v", released, current)
		}
		return
	}
	if domain.CodeOf(reserved.err) != domain.CodeInvalidState || !released.changed || released.err != nil ||
		released.value.State != host.StateStopping || current.State != host.StateStopping {
		t.Fatalf("launch/release race did not serialize on Host state: reserve=%#v release=%#v current=%#v", reserved, released, current)
	}
}

func TestBeginHostReleaseBlocksMCPRuntimeOperations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "release-mcp-runtime"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	fence := app.AgentHostFence{
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: runtimeInstanceID,
		ControlVersion: hostValue.ControlVersion, AIAllowed: true,
	}
	key := app.MCPRuntimeOperationKey{
		OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "release-session", HostID: hostValue.ID,
		IdempotencyKey: "active-runtime-call",
	}
	operation, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil || replayed || operation.Status != task.OperationPrepared {
		t.Fatalf("begin MCP runtime operation = %#v, replayed=%t, err=%v", operation, replayed, err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release with prepared MCP operation = %#v, %t, %v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostValue.ID, false)
	operation, err = store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, fence, operation.LeaseVersion)
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("dispatch MCP runtime operation = %#v, err=%v", operation, err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release with dispatched MCP operation = %#v, %t, %v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostValue.ID, false)
	operation, err = store.SettleMCPRuntimeOperation(ctx, key, operation.RuntimeTaskID, task.OperationUnknown, json.RawMessage(`{"outcome":"unknown"}`))
	if err != nil || operation.Status != task.OperationUnknown {
		t.Fatalf("mark MCP runtime operation unknown = %#v, err=%v", operation, err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false); err != nil || changed || value.State != host.StateRunning {
		t.Fatalf("release with unknown MCP operation = %#v, %t, %v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostValue.ID, false)
	if _, err := store.SettleMCPRuntimeOperation(ctx, key, operation.RuntimeTaskID, task.OperationSucceeded, json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	if value, changed, err := store.BeginHostRelease(ctx, hostValue.ID, false); err != nil || !changed || value.State != host.StateStopping {
		t.Fatalf("release after MCP operation completed = %#v, %t, %v", value, changed, err)
	}
	assertHostSiteDrainState(t, store, ctx, hostValue.ID, true)
}
