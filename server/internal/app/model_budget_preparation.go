package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

// TaskModelBudgetPrepareRequest is the provider-bound DSH projection at the
// prepare boundary. Prompt content is used transiently and is never persisted
// in the reservation record.
type TaskModelBudgetPrepareRequest struct {
	Fence           ModelTaskFence
	DSHCallID       string
	ModelID         string
	ThroughSequence int64
	Turn            int64
	Step            int64
	BudgetCallID    string
	RequestDigest   string
	ProviderRequest json.RawMessage
}

// TaskModelBudgetPlan is a single-use plan which already owns an account,
// Task and (when applicable) project reservation.
type TaskModelBudgetPlan struct {
	PlanID    string
	MaxTokens int
}

// ModelBudgetReservationPreparation contains only bounded plan metadata and
// identities. The PostgreSQL implementation computes the final output cap
// while holding the account/Task/project budget locks and writes both ledgers
// atomically.
type ModelBudgetReservationPreparation struct {
	Reservation             ModelCallReservation
	PlanID                  string
	BudgetCallID            string
	ProviderRequestDigest   string
	Turn                    int64
	Step                    int64
	Stage                   string
	StrategyVersion         string
	CapabilityVersion       string
	GoalTokens              int64
	MinimumTokens           int64
	StaticSafeTokens        int64
	ModelMaxOutputTokens    int64
	ContextWindowTokens     int64
	InputContextUpperBound  int64
	ContextSafetyMargin     int64
	InputTokenReservation   int64
	ProtocolMaxOutputTokens int64
	ProjectRate             *project.RateSnapshot
}

type ModelBudgetReservationResult struct {
	PlanID          string
	MaxOutputTokens int64
}

type ModelBudgetPreparationStore interface {
	PrepareTaskModelBudget(context.Context, ModelBudgetReservationPreparation) (ModelBudgetReservationResult, error)
	ModelBudgetPreparationValidator
}

type ModelBudgetPreparationValidator interface {
	ValidateTaskModelBudgetPreparation(context.Context, ModelCallReservation) error
}

// ModelBudgetOverageRecorder freezes a dispatched adaptive Task call and
// persists known provider usage when it exceeds its signed input/output hold.
type ModelBudgetOverageRecorder interface {
	RecordTaskModelBudgetOverage(context.Context, ModelCallReservation, ModelUsage) error
}

var modelBudgetDigestPattern = regexpModelBudgetDigest()

// ValidModelBudgetRequestDigest reports whether a provider projection digest
// has the canonical lowercase SHA-256 representation used by budget plans.
func ValidModelBudgetRequestDigest(value string) bool {
	return modelBudgetDigestPattern.MatchString(value)
}

func modelBudgetCapabilityVersion(endpoint ModelGatewayEndpoint) string {
	if endpoint.MaxOutputTokens == nil || endpoint.ContextWindowTokens == nil || endpoint.Version.IsZero() {
		return ""
	}
	routeKey := modelMaxOutputCapacityRouteKey(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID)
	if routeKey == "" {
		return ""
	}
	maxOutputCatalogVersion := ""
	if endpoint.MaxOutputCatalogVersion != nil {
		maxOutputCatalogVersion = *endpoint.MaxOutputCatalogVersion
	}
	contextCatalogVersion := ""
	if endpoint.ContextWindowCatalogVersion != nil {
		contextCatalogVersion = *endpoint.ContextWindowCatalogVersion
	}
	material := strings.Join([]string{
		"morde-model-capability-v1", routeKey,
		strconv.FormatInt(int64(*endpoint.MaxOutputTokens), 10), string(endpoint.MaxOutputSource), maxOutputCatalogVersion,
		strconv.FormatInt(int64(*endpoint.ContextWindowTokens), 10), string(endpoint.ContextWindowSource), contextCatalogVersion,
		endpoint.Version.UTC().Format("20060102T150405.000000000Z07:00"),
	}, "\n")
	digest := sha256.Sum256([]byte(material))
	return "cap_v1_" + hex.EncodeToString(digest[:])
}

