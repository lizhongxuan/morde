package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceSyncServiceStub struct {
	manifest workspace.ManifestPage
	changes  workspace.ChangePage
	session  workspace.SyncSession
	progress []workspace.DeviceSyncProgress
	block    []byte
	err      error

	principal        identity.Principal
	workspaceID      string
	sessionID        string
	digest           string
	key              string
	manifestRevision int64
	manifestPrefix   string
	manifestCursor   string
	manifestLimit    int32
	changesAfter     string
	changesWait      time.Duration
	changesLimit     int32
	createCommand    workspace.CreateSyncSession
	putBody          []byte
	manifestCalls    int
	changesCalls     int
	createCalls      int
	getSessionCalls  int
	progressCalls    int
	commitCalls      int
	cancelCalls      int
	putCalls         int
	getBlockCalls    int
	ackBlockCalls    int
	ackActionCalls   int
	ackActionPath    string
	ackAction        workspace.ChangeAction
	ackActionSHA256  string
	ackActionSize    int64
}

func (s *workspaceSyncServiceStub) GetManifest(_ context.Context, principal identity.Principal, workspaceID string, revision int64, prefix, cursor string, limit int32) (workspace.ManifestPage, error) {
	s.capture(principal, workspaceID)
	s.manifestRevision, s.manifestPrefix, s.manifestCursor, s.manifestLimit = revision, prefix, cursor, limit
	s.manifestCalls++
	return s.manifest, s.err
}

func (s *workspaceSyncServiceStub) GetChanges(_ context.Context, principal identity.Principal, workspaceID, after string, wait time.Duration, limit int32) (workspace.ChangePage, error) {
	s.capture(principal, workspaceID)
	s.changesAfter, s.changesWait, s.changesLimit = after, wait, limit
	s.changesCalls++
	return s.changes, s.err
}

func (s *workspaceSyncServiceStub) CreateSyncSession(_ context.Context, principal identity.Principal, workspaceID string, command workspace.CreateSyncSession, key string) (workspace.SyncSession, error) {
	s.capture(principal, workspaceID)
	s.createCommand, s.key = command, key
	s.createCalls++
	return s.session, s.err
}

func (s *workspaceSyncServiceStub) GetSyncSession(_ context.Context, principal identity.Principal, workspaceID, sessionID string) (workspace.SyncSession, error) {
	s.capture(principal, workspaceID)
	s.sessionID = sessionID
	s.getSessionCalls++
	return s.session, s.err
}

func (s *workspaceSyncServiceStub) ListDeviceSyncProgress(_ context.Context, principal identity.Principal, workspaceID string) ([]workspace.DeviceSyncProgress, error) {
	s.capture(principal, workspaceID)
	s.progressCalls++
	if principal.Kind != identity.KindSession {
		return nil, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return s.progress, s.err
}

func (s *workspaceSyncServiceStub) CommitSyncSession(_ context.Context, principal identity.Principal, workspaceID, sessionID, key string) (workspace.SyncSession, error) {
	s.capture(principal, workspaceID)
	s.sessionID, s.key = sessionID, key
	s.commitCalls++
	return s.session, s.err
}

func (s *workspaceSyncServiceStub) CancelSyncSession(_ context.Context, principal identity.Principal, workspaceID, sessionID, key string) (workspace.SyncSession, error) {
	s.capture(principal, workspaceID)
	s.sessionID, s.key = sessionID, key
	s.cancelCalls++
	return s.session, s.err
}

func (s *workspaceSyncServiceStub) PutSyncBlock(_ context.Context, principal identity.Principal, workspaceID, sessionID, digest string, body io.Reader) (int64, error) {
	s.capture(principal, workspaceID)
	s.sessionID, s.digest = sessionID, digest
	s.putBody, _ = io.ReadAll(body)
	s.putCalls++
	return int64(len(s.putBody)), s.err
}

func (s *workspaceSyncServiceStub) GetSyncBlock(_ context.Context, principal identity.Principal, workspaceID, sessionID, digest string) (io.ReadCloser, error) {
	s.capture(principal, workspaceID)
	s.sessionID, s.digest = sessionID, digest
	s.getBlockCalls++
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.block)), nil
}

func (s *workspaceSyncServiceStub) AckDownloadedBlock(_ context.Context, principal identity.Principal, workspaceID, sessionID, digest string) error {
	s.capture(principal, workspaceID)
	s.sessionID, s.digest = sessionID, digest
	s.ackBlockCalls++
	return s.err
}

