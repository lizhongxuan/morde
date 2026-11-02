package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalAttachmentServiceStub struct {
	principal identity.Principal
	upload    app.PersonalAttachmentUploadCommand
	chunk     app.PersonalAttachmentChunkCommand
	create    personal.AttachmentUploadReceipt
	put       personal.AttachmentChunkReceipt
	commit    personal.AttachmentCommitReceipt
	metadata  personal.AttachmentMetadata
	body      []byte
	calls     int
}

func (s *personalAttachmentServiceStub) CreatePersonalAttachmentUpload(_ context.Context, principal identity.Principal, command app.PersonalAttachmentUploadCommand) (personal.AttachmentUploadReceipt, error) {
	s.calls++
	s.principal, s.upload = principal, command
	return s.create, nil
}

func (s *personalAttachmentServiceStub) PutPersonalAttachmentChunk(_ context.Context, principal identity.Principal, command app.PersonalAttachmentChunkCommand) (personal.AttachmentChunkReceipt, error) {
	s.calls++
	s.principal, s.chunk = principal, command
	body, _ := io.ReadAll(command.Body)
	s.chunk.Body = bytes.NewReader(body)
	return s.put, nil
}

func (s *personalAttachmentServiceStub) CommitPersonalAttachment(_ context.Context, principal identity.Principal, uploadID, idempotencyKey string) (personal.AttachmentCommitReceipt, error) {
	s.calls++
	s.principal = principal
	return s.commit, nil
}

func (s *personalAttachmentServiceStub) CancelPersonalAttachmentUpload(_ context.Context, principal identity.Principal, _ string) error {
	s.calls++
	s.principal = principal
	return nil
}

func (s *personalAttachmentServiceStub) OpenPersonalAttachment(_ context.Context, principal identity.Principal, _ string) (io.ReadCloser, personal.AttachmentMetadata, error) {
	s.calls++
	s.principal = principal
	return io.NopCloser(bytes.NewReader(s.body)), s.metadata, nil
}

func TestPersonalAttachmentRoutesUseOwnerScopedServiceAndBoundedChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	service := &personalAttachmentServiceStub{
		create: personal.AttachmentUploadReceipt{UploadID: "pu_0123456789abcdef0123456789abcdef", State: personal.AttachmentUploadActive, ChunkCount: 1},
		put:    personal.AttachmentChunkReceipt{UploadID: "pu_0123456789abcdef0123456789abcdef", Sequence: 0, Replay: true},
	}
	router := gin.New()
	group := router.Group("/api/personal-data", func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	RegisterPersonalAttachmentRoutes(group, service)

	create := httptest.NewRequest(http.MethodPost, "/api/personal-data/uploads", strings.NewReader(`{"object_id":"library-item_1","base_revision":1,"size_bytes":5,"sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","media_type":"text/plain"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Idempotency-Key", "upload-key-001")
	created := httptest.NewRecorder()
	router.ServeHTTP(created, create)
	var uploadResponse personalAttachmentUploadResponse
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &uploadResponse) != nil ||
		service.upload.IdempotencyKey != "upload-key-001" || service.principal.UserID != principal.UserID || uploadResponse.ChunkCount != 1 {
		t.Fatalf("create response=%d %s upload=%+v calls=%d", created.Code, created.Body, service.upload, service.calls)
	}

	chunkBody := []byte("hello")
	digest := sha256.Sum256(chunkBody)
	chunk := httptest.NewRequest(http.MethodPut, "/api/personal-data/uploads/pu_0123456789abcdef0123456789abcdef/chunks/0", bytes.NewReader(chunkBody))
	chunk.Header.Set(personalAttachmentChunkDigestHeader, hex.EncodeToString(digest[:]))
	chunk.Header.Set("Content-Length", strconv.Itoa(len(chunkBody)))
	put := httptest.NewRecorder()
	router.ServeHTTP(put, chunk)
	var chunkResponse personalAttachmentChunkResponse
	if put.Code != http.StatusOK || json.Unmarshal(put.Body.Bytes(), &chunkResponse) != nil || !chunkResponse.Replay ||
		service.chunk.UploadID != "pu_0123456789abcdef0123456789abcdef" || service.chunk.SizeBytes != int64(len(chunkBody)) {
		t.Fatalf("chunk response=%d %s command=%+v", put.Code, put.Body, service.chunk)
	}

	before := service.calls
	oversized := httptest.NewRequest(http.MethodPut, "/api/personal-data/uploads/pu_0123456789abcdef0123456789abcdef/chunks/0", bytes.NewReader(bytes.Repeat([]byte("x"), int(personal.MaxAttachmentChunkBytes+1))))
	oversized.Header.Set(personalAttachmentChunkDigestHeader, strings.Repeat("a", 64))
	oversized.ContentLength = personal.MaxAttachmentChunkBytes + 1
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, oversized)
	if rejected.Code != http.StatusRequestEntityTooLarge || service.calls != before {
		t.Fatalf("oversized chunk response=%d %s service calls=%d", rejected.Code, rejected.Body, service.calls)
	}
}

func TestPersonalAttachmentReadStreamsPrivateNoStoreResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte("private file")
	digest := sha256.Sum256(body)
	service := &personalAttachmentServiceStub{
		body: body,
		metadata: personal.AttachmentMetadata{
			AttachmentID: "pa_0123456789abcdef0123456789abcdef", ObjectType: personal.ObjectTypeLibraryItem,
			ObjectID: "library-item_1", MediaType: "text/plain", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
		},
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := gin.New()
	group := router.Group("/api/personal-data", func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	RegisterPersonalAttachmentRoutes(group, service)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/personal-data/attachments/pa_0123456789abcdef0123456789abcdef", nil))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), body) ||
		response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" ||
		response.Header().Get("Content-Disposition") != "attachment" ||
		response.Header().Get("Content-Type") != "text/plain" || response.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("private attachment response=%d headers=%v body=%q", response.Code, response.Header(), response.Body.Bytes())
	}
}
