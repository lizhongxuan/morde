package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"

	"morde/server/internal/domain/project"
)

const (
	modelBudgetShadowStageUnknown = "stage_unknown"
	modelBudgetShadowProtocolMax  = MaxModelOutputTokens
	modelBudgetShadowReadTimeout  = 100 * time.Millisecond
)

// ModelOutputBudgetShadowObservation contains only bounded planning and usage
// metadata. It must never contain the request, response body, credentials, or
// request digest.
type ModelOutputBudgetShadowObservation struct {
	Context            context.Context
	Phase              string
	RouteFingerprint   string
	StrategyVersion    string
	Stage              string
	Outcome            string
	BlockReason        string
	LimitingDimensions []string
	GoalTokens         int64
	MinimumTokens      int64
	SafeTokens         int64
	SendTokens         int64
	CurrentOutputCap   int64
	UsageKnown         bool
	ActualOutputTokens int64
	StopReason         string
	FailureClass       string
	CapturedAt         time.Time
}

type modelOutputBudgetShadowSnapshot struct {
	strategy   modelOutputBudgetStrategySnapshot
	sampleRate float64
}

// SetModelOutputBudgetShadow installs a validated, immutable candidate policy
// for observation only. It does not enable adaptive admission or alter live
// model requests. A zero sample rate disables observations.
func (s *ModelGatewayService) SetModelOutputBudgetShadow(definition ModelOutputBudgetStrategyDefinition, sampleRate float64) error {
	if s == nil {
		return errors.New("model gateway is unavailable")
	}
	if math.IsNaN(sampleRate) || math.IsInf(sampleRate, 0) || sampleRate < 0 || sampleRate > 1 {
		return errors.New("model output budget shadow sample rate must be between 0 and 1")
	}
	snapshot, err := newModelOutputBudgetStrategySnapshot(definition)
	if err != nil {
		return err
	}
	s.modelBudgetShadowMu.Lock()
	s.modelBudgetShadow = &modelOutputBudgetShadowSnapshot{strategy: snapshot, sampleRate: sampleRate}
	s.modelBudgetShadowMu.Unlock()
	return nil
}

func (s *ModelGatewayService) SetModelOutputBudgetShadowObserver(observer func(ModelOutputBudgetShadowObservation)) {
	if s == nil {
		return
	}
	s.modelBudgetShadowMu.Lock()
	s.modelBudgetShadowObserver = observer
	s.modelBudgetShadowMu.Unlock()
}

func (s *ModelGatewayService) modelBudgetShadowConfig(routeKey, callID string) (*modelOutputBudgetShadowSnapshot, bool) {
	s.modelBudgetShadowMu.RLock()
	defer s.modelBudgetShadowMu.RUnlock()
	if s.modelBudgetShadow == nil || s.modelBudgetShadow.sampleRate <= 0 {
		return nil, false
	}
	if _, exists := s.modelBudgetShadow.strategy.models[routeKey]; !exists || !modelBudgetShadowSampled(callID, routeKey, s.modelBudgetShadow.sampleRate) {
		return nil, false
	}
	return s.modelBudgetShadow, true
}

func modelBudgetShadowSampled(callID, routeKey string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	digest := sha256.Sum256([]byte("morde-model-budget-shadow-sample-v1\n" + routeKey + "\n" + callID))
	sample := float64(binary.BigEndian.Uint64(digest[:8])) / float64(math.MaxUint64)
	return sample < rate
}

func modelBudgetShadowRouteFingerprint(baseURL, model string) string {
	key := modelMaxOutputCapacityRouteKey(baseURL, model)
	if key == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("morde-model-budget-route-v1\n" + key))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func modelBudgetShadowStage(fence *ModelTaskFence, request ModelRequest) string {
	if fence == nil {
		return modelBudgetShadowStageUnknown
	}
	if fence.AgentRootSessionID != "" && fence.AgentRootSessionID != fence.AgentSessionID {
		return string(modelOutputBudgetStageSubtask)
	}
	if len(request.Tools) > 0 {
		return string(modelOutputBudgetStageToolStep)
	}
	for _, input := range request.Input {
		if input.Type == "function_call" || input.Type == "function_call_output" {
			return string(modelOutputBudgetStageToolStep)
		}
	}
	// The root/no-tool shape is trustworthy, but it does not say whether the
	// requested answer is short or long. Keep that distinction out of prompt
	// heuristics and let the exact-route policy define a neutral root-text goal.
	return string(modelOutputBudgetStageRootTextOnly)
}

