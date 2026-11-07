package app

import (
	"context"
	"errors"
	"math"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ProjectWorkItemStore interface {
	// Mutations must recheck current membership, assignee membership, Task/Workspace scope, expectedVersion, and idempotency in the write transaction.
	GetProjectWorkItem(context.Context, string, string, string) (project.WorkItem, error)
	// GetProjectWorkItemMutation checks the idempotency key before a state transition can reject an exact retry.
	GetProjectWorkItemMutation(context.Context, string, string, ProjectWorkItemMutation) (project.WorkItem, bool, error)
	// ListProjectWorkItems must recheck active membership and use a stable cursor scoped to projectID.
	ListProjectWorkItems(context.Context, string, string, bool, PageRequest) (ProjectWorkItemPage, error)
	CreateProjectWorkItem(context.Context, string, project.WorkItem, string) (project.WorkItem, bool, error)
	UpdateProjectWorkItem(context.Context, string, project.WorkItem, int64, ProjectWorkItemMutation) (project.WorkItem, error)
}

type ProjectWorkItemMutation struct {
	Operation      string
	WorkItemID     string
	IdempotencyKey string
	RequestDigest  []byte
}

const projectWorkItemUpdateOperation = "update_work_item"

type ProjectWorkItemPage struct {
	Items      []project.WorkItem
	NextCursor string
}

type ProjectTaskLinkAuthorizer interface {
	AuthorizeProjectTaskLink(context.Context, identity.Principal, string, string) error
}

type ProjectDeliveryAuthorizer interface {
	AuthorizeSavedProjectDelivery(context.Context, identity.Principal, string, project.WorkItemDelivery) error
}

type ProjectWorkItemService struct {
	store      ProjectWorkItemStore
	access     ProjectAccessChecker
	taskLinks  ProjectTaskLinkAuthorizer
	deliveries ProjectDeliveryAuthorizer
}

func NewProjectWorkItemService(store ProjectWorkItemStore, access ProjectAccessChecker, taskLinks ProjectTaskLinkAuthorizer, deliveries ProjectDeliveryAuthorizer) (*ProjectWorkItemService, error) {
	if store == nil || access == nil {
		return nil, errors.New("project work item dependencies are required")
	}
	return &ProjectWorkItemService{store: store, access: access, taskLinks: taskLinks, deliveries: deliveries}, nil
}

type CreateProjectWorkItemCommand struct {
	Title              string
	Description        string
	AcceptanceCriteria []string
	Category           project.WorkItemCategory
	AssigneeID         string
	Unassigned         bool
	IdempotencyKey     string
}

func (s *ProjectWorkItemService) List(ctx context.Context, principal identity.Principal, projectID string, includeUnassigned bool, page PageRequest) (ProjectWorkItemPage, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectWorkItemPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectWorkItemPage{}, err
	}
	result, err := s.store.ListProjectWorkItems(ctx, principal.UserID, projectID, includeUnassigned, page)
	if err != nil {
		return ProjectWorkItemPage{}, err
	}
	if result.Items == nil || len(result.Items) > int(page.Limit) || len(result.NextCursor) > 1024 {
		return ProjectWorkItemPage{}, domain.NewError(domain.CodeInternal, "project work item store returned an invalid page")
	}
	seen := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		if item.ProjectID != projectID || item.Validate() != nil || (item.AssigneeID == "" && !includeUnassigned) {
			return ProjectWorkItemPage{}, domain.NewError(domain.CodeInternal, "project work item store returned an invalid record")
		}
		if _, exists := seen[item.ID]; exists {
			return ProjectWorkItemPage{}, domain.NewError(domain.CodeInternal, "project work item store returned duplicate records")
		}
		seen[item.ID] = struct{}{}
	}
	return result, nil
}

