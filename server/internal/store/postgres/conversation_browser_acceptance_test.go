package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/execution"
	domainhost "morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/worker"
)

// This opt-in fixture uses the real HTTP handlers, database, route worker and
// result publisher. The model and task execution are local synthetic stand-ins.
// It is never registered by the production Portal.
func TestConversationBrowserAcceptance(t *testing.T) {
	if os.Getenv("MORDE_BROWSER_ACCEPTANCE") != "1" {
		t.Skip("set MORDE_BROWSER_ACCEPTANCE=1 for the isolated browser fixture")
	}
	gin.SetMode(gin.ReleaseMode)
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != execution.StateEnabled {
		t.Fatalf("new browser acceptance account execution state = %q, err=%v; want enabled by default", control.State, err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	capacity, err := storage.NewWorkspaceCapacity(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	workspaceOps, err := app.NewWorkspaceService(store, blocks, nativeAcceptanceWorkspaceCurrentDeleter{}, capacity, audit, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	personalQuota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalQuotaUser, err := app.NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalAttachments, err := app.NewPersonalAttachmentService(store, personalQuota)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	crossClientAcceptance := os.Getenv("MORDE_BROWSER_CROSS_CLIENT") == "1"
	model := browserAcceptanceModel{routeRetry: &browserAcceptanceRouteRetryModel{}}
	if crossClientAcceptance {
		model.routeBarrier = &browserAcceptanceRouteBarrier{expected: 4, ready: make(chan struct{})}
	}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := app.NewOnboardingService(store, nativeAcceptanceHosts{}, models, "small")
	if err != nil {
		t.Fatal(err)
	}
	checked, err := models.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "本地验收模型", BaseURL: "https://example.invalid/v1", ModelName: "morde-browser-fixture", APIKey: "synthetic-test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,50000,4)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CheckModelCapability(ctx, principal, checked.ID, "browser-model-probe-0001", "all"); err != nil {
		t.Fatal(err)
	}
	projectResumeMode := os.Getenv("MORDE_BROWSER_PROJECT_ATTACHMENT_RESUME") == "1"
	var projects *app.ProjectService
	if projectResumeMode {
		projects, err = app.NewProjectService(store)
		if err != nil {
			t.Fatal(err)
		}
	}
	var tasks *app.TaskService
	if projects != nil {
		tasks, err = app.NewTaskService(store, credentials, models, projects)
	} else {
		tasks, err = app.NewTaskService(store, credentials, models)
	}
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	mainConversation, _, err := conversations.EnsureMain(ctx, principal, app.EnsureMainConversationCommand{
		IdempotencyKey: "browser-acceptance-main-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MORDE_BROWSER_LONG_HISTORY") == "1" {
		serviceInstanceID, err := store.ServiceInstanceID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seededMessageIDs := make([]string, 0, 840)
		for index := 0; index < 840; index++ {
			prefix := fmt.Sprintf("PC long-history row=%04d ", index)
			if index == 0 {
				prefix += browserAcceptanceLongHistorySentinel + " "
			}
			content := prefix + strings.Repeat("q", 10*1024-len(prefix))
			clientMessageID := fmt.Sprintf("browser-long-history-%04d", index)
			message, replayed, err := tasks.AcceptConversationMessage(ctx, principal, mainConversation.ConversationID,
				app.AcceptConversationMessageCommand{ServiceInstanceID: serviceInstanceID, ClientMessageID: clientMessageID,
					IdempotencyKey: clientMessageID, Content: content, ModelID: checked.ID,
					Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}})
			if err != nil || replayed {
				t.Fatalf("seed PC long-history message %d: replayed=%t err=%v", index, replayed, err)
			}
			seededMessageIDs = append(seededMessageIDs, message.ID)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE conversation_messages AS message
SET delivery_status='applied', created_at=clock_timestamp()-interval '1 day'+seed.position::double precision*interval '1 millisecond'
FROM unnest($1::text[]) WITH ORDINALITY AS seed(message_id,position) WHERE message.id=seed.message_id`, seededMessageIDs); err != nil {
			t.Fatalf("mark PC long-history messages as durable: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET state='routed' WHERE message_id=ANY($1::text[])`, seededMessageIDs); err != nil {
			t.Fatalf("settle PC long-history message routes: %v", err)
		}
		var messageCount, sentinelCount, historyBytes int
		if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE content LIKE $2),sum(octet_length(content))
FROM conversation_messages WHERE conversation_id=$1`, mainConversation.ConversationID, "%"+browserAcceptanceLongHistorySentinel+"%").
			Scan(&messageCount, &sentinelCount, &historyBytes); err != nil || messageCount != 840 || sentinelCount != 1 || historyBytes <= 8*1024*1024 {
			t.Fatalf("seeded PC long history: messages=%d sentinel=%d bytes=%d err=%v", messageCount, sentinelCount, historyBytes, err)
		}
	}
	var projectConversations *app.ProjectConversationService
	var projectAttachments *app.ProjectConversationAttachmentService
	var projectAttachmentResume *browserProjectAttachmentResumeFixture
	if projectResumeMode {
		projectValue := createProjectStoreFixture(t, ctx, store, owner.ID,
			"本地项目附件恢复验收", "loopback iOS project attachment recovery", "browser-project-attachment-create")
		projectConversations, err = app.NewProjectConversationService(store, projects, tasks)
		if err != nil {
			t.Fatal(err)
		}
		shared, _, err := projectConversations.Create(ctx, principal, projectValue.ID,
			app.CreateProjectConversationCommand{Title: "附件恢复验收", IdempotencyKey: "browser-project-attachment-conversation"})
		if err != nil {
			t.Fatal(err)
		}
		projectAttachments, err = app.NewProjectConversationAttachmentService(store)
		if err != nil {
			t.Fatal(err)
		}
		projectAttachmentResume = &browserProjectAttachmentResumeFixture{
			projectID: projectValue.ID, conversationID: shared.ID,
		}
	}
	execution, err := app.NewExecutionControlService(store, audit)
	if err != nil {
		t.Fatal(err)
	}
	var activity *browserAcceptanceActivity
	var executor worker.TaskExecutor = browserAcceptanceExecutor{store: store}
	var obsoletePredecessorErrorResponses atomic.Int32
	var attachmentResume *browserAttachmentResumeFixture
	var messageUploadResume *browserMessageUploadResumeFixture
	var messageReceiptRetry *browserMessageReceiptRetryFixture
	if os.Getenv("MORDE_BROWSER_ATTACHMENT_RESUME") == "1" {
		attachmentResume = &browserAttachmentResumeFixture{}
	}
	if os.Getenv("MORDE_BROWSER_MESSAGE_UPLOAD_RESUME") == "1" {
		messageUploadResume = newBrowserMessageUploadResumeFixture(mainConversation.ConversationID)
	}
	if os.Getenv("MORDE_BROWSER_MESSAGE_RECEIPT_RETRY") == "1" {
		messageReceiptRetry = &browserMessageReceiptRetryFixture{
			conversationID:           mainConversation.ConversationID,
			requireExplicitRetry:     os.Getenv("MORDE_BROWSER_MESSAGE_EXPLICIT_RETRY") == "1",
			clientOfflineBeforeRetry: os.Getenv("MORDE_BROWSER_MESSAGE_CLIENT_OFFLINE") == "1",
		}
	}
	if os.Getenv("MORDE_BROWSER_ACTIVITY") == "1" {
		activity, err = newBrowserAcceptanceActivity(store, sealer, mainConversation.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		activity.testUserName = owner.Name
		activity.executionControl = execution
		activity.ownerPrincipal = principal
		if retryRun := activity.runs["mcp-retry"]; retryRun != nil {
			retryHost := createRunningControlHost(t, ctx, store, owner.ID, domainhost.ControlAI, 0)
			runtimeInstanceID := "browser-m09-retry-" + retryHost.ID
			if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, retryHost.ID, runtimeInstanceID); err != nil {
				t.Fatalf("seed local MCP retry Host Runtime identity: %v", err)
			}
			if err := store.RecordHostReplicaHeartbeat(ctx, retryHost.ID, retryHost.RuntimeGeneration); err != nil {
				t.Fatalf("seed local MCP retry Host Runtime heartbeat: %v", err)
			}
			retryRun.retryHostID = retryHost.ID
		}
		if revokeRun := activity.runs["approval-host-member-revoked"]; revokeRun != nil {
			hostOwner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
			revokeHost := createRunningControlHost(t, ctx, store, hostOwner.ID, domainhost.ControlAI, 0)
			if _, err := store.UpsertHostMember(ctx, domainhost.Member{
				HostID: revokeHost.ID, OwnerID: hostOwner.ID, UserID: owner.ID,
				Permissions: []domainhost.Permission{domainhost.PermissionDesktopControl},
			}); err != nil {
				t.Fatalf("grant local approval Task Host control: %v", err)
			}
			revokeRun.revocationHostID = revokeHost.ID
			revokeRun.revocationHostOwnerID = hostOwner.ID
		}
		executor = activity
	}
	taskWorker, err := worker.NewTaskWorker(worker.TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "browser-acceptance-task",
		LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskStopWorker, err := worker.NewTaskStopWorker(worker.TaskStopWorkerOptions{
		Store: store, Executor: browserAcceptanceTaskStopExecutor{}, WorkerID: "browser-acceptance-stop",
		LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond, OperationTimeout: 2 * time.Second,
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	maxConcurrentRoutes := 2
	if crossClientAcceptance {
		maxConcurrentRoutes = 4
	}
	routerWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: gateway, WorkerID: "browser-acceptance-route",
		LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond, MaxConcurrent: maxConcurrentRoutes, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resultWorker, err := worker.NewConversationResultReviewWorker(store, 100*time.Millisecond, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workerDone := make(chan error, 4)
	workerCount := 0
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	workerCount++
	go func() { workerDone <- taskStopWorker.Run(workerCtx) }()
	workerCount++
	if (projectAttachmentResume == nil || activity != nil) && messageUploadResume == nil {
		go func() { workerDone <- routerWorker.Run(workerCtx) }()
		workerCount++
	}
	go func() { workerDone <- resultWorker.Run(workerCtx) }()
	workerCount++
	var retryHostHeartbeatDone chan struct{}
	if activity != nil {
		if retryRun := activity.runs["mcp-retry"]; retryRun != nil && retryRun.retryHostID != "" {
			retryHostID := retryRun.retryHostID
			retryHostHeartbeatDone = make(chan struct{})
			go func() {
				defer close(retryHostHeartbeatDone)
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-workerCtx.Done():
						return
					case <-ticker.C:
						if err := store.RecordHostReplicaHeartbeat(workerCtx, retryHostID, 1); err != nil {
							return
						}
					}
				}
			}()
		}
	}
	t.Cleanup(func() {
		cancelWorkers()
		for i := 0; i < workerCount; i++ {
			if err := <-workerDone; !errors.Is(err, context.Canceled) {
				t.Errorf("stop browser fixture worker: %v", err)
			}
		}
		if retryHostHeartbeatDone != nil {
			<-retryHostHeartbeatDone
		}
	})
	listenAddress := os.Getenv("MORDE_BROWSER_ACCEPTANCE_LISTEN_ADDR")
	if listenAddress == "" {
		listenAddress = "127.0.0.1:0"
	} else {
		host, _, splitErr := net.SplitHostPort(listenAddress)
		ip := net.ParseIP(host)
		if splitErr != nil || ip == nil || !ip.IsLoopback() {
			t.Fatalf("browser fixture listen address must use a loopback IP, got %q", listenAddress)
		}
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	portalRoot := filepath.Join("..", "..", "..", "..", "web", "portal", "dist")
	var hostService app.HostService = nativeAcceptanceHosts{}
	if activity != nil && activity.runs["mcp-retry"] != nil {
		hostService = browserAcceptanceHosts{nativeAcceptanceHosts: nativeAcceptanceHosts{}, store: store}
	}
	apiDeps := api.Deps{Hosts: hostService, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, ClientLogin: clients, Tasks: tasks, Conversations: conversations,
		WorkspaceOps: workspaceOps, Onboarding: onboarding,
		PersonalData: personalData, PersonalSync: personalSync, PersonalDataQuotaUser: personalQuotaUser,
		PersonalAttachments: personalAttachments,
		ModelConnections:    models, ModelGateway: gateway, ExecutionControl: execution,
		HostSharing: nativeAcceptanceSharing{}, MCPDisabled: true,
	}
	if projects != nil {
		apiDeps.Projects = projects
		apiDeps.ProjectConversations = projectConversations
		apiDeps.ProjectConversationAttachments = projectAttachments
	}
	router, err := api.New(apiDeps, api.Options{Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), ServiceInstanceID: instanceID,
		APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20, SessionTTL: 12 * time.Hour,
		HostScheme: "http", PortalDomain: listener.Addr().String(), HostSuffix: "hosts.invalid", PreviewPortMin: 1024,
		PortalFiles: os.DirFS(portalRoot)})
	if err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(t.TempDir(), "finish")
	// The production capability remains disabled until all acceptance checks
	// pass. This disposable fixture advertises it only to exercise the UI.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("LOCAL_HTTP %s %s authorization=%t\n", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		if os.Getenv("MORDE_BROWSER_OBSOLETE_PREDECESSOR_ERROR") == "1" && r.Method == http.MethodPost &&
			strings.HasPrefix(r.URL.Path, "/api/conversations/") && strings.HasSuffix(r.URL.Path, "/messages") {
			body, readErr := io.ReadAll(r.Body)
			var request struct {
				ReferenceTaskID string `json:"reference_task_id"`
			}
			if readErr == nil && json.Unmarshal(body, &request) == nil && request.ReferenceTaskID != "" &&
				obsoletePredecessorErrorResponses.CompareAndSwap(0, 1) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
					"code": "legacy_predecessor_error", "message": "前一条消息尚未确认，请先核对后重试。",
				}})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if messageReceiptRetry != nil && r.Method == http.MethodGet && r.URL.Path == "/__morde_fixture/message-receipt-retry-status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(messageReceiptRetry.view())
			return
		}
		if messageReceiptRetry != nil && r.Method == http.MethodPost && r.URL.Path == "/__morde_fixture/allow-message-receipt-retry" {
			messageReceiptRetry.allowRetry()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if messageReceiptRetry != nil && messageReceiptRetry.proxy(w, r, router) {
			return
		}
		if attachmentResume != nil && r.Method == http.MethodGet && r.URL.Path == "/__morde_fixture/attachment-status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(attachmentResume.view())
			return
		}
		if attachmentResume != nil && r.Method == http.MethodPost && r.URL.Path == "/__morde_fixture/allow-attachment-resume" {
			attachmentResume.allowResume()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if projectAttachmentResume != nil && r.Method == http.MethodGet && r.URL.Path == "/__morde_fixture/project-attachment-status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(projectAttachmentResume.view())
			return
		}
		if projectAttachmentResume != nil && r.Method == http.MethodPost && r.URL.Path == "/__morde_fixture/allow-project-attachment-resume" {
			projectAttachmentResume.allowResume()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if messageUploadResume != nil && r.Method == http.MethodGet && r.URL.Path == "/__morde_fixture/message-upload-status" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(messageUploadResume.view())
			return
		}
		if messageUploadResume != nil && r.Method == http.MethodPost && r.URL.Path == "/__morde_fixture/allow-message-upload-resume" {
			messageUploadResume.allowResume()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if messageUploadResume != nil && strings.HasPrefix(r.URL.Path,
			"/api/conversations/"+messageUploadResume.conversationID+"/message-uploads/") {
			if messageUploadResume.interruptFirstAttempt(w, r) {
				return
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			messageUploadResume.observe(r, recorder.Code)
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if projectAttachmentResume != nil && strings.Contains(r.URL.Path, "/attachments/") {
			if projectAttachmentResume.interruptFirstAttempt(w, r) {
				return
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			projectAttachmentResume.observe(r, recorder.Code)
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if attachmentResume != nil && strings.HasPrefix(r.URL.Path, "/api/personal-data/uploads") {
			if attachmentResume.interruptFirstAttempt(w, r) {
				return
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			attachmentResume.observe(r, recorder.Code)
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if activity != nil && strings.HasPrefix(r.URL.Path, "/__morde_fixture/") {
			activity.serveControl(w, r)
			if shouldAutoStopBrowserAcceptance(os.Getenv("MORDE_BROWSER_ACCEPTANCE_AUTO_STOP"), r.URL.Path, activity) {
				if err := os.WriteFile(stop, []byte("verified"), 0o600); err != nil {
					t.Errorf("signal completed browser acceptance: %v", err)
				}
			}
			return
		}
		if activity != nil && strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/events") {
			activity.serveTaskEvents(w, r, router)
			return
		}
		if activity != nil && activity.runs["interrupted"] != nil && r.Method == http.MethodPost &&
			strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/actions") {
			activity.serveTaskAction(w, r, router)
			return
		}
		if activity != nil && activity.runs["mcp-retry"] != nil && r.Method == http.MethodPost &&
			strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/retries") {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			if recorder.Code >= http.StatusBadRequest {
				var response struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal(recorder.Body.Bytes(), &response)
				fmt.Printf("LOCAL_TASK_RETRY_RESULT status=%d code=%s message=%q\n", recorder.Code, response.Error.Code, response.Error.Message)
			}
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if activity != nil && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/tasks/") {
			activity.serveTaskSnapshot(w, r, router)
			return
		}
		if activity != nil && r.Method == http.MethodGet && r.URL.Path == "/api/conversations/"+activity.conversationID+"/tasks" {
			activity.serveConversationTasks(w, r, router)
			return
		}
		if activity != nil && r.Method == http.MethodGet && r.URL.Path == "/api/conversations/"+activity.conversationID+"/messages" {
			activity.serveMessageSnapshot(w, r, router)
			return
		}
		if r.URL.Path == "/api/auth/client-login" {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			fmt.Printf("LOCAL_HTTP_RESULT %s %s status=%d\n", r.Method, r.URL.Path, recorder.Code)
			for name, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		if r.URL.Path != "/api/capabilities" {
			router.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, r)
		var value map[string]any
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &value) != nil {
			http.Error(w, "fixture capability unavailable", http.StatusServiceUnavailable)
			return
		}
		features, _ := value["supported_features"].([]any)
		value["supported_features"] = append(features, "conversation_auto_routing_v1")
		if projectAttachmentResume != nil {
			value["supported_features"] = append(value["supported_features"].([]any), "project_budget_execution")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
	// Task-event and streamed-message responses are long-lived SSE connections;
	// a normal response write timeout would silently sever them mid-task.
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("stop browser fixture: %v", err)
		}
	})
	t.Logf("LOCAL_PORTAL=http://%s TEST_USER=%s MAIN_CONVERSATION=%s STOP_FILE=%s", listener.Addr().String(), owner.Name, mainConversation.ConversationID, stop)
	if projectAttachmentResume != nil {
		t.Logf("PROJECT_ID=%s PROJECT_CONVERSATION_ID=%s", projectAttachmentResume.projectID, projectAttachmentResume.conversationID)
	}
	if activity != nil {
		tokenFile := filepath.Join(t.TempDir(), "activity-control-token")
		if err := os.WriteFile(tokenFile, []byte(activity.controlToken), 0o600); err != nil {
			t.Fatalf("write local activity control token: %v", err)
		}
		t.Logf("LOCAL_ACTIVITY_TOKEN_FILE=%s", tokenFile)
	}
	acceptanceTimeout := 10 * time.Minute
	if raw := os.Getenv("MORDE_BROWSER_ACCEPTANCE_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid MORDE_BROWSER_ACCEPTANCE_TIMEOUT %q", raw)
		}
		acceptanceTimeout = parsed
	}
	deadline := time.NewTimer(acceptanceTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(stop); err == nil {
				var messages, replies int
				minimumMessages := 2
				if activity != nil || attachmentResume != nil || projectAttachmentResume != nil || messageUploadResume != nil || messageReceiptRetry != nil {
					minimumMessages = 1
				}
				minimumReplies := 1
				if os.Getenv("MORDE_BROWSER_LONG_HISTORY") == "1" {
					minimumMessages = 840
					minimumReplies = 0
				}
				if activity != nil {
					minimumReplies = 0
					for _, scenario := range activity.required {
						if scenario != "failed" && scenario != "approval" && scenario != "approval-revoked" && scenario != "approval-host-member-revoked" && scenario != "mcp-retry" &&
							scenario != "recovery-unknown" && scenario != "target-control" {
							minimumReplies = 1
							break
						}
					}
				}
				if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE role='user'),count(*) FILTER (WHERE role='assistant') FROM conversation_messages WHERE owner_id=$1`, owner.ID).Scan(&messages, &replies); err != nil || messages < minimumMessages || (attachmentResume == nil && projectAttachmentResume == nil && messageUploadResume == nil && replies < minimumReplies) {
					t.Fatalf("browser messages=%d replies=%d err=%v", messages, replies, err)
				}
				if os.Getenv("MORDE_BROWSER_DUPLICATE_EXPECTED") == "1" {
					if err := assertBrowserDuplicateFlow(ctx, store, owner.ID, mainConversation.ConversationID); err != nil {
						t.Fatal(err)
					}
				}
				if messageReceiptRetry != nil {
					status := messageReceiptRetry.view()
					retryFailed := status.RequireExplicitRetry && (!status.FirstAttemptRejected || !status.RetryAllowed || !status.ServerAccepted || status.PostAttempts != 2)
					clientOfflineRetryFailed := status.ClientOfflineBeforeRetry &&
						(status.FirstAttemptRejected || status.FirstResponseLost || !status.ServerAccepted || status.PostAttempts != 1)
					lostReceipt := !status.RequireExplicitRetry && !status.ClientOfflineBeforeRetry &&
						(!status.FirstResponseLost || !status.ServerAccepted || status.PostAttempts != 1)
					if retryFailed || clientOfflineRetryFailed || lostReceipt || status.ClientMessageID == "" || status.ReconciliationChecks == 0 {
						t.Fatalf("message receipt retry was not reconciled without a duplicate POST: %+v", status)
					}
					var receipts int
					if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_routes
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
						instanceID, owner.ID, mainConversation.ConversationID, status.ClientMessageID).Scan(&receipts); err != nil || receipts != 1 {
						t.Fatalf("accepted message receipts=%d err=%v", receipts, err)
					}
				}
				if activity != nil {
					if err := activity.assertVerified(); err != nil {
						t.Fatal(err)
					}
				}
				if os.Getenv("MORDE_BROWSER_OBSOLETE_PREDECESSOR_ERROR") == "1" && obsoletePredecessorErrorResponses.Load() != 1 {
					t.Fatalf("obsolete predecessor response count=%d, want 1", obsoletePredecessorErrorResponses.Load())
				}
				if attachmentResume != nil {
					status := attachmentResume.view()
					if !status.FailureInjected || status.UploadID == "" || !status.ResumeInitializationSeen ||
						!status.ResumeAllowed || status.FirstChunkRepeatedAfterRestart || !status.MissingChunkRecovered || !status.Committed {
						t.Fatalf("attachment did not recover after restart: %+v", status)
					}
					var state string
					if err := store.pool.QueryRow(ctx, `SELECT state FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND upload_id=$3`, instanceID, owner.ID, status.UploadID).Scan(&state); err != nil || state != "committed" {
						t.Fatalf("recovered attachment state=%q err=%v", state, err)
					}
					var committedUploads int
					if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND state='committed'`, instanceID, owner.ID).Scan(&committedUploads); err != nil || committedUploads != 1 {
						t.Fatalf("attachment fixture created %d committed uploads, err=%v", committedUploads, err)
					}
				}
				if messageUploadResume != nil {
					status := messageUploadResume.view()
					if !status.FailureInjected || status.ClientMessageID == "" || !status.ResumeInitializationSeen ||
						!status.ResumeAllowed || status.FirstChunkRepeatedAfterRestart || !status.MissingChunksRecovered || !status.Committed {
						t.Fatalf("long message upload did not recover after restart: %+v", status)
					}
					var messages, completeMessages int
					if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE message.content=$4)
FROM conversation_message_routes route
JOIN conversation_messages message ON message.id=route.message_id
WHERE route.conversation_id=$1 AND route.client_message_id=$2 AND message.owner_id=$3 AND message.role='user'`,
						mainConversation.ConversationID, status.ClientMessageID, owner.ID, messageUploadResume.expectedContent).Scan(&messages, &completeMessages); err != nil || messages != 1 || completeMessages != 1 {
						t.Fatalf("long message persisted messages=%d complete=%d err=%v", messages, completeMessages, err)
					}
				}
				if projectAttachmentResume != nil {
					status := projectAttachmentResume.view()
					if !status.FailureInjected || status.MessageID == "" || status.ClientAttachmentID == "" ||
						!status.ResumeInitializationSeen || !status.ResumeAllowed || status.FirstChunkRepeatedAfterRestart ||
						!status.MissingChunkRecovered || !status.Committed || !status.DerivativesCompleted {
						t.Fatalf("project attachment did not recover after restart: %+v", status)
					}
					var state, derivativeState, derivativeMode string
					if err := store.pool.QueryRow(ctx, `SELECT state,derivative_state,derivative_mode FROM project_conversation_attachments
WHERE service_instance_id=$1::uuid AND project_id=$2 AND conversation_id=$3 AND message_id=$4 AND client_attachment_id=$5`,
						instanceID, projectAttachmentResume.projectID, projectAttachmentResume.conversationID,
						status.MessageID, status.ClientAttachmentID).Scan(&state, &derivativeState, &derivativeMode); err != nil || state != "committed" || derivativeState != "completed" {
						t.Fatalf("project attachment state=%q derivative_state=%q mode=%q err=%v", state, derivativeState, derivativeMode, err)
					}
					var derivatives, extractedText, pagePreviews, imageConversions, projectMessages int
					if err := store.pool.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER (WHERE kind='extracted_text'),
 count(*) FILTER (WHERE kind='page_preview'),
 count(*) FILTER (WHERE kind='image_conversion')
FROM project_conversation_attachment_derivatives
WHERE service_instance_id=$1::uuid AND project_id=$2 AND message_id=$3`, instanceID,
						projectAttachmentResume.projectID, status.MessageID).Scan(&derivatives, &extractedText, &pagePreviews, &imageConversions); err != nil ||
						derivatives == 0 || (derivativeMode == "pdf" && (extractedText == 0 || pagePreviews == 0)) ||
						(derivativeMode == "image" && imageConversions == 0) {
						t.Fatalf("project attachment derivatives=%d text=%d previews=%d image_conversions=%d mode=%q err=%v",
							derivatives, extractedText, pagePreviews, imageConversions, derivativeMode, err)
					}
					if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE owner_id=$1 AND conversation_id=$2 AND role='user'`, owner.ID, projectAttachmentResume.conversationID).Scan(&projectMessages); err != nil || projectMessages != 1 {
						t.Fatalf("project conversation user messages=%d err=%v", projectMessages, err)
					}
				}
				return
			}
		case <-deadline.C:
			t.Fatal("browser fixture timed out")
		}
	}
}