func (s *ModelGatewayService) planTaskModelBudgetShadow(ctx context.Context, call ModelCall,
	request ModelRequest, endpoint ModelGatewayEndpoint, projectID string) *ModelOutputBudgetShadowObservation {
	if call.Source != ModelCallTask || call.Task == nil || call.BudgetPlanID != "" || ctx == nil {
		return nil
	}
	routeKey := modelMaxOutputCapacityRouteKey(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID)
	if routeKey == "" {
		return nil
	}
	shadow, enabled := s.modelBudgetShadowConfig(routeKey, call.CallID)
	if !enabled {
		return nil
	}
	observation := &ModelOutputBudgetShadowObservation{
		Context: ctx, Phase: "plan", RouteFingerprint: modelBudgetShadowRouteFingerprint(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID),
		StrategyVersion: shadow.strategy.version, Stage: modelBudgetShadowStage(call.Task, request),
		Outcome: "blocked", CurrentOutputCap: int64(request.MaxOutputTokens), CapturedAt: time.Now().UTC(),
	}
	if observation.Stage == modelBudgetShadowStageUnknown {
		observation.BlockReason = "stage_unknown"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	policy, err := shadow.strategy.policyForModelStage(routeKey, observation.Stage)
	if err != nil {
		observation.BlockReason = "strategy_unavailable"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	minimum := policy.MinimumTokens
	if isFirstPartyGPT6Luna(endpoint.Endpoint) && s.gpt6LunaMaxReasoning && minimum < maxGPT6LunaReasoningTokens {
		minimum = maxGPT6LunaReasoningTokens
	}
	observation.GoalTokens, observation.MinimumTokens = policy.GoalTokens, minimum
	if policy.GoalTokens < minimum {
		observation.BlockReason = "reasoning_minimum_exceeds_goal"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	inputTokens := taskModelBudgetInputTokenReservation(request)
	if endpoint.MaxOutputTokens == nil || endpoint.ContextWindowTokens == nil {
		observation.BlockReason = "model_capability_unknown"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	reader, ok := s.store.(ModelTaskBudgetShadowPreviewReader)
	if !ok {
		observation.BlockReason = "budget_snapshot_unavailable"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	shadowCtx, cancelShadow := context.WithTimeout(ctx, modelBudgetShadowReadTimeout)
	defer cancelShadow()
	var rate *project.RateSnapshot
	if projectID != "" && !endpoint.Personal {
		if s.projectBudget == nil {
			observation.BlockReason = "project_rate_unknown"
			s.observeModelBudgetShadow(*observation)
			return observation
		}
		resolved, rateErr := s.projectBudget.projectRateSnapshot(shadowCtx, call.ModelID)
		if rateErr != nil {
			observation.BlockReason = "project_rate_unknown"
			s.observeModelBudgetShadow(*observation)
			return observation
		}
		rate = &resolved
	}
	preview, err := reader.PreviewTaskModelBudget(shadowCtx, ModelTaskBudgetShadowPreviewRequest{
		Fence: *call.Task, OwnerID: call.Task.InitiatorID, ModelID: call.ModelID,
		ProviderEndpoint: endpoint.Endpoint.BaseURL, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		ProjectID:     projectID,
		PersonalModel: endpoint.Personal, InputTokenReservation: inputTokens, ProjectRate: rate,
	})
	if err != nil {
		observation.BlockReason = "budget_snapshot_unavailable"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	if !preview.CapturedAt.IsZero() {
		observation.CapturedAt = preview.CapturedAt.UTC()
	}
	input := modelOutputBudgetInput{
		ModelMaxOutputTokens:    modelBudgetInt64Pointer(int64(*endpoint.MaxOutputTokens)),
		ContextWindowTokens:     modelBudgetInt64Pointer(int64(*endpoint.ContextWindowTokens)),
		ContextInputUpperBound:  modelBudgetInt64Pointer(inputTokens),
		ProtocolMaxOutputTokens: modelBudgetShadowProtocolMax,
	}
	// Applicability comes from the authoritative model and Task scope, not from
	// the preview response. Missing required dimensions stay unknown and block
	// the candidate instead of being silently treated as unlimited.
	input.AdditionalOutputTokenLimits = append(input.AdditionalOutputTokenLimits, modelOutputTokenLimit{
		Source: "task_budget", Applicable: true, OutputTokens: preview.TaskOutputLimit,
	})
	if !endpoint.Personal {
		input.AdditionalOutputTokenLimits = append(input.AdditionalOutputTokenLimits, modelOutputTokenLimit{
			Source: "account_budget", Applicable: true, OutputTokens: preview.AccountOutputLimit,
		})
	}
	if projectID != "" && !endpoint.Personal {
		input.AdditionalOutputTokenLimits = append(input.AdditionalOutputTokenLimits, modelOutputTokenLimit{
			Source: "project_budget", Applicable: true, OutputTokens: preview.ProjectOutputLimit,
		})
	}
	input, err = shadow.strategy.inputForModelStage(input, routeKey, observation.Stage)
	if err != nil {
		observation.BlockReason = "strategy_unavailable"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	input.MinimumTokens = minimum
	plan, err := buildModelOutputBudgetPlan(input)
	if err != nil {
		observation.BlockReason = "calculation_invalid"
		s.observeModelBudgetShadow(*observation)
		return observation
	}
	observation.GoalTokens, observation.MinimumTokens = plan.GoalTokens, plan.MinimumTokens
	observation.SafeTokens, observation.SendTokens = plan.SafeTokens, plan.SendTokens
	observation.LimitingDimensions = append([]string(nil), plan.LimitingDimensions...)
	observation.Outcome, observation.BlockReason = "calculated", plan.BlockReason
	if !plan.Dispatchable {
		observation.Outcome = "blocked"
	}
	// Unknown reads are retained as reasons on the observation instead of being
	// treated as a zero-cost or unlimited dimension.
	if len(preview.UnknownReasons) > 0 && observation.BlockReason == "" {
		observation.BlockReason = "budget_snapshot_partial"
	}
	s.observeModelBudgetShadow(*observation)
	return observation
}

func (s *ModelGatewayService) observeModelBudgetShadow(value ModelOutputBudgetShadowObservation) {
	s.modelBudgetShadowMu.RLock()
	observer := s.modelBudgetShadowObserver
	s.modelBudgetShadowMu.RUnlock()
	if observer == nil {
		return
	}
	value.LimitingDimensions = append([]string(nil), value.LimitingDimensions...)
	func() {
		defer func() { _ = recover() }()
		observer(value)
	}()
}

func observeTaskModelBudgetShadowResult(s *ModelGatewayService, plan *ModelOutputBudgetShadowObservation,
	response ModelResponse, callErr error) {
	if s == nil || plan == nil {
		return
	}
	result := *plan
	result.Phase = "result"
	result.UsageKnown = response.Usage.Known
	result.ActualOutputTokens = 0
	if response.Usage.Known && response.Usage.OutputTokens >= 0 {
		result.ActualOutputTokens = response.Usage.OutputTokens
	}
	result.StopReason = safeModelBudgetShadowStopReason(response.StopReason)
	result.FailureClass = safeModelBudgetShadowFailureClass(callErr)
	result.CapturedAt = time.Now().UTC()
	s.observeModelBudgetShadow(result)
}

func safeModelBudgetShadowStopReason(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stop", "tool_calls", "length", "content_filter":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func safeModelBudgetShadowFailureClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var modelErr *ModelClientError
	if errors.As(err, &modelErr) {
		switch modelErr.Kind {
		case ModelErrorAuthentication, ModelErrorAccess:
			return "access_rejected"
		case ModelErrorQuota:
			return "quota_exhausted"
		case ModelErrorInvalidRequest:
			return "invalid_request"
		case ModelErrorRateLimited:
			return "rate_limited"
		case ModelErrorUnavailable:
			return "unavailable"
		case ModelErrorTimeout:
			return "timeout"
		case ModelErrorTransport:
			return "transport"
		case ModelErrorInvalidResponse:
			return "invalid_response"
		case ModelErrorEmptyResponse:
			return "empty_response"
		case ModelErrorTruncatedResponse:
			return "truncated"
		case ModelErrorContentFiltered:
			return "content_filtered"
		}
	}
	return "provider_error"
}
