package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

const (
	taskReadScope    identity.Scope = identity.ScopeTaskRead
	taskSubmitScope  identity.Scope = identity.ScopeTaskSubmit
	taskControlScope identity.Scope = identity.ScopeTaskControl

	FileDeliveryIntentClarificationCallID      = "morde-file-delivery-intent"
	RouteFileDeliveryIntentClarificationCallID = task.RouteFileDeliveryIntentClarificationCallID
)

type ConfiguredModelCatalog interface {
	IsConfiguredModel(context.Context, string, string) (bool, error)
}

type TaskExecutionAuthorizer interface {
	AuthorizeTaskExecution(context.Context, identity.Principal, string, identity.Scope) (task.Authorization, error)
}

type taskWorkspaceSnapshotReader interface {
	GetWorkspace(context.Context, string) (workspace.Workspace, error)
}

type taskWorkspaceOwnerManifestReader interface {
	GetWorkspaceManifestPageForOwner(context.Context, string, string, int64, string, string, int32) (workspace.ManifestPage, error)
}

type SubmitTaskCommand struct {
	ClientMessageID    string
	AttachmentIDs      []string
	HostID             string
	WorkspaceID        string
	InputRevision      *int64
	ApplicationID      string
	ModelID            string
	ConversationID     string
	Objective          string
	FileDeliveryIntent task.FileDeliveryIntent
	Budget             task.Budget
	BatchItems         []BatchItemInput
	MCPConnectorIDs    []string
	IdempotencyKey     string
}

type BatchItemInput struct {
	Key                string
	Input              string
	FileDeliveryIntent task.FileDeliveryIntent `json:"file_delivery_intent,omitempty"`
}

type TaskService struct {
	store               TaskStore
	authorizer          TaskAuthorizer
	executionAuthorizer TaskExecutionAuthorizer
	models              ConfiguredModelCatalog
	artifactReader      TaskArtifactReader
	actions             TaskActionRequester
	viewDeletions       TaskWorkspaceViewDeletionStore
	projectAccess       ProjectAccessChecker
	stepDetailSealer    *Sealer
}

func (s *TaskService) SetTaskStepDetailSealer(sealer *Sealer) error {
	if s == nil || sealer == nil {
		return domain.NewError(domain.CodeInvalidArgument, "task step detail sealer is required")
	}
	if s.stepDetailSealer != nil && s.stepDetailSealer != sealer {
		return domain.NewError(domain.CodeInvalidState, "task step detail sealer cannot be replaced")
	}
	s.stepDetailSealer = sealer
	return nil
}

func NewTaskService(store TaskStore, authorizer TaskAuthorizer, models ConfiguredModelCatalog, projectAccess ...ProjectAccessChecker) (*TaskService, error) {
	executionAuthorizer, ok := authorizer.(TaskExecutionAuthorizer)
	if store == nil || !ok || models == nil || len(projectAccess) > 1 || (len(projectAccess) == 1 && projectAccess[0] == nil) {
		return nil, domain.NewError(domain.CodeInvalidArgument, "task service dependencies are required")
	}
	actions, _ := store.(TaskActionRequester)
	viewDeletions, _ := store.(TaskWorkspaceViewDeletionStore)
	service := &TaskService{store: store, authorizer: authorizer, executionAuthorizer: executionAuthorizer,
		models: models, actions: actions, viewDeletions: viewDeletions}
	if len(projectAccess) == 1 {
		service.projectAccess = projectAccess[0]
	}
	return service, nil
}

func (s *TaskService) workspaceInputSnapshot(ctx context.Context, ownerID, hostID, workspaceID string, requestedRevision *int64) (*int64, []byte, error) {
	if workspaceID == "" {
		if requestedRevision != nil {
			return nil, nil, domain.InvalidArgument("input_revision", "input_revision requires workspace_id")
		}
		return nil, nil, nil
	}
	if !domain.ValidWorkspaceID(workspaceID) || (hostID != "" && !domain.ValidHostID(hostID)) || requestedRevision == nil || *requestedRevision < 0 {
		return nil, nil, domain.InvalidArgument("workspace_id", "workspace and input revision are required together")
	}
	reader, ok := s.store.(taskWorkspaceSnapshotReader)
	if !ok {
		return nil, nil, domain.NewError(domain.CodeInternal, "workspace task storage is unavailable")
	}
	manifestReader, ok := s.store.(taskWorkspaceOwnerManifestReader)
	if !ok {
		return nil, nil, domain.NewError(domain.CodeInternal, "owner-scoped workspace manifest storage is unavailable")
	}
	value, err := reader.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	if value.OwnerID != ownerID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return nil, nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if *requestedRevision > value.Revision {
		return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "workspace input revision is not committed")
	}
	files := make([]workspace.File, 0)
	emptyDirectories := make([]string, 0)
	directoryByPath := make(map[string]workspace.Directory)
	err = func() error {
		afterPath := ""
		for {
			page, err := manifestReader.GetWorkspaceManifestPageForOwner(ctx, ownerID, workspaceID, *requestedRevision, "", afterPath, manifestPageLimit)
			if err != nil {
				return err
			}
			if page.WorkspaceID != workspaceID || page.Revision != *requestedRevision {
				return errors.New("workspace manifest snapshot does not match the requested revision")
			}
			if len(files)+len(emptyDirectories)+len(page.Files)+len(page.EmptyDirectories) > workspace.MaxSyncFiles {
				return errors.New("workspace manifest exceeds item limit")
			}
			files = append(files, page.Files...)
			emptyDirectories = append(emptyDirectories, page.EmptyDirectories...)
			pagePaths := make([]string, 0, len(page.Files)+len(page.EmptyDirectories))
			for _, directory := range page.Directories {
				if err := workspace.ValidateDirectory(directory, false); err != nil {
					return errors.New("workspace manifest directory metadata is invalid")
				}
				key := strings.ToLower(directory.Path)
				if previous, exists := directoryByPath[key]; exists {
					if !workspace.SameDirectoryMetadata(previous, directory) {
						return errors.New("workspace manifest directory metadata changed between pages")
					}
					continue
				}
				directoryByPath[key] = directory
			}
			for _, file := range page.Files {
				pagePaths = append(pagePaths, file.Path)
			}
			pagePaths = append(pagePaths, page.EmptyDirectories...)
			sort.Strings(pagePaths)
			previousPath := afterPath
			for _, path := range pagePaths {
				if path <= previousPath || strings.EqualFold(path, previousPath) {
					return errors.New("workspace manifest paths are not in cursor order")
				}
				previousPath = path
			}
			if page.NextCursor == nil {
				return nil
			}
			cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
			if err != nil || cursor.Revision != *requestedRevision || cursor.Prefix != "" || len(pagePaths) == 0 ||
				cursor.After != pagePaths[len(pagePaths)-1] || cursor.After <= afterPath {
				return errors.New("workspace manifest cursor is invalid")
			}
			afterPath = cursor.After
		}
	}()
	directories := make([]workspace.Directory, 0, len(directoryByPath))
	for _, directory := range directoryByPath {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })
	manifestDigest, digestErr := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err == nil {
		err = digestErr
	}
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceBusy {
			return nil, nil, err
		}
		return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "workspace input manifest is unavailable")
	}
	digest, err := hex.DecodeString(manifestDigest)
	if err != nil || len(digest) != 32 {
		return nil, nil, domain.NewError(domain.CodeInternal, "workspace input manifest digest is invalid")
	}
	revision := *requestedRevision
	return &revision, digest, nil
}