const browserAcceptanceDuplicateContent = "Morde browser acceptance: analyze project"
const browserAcceptanceStatusContent = "本机浏览器验收：进度如何"
const browserAcceptanceIndependentContent = "本机浏览器验收：独立总结当前会话"
const browserAcceptanceQuickReplyContent = "Morde browser acceptance: quick reply"
const browserAcceptanceAcknowledgedContent = "Morde browser acceptance: long background task"
const browserAcceptanceStopAllContent = "本地验收：结束我的全部活动工作"
const browserAcceptanceLongHistorySentinel = "ORCHID-PC-HISTORY-SENTINEL-8472"

type browserAcceptanceModel struct {
	nativeAcceptanceModel
	routeBarrier *browserAcceptanceRouteBarrier
	routeRetry   *browserAcceptanceRouteRetryModel
}

type browserAcceptanceRouteRetryModel struct {
	mu    sync.Mutex
	calls int
}

func (model *browserAcceptanceRouteRetryModel) failFirstRouteCall() bool {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.calls++
	return model.calls == 1
}

type browserAcceptanceRouteBarrier struct {
	mu       sync.Mutex
	expected int
	waiters  int
	ready    chan struct{}
}

func (barrier *browserAcceptanceRouteBarrier) wait(ctx context.Context) error {
	barrier.mu.Lock()
	barrier.waiters++
	if barrier.waiters >= barrier.expected {
		select {
		case <-barrier.ready:
		default:
			close(barrier.ready)
		}
	}
	ready := barrier.ready
	barrier.mu.Unlock()
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("cross-client route barrier timed out waiting for all four messages")
	}
}

type browserMessageReceiptRetryFixture struct {
	mu                       sync.Mutex
	conversationID           string
	requireExplicitRetry     bool
	clientOfflineBeforeRetry bool
	retryAllowed             bool
	postAttempts             int
	clientMessageID          string
	firstResponseLost        bool
	firstAttemptRejected     bool
	serverAccepted           bool
	reconciliationChecks     int
}

type browserMessageReceiptRetryStatus struct {
	PostAttempts             int    `json:"post_attempts"`
	ClientMessageID          string `json:"client_message_id"`
	RequireExplicitRetry     bool   `json:"require_explicit_retry"`
	ClientOfflineBeforeRetry bool   `json:"client_offline_before_retry"`
	RetryAllowed             bool   `json:"retry_allowed"`
	FirstResponseLost        bool   `json:"first_response_lost"`
	FirstAttemptRejected     bool   `json:"first_attempt_rejected"`
	ServerAccepted           bool   `json:"server_accepted"`
	ReconciliationChecks     int    `json:"reconciliation_checks"`
}

func (f *browserMessageReceiptRetryFixture) view() browserMessageReceiptRetryStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return browserMessageReceiptRetryStatus{PostAttempts: f.postAttempts, ClientMessageID: f.clientMessageID,
		RequireExplicitRetry: f.requireExplicitRetry, ClientOfflineBeforeRetry: f.clientOfflineBeforeRetry,
		RetryAllowed:      f.retryAllowed,
		FirstResponseLost: f.firstResponseLost, FirstAttemptRejected: f.firstAttemptRejected, ServerAccepted: f.serverAccepted,
		ReconciliationChecks: f.reconciliationChecks}
}

func (f *browserMessageReceiptRetryFixture) allowRetry() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryAllowed = true
}

func (f *browserMessageReceiptRetryFixture) proxy(w http.ResponseWriter, r *http.Request, router http.Handler) bool {
	messagePath := "/api/conversations/" + f.conversationID + "/messages"
	if r.Method == http.MethodGet && r.URL.Path == messagePath && r.URL.Query().Get("client_message_id") != "" {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, r)
		f.mu.Lock()
		if r.URL.Query().Get("client_message_id") != "" {
			f.reconciliationChecks++
		}
		f.mu.Unlock()
		copyBrowserFixtureResponse(w, recorder)
		return true
	}
	if r.Method != http.MethodPost || r.URL.Path != messagePath {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	var request struct {
		ClientMessageID string `json:"client_message_id"`
	}
	if json.Unmarshal(body, &request) != nil || request.ClientMessageID == "" {
		return false
	}
	f.mu.Lock()
	f.postAttempts++
	firstAttempt := f.postAttempts == 1
	requireExplicitRetry := f.requireExplicitRetry
	clientOfflineBeforeRetry := f.clientOfflineBeforeRetry
	retryAllowed := f.retryAllowed
	if firstAttempt {
		f.clientMessageID = request.ClientMessageID
	}
	f.mu.Unlock()
	if requireExplicitRetry && !retryAllowed {
		if firstAttempt {
			f.mu.Lock()
			f.firstAttemptRejected = true
			f.mu.Unlock()
		}
		http.Error(w, "synthetic local offline response before server acceptance", http.StatusServiceUnavailable)
		return true
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, r)
	if recorder.Code == http.StatusAccepted {
		f.mu.Lock()
		f.serverAccepted = true
		loseResponse := firstAttempt && !requireExplicitRetry && !clientOfflineBeforeRetry
		if loseResponse {
			f.firstResponseLost = true
		}
		f.mu.Unlock()
		if loseResponse {
			http.Error(w, "synthetic local response interruption after acceptance", http.StatusServiceUnavailable)
			return true
		}
	}
	copyBrowserFixtureResponse(w, recorder)
	return true
}

func copyBrowserFixtureResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for name, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func TestBrowserMessageReceiptRetryFixtureGate(t *testing.T) {
	fixture := &browserMessageReceiptRetryFixture{conversationID: "conversation-1"}
	var serverRequests int
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests++
		w.WriteHeader(http.StatusAccepted)
	})
	request := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/conversations/conversation-1/messages",
			strings.NewReader(`{"client_message_id":"message-0001","content":"hello"}`))
	}
	first := httptest.NewRecorder()
	if !fixture.proxy(first, request(), router) || first.Code != http.StatusServiceUnavailable || serverRequests != 1 {
		t.Fatalf("first accepted response was not hidden: status=%d server_requests=%d", first.Code, serverRequests)
	}
	status := fixture.view()
	if !status.FirstResponseLost || !status.ServerAccepted || status.PostAttempts != 1 || status.ClientMessageID != "message-0001" {
		t.Fatalf("receipt retry fixture did not record accepted-but-unknown: %+v", status)
	}
	second := httptest.NewRecorder()
	if !fixture.proxy(second, request(), router) || second.Code != http.StatusAccepted || serverRequests != 2 {
		t.Fatalf("retry response was not forwarded: status=%d server_requests=%d", second.Code, serverRequests)
	}
	reconciliation := httptest.NewRequest(http.MethodGet,
		"/api/conversations/conversation-1/messages?client_message_id=message-0001", nil)
	if !fixture.proxy(httptest.NewRecorder(), reconciliation, router) || fixture.view().ReconciliationChecks != 1 {
		t.Fatal("receipt reconciliation GET was not recorded")
	}
}

func TestBrowserMessageReceiptRetryFixtureRequiresExplicitRetry(t *testing.T) {
	fixture := &browserMessageReceiptRetryFixture{conversationID: "conversation-1", requireExplicitRetry: true}
	var serverRequests int
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests++
		w.WriteHeader(http.StatusAccepted)
	})
	request := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/conversations/conversation-1/messages",
			strings.NewReader(`{"client_message_id":"message-0001","content":"hello"}`))
	}
	first := httptest.NewRecorder()
	if !fixture.proxy(first, request(), router) || first.Code != http.StatusServiceUnavailable || serverRequests != 0 {
		t.Fatalf("offline attempt reached the server: status=%d server_requests=%d", first.Code, serverRequests)
	}
	status := fixture.view()
	if !status.RequireExplicitRetry || !status.FirstAttemptRejected || status.ServerAccepted || status.PostAttempts != 1 {
		t.Fatalf("fixture did not record the unaccepted message: %+v", status)
	}

	fixture.allowRetry()
	second := httptest.NewRecorder()
	if !fixture.proxy(second, request(), router) || second.Code != http.StatusAccepted || serverRequests != 1 {
		t.Fatalf("explicit retry was not accepted once: status=%d server_requests=%d", second.Code, serverRequests)
	}
	status = fixture.view()
	if !status.RetryAllowed || !status.ServerAccepted || status.PostAttempts != 2 || status.ClientMessageID != "message-0001" {
		t.Fatalf("explicit retry did not preserve the original idempotency key: %+v", status)
	}
}

func TestBrowserMessageReceiptRetryFixtureAcceptsAfterClientOffline(t *testing.T) {
	fixture := &browserMessageReceiptRetryFixture{
		conversationID: "conversation-1", clientOfflineBeforeRetry: true,
	}
	var serverRequests int
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests++
		w.WriteHeader(http.StatusAccepted)
	})
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/conversation-1/messages",
		strings.NewReader(`{"client_message_id":"message-0001","content":"hello"}`))
	accepted := httptest.NewRecorder()
	if !fixture.proxy(accepted, request, router) || accepted.Code != http.StatusAccepted || serverRequests != 1 {
		t.Fatalf("first server-visible retry was not accepted once: status=%d server_requests=%d", accepted.Code, serverRequests)
	}
	status := fixture.view()
	if !status.ClientOfflineBeforeRetry || status.FirstAttemptRejected || status.FirstResponseLost ||
		!status.ServerAccepted || status.PostAttempts != 1 || status.ClientMessageID != "message-0001" {
		t.Fatalf("client-offline retry outcome was not recorded: %+v", status)
	}
	reconciliation := httptest.NewRequest(http.MethodGet,
		"/api/conversations/conversation-1/messages?client_message_id=message-0001", nil)
	if !fixture.proxy(httptest.NewRecorder(), reconciliation, router) || fixture.view().ReconciliationChecks != 1 {
		t.Fatal("receipt reconciliation GET was not recorded")
	}
}

type browserMessageUploadResumeFixture struct {
	mu                             sync.Mutex
	conversationID                 string
	expectedContent                string
	totalChunks                    int
	uploadBase                     string
	clientMessageID                string
	failureInjected                bool
	resumeAllowed                  bool
	resumeInitializationSeen       bool
	firstChunkRepeatedAfterRestart bool
	receivedAfterRestart           map[int]struct{}
	missingChunksRecovered         bool
	committed                      bool
}

type browserMessageUploadResumeStatus struct {
	FailureInjected                bool   `json:"failure_injected"`
	ResumeAllowed                  bool   `json:"resume_allowed"`
	ClientMessageID                string `json:"client_message_id"`
	ResumeInitializationSeen       bool   `json:"resume_initialization_seen"`
	FirstChunkRepeatedAfterRestart bool   `json:"first_chunk_repeated_after_restart"`
	MissingChunksRecovered         bool   `json:"missing_chunks_recovered"`
	Committed                      bool   `json:"committed"`
}

func newBrowserMessageUploadResumeFixture(conversationID string) *browserMessageUploadResumeFixture {
	content := strings.Repeat("中文abc🙂", 3000) + "尾部完整"
	return &browserMessageUploadResumeFixture{
		conversationID: conversationID, expectedContent: content,
		totalChunks:          (len(content) + (16 << 10) - 1) / (16 << 10),
		receivedAfterRestart: make(map[int]struct{}),
	}
}

func (f *browserMessageUploadResumeFixture) view() browserMessageUploadResumeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return browserMessageUploadResumeStatus{
		FailureInjected: f.failureInjected, ResumeAllowed: f.resumeAllowed, ClientMessageID: f.clientMessageID,
		ResumeInitializationSeen:       f.resumeInitializationSeen,
		FirstChunkRepeatedAfterRestart: f.firstChunkRepeatedAfterRestart,
		MissingChunksRecovered:         f.missingChunksRecovered, Committed: f.committed,
	}
}

func conversationMessageUploadPath(path string) (base, conversationID, clientMessageID, operation, sequence string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 5 || parts[0] != "api" || parts[1] != "conversations" || parts[2] == "" ||
		parts[3] != "message-uploads" || parts[4] == "" {
		return "", "", "", "", "", false
	}
	conversationID, clientMessageID = parts[2], parts[4]
	base = "/" + strings.Join(parts[:5], "/")
	if len(parts) == 5 {
		return base, conversationID, clientMessageID, "initialize", "", true
	}
	if len(parts) == 6 && parts[5] == "commit" {
		return base, conversationID, clientMessageID, "commit", "", true
	}
	if len(parts) == 7 && parts[5] == "chunks" && parts[6] != "" {
		return base, conversationID, clientMessageID, "chunk", parts[6], true
	}
	return "", "", "", "", "", false
}

func (f *browserMessageUploadResumeFixture) interruptFirstAttempt(w http.ResponseWriter, r *http.Request) bool {
	base, conversationID, clientMessageID, operation, sequence, ok := conversationMessageUploadPath(r.URL.Path)
	if !ok || conversationID != f.conversationID {
		return false
	}
	f.mu.Lock()
	if !f.failureInjected && r.Method == http.MethodPut && operation == "chunk" && sequence == "1" {
		f.failureInjected = true
		f.uploadBase = base
		f.clientMessageID = clientMessageID
	}
	blocked := f.failureInjected && !f.resumeAllowed && base == f.uploadBase
	f.mu.Unlock()
	if !blocked {
		return false
	}
	http.Error(w, "synthetic local message chunk interruption", http.StatusServiceUnavailable)
	return true
}

func (f *browserMessageUploadResumeFixture) allowResume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureInjected {
		f.resumeAllowed = true
	}
}

func (f *browserMessageUploadResumeFixture) observe(r *http.Request, status int) {
	base, conversationID, clientMessageID, operation, sequence, ok := conversationMessageUploadPath(r.URL.Path)
	if !ok || conversationID != f.conversationID {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failureInjected || !f.resumeAllowed || base != f.uploadBase || clientMessageID != f.clientMessageID {
		return
	}
	if operation == "initialize" && (r.Method == http.MethodPut || r.Method == http.MethodGet) && status == http.StatusOK {
		f.resumeInitializationSeen = true
		return
	}
	if !f.resumeInitializationSeen {
		return
	}
	if operation == "chunk" && r.Method == http.MethodPut && status == http.StatusNoContent {
		chunk, err := strconv.Atoi(sequence)
		if err == nil && chunk == 0 {
			f.firstChunkRepeatedAfterRestart = true
		} else if err == nil && chunk > 0 && chunk < f.totalChunks {
			f.receivedAfterRestart[chunk] = struct{}{}
			f.missingChunksRecovered = len(f.receivedAfterRestart) == f.totalChunks-1
		}
	}
	if operation == "commit" && r.Method == http.MethodPost && status == http.StatusAccepted {
		f.committed = true
	}
}

func TestBrowserMessageUploadResumeFixtureGate(t *testing.T) {
	fixture := &browserMessageUploadResumeFixture{
		conversationID: "conversation-1", clientMessageID: "message-1", totalChunks: 3,
		receivedAfterRestart: make(map[int]struct{}),
	}
	chunkOne := httptest.NewRequest(http.MethodPut,
		"/api/conversations/conversation-1/message-uploads/message-1/chunks/1", nil)
	interruption := httptest.NewRecorder()
	intercepted := fixture.interruptFirstAttempt(interruption, chunkOne)
	if !intercepted || interruption.Code != http.StatusServiceUnavailable {
		t.Fatalf("first failed chunk was not intercepted: intercepted=%t status=%d",
			intercepted, interruption.Code)
	}

	retry := httptest.NewRecorder()
	intercepted = fixture.interruptFirstAttempt(retry, chunkOne)
	if !intercepted || retry.Code != http.StatusServiceUnavailable {
		t.Fatalf("retry passed before simulated app restart: intercepted=%t status=%d",
			intercepted, retry.Code)
	}
	fixture.allowResume()
	if fixture.interruptFirstAttempt(httptest.NewRecorder(), chunkOne) {
		t.Fatal("request remained blocked after releasing the restart gate")
	}

	fixture.observe(httptest.NewRequest(http.MethodGet,
		"/api/conversations/conversation-1/message-uploads/message-1", nil), http.StatusOK)
	for _, sequence := range []string{"1", "2"} {
		fixture.observe(httptest.NewRequest(http.MethodPut,
			"/api/conversations/conversation-1/message-uploads/message-1/chunks/"+sequence, nil), http.StatusNoContent)
	}
	fixture.observe(httptest.NewRequest(http.MethodPost,
		"/api/conversations/conversation-1/message-uploads/message-1/commit", nil), http.StatusAccepted)

	status := fixture.view()
	if !status.FailureInjected || !status.ResumeAllowed || !status.ResumeInitializationSeen ||
		status.FirstChunkRepeatedAfterRestart || !status.MissingChunksRecovered || !status.Committed {
		t.Fatalf("fixture did not record the expected restart recovery: %+v", status)
	}
}

type browserAttachmentResumeFixture struct {
	mu                             sync.Mutex
	failureInjected                bool
	resumeAllowed                  bool
	uploadID                       string
	resumeInitializationSeen       bool
	firstChunkRepeatedAfterRestart bool
	missingChunkRecovered          bool
	committed                      bool
}

type browserProjectAttachmentResumeFixture struct {
	mu                             sync.Mutex
	projectID                      string
	conversationID                 string
	messageID                      string
	clientAttachmentID             string
	uploadBase                     string
	failureInjected                bool
	resumeAllowed                  bool
	resumeInitializationSeen       bool
	firstChunkRepeatedAfterRestart bool
	missingChunkRecovered          bool
	committed                      bool
	derivativesCompleted           bool
}

type browserProjectAttachmentResumeStatus struct {
	FailureInjected                bool   `json:"failure_injected"`
	ResumeAllowed                  bool   `json:"resume_allowed"`
	MessageID                      string `json:"message_id"`
	ClientAttachmentID             string `json:"client_attachment_id"`
	ResumeInitializationSeen       bool   `json:"resume_initialization_seen"`
	FirstChunkRepeatedAfterRestart bool   `json:"first_chunk_repeated_after_restart"`
	MissingChunkRecovered          bool   `json:"missing_chunk_recovered"`
	Committed                      bool   `json:"committed"`
	DerivativesCompleted           bool   `json:"derivatives_completed"`
}

func (f *browserProjectAttachmentResumeFixture) view() browserProjectAttachmentResumeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return browserProjectAttachmentResumeStatus{
		FailureInjected: f.failureInjected, ResumeAllowed: f.resumeAllowed,
		MessageID: f.messageID, ClientAttachmentID: f.clientAttachmentID,
		ResumeInitializationSeen:       f.resumeInitializationSeen,
		FirstChunkRepeatedAfterRestart: f.firstChunkRepeatedAfterRestart,
		MissingChunkRecovered:          f.missingChunkRecovered, Committed: f.committed,
		DerivativesCompleted: f.derivativesCompleted,
	}
}

func projectAttachmentUploadPath(path string) (base, messageID, clientAttachmentID, sequence string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 9 || parts[0] != "api" || parts[1] != "projects" || parts[3] != "conversations" ||
		parts[5] != "messages" || parts[7] != "attachments" || parts[2] == "" || parts[4] == "" ||
		parts[6] == "" || parts[8] == "" {
		return "", "", "", "", false
	}
	base = "/" + strings.Join(parts[:9], "/")
	if len(parts) == 11 && parts[9] == "chunks" {
		sequence = parts[10]
	}
	return base, parts[6], parts[8], sequence, true
}

func (f *browserProjectAttachmentResumeFixture) interruptFirstAttempt(w http.ResponseWriter, r *http.Request) bool {
	base, messageID, clientAttachmentID, sequence, valid := projectAttachmentUploadPath(r.URL.Path)
	if !valid {
		return false
	}
	f.mu.Lock()
	if !f.failureInjected && r.Method == http.MethodPut && sequence == "1" &&
		strings.HasPrefix(base, "/api/projects/"+f.projectID+"/conversations/"+f.conversationID+"/") {
		f.failureInjected = true
		f.messageID = messageID
		f.clientAttachmentID = clientAttachmentID
		f.uploadBase = base
	}
	blocked := f.failureInjected && !f.resumeAllowed && base == f.uploadBase
	f.mu.Unlock()
	if !blocked {
		return false
	}
	http.Error(w, "synthetic local project attachment interruption", http.StatusServiceUnavailable)
	return true
}

func (f *browserProjectAttachmentResumeFixture) allowResume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureInjected {
		f.resumeAllowed = true
	}
}

func (f *browserProjectAttachmentResumeFixture) observe(r *http.Request, status int) {
	base, _, _, sequence, valid := projectAttachmentUploadPath(r.URL.Path)
	if !valid {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failureInjected || base != f.uploadBase {
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == f.uploadBase && f.resumeAllowed {
		f.resumeInitializationSeen = true
		return
	}
	if r.Method == http.MethodPut && sequence == "0" && status == http.StatusOK && f.resumeInitializationSeen {
		f.firstChunkRepeatedAfterRestart = true
	}
	if r.Method == http.MethodPut && sequence == "1" && status == http.StatusOK && f.resumeInitializationSeen {
		f.missingChunkRecovered = true
	}
	if r.Method == http.MethodPost && r.URL.Path == f.uploadBase+"/commit" && status == http.StatusOK {
		f.committed = true
	}
	if r.Method == http.MethodPut && r.URL.Path == f.uploadBase+"/derivatives" && status == http.StatusOK {
		f.derivativesCompleted = true
	}
}

type browserAttachmentResumeStatus struct {
	FailureInjected                bool   `json:"failure_injected"`
	ResumeAllowed                  bool   `json:"resume_allowed"`
	UploadID                       string `json:"upload_id"`
	ResumeInitializationSeen       bool   `json:"resume_initialization_seen"`
	FirstChunkRepeatedAfterRestart bool   `json:"first_chunk_repeated_after_restart"`
	MissingChunkRecovered          bool   `json:"missing_chunk_recovered"`
	Committed                      bool   `json:"committed"`
}

func (f *browserAttachmentResumeFixture) view() browserAttachmentResumeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return browserAttachmentResumeStatus{
		FailureInjected: f.failureInjected, ResumeAllowed: f.resumeAllowed, UploadID: f.uploadID,
		ResumeInitializationSeen:       f.resumeInitializationSeen,
		FirstChunkRepeatedAfterRestart: f.firstChunkRepeatedAfterRestart,
		MissingChunkRecovered:          f.missingChunkRecovered, Committed: f.committed,
	}
}

func (f *browserAttachmentResumeFixture) interruptFirstAttempt(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	if !f.failureInjected && r.Method == http.MethodPut {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/personal-data/uploads/"), "/")
		if len(parts) == 3 && parts[1] == "chunks" && parts[2] == "1" && parts[0] != "" {
			f.failureInjected = true
			f.uploadID = parts[0]
		}
	}
	blocked := f.failureInjected && !f.resumeAllowed
	f.mu.Unlock()
	if !blocked {
		return false
	}
	http.Error(w, "synthetic local attachment interruption", http.StatusServiceUnavailable)
	return true
}

func (f *browserAttachmentResumeFixture) allowResume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureInjected {
		f.resumeAllowed = true
	}
}

func (f *browserAttachmentResumeFixture) observe(r *http.Request, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/api/personal-data/uploads" && f.failureInjected {
		f.resumeInitializationSeen = true
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/personal-data/uploads/"), "/")
	if len(parts) < 1 || parts[0] != f.uploadID {
		return
	}
	if r.Method == http.MethodPut && len(parts) == 3 && parts[1] == "chunks" && status == http.StatusOK {
		switch parts[2] {
		case "0":
			if f.resumeInitializationSeen {
				f.firstChunkRepeatedAfterRestart = true
			}
		case "1":
			f.missingChunkRecovered = f.resumeInitializationSeen
		}
	}
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "commit" && status == http.StatusOK {
		f.committed = true
	}
}

