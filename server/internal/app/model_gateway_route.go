package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
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

type ConversationRouteCoordinatorOutputStore interface {
	RecoverConversationCoordinatorModelOutput(context.Context, ConversationRouteModelFence) (string, error)
}

var ErrRouteProjectReservationRequired = errors.New("route project reservation is required")

// DSHConversationCoordinatorModelCallID derives a stable route-call identity
// without inventing or borrowing a Task ID.
func DSHConversationCoordinatorModelCallID(conversationID, messageID, sessionID string, throughSequence int64) (string, error) {
	if !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) ||
		!dshModelIdentityPattern.MatchString(sessionID) || throughSequence < 0 || throughSequence > 1<<53-1 {
		return "", domain.InvalidArgument("model_call", "invalid coordinator model call identity")
	}
	digest := sha256.Sum256([]byte("morde-dsh-coordinator-model-v1\n" + conversationID + "\n" + messageID + "\n" + sessionID + "\n" + strconv.FormatInt(throughSequence, 10)))
	return "mdl_" + hex.EncodeToString(digest[:]), nil
}

func (s *ModelGatewayService) CompleteConversationSummaryModel(ctx context.Context, fence ConversationSummaryModelFence,
	modelID string, request ModelRequest) (ModelResponse, error) {
	claim := fence.Claim
	if ctx == nil || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) || !domain.ValidMessageID(claim.Message.ID) ||
		!domain.ValidConversationID(claim.Message.ConversationID) || !domain.ValidUserID(claim.Message.OwnerID) ||
		!domain.ValidUserID(claim.Message.ActorID) || claim.Message.Role != conversation.RoleUser || claim.Message.Sequence < 1 ||
		claim.LeaseOwner == "" || claim.LeaseVersion < 1 || claim.Authorization.Version < 1 ||
		!domain.ValidOperationID(fence.OperationID) || len(fence.InputSHA256) != sha256.Size ||
		strings.TrimSpace(fence.LeaseOwner) != fence.LeaseOwner || fence.LeaseOwner == "" || fence.LeaseVersion < 1 ||
		!dshModelIdentityPattern.MatchString(fence.SessionID) || fence.ThroughSequence < 0 || fence.ThroughSequence > 1<<53-1 ||
		fence.ContextWindowTokens < 4096 || fence.ContextWindowTokens > 1_000_000 || fence.BaselineVersion < 0 || !domain.ValidModelConnectionID(modelID) ||
		request.Endpoint != (ModelEndpoint{}) || len(request.Tools) != 0 || request.RequiredTool != "" || len(request.Input) == 0 || request.OnTextDelta != nil {
		return ModelResponse{}, domain.InvalidArgument("conversation_summary_model_call", "conversation summary model identity is invalid")
	}
	claimIdentity, err := ConversationSummaryScopeForClaim(claim)
	if err != nil {
		return ModelResponse{}, err
	}
	if !validConversationSummaryIdentity(fence.Identity) ||
		fence.Identity.ServiceInstanceID != claimIdentity.ServiceInstanceID || fence.Identity.OwnerID != claimIdentity.OwnerID ||
		fence.Identity.ActorID != claimIdentity.ActorID || fence.Identity.ConversationID != claimIdentity.ConversationID ||
		fence.Identity.ScopeKind != claimIdentity.ScopeKind || fence.Identity.ScopeID != claimIdentity.ScopeID ||
		fence.Identity.AuthorizationVersion != claimIdentity.AuthorizationVersion ||
		(fence.Identity.ScopeKind == ConversationSummaryPersonalScope && fence.Identity.ScopeVersion != 0) ||
		(fence.Identity.ScopeKind == ConversationSummaryProjectScope && fence.Identity.ScopeVersion < 1) {
		return ModelResponse{}, domain.NewError(domain.CodeConflict, "conversation summary authorization identity changed")
	}
	if err := validateGatewayModelInput(request.Input, nil); err != nil {
		return ModelResponse{}, domain.InvalidArgument("conversation_summary_model_call", "conversation summary model input is invalid")
	}
	for _, input := range request.Input {
		if input.Type != "message" || len(input.Images) != 0 || input.CallID != "" || input.Name != "" || input.Arguments != "" || input.Output != "" {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "conversation summary model call must be text-only and have no tools")
		}
		for _, part := range input.Parts {
			if part.Type == "image" || part.Image != nil {
				return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "conversation summary model call cannot include images")
			}
		}
	}
	callID, err := DSHConversationSummaryModelCallID(claim.Message.ConversationID, fence.OperationID, fence.SessionID, fence.ThroughSequence)
	if err != nil {
		return ModelResponse{}, err
	}
	encoded, err := json.Marshal(struct {
		ServiceInstanceID   string
		OwnerID             string
		ActorID             string
		ConversationID      string
		MessageID           string
		MessageSequence     int64
		ProjectID           string
		AuthorizationKind   string
		AuthorizationID     string
		InstallationID      string
		AuthorizationScopes []string
		Authorization       int64
		ScopeInputDigest    []byte
		ScopeKind           string
		ScopeID             string
		ScopeVersion        int64
		OperationID         string
		BaselineVersion     int64
		SessionID           string
		ThroughSequence     int64
		ModelID             string
		Request             struct {
			Instructions    string
			Input           []ModelInput
			MaxOutputTokens int
			Temperature     *float64
			DisableThinking bool
		}
	}{claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID,
		claim.Message.ID, claim.Message.Sequence, claim.Input.ProjectID, claim.Authorization.Kind, claim.Authorization.TokenID,
		claim.Authorization.ClientInstallationID, claim.Authorization.Scopes, claim.Authorization.Version,
		fence.InputSHA256, fence.Identity.ScopeKind, fence.Identity.ScopeID, fence.Identity.ScopeVersion,
		fence.OperationID, fence.BaselineVersion, fence.SessionID, fence.ThroughSequence, modelID,
		struct {
			Instructions    string
			Input           []ModelInput
			MaxOutputTokens int
			Temperature     *float64
			DisableThinking bool
		}{request.Instructions, request.Input, request.MaxOutputTokens, request.Temperature, request.DisableThinking}})
	if err != nil {
		return ModelResponse{}, domain.InvalidArgument("conversation_summary_model_call", "conversation summary model request cannot be encoded")
	}
	principal := identity.Principal{UserID: claim.Message.ActorID, Kind: identity.Kind(claim.Authorization.Kind),
		TokenID: claim.Authorization.TokenID, ClientInstallationID: claim.Authorization.ClientInstallationID}
	for _, scope := range claim.Authorization.Scopes {
		principal.Scopes = append(principal.Scopes, identity.Scope(scope))
	}
	return s.Complete(ctx, ModelCall{Source: ModelCallCompaction, Principal: principal, Summary: &fence,
		ModelID: modelID, CallID: callID, RawRequest: encoded, Request: request})
}

