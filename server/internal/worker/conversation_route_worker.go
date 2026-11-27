package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type ConversationRouteWorkerOptions struct {
	Queue         app.ConversationRouteQueue
	Actions       app.ConversationRouteActions
	Models        app.ConversationRouteClassifier
	SummaryStore  app.ConversationSummaryStore
	SummaryInputs app.ConversationSummaryInputPageStore
	SummarySealer *app.Sealer
	WorkerID      string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	MaxConcurrent int
	Logger        *zap.Logger
	Metrics       prometheus.Registerer
}

type ConversationRouteWorker struct {
	options ConversationRouteWorkerOptions
	metrics *conversationRouteMetrics
}

func NewConversationRouteWorker(options ConversationRouteWorkerOptions) (*ConversationRouteWorker, error) {
	if options.Queue == nil || options.Actions == nil || options.Models == nil || options.WorkerID == "" || strings.TrimSpace(options.WorkerID) != options.WorkerID ||
		len(options.WorkerID) > 128 || options.LeaseDuration < time.Minute || options.LeaseDuration > 5*time.Minute ||
		options.LeaseDuration%time.Second != 0 || options.PollInterval <= 0 || options.MaxConcurrent < 1 || options.MaxConcurrent > 16 ||
		options.Logger == nil || (options.SummaryStore == nil) != (options.SummaryInputs == nil) ||
		(options.SummaryStore != nil && options.SummarySealer == nil) {
		return nil, errors.New("conversation routing dependencies and bounded worker options are required")
	}
	metrics, err := newConversationRouteMetrics(options.Metrics)
	if err != nil {
		return nil, err
	}
	return &ConversationRouteWorker{options: options, metrics: metrics}, nil
}

// Run routes independent messages concurrently; each worker owns a separate
// lease. A restart can recover any route that did not commit an action.
func (w *ConversationRouteWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("conversation routing context is required")
	}
	var group sync.WaitGroup
	for index := 0; index < w.options.MaxConcurrent; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for ctx.Err() == nil {
				found, err := w.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					w.options.Logger.Warn("conversation routing iteration failed", zap.String("job", "conversation_route"))
				}
				if found && err == nil {
					continue
				}
				timer := time.NewTimer(w.options.PollInterval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	<-ctx.Done()
	group.Wait()
	return ctx.Err()
}

