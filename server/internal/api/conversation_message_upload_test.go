package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/identity"
)

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