func (model browserAcceptanceModel) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if strings.HasPrefix(request.Instructions, "Review the completed Task results") {
		var input struct {
			Results []app.ConversationTaskResultSource `json:"completed_task_results"`
		}
		if len(request.Input) > 0 && json.Unmarshal([]byte(request.Input[0].Text), &input) == nil && len(input.Results) > 0 {
			results := make([]string, 0, len(input.Results))
			for _, result := range input.Results {
				results = append(results, result.Content)
			}
			reply, err := json.Marshal(map[string]string{"action": "reply", "reply": strings.Join(results, "\n\n")})
			if err != nil {
				return app.ModelResponse{}, err
			}
			return app.ModelResponse{Text: string(reply), StopReason: "stop",
				Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
		}
		return app.ModelResponse{Text: `{"action":"reply","reply":"本地验收：任务结果已汇总。"}`, StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
	}
	if strings.HasPrefix(request.Instructions, "Classify the latest request") {
		if len(request.Input) > 0 {
			var input struct {
				Message       string                                  `json:"message"`
				Tasks         []app.ConversationRouteCandidate        `json:"active_tasks"`
				PriorRequests []app.ConversationRouteRequestCandidate `json:"prior_requests"`
			}
			if json.Unmarshal([]byte(request.Input[0].Text), &input) == nil {
				if input.Message == browserAcceptanceStopAllContent {
					return app.ModelResponse{Text: `{"action":"stop_all"}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
				}
				if input.Message == browserActivityCompletionRaceSteer {
					for _, candidate := range input.Tasks {
						if candidate.Objective != browserActivityObjectives["completion-race"] || candidate.Status != task.StatusRunning {
							continue
						}
						body, err := json.Marshal(map[string]string{"action": "steer", "task_id": candidate.TaskID})
						if err != nil {
							return app.ModelResponse{}, err
						}
						return app.ModelResponse{Text: string(body), StopReason: "stop",
							Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
					}
					return app.ModelResponse{Text: `{"action":"clarify","question":"无法确认要补充到哪个运行中的任务。"}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
				}
				if input.Message == browserActivityCompletionRaceIndependent {
					return app.ModelResponse{Text: `{"action":"new","task_id":"","explicit_new":false,"acknowledge":true}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
				}
				if input.Message == browserActivityObjectives["route-retry"] && model.routeRetry != nil {
					if model.routeRetry.failFirstRouteCall() {
						return app.ModelResponse{}, &app.ModelClientError{Kind: app.ModelErrorRateLimited,
							Stage: "provider_status", RetryAfterExceedsDurableHorizon: true}
					}
					return app.ModelResponse{Text: `{"action":"reply","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"本地验收：消息路由恢复成功。","dependency_task_ids":[],"acknowledge":false}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 8}}, nil
				}
				for name, objective := range browserActivityObjectives {
					if name != "search" && input.Message == objective {
						return app.ModelResponse{Text: `{"action":"new","task_id":"","question":"","acknowledge":true}`, StopReason: "stop",
							Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
					}
				}
				if input.Message == browserAcceptanceQuickReplyContent {
					return app.ModelResponse{Text: `{"action":"reply","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"本地验收：快速请求直接回复。","dependency_task_ids":[],"acknowledge":false}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 8}}, nil
				}
				if input.Message == browserAcceptanceAcknowledgedContent {
					return app.ModelResponse{Text: `{"action":"new","task_id":"","target_message_id":"","aggregate_message_ids":[],"explicit_new":false,"question":"","reply":"","dependency_task_ids":[],"acknowledge":true}`, StopReason: "stop",
						Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 8}}, nil
				}
				if input.Message == browserAcceptanceStatusContent {
					for _, candidate := range input.Tasks {
						if candidate.Objective == browserAcceptanceDuplicateContent {
							body, err := json.Marshal(map[string]string{"action": "status", "task_id": candidate.TaskID})
							if err != nil {
								return app.ModelResponse{}, err
							}
							return app.ModelResponse{Text: string(body), StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
						}
					}
				}
				if input.Message == browserAcceptanceDuplicateContent {
					if model.routeBarrier != nil {
						if err := model.routeBarrier.wait(ctx); err != nil {
							return app.ModelResponse{}, err
						}
						return app.ModelResponse{Text: `{"action":"new","task_id":"","question":""}`, StopReason: "stop",
							Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
					}
					for _, prior := range input.PriorRequests {
						if prior.Content == input.Message && prior.SameExecutionScope {
							body, err := json.Marshal(map[string]string{"action": "join", "target_message_id": prior.MessageID})
							if err != nil {
								return app.ModelResponse{}, err
							}
							return app.ModelResponse{Text: string(body), StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
						}
					}
				}
			}
		}
		return app.ModelResponse{Text: `{"action":"new","task_id":"","question":""}`, StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
	}
	return (nativeAcceptanceModel{}).Complete(ctx, request)
}

type browserAcceptanceExecutor struct{ store *Store }

type browserAcceptanceTaskStopExecutor struct{}

func (browserAcceptanceTaskStopExecutor) StopTaskOperation(ctx context.Context, _ task.Task, _ task.Operation) (task.OperationStatus, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	return task.OperationCancelled, "stopped by local browser acceptance", false, nil
}

func (e browserAcceptanceExecutor) ExecuteTask(ctx context.Context, current task.Task) error {
	delay := 750 * time.Millisecond
	switch current.Objective {
	case browserAcceptanceAcknowledgedContent:
		// Simulate DSH's persisted revision-start event for the PC acknowledgement fixture.
		tx, err := e.store.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin acknowledgement fixture revision: %w", err)
		}
		if err := recordTaskRevisionStartedInTx(ctx, tx, current.ID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record acknowledgement fixture revision: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit acknowledgement fixture revision: %w", err)
		}
		delay = 2 * time.Minute
	case browserAcceptanceDuplicateContent:
		delay = 30 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	result := "本地验收回复：" + current.Objective
	_, err = e.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: stepID, Result: result,
		PublishResultMessage: true, Usage: current.Usage,
		Steps: []task.Step{{ID: stepID, TaskID: current.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{}`)}})
	return err
}

func assertBrowserDuplicateFlow(ctx context.Context, store *Store, ownerID, conversationID string) error {
	rows, err := store.pool.Query(ctx, `SELECT message.id,COALESCE(route.duplicate_of_message_id,''),COALESCE(message.task_id,'')
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.owner_id=$1 AND message.conversation_id=$2 AND message.role='user' AND message.content=$3
ORDER BY message.sequence`, ownerID, conversationID, browserAcceptanceDuplicateContent)
	if err != nil {
		return err
	}
	defer rows.Close()
	type message struct{ id, duplicateOf, taskID string }
	var messages []message
	for rows.Next() {
		var value message
		if err := rows.Scan(&value.id, &value.duplicateOf, &value.taskID); err != nil {
			return err
		}
		messages = append(messages, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	expectedDuplicates := 2
	if os.Getenv("MORDE_BROWSER_CROSS_CLIENT") == "1" {
		expectedDuplicates = 4
	}
	if len(messages) != expectedDuplicates || messages[0].taskID == "" || messages[0].duplicateOf != "" {
		return fmt.Errorf("duplicate messages were not linked to one task: %+v", messages)
	}
	for _, duplicate := range messages[1:] {
		if duplicate.duplicateOf != messages[0].id || duplicate.taskID != "" {
			return fmt.Errorf("duplicate message did not link to the single root request: %+v", messages)
		}
	}
	var duplicateTasks int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND objective=$2`,
		conversationID, browserAcceptanceDuplicateContent).Scan(&duplicateTasks); err != nil {
		return err
	}
	if duplicateTasks != 1 {
		return fmt.Errorf("duplicate request created %d tasks", duplicateTasks)
	}
	var independentCount int
	var independentTaskID string
	if err := store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(max(message.task_id),'')
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.owner_id=$1 AND message.conversation_id=$2 AND message.role='user' AND message.content=$3`,
		ownerID, conversationID, browserAcceptanceIndependentContent).Scan(&independentCount, &independentTaskID); err != nil {
		return err
	}
	if independentCount != 1 || independentTaskID == "" || independentTaskID == messages[0].taskID {
		return fmt.Errorf("independent request did not create its own task: messages=%d task=%q", independentCount, independentTaskID)
	}
	var statusCount, statusTaskCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE message.task_id IS NOT NULL)
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.owner_id=$1 AND message.conversation_id=$2 AND message.role='user' AND message.content=$3
AND message.delivery_status='applied' AND route.state='routed'`, ownerID, conversationID,
		browserAcceptanceStatusContent).Scan(&statusCount, &statusTaskCount); err != nil {
		return err
	}
	if statusCount != 1 || statusTaskCount != 0 {
		return fmt.Errorf("status follow-up created a task or was not answered: messages=%d tasks=%d", statusCount, statusTaskCount)
	}
	page, err := store.ListMessages(ctx, ownerID, conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return err
	}
	duplicateFound, originalTaskReplies := false, 0
	for _, item := range page.Items {
		if item.ID == messages[1].id && (item.DuplicateOfMessageID != messages[0].id ||
			item.RelatedTaskID != messages[0].taskID || item.TaskID != "" ||
			item.RoutingStatus != conversation.RoutingRouted || item.DeliveryStatus != conversation.DeliveryApplied) {
			return fmt.Errorf("duplicate message projection lost its original task status: %+v", item)
		}
		if item.ID == messages[1].id {
			duplicateFound = true
		}
		if item.Role == conversation.RoleAssistant {
			for _, source := range item.Sources {
				if source.TaskID == messages[0].taskID {
					originalTaskReplies++
				}
			}
		}
	}
	if !duplicateFound || originalTaskReplies != 1 {
		return fmt.Errorf("duplicate projection or single original reply missing: duplicate=%t replies=%d", duplicateFound, originalTaskReplies)
	}
	return nil
}

// The opt-in activity stages are selected only by these synthetic messages.
// Its control API is loopback-only and protected by a fresh per-run token,
// written to a mode-0600 file inside the test's temporary directory.
// No file or user-provided content is needed by any of these scenarios.
// With MORDE_BROWSER_ACTIVITY=1, GET /__morde_fixture/status lists the fixed
// inputs and stages. A selected scenario exposes a loopback-only bootstrap at
// GET /__morde_fixture/bootstrap/{scenario} for native UI tests.
// MORDE_BROWSER_ACCEPTANCE_LISTEN_ADDR may select a fixed loopback port; absent
// it, the fixture binds a random loopback port. POST /__morde_fixture/advance/{name}/{start|progress|
// stream|complete|candidate|commit|fail|approval|clarification|unknown|retry-success} releases one stage, without a request body.
// The search scenario uses a fixed local result and never accesses the network.
// MORDE_BROWSER_ACTIVITY_SCENARIOS=search,queued,failed,approval,approval-resume,approval-host-member-revoked,clarification,recovery-unknown,completion-race,target-control selects individual
// scenarios; omit it to require the full merge, separate, search, stream and
// reconnect set. target-control waits for two similar independent Tasks, then
// verifies that stopping B leaves A running. For a full run, merge-a and merge-b must complete within the
// aggregation window; separate-a and separate-b must complete in order, more
// than maxConversationResultWindow apart. Start stream before submitting
// stream-other, then disconnect both clients using
// POST /__morde_fixture/disconnect/stream before completing the pair.
// MORDE_BROWSER_ACCEPTANCE_AUTO_STOP=1 lets the fixture stop itself after all
// selected activity scenarios have passed their verify endpoints.
// GET /__morde_fixture/verify/{merge|separate|reconnect|search|stream|failed|
// approval|approval-resume|approval-revoked|approval-host-member-revoked|clarification|recovery-unknown|completion-race|interrupted|mcp-retry|route-retry|target-control} checks result identity and ordering, reconnect cursors, or the
// persisted accepted route and requested Task outcome. The fixture does not
// pass until all selected checks succeed; visual deduplication still needs to
// be checked in both clients after refresh/relaunch. MORDE_BROWSER_ACTIVITY_CROSS_CLIENT=1
// requires matching PC and iOS Task reads for recovery-unknown; =approval
// requires matching reads for the approval-waiting scenario; =approval-revoked
// requires both clients to read the same authorization-fenced approval Task;
// =approval-host-member-revoked requires both clients to read the same Task
// after its shared Host member grant is revoked;
// =approval-resume
// requires matching completed Task reads after an approval; =interrupted
// requires both clients to read the same persisted interrupted assistant message.
var browserActivityObjectives = map[string]string{
	"merge-a":                      "跨端活动验收：合并 A",
	"merge-b":                      "跨端活动验收：合并 B",
	"search":                       "在本地示例索引中搜索 Morde 消息路由",
	"separate-a":                   "跨端活动验收：分开 A",
	"separate-b":                   "跨端活动验收：分开 B",
	"stream":                       "跨端活动验收：流式",
	"stream-other":                 "跨端活动验收：流式后的独立任务",
	"queued":                       "跨端活动验收：排队状态确认",
	"failed":                       "跨端活动验收：已接单后任务失败",
	"approval":                     "跨端活动验收：等待用户授权",
	"approval-resume":              "跨端活动验收：批准后继续任务",
	"approval-revoked":             "跨端活动验收：审批等待期间撤销执行授权",
	"approval-host-member-revoked": "跨端活动验收：审批等待期间撤销共享 Host 授权",
	"clarification":                "跨端活动验收：需要补充信息",
	"recovery-unknown":             "跨端活动验收：恢复检查点无法确认",
	"completion-race":              "跨端活动验收：完成结果竞争",
	"interrupted":                  "跨端活动验收：取消流式回复",
	"mcp-retry":                    "跨端活动验收：连接器限流后手动重试",
	"route-retry":                  "跨端活动验收：消息路由限流后显式重试",
	"target-control-a":             "跨端活动验收：整理项目总结 A",
	"target-control-b":             "跨端活动验收：整理项目总结 B",
}

const browserActivityStreamPrefix = "跨端活动验收：流式回复第一段"
const browserActivityStreamSuffix = "，第二段完成。"
const browserActivitySearchResult = "本地搜索结果：Morde 消息路由先持久接收消息，再异步分流。"
const browserActivityClarificationAnswer = "保存到 ./reports"
const browserActivityApprovalResumeResult = "审批已批准，任务已从同一等待点继续完成；未执行外部副作用。"
const browserActivityCompletionRaceSteer = "本地验收：最终结果补充为 BLUE。"
const browserActivityCompletionRaceIndependent = "独立核对另一项 YELLOW 事项。"
const browserActivityCompletionRaceResult = "已按补充要求完成，最终标记为 BLUE。"

func browserActivityProgressTitle(run *browserActivityRun) string {
	if run.name == "search" {
		return "正在检索固定的本地索引"
	}
	return "正在整理测试信息"
}

func browserActivityProgressSummary(run *browserActivityRun, completed bool) string {
	if run.name == "search" {
		if completed {
			return "固定索引查询完成，已整理出一条本地结果。"
		}
		return "正在查找 Morde 消息路由示例条目。"
	}
	if completed {
		return "合成文字处理完成。"
	}
	return "正在处理合成文字。"
}

func browserActivityResult(name, objective string) string {
	if name == "search" {
		return browserActivitySearchResult
	}
	return "本地验收结果：" + objective
}

type browserActivityCommand struct {
	phase string
	reply chan error
}

type browserActivityRun struct {
	name, objective, stage, failure                     string
	retryHostID                                         string
	revocationHostID, revocationHostOwnerID             string
	current                                             task.Task
	agentLease                                          app.AgentInstanceLease
	supervisorNonce                                     []byte
	userMessageID, progressStepID, mcpRetrySourceStepID string
	mcpRetryCallID                                      string
	mcpRetryDispatchCount                               int
	clarificationAnswer                                 string
	approvalID, approvalCallID                          string
	approvalVersion                                     int64
	approvalRequestHash                                 []byte
	approvalResumeCount                                 int
	completionCandidate                                 string
	completionSteerMessageID                            string
	completionRejected                                  bool
	stopStatus                                          string
	streamMessageID, streamTurnID                       string
	streamSequence                                      int64
	resultEventSequence                                 int64
	busy                                                bool
	commands                                            chan browserActivityCommand
	dropIndex                                           int
	dropCursors                                         map[string]int64
	checkpointFence                                     app.AgentCheckpointFence
	checkpointThrough                                   int64
	checkpointDigest                                    []byte
}

type browserActivityEventRequest struct {
	TaskID string `json:"task_id"`
	Client string `json:"client"`
	After  int64  `json:"after"`
	LastID int64  `json:"last_event_id"`
}

type browserActivityTaskListRequest struct {
	Client     string   `json:"client"`
	StatusCode int      `json:"status_code"`
	TaskIDs    []string `json:"task_ids"`
	Statuses   []string `json:"statuses"`
}

type browserActivityTaskSnapshotRequest struct {
	Client        string `json:"client"`
	TaskID        string `json:"task_id"`
	StatusCode    int    `json:"status_code"`
	Status        string `json:"status,omitempty"`
	WaitingReason string `json:"waiting_reason,omitempty"`
}

type browserActivityMessageSnapshotRequest struct {
	Client       string `json:"client"`
	TaskID       string `json:"task_id"`
	MessageID    string `json:"message_id"`
	StatusCode   int    `json:"status_code"`
	Role         string `json:"role,omitempty"`
	Content      string `json:"content,omitempty"`
	OutputStatus string `json:"output_status,omitempty"`
}

type browserActivityWatch struct {
	taskID string
	client string
	cancel context.CancelFunc
}

type browserAcceptanceActivity struct {
	store                     *Store
	executionControl          *app.ExecutionControlService
	ownerPrincipal            identity.Principal
	instances                 *app.AgentInstanceService
	checkpoints               *app.AgentCheckpointService
	conversationID            string
	testUserName              string
	controlToken              string
	supervisorKey             []byte
	required                  []string
	mu                        sync.Mutex
	runs                      map[string]*browserActivityRun
	verified                  map[string]bool
	validationPreviewInjected bool
	watches                   map[uint64]browserActivityWatch
	requests                  []browserActivityEventRequest
	taskListRequests          []browserActivityTaskListRequest
	taskSnapshots             []browserActivityTaskSnapshotRequest
	messageSnapshots          []browserActivityMessageSnapshotRequest
	nextWatch                 uint64
}

func shouldAutoStopBrowserAcceptance(enabled, path string, activity *browserAcceptanceActivity) bool {
	return enabled == "1" && activity != nil && strings.HasPrefix(path, "/__morde_fixture/verify/") && activity.assertVerified() == nil
}

type browserAcceptanceHosts struct {
	nativeAcceptanceHosts
	store *Store
}

func (h browserAcceptanceHosts) List(ctx context.Context, principal identity.Principal) ([]domainhost.Host, error) {
	return h.store.ListHostsByOwner(ctx, principal.UserID)
}

func newBrowserAcceptanceActivity(store *Store, sealer *app.Sealer, conversationID string) (*browserAcceptanceActivity, error) {
	key := make([]byte, 32)
	key[0] = 1 // local test supervisor, never used by a production process
	authenticator, err := app.NewAgentStopAuthenticator("browser-fixture-supervisor", key)
	if err != nil {
		return nil, err
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		return nil, err
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	selected, required, err := browserActivitySelection(os.Getenv("MORDE_BROWSER_ACTIVITY_SCENARIOS"))
	if err != nil {
		return nil, err
	}
	a := &browserAcceptanceActivity{store: store, instances: instances, checkpoints: checkpoints,
		conversationID: conversationID, controlToken: hex.EncodeToString(token), supervisorKey: key,
		required: required, runs: make(map[string]*browserActivityRun), verified: make(map[string]bool),
		watches: make(map[uint64]browserActivityWatch)}
	for _, name := range selected {
		objective := browserActivityObjectives[name]
		a.runs[name] = &browserActivityRun{name: name, objective: objective, stage: "awaiting_message",
			commands: make(chan browserActivityCommand)}
	}
	return a, nil
}

func browserActivitySelection(raw string) ([]string, []string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"merge-a", "merge-b", "separate-a", "separate-b", "search", "stream", "stream-other"},
			[]string{"merge", "separate", "search", "stream", "reconnect"}, nil
	}

	groups := map[string][]string{
		"merge":                        {"merge-a", "merge-b"},
		"separate":                     {"separate-a", "separate-b"},
		"search":                       {"search"},
		"stream":                       {"stream", "stream-other"},
		"queued":                       {"queued"},
		"failed":                       {"failed"},
		"approval":                     {"approval"},
		"approval-resume":              {"approval-resume"},
		"approval-revoked":             {"approval-revoked"},
		"approval-host-member-revoked": {"approval-host-member-revoked"},
		"clarification":                {"clarification"},
		"recovery-unknown":             {"recovery-unknown"},
		"completion-race":              {"completion-race"},
		"interrupted":                  {"interrupted"},
		"mcp-retry":                    {"mcp-retry"},
		"route-retry":                  {"route-retry"},
		"target-control":               {"target-control-a", "target-control-b"},
	}
	var selected, required []string
	seen := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		scenario := strings.TrimSpace(item)
		runs, ok := groups[scenario]
		if !ok || seen[scenario] {
			return nil, nil, fmt.Errorf("invalid or duplicate browser activity scenario %q", scenario)
		}
		seen[scenario] = true
		required = append(required, scenario)
		if scenario == "stream" {
			required = append(required, "reconnect")
		}
		selected = append(selected, runs...)
	}
	return selected, required, nil
}

func TestBrowserActivitySelection(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		selected string
		required string
		wantErr  bool
	}{
		{name: "full default", selected: "merge-a,merge-b,separate-a,separate-b,search,stream,stream-other",
			required: "merge,separate,search,stream,reconnect"},
		{name: "search only", input: "search", selected: "search", required: "search"},
		{name: "stream includes reconnect", input: "stream", selected: "stream,stream-other", required: "stream,reconnect"},
		{name: "queued only", input: "queued", selected: "queued", required: "queued"},
		{name: "failed only", input: "failed", selected: "failed", required: "failed"},
		{name: "approval only", input: "approval", selected: "approval", required: "approval"},
		{name: "approval resume only", input: "approval-resume", selected: "approval-resume", required: "approval-resume"},
		{name: "approval authorization revocation only", input: "approval-revoked", selected: "approval-revoked", required: "approval-revoked"},
		{name: "shared Host member grant revocation only", input: "approval-host-member-revoked", selected: "approval-host-member-revoked", required: "approval-host-member-revoked"},
		{name: "clarification only", input: "clarification", selected: "clarification", required: "clarification"},
		{name: "recovery unknown only", input: "recovery-unknown", selected: "recovery-unknown", required: "recovery-unknown"},
		{name: "completion race only", input: "completion-race", selected: "completion-race", required: "completion-race"},
		{name: "interrupted only", input: "interrupted", selected: "interrupted", required: "interrupted"},
		{name: "MCP rate-limit retry only", input: "mcp-retry", selected: "mcp-retry", required: "mcp-retry"},
		{name: "message route retry only", input: "route-retry", selected: "route-retry", required: "route-retry"},
		{name: "failed and approval", input: "failed,approval", selected: "failed,approval", required: "failed,approval"},
		{name: "approval and clarification", input: "approval,clarification", selected: "approval,clarification", required: "approval,clarification"},
		{name: "target control starts two running tasks", input: "target-control",
			selected: "target-control-a,target-control-b", required: "target-control"},
		{name: "combined logical scenarios", input: "search, merge", selected: "search,merge-a,merge-b", required: "search,merge"},
		{name: "reject reconnect without stream", input: "reconnect", wantErr: true},
		{name: "reject duplicate", input: "search,search", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, required, err := browserActivitySelection(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected invalid scenario selection to be rejected")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(selected, ",") != test.selected || strings.Join(required, ",") != test.required {
				t.Fatalf("selection=%v required=%v", selected, required)
			}
		})
	}
}

func TestBrowserAcceptanceAutoStopRequiresAllVerifiedScenarios(t *testing.T) {
	activity := &browserAcceptanceActivity{required: []string{"approval-host-member-revoked"},
		verified: map[string]bool{"approval-host-member-revoked": true}}
	if !shouldAutoStopBrowserAcceptance("1", "/__morde_fixture/verify/approval-host-member-revoked", activity) {
		t.Fatal("auto-stop should follow successful verification of all selected scenarios")
	}
	if shouldAutoStopBrowserAcceptance("1", "/__morde_fixture/verify/approval-host-member-revoked", nil) {
		t.Fatal("auto-stop should require an activity fixture")
	}
	if shouldAutoStopBrowserAcceptance("1", "/__morde_fixture/advance/approval-host-member-revoked/approval", activity) {
		t.Fatal("auto-stop should not follow an advance request")
	}
	activity.verified["approval-host-member-revoked"] = false
	if shouldAutoStopBrowserAcceptance("1", "/__morde_fixture/verify/approval-host-member-revoked", activity) {
		t.Fatal("auto-stop should wait until verification succeeds")
	}
	activity.verified["approval-host-member-revoked"] = true
	if shouldAutoStopBrowserAcceptance("0", "/__morde_fixture/verify/approval-host-member-revoked", activity) {
		t.Fatal("auto-stop should remain opt-in")
	}
}

func TestBrowserActivityCrossClientScenario(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "", want: ""},
		{value: "1", want: "recovery-unknown"},
		{value: "recovery-unknown", want: "recovery-unknown"},
		{value: "approval", want: "approval"},
		{value: "approval-resume", want: "approval-resume"},
		{value: "approval-revoked", want: "approval-revoked"},
		{value: "approval-host-member-revoked", want: "approval-host-member-revoked"},
		{value: "interrupted", want: "interrupted"},
		{value: " unsupported ", want: ""},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv("MORDE_BROWSER_ACTIVITY_CROSS_CLIENT", test.value)
			if got := browserActivityCrossClientScenario(); got != test.want {
				t.Fatalf("browserActivityCrossClientScenario() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBrowserActivityOutcomeVerifyEndpoint(t *testing.T) {
	for _, scenario := range []string{"failed", "approval", "approval-resume", "approval-revoked", "approval-host-member-revoked", "clarification", "recovery-unknown", "completion-race", "interrupted", "mcp-retry"} {
		t.Run(scenario, func(t *testing.T) {
			stage := "progress"
			if scenario == "mcp-retry" {
				stage = "retry_claimed"
			}
			activity := &browserAcceptanceActivity{controlToken: "fixture-token", runs: map[string]*browserActivityRun{
				scenario: {name: scenario, stage: stage},
			}}
			request := httptest.NewRequest(http.MethodGet, "/__morde_fixture/verify/"+scenario, nil)
			request.Header.Set("X-Morde-Fixture-Token", activity.controlToken)
			response := httptest.NewRecorder()
			activity.serveControl(response, request)
			wantError := "has not reached"
			if scenario == "approval-revoked" || scenario == "approval-host-member-revoked" {
				wantError = "authorization revocation is incomplete"
			}
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), wantError) {
				t.Fatalf("verify endpoint status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestBrowserActivityInterruptedVerifyEndpoint(t *testing.T) {
	activity := &browserAcceptanceActivity{controlToken: "fixture-token", runs: map[string]*browserActivityRun{
		"interrupted": {name: "interrupted", stage: "streaming"},
	}}
	request := httptest.NewRequest(http.MethodGet, "/__morde_fixture/verify/interrupted", nil)
	request.Header.Set("X-Morde-Fixture-Token", activity.controlToken)
	response := httptest.NewRecorder()
	activity.serveControl(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "has not reached the cancelled stage") {
		t.Fatalf("verify endpoint status=%d body=%q", response.Code, response.Body.String())
	}
}

func (a *browserAcceptanceActivity) ExecuteTask(ctx context.Context, current task.Task) error {
	if current.Objective == browserActivityObjectives["mcp-retry"] {
		fmt.Printf("LOCAL_MCP_RETRY_EXECUTE status=%s step_retry=%t\n", current.Status, current.StepRetry != nil)
	}
	if current.Objective == browserActivityObjectives["mcp-retry"] && current.StepRetry == nil {
		a.mu.Lock()
		run := a.runs["mcp-retry"]
		retryHostID := ""
		if run != nil {
			retryHostID = run.retryHostID
		}
		a.mu.Unlock()
		if retryHostID == "" {
			return errors.New("local MCP retry Host was not prepared")
		}
		if _, err := a.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1 AND status='running'`, current.ID, retryHostID); err != nil {
			return fmt.Errorf("bind local MCP retry Task to its isolated Host: %w", err)
		}
		current.HostID = retryHostID
	}
	a.mu.Lock()
	var run *browserActivityRun
	for _, candidate := range a.runs {
		if candidate.objective == current.Objective {
			run = candidate
			break
		}
	}
	if run == nil {
		a.mu.Unlock()
		return (browserAcceptanceExecutor{store: a.store}).ExecuteTask(ctx, current)
	}
	if run.name == "mcp-retry" && current.StepRetry != nil {
		if run.stage != "failed" {
			a.mu.Unlock()
			return errors.New("MCP retry fixture received an unexpected retry execution")
		}
		run.current, run.stage = current, "retry_starting"
		a.mu.Unlock()
		lease, retriedTask, err := a.startMCPRetryExecution(ctx, current)
		if err != nil {
			a.failRun(run, err)
			return err
		}
		a.mu.Lock()
		run.current = retriedTask
		run.mcpRetryCallID = current.StepRetry.RetryCallID
		run.stage = "retry_claimed"
		a.mu.Unlock()
		renewContext, cancelRenew := context.WithCancel(ctx)
		defer cancelRenew()
		renewErr := make(chan error, 1)
		renewDone := make(chan struct{})
		defer func() { <-renewDone }()
		go func() {
			defer close(renewDone)
			ticker := time.NewTicker(8 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-renewContext.Done():
					return
				case <-ticker.C:
					if _, err := a.instances.Renew(renewContext, lease.Fence, app.MaxAgentLease); err != nil {
						select {
						case renewErr <- err:
						default:
						}
						return
					}
				}
			}
		}()
		return a.waitPhase(ctx, run, "retry-success", "retry_succeeded", renewErr, func() error {
			return a.commitMCPRetrySuccessOutcome(ctx, run, lease)
		})
	}
	if run.name == "clarification" && run.stage == "waiting_user" && run.current.ID == current.ID {
		run.current, run.stage = current, "clarification_resuming"
		a.mu.Unlock()
		if err := a.commitClarificationContinuation(ctx, run); err != nil {
			a.mu.Lock()
			run.stage, run.failure = "failed", err.Error()
			a.mu.Unlock()
			return err
		}
		return nil
	}
	if run.name == "approval-resume" && run.stage == "waiting_user" && run.current.ID == current.ID {
		run.current, run.stage = current, "approval_resuming"
		a.mu.Unlock()
		if err := a.commitApprovalContinuation(ctx, run); err != nil {
			a.mu.Lock()
			run.stage, run.failure = "failed", err.Error()
			a.mu.Unlock()
			return err
		}
		return nil
	}
	if run.stage != "awaiting_message" || current.ConversationID != a.conversationID {
		a.mu.Unlock()
		return errors.New("activity fixture received a duplicate or wrong-conversation message")
	}
	run.current, run.stage = current, "claimed"
	a.mu.Unlock()

	var lease app.AgentInstanceLease
	var renewErr <-chan error
	var cancelRenew context.CancelFunc
	var renewDone <-chan struct{}
	defer func() {
		if cancelRenew != nil {
			cancelRenew()
			<-renewDone
		}
	}()
	start := func() error {
		before, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
		if err != nil {
			return fmt.Errorf("read task before start: %w", err)
		}
		if before.CurrentRevisionStarted == nil || *before.CurrentRevisionStarted {
			return errors.New("task claimed without an unstarted revision")
		}
		lease, err = a.startCheckpoint(ctx, run)
		if err != nil {
			return err
		}
		renewContext, cancel := context.WithCancel(ctx)
		cancelRenew = cancel
		failure := make(chan error, 1)
		renewErr = failure
		done := make(chan struct{})
		renewDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(8 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-renewContext.Done():
					return
				case <-ticker.C:
					if _, err := a.instances.Renew(renewContext, lease.Fence, app.MaxAgentLease); err != nil {
						select {
						case failure <- err:
						default:
						}
						return
					}
				}
			}
		}()
		return a.verifyActivityEvents(ctx, run, false)
	}
	if err := a.waitPhase(ctx, run, "start", "started", nil, start); err != nil {
		return err
	}
	if err := a.waitPhase(ctx, run, "progress", "progress", renewErr, func() error {
		return a.commitProgress(ctx, run)
	}); err != nil {
		return err
	}
	if run.name == "failed" {
		return a.waitPhase(ctx, run, "fail", "failed", renewErr, func() error {
			return a.commitFailedOutcome(ctx, run)
		})
	}
	if run.name == "approval" || run.name == "approval-resume" || run.name == "approval-revoked" || run.name == "approval-host-member-revoked" {
		return a.waitPhase(ctx, run, "approval", "waiting_user", renewErr, func() error {
			return a.commitApprovalOutcome(ctx, run, lease)
		})
	}
	if run.name == "clarification" {
		return a.waitPhase(ctx, run, "clarification", "waiting_user", renewErr, func() error {
			return a.commitClarificationOutcome(ctx, run)
		})
	}
	if run.name == "recovery-unknown" {
		return a.waitPhase(ctx, run, "unknown", "waiting_user", renewErr, func() error {
			return a.commitRecoveryUnknownOutcome(ctx, run)
		})
	}
	if run.name == "mcp-retry" {
		return a.waitPhase(ctx, run, "fail", "failed", renewErr, func() error {
			return a.commitMCPRetryFailureOutcome(ctx, run, lease)
		})
	}
	var delta app.CloudDSHTextDelta
	if run.name == "stream" || run.name == "interrupted" {
		if err := a.waitPhase(ctx, run, "stream", "streaming", renewErr, func() error {
			var err error
			delta, err = a.startStream(ctx, run, lease)
			return err
		}); err != nil {
			return err
		}
		if run.name == "interrupted" {
			<-ctx.Done()
			return ctx.Err()
		}
	}
	if run.name == "completion-race" {
		if err := a.waitPhase(ctx, run, "candidate", "completion_candidate", renewErr, func() error {
			a.mu.Lock()
			run.completionCandidate = "STALE COMPLETION RESULT"
			a.mu.Unlock()
			return nil
		}); err != nil {
			return err
		}
		return a.waitPhase(ctx, run, "commit", "completed", renewErr, func() error {
			return a.completeAfterSteer(ctx, run)
		})
	}
	return a.waitPhase(ctx, run, "complete", "completed", renewErr, func() error {
		return a.complete(ctx, run, delta)
	})
}

func (a *browserAcceptanceActivity) commitClarificationContinuation(ctx context.Context, run *browserActivityRun) error {
	current := run.current
	details, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read resumed clarification Task: %w", err)
	}
	if details.Task.Status != task.StatusRunning || len(details.ClarificationRequests) != 1 {
		return errors.New("clarification continuation requires a running Task and one answered request")
	}
	request := details.ClarificationRequests[0]
	if request.Status != task.ClarificationAnswered || request.Answer != browserActivityClarificationAnswer {
		return errors.New("clarification continuation did not receive the expected persisted answer")
	}
	var lastSequence int64
	for _, step := range details.Steps {
		if step.Sequence > lastSequence {
			lastSequence = step.Sequence
		}
	}
	progressStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	resultStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	progress := task.Step{ID: progressStepID, TaskID: current.ID, Sequence: lastSequence + 1, Kind: "progress",
		Status: task.StepSucceeded, Title: "收到补充信息", Summary: "已记录你提供的报告目录，继续完成本地合成任务。"}
	result := "已收到补充信息：报告目录为 " + request.Answer + "。本地合成任务已继续完成；未创建文件。"
	resultStep := task.Step{ID: resultStepID, TaskID: current.ID, Sequence: lastSequence + 2, Kind: "result",
		Status: task.StepSucceeded, Title: "处理完成", Summary: "本地合成任务已根据补充信息完成。"}
	payload, err := json.Marshal(map[string]any{"result": result, "step_id": resultStep.ID, "status": string(task.StepSucceeded)})
	if err != nil {
		return fmt.Errorf("encode resumed clarification result: %w", err)
	}
	event, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: resultStep.ID, Result: result,
		Usage: current.Usage, Steps: []task.Step{progress, resultStep}, PublishResultMessage: true,
		Event: task.Event{Type: "task.result", Payload: payload}})
	if err != nil {
		return fmt.Errorf("commit resumed clarification result: %w", err)
	}
	completed, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read completed clarification Task: %w", err)
	}
	a.mu.Lock()
	run.current = completed.Task
	run.resultEventSequence = event.Sequence
	run.clarificationAnswer = request.Answer
	a.mu.Unlock()
	if err := a.verifyActivityEvents(ctx, run, true); err != nil {
		return err
	}
	a.mu.Lock()
	run.stage = "completed"
	a.mu.Unlock()
	return nil
}

