package app

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type projectWorkItemAccessStub struct {
	mu    sync.Mutex
	role  project.Role
	err   error
	calls int
}

func (s *projectWorkItemAccessStub) RequireAccess(_ context.Context, principal identity.Principal, projectID string, minimum project.Role) (project.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	current               project.WorkItem
	created               project.WorkItem
	page                  ProjectWorkItemPage
	listActor             string
	listProject           string
	listIncludeUnassigned bool
	listRequest           PageRequest
	listErr               error
	listCalls             int
	updates               int
	createReplay          bool
	mutation              ProjectWorkItemMutation
	mutationItem          project.WorkItem
	mutationReplay        bool
	mutationCalls         int
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

func (s *projectWorkItemStoreStub) ListProjectWorkItems(_ context.Context, actorID, projectID string, includeUnassigned bool, page PageRequest) (ProjectWorkItemPage, error) {
	s.listActor, s.listProject, s.listIncludeUnassigned, s.listRequest = actorID, projectID, includeUnassigned, page
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

	got, err := service.List(ctx, principal, "j7654321", true, PageRequest{Cursor: "before-page"})
	if err != nil || len(got.Items) != 1 || got.NextCursor != "next-page" || access.calls != 1 ||
		store.listCalls != 1 || store.listActor != principal.UserID || store.listProject != "j7654321" || !store.listIncludeUnassigned ||
		store.listRequest != (PageRequest{Limit: defaultPageSize, Cursor: "before-page"}) {
		t.Fatalf("List() = %#v, %v; access=%#v store=%#v", got, err, access, store)
	}

	access.err = domain.NewError(domain.CodeForbidden, "membership was revoked")
	if _, err := service.List(ctx, principal, "j7654321", false, PageRequest{}); domain.CodeOf(err) != domain.CodeForbidden || store.listCalls != 1 {
		t.Fatalf("revoked List() error=%v store calls=%d", err, store.listCalls)
	}

	access.err = nil
	store.page.Items[0].ProjectID = "j0000000"
	if _, err := service.List(ctx, principal, "j7654321", false, PageRequest{}); domain.CodeOf(err) != domain.CodeInternal || store.listCalls != 2 {
		t.Fatalf("cross-project Store record error=%v store calls=%d", err, store.listCalls)
	}
	store.page = ProjectWorkItemPage{Items: make([]project.WorkItem, defaultPageSize+1)}
	if _, err := service.List(ctx, principal, "j7654321", false, PageRequest{}); domain.CodeOf(err) != domain.CodeInternal || store.listCalls != 3 {
		t.Fatalf("oversized Store page error=%v store calls=%d", err, store.listCalls)
	}
}

func TestProjectWorkItemReadsRequireUnassignedOptIn(t *testing.T) {
	ctx := context.Background()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	item := project.WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "Unclaimed", Category: project.WorkItemCategoryUnclassified,
		State: project.WorkItemOpen, Version: 1,
	}
	access := &projectWorkItemAccessStub{role: project.RoleViewer}
	store := &projectWorkItemStoreStub{current: item, page: ProjectWorkItemPage{Items: []project.WorkItem{item}}}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, principal, item.ProjectID, item.ID, false); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("default Get() error = %v, want not found", err)
	}
	if got, err := service.Get(ctx, principal, item.ProjectID, item.ID, true); err != nil || got.ID != item.ID || got.AssigneeID != "" {
		t.Fatalf("opted-in Get() = %#v, %v", got, err)
	}
	if _, err := service.List(ctx, principal, item.ProjectID, false, PageRequest{}); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("default List() leaked unassigned item: %v", err)
	}
	if page, err := service.List(ctx, principal, item.ProjectID, true, PageRequest{}); err != nil || len(page.Items) != 1 || page.Items[0].ID != item.ID {
		t.Fatalf("opted-in List() = %#v, %v", page, err)
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
	if err != nil || replay || created.AssigneeID != principal.UserID || created.Category != project.WorkItemCategoryUnclassified || created.State != project.WorkItemOpen {
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

func TestProjectWorkItemCreateDefaultsCategoryAndSupportsUnassigned(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	access := &projectWorkItemAccessStub{role: project.RoleMember}
	store := &projectWorkItemStoreStub{}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := service.Create(context.Background(), principal, "j7654321", CreateProjectWorkItemCommand{
		Title: "Legacy request", IdempotencyKey: "create-category-01",
	})
	if err != nil || created.AssigneeID != principal.UserID || created.Category != project.WorkItemCategoryUnclassified {
		t.Fatalf("legacy create = %#v, %v", created, err)
	}
	unassigned, _, err := service.Create(context.Background(), principal, "j7654321", CreateProjectWorkItemCommand{
		Title: "Open bug", Category: project.WorkItemCategoryBug, Unassigned: true, IdempotencyKey: "create-unassigned-01",
	})
	if err != nil || unassigned.AssigneeID != "" || unassigned.Category != project.WorkItemCategoryBug {
		t.Fatalf("unassigned create = %#v, %v", unassigned, err)
	}
	if _, _, err := service.Create(context.Background(), principal, "j7654321", CreateProjectWorkItemCommand{
		Title: "Invalid mix", AssigneeID: "u1234567", Unassigned: true, IdempotencyKey: "create-unassigned-02",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unassigned with assignee = %v, want invalid argument", err)
	}
	if _, _, err := service.Create(context.Background(), principal, "j7654321", CreateProjectWorkItemCommand{
		Title: "Invalid category", Category: "defect", IdempotencyKey: "create-category-02",
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid category = %v, want invalid argument", err)
	}
}

func TestProjectWorkItemClaimRequiresMemberAndReplaysIdempotently(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	current := project.WorkItem{ID: "j1234567", ProjectID: "j7654321", Title: "Claim me",
		Category: project.WorkItemCategoryUnclassified, State: project.WorkItemOpen, Version: 1}
	access := &projectWorkItemAccessStub{role: project.RoleMember}
	store := &projectWorkItemStoreStub{current: current}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 1, "claim-work-item-01")
	if err != nil || claimed.AssigneeID != principal.UserID || claimed.Version != 2 || store.updates != 1 {
		t.Fatalf("claim = %#v, %v; store=%#v", claimed, err, store)
	}
	store.mutationReplay = true
	store.mutationItem = claimed
	replayed, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 1, "claim-work-item-01")
	if err != nil || replayed.Version != claimed.Version || store.updates != 1 || store.mutation.Operation != projectWorkItemUpdateOperation ||
		store.mutation.IdempotencyKey != "claim-work-item-01" || len(store.mutation.RequestDigest) != 32 {
		t.Fatalf("claim replay = %#v, %v; store=%#v", replayed, err, store)
	}
	store.mutationReplay = false
	if _, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 1, "claim-work-item-02"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale claim = %v, want conflict", err)
	}

	store.current.Version = 3
	store.current.AssigneeID = "u7654321"
	if _, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 3, "claim-work-item-03"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("claim over an existing assignee = %v, want conflict", err)
	}
	if store.updates != 1 {
		t.Fatalf("rejected claims wrote %d updates", store.updates-1)
	}

	access.role = project.RoleViewer
	if _, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 3, "claim-work-item-04"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("viewer claim = %v, want forbidden", err)
	}
	access.err = domain.NewError(domain.CodeNotFound, "not a member")
	if _, err := service.Claim(context.Background(), principal, current.ProjectID, current.ID, 3, "claim-work-item-05"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("non-member claim = %v, want not found", err)
	}
}