// RunOnce processes at most one leased message. Model failures and stale
// context conflicts are durable; routing cannot rely on an absent predecessor.
func (w *ConversationRouteWorker) RunOnce(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, errors.New("conversation routing context is required")
	}
	if applied, err := w.options.Actions.ApplyNextConversationTaskRevision(ctx); err != nil || applied {
		return applied, err
	}
	claim, found, err := w.options.Queue.ClaimConversationRoute(ctx, w.options.WorkerID, w.options.LeaseDuration)
	if err != nil || !found {
		return found, err
	}
	routeCtx, cancel := context.WithDeadline(ctx, claim.LeaseExpiresAt.Add(-time.Second))
	defer cancel()
	snapshot, err := w.options.Actions.LoadConversationRouteSnapshot(routeCtx, claim)
	if err != nil {
		return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
	}
	if snapshot.ResultReviewSuperseded {
		return true, nil
	}
	if snapshot.AggregationCanWait {
		return true, w.options.Actions.DeferConversationRoute(routeCtx, claim, time.Second)
	}
	if snapshot.AggregationNeedsClarification {
		return true, w.options.Actions.ApplyConversationRoute(routeCtx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteClarify, Question: "短时间内收到的消息关系尚不明确，请先处理澄清；若属于同一项工作，请重新发送完整要求。",
		})
	}
	if snapshot.AggregationUnresolved {
		if resolver, ok := w.options.Actions.(app.ConversationRouteAggregationResolution); ok {
			return true, resolver.ResolveConversationRouteAggregation(routeCtx, claim)
		}
		return true, w.failRoute(ctx, claim, "routing_context_unavailable", errors.New("conversation aggregation contains unresolved messages"))
	}
	if len(snapshot.Context.Content) > app.MaxConversationMessageBytes {
		err := domain.NewError(domain.CodeResultTooLarge, "message exceeds the accepted conversation message limit")
		err.Details["stage"] = "route_message_storage_limit"
		contentBytes, historyBytes := conversationRouteContextSizeDetails(snapshot.Context)
		err.Details["content_bytes"] = contentBytes
		err.Details["history_bytes"] = historyBytes
		return true, w.failRoute(ctx, claim, "message_too_long", err)
	}
	if snapshot.PreviousCycle {
		return true, w.options.Actions.ApplyConversationRoute(routeCtx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteClarify, Question: "消息顺序存在循环，请重新发送。",
		})
	}
	modelID, err := w.options.Models.ResolveConversationRouteModel(routeCtx, claim)
	if err != nil || !domain.ValidModelConnectionID(modelID) {
		if routeCtx.Err() != nil && ctx.Err() == nil {
			return true, w.options.Actions.DeferConversationRoute(ctx, claim, 2*time.Second)
		}
		if err == nil {
			err = errors.New("resolved conversation model identity is invalid")
		}
		return true, w.failRoute(ctx, claim, "model_unavailable", err)
	}
	contextWindow, capacityKnown := 4096, false
	if resolver, ok := w.options.Models.(interface {
		ResolveConversationRouteContextWindow(context.Context, app.ConversationRouteClaim, string) (int, bool, error)
	}); ok {
		resolvedWindow, known, capacityErr := resolver.ResolveConversationRouteContextWindow(routeCtx, claim, modelID)
		if capacityErr != nil {
			if routeCtx.Err() != nil && ctx.Err() == nil {
				return true, w.options.Actions.DeferConversationRoute(ctx, claim, 2*time.Second)
			}
			return true, w.failRoute(ctx, claim, "model_unavailable", capacityErr)
		}
		if resolvedWindow >= 4096 && resolvedWindow <= app.MaxModelContextWindowTokens {
			contextWindow, capacityKnown = resolvedWindow, known
		}
	}
	if !capacityKnown {
		w.options.Logger.Warn("conversation route model capacity is unknown; using minimum supported window",
			zap.String("job", "conversation_route"))
	}
	if snapshot.Context.ResultReview == nil {
		compacted, err := w.compactConversationSummary(routeCtx, claim, modelID)
		if err != nil {
			if routeCtx.Err() != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return true, w.options.Actions.DeferConversationRoute(ctx, claim, 2*time.Second)
			}
			w.options.Logger.Warn("conversation summary compaction skipped", zap.String("job", "conversation_route"), zap.Error(err))
		} else if compacted {
			snapshot, err = w.options.Actions.LoadConversationRouteSnapshot(routeCtx, claim)
			if err != nil {
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
			}
		}
	}
	snapshot.Context.ModelID = modelID
	classifyContext := snapshot.Context
	if !classifyContext.ContentTruncated && classifyContext.Content == claim.Message.Content {
		classifyContext.CurrentMessageID = claim.Message.ID
	}
	if snapshot.Summary != nil {
		document, openErr := app.OpenConversationSummary(w.options.SummarySealer, *snapshot.Summary)
		if openErr == nil && document.Identity == snapshot.Summary.Identity && document.Version == classifyContext.SummaryVersion &&
			bytes.Equal(snapshot.Summary.SummarySHA256, classifyContext.SummarySHA256) &&
			bytes.Equal(snapshot.Summary.SourcesSHA256, classifyContext.SummarySourcesSHA256) {
			classifyContext.SummaryText = document.Text
			classifyContext.History = app.ConversationRouteHistoryWithoutSummarySources(snapshot.Context.History, snapshot.Summary.Sources)
		} else {
			classifyContext.SummaryVersion = 0
			classifyContext.SummarySHA256, classifyContext.SummarySourcesSHA256 = nil, nil
			w.options.Logger.Warn("conversation route summary could not be opened", zap.String("job", "conversation_route"))
		}
	} else {
		classifyContext.SummaryVersion = 0
		classifyContext.SummarySHA256, classifyContext.SummarySourcesSHA256 = nil, nil
	}
	// The snapshot carries at most eight recent history rows.
	if classifyContext.SummaryVersion == 0 && classifyContext.ResultReview == nil && claim.Message.Sequence > 9 {
		classifyContext.HistoryIncomplete = true
	}
	classifyContext.ModelID = modelID
	if claim.Input.ResultReviewID == "" {
		if repairStatus, ok := w.options.Actions.(app.ConversationRouteRepairStatus); ok {
			repair, statusErr := repairStatus.IsConversationRouteRepairAttempt(routeCtx, claim)
			if statusErr != nil {
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", statusErr)
			}
			classifyContext.Repair = repair
		}
	}
	classifyContext.History = append([]app.ConversationRouteHistory(nil), classifyContext.History...)
	copyConversationRouteCandidates(&classifyContext)
	if err := fitConversationRouteContext(&classifyContext, contextWindow); err != nil {
		return true, w.failRoute(ctx, claim, "message_too_long",
			conversationRouteBudgetDiagnostic(err, "worker_context_fit", contextWindow, capacityKnown, classifyContext))
	}
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	if classifyContext.Content != "" || len(classifyContext.Candidates) != 0 || classifyContext.ReferenceTaskID != "" ||
		classifyContext.PreviousUnresolved || classifyContext.SummaryVersion > 0 || classifyContext.ResultReview != nil {
		decision, err = w.options.Models.ClassifyConversationRoute(routeCtx, claim, classifyContext)
		if err != nil {
			if snapshot.Context.ResultReview != nil && routeCtx.Err() != nil && ctx.Err() == nil {
				return true, w.options.Actions.DeferConversationRoute(ctx, claim, 5*time.Second)
			}
			if domain.CodeOf(err) == domain.CodeResultTooLarge {
				return true, w.failRoute(ctx, claim, "message_too_long",
					conversationRouteBudgetDiagnostic(err, "model_request", contextWindow, capacityKnown, classifyContext))
			}
			if snapshot.PreviousMissing && domain.CodeOf(err) == domain.CodeConflict && ctx.Err() == nil {
				if recovery, ok := w.options.Actions.(app.ConversationRoutePredecessorRecovery); ok {
					requeued, recoveryErr := recovery.RequeueConversationRouteAfterPredecessor(ctx, claim, snapshot)
					if recoveryErr != nil {
						return true, errors.Join(err, recoveryErr)
					}
					if requeued {
						return true, nil
					}
				}
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
			}
			if !snapshot.PreviousMissing && claim.Input.ResultReviewID == "" && domain.CodeOf(err) == domain.CodeConflict && ctx.Err() == nil {
				if recovery, ok := w.options.Actions.(app.ConversationRouteContextRecovery); ok {
					requeued, recoveryErr := recovery.RequeueConversationRouteAfterContextChange(ctx, claim)
					if recoveryErr != nil {
						return true, errors.Join(err, recoveryErr)
					}
					if requeued {
						return true, nil
					}
				}
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
			}
			if claim.Input.ResultReviewID == "" {
				if delay, confirmed := conversationRouteRateLimitDelay(claim, err); confirmed {
					if recovery, ok := w.options.Actions.(app.ConversationRouteRateLimitRecovery); ok {
						recoveryCtx := ctx
						var cancelRecovery context.CancelFunc
						if ctx.Err() != nil {
							recoveryCtx, cancelRecovery = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
							defer cancelRecovery()
						}
						handled, recoveryErr := recovery.HandleConversationRouteRateLimit(recoveryCtx, claim, delay)
						if recoveryErr != nil {
							return true, errors.Join(err, recoveryErr)
						}
						if handled {
							return true, nil
						}
					}
				}
			}
			// Classification failure is an infrastructure outcome, not evidence
			// that the user's goal is ambiguous. Keep the accepted source message
			// retryable; the route ledger decides whether an unknown call must be
			// reconciled before a new attempt can be dispatched.
			w.options.Logger.Warn("conversation route classification failed; preserving source for retry",
				zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
				zap.String("failure_code", string(domain.CodeOf(err))))
			return true, w.failRoute(ctx, claim, "model_unavailable", err)
		}
	}
	if decision.InvalidOutput {
		if claim.Input.ResultReviewID == "" {
			if recovery, ok := w.options.Actions.(app.ConversationRouteInvalidOutputRecovery); ok {
				requeued, recoveryErr := recovery.RequeueConversationRouteAfterInvalidOutput(ctx, claim)
				if recoveryErr != nil {
					return true, recoveryErr
				}
				if requeued {
					return true, nil
				}
			}
		}
		// The model output can contain user data, so log metadata only.
		w.options.Logger.Warn("conversation route model output is invalid; requesting safe clarification",
			zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
			zap.String("failure_code", "invalid_model_output"),
			zap.Int("invalid_output_length_bytes", decision.InvalidOutputLengthBytes),
			zap.Bool("invalid_output_utf8_valid", decision.InvalidOutputUTF8Valid))
		return true, w.failRoute(ctx, claim, "routing_action_unavailable",
			errors.New("conversation route model output remained invalid after repair"))
	}
	decision = safeConversationRouteDecision(classifyContext, decision)
	if decision.InvalidOutput {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("route model output is invalid for the current context"))
	}
	if snapshot.Context.ResultReview != nil && decision.Action != app.ConversationRouteReply {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("task result review did not produce a reply"))
	}
	if decision.Action == app.ConversationRouteNew && !classifyContext.ContentTruncated {
		if aggregation, ok := w.options.Actions.(app.ConversationRouteAggregation); ok {
			started, err := aggregation.BeginConversationRouteAggregation(routeCtx, claim, snapshot, decision)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeConflict || routeCtx.Err() != nil {
					return true, err
				}
				return true, w.failRoute(ctx, claim, "routing_action_unavailable", err)
			}
			if started {
				if w.metrics != nil {
					w.metrics.aggregationStates.WithLabelValues("waiting").Inc()
				}
				return true, nil
			}
		}
	}
	if err := w.options.Actions.ApplyConversationRoute(routeCtx, claim, snapshot, decision); err != nil {
		w.observeRouteDecision(claim, decision, "failed")
		if domain.CodeOf(err) == domain.CodeConflict && (claim.Input.ResultReviewID != "" || routeCtx.Err() == nil) {
			return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
		}
		if domain.CodeOf(err) == domain.CodeConflict || routeCtx.Err() != nil {
			return true, err
		}
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", err)
	}
	w.observeRouteDecision(claim, decision, "applied")
	return true, nil
}