func (s *ProjectWorkItemService) Create(ctx context.Context, principal identity.Principal, projectID string, command CreateProjectWorkItemCommand) (project.WorkItem, bool, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return project.WorkItem{}, false, err
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return project.WorkItem{}, false, err
	}
	assigneeID := command.AssigneeID
	if command.Unassigned && assigneeID != "" {
		return project.WorkItem{}, false, domain.InvalidArgument("unassigned", "unassigned work items cannot include an assignee")
	}
	if command.Unassigned {
		assigneeID = ""
	} else if assigneeID == "" {
		assigneeID = principal.UserID
	}
	category := command.Category
	if category == "" {
		category = project.WorkItemCategoryUnclassified
	}
	if !category.Valid() || category == "" {
		return project.WorkItem{}, false, domain.InvalidArgument("category", "project work item category is invalid")
	}
	id, err := project.NewID()
	if err != nil {
		return project.WorkItem{}, false, err
	}
	item := project.WorkItem{
		ID: id, ProjectID: projectID, Title: command.Title, Description: command.Description,
		AcceptanceCriteria: append([]string(nil), command.AcceptanceCriteria...), Category: category, AssigneeID: assigneeID,
		State: project.WorkItemOpen, Version: 1,
	}
	if err := item.Validate(); err != nil {
		return project.WorkItem{}, false, domain.InvalidArgument("work_item", err.Error())
	}
	result, replay, err := s.store.CreateProjectWorkItem(ctx, principal.UserID, item, command.IdempotencyKey)
	return result, replay, err
}

func (s *ProjectWorkItemService) Get(ctx context.Context, principal identity.Principal, projectID, workItemID string, includeUnassigned bool) (project.WorkItem, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return project.WorkItem{}, err
	}
	if !project.ValidID(workItemID) {
		return project.WorkItem{}, domain.NewError(domain.CodeNotFound, "project work item not found")
	}
	item, err := s.store.GetProjectWorkItem(ctx, principal.UserID, projectID, workItemID)
	if err != nil {
		return project.WorkItem{}, err
	}
	if item.ID != workItemID || item.ProjectID != projectID || item.Validate() != nil {
		return project.WorkItem{}, domain.NewError(domain.CodeInternal, "project work item store returned an invalid record")
	}
	if item.AssigneeID == "" && !includeUnassigned {
		return project.WorkItem{}, domain.NewError(domain.CodeNotFound, "project work item not found")
	}
	return item, nil
}

func (s *ProjectWorkItemService) Assign(ctx context.Context, principal identity.Principal, projectID, workItemID, assigneeID string, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey,
		struct{ AssigneeID string }{assigneeID})
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		return item, nil
	}
	if !domain.ValidUserID(assigneeID) {
		return project.WorkItem{}, domain.InvalidArgument("assignee_id", "project work item assignee is invalid")
	}
	item.AssigneeID = assigneeID
	return s.save(ctx, principal, item, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) Claim(ctx context.Context, principal identity.Principal, projectID, workItemID string, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey,
		struct{ Claim bool }{true})
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		return item, nil
	}
	if item.AssigneeID != "" {
		return project.WorkItem{}, domain.NewError(domain.CodeConflict, "project work item is already assigned")
	}
	item.AssigneeID = principal.UserID
	return s.save(ctx, principal, item, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) Block(ctx context.Context, principal identity.Principal, projectID, workItemID, reason string, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey,
		struct{ BlockedReason string }{reason})
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		return item, nil
	}
	updated, err := item.Block(reason)
	if err != nil {
		return project.WorkItem{}, domain.InvalidArgument("blocked_reason", err.Error())
	}
	return s.save(ctx, principal, updated, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) Resume(ctx context.Context, principal identity.Principal, projectID, workItemID string, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey, struct{}{})
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		return item, nil
	}
	updated, err := item.Resume()
	if err != nil {
		return project.WorkItem{}, domain.NewError(domain.CodeConflict, "project work item is not blocked")
	}
	return s.save(ctx, principal, updated, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) LinkTask(ctx context.Context, principal identity.Principal, projectID, workItemID, taskID string, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey,
		struct{ TaskID string }{taskID})
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		if s.taskLinks == nil {
			return project.WorkItem{}, domain.NewError(domain.CodeUnsupported, "project task authorization is unavailable")
		}
		if err := s.taskLinks.AuthorizeProjectTaskLink(ctx, principal, projectID, taskID); err != nil {
			return project.WorkItem{}, err
		}
		return item, nil
	}
	updated, err := item.LinkTask(taskID)
	if err != nil {
		return project.WorkItem{}, domain.InvalidArgument("task_id", err.Error())
	}
	if s.taskLinks == nil {
		return project.WorkItem{}, domain.NewError(domain.CodeUnsupported, "project task authorization is unavailable")
	}
	if err := s.taskLinks.AuthorizeProjectTaskLink(ctx, principal, projectID, taskID); err != nil {
		return project.WorkItem{}, err
	}
	return s.save(ctx, principal, updated, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) Deliver(ctx context.Context, principal identity.Principal, projectID, workItemID string, evidence project.WorkItemDelivery, expectedVersion int64, idempotencyKey string) (project.WorkItem, error) {
	item, mutation, replay, err := s.loadMutable(ctx, principal, projectID, workItemID, expectedVersion, idempotencyKey, evidence)
	if err != nil {
		return project.WorkItem{}, err
	}
	if replay {
		if s.deliveries == nil {
			return project.WorkItem{}, domain.NewError(domain.CodeUnsupported, "workspace delivery verification is unavailable")
		}
		if err := s.deliveries.AuthorizeSavedProjectDelivery(ctx, principal, projectID, evidence); err != nil {
			return project.WorkItem{}, err
		}
		return item, nil
	}
	updated, err := item.Deliver(evidence)
	if err != nil {
		return project.WorkItem{}, domain.InvalidArgument("delivery", err.Error())
	}
	if s.deliveries == nil {
		return project.WorkItem{}, domain.NewError(domain.CodeUnsupported, "workspace delivery verification is unavailable")
	}
	if err := s.deliveries.AuthorizeSavedProjectDelivery(ctx, principal, projectID, evidence); err != nil {
		return project.WorkItem{}, err
	}
	return s.save(ctx, principal, updated, expectedVersion, mutation)
}

