package app

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

const (
	projectBudgetTestID     = "j1234567"
	projectBudgetTestOwner  = "u1234567"
	projectBudgetTestMember = "u7654321"
)

type projectBudgetAccessStub struct {
	mu        sync.Mutex
	member    project.Member
	err       error
	minimum   project.Role
	principal identity.Principal
	projectID string
	calls     int
}

func (s *projectBudgetAccessStub) RequireAccess(_ context.Context, principal identity.Principal, projectID string, minimum project.Role) (project.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.principal, s.projectID, s.minimum = principal, projectID, minimum
	if s.err != nil {
		return project.Member{}, s.err
	}
	if s.member.ProjectID != projectID || s.member.UserID != principal.UserID || !project.RoleAtLeast(s.member.Role, minimum) {
		return project.Member{}, domain.NewError(domain.CodeForbidden, "project role is insufficient")
	}
	return s.member, nil
}

type projectBudgetStoreStub struct {
	view                ProjectBudgetView
	getErr              error
	setErr              error
	reserveErr          error
	settleErr           error
	setActor            string
	setProjectID        string
	setBudget           project.Budget
	setExpected         int64
	setKey              string
	setCalls            int
	reservation         ProjectModelReservation
	reservationOverride *ProjectModelReservation
	reserveReplay       bool
	reserveCalls        int
	settlement          SettleProjectModelCallCommand
	settleCalls         int
	ledgerPage          ProjectLedgerPage
	ledgerErr           error
	ledgerActor         string
	ledgerProjectID     string
	ledgerRequest       PageRequest
	ledgerCalls         int
}

func (s *projectBudgetStoreStub) GetProjectBudget(_ context.Context, actorID, projectID string) (ProjectBudgetView, error) {
	if s.getErr != nil {
		return ProjectBudgetView{}, s.getErr
	}
	if actorID == "" || projectID == "" {
		return ProjectBudgetView{}, domain.NewError(domain.CodeNotFound, "project budget not found")
	}
	return s.view, nil
}

func (s *projectBudgetStoreStub) SetProjectBudget(_ context.Context, actorID, projectID string, budget project.Budget, expectedVersion int64, key string) (ProjectBudgetView, error) {
	s.setCalls++
	s.setActor, s.setProjectID, s.setBudget, s.setExpected, s.setKey = actorID, projectID, budget, expectedVersion, key
	if s.setErr != nil {
		return ProjectBudgetView{}, s.setErr
	}
	s.view = ProjectBudgetView{Configured: true, Budget: budget, SpentMicros: s.view.SpentMicros, ReservedMicros: s.view.ReservedMicros, HasUnknown: s.view.HasUnknown}
	return s.view, nil
}

func (s *projectBudgetStoreStub) ListProjectLedgerEntries(_ context.Context, actorID, projectID string, page PageRequest) (ProjectLedgerPage, error) {
	s.ledgerCalls++
	s.ledgerActor, s.ledgerProjectID, s.ledgerRequest = actorID, projectID, page
	return s.ledgerPage, s.ledgerErr
}

func (s *projectBudgetStoreStub) ReserveProjectModelCall(_ context.Context, reservation ProjectModelReservation) (ProjectModelReservation, bool, error) {
	s.reserveCalls++
	s.reservation = reservation
	if s.reserveErr != nil {
		return ProjectModelReservation{}, false, s.reserveErr
	}
	if s.reservationOverride != nil {
		value := *s.reservationOverride
		value.RequestDigest = append([]byte(nil), reservation.RequestDigest...)
		return value, s.reserveReplay, nil
	}
	if s.reservation.RequestDigest == nil {
		s.reservation.RequestDigest = append([]byte(nil), reservation.RequestDigest...)
	}
	return s.reservation, s.reserveReplay, nil
}

func (s *projectBudgetStoreStub) SettleProjectModelCall(_ context.Context, settlement SettleProjectModelCallCommand) error {
	s.settleCalls++
	s.settlement = settlement
	return s.settleErr
}

type projectBudgetRateStub struct {
	mu    sync.Mutex
	rate  project.RateSnapshot
	err   error
	model string
	calls int
}