func (s *ModelGatewayService) ResolveConversationSummaryModel(ctx context.Context, claim ConversationRouteClaim) (string, int, error) {
	modelID, err := s.ResolveConversationRouteModel(ctx, claim)
	if err != nil {
		return "", 0, err
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, claim.Message.ActorID, modelID, false, false)
	if err != nil {
		return "", 0, err
	}
	if endpoint.ContextWindowTokens == nil || *endpoint.ContextWindowTokens < 4096 || *endpoint.ContextWindowTokens > 1_000_000 {
		return "", 0, domain.NewError(domain.CodeUnsupported, "selected model context capacity is unavailable")
	}
	return modelID, *endpoint.ContextWindowTokens, nil
}

func (s *ModelGatewayService) CompleteConversationSummaryDSHModel(ctx context.Context, fence ConversationSummaryModelFence,
	modelID string, raw json.RawMessage) (ModelResponse, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return ModelResponse{}, domain.NewError(domain.CodeResultTooLarge, "conversation summary model request exceeds its limit")
	}
	request, err := parseDSHModelRequest(ctx, ModelTaskFence{}, modelID, raw, nil)
	if err != nil {
		return ModelResponse{}, err
	}
	return s.CompleteConversationSummaryModel(ctx, fence, modelID, request)
}

// ResolveConversationRouteModel selects under the current message lease and
// authorization. An explicit or saved selection is never silently replaced.
func (s *ModelGatewayService) ResolveConversationRouteModel(ctx context.Context, claim ConversationRouteClaim) (string, error) {
	store, ok := s.store.(ConversationRouteModelStore)
	if !ok {
		return "", domain.NewError(domain.CodeUnsupported, "conversation route model storage is unavailable")
	}
	return store.ResolveConversationRouteModel(ctx, claim)
}