type projectWorkItemClaimRaceStore struct {
	mu        sync.Mutex
	current   project.WorkItem
	readCount int
	bothRead  chan struct{}
	mutations map[string]projectWorkItemMutationReplay
}

type projectWorkItemMutationReplay struct {
	digest []byte
	item   project.WorkItem
}

func (s *projectWorkItemClaimRaceStore) GetProjectWorkItem(_ context.Context, _, projectID, id string) (project.WorkItem, error) {
	s.mu.Lock()
	item := s.current
	if item.ID != id || item.ProjectID != projectID {
		s.mu.Unlock()
		return project.WorkItem{}, domain.NewError(domain.CodeNotFound, "not found")
	}
	s.readCount++
	ready := s.readCount >= 2
	if ready {
		close(s.bothRead)
	}
	barrier := s.bothRead
	s.mu.Unlock()
	<-barrier
	return item, nil
}

func (s *projectWorkItemClaimRaceStore) GetProjectWorkItemMutation(_ context.Context, actorID, projectID string, mutation ProjectWorkItemMutation) (project.WorkItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	replay, ok := s.mutations[actorID+":"+mutation.IdempotencyKey]
	if !ok {
		return project.WorkItem{}, false, nil
	}
	if !bytes.Equal(replay.digest, mutation.RequestDigest) {
		return project.WorkItem{}, false, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused")
	}
	return replay.item, true, nil
}

func (s *projectWorkItemClaimRaceStore) ListProjectWorkItems(context.Context, string, string, bool, PageRequest) (ProjectWorkItemPage, error) {
	return ProjectWorkItemPage{Items: []project.WorkItem{}}, nil
}

func (s *projectWorkItemClaimRaceStore) CreateProjectWorkItem(context.Context, string, project.WorkItem, string) (project.WorkItem, bool, error) {
	return project.WorkItem{}, false, nil
}

func (s *projectWorkItemClaimRaceStore) UpdateProjectWorkItem(_ context.Context, actorID string, item project.WorkItem, expectedVersion int64, mutation ProjectWorkItemMutation) (project.WorkItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := actorID + ":" + mutation.IdempotencyKey
	if replay, ok := s.mutations[key]; ok {
		if !bytes.Equal(replay.digest, mutation.RequestDigest) {
			return project.WorkItem{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused")
		}
		return s.current, nil
	}
	if s.current.Version != expectedVersion {
		return project.WorkItem{}, domain.NewError(domain.CodeConflict, "version changed")
	}
	s.current = item
	s.mutations[key] = projectWorkItemMutationReplay{digest: append([]byte(nil), mutation.RequestDigest...), item: item}
	return item, nil
}

func TestConcurrentProjectWorkItemClaimsUseVersionCAS(t *testing.T) {
	workItem := project.WorkItem{ID: "j1234567", ProjectID: "j7654321", Title: "Competing claim",
		Category: project.WorkItemCategoryBug, State: project.WorkItemOpen, Version: 1}
	store := &projectWorkItemClaimRaceStore{current: workItem, bothRead: make(chan struct{}), mutations: map[string]projectWorkItemMutationReplay{}}
	access := &projectWorkItemAccessStub{role: project.RoleMember}
	service, err := NewProjectWorkItemService(store, access, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, actorID := range []string{"u1234567", "u7654321"} {
		actor := identity.Principal{UserID: actorID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
		go func(principal identity.Principal) {
			_, err := service.Claim(context.Background(), principal, workItem.ProjectID, workItem.ID, 1, "claim-race-"+principal.UserID)
			results <- err
		}(actor)
	}
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || (first != nil && domain.CodeOf(first) != domain.CodeConflict) ||
		(second != nil && domain.CodeOf(second) != domain.CodeConflict) {
		t.Fatalf("concurrent claims = %v, %v; exactly one should win and the other conflict", first, second)
	}
	if store.current.Version != 2 || (store.current.AssigneeID != "u1234567" && store.current.AssigneeID != "u7654321") {
		t.Fatalf("race result = %#v", store.current)
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