func (s *workspaceSyncServiceStub) AckDownloadAction(_ context.Context, principal identity.Principal, workspaceID, sessionID, path string, action workspace.ChangeAction, sha256 string, sizeBytes int64) error {
	s.capture(principal, workspaceID)
	s.sessionID, s.ackActionPath, s.ackAction, s.ackActionSHA256, s.ackActionSize = sessionID, path, action, sha256, sizeBytes
	s.ackActionCalls++
	return s.err
}

func (s *workspaceSyncServiceStub) capture(principal identity.Principal, workspaceID string) {
	s.principal, s.workspaceID = principal, workspaceID
}

func newWorkspaceSyncRouter(t *testing.T, service WorkspaceSyncService, options Options) *gin.Engine {
	t.Helper()
	deps := newServices().deps()
	deps.WorkspaceSync = service
	base := validOptions()
	if options.APIMaxBodyBytes != 0 {
		base.APIMaxBodyBytes = options.APIMaxBodyBytes
	}
	if options.Metrics != nil {
		base.Metrics = options.Metrics
	}
	router, err := New(deps, base)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func workspaceSyncRequest(router http.Handler, method, path string, body []byte, contentType, authorization string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "portal.example.test"
	req.RemoteAddr = "192.0.2.1:12345"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if method == http.MethodPost && strings.Contains(path, "/sync-sessions") && !strings.HasSuffix(path, "/ack") {
		req.Header.Set("Idempotency-Key", "workspace-sync-test-01")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Origin", "https://portal.example.test")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func TestWorkspaceSyncManifestAndChangesForwardPrincipalAndBounds(t *testing.T) {
	service := &workspaceSyncServiceStub{
		manifest: workspace.ManifestPage{WorkspaceID: testWorkspaceID, Revision: 4, Files: []workspace.File{}},
		changes:  workspace.ChangePage{Items: []workspace.Change{}},
	}
	router := newWorkspaceSyncRouter(t, service, Options{})

	manifest := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/manifest?revision=4&prefix=src&cursor=abc&limit=99", nil, "", "Bearer "+apiKeyCredential(), nil)
	if manifest.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", manifest.Code, manifest.Body)
	}
	if service.manifestCalls != 1 || service.workspaceID != testWorkspaceID || service.principal.Kind != identity.KindAPIKey ||
		service.manifestRevision != 4 || service.manifestPrefix != "src" || service.manifestCursor != "abc" || service.manifestLimit != 99 {
		t.Fatalf("manifest call = %#v", service)
	}

	changes := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/changes?after=cursor-1&wait_seconds=3&limit=17", nil, "", "Bearer "+apiKeyCredential(), nil)
	if changes.Code != http.StatusOK {
		t.Fatalf("changes status = %d, body = %s", changes.Code, changes.Body)
	}
	if service.changesCalls != 1 || service.workspaceID != testWorkspaceID || service.changesAfter != "cursor-1" || service.changesWait != 3*time.Second || service.changesLimit != 17 {
		t.Fatalf("changes call = %#v", service)
	}

	if response := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/manifest?limit=1001", nil, "", "Bearer "+apiKeyCredential(), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid manifest limit status = %d, body = %s", response.Code, response.Body)
	}
	if response := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/changes", nil, "", "Bearer "+apiKeyCredential(), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("missing change cursor status = %d, body = %s", response.Code, response.Body)
	}
	if response := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/changes?after=a&after=b", nil, "", "Bearer "+apiKeyCredential(), nil); response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate change cursor status = %d, body = %s", response.Code, response.Body)
	}
}

