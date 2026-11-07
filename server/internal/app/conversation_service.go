package app

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

// CreateConversationCommand creates a conversation independently of any later task.
type CreateConversationCommand struct {
	HostID            string
	Title             string
	IdempotencyKey    string
	ServiceInstanceID string
}

type ConversationService struct {
	store      TaskStore
	mainStore  MainConversationStore
	authorizer TaskAuthorizer
}

type MainConversationStore interface {
	GetMainConversation(context.Context, string) (conversation.MainConversation, error)
	EnsureMainConversation(context.Context, conversation.Conversation, IdempotencyRecord) (conversation.MainConversation, bool, error)
	SetMainConversation(context.Context, string, string, int64, IdempotencyRecord) (conversation.MainConversation, bool, error)
}

type EnsureMainConversationCommand struct {
	IdempotencyKey string
}

type SetMainConversationCommand struct {
	ConversationID  string
	ExpectedVersion int64
	IdempotencyKey  string
}

func NewConversationService(store TaskStore, authorizer TaskAuthorizer) (*ConversationService, error) {
	if store == nil || authorizer == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "conversation dependencies are required")
	}
	mainStore, _ := store.(MainConversationStore)
	return &ConversationService{store: store, mainStore: mainStore, authorizer: authorizer}, nil
}

func (s *ConversationService) Create(ctx context.Context, principal identity.Principal, command CreateConversationCommand) (conversation.Conversation, bool, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return conversation.Conversation{}, false, err
	}
	command.Title = strings.TrimSpace(command.Title)
	if !utf8.ValidString(command.Title) || utf8.RuneCountInString(command.Title) > 120 {
		return conversation.Conversation{}, false, domain.InvalidArgument("title", "title is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return conversation.Conversation{}, false, err
	}
	if command.HostID != "" {
		if _, err := authorizeTaskHost(ctx, s.authorizer, principal, command.HostID, identity.ScopeTaskSubmit); err != nil {
			return conversation.Conversation{}, false, err
		}
	}
	id, err := domain.NewConversationID()
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "conversation.create", command.IdempotencyKey, struct {
		HostID string `json:"host_id"`
		Title  string `json:"title"`
	}{HostID: command.HostID, Title: command.Title}, id, "")
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	value := conversation.Conversation{
		ID: id, OwnerID: principal.UserID, HostID: command.HostID, Title: command.Title,
	}
	if command.ServiceInstanceID != "" {
		if !domain.ValidServiceInstanceID(command.ServiceInstanceID) {
			return conversation.Conversation{}, false, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
		}
		store, ok := s.store.(interface {
			CreateConversationForServiceInstance(context.Context, conversation.Conversation, IdempotencyRecord, string) (conversation.Conversation, bool, error)
		})
		if !ok {
			return conversation.Conversation{}, false, domain.NewError(domain.CodeUnsupported, "scoped conversation creation is unavailable")
		}
		return store.CreateConversationForServiceInstance(ctx, value, record, command.ServiceInstanceID)
	}
	return s.store.CreateConversation(ctx, value, record)
}

func (s *ConversationService) List(ctx context.Context, principal identity.Principal, page PageRequest) (ConversationPage, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskRead); err != nil {
		return ConversationPage{}, err
	}
	if err := validatePageRequest(page); err != nil {
		return ConversationPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	return s.store.ListConversations(ctx, principal.UserID, nil, page)
}

func (s *ConversationService) Get(ctx context.Context, principal identity.Principal, id string) (conversation.Conversation, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskRead); err != nil {
		return conversation.Conversation{}, err
	}
	if !domain.ValidConversationID(id) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return s.store.GetConversation(ctx, id, principal.UserID)
}

func (s *ConversationService) GetMain(ctx context.Context, principal identity.Principal) (conversation.MainConversation, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskRead); err != nil {
		return conversation.MainConversation{}, err
	}
	if s.mainStore == nil {
		return conversation.MainConversation{}, domain.NewError(domain.CodeUnsupported, "main conversation storage is unavailable")
	}
	return validateMainConversationResult(s.mainStore.GetMainConversation(ctx, principal.UserID))
}