func (s *projectBudgetRateStub) ProjectRateSnapshot(_ context.Context, modelID string) (project.RateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.model = modelID
	return s.rate, s.err
}

func projectBudgetServiceFixture(t *testing.T, role project.Role) (*ProjectBudgetService, *projectBudgetStoreStub, *projectBudgetAccessStub, *projectBudgetRateStub) {
	t.Helper()
	store := &projectBudgetStoreStub{view: ProjectBudgetView{
		Configured: true, Budget: project.Budget{Currency: "USD", LimitMicros: 10_000_000, Version: 2},
	}}
	access := &projectBudgetAccessStub{member: project.Member{ProjectID: projectBudgetTestID, UserID: projectBudgetTestMember, Role: role, Version: 1}}
	rates := &projectBudgetRateStub{rate: project.RateSnapshot{
		Provider: "openai", ModelID: "p1234567", Model: "gpt-4o", Currency: "USD", Version: 7,
		InputMicrosPerMillion: 2_000_000, OutputMicrosPerMillion: 4_000_000,
	}}
	service, err := NewProjectBudgetService(store, access, rates)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, access, rates
}

func projectBudgetPrincipal(userID string) identity.Principal {
	return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}

func projectBudgetAccountReservation(principal identity.Principal, command ReserveProjectModelCallCommand) ModelCallReservation {
	call := ModelCallReservation{
		Source: command.Source, Principal: principal, ProjectID: command.ProjectID, OwnerID: principal.UserID,
		ModelID: command.ModelID, ProviderModel: "gpt-4o", CallID: command.CallID,
		RequestDigest: make([]byte, 32), ReservedTokens: command.Usage.InputTokens + command.Usage.OutputTokens,
	}
	if command.Source == ModelCallTask {
		call.Principal = identity.Principal{}
		call.Task = &ModelTaskFence{TaskID: command.TaskID, OwnerID: principal.UserID, InitiatorID: principal.UserID}
	}
	return call
}

func withProjectBudgetAccountReservation(principal identity.Principal, command ReserveProjectModelCallCommand) ReserveProjectModelCallCommand {
	command.AccountCall = projectBudgetAccountReservation(principal, command)
	return command
}

func TestProjectBudgetGetAndSetUseFreshMembershipRoles(t *testing.T) {
	service, store, access, _ := projectBudgetServiceFixture(t, project.RoleViewer)
	viewer := projectBudgetPrincipal(projectBudgetTestMember)
	got, err := service.GetBudget(context.Background(), viewer, projectBudgetTestID)
	if err != nil || !got.Configured || got.Budget.Currency != "USD" || access.minimum != project.RoleViewer || access.calls != 1 {
		t.Fatalf("GetBudget() = %#v, %v; access=%#v", got, err, access)
	}
	if _, err := service.SetBudget(context.Background(), viewer, projectBudgetTestID, SetProjectBudgetCommand{
		Currency: "USD", LimitMicros: 20_000_000, ExpectedVersion: 2, IdempotencyKey: "budget-update-2",
	}); domain.CodeOf(err) != domain.CodeForbidden || store.setCalls != 0 {
		t.Fatalf("viewer SetBudget() error=%v, store calls=%d", err, store.setCalls)
	}

	access.member.Role = project.RoleOwner
	owner := projectBudgetPrincipal(projectBudgetTestOwner)
	access.member.UserID = owner.UserID
	got, err = service.SetBudget(context.Background(), owner, projectBudgetTestID, SetProjectBudgetCommand{
		Currency: "EUR", LimitMicros: 35_000_000, ExpectedVersion: 2, IdempotencyKey: "budget-update-3",
	})
	if err != nil || !got.Configured || got.Budget != (project.Budget{Currency: "EUR", LimitMicros: 35_000_000, Version: 3}) ||
		store.setCalls != 1 || store.setExpected != 2 || store.setKey != "budget-update-3" || access.minimum != project.RoleOwner {
		t.Fatalf("owner SetBudget() = %#v, %v; store=%#v access=%#v", got, err, store, access)
	}
}

