package taskmedia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestStageOpenAndDelete(t *testing.T) {
	store, root := newStore(t)
	ctx := context.Background()
	const body = "generated image bytes"
	content, err := store.Stage(ctx, "u1234567", "q1234567", "o1234567", "IMAGE/PNG", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	if content.MediaType != "image/png" || content.SizeBytes != int64(len(body)) || !bytes.Equal(content.SHA256, sha256Bytes([]byte(body))) {
		t.Fatalf("Stage() content = %#v", content)
	}
	path := filepath.Join(root, "u1234567", "q1234567", "o1234567", "media")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("staged file mode = %04o, want 0600", info.Mode().Perm())
	}
	reader, err := store.OpenVerified(ctx, "u1234567", "q1234567", "o1234567", content)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(got) != body {
		t.Fatalf("read staged content = %q, %v, close %v", got, err, closeErr)
	}
	wrongDigest := content
	wrongDigest.SHA256 = sha256Bytes([]byte("wrong bytes"))
	if _, err := store.OpenVerified(ctx, "u1234567", "q1234567", "o1234567", wrongDigest); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Open() with a mismatched digest = %v, want ErrIntegrity", err)
	}
	if err := store.Delete(ctx, "u1234567", "q1234567", "o1234567"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "u1234567", "q1234567", "o1234567"); err != nil {
		t.Fatalf("repeat Delete() error = %v", err)
	}
	if _, err := store.Open(ctx, "u1234567", "q1234567", "o1234567"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open() after Delete() error = %v, want not-exist", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("repeat Close() error = %v", err)
	}
	if _, err := store.Open(ctx, "u1234567", "q1234567", "o1234567"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Open() after Close() error = %v, want ErrClosed", err)
	}
}

func TestStagedPreviewSurvivesStoreRestart(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	contentBytes := append([]byte(nil), encoded.Bytes()...)
	root := filepath.Join(t.TempDir(), "media")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	const ownerID, taskID, operationID = "u1234567", "q1234567", "o1234567"
	content, err := store.Stage(ctx, ownerID, taskID, operationID, "image/png", bytes.NewReader(contentBytes))
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() before restart = %v", err)
	}

	restarted, err := Open(root)
	if err != nil {
		t.Fatalf("Open() after restart = %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	metadata := app.TaskMediaStagedObject{
		TaskID: taskID, OwnerID: ownerID, OperationID: operationID,
		OperationStatus: string(task.OperationSucceeded), TokenState: "settled",
		MediaType: content.MediaType, SizeBytes: content.SizeBytes, SHA256: append([]byte(nil), content.SHA256...),
		ExpiresAt: time.Now().UTC().Add(time.Hour), State: app.TaskMediaPreviewPending, CleanupState: app.TaskMediaCleanupPending,
	}
	service, err := app.NewTaskMediaPreviewService(taskMediaPreviewMetadataStub{object: metadata}, restarted)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	preview, err := service.OpenPreview(ctx, principal, taskID, operationID)
	if err != nil {
		t.Fatalf("OpenPreview() after restart = %v", err)
	}
	defer func() {
		if err := preview.Body.Close(); err != nil {
			t.Errorf("close preview body: %v", err)
		}
	}()
	got, err := io.ReadAll(preview.Body)
	if err != nil || preview.MediaType != "image/png" || preview.SizeBytes != int64(len(contentBytes)) || !bytes.Equal(got, contentBytes) {
		t.Fatalf("restarted preview = type %q size %d bytes %d err %v", preview.MediaType, preview.SizeBytes, len(got), err)
	}
	if _, err := service.OpenPreview(ctx, identity.Principal{UserID: "u7654321", Role: identity.RoleUser,
		Kind: identity.KindSession, TokenID: "t7654321"}, taskID, operationID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign owner preview error = %v, want not found", err)
	}
}

type taskMediaPreviewMetadataStub struct {
	app.TaskMediaStagedObjectStore
	object app.TaskMediaStagedObject
}

func (s taskMediaPreviewMetadataStub) GetTaskMediaPreviewForOwner(ctx context.Context, ownerID, taskID, operationID string, now time.Time) (app.TaskMediaStagedObject, error) {
	if err := ctx.Err(); err != nil {
		return app.TaskMediaStagedObject{}, err
	}
	if ownerID != s.object.OwnerID || taskID != s.object.TaskID || operationID != s.object.OperationID || !s.object.ExpiresAt.After(now) {
		return app.TaskMediaStagedObject{}, domain.NewError(domain.CodeNotFound, "task media preview not found")
	}
	return s.object, nil
}