func (s *ConversationService) EnsureMain(ctx context.Context, principal identity.Principal, command EnsureMainConversationCommand) (conversation.MainConversation, bool, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return conversation.MainConversation{}, false, err
	}
	if s.mainStore == nil {
		return conversation.MainConversation{}, false, domain.NewError(domain.CodeUnsupported, "main conversation storage is unavailable")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return conversation.MainConversation{}, false, err
	}
	id, err := domain.NewConversationID()
	if err != nil {
		return conversation.MainConversation{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "main_conversation.ensure", command.IdempotencyKey, struct{}{}, id, "")
	if err != nil {
		return conversation.MainConversation{}, false, err
	}
	value, replayed, err := s.mainStore.EnsureMainConversation(ctx, conversation.Conversation{ID: id, OwnerID: principal.UserID}, record)
	value, err = validateMainConversationResult(value, err)
	return value, replayed, err
}

func (s *ConversationService) SetMain(ctx context.Context, principal identity.Principal, command SetMainConversationCommand) (conversation.MainConversation, bool, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return conversation.MainConversation{}, false, err
	}
	if s.mainStore == nil {
		return conversation.MainConversation{}, false, domain.NewError(domain.CodeUnsupported, "main conversation storage is unavailable")
	}
	if !domain.ValidConversationID(command.ConversationID) || command.ExpectedVersion < 0 {
		return conversation.MainConversation{}, false, domain.InvalidArgument("main_conversation", "main conversation selection is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return conversation.MainConversation{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "main_conversation.set", command.IdempotencyKey, struct {
		ConversationID  string `json:"conversation_id"`
		ExpectedVersion int64  `json:"expected_version"`
	}{ConversationID: command.ConversationID, ExpectedVersion: command.ExpectedVersion}, command.ConversationID, "")
	if err != nil {
		return conversation.MainConversation{}, false, err
	}
	value, replayed, err := s.mainStore.SetMainConversation(ctx, principal.UserID, command.ConversationID, command.ExpectedVersion, record)
	value, err = validateMainConversationResult(value, err)
	return value, replayed, err
}

func validateMainConversationResult(value conversation.MainConversation, err error) (conversation.MainConversation, error) {
	if err != nil {
		return conversation.MainConversation{}, err
	}
	if err := value.Validate(); err != nil {
		return conversation.MainConversation{}, domain.NewError(domain.CodeInternal, "main conversation state is invalid")
	}
	return value, nil
}

func (s *ConversationService) ListMessages(ctx context.Context, principal identity.Principal, conversationID string, page PageRequest) (MessagePage, error) {
	if err := validatePageRequest(page); err != nil {
		return MessagePage{}, err
	}
	if _, err := s.Get(ctx, principal, conversationID); err != nil {
		return MessagePage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	result, err := s.store.ListMessages(ctx, principal.UserID, conversationID, page)
	if err != nil {
		return MessagePage{}, err
	}
	for left, right := 0, len(result.Items)-1; left < right; left, right = left+1, right-1 {
		result.Items[left], result.Items[right] = result.Items[right], result.Items[left]
	}
	return result, nil
}

type ConversationMessageReceiptLookup interface {
	FindConversationMessageByClientID(context.Context, string, string, string, string, string) (conversation.Message, bool, error)
}

func (s *ConversationService) FindMessageByClientID(ctx context.Context, principal identity.Principal, conversationID, clientID, instanceID string) (MessagePage, error) {
	if _, err := s.Get(ctx, principal, conversationID); err != nil {
		return MessagePage{}, err
	}
	return findConversationMessageByClientID(ctx, s.store, principal.UserID, "", conversationID, clientID, instanceID)
}

func findConversationMessageByClientID(ctx context.Context, store any, actorID, projectID, conversationID, clientID, instanceID string) (MessagePage, error) {
	if !idempotencyKeyPattern.MatchString(clientID) {
		return MessagePage{}, domain.InvalidArgument("client_message_id", "client message id is invalid")
	}
	lookup, ok := store.(ConversationMessageReceiptLookup)
	if !ok {
		return MessagePage{}, domain.NewError(domain.CodeUnsupported, "conversation receipt lookup is unavailable")
	}
	if !domain.ValidServiceInstanceID(instanceID) {
		return MessagePage{}, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
	}
	message, found, err := lookup.FindConversationMessageByClientID(ctx, actorID, projectID, conversationID, clientID, instanceID)
	if err != nil {
		return MessagePage{}, err
	}
	items := make([]conversation.Message, 0, 1)
	if found {
		items = append(items, message)
	}
	return MessagePage{Items: items}, nil
}

func validateConversationPrincipal(principal identity.Principal, scope identity.Scope) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTokenID(principal.TokenID) || !principal.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "conversation access is not authorized")
	}
	return nil
}