func TestProjectLedgerListingRequiresFreshMemberAccessAndKeepsOnlyTraceMetadata(t *testing.T) {
	service, store, access, _ := projectBudgetServiceFixture(t, project.RoleViewer)
	store.ledgerPage = ProjectLedgerPage{Items: []ProjectLedgerEntry{{
		CallID: "task-call-0001", ActorID: projectBudgetTestMember, TaskID: "q1234567", Source: ModelCallTask,
		ModelID: "p1234567", RateVersion: 3, Currency: "USD", Status: ProjectLedgerUnknown,
		ReservedUsage: project.Usage{InputTokens: 100, OutputTokens: 20}, ReservedMicros: 500,
		CreatedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
	}}, NextCursor: "next-page"}
	viewer := projectBudgetPrincipal(projectBudgetTestMember)
	got, err := service.ListLedger(context.Background(), viewer, projectBudgetTestID, PageRequest{Limit: 10, Cursor: "before"})
	if err != nil || len(got.Items) != 1 || got.Items[0].ActorID != viewer.UserID || got.Items[0].TaskID != "q1234567" ||
		got.Items[0].Status != ProjectLedgerUnknown || got.Items[0].ActualMicros != nil || got.NextCursor != "next-page" ||
		access.minimum != project.RoleViewer || store.ledgerActor != viewer.UserID || store.ledgerProjectID != projectBudgetTestID ||
		store.ledgerRequest != (PageRequest{Limit: 10, Cursor: "before"}) {
		t.Fatalf("ListLedger() = %#v, %v; store=%#v access=%#v", got, err, store, access)
	}
	access.err = domain.NewError(domain.CodeForbidden, "project membership was revoked")
	if _, err := service.ListLedger(context.Background(), viewer, projectBudgetTestID, PageRequest{}); domain.CodeOf(err) != domain.CodeForbidden || store.ledgerCalls != 1 {
		t.Fatalf("revoked ListLedger() error=%v calls=%d", err, store.ledgerCalls)
	}
}

func TestProjectLedgerRejectsEntriesOutsideSafeSchema(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	store.ledgerPage = ProjectLedgerPage{Items: []ProjectLedgerEntry{{
		CallID: "chat-call-0001", ActorID: projectBudgetTestMember, TaskID: "q1234567", Source: ModelCallChat,
		ModelID: "p1234567", RateVersion: 1, Currency: "USD", Status: ProjectLedgerSettled,
		ReservedUsage: project.Usage{InputTokens: 1}, ActualUsage: &project.Usage{InputTokens: 1},
		ReservedMicros: 1, ActualMicros: projectBudgetInt64Pointer(1), CreatedAt: time.Now(),
	}}}
	if _, err := service.ListLedger(context.Background(), projectBudgetPrincipal(projectBudgetTestMember), projectBudgetTestID, PageRequest{}); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("ledger entry with a Task on a non-Task source was accepted: %v", err)
	}
}

func TestProjectLedgerRejectsOversizedStorePage(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleViewer)
	store.ledgerPage = ProjectLedgerPage{Items: make([]ProjectLedgerEntry, 2)}
	if _, err := service.ListLedger(context.Background(), projectBudgetPrincipal(projectBudgetTestMember), projectBudgetTestID, PageRequest{Limit: 1}); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("oversized project ledger page error=%v", err)
	}
}

func TestProjectBudgetSetRejectsInvalidValuesBeforeStore(t *testing.T) {
	service, store, access, _ := projectBudgetServiceFixture(t, project.RoleOwner)
	access.member.UserID = projectBudgetTestOwner
	owner := projectBudgetPrincipal(projectBudgetTestOwner)
	for _, command := range []SetProjectBudgetCommand{
		{Currency: "usd", LimitMicros: 1, ExpectedVersion: 0, IdempotencyKey: "budget-key-1"},
		{Currency: "USD", LimitMicros: 0, ExpectedVersion: 0, IdempotencyKey: "budget-key-2"},
		{Currency: "USD", LimitMicros: 1, ExpectedVersion: -1, IdempotencyKey: "budget-key-3"},
		{Currency: "USD", LimitMicros: 1, ExpectedVersion: math.MaxInt64, IdempotencyKey: "budget-key-4"},
		{Currency: "USD", LimitMicros: 1, ExpectedVersion: 0, IdempotencyKey: "short"},
	} {
		if _, err := service.SetBudget(context.Background(), owner, projectBudgetTestID, command); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("SetBudget(%+v) error=%v", command, err)
		}
	}
	if store.setCalls != 0 {
		t.Fatalf("invalid budget reached Store %d times", store.setCalls)
	}
}