// conversationRouteRetryAfterHorizon is the durable route queue's horizon
// for a provider-named wait (M09-02/M09-05). It is deliberately longer than
// the E12 ladder's ModelRetryMaxRetryAfter (600s): a provider-named wait is
// a scheduled resume persisted as available_after, not a ladder retry delay,
// so it is honoured up to 24h and only ends the attempt beyond that.
const conversationRouteRetryAfterHorizon = 24 * time.Hour

// conversationRouteRateLimitDelay decides whether a confirmed provider 429
// requeues the route and for how long. The delay is the E12 ladder: a
// provider Retry-After wins (honoured up to the durable 24h horizon, never
// truncated), otherwise the jittered backoff (base 5s / max 120s) indexed by
// the persisted route attempt. Returning (0, true) ends the attempt: the
// store then marks the route failed instead of requeueing it. The retry
// count itself stays durable in the store (three rejected calls, counted
// from the route ledger), so a worker restart cannot reset the ladder.
func conversationRouteRateLimitDelay(claim app.ConversationRouteClaim, err error) (time.Duration, bool) {
	var routeErr *domain.Error
	if !errors.As(err, &routeErr) || routeErr.Code != domain.CodeRateLimited ||
		routeErr.Details["reason"] != string(app.ModelErrorRateLimited) || routeErr.Details["stage"] != "provider_status" {
		return 0, false
	}
	classification := app.ClassifyModelClientError(err)
	if !classification.Known || classification.Reason != app.FailoverRateLimit {
		return 0, false
	}
	if routeErr.Details["retry_after_exceeds_durable_horizon"] == "true" || routeErr.RetryAfter > conversationRouteRetryAfterHorizon {
		// The durable route queue supports at most 24 hours; do not retry earlier than the provider requested.
		return 0, true
	}
	// Use the shared failover decision for the same-provider retry budget and
	// Retry-After handling. The route store remains authoritative for the
	// durable rejected-attempt count and next-attempt schedule.
	if claim.RouteAttempt < 0 || claim.RouteAttempt >= int64(app.ModelRetryMaxRetries) {
		return 0, true
	}
	decision := app.DecideModelRetry(classification, int(claim.RouteAttempt), routeErr.RetryAfter,
		conversationRouteRetryAfterHorizon, rand.Float64)
	if decision.Action != app.ModelRetryActionRetry {
		return 0, true
	}
	delay := decision.Delay
	// The store requires whole seconds; round up so the wait is never
	// shorter than the ladder computed.
	seconds := delay / time.Second
	if delay%time.Second != 0 {
		seconds++
	}
	return seconds * time.Second, true
}