func (s *ModelGatewayService) RecoverConversationCoordinatorModelOutput(ctx context.Context, claim ConversationRouteClaim,
	value ConversationRouteContext) (string, error) {
	store, ok := s.store.(ConversationRouteCoordinatorOutputStore)
	if !ok {
		return "", domain.NewError(domain.CodeUnsupported, "conversation coordinator result recovery is unavailable")
	}
	if ctx == nil || value.ActorID != claim.Message.ActorID || value.Content != claim.Message.Content ||
		value.ReferenceTaskID != claim.Input.ReferenceTaskID || !domain.ValidModelConnectionID(value.ModelID) {
		return "", domain.InvalidArgument("route_context", "coordinator recovery context is invalid")
	}
	return store.RecoverConversationCoordinatorModelOutput(ctx, ConversationRouteModelFence{Claim: claim, Context: value})
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
		ReviewAttempt     int64 `json:"review_attempt,omitempty"`
		Instructions      string
		Input             []ModelInput
		MaxOutputTokens   int
		DisableThinking   bool
		History           []historyEvidence
		Candidates        []ConversationRouteCandidate
	}{claim.ServiceInstanceID, claim.Message.ID, claim.Message.ConversationID, claim.Message.ActorID,
		value.ModelID, conversationRouteRetryAttempt(value), request.Instructions, request.Input, request.MaxOutputTokens,
		request.DisableThinking, history, value.Candidates})
	if err != nil {
		return nil, "", err
	}
	idHash := sha256.Sum256(append([]byte("morde-route-model-v1\n"+claim.Message.ID+"\n"), digest...))
	return digest, "route_" + hex.EncodeToString(idHash[:]), nil
}

