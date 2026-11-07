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
	tasks, err := app.NewTaskService(store, credentials, models)
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
	execution, err := app.NewExecutionControlService(store, audit)
	if err != nil {
		t.Fatal(err)
	}
	var activity *browserAcceptanceActivity
	var executor worker.TaskExecutor = browserAcceptanceExecutor{store: store}
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
	resultWorker, err := worker.NewConversationResultAggregationWorker(store, 100*time.Millisecond, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workerDone := make(chan error, 3)
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	go func() { workerDone <- routerWorker.Run(workerCtx) }()
	go func() { workerDone <- resultWorker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorkers()
		for i := 0; i < 3; i++ {
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
	router, err := api.New(api.Deps{Hosts: nativeAcceptanceHosts{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, ClientLogin: clients, Tasks: tasks, Conversations: conversations,
		ModelConnections: models, ModelGateway: gateway, ExecutionControl: execution, MCPDisabled: true,
	}, api.Options{Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), ServiceInstanceID: instanceID,
		APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20, SessionTTL: 12 * time.Hour,
		HostScheme: "http", PortalDomain: listener.Addr().String(), HostSuffix: "hosts.invalid", PreviewPortMin: 1024,
		PortalFiles: os.DirFS(portalRoot)})
	if err != nil {
		t.Fatal(err)
	}
	// The production capability remains disabled until all acceptance checks
	// pass. This disposable fixture advertises it only to exercise the UI.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if activity != nil && strings.HasPrefix(r.URL.Path, "/__morde_fixture/") {
			activity.serveControl(w, r)
			return
		}
		if activity != nil && strings.HasPrefix(r.URL.Path, "/api/tasks/") && strings.HasSuffix(r.URL.Path, "/events") {
			activity.serveTaskEvents(w, r, router)
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
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
	if activity != nil {
		t.Logf("LOCAL_ACTIVITY_TOKEN=%s", activity.controlToken)
	}
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(stop); err == nil {
				var messages, replies int
				if err := store.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE role='user'),count(*) FILTER (WHERE role='assistant') FROM conversation_messages WHERE owner_id=$1`, owner.ID).Scan(&messages, &replies); err != nil || messages < 2 || replies < 1 {
					t.Fatalf("browser messages=%d replies=%d err=%v", messages, replies, err)
				}
				if activity != nil {
					if err := activity.assertVerified(); err != nil {
						t.Fatal(err)
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
// Its control API is loopback-only and protected by a fresh per-run token.
// No file or user-provided content is needed by any of these scenarios.
// With MORDE_BROWSER_ACTIVITY=1, GET /__morde_fixture/status lists the fixed
// inputs and stages. POST /__morde_fixture/advance/{name}/{start|progress|
// stream|complete} releases one stage, without a request body. Complete
// merge-a then merge-b within the aggregation window; for separate-a and
// separate-b wait until the first answer is published before completing the
// second. Start stream before submitting stream-other, then disconnect both
// clients using POST /__morde_fixture/disconnect/stream before completing the
// pair. GET /__morde_fixture/verify/{merge|separate|reconnect|stream} checks
// persistent identities, sources, result ordering and reconnect cursors. The
// fixture does not pass until all four checks succeed; visual deduplication
// still needs to be checked in both clients after refresh/relaunch.
var browserActivityObjectives = map[string]string{
	"merge-a":      "跨端活动验收：合并 A",
	"merge-b":      "跨端活动验收：合并 B",
	"separate-a":   "跨端活动验收：分开 A",
	"separate-b":   "跨端活动验收：分开 B",
	"stream":       "跨端活动验收：流式",
	"stream-other": "跨端活动验收：流式后的独立任务",
}

const browserActivityStreamPrefix = "跨端活动验收：流式回复第一段"
const browserActivityStreamSuffix = "，第二段完成。"

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

type browserActivityWatch struct {
	taskID string
	client string
	cancel context.CancelFunc
}

type browserAcceptanceActivity struct {
	store          *Store
	instances      *app.AgentInstanceService
	checkpoints    *app.AgentCheckpointService
	conversationID string
	controlToken   string
	mu             sync.Mutex
	runs           map[string]*browserActivityRun
	verified       map[string]bool
	watches        map[uint64]browserActivityWatch
	requests       []browserActivityEventRequest
	nextWatch      uint64
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
	a := &browserAcceptanceActivity{store: store, instances: instances, checkpoints: checkpoints,
		conversationID: conversationID, controlToken: hex.EncodeToString(token),
		runs: make(map[string]*browserActivityRun), verified: make(map[string]bool), watches: make(map[uint64]browserActivityWatch)}
	for name, objective := range browserActivityObjectives {
		a.runs[name] = &browserActivityRun{name: name, objective: objective, stage: "awaiting_message",
			commands: make(chan browserActivityCommand)}
	}
	return a, nil
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
	nonce[0] = 1
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
		Title: "正在整理测试信息", Summary: "正在处理合成文字。"}
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
	turnID, result := "", "本地验收结果："+run.objective
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
			Title: "正在整理测试信息", Summary: "合成文字处理完成。"},
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
		if run.progressStepID != "" && step.ID == run.progressStepID && step.Title == "正在整理测试信息" {
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
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": runs, "active_event_streams": active, "event_requests": requests})
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
	for _, scenario := range []string{"merge", "separate", "stream", "reconnect"} {
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
		go func() { <-command.reply; clearBusy() }()
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
						source.Content != "本地验收结果："+other.Objective {
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
					source.EventSequence != run.ResultEventSequence || source.Content != "本地验收结果："+run.Objective {
					return errors.New("published reply source does not match the original task result")
				}
				matched[run.TaskID] = message
			}
		}
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