// conversationRouteHistoryPreviewBytes bounds one history entry when the
// route context is shrunk to fit the model window. A preview keeps the
// entry's identity plus its head and tail instead of blanking it outright.
const conversationRouteHistoryPreviewBytes = app.ConversationRouteHistoryPreviewBytes

func fitConversationRouteContext(value *app.ConversationRouteContext, contextWindow int) error {
	history, err := app.PrepareConversationRouteHistory(value.History)
	if err != nil {
		return err
	}
	value.History = history
	for _, item := range history {
		if item.Truncated {
			value.HistoryIncomplete = true
			break
		}
	}
	app.ProjectConversationRouteMessage(value, app.ConversationRouteCurrentMessagePreviewBytes)
	for {
		request, err := app.ConversationRouteModelRequest(*value)
		if err == nil && app.ConversationRouteInputFitsContextWindow(request, contextWindow) {
			return nil
		}
		if err != nil && domain.CodeOf(err) != domain.CodeResultTooLarge {
			return err
		}
		if trimConversationRouteCandidates(value) {
			continue
		}
		// Shrink history to previews before dropping the summary: the
		// summary is the compressed form of older history, so it is the
		// last thing to go, not the first.
		if previewConversationRouteHistory(value) {
			continue
		}
		if blankConversationRouteHistory(value) {
			continue
		}
		if value.SummaryVersion > 0 {
			value.SummaryVersion = 0
			value.SummarySHA256, value.SummarySourcesSHA256, value.SummaryText = nil, nil, ""
			value.HistoryIncomplete = true
			continue
		}
		if app.ShortenConversationRouteMessage(value) {
			continue
		}
		if trimConversationRouteHistoryReferences(value) {
			continue
		}
		if err != nil {
			return err
		}
		return domain.NewError(domain.CodeResultTooLarge, "conversation message exceeds the selected model context budget")
	}
}