func (s *ProjectWorkItemService) loadMutable(ctx context.Context, principal identity.Principal, projectID, workItemID string, expectedVersion int64, idempotencyKey string, request any) (project.WorkItem, ProjectWorkItemMutation, bool, error) {
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, err
	}
	if !project.ValidID(workItemID) || expectedVersion < 1 || expectedVersion == math.MaxInt64 {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, domain.InvalidArgument("work_item", "project work item request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, err
	}
	digest, err := hashIdempotencyRequest(struct {
		ActorID         string
		ProjectID       string
		WorkItemID      string
		Operation       string
		ExpectedVersion int64
		Request         any
	}{principal.UserID, projectID, workItemID, projectWorkItemUpdateOperation, expectedVersion, request})
	if err != nil {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, err
	}
	mutation := ProjectWorkItemMutation{
		Operation: projectWorkItemUpdateOperation, WorkItemID: workItemID,
		IdempotencyKey: idempotencyKey, RequestDigest: digest,
	}
	item, replay, err := s.store.GetProjectWorkItemMutation(ctx, principal.UserID, projectID, mutation)
	if err != nil {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, err
	}
	if replay {
		if item.ID != workItemID || item.ProjectID != projectID || item.Validate() != nil {
			return project.WorkItem{}, ProjectWorkItemMutation{}, false, domain.NewError(domain.CodeInternal, "project work item store returned an invalid idempotency result")
		}
		return item, mutation, true, nil
	}
	item, err = s.store.GetProjectWorkItem(ctx, principal.UserID, projectID, workItemID)
	if err != nil {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, err
	}
	if item.ProjectID != projectID || item.ID != workItemID {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, domain.NewError(domain.CodeNotFound, "project work item not found")
	}
	if item.Version != expectedVersion {
		return project.WorkItem{}, ProjectWorkItemMutation{}, false, domain.NewError(domain.CodeConflict, "project work item version changed")
	}
	return item, mutation, false, nil
}

func (s *ProjectWorkItemService) save(ctx context.Context, principal identity.Principal, item project.WorkItem, expectedVersion int64, mutation ProjectWorkItemMutation) (project.WorkItem, error) {
	item.Version = expectedVersion + 1
	if err := item.Validate(); err != nil {
		return project.WorkItem{}, domain.InvalidArgument("work_item", err.Error())
	}
	return s.store.UpdateProjectWorkItem(ctx, principal.UserID, item, expectedVersion, mutation)
}