func (s *TaskService) Submit(ctx context.Context, principal identity.Principal, command SubmitTaskCommand) (TaskSubmissionResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskSubmissionResult{}, err
	}
	if command.HostID != "" && !domain.ValidHostID(command.HostID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	if command.ApplicationID != "" && !ValidHostApplicationID(command.ApplicationID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("application_id", "application is not supported")
	}
	command.ModelID = strings.TrimSpace(command.ModelID)
	if command.ModelID == "" || len(command.ModelID) > 256 {
		return TaskSubmissionResult{}, domain.InvalidArgument("model_id", "model id is invalid")
	}
	if !validBoundedText(command.Objective, 32768) {
		return TaskSubmissionResult{}, domain.InvalidArgument("objective", "objective is required and must be at most 32768 characters")
	}
	if command.FileDeliveryIntent == "" {
		command.FileDeliveryIntent = task.FileDeliveryNone
	}
	if !command.FileDeliveryIntent.Valid() {
		return TaskSubmissionResult{}, domain.InvalidArgument("file_delivery_intent", "file delivery intent is invalid")
	}
	command.BatchItems = append([]BatchItemInput(nil), command.BatchItems...)
	for index := range command.BatchItems {
		if command.BatchItems[index].FileDeliveryIntent == "" {
			command.BatchItems[index].FileDeliveryIntent = task.FileDeliveryNone
		}
		if !command.BatchItems[index].FileDeliveryIntent.Valid() {
			return TaskSubmissionResult{}, domain.InvalidArgument("batch_items.file_delivery_intent", "batch item file delivery intent is invalid")
		}
	}
	if command.ConversationID != "" && !domain.ValidConversationID(command.ConversationID) {
		return TaskSubmissionResult{}, domain.InvalidArgument("conversation_id", "conversation id is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return TaskSubmissionResult{}, err
	}
	if command.ClientMessageID != "" && (!idempotencyKeyPattern.MatchString(command.ClientMessageID) || command.ClientMessageID != command.IdempotencyKey) {
		return TaskSubmissionResult{}, domain.InvalidArgument("client_message_id", "client message id must equal the idempotency key")
	}
	if len(command.AttachmentIDs) > conversation.MaxHistoryImportAttachments {
		return TaskSubmissionResult{}, domain.InvalidArgument("attachment_ids", "message attachment count exceeds the limit")
	}
	seenAttachments := make(map[string]struct{}, len(command.AttachmentIDs))
	for _, attachmentID := range command.AttachmentIDs {
		if !personal.ValidPersonalAttachmentID(attachmentID) {
			return TaskSubmissionResult{}, domain.InvalidArgument("attachment_ids", "personal attachment id is invalid")
		}
		if _, exists := seenAttachments[attachmentID]; exists {
			return TaskSubmissionResult{}, domain.InvalidArgument("attachment_ids", "attachment ids must be unique")
		}
		seenAttachments[attachmentID] = struct{}{}
	}
	connectorIDs, err := normalizeTaskMCPConnectorIDs(command.MCPConnectorIDs)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	command.MCPConnectorIDs = connectorIDs
	budget, err := normalizedBudget(command.Budget)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	command.Budget = budget
	batchItems, err := newBatchItems(command.BatchItems)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	fileDeliveryIntent := command.FileDeliveryIntent
	if fileDeliveryIntent == task.FileDeliveryRequired && command.WorkspaceID == "" {
		return TaskSubmissionResult{}, domain.InvalidArgument("workspace_id", "file delivery requires a Workspace")
	}
	var unclearBatchItem *task.BatchItem
	for index := range batchItems {
		item := &batchItems[index]
		if item.FileDeliveryIntent == task.FileDeliveryRequired && command.WorkspaceID == "" {
			return TaskSubmissionResult{}, domain.InvalidArgument("workspace_id", "file delivery requires a Workspace")
		}
		if item.FileDeliveryIntent == task.FileDeliveryUnclear {
			if unclearBatchItem != nil {
				return TaskSubmissionResult{}, domain.InvalidArgument("batch_items.file_delivery_intent", "only one unclear file delivery intent can be clarified per Task")
			}
			unclearBatchItem = item
		}
	}
	if fileDeliveryIntent == task.FileDeliveryUnclear && unclearBatchItem != nil {
		return TaskSubmissionResult{}, domain.InvalidArgument("file_delivery_intent", "Task and batch item file delivery intents cannot both be unclear")
	}
	authorization, err := s.authorizeTask(ctx, principal, command.HostID, taskSubmitScope)
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	if err := s.validateModel(ctx, principal.UserID, command.ModelID); err != nil {
		return TaskSubmissionResult{}, err
	}
	inputRevision, contextDigest, err := s.workspaceInputSnapshot(ctx, principal.UserID, command.HostID, command.WorkspaceID, command.InputRevision)
	if err != nil {
		return TaskSubmissionResult{}, err
	}

	conversationID := command.ConversationID
	var newConversation *conversation.Conversation
	if conversationID == "" {
		conversationID, err = domain.NewConversationID()
		if err != nil {
			return TaskSubmissionResult{}, fmt.Errorf("generate task conversation id: %w", err)
		}
		newConversation = &conversation.Conversation{
			ID: conversationID, OwnerID: principal.UserID, HostID: command.HostID,
			Title: truncateRunes(strings.TrimSpace(command.Objective), 120),
		}
	} else if _, err := s.store.GetConversation(ctx, conversationID, principal.UserID); err != nil {
		return TaskSubmissionResult{}, err
	}

	taskID, err := domain.NewTaskID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task id: %w", err)
	}
	messageID, err := domain.NewMessageID()
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("generate task message id: %w", err)
	}
	requestHash, err := hashIdempotencyRequest(struct {
		HostID             string                  `json:"host_id"`
		ApplicationID      string                  `json:"application_id,omitempty"`
		ModelID            string                  `json:"model_id"`
		ConversationID     string                  `json:"conversation_id,omitempty"`
		WorkspaceID        string                  `json:"workspace_id,omitempty"`
		InputRevision      *int64                  `json:"input_revision,omitempty"`
		ContextDigest      []byte                  `json:"context_digest,omitempty"`
		Objective          string                  `json:"objective"`
		FileDeliveryIntent task.FileDeliveryIntent `json:"file_delivery_intent,omitempty"`
		AttachmentIDs      []string                `json:"attachment_ids,omitempty"`
		Budget             task.Budget             `json:"budget"`
		BatchItems         []BatchItemInput        `json:"batch_items"`
		MCPConnectorIDs    []string                `json:"mcp_connector_ids"`
	}{
		HostID: command.HostID, ApplicationID: command.ApplicationID, ModelID: command.ModelID,
		ConversationID: command.ConversationID, WorkspaceID: command.WorkspaceID, InputRevision: inputRevision,
		ContextDigest: contextDigest, Objective: command.Objective, FileDeliveryIntent: hashFileDeliveryIntent(fileDeliveryIntent),
		AttachmentIDs: append([]string(nil), command.AttachmentIDs...),
		Budget:        command.Budget, BatchItems: normalizedBatchInputs(command.BatchItems), MCPConnectorIDs: command.MCPConnectorIDs,
	})
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	initialStatus := task.StatusQueued
	initialEventType := "task.status"
	initialWaitingReason := ""
	initialCurrentStep := ""
	var initialSteps []task.Step
	var initialClarification *task.ClarificationRequest
	initialEventPayload := map[string]string{"status": string(initialStatus)}
	if fileDeliveryIntent == task.FileDeliveryUnclear || unclearBatchItem != nil {
		initialSteps, initialClarification, err = newFileDeliveryIntentClarification(taskID, command.WorkspaceID != "", 0, 1)
		if err != nil {
			return TaskSubmissionResult{}, err
		}
		if unclearBatchItem != nil {
			initialClarification.BatchItemID = unclearBatchItem.ID
			initialClarification.BatchAttempt = unclearBatchItem.Attempt
			question := fmt.Sprintf("批次项目 %q 是否需要交付文件，还是只需聊天答复？ / Does batch item %q require a file deliverable, or is a chat answer enough?",
				unclearBatchItem.Key, unclearBatchItem.Key)
			initialClarification.Question = question
			initialSteps[1].Summary = question
		}
		initialStatus, initialEventType, initialWaitingReason, initialCurrentStep = task.StatusWaitingUser, "task.waiting_user", "clarification_required", initialSteps[1].ID
		initialEventPayload = map[string]string{
			"status": string(initialStatus), "reason": initialWaitingReason,
			"clarification_id": initialClarification.ID, "clarification_version": "1",
		}
	}
	initialEvent, err := json.Marshal(initialEventPayload)
	if err != nil {
		return TaskSubmissionResult{}, fmt.Errorf("encode initial task event: %w", err)
	}
	result, err := s.store.SubmitTask(ctx, TaskSubmission{
		Conversation: newConversation,
		Task: task.Task{
			ID: taskID, OwnerID: principal.UserID, InitiatorID: principal.UserID, ConversationID: conversationID,
			HostID: command.HostID, WorkspaceID: command.WorkspaceID, InputRevision: inputRevision, ContextDigest: contextDigest,
			ApplicationID: command.ApplicationID, ModelID: command.ModelID, Objective: command.Objective,
			QueuePriority:      task.QueuePriorityNormal,
			FileDeliveryIntent: fileDeliveryIntent,
			ExecutionLocation:  "server", Status: initialStatus, WaitingReason: initialWaitingReason, CurrentStep: initialCurrentStep,
			Authorization: authorization, Budget: command.Budget,
		},
		Message: conversation.Message{
			ID: messageID, ConversationID: conversationID, OwnerID: principal.UserID, ActorID: principal.UserID,
			Role: conversation.RoleUser, Content: command.Objective, ClientMessageID: command.ClientMessageID,
			AttachmentIDs: append([]string(nil), command.AttachmentIDs...),
		},
		InitialEvent: task.Event{TaskID: taskID, Type: initialEventType, Payload: initialEvent},
		InitialSteps: initialSteps, InitialClarificationRequest: initialClarification,
		BatchItems:      batchItems,
		MCPConnectorIDs: command.MCPConnectorIDs,
		Idempotency: IdempotencyRecord{
			OwnerID: principal.UserID, Operation: "task.submit", Key: command.IdempotencyKey,
			RequestHash: requestHash, ResourceID: taskID, RelatedID: messageID,
		},
	})
	if err != nil {
		return TaskSubmissionResult{}, err
	}
	return result, nil
}