// trimConversationRouteHistoryReferences drops the oldest already-blanked
// references only after the summary is exhausted; the incomplete marker then
// directs any missing historical detail to authorized source search.
func trimConversationRouteHistoryReferences(value *app.ConversationRouteContext) bool {
	if value == nil {
		return false
	}
	for index, item := range value.History {
		if item.Content != "" || !item.Truncated {
			continue
		}
		value.History = append(value.History[:index], value.History[index+1:]...)
		value.HistoryIncomplete = true
		return true
	}
	return false
}

// previewConversationRouteHistory replaces the oldest oversized history body
// with a head-and-tail view while preserving its original source digest.
func previewConversationRouteHistory(value *app.ConversationRouteContext) bool {
	for index := range value.History {
		item := &value.History[index]
		if len(item.Content) <= conversationRouteHistoryPreviewBytes {
			continue
		}
		preview, truncated := app.ConversationRouteTextPreview(item.Content, conversationRouteHistoryPreviewBytes)
		if !truncated {
			continue
		}
		item.Content = preview
		item.Truncated = true
		value.HistoryIncomplete = true
		return true
	}
	return false
}

// blankConversationRouteHistory removes the oldest remaining history text,
// keeping its identity (message id, sequence, task) so the model still sees
// the reference.
func blankConversationRouteHistory(value *app.ConversationRouteContext) bool {
	for index := range value.History {
		if value.History[index].Content != "" {
			value.History[index].Content = ""
			value.History[index].Truncated = true
			value.HistoryIncomplete = true
			return true
		}
	}
	return false
}

func (w *ConversationRouteWorker) observeRouteDecision(claim app.ConversationRouteClaim, decision app.ConversationRouteDecision, result string) {
	if w.metrics == nil {
		return
	}
	action := "other"
	switch decision.Action {
	case app.ConversationRouteNew:
		action = "new"
		if len(decision.AggregateMessageIDs) > 0 {
			action = "aggregate"
		}
	case app.ConversationRouteJoin:
		action = "join"
	case app.ConversationRouteUpdate:
		action = "update"
	case app.ConversationRouteStatus:
		action = "status"
	case app.ConversationRouteStop:
		action = "stop"
	case app.ConversationRouteStopAll:
		action = "stop_all"
	case app.ConversationRouteClarify:
		action = "clarify"
	case app.ConversationRouteReply:
		action = "reply"
	case app.ConversationRouteTool:
		action = "tool"
	}
	w.metrics.actions.WithLabelValues(action, result).Inc()
	if claim.AggregationStartedAt.IsZero() || result != "applied" || claim.AggregationStartedAt.After(time.Now()) {
		return
	}
	w.metrics.aggregationWaitTime.Observe(time.Since(claim.AggregationStartedAt).Seconds())
	state := "resolved"
	switch action {
	case "aggregate":
		state = "aggregated"
	case "update", "join":
		state = "corrected"
	case "clarify":
		state = "clarified"
	}
	w.metrics.aggregationStates.WithLabelValues(state).Inc()
}

func copyConversationRouteCandidates(value *app.ConversationRouteContext) {
	value.Candidates = append(value.Candidates[:0:0], value.Candidates...)
	value.RequestCandidates = append(value.RequestCandidates[:0:0], value.RequestCandidates...)
	value.AggregationCandidates = append(value.AggregationCandidates[:0:0], value.AggregationCandidates...)
}

