package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ModelCallSource string

const (
	ModelCallClient ModelCallSource = "client"
	ModelCallChat   ModelCallSource = "chat"
	ModelCallTask   ModelCallSource = "task"
	ModelCallProbe  ModelCallSource = "probe"
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
	ProjectID         string
	OwnerID           string
	ModelID           string
	ProviderModel     string
	ConnectionVersion time.Time
	NeedsTools        bool
	NeedsVision       bool
	CallID            string
	RequestDigest     []byte
	ReservedTokens    int64
}

type ModelCallSettlement struct {
	Source  ModelCallSource
	OwnerID string
	CallID  string
	Usage   ModelUsage
	Unknown bool
}

type ModelCallStore interface {
	ReserveModelCall(context.Context, ModelCallReservation) error
	BeginModelCallDispatch(context.Context, ModelCallReservation) error
	SettleModelCall(context.Context, ModelCallSettlement) error
}

type ModelGatewayResolver interface {
	ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (ModelGatewayEndpoint, error)
	ResolveProbeEndpoint(context.Context, identity.Principal, string) (ModelGatewayEndpoint, error)
	SaveProbeCapabilities(context.Context, identity.Principal, string, time.Time, ModelCapabilities) (ModelCapabilities, error)
}

type ModelGatewayService struct {
	store         ModelCallStore
	models        ModelGatewayResolver
	client        ModelClient
	projectBudget *ProjectBudgetService
}

