package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
)

// Model failure ladder (E12): a 31-value error classification table, a
// jittered backoff ladder, and the retry → switch method → switch agent →
// terminal progression. The table is Morde's own taxonomy, modeled on the
// failure classes a model provider and a tool runtime produce (the same shape
// as hermes' error_classifier.py FailoverReason table, not a copy of it).

// FailoverReason is one of the 31 classified model/tool failure causes.
type FailoverReason string

const (
	FailoverReasonRateLimited            FailoverReason = "rate_limited"
	FailoverReasonQuotaExceeded          FailoverReason = "quota_exceeded"
	FailoverReasonTimeout                FailoverReason = "timeout"
	FailoverReasonConnectionReset        FailoverReason = "connection_reset"
	FailoverReasonConnectionRefused      FailoverReason = "connection_refused"
	FailoverReasonDNSFailure             FailoverReason = "dns_failure"
	FailoverReasonTLSFailure             FailoverReason = "tls_failure"
	FailoverReasonServerError            FailoverReason = "server_error"
	FailoverReasonBadGateway             FailoverReason = "bad_gateway"
	FailoverReasonServiceUnavailable     FailoverReason = "service_unavailable"
	FailoverReasonGatewayTimeout         FailoverReason = "gateway_timeout"
	FailoverReasonInvalidResponse        FailoverReason = "invalid_response"
	FailoverReasonResponseShapeInvalid   FailoverReason = "response_shape_invalid"
	FailoverReasonEmptyResponse          FailoverReason = "empty_response"
	FailoverReasonTruncatedResponse      FailoverReason = "truncated_response"
	FailoverReasonContextLengthExceeded  FailoverReason = "context_length_exceeded"
	FailoverReasonContentFiltered        FailoverReason = "content_filtered"
	FailoverReasonAuthenticationRejected FailoverReason = "authentication_rejected"
	FailoverReasonAccessRejected         FailoverReason = "access_rejected"
	FailoverReasonInvalidRequest         FailoverReason = "invalid_request"
	FailoverReasonModelNotFound          FailoverReason = "model_not_found"
	FailoverReasonUnsupportedParameter   FailoverReason = "unsupported_parameter"
	FailoverReasonToolCallInvalid        FailoverReason = "tool_call_invalid"
	FailoverReasonToolTimeout            FailoverReason = "tool_timeout"
	FailoverReasonToolFailed             FailoverReason = "tool_failed"
	FailoverReasonBudgetExceeded         FailoverReason = "budget_exceeded"
	FailoverReasonCancelled              FailoverReason = "cancelled"
	FailoverReasonStreamInterrupted      FailoverReason = "stream_interrupted"
	FailoverReasonUsageUnknown           FailoverReason = "usage_unknown"
	FailoverReasonUpstreamError          FailoverReason = "upstream_error"
	FailoverReasonUnknown                FailoverReason = "unknown"
)

// AllFailoverReasons lists every classified reason, in table order.
func AllFailoverReasons() []FailoverReason {
	return []FailoverReason{
		FailoverReasonRateLimited, FailoverReasonQuotaExceeded, FailoverReasonTimeout,
		FailoverReasonConnectionReset, FailoverReasonConnectionRefused, FailoverReasonDNSFailure,
		FailoverReasonTLSFailure, FailoverReasonServerError, FailoverReasonBadGateway,
		FailoverReasonServiceUnavailable, FailoverReasonGatewayTimeout, FailoverReasonInvalidResponse,
		FailoverReasonResponseShapeInvalid, FailoverReasonEmptyResponse, FailoverReasonTruncatedResponse,
		FailoverReasonContextLengthExceeded, FailoverReasonContentFiltered,
		FailoverReasonAuthenticationRejected, FailoverReasonAccessRejected, FailoverReasonInvalidRequest,
		FailoverReasonModelNotFound, FailoverReasonUnsupportedParameter, FailoverReasonToolCallInvalid,
		FailoverReasonToolTimeout, FailoverReasonToolFailed, FailoverReasonBudgetExceeded,
		FailoverReasonCancelled, FailoverReasonStreamInterrupted, FailoverReasonUsageUnknown,
		FailoverReasonUpstreamError, FailoverReasonUnknown,
	}
}

// FailoverAction is the ladder step taken for a failure.
type FailoverAction string

const (
	// FailoverActionRetry retries the same call after a backoff delay.
	FailoverActionRetry FailoverAction = "retry"
	// FailoverActionSwitchMethod changes the approach (another model call
	// shape, a heuristic fallback, a different tool) and tries again.
	FailoverActionSwitchMethod FailoverAction = "switch_method"
	// FailoverActionSwitchAgent hands the task to a different agent.
	FailoverActionSwitchAgent FailoverAction = "switch_agent"
	// FailoverActionTerminal stops: the ladder is spent or the failure cannot
	// be retried, and the task enters the terminal告知 state.
	FailoverActionTerminal FailoverAction = "terminal"
)

