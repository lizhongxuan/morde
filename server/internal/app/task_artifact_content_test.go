package app

import (
	"context"
	"io"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type taskArtifactContentStoreStub struct {
	*taskStoreStub
	*syncServiceStore
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