func conversationRouteRequestSupportsActiveTaskControl(candidate app.ConversationRouteRequestCandidate,
	value *app.ConversationRouteContext) bool {
	if candidate.TaskID == "" || candidate.TaskStatus != task.StatusRunning || !candidate.SameControlScope || !candidate.SameAuthorization ||
		candidate.RequestInput.ModelID != value.CurrentRequestModelID || candidate.RequestInput.TimeZone != value.TimeZone {
		return false
	}
	for _, active := range value.Candidates {
		if active.TaskID == candidate.TaskID && active.InitiatorID == value.ActorID &&
			active.Status == task.StatusRunning && active.Revision >= 0 {
			return true
		}
	}
	return false
}

func conversationRouteTaskHasActiveControlEvidence(candidate app.ConversationRouteCandidate,
	value *app.ConversationRouteContext) bool {
	if candidate.TaskID == "" || candidate.InitiatorID != value.ActorID || candidate.Status != task.StatusRunning {
		return false
	}
	for _, prior := range value.RequestCandidates {
		if prior.TaskID == candidate.TaskID && prior.TaskStatus == task.StatusRunning && prior.SameControlScope && prior.SameAuthorization &&
			prior.RequestInput.ModelID == value.CurrentRequestModelID && prior.RequestInput.TimeZone == value.TimeZone {
			return true
		}
	}
	return false
}

func trimConversationRouteCandidates(value *app.ConversationRouteContext) bool {
	for index := len(value.RequestCandidates) - 1; index >= 0; index-- {
		candidate := value.RequestCandidates[index]
		if candidate.MessageID == value.PreviousMessageID || conversationRouteRequestSupportsActiveTaskControl(candidate, value) {
			continue
		}
		value.RequestCandidates = append(value.RequestCandidates[:index], value.RequestCandidates[index+1:]...)
		value.CandidateSetIncomplete = true
		value.RequestCandidateSetIncomplete = true
		return true
	}
	for index := len(value.Candidates) - 1; index >= 0; index-- {
		candidate := value.Candidates[index]
		if candidate.TaskID == value.ReferenceTaskID || candidate.TaskID == value.PreviousTaskID ||
			conversationRouteTaskHasActiveControlEvidence(candidate, value) {
			continue
		}
		value.Candidates = append(value.Candidates[:index], value.Candidates[index+1:]...)
		value.CandidateSetIncomplete = true
		value.TaskCandidateSetIncomplete = true
		return true
	}
	if len(value.AggregationCandidates) > 0 {
		value.AggregationCandidates = value.AggregationCandidates[:len(value.AggregationCandidates)-1]
		value.CandidateSetIncomplete = true
		return true
	}
	for index := len(value.RequestCandidates) - 1; index >= 0; index-- {
		candidate := &value.RequestCandidates[index]
		if candidate.MessageID != value.PreviousMessageID {
			continue
		}
		content, shortened := shortenConversationRoutePreview(candidate.Content)
		if !shortened {
			continue
		}
		if !candidate.ContentTruncated {
			digest := sha256.Sum256([]byte(candidate.Content))
			candidate.ContentSHA256 = digest[:]
		}
		candidate.Content = content
		candidate.ContentTruncated = true
		value.CandidateSetIncomplete = true
		return true
	}
	for index := len(value.Candidates) - 1; index >= 0; index-- {
		candidate := &value.Candidates[index]
		if candidate.TaskID != value.ReferenceTaskID && candidate.TaskID != value.PreviousTaskID {
			continue
		}
		objective, shortened := shortenConversationRoutePreview(candidate.Objective)
		if !shortened {
			continue
		}
		candidate.Objective = objective
		candidate.ObjectiveTruncated = true
		value.CandidateSetIncomplete = true
		return true
	}
	return false
}

func shortenConversationRoutePreview(value string) (string, bool) {
	if len(value) <= 128 {
		return value, false
	}
	return app.ConversationRouteTextPreview(value, max(128, len(value)/2))
}

