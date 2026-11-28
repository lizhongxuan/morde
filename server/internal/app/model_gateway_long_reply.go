package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
)

const conversationLongReplyUnavailableText = "这条长回复暂时无法完整生成，请调整问题后重试。"

// GenerateConversationLongReply performs one no-tools generation for an
// explicitly selected long-form message. The durable route call ledger owns
// idempotency and usage settlement; a settled retry returns its saved reply.
func (s *ModelGatewayService) GenerateConversationLongReply(ctx context.Context, claim ConversationRouteClaim,
	value ConversationRouteContext) (ConversationRouteDecision, error) {
	store, ok := s.store.(ConversationRouteModelStore)
	if !ok {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "conversation route model storage is unavailable")
	}
	if ctx == nil || claim.Input.ResponseMode != ConversationResponseLongForm || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) ||
		!domain.ValidMessageID(claim.Message.ID) || !domain.ValidConversationID(claim.Message.ConversationID) ||
		!domain.ValidUserID(claim.Message.OwnerID) || !domain.ValidUserID(claim.Message.ActorID) || claim.Message.Role != conversation.RoleUser ||
		claim.Message.TaskID != "" || claim.Message.Sequence < 1 || value.ActorID != claim.Message.ActorID ||
		!domain.ValidModelConnectionID(value.ModelID) || value.ReferenceTaskID != "" || value.ResultReview != nil ||
		claim.Input.ReferenceTaskID != "" || claim.Input.ResultReviewID != "" ||
		claim.Input.HostID != "" || claim.Input.WorkspaceID != "" || claim.Input.ChangeTaskTarget || claim.Input.ApplicationID != "" ||
		len(claim.Input.ClientAttachmentIDs) != 0 || len(claim.Input.MCPConnectorIDs) != 0 ||
		!utf8.ValidString(claim.Message.Content) || strings.ContainsRune(claim.Message.Content, '\x00') || len(claim.Message.Content) == 0 ||
		len(claim.Message.Content) > MaxConversationMessageBytes || !conversationRouteContextMatchesClaim(value, claim) {
		return ConversationRouteDecision{}, domain.InvalidArgument("long_reply", "long-form conversation identity or content is invalid")
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, value.ActorID, value.ModelID, false, false)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	if endpoint.ContextWindowTokens == nil || endpoint.MaxOutputTokens == nil || *endpoint.ContextWindowTokens < 4096 ||
		*endpoint.ContextWindowTokens > MaxModelContextWindowTokens || *endpoint.MaxOutputTokens < 1 ||
		*endpoint.MaxOutputTokens > MaxModelOutputTokens ||
		(endpoint.MaxOutputSource != ModelMaxOutputModelCatalog && endpoint.MaxOutputSource != ModelMaxOutputOverride) ||
		!supportedAdaptiveContextWindowSource(endpoint.ContextWindowSource) {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form reply requires verified model context and output limits")
	}
	routeKey := modelMaxOutputCapacityRouteKey(endpoint.Endpoint.BaseURL, endpoint.Endpoint.ModelID)
	if routeKey == "" {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form reply budget strategy is unavailable for this route")
	}
	snapshot, stagePolicy, err := s.modelOutputBudgetStrategyForStage(routeKey, string(modelOutputBudgetStageLongForm))
	if err != nil {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form reply budget strategy is unavailable for this route")
	}
	capabilityVersion := modelBudgetCapabilityVersion(endpoint)
	if capabilityVersion == "" {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form reply capability version is unavailable")
	}
	minimumTokens := stagePolicy.MinimumTokens
	if isFirstPartyGPT6Luna(endpoint.Endpoint) && s.gpt6LunaMaxReasoning && minimumTokens < maxGPT6LunaReasoningTokens {
		minimumTokens = maxGPT6LunaReasoningTokens
	}
	if stagePolicy.GoalTokens < minimumTokens {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeQuotaExceeded, "long-form reply budget is below the selected reasoning minimum")
	}
	contextRequest, err := ConversationLongReplyModelRequest(value, claim.Message.Content, 1)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	s.applyGPT6LunaReasoning(endpoint.Endpoint, &contextRequest, false)
	contextInputUpperBoundTokens, contextSafetyMarginTokens := conversationLongReplyContextBudget(contextRequest, *endpoint.ContextWindowTokens)
	contextCapacity := int64(*endpoint.ContextWindowTokens) - contextInputUpperBoundTokens - contextSafetyMarginTokens
	contextSafeTokens := min(int64(*endpoint.MaxOutputTokens), max(int64(0), contextCapacity))
	if contextSafeTokens < minimumTokens {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeQuotaExceeded, "long-form reply cannot meet the selected stage minimum")
	}
	inputReservedTokens := modelRequestInputTokenReservation(contextRequest)
	if inputReservedTokens < 1 || inputReservedTokens > math.MaxInt64/2-int64(*endpoint.MaxOutputTokens) {
		return ConversationRouteDecision{}, domain.InvalidArgument("long_reply", "long-form model reservation is invalid")
	}
	candidateSendTokens := min(stagePolicy.GoalTokens, contextSafeTokens)
	if candidateSendTokens < minimumTokens {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeQuotaExceeded, "long-form reply cannot meet the selected stage minimum")
	}
	buildCall := func(sendTokens, accountSafeTokens, projectSafeTokens int64) (ModelRequest, ModelCallReservation, error) {
		request, err := ConversationLongReplyModelRequest(value, claim.Message.Content, int(sendTokens))
		if err != nil {
			return ModelRequest{}, ModelCallReservation{}, err
		}
		s.applyGPT6LunaReasoning(endpoint.Endpoint, &request, false)
		if int64(request.MaxOutputTokens) != sendTokens || sendTokens > int64(*endpoint.MaxOutputTokens) ||
			!ConversationRouteInputFitsContextWindow(request, *endpoint.ContextWindowTokens) {
			return ModelRequest{}, ModelCallReservation{}, domain.NewError(domain.CodeResultTooLarge, "conversation context exceeds the verified model window")
		}
		digest, callID, err := conversationRouteCallDigest(claim, value, request)
		if err != nil {
			return ModelRequest{}, ModelCallReservation{}, err
		}
		safeTokens := min(contextSafeTokens, min(accountSafeTokens, projectSafeTokens))
		if sendTokens != min(stagePolicy.GoalTokens, safeTokens) {
			return ModelRequest{}, ModelCallReservation{}, domain.NewError(domain.CodeInvalidState, "long-form reply budget plan does not match its output cap")
		}
		reserve := inputReservedTokens + sendTokens
		plan := &ConversationRouteOutputBudgetPlan{
			Stage: string(modelOutputBudgetStageLongForm), StrategyVersion: snapshot.version, CapabilityVersion: capabilityVersion,
			ModelMaxOutputTokens: int64(*endpoint.MaxOutputTokens), ContextWindowTokens: int64(*endpoint.ContextWindowTokens),
			ContextInputUpperBoundTokens: contextInputUpperBoundTokens, ContextSafetyMarginTokens: contextSafetyMarginTokens,
			ContextSafeTokens: contextSafeTokens, AccountSafeTokens: accountSafeTokens, ProjectSafeTokens: projectSafeTokens,
			GoalTokens: stagePolicy.GoalTokens, MinimumTokens: minimumTokens, SafeTokens: safeTokens, SendTokens: sendTokens,
			InputReservedTokens: inputReservedTokens,
		}
		call := ModelCallReservation{Source: ModelCallRoute, Route: &ConversationRouteModelFence{Claim: claim, Context: value},
			ProjectID: claim.Input.ProjectID, PersonalModel: endpoint.Personal, OwnerID: value.ActorID,
			ModelID: value.ModelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
			CallID: callID, RequestDigest: digest, ReservedTokens: reserve, RouteOutputBudget: plan}
		return request, call, nil
	}
	// A successful full reservation already proves the selected send cap fits;
	// leaving a conservative cap in the receipt avoids a second DB round trip.
	request, call, err := buildCall(candidateSendTokens, candidateSendTokens, candidateSendTokens)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	var projectRate *project.RateSnapshot
	prepare := func(request ModelRequest, call ModelCallReservation) (*ConversationRouteDecision, error) {
		prior, err := store.PrepareConversationRouteModelCall(ctx, call, nil)
		if errors.Is(err, ErrRouteProjectReservationRequired) {
			if projectRate == nil {
				projectRate, err = s.routeProjectRateSnapshot(ctx, call)
				if err != nil {
					return nil, err
				}
			}
			projectReservation, rateErr := s.routeProjectReservationWithRate(call, request, *projectRate)
			if rateErr != nil {
				return nil, rateErr
			}
			return store.PrepareConversationRouteModelCall(ctx, call, projectReservation)
		}
		return prior, err
	}
	prior, err := prepare(request, call)
	if err != nil && domain.CodeOf(err) == domain.CodeQuotaExceeded {
		limiter, ok := s.store.(ConversationRouteOutputBudgetLimiter)
		if !ok {
			return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form model budget limiter is unavailable")
		}
		limits, limitErr := limiter.LimitConversationRouteOutputBudget(ctx, call, projectRate)
		if limitErr != nil {
			return ConversationRouteDecision{}, limitErr
		}
		safeTokens := min(contextSafeTokens, min(limits.AccountSafeTokens, limits.ProjectSafeTokens))
		sendTokens := min(stagePolicy.GoalTokens, safeTokens)
		if sendTokens < minimumTokens {
			return ConversationRouteDecision{}, domain.NewError(domain.CodeQuotaExceeded, "long-form reply budget is below the selected stage minimum")
		}
		request, call, err = buildCall(sendTokens, limits.AccountSafeTokens, limits.ProjectSafeTokens)
		if err != nil {
			return ConversationRouteDecision{}, err
		}
		prior, err = prepare(request, call)
	}
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	if prior != nil {
		if !prior.LongReply {
			return ConversationRouteDecision{}, domain.NewError(domain.CodeInvalidState, "saved route response does not match long-form mode")
		}
		return *prior, nil
	}
	if err := store.BeginConversationRouteModelCall(ctx, call); err != nil {
		return ConversationRouteDecision{}, err
	}
	request.Endpoint = endpoint.Endpoint
	dispatchCtx, cancel := context.WithTimeout(ctx, ConversationLongReplyTimeout)
	defer cancel()
	response, callErr := s.client.Complete(dispatchCtx, request)
	confirmedReject := confirmedConversationRouteRejection(callErr)
	usage := response.Usage
	if confirmedReject {
		usage = ModelUsage{Known: true}
	}
	unknown := !confirmedReject && (callErr != nil || !usage.Known || usage.InputTokens < 0 || usage.OutputTokens < 0 ||
		usage.InputTokens > math.MaxInt64-usage.OutputTokens || usage.InputTokens+usage.OutputTokens > call.ReservedTokens ||
		usage.OutputTokens > int64(request.MaxOutputTokens))
	var decision *ConversationRouteDecision
	if !unknown {
		result := ConversationRouteDecision{Action: ConversationRouteClarify, Question: conversationLongReplyUnavailableText, LongReply: true}
		if !confirmedReject && (response.StopReason == "stop" || response.StopReason == "length") && len(response.ToolCalls) == 0 &&
			ValidConversationLongReply(response.Text) {
			result = ConversationRouteDecision{Action: ConversationRouteReply, Reply: response.Text, LongReply: true,
				Partial: response.StopReason == "length"}
		}
		decision = &result
	}
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()
	if err := store.SettleConversationRouteModelCall(settleCtx, call, decision, usage, unknown); err != nil {
		return ConversationRouteDecision{}, fmt.Errorf("settle long-form conversation model call: %w", err)
	}
	if unknown {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeInvalidState, "long-form model result or usage is unknown and requires reconciliation")
	}
	if confirmedReject {
		return *decision, nil
	}
	if callErr != nil {
		return ConversationRouteDecision{}, conversationRouteProviderError(callErr)
	}
	return *decision, nil
}
