package app

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ProjectBudgetView struct {
	Configured     bool
	Budget         project.Budget
	SpentMicros    int64
	ReservedMicros int64
	HasUnknown     bool
}

type ProjectLedgerStatus string

const (
	ProjectLedgerReserved ProjectLedgerStatus = "reserved"
	ProjectLedgerSettled  ProjectLedgerStatus = "settled"
	ProjectLedgerUnknown  ProjectLedgerStatus = "unknown"
)

type ProjectLedgerEntry struct {
	CallID         string
	ActorID        string
	TaskID         string
	Source         ModelCallSource
	ModelID        string
	RateVersion    int64
	Currency       string
	Status         ProjectLedgerStatus
	ReservedUsage  project.Usage
	ActualUsage    *project.Usage
	ReservedMicros int64
	ActualMicros   *int64
	CreatedAt      time.Time
}

type ProjectLedgerPage struct {
	Items      []ProjectLedgerEntry
	NextCursor string
}

type SetProjectBudgetCommand struct {
	Currency        string
	LimitMicros     int64
	ExpectedVersion int64
	IdempotencyKey  string
}

type ReserveProjectModelCallCommand struct {
	ProjectID   string
	TaskID      string
	Source      ModelCallSource
	CallID      string
	ModelID     string
	Usage       project.Usage
	AccountCall ModelCallReservation
}

type ProjectModelReservation struct {
	ProjectID      string
	ActorID        string
	TaskID         string
	Source         ModelCallSource
	CallID         string
	ModelID        string
	Usage          project.Usage
	ReservedTokens int64
	Rate           project.RateSnapshot
	ReservedMicros int64
	RequestDigest  []byte
	AccountCall    ModelCallReservation
}

type SettleProjectModelCallCommand struct {
	ProjectID          string
	Source             ModelCallSource
	CallID             string
	Usage              project.Usage
	Unknown            bool
	Summary            *ConversationSummaryModelFence
	ResponseCiphertext []byte
	ResponseDigest     []byte
	ResponseInvalid    bool
}

type ProjectBudgetStore interface {
	// GetProjectBudget returns only the budget and totals visible to actorID; an unset budget is returned as Configured=false.
	GetProjectBudget(context.Context, string, string) (ProjectBudgetView, error)
	// SetProjectBudget must recheck owner membership, expected version and idempotency in the write transaction. It cannot lower the limit below committed/reserved usage or while an unknown call is held.
	SetProjectBudget(context.Context, string, string, project.Budget, int64, string) (ProjectBudgetView, error)
	// ListProjectLedgerEntries must recheck active membership and return only attribution, model/rate metadata, amounts, usage and timestamps; prompts and credentials are never ledger fields.
	ListProjectLedgerEntries(context.Context, string, string, PageRequest) (ProjectLedgerPage, error)
	// ReserveProjectModelCall must recheck current project membership, configured budget/currency/limit, and the existing account token budget in one transaction. For Task calls it must also verify the Task belongs to this project and ActorID is its actual initiator. It reserves both budgets atomically, stores the rate snapshot and actor/Task/source attribution, and treats project/source/call ID as an idempotency key whose request digest must match. Unknown project or account usage fails closed.
	ReserveProjectModelCall(context.Context, ProjectModelReservation) (ProjectModelReservation, bool, error)
	// SettleProjectModelCall must settle both the project ledger and existing account token ledger atomically using the persisted rate snapshot. If actual usage or cost exceeds the reservation, it must retain the reservation as unknown and fail settlement; it must never silently release the hold or charge beyond the reserved budget. Unknown usage remains held and prevents further budget-controlled reservations.
	SettleProjectModelCall(context.Context, SettleProjectModelCallCommand) error
}

type ProjectBudgetRateProvider interface {
	ProjectRateSnapshot(context.Context, string) (project.RateSnapshot, error)
}

type ProjectBudgetService struct {
	store  ProjectBudgetStore
	access ProjectAccessChecker
	rates  ProjectBudgetRateProvider
}

func NewProjectBudgetService(store ProjectBudgetStore, access ProjectAccessChecker, rates ProjectBudgetRateProvider) (*ProjectBudgetService, error) {
	if store == nil || access == nil || rates == nil {
		return nil, domain.InvalidArgument("dependencies", "project budget dependencies are required")
	}
	return &ProjectBudgetService{store: store, access: access, rates: rates}, nil
}