func (a *browserAcceptanceActivity) startMCPRetryExecution(ctx context.Context, current task.Task) (app.AgentInstanceLease, task.Task, error) {
	if current.StepRetry == nil || current.StepRetry.TaskID != current.ID || current.StepRetry.OwnerID != current.OwnerID ||
		current.StepRetry.AgentSessionID != current.AgentSessionID || current.StepRetry.RetryCallID == "" {
		return app.AgentInstanceLease{}, task.Task{}, errors.New("claimed MCP retry has no valid immutable intent")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return app.AgentInstanceLease{}, task.Task{}, fmt.Errorf("create MCP retry supervisor nonce: %w", err)
	}
	engineVersion, profileVersion := current.AgentEngineVersion, current.AgentProfileVersion
	if engineVersion == "" {
		engineVersion = "0.2.0-rc.2"
	}
	if profileVersion == "" {
		profileVersion = "v1"
	}
	lease, replayed, err := a.instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		AgentInstanceID: "browser_retry_agent_" + current.StepRetry.AttemptStepID,
		AgentSessionID:  current.StepRetry.AgentSessionID, SupervisorID: "browser-fixture-supervisor",
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		AuthorizationVersion: current.Authorization.Version, EngineVersion: engineVersion, ProfileVersion: profileVersion,
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed {
		if err == nil {
			err = errors.New("MCP retry Agent start unexpectedly replayed")
		}
		return app.AgentInstanceLease{}, task.Task{}, fmt.Errorf("start synthetic MCP retry Agent: %w", err)
	}
	if lease.Fence.AgentGeneration <= current.AgentGeneration || lease.Fence.AgentSessionID != current.StepRetry.AgentSessionID {
		return app.AgentInstanceLease{}, task.Task{}, errors.New("synthetic MCP retry Agent did not advance the pinned session generation")
	}
	if err := a.store.ConsumeTaskStepRetryIntent(ctx, *current.StepRetry, lease.Fence); err != nil {
		return app.AgentInstanceLease{}, task.Task{}, fmt.Errorf("consume MCP retry intent at the synthetic Agent fence: %w", err)
	}
	details, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return app.AgentInstanceLease{}, task.Task{}, fmt.Errorf("read claimed MCP retry Task after intent consumption: %w", err)
	}
	if details.Task.AgentInstanceID != lease.Fence.AgentInstanceID || details.Task.AgentGeneration != lease.Fence.AgentGeneration ||
		details.Task.Status != task.StatusRunning {
		return app.AgentInstanceLease{}, task.Task{}, errors.New("synthetic MCP retry fence was not durably consumed")
	}
	consumedIntent := *current.StepRetry
	consumedIntent.Consumed = true
	details.Task.StepRetry = &consumedIntent
	a.mu.Lock()
	run := a.runs["mcp-retry"]
	if run != nil {
		run.agentLease = lease
		run.supervisorNonce = append([]byte(nil), nonce...)
	}
	a.mu.Unlock()
	return lease, details.Task, nil
}

func (a *browserAcceptanceActivity) commitMCPRetrySuccessOutcome(ctx context.Context, run *browserActivityRun,
	lease app.AgentInstanceLease) error {
	a.mu.Lock()
	current, sourceStepID, retryCallID, dispatchCount := run.current, run.mcpRetrySourceStepID, run.mcpRetryCallID, run.mcpRetryDispatchCount
	nonce := append([]byte(nil), run.supervisorNonce...)
	a.mu.Unlock()
	intent := current.StepRetry
	if intent == nil || !intent.Consumed || intent.SourceStepID != sourceStepID || intent.RetryCallID != retryCallID ||
		intent.AttemptStepID != current.CurrentStep || dispatchCount != 0 {
		return errors.New("synthetic MCP retry execution lost its consumed single-use intent")
	}
	details, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read MCP retry Task before synthetic success: %w", err)
	}
	var source, attempt *task.Step
	for i := range details.Steps {
		step := &details.Steps[i]
		if step.ID == sourceStepID {
			source = step
		}
		if step.ID == intent.AttemptStepID {
			attempt = step
		}
	}
	if details.Task.Status != task.StatusRunning || source == nil || attempt == nil || source.Status != task.StepFailed ||
		source.FailureReason != "connector_rate_limited" ||
		(attempt.Status != task.StepQueued && attempt.Status != task.StepRunning) ||
		attempt.RetryOfStepID != source.ID || attempt.Attempt != intent.Attempt || attempt.ToolCallKey != intent.RetryCallID ||
		attempt.ToolName != intent.ToolName || !bytes.Equal(attempt.RequestDigest, intent.RequestDigest) {
		return fmt.Errorf("synthetic MCP retry source or reserved attempt changed: source=%+v retry=%+v", source, attempt)
	}
	if len(intent.RequestDigest) != sha256.Size {
		return errors.New("synthetic MCP retry request digest is invalid")
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return err
	}
	operationResult := `{"is_error":false,"text":["synthetic connector result after explicit retry"]}`
	inserted, err := a.store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,result)
SELECT $1,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,$2,
host_id,workspace_id,binding_version,$3,$4,$5,tool_name,request_hash,'succeeded',$6
FROM task_mcp_connector_operations WHERE task_id=$7 AND owner_id=$8 AND call_id=$9 AND status='failed'
AND request_hash=$10`, operationID, current.Lease.Version, intent.TaskRevision, intent.RetryCallID,
		app.AgentOperationKey(intent.RetryCallID), operationResult, current.ID, current.OwnerID, intent.SourceCallID,
		intent.RequestDigest)
	if err != nil {
		return fmt.Errorf("persist one synthetic successful MCP retry operation: %w", err)
	}
	if inserted.RowsAffected() != 1 {
		return fmt.Errorf("synthetic MCP retry operation rows inserted=%d, want exactly one", inserted.RowsAffected())
	}
	a.mu.Lock()
	run.mcpRetryDispatchCount++
	dispatchCount = run.mcpRetryDispatchCount
	a.mu.Unlock()
	if dispatchCount != 1 {
		return fmt.Errorf("synthetic MCP retry dispatch count=%d, want one", dispatchCount)
	}
	step := *attempt
	step.Status, step.Summary, step.Phase = task.StepSucceeded, "本地合成连接器重试成功。", "execute"
	step.AgentSessionID, step.TaskRevision = intent.AgentSessionID, &intent.TaskRevision
	step.ToolCallKey, step.ToolName = intent.RetryCallID, intent.ToolName
	step.RequestDigest = append([]byte(nil), intent.RequestDigest...)
	step.FailureReason, step.Retryable = "", false
	result := "本地合成连接器在用户显式重试后成功。"
	payload, err := json.Marshal(map[string]any{"result": result, "step_id": step.ID, "status": string(task.StepSucceeded)})
	if err != nil {
		return fmt.Errorf("encode synthetic MCP retry result: %w", err)
	}
	usage := current.Usage
	usage.ToolCalls++
	event, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: step.ID, Result: result,
		Usage: usage, Steps: []task.Step{step}, PublishResultMessage: true,
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
			AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			TaskRevision: intent.TaskRevision, CallKey: intent.RetryCallID, ToolName: intent.ToolName,
			RequestDigest: append([]byte(nil), intent.RequestDigest...), Phase: "execute", DetailState: "not_recorded"},
		Event: task.Event{TaskID: current.ID, Type: "task.result", Payload: payload}})
	if err != nil {
		return fmt.Errorf("commit successful synthetic MCP retry step and Task result: %w", err)
	}
	receipt := app.AgentStopReceipt{Fence: lease.Fence, SupervisorNonce: nonce, TreeStopped: true}
	signingPayload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		return fmt.Errorf("encode synthetic MCP retry Agent stop receipt: %w", err)
	}
	signer := hmac.New(sha256.New, a.supervisorKey)
	_, _ = signer.Write(signingPayload)
	receipt.Signature = signer.Sum(nil)
	if _, err := a.instances.ConfirmStopped(ctx, receipt); err != nil {
		return fmt.Errorf("confirm synthetic MCP retry Agent stopped: %w", err)
	}
	completed, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read completed synthetic MCP retry Task: %w", err)
	}
	a.mu.Lock()
	run.current = completed.Task
	run.resultEventSequence = event.Sequence
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, true)
}

func (a *browserAcceptanceActivity) waitPhase(ctx context.Context, run *browserActivityRun, expected, next string,
	renewErr <-chan error, execute func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-renewErr:
		a.failRun(run, err)
		return err
	case command := <-run.commands:
		var err error
		if command.phase != expected {
			err = fmt.Errorf("fixture expected %s, got %s", expected, command.phase)
		} else {
			err = execute()
		}
		a.mu.Lock()
		if err != nil {
			run.stage, run.failure = "failed", err.Error()
		} else {
			run.stage = next
		}
		a.mu.Unlock()
		command.reply <- err
		return err
	}
}

func (a *browserAcceptanceActivity) failRun(run *browserActivityRun, err error) {
	a.mu.Lock()
	run.stage, run.failure = "failed", err.Error()
	a.mu.Unlock()
}

func (a *browserAcceptanceActivity) startCheckpoint(ctx context.Context, run *browserActivityRun) (app.AgentInstanceLease, error) {
	current := run.current
	var userID, userContent string
	err := a.store.pool.QueryRow(ctx, `SELECT id,content FROM conversation_messages
WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1`, current.ID).Scan(&userID, &userContent)
	if err != nil {
		return app.AgentInstanceLease{}, fmt.Errorf("read synthetic task input: %w", err)
	}
	if userContent != run.objective {
		return app.AgentInstanceLease{}, errors.New("task input is not the expected synthetic message")
	}
	a.mu.Lock()
	run.userMessageID = userID
	a.mu.Unlock()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return app.AgentInstanceLease{}, fmt.Errorf("create synthetic supervisor nonce: %w", err)
	}
	lease, replayed, err := a.instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		AgentInstanceID: "browser_agent_" + current.ID, AgentSessionID: "browser_session_" + current.ID,
		SupervisorID: "browser-fixture-supervisor", LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		AuthorizationVersion: current.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		return app.AgentInstanceLease{}, fmt.Errorf("start synthetic agent: %w", err)
	}
	if replayed {
		return app.AgentInstanceLease{}, errors.New("synthetic agent start unexpectedly replayed")
	}
	a.mu.Lock()
	run.agentLease = lease
	run.supervisorNonce = append([]byte(nil), nonce...)
	a.mu.Unlock()
	fence := app.AgentCheckpointFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	payload, err := json.Marshal(map[string]any{"header": map[string]any{"id": lease.Fence.AgentSessionID}, "from_seq": 0, "events": []any{
		map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{"id": userID, "role": "user",
			"content": []any{map[string]string{"type": "text", "text": userContent}}}},
		map[string]any{"seq": 1, "type": "turn/start", "data": map[string]any{"turn": 1}},
	}})
	if err != nil {
		return app.AgentInstanceLease{}, err
	}
	digest := sha256.Sum256(payload)
	command := app.AgentCheckpointCommand{Fence: fence, From: 0, Through: 1, Digest: digest[:], Payload: payload}
	if replayed, err := a.checkpoints.Save(ctx, command); err != nil {
		return app.AgentInstanceLease{}, fmt.Errorf("save start checkpoint: %w", err)
	} else if replayed {
		return app.AgentInstanceLease{}, errors.New("first start checkpoint unexpectedly replayed")
	}
	if replayed, err := a.checkpoints.Save(ctx, command); err != nil {
		return app.AgentInstanceLease{}, fmt.Errorf("replay start checkpoint: %w", err)
	} else if !replayed {
		return app.AgentInstanceLease{}, errors.New("start checkpoint replay created a second checkpoint")
	}
	a.mu.Lock()
	run.checkpointFence = fence
	run.checkpointThrough = command.Through
	run.checkpointDigest = append([]byte(nil), command.Digest...)
	a.mu.Unlock()
	return lease, nil
}

func (a *browserAcceptanceActivity) commitProgress(ctx context.Context, run *browserActivityRun) error {
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	current := run.current
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: 2, Kind: "progress", Status: task.StepRunning,
		Title: browserActivityProgressTitle(run), Summary: browserActivityProgressSummary(run, false)}
	payload, err := json.Marshal(map[string]any{"step_id": step.ID, "kind": step.Kind, "status": step.Status,
		"title": step.Title, "summary": step.Summary})
	if err != nil {
		return err
	}
	_, err = a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusRunning, CurrentStep: step.ID, Usage: current.Usage,
		Steps: []task.Step{step}, Event: task.Event{Type: "task.step", Payload: payload}})
	if err != nil {
		return err
	}
	a.mu.Lock()
	run.progressStepID = step.ID
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) commitFailedOutcome(ctx context.Context, run *browserActivityRun) error {
	current := run.current
	step := task.Step{ID: run.progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress", Status: task.StepFailed,
		Title: browserActivityProgressTitle(run), Summary: "测试任务已在接单后失败。"}
	_, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusFailed, CurrentStep: step.ID,
		Result: "本地合成失败：任务已接单后失败。", Usage: current.Usage, Steps: []task.Step{step},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"error":"synthetic_failure"}`)}})
	if err != nil {
		return fmt.Errorf("commit synthetic task failure: %w", err)
	}
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) commitMCPRetryFailureOutcome(ctx context.Context, run *browserActivityRun,
	lease app.AgentInstanceLease) error {
	current := run.current
	var revision int64
	if err := a.store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, current.ID).Scan(&revision); err != nil {
		return fmt.Errorf("read MCP retry Task revision: %w", err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return err
	}
	callID := "browser-rate-limit-call-" + current.ID
	connectorID := "k1234567"
	requestDigest := sha256.Sum256([]byte(`{"connector_id":"k1234567","operation":"lookup","arguments":{"query":"synthetic"}}`))
	operationResult := `{"is_error":true,"structured_content":{"code":"connector_rate_limited","terminal_for_attempt":true,"retry_after_seconds":1}}`
	if _, err := a.store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,builtin_service,connector_revision,credential_version,grant_version,lease_version,
host_id,workspace_id,binding_version,task_revision,call_id,operation_key,tool_name,request_hash,status,result,updated_at)
VALUES ($1,$2,$3,$4,NULL,1,0,1,$5,$6,'',0,$7,$8,$9,'mcp_k1234567_0123456789ab',$10,'failed',$11,clock_timestamp()-interval '2 seconds')`,
		operationID, current.ID, current.OwnerID, connectorID, current.Lease.Version, current.HostID, revision,
		callID, app.AgentOperationKey(callID), requestDigest[:], operationResult); err != nil {
		return fmt.Errorf("seed local MCP rate-limit ledger: %w", err)
	}
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: 3, Kind: "tool", Status: task.StepFailed,
		Title: "调用本地合成连接器", Summary: "本地合成连接器返回明确限流结果。", AgentSessionID: lease.Fence.AgentSessionID,
		TaskRevision: &revision, ToolCallKey: callID, ToolName: "mcp_call", RequestDigest: requestDigest[:],
		Phase: "execute", DetailState: "not_recorded", Attempt: 1, FailureReason: "connector_rate_limited", Retryable: true}
	a.mu.Lock()
	run.mcpRetrySourceStepID = step.ID
	a.mu.Unlock()
	toolCall := &app.TaskToolCallProgress{AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
		AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		TaskRevision: revision, CallKey: callID, ToolName: step.ToolName, RequestDigest: requestDigest[:],
		Phase: step.Phase, DetailState: step.DetailState, FailureReason: step.FailureReason, Retryable: true}
	payload, err := json.Marshal(map[string]any{"status": task.StatusFailed, "reason": "connector_rate_limited"})
	if err != nil {
		return err
	}
	if _, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusFailed, CurrentStep: step.ID,
		Result: "The selected connector rate limited this attempt.", Usage: current.Usage, Steps: []task.Step{step}, ToolCall: toolCall,
		Event: task.Event{TaskID: current.ID, Type: "task.result", Payload: payload}}); err != nil {
		return fmt.Errorf("commit local MCP rate-limit failure: %w", err)
	}
	receipt := app.AgentStopReceipt{Fence: lease.Fence, SupervisorNonce: append([]byte(nil), run.supervisorNonce...), TreeStopped: true}
	signingPayload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		return err
	}
	signer := hmac.New(sha256.New, a.supervisorKey)
	_, _ = signer.Write(signingPayload)
	receipt.Signature = signer.Sum(nil)
	if _, err := a.instances.ConfirmStopped(ctx, receipt); err != nil {
		return fmt.Errorf("confirm local synthetic Agent stop: %w", err)
	}
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) commitApprovalOutcome(ctx context.Context, run *browserActivityRun, lease app.AgentInstanceLease) error {
	current := run.current
	step := task.Step{ID: run.progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress", Status: task.StepWaitingUser,
		Title: browserActivityProgressTitle(run), Summary: "测试任务正在等待用户授权。"}
	pendingID, err := domain.NewPendingRequestID()
	if err != nil {
		return err
	}
	requestHash := sha256.Sum256([]byte(current.ID + ":browser-fixture-approval"))
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	pending := &task.PendingRequest{
		ID: pendingID, TaskID: current.ID, Version: 1, Kind: "approval", Title: "确认合成测试操作",
		Object: "本地合成资源", Content: "此测试任务需要用户授权后继续。",
		Impact: "仅用于本地浏览器验收，不会执行外部操作。",
		CallID: "browser-approval-" + current.ID, ToolName: "browser_fixture_approval",
		RequestHash: requestHash[:], RuntimeGeneration: lease.Fence.AgentGeneration,
		RuntimeInstanceID: lease.Fence.AgentInstanceID, ControlVersion: 0,
		Status: task.PendingRequestPending, ExpiresAt: &expiresAt,
	}
	_, err = a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusWaitingUser, CurrentStep: step.ID,
		WaitingReason: "approval_required", Usage: current.Usage, Steps: []task.Step{step},
		Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)}, PendingRequest: pending})
	if err != nil {
		return fmt.Errorf("commit synthetic approval wait: %w", err)
	}
	if run.name == "approval-host-member-revoked" {
		if run.revocationHostID == "" || run.revocationHostOwnerID == "" {
			return errors.New("local shared Host grant fixture was not prepared")
		}
		bound, err := a.store.pool.Exec(ctx, `UPDATE tasks SET host_id=$2 WHERE id=$1 AND initiator_id=$3 AND status='waiting_user'`,
			current.ID, run.revocationHostID, current.InitiatorID)
		if err != nil {
			return fmt.Errorf("bind approval-waiting Task to shared Host: %w", err)
		}
		if bound.RowsAffected() != 1 {
			return fmt.Errorf("bind approval-waiting Task to shared Host affected %d rows, want one", bound.RowsAffected())
		}
		current.HostID = run.revocationHostID
	}
	a.mu.Lock()
	run.current = current
	run.approvalID, run.approvalVersion = pending.ID, pending.Version
	run.approvalCallID = pending.CallID
	run.approvalRequestHash = append([]byte(nil), pending.RequestHash...)
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) commitApprovalContinuation(ctx context.Context, run *browserActivityRun) error {
	current := run.current
	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read approved synthetic Task: %w", err)
	}
	if details.Task.Status != task.StatusRunning || details.PendingRequest == nil ||
		details.PendingRequest.ID != run.approvalID || details.PendingRequest.Version != run.approvalVersion ||
		details.PendingRequest.Status != task.PendingRequestApproved ||
		details.PendingRequest.Decision != task.PendingDecisionApprove ||
		details.PendingRequest.CallID != run.approvalCallID ||
		!bytes.Equal(details.PendingRequest.RequestHash, run.approvalRequestHash) {
		return errors.New("approval continuation did not resume the exact approved Task call")
	}

	progress := task.Step{ID: run.progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress",
		Status: task.StepSucceeded, Title: browserActivityProgressTitle(run), Summary: "审批已批准，任务从原等待点继续；未执行外部副作用。"}
	resultStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	resultStep := task.Step{ID: resultStepID, TaskID: current.ID, Sequence: 3, Kind: "result",
		Status: task.StepSucceeded, Title: "处理完成", Summary: "本地合成操作已获批准，任务从原等待点继续完成。"}
	payload, err := json.Marshal(map[string]any{"result": browserActivityApprovalResumeResult,
		"step_id": resultStep.ID, "status": task.StepSucceeded, "approval_id": run.approvalID,
		"approval_version": run.approvalVersion, "call_id": run.approvalCallID})
	if err != nil {
		return fmt.Errorf("encode synthetic approval continuation result: %w", err)
	}
	event, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: resultStep.ID,
		Result: browserActivityApprovalResumeResult, Usage: current.Usage, Steps: []task.Step{progress, resultStep},
		PublishResultMessage: true,
		Event:                task.Event{TaskID: current.ID, Type: "task.result", Payload: payload}})
	if err != nil {
		return fmt.Errorf("commit synthetic approval continuation: %w", err)
	}
	completed, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read completed synthetic approval Task: %w", err)
	}
	a.mu.Lock()
	run.current = completed.Task
	run.resultEventSequence = event.Sequence
	run.approvalResumeCount++
	run.stage = "completed"
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, true)
}

func (a *browserAcceptanceActivity) commitClarificationOutcome(ctx context.Context, run *browserActivityRun) error {
	current := run.current
	modelStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	clarificationStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	clarificationID, err := domain.NewPendingRequestID()
	if err != nil {
		return err
	}
	callID := "browser-clarification-" + current.ID
	question := "请告诉我应该把报告保存到哪个目录？"
	modelStep := task.Step{ID: modelStepID, TaskID: current.ID, Sequence: 3, Kind: "model", Status: task.StepSucceeded,
		Title: "整理任务要求", Summary: "已识别还缺少一个必要信息。"}
	clarificationStep := task.Step{ID: clarificationStepID, TaskID: current.ID, Sequence: 4, Kind: "clarification",
		Status: task.StepWaitingUser, Title: "需要补充一个信息", Summary: question}
	request := &task.ClarificationRequest{ID: clarificationID, TaskID: current.ID, Version: 1,
		ModelStepID: modelStep.ID, CallID: callID, Question: question, Status: task.ClarificationPending}
	payload, err := json.Marshal(map[string]any{"reason": "clarification_required", "step_id": clarificationStep.ID,
		"clarification_id": request.ID, "clarification_version": request.Version, "call_id": request.CallID})
	if err != nil {
		return err
	}
	_, err = a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusWaitingUser, CurrentStep: clarificationStep.ID,
		WaitingReason: "clarification_required", Usage: current.Usage, Steps: []task.Step{modelStep, clarificationStep},
		Event: task.Event{Type: "task.waiting_user", Payload: payload}, ClarificationRequest: request})
	if err != nil {
		return fmt.Errorf("commit synthetic clarification wait: %w", err)
	}
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) commitRecoveryUnknownOutcome(ctx context.Context, run *browserActivityRun) error {
	current := run.current
	if !domain.ValidStepID(run.progressStepID) {
		return errors.New("recovery-unknown fixture is missing its previously running progress step")
	}
	confirmedStep := task.Step{ID: run.progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress",
		Status: task.StepSucceeded, Title: browserActivityProgressTitle(run), Summary: browserActivityProgressSummary(run, true)}
	stepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	step := task.Step{ID: stepID, TaskID: current.ID, Sequence: 3, Kind: "progress", Status: task.StepUnknown,
		Title: "核对中断的执行状态", Summary: "无法确认上次响应是否完整，任务已暂停等待核对。"}
	result := "恢复检查无法确认上一轮响应是否完成；任务已暂停，请在任务详情核对后再继续。"
	payload, err := json.Marshal(map[string]any{"reason": "dsh_recovery_unresolved", "step_id": step.ID})
	if err != nil {
		return err
	}
	_, err = a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusWaitingUser, CurrentStep: step.ID,
		WaitingReason: "dsh_recovery_unresolved", Result: result, Usage: current.Usage, Steps: []task.Step{confirmedStep, step},
		Event: task.Event{Type: "task.waiting_user", Payload: payload}})
	if err != nil {
		return fmt.Errorf("commit synthetic unresolved recovery wait: %w", err)
	}
	return a.verifyActivityEvents(ctx, run, false)
}

func (a *browserAcceptanceActivity) startStream(ctx context.Context, run *browserActivityRun, lease app.AgentInstanceLease) (app.CloudDSHTextDelta, error) {
	current := run.current
	fence := app.ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AgentRootSessionID: lease.Fence.AgentSessionID,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		AuthorizationVersion: lease.Fence.AuthorizationVersion, ThroughSequence: 1,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
	callID, err := app.DSHModelCallID(current.ID, lease.Fence.AgentSessionID, 1)
	if err != nil {
		return app.CloudDSHTextDelta{}, err
	}
	// The gateway dispatch is synthetic; actual delta persistence, event
	// creation and final-message reuse use the production store paths.
	result, err := a.store.pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,task_id,reserved_tokens,state,personal_model)
SELECT $1,'task',$2,$3,id,model_name,$4,100,'dispatched',owner_id IS NOT NULL FROM model_connections WHERE id=$5`,
		current.InitiatorID, callID, make([]byte, 32), current.ID, current.ModelID)
	if err != nil {
		return app.CloudDSHTextDelta{}, fmt.Errorf("create synthetic model ledger: %w", err)
	}
	if result.RowsAffected() != 1 {
		return app.CloudDSHTextDelta{}, errors.New("synthetic stream model is unavailable")
	}
	result, err = a.store.pool.Exec(ctx, `INSERT INTO model_text_streaming_dispatches
(owner_id,source,call_id,task_id,task_owner_id,initiator_id,model_id,connection_version,task_revision,
 agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,lease_owner,lease_version,authorization_version,through_sequence,engine_version,profile_version)
SELECT $1,'task',$2,$3,$4,$1,id,updated_at,0,$5,$6,$7,$7,$8,$9,$10,1,$11,$12 FROM model_connections WHERE id=$13`,
		current.InitiatorID, callID, current.ID, current.OwnerID, lease.Fence.AgentInstanceID, lease.Fence.AgentGeneration,
		lease.Fence.AgentSessionID, lease.Fence.LeaseOwner, lease.Fence.LeaseVersion, lease.Fence.AuthorizationVersion,
		lease.Fence.EngineVersion, lease.Fence.ProfileVersion, current.ModelID)
	if err != nil {
		return app.CloudDSHTextDelta{}, fmt.Errorf("create synthetic stream dispatch: %w", err)
	}
	if result.RowsAffected() != 1 {
		return app.CloudDSHTextDelta{}, errors.New("synthetic stream dispatch model is unavailable")
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		return app.CloudDSHTextDelta{}, err
	}
	turnID, err := domain.NewStepID()
	if err != nil {
		return app.CloudDSHTextDelta{}, err
	}
	delta := app.CloudDSHTextDelta{Fence: fence, CallID: callID, MessageID: messageID, TurnID: turnID,
		Text: browserActivityStreamPrefix}
	appended, err := a.store.AppendCloudDSHTextDelta(ctx, delta)
	if err != nil {
		return app.CloudDSHTextDelta{}, fmt.Errorf("start visible stream: %w", err)
	}
	if !appended {
		return app.CloudDSHTextDelta{}, errors.New("visible stream could not start while other work was active")
	}
	var sequence int64
	var status, content string
	if err := a.store.pool.QueryRow(ctx, `SELECT sequence,output_status,content FROM conversation_messages WHERE id=$1`, messageID).
		Scan(&sequence, &status, &content); err != nil {
		return app.CloudDSHTextDelta{}, fmt.Errorf("read first visible stream: %w", err)
	}
	if sequence < 1 || status != "streaming" || content != browserActivityStreamPrefix {
		return app.CloudDSHTextDelta{}, fmt.Errorf("first visible stream is invalid: sequence=%d status=%s", sequence, status)
	}
	a.mu.Lock()
	run.streamMessageID, run.streamTurnID, run.streamSequence = messageID, turnID, sequence
	a.mu.Unlock()
	return delta, nil
}

func (a *browserAcceptanceActivity) completeAfterSteer(ctx context.Context, run *browserActivityRun) error {
	a.mu.Lock()
	current, checkpointFence := run.current, run.checkpointFence
	checkpointThrough, checkpointDigest := run.checkpointThrough, append([]byte(nil), run.checkpointDigest...)
	candidateResult, progressStepID := run.completionCandidate, run.progressStepID
	a.mu.Unlock()
	if current.ID == "" || candidateResult == "" || !domain.ValidStepID(progressStepID) || checkpointThrough < 1 || len(checkpointDigest) != sha256.Size {
		return errors.New("completion-race fixture is missing its fenced candidate")
	}
	var pendingRoute int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages message
JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.task_id=$1 AND message.owner_id=$2 AND message.content=$3 AND message.delivery_status='pending'
 AND message.delivery_mode='steer' AND route.state='routed'`, current.ID, current.OwnerID,
		browserActivityCompletionRaceSteer).Scan(&pendingRoute); err != nil || pendingRoute != 1 {
		return fmt.Errorf("completion-race steer was not routed to the original Task before commit: count=%d: %w", pendingRoute, err)
	}
	staleStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	stalePayload, err := json.Marshal(map[string]any{"result": candidateResult, "step_id": staleStepID})
	if err != nil {
		return fmt.Errorf("encode stale completion candidate: %w", err)
	}
	_, err = a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: staleStepID, Result: candidateResult,
		Usage: current.Usage, Steps: []task.Step{{ID: staleStepID, TaskID: current.ID, Sequence: 3, Kind: "result",
			Status: task.StepSucceeded, Title: "处理完成", Summary: "补充到达前生成的旧结果候选。"}}, PublishResultMessage: true,
		AgentCompletionFence: &app.TaskAgentCompletionFence{AgentSessionID: checkpointFence.AgentSessionID,
			AgentInstanceID: checkpointFence.AgentInstanceID, AgentGeneration: checkpointFence.AgentGeneration,
			AuthorizationVersion: checkpointFence.AuthorizationVersion, CheckpointThroughSequence: checkpointThrough,
			CheckpointDigest: checkpointDigest},
		Event: task.Event{TaskID: current.ID, Type: "task.result", Payload: stalePayload}})
	var pendingErr *app.TaskInputPendingError
	if !errors.Is(err, app.ErrTaskInputPending) || !errors.As(err, &pendingErr) || len(pendingErr.PendingMessages) != 1 ||
		pendingErr.PendingMessages[0].Content != browserActivityCompletionRaceSteer {
		if err == nil {
			return errors.New("stale completion candidate was not fenced by the routed steer")
		}
		pendingCount := 0
		if pendingErr != nil {
			pendingCount = len(pendingErr.PendingMessages)
		}
		return fmt.Errorf("stale completion candidate was not fenced by the routed steer: pending_count=%d: %w", pendingCount, err)
	}
	var staleEvents, staleReceipts int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, current.ID).Scan(&staleEvents); err != nil {
		return fmt.Errorf("check rejected completion event: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND role='assistant'`, current.ID).Scan(&staleReceipts); err != nil {
		return fmt.Errorf("check rejected completion receipt: %w", err)
	}
	if staleEvents != 0 || staleReceipts != 0 {
		return fmt.Errorf("rejected completion candidate escaped its fence: events=%d receipts=%d", staleEvents, staleReceipts)
	}
	if err := a.waitForIndependentCompletionResultWindow(ctx, current); err != nil {
		return err
	}
	pendingMessages, err := a.checkpoints.PendingMessages(ctx, checkpointFence)
	if err != nil || len(pendingMessages) != 1 || pendingMessages[0].Mode != app.AgentMessageDeliverySteer ||
		pendingMessages[0].Content != browserActivityCompletionRaceSteer {
		return fmt.Errorf("read pending steer from the original Agent session: messages=%+v: %w", pendingMessages, err)
	}
	steer := pendingMessages[0]
	checkpointPayload, err := json.Marshal(map[string]any{"header": map[string]any{"id": checkpointFence.AgentSessionID},
		"from_seq": checkpointThrough + 1, "events": []any{
			map[string]any{"seq": checkpointThrough + 1, "type": "agent/inbox/spliced", "data": map[string]any{
				"target": "next-step", "start": 0, "inserted": []any{map[string]any{"id": steer.ID, "role": "user",
					"content": []any{map[string]string{"type": "text", "text": steer.Content}},
					"source":  map[string]string{"kind": "user"}}}}},
			map[string]any{"seq": checkpointThrough + 2, "type": "user/message", "data": map[string]any{"id": steer.ID,
				"role": "user", "content": []any{map[string]string{"type": "text", "text": steer.Content}}}},
			map[string]any{"seq": checkpointThrough + 3, "type": "turn/start", "data": map[string]any{"turn": 2}},
		}})
	if err != nil {
		return fmt.Errorf("encode resumed Agent session checkpoint: %w", err)
	}
	checkpointSum := sha256.Sum256(checkpointPayload)
	checkpointDigest = checkpointSum[:]
	checkpointCommand := app.AgentCheckpointCommand{Fence: checkpointFence, From: checkpointThrough + 1,
		Through: checkpointThrough + 3, Digest: checkpointDigest, Payload: checkpointPayload}
	replayed, err := a.checkpoints.Save(ctx, checkpointCommand)
	if err != nil {
		return fmt.Errorf("save resumed original-session steer checkpoint: %w", err)
	}
	if replayed {
		return errors.New("save resumed original-session steer checkpoint: unexpectedly replayed checkpoint")
	}
	a.mu.Lock()
	run.checkpointThrough, run.checkpointDigest, run.completionSteerMessageID = checkpointCommand.Through,
		append([]byte(nil), checkpointCommand.Digest...), steer.ID
	a.mu.Unlock()
	resultStepID, err := domain.NewStepID()
	if err != nil {
		return err
	}
	progressStep := task.Step{ID: progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress", Status: task.StepSucceeded,
		Title: browserActivityProgressTitle(run), Summary: "已收到补充要求，并在原会话中继续处理。"}
	resultStep := task.Step{ID: resultStepID, TaskID: current.ID, Sequence: 3, Kind: "result", Status: task.StepSucceeded,
		Title: "处理完成", Summary: "最终结果已按 BLUE 补充要求更新。"}
	resultPayload, err := json.Marshal(map[string]any{"result": browserActivityCompletionRaceResult, "step_id": resultStep.ID})
	if err != nil {
		return fmt.Errorf("encode revised completion result: %w", err)
	}
	event, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: resultStep.ID,
		Result: browserActivityCompletionRaceResult, Usage: current.Usage, Steps: []task.Step{progressStep, resultStep},
		PublishResultMessage: true,
		AgentCompletionFence: &app.TaskAgentCompletionFence{AgentSessionID: checkpointFence.AgentSessionID,
			AgentInstanceID: checkpointFence.AgentInstanceID, AgentGeneration: checkpointFence.AgentGeneration,
			AuthorizationVersion: checkpointFence.AuthorizationVersion, CheckpointThroughSequence: checkpointCommand.Through,
			CheckpointDigest: checkpointCommand.Digest},
		Event: task.Event{TaskID: current.ID, Type: "task.result", Payload: resultPayload}})
	if err != nil {
		return fmt.Errorf("commit revised completion after applying steer: %w", err)
	}
	completed, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read revised completion Task: %w", err)
	}
	a.mu.Lock()
	run.current, run.resultEventSequence, run.completionRejected = completed.Task, event.Sequence, true
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, true)
}

func (a *browserAcceptanceActivity) waitForIndependentCompletionResultWindow(ctx context.Context, original task.Task) error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var independentTaskID string
		if err := a.store.pool.QueryRow(ctx, `SELECT COALESCE((SELECT message.task_id FROM conversation_messages message
JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.owner_id=$1 AND message.conversation_id=$2 AND message.content=$3
 AND message.role='user' AND message.task_id<>$4 AND message.delivery_status='applied' AND route.state='routed'
ORDER BY message.created_at DESC LIMIT 1),'')`, original.OwnerID, a.conversationID,
			browserActivityCompletionRaceIndependent, original.ID).Scan(&independentTaskID); err != nil {
			return fmt.Errorf("find independent completion-race Task: %w", err)
		}
		if independentTaskID != "" {
			var status string
			var resultAt time.Time
			if err := a.store.pool.QueryRow(ctx, `SELECT task.status,COALESCE(event.created_at,'epoch'::timestamptz)
FROM tasks task LEFT JOIN task_events event ON event.task_id=task.id AND event.event_type='task.result'
WHERE task.id=$1 AND task.owner_id=$2`, independentTaskID, original.OwnerID).Scan(&status, &resultAt); err != nil {
				return fmt.Errorf("read independent completion-race result time: %w", err)
			}
			if status == string(task.StatusCompleted) && !resultAt.IsZero() && resultAt.After(time.Unix(0, 0)) {
				readyAt := resultAt.Add(maxConversationResultWindow + 250*time.Millisecond)
				if delay := time.Until(readyAt); delay > 0 {
					timer := time.NewTimer(delay)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-timer.C:
					}
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("independent completion-race Task did not finish within 30 seconds")
		case <-ticker.C:
		}
	}
}

func (a *browserAcceptanceActivity) complete(ctx context.Context, run *browserActivityRun, delta app.CloudDSHTextDelta) error {
	current := run.current
	turnID, result := "", browserActivityResult(run.name, run.objective)
	if run.name == "stream" {
		delta.Text = browserActivityStreamSuffix
		appended, err := a.store.AppendCloudDSHTextDelta(ctx, delta)
		if err != nil {
			return fmt.Errorf("continue visible stream: %w", err)
		}
		if !appended {
			return errors.New("visible stream continuation was rejected")
		}
		turnID, result = delta.TurnID, browserActivityStreamPrefix+browserActivityStreamSuffix
	} else {
		var err error
		turnID, err = domain.NewStepID()
		if err != nil {
			return err
		}
	}
	steps := []task.Step{
		{ID: run.progressStepID, TaskID: current.ID, Sequence: 2, Kind: "progress", Status: task.StepSucceeded,
			Title: browserActivityProgressTitle(run), Summary: browserActivityProgressSummary(run, true)},
		{ID: turnID, TaskID: current.ID, Sequence: 3, Kind: "result", Status: task.StepSucceeded, Title: "处理完成"},
	}
	payload, err := json.Marshal(map[string]any{"result": result})
	if err != nil {
		return err
	}
	event, err := a.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.StatusCompleted, CurrentStep: turnID, Result: result,
		Usage: current.Usage, Steps: steps, PublishResultMessage: true,
		Event: task.Event{Type: "task.result", Payload: payload}})
	if err != nil {
		return err
	}
	a.mu.Lock()
	run.resultEventSequence = event.Sequence
	a.mu.Unlock()
	return a.verifyActivityEvents(ctx, run, true)
}

