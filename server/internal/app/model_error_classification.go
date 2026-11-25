package app

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"morde/server/internal/domain"
)

// FailoverReason names why a model API call failed. The 31 values mirror
// hermes agent/error_classifier.py:35-69 (the earlier "33 values" claim
// contradicts the source and is obsolete). Morde classifies its own
// ModelClientErrorKind values into this table instead of copying hermes's
// string-matching pipeline: the provider status and error code are already
// structured by the time they reach the gateway.
type FailoverReason string

const (
	FailoverAuth                             FailoverReason = "auth"
	FailoverAuthPermanent                    FailoverReason = "auth_permanent"
	FailoverBilling                          FailoverReason = "billing"
	FailoverRateLimit                        FailoverReason = "rate_limit"
	FailoverUpstreamRateLimit                FailoverReason = "upstream_rate_limit"
	FailoverUpstreamBlocked                  FailoverReason = "upstream_blocked"
	FailoverOverloaded                       FailoverReason = "overloaded"
	FailoverServerError                      FailoverReason = "server_error"
	FailoverTimeout                          FailoverReason = "timeout"
	FailoverSSLCertVerification              FailoverReason = "ssl_cert_verification"
	FailoverContextOverflow                  FailoverReason = "context_overflow"
	FailoverPayloadTooLarge                  FailoverReason = "payload_too_large"
	FailoverImageTooLarge                    FailoverReason = "image_too_large"
	FailoverImageCorrupt                     FailoverReason = "image_corrupt"
	FailoverModelNotFound                    FailoverReason = "model_not_found"
	FailoverProviderPolicyBlocked            FailoverReason = "provider_policy_blocked"
	FailoverDataPolicyDenied                 FailoverReason = "data_policy_denied"
	FailoverContentPolicyBlocked             FailoverReason = "content_policy_blocked"
	FailoverGuardrailBlocked                 FailoverReason = "guardrail_blocked"
	FailoverModelEntitlement                 FailoverReason = "model_entitlement"
	FailoverIncompleteResponse               FailoverReason = "incomplete_response"
	FailoverFormatError                      FailoverReason = "format_error"
	FailoverRoleAlternation                  FailoverReason = "role_alternation"
	FailoverInvalidEncryptedContent          FailoverReason = "invalid_encrypted_content"
	FailoverMultimodalToolContentUnsupported FailoverReason = "multimodal_tool_content_unsupported"
	FailoverReasoningMandatory               FailoverReason = "reasoning_mandatory"
	FailoverThinkingSignature                FailoverReason = "thinking_signature"
	FailoverLongContextTier                  FailoverReason = "long_context_tier"
	FailoverOAuthLongContextBetaForbidden    FailoverReason = "oauth_long_context_beta_forbidden"
	FailoverLlamaCppGrammarPattern           FailoverReason = "llama_cpp_grammar_pattern"
	FailoverUnknown                          FailoverReason = "unknown"
)

// AllFailoverReasons returns every FailoverReason in hermes's table order.
// The count is part of the contract: the classification table must cover
// all 31 values.
func AllFailoverReasons() []FailoverReason {
	return []FailoverReason{
		FailoverAuth, FailoverAuthPermanent, FailoverBilling, FailoverRateLimit,
		FailoverUpstreamRateLimit, FailoverUpstreamBlocked, FailoverOverloaded,
		FailoverServerError, FailoverTimeout, FailoverSSLCertVerification,
		FailoverContextOverflow, FailoverPayloadTooLarge, FailoverImageTooLarge,
		FailoverImageCorrupt, FailoverModelNotFound, FailoverProviderPolicyBlocked,
		FailoverDataPolicyDenied, FailoverContentPolicyBlocked, FailoverGuardrailBlocked,
		FailoverModelEntitlement, FailoverIncompleteResponse, FailoverFormatError,
		FailoverRoleAlternation, FailoverInvalidEncryptedContent,
		FailoverMultimodalToolContentUnsupported, FailoverReasoningMandatory,
		FailoverThinkingSignature, FailoverLongContextTier,
		FailoverOAuthLongContextBetaForbidden, FailoverLlamaCppGrammarPattern,
		FailoverUnknown,
	}
}