// FailoverStrategy is the ladder entry for one reason: whether the same call
// may be retried, how many retries it gets, and what happens after the
// retries are spent.
type FailoverStrategy struct {
	Retryable       bool
	MaxAttempts     int
	AfterExhaustion FailoverAction
}

const (
	// ModelRetryMaxRetries is the number of same-call retries the ladder
	// allows before moving on (E12 acceptance: network error retries 3 times).
	ModelRetryMaxRetries = 3
	// ModelRetryBaseDelay is the first backoff step; ModelRetryMaxDelay caps
	// the computed backoff (a provider Retry-After still wins, see
	// DecideModelRetry).
	ModelRetryBaseDelay = 5 * time.Second
	ModelRetryMaxDelay  = 120 * time.Second
	// ModelRetryMaxAgentDepth is the default sub-agent depth for a handover
	// (待拍板: 1 层还是 2 层; default 1, configurable via
	// ApplyTaskModelFailureLadder).
	ModelRetryMaxAgentDepth = 1
)

func failoverStrategyTable() map[FailoverReason]FailoverStrategy {
	retry := func(maxAttempts int, after FailoverAction) FailoverStrategy {
		return FailoverStrategy{Retryable: true, MaxAttempts: maxAttempts, AfterExhaustion: after}
	}
	terminal := FailoverStrategy{Retryable: false, AfterExhaustion: FailoverActionTerminal}
	return map[FailoverReason]FailoverStrategy{
		FailoverReasonRateLimited:            retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonTimeout:                retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonConnectionReset:        retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonConnectionRefused:      retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonDNSFailure:             retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonTLSFailure:             retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonServerError:            retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonBadGateway:             retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonServiceUnavailable:     retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonGatewayTimeout:         retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonStreamInterrupted:      retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonUpstreamError:          retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonInvalidResponse:        retry(2, FailoverActionSwitchMethod),
		FailoverReasonResponseShapeInvalid:   retry(2, FailoverActionSwitchMethod),
		FailoverReasonEmptyResponse:          retry(2, FailoverActionSwitchMethod),
		FailoverReasonTruncatedResponse:      retry(2, FailoverActionSwitchMethod),
		FailoverReasonToolTimeout:            retry(ModelRetryMaxRetries, FailoverActionSwitchMethod),
		FailoverReasonToolFailed:             retry(1, FailoverActionSwitchAgent),
		FailoverReasonUsageUnknown:           retry(1, FailoverActionSwitchMethod),
		FailoverReasonUnknown:                retry(1, FailoverActionSwitchMethod),
		FailoverReasonQuotaExceeded:          terminal,
		FailoverReasonContextLengthExceeded:  terminal,
		FailoverReasonContentFiltered:        terminal,
		FailoverReasonAuthenticationRejected: terminal,
		FailoverReasonAccessRejected:         terminal,
		FailoverReasonInvalidRequest:         terminal,
		FailoverReasonModelNotFound:          terminal,
		FailoverReasonUnsupportedParameter:   terminal,
		FailoverReasonToolCallInvalid:        terminal,
		FailoverReasonBudgetExceeded:         terminal,
		FailoverReasonCancelled:              terminal,
	}
}

// FailoverStrategyFor returns the ladder entry for a reason. An unlisted
// reason falls back to one retry and then a method switch, never to a silent
// pass.
func FailoverStrategyFor(reason FailoverReason) FailoverStrategy {
	if strategy, ok := failoverStrategyTable()[reason]; ok {
		return strategy
	}
	return FailoverStrategy{Retryable: true, MaxAttempts: 1, AfterExhaustion: FailoverActionSwitchMethod}
}

