package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type conversationUploadModelCatalogStub struct{}

func (conversationUploadModelCatalogStub) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return false, nil
}

func TestConversationMessageUploadHTTPProtocolPersistsAndReplays(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	store, credentials, _, owner := clientLoginFixture(t)
	pool := store.pool
	ctx := context.Background()
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, conversationUploadModelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts: nativeAcceptanceHosts{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, Tasks: tasks, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), ServiceInstanceID: instanceID,
		APIMaxBodyBytes: 1024, MCPMaxBodyBytes: 1024, SessionTTL: 12 * time.Hour,
		HostScheme: "http", PortalDomain: "portal.test", HostSuffix: "hosts.invalid", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	clientMessageID := "upload-http-message-0001"
	content := []byte(strings.Repeat("中文abc🙂", 5000) + "末尾完整")
	contentDigest := sha256.Sum256(content)
	totalChunks := (len(content) + app.ConversationMessageChunkBytes - 1) / app.ConversationMessageChunkBytes
	basePath := "/api/conversations/" + conversationValue.ID + "/message-uploads/" + clientMessageID
	do := func(method, path string, body []byte, idempotencyKey string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Host = "portal.test"
		request.AddCookie(&http.Cookie{Name: "ch_portal", Value: login.Credential})
		request.Header.Set("Origin", "http://portal.test")
		request.Header.Set("X-Morde-Actor-ID", owner.ID)
		request.Header.Set("X-Morde-Service-Instance-ID", instanceID)
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	initBody, err := json.Marshal(map[string]any{
		"total_bytes": len(content), "total_chunks": totalChunks, "sha256": hex.EncodeToString(contentDigest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	initialized := do(http.MethodPut, basePath, initBody, clientMessageID)
	if initialized.Code != http.StatusOK {
		t.Fatalf("initialize upload: status=%d body=%s", initialized.Code, initialized.Body.String())
	}
	putChunk := func(sequence int, replays int) {
		t.Helper()
		start := sequence * app.ConversationMessageChunkBytes
		chunk := content[start:min(start+app.ConversationMessageChunkBytes, len(content))]
		chunkDigest := sha256.Sum256(chunk)
		chunkBody, err := json.Marshal(map[string]string{
			"content_base64": base64.StdEncoding.EncodeToString(chunk), "sha256": hex.EncodeToString(chunkDigest[:]),
		})
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("%s/chunks/%d", basePath, sequence)
		for replay := 0; replay < replays; replay++ {
			response := do(http.MethodPut, path, chunkBody, "")
			if response.Code != http.StatusNoContent {
				t.Fatalf("upload chunk %d replay %d: status=%d body=%s", sequence, replay, response.Code, response.Body.String())
			}
		}
	}
	putChunk(totalChunks-1, 2)
	// Model a client process restart after only one chunk reached the server.
	initializedAgain := do(http.MethodPut, basePath, initBody, clientMessageID)
	if initializedAgain.Code != http.StatusOK {
		t.Fatalf("resume upload initialization: status=%d body=%s", initializedAgain.Code, initializedAgain.Body.String())
	}
	status := do(http.MethodGet, basePath, nil, "")
	var upload struct {
		State             string `json:"state"`
		ReceivedSequences []int  `json:"received_sequences"`
	}
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &upload) != nil || upload.State != "uploading" ||
		len(upload.ReceivedSequences) != 1 || upload.ReceivedSequences[0] != totalChunks-1 {
		t.Fatalf("recover partial upload: status=%d body=%s", status.Code, status.Body.String())
	}
	for sequence := totalChunks - 2; sequence >= 0; sequence-- {
		putChunk(sequence, 2)
	}
	status = do(http.MethodGet, basePath, nil, "")
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &upload) != nil || upload.State != "uploading" || len(upload.ReceivedSequences) != totalChunks {
		t.Fatalf("recover uploaded chunks: status=%d body=%s", status.Code, status.Body.String())
	}

	commitPath := basePath + "/commit"
	committed := do(http.MethodPost, commitPath, nil, clientMessageID)
	var receipt struct {
		Message struct {
			ID            string `json:"id"`
			ClientMessage string `json:"client_message_id"`
			Content       string `json:"content"`
		} `json:"message"`
	}
	if committed.Code != http.StatusAccepted || json.Unmarshal(committed.Body.Bytes(), &receipt) != nil ||
		receipt.Message.ID == "" || receipt.Message.ClientMessage != clientMessageID || receipt.Message.Content != string(content) {
		t.Fatalf("commit upload: status=%d body=%s", committed.Code, committed.Body.String())
	}
	replayed := do(http.MethodPost, commitPath, nil, clientMessageID)
	var replayedReceipt struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if replayed.Code != http.StatusAccepted || json.Unmarshal(replayed.Body.Bytes(), &replayedReceipt) != nil || replayedReceipt.Message.ID != receipt.Message.ID {
		t.Fatalf("replay committed upload: status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	committedView := do(http.MethodGet, basePath, nil, "")
	if committedView.Code != http.StatusOK || !bytes.Contains(committedView.Body.Bytes(), []byte(`"state":"committed"`)) ||
		!bytes.Contains(committedView.Body.Bytes(), []byte(receipt.Message.ID)) {
		t.Fatalf("read committed receipt: status=%d body=%s", committedView.Code, committedView.Body.String())
	}
	var messages, routes int
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
  WHERE route.conversation_id=$1 AND route.client_message_id=$2),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$1 AND client_message_id=$2)`,
		conversationValue.ID, clientMessageID).Scan(&messages, &routes); err != nil || messages != 1 || routes != 1 {
		t.Fatalf("HTTP upload persisted messages=%d routes=%d err=%v", messages, routes, err)
	}
}

func TestConversationMessageUploadPreservesLongTextAndIdempotency(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	other := createTestUser(t, store, 1, 1000, 2048)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(strings.Repeat("中文a", 18000) + "结束")
	contentHash := sha256.Sum256(content)
	requestHash := sha256.Sum256([]byte("stable upload metadata"))
	receipt := app.ConversationMessageReceipt{
		Message:           conversation.Message{ConversationID: conversationValue.ID, OwnerID: owner.ID, ActorID: owner.ID, Role: conversation.RoleUser},
		ServiceInstanceID: instanceID, ClientID: "long-message-001", Authorization: task.Authorization{Kind: "session", Version: 1},
	}
	init := app.ConversationMessageUploadInit{Receipt: receipt, TotalBytes: len(content),
		TotalChunks: (len(content) + app.ConversationMessageChunkBytes - 1) / app.ConversationMessageChunkBytes,
		ContentHash: contentHash[:], RequestHash: requestHash[:]}
	upload, err := store.InitializeConversationMessageUpload(ctx, init)
	if err != nil || upload.State != "uploading" || upload.TotalChunks != init.TotalChunks || len(upload.ReceivedSequences) != 0 {
		t.Fatalf("initialize upload = %+v err=%v", upload, err)
	}
	if _, err := store.CommitConversationMessageUpload(ctx, receipt); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("incomplete upload was committed: %v", err)
	}
	for sequence := init.TotalChunks - 1; sequence >= 0; sequence-- {
		start := sequence * app.ConversationMessageChunkBytes
		chunk := content[start:min(start+app.ConversationMessageChunkBytes, len(content))]
		digest := sha256.Sum256(chunk)
		if err := store.PutConversationMessageChunk(ctx, receipt, sequence, chunk, digest[:]); err != nil {
			t.Fatalf("write chunk %d: %v", sequence, err)
		}
		if err := store.PutConversationMessageChunk(ctx, receipt, sequence, chunk, digest[:]); err != nil {
			t.Fatalf("replay chunk %d: %v", sequence, err)
		}
	}
	changed := append([]byte(nil), content[:app.ConversationMessageChunkBytes]...)
	changed[0] ^= 1
	changedHash := sha256.Sum256(changed)
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, changed, changedHash[:]); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("conflicting chunk replay = %v", err)
	}
	foreign := receipt
	foreign.Message.OwnerID, foreign.Message.ActorID = other.ID, other.ID
	if _, err := store.GetConversationMessageUpload(ctx, foreign); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account read upload: %v", err)
	}

	const callers = 6
	type result struct {
		message conversation.Message
		err     error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			message, err := store.CommitConversationMessageUpload(ctx, receipt)
			results <- result{message, err}
		}()
	}
	wg.Wait()
	close(results)
	var messageID string
	for value := range results {
		if value.err != nil || value.message.Content != string(content) || value.message.TaskID != "" ||
			value.message.RoutingStatus != conversation.RoutingPending {
			t.Fatalf("commit result content bytes=%d task=%q state=%q err=%v", len(value.message.Content), value.message.TaskID, value.message.RoutingStatus, value.err)
		}
		if messageID == "" {
			messageID = value.message.ID
		}
		if value.message.ID != messageID {
			t.Fatalf("concurrent commit created different messages: %s and %s", messageID, value.message.ID)
		}
	}
	upload, err = store.GetConversationMessageUpload(ctx, receipt)
	if err != nil || upload.State != "committed" || upload.Message == nil || upload.Message.ID != messageID || len(upload.ReceivedSequences) != init.TotalChunks {
		t.Fatalf("recover committed upload = %+v err=%v", upload, err)
	}
	var retainedPayloadBytes int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(content)),0) FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
		receipt.ServiceInstanceID, owner.ID, conversationValue.ID, receipt.ClientID).Scan(&retainedPayloadBytes); err != nil || retainedPayloadBytes != 0 {
		t.Fatalf("committed upload retained %d payload bytes, err=%v", retainedPayloadBytes, err)
	}
	firstChunk := content[:app.ConversationMessageChunkBytes]
	firstDigest := sha256.Sum256(firstChunk)
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, firstChunk, firstDigest[:]); err != nil {
		t.Fatalf("replay committed chunk by digest: %v", err)
	}
	conflictingChunk := append([]byte(nil), firstChunk...)
	conflictingChunk[0] ^= 1
	conflictingDigest := sha256.Sum256(conflictingChunk)
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, conflictingChunk, conflictingDigest[:]); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("conflicting committed chunk replay = %v", err)
	}
	var messages, routes int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$1)`, conversationValue.ID).Scan(&messages, &routes); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || routes != 1 {
		t.Fatalf("long upload created messages=%d routes=%d", messages, routes)
	}
}

func TestConversationMessageUploadExpiresAndRetainsCommittedReceipt(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("a resumable local upload")
	contentHash := sha256.Sum256(content)
	requestHash := sha256.Sum256([]byte("stable upload metadata"))
	receipt := app.ConversationMessageReceipt{
		Message:           conversation.Message{ConversationID: conversationValue.ID, OwnerID: owner.ID, ActorID: owner.ID, Role: conversation.RoleUser},
		ServiceInstanceID: instanceID, ClientID: "expiry-message-001", Authorization: task.Authorization{Kind: "session", Version: 1},
	}
	init := app.ConversationMessageUploadInit{Receipt: receipt, TotalBytes: len(content), TotalChunks: 1,
		ContentHash: contentHash[:], RequestHash: requestHash[:]}
	if _, err := store.InitializeConversationMessageUpload(ctx, init); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, content, digest[:]); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.CleanupExpiredConversationMessageUploads(ctx, time.Now().UTC(), 10); err != nil || removed != 0 {
		t.Fatalf("cleanup before expiry removed=%d err=%v", removed, err)
	}
	active, err := store.GetConversationMessageUpload(ctx, receipt)
	if err != nil || active.State != "uploading" || len(active.ReceivedSequences) != 1 {
		t.Fatalf("active upload recovery = %+v err=%v", active, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_uploads SET expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
		instanceID, owner.ID, conversationValue.ID, receipt.ClientID); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.CleanupExpiredConversationMessageUploads(ctx, time.Now().UTC(), 10); err != nil || removed != 1 {
		t.Fatalf("expired upload cleanup removed=%d err=%v", removed, err)
	}
	if _, err := store.GetConversationMessageUpload(ctx, receipt); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired upload still readable: %v", err)
	}
	if _, err := store.CommitConversationMessageUpload(ctx, receipt); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("expired upload still committable: %v", err)
	}
	reset, err := store.InitializeConversationMessageUpload(ctx, init)
	if err != nil || reset.State != "uploading" || len(reset.ReceivedSequences) != 0 {
		t.Fatalf("reinitialize expired upload = %+v err=%v", reset, err)
	}
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, content, digest[:]); err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitConversationMessageUpload(ctx, receipt)
	if err != nil || committed.Content != string(content) {
		t.Fatalf("commit resumed upload = %+v err=%v", committed, err)
	}
	view, err := store.GetConversationMessageUpload(ctx, receipt)
	if err != nil || view.State != "committed" || view.Message == nil || view.Message.ID != committed.ID || len(view.ReceivedSequences) != 1 {
		t.Fatalf("committed receipt after payload cleanup = %+v err=%v", view, err)
	}
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, content, digest[:]); err != nil {
		t.Fatalf("committed chunk retry after cleanup: %v", err)
	}
	if removed, err := store.CleanupExpiredConversationMessageUploads(ctx, time.Now().Add(48*time.Hour), 10); err != nil || removed != 0 {
		t.Fatalf("cleanup removed committed receipt count=%d err=%v", removed, err)
	}
}
