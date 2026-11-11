package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type ModelCallSource string

const (
	ModelCallClient     ModelCallSource = "client"
	ModelCallChat       ModelCallSource = "chat"
	ModelCallTask       ModelCallSource = "task"
	ModelCallProbe      ModelCallSource = "probe"
	ModelCallRoute      ModelCallSource = "route"
	ModelCallCompaction ModelCallSource = "compaction"
)

type ModelTaskFence struct {
	TaskID               string
	OwnerID              string
	InitiatorID          string
	LeaseOwner           string
	LeaseVersion         int64
	AgentInstanceID      string
	AgentGeneration      int64
	AgentSessionID       string
	AgentRootSessionID   string
	AuthorizationVersion int64
	ThroughSequence      int64
	EngineVersion        string
	ProfileVersion       string
}

// ModelTaskProjectResolver reads the project scope from the persisted Task so
// a missing fence field can never turn a project call into personal billing.
type ModelTaskProjectResolver interface {
	ModelTaskProjectID(context.Context, ModelTaskFence) (string, error)
}

type ModelCall struct {
	Source        ModelCallSource
	Principal     identity.Principal
	Task          *ModelTaskFence
	Summary       *ConversationSummaryModelFence
	ModelID       string
	CallID        string
	RawRequest    json.RawMessage
	ImageResolver TaskModelImageResolver
	Request       ModelRequest
}

type ModelCallReservation struct {
	Source            ModelCallSource
	Principal         identity.Principal
	Task              *ModelTaskFence
	Summary           *ConversationSummaryModelFence
	Route             *ConversationRouteModelFence
	ProjectID         string
	PersonalModel     bool
	OwnerID           string
	ModelID           string
	ProviderModel     string
	ConnectionVersion time.Time
	NeedsTools        bool
	NeedsVision       bool
	CallID            string
	RequestDigest     []byte
	LegacyCallID      string
	LegacyDigest      []byte
	ReservedTokens    int64
}

type ModelCallSettlement struct {
	Source             ModelCallSource
	OwnerID            string
	CallID             string
	Usage              ModelUsage
	Unknown            bool
	Summary            *ConversationSummaryModelFence
	TaskCall           *ModelCallReservation
	ResponseCiphertext []byte
	ResponseDigest     []byte
	ResponseInvalid    bool
}

type ModelCallStore interface {
	ReserveModelCall(context.Context, ModelCallReservation) error
	BeginModelCallDispatch(context.Context, ModelCallReservation) error
	// AbandonModelCallReservation releases exact holds only when the caller can
	// prove no provider request was sent, including a failed Begin.
	AbandonModelCallReservation(context.Context, ModelCallReservation) error
	SettleModelCall(context.Context, ModelCallSettlement) error
}

type ConversationSummaryModelOutputStore interface {
	RecoverConversationSummaryModelOutput(context.Context, ModelCallReservation) (ConversationSummaryModelOutput, bool, error)
}

type ModelGatewayResolver interface {
	ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (ModelGatewayEndpoint, error)
	ResolveProbeEndpoint(context.Context, identity.Principal, string) (ModelGatewayEndpoint, error)
	SaveProbeCapabilities(context.Context, identity.Principal, string, time.Time, ModelCapabilities) (ModelCapabilities, error)
}

type ModelGatewayService struct {
	store          ModelCallStore
	models         ModelGatewayResolver
	client         ModelClient
	projectBudget  *ProjectBudgetService
	responseSealer *Sealer
}

const modelRequestItemTokenReserve int64 = 32

// MaxDSHModelResponseTextBytes is the maximum response text accepted by the cloud DSH worker.
// MaxDSHModelResponseToolCalls is the maximum tool-call count accepted by the cloud DSH worker.
const (
	MaxDSHModelResponseTextBytes  = 256 << 10
	MaxDSHModelResponseToolCalls  = 64
	maxDSHModelResponseTokenCount = 1<<53 - 1
)

func normalizedModelTaskFence(value *ModelTaskFence) *ModelTaskFence {
	if value == nil {
		return nil
	}
	fence := *value
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	return &fence
}

