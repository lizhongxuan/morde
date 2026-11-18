package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

func TestPostgresAgentWorkspaceIsStableAndProtectedFromDeleteAndTransfer(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	target := createTestUser(t, store, 3, 10_000, 20_000)

	legacy := createTestWorkspace(t, ctx, store, owner.ID, "Agent Workspace")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET name='Agent Workspace' WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	first := newAgentWorkspaceCandidate(t, owner.ID)
	stable, _, err := store.CreateAgentWorkspace(ctx, first)
	if err != nil {
		t.Fatalf("create Agent Workspace: %v", err)
	}
	if err := store.MarkAgentWorkspaceDirectoryInitialized(ctx, owner.ID, stable.ID); err != nil {
		t.Fatalf("mark Agent Workspace initialized: %v", err)
	}
	retry, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil || retry.ID != stable.ID {
		t.Fatalf("retry Agent Workspace = %#v, %v; want stable ID %s", retry, err, stable.ID)
	}
	resolved, initialized, err := store.GetAgentWorkspace(ctx, owner.ID)
	if err != nil || resolved.ID != stable.ID || resolved.OwnerID != owner.ID {
		t.Fatalf("GetAgentWorkspace() = %#v, %v", resolved, err)
	}
	if !initialized {
		t.Fatal("Agent Workspace directory initialization marker was not retained")
	}
	if legacy.ID == stable.ID {
		t.Fatal("same-named existing Workspace was adopted as the Agent Workspace")
	}
	other, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, target.ID))
	if err != nil || other.ID == stable.ID {
		t.Fatalf("another owner's Agent Workspace = %#v, %v; want independent ID", other, err)
	}

	if _, err := store.ClaimWorkspaceForDelete(ctx, stable.ID, stable.Revision); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("claim Agent Workspace for deletion = %v, want explanatory Workspace conflict", err)
	}
	if _, _, err := newTestWorkspaceOwnershipTransferService(t, store, 1<<30).RequestWorkspaceTransfer(ctx,
		workspaceTransferPrincipal(owner.ID), stable.ID, target.ID, stable.Revision, "agent-transfer-01"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("request Agent Workspace transfer = %v, want explanatory Workspace conflict", err)
	}
	current, err := store.GetWorkspace(ctx, stable.ID)
	if err != nil || current.State != workspace.StateReady || current.OwnerID != owner.ID {
		t.Fatalf("Agent Workspace changed after rejected mutations: %#v, %v", current, err)
	}
	if _, err := store.ClaimWorkspaceForDelete(ctx, legacy.ID, legacy.Revision); err != nil {
		t.Fatalf("ordinary Workspace explicit delete claim = %v", err)
	}
	var mappings int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM user_agent_workspaces WHERE owner_id=$1 AND workspace_id=$2`, owner.ID, stable.ID).Scan(&mappings); err != nil || mappings != 1 {
		t.Fatalf("stable owner mapping count = %d, %v; want one", mappings, err)
	}
	if _, err := store.ClaimAgentWorkspaceForDelete(ctx, target.ID, stable.ID, stable.Revision); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign owner Agent Workspace delete claim = %v, want not found", err)
	}
	claim, err := store.ClaimAgentWorkspaceForDelete(ctx, owner.ID, stable.ID, stable.Revision)
	if err != nil {
		t.Fatalf("owner explicit Agent Workspace delete claim: %v", err)
	}
	deleted, err := store.CompleteAgentWorkspaceDelete(ctx, owner.ID, stable.ID, claim.Revision)
	if err != nil || deleted.State != workspace.StateDeleted {
		t.Fatalf("owner explicit Agent Workspace delete completion = %#v, %v", deleted, err)
	}
	if _, _, err := store.GetAgentWorkspace(ctx, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("GetAgentWorkspace() after explicit delete = %v, want no current mapping", err)
	}
	recreated, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil || recreated.ID == stable.ID {
		t.Fatalf("new Agent Workspace after explicit deletion = %#v, %v", recreated, err)
	}
	isRetiredAgentWorkspace, err := store.IsAgentWorkspaceWorkspace(ctx, owner.ID, stable.ID)
	if err != nil || !isRetiredAgentWorkspace {
		t.Fatalf("retired Agent Workspace history lookup = %t, %v; want true", isRetiredAgentWorkspace, err)
	}
}

func TestPostgresAdoptExistingWorkspaceAsAgentWorkspace(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	current, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, current.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAgentWorkspaceDirectoryInitialized(ctx, owner.ID, current.ID); err != nil {
		t.Fatal(err)
	}
	source := createTestWorkspace(t, ctx, store, owner.ID, "Legacy Host Workspace")
	markWorkspaceAsAuditedLegacySource(t, ctx, store, source.ID)
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, source.ID, 0); err != nil {
		t.Fatal(err)
	}
	legacyHost := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 10_000, MemMiB: 20_000}, []string{source.ID})
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, legacyHost.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET state='active',desired_revision=0,applied_revision=0,
sync_state='healthy',sync_conflicts='{}' WHERE host_id=$1 AND workspace_id=$2`, legacyHost.ID, source.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.AdoptExistingWorkspaceAsAgentWorkspace(ctx, owner.ID, source.ID, 0, 0); err != nil {
		t.Fatalf("adopt existing Workspace: %v", err)
	}
	resolved, initialized, err := store.GetAgentWorkspace(ctx, owner.ID)
	if err != nil || resolved.ID != source.ID || !initialized {
		t.Fatalf("Agent Workspace after adoption = %#v initialized=%t err=%v", resolved, initialized, err)
	}
	var historyCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM agent_workspace_history WHERE owner_id=$1 AND workspace_id IN ($2,$3)`,
		owner.ID, current.ID, source.ID).Scan(&historyCount); err != nil || historyCount != 2 {
		t.Fatalf("adopted and retired Agent Workspace history count = %d, %v; want 2", historyCount, err)
	}
	bindings, err := store.GetWorkspaceBindingsByHost(ctx, legacyHost.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Workspace.ID != source.ID || bindings[0].State != workspace.BindingActive ||
		bindings[0].DesiredRevision != 0 || bindings[0].AppliedRevision != 0 || bindings[0].BindingVersion != 2 {
		t.Fatalf("stopped source Host binding after adoption = %#v, %v; want it retained on adopted Agent Workspace", bindings, err)
	}
	if err := store.AdoptExistingWorkspaceAsAgentWorkspace(ctx, owner.ID, source.ID, 0, 0); err != nil {
		t.Fatalf("repeat Agent Workspace adoption: %v", err)
	}
}

func TestPostgresAgentWorkspaceAdoptionFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name               string
		bindSource         bool
		hostState          string
		stoppedBinding     bool
		bindingConflict    bool
		bindingPendingSync bool
		volumeState        string
		importState        string
		omitImportAudit    bool
		expectedRevision   int64
		wantCode           domain.ErrorCode
	}{
		{name: "creating host", bindSource: true, expectedRevision: 0, wantCode: domain.CodeWorkspaceBusy},
		{name: "failed host", bindSource: true, hostState: "failed", expectedRevision: 0, wantCode: domain.CodeWorkspaceBusy},
		{name: "source binding has unresolved sync conflict", bindSource: true, stoppedBinding: true, bindingConflict: true, expectedRevision: 0, wantCode: domain.CodeWorkspaceConflict},
		{name: "source binding sync is unsettled", bindSource: true, stoppedBinding: true, bindingPendingSync: true, expectedRevision: 0, wantCode: domain.CodeWorkspaceBusy},
		{name: "stale source revision", expectedRevision: 1, wantCode: domain.CodeWorkspaceConflict},
		{name: "pending volume", volumeState: "pending", expectedRevision: 0, wantCode: domain.CodeWorkspaceConflict},
		{name: "ready volume", volumeState: "ready", expectedRevision: 0, wantCode: domain.CodeWorkspaceConflict},
		{name: "pending legacy import", importState: "pending", expectedRevision: 0, wantCode: domain.CodeWorkspaceConflict},
		{name: "missing legacy import audit", omitImportAudit: true, expectedRevision: 0, wantCode: domain.CodeWorkspaceConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := newTemporaryStore(t)
			ctx := context.Background()
			owner := createTestUser(t, store, 3, 10_000, 20_000)
			source := createTestWorkspace(t, ctx, store, owner.ID, "Legacy Host Workspace")
			markWorkspaceAsAuditedLegacySource(t, ctx, store, source.ID)
			if test.importState != "" {
				if _, err := store.pool.Exec(ctx, `UPDATE workspace_legacy_volumes SET import_state=$2 WHERE workspace_id=$1`, source.ID, test.importState); err != nil {
					t.Fatal(err)
				}
			}
			if test.omitImportAudit {
				if _, err := store.pool.Exec(ctx, `DELETE FROM workspace_legacy_volumes WHERE workspace_id=$1`, source.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.volumeState != "" && test.volumeState != "legacy" {
				if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state=$2,
volume_name=CASE WHEN $2='legacy' THEN NULL ELSE 'workspace-' || workspace_id END,
ready_at=CASE WHEN $2='ready' THEN clock_timestamp() ELSE NULL END WHERE workspace_id=$1`, source.ID, test.volumeState); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.MarkWorkspaceCurrentMaterialized(ctx, source.ID, 0); err != nil {
				t.Fatal(err)
			}
			if test.bindSource {
				hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 10_000, MemMiB: 20_000}, []string{source.ID})
				if test.hostState != "" {
					if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state=$2 WHERE id=$1`, hostValue.ID, test.hostState); err != nil {
						t.Fatal(err)
					}
				}
				if test.stoppedBinding {
					if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, hostValue.ID); err != nil {
						t.Fatal(err)
					}
					syncState, syncConflicts := "healthy", []string{}
					if test.bindingConflict {
						syncState, syncConflicts = "conflict", []string{"file.txt"}
					}
					desiredRevision := int64(0)
					if test.bindingPendingSync {
						desiredRevision = 1
					}
					if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET state='active',sync_state=$3,sync_conflicts=$4,desired_revision=$5
WHERE host_id=$1 AND workspace_id=$2`, hostValue.ID, source.ID, syncState, syncConflicts, desiredRevision); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.AdoptExistingWorkspaceAsAgentWorkspace(ctx, owner.ID, source.ID, test.expectedRevision, 0); domain.CodeOf(err) != test.wantCode {
				t.Fatalf("adoption with %s error = %v, want code %q", test.name, err, test.wantCode)
			}
			if _, _, err := store.GetAgentWorkspace(ctx, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
				t.Fatalf("Agent Workspace mapping after rejected adoption = %v, want no mapping", err)
			}
		})
	}
}