func (a *browserAcceptanceActivity) verifyActivityEvents(ctx context.Context, run *browserActivityRun, completed bool) error {
	current := run.current
	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read task start projection: %w", err)
	}
	if details.CurrentRevisionStarted == nil || !*details.CurrentRevisionStarted ||
		details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != 0 ||
		details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != 0 {
		return errors.New("task start projection is invalid")
	}
	if completed && details.Task.Status != task.StatusCompleted {
		return errors.New("task was not completed")
	}
	startSteps, progressSteps := 0, 0
	for _, step := range details.Steps {
		if step.Kind == "progress" && step.Title == "开始处理" && step.Summary == "正在处理你的请求。" {
			startSteps++
		}
		if run.progressStepID != "" && step.ID == run.progressStepID && step.Title == browserActivityProgressTitle(run) {
			progressSteps++
		}
	}
	if startSteps != 1 || run.progressStepID != "" && progressSteps != 1 {
		return fmt.Errorf("unexpected task steps: start=%d progress=%d", startSteps, progressSteps)
	}
	events, err := a.store.ListTaskEvents(ctx, current.InitiatorID, current.ID, 0, 100)
	if err != nil {
		return err
	}
	var previous int64
	starts, progress, results, matchingResultEvents, streamEvents := 0, 0, 0, 0, 0
	for _, event := range events {
		if event.Sequence <= previous {
			return errors.New("task event sequence is not strictly increasing")
		}
		previous = event.Sequence
		var payload struct {
			Phase  string `json:"phase"`
			StepID string `json:"step_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return errors.New("task event contains invalid JSON")
		}
		if event.Type == "task.step" && payload.Phase == "started" {
			starts++
		}
		if event.Type == "task.step" && run.progressStepID != "" && payload.StepID == run.progressStepID {
			progress++
		}
		if event.Type == "task.result" {
			results++
			if completed && event.Sequence == run.resultEventSequence {
				matchingResultEvents++
			}
		}
		if event.Type == "task.message" {
			streamEvents++
		}
	}
	wantResults := 1
	if run.name == "mcp-retry" {
		wantResults = 2
	}
	if starts != 1 || run.progressStepID != "" && progress != 1 || completed && (results != wantResults || matchingResultEvents != 1) ||
		completed && run.name == "stream" && streamEvents < 2 {
		return fmt.Errorf("unexpected task events: start=%d progress=%d result=%d matching_final_result=%d stream=%d",
			starts, progress, results, matchingResultEvents, streamEvents)
	}
	return nil
}

type browserActivityRunView struct {
	Name                string `json:"name"`
	Objective           string `json:"objective"`
	Stage               string `json:"stage"`
	Failure             string `json:"failure,omitempty"`
	StopStatus          string `json:"stop_status,omitempty"`
	TaskID              string `json:"task_id,omitempty"`
	UserMessageID       string `json:"user_message_id,omitempty"`
	StreamMessageID     string `json:"stream_message_id,omitempty"`
	StreamSequence      int64  `json:"stream_sequence,omitempty"`
	ResultEventSequence int64  `json:"result_event_sequence,omitempty"`
}

func browserActivityView(run *browserActivityRun) browserActivityRunView {
	return browserActivityRunView{Name: run.name, Objective: run.objective, Stage: run.stage, Failure: run.failure,
		StopStatus: run.stopStatus, TaskID: run.current.ID, UserMessageID: run.userMessageID, StreamMessageID: run.streamMessageID,
		StreamSequence: run.streamSequence, ResultEventSequence: run.resultEventSequence}
}

func (a *browserAcceptanceActivity) serveControl(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__morde_fixture/"), "/")
	if len(parts) == 2 && parts[0] == "bootstrap" && r.Method == http.MethodGet {
		if a.runs[parts[1]] == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username": a.testUserName, "password": "client-password-123", "activity_token": a.controlToken,
		})
		return
	}
	if r.Header.Get("X-Morde-Fixture-Token") != a.controlToken {
		http.Error(w, "fixture control is unauthorized", http.StatusUnauthorized)
		return
	}
	if len(parts) == 1 && parts[0] == "status" && r.Method == http.MethodGet {
		a.mu.Lock()
		runs := make([]browserActivityRunView, 0, len(a.runs))
		for _, run := range a.runs {
			runs = append(runs, browserActivityView(run))
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
		active := make(map[string]map[string]int)
		for _, watch := range a.watches {
			if active[watch.taskID] == nil {
				active[watch.taskID] = make(map[string]int)
			}
			active[watch.taskID][watch.client]++
		}
		requests := append([]browserActivityEventRequest(nil), a.requests...)
		taskListRequests := append([]browserActivityTaskListRequest(nil), a.taskListRequests...)
		taskSnapshots := append([]browserActivityTaskSnapshotRequest(nil), a.taskSnapshots...)
		messageSnapshots := append([]browserActivityMessageSnapshotRequest(nil), a.messageSnapshots...)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs, "active_event_streams": active,
			"event_requests": requests, "conversation_task_list_requests": taskListRequests,
			"task_snapshot_requests": taskSnapshots, "message_snapshot_requests": messageSnapshots})
		return
	}
	if len(parts) == 3 && parts[0] == "advance" && r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := a.advance(ctx, parts[1], parts[2]); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[0] == "disconnect" && r.Method == http.MethodPost {
		if err := a.disconnect(parts[1]); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[0] == "verify" && r.Method == http.MethodGet {
		if err := a.verifyResult(r.Context(), parts[1]); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		a.mu.Lock()
		a.verified[parts[1]] = true
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{"scenario": parts[1], "status": "verified"}
		switch parts[1] {
		case "failed":
			result["agent_acknowledged"], result["task_status"] = true, string(task.StatusFailed)
		case "approval":
			result["agent_acknowledged"], result["task_status"] = true, string(task.StatusWaitingUser)
			result["waiting_reason"], result["pending_request_status"] = "approval_required", string(task.PendingRequestPending)
		case "approval-resume":
			a.mu.Lock()
			run := a.runs["approval-resume"]
			if run != nil {
				result["task_id"], result["task_status"] = run.current.ID, string(run.current.Status)
				result["approval_id"], result["approval_version"] = run.approvalID, run.approvalVersion
				result["synthetic_resume_count"] = run.approvalResumeCount
			}
			a.mu.Unlock()
		case "approval-revoked":
			a.mu.Lock()
			run := a.runs["approval-revoked"]
			if run != nil {
				result["task_id"], result["task_status"] = run.current.ID, run.stopStatus
				result["approval_id"], result["approval_version"] = run.approvalID, run.approvalVersion
			}
			a.mu.Unlock()
		case "approval-host-member-revoked":
			a.mu.Lock()
			run := a.runs["approval-host-member-revoked"]
			if run != nil {
				result["task_id"], result["task_status"] = run.current.ID, run.stopStatus
				result["approval_id"], result["approval_version"] = run.approvalID, run.approvalVersion
				result["host_id"] = run.revocationHostID
			}
			a.mu.Unlock()
		case "clarification":
			a.mu.Lock()
			run := a.runs["clarification"]
			completed, answer := run != nil && run.stage == "completed", ""
			if run != nil {
				answer = run.clarificationAnswer
			}
			a.mu.Unlock()
			result["agent_acknowledged"] = true
			if completed {
				result["task_status"], result["clarification_status"], result["answer"] = string(task.StatusCompleted), string(task.ClarificationAnswered), answer
			} else {
				result["task_status"], result["waiting_reason"], result["clarification_status"] = string(task.StatusWaitingUser), "clarification_required", string(task.ClarificationPending)
			}
		case "recovery-unknown":
			result["agent_acknowledged"], result["task_status"] = true, string(task.StatusWaitingUser)
			result["waiting_reason"], result["current_step_status"] = "dsh_recovery_unresolved", string(task.StepUnknown)
		case "interrupted":
			a.mu.Lock()
			run := a.runs["interrupted"]
			if run != nil {
				result["task_id"], result["task_status"] = run.current.ID, run.stopStatus
				result["assistant_message_id"], result["assistant_message_status"] = run.streamMessageID, string(conversation.OutputInterrupted)
				result["assistant_message_content"] = browserActivityStreamPrefix
			}
			a.mu.Unlock()
		case "queued":
			result["task_status"] = string(task.StatusCompleted)
		case "mcp-retry":
			result["agent_acknowledged"], result["task_status"] = true, string(task.StatusCompleted)
			result["retry_step_status"], result["mcp_operation_count"] = string(task.StepSucceeded), 2
		case "target-control":
			result["target_task_id"] = a.runs["target-control-b"].current.ID
			result["sibling_task_id"] = a.runs["target-control-a"].current.ID
			result["target_status"], result["sibling_status"] = string(task.StatusCancelling), string(task.StatusRunning)
		}
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	http.NotFound(w, r)
}

func (a *browserAcceptanceActivity) assertVerified() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, scenario := range a.required {
		if !a.verified[scenario] {
			return fmt.Errorf("synthetic activity scenario %s was not verified", scenario)
		}
	}
	return nil
}

func (a *browserAcceptanceActivity) advance(ctx context.Context, name, phase string) error {
	if phase == "revoke" {
		if name != "approval-revoked" && name != "approval-host-member-revoked" {
			return errors.New("authorization revocation is only available for approval-revoked scenarios")
		}
		a.mu.Lock()
		run := a.runs[name]
		if run == nil || run.stage != "waiting_user" || run.busy || run.current.ID == "" {
			stage := "missing"
			if run != nil {
				stage = run.stage
			}
			a.mu.Unlock()
			return fmt.Errorf("authorization revocation is unavailable at stage %s", stage)
		}
		run.busy = true
		current := run.current
		executionControl, ownerPrincipal := a.executionControl, a.ownerPrincipal
		hostID, hostOwnerID := run.revocationHostID, run.revocationHostOwnerID
		a.mu.Unlock()
		var err error
		if name == "approval-host-member-revoked" {
			if hostID == "" || hostOwnerID == "" || ownerPrincipal.UserID != current.InitiatorID {
				err = errors.New("shared Host grant revocation fixture is missing its Host or Task initiator")
			} else {
				err = a.store.RevokeHostMember(ctx, hostID, hostOwnerID, current.InitiatorID)
			}
		} else if executionControl == nil || ownerPrincipal.UserID != current.InitiatorID {
			err = errors.New("authorization revocation fixture is missing its account execution control")
		} else {
			var control execution.Control
			control, err = executionControl.Status(ctx, ownerPrincipal)
			if err == nil {
				_, err = executionControl.StopAll(ctx, ownerPrincipal, control.Epoch, "browser-approval-revocation-stop-all")
			}
		}
		a.mu.Lock()
		run.busy = false
		if err != nil {
			run.stage, run.failure = "failed", err.Error()
		} else {
			run.stage, run.stopStatus = "revoked", string(task.StatusCancelling)
			run.current.Status = task.StatusCancelling
			run.current.WaitingReason = ""
		}
		a.mu.Unlock()
		if err != nil {
			return fmt.Errorf("apply authorization cancellation fence: %w", err)
		}
		return nil
	}
	a.mu.Lock()
	run := a.runs[name]
	if run == nil {
		a.mu.Unlock()
		return errors.New("unknown synthetic scenario")
	}
	expected := map[string]string{"start": "claimed", "progress": "started", "stream": "progress", "complete": "progress",
		"candidate": "progress", "commit": "completion_candidate",
		"fail": "progress", "approval": "progress", "clarification": "progress", "unknown": "progress", "retry-success": "retry_claimed"}[phase]
	if name == "stream" && phase == "complete" {
		expected = "streaming"
	}
	if expected == "" || phase == "stream" && name != "stream" && name != "interrupted" ||
		(phase == "candidate" || phase == "commit") && name != "completion-race" ||
		phase == "complete" && name == "completion-race" ||
		phase == "fail" && name != "failed" && name != "mcp-retry" || phase == "approval" && name != "approval" && name != "approval-resume" && name != "approval-revoked" && name != "approval-host-member-revoked" ||
		phase == "clarification" && name != "clarification" || phase == "unknown" && name != "recovery-unknown" ||
		phase == "retry-success" && name != "mcp-retry" ||
		(name == "failed" || name == "approval" || name == "clarification" || name == "recovery-unknown") && phase == "complete" || run.stage != expected || run.busy {
		stage := run.stage
		a.mu.Unlock()
		return fmt.Errorf("phase %s is unavailable at stage %s", phase, stage)
	}
	run.busy = true
	a.mu.Unlock()
	clearBusy := func() {
		a.mu.Lock()
		run.busy = false
		a.mu.Unlock()
	}
	command := browserActivityCommand{phase: phase, reply: make(chan error, 1)}
	select {
	case <-ctx.Done():
		clearBusy()
		return ctx.Err()
	case run.commands <- command:
	}
	select {
	case err := <-command.reply:
		clearBusy()
		return err
	case <-ctx.Done():
		go func() {
			if err := <-command.reply; err != nil {
				clearBusy()
				return
			}
			clearBusy()
		}()
		return ctx.Err()
	}
}

func (a *browserAcceptanceActivity) serveTaskEvents(w http.ResponseWriter, r *http.Request, next http.Handler) {
	taskID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/events")
	if !domain.ValidTaskID(taskID) {
		next.ServeHTTP(w, r)
		return
	}
	client := "browser"
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		client = "ios"
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lastID, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	ctx, cancel := context.WithCancel(r.Context())
	a.mu.Lock()
	a.nextWatch++
	watchID := a.nextWatch
	a.watches[watchID] = browserActivityWatch{taskID: taskID, client: client, cancel: cancel}
	a.requests = append(a.requests, browserActivityEventRequest{TaskID: taskID, Client: client, After: after, LastID: lastID})
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		delete(a.watches, watchID)
		a.mu.Unlock()
	}()
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (a *browserAcceptanceActivity) serveConversationTasks(w http.ResponseWriter, r *http.Request, next http.Handler) {
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.Code == http.StatusOK {
		var page struct {
			Items []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &page) == nil {
			client := "browser"
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				client = "ios"
			}
			request := browserActivityTaskListRequest{Client: client, StatusCode: recorder.Code}
			for _, item := range page.Items {
				request.TaskIDs = append(request.TaskIDs, item.ID)
				request.Statuses = append(request.Statuses, item.Status)
			}
			a.mu.Lock()
			a.taskListRequests = append(a.taskListRequests, request)
			a.mu.Unlock()
		}
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func browserActivityCrossClientScenario() string {
	switch strings.TrimSpace(os.Getenv("MORDE_BROWSER_ACTIVITY_CROSS_CLIENT")) {
	case "1", "recovery-unknown":
		return "recovery-unknown"
	case "approval":
		return "approval"
	case "approval-resume":
		return "approval-resume"
	case "approval-revoked":
		return "approval-revoked"
	case "approval-host-member-revoked":
		return "approval-host-member-revoked"
	case "interrupted":
		return "interrupted"
	default:
		return ""
	}
}

func (a *browserAcceptanceActivity) serveTaskSnapshot(w http.ResponseWriter, r *http.Request, next http.Handler) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if !domain.ValidTaskID(id) {
		next.ServeHTTP(w, r)
		return
	}
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	body := recorder.Body.Bytes()
	validationPreviewApplied := false
	if recorder.Code == http.StatusOK {
		preview, injected, err := a.withValidationEvidencePreview(r.Context(), id, body)
		if err != nil {
			http.Error(w, "local validation evidence preview failed", http.StatusInternalServerError)
			return
		}
		if injected {
			body = preview
			validationPreviewApplied = true
		}
	}
	crossClientScenario := browserActivityCrossClientScenario()
	crossClientTaskSnapshot := false
	if crossClientScenario != "" {
		a.mu.Lock()
		run := a.runs[crossClientScenario]
		crossClientTaskSnapshot = run != nil && run.current.ID == id
		a.mu.Unlock()
	}
	client := "browser"
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		client = "ios"
	} else if crossClientTaskSnapshot {
		cookie, err := r.Cookie("ch_portal")
		if err != nil || cookie.Value == "" {
			client = "other"
		}
	}
	snapshotStatus, waitingReason := "", ""
	if crossClientTaskSnapshot && recorder.Code == http.StatusOK {
		var snapshot struct {
			Status        string  `json:"status"`
			WaitingReason *string `json:"waiting_reason"`
		}
		if json.Unmarshal(body, &snapshot) == nil {
			snapshotStatus = snapshot.Status
			if snapshot.WaitingReason != nil {
				waitingReason = *snapshot.WaitingReason
			}
		}
	}
	a.mu.Lock()
	a.taskSnapshots = append(a.taskSnapshots, browserActivityTaskSnapshotRequest{
		Client: client, TaskID: id, StatusCode: recorder.Code,
		Status: snapshotStatus, WaitingReason: waitingReason,
	})
	a.mu.Unlock()
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	if validationPreviewApplied {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(body)
}

func (a *browserAcceptanceActivity) serveTaskAction(w http.ResponseWriter, r *http.Request, next http.Handler) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/actions")
	if !domain.ValidTaskID(id) {
		next.ServeHTTP(w, r)
		return
	}
	body, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		http.Error(w, "unable to read local task action", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var request struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &request)
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	if request.Action == string(task.ActionCancel) && recorder.Code == http.StatusAccepted {
		if err := a.confirmInterruptedCancellation(id, recorder.Body.Bytes()); err != nil {
			a.mu.Lock()
			if run := a.runs["interrupted"]; run != nil && run.current.ID == id {
				run.failure = err.Error()
			}
			a.mu.Unlock()
		}
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func (a *browserAcceptanceActivity) confirmInterruptedCancellation(taskID string, responseBody []byte) error {
	a.mu.Lock()
	run := a.runs["interrupted"]
	if run == nil || run.current.ID != taskID || run.stage != "streaming" || run.streamMessageID == "" {
		a.mu.Unlock()
		return errors.New("cancel did not target the active interrupted-stream fixture")
	}
	current, messageID := run.current, run.streamMessageID
	a.mu.Unlock()

	var response struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil || response.TaskID != taskID ||
		(response.Status != string(task.StatusCancelling) && response.Status != string(task.StatusCancelled)) {
		return fmt.Errorf("cancel action returned an unexpected Task state: task=%s status=%s", response.TaskID, response.Status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var persistedStatus string
	if err := a.store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1 AND initiator_id=$2`,
		taskID, current.InitiatorID).Scan(&persistedStatus); err != nil {
		return fmt.Errorf("read interrupted Task state after cancel: %w", err)
	}
	if (persistedStatus != string(task.StatusCancelling) && persistedStatus != string(task.StatusCancelled)) ||
		(response.Status != persistedStatus && persistedStatus != string(task.StatusCancelled)) {
		return fmt.Errorf("cancelled stream Task state is inconsistent: response=%s persisted=%s", response.Status, persistedStatus)
	}

	var storedTaskID, role, content, outputStatus string
	var revision int64
	if err := a.store.pool.QueryRow(ctx, `SELECT task_id,role,content,output_status,output_task_revision
FROM conversation_messages WHERE id=$1`, messageID).Scan(&storedTaskID, &role, &content, &outputStatus, &revision); err != nil {
		return fmt.Errorf("read interrupted assistant message: %w", err)
	}
	var snapshotMessageID, snapshotContent, snapshotStatus string
	var snapshotRevision int64
	if err := a.store.pool.QueryRow(ctx, `SELECT message_id,content,status,task_revision
FROM task_assistant_message_snapshots WHERE task_id=$1`, taskID).
		Scan(&snapshotMessageID, &snapshotContent, &snapshotStatus, &snapshotRevision); err != nil {
		return fmt.Errorf("read interrupted assistant message snapshot: %w", err)
	}
	var assistantMessages int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND role='assistant'`, taskID).
		Scan(&assistantMessages); err != nil {
		return fmt.Errorf("count interrupted assistant messages: %w", err)
	}
	if storedTaskID != taskID || role != string(conversation.RoleAssistant) || content != browserActivityStreamPrefix ||
		outputStatus != string(conversation.OutputInterrupted) || snapshotMessageID != messageID ||
		snapshotContent != content || snapshotStatus != outputStatus || snapshotRevision != revision || assistantMessages != 1 {
		return fmt.Errorf("interrupted stream persistence mismatch: task=%s role=%s status=%s snapshot=%s/%s/%s messages=%d",
			storedTaskID, role, outputStatus, snapshotMessageID, snapshotStatus, snapshotContent, assistantMessages)
	}
	a.mu.Lock()
	if run := a.runs["interrupted"]; run != nil && run.current.ID == taskID {
		run.stage, run.stopStatus = "interrupted", persistedStatus
	}
	a.mu.Unlock()
	return nil
}

func (a *browserAcceptanceActivity) serveMessageSnapshot(w http.ResponseWriter, r *http.Request, next http.Handler) {
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.Code == http.StatusOK {
		client := "browser"
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			client = "ios"
		}
		var page struct {
			Items []struct {
				ID           string `json:"id"`
				TaskID       string `json:"task_id"`
				Role         string `json:"role"`
				Content      string `json:"content"`
				OutputStatus string `json:"output_status"`
			} `json:"items"`
		}
		if json.Unmarshal(recorder.Body.Bytes(), &page) == nil {
			a.mu.Lock()
			run := a.runs["interrupted"]
			taskID := ""
			if run != nil && run.stage == "interrupted" {
				taskID = run.current.ID
			}
			a.mu.Unlock()
			if taskID != "" {
				for _, item := range page.Items {
					if item.TaskID != taskID {
						continue
					}
					a.mu.Lock()
					a.messageSnapshots = append(a.messageSnapshots, browserActivityMessageSnapshotRequest{
						Client: client, TaskID: item.TaskID, MessageID: item.ID, StatusCode: recorder.Code,
						Role: item.Role, Content: item.Content, OutputStatus: item.OutputStatus,
					})
					a.mu.Unlock()
				}
			}
		}
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}

func verifyCrossClientTaskSnapshots(snapshots []browserActivityTaskSnapshotRequest, taskID, status, waitingReason string) error {
	var latestByClient = make(map[string]browserActivityTaskSnapshotRequest, 2)
	for _, snapshot := range snapshots {
		if snapshot.StatusCode != http.StatusOK || (snapshot.Client != "browser" && snapshot.Client != "ios") {
			continue
		}
		latestByClient[snapshot.Client] = snapshot
	}
	pc, hasPC := latestByClient["browser"]
	ios, hasIOS := latestByClient["ios"]
	if !hasPC {
		return errors.New("cross-client verification is missing a successful PC Task snapshot")
	}
	if !hasIOS {
		return errors.New("cross-client verification is missing a successful iOS Task snapshot")
	}
	if pc.TaskID != taskID || ios.TaskID != taskID || pc.TaskID != ios.TaskID {
		return fmt.Errorf("clients read different Tasks: PC=%s iOS=%s expected=%s", pc.TaskID, ios.TaskID, taskID)
	}
	if pc.Status != status || pc.WaitingReason != waitingReason ||
		ios.Status != status || ios.WaitingReason != waitingReason ||
		pc.Status != ios.Status || pc.WaitingReason != ios.WaitingReason {
		return fmt.Errorf("client snapshots disagree: PC=%s/%s iOS=%s/%s expected=%s/%s",
			pc.Status, pc.WaitingReason, ios.Status, ios.WaitingReason, status, waitingReason)
	}
	return nil
}

func verifyRecoveryUnknownClientSnapshots(snapshots []browserActivityTaskSnapshotRequest, taskID string) error {
	return verifyCrossClientTaskSnapshots(snapshots, taskID, string(task.StatusWaitingUser), "dsh_recovery_unresolved")
}

func verifyApprovalClientSnapshots(snapshots []browserActivityTaskSnapshotRequest, taskID string) error {
	return verifyCrossClientTaskSnapshots(snapshots, taskID, string(task.StatusWaitingUser), "approval_required")
}

func verifyApprovalResumeClientSnapshots(snapshots []browserActivityTaskSnapshotRequest, taskID string) error {
	latestByClient := make(map[string]browserActivityTaskSnapshotRequest, 2)
	for _, snapshot := range snapshots {
		if snapshot.Client == "browser" || snapshot.Client == "ios" {
			latestByClient[snapshot.Client] = snapshot
		}
	}
	pc, hasPC := latestByClient["browser"]
	ios, hasIOS := latestByClient["ios"]
	if !hasPC {
		return errors.New("approval-resume verification is missing a PC Task snapshot")
	}
	if !hasIOS {
		return errors.New("approval-resume verification is missing an iOS Task snapshot")
	}
	for _, snapshot := range []browserActivityTaskSnapshotRequest{pc, ios} {
		if snapshot.StatusCode != http.StatusOK || snapshot.TaskID != taskID ||
			snapshot.Status != string(task.StatusCompleted) || snapshot.WaitingReason != "" {
			return fmt.Errorf("%s did not read the completed approved Task: status_code=%d task=%s state=%s/%s",
				snapshot.Client, snapshot.StatusCode, snapshot.TaskID, snapshot.Status, snapshot.WaitingReason)
		}
	}
	return nil
}

func verifyInterruptedMessageSnapshots(snapshots []browserActivityMessageSnapshotRequest, taskID, messageID string) error {
	latestByClient := make(map[string]browserActivityMessageSnapshotRequest, 2)
	for _, snapshot := range snapshots {
		if snapshot.Client == "browser" || snapshot.Client == "ios" {
			latestByClient[snapshot.Client] = snapshot
		}
	}
	pc, hasPC := latestByClient["browser"]
	ios, hasIOS := latestByClient["ios"]
	if !hasPC {
		return errors.New("interrupted cross-client verification is missing a successful PC message read")
	}
	if !hasIOS {
		return errors.New("interrupted cross-client verification is missing a successful iOS message read")
	}
	for _, snapshot := range []browserActivityMessageSnapshotRequest{pc, ios} {
		if snapshot.StatusCode != http.StatusOK || snapshot.TaskID != taskID || snapshot.MessageID != messageID ||
			snapshot.Role != string(conversation.RoleAssistant) || snapshot.Content != browserActivityStreamPrefix ||
			snapshot.OutputStatus != string(conversation.OutputInterrupted) {
			return fmt.Errorf("%s did not read the expected interrupted assistant message: status=%d task=%s message=%s role=%s output_status=%s content=%q",
				snapshot.Client, snapshot.StatusCode, snapshot.TaskID, snapshot.MessageID, snapshot.Role, snapshot.OutputStatus, snapshot.Content)
		}
	}
	if pc.TaskID != ios.TaskID || pc.MessageID != ios.MessageID || pc.Content != ios.Content || pc.OutputStatus != ios.OutputStatus {
		return fmt.Errorf("PC and iOS read different interrupted assistant messages: PC=%s/%s/%q/%s iOS=%s/%s/%q/%s",
			pc.TaskID, pc.MessageID, pc.Content, pc.OutputStatus, ios.TaskID, ios.MessageID, ios.Content, ios.OutputStatus)
	}
	return nil
}

func TestVerifyInterruptedMessageSnapshots(t *testing.T) {
	pc := browserActivityMessageSnapshotRequest{Client: "browser", TaskID: "t1234567", MessageID: "m1234567",
		StatusCode: http.StatusOK, Role: string(conversation.RoleAssistant), Content: browserActivityStreamPrefix,
		OutputStatus: string(conversation.OutputInterrupted)}
	ios := pc
	ios.Client = "ios"
	for _, test := range []struct {
		name      string
		snapshots []browserActivityMessageSnapshotRequest
		wantErr   bool
	}{
		{name: "matching client reads", snapshots: []browserActivityMessageSnapshotRequest{pc, ios}},
		{name: "missing PC read", snapshots: []browserActivityMessageSnapshotRequest{ios}, wantErr: true},
		{name: "missing iOS read", snapshots: []browserActivityMessageSnapshotRequest{pc}, wantErr: true},
		{name: "different message identity", snapshots: []browserActivityMessageSnapshotRequest{pc,
			{Client: "ios", TaskID: ios.TaskID, MessageID: "m7654321", StatusCode: http.StatusOK,
				Role: ios.Role, Content: ios.Content, OutputStatus: ios.OutputStatus}}, wantErr: true},
		{name: "different persisted content", snapshots: []browserActivityMessageSnapshotRequest{pc,
			{Client: "ios", TaskID: ios.TaskID, MessageID: ios.MessageID, StatusCode: http.StatusOK,
				Role: ios.Role, Content: browserActivityStreamPrefix + browserActivityStreamSuffix, OutputStatus: ios.OutputStatus}}, wantErr: true},
		{name: "wrong output status", snapshots: []browserActivityMessageSnapshotRequest{pc,
			{Client: "ios", TaskID: ios.TaskID, MessageID: ios.MessageID, StatusCode: http.StatusOK,
				Role: ios.Role, Content: ios.Content, OutputStatus: string(conversation.OutputCompleted)}}, wantErr: true},
		{name: "non-200 read", snapshots: []browserActivityMessageSnapshotRequest{
			{Client: "browser", TaskID: pc.TaskID, MessageID: pc.MessageID, StatusCode: http.StatusForbidden}, ios}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyInterruptedMessageSnapshots(test.snapshots, "t1234567", "m1234567")
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyInterruptedMessageSnapshots() error=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestVerifyRecoveryUnknownClientSnapshots(t *testing.T) {
	taskID := "t1234567"
	pc := browserActivityTaskSnapshotRequest{Client: "browser", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusWaitingUser), WaitingReason: "dsh_recovery_unresolved"}
	ios := browserActivityTaskSnapshotRequest{Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusWaitingUser), WaitingReason: "dsh_recovery_unresolved"}
	tests := []struct {
		name      string
		snapshots []browserActivityTaskSnapshotRequest
		wantErr   bool
	}{
		{name: "matching snapshots", snapshots: []browserActivityTaskSnapshotRequest{pc, ios}},
		{name: "missing PC snapshot", snapshots: []browserActivityTaskSnapshotRequest{ios}, wantErr: true},
		{name: "missing iOS snapshot", snapshots: []browserActivityTaskSnapshotRequest{pc}, wantErr: true},
		{name: "different task IDs", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: "t7654321", StatusCode: http.StatusOK,
				Status: string(task.StatusWaitingUser), WaitingReason: "dsh_recovery_unresolved"},
		}, wantErr: true},
		{name: "wrong status", snapshots: []browserActivityTaskSnapshotRequest{
			{Client: "browser", TaskID: taskID, StatusCode: http.StatusOK,
				Status: string(task.StatusFailed), WaitingReason: "dsh_recovery_unresolved"}, ios,
		}, wantErr: true},
		{name: "wrong waiting reason", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
				Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"},
		}, wantErr: true},
		{name: "non-200 snapshots do not satisfy verification", snapshots: []browserActivityTaskSnapshotRequest{
			{Client: "browser", TaskID: taskID, StatusCode: http.StatusForbidden},
			{Client: "ios", TaskID: taskID, StatusCode: http.StatusInternalServerError},
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyRecoveryUnknownClientSnapshots(test.snapshots, taskID)
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyRecoveryUnknownClientSnapshots() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestVerifyApprovalClientSnapshots(t *testing.T) {
	taskID := "t1234567"
	pc := browserActivityTaskSnapshotRequest{Client: "browser", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"}
	ios := browserActivityTaskSnapshotRequest{Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"}
	tests := []struct {
		name      string
		snapshots []browserActivityTaskSnapshotRequest
		wantErr   bool
	}{
		{name: "matching snapshots", snapshots: []browserActivityTaskSnapshotRequest{pc, ios}},
		{name: "missing PC snapshot", snapshots: []browserActivityTaskSnapshotRequest{ios}, wantErr: true},
		{name: "missing iOS snapshot", snapshots: []browserActivityTaskSnapshotRequest{pc}, wantErr: true},
		{name: "different Task IDs", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: "t7654321", StatusCode: http.StatusOK,
				Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"},
		}, wantErr: true},
		{name: "failed Task", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
				Status: string(task.StatusFailed), WaitingReason: "approval_required"},
		}, wantErr: true},
		{name: "wrong waiting reason", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
				Status: string(task.StatusWaitingUser), WaitingReason: "dsh_recovery_unresolved"},
		}, wantErr: true},
		{name: "non-200 client snapshot", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: taskID, StatusCode: http.StatusForbidden,
				Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"},
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyApprovalClientSnapshots(test.snapshots, taskID)
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyApprovalClientSnapshots() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestVerifyApprovalResumeClientSnapshots(t *testing.T) {
	taskID := "t1234567"
	pc := browserActivityTaskSnapshotRequest{Client: "browser", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusCompleted)}
	ios := browserActivityTaskSnapshotRequest{Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
		Status: string(task.StatusCompleted)}
	for _, test := range []struct {
		name      string
		snapshots []browserActivityTaskSnapshotRequest
		wantErr   bool
	}{
		{name: "both clients read completed Task", snapshots: []browserActivityTaskSnapshotRequest{pc, ios}},
		{name: "missing PC", snapshots: []browserActivityTaskSnapshotRequest{ios}, wantErr: true},
		{name: "missing iOS", snapshots: []browserActivityTaskSnapshotRequest{pc}, wantErr: true},
		{name: "different Task", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: "t7654321", StatusCode: http.StatusOK, Status: string(task.StatusCompleted)},
		}, wantErr: true},
		{name: "still waiting", snapshots: []browserActivityTaskSnapshotRequest{
			pc, {Client: "ios", TaskID: taskID, StatusCode: http.StatusOK,
				Status: string(task.StatusWaitingUser), WaitingReason: "approval_required"},
		}, wantErr: true},
		{name: "latest read failed", snapshots: []browserActivityTaskSnapshotRequest{
			pc, ios, {Client: "ios", TaskID: taskID, StatusCode: http.StatusUnauthorized},
		}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyApprovalResumeClientSnapshots(test.snapshots, taskID)
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyApprovalResumeClientSnapshots() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func (a *browserAcceptanceActivity) disconnect(name string) error {
	a.mu.Lock()
	run := a.runs[name]
	if run == nil || run.current.ID == "" || run.stage == "completed" || run.stage == "failed" {
		a.mu.Unlock()
		return errors.New("active synthetic task is required")
	}
	cancels := make([]context.CancelFunc, 0)
	active := map[string]bool{}
	for _, watch := range a.watches {
		if watch.taskID == run.current.ID {
			active[watch.client] = true
			cancels = append(cancels, watch.cancel)
		}
	}
	if !active["browser"] || !active["ios"] {
		a.mu.Unlock()
		return errors.New("wait until both browser and iOS task event streams are connected")
	}
	run.dropIndex = len(a.requests)
	run.dropCursors = make(map[string]int64)
	for _, request := range a.requests {
		if request.TaskID == run.current.ID {
			cursor := request.After
			if request.LastID > cursor {
				cursor = request.LastID
			}
			if cursor > run.dropCursors[request.Client] {
				run.dropCursors[request.Client] = cursor
			}
		}
	}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyRouteRetryAccepted(ctx context.Context) error {
	var messageID, routeState, failureReason, taskID string
	var routeAttempt int64
	err := a.store.pool.QueryRow(ctx, `SELECT message.id,route.state,route.failure_reason,route.route_attempt,COALESCE(message.task_id,'')
FROM conversation_message_routes route JOIN conversation_messages message ON message.id=route.message_id
WHERE route.conversation_id=$1 AND message.role='user' AND message.content=$2`,
		a.conversationID, browserActivityObjectives["route-retry"]).
		Scan(&messageID, &routeState, &failureReason, &routeAttempt, &taskID)
	if err != nil {
		return fmt.Errorf("load original route retry message: %w", err)
	}
	var matchingUserMessages, replies, modelCalls, rejectedFirstCalls, settledRetryCalls int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE conversation_id=$1 AND role='user' AND content=$2`, a.conversationID, browserActivityObjectives["route-retry"]).Scan(&matchingUserMessages); err != nil {
		return fmt.Errorf("count original route retry messages: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE conversation_id=$1 AND role='assistant' AND content=$2`, a.conversationID, "本地验收：消息路由恢复成功。").Scan(&replies); err != nil {
		return fmt.Errorf("count route retry replies: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*),
count(*) FILTER (WHERE attempt=1 AND state='rejected'),
count(*) FILTER (WHERE attempt=2 AND state='settled')
FROM conversation_route_model_calls WHERE message_id=$1 AND phase='normal'`, messageID).
		Scan(&modelCalls, &rejectedFirstCalls, &settledRetryCalls); err != nil {
		return fmt.Errorf("read route retry model-call evidence: %w", err)
	}
	if routeState != "routed" || routeAttempt != 2 || failureReason != "" || taskID != "" ||
		matchingUserMessages != 1 || replies != 1 || modelCalls != 2 || rejectedFirstCalls != 1 || settledRetryCalls != 1 {
		return fmt.Errorf("message route retry state=%q attempt=%d failure=%q task=%q users=%d replies=%d calls=%d first_rejected=%d retry_settled=%d",
			routeState, routeAttempt, failureReason, taskID, matchingUserMessages, replies, modelCalls, rejectedFirstCalls, settledRetryCalls)
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyResult(ctx context.Context, scenario string) error {
	if scenario == "reconnect" {
		return a.verifyReconnect("stream")
	}
	if scenario == "target-control" {
		return a.verifyTargetControl(ctx)
	}
	if scenario == "clarification" {
		return a.verifyClarificationOutcome(ctx)
	}
	if scenario == "approval-resume" {
		return a.verifyApprovalResumeOutcome(ctx)
	}
	if scenario == "approval-revoked" || scenario == "approval-host-member-revoked" {
		return a.verifyApprovalRevokedOutcome(ctx, scenario)
	}
	if scenario == "completion-race" {
		if err := a.verifyCompletionRace(ctx); err != nil {
			return err
		}
		return a.verifyCompletionRaceOutcome(ctx)
	}
	if scenario == "failed" || scenario == "approval" || scenario == "recovery-unknown" {
		return a.verifyAcceptedOutcome(ctx, scenario)
	}
	if scenario == "mcp-retry" {
		return a.verifyMCPRetrySucceeded(ctx)
	}
	if scenario == "route-retry" {
		return a.verifyRouteRetryAccepted(ctx)
	}
	if scenario == "interrupted" {
		return a.verifyInterrupted(ctx)
	}
	var names []string
	switch scenario {
	case "merge":
		names = []string{"merge-a", "merge-b"}
	case "separate":
		names = []string{"separate-a", "separate-b"}
	case "search":
		names = []string{"search"}
	case "stream":
		names = []string{"stream", "stream-other"}
	case "queued":
		names = []string{"queued"}
	default:
		return errors.New("unknown synthetic verification")
	}
	a.mu.Lock()
	runs := make([]browserActivityRunView, 0, len(names))
	ownerID := ""
	for _, name := range names {
		run := a.runs[name]
		if run == nil || run.stage != "completed" {
			a.mu.Unlock()
			return fmt.Errorf("scenario %s has not completed", name)
		}
		if ownerID == "" {
			ownerID = run.current.InitiatorID
		} else if ownerID != run.current.InitiatorID {
			a.mu.Unlock()
			return errors.New("fixture tasks have different owners")
		}
		runs = append(runs, browserActivityView(run))
	}
	a.mu.Unlock()
	page, err := a.store.ListMessages(ctx, ownerID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return err
	}
	if scenario == "stream" {
		run := runs[0]
		other := runs[1]
		count, otherCount := 0, 0
		for _, message := range page.Items {
			if message.Role != conversation.RoleAssistant {
				continue
			}
			if message.TaskID == run.TaskID {
				count++
				if message.ID != run.StreamMessageID || message.Sequence != run.StreamSequence ||
					message.OutputStatus != conversation.OutputCompleted || message.OutputTaskRevision == nil ||
					*message.OutputTaskRevision != 0 || message.OutputResultEventSequence == nil ||
					*message.OutputResultEventSequence != run.ResultEventSequence ||
					message.Content != browserActivityStreamPrefix+browserActivityStreamSuffix {
					return errors.New("stream completion changed its message identity, revision, sequence or result")
				}
			}
			for _, source := range message.Sources {
				if source.TaskID == other.TaskID {
					otherCount++
					if message.ID == run.StreamMessageID || len(message.Sources) != 1 || source.UserMessageID != other.UserMessageID ||
						source.TaskRevision != 0 || source.EventSequence != other.ResultEventSequence ||
						source.Content != browserActivityResult(other.Name, other.Objective) {
						return errors.New("the later independent result was combined with the visible stream")
					}
				}
			}
		}
		if count != 1 || otherCount != 1 {
			return fmt.Errorf("expected one stream and one independent result, got %d and %d", count, otherCount)
		}
		return nil
	}
	matched := make(map[string]conversation.Message)
	counts := make(map[string]int)
	for _, message := range page.Items {
		for _, source := range message.Sources {
			for _, run := range runs {
				if source.TaskID != run.TaskID {
					continue
				}
				counts[run.TaskID]++
				if source.UserMessageID != run.UserMessageID || source.TaskRevision != 0 ||
					source.EventSequence != run.ResultEventSequence || source.Content != browserActivityResult(run.Name, run.Objective) {
					return errors.New("published reply source does not match the original task result")
				}
				matched[run.TaskID] = message
			}
		}
	}
	if scenario == "search" || scenario == "queued" {
		run := runs[0]
		message := matched[run.TaskID]
		wantContent := browserActivityResult(run.Name, run.Objective)
		if scenario == "search" {
			wantContent = browserActivitySearchResult
		}
		if counts[run.TaskID] != 1 || message.ID == "" || len(message.Sources) != 1 || !strings.Contains(message.Content, wantContent) {
			if scenario == "search" {
				return errors.New("fixed local search result is missing, duplicated or detached from its task")
			}
			return errors.New("queued Task result is missing, duplicated or detached from its task")
		}
		if scenario == "search" && os.Getenv("MORDE_BROWSER_ACTIVITY_VALIDATION_PREVIEW") == "1" {
			a.mu.Lock()
			injected := a.validationPreviewInjected
			a.mu.Unlock()
			if !injected {
				return errors.New("PC did not load the local validation evidence preview")
			}
		}
		return nil
	}
	first, second := matched[runs[0].TaskID], matched[runs[1].TaskID]
	if counts[runs[0].TaskID] != 1 || counts[runs[1].TaskID] != 1 || first.ID == "" || second.ID == "" {
		return errors.New("result sources are missing or duplicated")
	}
	if scenario == "merge" {
		if first.ID != second.ID || len(first.Sources) != 2 || first.Sources[0].TaskID != runs[0].TaskID ||
			first.Sources[1].TaskID != runs[1].TaskID ||
			!strings.Contains(first.Content, "本地验收结果："+runs[0].Objective+"\n\n本地验收结果："+runs[1].Objective) {
			return errors.New("nearby unstreamed results were not combined in completion order")
		}
	} else if first.ID == second.ID || len(first.Sources) != 1 || len(second.Sources) != 1 {
		return errors.New("results outside the grouping window were not published separately")
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyCompletionRace(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["completion-race"]
	if run == nil || run.stage != "completed" || !run.completionRejected || run.completionCandidate == "" ||
		run.completionSteerMessageID == "" || run.current.ID == "" {
		stage := "missing"
		if run != nil {
			stage = run.stage
		}
		a.mu.Unlock()
		return fmt.Errorf("completion-race Task has not reached its fenced completion stage (stage=%s)", stage)
	}
	current, originalMessageID, steerMessageID, resultEventSequence := run.current, run.userMessageID,
		run.completionSteerMessageID, run.resultEventSequence
	a.mu.Unlock()
	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read completed completion-race Task: %w", err)
	}
	if details.Task.Status != task.StatusCompleted || details.Task.Result != browserActivityCompletionRaceResult ||
		strings.Contains(details.Task.Result, "STALE") || resultEventSequence < 1 {
		return fmt.Errorf("completion-race result status/content/sequence mismatch: %s/%q/%d",
			details.Task.Status, details.Task.Result, resultEventSequence)
	}
	var routedTaskID, deliveryStatus, deliveryMode, routeState string
	if err := a.store.pool.QueryRow(ctx, `SELECT COALESCE(message.task_id,''),message.delivery_status,message.delivery_mode,route.state
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.id=$1 AND message.owner_id=$2 AND message.conversation_id=$3 AND message.role='user'`,
		steerMessageID, current.OwnerID, a.conversationID).Scan(&routedTaskID, &deliveryStatus, &deliveryMode, &routeState); err != nil ||
		routedTaskID != current.ID || deliveryStatus != "applied" || deliveryMode != string(app.AgentMessageDeliverySteer) || routeState != "routed" {
		if err != nil {
			return fmt.Errorf("steer was not applied to the original Task: %w", err)
		}
		return fmt.Errorf("steer was not applied to the original Task: task=%q delivery=%s/%s route=%s",
			routedTaskID, deliveryStatus, deliveryMode, routeState)
	}
	var independentMessageID, independentTaskID, independentDelivery, independentRoute string
	err = a.store.pool.QueryRow(ctx, `SELECT message.id,COALESCE(message.task_id,''),message.delivery_status,route.state
FROM conversation_messages message JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.owner_id=$1 AND message.conversation_id=$2 AND message.role='user' AND message.content=$3`,
		current.OwnerID, a.conversationID, browserActivityCompletionRaceIndependent).Scan(&independentMessageID,
		&independentTaskID, &independentDelivery, &independentRoute)
	if err != nil {
		return fmt.Errorf("read independent request route: %w", err)
	}
	if independentTaskID == "" || independentTaskID == current.ID || independentDelivery != "applied" || independentRoute != "routed" {
		return fmt.Errorf("independent request was not routed to a separate Task: message=%q task=%q delivery=%s route=%s",
			independentMessageID, independentTaskID, independentDelivery, independentRoute)
	}
	independentDetails, err := a.store.GetTask(ctx, independentTaskID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read independent completion-race Task: %w", err)
	}
	if independentDetails.Task.Status != task.StatusCompleted ||
		independentDetails.Task.Result != "本地验收回复："+browserActivityCompletionRaceIndependent {
		return fmt.Errorf("independent Task result was changed by the first Task: status=%s result=%q",
			independentDetails.Task.Status, independentDetails.Task.Result)
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read completion-race conversation results: %w", err)
	}
	resultMessages := make(map[string]conversation.Message, 2)
	resultCounts := map[string]int{current.ID: 0, independentTaskID: 0}
	for _, message := range page.Items {
		if message.Role != conversation.RoleAssistant {
			continue
		}
		for _, source := range message.Sources {
			if source.TaskID != current.ID && source.TaskID != independentTaskID {
				continue
			}
			resultCounts[source.TaskID]++
			if source.TaskID == current.ID && (message.ID == "" || len(message.Sources) != 1 ||
				source.UserMessageID != originalMessageID || source.TaskRevision != 0 || source.EventSequence != resultEventSequence ||
				source.Content != browserActivityCompletionRaceResult || !strings.Contains(message.Content, browserActivityCompletionRaceResult)) {
				return errors.New("original Task result source is stale or detached")
			}
			if source.TaskID == independentTaskID && (message.ID == "" || len(message.Sources) != 1 ||
				source.UserMessageID != independentMessageID || source.TaskRevision != 0 ||
				source.Content != independentDetails.Task.Result || !strings.Contains(message.Content, independentDetails.Task.Result)) {
				return errors.New("independent Task result source is detached or mixed")
			}
			resultMessages[source.TaskID] = message
		}
		if strings.Contains(message.Content, "STALE COMPLETION RESULT") {
			return errors.New("stale completion candidate was published to the conversation")
		}
	}
	if resultCounts[current.ID] != 1 || resultCounts[independentTaskID] != 1 ||
		resultMessages[current.ID].ID == "" || resultMessages[independentTaskID].ID == "" ||
		resultMessages[current.ID].ID == resultMessages[independentTaskID].ID {
		return fmt.Errorf("Task results were missing, duplicated or combined: original=%d/%q independent=%d/%q",
			resultCounts[current.ID], resultMessages[current.ID].ID, resultCounts[independentTaskID], resultMessages[independentTaskID].ID)
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyCompletionRaceOutcome(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["completion-race"]
	if run == nil {
		a.mu.Unlock()
		return errors.New("completion-race scenario is missing")
	}
	current, steerMessageID, stage := run.current, run.completionSteerMessageID, run.stage
	completionRejected := run.completionRejected
	a.mu.Unlock()
	if stage != "completed" || !completionRejected {
		return fmt.Errorf("completion-race has not reached a fenced completed result (stage=%s)", stage)
	}
	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read completion-race Task: %w", err)
	}
	if details.Task.Status != task.StatusCompleted || details.Task.Result != browserActivityCompletionRaceResult ||
		!domain.ValidMessageID(steerMessageID) {
		return errors.New("completion-race Task did not preserve the steered final result")
	}
	var resultEvents, correctResultEvents, steerMessages int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE payload->>'result'=$2)
FROM task_events WHERE task_id=$1 AND event_type='task.result'`, current.ID, browserActivityCompletionRaceResult).
		Scan(&resultEvents, &correctResultEvents); err != nil {
		return fmt.Errorf("read completion-race result events: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages message
JOIN conversation_message_routes route ON route.message_id=message.id
WHERE message.id=$1 AND message.task_id=$2 AND message.role='user' AND message.content=$3
 AND message.delivery_status='applied' AND route.state='routed'`, steerMessageID, current.ID, browserActivityCompletionRaceSteer).Scan(&steerMessages); err != nil {
		return fmt.Errorf("read applied completion-race steer: %w", err)
	}
	if resultEvents != 1 || correctResultEvents != 1 || steerMessages != 1 {
		return fmt.Errorf("completion-race final evidence is inconsistent: result_events=%d correct_results=%d applied_steers=%d",
			resultEvents, correctResultEvents, steerMessages)
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read completion-race delivered result: %w", err)
	}
	matchingResults := 0
	for _, message := range page.Items {
		if message.Role != conversation.RoleAssistant {
			continue
		}
		for _, source := range message.Sources {
			if source.TaskID != current.ID {
				continue
			}
			if source.Content != browserActivityCompletionRaceResult || !strings.Contains(message.Content, browserActivityCompletionRaceResult) ||
				strings.Contains(message.Content, "STALE COMPLETION RESULT") {
				return errors.New("stale completion candidate escaped into the delivered Task result")
			}
			matchingResults++
		}
	}
	if matchingResults != 1 {
		return fmt.Errorf("completion-race delivered %d Task-linked result facts, want one", matchingResults)
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyInterrupted(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["interrupted"]
	if run == nil {
		a.mu.Unlock()
		return errors.New("interrupted scenario is missing")
	}
	current, messageID, stage, failure, stopStatus := run.current, run.streamMessageID, run.stage, run.failure, run.stopStatus
	messages := append([]browserActivityMessageSnapshotRequest(nil), a.messageSnapshots...)
	a.mu.Unlock()
	if stage != "interrupted" {
		if failure != "" {
			return fmt.Errorf("interrupted scenario has not reached the cancelled stage: %s", failure)
		}
		return fmt.Errorf("interrupted scenario has not reached the cancelled stage (stage=%s)", stage)
	}
	if current.ID == "" || messageID == "" {
		return errors.New("interrupted scenario is missing its Task or assistant message identity")
	}
	var taskStatus string
	if err := a.store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1 AND initiator_id=$2`,
		current.ID, current.InitiatorID).Scan(&taskStatus); err != nil {
		return fmt.Errorf("read interrupted Task status: %w", err)
	}
	if taskStatus != string(task.StatusCancelling) && taskStatus != string(task.StatusCancelled) {
		return fmt.Errorf("interrupted Task status=%s, want cancelling or cancelled", taskStatus)
	}
	var taskID, content, outputStatus string
	var revision int64
	if err := a.store.pool.QueryRow(ctx, `SELECT task_id,content,output_status,output_task_revision
FROM conversation_messages WHERE id=$1`, messageID).Scan(&taskID, &content, &outputStatus, &revision); err != nil {
		return fmt.Errorf("read cancelled assistant message: %w", err)
	}
	var snapshotMessageID, snapshotContent, snapshotStatus string
	var snapshotRevision int64
	if err := a.store.pool.QueryRow(ctx, `SELECT message_id,content,status,task_revision
FROM task_assistant_message_snapshots WHERE task_id=$1`, current.ID).
		Scan(&snapshotMessageID, &snapshotContent, &snapshotStatus, &snapshotRevision); err != nil {
		return fmt.Errorf("read cancelled assistant message snapshot: %w", err)
	}
	var finalResultEvents int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, current.ID).
		Scan(&finalResultEvents); err != nil {
		return fmt.Errorf("count final result events after cancel: %w", err)
	}
	if taskID != current.ID || taskStatus != stopStatus && taskStatus != string(task.StatusCancelled) ||
		content != browserActivityStreamPrefix || outputStatus != string(conversation.OutputInterrupted) ||
		snapshotMessageID != messageID || snapshotContent != content || snapshotStatus != outputStatus ||
		snapshotRevision != revision || finalResultEvents != 0 {
		return fmt.Errorf("cancelled stream evidence mismatch: task=%s task_status=%s content=%q output_status=%s snapshot=%s/%q/%s result_events=%d",
			taskID, taskStatus, content, outputStatus, snapshotMessageID, snapshotContent, snapshotStatus, finalResultEvents)
	}
	return verifyInterruptedMessageSnapshots(messages, current.ID, messageID)
}

func (a *browserAcceptanceActivity) verifyTargetControl(ctx context.Context) error {
	a.mu.Lock()
	first, second := a.runs["target-control-a"], a.runs["target-control-b"]
	if first == nil || second == nil || first.current.ID == "" || second.current.ID == "" ||
		first.current.ID == second.current.ID || first.current.InitiatorID != second.current.InitiatorID ||
		first.current.ConversationID != second.current.ConversationID || first.userMessageID == "" || second.userMessageID == "" {
		a.mu.Unlock()
		return errors.New("two distinct target-control Tasks in one conversation are required")
	}
	firstCurrent, secondCurrent := first.current, second.current
	a.mu.Unlock()

	firstDetails, err := a.store.GetTask(ctx, firstCurrent.ID, firstCurrent.InitiatorID)
	if err != nil {
		return fmt.Errorf("read sibling Task: %w", err)
	}
	secondDetails, err := a.store.GetTask(ctx, secondCurrent.ID, secondCurrent.InitiatorID)
	if err != nil {
		return fmt.Errorf("read target Task: %w", err)
	}
	if firstDetails.Task.Status != task.StatusRunning || secondDetails.Task.Status != task.StatusCancelling {
		return fmt.Errorf("Task isolation mismatch: sibling=%s target=%s, want running/cancelling",
			firstDetails.Task.Status, secondDetails.Task.Status)
	}
	a.mu.Lock()
	second.stage, second.failure = string(secondDetails.Task.Status), ""
	a.mu.Unlock()
	for _, details := range []struct {
		id              string
		currentRevision *int64
		desiredRevision *int64
		started         *bool
	}{
		{id: firstDetails.Task.ID, currentRevision: firstDetails.Task.CurrentRevision,
			desiredRevision: firstDetails.Task.DesiredRevision, started: firstDetails.CurrentRevisionStarted},
		{id: secondDetails.Task.ID, currentRevision: secondDetails.Task.CurrentRevision,
			desiredRevision: secondDetails.Task.DesiredRevision, started: secondDetails.CurrentRevisionStarted},
	} {
		if details.started == nil || !*details.started || details.currentRevision == nil || *details.currentRevision != 0 ||
			details.desiredRevision == nil || *details.desiredRevision != 0 {
			return fmt.Errorf("Task %s did not reach its expected active revision", details.id)
		}
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyClarificationOutcome(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["clarification"]
	if run == nil {
		a.mu.Unlock()
		return errors.New("clarification scenario is missing")
	}
	stage := run.stage
	if stage != "completed" {
		a.mu.Unlock()
		return a.verifyAcceptedOutcome(ctx, "clarification")
	}
	current, userMessageID, answer, resultEventSequence := run.current, run.userMessageID, run.clarificationAnswer, run.resultEventSequence
	a.mu.Unlock()

	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read completed clarification Task: %w", err)
	}
	if details.Task.Status != task.StatusCompleted || len(details.ClarificationRequests) != 1 ||
		details.ClarificationRequests[0].Status != task.ClarificationAnswered ||
		details.ClarificationRequests[0].Answer != browserActivityClarificationAnswer || answer != browserActivityClarificationAnswer ||
		!strings.Contains(details.Task.Result, answer) || resultEventSequence < 1 {
		return errors.New("completed clarification Task does not preserve the submitted answer and result")
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read clarification result message: %w", err)
	}
	matchingResults := 0
	for _, message := range page.Items {
		if message.Role != conversation.RoleAssistant {
			continue
		}
		for _, source := range message.Sources {
			if source.TaskID != current.ID {
				continue
			}
			if source.UserMessageID != userMessageID || source.TaskRevision != 0 || source.EventSequence != resultEventSequence ||
				source.Content != details.Task.Result || !strings.Contains(message.Content, details.Task.Result) {
				return errors.New("clarification result is detached from its answered Task revision")
			}
			matchingResults++
		}
	}
	if matchingResults != 1 {
		return fmt.Errorf("expected one original-session clarification result, got %d", matchingResults)
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyAcceptedOutcome(ctx context.Context, scenario string) error {
	wantStage, wantStatus := "failed", task.StatusFailed
	if scenario == "approval" || scenario == "clarification" || scenario == "recovery-unknown" {
		wantStage, wantStatus = "waiting_user", task.StatusWaitingUser
	}
	a.mu.Lock()
	run := a.runs[scenario]
	if run == nil || run.stage != wantStage {
		stage := "missing"
		if run != nil {
			stage = run.stage
		}
		a.mu.Unlock()
		return fmt.Errorf("scenario %s has not reached %s (stage %s)", scenario, wantStage, stage)
	}
	current, userMessageID, progressStepID := run.current, run.userMessageID, run.progressStepID
	a.mu.Unlock()

	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read persisted %s task: %w", scenario, err)
	}
	if details.Task.Status != wantStatus || details.CurrentRevisionStarted == nil || !*details.CurrentRevisionStarted {
		return fmt.Errorf("persisted task status/start = %s/%v, want %s and an accepted revision",
			details.Task.Status, details.CurrentRevisionStarted, wantStatus)
	}
	if scenario == "failed" {
		if details.PendingRequest != nil {
			return errors.New("failed task unexpectedly has a pending approval request")
		}
	} else if scenario == "approval" && (details.Task.WaitingReason != "approval_required" || details.PendingRequest == nil ||
		!domain.ValidPendingRequestID(details.PendingRequest.ID) || details.PendingRequest.TaskID != current.ID || details.PendingRequest.Version < 1 ||
		details.PendingRequest.Kind != "approval" || details.PendingRequest.Status != task.PendingRequestPending ||
		details.PendingRequest.Decision != "" || len(details.PendingRequest.RequestHash) != sha256.Size ||
		details.PendingRequest.RuntimeGeneration < 0 || details.PendingRequest.RuntimeInstanceID == "" ||
		details.PendingRequest.ControlVersion < 0 || details.PendingRequest.CallID == "" || details.PendingRequest.ToolName == "" ||
		details.PendingRequest.ExpiresAt != nil && !details.PendingRequest.ExpiresAt.After(time.Now())) {
		return errors.New("waiting task does not have a valid active approval request")
	} else if scenario == "clarification" {
		if details.Task.WaitingReason != "clarification_required" || len(details.ClarificationRequests) != 1 {
			return errors.New("waiting task does not have one active clarification request")
		}
		request := details.ClarificationRequests[0]
		if !domain.ValidPendingRequestID(request.ID) || request.TaskID != current.ID || request.Version != 1 ||
			request.ModelStepID == "" || request.CallID == "" || request.Question != "请告诉我应该把报告保存到哪个目录？" ||
			request.Status != task.ClarificationPending || request.Answer != "" {
			return errors.New("persisted clarification request is incomplete or not pending")
		}
		var currentStep *task.Step
		for i := range details.Steps {
			if details.Steps[i].ID == details.Task.CurrentStep {
				currentStep = &details.Steps[i]
				break
			}
		}
		if currentStep == nil || currentStep.Kind != "clarification" || currentStep.Status != task.StepWaitingUser {
			return errors.New("clarification task does not point to its waiting work item")
		}
	} else if scenario == "recovery-unknown" {
		if details.Task.WaitingReason != "dsh_recovery_unresolved" || details.PendingRequest != nil || len(details.ClarificationRequests) != 0 ||
			!strings.Contains(details.Task.Result, "恢复检查无法确认上一轮响应是否完成") {
			return errors.New("recovery-unknown task did not preserve its unresolved state without inventing a request")
		}
		var currentStep, lastConfirmedStep *task.Step
		for i := range details.Steps {
			if details.Steps[i].ID == details.Task.CurrentStep {
				currentStep = &details.Steps[i]
			}
			if details.Steps[i].ID == progressStepID {
				lastConfirmedStep = &details.Steps[i]
			}
		}
		if lastConfirmedStep == nil || lastConfirmedStep.Sequence != 2 || lastConfirmedStep.Kind != "progress" ||
			lastConfirmedStep.Status != task.StepSucceeded || lastConfirmedStep.Summary != "合成文字处理完成。" {
			return errors.New("recovery-unknown task is missing its last confirmed successful work item")
		}
		if currentStep == nil || currentStep.Kind != "progress" || currentStep.Status != task.StepUnknown ||
			!strings.Contains(currentStep.Summary, "无法确认上次响应是否完整") {
			return errors.New("recovery-unknown task does not point to a clearly unknown progress step")
		}
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read accepted synthetic route: %w", err)
	}
	for _, message := range page.Items {
		if message.ID == userMessageID && message.TaskID == current.ID && message.Role == conversation.RoleUser &&
			message.AgentAcknowledged && message.RoutingStatus == conversation.RoutingRouted &&
			message.DeliveryStatus == conversation.DeliveryApplied {
			if scenario == browserActivityCrossClientScenario() {
				a.mu.Lock()
				snapshots := append([]browserActivityTaskSnapshotRequest(nil), a.taskSnapshots...)
				a.mu.Unlock()
				switch scenario {
				case "approval":
					return verifyApprovalClientSnapshots(snapshots, current.ID)
				case "recovery-unknown":
					return verifyRecoveryUnknownClientSnapshots(snapshots, current.ID)
				}
			}
			return nil
		}
	}
	return errors.New("synthetic route was not persisted with agent_acknowledged=true")
}

func (a *browserAcceptanceActivity) verifyApprovalResumeOutcome(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["approval-resume"]
	if run == nil {
		a.mu.Unlock()
		return errors.New("approval-resume scenario is missing")
	}
	current, userMessageID := run.current, run.userMessageID
	resultEventSequence := run.resultEventSequence
	approvalID, approvalVersion, approvalCallID := run.approvalID, run.approvalVersion, run.approvalCallID
	approvalRequestHash := append([]byte(nil), run.approvalRequestHash...)
	resumeCount, stage := run.approvalResumeCount, run.stage
	snapshots := append([]browserActivityTaskSnapshotRequest(nil), a.taskSnapshots...)
	a.mu.Unlock()
	if stage != "completed" {
		return fmt.Errorf("approval-resume scenario has not reached completed (stage %s)", stage)
	}
	if resumeCount != 1 || !domain.ValidPendingRequestID(approvalID) || approvalVersion < 1 ||
		approvalCallID == "" || len(approvalRequestHash) != sha256.Size {
		return fmt.Errorf("approval continuation state stage=%s resumes=%d approval=%s/%d call=%s hash_bytes=%d",
			stage, resumeCount, approvalID, approvalVersion, approvalCallID, len(approvalRequestHash))
	}

	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read completed approval-resume Task: %w", err)
	}
	pending := details.PendingRequest
	if details.Task.ID != current.ID || details.Task.Status != task.StatusCompleted || details.Task.WaitingReason != "" ||
		pending == nil || pending.ID != approvalID || pending.Version != approvalVersion ||
		pending.Status != task.PendingRequestApproved || pending.Decision != task.PendingDecisionApprove ||
		pending.CallID != approvalCallID || !bytes.Equal(pending.RequestHash, approvalRequestHash) {
		return errors.New("completed Task is not bound to the exact approved request")
	}
	var completedStep *task.Step
	for i := range details.Steps {
		if details.Steps[i].ID == details.Task.CurrentStep {
			completedStep = &details.Steps[i]
			break
		}
	}
	if completedStep == nil || completedStep.Status != task.StepSucceeded || completedStep.Kind != "result" ||
		completedStep.Title != "处理完成" || details.Task.Result != browserActivityApprovalResumeResult {
		return errors.New("approval-resume Task did not finish its explicit local synthetic continuation")
	}

	events, err := a.store.ListTaskEvents(ctx, current.InitiatorID, current.ID, 0, 100)
	if err != nil {
		return fmt.Errorf("read approval-resume Task events: %w", err)
	}
	responseEvents, resultEvents := 0, 0
	for _, event := range events {
		switch event.Type {
		case "task.response":
			responseEvents++
			var response struct {
				PendingID      string `json:"pending_id"`
				PendingVersion int64  `json:"pending_version"`
				Decision       string `json:"decision"`
				Status         string `json:"status"`
			}
			if err := json.Unmarshal(event.Payload, &response); err != nil || response.PendingID != approvalID ||
				response.PendingVersion != approvalVersion || response.Decision != string(task.PendingDecisionApprove) ||
				response.Status != string(task.StatusQueued) {
				return errors.New("approval response event does not match the approved request identity")
			}
		case "task.result":
			resultEvents++
			var result struct {
				Status          task.StepStatus `json:"status"`
				Result          string          `json:"result"`
				StepID          string          `json:"step_id"`
				ApprovalID      string          `json:"approval_id"`
				ApprovalVersion int64           `json:"approval_version"`
				CallID          string          `json:"call_id"`
			}
			if err := json.Unmarshal(event.Payload, &result); err != nil || result.Status != task.StepSucceeded ||
				result.Result != browserActivityApprovalResumeResult || result.StepID != details.Task.CurrentStep ||
				result.ApprovalID != approvalID || result.ApprovalVersion != approvalVersion || result.CallID != approvalCallID {
				return errors.New("completion event does not match the approved request identity")
			}
		}
	}
	if responseEvents != 1 || resultEvents != 1 {
		return fmt.Errorf("approval-resume Task events contain %d approval responses and %d results, want one each", responseEvents, resultEvents)
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read approval-resume accepted route: %w", err)
	}
	accepted := false
	assistantMessageIDs := make([]string, 0, len(page.Items))
	for _, message := range page.Items {
		if message.ID == userMessageID && message.TaskID == current.ID && message.Role == conversation.RoleUser &&
			message.AgentAcknowledged && message.RoutingStatus == conversation.RoutingRouted &&
			message.DeliveryStatus == conversation.DeliveryApplied {
			accepted = true
		}
		if message.Role == conversation.RoleAssistant && message.DeliveryStatus == conversation.DeliveryApplied {
			assistantMessageIDs = append(assistantMessageIDs, message.ID)
		}
	}
	if !accepted {
		return errors.New("approval-resume Task is missing its persisted accepted user message")
	}
	resultSources, err := a.store.ListConversationResultSources(ctx, current.InitiatorID, a.conversationID, assistantMessageIDs)
	if err != nil {
		return fmt.Errorf("read approval-resume published result provenance: %w", err)
	}
	approvalResultSources := 0
	for _, sources := range resultSources {
		for _, source := range sources {
			if source.TaskID != current.ID {
				continue
			}
			if source.UserMessageID != userMessageID || source.Content != browserActivityApprovalResumeResult ||
				source.EventSequence != resultEventSequence {
				return errors.New("published assistant result is not bound to the completed approval-resume Task result")
			}
			approvalResultSources++
		}
	}
	if approvalResultSources != 1 {
		return fmt.Errorf("approval-resume Task has %d published assistant result sources, want one", approvalResultSources)
	}
	return verifyApprovalResumeClientSnapshots(snapshots, current.ID)
}

func (a *browserAcceptanceActivity) verifyApprovalRevokedOutcome(ctx context.Context, scenario string) error {
	a.mu.Lock()
	run := a.runs[scenario]
	if run == nil {
		a.mu.Unlock()
		return fmt.Errorf("%s scenario is missing", scenario)
	}
	current, userMessageID := run.current, run.userMessageID
	approvalID, approvalVersion := run.approvalID, run.approvalVersion
	hostID, hostOwnerID := run.revocationHostID, run.revocationHostOwnerID
	stage := run.stage
	snapshots := append([]browserActivityTaskSnapshotRequest(nil), a.taskSnapshots...)
	a.mu.Unlock()
	if stage != "revoked" || !domain.ValidPendingRequestID(approvalID) || approvalVersion < 1 {
		return fmt.Errorf("approval authorization revocation is incomplete: scenario=%s stage=%s approval=%s/%d", scenario, stage, approvalID, approvalVersion)
	}
	details, err := a.store.GetTask(ctx, current.ID, current.InitiatorID)
	if err != nil {
		return fmt.Errorf("read authorization-fenced approval Task: %w", err)
	}
	if details.Task.Status != task.StatusCancelling && details.Task.Status != task.StatusCancelled {
		return fmt.Errorf("revoked approval Task status=%s, want cancelling or cancelled", details.Task.Status)
	}
	if scenario == "approval-host-member-revoked" && (hostID == "" || hostOwnerID == "" || details.Task.HostID != hostID) {
		return fmt.Errorf("revoked approval Task Host=%q, fixture Host=%q owner=%q; want matching shared Host target", details.Task.HostID, hostID, hostOwnerID)
	}
	if details.Task.WaitingReason != "" || details.PendingRequest == nil || details.PendingRequest.ID != approvalID ||
		details.PendingRequest.Version != approvalVersion || details.PendingRequest.Status != task.PendingRequestCancelled ||
		details.StopRequest == nil || details.StopRequest.Action != task.ActionCancel {
		return errors.New("authorization fence did not clear the wait, cancel the exact approval, and persist a stop request")
	}
	if details.Task.Status == task.StatusCancelling && details.StopRequest.Status != task.StopRequestPending ||
		details.Task.Status == task.StatusCancelled && details.StopRequest.Status != task.StopRequestCompleted {
		return fmt.Errorf("revoked approval stop request status=%s does not match Task status=%s", details.StopRequest.Status, details.Task.Status)
	}
	var hostOperations, connectorOperations, resultEvents int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_operations WHERE task_id=$1`, current.ID).Scan(&hostOperations); err != nil {
		return fmt.Errorf("count dispatched Host operations after approval revocation: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_mcp_connector_operations WHERE task_id=$1`, current.ID).Scan(&connectorOperations); err != nil {
		return fmt.Errorf("count connector operations after approval revocation: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND event_type='task.result'`, current.ID).Scan(&resultEvents); err != nil {
		return fmt.Errorf("count Task result events after approval revocation: %w", err)
	}
	if hostOperations != 0 || connectorOperations != 0 || resultEvents != 0 {
		return fmt.Errorf("revoked approval created work or a completion: Host operations=%d connector operations=%d result events=%d",
			hostOperations, connectorOperations, resultEvents)
	}
	page, err := a.store.ListMessages(ctx, current.InitiatorID, a.conversationID, app.PageRequest{Limit: 100})
	if err != nil {
		return fmt.Errorf("read revoked approval Task's accepted route: %w", err)
	}
	accepted := false
	for _, message := range page.Items {
		if message.ID == userMessageID && message.TaskID == current.ID && message.Role == conversation.RoleUser &&
			message.AgentAcknowledged && message.RoutingStatus == conversation.RoutingRouted &&
			message.DeliveryStatus == conversation.DeliveryApplied {
			accepted = true
			break
		}
	}
	if !accepted {
		return errors.New("revoked approval Task is missing its accepted user message")
	}
	a.mu.Lock()
	run.stopStatus = string(details.Task.Status)
	a.mu.Unlock()
	return verifyCrossClientTaskSnapshots(snapshots, current.ID, string(details.Task.Status), "")
}

func (a *browserAcceptanceActivity) verifyMCPRetrySucceeded(ctx context.Context) error {
	a.mu.Lock()
	run := a.runs["mcp-retry"]
	if run == nil {
		a.mu.Unlock()
		return errors.New("MCP retry scenario is missing")
	}
	current, sourceStepID, retryCallID := run.current, run.mcpRetrySourceStepID, run.mcpRetryCallID
	stage, dispatchCount := run.stage, run.mcpRetryDispatchCount
	a.mu.Unlock()
	if stage != "retry_succeeded" {
		return fmt.Errorf("MCP retry has not reached successful completion (stage=%s)", stage)
	}
	details, err := a.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return fmt.Errorf("read completed MCP retry Task: %w", err)
	}
	if details.Task.Status != task.StatusCompleted || details.Task.Result != "本地合成连接器在用户显式重试后成功。" ||
		!domain.ValidStepID(sourceStepID) ||
		!domain.ValidStepID(details.Task.CurrentStep) || retryCallID == "" {
		return fmt.Errorf("MCP retry Task state/current step=%s/%s, want a completed Task at the retry step", details.Task.Status, details.Task.CurrentStep)
	}
	var source, attempt *task.Step
	for i := range details.Steps {
		step := &details.Steps[i]
		if step.ID == sourceStepID {
			source = step
		}
		if step.ID == details.Task.CurrentStep {
			attempt = step
		}
	}
	if source == nil || attempt == nil || source.Status != task.StepFailed || source.FailureReason != "connector_rate_limited" ||
		!source.Retryable || attempt.Status != task.StepSucceeded || attempt.RetryOfStepID != source.ID ||
		attempt.Attempt != 2 || attempt.ToolCallKey != retryCallID || attempt.ToolName != source.ToolName ||
		!bytes.Equal(attempt.RequestDigest, source.RequestDigest) {
		return fmt.Errorf("source/retry Task steps are not preserved and linked: source=%+v retry=%+v", source, attempt)
	}
	if attempt.ID != details.Task.CurrentStep {
		return errors.New("completed Task current step is not the successful retry attempt")
	}
	var attemptCount, operationCount, sourceOperationCount, successfulRetryOperationCount, retryOperationCount int
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_steps WHERE task_id=$1 AND retry_of_step_id=$2`, current.ID, source.ID).Scan(&attemptCount); err != nil {
		return fmt.Errorf("count successful MCP retry attempts: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT count(*),
count(*) FILTER (WHERE call_id=$2 AND status='failed'),
count(*) FILTER (WHERE call_id=$3 AND status='succeeded'),
count(*) FILTER (WHERE call_id=$3)
FROM task_mcp_connector_operations WHERE task_id=$1`, current.ID, source.ToolCallKey, retryCallID).
		Scan(&operationCount, &sourceOperationCount, &successfulRetryOperationCount, &retryOperationCount); err != nil {
		return fmt.Errorf("count source and retry MCP operation ledger rows: %w", err)
	}
	if attemptCount != 1 || operationCount != 2 || sourceOperationCount != 1 || successfulRetryOperationCount != 1 ||
		retryOperationCount != 1 || dispatchCount != 1 {
		return fmt.Errorf("MCP retry duplicated or lost an operation: retry steps=%d ledger rows=%d source failures=%d retry successes=%d retry rows=%d synthetic dispatches=%d",
			attemptCount, operationCount, sourceOperationCount, successfulRetryOperationCount, retryOperationCount, dispatchCount)
	}
	var sourceStatus, retryStatus, sourceConnectorID, retryConnectorID, retryOperationKey, retryResult string
	var sourceDigest, retryDigest []byte
	if err := a.store.pool.QueryRow(ctx, `SELECT status,request_hash,connector_id FROM task_mcp_connector_operations
WHERE task_id=$1 AND call_id=$2`, current.ID, source.ToolCallKey).
		Scan(&sourceStatus, &sourceDigest, &sourceConnectorID); err != nil {
		return fmt.Errorf("read source MCP failure ledger: %w", err)
	}
	if err := a.store.pool.QueryRow(ctx, `SELECT status,request_hash,connector_id,operation_key,result
FROM task_mcp_connector_operations WHERE task_id=$1 AND call_id=$2`, current.ID, retryCallID).
		Scan(&retryStatus, &retryDigest, &retryConnectorID, &retryOperationKey, &retryResult); err != nil {
		return fmt.Errorf("read successful MCP retry ledger: %w", err)
	}
	if sourceStatus != "failed" || retryStatus != "succeeded" || sourceConnectorID != retryConnectorID ||
		!bytes.Equal(sourceDigest, source.RequestDigest) || !bytes.Equal(retryDigest, attempt.RequestDigest) ||
		retryOperationKey != app.AgentOperationKey(retryCallID) ||
		retryResult != `{"is_error":false,"text":["synthetic connector result after explicit retry"]}` {
		return fmt.Errorf("MCP source/retry operation ledger evidence is inconsistent: source=%s retry=%s connectors_match=%t key=%q result=%q",
			sourceStatus, retryStatus, sourceConnectorID == retryConnectorID, retryOperationKey, retryResult)
	}
	return nil
}

func (a *browserAcceptanceActivity) verifyReconnect(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.runs[name]
	if run == nil || run.dropCursors == nil {
		return errors.New("the fixture has not disconnected both clients")
	}
	seen := make(map[string]bool)
	for _, request := range a.requests[run.dropIndex:] {
		if request.TaskID != run.current.ID {
			continue
		}
		cursor := request.After
		if request.LastID > cursor {
			cursor = request.LastID
		}
		if cursor < run.dropCursors[request.Client] {
			return fmt.Errorf("%s event cursor moved backwards", request.Client)
		}
		seen[request.Client] = true
	}
	if !seen["browser"] || !seen["ios"] {
		return errors.New("wait for both clients to reconnect to the same task")
	}
	return nil
}