func (s *ProjectBudgetService) GetBudget(ctx context.Context, principal identity.Principal, projectID string) (ProjectBudgetView, error) {
	if ctx == nil {
		return ProjectBudgetView{}, domain.InvalidArgument("context", "project budget context is required")
	}
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectBudgetView{}, err
	}
	value, err := s.store.GetProjectBudget(ctx, principal.UserID, projectID)
	if err != nil {
		return ProjectBudgetView{}, err
	}
	if err := validateProjectBudgetView(value); err != nil {
		return ProjectBudgetView{}, fmt.Errorf("read project budget: %w", err)
	}
	return value, nil
}

func (s *ProjectBudgetService) SetBudget(ctx context.Context, principal identity.Principal, projectID string, command SetProjectBudgetCommand) (ProjectBudgetView, error) {
	if ctx == nil {
		return ProjectBudgetView{}, domain.InvalidArgument("context", "project budget context is required")
	}
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleOwner); err != nil {
		return ProjectBudgetView{}, err
	}
	if command.ExpectedVersion < 0 || command.ExpectedVersion == math.MaxInt64 {
		return ProjectBudgetView{}, domain.InvalidArgument("expected_version", "project budget version is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return ProjectBudgetView{}, err
	}
	budget := project.Budget{Currency: command.Currency, LimitMicros: command.LimitMicros, Version: command.ExpectedVersion + 1}
	if err := budget.Validate(); err != nil {
		return ProjectBudgetView{}, domain.InvalidArgument("budget", err.Error())
	}
	value, err := s.store.SetProjectBudget(ctx, principal.UserID, projectID, budget, command.ExpectedVersion, command.IdempotencyKey)
	if err != nil {
		return ProjectBudgetView{}, err
	}
	if err := validateProjectBudgetView(value); err != nil {
		return ProjectBudgetView{}, fmt.Errorf("write project budget: %w", err)
	}
	if !value.Configured || value.Budget != budget {
		return ProjectBudgetView{}, domain.NewError(domain.CodeInternal, "project budget store returned an unexpected version")
	}
	return value, nil
}

func (s *ProjectBudgetService) ListLedger(ctx context.Context, principal identity.Principal, projectID string, page PageRequest) (ProjectLedgerPage, error) {
	if ctx == nil || !project.ValidID(projectID) {
		return ProjectLedgerPage{}, domain.InvalidArgument("project_ledger", "project ledger query is invalid")
	}
	if _, err := s.access.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectLedgerPage{}, err
	}
	page, err := normalizeProjectPage(page)
	if err != nil {
		return ProjectLedgerPage{}, err
	}
	result, err := s.store.ListProjectLedgerEntries(ctx, principal.UserID, projectID, page)
	if err != nil {
		return ProjectLedgerPage{}, err
	}
	if result.Items == nil || len(result.Items) > int(page.Limit) || len(result.NextCursor) > 1024 {
		return ProjectLedgerPage{}, domain.NewError(domain.CodeInternal, "project ledger store returned an invalid page")
	}
	for _, item := range result.Items {
		if err := validateProjectLedgerEntry(item); err != nil {
			return ProjectLedgerPage{}, fmt.Errorf("read project ledger entry: %w", err)
		}
	}
	return result, nil
}