func regexpModelBudgetDigest() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{64}$`)
}

// ModelBudgetFinalRequestDigest binds the DSH canonical provider projection to
// the exact output cap. The projection digest includes provider/model/messages
// and tools; connection and capability versions are stored alongside it.
func ModelBudgetFinalRequestDigest(providerRequestDigest string, maxOutputTokens int64) ([]byte, error) {
	if !modelBudgetDigestPattern.MatchString(providerRequestDigest) || maxOutputTokens < 1 || maxOutputTokens > MaxModelOutputTokens {
		return nil, domain.InvalidArgument("model_budget", "model budget request identity is invalid")
	}
	value := sha256.Sum256([]byte("morde-model-budget-request-v1\n" + providerRequestDigest + "\n" + strconv.FormatInt(maxOutputTokens, 10)))
	return append([]byte(nil), value[:]...), nil
}

// SetModelOutputBudgetStrategy installs an immutable stage snapshot. The
// production bootstrap calls it only when explicit adaptive admission is
// enabled; configuration and deployment defaults keep that opt-in disabled.
func (s *ModelGatewayService) SetModelOutputBudgetStrategy(definition ModelOutputBudgetStrategyDefinition) error {
	if s == nil {
		return errors.New("model gateway is unavailable")
	}
	snapshot, err := newModelOutputBudgetStrategySnapshot(definition)
	if err != nil {
		return err
	}
	s.modelBudgetStrategyMu.Lock()
	s.modelBudgetStrategy = &snapshot
	s.modelBudgetStrategyMu.Unlock()
	return nil
}

func (s *ModelGatewayService) modelOutputBudgetStrategyForStage(routeKey, stage string) (modelOutputBudgetStrategySnapshot, modelOutputBudgetStagePolicy, error) {
	if s == nil {
		return modelOutputBudgetStrategySnapshot{}, modelOutputBudgetStagePolicy{}, domain.NewError(domain.CodeUnsupported, "adaptive model budget strategy is not configured")
	}
	s.modelBudgetStrategyMu.RLock()
	defer s.modelBudgetStrategyMu.RUnlock()
	if s.modelBudgetStrategy == nil {
		return modelOutputBudgetStrategySnapshot{}, modelOutputBudgetStagePolicy{}, domain.NewError(domain.CodeUnsupported, "adaptive model budget strategy is not configured")
	}
	policy, err := s.modelBudgetStrategy.policyForModelStage(routeKey, stage)
	return *s.modelBudgetStrategy, policy, err
}

// SupportsTaskModelBudgetAdmission reports whether the Task's exact selected
// model route has verified output/context capacity and a reviewed strategy.
// Routes outside that allowlist keep the legacy DSH output cap.
func (s *ModelGatewayService) SupportsTaskModelBudgetAdmission(ctx context.Context, value task.Task) (bool, error) {
	if s == nil || s.models == nil || ctx == nil || strings.TrimSpace(value.InitiatorID) == "" ||
		!domain.ValidModelConnectionID(value.ModelID) {
		return false, domain.InvalidArgument("model_budget", "task model budget eligibility request is invalid")
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, value.InitiatorID, value.ModelID, false, false)
	if err != nil {
		return false, err
	}
	if endpoint.MaxOutputTokens == nil || *endpoint.MaxOutputTokens < 1 || *endpoint.MaxOutputTokens > MaxModelOutputTokens ||
		endpoint.ContextWindowTokens == nil || *endpoint.ContextWindowTokens < 4096 || *endpoint.ContextWindowTokens > MaxModelContextWindowTokens ||
		(endpoint.MaxOutputSource != ModelMaxOutputModelCatalog && endpoint.MaxOutputSource != ModelMaxOutputOverride) ||
		!supportedAdaptiveContextWindowSource(endpoint.ContextWindowSource) {
		return false, nil
	}
	routeKey := modelMaxOutputCapacityRouteKey(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID)
	if routeKey == "" {
		return false, nil
	}
	_, _, err = s.modelOutputBudgetStrategyForStage(routeKey, string(modelOutputBudgetStageShortAnswer))
	return err == nil, nil
}

// PrepareTaskModelBudget computes a plan from the exact DSH projection, then
// atomically reserves the resulting token and project-money envelope. It is
// available to an explicitly injected DSH planner; the default worker path
// remains unchanged until production acceptance gates are met.
func (s *ModelGatewayService) PrepareTaskModelBudget(ctx context.Context, input TaskModelBudgetPrepareRequest) (TaskModelBudgetPlan, error) {
	if ctx == nil || input.ThroughSequence < 0 || input.Turn < 0 || input.Step < 0 ||
		!domain.ValidModelConnectionID(input.ModelID) || !validModelBudgetProtocolID(input.BudgetCallID) ||
		!modelBudgetDigestPattern.MatchString(input.RequestDigest) || len(input.ProviderRequest) == 0 ||
		input.Fence.ThroughSequence != input.ThroughSequence || !modelBudgetRequestMatchesProviderProjection(input.ProviderRequest, input.RequestDigest) {
		return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "model budget prepare request is invalid")
	}
	fence := normalizedModelTaskFence(&input.Fence)
	if fence == nil || !validModelBudgetTaskFence(*fence) {
		return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "model budget Task fence is invalid")
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(input.ProviderRequest, &rawFields); err != nil || rawFields == nil {
		return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "model budget provider request is invalid")
	}
	if _, exists := rawFields["max_tokens"]; exists {
		return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "model budget prepare request already contains an output cap")
	}
	request, err := parseDSHModelRequest(ctx, *fence, input.ModelID, input.ProviderRequest, nil)
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	needsTools := len(request.Tools) != 0
	needsVision := false
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, fence.InitiatorID, input.ModelID, needsTools, needsVision)
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	if endpoint.MaxOutputTokens == nil || *endpoint.MaxOutputTokens < 1 || *endpoint.MaxOutputTokens > MaxModelOutputTokens ||
		endpoint.ContextWindowTokens == nil || *endpoint.ContextWindowTokens < 4096 || *endpoint.ContextWindowTokens > MaxModelContextWindowTokens ||
		(endpoint.MaxOutputSource != ModelMaxOutputModelCatalog && endpoint.MaxOutputSource != ModelMaxOutputOverride) ||
		!supportedAdaptiveContextWindowSource(endpoint.ContextWindowSource) {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "model output or context capacity is unknown")
	}
	s.applyGPT6LunaReasoning(endpoint.Endpoint, &request, true)
	stage := modelOutputBudgetStageShortAnswer
	if fence.AgentRootSessionID != fence.AgentSessionID {
		stage = modelOutputBudgetStageSubtask
	} else if needsTools {
		stage = modelOutputBudgetStageToolStep
	}
	routeKey := modelMaxOutputCapacityRouteKey(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID)
	if routeKey == "" {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "model output budget strategy is unavailable for this route")
	}
	snapshot, stagePolicy, err := s.modelOutputBudgetStrategyForStage(routeKey, string(stage))
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	minimum := stagePolicy.MinimumTokens
	if isFirstPartyGPT6Luna(endpoint.Endpoint) && s.gpt6LunaMaxReasoning && minimum < maxGPT6LunaReasoningTokens {
		minimum = maxGPT6LunaReasoningTokens
	}
	if stagePolicy.GoalTokens < minimum {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeQuotaExceeded, "model budget is below the selected reasoning minimum")
	}
	// The gateway appends its fixed Morde execution policy after parsing the
	// DSH projection. Include those trusted instructions in the hard input hold
	// so admission and the provider request use the same envelope.
	inputTokens := taskModelBudgetInputTokenReservation(request)
	strategyInput, err := snapshot.inputForModelStage(modelOutputBudgetInput{
		ModelMaxOutputTokens:    modelBudgetInt64Pointer(int64(*endpoint.MaxOutputTokens)),
		ContextWindowTokens:     modelBudgetInt64Pointer(int64(*endpoint.ContextWindowTokens)),
		ContextInputUpperBound:  modelBudgetInt64Pointer(inputTokens),
		ContextSafetyMargin:     0, // modelRequestInputTokenReservation includes bounded framing margins for text-only DSH projections.
		ProtocolMaxOutputTokens: MaxModelOutputTokens,
	}, routeKey, string(stage))
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	strategyInput.MinimumTokens = minimum
	shadowPlan, err := buildModelOutputBudgetPlan(strategyInput)
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	if !shadowPlan.Dispatchable {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeQuotaExceeded, "model budget cannot meet the selected stage minimum")
	}
	failedCallID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	dshCallID, ledgerCallID := failedCallID, failedCallID
	if fence.RecoveryCallID != "" || fence.RecoveryDSHCallID != "" {
		expectedDSHCallID, dshErr := TaskModelRecoveryDSHCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
		expectedLedgerCallID, ledgerErr := TaskModelRecoveryCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
		if dshErr != nil || ledgerErr != nil || fence.RecoveryDSHCallID != expectedDSHCallID ||
			fence.RecoveryCallID != expectedLedgerCallID || input.DSHCallID != expectedDSHCallID {
			return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "adaptive recovery identity is invalid")
		}
		dshCallID, ledgerCallID = expectedDSHCallID, expectedLedgerCallID
	} else if input.DSHCallID != failedCallID {
		return TaskModelBudgetPlan{}, domain.InvalidArgument("model_budget", "DSH model call identity is invalid")
	}
	projectResolver, ok := s.store.(ModelTaskProjectResolver)
	if !ok {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "task project scope is unavailable")
	}
	projectID, err := projectResolver.ModelTaskProjectID(ctx, *fence)
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	reservation := ModelCallReservation{Source: ModelCallTask, Task: fence, OwnerID: fence.InitiatorID,
		ProjectID: projectID, PersonalModel: endpoint.Personal, ModelID: input.ModelID,
		ProviderEndpoint: endpoint.Endpoint.BaseURL, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		NeedsTools: needsTools, NeedsVision: needsVision, CallID: ledgerCallID, DSHCallID: dshCallID, BudgetCallID: input.BudgetCallID,
		BudgetRequestDigest: input.RequestDigest}
	capabilityVersion := modelBudgetCapabilityVersion(endpoint)
	if capabilityVersion == "" {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "model capacity snapshot is unavailable")
	}
	reservation.BudgetCapabilityVersion = capabilityVersion
	var rate *project.RateSnapshot
	if projectID != "" && !endpoint.Personal {
		if s.projectBudget == nil {
			return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "project model budget is unavailable")
		}
		resolved, err := s.projectBudget.projectRateSnapshot(ctx, input.ModelID)
		if err != nil {
			return TaskModelBudgetPlan{}, err
		}
		rate = &resolved
	}
	store, ok := s.store.(ModelBudgetPreparationStore)
	if !ok {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeUnsupported, "adaptive model budget ledger is unavailable")
	}
	planID, err := newModelBudgetPlanID()
	if err != nil {
		return TaskModelBudgetPlan{}, err
	}
	result, err := store.PrepareTaskModelBudget(ctx, ModelBudgetReservationPreparation{
		Reservation: reservation, PlanID: planID, BudgetCallID: input.BudgetCallID,
		ProviderRequestDigest: input.RequestDigest, Turn: input.Turn, Step: input.Step, Stage: string(stage), StrategyVersion: snapshot.version,
		CapabilityVersion: capabilityVersion, GoalTokens: stagePolicy.GoalTokens, MinimumTokens: minimum,
		StaticSafeTokens: shadowPlan.SafeTokens, ModelMaxOutputTokens: int64(*endpoint.MaxOutputTokens),
		ContextWindowTokens: int64(*endpoint.ContextWindowTokens), InputContextUpperBound: inputTokens,
		InputTokenReservation: inputTokens, ProtocolMaxOutputTokens: strategyInput.ProtocolMaxOutputTokens, ProjectRate: rate,
	})
	if err != nil {
		return TaskModelBudgetPlan{}, s.waitForTaskModelBudgetCapacity(ctx, err, *fence, input.ModelID,
			dshCallID, ledgerCallID, input.BudgetCallID, input.RequestDigest, inputTokens, minimum, projectID, rate)
	}
	if !validModelBudgetProtocolID(result.PlanID) || result.MaxOutputTokens < minimum || result.MaxOutputTokens > shadowPlan.SafeTokens || result.MaxOutputTokens > MaxModelOutputTokens {
		return TaskModelBudgetPlan{}, domain.NewError(domain.CodeInternal, "adaptive model budget store returned an invalid plan")
	}
	return TaskModelBudgetPlan{PlanID: result.PlanID, MaxTokens: int(result.MaxOutputTokens)}, nil
}

func (s *ModelGatewayService) waitForTaskModelBudgetCapacity(ctx context.Context, prepareErr error, fence ModelTaskFence,
	modelID, dshCallID, ledgerCallID, budgetCallID, requestDigest string, inputTokens, minimumOutputTokens int64,
	projectID string, rate *project.RateSnapshot) error {
	if !isTaskModelBudgetConcurrencyLimit(prepareErr) {
		return prepareErr
	}
	store, ok := s.store.(TaskModelBudgetCapacityWaitStore)
	if !ok {
		return prepareErr
	}
	wait := TaskModelBudgetCapacityWaitRequest{
		Fence: fence, ModelID: modelID, ProjectID: projectID, DSHCallID: dshCallID, LedgerCallID: ledgerCallID,
		BudgetCallID: budgetCallID, RequestDigest: requestDigest, InputTokenReservation: inputTokens,
		MinimumOutputTokens: minimumOutputTokens,
	}
	if rate != nil {
		minimumMicros, err := rate.EstimateTokenCost(project.Usage{InputTokens: inputTokens, OutputTokens: minimumOutputTokens})
		if err != nil {
			return errors.Join(prepareErr, fmt.Errorf("estimate minimum project budget for capacity wait: %w", err))
		}
		wait.MinimumProjectMicros = minimumMicros
		wait.ProjectBudgetCurrency = rate.Currency
	}
	if err := store.WaitForTaskModelBudgetCapacity(ctx, wait); err != nil {
		return errors.Join(prepareErr, fmt.Errorf("persist model capacity wait: %w", err))
	}
	return ErrTaskModelBudgetCapacityWaiting
}

func isTaskModelBudgetConcurrencyLimit(err error) bool {
	var modelErr *domain.Error
	return errors.As(err, &modelErr) && modelErr.Code == domain.CodeRateLimited &&
		modelErr.Details["reason"] == "model_concurrency_limit" && modelErr.Details["stage"] == "reservation"
}

func validModelBudgetProtocolID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validModelBudgetTaskFence(fence ModelTaskFence) bool {
	valid := domain.ValidTaskID(fence.TaskID) && domain.ValidUserID(fence.OwnerID) && domain.ValidUserID(fence.InitiatorID) &&
		fence.TaskRevision >= 0 && fence.LeaseOwner != "" && len(fence.LeaseOwner) <= 128 && fence.LeaseVersion > 0 &&
		dshModelIdentityPattern.MatchString(fence.AgentInstanceID) &&
		fence.AgentGeneration > 0 && dshModelIdentityPattern.MatchString(fence.AgentSessionID) &&
		dshModelIdentityPattern.MatchString(fence.AgentRootSessionID) && fence.AuthorizationVersion > 0 &&
		fence.ThroughSequence >= 0 && !invalidCheckpointLabel(fence.EngineVersion, 128) && !invalidCheckpointLabel(fence.ProfileVersion, 128)
	if !valid {
		return false
	}
	if fence.RecoveryCallID == "" && fence.RecoveryDSHCallID == "" {
		return true
	}
	if fence.RecoveryCallID == "" || fence.RecoveryDSHCallID == "" {
		return false
	}
	failedCallID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		return false
	}
	ledgerCallID, ledgerErr := TaskModelRecoveryCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
	dshCallID, dshErr := TaskModelRecoveryDSHCallID(fence.TaskID, fence.TaskRevision, failedCallID, fence.ThroughSequence)
	return ledgerErr == nil && dshErr == nil && fence.RecoveryCallID == ledgerCallID && fence.RecoveryDSHCallID == dshCallID
}

func supportedAdaptiveContextWindowSource(source ModelContextWindowSource) bool {
	switch source {
	case ModelContextWindowOverride, ModelContextWindowLegacyManual, ModelContextWindowProviderMetadata, ModelContextWindowModelCatalog:
		return true
	default:
		return false
	}
}

func modelBudgetInt64Pointer(value int64) *int64 { return &value }

func taskModelBudgetInputTokenReservation(request ModelRequest) int64 {
	request.Instructions = mordeSoulInstructions(request.Instructions)
	return modelRequestInputTokenReservation(request)
}

func newModelBudgetPlanID() (string, error) {
	var bytes [24]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", fmt.Errorf("create model budget plan identity: %w", err)
	}
	return "plan_" + hex.EncodeToString(bytes[:]), nil
}

func modelBudgetRequestMatchesProviderProjection(raw json.RawMessage, providerRequestDigest string) bool {
	if len(raw) == 0 || !modelBudgetDigestPattern.MatchString(providerRequestDigest) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) != 4 {
		return false
	}
	for _, key := range []string{"provider", "model", "messages", "tools"} {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}
