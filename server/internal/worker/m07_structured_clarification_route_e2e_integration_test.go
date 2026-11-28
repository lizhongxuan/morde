package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

const (
	m07StructuredClarificationClientMessageID = "m07-structured-clarification-001"
	m07StructuredClarificationCallID          = "m07-structured-ask-call-001"
	m07StructuredClarificationArguments       = `{"question":null,"questions":[{"id":"format","prompt":"Which output format should I use?","type":"single_choice","required":true,"allow_skip":false,"options":[{"id":"markdown","label":"Markdown"},{"id":"pdf","label":"PDF"}]}]}`
)

func TestM07RoutedConversationExposesStructuredTaskClarification(t *testing.T) {
	requireT07LocalPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic Task execution: %v", err)
	}
	if _, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: "p1234567", OwnerID: owner.ID, Name: "M07 synthetic route model", BaseURL: "https://models.example.invalid/v1",
		ModelName: "m07-synthetic", APIKeyCipher: bytes.Repeat([]byte{0x37}, 32),
	}); err != nil {
		t.Fatalf("create local synthetic model connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET checked_at=clock_timestamp(),capability_text=true WHERE id=$1`, "p1234567"); err != nil {
		t.Fatalf("mark local synthetic text model as checked: %v", err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify synthetic user session: principal=%+v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	createdConversation, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
		Title: "M07 structured clarification route", IdempotencyKey: "m07-structured-clarification-conversation",
		ServiceInstanceID: serviceInstanceID,
	})
	if err != nil {
		t.Fatalf("create synthetic conversation: %v", err)
	}
	acceptedMessage, replayed, err := tasks.AcceptConversationMessage(ctx, principal, createdConversation.ID,
		app.AcceptConversationMessageCommand{
			ServiceInstanceID: serviceInstanceID, ClientMessageID: m07StructuredClarificationClientMessageID,
			IdempotencyKey: m07StructuredClarificationClientMessageID, Content: "Prepare a short report and ask me which format to use before continuing.",
			ModelID: "p1234567", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 4, ModelTokens: 4000},
		})
	if err != nil || replayed || acceptedMessage.ID == "" {
		t.Fatalf("accept original M07 conversation message: message=%+v replayed=%t err=%v", acceptedMessage, replayed, err)
	}

	routeWorker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{
		Queue: store, Actions: store, Models: m07NewRouteClassifier{}, WorkerID: "m07-structured-route-worker",
		LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create synthetic ConversationRouteWorker: %v", err)
	}
	routeWorkerCtx, stopRouteWorker := context.WithCancel(context.Background())
	routeWorkerDone := make(chan error, 1)
	go func() { routeWorkerDone <- routeWorker.Run(routeWorkerCtx) }()
	defer func() {
		stopRouteWorker()
		select {
		case workerErr := <-routeWorkerDone:
			if !errors.Is(workerErr, context.Canceled) {
				t.Errorf("ConversationRouteWorker stopped with %v; want context cancellation", workerErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("ConversationRouteWorker did not stop")
		}
	}()
	var taskID string
	if err := waitF01(t, ctx, func() (bool, error) {
		message, found, err := store.FindConversationMessageByClientID(ctx, owner.ID, "", createdConversation.ID,
			m07StructuredClarificationClientMessageID, serviceInstanceID)
		if err != nil || !found {
			return false, err
		}
		if message.RoutingStatus == "failed" {
			return false, errors.New("ConversationRouteWorker failed to route the original message")
		}
		if message.RoutingStatus != "routed" || message.TaskID == "" {
			return false, nil
		}
		taskID = message.TaskID
		return true, nil
	}, nil, nil); err != nil {
		t.Fatalf("ConversationRouteWorker did not route the original message as a new Task: %v", err)
	}
	routedMessage, found, err := store.FindConversationMessageByClientID(ctx, owner.ID, "", createdConversation.ID,
		m07StructuredClarificationClientMessageID, serviceInstanceID)
	if err != nil || !found || routedMessage.TaskID == "" || routedMessage.TaskID == acceptedMessage.TaskID || routedMessage.ID != acceptedMessage.ID {
		t.Fatalf("routed original message is not linked to its new Task: message=%+v accepted=%+v found=%t err=%v",
			routedMessage, acceptedMessage, found, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "workspace-blocks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close synthetic Workspace blocks: %v", err)
		}
	}()
	workspaceSync, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	runtimeControl, err := app.NewRuntimeControl(store, f01SandboxRouter{endpoint: "https://unused.invalid"}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	observedBroker := &m06ObservedClarificationBroker{CloudDSHToolBroker: broker}

	node, dshScript := f01NodeRuntimePaths(t)
	const supervisorID = "m07-structured-clarification-supervisor"
	instances, accountRuntimes, _ := newF01ConversationRuntime(t, store, node, dshScript, supervisorID,
		bytes.Repeat([]byte{0x6e}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	syntheticModel := newM06StructuredClarificationModel()
	syntheticModel.register(taskID, owner.ID, m07StructuredClarificationCallID, m07StructuredClarificationArguments, "", "")
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: supervisorID, AccountRuntimes: accountRuntimes, Instances: instances,
		Checkpoints: checkpoints, Models: syntheticModel, Tools: observedBroker, Completion: observedBroker,
	})
	if err != nil {
		t.Fatalf("create local DSH executor: %v", err)
	}
	taskWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "m07-structured-clarification-worker",
		LeaseDuration: 10 * time.Second, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	defer func() {
		stopWorker()
		select {
		case workerErr := <-workerDone:
			if !errors.Is(workerErr, context.Canceled) {
				t.Errorf("TaskWorker stopped with %v; want context cancellation", workerErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()

	waiting := waitM06ClarificationTask(t, ctx, store, taskID, owner.ID, task.StatusWaitingUser)
	if waiting.Task.WaitingReason != "clarification_required" || len(waiting.ClarificationRequests) != 1 {
		t.Fatalf("routed Task did not persist one pending clarification: task=%+v requests=%#v",
			waiting.Task, waiting.ClarificationRequests)
	}
	request := waiting.ClarificationRequests[0]
	if request.Status != task.ClarificationPending || request.Answer != "" || len(request.Questions) != 1 ||
		request.Questions[0].ID != "format" || request.Questions[0].Type != task.ClarificationSingleChoice {
		t.Fatalf("routed Task clarification is not the synthetic structured group: %#v", request)
	}

	apiRouter, err := api.New(api.Deps{
		Hosts: f01UnusedHostService{}, TLSAuthorizer: f01UnusedTLSAuthorizer{}, Verifier: credentials,
		Auth: f01UnusedAuth{}, Tasks: tasks, Conversations: conversations, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "hosts.example.test", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatalf("create local Portal API router: %v", err)
	}
	portal := httptest.NewServer(apiRouter)
	defer portal.Close()
	client := portal.Client()

	taskRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, portal.URL+"/api/tasks/"+taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	taskRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	taskResponse, err := client.Do(taskRequest)
	if err != nil {
		t.Fatalf("GET routed Task: %v", err)
	}
	var taskBody struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		WaitingReason string `json:"waiting_reason"`
		Clarification *struct {
			ID            string `json:"id"`
			Version       int64  `json:"version"`
			Question      string `json:"question"`
			QuestionGroup *struct {
				ID        string `json:"id"`
				Version   int64  `json:"version"`
				Questions []struct {
					ID        string `json:"id"`
					Prompt    string `json:"prompt"`
					Type      string `json:"type"`
					Required  bool   `json:"required"`
					AllowSkip bool   `json:"allow_skip"`
					Options   []struct {
						ID    string `json:"id"`
						Label string `json:"label"`
					} `json:"options"`
				} `json:"questions"`
			} `json:"question_group"`
		} `json:"clarification_request"`
	}
	if err := json.NewDecoder(taskResponse.Body).Decode(&taskBody); err != nil {
		_ = taskResponse.Body.Close()
		t.Fatalf("decode GET Task response: %v", err)
	}
	_ = taskResponse.Body.Close()
	if taskResponse.StatusCode != http.StatusOK || taskBody.ID != taskID || taskBody.Status != string(task.StatusWaitingUser) ||
		taskBody.WaitingReason != "clarification_required" || taskBody.Clarification == nil ||
		taskBody.Clarification.ID != request.ID || taskBody.Clarification.Version != request.Version ||
		taskBody.Clarification.QuestionGroup == nil || taskBody.Clarification.QuestionGroup.ID != request.ID ||
		taskBody.Clarification.QuestionGroup.Version != request.Version || len(taskBody.Clarification.QuestionGroup.Questions) != 1 {
		t.Fatalf("GET Task did not project the pending question_group: status=%d body=%+v",
			taskResponse.StatusCode, taskBody)
	}
	projectedQuestion := taskBody.Clarification.QuestionGroup.Questions[0]
	if projectedQuestion.ID != "format" || projectedQuestion.Type != string(task.ClarificationSingleChoice) ||
		projectedQuestion.Prompt != "Which output format should I use?" || !projectedQuestion.Required || projectedQuestion.AllowSkip ||
		len(projectedQuestion.Options) != 2 || projectedQuestion.Options[0].ID != "markdown" || projectedQuestion.Options[1].ID != "pdf" {
		t.Fatalf("GET Task projected a different structured question: %+v", projectedQuestion)
	}

	messagesRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		portal.URL+"/api/conversations/"+createdConversation.ID+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	messagesRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: sessionCredential})
	messagesResponse, err := client.Do(messagesRequest)
	if err != nil {
		t.Fatalf("GET routed conversation messages: %v", err)
	}
	var messagesBody struct {
		Items []struct {
			ID              string `json:"id"`
			Role            string `json:"role"`
			TaskID          string `json:"task_id"`
			ClientMessageID string `json:"client_message_id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(messagesResponse.Body).Decode(&messagesBody); err != nil {
		_ = messagesResponse.Body.Close()
		t.Fatalf("decode GET conversation messages response: %v", err)
	}
	_ = messagesResponse.Body.Close()
	var projectedOriginalMessage *struct {
		ID              string `json:"id"`
		Role            string `json:"role"`
		TaskID          string `json:"task_id"`
		ClientMessageID string `json:"client_message_id"`
	}
	for index := range messagesBody.Items {
		if messagesBody.Items[index].ClientMessageID == m07StructuredClarificationClientMessageID {
			projectedOriginalMessage = &messagesBody.Items[index]
			break
		}
	}
	if messagesResponse.StatusCode != http.StatusOK || projectedOriginalMessage == nil ||
		projectedOriginalMessage.ID != acceptedMessage.ID || projectedOriginalMessage.Role != "user" || projectedOriginalMessage.TaskID != taskID {
		t.Fatalf("GET conversation messages did not link the original user message to the waiting Task: status=%d items=%+v",
			messagesResponse.StatusCode, messagesBody.Items)
	}
}

type m07NewRouteClassifier struct{}

func (m07NewRouteClassifier) ResolveConversationRouteModel(_ context.Context, claim app.ConversationRouteClaim) (string, error) {
	return claim.Input.ModelID, nil
}

func (m07NewRouteClassifier) ClassifyConversationRoute(ctx context.Context, _ app.ConversationRouteClaim,
	_ app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	if err := ctx.Err(); err != nil {
		return app.ConversationRouteDecision{}, err
	}
	return app.ConversationRouteDecision{Action: app.ConversationRouteNew}, nil
}
