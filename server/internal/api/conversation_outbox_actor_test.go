package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type outboxActorTaskStub struct {
	*taskServiceStub
	uploadCalls                                     int
	attachmentCalls                                 int
	conversationID, messageID, slotID, attachmentID string
	instanceID                                      string
	processingRetryCalls                            int
	retryProjectID, retryKey                        string
}

func (s *outboxActorTaskStub) RetryConversationMessageProcessing(_ context.Context, _ identity.Principal, projectID, conversationID, messageID, instanceID, key string) error {
	s.processingRetryCalls++
	s.retryProjectID, s.conversationID, s.messageID, s.instanceID, s.retryKey = projectID, conversationID, messageID, instanceID, key
	return nil
}

func TestRetryConversationProcessingForwardsScopedIdentity(t *testing.T) {
	for _, tc := range []struct{ path, projectID string }{
		{"/api/conversations/c1234567/messages/m1234567/retry-processing", ""},
		{"/api/projects/j1234567/conversations/c1234567/messages/m1234567/retry-processing", "j1234567"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			tasks := &outboxActorTaskStub{taskServiceStub: &taskServiceStub{}}
			router := taskAPIRouter(tasks, nil)
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Morde-Actor-ID", "u1234567")
			req.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
			req.Header.Set("Idempotency-Key", "retry-processing-001")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusAccepted || tasks.processingRetryCalls != 1 || tasks.retryProjectID != tc.projectID ||
				tasks.conversationID != "c1234567" || tasks.messageID != "m1234567" ||
				tasks.instanceID != "11111111-1111-4111-8111-111111111111" || tasks.retryKey != "retry-processing-001" {
				t.Fatalf("retry status=%d task=%+v body=%s", resp.Code, tasks, resp.Body.String())
			}
		})
	}
}

func TestRetryConversationProcessingRequiresIdempotencyKey(t *testing.T) {
	tasks := &outboxActorTaskStub{taskServiceStub: &taskServiceStub{}}
	router := taskAPIRouter(tasks, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1234567/messages/m1234567/retry-processing", nil)
	req.Header.Set("X-Morde-Actor-ID", "u1234567")
	req.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest || tasks.processingRetryCalls != 0 {
		t.Fatalf("missing idempotency key: status=%d calls=%d", resp.Code, tasks.processingRetryCalls)
	}
}

func (s *outboxActorTaskStub) InitializeConversationMessageUpload(context.Context, identity.Principal, string, app.InitializeConversationMessageUploadCommand) (app.ConversationMessageUpload, error) {
	s.uploadCalls++
	return app.ConversationMessageUpload{}, nil
}
func (s *outboxActorTaskStub) PutConversationMessageChunk(context.Context, identity.Principal, string, string, string, string, int, []byte, string) error {
	s.uploadCalls++
	return nil
}
func (s *outboxActorTaskStub) GetConversationMessageUpload(context.Context, identity.Principal, string, string, string, string) (app.ConversationMessageUpload, error) {
	s.uploadCalls++
	return app.ConversationMessageUpload{}, nil
}
func (s *outboxActorTaskStub) CommitConversationMessageUpload(context.Context, identity.Principal, string, string, string, string, string) (conversation.Message, error) {
	s.uploadCalls++
	return conversation.Message{}, nil
}
func (s *outboxActorTaskStub) AssociateConversationMessageAttachment(_ context.Context, _ identity.Principal, conversationID, messageID, slotID, attachmentID, instanceID string) error {
	s.attachmentCalls++
	s.conversationID, s.messageID, s.slotID, s.attachmentID, s.instanceID = conversationID, messageID, slotID, attachmentID, instanceID
	return nil
}

func TestConversationAttachmentEndpointForwardsScopedSlot(t *testing.T) {
	tasks := &outboxActorTaskStub{taskServiceStub: &taskServiceStub{}}
	router := taskAPIRouter(tasks, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/conversations/c1234567/messages/m1234567/attachments/client-attachment-001",
		strings.NewReader(`{"attachment_id":"pa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Morde-Actor-ID", "u1234567")
	req.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent || tasks.attachmentCalls != 1 || tasks.conversationID != "c1234567" ||
		tasks.messageID != "m1234567" || tasks.slotID != "client-attachment-001" ||
		tasks.attachmentID != "pa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || tasks.instanceID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("attachment association status=%d task=%+v body=%s", resp.Code, tasks, resp.Body.String())
	}
}

func TestOutboxRejectsAuthenticatedAccountSwitchBeforeAccess(t *testing.T) {
	tasks := &outboxActorTaskStub{taskServiceStub: &taskServiceStub{}}
	conversations := &conversationServiceStub{}
	router := taskAPIRouter(tasks, conversations)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/conversations"},
		{http.MethodPost, "/api/conversations/c1234567/messages"},
		{http.MethodGet, "/api/conversations/c1234567/messages?client_message_id=client-message-001"},
		{http.MethodPut, "/api/conversations/c1234567/message-uploads/client-message-001"},
		{http.MethodGet, "/api/conversations/c1234567/message-uploads/client-message-001"},
		{http.MethodPut, "/api/conversations/c1234567/message-uploads/client-message-001/chunks/0"},
		{http.MethodPost, "/api/conversations/c1234567/message-uploads/client-message-001/commit"},
		{http.MethodPut, "/api/conversations/c1234567/messages/m1234567/attachments/client-attachment-001"},
		{http.MethodPost, "/api/conversations/c1234567/messages/m1234567/retry-processing"},
		{http.MethodPost, "/api/projects/j1234567/conversations/c1234567/messages/m1234567/retry-processing"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("X-Morde-Actor-ID", "u7654321") // request authenticates as u1234567
		req.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden || tasks.acceptCalls != 0 || tasks.uploadCalls != 0 || tasks.attachmentCalls != 0 || tasks.processingRetryCalls != 0 || conversations.lookupCalls != 0 || conversations.createCalls != 0 {
			t.Fatalf("%s %s: status=%d accept=%d upload=%d lookup=%d", tc.method, tc.path, resp.Code, tasks.acceptCalls, tasks.uploadCalls, conversations.lookupCalls)
		}
	}
}

func TestPersonalOutboxAttachmentWritesRejectChangedAccount(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	attachments := &personalAttachmentServiceStub{}
	personal := &personalDataServiceStub{}
	router := gin.New()
	group := router.Group("/api", func(c *gin.Context) { c.Set(principalKey, principal); c.Next() })
	registerPersonalDataRoutes(group, personal)
	RegisterPersonalAttachmentRoutes(group.Group("/personal-data"), attachments)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/personal-data/library/item1"},
		{http.MethodPost, "/api/personal-data/uploads"},
		{http.MethodPut, "/api/personal-data/uploads/upload1/chunks/0"},
		{http.MethodPost, "/api/personal-data/uploads/upload1/commit"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("X-Morde-Actor-ID", "u7654321")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden || attachments.calls != 0 || personal.putCalls != 0 {
			t.Fatalf("%s %s: status=%d attachment calls=%d personal calls=%d", tc.method, tc.path, resp.Code, attachments.calls, personal.putCalls)
		}
	}
}
