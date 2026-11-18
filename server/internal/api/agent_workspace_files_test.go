package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type agentWorkspaceFilesHTTPStub struct {
	view      app.AgentWorkspaceView
	page      app.AgentWorkspaceFilePage
	filePath  string
	listPath  string
	openPath  string
	principal identity.Principal
}

func (s *agentWorkspaceFilesHTTPStub) GetContext(_ context.Context, principal identity.Principal) (app.AgentWorkspaceView, error) {
	s.principal = principal
	return s.view, nil
}

func (s *agentWorkspaceFilesHTTPStub) List(_ context.Context, principal identity.Principal, workspaceID, relativePath, cursor string, limit int) (app.AgentWorkspaceFilePage, error) {
	s.principal, s.listPath = principal, relativePath
	return s.page, nil
}

func (s *agentWorkspaceFilesHTTPStub) Open(_ context.Context, principal identity.Principal, workspaceID, relativePath string) (*os.File, os.FileInfo, error) {
	s.principal, s.openPath = principal, relativePath
	file, err := os.Open(s.filePath)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	return file, info, err
}

func TestAgentWorkspaceFileRoutesUsePortalSessionAndStreamRelativeFiles(t *testing.T) {
	services := newServices()
	root := t.TempDir()
	filePath := filepath.Join(root, "summary.txt")
	if err := os.WriteFile(filePath, []byte("workspace contents"), 0600); err != nil {
		t.Fatal(err)
	}
	files := &agentWorkspaceFilesHTTPStub{
		view:     app.AgentWorkspaceView{ID: testWorkspaceID, RootPath: "~/workspace", State: "ready"},
		page:     app.AgentWorkspaceFilePage{WorkspaceID: testWorkspaceID, Path: "reports", Entries: []app.AgentWorkspaceFileEntry{{Name: "summary.txt", Path: "reports/summary.txt", Kind: "file"}}},
		filePath: filePath,
	}
	deps := services.deps()
	deps.AgentWorkspaceFiles = files
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	workspaceResponse := requestWithCookie(router, http.MethodGet, "/api/me/agent-workspace", cookie, "https://portal.example.test")
	if workspaceResponse.Code != http.StatusOK || files.principal.Kind != identity.KindSession {
		t.Fatalf("GET Agent Workspace = %d %s, principal=%#v", workspaceResponse.Code, workspaceResponse.Body, files.principal)
	}
	entriesResponse := requestWithCookie(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/entries?path=reports&limit=10", cookie, "https://portal.example.test")
	if entriesResponse.Code != http.StatusOK || files.listPath != "reports" {
		t.Fatalf("GET entries = %d %s, path=%q", entriesResponse.Code, entriesResponse.Body, files.listPath)
	}
	fileResponse := requestWithCookie(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/files/reports/summary.txt", cookie, "https://portal.example.test")
	if fileResponse.Code != http.StatusOK || fileResponse.Body.String() != "workspace contents" || files.openPath != "reports/summary.txt" ||
		fileResponse.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET file = %d %q, path=%q, headers=%v", fileResponse.Code, fileResponse.Body.String(), files.openPath, fileResponse.Header())
	}
}

func TestAgentWorkspaceFileRoutesRejectClientCredential(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.AgentWorkspaceFiles = &agentWorkspaceFilesHTTPStub{}
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	clientCredential, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	response := requestWithAgentWorkspaceAuthHeader(router, http.MethodGet, "/api/me/agent-workspace", "Bearer "+clientCredential, "")
	if response.Code == http.StatusOK {
		t.Fatalf("client credential accessed Agent Workspace: %s", response.Body)
	}
}

func requestWithAgentWorkspaceAuthHeader(router http.Handler, method, path, authorization, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set("Authorization", authorization)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}
