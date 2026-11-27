package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type ModelCallSource string

const (
	ModelCallClient           ModelCallSource = "client"
	ModelCallChat             ModelCallSource = "chat"
	ModelCallTask             ModelCallSource = "task"
	ModelCallProbe            ModelCallSource = "probe"
	ModelCallRoute            ModelCallSource = "route"
	ModelCallCompaction       ModelCallSource = "compaction"
	ModelCallReview           ModelCallSource = "review"
	ModelCallTerminalDelivery ModelCallSource = "terminal_delivery"
)

type ModelTaskFence struct {
	TaskID               string
	OwnerID              string
	InitiatorID          string
	TaskRevision         int64
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
	// RecoveryCallID is the durable model-call ledger identity for the one
	// scheduled recovery. RecoveryDSHCallID is a separate DSH frame identity;
	// neither reuses the failed call's identity.
	RecoveryCallID    string
	RecoveryDSHCallID string
}

// ModelTaskProjectResolver reads the project scope from the persisted Task so
// a missing fence field can never turn a project call into personal billing.
type ModelTaskProjectResolver interface {
	ModelTaskProjectID(context.Context, ModelTaskFence) (string, error)
}

type ModelCall struct {
	Source                      ModelCallSource
	Principal                   identity.Principal
	Task                        *ModelTaskFence
	Review                      *TaskValidationReviewModelFence
	TerminalDelivery            *TaskTerminalDeliveryModelFence
	Summary                     *ConversationSummaryModelFence
	ModelID                     string
	CallID                      string
	BudgetPlanID                string
	BudgetCallID                string
	BudgetRequestDigest         string
	RawRequest                  json.RawMessage
	ImageResolver               TaskModelImageResolver
	Request                     ModelRequest
	ReplayTaskModelResponseOnly bool
}

type ModelCallReservation struct {
	Source                  ModelCallSource
	Principal               identity.Principal
	Task                    *ModelTaskFence
	Review                  *TaskValidationReviewModelFence
	TerminalDelivery        *TaskTerminalDeliveryModelFence
	Summary                 *ConversationSummaryModelFence
	Route                   *ConversationRouteModelFence
	ProjectID               string
	PersonalModel           bool
	OwnerID                 string
	ModelID                 string
	ProviderEndpoint        string
	ProviderModel           string
	ConnectionVersion       time.Time
	NeedsTools              bool
	NeedsVision             bool
	CallID                  string
	BudgetPlanID            string
	BudgetCallID            string
	BudgetRequestDigest     string
	BudgetCapabilityVersion string
	DSHCallID               string
	RequestDigest           []byte
	LegacyCallID            string
	LegacyDigest            []byte
	ReservedTokens          int64
}

type ModelCallSettlement struct {
	Source                ModelCallSource
	OwnerID               string
	CallID                string
	Usage                 ModelUsage
	ProviderReportedModel string
	Unknown               bool
	Summary               *ConversationSummaryModelFence
	TaskCall              *ModelCallReservation
	ReviewCall            *ModelCallReservation
	TerminalDeliveryCall  *ModelCallReservation
	ResponseCiphertext    []byte
	ResponseDigest        []byte
	ResponseTextDigest    []byte
	ResponseInvalid       bool
}

// TaskModelNoDispatchEvidence is proof that a specific Task model call did not
// reach the provider. The Model Gateway creates it only after a failed
// BeginModelCallDispatch is followed by a successful reservation abandonment.
// A provider error, settlement error, or failed abandonment must never create
// this evidence.
type TaskModelNoDispatchEvidence struct {
	TaskFence     ModelTaskFence
	CallID        string
	ModelID       string
	RequestDigest []byte
	UsageKnown    bool
	InputTokens   int64
	OutputTokens  int64
}

type TaskModelRecoveryFailureKind string

const (
	TaskModelRecoveryFailureNoDispatch     TaskModelRecoveryFailureKind = "no_dispatch"
	TaskModelRecoveryFailureProviderReject TaskModelRecoveryFailureKind = "provider_reject"
)

// TaskModelRecoveryFailureEvidence is bounded proof that a model call failed
// in a way that can be considered once by the read-only recovery path.
type TaskModelRecoveryFailureEvidence struct {
	Kind              TaskModelRecoveryFailureKind
	TaskFence         ModelTaskFence
	CallID            string
	ModelID           string
	RequestDigest     []byte
	UsageKnown        bool
	InputTokens       int64
	OutputTokens      int64
	ProviderErrorKind ModelClientErrorKind
}

type TaskModelRecoveryFailure interface {
	TaskModelRecoveryFailureEvidence() TaskModelRecoveryFailureEvidence
}

// TaskModelNoDispatchError carries bounded identity evidence while keeping the
// underlying persistence error out of Error().
type TaskModelNoDispatchError struct {
	evidence TaskModelNoDispatchEvidence
	cause    error
}

func (e *TaskModelNoDispatchError) Error() string {
	return "task model request was confirmed not dispatched"
}