func conversationRouteRetryAttempt(value ConversationRouteContext) int64 {
	if value.ResultReview != nil {
		return value.ResultReview.RetryAttempt
	}
	return 0
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

// CompleteConversationCoordinatorModel validates, authorizes, meters and
// durably settles one no-tools DSH coordinator model call. If a worker retries
// after dispatch, it reuses the saved output instead of calling the provider a
// second time.
func (s *ModelGatewayService) CompleteConversationCoordinatorModel(ctx context.Context, claim ConversationRouteClaim,
	value ConversationRouteContext, sessionID string, throughSequence int64, raw json.RawMessage) (ModelResponse, error) {
	store, ok := s.store.(ConversationRouteModelStore)
	if !ok {
		return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "conversation route model storage is unavailable")
	}
	if ctx == nil || !domain.ValidServiceInstanceID(claim.ServiceInstanceID) || !domain.ValidMessageID(claim.Message.ID) ||
		!domain.ValidConversationID(claim.Message.ConversationID) || !domain.ValidUserID(claim.Message.OwnerID) ||
		!domain.ValidUserID(claim.Message.ActorID) || claim.Message.Role != conversation.RoleUser || claim.Message.Sequence < 1 ||
		claim.LeaseOwner == "" || claim.LeaseVersion < 1 || value.ActorID != claim.Message.ActorID || value.Content != claim.Message.Content ||
		value.ReferenceTaskID != claim.Input.ReferenceTaskID || !domain.ValidModelConnectionID(value.ModelID) ||
		!dshModelIdentityPattern.MatchString(sessionID) || throughSequence < 0 || throughSequence > 1<<53-1 {
		return ModelResponse{}, domain.InvalidArgument("route_context", "coordinator model identity or content is invalid")
	}
	request, err := parseDSHModelRequest(ctx, ModelTaskFence{}, value.ModelID, raw, nil)
	if err != nil || len(request.Tools) != 0 || len(request.Input) == 0 {
		return ModelResponse{}, domain.InvalidArgument("model_call", "coordinator model request must be text-only and have no tools")
	}
	for _, item := range request.Input {
		if len(item.Images) != 0 {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "coordinator model request cannot include images")
		}
		for _, part := range item.Parts {
			if part.Type == "image" {
				return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "coordinator model request cannot include images")
			}
		}
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, value.ActorID, value.ModelID, false, false)
	if err != nil {
		return ModelResponse{}, err
	}
	if isFirstPartyGPT6Luna(endpoint.Endpoint) {
		request.DisableThinking = true
	}
	legacyRequest, err := ConversationRouteModelRequest(value)
	if err != nil {
		return ModelResponse{}, err
	}
	if isFirstPartyGPT6Luna(endpoint.Endpoint) {
		legacyRequest.DisableThinking = true
	}
	legacyDigest, legacyCallID, err := conversationRouteCallDigest(claim, value, legacyRequest)
	if err != nil {
		return ModelResponse{}, err
	}
	rawDigest := sha256.Sum256(raw)
	digest, err := hashIdempotencyRequest(struct {
		Source            string
		ServiceInstanceID string
		MessageID         string
		ConversationID    string
		ActorID           string
		SessionID         string
		ThroughSequence   int64
		Authorization     int64
		ModelID           string
		RequestDigest     []byte
	}{string(ModelCallRoute), claim.ServiceInstanceID, claim.Message.ID, claim.Message.ConversationID, claim.Message.ActorID,
		sessionID, throughSequence, claim.Authorization.Version, value.ModelID, rawDigest[:]})
	if err != nil {
		return ModelResponse{}, err
	}
	callID, err := DSHConversationCoordinatorModelCallID(claim.Message.ConversationID, claim.Message.ID, sessionID, throughSequence)
	if err != nil {
		return ModelResponse{}, err
	}
	reserve := modelRequestInputTokenReservation(request) + int64(request.MaxOutputTokens)
	if reserve < 1 {
		return ModelResponse{}, domain.InvalidArgument("model_call", "coordinator model reservation is invalid")
	}
	call := ModelCallReservation{Source: ModelCallRoute, Route: &ConversationRouteModelFence{Claim: claim, Context: value},
		ProjectID: claim.Input.ProjectID, PersonalModel: endpoint.Personal, OwnerID: value.ActorID,
		ModelID: value.ModelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		CallID: callID, RequestDigest: digest, LegacyCallID: legacyCallID, LegacyDigest: legacyDigest, ReservedTokens: reserve}
	prior, err := store.PrepareConversationRouteModelCall(ctx, call, nil)
	if errors.Is(err, ErrRouteProjectReservationRequired) {
		projectReservation, rateErr := s.routeProjectReservation(ctx, call, request)
		if rateErr != nil {
			return ModelResponse{}, rateErr
		}
		prior, err = store.PrepareConversationRouteModelCall(ctx, call, projectReservation)
	}
	if err != nil {
		return ModelResponse{}, err
	}
	if prior != nil {
		output := prior.ModelOutput
		if output == "" {
			// Older settled route rows did not retain their source text. Rebuild
			// only the already committed decision; never redispatch it.
			var encoded []byte
			encoded, err = json.Marshal(struct {
				Action   ConversationRouteAction `json:"action"`
				TaskID   string                  `json:"task_id"`
				Question string                  `json:"question"`
				Reply    string                  `json:"reply,omitempty"`
			}{prior.Action, prior.TaskID, prior.Question, prior.Reply})
			if err != nil {
				return ModelResponse{}, fmt.Errorf("encode saved coordinator decision: %w", err)
			}
			output = string(encoded)
		}
		return ModelResponse{Text: output, StopReason: "stop", Usage: ModelUsage{Known: true}}, nil
	}
	if err := store.BeginConversationRouteModelCall(ctx, call); err != nil {
		return ModelResponse{}, err
	}
	request.Endpoint = endpoint.Endpoint
	dispatchCtx, cancel := context.WithTimeout(ctx, ModelCallTimeout)
	defer cancel()
	response, callErr := s.client.Complete(dispatchCtx, request)
	unknown := callErr != nil || len(response.ToolCalls) != 0 || response.StopReason != "stop" ||
		!response.Usage.Known || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > math.MaxInt64-response.Usage.OutputTokens ||
		!utf8.ValidString(response.Text) || !ValidConversationRouteModelOutput(response.Text) ||
		(!endpoint.Personal && response.Usage.InputTokens+response.Usage.OutputTokens > reserve)
	var decision *ConversationRouteDecision
	if !unknown {
		parsed := ParseConversationRouteDecisionForContext(response.Text, value)
		decision = &parsed
	}
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelSettle()
	if err := store.SettleConversationRouteModelCall(settleCtx, call, decision, response.Usage, unknown); err != nil {
		return ModelResponse{}, fmt.Errorf("settle coordinator model call: %w", err)
	}
	if callErr != nil {
		return ModelResponse{}, modelConnectionCheckError(callErr)
	}
	if unknown {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "coordinator model result or usage is unknown and requires reconciliation")
	}
	response.ToolCalls = nil
	return response, nil
}
