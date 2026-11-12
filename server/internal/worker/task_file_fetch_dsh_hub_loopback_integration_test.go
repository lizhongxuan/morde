package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

const (
	a30FileFetchAnswer        = "A30 local file retrieval finished."
	a30FileFetchWaitingAnswer = "A30 file retrieval is waiting for the PC to reconnect."
	a30FileFetchFailedAnswer  = "A30 file retrieval failed; no retrievable copy was published."
)

// TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge covers the
// real Task/DSH -> PostgreSQL -> Hub WebSocket -> desktop bridge -> morde-sync
// path. The model, user, files, server and helper are synthetic and loopback-only.
func TestA30TaskFileFetchRunsFromChatThroughPostgresHubAndPCBridge(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL")) == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; local PostgreSQL Task/Hub/file.fetch integration was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	store, pool := newF01TemporaryStore(t)
	owner := createA30FileFetchUser(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic owner Task execution: %v", err)
	}
	if _, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: "p1234567", OwnerID: owner.ID, Name: "A30 loopback model stub", BaseURL: "http://127.0.0.1:9/v1",
		ModelName: "a30-deterministic-test-model", APIKeyCipher: bytes.Repeat([]byte{0x51}, 32),
	}); err != nil {
		t.Fatalf("create synthetic model connection for the real DSH Task context: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true WHERE id=$1`, "p1234567"); err != nil {
		t.Fatalf("mark synthetic model as checked without contacting a provider: %v", err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	hubService, err := app.NewHubService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialServiceWithHub(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 10, LoginWindow: time.Minute,
	}, f01DefaultHosts{}, hubService)
	if err != nil {
		t.Fatalf("create local credential service: %v", err)
	}
	clientLogin, err := app.NewClientLoginService(store, credentials, nil, hubService)
	if err != nil {
		t.Fatal(err)
	}
	taskService, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	conversationService, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	router, err := api.New(api.Deps{
		Hosts: f01UnusedHostService{}, TLSAuthorizer: f01UnusedTLSAuthorizer{}, Verifier: credentials,
		Auth: a30PortalAuth{credentials: credentials}, ClientLogin: clientLogin, Tasks: taskService, Conversations: conversationService,
		TaskFileFetch: taskService, Hub: hubService, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		ServiceInstanceID: serviceInstanceID,
		SessionTTL:        time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "hosts.example.test",
		PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatalf("create local Portal and Hub router: %v", err)
	}
	chunkBarrier := newA30ChunkBarrier(router)
	portal := httptest.NewServer(chunkBarrier)
	defer func() {
		hubService.Close()
		portal.Close()
	}()
	httpClient := portal.Client()

	clientCredential, err := a30ClientLogin(ctx, t, httpClient, portal.URL, owner.Name, "A30 synthetic password 2026", "ios")
	if err != nil {
		t.Fatalf("perform local ordered iOS ClientLogin: %v", err)
	}
	webSession, err := a30CreateWebSession(ctx, httpClient, portal.URL, clientCredential)
	if err != nil {
		t.Fatalf("exchange Client credential for Portal Session: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, webSession)
	if err != nil || principal.Kind != identity.KindSession || principal.ClientInstallationID == "" || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic iOS Portal Session: principal=%+v err=%v", principal, err)
	}
	// The iOS ClientWebSession submits normal chat. Owner-only file status and
	// content use an ordinary owner Portal Session, which has no installation ID.
	ownerSession, err := a30PortalLogin(ctx, httpClient, portal.URL, owner.Name, "A30 synthetic password 2026")
	if err != nil {
		t.Fatalf("log into the local owner Portal Session for owner-only file status: %v", err)
	}
	ownerPrincipal, err := credentials.VerifySession(ctx, ownerSession)
	if err != nil || ownerPrincipal.Kind != identity.KindSession || ownerPrincipal.ClientInstallationID != "" || ownerPrincipal.UserID != owner.ID {
		t.Fatalf("verify local owner Portal Session: principal=%+v err=%v", ownerPrincipal, err)
	}

	tempRoot := t.TempDir()
	fileRoot := filepath.Join(tempRoot, "a30-selected-files")
	reports := filepath.Join(fileRoot, "reports")
	if err := os.MkdirAll(reports, 0o700); err != nil {
		t.Fatal(err)
	}
	fullSource := a30SyntheticPDF(700<<10, 0x51)
	changeSource := a30SyntheticPDF(3<<20, 0x62)
	revokeSource := a30SyntheticPDF(8<<20, 0x73)
	tooLargeSource := a30SyntheticPDF(int(app.TaskFileFetchMaxFileBytes+1), 0x24)
	for name, content := range map[string][]byte{
		"full.pdf": fullSource, "change.pdf": changeSource, "revoke.pdf": revokeSource,
		"too-large.pdf": tooLargeSource,
	} {
		if err := os.WriteFile(filepath.Join(reports, name), content, 0o600); err != nil {
			t.Fatalf("write synthetic file %s: %v", name, err)
		}
	}
	outsideSource := a30SyntheticPDF(128<<10, 0x35)
	if err := os.WriteFile(filepath.Join(tempRoot, "outside.pdf"), outsideSource, 0o600); err != nil {
		t.Fatal(err)
	}

	helperBinary := filepath.Join(t.TempDir(), "morde-sync")
	build := exec.CommandContext(ctx, "go", "build", "-o", helperBinary, "./cmd/morde-sync")
	build.Dir = filepath.Join("..", "..", "..", "runtime", "morde-sync")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the production local morde-sync helper: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	pcBridge, err := startA30PCBridge(ctx, t, portal.URL, owner.Name, "A30 synthetic password 2026", fileRoot, helperBinary)
	if err != nil {
		t.Fatalf("start the production local PC bridge: %v", err)
	}
	defer func() {
		pcBridge.close()
	}()
	installationID := pcBridge.installationID
	if !domain.ValidClientInstallationID(installationID) {
		t.Fatalf("PC bridge returned invalid local installation id %q", installationID)
	}
	if !waitA30Until(ctx, func() bool { return hubService.InstallationOnline(owner.ID, installationID) }) {
		t.Fatal("production PC bridge did not establish its authenticated local Hub connection")
	}

	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatalf("open isolated Workspace block storage: %v", err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close isolated Workspace block storage: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x3a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "http://127.0.0.1:9"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	toolBroker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil, hubService)
	if err != nil {
		t.Fatal(err)
	}
	observedTools := &a30ObservedTaskTools{CloudDSHToolBroker: toolBroker}
	model := newA30FileFetchModel()
	node, dshScript := f01NodeRuntimePaths(t)
	instances, accountRuntimes, supervisor := newF01ConversationRuntime(t, store, node, dshScript, "a30-file-fetch-supervisor",
		bytes.Repeat([]byte{0x4b}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: "a30-file-fetch-supervisor", AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true},
		Tools: observedTools, Children: children, Completion: toolBroker,
	})
	if err != nil {
		t.Fatal(err)
	}
	mainConversation, _, err := conversationService.EnsureMain(ctx, principal,
		app.EnsureMainConversationCommand{IdempotencyKey: "a30-file-fetch-main-001"})
	if err != nil {
		t.Fatalf("ensure synthetic main conversation: %v", err)
	}
	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: &f01ConversationRouteClassifier{}, WorkerID: "a30-file-fetch-route-worker",
		LeaseDuration: time.Minute, PollInterval: 5 * time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	chatClient := &a30PortalSessionClient{http: httpClient, baseURL: portal.URL, credential: webSession}
	ownerClient := &a30PortalSessionClient{http: httpClient, baseURL: portal.URL, credential: ownerSession}
	sequence := 0
	runChatTask := func(title, messageID, objective, relativePath string, beforeTool func(string) error) app.TaskDetails {
		t.Helper()
		sequence++
		body := map[string]any{
			"client_message_id": messageID, "model_id": "p1234567", "content": fmt.Sprintf("A30 %s: %s", title, objective),
			"budget": map[string]int64{"active_seconds": 180, "tool_calls": 8, "model_tokens": 4000},
		}
		response, responseBody, requestErr := chatClient.do(ctx, http.MethodPost,
			"/api/conversations/"+mainConversation.ConversationID+"/messages", body, "", map[string]string{
				"Idempotency-Key": messageID, "X-Morde-Actor-ID": owner.ID, "X-Morde-Service-Instance-ID": serviceInstanceID,
			})
		if requestErr != nil {
			t.Fatalf("submit ordinary local chat message: %v", requestErr)
		}
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("ordinary chat message status=%d body=%s", response.StatusCode, responseBody)
		}
		_ = response.Body.Close()
		var accepted struct {
			Message struct {
				ID            string `json:"id"`
				TaskID        string `json:"task_id"`
				RoutingStatus string `json:"routing_status"`
			} `json:"message"`
		}
		if err := json.Unmarshal(responseBody, &accepted); err != nil || accepted.Message.ID == "" || accepted.Message.TaskID != "" || accepted.Message.RoutingStatus != "pending" {
			t.Fatalf("ordinary chat message receipt=%s err=%v", responseBody, err)
		}
		var message conversation.Message
		var found, routed bool
		var err error
		routeDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(routeDeadline) {
			if _, err = routeWorker.RunOnce(ctx); err != nil {
				t.Fatalf("route ordinary chat message into a Task: %v", err)
			}
			message, found, err = store.FindConversationMessageByClientID(ctx, owner.ID, "", mainConversation.ConversationID, messageID, serviceInstanceID)
			if err != nil || !found {
				t.Fatalf("read routed chat message=%+v found=%t err=%v", message, found, err)
			}
			if message.RoutingStatus == "routed" && message.TaskID != "" {
				routed = true
				break
			}
			if message.RoutingStatus != "pending" {
				t.Fatalf("chat message finished routing unexpectedly: %+v", message)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !routed {
			t.Fatalf("ordinary chat message was not routed before deadline: %+v", message)
		}
		model.configure(message.TaskID, relativePath, beforeTool)
		details, err := runA30TaskWorker(ctx, store, executor, "a30-file-fetch-worker", sequence, owner.ID, message.TaskID)
		if err != nil {
			t.Fatalf("execute chat Task %s through the real local DSH runtime: %v", message.TaskID, err)
		}
		return details
	}

	// Main chat: a genuine Portal message routes into a Task, DSH selects the
	// online PC action, and the production helper sends two acknowledged chunks.
	fullDetails := runChatTask("main file fetch", "a30-chat-success-001", "A30_MAIN_FETCH reports/full.pdf", "reports/full.pdf", nil)
	if fullDetails.Task.Status != task.StatusCompleted || fullDetails.Task.Result != a30FileFetchAnswer {
		model.mu.Lock()
		modelCalls := model.calls[fullDetails.Task.ID]
		model.mu.Unlock()
		t.Fatalf("full local file.fetch Task status=%q waiting_reason=%q model_calls=%d runtime=%s",
			fullDetails.Task.Status, fullDetails.Task.WaitingReason, modelCalls,
			f01AccountRuntimeFrameSummary([]byte(supervisor.output()))+" tools={"+observedTools.summary()+"}")
	}
	fullFetch := a30GetOnlyFetch(t, ctx, ownerClient, fullDetails.Task.ID)
	fullHash := sha256.Sum256(fullSource)
	if fullFetch.Status != string(task.FileFetchCompleted) || fullFetch.ContentState != string(task.FileFetchContentRetained) ||
		fullFetch.ParseStatus != string(task.FileFetchParseRawOnly) || fullFetch.SizeBytes == nil || *fullFetch.SizeBytes != int64(len(fullSource)) ||
		fullFetch.SourceSHA256 == nil || *fullFetch.SourceSHA256 != hex.EncodeToString(fullHash[:]) || fullFetch.ConfirmedBytes != int64(len(fullSource)) ||
		fullFetch.SourceDirectoryName == nil || *fullFetch.SourceDirectoryName != filepath.Base(fileRoot) {
		t.Fatalf("completed owner file.fetch projection=%+v", fullFetch)
	}
	fullContentResponse, fullContent, err := ownerClient.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/tasks/%s/file-fetches/%s/content", fullDetails.Task.ID, fullFetch.FetchID), nil, "", nil)
	if err != nil || fullContentResponse.StatusCode != http.StatusOK || fullContentResponse.Header.Get("Content-Type") != "application/pdf" || !bytes.Equal(fullContent, fullSource) {
		t.Fatalf("owner content download status=%v bytes=%d err=%v", responseStatus(fullContentResponse), len(fullContent), err)
	}
	_ = fullContentResponse.Body.Close()
	if modelOutput := model.outputFor(fullDetails.Task.ID); strings.Contains(strings.ToLower(modelOutput), "ticket") ||
		!strings.Contains(modelOutput, "transfer succeeded") || !strings.Contains(modelOutput, "raw-only") {
		t.Fatalf("model received an unsafe or incomplete file.fetch result: %q", modelOutput)
	}
	boundSessionResponse, _, requestErr := chatClient.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/tasks/%s/file-fetches", fullDetails.Task.ID), nil, "", nil)
	if boundSessionResponse != nil {
		_ = boundSessionResponse.Body.Close()
	}
	if requestErr != nil || boundSessionResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("iOS ClientWebSession accessed owner-only fetch list: status=%v err=%v", responseStatus(boundSessionResponse), requestErr)
	}
	boundSessionResponse, _, requestErr = chatClient.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/tasks/%s/file-fetches/%s/content", fullDetails.Task.ID, fullFetch.FetchID), nil, "", nil)
	if boundSessionResponse != nil {
		_ = boundSessionResponse.Body.Close()
	}
	if requestErr != nil || boundSessionResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("iOS ClientWebSession accessed owner-only file content: status=%v err=%v", responseStatus(boundSessionResponse), requestErr)
	}
	clientCredentialResponse, _, requestErr := a30HTTPDo(ctx, httpClient, portal.URL,
		http.MethodGet, fmt.Sprintf("/api/tasks/%s/file-fetches", fullDetails.Task.ID), nil, nil, clientCredential, nil)
	if clientCredentialResponse != nil {
		_ = clientCredentialResponse.Body.Close()
	}
	if requestErr != nil || clientCredentialResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("iOS Client credential accessed owner-only fetch list: status=%v err=%v", responseStatus(clientCredentialResponse), requestErr)
	}
	clientCredentialResponse, _, requestErr = a30HTTPDo(ctx, httpClient, portal.URL,
		http.MethodGet, fmt.Sprintf("/api/tasks/%s/file-fetches/%s/content", fullDetails.Task.ID, fullFetch.FetchID), nil, nil, clientCredential, nil)
	if clientCredentialResponse != nil {
		_ = clientCredentialResponse.Body.Close()
	}
	if requestErr != nil || clientCredentialResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("iOS Client credential accessed owner-only file content: status=%v err=%v", responseStatus(clientCredentialResponse), requestErr)
	}

	// Offline PC: DSH had already received the authorized tool definition, then
	// the same authenticated installation drops its Hub connection before dispatch.
	offlineDetails := runChatTask("offline PC", "a30-chat-offline-001", "A30_OFFLINE_FETCH reports/full.pdf", "reports/full.pdf", func(_ string) error {
		if _, err := pcBridge.command("disconnect"); err != nil {
			return err
		}
		if !waitA30Until(ctx, func() bool { return !hubService.InstallationOnline(owner.ID, installationID) }) {
			return errors.New("Hub did not observe the PC disconnect")
		}
		return nil
	})
	if offlineDetails.Task.Status != task.StatusCompleted || offlineDetails.Task.Result != a30FileFetchWaitingAnswer {
		t.Fatalf("offline file.fetch Task status=%q waiting_reason=%q", offlineDetails.Task.Status, offlineDetails.Task.WaitingReason)
	}
	offlineFetch := a30GetOnlyFetch(t, ctx, ownerClient, offlineDetails.Task.ID)
	if offlineFetch.Status != string(task.FileFetchWaitingDevice) || offlineFetch.ContentState != string(task.FileFetchContentNone) ||
		a30OwnerContentStatus(t, ctx, ownerClient, offlineDetails.Task.ID, offlineFetch.FetchID) != http.StatusNotFound {
		t.Fatalf("offline PC produced a retrievable copy: %+v", offlineFetch)
	}
	if _, err := pcBridge.command("connect"); err != nil {
		t.Fatalf("reconnect production local PC Hub bridge: %v", err)
	}
	if !waitA30Until(ctx, func() bool { return hubService.InstallationOnline(owner.ID, installationID) }) {
		t.Fatal("production PC bridge did not reconnect after the offline scenario")
	}

	// Source drift after one durable chunk must fail closed and leave no copy.
	changeDone := make(chan error, 1)
	changeDetails := runChatTask("source changed", "a30-chat-change-001", "A30_CHANGED_FETCH reports/change.pdf", "reports/change.pdf", func(taskID string) error {
		gate := chunkBarrier.install(taskID)
		interleaveCtx, interleaveCancel := context.WithTimeout(ctx, 45*time.Second)
		go func() {
			defer interleaveCancel()
			select {
			case <-gate.entered:
			case <-interleaveCtx.Done():
				changeDone <- interleaveCtx.Err()
				return
			}
			defer gate.release()
			fetch, err := waitA30FetchProgress(interleaveCtx, ownerClient, taskID, app.TaskFileFetchChunkBytes)
			if err != nil {
				changeDone <- err
				return
			}
			if a30OwnerContentStatus(t, interleaveCtx, ownerClient, taskID, fetch.FetchID) != http.StatusNotFound {
				changeDone <- errors.New("staged content became readable before commit")
				return
			}
			replacement := a30SyntheticPDF(len(changeSource), 0x64)
			temporary := filepath.Join(reports, "change.pdf.replacement")
			if err := os.WriteFile(temporary, replacement, 0o600); err != nil {
				changeDone <- err
				return
			}
			if err := os.Rename(temporary, filepath.Join(reports, "change.pdf")); err != nil {
				changeDone <- err
				return
			}
			gate.release()
			changeDone <- nil
		}()
		return nil
	})
	if err := <-changeDone; err != nil {
		t.Fatalf("source-change interleaving: %v", err)
	}
	changeFetch := a30GetOnlyFetch(t, ctx, ownerClient, changeDetails.Task.ID)
	if changeDetails.Task.Status != task.StatusCompleted || changeDetails.Task.Result != a30FileFetchFailedAnswer ||
		changeFetch.Status != string(task.FileFetchFailed) ||
		changeFetch.FailureReason != task.FileFetchFailureSourceChanged || changeFetch.ConfirmedBytes != 0 ||
		changeFetch.ContentState != string(task.FileFetchContentNone) ||
		a30OwnerContentStatus(t, ctx, ownerClient, changeDetails.Task.ID, changeFetch.FetchID) != http.StatusNotFound {
		t.Fatalf("source mutation did not fail closed: task=%q fetch=%+v", changeDetails.Task.Status, changeFetch)
	}

	// Oversize input and traversal are rejected without publishing bytes.
	largeDetails := runChatTask("oversize file", "a30-chat-oversize-001", "A30_OVERSIZE_FETCH reports/too-large.pdf", "reports/too-large.pdf", nil)
	largeFetch := a30GetOnlyFetch(t, ctx, ownerClient, largeDetails.Task.ID)
	if largeDetails.Task.Status != task.StatusCompleted || largeDetails.Task.Result != a30FileFetchFailedAnswer ||
		largeFetch.Status != string(task.FileFetchFailed) ||
		largeFetch.ContentState != string(task.FileFetchContentNone) ||
		a30OwnerContentStatus(t, ctx, ownerClient, largeDetails.Task.ID, largeFetch.FetchID) != http.StatusNotFound {
		t.Fatalf("oversize PC file produced a retrievable copy: task=%q fetch=%+v", largeDetails.Task.Status, largeFetch)
	}
	traversalDetails := runChatTask("path traversal", "a30-chat-traversal-001", "A30_TRAVERSAL_FETCH ../outside.pdf", "../outside.pdf", nil)
	if traversalDetails.Task.Status != task.StatusCompleted || traversalDetails.Task.Result != a30FileFetchFailedAnswer {
		t.Fatalf("traversal refusal unexpectedly failed the Task: status=%q reason=%q", traversalDetails.Task.Status, traversalDetails.Task.WaitingReason)
	}
	if traversalFetches := a30ListFetches(t, ctx, ownerClient, traversalDetails.Task.ID); len(traversalFetches) != 0 {
		t.Fatalf("traversal reached the PC/server transfer path: %+v", traversalFetches)
	}

	// Revoking the local file grant during a multi-chunk transfer interrupts the
	// live Hub call. A partial stage must remain unavailable to the owner API.
	revokeDone := make(chan error, 1)
	revokeDetails := runChatTask("grant revoked in flight", "a30-chat-revoke-001", "A30_REVOKE_FETCH reports/revoke.pdf", "reports/revoke.pdf", func(taskID string) error {
		gate := chunkBarrier.install(taskID)
		interleaveCtx, interleaveCancel := context.WithTimeout(ctx, 45*time.Second)
		go func() {
			defer interleaveCancel()
			select {
			case <-gate.entered:
			case <-interleaveCtx.Done():
				revokeDone <- interleaveCtx.Err()
				return
			}
			defer gate.release()
			fetch, err := waitA30FetchProgress(interleaveCtx, ownerClient, taskID, app.TaskFileFetchChunkBytes)
			if err != nil {
				revokeDone <- err
				return
			}
			if a30OwnerContentStatus(t, interleaveCtx, ownerClient, taskID, fetch.FetchID) != http.StatusNotFound {
				revokeDone <- errors.New("in-flight content became readable before grant revocation")
				return
			}
			if _, err := pcBridge.command("revoke"); err != nil {
				revokeDone <- err
				return
			}
			gate.release()
			revokeDone <- nil
		}()
		return nil
	})
	if err := <-revokeDone; err != nil {
		t.Fatalf("grant-revocation interleaving: %v", err)
	}
	revokeWaitCtx, revokeWaitCancel := context.WithTimeout(ctx, 5*time.Second)
	revokeFetch, err := waitA30FetchStatus(revokeWaitCtx, ownerClient, revokeDetails.Task.ID, string(task.FileFetchFailed))
	revokeWaitCancel()
	if err != nil {
		t.Fatalf("wait for PC ticket abort after local file-grant revocation: %v", err)
	}
	var revokeStagingChunks int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_file_fetch_chunks WHERE fetch_id=$1`, revokeFetch.FetchID).Scan(&revokeStagingChunks); err != nil {
		t.Fatalf("count revoked file-fetch staging chunks: %v", err)
	}
	var revokedTicketHash []byte
	if err := pool.QueryRow(ctx, `SELECT ticket_hash FROM task_file_fetches WHERE fetch_id=$1`, revokeFetch.FetchID).Scan(&revokedTicketHash); err != nil {
		t.Fatalf("read revoked file-fetch ticket hash: %v", err)
	}
	if revokeFetch.ConfirmedBytes != 0 || revokeFetch.FailureReason != task.FileFetchFailureAccessDenied ||
		revokeFetch.ContentState != string(task.FileFetchContentNone) || revokeStagingChunks != 0 || len(revokedTicketHash) != 0 ||
		a30OwnerContentStatus(t, ctx, ownerClient, revokeDetails.Task.ID, revokeFetch.FetchID) != http.StatusNotFound {
		t.Fatalf("revoked in-flight transfer retained staging or published content: task=%q fetch=%+v chunks=%d ticket_hash_bytes=%d",
			revokeDetails.Task.Status, revokeFetch, revokeStagingChunks, len(revokedTicketHash))
	}
	if revokeDetails.Task.Status != task.StatusWaitingUser || revokeDetails.Task.WaitingReason != "hub_action_unknown" ||
		revokeDetails.Task.Result == a30FileFetchAnswer {
		t.Fatalf("uncertain interrupted Hub call was falsely finalized: status=%q waiting_reason=%q", revokeDetails.Task.Status, revokeDetails.Task.WaitingReason)
	}
}

type a30PortalSessionClient struct {
	http       *http.Client
	baseURL    string
	credential string
}

type a30PortalAuth struct {
	api.AuthService
	credentials *app.CredentialService
}

func (auth a30PortalAuth) Login(ctx context.Context, name, password, ip string) (api.LoginResult, error) {
	result, err := auth.credentials.Login(ctx, name, password, ip)
	if err != nil {
		return api.LoginResult{}, err
	}
	return api.LoginResult{
		Credential: result.Credential, ExpiresAt: result.ExpiresAt,
		User: api.UserView{ID: result.User.ID, Name: result.User.Name, Role: result.User.Role,
			Status: result.User.Status, MustChangePassword: result.User.MustChangePassword},
	}, nil
}

func (client *a30PortalSessionClient) do(ctx context.Context, method, path string, body any, bearer string, extra map[string]string) (*http.Response, []byte, error) {
	var cookie *http.Cookie
	if client.credential != "" {
		cookie = &http.Cookie{Name: "ch_portal", Value: client.credential}
	}
	return a30HTTPDo(ctx, client.http, client.baseURL, method, path, body, cookie, bearer, extra)
}

type a30ChunkGate struct {
	entered     chan struct{}
	releaseCh   chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (gate *a30ChunkGate) release() {
	gate.releaseOnce.Do(func() { close(gate.releaseCh) })
}

type a30ChunkBarrier struct {
	next  http.Handler
	mu    sync.RWMutex
	gates map[string]*a30ChunkGate
}

func newA30ChunkBarrier(next http.Handler) *a30ChunkBarrier {
	return &a30ChunkBarrier{next: next, gates: make(map[string]*a30ChunkGate)}
}

func (barrier *a30ChunkBarrier) install(taskID string) *a30ChunkGate {
	gate := &a30ChunkGate{entered: make(chan struct{}), releaseCh: make(chan struct{})}
	barrier.mu.Lock()
	barrier.gates[taskID] = gate
	barrier.mu.Unlock()
	return gate
}

// ServeHTTP pauses the first successful chunk response after the real router
// has committed it. This makes source mutation and grant revocation races
// deterministic without replacing the production API or helper.
func (barrier *a30ChunkBarrier) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != http.MethodPut || len(parts) != 7 || parts[0] != "api" || parts[1] != "tasks" ||
		parts[3] != "file-fetches" || parts[5] != "chunks" || parts[6] != "0" {
		barrier.next.ServeHTTP(writer, request)
		return
	}
	barrier.mu.RLock()
	gate := barrier.gates[parts[2]]
	barrier.mu.RUnlock()
	if gate == nil {
		barrier.next.ServeHTTP(writer, request)
		return
	}
	response := httptest.NewRecorder()
	barrier.next.ServeHTTP(response, request)
	if response.Code >= http.StatusOK && response.Code < http.StatusMultipleChoices {
		gate.enteredOnce.Do(func() { close(gate.entered) })
		select {
		case <-gate.releaseCh:
		case <-request.Context().Done():
		}
	}
	for key, values := range response.Header() {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(response.Code)
	_, _ = writer.Write(response.Body.Bytes())
}

type a30FileFetchResponse struct {
	FetchID             string  `json:"fetch_id"`
	RelativePath        string  `json:"relative_path"`
	SourceDirectoryName *string `json:"source_directory_name"`
	SourceSHA256        *string `json:"source_sha256"`
	SizeBytes           *int64  `json:"size_bytes"`
	ConfirmedBytes      int64   `json:"confirmed_bytes"`
	Status              string  `json:"status"`
	FailureReason       string  `json:"failure_reason"`
	ContentState        string  `json:"content_state"`
	ParseStatus         string  `json:"parse_status"`
}

type a30FileFetchModel struct {
	mu      sync.Mutex
	paths   map[string]string
	hooks   map[string]func(string) error
	calls   map[string]int
	outputs map[string]string
}

type a30ObservedTaskTools struct {
	*app.CloudDSHToolBroker
	mu             sync.Mutex
	calls          int
	lastErrorCode  string
	lastErrorType  string
	lastToolStatus app.AgentToolResultStatus
}

func (tools *a30ObservedTaskTools) Execute(ctx context.Context, current task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	result, err := tools.CloudDSHToolBroker.Execute(ctx, current, call)
	tools.mu.Lock()
	defer tools.mu.Unlock()
	tools.calls++
	tools.lastToolStatus = result.Status
	if err != nil {
		tools.lastErrorCode, tools.lastErrorType = string(domain.CodeOf(err)), fmt.Sprintf("%T", err)
	}
	return result, err
}

func (tools *a30ObservedTaskTools) summary() string {
	tools.mu.Lock()
	defer tools.mu.Unlock()
	return fmt.Sprintf("execute_calls=%d last_status=%q error_code=%q error_type=%q",
		tools.calls, tools.lastToolStatus, tools.lastErrorCode, tools.lastErrorType)
}

func newA30FileFetchModel() *a30FileFetchModel {
	return &a30FileFetchModel{paths: make(map[string]string), hooks: make(map[string]func(string) error),
		calls: make(map[string]int), outputs: make(map[string]string)}
}

func (model *a30FileFetchModel) configure(taskID, path string, hook func(string) error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.paths[taskID], model.hooks[taskID] = path, hook
}

func (model *a30FileFetchModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	if call.Task == nil || call.Task.TaskID == "" {
		return app.ModelResponse{}, errors.New("local synthetic model received no Task fence")
	}
	taskID := call.Task.TaskID
	model.mu.Lock()
	model.calls[taskID]++
	round, path, hook := model.calls[taskID], model.paths[taskID], model.hooks[taskID]
	model.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 3}
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode local DSH model request: %w", err)
	}
	if round == 1 {
		toolName := ""
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Name, "morde_hub_file_fetch_") {
				toolName = tool.Name
				break
			}
		}
		if toolName == "" || path == "" {
			return app.ModelResponse{}, fmt.Errorf("local Hub file.fetch tool was not offered for Task %s", taskID)
		}
		promptContainsPath := false
		for _, message := range request.Messages {
			if message.Role != "user" {
				continue
			}
			for _, block := range message.Content {
				promptContainsPath = promptContainsPath || block.Type == "text" && strings.Contains(block.Text, path)
			}
		}
		if !promptContainsPath {
			return app.ModelResponse{}, fmt.Errorf("ordinary chat prompt for Task %s did not request %q", taskID, path)
		}
		if hook != nil {
			if err := hook(taskID); err != nil {
				return app.ModelResponse{}, fmt.Errorf("prepare local file.fetch scenario: %w", err)
			}
		}
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "a30-file-fetch-" + taskID,
			Name: toolName, Arguments: fmt.Sprintf(`{"path":%q}`, path)}}, StopReason: "tool_calls", Usage: usage}, nil
	}
	output, found := f01ToolResult(call.RawRequest, "a30-file-fetch-"+taskID)
	if !found {
		return app.ModelResponse{}, fmt.Errorf("local DSH model request for Task %s has no prior tool result", taskID)
	}
	model.mu.Lock()
	model.outputs[taskID] = output
	model.mu.Unlock()
	return app.ModelResponse{Text: a30FileFetchAnswerForToolOutput(output), StopReason: "stop", Usage: usage}, nil
}

func a30FileFetchAnswerForToolOutput(output string) string {
	var result struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(output), &result)
	switch result.Status {
	case string(task.FileFetchCompleted):
		return a30FileFetchAnswer
	case string(task.FileFetchWaitingDevice):
		return a30FileFetchWaitingAnswer
	case string(task.FileFetchFailed), string(task.FileFetchCancelled), string(task.FileFetchCancelling):
		return a30FileFetchFailedAnswer
	}
	if strings.Contains(strings.ToLower(output), "transfer succeeded") {
		return a30FileFetchAnswer
	}
	return a30FileFetchFailedAnswer
}

func (model *a30FileFetchModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

func (model *a30FileFetchModel) outputFor(taskID string) string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.outputs[taskID]
}

type a30PCBridge struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	lines          chan string
	installationID string
	mu             sync.Mutex
}

func startA30PCBridge(ctx context.Context, t *testing.T, baseURL, account, password, fileRoot, helper string) (*a30PCBridge, error) {
	t.Helper()
	packageRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "clients", "desktop", "harness", "dsh-nanomuse"))
	if err != nil {
		return nil, err
	}
	cloudModulePath := filepath.Join(packageRoot, "lib", "cloud.js")
	if _, err := os.Stat(cloudModulePath); err != nil {
		return nil, fmt.Errorf("locate built production PC bridge: %w", err)
	}
	cloudModuleURL := "file://" + filepath.ToSlash(cloudModulePath)
	script := `
