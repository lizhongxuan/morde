package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type taskChatCompatRequest struct {
	hash   []byte
	result TaskSubmissionResult
}

type taskChatLegacyReplay struct {
	hash   []byte
	result LegacyInstantChatReplay
}

type taskChatCompatStoreFake struct {
	TaskStore
	mu                   sync.Mutex
	conversations        map[string]conversation.Conversation
	messages             map[string][]conversation.Message
	tasks                map[string]task.Task
	requests             map[string]taskChatCompatRequest
	legacyRequests       map[string]taskChatLegacyReplay
	submitCalls          int
	appendCalls          int
	acceptedSubmissions  int
	acceptedAppends      int
	listConversationCall int
	listMessageCalls     int
	getTask              func(context.Context, string, string) (TaskDetails, error)
}

func newTaskChatCompatStoreFake() *taskChatCompatStoreFake {
	return &taskChatCompatStoreFake{
		conversations: map[string]conversation.Conversation{}, messages: map[string][]conversation.Message{},
		tasks: map[string]task.Task{}, requests: map[string]taskChatCompatRequest{}, legacyRequests: map[string]taskChatLegacyReplay{},
	}
}

func (s *taskChatCompatStoreFake) GetLegacyInstantChatReplay(_ context.Context, ownerID, key string, requestHash []byte) (LegacyInstantChatReplay, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.legacyRequests[ownerID+"\n"+key]
	if !ok {
		return LegacyInstantChatReplay{}, false, nil
	}
	if !bytes.Equal(prior.hash, requestHash) {
		return LegacyInstantChatReplay{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
	}
	return prior.result, true, nil
}

func (s *taskChatCompatStoreFake) SubmitTask(_ context.Context, submission TaskSubmission) (TaskSubmissionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitCalls++
	if prior, ok := s.requests[compatRequestKey(submission.Idempotency)]; ok {
		if !bytes.Equal(prior.hash, submission.Idempotency.RequestHash) {
			return TaskSubmissionResult{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
		}
		prior.result.Replayed = true
		return prior.result, nil
	}
	if submission.Conversation == nil {
		return TaskSubmissionResult{}, domain.NewError(domain.CodeInvalidArgument, "new conversation is required")
	}
	value := *submission.Conversation
	value.ModelID = submission.Task.ModelID
	value.CreatedAt, value.UpdatedAt = time.Now(), time.Now()
	s.conversations[value.ID] = value
	result := s.acceptSubmissionLocked(submission, value)
	s.requests[compatRequestKey(submission.Idempotency)] = taskChatCompatRequest{hash: append([]byte(nil), submission.Idempotency.RequestHash...), result: result}
	s.acceptedSubmissions++
	return result, nil
}

func (s *taskChatCompatStoreFake) AppendMessage(_ context.Context, submission TaskSubmission) (TaskSubmissionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendCalls++
	if prior, ok := s.requests[compatRequestKey(submission.Idempotency)]; ok {
		if !bytes.Equal(prior.hash, submission.Idempotency.RequestHash) {
			return TaskSubmissionResult{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
		}
		prior.result.Replayed = true
		return prior.result, nil
	}
	value, ok := s.conversations[submission.Task.ConversationID]
	if !ok || value.OwnerID != submission.Task.OwnerID {
		return TaskSubmissionResult{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	value.ModelID, value.UpdatedAt = submission.Task.ModelID, time.Now()
	s.conversations[value.ID] = value
	result := s.acceptSubmissionLocked(submission, value)
	s.requests[compatRequestKey(submission.Idempotency)] = taskChatCompatRequest{hash: append([]byte(nil), submission.Idempotency.RequestHash...), result: result}
	s.acceptedAppends++
	return result, nil
}

func (s *taskChatCompatStoreFake) acceptSubmissionLocked(submission TaskSubmission, value conversation.Conversation) TaskSubmissionResult {
	message := submission.Message
	message.TaskID, message.ModelID, message.Sequence = submission.Task.ID, submission.Task.ModelID, int64(len(s.messages[value.ID])+1)
	message.CreatedAt = time.Now()
	taskValue := submission.Task
	s.tasks[taskValue.ID] = taskValue
	s.messages[value.ID] = append(s.messages[value.ID], message)
	return TaskSubmissionResult{Conversation: value, Message: message, Task: taskValue}
}

func (s *taskChatCompatStoreFake) GetTask(ctx context.Context, id, ownerID string) (TaskDetails, error) {
	if s.getTask != nil {
		return s.getTask(ctx, id, ownerID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok || value.OwnerID != ownerID {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	return TaskDetails{Task: value}, nil
}

func (s *taskChatCompatStoreFake) GetConversation(_ context.Context, id, ownerID string) (conversation.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.conversations[id]
	if !ok || value.OwnerID != ownerID {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return value, nil
}

func (s *taskChatCompatStoreFake) ListConversations(_ context.Context, ownerID string, _ []string, page PageRequest) (ConversationPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listConversationCall++
	values := make([]conversation.Conversation, 0, len(s.conversations))
	for _, value := range s.conversations {
		if value.OwnerID == ownerID {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].CreatedAt.After(values[j].CreatedAt)
	})
	start, err := compatPageOffset(page.Cursor)
	if err != nil {
		return ConversationPage{}, err
	}
	if start >= len(values) {
		return ConversationPage{Items: []conversation.Conversation{}}, nil
	}
	end := start + int(page.Limit)
	if end > len(values) {
		end = len(values)
	}
	result := ConversationPage{Items: append([]conversation.Conversation(nil), values[start:end]...)}
	if end < len(values) {
		result.NextCursor = strconv.Itoa(end)
	}
	return result, nil
}

func (s *taskChatCompatStoreFake) ListMessages(_ context.Context, ownerID, conversationID string, page PageRequest) (MessagePage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listMessageCalls++
	conversationValue, ok := s.conversations[conversationID]
	if !ok || conversationValue.OwnerID != ownerID {
		return MessagePage{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	values := append([]conversation.Message(nil), s.messages[conversationID]...)
	sort.Slice(values, func(i, j int) bool { return values[i].Sequence > values[j].Sequence })
	start, err := compatPageOffset(page.Cursor)
	if err != nil {
		return MessagePage{}, err
	}
	if start >= len(values) {
		return MessagePage{Items: []conversation.Message{}}, nil
	}
	end := start + int(page.Limit)
	if end > len(values) {
		end = len(values)
	}
	result := MessagePage{Items: append([]conversation.Message(nil), values[start:end]...)}
	if end < len(values) {
		result.NextCursor = strconv.Itoa(end)
	}
	return result, nil
}

func compatRequestKey(record IdempotencyRecord) string {
	return record.OwnerID + "\n" + record.Operation + "\n" + record.Key
}

func compatPageOffset(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	return strconv.Atoi(cursor)
}

func newTaskChatCompatService(t *testing.T, store *taskChatCompatStoreFake) *TaskChatCompatibilityService {
	t.Helper()
	tasks, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := NewConversationService(store, &taskAuthorizerStub{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewTaskChatCompatibilityService(tasks, conversations)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (s *taskChatCompatStoreFake) completeTask(taskID, answer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.tasks[taskID]
	value.Status = task.StatusCompleted
	s.tasks[taskID] = value
	messageID, _ := domain.NewMessageID()
	s.messages[value.ConversationID] = append(s.messages[value.ConversationID], conversation.Message{
		ID: messageID, ConversationID: value.ConversationID, OwnerID: value.OwnerID,
		Role: conversation.RoleAssistant, Content: answer, TaskID: value.ID, ModelID: value.ModelID,
		Sequence: int64(len(s.messages[value.ConversationID]) + 1), CreatedAt: time.Now(),
	})
}

func TestTaskChatCompatibilityUsesCanonicalTaskAndMessages(t *testing.T) {
	store := newTaskChatCompatStoreFake()
	store.getTask = func(ctx context.Context, taskID, ownerID string) (TaskDetails, error) {
		if err := ctx.Err(); err != nil {
			return TaskDetails{}, err
		}
		store.mu.Lock()
		value := store.tasks[taskID]
		store.mu.Unlock()
		if value.OwnerID != ownerID {
			return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
		}
		if value.Status != task.StatusCompleted {
			store.completeTask(taskID, "canonical answer")
		}
		store.mu.Lock()
		value = store.tasks[taskID]
		store.mu.Unlock()
		return TaskDetails{Task: value}, nil
	}
	service := newTaskChatCompatService(t, store)
	command := InstantChatSend{ModelID: "p1234567", Content: "  hello  ", RequestKey: "compat-request-001"}
	result, err := service.Send(context.Background(), taskTestPrincipal(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Role != "user" || result.Message.Content != "hello" || result.Reply.Role != "assistant" || result.Reply.Content != "canonical answer" {
		t.Fatalf("unexpected compatibility reply: %+v", result)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.submitCalls != 1 || store.appendCalls != 0 || store.acceptedSubmissions != 1 || len(store.tasks) != 1 {
		t.Fatalf("new chat did not use one canonical task submission: submits=%d appends=%d accepted=%d tasks=%d", store.submitCalls, store.appendCalls, store.acceptedSubmissions, len(store.tasks))
	}
	messages := store.messages[result.Conversation.ID]
	if len(messages) != 2 || messages[0].ID != result.Message.ID || messages[1].ID != result.Reply.ID || messages[1].TaskID != messages[0].TaskID {
		t.Fatalf("compatibility response did not reference canonical conversation messages: %+v", messages)
	}
}

func TestTaskChatCompatibilityReplaysMigratedLegacyExchangeWithoutCreatingTask(t *testing.T) {
	store := newTaskChatCompatStoreFake()
	now := time.Now().UTC()
	conversationValue := conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", ModelID: "p1234567", Title: "original title", CreatedAt: now, UpdatedAt: now}
	userMessage := conversation.Message{ID: "m1234567", ConversationID: conversationValue.ID, OwnerID: conversationValue.OwnerID,
		Role: conversation.RoleUser, Content: "old question", ModelID: conversationValue.ModelID, Sequence: 1,
		SourceKind: "legacy_instant", SourceExchangeID: "m7654321", RequestedConversationID: "", CreatedAt: now}
	assistantMessage := conversation.Message{ID: "m7654322", ConversationID: conversationValue.ID, OwnerID: conversationValue.OwnerID,
		Role: conversation.RoleAssistant, Content: "old answer", ModelID: conversationValue.ModelID, Sequence: 2,
		SourceKind: "legacy_instant", SourceExchangeID: "m7654321", RequestedConversationID: "", CreatedAt: now}
	legacyHash := sha256.Sum256([]byte("old question\n" + conversationValue.ModelID + "\n"))
	store.conversations[conversationValue.ID] = conversationValue
	store.messages[conversationValue.ID] = []conversation.Message{userMessage, assistantMessage}
	store.legacyRequests["u1234567\nlegacy-key-001"] = taskChatLegacyReplay{
		hash: legacyHash[:], result: LegacyInstantChatReplay{Conversation: conversationValue, UserMessage: userMessage, AssistantMessage: assistantMessage},
	}
	service := newTaskChatCompatService(t, store)
	command := InstantChatSend{ModelID: conversationValue.ModelID, Content: " old question ", RequestKey: "legacy-key-001"}
	first, err := service.Send(context.Background(), taskTestPrincipal(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Send(context.Background(), taskTestPrincipal(), command)
	if err != nil || first.Conversation.ID != conversationValue.ID || first.Message.ID != userMessage.ID || first.Reply.ID != assistantMessage.ID ||
		second.Message.ID != first.Message.ID || second.Reply.ID != first.Reply.ID || second.Reply.Content != "old answer" {
		t.Fatalf("legacy exchange replay = first %#v, second %#v, err=%v", first, second, err)
	}
	if _, err := service.Send(context.Background(), taskTestPrincipal(), InstantChatSend{
		ModelID: conversationValue.ModelID, Content: "different question", RequestKey: "legacy-key-001",
	}); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("legacy key with changed request error = %v, want idempotency conflict", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.submitCalls != 0 || store.appendCalls != 0 || store.acceptedSubmissions != 0 || store.acceptedAppends != 0 || len(store.tasks) != 0 ||
		len(store.messages[conversationValue.ID]) != 2 {
		t.Fatalf("legacy replay created canonical work: submits=%d appends=%d tasks=%d messages=%d", store.submitCalls, store.appendCalls,
			len(store.tasks), len(store.messages[conversationValue.ID]))
	}
}

func TestTaskChatCompatibilityContinuesAndRetriesIdempotently(t *testing.T) {
	store := newTaskChatCompatStoreFake()
	conversationValue := conversation.Conversation{ID: "c1234567", OwnerID: "u1234567", ModelID: "p1234567", Title: "previous", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	store.conversations[conversationValue.ID] = conversationValue
	store.getTask = func(ctx context.Context, taskID, ownerID string) (TaskDetails, error) {
		if err := ctx.Err(); err != nil {
			return TaskDetails{}, err
		}
		store.mu.Lock()
		value := store.tasks[taskID]
		store.mu.Unlock()
		if value.OwnerID != ownerID {
			return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
		}
		if value.Status != task.StatusCompleted {
			store.completeTask(taskID, "continued answer")
		}
		store.mu.Lock()
		value = store.tasks[taskID]
		store.mu.Unlock()
		return TaskDetails{Task: value}, nil
	}
	service := newTaskChatCompatService(t, store)
	command := InstantChatSend{ConversationID: conversationValue.ID, ModelID: "p7654321", Content: "continue", RequestKey: "compat-append-001"}
	first, err := service.Send(context.Background(), taskTestPrincipal(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Send(context.Background(), taskTestPrincipal(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Message.ID != second.Message.ID || first.Reply.ID != second.Reply.ID || first.Reply.Content != "continued answer" {
		t.Fatalf("idempotent retry returned different canonical messages: first=%+v second=%+v", first, second)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.submitCalls != 0 || store.appendCalls != 2 || store.acceptedAppends != 1 || len(store.tasks) != 1 || len(store.messages[conversationValue.ID]) != 2 {
		t.Fatalf("continuation retry created an extra execution or message: submit=%d append=%d accepted=%d tasks=%d messages=%d", store.submitCalls, store.appendCalls, store.acceptedAppends, len(store.tasks), len(store.messages[conversationValue.ID]))
	}
	for _, taskValue := range store.tasks {
		if taskValue.ModelID != command.ModelID || store.conversations[conversationValue.ID].ModelID != command.ModelID {
			t.Fatalf("selected model was not preserved: task=%+v conversation=%+v", taskValue, store.conversations[conversationValue.ID])
		}
	}
}

func TestTaskChatCompatibilityCancellationStopsWaitingOnly(t *testing.T) {
	store := newTaskChatCompatStoreFake()
	getStarted := make(chan struct{})
	store.getTask = func(ctx context.Context, _, _ string) (TaskDetails, error) {
		close(getStarted)
		<-ctx.Done()
		return TaskDetails{}, ctx.Err()
	}
	service := newTaskChatCompatService(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := service.Send(ctx, taskTestPrincipal(), InstantChatSend{
			ModelID: "p1234567", Content: "keep running", RequestKey: "compat-cancel-001",
		})
		result <- err
	}()
	<-getStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request should stop waiting, got %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.tasks) != 1 || len(store.messages) != 1 {
		t.Fatalf("request cancellation removed the submitted task or message: tasks=%d conversations=%d", len(store.tasks), len(store.messages))
	}
	for _, value := range store.tasks {
		if value.Status != task.StatusQueued {
			t.Fatalf("request cancellation changed task status: %q", value.Status)
		}
	}
}

func TestTaskChatCompatibilityReadsAllConversationPages(t *testing.T) {
	store := newTaskChatCompatStoreFake()
	for i := 1; i <= 105; i++ {
		id := fmt.Sprintf("c%07d", i)
		store.conversations[id] = conversation.Conversation{
			ID: id, OwnerID: "u1234567", Title: id, CreatedAt: time.Unix(int64(i), 0), UpdatedAt: time.Unix(int64(106-i), 0),
		}
	}
	conversationID := "c0000001"
	for i := 1; i <= 105; i++ {
		store.messages[conversationID] = append(store.messages[conversationID], conversation.Message{
			ID: fmt.Sprintf("m%07d", i), ConversationID: conversationID, OwnerID: "u1234567", Role: conversation.RoleUser,
			Content: strconv.Itoa(i), Sequence: int64(i), CreatedAt: time.Unix(int64(i), 0),
		})
	}
	service := newTaskChatCompatService(t, store)
	listed, err := service.List(context.Background(), taskTestPrincipal())
	if err != nil || len(listed) != 105 || listed[0].ID != "c0000001" || listed[len(listed)-1].ID != "c0000105" {
		t.Fatalf("List() = %d conversations, %v; want all 105", len(listed), err)
	}
	messages, err := service.Messages(context.Background(), taskTestPrincipal(), conversationID)
	if err != nil || len(messages) != 105 || messages[0].Content != "1" || messages[len(messages)-1].Content != "105" {
		t.Fatalf("Messages() = %d messages, %v; want chronological complete history", len(messages), err)
	}
	for i, message := range messages {
		if message.Content != strconv.Itoa(i+1) {
			t.Fatalf("Messages()[%d] = %q, want chronological message %d", i, message.Content, i+1)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.listConversationCall != 2 || store.listMessageCalls != 2 {
		t.Fatalf("compatibility reads did not paginate unified conversation storage: conversation pages=%d message pages=%d", store.listConversationCall, store.listMessageCalls)
	}
}

func TestTaskChatCompatibilityErrorsOnUnsupportedTaskStates(t *testing.T) {
	for _, status := range []task.Status{task.StatusWaitingUser, task.StatusPaused, task.StatusFailed, task.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			store := newTaskChatCompatStoreFake()
			store.getTask = func(ctx context.Context, taskID, ownerID string) (TaskDetails, error) {
				if err := ctx.Err(); err != nil {
					return TaskDetails{}, err
				}
				store.mu.Lock()
				defer store.mu.Unlock()
				value := store.tasks[taskID]
				value.Status = status
				store.tasks[taskID] = value
				if value.OwnerID != ownerID {
					return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
				}
				return TaskDetails{Task: value}, nil
			}
			service := newTaskChatCompatService(t, store)
			_, err := service.Send(context.Background(), taskTestPrincipal(), InstantChatSend{
				ModelID: "p1234567", Content: "unsupported completion", RequestKey: "compat-state-001",
			})
			if domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("Send() with task status %q error = %v", status, err)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			for _, value := range store.tasks {
				if value.Status != status {
					t.Fatalf("compatibility error changed task status: got %q want %q", value.Status, status)
				}
			}
		})
	}
}