func TestWorkspaceSyncProgressIsPortalOnlyAndOmitsFileDetails(t *testing.T) {
	service := &workspaceSyncServiceStub{progress: []workspace.DeviceSyncProgress{{
		WorkspaceID: testWorkspaceID, DeviceID: "t7654321", SessionID: "p1234567",
		Direction: workspace.SyncDownload, State: workspace.SyncApplying, Stage: workspace.SyncStageApply,
		PlannedBytes: 20, ConfirmedBytes: 20, DownloadActionsTotal: 3, DownloadActionsDone: 2,
	}}}
	router := newWorkspaceSyncRouter(t, service, Options{})
	path := "/api/workspaces/" + testWorkspaceID + "/sync-progress"
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := workspaceSyncRequest(router, http.MethodGet, path, nil, "", "", cookie)
	if response.Code != http.StatusOK || service.progressCalls != 1 || service.principal.Kind != identity.KindSession {
		t.Fatalf("progress response status=%d calls=%d body=%s", response.Code, service.progressCalls, response.Body)
	}
	var body struct {
		Items []workspace.DeviceSyncProgress `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.Items[0].DownloadActionsDone != 2 {
		t.Fatalf("progress response = %#v, err=%v", body, err)
	}
	for _, omitted := range []string{"file_path", "conflicts", "manifest"} {
		if strings.Contains(response.Body.String(), omitted) {
			t.Fatalf("progress response leaked %q: %s", omitted, response.Body)
		}
	}
	if denied := workspaceSyncRequest(router, http.MethodGet, path, nil, "", "Bearer "+apiKeyCredential(), nil); denied.Code != http.StatusForbidden {
		t.Fatalf("API key progress status = %d, body=%s", denied.Code, denied.Body)
	}
	device, _, err := identity.NewCredential(identity.KindDevice, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	if denied := workspaceSyncRequest(router, http.MethodGet, path, nil, "", "Bearer "+device, nil); denied.Code != http.StatusForbidden {
		t.Fatalf("device progress status = %d, body=%s", denied.Code, denied.Body)
	}
	if invalid := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/invalid/sync-progress", nil, "", "", cookie); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid workspace progress status = %d, body=%s", invalid.Code, invalid.Body)
	}
}

func TestWorkspaceSyncSessionActionsAndBinaryBlocks(t *testing.T) {
	digest := strings.Repeat("a", 64)
	service := &workspaceSyncServiceStub{
		session: workspace.SyncSession{
			ID: "p1234567", WorkspaceID: testWorkspaceID, State: workspace.SyncPlanned,
			TransferBlocks: []workspace.TransferBlock{{
				Chunk: workspace.Chunk{SHA256: digest, SizeBytes: 14}, Role: workspace.BlockDownload, Received: false,
			}},
		},
		block: []byte("verified block"),
	}
	router := newWorkspaceSyncRouter(t, service, Options{})
	body := []byte(`{"direction":"upload","policy":"fill_missing","base_revision":2,"files":[]}`)

	create := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions", body, "application/json", "Bearer "+apiKeyCredential(), nil)
	if create.Code != http.StatusCreated || service.createCalls != 1 || service.key == "" || service.createCommand.Direction != workspace.SyncUpload || service.createCommand.BaseRevision != 2 {
		t.Fatalf("create status/call = %d, %#v, body = %s", create.Code, service, create.Body)
	}
	var createBody map[string]json.RawMessage
	if err := json.Unmarshal(create.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("decode sync session response: %v", err)
	}
	if _, exists := createBody["required_blocks"]; exists {
		t.Fatalf("sync session response still includes required_blocks: %s", create.Body)
	}
	var transferBlocks []workspace.TransferBlock
	if value, exists := createBody["transfer_blocks"]; !exists || json.Unmarshal(value, &transferBlocks) != nil || len(transferBlocks) != 1 ||
		transferBlocks[0].SHA256 != digest || transferBlocks[0].SizeBytes != 14 || transferBlocks[0].Role != workspace.BlockDownload || transferBlocks[0].Received {
		t.Fatalf("sync session transfer_blocks = %s", createBody["transfer_blocks"])
	}

	get := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567", nil, "", "Bearer "+apiKeyCredential(), nil)
	if get.Code != http.StatusOK || service.getSessionCalls != 1 || service.sessionID != "p1234567" {
		t.Fatalf("get session status/call = %d, %#v, body = %s", get.Code, service, get.Body)
	}
	commit := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/commit", nil, "", "Bearer "+apiKeyCredential(), nil)
	if commit.Code != http.StatusOK || service.commitCalls != 1 || service.key == "" {
		t.Fatalf("commit status/call = %d, %#v, body = %s", commit.Code, service, commit.Body)
	}
	cancel := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/cancel", nil, "", "Bearer "+apiKeyCredential(), nil)
	if cancel.Code != http.StatusOK || service.cancelCalls != 1 || service.key == "" {
		t.Fatalf("cancel status/call = %d, %#v, body = %s", cancel.Code, service, cancel.Body)
	}

	deviceCredential, _, err := identity.NewCredential(identity.KindDevice, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	put := workspaceSyncRequest(router, http.MethodPut, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/blocks/"+digest,
		[]byte("verified block"), "application/octet-stream", "Bearer "+deviceCredential, nil)
	if put.Code != http.StatusNoContent || service.putCalls != 1 || !bytes.Equal(service.putBody, []byte("verified block")) ||
		service.principal.Kind != identity.KindDevice || service.workspaceID != testWorkspaceID || service.sessionID != "p1234567" || service.digest != digest {
		t.Fatalf("put block status/call = %d, %#v, body = %s", put.Code, service, put.Body)
	}

	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	getBlock := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/blocks/"+digest, nil, "", "", cookie)
	if getBlock.Code != http.StatusOK || getBlock.Header().Get("Content-Type") != "application/octet-stream" || getBlock.Header().Get("Cache-Control") != "no-store" || getBlock.Body.String() != "verified block" {
		t.Fatalf("get block status/headers/body = %d, %#v, %q", getBlock.Code, getBlock.Header(), getBlock.Body.String())
	}
	actionHash := strings.Repeat("b", 64)
	actionSize := int64(17)
	actionBody, _ := json.Marshal(ackWorkspaceSyncDownloadActionRequest{Path: "src/main.go", Action: workspace.ChangeUpsert, SHA256: &actionHash, SizeBytes: &actionSize})
	actionAck := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack", actionBody, "application/json", "Bearer "+deviceCredential, nil)
	if actionAck.Code != http.StatusNoContent || service.ackActionCalls != 1 || service.ackActionPath != "src/main.go" ||
		service.ackAction != workspace.ChangeUpsert || service.ackActionSHA256 != actionHash || service.ackActionSize != 17 || service.principal.Kind != identity.KindDevice {
		t.Fatalf("download action ack status/call = %d, %#v, body = %s", actionAck.Code, service, actionAck.Body)
	}
	deleteAck := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack",
		[]byte(`{"path":"src/old.go","action":"delete","sha256":null,"size_bytes":0}`), "application/json", "Bearer "+deviceCredential, nil)
	if deleteAck.Code != http.StatusNoContent || service.ackActionCalls != 2 || service.ackActionPath != "src/old.go" ||
		service.ackAction != workspace.ChangeDelete || service.ackActionSHA256 != "" || service.ackActionSize != 0 {
		t.Fatalf("download delete action ack status/call = %d, %#v, body = %s", deleteAck.Code, service, deleteAck.Body)
	}
	invalidAction := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack",
		[]byte(`{"path":"../secret","action":"upsert","sha256":"`+actionHash+`","size_bytes":1}`), "application/json", "Bearer "+apiKeyCredential(), nil)
	if invalidAction.Code != http.StatusBadRequest || service.ackActionCalls != 2 {
		t.Fatalf("unsafe download action ack status/call = %d, %#v, body = %s", invalidAction.Code, service, invalidAction.Body)
	}
	missingActionSize := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack",
		[]byte(`{"path":"empty.txt","action":"upsert","sha256":"`+actionHash+`"}`), "application/json", "Bearer "+deviceCredential, nil)
	if missingActionSize.Code != http.StatusBadRequest || service.ackActionCalls != 2 {
		t.Fatalf("download action ack without size = %d, %#v, body = %s", missingActionSize.Code, service, missingActionSize.Body)
	}

	ack := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/blocks/"+digest+"/ack", nil, "", "Bearer "+deviceCredential, nil)
	if ack.Code != http.StatusNoContent || ack.Body.Len() != 0 || service.ackBlockCalls != 1 || service.principal.Kind != identity.KindDevice ||
		service.workspaceID != testWorkspaceID || service.sessionID != "p1234567" || service.digest != digest {
		t.Fatalf("ack downloaded block = %d %s; service = %#v", ack.Code, ack.Body, service)
	}

	ack = workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/blocks/"+digest+"/ack", nil, "application/json", "", cookie)
	if ack.Code != http.StatusNoContent || ack.Body.Len() != 0 || service.ackBlockCalls != 2 || service.principal.Kind != identity.KindSession {
		t.Fatalf("session ack downloaded block = %d %s; service = %#v", ack.Code, ack.Body, service)
	}
	ack = workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/blocks/"+digest+"/ack", []byte("unexpected"), "application/json", "Bearer "+deviceCredential, nil)
	if ack.Code != http.StatusBadRequest || service.ackBlockCalls != 2 {
		t.Fatalf("ack with request body = %d %s; calls = %d", ack.Code, ack.Body, service.ackBlockCalls)
	}

	if response := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions", body, "application/json", "Bearer "+apiKeyCredential(), nil); response.Code != http.StatusCreated {
		t.Fatalf("request repeated with fresh idempotency key: %d %s", response.Code, response.Body)
	}
}

func TestWorkspaceSyncRejectsMalformedRequestsAndPropagatesAuthorizationErrors(t *testing.T) {
	service := &workspaceSyncServiceStub{}
	router := newWorkspaceSyncRouter(t, service, Options{})
	path := "/api/workspaces/" + testWorkspaceID + "/sync-sessions"

	for _, test := range []struct {
		name        string
		body        []byte
		contentType string
		key         string
	}{
		{name: "missing key", body: []byte(`{"direction":"upload","policy":"fill_missing","base_revision":0,"files":[]}`), contentType: "application/json"},
		{name: "unknown field", body: []byte(`{"direction":"upload","policy":"fill_missing","base_revision":0,"files":[],"script":"alert(1)"}`), contentType: "application/json", key: "sync-create-1"},
		{name: "wrong content type", body: []byte(`{}`), contentType: "text/plain", key: "sync-create-2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(test.body))
			req.Host = "portal.example.test"
			req.Header.Set("Content-Type", test.contentType)
			if test.key != "" {
				req.Header.Set("Idempotency-Key", test.key)
			}
			req.Header.Set("Authorization", "Bearer "+apiKeyCredential())
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
		})
	}
	if service.createCalls != 0 {
		t.Fatalf("malformed requests reached service %d times", service.createCalls)
	}
	duplicateKey := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"direction":"upload","policy":"fill_missing","base_revision":0,"files":[]}`))
	duplicateKey.Host = "portal.example.test"
	duplicateKey.Header.Set("Content-Type", "application/json")
	duplicateKey.Header.Add("Idempotency-Key", "sync-key-one")
	duplicateKey.Header.Add("Idempotency-Key", "sync-key-two")
	duplicateKey.Header.Set("Authorization", "Bearer "+apiKeyCredential())
	duplicateResponse := httptest.NewRecorder()
	router.ServeHTTP(duplicateResponse, duplicateKey)
	if duplicateResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate idempotency key status = %d, body = %s", duplicateResponse.Code, duplicateResponse.Body)
	}

	for _, invalidPath := range []string{
		"/api/workspaces/not-a-workspace/manifest",
		"/api/workspaces/" + testWorkspaceID + "/sync-sessions/bad-id",
		"/api/workspaces/" + testWorkspaceID + "/sync-sessions/p1234567/blocks/" + strings.Repeat("A", 64),
	} {
		method := http.MethodGet
		if strings.Contains(invalidPath, "/blocks/") {
			method = http.MethodPut
		}
		response := workspaceSyncRequest(router, method, invalidPath, []byte("x"), "application/octet-stream", "Bearer "+apiKeyCredential(), nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid path %q status = %d, body = %s", invalidPath, response.Code, response.Body)
		}
	}

	service.err = domain.NewError(domain.CodeForbidden, "workspace access denied")
	response := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/manifest", nil, "", "Bearer "+apiKeyCredential(), nil)
	assertError(t, response, http.StatusForbidden, "forbidden")
	if service.principal.UserID != testUserID || service.workspaceID != testWorkspaceID {
		t.Fatalf("authorization call did not receive principal/workspace: %#v", service)
	}
}