type ModelGatewayEndpoint struct {
	Endpoint ModelEndpoint
	Version  time.Time
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

var dshModelIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func DSHModelCallID(taskID, sessionID string, throughSequence int64) (string, error) {
	if !dshModelIdentityPattern.MatchString(taskID) || !dshModelIdentityPattern.MatchString(sessionID) || throughSequence < 0 {
		return "", domain.InvalidArgument("model_call", "invalid task model call identity")
	}
	digest := sha256.Sum256([]byte("morde-dsh-model-v1\n" + taskID + "\n" + sessionID + "\n" + strconv.FormatInt(throughSequence, 10)))
	return "mdl_" + hex.EncodeToString(digest[:]), nil
}

// Complete only performs model inference. Tool calls are returned to the caller.
func (s *ModelGatewayService) Complete(ctx context.Context, call ModelCall) (ModelResponse, error) {
	if ctx == nil {
		return ModelResponse{}, domain.InvalidArgument("model_call", "model call context is required")
	}
	if call.Source == ModelCallTask {
		if call.Task == nil || call.Request.Endpoint != (ModelEndpoint{}) {
			return ModelResponse{}, domain.InvalidArgument("model_call", "invalid task model call")
		}
		request, err := parseDSHModelRequest(ctx, *call.Task, call.ModelID, call.RawRequest, call.ImageResolver)
		if err != nil {
			return ModelResponse{}, err
		}
		request.OnTextDelta = call.Request.OnTextDelta
		call.Request = request
	}
	ownerID, digest, reserve, err := validateModelCall(call)
	if err != nil {
		return ModelResponse{}, err
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
		if projectID != "" && s.projectBudget == nil {
			return ModelResponse{}, domain.NewError(domain.CodeUnsupported, "project model budget is unavailable")
		}
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
	reservation := ModelCallReservation{
		Source: call.Source, Principal: call.Principal, Task: call.Task, OwnerID: ownerID,
		ProjectID: projectID,
		ModelID:   call.ModelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		NeedsTools: needsTools, NeedsVision: needsVision, CallID: call.CallID, RequestDigest: digest[:], ReservedTokens: reserve,
	}
	projectCall := projectID != ""
	if projectCall {
		usage := projectUsageReservation(call.Request, reserve)
		if _, _, err := s.projectBudget.reserveTaskModelCall(ctx, projectID, reservation, usage); err != nil {
			return ModelResponse{}, err
		}
	} else if err := s.store.ReserveModelCall(ctx, reservation); err != nil {
		return ModelResponse{}, err
	}
	request := call.Request
	request.Endpoint = endpoint.Endpoint
	dispatchCtx, cancelDispatch := context.WithTimeout(ctx, ModelCallTimeout)
	defer cancelDispatch()
	if err := s.store.BeginModelCallDispatch(dispatchCtx, reservation); err != nil {
		return ModelResponse{}, err
	}
	response, callErr := s.client.Complete(dispatchCtx, request)
	settlement := ModelCallSettlement{Source: call.Source, OwnerID: ownerID, CallID: call.CallID, Usage: response.Usage, Unknown: callErr != nil || !response.Usage.Known}
	if response.Usage.Known && (response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || response.Usage.InputTokens > math.MaxInt64-response.Usage.OutputTokens) {
		settlement.Unknown = true
		settlement.Usage = ModelUsage{}
		callErr = &ModelClientError{Kind: ModelErrorInvalidResponse}
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var settleErr error
	if projectCall {
		settleErr = s.projectBudget.SettleModelCall(settleCtx, SettleProjectModelCallCommand{
			ProjectID: projectID, Source: call.Source, CallID: call.CallID,
			Usage: project.Usage{InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}, Unknown: settlement.Unknown,
		})
	} else {
		settleErr = s.store.SettleModelCall(settleCtx, settlement)
	}
	if settleErr != nil {
		return ModelResponse{}, fmt.Errorf("settle model call: %w", settleErr)
	}
	if callErr != nil {
		return ModelResponse{}, modelConnectionCheckError(callErr)
	}
	if !response.Usage.Known {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "model usage is unknown and requires reconciliation")
	}
	if response.Usage.InputTokens+response.Usage.OutputTokens > reserve {
		return ModelResponse{}, domain.NewError(domain.CodeQuotaExceeded, "model provider usage exceeded the reserved budget")
	}
	return response, nil
}

func projectUsageReservation(request ModelRequest, reservedTokens int64) project.Usage {
	output := int64(request.MaxOutputTokens)
	return project.Usage{InputTokens: reservedTokens - output, OutputTokens: output}
}

func validateModelCall(call ModelCall) (string, [32]byte, int64, error) {
	var empty [32]byte
	if !domain.ValidModelConnectionID(call.ModelID) || !idempotencyKeyPattern.MatchString(call.CallID) || call.Request.Endpoint != (ModelEndpoint{}) {
		return "", empty, 0, domain.InvalidArgument("model_call", "invalid model call")
	}
	ownerID := call.Principal.UserID
	switch call.Source {
	case ModelCallClient, ModelCallChat, ModelCallProbe:
		if call.Task != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "invalid user model call")
		}
		if err := validateClientAccountPrincipal(call.Principal); err != nil {
			return "", empty, 0, err
		}
		if call.Source == ModelCallProbe && call.Principal.Kind != identity.KindSession {
			return "", empty, 0, domain.NewError(domain.CodeForbidden, "portal session required to check model")
		}
	case ModelCallTask:
		if call.Task != nil && call.Task.AgentRootSessionID == "" {
			call.Task.AgentRootSessionID = call.Task.AgentSessionID
		}
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
	default:
		return "", empty, 0, domain.InvalidArgument("model_call", "invalid model call source")
	}
	if call.Request.MaxOutputTokens < 1 || call.Request.MaxOutputTokens > 32768 || len(call.Request.Instructions) > maxAgentInputBytes {
		return "", empty, 0, domain.InvalidArgument("model_call", "model token or input limit is invalid")
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
	reserve := int64(len(call.Request.Instructions) + call.Request.MaxOutputTokens)
	for _, input := range call.Request.Input {
		reserve += int64(len(input.Text) + len(input.Arguments) + len(input.Output))
		reserve += int64(len(input.Images)) * agentImageTokenReserve
		for _, part := range input.Parts {
			reserve += int64(len(part.Text))
			if part.Type == "image" {
				reserve += agentImageTokenReserve
			}
		}
	}
	for _, tool := range call.Request.Tools {
		reserve += int64(len(tool.Name) + len(tool.Description) + len(tool.Parameters))
	}
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
		}{call.ModelID, call.Request.Instructions, call.Request.Input, call.Request.Tools, call.Request.RequiredTool, call.Request.MaxOutputTokens, call.Request.Temperature})
		if err != nil {
			return "", empty, 0, domain.InvalidArgument("model_call", "model request cannot be encoded")
		}
	}
	digest := sha256.Sum256(raw)
	return ownerID, digest, reserve, nil
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