// ReserveModelCall is an internal model-gateway use case; no client endpoint accepts rate or reservation amounts.
func (s *ProjectBudgetService) ReserveModelCall(ctx context.Context, principal identity.Principal, command ReserveProjectModelCallCommand) (ProjectModelReservation, bool, error) {
	if ctx == nil || command.Source == ModelCallRoute || !project.ValidID(command.ProjectID) || !validProjectBudgetSource(command.Source) ||
		!idempotencyKeyPattern.MatchString(command.CallID) || !domain.ValidModelConnectionID(command.ModelID) ||
		(command.Source == ModelCallTask && !domain.ValidTaskID(command.TaskID)) ||
		(command.Source != ModelCallTask && command.TaskID != "") || !validProjectTokenUsage(command.Usage) {
		return ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "project model call reservation is invalid")
	}
	reservedTokens := command.Usage.InputTokens + command.Usage.OutputTokens
	if !projectAccountCallMatchesCommand(command.AccountCall, principal, command, reservedTokens, "") {
		return ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "account model reservation is required")
	}
	if _, err := s.access.RequireAccess(ctx, principal, command.ProjectID, project.RoleMember); err != nil {
		return ProjectModelReservation{}, false, err
	}
	rate, err := s.rates.ProjectRateSnapshot(ctx, command.ModelID)
	if err != nil || rate.Validate() != nil || rate.ModelID != command.ModelID || rate.Model != command.AccountCall.ProviderModel {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	reservedMicros, err := rate.EstimateTokenCost(command.Usage)
	if err != nil || reservedMicros < 1 {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	digest, err := hashIdempotencyRequest(struct {
		ProjectID string
		ActorID   string
		TaskID    string
		Source    ModelCallSource
		CallID    string
		ModelID   string
		Usage     project.Usage
		Request   []byte
	}{command.ProjectID, principal.UserID, command.TaskID, command.Source, command.CallID, command.ModelID, command.Usage,
		command.AccountCall.RequestDigest})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	reservation, replay, err := s.store.ReserveProjectModelCall(ctx, ProjectModelReservation{
		ProjectID: command.ProjectID, ActorID: principal.UserID, TaskID: command.TaskID, Source: command.Source,
		CallID: command.CallID, ModelID: command.ModelID, Usage: command.Usage, ReservedTokens: reservedTokens,
		Rate: rate, ReservedMicros: reservedMicros, RequestDigest: digest, AccountCall: command.AccountCall,
	})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	if err := validateProjectModelReservation(reservation, command, principal.UserID, digest); err != nil {
		return ProjectModelReservation{}, false, fmt.Errorf("reserve project model call: %w", err)
	}
	if !sameProjectAccountCall(reservation.AccountCall, command.AccountCall) {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInternal, "project budget store returned an invalid account reservation")
	}
	return reservation, replay, nil
}