func (e *TaskModelNoDispatchError) Unwrap() error { return e.cause }

func (e *TaskModelNoDispatchError) TaskModelNoDispatchEvidence() TaskModelNoDispatchEvidence {
	if e == nil {
		return TaskModelNoDispatchEvidence{}
	}
	return e.evidence
}

func (e *TaskModelNoDispatchError) TaskModelRecoveryFailureEvidence() TaskModelRecoveryFailureEvidence {
	if e == nil {
		return TaskModelRecoveryFailureEvidence{}
	}
	return TaskModelRecoveryFailureEvidence{Kind: TaskModelRecoveryFailureNoDispatch, TaskFence: e.evidence.TaskFence,
		CallID: e.evidence.CallID, ModelID: e.evidence.ModelID, RequestDigest: append([]byte(nil), e.evidence.RequestDigest...),
		UsageKnown: e.evidence.UsageKnown, InputTokens: e.evidence.InputTokens,
		OutputTokens: e.evidence.OutputTokens}
}

// TaskModelRetryableFailureError is returned only after a bounded provider
// rejection and its known usage have been durably settled.
type TaskModelRetryableFailureError struct {
	evidence TaskModelRecoveryFailureEvidence
	cause    error
}

func (e *TaskModelRetryableFailureError) Error() string {
	return "task model request was rejected with confirmed zero usage"
}

func (e *TaskModelRetryableFailureError) Unwrap() error { return e.cause }

func (e *TaskModelRetryableFailureError) TaskModelRecoveryFailureEvidence() TaskModelRecoveryFailureEvidence {
	if e == nil {
		return TaskModelRecoveryFailureEvidence{}
	}
	result := e.evidence
	result.RequestDigest = append([]byte(nil), e.evidence.RequestDigest...)
	return result
}

func (e *TaskModelRetryableFailureError) ModelFailureCategory() string {
	if e == nil {
		return "unknown"
	}
	return string(e.evidence.ProviderErrorKind)
}

func (e *TaskModelRetryableFailureError) ModelFailureDiagnostic() AgentModelFailureDiagnostic {
	return AgentModelFailureDiagnostic{Stage: "model_gateway", Category: e.ModelFailureCategory(),
		ReasonCode: "provider_rejected_request"}
}

func confirmedTaskModelProviderRejection(err error, usage ModelUsage) bool {
	if err == nil || usage.Known && (usage.InputTokens != 0 || usage.OutputTokens != 0) {
		return false
	}
	var providerErr *ModelClientError
	if !errors.As(err, &providerErr) || providerErr.Stage != "provider_status" || !providerErr.ConfirmedNoUsage {
		return false
	}
	return providerErr.Kind == ModelErrorRateLimited || providerErr.Kind == ModelErrorUnavailable
}

type ModelUsageObservation struct {
	Source                    ModelCallSource
	Known                     bool
	InputTokens, OutputTokens int64
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
	store                 ModelCallStore
	models                ModelGatewayResolver
	client                ModelClient
	projectBudget         *ProjectBudgetService
	responseSealer        *Sealer
	gpt6LunaMaxReasoning  bool
	usageObserverMu       sync.RWMutex
	usageObserver         func(ModelUsageObservation)
	modelBudgetStrategyMu sync.RWMutex
	modelBudgetStrategy   *modelOutputBudgetStrategySnapshot
}

func (s *ModelGatewayService) SetModelUsageObserver(observer func(ModelUsageObservation)) {
	if s == nil {
		return
	}
	s.usageObserverMu.Lock()
	s.usageObserver = observer
	s.usageObserverMu.Unlock()
}

func (s *ModelGatewayService) observeModelUsage(settlement ModelCallSettlement) {
	s.usageObserverMu.RLock()
	observer := s.usageObserver
	s.usageObserverMu.RUnlock()
	if observer == nil {
		return
	}
	value := ModelUsageObservation{Source: settlement.Source, Known: !settlement.Unknown && settlement.Usage.Known}
	if value.Known {
		value.InputTokens, value.OutputTokens = settlement.Usage.InputTokens, settlement.Usage.OutputTokens
	}
	// shortcut: Metrics are advisory; drop a panicking observer so it cannot
	// change the result of an already-settled model call.
	func() {
		defer func() { _ = recover() }()
		observer(value)
	}()
}

const modelRequestItemTokenReserve int64 = 32

const modelConcurrencyReservationRetryMaxDelay = time.Second