func TestProjectBudgetReserveUsesServerRateSnapshotAndStableRequestDigest(t *testing.T) {
	service, store, access, rates := projectBudgetServiceFixture(t, project.RoleMember)
	principal := projectBudgetPrincipal(projectBudgetTestMember)
	command := ReserveProjectModelCallCommand{
		ProjectID: projectBudgetTestID, TaskID: "q1234567", Source: ModelCallTask, CallID: "task-call-0001",
		ModelID: "p1234567", Usage: project.Usage{InputTokens: 1_000_000, OutputTokens: 2_000_000},
	}
	command = withProjectBudgetAccountReservation(principal, command)
	store.reserveReplay = true
	got, replay, err := service.ReserveModelCall(context.Background(), principal, command)
	if err != nil || !replay || got.ProjectID != projectBudgetTestID || got.ActorID != principal.UserID || got.TaskID != command.TaskID ||
		got.Source != ModelCallTask || got.CallID != command.CallID || got.ModelID != command.ModelID || got.ReservedTokens != 3_000_000 ||
		got.ReservedMicros != 10_000_000 || got.Rate.Version != 7 || len(got.RequestDigest) != 32 ||
		store.reserveCalls != 1 || rates.model != command.ModelID || access.minimum != project.RoleMember {
		t.Fatalf("ReserveModelCall() = %#v, replay=%v, err=%v; store=%#v rate=%#v access=%#v", got, replay, err, store, rates, access)
	}
	firstDigest := append([]byte(nil), got.RequestDigest...)
	got, _, err = service.ReserveModelCall(context.Background(), principal, command)
	if err != nil || string(firstDigest) != string(got.RequestDigest) {
		t.Fatalf("retry changed reservation digest: first=%x retry=%x err=%v", firstDigest, got.RequestDigest, err)
	}
	if store.reservation.Rate != rates.rate || store.reservation.ReservedMicros != 10_000_000 {
		t.Fatalf("reservation did not carry the server rate snapshot: %#v", store.reservation)
	}
}

func TestProjectBudgetReserveFailsClosedForUnknownRateOrQuota(t *testing.T) {
	service, store, access, rates := projectBudgetServiceFixture(t, project.RoleMember)
	principal := projectBudgetPrincipal(projectBudgetTestMember)
	command := ReserveProjectModelCallCommand{
		ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", ModelID: "p1234567",
		Usage: project.Usage{InputTokens: 10},
	}
	command = withProjectBudgetAccountReservation(principal, command)
	rates.err = errors.New("rate not configured")
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidState || store.reserveCalls != 0 {
		t.Fatalf("unknown rate reservation error=%v, store calls=%d", err, store.reserveCalls)
	}
	rates.err = nil
	rates.rate = project.RateSnapshot{Provider: "openai", ModelID: "p1234567", Model: "gpt-4o", Currency: "USD", Version: 0, InputMicrosPerMillion: 1}
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidState || store.reserveCalls != 0 {
		t.Fatalf("invalid rate reservation error=%v, store calls=%d", err, store.reserveCalls)
	}
	rates.rate = project.RateSnapshot{Provider: "openai", ModelID: "p1234567", Model: "gpt-4o", Currency: "USD", Version: 7, InputMicrosPerMillion: 2_000_000}
	store.reserveErr = domain.NewError(domain.CodeQuotaExceeded, "project or account budget is unavailable")
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeQuotaExceeded || store.reserveCalls != 1 {
		t.Fatalf("combined budget reservation error=%v, store calls=%d", err, store.reserveCalls)
	}
	if access.minimum != project.RoleMember || rates.calls != 3 {
		t.Fatalf("authorization/rate lookups = %d/%d", access.calls, rates.calls)
	}
}

