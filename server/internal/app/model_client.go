package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ModelCallTimeout bounds both dispatch authorization and the provider request.
const ModelCallTimeout = 45 * time.Second

type ModelEndpoint struct {
	BaseURL string
	APIKey  string
	ModelID string
}

type ModelInput struct {
	Type      string
	Role      string
	Text      string
	Images    []ModelImage
	Parts     []ModelContentPart
	CallID    string
	Name      string
	Arguments string
	Output    string
}

type ModelContentPart struct {
	Type  string
	Text  string
	Image *ModelImage
}

type ModelImage struct {
	MediaType string
	Data      []byte
}

type ModelTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      bool
}

type ModelRequest struct {
	Endpoint     ModelEndpoint
	Instructions string
	Input        []ModelInput
	Tools        []ModelTool
	// CurrentTaskTurnMessageID is trusted DSH request metadata. It is never sent to a model provider.
	CurrentTaskTurnMessageID string `json:"-"`
	RequiredTool             string
	MaxOutputTokens          int
	Temperature              *float64
	ReasoningEffort          string
	DisableThinking          bool
	JSONMode                 bool
	OnTextDelta              func(string) error `json:"-"`
}

type ModelToolCall struct {
	CallID    string
	Name      string
	Arguments string
	// CurrentTaskTurnMessageID is attached by the Task ModelGateway and consumed by the tool broker.
	CurrentTaskTurnMessageID string `json:"-"`
	// TaskFence is attached at broker dispatch; its checkpoint head may advance past the model request.
	TaskFence *ModelTaskFence `json:"-"`
}

type ModelUsage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
}

const MaxProviderReportedModelBytes = 256

func ValidProviderReportedModel(value string) bool {
	if value == "" {
		return true
	}
	if !utf8.ValidString(value) || len(value) > MaxProviderReportedModelBytes || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

type ModelResponse struct {
	Reasoning             string `json:"-"`
	Text                  string
	ToolCalls             []ModelToolCall
	Usage                 ModelUsage
	StopReason            string
	ProviderReportedModel string                     `json:"-"`
	TaskValidationReview  *TaskValidationReviewModel `json:"-"`
}

type ModelCapabilities struct {
	Text            bool
	TextStream      bool
	Tools           bool
	Vision          bool
	MediaGeneration ModelMediaGenerationCapabilities
	ModelID         string
}

type ModelMediaGenerationCapabilities struct {
	ImageGeneration  bool
	VideoGeneration  bool
	SpeechGeneration bool
}

type ModelMediaGenerationCapabilityReport struct {
	Supported                bool
	ExactTokenReservation    bool
	ActualTokenUsage         bool
	IdempotentOperation      bool
	DurableOperationRecovery bool
	ConfirmedCancellation    bool
}

// ModelMediaGenerationCapabilityClient must inspect provider metadata only; it must not dispatch a generation.
type ModelMediaGenerationCapabilityClient interface {
	CheckMediaGenerationCapability(context.Context, ModelEndpoint, string) (ModelMediaGenerationCapabilityReport, error)
}

type ModelClient interface {
	Complete(context.Context, ModelRequest) (ModelResponse, error)
	Check(context.Context, ModelEndpoint) (ModelCapabilities, error)
}

type ModelClientErrorKind string

const (
	ModelErrorAuthentication    ModelClientErrorKind = "authentication_rejected"
	ModelErrorAccess            ModelClientErrorKind = "access_rejected"
	ModelErrorRateLimited       ModelClientErrorKind = "rate_limited"
	ModelErrorQuota             ModelClientErrorKind = "quota_exhausted"
	ModelErrorInvalidRequest    ModelClientErrorKind = "invalid_request"
	ModelErrorUnavailable       ModelClientErrorKind = "unavailable"
	ModelErrorTimeout           ModelClientErrorKind = "timeout"
	ModelErrorTransport         ModelClientErrorKind = "transport"
	ModelErrorInvalidResponse   ModelClientErrorKind = "invalid_response"
	ModelErrorEmptyResponse     ModelClientErrorKind = "empty_response"
	ModelErrorTruncatedResponse ModelClientErrorKind = "truncated_response"
)

type ModelClientError struct {
	Kind         ModelClientErrorKind
	Stage        string
	ProviderCode string
	// ConfirmedNoUsage is set only when an explicit provider status rejects
	// the request before model execution (currently HTTP 429 or 503).
	ConfirmedNoUsage                bool
	RetryAfter                      time.Duration
	RetryAfterExceedsDurableHorizon bool
}

func (e *ModelClientError) Error() string {
	return "model provider request failed: " + string(e.Kind)
}