func (w *ConversationRouteWorker) failRoute(ctx context.Context, claim app.ConversationRouteClaim, reason string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fields := []zap.Field{
		zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
		zap.String("failure_reason", reason), zap.String("failure_code", string(domain.CodeOf(cause))),
		zap.String("failure_category", cloudDSHModelErrorCategory(cause)),
	}
	var routeErr *domain.Error
	if errors.As(cause, &routeErr) {
		if stage, ok := routeErr.Details["stage"].(string); ok {
			fields = append(fields, zap.String("failure_stage", stage))
		}
		if tokens, ok := routeErr.Details["context_window_tokens"].(int); ok {
			fields = append(fields, zap.Int("context_window_tokens", tokens))
		}
		if known, ok := routeErr.Details["context_window_known"].(bool); ok {
			fields = append(fields, zap.Bool("context_window_known", known))
		}
		if contentBytes, ok := routeErr.Details["content_bytes"].(int); ok {
			fields = append(fields, zap.Int("content_bytes", contentBytes))
		}
		if historyBytes, ok := routeErr.Details["history_bytes"].(int); ok {
			fields = append(fields, zap.Int("history_bytes", historyBytes))
		}
	}
	// The failover reason is a bounded 31-value label, never raw provider
	// text, so the terminal state stays locatable from the log alone.
	if classification := app.ClassifyModelClientError(cause); classification.Known {
		fields = append(fields, zap.String("failover_reason", string(classification.Reason)))
		if classification.Strategy.UserVisibleCase > 0 {
			fields = append(fields, zap.Int("user_visible_case", classification.Strategy.UserVisibleCase))
		}
	}
	w.options.Logger.Warn("conversation route failed", fields...)
	if err := w.options.Actions.FailConversationRoute(ctx, claim, reason); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func conversationRouteBudgetDiagnostic(err error, stage string, contextWindow int, capacityKnown bool, value app.ConversationRouteContext) error {
	var routeErr *domain.Error
	if !errors.As(err, &routeErr) {
		return err
	}
	if routeErr.Details == nil {
		routeErr.Details = make(map[string]any)
	}
	if _, exists := routeErr.Details["stage"]; !exists {
		routeErr.Details["stage"] = stage
	}
	routeErr.Details["context_window_tokens"] = contextWindow
	routeErr.Details["context_window_known"] = capacityKnown
	contentBytes, historyBytes := conversationRouteContextSizeDetails(value)
	routeErr.Details["content_bytes"] = contentBytes
	routeErr.Details["history_bytes"] = historyBytes
	return err
}

// conversationRouteContextSizeDetails records the byte sizes that decide
// whether a route context fits, so a size failure can be diagnosed from the
// log alone instead of re-reading the database.
func conversationRouteContextSizeDetails(value app.ConversationRouteContext) (int, int) {
	historyBytes := 0
	for _, item := range value.History {
		historyBytes += len(item.Content)
	}
	return len(value.Content), historyBytes
}

func safeConversationRouteDecision(value app.ConversationRouteContext, decision app.ConversationRouteDecision) app.ConversationRouteDecision {
	clarify := app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
		Question: "我还不能安全确认这项操作的目标或范围，因此尚未执行。请补充具体目标，或说明要处理哪项任务。"}
	clarify.ModelOutput = decision.ModelOutput
	if !app.ValidConversationRouteModelOutput(decision.ModelOutput) ||
		!app.ValidConversationRouteFileDeliveryIntent(decision.Action, decision.FileDeliveryIntent) ||
		decision.Action != app.ConversationRouteReply && decision.Reply != "" ||
		decision.Action != app.ConversationRouteTool && (decision.ToolName != "" || decision.ToolArguments != "") {
		return clarify
	}
	if (decision.Action == app.ConversationRouteStop || decision.Action == app.ConversationRouteStopAll) &&
		!app.ValidConversationRouteStopDecision(value, decision) {
		if value.ReferenceUnavailable {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: app.UnavailableRouteReferenceMessage, ModelOutput: decision.ModelOutput}
		}
		if value.PreviousMissing {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。", ModelOutput: decision.ModelOutput}
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "我无法安全确认要停止的任务，请重新说明停止范围。", ModelOutput: decision.ModelOutput}
	}
	if value.ContentTruncated && decision.Action != app.ConversationRouteNew && decision.Action != app.ConversationRouteClarify {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question:    "这条消息的完整原文尚未用于判断，暂不能安全关联到已有任务。请明确它是一项新工作，还是要修改/停止哪项任务。",
			ModelOutput: decision.ModelOutput}
	}
	if value.PreviousMissing && decision.Action != app.ConversationRouteClarify &&
		((decision.Action != app.ConversationRouteNew && decision.Action != app.ConversationRouteReply) || strings.TrimSpace(value.Content) == "") {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。",
			ModelOutput: decision.ModelOutput}
	}
	switch decision.Action {
	case app.ConversationRouteStopAll:
		if app.ConversationRouteCanClassifyStopAll(value) && decision.TaskID == "" && decision.TargetMessageID == "" &&
			!decision.Acknowledge && !decision.ExplicitNew && decision.ExpectedRevision == 0 && decision.Question == "" && decision.Reply == "" &&
			len(decision.DependencyTaskIDs) == 0 && len(decision.AggregateMessageIDs) == 0 {
			return decision
		}
		return clarify
	case app.ConversationRouteNew:
		if decision.TaskID == "" && decision.TargetMessageID == "" &&
			decision.ExpectedRevision == 0 && decision.Question == "" &&
			(!decision.ExplicitNew || len(decision.AggregateMessageIDs) == 0) &&
			app.ValidConversationRouteDependencyIDs(decision.DependencyTaskIDs, value.Candidates) &&
			app.ValidConversationRouteAggregateIDs(decision.AggregateMessageIDs, value.AggregationCandidates) {
			return decision
		}
	case app.ConversationRouteJoin:
		if decision.TaskID == "" && decision.TargetMessageID != "" && !decision.ExplicitNew && decision.ExpectedRevision == 0 &&
			decision.Question == "" && decision.Reply == "" && len(decision.DependencyTaskIDs) == 0 &&
			len(decision.AggregateMessageIDs) == 0 &&
			app.ValidConversationRouteJoinTarget(decision.TargetMessageID, value.RequestCandidates) {
			return decision
		}
	case app.ConversationRouteReply:
		if !decision.Acknowledge && decision.TaskID == "" && decision.TargetMessageID == "" && !decision.ExplicitNew && decision.ExpectedRevision == 0 && decision.Question == "" &&
			len(decision.DependencyTaskIDs) == 0 && len(decision.AggregateMessageIDs) == 0 && app.ValidConversationRouteReply(decision.Reply) {
			return decision
		}
	case app.ConversationRouteTool:
		if !decision.Acknowledge && decision.TaskID == "" && decision.TargetMessageID == "" && !decision.ExplicitNew &&
			decision.ExpectedRevision == 0 && decision.Question == "" && decision.Reply == "" &&
			len(decision.DependencyTaskIDs) == 0 && len(decision.AggregateMessageIDs) == 0 &&
			app.ConversationRouteDirectToolContextEligible(value) &&
			app.ValidConversationRouteDirectToolCall(decision.ToolName, json.RawMessage(decision.ToolArguments)) {
			return decision
		}
	case app.ConversationRouteSteer:
		if decision.Acknowledge || decision.TaskID == "" || decision.TargetMessageID != "" || decision.ExplicitNew || decision.Question != "" ||
			decision.ExpectedRevision < 0 || len(decision.DependencyTaskIDs) != 0 || len(decision.AggregateMessageIDs) != 0 ||
			!app.ValidConversationRouteSteerTarget(decision.TaskID, value) {
			return clarify
		}
		for _, candidate := range value.Candidates {
			if candidate.TaskID == decision.TaskID && candidate.InitiatorID == value.ActorID && candidate.Status == task.StatusRunning &&
				candidate.Revision == decision.ExpectedRevision {
				return decision
			}
		}
	case app.ConversationRouteClarify:
		if !decision.Acknowledge && decision.TaskID == "" && decision.TargetMessageID == "" && !decision.ExplicitNew && decision.ExpectedRevision == 0 &&
			len(decision.AggregateMessageIDs) == 0 && strings.TrimSpace(decision.Question) != "" && utf8.ValidString(decision.Question) &&
			!strings.ContainsRune(decision.Question, '\x00') && len([]rune(decision.Question)) <= 200 {
			return decision
		}
	case app.ConversationRouteUpdate, app.ConversationRouteStatus, app.ConversationRouteStop:
		if decision.Action != app.ConversationRouteUpdate && decision.Acknowledge || decision.ExplicitNew ||
			decision.Question != "" || decision.Reply != "" || len(decision.AggregateMessageIDs) != 0 {
			return clarify
		}
		if decision.Action == app.ConversationRouteStop && decision.TargetMessageID != "" {
			if decision.TaskID == "" && decision.ExpectedRevision == 0 &&
				app.ValidConversationRoutePendingRootStop(decision.TargetMessageID, value.RequestCandidates) {
				return decision
			}
			return clarify
		}
		if decision.TargetMessageID != "" {
			return clarify
		}
		if value.ReferenceUnavailable {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: app.UnavailableRouteReferenceMessage}
		}
		for _, candidate := range value.Candidates {
			if candidate.TaskID == decision.TaskID && candidate.InitiatorID == value.ActorID && candidate.Revision == decision.ExpectedRevision {
				if value.ReferenceTaskID != "" && value.ReferenceTaskID != decision.TaskID {
					return clarify
				}
				return decision
			}
		}
	}
	return clarify
}