func TestStageRejectsInvalidInputAndExistingObject(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		ownerID   string
		taskID    string
		operation string
		mediaType string
		source    io.Reader
		want      error
	}{
		{name: "invalid owner", ownerID: "../user", taskID: "q1234567", operation: "o1234567", mediaType: "image/png", source: bytes.NewReader([]byte("x")), want: ErrInvalidIdentity},
		{name: "unsupported media", ownerID: "u1234567", taskID: "q1234567", operation: "o1234567", mediaType: "text/html", source: bytes.NewReader([]byte("x")), want: ErrInvalidMedia},
		{name: "media parameters", ownerID: "u1234567", taskID: "q1234567", operation: "o1234567", mediaType: "image/png; charset=binary", source: bytes.NewReader([]byte("x")), want: ErrInvalidMedia},
		{name: "nil source", ownerID: "u1234567", taskID: "q1234567", operation: "o1234567", mediaType: "image/png", want: ErrInvalidSource},
		{name: "empty source", ownerID: "u1234567", taskID: "q1234567", operation: "o1234567", mediaType: "image/png", source: bytes.NewReader(nil), want: ErrEmpty},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Stage(ctx, test.ownerID, test.taskID, test.operation, test.mediaType, test.source); !errors.Is(err, test.want) {
				t.Fatalf("Stage() error = %v, want %v", err, test.want)
			}
		})
	}
	if _, err := store.Stage(ctx, "u1234567", "q1234567", "o1234567", "image/png", bytes.NewBufferString("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stage(ctx, "u1234567", "q1234567", "o1234567", "image/png", bytes.NewBufferString("x")); err == nil {
		t.Fatal("Stage() reused an existing operation path")
	}
	if err := store.Delete(ctx, "u1234567", "q1234567", "o1234567"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "u1234567", "q1234567", "o7654321"); err != nil {
		t.Fatalf("Delete() for missing object = %v", err)
	}
}

func TestStageHonorsCancellation(t *testing.T) {
	store, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Stage(ctx, "u1234567", "q1234567", "o1234567", "image/png", bytes.NewBufferString("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage() with canceled context = %v, want context.Canceled", err)
	}
}

func TestCopyContentBoundsAndCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		body string
		ctx  context.Context
		want error
		size int64
	}{
		{name: "within limit", body: "1234", ctx: context.Background(), size: 4},
		{name: "over limit", body: "12345", ctx: context.Background(), want: ErrTooLarge},
		{name: "cancelled", body: "1234", ctx: cancelled, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			var destination bytes.Buffer
			content, err := copyContent(test.ctx, &destination, bytes.NewBufferString(test.body), 4)
			if !errors.Is(err, test.want) {
				t.Fatalf("copyContent() error = %v, want %v", err, test.want)
			}
			if err == nil && (content.SizeBytes != test.size || destination.String() != test.body) {
				t.Fatalf("copyContent() = %#v, %q", content, destination.String())
			}
		})
	}
	if _, err := copyContent(context.Background(), errorWriter{}, bytes.NewBufferString("x"), 1); err == nil {
		t.Fatal("copyContent() ignored destination failure")
	}
	if _, err := copyContent(context.Background(), io.Discard, bytes.NewBufferString("x"), workspace.MaxSyncFileBytes); err != nil {
		t.Fatalf("copyContent() with Workspace limit: %v", err)
	}
}

func TestOpenRejectsSymlinkedParentsAndObjects(t *testing.T) {
	store, root := newStore(t)
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(root, "u1234567")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stage(context.Background(), "u1234567", "q1234567", "o1234567", "image/png", bytes.NewBufferString("x")); err == nil {
		t.Fatal("Stage() followed a symlinked owner directory")
	}
	if err := store.Delete(context.Background(), "u1234567", "q1234567", "o1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Delete() with symlinked owner directory = %v, want ErrUnsafePath", err)
	}
	if _, err := store.Open(context.Background(), "u1234567", "q1234567", "o1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open() with symlinked owner directory = %v, want ErrUnsafePath", err)
	}
	if err := os.Remove(filepath.Join(root, "u1234567")); err != nil {
		t.Fatal(err)
	}
	_, err := store.Stage(context.Background(), "u1234567", "q1234567", "o1234567", "image/png", bytes.NewBufferString("x"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "u1234567", "q1234567", "o1234567", "media")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "secret"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), "u1234567", "q1234567", "o1234567"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open() with symlinked media object = %v, want ErrUnsafePath", err)
	}
}

func TestOpenRejectsMissingRootParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "media")
	if _, err := Open(path); err == nil {
		t.Fatal("Open() created a root with missing parent")
	}
}

func TestOpenRejectsSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "media-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Open() symlink root error = %v, want ErrUnsafePath", err)
	}
	if _, err := Open(string(filepath.Separator)); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("Open() root path error = %v, want ErrInvalidRoot", err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "media")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("root mode = %04o, want 0700", info.Mode().Perm())
	}
	return store, root
}

func sha256Bytes(value []byte) []byte {
	hash := sha256.Sum256(value)
	return hash[:]
}