func markWorkspaceAsAuditedLegacySource(t *testing.T, ctx context.Context, store *Store, workspaceID string) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_volumes SET state='legacy',volume_name=NULL,ready_at=NULL WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes(workspace_id,volume_name,import_state)
VALUES ($1,$2,'not_required') ON CONFLICT (workspace_id) DO UPDATE SET import_state='not_required'`, workspaceID, "legacy-"+workspaceID); err != nil {
		t.Fatal(err)
	}
}

func TestHostWorkspaceDeletionKeepsAgentWorkspaceDirectoryAndContents(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	agent, _, err := store.CreateAgentWorkspace(ctx, newAgentWorkspaceCandidate(t, owner.ID))
	if err != nil {
		t.Fatal(err)
	}

	currentRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current, err := workspacecurrent.Open(currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	rootMetadata := []workspace.Directory{{Path: ".", MetadataKnown: true}}
	if err := current.Materialize(ctx, owner.ID, agent.ID, 0, nil, nil, rootMetadata, workspaceTransferTestBlocks{}); err != nil {
		t.Fatalf("materialize Agent Workspace current directory: %v", err)
	}
	if err := store.MarkAgentWorkspaceDirectoryInitialized(ctx, owner.ID, agent.ID); err != nil {
		t.Fatalf("mark Agent Workspace directory initialized: %v", err)
	}
	ownerHash := sha256.Sum256([]byte(owner.ID))
	agentDirectory := filepath.Join(currentRoot, hex.EncodeToString(ownerHash[:]), agent.ID)
	sentinel := filepath.Join(agentDirectory, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("agent data"), 0600); err != nil {
		t.Fatalf("write Agent Workspace sentinel: %v", err)
	}

	hostWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "Host computer storage")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 10_000, MemMiB: 20_000}, []string{hostWorkspace.ID})
	hostValue, err = store.TransitionHost(ctx, hostValue.ID, host.StateCreating, host.StateRunning, "")
	if err != nil {
		t.Fatalf("mark Host running: %v", err)
	}
	binding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, hostWorkspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, hostWorkspace.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate Host Workspace binding: %v", err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, hostWorkspace.ID, binding.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, 0, 0)
	if err != nil {
		t.Fatalf("begin Host Workspace detach: %v", err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, hostWorkspace.ID, binding.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, 0, 0); err != nil {
		t.Fatalf("detach Host Workspace: %v", err)
	}
	if err := store.MarkWorkspaceVolumeReady(ctx, owner.ID, hostWorkspace.ID, app.WorkspaceNamedVolumeName(hostWorkspace.ID)); err != nil {
		t.Fatalf("mark Host Workspace volume ready: %v", err)
	}
	volumes, err := app.NewWorkspaceVolumeService(store, agentWorkspaceVolumeStub{}, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewWorkspaceService(store, agentWorkspaceContentStub{}, agentWorkspaceCurrentDeleter{current}, agentWorkspaceCapacityStub{}, agentWorkspaceAuditStub{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetHostWorkspaceVolumes(volumes); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteHostWorkspace(ctx, owner.ID, hostValue.ID); err != nil {
		t.Fatalf("delete Host-specific Workspace: %v", err)
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "agent data" {
		t.Fatalf("Agent Workspace file after Host Workspace deletion = %q, %v", contents, err)
	}
	resolved, initialized, err := store.GetAgentWorkspace(ctx, owner.ID)
	if err != nil || resolved.ID != agent.ID {
		t.Fatalf("Agent Workspace mapping after Host deletion = %#v, %v", resolved, err)
	}
	if !initialized {
		t.Fatal("Agent Workspace directory initialization marker changed during Host-specific Workspace deletion")
	}
	if _, err := os.Stat(agentDirectory); err != nil {
		t.Fatalf("Agent Workspace directory was removed by Host deletion: %v", err)
	}
}

func newAgentWorkspaceCandidate(t *testing.T, ownerID string) workspace.Workspace {
	t.Helper()
	id, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	return workspace.Workspace{ID: id, OwnerID: ownerID, Name: "Agent Workspace", State: workspace.StateReady, QuotaBytes: 2_048 << 20}
}

type agentWorkspaceContentStub struct{}

func (agentWorkspaceContentStub) DeleteWorkspace(string) error { return nil }

type agentWorkspaceCurrentDeleter struct{ current *workspacecurrent.Store }

func (s agentWorkspaceCurrentDeleter) Delete(ctx context.Context, ownerID, workspaceID string) error {
	return s.current.Delete(ctx, ownerID, workspaceID)
}

type agentWorkspaceCapacityStub struct{}

func (agentWorkspaceCapacityStub) CheckCapacity(context.Context) error { return nil }

type agentWorkspaceAuditStub struct{}

func (agentWorkspaceAuditStub) Write(context.Context, audit.Entry) error { return nil }

type agentWorkspaceVolumeStub struct{}

func (agentWorkspaceVolumeStub) EnsureWorkspaceVolume(context.Context, string, string, string, string) error {
	return nil
}
func (agentWorkspaceVolumeStub) VerifyWorkspaceVolume(context.Context, string, string, string, string) error {
	return nil
}
func (agentWorkspaceVolumeStub) VerifyWorkspaceVolumeForDelete(context.Context, string, string, string, string) error {
	return nil
}
func (agentWorkspaceVolumeStub) DeleteWorkspaceVolume(context.Context, string, string, string, string) error {
	return nil
}