func TestWorkspaceSyncDeviceCredentialFenceIsExact(t *testing.T) {
	service := &workspaceSyncServiceStub{manifest: workspace.ManifestPage{WorkspaceID: testWorkspaceID}}
	router := newWorkspaceSyncRouter(t, service, Options{})
	deviceCredential, _, err := identity.NewCredential(identity.KindDevice, testTokenID)
	if err != nil {
		t.Fatal(err)
	}

	response := workspaceSyncRequest(router, http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/manifest", nil, "", "Bearer "+deviceCredential, nil)
	if response.Code != http.StatusOK || service.principal.Kind != identity.KindDevice || service.principal.UserID != testUserID {
		t.Fatalf("device manifest = %d %s; principal = %#v", response.Code, response.Body, service.principal)
	}
	response = workspaceSyncRequest(router, http.MethodDelete, "/api/workspaces/"+testWorkspaceID, nil, "", "Bearer "+deviceCredential, nil)
	assertError(t, response, http.StatusForbidden, "forbidden")

	for _, test := range []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/workspaces/:id/manifest", true},
		{http.MethodGet, "/api/device/workspaces", true},
		{http.MethodGet, "/api/workspaces/:id/changes", true},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions", true},
		{http.MethodGet, "/api/workspaces/:id/sync-sessions/:session_id", true},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions/:session_id/commit", true},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions/:session_id/cancel", true},
		{http.MethodPut, "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest", true},
		{http.MethodGet, "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest", true},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest/ack", true},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions/:session_id/download-actions/ack", true},
		{http.MethodDelete, "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest", false},
		{http.MethodPut, "/api/workspaces/:id/sync-sessions/:session_id/blocks/:digest/ack", false},
		{http.MethodPost, "/api/workspaces/:id/manifest", false},
		{http.MethodPost, "/api/device/workspaces", false},
		{http.MethodGet, "/api/device/workspaces/extra", false},
		{http.MethodPost, "/api/workspaces/:id/sync-sessions/:session_id", false},
		{http.MethodDelete, "/api/workspaces/:id", false},
	} {
		if got := deviceCredentialRoute(test.method, test.path); got != test.want {
			t.Errorf("deviceCredentialRoute(%q, %q) = %t, want %t", test.method, test.path, got, test.want)
		}
	}
}