import { createInterface } from 'node:readline'
const { default: NanomuseCloud } = await import(process.env.A30_CLOUD_MODULE)
const fetch = globalThis.fetch
globalThis.fetch = async (input, init) => {
  const response = await fetch(input, init)
  const rawURL = typeof input === 'string' ? input : input.url
  const path = new URL(rawURL).pathname
  const route = path === '/api/capabilities' || path === '/api/auth/client-login' ? path : '/other'
  process.stdout.write('HTTP ' + JSON.stringify({ route, status: response.status }) + '\n')
  return response
}
const credentials = new Map()
const ctx = {
  reflect: { provide() {} },
  logger: { info() {}, warn() {}, debug() {}, error() {} },
  credentials: {
    async set(ref, value) { credentials.set(String(ref), value) },
    async resolve(ref) { return credentials.has(String(ref)) ? { value: credentials.get(String(ref)) } : undefined },
    async unset(ref) { credentials.delete(String(ref)) },
  },
  settings: { async update() {}, async mutate() {}, describe() { return [] } },
  get() { return undefined }, inject() {}, effect(fn) { fn() }, on() { return () => undefined },
}
const cloud = new NanomuseCloud(ctx, {
  baseURL: process.env.A30_SERVER_URL,
  deviceName: 'A30 synthetic PC',
  statePath: process.env.A30_STATE_DIR + '/cloud.json',
})
cloud.state = {}
async function waitConnected(want) {
  const until = Date.now() + 15000
  while (Date.now() < until) {
    const state = cloud.mordeHubView()
    if (state.connected === want) return state
    await new Promise((resolve) => setTimeout(resolve, 25))
  }
  throw new Error('local Hub connection did not reach the requested state')
}
let initial
let stage = 'hub_login'
try {
  await cloud.mordeHubLogin(process.env.A30_SERVER_URL, process.env.A30_ACCOUNT, process.env.A30_PASSWORD)
  stage = 'grants'
  await cloud.mordeHubUpdateGrants({ file_root: process.env.A30_FILE_ROOT, files: true })
  stage = 'wait_connected'
  initial = await waitConnected(true)
} catch (error) {
  const code = /^[a-z0-9_]{1,80}$/i.test(String(error?.code ?? '')) ? String(error.code) : 'unknown'
  process.stdout.write('FATAL ' + JSON.stringify({ stage, code, status: Number.isInteger(error?.status) ? error.status : 0 }) + '\n')
  process.exit(1)
}
process.stdout.write('READY ' + JSON.stringify({ installation_id: initial.installationID, target_grant_version: initial.targetGrantVersion }) + '\n')
const lines = createInterface({ input: process.stdin })
for await (const line of lines) {
  let command
  try { command = JSON.parse(line) } catch { process.stdout.write('ERROR invalid command\n'); continue }
  try {
    if (command.action === 'disconnect') cloud.mordeHub?.stop('A30 local offline scenario')
    else if (command.action === 'connect') { cloud.startMordeHub(); await waitConnected(true) }
    else if (command.action === 'revoke') { await cloud.mordeHubUpdateGrants({ files: false }); await waitConnected(true) }
    else if (command.action === 'shutdown') { await cloud.mordeHubSignOut(); process.stdout.write('ACK shutdown {}\n'); break }
    else throw new Error('unknown local control')
    const state = cloud.mordeHubView()
    process.stdout.write('ACK ' + command.action + ' ' + JSON.stringify({ connected: state.connected, target_grant_version: state.targetGrantVersion, files: state.grants.files }) + '\n')
  } catch {
    process.stdout.write('ERROR ' + command.action + '\n')
  }
}
cloud.mordeHub?.stop('A30 local fixture stopped')
`
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script)
	cmd.Dir = packageRoot
	cmd.Env = a30ChildEnv(map[string]string{
		"MORDE_SYNC_BIN":   helper,
		"A30_CLOUD_MODULE": cloudModuleURL,
		"A30_SERVER_URL":   baseURL,
		"A30_FILE_ROOT":    fileRoot,
		"A30_ACCOUNT":      account,
		"A30_PASSWORD":     password,
		"A30_STATE_DIR":    t.TempDir(),
	})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	bridge := &a30PCBridge{cmd: cmd, stdin: stdin, lines: make(chan string, 32)}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		defer close(bridge.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			bridge.lines <- scanner.Text()
		}
	}()
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	var diagnostics []string
	for {
		select {
		case line, ok := <-bridge.lines:
			if !ok {
				_ = cmd.Wait()
				return nil, errors.New("production local PC bridge exited before Hub login")
			}
			if strings.HasPrefix(line, "FATAL ") {
				return nil, fmt.Errorf("production local PC bridge initialization failed: %s http=%v", strings.TrimPrefix(line, "FATAL "), diagnostics)
			}
			if strings.HasPrefix(line, "HTTP ") {
				diagnostics = append(diagnostics, strings.TrimPrefix(line, "HTTP "))
				continue
			}
			if !strings.HasPrefix(line, "READY ") {
				continue
			}
			var ready struct {
				InstallationID string `json:"installation_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "READY ")), &ready); err != nil {
				return nil, fmt.Errorf("decode production PC bridge readiness receipt: %w", err)
			}
			if ready.InstallationID == "" {
				return nil, errors.New("production PC bridge returned an empty installation ID")
			}
			bridge.installationID = ready.InstallationID
			return bridge, nil
		case <-readyCtx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("wait for local PC bridge login: %w", readyCtx.Err())
		}
	}
}

