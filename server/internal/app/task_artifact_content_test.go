package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskArtifactContentStoreStub struct {
	*taskStoreStub
	*syncServiceStore
}

type taskValidationReviewArtifactReaderStub struct {
	delegate TaskArtifactReader
	proofs   []TaskArtifactProof
	bodies   []*countingTaskArtifactBody
	err      error
}

type taskValidationReviewClaimChangingStore struct {
	*taskArtifactContentStoreStub
	currentChecks int
	validChecks   int
}

func (s *taskValidationReviewClaimChangingStore) IsCurrentTaskValidationReviewClaim(context.Context, TaskValidationReviewClaim) (bool, error) {
	s.currentChecks++
	return s.currentChecks <= s.validChecks, nil
}

func (s *taskValidationReviewArtifactReaderStub) OpenCommittedArtifact(ctx context.Context, proof TaskArtifactProof) (io.ReadSeekCloser, error) {
	s.proofs = append(s.proofs, proof)
	if s.err != nil {
		return nil, s.err
	}
	body, err := s.delegate.OpenCommittedArtifact(ctx, proof)
	if err != nil {
		return nil, err
	}
	counted := &countingTaskArtifactBody{ReadSeekCloser: body}
	s.bodies = append(s.bodies, counted)
	return counted, nil
}

type countingTaskArtifactBody struct {
	io.ReadSeekCloser
	reads int
}

func (b *countingTaskArtifactBody) Read(value []byte) (int, error) {
	b.reads++
	return b.ReadSeekCloser.Read(value)
}

func TestOpenTaskArtifactContentRequiresVisibleVerifiedArtifact(t *testing.T) {
	workspaces, workspaceStore, _, proof := proofFixture(t)
	artifact := task.Artifact{ID: "a1234567", TaskID: "q1234567", StepID: "s1234567", WorkspaceID: proof.WorkspaceID,
		Revision: proof.Revision, SizeBytes: proof.SizeBytes, SHA256: proof.SHA256, Label: "Design", Path: proof.Path,
		MediaType: "text/markdown; charset=utf-8"}
	store := &taskArtifactContentStoreStub{
		taskStoreStub: &taskStoreStub{details: TaskDetails{Task: task.Task{ID: artifact.TaskID, OwnerID: proof.OwnerID,
			InitiatorID: proof.OwnerID, HostID: "h1234567", WorkspaceID: proof.WorkspaceID,
			WorkspaceOwnerID: proof.OwnerID}, Artifacts: []task.Artifact{artifact}}},
		syncServiceStore: workspaceStore,
	}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetTaskArtifactReader(workspaces); err != nil {
		t.Fatal(err)
	}
	content, err := service.OpenTaskArtifactContent(context.Background(), taskTestPrincipal(), artifact.TaskID, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := content.Body.Close(); err != nil {
			t.Errorf("close task artifact body: %v", err)
		}
	}()
	body, err := io.ReadAll(content.Body)
	if err != nil || string(body) != "hello world" || content.Filename != "result.txt" ||
		content.MediaType != artifact.MediaType || content.SizeBytes != proof.SizeBytes || content.SHA256 != proof.SHA256 {
		t.Fatalf("task artifact content=%+v body=%q err=%v", content, body, err)
	}

	store.details.Artifacts[0].SHA256 = ""
	if _, err := service.OpenTaskArtifactContent(context.Background(), taskTestPrincipal(), artifact.TaskID, artifact.ID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unverified legacy artifact error = %v", err)
	}
	store.details.Artifacts[0] = artifact
	other := taskTestPrincipal()
	other.UserID = "u7654321"
	if _, err := service.OpenTaskArtifactContent(context.Background(), other, artifact.TaskID, artifact.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("artifact from an inaccessible Workspace was returned: %v", err)
	}
}