func newFileDeliveryIntentClarification(taskID string, hasWorkspace bool, afterSequence, version int64) ([]task.Step, *task.ClarificationRequest, error) {
	if afterSequence < 0 || afterSequence > int64(^uint64(0)>>1)-2 {
		return nil, nil, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
	}
	if version < 1 {
		return nil, nil, domain.NewError(domain.CodeInvalidState, "clarification version is exhausted")
	}
	classificationStepID, err := domain.NewStepID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate file intent classification step ID: %w", err)
	}
	clarificationStepID, err := domain.NewStepID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate file intent clarification step ID: %w", err)
	}
	requestID, err := domain.NewPendingRequestID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate file intent clarification ID: %w", err)
	}
	question := "你需要生成并保存文件到此工作区，还是只要在聊天中回答？请明确说明。 / Do you need a file saved in this Workspace, or is a chat answer enough?"
	if !hasWorkspace {
		question = "你需要交付文件，还是只要在聊天中回答？如需文件，必须有工作区。 / Do you need a file, or is a chat answer enough? File delivery requires a Workspace."
	}
	steps := []task.Step{
		{ID: classificationStepID, TaskID: taskID, Sequence: afterSequence + 1, Kind: "verification", Status: task.StepSucceeded,
			Title: "Check file delivery intent", Summary: "The server could not determine whether a file is required."},
		{ID: clarificationStepID, TaskID: taskID, Sequence: afterSequence + 2, Kind: "clarification", Status: task.StepWaitingUser,
			Title: "Confirm file delivery", Summary: question},
	}
	request := &task.ClarificationRequest{
		ID: requestID, TaskID: taskID, Version: version, ModelStepID: classificationStepID,
		CallID: FileDeliveryIntentClarificationCallID, Question: question, Status: task.ClarificationPending,
	}
	return steps, request, nil
}

func (s *TaskService) List(ctx context.Context, principal identity.Principal, filter TaskListRequest) (TaskPage, error) {
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskPage{}, err
	}
	if err := validatePageRequest(filter.Page); err != nil {
		return TaskPage{}, err
	}
	if filter.Page.Limit == 0 {
		filter.Page.Limit = defaultPageSize
	}
	if filter.Status != "" && !validTaskStatus(filter.Status) {
		return TaskPage{}, domain.InvalidArgument("status", "task status is invalid")
	}
	hostIDs, err := authorizedTaskHostIDs(ctx, s.authorizer, principal, taskReadScope)
	if err != nil {
		return TaskPage{}, err
	}
	page, err := s.store.ListTasks(ctx, principal.UserID, TaskListRequest{HostIDs: hostIDs, Status: filter.Status, Page: filter.Page})
	if err != nil {
		return TaskPage{}, err
	}
	visible := make([]task.Task, 0, len(page.Items))
	for _, item := range page.Items {
		revokedAccess, err := s.authorizeTaskRead(ctx, principal, item)
		if err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound, domain.CodeForbidden:
				continue
			default:
				return TaskPage{}, err
			}
		}
		if revokedAccess {
			item = taskStatusOnlyProjection(item)
			delete(page.K2Projections, item.ID)
		}
		visible = append(visible, item)
	}
	page.Items = visible
	retainTaskK2Projections(&page)
	return page, nil
}

func retainTaskK2Projections(page *TaskPage) {
	if page == nil || page.K2Projections == nil {
		return
	}
	visible := make(map[string]struct{}, len(page.Items))
	for _, item := range page.Items {
		visible[item.ID] = struct{}{}
	}
	for taskID := range page.K2Projections {
		if _, ok := visible[taskID]; !ok {
			delete(page.K2Projections, taskID)
		}
	}
}

// ListConversationActiveTasks returns only tasks visible to this caller in the selected conversation.
func (s *TaskService) ListConversationActiveTasks(ctx context.Context, principal identity.Principal, projectID, conversationID string, page PageRequest) (TaskPage, error) {
	return s.ListConversationTasks(ctx, principal, projectID, conversationID, ConversationTaskViewActive, page)
}

