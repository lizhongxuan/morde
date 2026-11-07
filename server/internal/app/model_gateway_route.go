package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
)

// ConversationRouteModelFence carries only the persisted message claim and the
// exact authorized snapshot used to form this model request.
type ConversationRouteModelFence struct {
	Claim   ConversationRouteClaim
	Context ConversationRouteContext
}

type ConversationRouteModelStore interface {
	ResolveConversationRouteModel(context.Context, ConversationRouteClaim) (string, error)
	PrepareConversationRouteModelCall(context.Context, ModelCallReservation, *ProjectModelReservation) (*ConversationRouteDecision, error)
	BeginConversationRouteModelCall(context.Context, ModelCallReservation) error
	SettleConversationRouteModelCall(context.Context, ModelCallReservation, *ConversationRouteDecision, ModelUsage, bool) error
}

var ErrRouteProjectReservationRequired = errors.New("route project reservation is required")

// ResolveConversationRouteModel selects under the current message lease and
// authorization. An explicit or saved selection is never silently replaced.
func (s *ModelGatewayService) ResolveConversationRouteModel(ctx context.Context, claim ConversationRouteClaim) (string, error) {
	store, ok := s.store.(ConversationRouteModelStore)
	if !ok {
		return "", domain.NewError(domain.CodeUnsupported, "conversation route model storage is unavailable")
	}
	return store.ResolveConversationRouteModel(ctx, claim)
}

func conversationRouteCallDigest(claim ConversationRouteClaim, value ConversationRouteContext, request ModelRequest) ([]byte, string, error) {
	type historyEvidence struct {
		MessageID string
		Sequence  int64
	}
	history := make([]historyEvidence, 0, len(value.History))
	for _, item := range value.History {
		history = append(history, historyEvidence{MessageID: item.MessageID, Sequence: item.Sequence})
	}
	digest, err := hashIdempotencyRequest(struct {
		ServiceInstanceID string
		MessageID         string
		ConversationID    string
		ActorID           string
		ModelID           string
		Instructions      string
		Input             []ModelInput
		MaxOutputTokens   int
		DisableThinking   bool
		History           []historyEvidence
		Candidates        []ConversationRouteCandidate
	}{claim.ServiceInstanceID, claim.Message.ID, claim.Message.ConversationID, claim.Message.ActorID,
		value.ModelID, request.Instructions, request.Input, request.MaxOutputTokens, request.DisableThinking, history, value.Candidates})
	if err != nil {
		return nil, "", err
	}
	idHash := sha256.Sum256(append([]byte("morde-route-model-v1\n"+claim.Message.ID+"\n"), digest...))
	return digest, "route_" + hex.EncodeToString(idHash[:]), nil
}

func (s *ModelGatewayService) routeProjectReservation(ctx context.Context, call ModelCallReservation, request ModelRequest) (*ProjectModelReservation, error) {
	if call.ProjectID == "" || call.PersonalModel {
		return nil, nil
	}
	if s.projectBudget == nil {
		return nil, domain.NewError(domain.CodeUnsupported, "project model budget is unavailable")
	}
	usage := projectUsageReservation(request, call.ReservedTokens)
	rate, err := s.projectBudget.rates.ProjectRateSnapshot(ctx, call.ModelID)
	if err != nil || rate.Validate() != nil || rate.ModelID != call.ModelID || rate.Model != call.ProviderModel {
		return nil, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	cost, err := rate.EstimateTokenCost(usage)
	if err != nil || cost < 1 {
		return nil, domain.NewError(domain.CodeInvalidState, "project model rate is unavailable")
	}
	digest, err := hashIdempotencyRequest(struct {
		ProjectID string
		ActorID   string
		Source    ModelCallSource
		CallID    string
		ModelID   string
		Usage     project.Usage
		Request   []byte
	}{call.ProjectID, call.OwnerID, call.Source, call.CallID, call.ModelID, usage, call.RequestDigest})
	if err != nil {
		return nil, err
	}
	return &ProjectModelReservation{ProjectID: call.ProjectID, ActorID: call.OwnerID, Source: call.Source, CallID: call.CallID,
		ModelID: call.ModelID, Usage: usage, ReservedTokens: call.ReservedTokens, Rate: rate,
		ReservedMicros: cost, RequestDigest: digest, AccountCall: call}, nil
}

// ClassifyConversationRoute persists one text-only inference and its charges for
// the accepted message. An uncertain dispatched call is never replayed.
func (s *ModelGatewayService) ClassifyConversationRoute(ctx context.Context, claim ConversationRouteClaim, value ConversationRouteContext) (ConversationRouteDecision, error) {
	store, ok := s.store.(ConversationRouteModelStore)
	if !ok {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeUnsupported, "conversation route model storage is unavailable")
	}
	if ctx == nil || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) || !domain.ValidMessageID(claim.Message.ID) ||
		!domain.ValidConversationID(claim.Message.ConversationID) || !domain.ValidUserID(claim.Message.OwnerID) ||
		!domain.ValidUserID(claim.Message.ActorID) || claim.Message.Role != conversation.RoleUser || claim.Message.Sequence < 1 ||
		claim.LeaseOwner == "" || claim.LeaseVersion < 1 || value.ActorID != claim.Message.ActorID || value.Content != claim.Message.Content ||
		value.ReferenceTaskID != claim.Input.ReferenceTaskID || !domain.ValidModelConnectionID(value.ModelID) {
		return ConversationRouteDecision{}, domain.InvalidArgument("route_context", "routing identity or content is invalid")
	}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, value.ActorID, value.ModelID, false, false)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	if isFirstPartyGPT6Luna(endpoint.Endpoint) {
		request.DisableThinking = true
	}
	digest, callID, err := conversationRouteCallDigest(claim, value, request)
	if err != nil {
		return ConversationRouteDecision{}, err
	}
	reserve := modelRequestInputTokenReservation(request) + int64(request.MaxOutputTokens)
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
		return *prior, nil
	}
	if err := store.BeginConversationRouteModelCall(ctx, call); err != nil {
		return ConversationRouteDecision{}, err
	}
	request.Endpoint = endpoint.Endpoint
	dispatchCtx, cancel := context.WithTimeout(ctx, ModelCallTimeout)
	defer cancel()
	response, callErr := s.client.Complete(dispatchCtx, request)
	unknown := callErr != nil || !response.Usage.Known || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > math.MaxInt64-response.Usage.OutputTokens ||
		(!endpoint.Personal && response.Usage.InputTokens+response.Usage.OutputTokens > reserve)
	var decision *ConversationRouteDecision
	if !unknown {
		parsed := ParseConversationRouteDecisionForContext(response.Text, value)
		decision = &parsed
	}
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()
	if err := store.SettleConversationRouteModelCall(settleCtx, call, decision, response.Usage, unknown); err != nil {
		return ConversationRouteDecision{}, fmt.Errorf("settle conversation route model call: %w", err)
	}
	if callErr != nil {
		return ConversationRouteDecision{}, modelConnectionCheckError(callErr)
	}
	if unknown {
		return ConversationRouteDecision{}, domain.NewError(domain.CodeInvalidState, "route model result or usage is unknown and requires reconciliation")
	}
	return *decision, nil
}