func (bridge *a30PCBridge) command(action string) (map[string]any, error) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if _, err := fmt.Fprintf(bridge.stdin, `{"action":%q}`+"\n", action); err != nil {
		return nil, err
	}
	for {
		select {
		case line, ok := <-bridge.lines:
			if !ok {
				return nil, errors.New("production local PC bridge exited")
			}
			if strings.HasPrefix(line, "ERROR ") {
				return nil, fmt.Errorf("production local PC bridge rejected %s", action)
			}
			if !strings.HasPrefix(line, "ACK "+action+" ") {
				continue
			}
			var receipt map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "ACK "+action+" ")), &receipt); err != nil {
				return nil, fmt.Errorf("decode production PC bridge receipt: %w", err)
			}
			return receipt, nil
		case <-time.After(20 * time.Second):
			return nil, fmt.Errorf("timed out waiting for PC bridge %s receipt", action)
		}
	}
}

func (bridge *a30PCBridge) close() {
	if bridge == nil || bridge.cmd == nil || bridge.cmd.ProcessState != nil {
		return
	}
	_, _ = bridge.command("shutdown")
	_ = bridge.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- bridge.cmd.Wait() }()
	select {
	case waitErr := <-done:
		_ = waitErr
	case <-time.After(5 * time.Second):
		_ = bridge.cmd.Process.Kill()
		waitErr := <-done
		_ = waitErr
	}
}