// ListConversationTasks returns one status view and a count of visible tasks awaiting the caller.
func (s *TaskService) ListConversationTasks(ctx context.Context, principal identity.Principal, projectID, conversationID string, view ConversationTaskView, page PageRequest) (TaskPage, error) {
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskPage{}, err
	}
	if view != ConversationTaskViewActive && view != ConversationTaskViewRecent && view != ConversationTaskViewPanel {
		return TaskPage{}, domain.InvalidArgument("view", "conversation task view is invalid")
	}
	if !domain.ValidConversationID(conversationID) {
		return TaskPage{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if err := validatePageRequest(page); err != nil {
		return TaskPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	if projectID != "" {
		if !project.ValidID(projectID) || s.projectAccess == nil {
			return TaskPage{}, domain.NewError(domain.CodeNotFound, "project conversation not found")
		}
		if _, err := s.projectAccess.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
			return TaskPage{}, err
		}
		if _, err := s.store.GetProjectConversation(ctx, principal.UserID, projectID, conversationID); err != nil {
			return TaskPage{}, err
		}
	} else if _, err := s.store.GetConversation(ctx, conversationID, principal.UserID); err != nil {
		return TaskPage{}, err
	}
	hostIDs, err := authorizedTaskHostIDs(ctx, s.authorizer, principal, taskReadScope)
	if err != nil {
		return TaskPage{}, err
	}
	result, err := s.store.ListConversationTasks(ctx, principal.UserID, projectID, conversationID, hostIDs, view, page)
	if err != nil {
		return TaskPage{}, err
	}
	for hostID, count := range result.PendingCountsByHostID {
		if hostID == "" {
			result.PendingCount += count
			continue
		}
		if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, taskReadScope); err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound, domain.CodeForbidden:
				continue
			default:
				return TaskPage{}, err
			}
		}
		result.PendingCount += count
	}
	result.PendingCountsByHostID = nil
	visible := make([]task.Task, 0, len(result.Items))
	for _, item := range result.Items {
		if item.HostID == "" {
			visible = append(visible, item)
			continue
		}
		revokedAccess, err := s.authorizeTaskRead(ctx, principal, item)
		if err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound, domain.CodeForbidden:
				continue
			default:
				return TaskPage{}, err
			}
		}
		if revokedAccess {
			item = taskStatusOnlyProjection(item)
			delete(result.K2Projections, item.ID)
		}
		visible = append(visible, item)
	}
	result.Items = visible
	retainTaskK2Projections(&result)
	return result, nil
}