// FailoverStrategy is the recovery the ladder may attempt for a reason.
// UserVisibleCase is the muse-user-service-design §4.3 case the failure
// reaches the user under once the ladder is exhausted (0: it never reaches
// the user on its own; the ladder only ends in a terminal state).
// A reason with neither Retryable nor Fallback nor NeedsRepair is terminal
// for the ladder: resending the same request cannot help.
type FailoverStrategy struct {
	// Retryable retries the same provider with backoff.
	Retryable bool
	// Fallback tries a different method or model after same-provider retries.
	Fallback bool
	// NeedsRepair needs a request fix (compress, strip, re-shape) Morde
	// cannot perform automatically, so the ladder ends there.
	NeedsRepair bool
	// UserVisibleCase is 0 or one of the §4.3 cases 1-4.
	UserVisibleCase int
}

// failoverReasonStrategies covers all 31 FailoverReason values. Grouping
// follows service-design §4.1: transient (timeout/overloaded/server_error),
// rate limit (rate_limit/upstream_rate_limit), repair-then-retry (12),
// terminal (13), and the unknown fallback.
var failoverReasonStrategies = map[FailoverReason]FailoverStrategy{
	FailoverAuth:                             {Retryable: false, Fallback: true, UserVisibleCase: 2},
	FailoverAuthPermanent:                    {UserVisibleCase: 2},
	FailoverBilling:                          {Fallback: true, UserVisibleCase: 2},
	FailoverRateLimit:                        {Retryable: true, Fallback: true, UserVisibleCase: 1},
	FailoverUpstreamRateLimit:                {Retryable: true, Fallback: true, UserVisibleCase: 1},
	FailoverUpstreamBlocked:                  {Fallback: true, UserVisibleCase: 4},
	FailoverOverloaded:                       {Retryable: true, UserVisibleCase: 1},
	FailoverServerError:                      {Retryable: true, UserVisibleCase: 1},
	FailoverTimeout:                          {Retryable: true, UserVisibleCase: 1},
	FailoverSSLCertVerification:              {UserVisibleCase: 1},
	FailoverContextOverflow:                  {NeedsRepair: true, UserVisibleCase: 4},
	FailoverPayloadTooLarge:                  {NeedsRepair: true, UserVisibleCase: 4},
	FailoverImageTooLarge:                    {NeedsRepair: true, UserVisibleCase: 4},
	FailoverImageCorrupt:                     {NeedsRepair: true, UserVisibleCase: 3},
	FailoverModelNotFound:                    {Fallback: true, UserVisibleCase: 4},
	FailoverProviderPolicyBlocked:            {UserVisibleCase: 3},
	FailoverDataPolicyDenied:                 {UserVisibleCase: 3},
	FailoverContentPolicyBlocked:             {UserVisibleCase: 3},
	FailoverGuardrailBlocked:                 {UserVisibleCase: 3},
	FailoverModelEntitlement:                 {Fallback: true, UserVisibleCase: 4},
	FailoverIncompleteResponse:               {Fallback: true, UserVisibleCase: 4},
	FailoverFormatError:                      {UserVisibleCase: 4},
	FailoverRoleAlternation:                  {NeedsRepair: true, UserVisibleCase: 4},
	FailoverInvalidEncryptedContent:          {NeedsRepair: true, UserVisibleCase: 4},
	FailoverMultimodalToolContentUnsupported: {NeedsRepair: true, UserVisibleCase: 4},
	FailoverReasoningMandatory:               {NeedsRepair: true, UserVisibleCase: 4},
	FailoverThinkingSignature:                {NeedsRepair: true, UserVisibleCase: 4},
	FailoverLongContextTier:                  {NeedsRepair: true, UserVisibleCase: 4},
	FailoverOAuthLongContextBetaForbidden:    {NeedsRepair: true, UserVisibleCase: 4},
	FailoverLlamaCppGrammarPattern:           {NeedsRepair: true, UserVisibleCase: 4},
	FailoverUnknown:                          {Retryable: true, UserVisibleCase: 4},
}