func createA30FileFetchUser(t *testing.T, ctx context.Context, store interface {
	CreateUser(context.Context, identity.User) (identity.User, error)
	UpdateUser(context.Context, string, identity.AccountStatus, identity.User) (identity.User, error)
}) identity.User {
	t.Helper()
	userID, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("A30 synthetic password 2026"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	value := identity.User{ID: userID, Name: "a30-file-fetch-" + userID, PasswordHash: string(hash), Role: identity.RoleUser,
		Status: identity.StatusPending, QuotaHosts: 3, QuotaCPUMilli: 100_000, QuotaMemMiB: 200_000,
		DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048}
	value, err = store.CreateUser(ctx, value)
	if err != nil {
		t.Fatalf("create synthetic local user: %v", err)
	}
	value.Status = identity.StatusApproved
	value, err = store.UpdateUser(ctx, "system", identity.StatusPending, value)
	if err != nil {
		t.Fatalf("approve synthetic local user: %v", err)
	}
	return value
}

func a30ClientLogin(ctx context.Context, t *testing.T, client *http.Client, baseURL, account, password, platform string) (string, error) {
	t.Helper()
	body := map[string]any{
		"name": account, "password": password, "platform": platform, "display_name": "A30 synthetic " + platform,
		"client_instance_id": "a30f0010-0000-4000-8000-000000000001", "attempt_generation": 1,
	}
	response, responseBody, err := a30HTTPDo(ctx, client, baseURL, http.MethodPost, "/api/auth/client-login", body, nil, "", map[string]string{
		"Idempotency-Key": "a30-client-login-" + platform,
	})
	if err != nil {
		return "", err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ClientLogin status=%d body=%s", response.StatusCode, responseBody)
	}
	var result struct {
		ClientCredential string `json:"client_credential"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("decode ClientLogin receipt: %w", err)
	}
	if result.ClientCredential == "" {
		return "", errors.New("decode ClientLogin receipt: credential is empty")
	}
	return result.ClientCredential, nil
}

func a30CreateWebSession(ctx context.Context, client *http.Client, baseURL, clientCredential string) (string, error) {
	response, body, err := a30HTTPDo(ctx, client, baseURL, http.MethodPost, "/api/auth/client-web-session", map[string]any{}, nil,
		clientCredential, nil)
	if err != nil {
		return "", err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Client Web Session status=%d body=%s", response.StatusCode, body)
	}
	var result struct {
		Credential string `json:"web_session_credential"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode Client Web Session receipt: %w", err)
	}
	if result.Credential == "" {
		return "", errors.New("decode Client Web Session receipt: credential is empty")
	}
	return result.Credential, nil
}

func a30PortalLogin(ctx context.Context, client *http.Client, baseURL, account, password string) (string, error) {
	response, body, err := a30HTTPDo(ctx, client, baseURL, http.MethodPost, "/api/auth/login",
		map[string]string{"name": account, "password": password}, nil, "", nil)
	if err != nil {
		return "", err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("owner Portal Session login status=%d body=%s", response.StatusCode, body)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "ch_portal" && cookie.Value != "" {
			return cookie.Value, nil
		}
	}
	return "", errors.New("owner Portal Session login returned no ch_portal cookie")
}

func a30HTTPDo(ctx context.Context, client *http.Client, baseURL, method, path string, body any,
	cookie *http.Cookie, bearer string, extra map[string]string) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Origin", baseURL)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return response, nil, err
	}
	return response, responseBody, nil
}

