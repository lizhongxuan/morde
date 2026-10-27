package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type instantChatServiceFake struct {
	sends   int
	command app.InstantChatSend
}

func (*instantChatServiceFake) List(context.Context, identity.Principal) ([]app.InstantChatConversation, error) {
	return []app.InstantChatConversation{}, nil
}
func (*instantChatServiceFake) Messages(context.Context, identity.Principal, string) ([]app.InstantChatMessage, error) {
	return []app.InstantChatMessage{}, nil
}
func (f *instantChatServiceFake) Send(_ context.Context, _ identity.Principal, command app.InstantChatSend) (app.InstantChatReply, error) {
	f.sends++
	f.command = command
	now := time.Now()
	return app.InstantChatReply{
		Conversation: app.InstantChatConversation{ID: "c1234567", Title: "hello", ModelID: command.ModelID, CreatedAt: now, UpdatedAt: now},
		Message:      app.InstantChatMessage{ID: "m1234567", ConversationID: "c1234567", Role: "user", Content: command.Content, CreatedAt: now},
		Reply:        app.InstantChatMessage{ID: "m7654321", ConversationID: "c1234567", Role: "assistant", Content: "Hi", CreatedAt: now},
	}, nil
}

func TestInstantChatRequiresPortalSessionAndSendsMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &instantChatServiceFake{}
	handler := &api{deps: Deps{InstantChat: service}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	handler.registerInstantChat(engine.Group("/api"))
	request := httptest.NewRequest(http.MethodPost, "/api/instant-chat/messages", bytes.NewBufferString(`{"model_id":"p1234567","content":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "request-12345678")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.sends != 1 || service.command.RequestKey != "request-12345678" || service.command.Content != "hello" {
		t.Fatalf("send failed: status=%d body=%s command=%+v", recorder.Code, recorder.Body.String(), service.command)
	}

	principal.Kind = identity.KindClient
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request.Clone(context.Background()))
	if recorder.Code != http.StatusForbidden || service.sends != 1 {
		t.Fatalf("client credential should be denied: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