// MaxDSHModelResponseTextBytes is the maximum response text accepted by the cloud DSH worker.
// MaxDSHModelResponseToolCalls is the maximum tool-call count accepted by the cloud DSH worker.
const (
	MaxDSHModelResponseTextBytes  = 256 << 10
	MaxDSHModelResponseToolCalls  = 64
	maxDSHModelResponseTokenCount = 1<<53 - 1
	// OpenAI recommends reserving at least 25,000 tokens for max reasoning and output.
	maxGPT6LunaReasoningTokens = 25_000
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
	Endpoint                    ModelEndpoint
	Version                     time.Time
	Personal                    bool
	ContextWindowTokens         *int
	ContextWindowSource         ModelContextWindowSource
	ContextWindowCatalogVersion *string
	MaxOutputTokens             *int
	MaxOutputSource             ModelMaxOutputSource
	MaxOutputResolvedAt         *time.Time
	MaxOutputCatalogVersion     *string
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

func (s *ModelGatewayService) SetGPT6LunaMaxReasoning(enabled bool) {
	s.gpt6LunaMaxReasoning = enabled
}

func modelConcurrencyReservationLimited(err error) bool {
	var modelErr *domain.Error
	return errors.As(err, &modelErr) && modelErr.Code == domain.CodeRateLimited &&
		modelErr.Details["reason"] == "model_concurrency_limit" && modelErr.Details["stage"] == "reservation"
}

// reserveModelCall retries only a typed, pre-dispatch account concurrency limit.
// Other rate limits, uncertain reservations, and provider errors remain terminal.
// The wait between attempts is the shared jittered backoff with this path's
// own policy (100ms base, 1s cap): a local concurrency slot is not a provider
// error, so the E12 provider ladder (base 5s / max 120s, 3 retries) does not
// apply here. Task calls wait until their context ends, as before. Summary
// and review calls retry at most ModelRetryMaxRetries times so they cannot
// hold a lease indefinitely. A single wait stays bounded (base cap 1s plus
// at most 50% jitter).
func (s *ModelGatewayService) reserveModelCall(ctx context.Context, source ModelCallSource, projectCall bool, projectID string,
	reservation ModelCallReservation, usage project.Usage) error {
	attempt := 0
	for {
		var err error
		if projectCall {
			switch source {
			case ModelCallCompaction:
				_, _, err = s.projectBudget.reserveConversationSummaryModelCall(ctx, projectID, reservation, usage)
			case ModelCallReview:
				_, _, err = s.projectBudget.reserveTaskValidationReviewModelCall(ctx, projectID, reservation, usage)
			case ModelCallTerminalDelivery:
				_, _, err = s.projectBudget.reserveTaskTerminalDeliveryModelCall(ctx, projectID, reservation, usage)
			default:
				_, _, err = s.projectBudget.reserveTaskModelCall(ctx, projectID, reservation, usage)
			}
		} else {
			err = s.store.ReserveModelCall(ctx, reservation)
		}
		if err == nil || (source != ModelCallTask && source != ModelCallCompaction && source != ModelCallReview) ||
			!modelConcurrencyReservationLimited(err) || (source != ModelCallTask && attempt >= ModelRetryMaxRetries) {
			return err
		}
		attempt++
		delay := JitteredBackoff(attempt, 100*time.Millisecond, modelConcurrencyReservationRetryMaxDelay, ModelRetryJitterRatio, rand.Float64)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func (s *ModelGatewayService) applyGPT6LunaReasoning(endpoint ModelEndpoint, request *ModelRequest, disableByDefault bool) {
	if !isFirstPartyGPT6Luna(endpoint) {
		return
	}
	if s.gpt6LunaMaxReasoning {
		request.DisableThinking = false
		request.ReasoningEffort = "max"
		if request.MaxOutputTokens < maxGPT6LunaReasoningTokens {
			request.MaxOutputTokens = maxGPT6LunaReasoningTokens
		}
	} else if disableByDefault {
		request.DisableThinking = true
	}
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

func (s *ModelGatewayService) ResolveTaskContextWindow(ctx context.Context, value task.Task) (*int, error) {
	if ctx == nil || !domain.ValidUserID(value.InitiatorID) || !domain.ValidModelConnectionID(value.ModelID) {
		return nil, domain.InvalidArgument("model", "task model identity is invalid")
	}
	endpoint, err := s.models.ResolveGatewayEndpoint(ctx, value.InitiatorID, value.ModelID, false, false)
	if err != nil {
		return nil, err
	}
	if endpoint.ContextWindowTokens == nil || *endpoint.ContextWindowTokens < 4096 || *endpoint.ContextWindowTokens > MaxModelContextWindowTokens {
		return nil, nil
	}
	return copyInt(endpoint.ContextWindowTokens), nil
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
	if !ValidProviderReportedModel(response.ProviderReportedModel) || !response.Usage.Known || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
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
	if call.ReplayTaskModelResponseOnly && call.Source != ModelCallTask {
		return ModelResponse{}, domain.InvalidArgument("model_call", "replay-only model recovery requires a Task call")
	}
	call.Task = normalizedModelTaskFence(call.Task)
	if (call.Source == ModelCallCompaction || call.Source == ModelCallTask || call.Source == ModelCallReview || call.Source == ModelCallTerminalDelivery) && s.responseSealer == nil {
		return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "model response encryption is unavailable")
	}
	if call.Source == ModelCallTerminalDelivery {
		if call.TerminalDelivery == nil {
			return ModelResponse{}, domain.InvalidArgument("model_call", "invalid terminal delivery model call")
		}
		stored, found, err := s.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, *call.TerminalDelivery, call.RawRequest)
		if err != nil {
			return ModelResponse{}, err
		}
		if found {
			return stored, nil
		}
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
	if call.Source == ModelCallReview && (call.Review == nil || call.Task != nil || call.Summary != nil ||
		len(call.Request.Tools) != 0 || call.Request.RequiredTool != "") {
		return ModelResponse{}, domain.InvalidArgument("model_call", "invalid Task validation review model call")
	}
	if call.Source == ModelCallTerminalDelivery && (call.TerminalDelivery == nil || !ValidTaskTerminalDeliveryModelDispatchFence(*call.TerminalDelivery) || call.Task != nil || call.Review != nil ||
		call.Summary != nil || call.Principal.UserID != "" || call.Principal.TokenID != "" ||
		call.Principal.ClientInstallationID != "" || len(call.Request.Tools) != 0 || call.Request.RequiredTool != "") {
		return ModelResponse{}, domain.InvalidArgument("model_call", "invalid terminal delivery model call")
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
	} else if call.Source == ModelCallReview {
		resolver, ok := s.store.(TaskValidationReviewModelProjectResolver)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "Task validation review project scope is unavailable")
		}
		projectID, err = resolver.ModelTaskProjectIDForReview(ctx, *call.Review)
		if err != nil {
			return ModelResponse{}, err
		}
	} else if call.Source == ModelCallTerminalDelivery {
		resolver, ok := s.store.(TaskTerminalDeliveryModelProjectResolver)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "terminal delivery project scope is unavailable")
		}
		projectID, err = resolver.ModelTaskProjectIDForTerminalDelivery(ctx, *call.TerminalDelivery)
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
	if call.Source == ModelCallTask || call.Source == ModelCallCompaction || call.Source == ModelCallReview {
		request := call.Request
		if call.Source == ModelCallCompaction {
			// Summary requests have a small, strict output budget. Do not spend it
			// on hidden reasoning, even when the account-wide GPT-6 setting asks
			// normal Task calls to use maximum reasoning.
			request.DisableThinking = true
			request.ReasoningEffort = ""
		} else {
			s.applyGPT6LunaReasoning(endpoint.Endpoint, &request, true)
		}
		call.Request = request
		ownerID, digest, reserve, err = validateModelCall(call)
		if err != nil {
			return ModelResponse{}, err
		}
	}
	if call.Source == ModelCallCompaction {
		contextWindow := 4096
		if endpoint.ContextWindowTokens != nil {
			contextWindow = *endpoint.ContextWindowTokens
		}
		if call.Summary == nil || contextWindow < 4096 || contextWindow > MaxModelContextWindowTokens ||
			call.Summary.ContextWindowTokens > contextWindow {
			return ModelResponse{}, domain.NewError(domain.CodeConflict, "conversation summary model capacity changed")
		}
	}
	if projectID != "" && !endpoint.Personal && s.projectBudget == nil {
		return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "project model budget is unavailable")
	}
	ledgerCallID := call.CallID
	if call.Source == ModelCallTask && call.Task.RecoveryCallID != "" {
		if !idempotencyKeyPattern.MatchString(call.Task.RecoveryCallID) ||
			!idempotencyKeyPattern.MatchString(call.Task.RecoveryDSHCallID) || call.Task.RecoveryCallID == call.CallID ||
			call.Task.RecoveryDSHCallID != call.CallID {
			return ModelResponse{}, domain.InvalidArgument("model_call", "invalid task recovery call identity")
		}
		ledgerCallID = call.Task.RecoveryCallID
	} else if call.Source == ModelCallTask && call.Task.RecoveryDSHCallID != "" {
		return ModelResponse{}, domain.InvalidArgument("model_call", "incomplete task recovery call identity")
	}
	reservation := ModelCallReservation{
		Source: call.Source, Principal: call.Principal, Task: call.Task, Review: call.Review,
		TerminalDelivery: call.TerminalDelivery, Summary: call.Summary, OwnerID: ownerID,
		ProjectID: projectID, PersonalModel: endpoint.Personal,
		ModelID: call.ModelID, ProviderEndpoint: endpoint.Endpoint.BaseURL, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		BudgetCapabilityVersion: modelBudgetCapabilityVersion(endpoint),
		NeedsTools:              needsTools, NeedsVision: needsVision, CallID: ledgerCallID, DSHCallID: call.CallID,
		BudgetPlanID: call.BudgetPlanID, BudgetCallID: call.BudgetCallID, BudgetRequestDigest: call.BudgetRequestDigest,
		RequestDigest: digest[:], ReservedTokens: reserve,
	}
	if call.BudgetPlanID != "" {
		preparations, ok := s.store.(ModelBudgetPreparationValidator)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "adaptive model budget ledger is unavailable")
		}
		if err := preparations.ValidateTaskModelBudgetPreparation(ctx, reservation); err != nil {
			return ModelResponse{}, err
		}
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
			attachCurrentTaskTurnMessageID(&response, call.Request.CurrentTaskTurnMessageID)
			if err := taskDelivery.ValidateTaskModelDelivery(ctx, reservation); err != nil {
				return ModelResponse{}, err
			}
			if err := validateTaskModelResponse(call.Request, response); err != nil {
				return ModelResponse{}, err
			}
			return response, nil
		}
		if call.ReplayTaskModelResponseOnly {
			return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "task model recovery response is not available")
		}
	}
	if call.Source == ModelCallReview {
		recovery, ok := s.store.(TaskValidationReviewModelResponseStore)
		if !ok {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "Task validation review model recovery is unavailable")
		}
		stored, found, err := recovery.RecoverTaskValidationReviewModelResponse(ctx, reservation)
		if err != nil {
			return ModelResponse{}, err
		}
		if found {
			response, err := OpenTaskValidationReviewModelResponse(s.responseSealer, stored, reservation)
			if err != nil {
				return ModelResponse{}, err
			}
			model, err := taskValidationReviewModelFromResponse(call.ModelID, endpoint.Version, endpoint.Endpoint.BaseURL,
				endpoint.Endpoint.ModelID, response.ProviderReportedModel, call.CallID, stored.Digest)
			if err != nil {
				return ModelResponse{}, err
			}
			response.TaskValidationReview = &model
			return response, nil
		}
	}
	projectCall := projectID != "" && !endpoint.Personal
	if call.BudgetPlanID == "" {
		if err := s.reserveModelCall(ctx, call.Source, projectCall, projectID, reservation, projectUsageReservation(call.Request, reserve)); err != nil {
			return ModelResponse{}, err
		}
	}
	request := call.Request
	currentTaskTurnMessageID := request.CurrentTaskTurnMessageID
	request.CurrentTaskTurnMessageID = ""
	request.Endpoint = endpoint.Endpoint
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
		if call.Source == ModelCallTask && reservation.Task != nil {
			return ModelResponse{}, &TaskModelNoDispatchError{evidence: TaskModelNoDispatchEvidence{
				TaskFence: *reservation.Task, CallID: reservation.DSHCallID, ModelID: reservation.ModelID,
				RequestDigest: append([]byte(nil), reservation.RequestDigest...), UsageKnown: true,
			}, cause: dispatchErr}
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
	if (call.Source == ModelCallTask || call.Source == ModelCallTerminalDelivery) && dispatchErr != nil {
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
	if (call.Source == ModelCallTask || call.Source == ModelCallTerminalDelivery) && providerCtx.Err() != nil {
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
	if call.Source == ModelCallTask {
		attachCurrentTaskTurnMessageID(&response, currentTaskTurnMessageID)
	}
	retryableFailure := false
	var retryableFailureKind ModelClientErrorKind
	if call.Source == ModelCallTask && confirmedTaskModelProviderRejection(callErr, response.Usage) {
		var providerErr *ModelClientError
		if errors.As(callErr, &providerErr) {
			retryableFailure = true
			retryableFailureKind = providerErr.Kind
			response.Usage = ModelUsage{Known: true}
		}
	}
	settlement := ModelCallSettlement{Source: call.Source, OwnerID: ownerID, CallID: reservation.CallID, Usage: response.Usage,
		ProviderReportedModel: response.ProviderReportedModel,
		Unknown:               callErr != nil && !retryableFailure || !response.Usage.Known, Summary: call.Summary}
	if call.Source == ModelCallTerminalDelivery {
		settlement.TerminalDeliveryCall = &reservation
	}
	if response.Usage.Known && (response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || response.Usage.InputTokens > math.MaxInt64-response.Usage.OutputTokens) {
		settlement.Unknown = true
		settlement.Usage = ModelUsage{}
		callErr = &ModelClientError{Kind: ModelErrorInvalidResponse}
	}
	if call.BudgetPlanID != "" && callErr == nil && !settlement.Unknown {
		reservedOutput := int64(request.MaxOutputTokens)
		reservedInput := reservation.ReservedTokens - reservedOutput
		if response.Usage.InputTokens > reservedInput || response.Usage.OutputTokens > reservedOutput {
			overageErr := domain.NewError(domain.CodeQuotaExceeded, "provider usage exceeded the signed model budget; reconciliation is required")
			overageErr.Details["reason"] = "model_usage_exceeded_reservation"
			overageCtx, cancelOverage := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelOverage()
			recorder, ok := s.store.(ModelBudgetOverageRecorder)
			var recordErr error
			if ok {
				recordErr = recorder.RecordTaskModelBudgetOverage(overageCtx, reservation, response.Usage)
			} else {
				recordErr = domain.NewError(domain.CodeUnsupported, "adaptive model overage reconciliation is unavailable")
			}
			if recordErr != nil {
				var freezeErr error
				if projectCall {
					freezeErr = s.projectBudget.SettleModelCall(overageCtx, SettleProjectModelCallCommand{
						ProjectID: projectID, Source: call.Source, CallID: reservation.CallID, Unknown: true,
					})
				} else {
					freezeErr = s.store.SettleModelCall(overageCtx, ModelCallSettlement{
						Source: call.Source, OwnerID: ownerID, CallID: reservation.CallID, Unknown: true,
					})
				}
				if freezeErr != nil {
					freezeErr = fmt.Errorf("freeze adaptive model overage: %w", freezeErr)
				}
				return ModelResponse{}, errors.Join(overageErr, fmt.Errorf("persist adaptive model overage: %w", recordErr), freezeErr)
			}
			s.observeModelUsage(ModelCallSettlement{Source: call.Source, Usage: response.Usage})
			return ModelResponse{}, overageErr
		}
	}
	var summaryOutputErr error
	var reviewOutputErr error
	var terminalDeliveryOutputErr error
	if call.Source == ModelCallCompaction && !settlement.Unknown {
		if failure := ValidateConversationSummaryModelResponse(response); failure != nil {
			settlement.ResponseInvalid = true
			summaryOutputErr = domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid conversation summary: "+failure.Error())
		} else {
			settlement.ResponseCiphertext, settlement.ResponseDigest, err = SealConversationSummaryModelOutput(s.responseSealer,
				ConversationSummaryModelOutput{Identity: call.Summary.Identity, OperationID: call.Summary.OperationID,
					CallID: call.CallID, ModelID: call.ModelID, Text: response.Text, Usage: response.Usage})
			if err != nil {
				settlement.ResponseInvalid = true
				summaryOutputErr = domain.NewError(domain.CodeInvalidState, "conversation summary model result could not be stored safely: "+err.Error())
			}
		}
	}
	if call.Source == ModelCallReview && !settlement.Unknown {
		if !validTaskValidationReviewModelResponse(response) {
			settlement.ResponseInvalid = true
			reviewOutputErr = domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid Task validation review response")
		} else {
			stored, sealErr := SealTaskValidationReviewModelResponse(s.responseSealer, reservation, response)
			if sealErr != nil {
				settlement.ResponseInvalid = true
				reviewOutputErr = domain.NewError(domain.CodeInvalidState, "Task validation review model result could not be stored safely")
			} else {
				settlement.ReviewCall = &reservation
				settlement.ResponseCiphertext, settlement.ResponseDigest = stored.Ciphertext, stored.Digest
				settlement.ResponseTextDigest = stored.TextDigest
			}
		}
	}
	if call.Source == ModelCallTerminalDelivery && !settlement.Unknown {
		if !validTaskTerminalDeliveryModelResponse(response) {
			settlement.ResponseInvalid = true
			terminalDeliveryOutputErr = domain.NewError(domain.CodeUpstreamError, "model provider returned an invalid terminal delivery response")
		} else {
			ciphertext, sealErr := sealTaskTerminalDeliveryModelResponse(s.responseSealer, *call.TerminalDelivery,
				call.ModelID, reservation.RequestDigest, response)
			if sealErr != nil {
				settlement.ResponseInvalid = true
				terminalDeliveryOutputErr = domain.NewError(domain.CodeInvalidState, "terminal delivery model result could not be stored safely")
			} else {
				cipherDigest := sha256.Sum256(ciphertext)
				textDigest := sha256.Sum256([]byte(response.Text))
				settlement.ResponseCiphertext = ciphertext
				settlement.ResponseDigest = append([]byte(nil), cipherDigest[:]...)
				settlement.ResponseTextDigest = append([]byte(nil), textDigest[:]...)
			}
		}
	}
	var taskOutputErr error
	if call.Source == ModelCallTask && !settlement.Unknown && !retryableFailure {
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
			ProjectID: projectID, Source: call.Source, CallID: reservation.CallID,
			Usage: project.Usage{InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}, Unknown: settlement.Unknown,
			ProviderReportedModel: settlement.ProviderReportedModel,
			Summary:               call.Summary, TaskCall: settlement.TaskCall, ResponseCiphertext: settlement.ResponseCiphertext,
			ReviewCall: settlement.ReviewCall, ResponseDigest: settlement.ResponseDigest, ResponseInvalid: settlement.ResponseInvalid,
			TerminalDeliveryCall: settlement.TerminalDeliveryCall, ResponseTextDigest: settlement.ResponseTextDigest,
		})
	} else {
		settleErr = s.store.SettleModelCall(settleCtx, settlement)
	}
	if settleErr != nil {
		if (call.Source == ModelCallReview || call.Source == ModelCallTerminalDelivery) && projectCall && !settlement.Unknown {
			unknownCtx, cancelUnknown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelUnknown()
			unknownErr := s.projectBudget.SettleModelCall(unknownCtx, SettleProjectModelCallCommand{
				ProjectID: projectID, Source: call.Source, CallID: call.CallID, Unknown: true,
				TerminalDeliveryCall: settlement.TerminalDeliveryCall,
			})
			if unknownErr != nil {
				return ModelResponse{}, errors.Join(fmt.Errorf("settle project review model call: %w", settleErr),
					fmt.Errorf("preserve unknown project review model usage: %w", unknownErr))
			}
			s.observeModelUsage(ModelCallSettlement{Source: call.Source, Unknown: true})
			return ModelResponse{}, fmt.Errorf("settle project review model call: %w", settleErr)
		}
		if (call.Source == ModelCallTask && !settlement.Unknown ||
			call.Source == ModelCallCompaction || call.Source == ModelCallReview || call.Source == ModelCallTerminalDelivery) && !projectCall {
			unknownCtx, cancelUnknown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelUnknown()
			unknownErr := s.store.SettleModelCall(unknownCtx, ModelCallSettlement{Source: call.Source, OwnerID: ownerID,
				CallID: reservation.CallID, Unknown: true, Summary: call.Summary,
				TerminalDeliveryCall: settlement.TerminalDeliveryCall})
			if unknownErr != nil {
				return ModelResponse{}, errors.Join(fmt.Errorf("settle model call: %w", settleErr),
					fmt.Errorf("preserve unknown model usage: %w", unknownErr))
			}
			s.observeModelUsage(ModelCallSettlement{Source: call.Source, Unknown: true})
		}
		return ModelResponse{}, fmt.Errorf("settle model call: %w", settleErr)
	}
	s.observeModelUsage(settlement)
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
		if retryableFailure && reservation.Task != nil {
			return ModelResponse{}, &TaskModelRetryableFailureError{evidence: TaskModelRecoveryFailureEvidence{
				Kind: TaskModelRecoveryFailureProviderReject, TaskFence: *reservation.Task, CallID: reservation.DSHCallID,
				ModelID: reservation.ModelID, RequestDigest: append([]byte(nil), reservation.RequestDigest...), UsageKnown: true,
				ProviderErrorKind: retryableFailureKind}, cause: callErr}
		}
		return ModelResponse{}, modelConnectionCheckError(callErr)
	}
	if summaryOutputErr != nil {
		return ModelResponse{}, summaryOutputErr
	}
	if reviewOutputErr != nil {
		return ModelResponse{}, reviewOutputErr
	}
	if taskOutputErr != nil {
		return ModelResponse{}, taskOutputErr
	}
	if terminalDeliveryOutputErr != nil {
		return ModelResponse{}, terminalDeliveryOutputErr
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
	if call.Source == ModelCallReview {
		model, err := taskValidationReviewModelFromResponse(call.ModelID, endpoint.Version, endpoint.Endpoint.BaseURL,
			endpoint.Endpoint.ModelID, response.ProviderReportedModel, call.CallID, settlement.ResponseDigest)
		if err != nil {
			return ModelResponse{}, err
		}
		response.TaskValidationReview = &model
	}
	return response, nil
}

