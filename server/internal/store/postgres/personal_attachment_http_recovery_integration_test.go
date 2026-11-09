package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/personal"
)

// TestPersonalAttachmentHTTPUploadResumesAfterClientRestart exercises the
// production upload handlers against PostgreSQL. Rebuilding the API router
// between the first persisted chunk and the replay models a fresh client/server
// process reading only the durable upload receipt.
func TestPersonalAttachmentHTTPUploadResumesAfterClientRestart(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	store, credentials, _, owner := clientLoginFixture(t)
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	objectID := "library-item_http-resume-001"
	createPersonalLibraryItemForAttachmentTest(t, store, principal, objectID)

	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	quota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	quotaUser, err := app.NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	newRouter := func() http.Handler {
		t.Helper()
		attachments, err := app.NewPersonalAttachmentService(store, quota)
		if err != nil {
			t.Fatal(err)
		}
		router, err := api.New(api.Deps{
			Hosts: nativeAcceptanceHosts{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
			Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, PersonalData: personalData,
			PersonalSync: personalSync, PersonalDataQuotaUser: quotaUser, PersonalAttachments: attachments,
			MCPDisabled: true,
		}, api.Options{
			Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), ServiceInstanceID: instanceID,
			APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20, SessionTTL: 12 * time.Hour,
			HostScheme: "http", PortalDomain: "portal.test", HostSuffix: "hosts.invalid", PreviewPortMin: 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		return router
	}
	router := newRouter()
	do := func(method, path string, body []byte, idempotencyKey string, extra http.Header) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Host = "portal.test"
		request.AddCookie(&http.Cookie{Name: "ch_portal", Value: login.Credential})
		request.Header.Set("Origin", "http://portal.test")
		request.Header.Set("X-Morde-Actor-ID", owner.ID)
		request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		for name, values := range extra {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	const chunkBytes = int(personal.MaxAttachmentChunkBytes)
	pattern := []byte("附件跨重启恢复-hello-🙂-")
	content := bytes.Repeat(pattern, chunkBytes/len(pattern)+2)
	content = content[:chunkBytes+37]
	digest := sha256.Sum256(content)
	const uploadKey = "ios-http-resume-upload-001"
	initBody, err := json.Marshal(map[string]any{
		"object_id": objectID, "base_revision": 1, "size_bytes": len(content),
		"sha256": hex.EncodeToString(digest[:]), "media_type": "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	decodeUpload := func(response *httptest.ResponseRecorder) struct {
		UploadID          string  `json:"upload_id"`
		State             string  `json:"state"`
		ChunkCount        int32   `json:"chunk_count"`
		ReceivedSequences []int32 `json:"received_sequences"`
		Replay            bool    `json:"replay"`
	} {
		t.Helper()
		var value struct {
			UploadID          string  `json:"upload_id"`
			State             string  `json:"state"`
			ChunkCount        int32   `json:"chunk_count"`
			ReceivedSequences []int32 `json:"received_sequences"`
			Replay            bool    `json:"replay"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatalf("decode upload receipt: %v; response=%s", err, response.Body.String())
		}
		return value
	}
	const uploadPath = "/api/personal-data/uploads"
	createdResponse := do(http.MethodPost, uploadPath, initBody, uploadKey,
		http.Header{"Content-Type": []string{"application/json"}})
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("initialize attachment upload: status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeUpload(createdResponse)
	if created.UploadID == "" || created.State != string(personal.AttachmentUploadActive) || created.ChunkCount != 2 || len(created.ReceivedSequences) != 0 {
		t.Fatalf("unexpected initial upload receipt: %+v", created)
	}
	putChunk := func(sequence int32) {
		t.Helper()
		start := int(sequence) * chunkBytes
		chunk := content[start:min(start+chunkBytes, len(content))]
		chunkDigest := sha256.Sum256(chunk)
		path := fmt.Sprintf("%s/%s/chunks/%d", uploadPath, created.UploadID, sequence)
		response := do(http.MethodPut, path, chunk, "", http.Header{
			"Content-Type":   []string{"application/octet-stream"},
			"X-Chunk-SHA256": []string{hex.EncodeToString(chunkDigest[:])},
			"Content-Length": []string{fmt.Sprint(len(chunk))},
		})
		if response.Code != http.StatusOK {
			t.Fatalf("upload chunk %d: status=%d body=%s", sequence, response.Code, response.Body.String())
		}
	}
	putChunk(0)

	// Reconstruct both service and HTTP handlers to ensure resume depends on the
	// persisted idempotency receipt, not any process-local upload state.
	router = newRouter()
	resumedResponse := do(http.MethodPost, uploadPath, initBody, uploadKey,
		http.Header{"Content-Type": []string{"application/json"}})
	if resumedResponse.Code != http.StatusOK {
		t.Fatalf("resume attachment upload: status=%d body=%s", resumedResponse.Code, resumedResponse.Body.String())
	}
	resumed := decodeUpload(resumedResponse)
	if resumed.UploadID != created.UploadID || !resumed.Replay || resumed.State != string(personal.AttachmentUploadActive) ||
		resumed.ChunkCount != 2 || len(resumed.ReceivedSequences) != 1 || resumed.ReceivedSequences[0] != 0 {
		t.Fatalf("resumed receipt did not report the persisted first chunk: %+v", resumed)
	}
	putChunk(1)
	commitKey := "ios-http-resume-commit-001"
	commitPath := uploadPath + "/" + created.UploadID + "/commit"
	committedResponse := do(http.MethodPost, commitPath, nil, commitKey, nil)
	if committedResponse.Code != http.StatusOK {
		t.Fatalf("commit resumed attachment: status=%d body=%s", committedResponse.Code, committedResponse.Body.String())
	}
	var committed struct {
		AttachmentID string `json:"attachment_id"`
		ObjectID     string `json:"object_id"`
		Replay       bool   `json:"replay"`
	}
	if err := json.Unmarshal(committedResponse.Body.Bytes(), &committed); err != nil ||
		!personal.ValidPersonalAttachmentID(committed.AttachmentID) || committed.ObjectID != objectID || committed.Replay {
		t.Fatalf("unexpected commit receipt: %+v err=%v body=%s", committed, err, committedResponse.Body.String())
	}
	replayedCommit := do(http.MethodPost, commitPath, nil, commitKey, nil)
	var replay struct {
		AttachmentID string `json:"attachment_id"`
		Replay       bool   `json:"replay"`
	}
	if replayedCommit.Code != http.StatusOK || json.Unmarshal(replayedCommit.Body.Bytes(), &replay) != nil ||
		replay.AttachmentID != committed.AttachmentID || !replay.Replay {
		t.Fatalf("commit retry changed attachment: status=%d body=%s", replayedCommit.Code, replayedCommit.Body.String())
	}
	read := do(http.MethodGet, "/api/personal-data/attachments/"+committed.AttachmentID, nil, "", nil)
	if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), content) {
		t.Fatalf("read resumed attachment: status=%d bytes=%d want=%d", read.Code, read.Body.Len(), len(content))
	}
	var committedUploads int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3 AND state='committed'`,
		instanceID, owner.ID, created.UploadID).Scan(&committedUploads); err != nil || committedUploads != 1 {
		t.Fatalf("committed upload records=%d err=%v", committedUploads, err)
	}
}
