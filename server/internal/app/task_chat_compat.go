package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const taskChatCompatibilityPollInterval = 250 * time.Millisecond

// TaskChatCompatibilityService keeps the legacy instant-chat contract on the
// canonical Task and Conversation services while the old route is being retired.
type TaskChatCompatibilityService struct {
	tasks         *TaskService
	conversations *ConversationService
	legacyReplay  LegacyInstantChatReplayStore
}

// LegacyInstantChatReplayStore reads only idempotency rows copied from the
// retired instant-chat API. It lets retries return the original exchange
// without creating a new canonical Task.
type LegacyInstantChatReplayStore interface {
	GetLegacyInstantChatReplay(context.Context, string, string, []byte) (LegacyInstantChatReplay, bool, error)
}

type LegacyInstantChatReplay struct {
	Conversation     conversation.Conversation
	UserMessage      conversation.Message
	AssistantMessage conversation.Message
}

func NewTaskChatCompatibilityService(tasks *TaskService, conversations *ConversationService) (*TaskChatCompatibilityService, error) {
	if tasks == nil || conversations == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "task chat compatibility dependencies are required")
	}
	legacyReplay, ok := tasks.store.(LegacyInstantChatReplayStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInvalidArgument, "legacy instant-chat replay storage is required")
	}
	return &TaskChatCompatibilityService{tasks: tasks, conversations: conversations, legacyReplay: legacyReplay}, nil
}