// reserveTaskModelCall is only used by the internal model gateway. The Store
// transaction rechecks Task attribution and membership while reserving both
// the account token budget and project money budget atomically.
func (s *ProjectBudgetService) reserveTaskModelCall(ctx context.Context, projectID string, call ModelCallReservation, usage project.Usage) (ProjectModelReservation, bool, error) {
	if ctx == nil || !project.ValidID(projectID) || call.Source != ModelCallTask || call.Task == nil ||
		!domain.ValidTaskID(call.Task.TaskID) || !domain.ValidUserID(call.OwnerID) || call.OwnerID != call.Task.InitiatorID ||
		!domain.ValidModelConnectionID(call.ModelID) || !idempotencyKeyPattern.MatchString(call.CallID) ||
		len(call.RequestDigest) != 32 || !validProjectTokenUsage(usage) || call.ReservedTokens != usage.InputTokens+usage.OutputTokens {
		return ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "project task model call reservation is invalid")
	}
	rate, err := s.rates.ProjectRateSnapshot(ctx, call.ModelID)
	if err != nil || rate.Validate() != nil || rate.ModelID != call.ModelID || rate.Model != call.ProviderModel {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	reservedMicros, err := rate.EstimateTokenCost(usage)
	if err != nil || reservedMicros < 1 {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	digest, err := hashIdempotencyRequest(struct {
		ProjectID string
		ActorID   string
		TaskID    string
		Source    ModelCallSource
		CallID    string
		ModelID   string
		Usage     project.Usage
		Request   []byte
	}{projectID, call.OwnerID, call.Task.TaskID, call.Source, call.CallID, call.ModelID, usage, call.RequestDigest})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	reservation, replay, err := s.store.ReserveProjectModelCall(ctx, ProjectModelReservation{
		ProjectID: projectID, ActorID: call.OwnerID, TaskID: call.Task.TaskID, Source: call.Source,
		CallID: call.CallID, ModelID: call.ModelID, Usage: usage, ReservedTokens: call.ReservedTokens,
		Rate: rate, ReservedMicros: reservedMicros, RequestDigest: digest, AccountCall: call,
	})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	command := ReserveProjectModelCallCommand{ProjectID: projectID, TaskID: call.Task.TaskID, Source: call.Source,
		CallID: call.CallID, ModelID: call.ModelID, Usage: usage, AccountCall: call}
	if err := validateProjectModelReservation(reservation, command, call.OwnerID, digest); err != nil ||
		!sameProjectAccountCall(reservation.AccountCall, call) {
		return ProjectModelReservation{}, false, fmt.Errorf("reserve project task model call: %w", domain.NewError(domain.CodeInternal, "project budget store returned an invalid account reservation"))
	}
	return reservation, replay, nil
}

func (s *ProjectBudgetService) reserveConversationSummaryModelCall(ctx context.Context, projectID string,
	call ModelCallReservation, usage project.Usage) (ProjectModelReservation, bool, error) {
	if ctx == nil || !project.ValidID(projectID) || call.Source != ModelCallCompaction || call.Summary == nil ||
		!domain.ValidUserID(call.OwnerID) || call.OwnerID != call.Summary.Identity.ActorID ||
		call.Summary.Identity.ScopeKind != ConversationSummaryProjectScope || call.Summary.Identity.ScopeID != projectID ||
		!domain.ValidModelConnectionID(call.ModelID) || !idempotencyKeyPattern.MatchString(call.CallID) ||
		len(call.RequestDigest) != 32 || !validProjectTokenUsage(usage) || call.ReservedTokens != usage.InputTokens+usage.OutputTokens {
		return ProjectModelReservation{}, false, domain.InvalidArgument("project_model_call", "project summary model call reservation is invalid")
	}
	rate, err := s.rates.ProjectRateSnapshot(ctx, call.ModelID)
	if err != nil || rate.Validate() != nil || rate.ModelID != call.ModelID || rate.Model != call.ProviderModel {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	reservedMicros, err := rate.EstimateTokenCost(usage)
	if err != nil || reservedMicros < 1 {
		return ProjectModelReservation{}, false, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	digest, err := hashIdempotencyRequest(struct {
		ProjectID string
		ActorID   string
		Source    ModelCallSource
		CallID    string
		ModelID   string
		Usage     project.Usage
		Request   []byte
	}{projectID, call.OwnerID, call.Source, call.CallID, call.ModelID, usage, call.RequestDigest})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	reservation, replay, err := s.store.ReserveProjectModelCall(ctx, ProjectModelReservation{
		ProjectID: projectID, ActorID: call.OwnerID, Source: call.Source, CallID: call.CallID,
		ModelID: call.ModelID, Usage: usage, ReservedTokens: call.ReservedTokens,
		Rate: rate, ReservedMicros: reservedMicros, RequestDigest: digest, AccountCall: call,
	})
	if err != nil {
		return ProjectModelReservation{}, false, err
	}
	command := ReserveProjectModelCallCommand{ProjectID: projectID, Source: call.Source, CallID: call.CallID,
		ModelID: call.ModelID, Usage: usage, AccountCall: call}
	if err := validateProjectModelReservation(reservation, command, call.OwnerID, digest); err != nil ||
		!sameProjectAccountCall(reservation.AccountCall, call) {
		return ProjectModelReservation{}, false, fmt.Errorf("reserve project summary model call: %w", domain.NewError(domain.CodeInternal,
			"project budget store returned an invalid account reservation"))
	}
	return reservation, replay, nil
}

func projectAccountCallMatchesCommand(call ModelCallReservation, principal identity.Principal, command ReserveProjectModelCallCommand, reservedTokens int64, providerModel string) bool {
	if call.Source != command.Source || call.ProjectID != command.ProjectID || call.OwnerID != principal.UserID ||
		call.CallID != command.CallID || call.ModelID != command.ModelID || call.ReservedTokens != reservedTokens ||
		len(call.RequestDigest) != 32 || (providerModel != "" && call.ProviderModel != providerModel) ||
		call.ProviderModel == "" || len(call.ProviderModel) > 256 {
		return false
	}
	if command.Source == ModelCallTask {
		return call.Task != nil && call.Task.TaskID == command.TaskID && call.Task.InitiatorID == principal.UserID &&
			domain.ValidUserID(call.Task.OwnerID) && call.Principal.UserID == ""
	}
	if command.Source == ModelCallCompaction {
		return call.Task == nil && call.Summary != nil && call.Summary.Claim.Message.ActorID == principal.UserID &&
			call.Summary.Identity.ScopeKind == ConversationSummaryProjectScope && call.Summary.Identity.ScopeID == command.ProjectID
	}
	return call.Task == nil && call.Principal.UserID == principal.UserID
}

func sameProjectAccountCall(value, expected ModelCallReservation) bool {
	if value.Source != expected.Source || value.ProjectID != expected.ProjectID || value.PersonalModel != expected.PersonalModel || value.OwnerID != expected.OwnerID ||
		value.ModelID != expected.ModelID || value.ProviderModel != expected.ProviderModel ||
		!value.ConnectionVersion.Equal(expected.ConnectionVersion) || value.NeedsTools != expected.NeedsTools ||
		value.NeedsVision != expected.NeedsVision || value.CallID != expected.CallID ||
		value.ReservedTokens != expected.ReservedTokens || !bytes.Equal(value.RequestDigest, expected.RequestDigest) ||
		!sameProjectAccountPrincipal(value.Principal, expected.Principal) {
		return false
	}
	if (value.Task == nil) != (expected.Task == nil) {
		return false
	}
	if value.Task != nil {
		return *value.Task == *expected.Task
	}
	return sameConversationSummaryModelFence(value.Summary, expected.Summary)
}

func sameConversationSummaryModelFence(value, expected *ConversationSummaryModelFence) bool {
	if value == nil || expected == nil {
		return value == expected
	}
	return value.Identity == expected.Identity && value.OperationID == expected.OperationID && value.LeaseOwner == expected.LeaseOwner &&
		value.LeaseVersion == expected.LeaseVersion && value.SessionID == expected.SessionID &&
		value.ThroughSequence == expected.ThroughSequence && value.BaselineVersion == expected.BaselineVersion &&
		value.ContextWindowTokens == expected.ContextWindowTokens &&
		bytes.Equal(value.InputSHA256, expected.InputSHA256) && value.Claim.ServiceInstanceID == expected.Claim.ServiceInstanceID &&
		value.Claim.Message.ID == expected.Claim.Message.ID && value.Claim.Message.OwnerID == expected.Claim.Message.OwnerID &&
		value.Claim.Message.ActorID == expected.Claim.Message.ActorID && value.Claim.Message.ConversationID == expected.Claim.Message.ConversationID &&
		value.Claim.Message.Sequence == expected.Claim.Message.Sequence && value.Claim.LeaseOwner == expected.Claim.LeaseOwner &&
		value.Claim.LeaseVersion == expected.Claim.LeaseVersion && value.Claim.Authorization.Version == expected.Claim.Authorization.Version &&
		value.Claim.Authorization.TokenID == expected.Claim.Authorization.TokenID && value.Claim.Input.ProjectID == expected.Claim.Input.ProjectID
}

func sameProjectAccountPrincipal(value, expected identity.Principal) bool {
	return value.UserID == expected.UserID && value.Role == expected.Role && value.MustChangePassword == expected.MustChangePassword &&
		value.Kind == expected.Kind && value.TokenID == expected.TokenID && value.ClientInstallationID == expected.ClientInstallationID &&
		value.HostScope == expected.HostScope && value.HostGeneration == expected.HostGeneration &&
		value.ReplicaWorkspaceID == expected.ReplicaWorkspaceID && value.BindingVersion == expected.BindingVersion &&
		slices.Equal(value.HostIDs, expected.HostIDs) && slices.Equal(value.WorkspaceIDs, expected.WorkspaceIDs) &&
		slices.Equal(value.Scopes, expected.Scopes)
}

// SettleModelCall is an internal model-gateway use case. Unknown charges stay reserved until reconciliation.
func (s *ProjectBudgetService) SettleModelCall(ctx context.Context, command SettleProjectModelCallCommand) error {
	if ctx == nil || command.Source == ModelCallRoute || !project.ValidID(command.ProjectID) || !validProjectBudgetSource(command.Source) ||
		!idempotencyKeyPattern.MatchString(command.CallID) || (!command.Unknown && !validProjectTokenUsage(command.Usage)) ||
		(command.Source == ModelCallCompaction) != (command.Summary != nil) || command.Unknown && (len(command.ResponseCiphertext) != 0 || len(command.ResponseDigest) != 0) ||
		command.Source != ModelCallCompaction && (len(command.ResponseCiphertext) != 0 || len(command.ResponseDigest) != 0 || command.ResponseInvalid) ||
		command.ResponseInvalid && (len(command.ResponseCiphertext) != 0 || len(command.ResponseDigest) != 0) ||
		!command.Unknown && !command.ResponseInvalid && command.Source == ModelCallCompaction &&
			(len(command.ResponseCiphertext) == 0 || len(command.ResponseCiphertext) > MaxConversationSummaryCiphertext || len(command.ResponseDigest) != 32) {
		return domain.InvalidArgument("project_model_settlement", "project model call settlement is invalid")
	}
	return s.store.SettleProjectModelCall(ctx, command)
}

func validateProjectBudgetView(value ProjectBudgetView) error {
	if value.SpentMicros < 0 || value.ReservedMicros < 0 {
		return domain.NewError(domain.CodeInternal, "project budget store returned invalid totals")
	}
	if value.Configured {
		if err := value.Budget.Validate(); err != nil {
			return domain.NewError(domain.CodeInternal, "project budget store returned an invalid budget")
		}
	} else if value.Budget != (project.Budget{}) || value.SpentMicros != 0 || value.ReservedMicros != 0 || value.HasUnknown {
		return domain.NewError(domain.CodeInternal, "project budget store returned inconsistent configuration")
	}
	return nil
}

func validateProjectLedgerEntry(value ProjectLedgerEntry) error {
	if !idempotencyKeyPattern.MatchString(value.CallID) || !domain.ValidUserID(value.ActorID) ||
		!validProjectBudgetSource(value.Source) ||
		(value.Source == ModelCallTask && !domain.ValidTaskID(value.TaskID)) ||
		(value.Source != ModelCallTask && value.TaskID != "") || !domain.ValidModelConnectionID(value.ModelID) ||
		value.RateVersion < 1 || (project.Budget{Currency: value.Currency, LimitMicros: 1, Version: 1}).Validate() != nil ||
		value.ReservedMicros < 1 || !validProjectTokenUsage(value.ReservedUsage) || value.CreatedAt.IsZero() {
		return domain.NewError(domain.CodeInternal, "project ledger store returned an invalid entry")
	}
	switch value.Status {
	case ProjectLedgerReserved, ProjectLedgerUnknown:
		if value.ActualUsage != nil || value.ActualMicros != nil {
			return domain.NewError(domain.CodeInternal, "project ledger store returned inconsistent unsettled usage")
		}
	case ProjectLedgerSettled:
		if value.ActualUsage == nil || value.ActualMicros == nil || !validProjectTokenUsage(*value.ActualUsage) || *value.ActualMicros < 0 {
			return domain.NewError(domain.CodeInternal, "project ledger store returned invalid settled usage")
		}
	default:
		return domain.NewError(domain.CodeInternal, "project ledger store returned an invalid status")
	}
	return nil
}

func validProjectBudgetSource(source ModelCallSource) bool {
	return source == ModelCallClient || source == ModelCallChat || source == ModelCallTask || source == ModelCallRoute || source == ModelCallCompaction
}

func validProjectTokenUsage(value project.Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens >= 0 && value.InputTokens <= math.MaxInt64-value.OutputTokens &&
		(value.InputTokens > 0 || value.OutputTokens > 0)
}

func validateProjectModelReservation(value ProjectModelReservation, command ReserveProjectModelCallCommand, actorID string, requestDigest []byte) error {
	if !project.ValidID(value.ProjectID) || !domain.ValidUserID(value.ActorID) || !validProjectBudgetSource(value.Source) ||
		!idempotencyKeyPattern.MatchString(value.CallID) || !domain.ValidModelConnectionID(value.ModelID) ||
		(value.Source == ModelCallTask && !domain.ValidTaskID(value.TaskID)) ||
		(value.Source != ModelCallTask && value.TaskID != "") || !validProjectTokenUsage(value.Usage) ||
		value.Usage.InputTokens+value.Usage.OutputTokens != value.ReservedTokens || value.ReservedMicros < 1 ||
		value.Rate.Validate() != nil || value.Rate.ModelID != value.ModelID || len(value.RequestDigest) != 32 ||
		!bytes.Equal(value.RequestDigest, requestDigest) || value.ProjectID != command.ProjectID || value.ActorID != actorID ||
		value.TaskID != command.TaskID || value.Source != command.Source || value.CallID != command.CallID ||
		value.ModelID != command.ModelID || value.Usage != command.Usage {
		return domain.NewError(domain.CodeInternal, "project budget store returned an invalid reservation")
	}
	settledEstimate, err := value.Rate.EstimateTokenCost(value.Usage)
	if err != nil || settledEstimate != value.ReservedMicros {
		return domain.NewError(domain.CodeInternal, "project budget store returned an invalid rate snapshot")
	}
	return nil
}