func (s *TaskService) Get(ctx context.Context, principal identity.Principal, id string) (TaskDetails, error) {
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskDetails{}, err
	}
	if !domain.ValidTaskID(id) {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	details, err := s.store.GetTask(ctx, id, principal.UserID)
	if err != nil {
		return TaskDetails{}, err
	}
	if details.Task.InternalTool {
		return TaskDetails{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	revokedAccess, err := s.authorizeTaskRead(ctx, principal, details.Task)
	if err != nil {
		return TaskDetails{}, err
	}
	if revokedAccess {
		return taskStatusOnlyDetails(details), nil
	}
	if pending := details.PendingRequest; pending != nil && pending.Status == task.PendingRequestPending && pending.ExpiresAt != nil {
		expirer, ok := s.store.(TaskPendingRequestExpirer)
		if !ok {
			return TaskDetails{}, domain.NewError(domain.CodeInternal, "approval expiry storage is unavailable")
		}
		if _, err := expirer.ExpirePendingRequest(ctx, principal.UserID, id, pending.ID, pending.Version); err != nil {
			return TaskDetails{}, err
		}
		details, err = s.store.GetTask(ctx, id, principal.UserID)
		if err != nil {
			return TaskDetails{}, err
		}
		revokedAccess, err = s.authorizeTaskRead(ctx, principal, details.Task)
		if err != nil {
			return TaskDetails{}, err
		}
		if revokedAccess {
			return taskStatusOnlyDetails(details), nil
		}
	}
	artifactsHidden := false
	if len(details.Artifacts) > 0 {
		visible, err := s.taskArtifactWorkspaceVisible(ctx, principal.UserID, details.Task.WorkspaceID)
		if err != nil {
			return TaskDetails{}, err
		}
		if !visible {
			details.Artifacts = nil
			artifactsHidden = true
		}
	}
	workspaceVisible, err := s.taskWorkspaceDetailVisible(ctx, principal, details.Task)
	if err != nil {
		return TaskDetails{}, err
	}
	if !workspaceVisible {
		for index := range details.Steps {
			if !strings.HasPrefix(details.Steps[index].ToolName, "morde_workspace_") {
				continue
			}
			details.Steps[index].Title = "Workspace action"
			details.Steps[index].Summary = "The Workspace resource is not available with the current permissions."
			details.Steps[index].DetailState = "restricted"
		}
	}
	details.ValidationEvidence = restrictTaskValidationEvidence(details.ValidationEvidence, details.Steps, !workspaceVisible, artifactsHidden)
	return details, nil
}

// GetTaskValidationReviewSnapshot reads only the still-current Task result and
// its same-revision evidence for the leased internal review worker.
func (s *TaskService) GetTaskValidationReviewSnapshot(ctx context.Context, claim TaskValidationReviewClaim) (TaskDetails, error) {
	if ctx == nil || !ValidTaskValidationReviewClaim(claim) {
		return TaskDetails{}, domain.InvalidArgument("task_validation_review", "Task validation review claim is invalid")
	}
	claimVerifier, ok := s.store.(TaskValidationReviewClaimVerifier)
	if !ok {
		return TaskDetails{}, domain.NewError(domain.CodeUnsupported, "Task validation review claim verification is unavailable")
	}
	currentClaim, err := claimVerifier.IsCurrentTaskValidationReviewClaim(ctx, claim)
	if err != nil {
		return TaskDetails{}, err
	}
	if !currentClaim {
		return TaskDetails{}, domain.NewError(domain.CodeConflict, "Task validation review claim is no longer current")
	}
	details, err := s.store.GetTask(ctx, claim.TaskID, claim.InitiatorID)
	if err != nil {
		return TaskDetails{}, err
	}
	if details.Task.ID != claim.TaskID || details.Task.OwnerID != claim.OwnerID || details.Task.InitiatorID != claim.InitiatorID ||
		details.Task.ConversationID != claim.ConversationID || details.Task.Status != task.StatusCompleted ||
		details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != claim.TaskRevision ||
		details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != claim.TaskRevision ||
		!bytes.Equal(sha256Bytes([]byte(details.Task.Result)), claim.ResultSHA256) {
		return TaskDetails{}, domain.NewError(domain.CodeConflict, "Task validation review result is no longer current")
	}
	principal := identity.Principal{UserID: claim.InitiatorID, Role: identity.RoleUser, Kind: identity.Kind(details.Task.Authorization.Kind),
		TokenID: details.Task.Authorization.TokenID, ClientInstallationID: details.Task.Authorization.ClientInstallationID,
		Scopes: agentScopeSnapshot(details.Task.Authorization.Scopes)}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskReadScope); err != nil {
		return TaskDetails{}, hideTaskAccessError(err)
	}
	artifactsHidden := false
	if len(details.Artifacts) > 0 {
		visible, err := s.taskArtifactWorkspaceVisible(ctx, principal.UserID, details.Task.WorkspaceID)
		if err != nil {
			return TaskDetails{}, err
		}
		if !visible {
			details.Artifacts = nil
			artifactsHidden = true
		}
	}
	workspaceVisible, err := s.taskWorkspaceDetailVisible(ctx, principal, details.Task)
	if err != nil {
		return TaskDetails{}, err
	}
	if !workspaceVisible {
		for index := range details.Steps {
			if strings.HasPrefix(details.Steps[index].ToolName, "morde_workspace_") {
				details.Steps[index].Title = "Workspace action"
				details.Steps[index].Summary = "The Workspace resource is not available with the current permissions."
				details.Steps[index].DetailState = "restricted"
			}
		}
	}
	details.ValidationEvidence = restrictTaskValidationEvidence(details.ValidationEvidence, details.Steps, !workspaceVisible, artifactsHidden)
	return details, nil
}

func (s *TaskService) GetTaskValidationReviewStepDetail(ctx context.Context, claim TaskValidationReviewClaim, stepID string) (TaskStepDetail, error) {
	details, err := s.GetTaskValidationReviewSnapshot(ctx, claim)
	if err != nil {
		return TaskStepDetail{}, err
	}
	if !taskValidationReviewEvidenceContainsStep(details.ValidationEvidence, stepID) {
		return TaskStepDetail{}, domain.NewError(domain.CodeNotFound, "task step detail not found")
	}
	return s.taskStepDetailFromDetails(ctx, identity.Principal{UserID: claim.InitiatorID}, details, claim.TaskID, stepID)
}

func taskValidationReviewEvidenceContainsStep(evidence *TaskValidationEvidenceRecord, stepID string) bool {
	if evidence == nil || !domain.ValidStepID(stepID) {
		return false
	}
	for _, claim := range evidence.Claims {
		for _, ref := range claim.EvidenceRefs {
			if ref.ID == stepID && (ref.Kind == TaskValidationEvidenceAction || ref.Kind == TaskValidationEvidenceBaseline ||
				ref.Kind == TaskValidationEvidenceResult || ref.Kind == TaskValidationEvidenceBeforeAfter) {
				return true
			}
		}
		if comparison := claim.PerformanceComparison; comparison != nil &&
			(comparison.BaselineRef.ID == stepID || comparison.AfterRef.ID == stepID) {
			return true
		}
	}
	return false
}

func restrictTaskValidationEvidence(value *TaskValidationEvidenceRecord, steps []task.Step, hideWorkspace, hideArtifacts bool) *TaskValidationEvidenceRecord {
	if value == nil {
		return nil
	}
	workspaceStepIDs := make(map[string]struct{})
	if hideWorkspace {
		for _, step := range steps {
			if strings.HasPrefix(step.ToolName, "morde_workspace_") {
				workspaceStepIDs[step.ID] = struct{}{}
			}
		}
	}
	result := *value
	result.Claims = make([]TaskValidationEvidenceClaimRecord, 0, len(value.Claims))
	for _, claim := range value.Claims {
		filtered := claim
		filtered.EvidenceRefs = make([]TaskValidationEvidenceRefRecord, 0, len(claim.EvidenceRefs))
		comparisonVisible := func(ref TaskValidationEvidenceRefRecord) bool {
			_, hidden := workspaceStepIDs[ref.ID]
			return !(hidden && (ref.Kind == TaskValidationEvidenceBaseline || ref.Kind == TaskValidationEvidenceAction ||
				ref.Kind == TaskValidationEvidenceResult || ref.Kind == TaskValidationEvidenceBeforeAfter))
		}
		for _, ref := range claim.EvidenceRefs {
			if hideArtifacts && ref.Kind == TaskValidationEvidenceSideEffect {
				continue
			}
			if !comparisonVisible(ref) {
				continue
			}
			filtered.EvidenceRefs = append(filtered.EvidenceRefs, ref)
		}
		if comparison := claim.PerformanceComparison; comparison != nil {
			if !comparisonVisible(comparison.BaselineRef) || !comparisonVisible(comparison.AfterRef) {
				filtered.PerformanceComparison = nil
			} else {
				copy := *comparison
				filtered.PerformanceComparison = &copy
			}
		}
		if len(filtered.EvidenceRefs) > 0 {
			result.Claims = append(result.Claims, filtered)
		}
	}
	if len(result.Claims) == 0 {
		return nil
	}
	return &result
}

func (s *TaskService) taskWorkspaceDetailVisible(ctx context.Context, principal identity.Principal, value task.Task) (bool, error) {
	if value.WorkspaceID == "" {
		return true, nil
	}
	reader, ok := s.store.(taskWorkspaceSnapshotReader)
	if !ok || !domain.ValidWorkspaceID(value.WorkspaceID) {
		return false, nil
	}
	current, err := reader.GetWorkspace(ctx, value.WorkspaceID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ownerID := value.WorkspaceOwnerID
	if ownerID == "" {
		ownerID = value.OwnerID
	}
	if current.ID != value.WorkspaceID || current.OwnerID != ownerID || current.State != workspace.StateReady || current.DeletedAt != nil {
		return false, nil
	}
	if value.ProjectID != "" {
		if s.projectAccess == nil {
			return false, nil
		}
		if _, err := s.projectAccess.RequireAccess(ctx, principal, value.ProjectID, project.RoleMember); err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return current.OwnerID == principal.UserID, nil
}

func (s *TaskService) GetStepDetail(ctx context.Context, principal identity.Principal, taskID, stepID string) (TaskStepDetail, error) {
	if !domain.ValidStepID(stepID) {
		return TaskStepDetail{}, domain.NewError(domain.CodeNotFound, "task step not found")
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskStepDetail{}, err
	}
	return s.taskStepDetailFromDetails(ctx, principal, details, taskID, stepID)
}

func (s *TaskService) taskStepDetailFromDetails(ctx context.Context, principal identity.Principal, details TaskDetails, taskID, stepID string) (TaskStepDetail, error) {
	var step *task.Step
	for i := range details.Steps {
		if details.Steps[i].ID == stepID {
			step = &details.Steps[i]
			break
		}
	}
	if step == nil {
		return TaskStepDetail{}, domain.NewError(domain.CodeNotFound, "task step not found")
	}
	result := TaskStepDetail{TaskID: taskID, StepID: stepID, EventSequence: details.Task.EventSequence,
		ToolName: step.ToolName, DetailState: step.DetailState}
	if result.DetailState == "" {
		result.DetailState = "not_recorded"
	}
	if result.DetailState != "available" {
		return result, nil
	}
	reader, ok := s.store.(TaskStepDetailReader)
	if !ok || s.stepDetailSealer == nil || step.TaskRevision == nil {
		result.DetailState = "not_recorded"
		return result, nil
	}
	record, err := reader.GetTaskStepDetail(ctx, principal.UserID, taskID, stepID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			result.DetailState = "expired"
			return result, nil
		}
		return TaskStepDetail{}, err
	}
	if record.TaskID != taskID || record.OwnerID != details.Task.OwnerID || record.StepID != stepID || record.TaskRevision != *step.TaskRevision ||
		len(record.Ciphertext) < 29 || len(record.Ciphertext) > MaxTaskStepDetailCiphertextBytes ||
		record.SizeBytes < 1 || record.SizeBytes > MaxTaskStepDetailPayloadBytes || len(record.SHA256) != 32 {
		return TaskStepDetail{}, domain.NewError(domain.CodeInvalidState, "task step detail identity is invalid")
	}
	plain, err := s.stepDetailSealer.OpenWithAAD(record.Ciphertext, taskStepDetailAAD(record.OwnerID, taskID, stepID, record.TaskRevision))
	if err != nil || len(plain) != int(record.SizeBytes) || !bytes.Equal(sha256Bytes(plain), record.SHA256) {
		return TaskStepDetail{}, domain.NewError(domain.CodeInvalidState, "task step detail integrity check failed")
	}
	var document TaskStepDetailDocument
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || !validTaskStepDetailDocument(document) {
		return TaskStepDetail{}, domain.NewError(domain.CodeInvalidState, "task step detail document is invalid")
	}
	if taskStepExecutionRequiresWorkspace(document.Execution) {
		if document.ExecutionWorkspaceID == "" || document.ExecutionWorkspaceID != details.Task.WorkspaceID {
			result.DetailState = "restricted"
			return result, nil
		}
		visible, err := s.taskWorkspaceDetailVisible(ctx, principal, details.Task)
		if err != nil {
			return TaskStepDetail{}, err
		}
		if !visible {
			result.DetailState = "restricted"
			return result, nil
		}
	}
	if document.ContentEvidence == nil {
		if len(record.ContentEvidenceSHA256) != 0 {
			return TaskStepDetail{}, domain.NewError(domain.CodeInvalidState, "task step content evidence identity is invalid")
		}
	} else {
		contentDigest, digestErr := hex.DecodeString(document.ContentEvidence.SHA256)
		if digestErr != nil || len(record.ContentEvidenceSHA256) != sha256.Size || !bytes.Equal(contentDigest, record.ContentEvidenceSHA256) ||
			(step.ToolName != "morde_web_search" && step.ToolName != "morde_web_read") || step.Status != task.StepSucceeded {
			return TaskStepDetail{}, domain.NewError(domain.CodeInvalidState, "task step content evidence identity is invalid")
		}
	}
	result.DetailState, result.Truncated = "available", record.Truncated
	result.EventSequence = record.EventSequence
	result.Object, result.Input, result.Result = &document.Object, &document.Input, &document.Result
	result.ShellExecution, result.ContentEvidence = document.ShellExecution, document.ContentEvidence
	result.Execution = document.Execution
	if step.Status == task.StepUnknown || step.Status == task.StepCancelled {
		message := "The tool result could not be confirmed and needs reconciliation."
		if step.Status == task.StepCancelled {
			message = "The tool call was cancelled before its result was committed."
		}
		result.Result = &message
		result.Execution = taskStepExecutionWithoutResult(document.Execution)
		result.ShellExecution = nil
	}
	return result, nil
}

func taskStepExecutionWithoutResult(value *TaskStepExecution) *TaskStepExecution {
	if value == nil {
		return nil
	}
	copy := *value
	copy.ExitCode, copy.TimedOut, copy.Stdout, copy.Stderr = nil, nil, nil, nil
	copy.StdoutTotalBytes, copy.StderrTotalBytes, copy.StdoutOmittedBytes, copy.StderrOmittedBytes = nil, nil, nil, nil
	copy.StdoutTruncated, copy.StderrTruncated = nil, nil
	copy.StdoutRuntimeTruncated, copy.StderrRuntimeTruncated = nil, nil
	copy.StdoutDetailTruncated, copy.StderrDetailTruncated = nil, nil
	return &copy
}

// RetryStep reserves an immutable retry of one confirmed, not-dispatched Host
// tool call. It never accepts replacement arguments from the caller.
func (s *TaskService) RetryStep(ctx context.Context, principal identity.Principal, taskID, stepID string,
	expectedRevision, expectedAttempt int64, idempotencyKey string) (TaskStepRetryResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskStepRetryResult{}, err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidStepID(stepID) {
		return TaskStepRetryResult{}, domain.NewError(domain.CodeNotFound, "task step not found")
	}
	if expectedRevision < 0 || expectedAttempt < 1 {
		return TaskStepRetryResult{}, domain.InvalidArgument("retry", "step retry fence is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return TaskStepRetryResult{}, err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return TaskStepRetryResult{}, err
	}
	if details.Task.InternalTool {
		return TaskStepRetryResult{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	current := details.Task
	authorization, err := s.authorizeTask(ctx, principal, current.HostID, taskControlScope)
	if err != nil {
		return TaskStepRetryResult{}, hideTaskAccessError(err)
	}
	if authorization.Kind != current.Authorization.Kind || authorization.TokenID != current.Authorization.TokenID ||
		authorization.ClientInstallationID != current.Authorization.ClientInstallationID || authorization.Version != current.Authorization.Version ||
		!sameTaskAuthorizationScopes(authorization.Scopes, current.Authorization.Scopes) {
		return TaskStepRetryResult{}, domain.NewError(domain.CodeForbidden, "task authorization changed")
	}
	retryStore, ok := s.store.(TaskStepRetryStore)
	if !ok {
		return TaskStepRetryResult{}, domain.NewError(domain.CodeInternal, "task step retry storage is unavailable")
	}
	attemptStepID, err := domain.NewStepID()
	if err != nil {
		return TaskStepRetryResult{}, fmt.Errorf("generate task step retry identity: %w", err)
	}
	request := TaskStepRetryRequest{TaskID: taskID, StepID: stepID, ExpectedTaskRevision: expectedRevision,
		ExpectedAttempt: expectedAttempt, AttemptStepID: attemptStepID, RetryCallID: "morde_step_retry_" + attemptStepID}
	idempotency, err := idempotencyRecord(principal.UserID, "task.step.retry", idempotencyKey, struct {
		TaskID   string `json:"task_id"`
		StepID   string `json:"step_id"`
		Revision int64  `json:"expected_task_revision"`
		Attempt  int64  `json:"expected_attempt"`
	}{taskID, stepID, expectedRevision, expectedAttempt}, attemptStepID, stepID)
	if err != nil {
		return TaskStepRetryResult{}, err
	}
	reserved, err := retryStore.RequestTaskStepRetry(ctx, idempotency, request)
	if err != nil {
		return TaskStepRetryResult{}, err
	}
	return TaskStepRetryResult{TaskID: taskID, SourceStepID: stepID, AttemptStepID: reserved.Intent.AttemptStepID,
		Attempt: reserved.Intent.Attempt, Status: "queued", Replayed: reserved.Replayed}, nil
}

func sameTaskAuthorizationScopes(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Project Task owners can differ from the Workspace-owning member reading artifacts.
func (s *TaskService) taskArtifactWorkspaceVisible(ctx context.Context, requesterID, workspaceID string) (bool, error) {
	reader, ok := s.store.(taskWorkspaceSnapshotReader)
	if !ok || requesterID == "" || !domain.ValidWorkspaceID(workspaceID) {
		return false, nil
	}
	value, err := reader.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return false, err
	}
	return value.ID == workspaceID && value.OwnerID == requesterID && value.State == workspace.StateReady && value.DeletedAt == nil, nil
}

// RetryBatchItem resets one failed batch item and queues its task for execution.
// The store applies the idempotency record and state transition atomically.
func (s *TaskService) RetryBatchItem(ctx context.Context, principal identity.Principal, taskID, batchItemID, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidBatchItemID(batchItemID) {
		return domain.NewError(domain.CodeNotFound, "task or batch item not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if details.Task.InternalTool {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.batch_item.retry", idempotencyKey, struct {
		Action      string `json:"action"`
		TaskID      string `json:"task_id"`
		BatchItemID string `json:"batch_item_id"`
	}{"retry_batch_item", taskID, batchItemID}, taskID, batchItemID)
	if err != nil {
		return err
	}
	return s.store.RetryFailedBatchItem(ctx, record, taskID, batchItemID)
}

// RespondToPendingRequest records one decision against an immutable approval
// request version. The store performs the idempotent state transition and
// queues the task atomically.
func (s *TaskService) RespondToPendingRequest(ctx context.Context, principal identity.Principal, taskID, pendingID string, version int64, decision task.PendingDecision, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(pendingID) {
		return domain.NewError(domain.CodeNotFound, "task or pending request not found")
	}
	if version < 1 || (decision != task.PendingDecisionApprove && decision != task.PendingDecisionReject) {
		return domain.InvalidArgument("pending_request", "pending request version or decision is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if details.Task.InternalTool {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	record, err := idempotencyRecord(principal.UserID, "task.pending.respond", idempotencyKey, struct {
		Action   string               `json:"action"`
		TaskID   string               `json:"task_id"`
		ID       string               `json:"pending_id"`
		Version  int64                `json:"pending_version"`
		Decision task.PendingDecision `json:"decision"`
	}{"respond", taskID, pendingID, version, decision}, taskID, pendingID)
	if err != nil {
		return err
	}
	return s.store.RespondToPendingRequest(ctx, record, taskID, pendingID, version, decision)
}

// AnswerClarificationRequest records a free-form answer against the exact
// active Agent question and atomically queues the waiting task.
func (s *TaskService) AnswerClarificationRequest(ctx context.Context, principal identity.Principal, taskID, clarificationID string, version int64,
	answer string, fileIntent task.FileDeliveryIntent, idempotencyKey string) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidPendingRequestID(clarificationID) {
		return domain.NewError(domain.CodeNotFound, "task or clarification request not found")
	}
	if version < 1 || !validBoundedText(answer, 16384) || strings.ContainsRune(answer, '\x00') {
		return domain.InvalidArgument("clarification", "clarification version or answer is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}
	if fileIntent != "" && !fileIntent.Valid() {
		return domain.InvalidArgument("file_delivery_intent", "file delivery intent is invalid")
	}
	details, err := s.store.GetTask(ctx, taskID, principal.UserID)
	if err != nil {
		return err
	}
	if details.Task.InternalTool {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	if _, err := s.authorizeTask(ctx, principal, details.Task.HostID, taskControlScope); err != nil {
		return hideTaskAccessError(err)
	}
	var objective string
	fileDeliveryClarification := false
	routeFileDeliveryClarification := false
	for _, request := range details.ClarificationRequests {
		if request.ID != clarificationID || (request.CallID != FileDeliveryIntentClarificationCallID &&
			request.CallID != RouteFileDeliveryIntentClarificationCallID) {
			continue
		}
		fileDeliveryClarification = true
		routeFileDeliveryClarification = request.CallID == RouteFileDeliveryIntentClarificationCallID
		if request.Version == version && request.Status == task.ClarificationPending {
			if routeFileDeliveryClarification {
				if (details.Task.Status != task.StatusPausing && details.Task.Status != task.StatusPaused) || details.Task.WaitingReason != "" {
					return domain.NewError(domain.CodeConflict, "route file delivery clarification is no longer active")
				}
				if fileIntent == task.FileDeliveryRequired && (details.Task.WorkspaceID == "" || details.Task.InputRevision == nil ||
					details.Task.WorkspaceCheckpointRevision == nil || *details.Task.WorkspaceCheckpointRevision != *details.Task.InputRevision ||
					len(details.Task.ContextDigest) != 32) {
					return domain.InvalidArgument("workspace_id", "文件交付需要任务当前绑定完整的工作区输入快照。 / File delivery requires a complete Workspace input snapshot bound to the Task.")
				}
			} else {
				unresolvedIntents := 0
				if details.Task.FileDeliveryIntent == task.FileDeliveryUnclear {
					unresolvedIntents++
				}
				for _, item := range details.BatchItems {
					if item.FileDeliveryIntent == task.FileDeliveryUnclear {
						unresolvedIntents++
					}
				}
				if unresolvedIntents != 1 {
					return domain.NewError(domain.CodeConflict, "file delivery intent has already been resolved")
				}
				if fileIntent == task.FileDeliveryRequired && details.Task.WorkspaceID == "" {
					return domain.InvalidArgument("workspace_id", "文件交付需要先为任务选择工作区，请使用工作区重新提交任务。 / File delivery requires a Workspace; resubmit the task with one selected.")
				}
			}
		}
		if !routeFileDeliveryClarification {
			objective = strings.TrimSpace(details.Task.Objective) + "\n\nUser clarification about file delivery: " + strings.TrimSpace(answer)
		}
		break
	}
	if !fileDeliveryClarification && fileIntent != "" {
		return domain.InvalidArgument("file_delivery_intent", "only file delivery clarifications accept a file delivery intent")
	}
	if fileDeliveryClarification {
		if fileIntent != task.FileDeliveryNone && fileIntent != task.FileDeliveryRequired {
			return domain.InvalidArgument("file_delivery_intent", "file delivery clarification requires none or required")
		}
		if routeFileDeliveryClarification && objective != "" {
			return domain.NewError(domain.CodeInternal, "route file delivery clarification cannot change the Task objective")
		}
	}
	record, err := idempotencyRecord(principal.UserID, "task.clarification.answer", idempotencyKey, struct {
		Action             string                  `json:"action"`
		TaskID             string                  `json:"task_id"`
		ID                 string                  `json:"clarification_id"`
		Version            int64                   `json:"clarification_version"`
		Answer             string                  `json:"answer"`
		FileDeliveryIntent task.FileDeliveryIntent `json:"file_delivery_intent,omitempty"`
	}{"answer_clarification", taskID, clarificationID, version, answer, fileIntent}, taskID, clarificationID)
	if err != nil {
		return err
	}
	return s.store.AnswerClarificationRequest(ctx, record, taskID, clarificationID, version, answer, fileIntent, objective)
}

func (s *TaskService) ListEvents(ctx context.Context, principal identity.Principal, id string, after int64, limit int32) (TaskEventPage, error) {
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskEventPage{}, err
	}
	if !domain.ValidTaskID(id) {
		return TaskEventPage{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	if after < 0 || limit < 1 || limit > maxEventPage {
		return TaskEventPage{}, domain.InvalidArgument("after", "event cursor or limit is out of range")
	}
	details, err := s.store.GetTask(ctx, id, principal.UserID)
	if err != nil {
		return TaskEventPage{}, err
	}
	if details.Task.InternalTool {
		return TaskEventPage{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	page, err := s.store.GetTaskEventPage(ctx, principal.UserID, id, after, limit)
	if err != nil && domain.CodeOf(err) != domain.CodeEventCursorExpired {
		return TaskEventPage{}, err
	}
	pageErr := err
	revokedAccess, authorizationErr := s.authorizeTaskRead(ctx, principal, details.Task)
	if authorizationErr != nil {
		return TaskEventPage{}, authorizationErr
	}
	if revokedAccess {
		return TaskEventPage{HostID: details.Task.HostID, Status: details.Task.Status,
			CurrentSequence: page.CurrentSequence}, nil
	}
	if pageErr != nil {
		return page, pageErr
	}
	previous := after
	for _, event := range page.Items {
		if event.TaskID != id || event.Sequence < 1 || event.Sequence != previous+1 ||
			!validTaskEventReadType(event.Type) || !json.Valid(event.Payload) || event.CreatedAt.IsZero() {
			return TaskEventPage{}, domain.NewError(domain.CodeInternal, "stored task events are invalid")
		}
		previous = event.Sequence
	}
	if hasArtifactEvent(page.Items) {
		details, err := s.store.GetTask(ctx, id, principal.UserID)
		if err != nil {
			return TaskEventPage{}, err
		}
		visible, err := s.taskArtifactWorkspaceVisible(ctx, principal.UserID, details.Task.WorkspaceID)
		if err != nil {
			return TaskEventPage{}, err
		}
		if !visible {
			for index := range page.Items {
				if page.Items[index].Type == "task.artifact" {
					page.Items[index].Payload = []byte(`{"redacted":true}`)
				}
			}
		}
	}
	return page, nil
}

func hasArtifactEvent(events []task.Event) bool {
	for _, event := range events {
		if event.Type == "task.artifact" {
			return true
		}
	}
	return false
}

func (s *TaskService) validateModel(ctx context.Context, ownerID, modelID string) error {
	configured, err := s.models.IsConfiguredModel(ctx, ownerID, modelID)
	if err != nil {
		return fmt.Errorf("check configured model: %w", err)
	}
	if !configured {
		return domain.InvalidArgument("model_id", "model is not configured for this user")
	}
	return nil
}

func (s *TaskService) authorizeTask(ctx context.Context, principal identity.Principal, hostID string, scope identity.Scope) (task.Authorization, error) {
	if s.executionAuthorizer == nil {
		return task.Authorization{}, domain.NewError(domain.CodeUnsupported, "task authorization is unavailable")
	}
	return s.executionAuthorizer.AuthorizeTaskExecution(ctx, principal, hostID, scope)
}

func (s *TaskService) authorizeTaskRead(ctx context.Context, principal identity.Principal, value task.Task) (bool, error) {
	if _, err := s.authorizeTask(ctx, principal, value.HostID, taskReadScope); err != nil {
		if (domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden) &&
			canReadInitiatedTask(principal, value) &&
			(value.Status == task.StatusCancelling || value.Status == task.StatusCancelled) {
			return true, nil
		}
		return false, hideTaskAccessError(err)
	}
	return false, nil
}

func taskStatusOnlyProjection(value task.Task) task.Task {
	return task.Task{
		ID: value.ID, OwnerID: value.OwnerID, ProjectID: value.ProjectID, InitiatorID: value.InitiatorID,
		ConversationID: value.ConversationID, HostID: value.HostID, ExecutionLocation: value.ExecutionLocation,
		ModelID: value.ModelID, Budget: value.Budget, Status: value.Status,
		EventSequence: value.EventSequence, Objective: value.Objective, CreatedAt: value.CreatedAt,
		UpdatedAt: value.UpdatedAt, FinishedAt: value.FinishedAt,
	}
}

func taskStatusOnlyDetails(value TaskDetails) TaskDetails {
	value.Task = taskStatusOnlyProjection(value.Task)
	value.Dependencies = nil
	value.ChildJobs = nil
	value.ResultReview = nil
	value.Validation = TaskValidationSummary{}
	value.ValidationEvidence = nil
	value.AssistantMessage = nil
	value.CurrentRevisionStarted = nil
	value.ResultMessagePending = false
	value.ResultMessagePublished = false
	value.ScheduleObservation = nil
	value.Steps = nil
	value.Agents = nil
	value.BatchItems = nil
	value.Artifacts = nil
	value.PendingRequest = nil
	value.ClarificationRequests = nil
	return value
}

func authorizedTaskHostIDs(ctx context.Context, authorizer TaskAuthorizer, principal identity.Principal, scope identity.Scope) ([]string, error) {
	if principal.Kind == identity.KindSession || principal.Kind == identity.KindClient {
		// The service authorizes each returned Host because the store also lists project Tasks.
		return nil, nil
	}
	if !principal.HasScope(scope) {
		return nil, domain.NewError(domain.CodeForbidden, "task scope is required")
	}
	ids := append([]string{}, principal.HostIDs...)
	if len(ids) == 0 {
		return ids, nil
	}
	allowed := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !domain.ValidHostID(id) {
			return nil, domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		_, err := authorizeTaskHost(ctx, authorizer, principal, id, scope)
		if domain.CodeOf(err) == domain.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, id)
	}
	sort.Strings(allowed)
	return allowed, nil
}

func normalizedBudget(value task.Budget) (task.Budget, error) {
	if value == (task.Budget{}) {
		value = task.Budget{ActiveSeconds: task.MaxActiveSeconds, ToolCalls: task.MaxToolCalls, ModelTokens: task.MaxModelTokens}
	}
	if err := value.Validate(); err != nil {
		return task.Budget{}, domain.InvalidArgument("budget", err.Error())
	}
	return value, nil
}

func newBatchItems(values []BatchItemInput) ([]task.BatchItem, error) {
	if len(values) > 100 {
		return nil, domain.InvalidArgument("batch_items", "at most 100 batch items are allowed")
	}
	items := make([]task.BatchItem, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validBoundedText(value.Key, 128) || !validBoundedText(value.Input, 16384) {
			return nil, domain.InvalidArgument("batch_items", "batch item key or input is invalid")
		}
		if _, exists := seen[value.Key]; exists {
			return nil, domain.InvalidArgument("batch_items", "batch item keys must be unique")
		}
		seen[value.Key] = struct{}{}
		id, err := domain.NewBatchItemID()
		if err != nil {
			return nil, fmt.Errorf("generate batch item id: %w", err)
		}
		items = append(items, task.BatchItem{ID: id, Key: value.Key, Input: value.Input,
			FileDeliveryIntent: value.FileDeliveryIntent, Status: task.BatchItemQueued, Attempt: 1})
	}
	return items, nil
}

func normalizedBatchInputs(values []BatchItemInput) []BatchItemInput {
	if values == nil {
		return []BatchItemInput{}
	}
	normalized := append([]BatchItemInput(nil), values...)
	for index := range normalized {
		normalized[index].FileDeliveryIntent = hashFileDeliveryIntent(normalized[index].FileDeliveryIntent)
	}
	return normalized
}

func hashFileDeliveryIntent(intent task.FileDeliveryIntent) task.FileDeliveryIntent {
	if intent == task.FileDeliveryNone {
		return ""
	}
	return intent
}

func truncateRunes(value string, maxRunes int) string {
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	return string([]rune(value)[:maxRunes])
}

func validTaskStatus(status task.Status) bool {
	switch status {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusPaused,
		task.StatusCancelling, task.StatusCompleted, task.StatusFailed, task.StatusCancelled:
		return true
	default:
		return false
	}
}

func validTaskEventType(value string) bool {
	switch value {
	case "task.status", "task.step", "task.operation", "task.message", "task.batch_item", "task.artifact", "task.waiting_user", "task.response", "task.result", "task.plan", "task.priority":
		return true
	default:
		return false
	}
}

func validTaskEventReadType(value string) bool {
	if validTaskEventType(value) {
		return true
	}
	if len(value) > 128 || !strings.HasPrefix(value, "task.") {
		return false
	}
	name := strings.TrimPrefix(value, "task.")
	if name == "" {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '.' && char != '-' {
			return false
		}
	}
	return true
}