func (s *TaskChatCompatibilityService) List(ctx context.Context, principal identity.Principal) ([]InstantChatConversation, error) {
	var result []InstantChatConversation
	cursor := ""
	seen := map[string]struct{}{}
	for {
		page, err := s.conversations.List(ctx, principal, PageRequest{Limit: maxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, value := range page.Items {
			result = append(result, instantChatConversationFromCanonical(value))
		}
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			return nil, domain.NewError(domain.CodeInternal, "conversation pagination cursor repeated")
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *TaskChatCompatibilityService) Messages(ctx context.Context, principal identity.Principal, conversationID string) ([]InstantChatMessage, error) {
	var pages [][]conversation.Message
	cursor := ""
	seen := map[string]struct{}{}
	for {
		page, err := s.conversations.ListMessages(ctx, principal, conversationID, PageRequest{Limit: maxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		pages = append(pages, page.Items)
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			return nil, domain.NewError(domain.CodeInternal, "message pagination cursor repeated")
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}

	result := make([]InstantChatMessage, 0)
	for pageIndex := len(pages) - 1; pageIndex >= 0; pageIndex-- {
		for _, value := range pages[pageIndex] {
			result = append(result, instantChatMessageFromCanonical(value))
		}
	}
	return result, nil
}

func (s *TaskChatCompatibilityService) Send(ctx context.Context, principal identity.Principal, command InstantChatSend) (InstantChatReply, error) {
	if err := validateInstantChatPrincipal(principal); err != nil {
		return InstantChatReply{}, err
	}
	content := strings.TrimSpace(command.Content)
	if content == "" || utf8.RuneCountInString(content) > 32768 {
		return InstantChatReply{}, domain.InvalidArgument("content", "content must be 1 to 32768 characters")
	}
	if !domain.ValidModelConnectionID(command.ModelID) {
		return InstantChatReply{}, domain.InvalidArgument("model_id", "invalid model connection")
	}
	if command.ConversationID != "" && !domain.ValidConversationID(command.ConversationID) {
		return InstantChatReply{}, domain.InvalidArgument("conversation_id", "invalid conversation")
	}
	if len(command.RequestKey) < 8 || len(command.RequestKey) > 128 || !validInstantChatKey(command.RequestKey) {
		return InstantChatReply{}, domain.InvalidArgument("Idempotency-Key", "invalid idempotency key")
	}
	legacyHash := sha256.Sum256([]byte(content + "\n" + command.ModelID + "\n" + command.ConversationID))
	legacy, found, err := s.legacyReplay.GetLegacyInstantChatReplay(ctx, principal.UserID, command.RequestKey, legacyHash[:])
	if err != nil {
		return InstantChatReply{}, err
	}
	if found {
		return InstantChatReply{
			Conversation: instantChatConversationFromCanonical(legacy.Conversation),
			Message:      instantChatMessageFromCanonical(legacy.UserMessage),
			Reply:        instantChatMessageFromCanonical(legacy.AssistantMessage),
		}, nil
	}

	var submitted TaskSubmissionResult
	if command.ConversationID == "" {
		submitted, err = s.tasks.Submit(ctx, principal, SubmitTaskCommand{
			ModelID: command.ModelID, Objective: content, IdempotencyKey: command.RequestKey,
		})
	} else {
		submitted, err = s.tasks.AppendMessage(ctx, principal, command.ConversationID, AppendMessageCommand{
			ModelID: command.ModelID, Content: content, IdempotencyKey: command.RequestKey,
		})
	}
	if err != nil {
		return InstantChatReply{}, err
	}

	completed, err := s.waitForTask(ctx, principal, submitted.Task.ID)
	if err != nil {
		return InstantChatReply{}, err
	}
	reply, err := s.assistantReply(ctx, principal, completed.Task.ConversationID, completed.Task.ID)
	if err != nil {
		return InstantChatReply{}, err
	}
	return InstantChatReply{
		Conversation: instantChatConversationFromCanonical(submitted.Conversation),
		Message:      instantChatMessageFromCanonical(submitted.Message),
		Reply:        instantChatMessageFromCanonical(reply),
	}, nil
}

func (s *TaskChatCompatibilityService) waitForTask(ctx context.Context, principal identity.Principal, taskID string) (TaskDetails, error) {
	ticker := time.NewTicker(taskChatCompatibilityPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return TaskDetails{}, err
		}
		details, err := s.tasks.Get(ctx, principal, taskID)
		if err != nil {
			return TaskDetails{}, err
		}
		switch details.Task.Status {
		case task.StatusCompleted:
			return details, nil
		case task.StatusWaitingUser, task.StatusPaused, task.StatusFailed, task.StatusCancelled:
			return TaskDetails{}, taskChatCompatibilityStatusError(details.Task.Status)
		case task.StatusQueued, task.StatusRunning, task.StatusPausing, task.StatusCancelling:
			select {
			case <-ctx.Done():
				return TaskDetails{}, ctx.Err()
			case <-ticker.C:
			}
		default:
			return TaskDetails{}, taskChatCompatibilityStatusError(details.Task.Status)
		}
	}
}

func (s *TaskChatCompatibilityService) assistantReply(ctx context.Context, principal identity.Principal, conversationID, taskID string) (conversation.Message, error) {
	var result *conversation.Message
	cursor := ""
	seen := map[string]struct{}{}
	for {
		page, err := s.conversations.ListMessages(ctx, principal, conversationID, PageRequest{Limit: maxPageSize, Cursor: cursor})
		if err != nil {
			return conversation.Message{}, err
		}
		for i := range page.Items {
			message := &page.Items[i]
			if message.Role == conversation.RoleAssistant && message.TaskID == taskID &&
				(result == nil || message.Sequence > result.Sequence) {
				copy := *message
				result = &copy
			}
		}
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			return conversation.Message{}, domain.NewError(domain.CodeInternal, "message pagination cursor repeated")
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	if result == nil {
		return conversation.Message{}, taskChatCompatibilityStatusError(task.StatusCompleted)
	}
	return *result, nil
}

func instantChatConversationFromCanonical(value conversation.Conversation) InstantChatConversation {
	return InstantChatConversation{
		ID: value.ID, OwnerID: value.OwnerID, ModelID: value.ModelID, Title: value.Title,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func instantChatMessageFromCanonical(value conversation.Message) InstantChatMessage {
	return InstantChatMessage{
		ID: value.ID, ConversationID: value.ConversationID, Role: string(value.Role),
		Content: value.Content, CreatedAt: value.CreatedAt,
	}
}

func taskChatCompatibilityStatusError(status task.Status) *domain.Error {
	err := domain.NewError(domain.CodeInvalidState, fmt.Sprintf("instant chat cannot return a task response for status %q", status))
	err.Details["status"] = string(status)
	return err
}