func TestProjectBudgetRateMustMatchRequestedModelAndCostMustBeKnown(t *testing.T) {
	service, store, _, rates := projectBudgetServiceFixture(t, project.RoleMember)
	principal := projectBudgetPrincipal(projectBudgetTestMember)
	command := ReserveProjectModelCallCommand{
		ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", ModelID: "p1234567",
		Usage: project.Usage{InputTokens: 1},
	}
	command = withProjectBudgetAccountReservation(principal, command)
	rates.rate.ModelID = "p7654321"
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidState || store.reserveCalls != 0 {
		t.Fatalf("mismatched rate error=%v, store calls=%d", err, store.reserveCalls)
	}
	rates.rate.ModelID = command.ModelID
	rates.rate.InputMicrosPerMillion = 0
	rates.rate.OutputMicrosPerMillion = 1
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidState || store.reserveCalls != 0 {
		t.Fatalf("zero-cost rate error=%v, store calls=%d", err, store.reserveCalls)
	}
}

func TestProjectBudgetStoreChecksTaskProjectAndActualActorFence(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	store.reserveErr = domain.NewError(domain.CodeForbidden, "task actor or project does not match")
	principal := projectBudgetPrincipal(projectBudgetTestMember)
	command := ReserveProjectModelCallCommand{
		ProjectID: projectBudgetTestID, TaskID: "q1234567", Source: ModelCallTask, CallID: "task-call-0002",
		ModelID: "p1234567", Usage: project.Usage{InputTokens: 1},
	}
	command = withProjectBudgetAccountReservation(principal, command)
	if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Task actor fence error=%v", err)
	}
	if store.reservation.ProjectID != projectBudgetTestID || store.reservation.ActorID != principal.UserID ||
		store.reservation.TaskID != command.TaskID || store.reserveCalls != 1 {
		t.Fatalf("Task authorization attribution was not forwarded: %#v", store.reservation)
	}
}

func TestProjectBudgetRejectsStoreReservationWithoutPositiveCost(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	command := ReserveProjectModelCallCommand{
		ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", ModelID: "p1234567",
		Usage: project.Usage{InputTokens: 1},
	}
	command = withProjectBudgetAccountReservation(projectBudgetPrincipal(projectBudgetTestMember), command)
	bad := ProjectModelReservation{
		ProjectID: projectBudgetTestID, ActorID: projectBudgetTestMember, Source: command.Source, CallID: command.CallID,
		ModelID: command.ModelID, Usage: command.Usage, ReservedTokens: 1, Rate: project.RateSnapshot{
			Provider: "openai", ModelID: command.ModelID, Model: "gpt-4o", Currency: "USD", Version: 7, InputMicrosPerMillion: 1_000_000,
		}, RequestDigest: make([]byte, 32),
	}
	store.reservationOverride = &bad
	if _, _, err := service.ReserveModelCall(context.Background(), projectBudgetPrincipal(projectBudgetTestMember), command); err == nil || store.reserveCalls != 1 {
		t.Fatalf("zero-cost Store reservation error=%v calls=%d", err, store.reserveCalls)
	}
}

