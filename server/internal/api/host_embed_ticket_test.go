package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type embedTicketHostSharing struct {
	app.HostSharingService
	command app.HostEmbedTicketCommand
	calls   int
}

func (s *embedTicketHostSharing) IssueEmbedTicket(_ context.Context, _ identity.Principal, command app.HostEmbedTicketCommand) (app.HostEmbedTicket, error) {
	s.command = command
	s.calls++
	return app.HostEmbedTicket{
		ExchangeURL: "https://h1234567.example-hosts.test/__ch/embed-auth",
		Ticket:      strings.Repeat("A", 43),
		EmbedNonce:  strings.Repeat("B", 32),
		Targets:     []string{app.HostEmbedTargetBrowser, app.HostEmbedTargetWorkspace},
		ExpiresAt:   time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC),
	}, nil
}

func TestHostEmbedTicketEndpointBindsTrustedContextAndIsNoStore(t *testing.T) {
	services := newServices()
	deps := services.deps()
	sharing := &embedTicketHostSharing{}
	deps.HostSharing = sharing
	deps.Conversations = &conversationServiceStub{}
	options := validOptions()
	options.ServiceInstanceID = "11111111-1111-4111-8111-111111111111"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := requestWithCookieBody(router, http.MethodPost, "/api/hosts/"+testHostID+"/embed-tickets",
		`{"conversation_id":"c1234567","target":"browser","targets":["browser","workspace"]}`, &http.Cookie{Name: portalCookieName, Value: sessionCredential()}, "https://portal.example.test")
	if response.Code != http.StatusCreated || sharing.calls != 1 {
		t.Fatalf("issue embed ticket = %d %s, calls=%d", response.Code, response.Body.String(), sharing.calls)
	}
	if response.Header().Get("Cache-Control") != "no-store, private" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Expires") != "0" {
		t.Fatalf("ticket cache headers = %#v", response.Header())
	}
	var body hostEmbedTicketResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ExchangeURL != "https://h1234567.example-hosts.test/__ch/embed-auth" || strings.Contains(body.ExchangeURL, "?") ||
		body.Ticket != strings.Repeat("A", 43) || body.EmbedNonce != strings.Repeat("B", 32) || len(body.Targets) != 2 || body.ExpiresAt.IsZero() {
		t.Fatalf("ticket response = %#v", body)
	}
	if sharing.command.HostID != testHostID || sharing.command.ConversationID != "c1234567" ||
		sharing.command.Target != app.HostEmbedTargetBrowser || len(sharing.command.Targets) != 2 || sharing.command.Targets[1] != app.HostEmbedTargetWorkspace || sharing.command.ServiceInstanceID != options.ServiceInstanceID ||
		sharing.command.PortalOrigin != "https://portal.example.test" {
		t.Fatalf("trusted ticket binding = %#v", sharing.command)
	}
}

func TestHostEmbedTicketEndpointRejectsUntrustedOrIncompleteRequests(t *testing.T) {
	services := newServices()
	deps := services.deps()
	sharing := &embedTicketHostSharing{}
	deps.HostSharing = sharing
	deps.Conversations = &conversationServiceStub{}
	options := validOptions()
	options.ServiceInstanceID = "11111111-1111-4111-8111-111111111111"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	for _, test := range []struct {
		name, path, body, origin string
		wantStatus               int
	}{
		{name: "unknown target", path: "/api/hosts/" + testHostID + "/embed-tickets", body: `{"conversation_id":"c1234567","target":"printer","targets":["browser"]}`, origin: "https://portal.example.test", wantStatus: http.StatusBadRequest},
		{name: "arbitrary query", path: "/api/hosts/" + testHostID + "/embed-tickets?next=https://evil.test", body: `{"conversation_id":"c1234567","target":"browser","targets":["browser"]}`, origin: "https://portal.example.test", wantStatus: http.StatusBadRequest},
		{name: "untrusted origin", path: "/api/hosts/" + testHostID + "/embed-tickets", body: `{"conversation_id":"c1234567","target":"browser","targets":["browser"]}`, origin: "https://evil.test", wantStatus: http.StatusForbidden},
		{name: "client supplied identity", path: "/api/hosts/" + testHostID + "/embed-tickets", body: `{"conversation_id":"c1234567","target":"browser","targets":["browser"],"actor_id":"u1234567"}`, origin: "https://portal.example.test", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := requestWithCookieBody(router, http.MethodPost, test.path, test.body, cookie, test.origin)
			if response.Code != test.wantStatus {
				t.Fatalf("request = %d %s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
		})
	}
	if sharing.calls != 0 {
		t.Fatalf("invalid request reached ticket issuer %d times", sharing.calls)
	}
}