func TestWorkspaceSyncBodyLimitsAreRouteAware(t *testing.T) {
	service := &workspaceSyncServiceStub{session: workspace.SyncSession{ID: "p1234567", WorkspaceID: testWorkspaceID}}
	router := newWorkspaceSyncRouter(t, service, Options{APIMaxBodyBytes: 128})

	validJSON := []byte(`{"direction":"upload","policy":"fill_missing","base_revision":0,"files":[]}`)
	largeJSON := append(append([]byte(nil), validJSON...), bytes.Repeat([]byte(" "), 1<<20)...)
	create := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions", largeJSON, "application/json", "Bearer "+apiKeyCredential(), nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("1 MiB create body hit global limit: %d %s", create.Code, create.Body)
	}
	actionHash := strings.Repeat("d", 64)
	actionSize := int64(1)
	actionBody, _ := json.Marshal(ackWorkspaceSyncDownloadActionRequest{Path: "nested/project/file.go", Action: workspace.ChangeUpsert, SHA256: &actionHash, SizeBytes: &actionSize})
	actionAck := workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack", actionBody, "application/json", "Bearer "+apiKeyCredential(), nil)
	if actionAck.Code != http.StatusNoContent {
		t.Fatalf("download action body hit global limit: %d %s", actionAck.Code, actionAck.Body)
	}
	oversizedAction := bytes.Repeat([]byte(" "), workspaceSyncActionBodyLimit+1)
	actionAck = workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions/p1234567/download-actions/ack", oversizedAction, "application/json", "Bearer "+apiKeyCredential(), nil)
	assertError(t, actionAck, http.StatusRequestEntityTooLarge, "request_too_large")
	tooLargeJSON := append(append([]byte(nil), validJSON...), bytes.Repeat([]byte(" "), workspaceSyncJSONBodyLimit)...)
	create = workspaceSyncRequest(router, http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/sync-sessions", tooLargeJSON, "application/json", "Bearer "+apiKeyCredential(), nil)
	assertError(t, create, http.StatusRequestEntityTooLarge, "request_too_large")

	digest := strings.Repeat("b", 64)
	blockPath := "/api/workspaces/" + testWorkspaceID + "/sync-sessions/p1234567/blocks/" + digest
	block := bytes.Repeat([]byte("x"), 256)
	put := workspaceSyncRequest(router, http.MethodPut, blockPath, block, "application/octet-stream", "Bearer "+apiKeyCredential(), nil)
	if put.Code != http.StatusNoContent || !bytes.Equal(service.putBody, block) {
		t.Fatalf("block hit global limit: %d %s", put.Code, put.Body)
	}
	oversizedBlock := bytes.Repeat([]byte("x"), int(workspaceSyncBlockBodyLimit+1))
	put = workspaceSyncRequest(router, http.MethodPut, blockPath, oversizedBlock, "application/octet-stream", "Bearer "+apiKeyCredential(), nil)
	assertError(t, put, http.StatusRequestEntityTooLarge, "request_too_large")
}

func TestWorkspaceSyncSessionUserCanUploadRawBlock(t *testing.T) {
	service := &workspaceSyncServiceStub{}
	router := newWorkspaceSyncRouter(t, service, Options{})
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	path := "/api/workspaces/" + testWorkspaceID + "/sync-sessions/p1234567/blocks/" + strings.Repeat("c", 64)
	response := workspaceSyncRequest(router, http.MethodPut, path, []byte("block"), "application/octet-stream", "", cookie)
	if response.Code != http.StatusNoContent || service.putCalls != 1 || service.principal.Kind != identity.KindSession {
		t.Fatalf("session block upload = %d %s; service = %#v", response.Code, response.Body, service)
	}
}
