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
		*endpoint.ContextWindowTokens > MaxModelContextWindowTokens || *endpoint.MaxOutputTokens < 1 {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "long-form reply requires verified model context and output limits")
	}
	maxOutput, err := ConversationLongReplyOutputTokens(value, claim.Message.Content, *endpoint.ContextWindowTokens, *endpoint.MaxOutputTokens)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	request, err := ConversationLongReplyModelRequest(value, claim.Message.Content, maxOutput)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	s.applyGPT6LunaReasoning(endpoint.Endpoint, &request, false)
	if request.MaxOutputTokens > *endpoint.MaxOutputTokens || !ConversationRouteInputFitsContextWindow(request, *endpoint.ContextWindowTokens) {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeResultTooLarge, "conversation context exceeds the verified model window")
	}
	digest, callID, err := conversationRouteCallDigest(claim, value, request)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	reserve := modelRequestInputTokenReservation(request) + int64(request.MaxOutputTokens)
	if reserve < 1 || reserve > math.MaxInt64/2 {
		return ConversationRouteDecision{}, domain.InvalidArgument("long_reply", "long-form model reservation is invalid")
	}
	call := ModelCallReservation{Source: ModelCallRoute, Route: &ConversationRouteModelFence{Claim: claim, Context: value},
		ProjectID: claim.Input.ProjectID, PersonalModel: endpoint.Personal, OwnerID: value.ActorID,
		ModelID: value.ModelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		CallID: callID, RequestDigest: digest, ReservedTokens: reserve}
	prior, err := store.PrepareConversationRouteModelCall(ctx, call, nil)
	if errors.Is(err, ErrRouteProjectReservationRequired) {
		projectReservation, rateErr := s.routeProjectReservation(ctx, call, request)
		if rateErr != nil {
			return ConversationRouteDecision{}, rateErr
		}
		prior, err = store.PrepareConversationRouteModelCall(ctx, call, projectReservation)
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
		usage.InputTokens > math.MaxInt64-usage.OutputTokens || usage.InputTokens+usage.OutputTokens > reserve ||
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