type ModelGatewayEndpoint struct {
	Endpoint            ModelEndpoint
	Version             time.Time
	Personal            bool
	ContextWindowTokens *int
}

func isFirstPartyGPT6Luna(endpoint ModelEndpoint) bool {
	if endpoint.ModelID != "gpt-6-luna" {
		return false
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.openai.com")
}

func NewModelGatewayService(store ModelCallStore, models ModelGatewayResolver, client ModelClient, projectBudgets ...*ProjectBudgetService) (*ModelGatewayService, error) {
	if store == nil || models == nil || client == nil || len(projectBudgets) > 1 || len(projectBudgets) == 1 && projectBudgets[0] == nil {
		return nil, errors.New("model gateway dependencies are required")
	}
	service := &ModelGatewayService{store: store, models: models, client: client}
	if len(projectBudgets) == 1 {
		service.projectBudget = projectBudgets[0]
	}
	return service, nil
}

func (s *ModelGatewayService) SetModelResponseSealer(sealer *Sealer) error {
	if s == nil || sealer == nil {
		return errors.New("model response sealer is required")
	}
	s.responseSealer = sealer
	return nil
}

// TaskToolCapability resolves the selected model for the task initiator, not
// the owner of a shared conversation. Only a verified text model without tool
// support takes the text-only path; lookup and authorization failures propagate.
func (s *ModelGatewayService) TaskToolCapability(ctx context.Context, value task.Task) (bool, error) {
	if ctx == nil || !domain.ValidUserID(value.InitiatorID) || !domain.ValidModelConnectionID(value.ModelID) {
		return false, domain.InvalidArgument("model", "task model identity is invalid")
	}
	_, err := s.models.ResolveGatewayEndpoint(ctx, value.InitiatorID, value.ModelID, true, false)
	if err == nil {
		return true, nil
	}
	if domain.CodeOf(err) != domain.CodeUnsupported {
		return false, err
	}
	if _, err := s.models.ResolveGatewayEndpoint(ctx, value.InitiatorID, value.ModelID, false, false); err != nil {
		return false, err
	}
	return false, nil
}

var dshModelIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func DSHModelCallID(taskID, sessionID string, throughSequence int64) (string, error) {
	if !dshModelIdentityPattern.MatchString(taskID) || !dshModelIdentityPattern.MatchString(sessionID) || throughSequence < 0 {
		return "", domain.InvalidArgument("model_call", "invalid task model call identity")
	}
	digest := sha256.Sum256([]byte("morde-dsh-model-v1\n" + taskID + "\n" + sessionID + "\n" + strconv.FormatInt(throughSequence, 10)))
	return "mdl_" + hex.EncodeToString(digest[:]), nil
}

// ValidDSHModelToolCallID reports whether a provider tool-call ID is valid in DSH history and IPC.
func ValidDSHModelToolCallID(value string) bool { return dshModelIdentityPattern.MatchString(value) }

// ValidDSHModelResponse reports whether a model response can safely cross the DSH IPC boundary.
func ValidDSHModelResponse(response ModelResponse) bool {
	if !response.Usage.Known || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > maxDSHModelResponseTokenCount || response.Usage.OutputTokens > maxDSHModelResponseTokenCount ||
		!utf8.ValidString(response.Reasoning) || len(response.Reasoning) > MaxDSHModelResponseTextBytes ||
		!utf8.ValidString(response.Text) || len(response.Text) > MaxDSHModelResponseTextBytes ||
		len(response.ToolCalls) > MaxDSHModelResponseToolCalls {
		return false
	}
	if len(response.ToolCalls) == 0 {
		return response.StopReason == "stop" && strings.TrimSpace(response.Text) != ""
	}
	if response.StopReason != "tool_calls" {
		return false
	}
	seenCallIDs := make(map[string]struct{}, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		if !ValidDSHModelToolCallID(call.CallID) {
			return false
		}
		if _, exists := seenCallIDs[call.CallID]; exists {
			return false
		}
		seenCallIDs[call.CallID] = struct{}{}
	}
	return true
}

// Complete only performs model inference. Tool calls are returned to the caller.
func (s *ModelGatewayService) Complete(ctx context.Context, call ModelCall) (ModelResponse, error) {
	if ctx == nil {
		return ModelResponse{}, domain.InvalidArgument("model_call", "model call context is required")
	}
	call.Task = normalizedModelTaskFence(call.Task)
	if (call.Source == ModelCallCompaction || call.Source == ModelCallTask) && s.responseSealer == nil {
		return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "model response encryption is unavailable")
	}
	if call.Source == ModelCallTask {
		if call.Task == nil || call.Request.Endpoint != (ModelEndpoint{}) {
			return ModelResponse{}, domain.InvalidArgument("model_call", "invalid task model call")
		}
		request, err := parseDSHModelRequest(ctx, *call.Task, call.ModelID, call.RawRequest, call.ImageResolver)
		if err != nil {
			return ModelResponse{}, err
		}
		request.Instructions = mordeSoulInstructions(request.Instructions)
		request.OnTextDelta = call.Request.OnTextDelta
		call.Request = request
	}
	ownerID, digest, reserve, err := validateModelCall(call)
	if err != nil {
		return ModelResponse{}, err
	}
	var taskDelivery ModelTaskDeliveryGuard
	var taskResponseStore TaskModelResponseStore
	if call.Source == ModelCallTask {
		var ok bool
		taskDelivery, ok = s.store.(ModelTaskDeliveryGuard)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "task model delivery fence is unavailable")
		}
		taskResponseStore, ok = s.store.(TaskModelResponseStore)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "task model response recovery is unavailable")
		}
	}
	projectID := ""
	if call.Source == ModelCallTask {
		resolver, ok := s.store.(ModelTaskProjectResolver)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "task project scope is unavailable")
		}
		projectID, err = resolver.ModelTaskProjectID(ctx, *call.Task)
		if err != nil {
			return ModelResponse{}, err
		}
	} else if call.Source == ModelCallCompaction && call.Summary != nil {
		projectID = call.Summary.Claim.Input.ProjectID
	}
	needsTools := len(call.Request.Tools) != 0
	needsVision := false
	for _, input := range call.Request.Input {
		needsTools = needsTools || input.Type == "function_call" || input.Type == "function_call_output"
		needsVision = needsVision || len(input.Images) != 0
		for _, part := range input.Parts {
			needsVision = needsVision || part.Type == "image"
		}
	}
	var endpoint ModelGatewayEndpoint
	if call.Source == ModelCallProbe {
		endpoint, err = s.models.ResolveProbeEndpoint(ctx, call.Principal, call.ModelID)
	} else {
		endpoint, err = s.models.ResolveGatewayEndpoint(ctx, ownerID, call.ModelID, needsTools, needsVision)
	}
	if err != nil {
		return ModelResponse{}, err
	}
	if call.Source == ModelCallCompaction && (call.Summary == nil || endpoint.ContextWindowTokens == nil ||
		*endpoint.ContextWindowTokens != call.Summary.ContextWindowTokens) {
		return ModelResponse{}, domain.NewError(domain.CodeConflict, "conversation summary model capacity changed")
	}
	if projectID != "" && !endpoint.Personal && s.projectBudget == nil {
		return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "project model budget is unavailable")
	}
	reservation := ModelCallReservation{
		Source: call.Source, Principal: call.Principal, Task: call.Task, Summary: call.Summary, OwnerID: ownerID,
		ProjectID: projectID, PersonalModel: endpoint.Personal,
		ModelID: call.ModelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		NeedsTools: needsTools, NeedsVision: needsVision, CallID: call.CallID, RequestDigest: digest[:], ReservedTokens: reserve,
	}
	if call.Source == ModelCallCompaction {
		if recovery, ok := s.store.(ConversationSummaryModelOutputStore); ok {
			stored, found, err := recovery.RecoverConversationSummaryModelOutput(ctx, reservation)
			if err != nil {
				return ModelResponse{}, err
			}
			if found {
				output, err := OpenConversationSummaryModelOutput(s.responseSealer, stored.Ciphertext, stored.Digest,
					call.Summary.Identity, call.Summary.OperationID, call.CallID, call.ModelID)
				if err != nil {
					return ModelResponse{}, err
				}
				return ModelResponse{Text: output.Text, StopReason: "stop", Usage: output.Usage}, nil
			}
		} else {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "conversation summary model recovery is unavailable")
		}
	}
	if call.Source == ModelCallTask {
		stored, found, err := taskResponseStore.RecoverTaskModelResponse(ctx, reservation)
		if err != nil {
			return ModelResponse{}, err
		}
		if found {
			response, err := OpenTaskModelResponse(s.responseSealer, stored, reservation)
			if err != nil {
				return ModelResponse{}, err
			}
			if err := taskDelivery.ValidateTaskModelDelivery(ctx, reservation); err != nil {
				return ModelResponse{}, err
			}
			if err := validateTaskModelResponse(call.Request, response); err != nil {
				return ModelResponse{}, err
			}
			return response, nil
		}
	}
	projectCall := projectID != "" && !endpoint.Personal
	if projectCall {
		usage := projectUsageReservation(call.Request, reserve)
		var reserveErr error
		if call.Source == ModelCallCompaction {
			_, _, reserveErr = s.projectBudget.reserveConversationSummaryModelCall(ctx, projectID, reservation, usage)
		} else {
			_, _, reserveErr = s.projectBudget.reserveTaskModelCall(ctx, projectID, reservation, usage)
		}
		if reserveErr != nil {
			return ModelResponse{}, reserveErr
		}
	} else if err := s.store.ReserveModelCall(ctx, reservation); err != nil {
		return ModelResponse{}, err
	}
	request := call.Request
	request.Endpoint = endpoint.Endpoint
	if call.Source == ModelCallTask && isFirstPartyGPT6Luna(endpoint.Endpoint) {
		request.DisableThinking = true
	}
	dispatchCtx, cancelDispatch := context.WithTimeout(ctx, ModelCallTimeout)
	defer cancelDispatch()
	var dispatchErr error
	if call.Source == ModelCallTask && request.OnTextDelta != nil {
		if streamStore, ok := s.store.(ModelTaskStreamingDispatchStore); ok {
			var allowed bool
			allowed, dispatchErr = streamStore.BeginModelCallDispatchWithTextStreaming(dispatchCtx, reservation)
			if !allowed {
				request.OnTextDelta = nil
			}
		} else {
			request.OnTextDelta = nil
			dispatchErr = s.store.BeginModelCallDispatch(dispatchCtx, reservation)
		}
	} else {
		dispatchErr = s.store.BeginModelCallDispatch(dispatchCtx, reservation)
	}
	if dispatchErr != nil {
		abandonCtx, cancelAbandon := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelAbandon()
		if abandonErr := s.store.AbandonModelCallReservation(abandonCtx, reservation); abandonErr != nil {
			return ModelResponse{}, errors.Join(dispatchErr, fmt.Errorf("release undispatched model reservation: %w", abandonErr))
		}
		return ModelResponse{}, dispatchErr
	}
	dispatchErr = dispatchCtx.Err()
	if call.Source == ModelCallTask {
		if lifecycle := ModelTaskLifecycle(ctx); dispatchErr == nil && lifecycle != nil {
			dispatchErr = lifecycle.Err()
		}
		if explicit := ModelExplicitCancellation(ctx); dispatchErr == nil && explicit != nil {
			dispatchErr = explicit.Err()
		}
	}
	if call.Source == ModelCallTask && dispatchErr != nil {
		abandonCtx, cancelAbandon := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelAbandon()
		if abandonErr := s.store.AbandonModelCallReservation(abandonCtx, reservation); abandonErr != nil {
			return ModelResponse{}, errors.Join(dispatchErr, fmt.Errorf("release undispatched model reservation: %w", abandonErr))
		}
		return ModelResponse{}, dispatchErr
	}
	providerCtx := context.Context(dispatchCtx)
	finishProvider := func() {}
	if call.Source == ModelCallTask {
		providerCtx, finishProvider = taskModelProviderContext(ctx, dispatchCtx, reservation, s.store)
		if callback := request.OnTextDelta; callback != nil {
			explicit := ModelExplicitCancellation(ctx)
			lifecycle := ModelTaskLifecycle(ctx)
			request.OnTextDelta = func(delta string) error {
				if ctx.Err() != nil || explicit != nil && explicit.Err() != nil || lifecycle != nil && lifecycle.Err() != nil {
					return nil
				}
				err := callback(delta)
				if ctx.Err() != nil || explicit != nil && explicit.Err() != nil || lifecycle != nil && lifecycle.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
	if call.Source == ModelCallTask && providerCtx.Err() != nil {
		err := providerCtx.Err()
		finishProvider()
		abandonCtx, cancelAbandon := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelAbandon()
		if abandonErr := s.store.AbandonModelCallReservation(abandonCtx, reservation); abandonErr != nil {
			return ModelResponse{}, errors.Join(err, fmt.Errorf("release undispatched model reservation: %w", abandonErr))
		}
		return ModelResponse{}, err
	}
	response, callErr := s.client.Complete(providerCtx, request)
	finishProvider()
	settlement := ModelCallSettlement{Source: call.Source, OwnerID: ownerID, CallID: call.CallID, Usage: response.Usage,
		Unknown: callErr != nil || !response.Usage.Known, Summary: call.Summary}
	if response.Usage.Known && (response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || response.Usage.InputTokens > math.MaxInt64-response.Usage.OutputTokens) {
		settlement.Unknown = true
		settlement.Usage = ModelUsage{}
		callErr = &ModelClientError{Kind: ModelErrorInvalidResponse}
	}
	var summaryOutputErr error
	if call.Source == ModelCallCompaction && !settlement.Unknown {
		if response.StopReason != "stop" || len(response.ToolCalls) != 0 || strings.TrimSpace(response.Text) == "" ||
			!utf8.ValidString(response.Text) || strings.ContainsRune(response.Text, '\x00') || len(response.Text) > MaxConversationSummaryBytes {
			settlement.ResponseInvalid = true
			summaryOutputErr = domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid conversation summary")
		} else {
			settlement.ResponseCiphertext, settlement.ResponseDigest, err = SealConversationSummaryModelOutput(s.responseSealer,
				ConversationSummaryModelOutput{Identity: call.Summary.Identity, OperationID: call.Summary.OperationID,
					CallID: call.CallID, ModelID: call.ModelID, Text: response.Text, Usage: response.Usage})
			if err != nil {
				settlement.ResponseInvalid = true
				summaryOutputErr = domain.NewError(domain.CodeInvalidState, "conversation summary model result could not be stored safely")
			}
		}
	}
	var taskOutputErr error
	if call.Source == ModelCallTask && !settlement.Unknown {
		taskOutputErr = validateTaskModelResponse(call.Request, response)
		if taskOutputErr == nil {
			taskOutputErr = taskDelivery.ValidateTaskModelDelivery(ctx, reservation)
		}
		if taskOutputErr == nil {
			stored, sealErr := SealTaskModelResponse(s.responseSealer, reservation, response)
			if sealErr != nil {
				settlement.ResponseInvalid = true
				taskOutputErr = domain.NewError(domain.CodeInvalidState, "task model response could not be stored safely")
			} else {
				settlement.TaskCall = &reservation
				settlement.ResponseCiphertext, settlement.ResponseDigest = stored.Ciphertext, stored.Digest
			}
		} else {
			settlement.ResponseInvalid = true
		}
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var settleErr error
	if projectCall {
		settleErr = s.projectBudget.SettleModelCall(settleCtx, SettleProjectModelCallCommand{
			ProjectID: projectID, Source: call.Source, CallID: call.CallID,
			Usage: project.Usage{InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}, Unknown: settlement.Unknown,
			Summary: call.Summary, TaskCall: settlement.TaskCall, ResponseCiphertext: settlement.ResponseCiphertext,
			ResponseDigest: settlement.ResponseDigest, ResponseInvalid: settlement.ResponseInvalid,
		})
	} else {
		settleErr = s.store.SettleModelCall(settleCtx, settlement)
	}
	if settleErr != nil {
		if call.Source == ModelCallCompaction && !projectCall {
			unknownCtx, cancelUnknown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelUnknown()
			unknownErr := s.store.SettleModelCall(unknownCtx, ModelCallSettlement{Source: call.Source, OwnerID: ownerID,
				CallID: call.CallID, Unknown: true, Summary: call.Summary})
			if unknownErr != nil {
				return ModelResponse{}, errors.Join(fmt.Errorf("settle model call: %w", settleErr),
					fmt.Errorf("preserve unknown model usage: %w", unknownErr))
			}
		}
		return ModelResponse{}, fmt.Errorf("settle model call: %w", settleErr)
	}
	if err := ctx.Err(); err != nil {
		return ModelResponse{}, err
	}
	if call.Source == ModelCallTask {
		if lifecycle := ModelTaskLifecycle(ctx); lifecycle != nil && lifecycle.Err() != nil {
			return ModelResponse{}, lifecycle.Err()
		}
		if explicit := ModelExplicitCancellation(ctx); explicit != nil && explicit.Err() != nil {
			return ModelResponse{}, explicit.Err()
		}
	}
	if callErr != nil {
		return ModelResponse{}, modelConnectionCheckError(callErr)
	}
	if summaryOutputErr != nil {
		return ModelResponse{}, summaryOutputErr
	}
	if taskOutputErr != nil {
		return ModelResponse{}, taskOutputErr
	}
	if !response.Usage.Known {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation")
	}
	if !endpoint.Personal && response.Usage.InputTokens+response.Usage.OutputTokens > reserve {
		return ModelResponse{}, domain.NewError(domain.CodeQuotaExceeded, "model provider usage exceeded the reserved budget")
	}
	if taskDelivery != nil {
		if err := taskDelivery.ValidateTaskModelDelivery(ctx, reservation); err != nil {
			return ModelResponse{}, err
		}
	}
	if call.Source == ModelCallTask && !ValidDSHModelResponse(response) {
		return ModelResponse{}, domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid task response")
	}
	seenCallIDs := make(map[string]struct{}, len(response.ToolCalls))
	for _, toolCall := range response.ToolCalls {
		if !validAgentJSONObject([]byte(toolCall.Arguments), maxAgentToolArgsBytes) ||
			(call.Source != ModelCallTask && (len(toolCall.CallID) < 1 || len(toolCall.CallID) > 256)) {
			return ModelResponse{}, domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid tool call")
		}
		if call.Source != ModelCallTask {
			if _, exists := seenCallIDs[toolCall.CallID]; exists {
				return ModelResponse{}, domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid tool call")
			}
			seenCallIDs[toolCall.CallID] = struct{}{}
		}
		declared := false
		for _, tool := range call.Request.Tools {
			declared = declared || tool.Name == toolCall.Name
		}
		if !declared || (call.Request.RequiredTool != "" && toolCall.Name != call.Request.RequiredTool) {
			return ModelResponse{}, domain.NewError(domain.CodeUpstreamError, "model provider returned a tool call not allowed by the request")
		}
	}
	return response, nil
}

func projectUsageReservation(request ModelRequest, reservedTokens int64) project.Usage {
	output := int64(request.MaxOutputTokens)
	return project.Usage{InputTokens: reservedTokens - output, OutputTokens: output}
}

func validateModelCall(call ModelCall) (string, [32]byte, int64, error) {
	var empty [32]byte
	if call.Task != nil && call.Task.AgentRootSessionID == "" {
		call.Task = normalizedModelTaskFence(call.Task)
	}
	if !domain.ValidModelConnectionID(call.ModelID) || !idempotencyKeyPattern.MatchString(call.CallID) || call.Request.Endpoint != (ModelEndpoint{}) {
		return "", empty, 0, domain.InvalidArgument("model_call", "invalid model call")
	}
	ownerID := call.Principal.UserID
	switch call.Source {
	case ModelCallClient, ModelCallChat, ModelCallProbe:
		if call.Task != nil || call.Summary != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid user model call")
		}
		if err := validateClientAccountPrincipal(call.Principal); err != nil {
			return "", empty, 0, err
		}
	case ModelCallTask:
		if call.Task == nil || !domain.ValidUserID(call.Task.OwnerID) || !domain.ValidUserID(call.Task.InitiatorID) || call.Task.LeaseOwner == "" || call.Task.LeaseVersion < 1 ||
			call.Task.AgentInstanceID == "" || call.Task.AgentGeneration < 1 || call.Task.AuthorizationVersion < 1 ||
			!dshModelIdentityPattern.MatchString(call.Task.AgentRootSessionID) ||
			invalidCheckpointLabel(call.Task.EngineVersion, 128) || invalidCheckpointLabel(call.Task.ProfileVersion, 128) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid task execution fence")
		}
		id, err := DSHModelCallID(call.Task.TaskID, call.Task.AgentSessionID, call.Task.ThroughSequence)
		if err != nil || id != call.CallID || len(call.RawRequest) == 0 || !json.Valid(call.RawRequest) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid task model call identity or request")
		}
		ownerID = call.Task.InitiatorID
	case ModelCallCompaction:
		if call.Task != nil || call.Summary == nil || ownerID != call.Summary.Claim.Message.ActorID ||
			call.Summary.Identity.ActorID != ownerID || call.Summary.BaselineVersion < 0 ||
			len(call.Summary.InputSHA256) != sha256.Size || len(call.Request.Tools) != 0 || call.Request.RequiredTool != "" {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid conversation summary model fence")
		}
		for _, input := range call.Request.Input {
			if input.Type != "message" || len(input.Images) != 0 || input.CallID != "" || input.Name != "" || input.Arguments != "" || input.Output != "" {
				return "", empty, 0, domain.InvalidArgument("model_call", "conversation summary model call must be text-only")
			}
			for _, part := range input.Parts {
				if part.Type == "image" || part.Image != nil {
					return "", empty, 0, domain.InvalidArgument("model_call", "conversation summary model call cannot include images")
				}
			}
		}
		expectedCallID, err := DSHConversationSummaryModelCallID(call.Summary.Claim.Message.ConversationID,
			call.Summary.OperationID, call.Summary.SessionID, call.Summary.ThroughSequence)
		if err != nil || expectedCallID != call.CallID {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid conversation summary model call identity")
		}
	default:
		return "", empty, 0, domain.InvalidArgument("model_call", "invalid model call source")
	}
	if call.Request.MaxOutputTokens < 1 || call.Request.MaxOutputTokens > 32768 {
		return "", empty, 0, domain.InvalidArgument("model_call", "model token limit is invalid")
	}
	inputReservation := modelRequestInputTokenReservation(call.Request)
	if inputReservation > maxAgentInputBytes {
		return "", empty, 0, domain.NewError(domain.CodeResultTooLarge, "model request exceeds input budget")
	}
	if call.Request.Temperature != nil && (math.IsNaN(*call.Request.Temperature) || math.IsInf(*call.Request.Temperature, 0) ||
		*call.Request.Temperature < 0 || *call.Request.Temperature > 2) {
		return "", empty, 0, domain.InvalidArgument("model_call", "model temperature is invalid")
	}
	if !utf8.ValidString(call.Request.Instructions) || validateGatewayModelInput(call.Request.Input, call.Request.Tools) != nil {
		return "", empty, 0, domain.InvalidArgument("model_call", "model input is invalid")
	}
	if call.Request.RequiredTool != "" {
		found := false
		for _, tool := range call.Request.Tools {
			found = found || tool.Name == call.Request.RequiredTool
		}
		if !found {
			return "", empty, 0, domain.InvalidArgument("model_call", "required model tool is invalid")
		}
	}
	reserve := inputReservation + int64(call.Request.MaxOutputTokens)
	if reserve < 1 {
		return "", empty, 0, domain.InvalidArgument("model_call", "model token reservation is invalid")
	}
	raw := call.RawRequest
	if len(raw) != 0 && !json.Valid(raw) {
		return "", empty, 0, domain.InvalidArgument("model_call", "model request is not valid JSON")
	}
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(struct {
			ModelID         string
			Instructions    string
			Input           []ModelInput
			Tools           []ModelTool
			RequiredTool    string
			MaxOutputTokens int
			Temperature     *float64
			DisableThinking bool `json:",omitempty"`
		}{call.ModelID, call.Request.Instructions, call.Request.Input, call.Request.Tools, call.Request.RequiredTool, call.Request.MaxOutputTokens, call.Request.Temperature, call.Request.DisableThinking})
		if err != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "model request cannot be encoded")
		}
	}
	digest := sha256.Sum256(raw)
	return ownerID, digest, reserve, nil
}

