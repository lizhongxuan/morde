package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
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
	enableTaskExecutionForTest(t, store, principal)
	audit, err := app.NewAuditService(store)
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
	model := browserAcceptanceModel{}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"example.invalid"})
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
	var attachmentResume *browserAttachmentResumeFixture
	var messageUploadResume *browserMessageUploadResumeFixture
	if os.Getenv("MORDE_BROWSER_ATTACHMENT_RESUME") == "1" {
		attachmentResume = &browserAttachmentResumeFixture{}
	}
	if os.Getenv("MORDE_BROWSER_MESSAGE_UPLOAD_RESUME") == "1" {
		messageUploadResume = newBrowserMessageUploadResumeFixture(mainConversation.ConversationID)
	}
	if os.Getenv("MORDE_BROWSER_ACTIVITY") == "1" {
		activity, err = newBrowserAcceptanceActivity(store, sealer, mainConversation.ConversationID)
		if err != nil {
			t.Fatal(err)
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
	routerWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: gateway, WorkerID: "browser-acceptance-route",
		LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond, MaxConcurrent: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resultWorker, err := worker.NewConversationResultReviewWorker(store, 100*time.Millisecond, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workerDone := make(chan error, 3)
	workerCount := 0
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	workerCount++
	if (projectAttachmentResume == nil || activity != nil) && messageUploadResume == nil {
		go func() { workerDone <- routerWorker.Run(workerCtx) }()
		workerCount++
	}
	go func() { workerDone <- resultWorker.Run(workerCtx) }()
	workerCount++
	t.Cleanup(func() {
		cancelWorkers()
		for i := 0; i < workerCount; i++ {
			if err := <-workerDone; !errors.Is(err, context.Canceled) {
				t.Errorf("stop browser fixture worker: %v", err)
			}
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	portalRoot := filepath.Join("..", "..", "..", "..", "web", "portal", "dist")
	apiDeps := api.Deps{Hosts: nativeAcceptanceHosts{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, ClientLogin: clients, Tasks: tasks, Conversations: conversations,
		PersonalData: personalData, PersonalSync: personalSync, PersonalDataQuotaUser: personalQuotaUser,
		PersonalAttachments: personalAttachments,
		ModelConnections:    models, ModelGateway: gateway, ExecutionControl: execution, MCPDisabled: true,
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
	// The production capability remains disabled until all acceptance checks
	// pass. This disposable fixture advertises it only to exercise the UI.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("LOCAL_HTTP %s %s authorization=%t\n", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
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
			return
		}
		if activity != nil && strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/events") {
			activity.serveTaskEvents(w, r, router)
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
	stop := filepath.Join(t.TempDir(), "finish")
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
				if activity != nil || attachmentResume != nil || projectAttachmentResume != nil || messageUploadResume != nil {
					minimumMessages = 1
				}
				if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE role='user'),count(*) FILTER (WHERE role='assistant') FROM conversation_messages WHERE owner_id=$1`, owner.ID).Scan(&messages, &replies); err != nil || messages < minimumMessages || (attachmentResume == nil && projectAttachmentResume == nil && messageUploadResume == nil && replies < 1) {
					t.Fatalf("browser messages=%d replies=%d err=%v", messages, replies, err)
				}
				if activity != nil {
					if err := activity.assertVerified(); err != nil {
						t.Fatal(err)
					}
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
					// The synthetic 5 MiB JPEG produces one readable derivative beside its original.
					if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND state='committed'`, instanceID, owner.ID).Scan(&committedUploads); err != nil || committedUploads != 2 {
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

type browserAcceptanceModel struct{ nativeAcceptanceModel }

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

func (browserAcceptanceModel) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if strings.HasPrefix(request.Instructions, "Classify the user's new message") {
		return app.ModelResponse{Text: `{"action":"new","task_id":"","question":""}`,
			Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
	}
	return (nativeAcceptanceModel{}).Complete(ctx, request)
}

type browserAcceptanceExecutor struct{ store *Store }

func (e browserAcceptanceExecutor) ExecuteTask(ctx context.Context, current task.Task) error {
	timer := time.NewTimer(750 * time.Millisecond)
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

// The opt-in activity stages are selected only by these synthetic messages.
// Its control API is loopback-only and protected by a fresh per-run token,
// written to a mode-0600 file inside the test's temporary directory.
// No file or user-provided content is needed by any of these scenarios.
// With MORDE_BROWSER_ACTIVITY=1, GET /__morde_fixture/status lists the fixed
// inputs and stages. POST /__morde_fixture/advance/{name}/{start|progress|
// stream|complete} releases one stage, without a request body. The search
// scenario uses a fixed local result and never accesses the network. Setting
// MORDE_BROWSER_ACTIVITY_SCENARIOS=search runs only that scenario; omit it to
// require the full merge, separate, search, stream and reconnect set. For a
// full run, merge-a and merge-b must complete within the aggregation window;
// separate-a and separate-b must complete in order. Start stream before
// submitting stream-other, then disconnect both
// clients using POST /__morde_fixture/disconnect/stream before completing the
// pair. GET /__morde_fixture/verify/{merge|separate|reconnect|search|stream}
// checks progress/result identity, sources, result ordering and reconnect
// cursors. The fixture does not pass until all checks succeed; visual
// deduplication still needs to be checked in both clients after refresh/relaunch.
var browserActivityObjectives = map[string]string{
	"merge-a":      "跨端活动验收：合并 A",
	"merge-b":      "跨端活动验收：合并 B",
	"search":       "在本地示例索引中搜索 Morde 消息路由",
	"separate-a":   "跨端活动验收：分开 A",
	"separate-b":   "跨端活动验收：分开 B",
	"stream":       "跨端活动验收：流式",
	"stream-other": "跨端活动验收：流式后的独立任务",
}

const browserActivityStreamPrefix = "跨端活动验收：流式回复第一段"
const browserActivityStreamSuffix = "，第二段完成。"
const browserActivitySearchResult = "本地搜索结果：Morde 消息路由先持久接收消息，再异步分流。"

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
	name, objective, stage, failure string
	current                         task.Task
	userMessageID, progressStepID   string
	streamMessageID, streamTurnID   string
	streamSequence                  int64
	resultEventSequence             int64
	busy                            bool
	commands                        chan browserActivityCommand
	dropIndex                       int
	dropCursors                     map[string]int64
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
	Client     string `json:"client"`
	TaskID     string `json:"task_id"`
	StatusCode int    `json:"status_code"`
}

type browserActivityWatch struct {
	taskID string
	client string
	cancel context.CancelFunc
}

type browserAcceptanceActivity struct {
	store            *Store
	instances        *app.AgentInstanceService
	checkpoints      *app.AgentCheckpointService
	conversationID   string
	controlToken     string
	required         []string
	mu               sync.Mutex
	runs             map[string]*browserActivityRun
	verified         map[string]bool
	watches          map[uint64]browserActivityWatch
	requests         []browserActivityEventRequest
	taskListRequests []browserActivityTaskListRequest
	taskSnapshots    []browserActivityTaskSnapshotRequest
	nextWatch        uint64
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
		conversationID: conversationID, controlToken: hex.EncodeToString(token),
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
		"merge":    {"merge-a", "merge-b"},
		"separate": {"separate-a", "separate-b"},
		"search":   {"search"},
		"stream":   {"stream", "stream-other"},
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

func (a *browserAcceptanceActivity) ExecuteTask(ctx context.Context, current task.Task) error {
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
	var delta app.CloudDSHTextDelta
	if run.name == "stream" {
		if err := a.waitPhase(ctx, run, "stream", "streaming", renewErr, func() error {
			var err error
			delta, err = a.startStream(ctx, run, lease)
			return err
		}); err != nil {
			return err
		}
	}
	return a.waitPhase(ctx, run, "complete", "completed", renewErr, func() error {
		return a.complete(ctx, run, delta)
	})
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
	starts, progress, results, streamEvents := 0, 0, 0, 0
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
			if completed && event.Sequence != run.resultEventSequence {
				return errors.New("task result event identity changed")
			}
		}
		if event.Type == "task.message" {
			streamEvents++
		}
	}
	if starts != 1 || run.progressStepID != "" && progress != 1 || completed && results != 1 ||
		completed && run.name == "stream" && streamEvents < 2 {
		return fmt.Errorf("unexpected task events: start=%d progress=%d result=%d stream=%d", starts, progress, results, streamEvents)
	}
	return nil
}

type browserActivityRunView struct {
	Name                string `json:"name"`
	Objective           string `json:"objective"`
	Stage               string `json:"stage"`
	Failure             string `json:"failure,omitempty"`
	TaskID              string `json:"task_id,omitempty"`
	UserMessageID       string `json:"user_message_id,omitempty"`
	StreamMessageID     string `json:"stream_message_id,omitempty"`
	StreamSequence      int64  `json:"stream_sequence,omitempty"`
	ResultEventSequence int64  `json:"result_event_sequence,omitempty"`
}

func browserActivityView(run *browserActivityRun) browserActivityRunView {
	return browserActivityRunView{Name: run.name, Objective: run.objective, Stage: run.stage, Failure: run.failure,
		TaskID: run.current.ID, UserMessageID: run.userMessageID, StreamMessageID: run.streamMessageID,
		StreamSequence: run.streamSequence, ResultEventSequence: run.resultEventSequence}
}

func (a *browserAcceptanceActivity) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Morde-Fixture-Token") != a.controlToken {
		http.Error(w, "fixture control is unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__morde_fixture/"), "/")
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
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs, "active_event_streams": active,
			"event_requests": requests, "conversation_task_list_requests": taskListRequests,
			"task_snapshot_requests": taskSnapshots})
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
		_ = json.NewEncoder(w).Encode(map[string]string{"scenario": parts[1], "status": "verified"})
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
	a.mu.Lock()
	run := a.runs[name]
	if run == nil {
		a.mu.Unlock()
		return errors.New("unknown synthetic scenario")
	}
	expected := map[string]string{"start": "claimed", "progress": "started", "stream": "progress", "complete": "progress"}[phase]
	if name == "stream" && phase == "complete" {
		expected = "streaming"
	}
	if expected == "" || phase == "stream" && name != "stream" || run.stage != expected || run.busy {
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

func (a *browserAcceptanceActivity) serveTaskSnapshot(w http.ResponseWriter, r *http.Request, next http.Handler) {
	id := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	client := "browser"
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		client = "ios"
	}
	a.mu.Lock()
	a.taskSnapshots = append(a.taskSnapshots, browserActivityTaskSnapshotRequest{
		Client: client, TaskID: id, StatusCode: recorder.Code,
	})
	a.mu.Unlock()
	for key, values := range recorder.Header() {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
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

func (a *browserAcceptanceActivity) verifyResult(ctx context.Context, scenario string) error {
	if scenario == "reconnect" {
		return a.verifyReconnect("stream")
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
	if scenario == "search" {
		run := runs[0]
		message := matched[run.TaskID]
		if counts[run.TaskID] != 1 || message.ID == "" || len(message.Sources) != 1 || message.Content != browserActivitySearchResult {
			return errors.New("fixed local search result is missing, duplicated or detached from its task")
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
			first.Content != "本地验收结果："+runs[0].Objective+"\n\n本地验收结果："+runs[1].Objective {
			return errors.New("nearby unstreamed results were not combined in completion order")
		}
	} else if first.ID == second.ID || len(first.Sources) != 1 || len(second.Sources) != 1 {
		return errors.New("results outside the grouping window were not published separately")
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
