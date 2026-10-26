package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type entryTicketHostSharing struct {
	app.HostSharingService
	desktopCalls  int
	terminalCalls int
}

func (s *entryTicketHostSharing) IssueObservationTicket(context.Context, identity.Principal, string) (app.EnterInfo, error) {
	s.desktopCalls++
	return app.EnterInfo{URL: "https://h1234567.hosts.test/__ch/auth?t=desktop", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (s *entryTicketHostSharing) IssueTerminalTicket(context.Context, identity.Principal, string) (app.EnterInfo, error) {
	s.terminalCalls++
	return app.EnterInfo{URL: "https://h1234567.hosts.test/__ch/auth?t=terminal&next=%2F__ch%2Fterminal%2F", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func TestHostEntryTicketTargetIsStrictAndPermissionSpecific(t *testing.T) {
	services := newServices()
	deps := services.deps()
	sharing := &entryTicketHostSharing{}
	deps.HostSharing = sharing
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}

	desktop := requestWithCookie(router, http.MethodPost, "/api/hosts/"+testHostID+"/observation-ticket", cookie, "https://portal.example.test")
	if desktop.Code != http.StatusOK || sharing.desktopCalls != 1 || sharing.terminalCalls != 0 {
		t.Fatalf("default desktop ticket = %d calls=%d/%d", desktop.Code, sharing.desktopCalls, sharing.terminalCalls)
	}
	terminal := requestWithCookie(router, http.MethodPost, "/api/hosts/"+testHostID+"/observation-ticket?target=terminal", cookie, "https://portal.example.test")
	if terminal.Code != http.StatusOK || sharing.desktopCalls != 1 || sharing.terminalCalls != 1 {
		t.Fatalf("terminal ticket = %d calls=%d/%d", terminal.Code, sharing.desktopCalls, sharing.terminalCalls)
	}
	for _, path := range []string{
		"/api/hosts/" + testHostID + "/observation-ticket?target=unknown",
		"/api/hosts/" + testHostID + "/observation-ticket?target=terminal&target=desktop",
		"/api/hosts/" + testHostID + "/observation-ticket?next=%2F__ch%2Fterminal%2F",
	} {
		response := requestWithCookie(router, http.MethodPost, path, cookie, "https://portal.example.test")
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid ticket target %s = %d %s; want 400", path, response.Code, response.Body)
		}
	}
	if sharing.desktopCalls != 1 || sharing.terminalCalls != 1 {
		t.Fatalf("invalid target reached issuer: calls=%d/%d", sharing.desktopCalls, sharing.terminalCalls)
	}
}