// shortcut: UTF-8 byte counts plus 32 tokens per bounded request item; use provider token counters if a supported endpoint can exceed this framing margin.
func modelRequestInputTokenReservation(request ModelRequest) int64 {
	reserve := modelRequestItemTokenReserve + int64(len(request.Instructions)+len(request.RequiredTool))
	if request.RequiredTool != "" {
		reserve += modelRequestItemTokenReserve
	}
	for _, input := range request.Input {
		reserve += int64(len(input.Type) + len(input.Role) + len(input.Text) + len(input.CallID) + len(input.Name) + len(input.Arguments) + len(input.Output))
		reserve += modelRequestItemTokenReserve + int64(len(input.Images))*agentImageTokenReserve
		for _, image := range input.Images {
			reserve += int64(len(image.MediaType))
		}
		for _, part := range input.Parts {
			reserve += int64(len(part.Type)+len(part.Text)) + modelRequestItemTokenReserve
			if part.Image != nil {
				reserve += agentImageTokenReserve + int64(len(part.Image.MediaType))
			}
		}
	}
	for _, tool := range request.Tools {
		reserve += int64(len(tool.Name)+len(tool.Description)+len(tool.Parameters)) + modelRequestItemTokenReserve
	}
	return reserve
}

func validateGatewayModelInput(inputs []ModelInput, tools []ModelTool) error {
	validationInputs := append([]ModelInput(nil), inputs...)
	for i := range validationInputs {
		input := &validationInputs[i]
		if input.Type == "message" {
			if len(input.Parts) > 128 {
				return errors.New("too many message parts")
			}
			if len(input.Parts) != 0 {
				if input.Text != "" || len(input.Images) != 0 {
					return errors.New("model message content is ambiguous")
				}
				for _, part := range input.Parts {
					switch part.Type {
					case "text":
						if part.Image != nil || !utf8.ValidString(part.Text) {
							return errors.New("invalid text content")
						}
						input.Text += part.Text
					case "reasoning":
						if input.Role != "assistant" || part.Image != nil || !utf8.ValidString(part.Text) {
							return errors.New("invalid reasoning content")
						}
						input.Text += part.Text
					case "image":
						if part.Image == nil || part.Text != "" {
							return errors.New("invalid image content")
						}
						input.Images = append(input.Images, *part.Image)
					default:
						return errors.New("invalid message content")
					}
				}
				input.Parts = nil
			}
			if !utf8.ValidString(input.Text) || (input.Text == "" && len(input.Images) == 0) {
				return errors.New("model message is empty or invalid")
			}
			if input.Role == "system" || input.Role == "developer" {
				input.Role = "user"
			}
		} else if len(input.Parts) != 0 {
			return errors.New("non-message input cannot contain message parts")
		}
	}
	if err := validateAgentInput(validationInputs, tools); err != nil {
		return err
	}
	definitions := make([]AgentToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, AgentToolDefinition{ModelTool: tool, RequiredScope: identity.ScopeComputerObserve})
	}
	_, err := validateAgentTools(definitions)
	return err
}
