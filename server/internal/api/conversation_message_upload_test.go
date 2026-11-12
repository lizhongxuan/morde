package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type conversationUploadRoutingStub struct {
	*taskServiceStub
	command app.InitializeConversationMessageUploadCommand
}

func (s *conversationUploadRoutingStub) InitializeConversationMessageUpload(_ context.Context, _ identity.Principal, _ string, command app.InitializeConversationMessageUploadCommand) (app.ConversationMessageUpload, error) {
	s.command = command
	return app.ConversationMessageUpload{ClientMessageID: command.ClientMessageID, State: "uploading", TotalBytes: command.TotalBytes, TotalChunks: command.TotalChunks}, nil
}

func TestConversationChunkBodyLimitAllowsFullChunkWithSmallDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &api{options: Options{APIMaxBodyBytes: 1024}}
	group := router.Group("/api", handler.limitBody, func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	registerConversationMessageUploads(group, &taskServiceStub{})
	payload := bytes.Repeat([]byte("x"), 16<<10)
	digest := sha256.Sum256(payload)
	body, err := json.Marshal(messageUploadChunkRequest{ContentBase64: base64.StdEncoding.EncodeToString(payload), SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/conversations/c1234567/message-uploads/client-message-001/chunks/0",
		"/api/projects/j1234567/conversations/c1234567/message-uploads/client-message-001/chunks/0",
	} {
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Morde-Actor-ID", "u1234567")
		req.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("full chunk %s: status=%d body=%s", path, resp.Code, resp.Body.String())
		}
	}
}

func TestConversationMessageUploadCarriesExplicitTaskTargetChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	service := &conversationUploadRoutingStub{taskServiceStub: &taskServiceStub{}}
	group := router.Group("/api", func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	registerConversationMessageUploads(group, service)
	for _, path := range []string{
		"/api/conversations/c1234567/message-uploads/client-message-001",
		"/api/projects/j1234567/conversations/c1234567/message-uploads/client-message-001",
	} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"total_bytes":1,"total_chunks":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","change_task_target":true,"time_zone":"Asia/Shanghai"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "client-message-001")
		request.Header.Set("X-Morde-Actor-ID", "u1234567")
		request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !service.command.ChangeTaskTarget || service.command.TimeZone != "Asia/Shanghai" {
			t.Fatalf("initialize %s: status=%d body=%s target-change=%t", path, response.Code, response.Body.String(), service.command.ChangeTaskTarget)
		}
	}
}