func a30SyntheticPDF(size int, value byte) []byte {
	if size < 9 {
		size = 9
	}
	content := bytes.Repeat([]byte{value}, size)
	copy(content, []byte("%PDF-1.7\n"))
	return content
}

func runA30TaskWorker(ctx context.Context, store interface {
	TaskLeaseStore
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}, executor TaskExecutor, workerPrefix string, sequence int, ownerID, taskID string) (app.TaskDetails, error) {
	workerID := fmt.Sprintf("%s-%02d", workerPrefix, sequence)
	worker, err := NewTaskWorker(TaskWorkerOptions{Store: store, Executor: executor, WorkerID: workerID,
		LeaseDuration: 30 * time.Second, PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 1, Logger: zap.NewNop()})
	if err != nil {
		return app.TaskDetails{}, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	details, waitErr := waitA30Task(workerCtx, store, ownerID, taskID)
	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
			waitErr = errors.Join(waitErr, fmt.Errorf("TaskWorker stopped: %w", runErr))
		}
	case <-time.After(5 * time.Second):
		waitErr = errors.Join(waitErr, errors.New("TaskWorker did not stop after the synthetic Task"))
	}
	return details, waitErr
}

func waitA30Task(ctx context.Context, store interface {
	TaskLeaseStore
	GetTask(context.Context, string, string) (app.TaskDetails, error)
}, ownerID, taskID string) (app.TaskDetails, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		details, err := store.GetTask(ctx, taskID, ownerID)
		if err != nil {
			return app.TaskDetails{}, err
		}
		switch details.Task.Status {
		case task.StatusCompleted, task.StatusFailed, task.StatusWaitingUser:
			return details, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return app.TaskDetails{}, ctx.Err()
		}
	}
}