func taskValidationReviewArtifactFixture(t *testing.T) (*TaskService, *taskArtifactContentStoreStub,
	*taskValidationReviewArtifactReaderStub, TaskValidationReviewClaim, task.Artifact, TaskArtifactProof) {
	t.Helper()
	workspaces, workspaceStore, _, proof := proofFixture(t)
	result := "Task output was written and read back."
	resultDigest := sha256.Sum256([]byte(result))
	taskRevision, resultSequence := int64(2), int64(9)
	artifact := task.Artifact{ID: "a1234567", TaskID: "q1234567", StepID: "s1234567", SourceCallID: "write-1",
		WorkspaceID: proof.WorkspaceID, Revision: proof.Revision, SizeBytes: proof.SizeBytes, SHA256: proof.SHA256,
		Label: "Output", Path: proof.Path, MediaType: "text/plain; charset=utf-8"}
	step := task.Step{ID: artifact.StepID, TaskID: artifact.TaskID, Sequence: 5, Kind: "tool", Status: task.StepSucceeded,
		TaskRevision: &taskRevision, ToolCallKey: artifact.SourceCallID, ToolName: "morde_workspace_write"}
	details := TaskDetails{
		Task: task.Task{ID: artifact.TaskID, OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
			HostID: "h1234567", WorkspaceID: proof.WorkspaceID, WorkspaceOwnerID: "u1234567", Status: task.StatusCompleted,
			CurrentRevision: &taskRevision, DesiredRevision: &taskRevision, Result: result},
		Steps: []task.Step{step}, Artifacts: []task.Artifact{artifact},
		ValidationEvidence: &TaskValidationEvidenceRecord{TaskRevision: taskRevision, EventSequence: resultSequence,
			ResultSHA256: hex.EncodeToString(resultDigest[:]), Claims: []TaskValidationEvidenceClaimRecord{{ID: "claim-1",
				EvidenceRefs: []TaskValidationEvidenceRefRecord{{Kind: TaskValidationEvidenceSideEffect, ID: artifact.ID,
					TaskRevision: taskRevision, Sequence: step.Sequence, CallID: artifact.SourceCallID}},
			}}},
	}
	store := &taskArtifactContentStoreStub{taskStoreStub: &taskStoreStub{details: details}, syncServiceStore: workspaceStore}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	reader := &taskValidationReviewArtifactReaderStub{delegate: workspaces}
	if err := service.SetTaskArtifactReader(reader); err != nil {
		t.Fatal(err)
	}
	claim := TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: artifact.TaskID, TaskRevision: taskRevision, ResultEventSequence: resultSequence,
		ResultSHA256: append([]byte(nil), resultDigest[:]...), ClaimGeneration: 1, LeaseOwner: "review-worker",
		LeaseExpiresAt: time.Now().Add(time.Minute)}
	return service, store, reader, claim, artifact, proof
}

func TestOpenTaskValidationReviewArtifactContentReadsCurrentEvidenceBoundProof(t *testing.T) {
	service, _, reader, claim, artifact, proof := taskValidationReviewArtifactFixture(t)
	content, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := content.Body.Close(); err != nil {
			t.Errorf("close review artifact body: %v", err)
		}
	}()
	expectedProof := TaskArtifactProof{OwnerID: claim.OwnerID, WorkspaceID: proof.WorkspaceID, Revision: proof.Revision,
		Path: proof.Path, SizeBytes: proof.SizeBytes, SHA256: proof.SHA256}
	if len(reader.proofs) != 1 || reader.proofs[0] != expectedProof || len(reader.bodies) != 1 || reader.bodies[0].reads != 0 {
		t.Fatalf("reader was not given the exact pinned proof or body was read early: proofs=%+v bodies=%+v", reader.proofs, reader.bodies)
	}
	body, err := io.ReadAll(content.Body)
	if err != nil || string(body) != "hello world" || content.Filename != "result.txt" ||
		content.MediaType != artifact.MediaType || content.SizeBytes != proof.SizeBytes || content.SHA256 != proof.SHA256 {
		t.Fatalf("review artifact content=%+v body=%q err=%v", content, body, err)
	}
}

func TestOpenTaskValidationReviewArtifactContentRequiresExactEvidenceBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TaskDetails)
		want   domain.ErrorCode
	}{
		{name: "missing evidence", mutate: func(details *TaskDetails) { details.ValidationEvidence = nil }, want: domain.CodeNotFound},
		{name: "artifact not referenced", mutate: func(details *TaskDetails) { details.ValidationEvidence.Claims = nil }, want: domain.CodeNotFound},
		{name: "source call mismatch", mutate: func(details *TaskDetails) { details.ValidationEvidence.Claims[0].EvidenceRefs[0].CallID = "other-call" }, want: domain.CodeInvalidState},
		{name: "source step sequence mismatch", mutate: func(details *TaskDetails) { details.ValidationEvidence.Claims[0].EvidenceRefs[0].Sequence++ }, want: domain.CodeInvalidState},
		{name: "evidence task revision mismatch", mutate: func(details *TaskDetails) { details.ValidationEvidence.TaskRevision++ }, want: domain.CodeInvalidState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
			test.mutate(&store.details)
			if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != test.want {
				t.Fatalf("artifact read error = %v; want %s", err, test.want)
			}
			if len(reader.proofs) != 0 {
				t.Fatalf("unbound evidence reached artifact reader: %+v", reader.proofs)
			}
		})
	}
}