// FailoverStrategyFor returns the ladder strategy for a reason. Unknown
// reasons fall back to the unknown strategy instead of failing open.
func FailoverStrategyFor(reason FailoverReason) FailoverStrategy {
	if strategy, ok := failoverReasonStrategies[reason]; ok {
		return strategy
	}
	return failoverReasonStrategies[FailoverUnknown]
}

// modelClientErrorKindReasons maps Morde's 11 ModelClientErrorKind values to
// the closest FailoverReason before provider-code refinement.
var modelClientErrorKindReasons = map[ModelClientErrorKind]FailoverReason{
	ModelErrorAuthentication:    FailoverAuth,
	ModelErrorAccess:            FailoverAuthPermanent,
	ModelErrorRateLimited:       FailoverRateLimit,
	ModelErrorQuota:             FailoverBilling,
	ModelErrorInvalidRequest:    FailoverFormatError,
	ModelErrorUnavailable:       FailoverServerError,
	ModelErrorTimeout:           FailoverTimeout,
	ModelErrorTransport:         FailoverTimeout,
	ModelErrorInvalidResponse:   FailoverFormatError,
	ModelErrorEmptyResponse:     FailoverIncompleteResponse,
	ModelErrorTruncatedResponse: FailoverIncompleteResponse,
}

// providerCodeFailoverReasons refines the kind mapping with the structured
// provider code. Only codes the gateway already recognises (see
// modelConnectionCheckError and providerStatusError) are listed; anything
// else keeps the kind-level reason.
var providerCodeFailoverReasons = map[string]FailoverReason{
	"insufficient_quota":         FailoverBilling,
	"billing_hard_limit_reached": FailoverBilling,
	"context_length_exceeded":    FailoverContextOverflow,
	"model_not_found":            FailoverModelNotFound,
	"invalid_request_error":      FailoverFormatError,
	"unsupported_parameter":      FailoverFormatError,
	"server_error":               FailoverServerError,
	"rate_limit_exceeded":        FailoverRateLimit,
	"permission_denied":          FailoverAuthPermanent,
	"invalid_api_key":            FailoverAuth,
	"overloaded":                 FailoverOverloaded,
	"1305":                       FailoverOverloaded,
}

// ErrorClassification is the ladder input for one failed model call.
type ErrorClassification struct {
	Reason FailoverReason
	// Strategy is the table entry for Reason.
	Strategy FailoverStrategy
	// SourceKind is the Morde ModelClientErrorKind the reason came from
	// ("" when the error carried no such kind).
	SourceKind ModelClientErrorKind
	// Known is false when the error carried no model error at all, so the
	// reason is the unknown fallback rather than a real classification.
	Known bool
}

func classifyModelClientErrorKind(kind ModelClientErrorKind, providerCode string) ErrorClassification {
	reason, ok := modelClientErrorKindReasons[kind]
	if !ok {
		reason = FailoverUnknown
	}
	if refined, ok := providerCodeFailoverReasons[strings.ToLower(strings.TrimSpace(providerCode))]; ok {
		reason = refined
	}
	return ErrorClassification{Reason: reason, Strategy: FailoverStrategyFor(reason), SourceKind: kind, Known: true}
}