func attachCurrentTaskTurnMessageID(response *ModelResponse, messageID string) {
	if response == nil {
		return
	}
	if !domain.ValidMessageID(messageID) {
		messageID = ""
	}
	for index := range response.ToolCalls {
		response.ToolCalls[index].CurrentTaskTurnMessageID = messageID
	}
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
	adaptiveBudget := call.BudgetPlanID != "" || call.BudgetCallID != "" || call.BudgetRequestDigest != ""
	if adaptiveBudget && (call.Source != ModelCallTask || !idempotencyKeyPattern.MatchString(call.BudgetPlanID) ||
		!idempotencyKeyPattern.MatchString(call.BudgetCallID) || !modelBudgetDigestPattern.MatchString(call.BudgetRequestDigest)) {
		return "", empty, 0, domain.InvalidArgument("model_call", "invalid adaptive model budget identity")
	}
	ownerID := call.Principal.UserID
	switch call.Source {
	case ModelCallClient, ModelCallChat, ModelCallProbe:
		if call.Task != nil || call.Review != nil || call.Summary != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid user model call")
		}
		if err := validateClientAccountPrincipal(call.Principal); err != nil {
			return "", empty, 0, err
		}
	case ModelCallTask:
		if call.Task == nil || call.Review != nil || call.Summary != nil || !domain.ValidUserID(call.Task.OwnerID) || !domain.ValidUserID(call.Task.InitiatorID) || call.Task.LeaseOwner == "" || call.Task.LeaseVersion < 1 ||
			call.Task.AgentInstanceID == "" || call.Task.AgentGeneration < 1 || call.Task.AuthorizationVersion < 1 ||
			!dshModelIdentityPattern.MatchString(call.Task.AgentRootSessionID) ||
			invalidCheckpointLabel(call.Task.EngineVersion, 128) || invalidCheckpointLabel(call.Task.ProfileVersion, 128) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid task execution fence")
		}
		id, err := DSHModelCallID(call.Task.TaskID, call.Task.AgentSessionID, call.Task.ThroughSequence)
		if err != nil || len(call.RawRequest) == 0 || !json.Valid(call.RawRequest) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid task model call identity or request")
		}
		if call.Task.RecoveryCallID == "" && call.Task.RecoveryDSHCallID == "" {
			if id != call.CallID || call.Task.RecoveryDSHCallID != "" {
				return "", empty, 0, domain.InvalidArgument("model_call", "invalid task model call identity")
			}
		} else {
			recoveryDSHCallID, dshErr := TaskModelRecoveryDSHCallID(call.Task.TaskID, call.Task.TaskRevision, id, call.Task.ThroughSequence)
			recoveryLedgerCallID, ledgerErr := TaskModelRecoveryCallID(call.Task.TaskID, call.Task.TaskRevision, id, call.Task.ThroughSequence)
			if dshErr != nil || ledgerErr != nil || call.Task.RecoveryDSHCallID != recoveryDSHCallID ||
				call.Task.RecoveryCallID != recoveryLedgerCallID || call.CallID != recoveryDSHCallID ||
				call.Task.RecoveryDSHCallID == call.Task.RecoveryCallID || call.Task.RecoveryCallID == id {
				return "", empty, 0, domain.InvalidArgument("model_call", "invalid task recovery call identity")
			}
		}
		ownerID = call.Task.InitiatorID
	case ModelCallReview:
		if call.Review == nil || call.Task != nil || call.Summary != nil || call.Principal.UserID != "" ||
			call.Principal.TokenID != "" || call.Principal.ClientInstallationID != "" || len(call.Request.Tools) != 0 ||
			call.Request.RequiredTool != "" || len(call.RawRequest) == 0 || !json.Valid(call.RawRequest) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid Task validation review model fence")
		}
		if err := func() error {
			for _, input := range call.Request.Input {
				if input.Type != "message" || len(input.Images) != 0 || input.CallID != "" || input.Name != "" || input.Arguments != "" || input.Output != "" {
					return domain.InvalidArgument("model_call", "Task validation review model call must be text-only")
				}
				for _, part := range input.Parts {
					if part.Type == "image" || part.Image != nil {
						return domain.InvalidArgument("model_call", "Task validation review model call must be text-only")
					}
				}
			}
			return nil
		}(); err != nil {
			return "", empty, 0, err
		}
		id, err := DSHTaskValidationReviewModelCallID(*call.Review)
		if err != nil || id != call.CallID {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid Task validation review model call identity")
		}
		ownerID = call.Review.InitiatorID
	case ModelCallCompaction:
		if call.Task != nil || call.Review != nil || call.Summary == nil || ownerID != call.Summary.Claim.Message.ActorID ||
			call.Summary.Identity.ActorID != ownerID || call.Summary.BaselineVersion < 0 ||
			call.Summary.ContextWindowTokens < 4096 || call.Summary.ContextWindowTokens > MaxModelContextWindowTokens ||
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
	case ModelCallTerminalDelivery:
		if call.TerminalDelivery == nil || call.Task != nil || call.Review != nil || call.Summary != nil ||
			call.Principal.UserID != "" || call.Principal.TokenID != "" || call.Principal.ClientInstallationID != "" ||
			call.Request.RequiredTool != "" || len(call.Request.Tools) != 0 || len(call.RawRequest) == 0 || !json.Valid(call.RawRequest) ||
			!ValidTaskTerminalDeliveryModelFence(*call.TerminalDelivery) {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid terminal delivery model fence")
		}
		for _, input := range call.Request.Input {
			if input.Type != "message" || len(input.Images) != 0 || input.CallID != "" || input.Name != "" || input.Arguments != "" || input.Output != "" {
				return "", empty, 0, domain.InvalidArgument("model_call", "terminal delivery model call must be text-only")
			}
			for _, part := range input.Parts {
				if part.Type == "image" || part.Image != nil {
					return "", empty, 0, domain.InvalidArgument("model_call", "terminal delivery model call must be text-only")
				}
			}
		}
		id, err := DSHTerminalDeliveryModelCallID(*call.TerminalDelivery)
		if err != nil || id != call.CallID {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid terminal delivery model call identity")
		}
		ownerID = call.TerminalDelivery.InitiatorID
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
			ReasoningEffort string `json:",omitempty"`
			DisableThinking bool   `json:",omitempty"`
		}{call.ModelID, call.Request.Instructions, call.Request.Input, call.Request.Tools, call.Request.RequiredTool, call.Request.MaxOutputTokens, call.Request.Temperature, call.Request.ReasoningEffort, call.Request.DisableThinking})
		if err != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "model request cannot be encoded")
		}
	} else if call.Request.ReasoningEffort != "" {
		var err error
		raw, err = json.Marshal(struct {
			RawRequest      json.RawMessage
			ModelID         string
			MaxOutputTokens int
			ReasoningEffort string
		}{raw, call.ModelID, call.Request.MaxOutputTokens, call.Request.ReasoningEffort})
		if err != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "model request cannot be encoded")
		}
	}
	digest := sha256.Sum256(raw)
	if adaptiveBudget {
		boundDigest, err := ModelBudgetFinalRequestDigest(call.BudgetRequestDigest, int64(call.Request.MaxOutputTokens))
		if err != nil {
			return "", empty, 0, err
		}
		copy(digest[:], boundDigest)
	}
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