// ProjectConversationStore provides project-scoped queries and writes. The
// adapter must recheck current membership in the query or transaction itself;
// the app-layer check is an early gate, not the persistence authorization.
type ProjectConversationStore interface {
	CreateProjectConversation(context.Context, string, conversation.Conversation, IdempotencyRecord) (conversation.Conversation, bool, error)
	GetProjectConversation(context.Context, string, string, string) (conversation.Conversation, error)
	ListProjectConversations(context.Context, string, string, PageRequest) (ConversationPage, error)
	ListProjectConversationMessages(context.Context, string, string, string, PageRequest) (MessagePage, error)
}

type ProjectConversationTaskAppender interface {
	AcceptProjectMessage(context.Context, identity.Principal, string, string, AcceptConversationMessageCommand) (conversation.Message, bool, error)
	ListConversationActiveTasks(context.Context, identity.Principal, string, string, PageRequest) (TaskPage, error)
}

type ProjectConversationService struct {
	store  ProjectConversationStore
	access ProjectAccessChecker
	tasks  ProjectConversationTaskAppender
}

type CreateProjectConversationCommand struct {
	Title             string
	IdempotencyKey    string
	ServiceInstanceID string
}

func NewProjectConversationService(store ProjectConversationStore, access ProjectAccessChecker, tasks ProjectConversationTaskAppender) (*ProjectConversationService, error) {
	if store == nil || access == nil || tasks == nil {
		return nil, errors.New("project conversation dependencies are required")
	}
	return &ProjectConversationService{store: store, access: access, tasks: tasks}, nil
}

func (s *ProjectConversationService) List(ctx context.Context, principal identity.Principal, projectID string, page PageRequest) (ConversationPage, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ConversationPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ConversationPage{}, err
	}
	result, err := s.store.ListProjectConversations(ctx, principal.UserID, projectID, page)
	if err != nil {
		return ConversationPage{}, err
	}
	if result.Items == nil || len(result.Items) > int(page.Limit) || len(result.NextCursor) > 1024 {
		return ConversationPage{}, domain.NewError(domain.CodeInternal, "project conversation store returned an invalid page")
	}
	seen := make(map[string]struct{}, len(result.Items))
	for _, value := range result.Items {
		if err := validateProjectConversationResult(value, projectID); err != nil {
			return ConversationPage{}, err
		}
		if _, exists := seen[value.ID]; exists {
			return ConversationPage{}, domain.NewError(domain.CodeInternal, "project conversation store returned duplicate records")
		}
		seen[value.ID] = struct{}{}
	}
	return result, nil
}

func (s *ProjectConversationService) Create(ctx context.Context, principal identity.Principal, projectID string, command CreateProjectConversationCommand) (conversation.Conversation, bool, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return conversation.Conversation{}, false, err
	}
	command.Title = strings.TrimSpace(command.Title)
	if !utf8.ValidString(command.Title) || utf8.RuneCountInString(command.Title) > 120 {
		return conversation.Conversation{}, false, domain.InvalidArgument("title", "title is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return conversation.Conversation{}, false, err
	}
	id, err := domain.NewConversationID()
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "project_conversation.create", command.IdempotencyKey, struct {
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
	}{ProjectID: projectID, Title: command.Title}, id, "")
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	input := conversation.Conversation{
		ID: id, OwnerID: principal.UserID, ProjectID: projectID, Title: command.Title,
	}
	var value conversation.Conversation
	var replay bool
	if command.ServiceInstanceID != "" {
		if !domain.ValidServiceInstanceID(command.ServiceInstanceID) {
			return conversation.Conversation{}, false, domain.InvalidArgument("service_instance_id", "service instance id is invalid")
		}
		store, ok := s.store.(interface {
			CreateProjectConversationForServiceInstance(context.Context, string, conversation.Conversation, IdempotencyRecord, string) (conversation.Conversation, bool, error)
		})
		if !ok {
			return conversation.Conversation{}, false, domain.NewError(domain.CodeUnsupported, "scoped conversation creation is unavailable")
		}
		value, replay, err = store.CreateProjectConversationForServiceInstance(ctx, principal.UserID, input, record, command.ServiceInstanceID)
	} else {
		value, replay, err = s.store.CreateProjectConversation(ctx, principal.UserID, input, record)
	}
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	if err := validateProjectConversationResult(value, projectID); err != nil {
		return conversation.Conversation{}, false, err
	}
	return value, replay, nil
}

