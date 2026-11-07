package app

import (
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskMediaPreviewStoreStub struct {
	TaskMediaStagedObjectStore
	get func(context.Context, string, string, string, time.Time) (TaskMediaStagedObject, error)
}

func (s taskMediaPreviewStoreStub) GetTaskMediaPreviewForOwner(ctx context.Context, ownerID, taskID, operationID string, now time.Time) (TaskMediaStagedObject, error) {
	return s.get(ctx, ownerID, taskID, operationID, now)
}

type taskMediaPreviewContentStub struct {
	TaskMediaVerifiedContentStore
	open func(context.Context, string, string, string, TaskMediaStagedContent) (io.ReadCloser, error)
}

func (s taskMediaPreviewContentStub) OpenVerified(ctx context.Context, ownerID, taskID, operationID string, expected TaskMediaStagedContent) (io.ReadCloser, error) {
	return s.open(ctx, ownerID, taskID, operationID, expected)
}

func TestTaskMediaPreviewServiceOpensOnlyValidatedPendingPreview(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	digest := sha256.Sum256([]byte("png"))
	staged := TaskMediaStagedObject{TaskID: "q1234567", OwnerID: principal.UserID, OperationID: "o1234567",
		OperationStatus: string(task.OperationSucceeded), TokenState: "settled", MediaType: "image/png", SizeBytes: 3,
		SHA256: digest[:], ExpiresAt: time.Now().UTC().Add(time.Hour), State: TaskMediaPreviewPending, CleanupState: TaskMediaCleanupPending}
	var opened bool
	service, err := NewTaskMediaPreviewService(taskMediaPreviewStoreStub{get: func(_ context.Context, ownerID, taskID, operationID string, now time.Time) (TaskMediaStagedObject, error) {
		if ownerID != principal.UserID || taskID != staged.TaskID || operationID != staged.OperationID || now.IsZero() {
			t.Fatalf("unexpected owner-scoped preview lookup: %q %q %q", ownerID, taskID, operationID)
		}
		return staged, nil
	}}, taskMediaPreviewContentStub{open: func(_ context.Context, ownerID, taskID, operationID string, expected TaskMediaStagedContent) (io.ReadCloser, error) {
		opened = true
		if ownerID != principal.UserID || taskID != staged.TaskID || operationID != staged.OperationID {
			t.Fatalf("unexpected staged content identity: %q %q %q", ownerID, taskID, operationID)
		}
		if expected.MediaType != staged.MediaType || expected.SizeBytes != staged.SizeBytes || string(expected.SHA256) != string(staged.SHA256) {
			t.Fatalf("staged content integrity fence = %#v", expected)
		}
		return io.NopCloser(strings.NewReader("png")), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if service.GenerationReady() {
		t.Fatal("preview-only service advertised generation before provider, accounting, and save flow were assembled")
	}
	preview, err := service.OpenPreview(context.Background(), principal, staged.TaskID, staged.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := preview.Body.Close(); err != nil {
			t.Errorf("close preview body: %v", err)
		}
	})
	body, err := io.ReadAll(preview.Body)
	if err != nil || string(body) != "png" || preview.MediaType != "image/png" || preview.SizeBytes != 3 || !opened {
		t.Fatalf("unexpected preview: body=%q media=%q size=%d opened=%t err=%v", body, preview.MediaType, preview.SizeBytes, opened, err)
	}
}

func TestTaskMediaPreviewServiceRejectsInvalidMetadataBeforeOpeningContent(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	digest := sha256.Sum256([]byte("png"))
	valid := TaskMediaStagedObject{TaskID: "q1234567", OwnerID: principal.UserID, OperationID: "o1234567",
		OperationStatus: string(task.OperationSucceeded), TokenState: "settled", MediaType: "image/png", SizeBytes: 3,
		SHA256: digest[:], ExpiresAt: time.Now().UTC().Add(time.Hour), State: TaskMediaPreviewPending, CleanupState: TaskMediaCleanupPending}
	cases := map[string]func(*TaskMediaStagedObject){
		"wrong owner":          func(value *TaskMediaStagedObject) { value.OwnerID = "u7654321" },
		"unknown usage":        func(value *TaskMediaStagedObject) { value.TokenState = "unknown" },
		"unfinished operation": func(value *TaskMediaStagedObject) { value.OperationStatus = string(task.OperationDispatched) },
		"not pending":          func(value *TaskMediaStagedObject) { value.State = TaskMediaPreviewSaved },
		"expired":              func(value *TaskMediaStagedObject) { value.ExpiresAt = time.Now().UTC().Add(-time.Second) },
		"bad digest":           func(value *TaskMediaStagedObject) { value.SHA256 = nil },
		"unsupported type":     func(value *TaskMediaStagedObject) { value.MediaType = "application/octet-stream" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			staged := valid
			mutate(&staged)
			opened := false
			service, err := NewTaskMediaPreviewService(taskMediaPreviewStoreStub{get: func(context.Context, string, string, string, time.Time) (TaskMediaStagedObject, error) {
				return staged, nil
			}}, taskMediaPreviewContentStub{open: func(context.Context, string, string, string, TaskMediaStagedContent) (io.ReadCloser, error) {
				opened = true
				return io.NopCloser(strings.NewReader("png")), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.OpenPreview(context.Background(), principal, valid.TaskID, valid.OperationID); err == nil || opened {
				t.Fatalf("invalid preview was opened: opened=%t err=%v", opened, err)
			}
		})
	}
}
