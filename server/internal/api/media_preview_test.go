package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type taskMediaPreviewStub struct {
	preview             app.TaskMediaPreview
	principal           identity.Principal
	taskID, operationID string
	err                 error
	generationReady     bool
}

func (s *taskMediaPreviewStub) OpenPreview(_ context.Context, principal identity.Principal, taskID, operationID string) (app.TaskMediaPreview, error) {
	s.principal, s.taskID, s.operationID = principal, taskID, operationID
	return s.preview, s.err
}

func (s *taskMediaPreviewStub) GenerationReady() bool { return s.generationReady }

type closeTrackingReader struct {
	*bytes.Reader
	closed   bool
	closeErr error
}

func (r *closeTrackingReader) Close() error { r.closed = true; return r.closeErr }

func TestTaskMediaPreviewRequiresAssemblyAndStreamsOnlySafeMedia(t *testing.T) {
	services := newServices()
	withoutMedia := newRouter(t, services, Options{})
	if response := request(withoutMedia, http.MethodGet, "/api/tasks/q1234567/media/o1234567/preview", "", "Bearer "+apiKeyCredential(), ""); response.Code != http.StatusNotFound {
		t.Fatalf("unassembled Task media route = %d", response.Code)
	}

	body := &closeTrackingReader{Reader: bytes.NewReader([]byte("image bytes"))}
	media := &taskMediaPreviewStub{preview: app.TaskMediaPreview{Body: body, MediaType: "image/png", SizeBytes: int64(body.Len())}}
	deps := services.deps()
	deps.TaskMedia = media
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/tasks/q1234567/media/o1234567/preview", "", "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusOK || response.Body.String() != "image bytes" ||
		response.Header().Get("Content-Type") != "image/png" || response.Header().Get("Cache-Control") != "private, no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" || !body.closed {
		t.Fatalf("media preview = %d, headers=%v, body=%q, closed=%t", response.Code, response.Header(), response.Body.String(), body.closed)
	}
	if media.principal.UserID != testUserID || media.taskID != "q1234567" || media.operationID != "o1234567" {
		t.Fatalf("media preview scope = principal=%+v task=%q operation=%q", media.principal, media.taskID, media.operationID)
	}
}

func TestTaskMediaPreviewRejectsInvalidIDAndActiveContent(t *testing.T) {
	services := newServices()
	body := &closeTrackingReader{Reader: bytes.NewReader([]byte("not returned"))}
	media := &taskMediaPreviewStub{preview: app.TaskMediaPreview{Body: body, MediaType: "text/html", SizeBytes: int64(body.Len())}}
	deps := services.deps()
	deps.TaskMedia = media
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	key := "Bearer " + apiKeyCredential()
	invalidID := request(router, http.MethodGet, "/api/tasks/not-a-task/media/o1234567/preview", "", key, "")
	if invalidID.Code != http.StatusBadRequest || media.taskID != "" {
		t.Fatalf("invalid media Task ID = %d, service task=%q", invalidID.Code, media.taskID)
	}
	active := request(router, http.MethodGet, "/api/tasks/q1234567/media/o1234567/preview", "", key, "")
	if active.Code != http.StatusInternalServerError || strings.Contains(active.Body.String(), "not returned") || !body.closed {
		t.Fatalf("active media response = %d %q closed=%t", active.Code, active.Body.String(), body.closed)
	}
	for _, mediaType := range []string{"image/png; charset=utf-8", "IMAGE/PNG"} {
		body := &closeTrackingReader{Reader: bytes.NewReader([]byte("not returned"))}
		media.preview = app.TaskMediaPreview{Body: body, MediaType: mediaType, SizeBytes: int64(body.Len())}
		response := request(router, http.MethodGet, "/api/tasks/q1234567/media/o1234567/preview", "", key, "")
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "not returned") || !body.closed {
			t.Fatalf("noncanonical media type %q response = %d %q closed=%t", mediaType, response.Code, response.Body.String(), body.closed)
		}
	}
}

func TestTaskMediaPreviewLogsBodyCloseError(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	options := validOptions()
	options.Logger = zap.New(core)

	services := newServices()
	body := &closeTrackingReader{Reader: bytes.NewReader([]byte("image bytes")), closeErr: errors.New("close failed")}
	media := &taskMediaPreviewStub{preview: app.TaskMediaPreview{Body: body, MediaType: "image/png", SizeBytes: int64(body.Len())}}
	deps := services.deps()
	deps.TaskMedia = media
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/tasks/q1234567/media/o1234567/preview", "", "Bearer "+apiKeyCredential(), "")
	if response.Code != http.StatusOK || !body.closed || logs.FilterMessage("close task media preview body").Len() != 1 {
		t.Fatalf("media preview close error = status %d, closed=%t, logs=%v", response.Code, body.closed, logs.All())
	}
}

var _ io.ReadCloser = (*closeTrackingReader)(nil)
