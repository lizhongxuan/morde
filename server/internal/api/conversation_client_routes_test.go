package api

import (
	"net/http"
	"testing"
)

func TestClientCredentialsCanUseOutboxRoutesOnlyForUserOperations(t *testing.T) {
	for _, item := range []struct{ method, path string }{
		{http.MethodGet, "/api/conversations/:id/tasks"},
		{http.MethodGet, "/api/conversations/:id/message-uploads/:client_message_id"},
		{http.MethodPut, "/api/conversations/:id/message-uploads/:client_message_id"},
		{http.MethodPut, "/api/conversations/:id/message-uploads/:client_message_id/chunks/:sequence"},
		{http.MethodPost, "/api/conversations/:id/message-uploads/:client_message_id/commit"},
		{http.MethodPut, "/api/conversations/:id/messages/:message_id/attachments/:client_attachment_id"},
		{http.MethodPost, "/api/conversations/:id/messages/:message_id/retry-processing"},
		{http.MethodGet, "/api/projects/:id/conversations"},
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, "/api/projects/:id/members"},
		{http.MethodGet, "/api/projects/:id/resources"},
		{http.MethodPost, "/api/projects/:id/conversations"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/messages"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/tasks"},
		{http.MethodPost, "/api/projects/:id/conversations/:conversation_id/messages"},
		{http.MethodPost, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/retry-processing"},
		{http.MethodPut, "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id"},
		{http.MethodPut, "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id/chunks/:sequence"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id"},
		{http.MethodPost, "/api/projects/:id/conversations/:conversation_id/message-uploads/:client_message_id/commit"},
		{http.MethodPut, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id"},
		{http.MethodPut, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/chunks/:sequence"},
		{http.MethodPost, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/commit"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments/:client_attachment_id/content"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments-by-id/:attachment_id"},
		{http.MethodGet, "/api/projects/:id/conversations/:conversation_id/messages/:message_id/attachments-by-id/:attachment_id/content"},
	} {
		if !clientCredentialRoute(item.method, item.path) {
			t.Errorf("client credential rejected %s %s", item.method, item.path)
		}
	}
	if clientCredentialRoute(http.MethodPost, "/api/admin/settings/model-default") {
		t.Fatal("admin action was allowed")
	}
}