// ClassifyModelError maps an error to its failover reason. It reads, in
// order: the model client error kind, the domain error code and its provider
// reason detail, then plain network-error text. Anything else is unknown.
func ClassifyModelError(err error) FailoverReason {
	if err == nil {
		return FailoverReasonUnknown
	}
	var clientErr *ModelClientError
	if errors.As(err, &clientErr) {
		switch clientErr.Kind {
		case ModelErrorRateLimited:
			return FailoverReasonRateLimited
		case ModelErrorQuota:
			return FailoverReasonQuotaExceeded
		case ModelErrorTimeout:
			return FailoverReasonTimeout
		case ModelErrorTransport:
			return FailoverReasonUpstreamError
		case ModelErrorInvalidResponse:
			return FailoverReasonInvalidResponse
		case ModelErrorEmptyResponse:
			return FailoverReasonEmptyResponse
		case ModelErrorTruncatedResponse:
			return FailoverReasonTruncatedResponse
		case ModelErrorAuthentication:
			return FailoverReasonAuthenticationRejected
		case ModelErrorAccess:
			return FailoverReasonAccessRejected
		case ModelErrorInvalidRequest:
			return FailoverReasonInvalidRequest
		case ModelErrorUnavailable:
			return FailoverReasonServiceUnavailable
		}
	}
	var routeErr *domain.Error
	if errors.As(err, &routeErr) {
		if reason, ok := routeErr.Details["reason"].(string); ok {
			if classified := classifyProviderReason(reason); classified != FailoverReasonUnknown {
				return classified
			}
		}
		switch routeErr.Code {
		case domain.CodeRateLimited:
			return FailoverReasonRateLimited
		case domain.CodeQuotaExceeded:
			return FailoverReasonQuotaExceeded
		case domain.CodeUpstreamTimeout:
			return FailoverReasonTimeout
		case domain.CodeUpstreamError:
			return FailoverReasonUpstreamError
		case domain.CodeUnauthenticated:
			return FailoverReasonAuthenticationRejected
		case domain.CodeForbidden:
			return FailoverReasonAccessRejected
		case domain.CodeNotFound:
			return FailoverReasonModelNotFound
		case domain.CodeResultTooLarge:
			return FailoverReasonContextLengthExceeded
		}
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "connection refused"):
		return FailoverReasonConnectionRefused
	case strings.Contains(message, "connection reset"):
		return FailoverReasonConnectionReset
	case strings.Contains(message, "no such host"), strings.Contains(message, "dns"):
		return FailoverReasonDNSFailure
	case strings.Contains(message, "tls"), strings.Contains(message, "certificate"):
		return FailoverReasonTLSFailure
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline exceeded"):
		return FailoverReasonTimeout
	case strings.Contains(message, "429"), strings.Contains(message, "too many requests"):
		return FailoverReasonRateLimited
	case strings.Contains(message, "503"), strings.Contains(message, "service unavailable"):
		return FailoverReasonServiceUnavailable
	case strings.Contains(message, "502"), strings.Contains(message, "bad gateway"):
		return FailoverReasonBadGateway
	case strings.Contains(message, "504"), strings.Contains(message, "gateway timeout"):
		return FailoverReasonGatewayTimeout
	case strings.Contains(message, "500"), strings.Contains(message, "internal server error"):
		return FailoverReasonServerError
	case strings.Contains(message, "context canceled"), strings.Contains(message, "canceled"):
		return FailoverReasonCancelled
	}
	return FailoverReasonUnknown
}

func classifyProviderReason(reason string) FailoverReason {
	switch reason {
	case string(ModelErrorRateLimited):
		return FailoverReasonRateLimited
	case string(ModelErrorQuota):
		return FailoverReasonQuotaExceeded
	case string(ModelErrorTimeout):
		return FailoverReasonTimeout
	case string(ModelErrorTransport):
		return FailoverReasonUpstreamError
	case string(ModelErrorInvalidResponse):
		return FailoverReasonInvalidResponse
	case string(ModelErrorEmptyResponse):
		return FailoverReasonEmptyResponse
	case string(ModelErrorTruncatedResponse):
		return FailoverReasonTruncatedResponse
	case string(ModelErrorAuthentication):
		return FailoverReasonAuthenticationRejected
	case string(ModelErrorAccess):
		return FailoverReasonAccessRejected
	case string(ModelErrorInvalidRequest):
		return FailoverReasonInvalidRequest
	case string(ModelErrorUnavailable):
		return FailoverReasonServiceUnavailable
	default:
		return FailoverReasonUnknown
	}
}

// JitteredBackoff returns the backoff delay for a retry: base doubled per
// attempt already spent, capped at max, plus up to 25% jitter. jitter must be
// in [0, 1); 0 means no jitter. The result never exceeds max.
func JitteredBackoff(attempt int64, base, max time.Duration, jitter float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	shift := attempt
	if shift > 10 {
		shift = 10
	}
	delay := base << uint(shift)
	if delay > max || delay < base {
		delay = max
	}
	withJitter := delay + time.Duration(float64(delay)*0.25*jitter)
	if withJitter > max {
		withJitter = max
	}
	return withJitter
}

// ModelRetryDecision is one ladder step.
type ModelRetryDecision struct {
	Action    FailoverAction
	Delay     time.Duration
	Exhausted bool
}