func TestOpenTaskValidationReviewArtifactContentRejectsExpiredOrReplacedClaim(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TaskValidationReviewClaim)
	}{
		{name: "expired lease", mutate: func(claim *TaskValidationReviewClaim) { claim.LeaseExpiresAt = time.Now().Add(-time.Second) }},
		{name: "replaced generation", mutate: func(claim *TaskValidationReviewClaim) { claim.ClaimGeneration++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
			current := false
			store.validationClaimCurrent = &current
			test.mutate(&claim)
			if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("stale review claim error = %v; want conflict", err)
			}
			if len(reader.proofs) != 0 {
				t.Fatalf("stale claim reached artifact reader: %+v", reader.proofs)
			}
		})
	}
}

func TestOpenTaskValidationReviewArtifactContentClosesBodyWhenClaimExpiresDuringRead(t *testing.T) {
	service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
	changingStore := &taskValidationReviewClaimChangingStore{taskArtifactContentStoreStub: store, validChecks: 2}
	service.store = changingStore
	if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("claim expiring during artifact read error = %v; want conflict", err)
	}
	if changingStore.currentChecks != 3 || len(reader.bodies) != 1 {
		t.Fatalf("claim was not rechecked after opening the pinned proof: checks=%d bodies=%d", changingStore.currentChecks, len(reader.bodies))
	}
	if _, err := reader.bodies[0].Read(make([]byte, 1)); err == nil {
		t.Fatal("artifact body remained readable after claim expiry")
	}
}

func TestOpenTaskValidationReviewArtifactContentFailsClosedWithoutClaimVerifier(t *testing.T) {
	service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
	service.store = taskValidationReviewArtifactStoreWithoutVerifier{TaskStore: store, syncServiceStore: store.syncServiceStore}
	if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("missing review claim verifier error = %v; want unsupported", err)
	}
	if len(reader.proofs) != 0 {
		t.Fatalf("unverified claim reached artifact reader: %+v", reader.proofs)
	}
}

type taskValidationReviewArtifactStoreWithoutVerifier struct {
	TaskStore
	*syncServiceStore
}

func TestOpenTaskValidationReviewArtifactContentRejectsTaskWorkspaceAccessAndVersionChanges(t *testing.T) {
	t.Run("task permission revoked", func(t *testing.T) {
		service, _, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
		service.executionAuthorizer.(*taskAuthorizerStub).hostErrors = map[string]error{
			"h1234567": domain.NewError(domain.CodeForbidden, "task access revoked"),
		}
		if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("revoked task access error = %v; want not found", err)
		}
		if len(reader.proofs) != 0 {
			t.Fatalf("revoked task access reached artifact reader: %+v", reader.proofs)
		}
	})
	t.Run("workspace not visible", func(t *testing.T) {
		service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
		store.workspace.OwnerID = "u7654321"
		if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("inaccessible Workspace error = %v; want not found", err)
		}
		if len(reader.proofs) != 0 {
			t.Fatalf("inaccessible Workspace reached artifact reader: %+v", reader.proofs)
		}
	})
	t.Run("task revision changed", func(t *testing.T) {
		service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
		newRevision := claim.TaskRevision + 1
		store.details.Task.CurrentRevision = &newRevision
		store.details.Task.DesiredRevision = &newRevision
		if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("stale Task revision error = %v; want conflict", err)
		}
		if len(reader.proofs) != 0 {
			t.Fatalf("stale Task revision reached artifact reader: %+v", reader.proofs)
		}
	})
}

func TestOpenTaskValidationReviewArtifactContentRejectsInvalidOrUncommittedProof(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*task.Artifact)
		want   domain.ErrorCode
	}{
		{name: "wrong Task", mutate: func(artifact *task.Artifact) { artifact.TaskID = "q7654321" }, want: domain.CodeInvalidState},
		{name: "wrong Workspace", mutate: func(artifact *task.Artifact) { artifact.WorkspaceID = "w7654321" }, want: domain.CodeInvalidState},
		{name: "unsafe path", mutate: func(artifact *task.Artifact) { artifact.Path = "../outside.txt" }, want: domain.CodeInvalidState},
		{name: "future Workspace revision", mutate: func(artifact *task.Artifact) { artifact.Revision++ }, want: domain.CodeWorkspaceConflict},
		{name: "wrong size", mutate: func(artifact *task.Artifact) { artifact.SizeBytes++ }, want: domain.CodeWorkspaceConflict},
		{name: "wrong digest", mutate: func(artifact *task.Artifact) { artifact.SHA256 = strings.Repeat("0", 64) }, want: domain.CodeWorkspaceConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, store, reader, claim, artifact, _ := taskValidationReviewArtifactFixture(t)
			test.mutate(&store.details.Artifacts[0])
			if _, err := service.OpenTaskValidationReviewArtifactContent(context.Background(), claim, artifact.ID); domain.CodeOf(err) != test.want {
				t.Fatalf("invalid artifact proof error = %v; want %s", err, test.want)
			}
			if len(reader.proofs) > 1 {
				t.Fatalf("artifact reader received repeated proofs: %+v", reader.proofs)
			}
		})
	}
}