func (s *ProjectConversationService) Get(ctx context.Context, principal identity.Principal, projectID, conversationID string) (conversation.Conversation, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return conversation.Conversation{}, err
	}
	if !domain.ValidConversationID(conversationID) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
	}
	value, err := s.store.GetProjectConversation(ctx, principal.UserID, projectID, conversationID)
	if err != nil {
		return conversation.Conversation{}, err
	}
	if err := validateProjectConversationResult(value, projectID); err != nil {
		return conversation.Conversation{}, err
	}
	return value, nil
}

func (s *ProjectConversationService) ListMessages(ctx context.Context, principal identity.Principal, projectID, conversationID string, page PageRequest) (MessagePage, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return MessagePage{}, err
	}
	if !domain.ValidConversationID(conversationID) {
		return MessagePage{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return MessagePage{}, err
	}
	result, err := s.store.ListProjectConversationMessages(ctx, principal.UserID, projectID, conversationID, page)
	if err != nil {
		return MessagePage{}, err
	}
	if result.Items == nil || len(result.Items) > int(page.Limit) || len(result.NextCursor) > 1024 {
		return MessagePage{}, domain.NewError(domain.CodeInternal, "project conversation store returned an invalid message page")
	}
	for left, right := 0, len(result.Items)-1; left < right; left, right = left+1, right-1 {
		result.Items[left], result.Items[right] = result.Items[right], result.Items[left]
	}
	for _, value := range result.Items {
		if !domain.ValidMessageID(value.ID) || value.ConversationID != conversationID || !domain.ValidUserID(value.OwnerID) ||
			!domain.ValidUserID(value.ActorID) || (value.Role != conversation.RoleUser && value.Role != conversation.RoleAssistant) || value.CreatedAt.IsZero() {
			return MessagePage{}, domain.NewError(domain.CodeInternal, "project conversation store returned an invalid message")
		}
	}
	return result, nil
}

func (s *ProjectConversationService) FindMessageByClientID(ctx context.Context, principal identity.Principal, projectID, conversationID, clientID, instanceID string) (MessagePage, error) {
	if _, err := s.Get(ctx, principal, projectID, conversationID); err != nil {
		return MessagePage{}, err
	}
	return findConversationMessageByClientID(ctx, s.store, principal.UserID, projectID, conversationID, clientID, instanceID)
}

func (s *ProjectConversationService) AcceptMessage(ctx context.Context, principal identity.Principal, projectID, conversationID string, command AcceptConversationMessageCommand) (conversation.Message, bool, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return conversation.Message{}, false, err
	}
	command.ProjectID = projectID
	return s.tasks.AcceptProjectMessage(ctx, principal, projectID, conversationID, command)
}

func (s *ProjectConversationService) ListTasks(ctx context.Context, principal identity.Principal, projectID, conversationID string, page PageRequest) (TaskPage, error) {
	return s.tasks.ListConversationActiveTasks(ctx, principal, projectID, conversationID, page)
}

func validateProjectConversationResult(value conversation.Conversation, projectID string) error {
	if !domain.ValidConversationID(value.ID) || !project.ValidID(value.ProjectID) || value.ProjectID != projectID ||
		!domain.ValidUserID(value.OwnerID) || value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() ||
		!utf8.ValidString(value.Title) || utf8.RuneCountInString(value.Title) > 120 {
		return domain.NewError(domain.CodeInternal, "project conversation store returned an invalid record")
	}
	return nil
}