// DecideModelRetry decides the next ladder step for a failure. attempt is the
// number of retries already spent. A provider Retry-After always wins over
// the computed backoff (rounded up to whole seconds); the 120s cap applies
// to the computed backoff only. jitter must be in [0, 1).
func DecideModelRetry(reason FailoverReason, attempt int64, retryAfter time.Duration, jitter float64) ModelRetryDecision {
	strategy := FailoverStrategyFor(reason)
	if !strategy.Retryable {
		return ModelRetryDecision{Action: FailoverActionTerminal, Exhausted: true}
	}
	if attempt >= int64(strategy.MaxAttempts) {
		return ModelRetryDecision{Action: strategy.AfterExhaustion, Exhausted: true}
	}
	delay := JitteredBackoff(attempt, ModelRetryBaseDelay, ModelRetryMaxDelay, jitter)
	if retryAfter > 0 {
		delay = retryAfter
		if delay%time.Second != 0 {
			delay += time.Second - delay%time.Second
		}
	}
	return ModelRetryDecision{Action: FailoverActionRetry, Delay: delay}
}

// ModelLadderDecision is the task-level ladder outcome: the next step, and
// whether handing the task to another agent is allowed under the configured
// depth. maxAgentDepth is the sub-agent depth the handover may use
// (default ModelRetryMaxAgentDepth = 1; 0 means no handover is allowed).
func ApplyTaskModelFailureLadder(reason FailoverReason, attempt int64, retryAfter time.Duration, jitter float64, maxAgentDepth int) ModelRetryDecision {
	decision := DecideModelRetry(reason, attempt, retryAfter, jitter)
	if decision.Action == FailoverActionSwitchAgent && maxAgentDepth < 1 {
		return ModelRetryDecision{Action: FailoverActionTerminal, Exhausted: true}
	}
	return decision
}

// LadderExhaustedMessage is the terminal user-facing message for a spent
// ladder. It states what was tried and that the task is paused with its
// progress saved; it carries no stack trace and no raw error text. When the
// ladder actually retried, it tells the user no action is needed and a new
// message can retry later; when the failure could never be retried, it
// states only the pause, without promising a retry will help.
func LadderExhaustedMessage(reason FailoverReason, attempts int64) string {
	if attempts < 0 {
		attempts = 0
	}
	cause := failoverReasonUserText(reason)
	if attempts == 0 {
		return fmt.Sprintf("模型请求失败（%s），重试也无法解决，任务已暂停并保存进度。", cause)
	}
	return fmt.Sprintf("模型请求失败（%s），已重试 %d 次仍未成功，也换过处理方法，任务已暂停并保存进度；你不需要做什么，发送新消息可以再试一次。", cause, attempts)
}

func failoverReasonUserText(reason FailoverReason) string {
	switch reason {
	case FailoverReasonRateLimited:
		return "模型限流"
	case FailoverReasonQuotaExceeded:
		return "模型用量已用完"
	case FailoverReasonTimeout:
		return "网络超时"
	case FailoverReasonConnectionReset, FailoverReasonConnectionRefused, FailoverReasonDNSFailure, FailoverReasonTLSFailure:
		return "网络连接失败"
	case FailoverReasonServerError, FailoverReasonBadGateway, FailoverReasonServiceUnavailable, FailoverReasonGatewayTimeout, FailoverReasonUpstreamError:
		return "模型服务暂不可用"
	case FailoverReasonInvalidResponse, FailoverReasonResponseShapeInvalid, FailoverReasonEmptyResponse, FailoverReasonTruncatedResponse:
		return "模型返回内容异常"
	case FailoverReasonContextLengthExceeded:
		return "消息超过模型输入上限"
	case FailoverReasonContentFiltered:
		return "内容被模型过滤"
	case FailoverReasonAuthenticationRejected:
		return "模型认证失败"
	case FailoverReasonAccessRejected:
		return "模型访问被拒绝"
	case FailoverReasonInvalidRequest:
		return "模型请求无效"
	case FailoverReasonModelNotFound:
		return "模型不存在"
	case FailoverReasonUnsupportedParameter:
		return "模型不支持该参数"
	case FailoverReasonToolCallInvalid:
		return "工具调用无效"
	case FailoverReasonToolTimeout:
		return "工具执行超时"
	case FailoverReasonToolFailed:
		return "工具执行失败"
	case FailoverReasonBudgetExceeded:
		return "预算已用完"
	case FailoverReasonCancelled:
		return "请求已取消"
	case FailoverReasonStreamInterrupted:
		return "模型输出中断"
	case FailoverReasonUsageUnknown:
		return "模型用量未知"
	default:
		return "原因未知"
	}
}
