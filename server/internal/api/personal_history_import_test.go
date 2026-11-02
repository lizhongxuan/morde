package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type personalHistoryImportServiceStub struct {
	calls     int
	principal identity.Principal
	input     conversation.HistoryImport
	result    conversation.HistoryImportResult
	err       error
}

func (s *personalHistoryImportServiceStub) Import(_ context.Context, principal identity.Principal, input conversation.HistoryImport) (conversation.HistoryImportResult, error) {
	s.calls++
	s.principal, s.input = principal, input
	return s.result, s.err
}

func TestPersonalHistoryImportRouteUsesAuthenticatedPrincipalAndReturnsMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &personalHistoryImportServiceStub{result: conversation.HistoryImportResult{
		Conversation: conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", Title: "Imported"},
		Messages: []conversation.ImportedHistoryMessage{{SourceMessageID: "source-message", AttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"}, Message: conversation.Message{
			ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567", Role: conversation.RoleUser,
			Content: "hello", Sequence: 2,
		}}},
	}}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := gin.New()
	group := router.Group("/api", func(c *gin.Context) {
		c.Set(principalKey, principal)
		c.Next()
	})
	RegisterPersonalHistoryImportRoutes(group, service)

	body := `{"source_id":"app","device_account_id":"acct-01","source_conversation_id":"chat-01","title":"Imported","messages":[{"source_message_id":"source-message","role":"user","content":"hello","attachment_ids":["pa_0123456789abcdef0123456789abcdef"]}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/history/import", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var result personalHistoryImportResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || service.calls != 1 ||
		service.principal.UserID != principal.UserID || service.input.SourceConversationID != "chat-01" ||
		result.ConversationID != "c1234567" || len(result.Messages) != 1 || result.Messages[0].MessageID != "m1234567" ||
		result.Messages[0].Sequence != 2 || len(result.Messages[0].AttachmentIDs) != 1 || result.Messages[0].AttachmentIDs[0] != "pa_0123456789abcdef0123456789abcdef" {
		t.Fatalf("response = %d %s, decoded=%+v calls=%d input=%+v", response.Code, response.Body, result, service.calls, service.input)
	}
	if strings.Contains(response.Body.String(), `"task_id"`) {
		t.Fatalf("history import response exposes a task ID: %s", response.Body)
	}
}

func TestPersonalHistoryImportRouteRejectsOwnerFieldAndMissingPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, testCase := range []struct {
		name       string
		middleware gin.HandlerFunc
		body       string
	}{
		{name: "owner field", middleware: func(c *gin.Context) {
			c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"})
			c.Next()
		}, body: `{"owner_id":"u7654321"}`},
		{name: "missing principal", middleware: func(c *gin.Context) { c.Next() }, body: `{}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &personalHistoryImportServiceStub{}
			router := gin.New()
			group := router.Group("/api", testCase.middleware)
			RegisterPersonalHistoryImportRoutes(group, service)
			request := httptest.NewRequest(http.MethodPost, "/api/history/import", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code < 400 || service.calls != 0 {
				t.Fatalf("response = %d %s, service calls=%d; want rejection before service", response.Code, response.Body, service.calls)
			}
		})
	}
}

func TestPersonalHistoryImportRouteCapsRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &personalHistoryImportServiceStub{}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	router := gin.New()
	group := router.Group("/api", func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	RegisterPersonalHistoryImportRoutes(group, service)
	oversizedJSON := []byte(`{"source_id":"` + strings.Repeat("x", int(personalHistoryImportMaxBodyBytes)) + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/history/import", bytes.NewReader(oversizedJSON))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || service.calls != 0 {
		t.Fatalf("oversized request response = %d %s, service calls=%d", response.Code, response.Body, service.calls)
	}
}