func (client *a30PortalSessionClient) doTaskRequest(ctx context.Context, method, path string, body any, extra map[string]string) (*http.Response, []byte, error) {
	return client.do(ctx, method, path, body, "", extra)
}

func a30ListFetches(t *testing.T, ctx context.Context, client *a30PortalSessionClient, taskID string) []a30FileFetchResponse {
	t.Helper()
	response, body, err := client.doTaskRequest(ctx, http.MethodGet, fmt.Sprintf("/api/tasks/%s/file-fetches", taskID), nil, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("list owner Task file fetches: status=%v err=%v body=%s", responseStatus(response), err, body)
	}
	_ = response.Body.Close()
	if bytes.Contains(bytes.ToLower(body), []byte("ticket")) || bytes.Contains(bytes.ToLower(body), []byte("ticket_hash")) {
		t.Fatal("owner file.fetch projection leaked a ticket field")
	}
	var result struct {
		Items []a30FileFetchResponse `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode owner Task file.fetch list: %v", err)
	}
	return result.Items
}

func a30GetOnlyFetch(t *testing.T, ctx context.Context, client *a30PortalSessionClient, taskID string) a30FileFetchResponse {
	t.Helper()
	items := a30ListFetches(t, ctx, client, taskID)
	if len(items) != 1 {
		t.Fatalf("Task %s has %d file.fetch records, want exactly one: %+v", taskID, len(items), items)
	}
	return items[0]
}

func a30OwnerContentStatus(t *testing.T, ctx context.Context, client *a30PortalSessionClient, taskID, fetchID string) int {
	t.Helper()
	response, _, err := client.doTaskRequest(ctx, http.MethodGet,
		fmt.Sprintf("/api/tasks/%s/file-fetches/%s/content", taskID, fetchID), nil, nil)
	if err != nil {
		t.Errorf("request owner file.fetch content: %v", err)
		return 0
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func waitA30FetchProgress(ctx context.Context, client *a30PortalSessionClient, taskID string, minConfirmed int64) (a30FileFetchResponse, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, body, err := client.doTaskRequest(ctx, http.MethodGet, fmt.Sprintf("/api/tasks/%s/file-fetches", taskID), nil, nil)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil && response.StatusCode == http.StatusOK {
			var result struct {
				Items []a30FileFetchResponse `json:"items"`
			}
			if json.Unmarshal(body, &result) == nil && len(result.Items) == 1 && result.Items[0].ConfirmedBytes >= minConfirmed {
				return result.Items[0], nil
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return a30FileFetchResponse{}, ctx.Err()
		}
	}
}

func waitA30FetchStatus(ctx context.Context, client *a30PortalSessionClient, taskID, wantStatus string) (a30FileFetchResponse, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, body, err := client.doTaskRequest(ctx, http.MethodGet, fmt.Sprintf("/api/tasks/%s/file-fetches", taskID), nil, nil)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil && response.StatusCode == http.StatusOK {
			var result struct {
				Items []a30FileFetchResponse `json:"items"`
			}
			if json.Unmarshal(body, &result) == nil && len(result.Items) == 1 && result.Items[0].Status == wantStatus {
				return result.Items[0], nil
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return a30FileFetchResponse{}, ctx.Err()
		}
	}
}

func waitA30Until(ctx context.Context, condition func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return false
		}
	}
}

func responseStatus(response *http.Response) any {
	if response == nil {
		return "no response"
	}
	return response.StatusCode
}

func a30ChildEnv(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			environment = append(environment, item)
			continue
		}
		if _, overridden := overrides[name]; !overridden {
			environment = append(environment, item)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}