func TestProjectBudgetReserveRejectsInvalidScopeAndUsage(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	principal := projectBudgetPrincipal(projectBudgetTestMember)
	for _, command := range []ReserveProjectModelCallCommand{
		{ProjectID: projectBudgetTestID, Source: ModelCallProbe, CallID: "probe-call-0001", ModelID: "p1234567", Usage: project.Usage{InputTokens: 1}},
		{ProjectID: projectBudgetTestID, Source: ModelCallTask, CallID: "task-call-0001", ModelID: "p1234567", Usage: project.Usage{InputTokens: 1}},
		{ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", ModelID: "p1234567", Usage: project.Usage{InputTokens: math.MaxInt64, OutputTokens: 1}},
		{ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", ModelID: "p1234567"},
	} {
		if _, _, err := service.ReserveModelCall(context.Background(), principal, command); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid reservation %+v error=%v", command, err)
		}
	}
	if store.reserveCalls != 0 {
		t.Fatalf("invalid reservation reached Store %d times", store.reserveCalls)
	}
}

func TestProjectBudgetSettlementKeepsUnknownUsageHeld(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	unknown := SettleProjectModelCallCommand{ProjectID: projectBudgetTestID, Source: ModelCallTask, CallID: "task-call-0001", Unknown: true}
	if err := service.SettleModelCall(context.Background(), unknown); err != nil || store.settleCalls != 1 || !store.settlement.Unknown || store.settlement.Usage != (project.Usage{}) {
		t.Fatalf("unknown SettleModelCall() = %#v, err=%v", store.settlement, err)
	}
	known := SettleProjectModelCallCommand{ProjectID: projectBudgetTestID, Source: ModelCallChat, CallID: "chat-call-0001", Usage: project.Usage{InputTokens: 120}}
	if err := service.SettleModelCall(context.Background(), known); err != nil || store.settleCalls != 2 || store.settlement != known {
		t.Fatalf("known SettleModelCall() = %#v, err=%v", store.settlement, err)
	}
	known.Usage.InputTokens = -1
	if err := service.SettleModelCall(context.Background(), known); domain.CodeOf(err) != domain.CodeInvalidArgument || store.settleCalls != 2 {
		t.Fatalf("invalid settlement error=%v, store calls=%d", err, store.settleCalls)
	}
}

func TestProjectBudgetSettlementDoesNotHideOverReservationResult(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	store.settleErr = domain.NewError(domain.CodeQuotaExceeded, "actual model charge exceeds reserved amount")
	command := SettleProjectModelCallCommand{
		ProjectID: projectBudgetTestID, Source: ModelCallTask, CallID: "task-call-0001",
		Usage: project.Usage{InputTokens: 200, OutputTokens: 100},
	}
	if err := service.SettleModelCall(context.Background(), command); domain.CodeOf(err) != domain.CodeQuotaExceeded ||
		store.settleCalls != 1 || store.settlement != command {
		t.Fatalf("over-reservation settlement error=%v, stored=%#v calls=%d", err, store.settlement, store.settleCalls)
	}
}

func TestProjectBudgetStoreFailuresPropagate(t *testing.T) {
	service, store, access, _ := projectBudgetServiceFixture(t, project.RoleViewer)
	store.getErr = errors.New("read failed")
	if _, err := service.GetBudget(context.Background(), projectBudgetPrincipal(projectBudgetTestMember), projectBudgetTestID); err == nil {
		t.Fatal("GetBudget swallowed Store error")
	}
	access.member.Role = project.RoleOwner
	access.member.UserID = projectBudgetTestOwner
	store.setErr = errors.New("write failed")
	if _, err := service.SetBudget(context.Background(), projectBudgetPrincipal(projectBudgetTestOwner), projectBudgetTestID,
		SetProjectBudgetCommand{Currency: "USD", LimitMicros: 1, IdempotencyKey: "budget-key-1"}); err == nil {
		t.Fatal("SetBudget swallowed Store error")
	}
	store.settleErr = errors.New("settlement failed")
	if err := service.SettleModelCall(context.Background(), SettleProjectModelCallCommand{ProjectID: projectBudgetTestID,
		Source: ModelCallChat, CallID: "chat-call-0001", Unknown: true}); err == nil {
		t.Fatal("SettleModelCall swallowed Store error")
	}
}

func TestProjectBudgetKnownSettlementFailurePropagates(t *testing.T) {
	service, store, _, _ := projectBudgetServiceFixture(t, project.RoleMember)
	want := errors.New("known settlement failed")
	store.settleErr = want
	command := SettleProjectModelCallCommand{
		ProjectID: projectBudgetTestID, Source: ModelCallTask, CallID: "task-call-0001",
		Usage: project.Usage{InputTokens: 90, OutputTokens: 10},
	}
	if err := service.SettleModelCall(context.Background(), command); !errors.Is(err, want) || err == nil ||
		store.settleCalls != 1 || store.settlement != command {
		t.Fatalf("known settlement failure was hidden or changed: err=%v stored=%#v calls=%d", err, store.settlement, store.settleCalls)
	}
}

func projectBudgetInt64Pointer(value int64) *int64 { return &value }
