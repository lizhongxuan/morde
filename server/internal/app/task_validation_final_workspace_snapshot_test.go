package app

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

func TestRuntimeTaskValidationFinalWorkspaceSnapshotReaderBindsManifestToCheckFence(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	ctx := context.Background()
	current := fixture.store.details.Task
	current.ConversationID = "c1234567"
	current.Status = task.StatusCompleted
	taskRevision := int64(1)
	current.CurrentRevision, current.DesiredRevision = &taskRevision, &taskRevision

	session, err := fixture.executor.runtime.Connect(ctx, current.HostID, nil)
	if err != nil {
		t.Fatalf("connect Runtime: %v", err)
	}
	defer session.Close()
	bindingVersion, viewID, err := session.SetTaskWorkspaceView(ctx, current, current.WorkspaceID, 1)
	if err != nil || bindingVersion != 1 {
		t.Fatalf("prepare Task view = binding %d, view %q, err %v", bindingVersion, viewID, err)
	}
	privateRoot := filepath.Join(fixture.workspaceRoot, ".morde-task-views", current.ID, viewID, "workspace")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateRoot, "result.txt"), []byte("verified output"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkSnapshot, err := session.TaskWorkspaceManifestPage(ctx, current, bindingVersion, viewID, "", "", 1000)
	if err != nil {
		t.Fatalf("read check Workspace snapshot: %v", err)
	}
	if _, clearedView, err := session.SetTaskWorkspaceView(ctx, current, current.WorkspaceID, bindingVersion); err != nil || clearedView != viewID {
		t.Fatalf("clear check snapshot export fence = view %q, err %v", clearedView, err)
	}

	resultDigest := sha256Bytes([]byte("Task result"))
	claim := TaskValidationReviewClaim{OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		ConversationID: current.ConversationID, TaskID: current.ID, TaskRevision: taskRevision,
		ResultEventSequence: 4, ResultSHA256: resultDigest, ClaimGeneration: 1, LeaseOwner: "review-worker",
		LeaseExpiresAt: time.Now().Add(time.Minute).UTC()}
	replay := &TaskValidationReplayEvidence{PlanRevision: 1, Checks: []TaskValidationReplayCheckEvidence{{
		CheckID: "verify", TaskID: current.ID, Status: TaskValidationReplayPass, TaskRevision: taskRevision,
		PlanRevision: 1, HostID: current.HostID, HostRuntimeGeneration: session.Fence.RuntimeGeneration,
		HostRuntimeInstanceID: session.Fence.RuntimeInstanceID, HostControlVersion: session.Fence.ControlVersion,
		WorkspaceID: current.WorkspaceID, WorkspaceBindingVersion: bindingVersion,
		InputRevision: *current.InputRevision, InputManifestSHA256: hex.EncodeToString(current.ContextDigest),
		WorkspaceCheckpointRevision: *current.WorkspaceCheckpointRevision, WorkspaceViewID: viewID,
		WorkspaceManifestSHA256Before: checkSnapshot.ManifestSHA256,
		WorkspaceManifestSHA256After:  checkSnapshot.ManifestSHA256,
		WorkspaceSnapshotSHA256Before: checkSnapshot.WorkspaceSnapshotSHA256,
		WorkspaceSnapshotSHA256After:  checkSnapshot.WorkspaceSnapshotSHA256,
	}}}
	reader, err := NewRuntimeTaskValidationFinalWorkspaceSnapshotReader(fixture.executor.runtime)
	if err != nil {
		t.Fatal(err)
	}
	proof, release, err := reader.ReadTaskValidationFinalWorkspaceSnapshot(ctx, claim, current, replay)
	if err != nil || proof == nil || release == nil {
		t.Fatalf("read final Task Workspace snapshot proof = %+v, release=%t, err %v", proof, release != nil, err)
	}
	if _, err := session.TaskWorkspaceManifestPage(ctx, current, bindingVersion, viewID, "", "", 1000); err != nil {
		t.Fatalf("snapshot fence should remain active until review completion releases it: %v", err)
	}
	if proof.Claim.TaskID != claim.TaskID || proof.TaskID != current.ID || proof.TaskRevision != taskRevision ||
		proof.HostID != session.Fence.HostID || proof.RuntimeGeneration != session.Fence.RuntimeGeneration ||
		proof.RuntimeInstanceID != session.Fence.RuntimeInstanceID || proof.ControlVersion != session.Fence.ControlVersion ||
		proof.WorkspaceID != current.WorkspaceID || proof.BindingVersion != bindingVersion ||
		proof.InputRevision != *current.InputRevision || proof.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
		proof.InputManifestSHA256 != hex.EncodeToString(current.ContextDigest) || proof.ViewID != viewID ||
		proof.SnapshotSHA256 != checkSnapshot.WorkspaceSnapshotSHA256 {
		t.Fatalf("final Task Workspace proof = %+v; want the check's immutable Runtime snapshot", proof)
	}
	baseline := TaskValidationReplaySnapshotBaseline{OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		ConversationID: current.ConversationID, TaskID: current.ID, TaskRevision: taskRevision,
		HostID: session.Fence.HostID, HostRuntimeGeneration: session.Fence.RuntimeGeneration,
		HostRuntimeInstanceID: session.Fence.RuntimeInstanceID, HostControlVersion: session.Fence.ControlVersion,
		HostState: "running", HostControlState: "ai", WorkspaceID: current.WorkspaceID,
		InputRevision: *current.InputRevision, WorkspaceCheckpointRevision: *current.WorkspaceCheckpointRevision,
		InputManifestSHA256: hex.EncodeToString(current.ContextDigest)}
	if !TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, baseline, replay.Checks, proof) {
		t.Fatal("Runtime proof did not match its database-style Task and check baseline")
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release final Task Workspace snapshot fence: %v", err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release final Task Workspace snapshot fence twice: %v", err)
	}
	wrongManifest := *proof
	wrongManifest.SnapshotSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, baseline, replay.Checks, &wrongManifest) {
		t.Fatal("a different final manifest incorrectly matched the typed check")
	}
}