// ClassifyModelClientError maps a model call failure to a FailoverReason.
// It accepts the *ModelClientError the gateway produces and the *domain.Error
// the gateway converts it to (Details reason/stage/provider_code); anything
// else classifies as unknown and retryable, never as a terminal verdict.
func ClassifyModelClientError(err error) ErrorClassification {
	var clientErr *ModelClientError
	if errors.As(err, &clientErr) && clientErr != nil {
		return classifyModelClientErrorKind(clientErr.Kind, clientErr.ProviderCode)
	}
	var routeErr *domain.Error
	if errors.As(err, &routeErr) && routeErr != nil {
		if reason, ok := routeErr.Details["reason"].(string); ok && reason != "" {
			code, _ := routeErr.Details["provider_code"].(string)
			return classifyModelClientErrorKind(ModelClientErrorKind(reason), code)
		}
		// Some route errors carry only the structured provider code (for
		// example the context-window error), which is enough to refine.
		if code, ok := routeErr.Details["provider_code"].(string); ok {
			if reason, ok := providerCodeFailoverReasons[strings.ToLower(strings.TrimSpace(code))]; ok {
				return ErrorClassification{Reason: reason, Strategy: FailoverStrategyFor(reason), Known: true}
			}
		}
	}
	return ErrorClassification{Reason: FailoverUnknown, Strategy: FailoverStrategyFor(FailoverUnknown)}
}

// Retry ladder constants (enhancement plan E12; hermes api_max_retries = 3,
// jittered backoff base 5s / max 120s). The ladder is "3 retries, then
// fallback, then terminal" — hermes's 5-cycle CLI auto-recovery ladder is
// deliberately NOT copied (CLI shape; see TODO 09).
const (
	// ModelRetryMaxRetries bounds same-provider retries per ladder run.
	ModelRetryMaxRetries = 3
	// ModelRetryBackoffBase / ModelRetryBackoffMax bound the jittered
	// exponential backoff between retries.
	ModelRetryBackoffBase = 5 * time.Second
	ModelRetryBackoffMax  = 120 * time.Second
	// ModelRetryMaxRetryAfter caps a provider Retry-After used as a ladder
	// delay. The route worker's durable queue honours provider waits up to
	// its own 24h horizon instead (M09-02/M09-05); that horizon is passed
	// explicitly at the call site, not through this default.
	ModelRetryMaxRetryAfter = 600 * time.Second
	// ModelRetryJitterRatio is the hermes jitter: delay + uniform[0, ratio*delay].
	ModelRetryJitterRatio = 0.5
)

// RouteRetryLadderExhaustedReason is the durable failure_reason written when
// the route ladder spent its three provider rejections. It is distinct from
// "model_unavailable" (any other model failure) so a failed route tells
// whether the ladder ran out. The column is free text; no migration.
const RouteRetryLadderExhaustedReason = "route_retry_ladder_exhausted"

