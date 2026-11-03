package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectWorkItemAccessStub struct {
	role  project.Role
	err   error
	calls int
}

func (s *projectWorkItemAccessStub) RequireAccess(_ context.Context, principal identity.Principal, projectID string, minimum project.Role) (project.Member, error) {
	s.calls++
	if s.err != nil {
		return project.Member{}, s.err
	}
	if !project.RoleAtLeast(s.role, minimum) {
		return project.Member{}, domain.NewError(domain.CodeForbidden, "denied")
	}
	return project.Member{ProjectID: projectID, UserID: principal.UserID, Role: s.role, Version: 1}, nil
}

type projectWorkItemStoreStub struct {
	current        project.WorkItem
	created        project.WorkItem
	page           ProjectWorkItemPage
	listActor      string
	listProject    string
	listRequest    PageRequest
	listErr        error
	listCalls      int
	updates        int
	createReplay   bool
	mutation       ProjectWorkItemMutation
	mutationItem   project.WorkItem
	mutationReplay bool
	mutationCalls  int
}

func (s *projectWorkItemStoreStub) GetProjectWorkItem(_ context.Context, _, projectID, id string) (project.WorkItem, error) {
	if s.current.ID != id || s.current.ProjectID != projectID {
		return project.WorkItem{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	return s.current, nil
}

func (s *projectWorkItemStoreStub) GetProjectWorkItemMutation(_ context.Context, _, _ string, mutation ProjectWorkItemMutation) (project.WorkItem, bool, error) {
	s.mutation, s.mutationCalls = mutation, s.mutationCalls+1
	if s.mutationReplay {
		return s.mutationItem, true, nil
	}
	return project.WorkItem{}, false, nil
}

func (s *projectWorkItemStoreStub) ListProjectWorkItems(_ context.Context, actorID, projectID string, page PageRequest) (ProjectWorkItemPage, error) {
	s.listActor, s.listProject, s.listRequest = actorID, projectID, page
	s.listCalls++
	return s.page, s.listErr
}

func (s *projectWorkItemStoreStub) CreateProjectWorkItem(_ context.Context, _ string, item project.WorkItem, _ string) (project.WorkItem, bool, error) {
	s.created = item
	s.current = item
	return item, s.createReplay, nil
}

func (s *projectWorkItemStoreStub) UpdateProjectWorkItem(_ context.Context, _ string, item project.WorkItem, expectedVersion int64, _ ProjectWorkItemMutation) (project.WorkItem, error) {
	if s.current.Version != expectedVersion {
		return project.WorkItem{}, domain.NewError(domain.CodeConflict, "conflict")
	}
	s.current = item
	s.updates++
	return item, nil
}

func TestProjectWorkItemListRequiresMembershipAndValidatesStorePage(t *testing.T) {
	ctx := context.Background()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	access := &projectWorkItemAccessStub{role: project.RoleViewer}
	store := &projectWorkItemStoreStub{page: ProjectWorkItemPage{
		Items: []project.WorkItem{{
			ID: "j1234567", ProjectID: "j7654321", Title: "交付报告", AssigneeID: "u1234567",
			State: project.WorkItemOpen, Version: 1,
		}}, NextCursor: "next-page",
	}}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.List(ctx, principal, "j7654321", PageRequest{Cursor: "before-page"})
	if err != nil || len(got.Items) != 1 || got.NextCursor != "next-page" || access.calls != 1 ||
		store.listCalls != 1 || store.listActor != principal.UserID || store.listProject != "j7654321" ||
		store.listRequest != (PageRequest{Limit: defaultPageSize, Cursor: "before-page"}) {
		t.Fatalf("List() = %#v, %v; access=%#v store=%#v", got, err, access, store)
	}

	access.err = domain.NewError(domain.CodeForbidden, "membership was revoked")
	if _, err := service.List(ctx, principal, "j7654321", PageRequest{}); domain.CodeOf(err) != domain.CodeForbidden || store.listCalls != 1 {
		t.Fatalf("revoked List() error=%v store calls=%d", err, store.listCalls)
	}

	access.err = nil
	store.page.Items[0].ProjectID = "j0000000"
	if _, err := service.List(ctx, principal, "j7654321", PageRequest{}); domain.CodeOf(err) != domain.CodeInternal || store.listCalls != 2 {
		t.Fatalf("cross-project Store record error=%v store calls=%d", err, store.listCalls)
	}
	store.page = ProjectWorkItemPage{Items: make([]project.WorkItem, defaultPageSize+1)}
	if _, err := service.List(ctx, principal, "j7654321", PageRequest{}); domain.CodeOf(err) != domain.CodeInternal || store.listCalls != 3 {
		t.Fatalf("oversized Store page error=%v store calls=%d", err, store.listCalls)
	}
}

type projectTaskLinkAuthorizerStub struct {
	calls int
	err   error
}

func (s *projectTaskLinkAuthorizerStub) AuthorizeProjectTaskLink(context.Context, identity.Principal, string, string) error {
	s.calls++
	return s.err
}

type projectDeliveryAuthorizerStub struct {
	calls int
	err   error
}

func (s *projectDeliveryAuthorizerStub) AuthorizeSavedProjectDelivery(context.Context, identity.Principal, string, project.WorkItemDelivery) error {
	s.calls++
	return s.err
}

func TestProjectWorkItemLifecycleChecksAccessAndSavedDelivery(t *testing.T) {
	ctx := context.Background()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	access := &projectWorkItemAccessStub{role: project.RoleMember}
	store := &projectWorkItemStoreStub{}
	links := &projectTaskLinkAuthorizerStub{}
	deliveries := &projectDeliveryAuthorizerStub{}
	service, err := NewProjectWorkItemService(store, access, links, deliveries)
	if err != nil {
		t.Fatal(err)
	}
	created, replay, err := service.Create(ctx, principal, "j7654321", CreateProjectWorkItemCommand{
		Title: "交付报告", AcceptanceCriteria: []string{"已保存并可读取"}, IdempotencyKey: "work-item-create-01",
	})
	if err != nil || replay || created.AssigneeID != principal.UserID || created.State != project.WorkItemOpen {
		t.Fatalf("create = %#v, replay=%v, err=%v", created, replay, err)
	}
	taskID := "q1234567"
	linked, err := service.LinkTask(ctx, principal, created.ProjectID, created.ID, taskID, created.Version, "work-item-task-001")
	if err != nil || links.calls != 1 || len(linked.TaskIDs) != 1 {
		t.Fatalf("link task = %#v, calls=%d, err=%v", linked, links.calls, err)
	}
	blocked, err := service.Block(ctx, principal, linked.ProjectID, linked.ID, "缺少审批", linked.Version, "work-item-block-001")
	if err != nil || blocked.State != project.WorkItemBlocked {
		t.Fatalf("block = %#v, %v", blocked, err)
	}
	store.mutationReplay, store.mutationItem = true, blocked
	replayedBlock, err := service.Block(ctx, principal, blocked.ProjectID, blocked.ID, "缺少审批", linked.Version, "work-item-block-001")
	if err != nil || replayedBlock.Version != blocked.Version || replayedBlock.State != project.WorkItemBlocked || store.updates != 2 ||
		store.mutation.Operation != projectWorkItemUpdateOperation || store.mutation.WorkItemID != blocked.ID ||
		store.mutation.IdempotencyKey != "work-item-block-001" || len(store.mutation.RequestDigest) != 32 {
		t.Fatalf("idempotent block replay = %#v, %v; store=%#v", replayedBlock, err, store)
	}
	store.mutationReplay = false
	resumed, err := service.Resume(ctx, principal, blocked.ProjectID, blocked.ID, blocked.Version, "work-item-resume-01")
	if err != nil || resumed.State != project.WorkItemOpen {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	delivery := project.WorkItemDelivery{TaskID: taskID, WorkspaceID: "w1234567", Revision: 4}
	delivered, err := service.Deliver(ctx, principal, resumed.ProjectID, resumed.ID, delivery, resumed.Version, "work-item-deliver-01")
	if err != nil || deliveries.calls != 1 || delivered.Delivery == nil || delivered.Delivery.Revision != 4 {
		t.Fatalf("deliver = %#v, calls=%d, err=%v", delivered, deliveries.calls, err)
	}
	if access.calls != 6 || store.updates != 4 || store.mutationCalls != 5 {
		t.Fatalf("access calls=%d, store updates=%d", access.calls, store.updates)
	}
}

func TestProjectWorkItemTaskAndDeliveryFailClosedWithoutValidators(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	access := &projectWorkItemAccessStub{role: project.RoleMember}
	store := &projectWorkItemStoreStub{current: project.WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "交付报告", AssigneeID: principal.UserID,
		State: project.WorkItemOpen, Version: 1,
	}}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LinkTask(context.Background(), principal, store.current.ProjectID, store.current.ID, "q1234567", 1, "work-item-task-002"); err == nil {
		t.Fatal("task link was accepted without an authorizer")
	}
	if _, err := service.Deliver(context.Background(), principal, store.current.ProjectID, store.current.ID,
		project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 1}, 1, "work-item-deliver-02"); err == nil {
		t.Fatal("delivery was accepted without a verifier")
	}
	if store.updates != 0 {
		t.Fatalf("unauthorized requests wrote %d updates", store.updates)
	}
}