// JitteredBackoff returns min(base*2^(attempt-1), max) plus uniform jitter in
// [0, jitterRatio*delay], following hermes retry_utils.jittered_backoff.
// attempt is 1-based; values below 1 count as the first attempt. randFloat
// supplies the [0,1) sample (nil counts as 0, the lower bound) so tests stay
// deterministic without real sleeps.
func JitteredBackoff(attempt int, base, max time.Duration, jitterRatio float64, randFloat func() float64) time.Duration {
	if base <= 0 || max <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 62 {
		exponent = 62
	}
	delay := float64(base) * math.Pow(2, float64(exponent))
	if delay > float64(max) {
		delay = float64(max)
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	sample := 0.0
	if randFloat != nil {
		sample = randFloat()
		if sample < 0 {
			sample = 0
		}
		if sample > 1 {
			sample = 1
		}
	}
	return time.Duration(delay + sample*jitterRatio*delay)
}

// ModelRetryDelay computes the wait before retry attempt (1-based). A
// provider Retry-After wins, capped at retryAfterCap; otherwise the jittered
// backoff applies. It returns false once the retry budget is spent.
func ModelRetryDelay(attempt int, retryAfter, retryAfterCap time.Duration, randFloat func() float64) (time.Duration, bool) {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > ModelRetryMaxRetries {
		return 0, false
	}
	if retryAfter > 0 {
		if retryAfterCap > 0 && retryAfter > retryAfterCap {
			return retryAfterCap, true
		}
		return retryAfter, true
	}
	return JitteredBackoff(attempt, ModelRetryBackoffBase, ModelRetryBackoffMax, ModelRetryJitterRatio, randFloat), true
}

// ModelRetryAction is the ladder's next step for a classified failure.
type ModelRetryAction string

const (
	// ModelRetryActionRetry resends to the same provider after Delay.
	ModelRetryActionRetry ModelRetryAction = "retry"
	// ModelRetryActionFallback changes method or agent (route heuristic
	// fallback, a different model, or a subagent handover).
	ModelRetryActionFallback ModelRetryAction = "fallback"
	// ModelRetryActionTerminal ends the ladder; the caller records the
	// terminal state and, only for §4.3 cases, tells the user.
	ModelRetryActionTerminal ModelRetryAction = "terminal"
)

// ModelRetryDecision is the pure ladder verdict for one failure.
type ModelRetryDecision struct {
	Action ModelRetryAction
	// Delay is the wait before Action == retry.
	Delay time.Duration
	// Exhausted is true when no further same-provider retry will be tried
	// (budget spent, or the reason cannot be retried at all).
	Exhausted bool
	Reason    FailoverReason
	// UserVisibleCase is the §4.3 case (0: never user-visible on its own).
	UserVisibleCase int
}

// DecideModelRetry applies the ladder to a classified failure.
// attemptsUsed counts already-spent same-provider retries. A terminal or
// repair-only reason ends the ladder immediately; a retryable reason retries
// while budget remains and falls back to a different method/agent once the
// budget is spent.
func DecideModelRetry(classification ErrorClassification, attemptsUsed int, retryAfter, retryAfterCap time.Duration, randFloat func() float64) ModelRetryDecision {
	decision := ModelRetryDecision{Reason: classification.Reason, UserVisibleCase: classification.Strategy.UserVisibleCase}
	if attemptsUsed < 0 {
		attemptsUsed = 0
	}
	switch {
	case !classification.Strategy.Retryable && !classification.Strategy.Fallback:
		decision.Action = ModelRetryActionTerminal
		decision.Exhausted = true
	case attemptsUsed >= ModelRetryMaxRetries:
		decision.Exhausted = true
		if classification.Strategy.Fallback {
			decision.Action = ModelRetryActionFallback
		} else {
			decision.Action = ModelRetryActionTerminal
		}
	case !classification.Strategy.Retryable:
		decision.Action = ModelRetryActionFallback
		decision.Exhausted = true
	default:
		delay, ok := ModelRetryDelay(attemptsUsed+1, retryAfter, retryAfterCap, randFloat)
		if !ok {
			decision.Action = ModelRetryActionTerminal
			decision.Exhausted = true
			break
		}
		decision.Action = ModelRetryActionRetry
		decision.Delay = delay
	}
	return decision
}

// ladderExhaustedUserMessages states, per §4.3 case, what the user is told
// once the ladder is spent. The text names where it got stuck, what was
// tried, and what is needed next. It is built only from the reason's case
// and the attempt count, so no provider text, stack trace, or error code
// can leak into it.
var ladderExhaustedUserMessages = map[int]string{
	1: "模型服务暂时连不上，已重试 %d 次仍未成功。任务已暂停，请检查网络或模型连接后让我再试一次。",
	2: "这一步需要你的授权或可用的模型凭证，已重试 %d 次仍未通过。请检查模型连接与授权后让我再试一次。",
	3: "这个请求模型方无法接受，已重试 %d 次仍是同样结果。请换一种说法或换一个模型再试。",
	4: "这件事我现在做不下去，已重试 %d 次仍未成功。任务已暂停，失败原因已记在任务详情里。",
}

// LadderExhaustedUserMessage renders the user-visible terminal text for a
// spent ladder. Reasons with no §4.3 case (UserVisibleCase 0) have no
// user-facing text; the caller must not invent one.
func LadderExhaustedUserMessage(classification ErrorClassification, attempts int) (string, bool) {
	template, ok := ladderExhaustedUserMessages[classification.Strategy.UserVisibleCase]
	if !ok || classification.Strategy.UserVisibleCase == 0 {
		return "", false
	}
	if attempts < 0 {
		attempts = 0
	}
	return fmt.Sprintf(template, attempts), true
}
