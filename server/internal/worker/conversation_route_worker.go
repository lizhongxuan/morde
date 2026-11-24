package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
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
			// Classification failed after every model-side recovery. Fall
			// back to the keyword + last-route heuristic instead of
			// failing the route: follow the previous task on a clear
			// continuation, start a new task on an explicit new-task
			// signal, and ask the user when uncertain. An ambiguous
			// message is never silently dropped into a task.
			w.options.Logger.Warn("conversation route classification failed; applying heuristic fallback",
				zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
				zap.String("failure_code", string(domain.CodeOf(err))))
			decision = conversationRouteClassifyFallback(classifyContext)
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
		w.options.Logger.Warn("conversation route model output is invalid; applying heuristic fallback",
			zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
			zap.String("failure_code", "invalid_model_output"),
			zap.Int("invalid_output_length_bytes", decision.InvalidOutputLengthBytes),
			zap.Bool("invalid_output_utf8_valid", decision.InvalidOutputUTF8Valid))
		decision = conversationRouteClassifyFallback(classifyContext)
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

func conversationRouteRateLimitDelay(claim app.ConversationRouteClaim, err error) (time.Duration, bool) {
	var routeErr *domain.Error
	if !errors.As(err, &routeErr) || routeErr.Code != domain.CodeRateLimited ||
		routeErr.Details["reason"] != string(app.ModelErrorRateLimited) || routeErr.Details["stage"] != "provider_status" {
		return 0, false
	}
	if routeErr.Details["retry_after_exceeds_durable_horizon"] == "true" || routeErr.RetryAfter > 24*time.Hour {
		// The durable route queue supports at most 24 hours; do not retry earlier than the provider requested.
		return 0, true
	}
	if routeErr.RetryAfter > 0 {
		retryAfter := routeErr.RetryAfter
		delay := retryAfter / time.Second
		if retryAfter%time.Second != 0 {
			delay++
		}
		return delay * time.Second, true
	}
	shift := min(max(claim.RouteAttempt-1, int64(0)), int64(2))
	return time.Second << shift, true
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
	clarify := app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请说明要修改哪个任务，或是否开始一个新任务。"}
	clarify.ModelOutput = decision.ModelOutput
	if !app.ValidConversationRouteModelOutput(decision.ModelOutput) ||
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
		if decision.TaskID == "" && decision.TargetMessageID == "" && (!decision.ExplicitNew || app.ConversationRouteHasExplicitNewIntentForContext(value)) &&
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
			app.ConversationRouteHasHighRiskModificationIntent(value.Content) ||
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

// conversationRouteClassifyFallback safely routes a message whose model
// classification failed. It never issues a stop action: task control must be
// semantically classified by the model, and uncertain control requests are
// clarified. Existing narrow fallbacks for independent work and safe
// continuation remain bounded by the route snapshot.
func conversationRouteClassifyFallback(value app.ConversationRouteContext) app.ConversationRouteDecision {
	if app.ConversationRouteHasExplicitControlIntentForContext(value) {
		if value.ReferenceUnavailable {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: app.UnavailableRouteReferenceMessage}
		}
		if app.ConversationRouteHasExplicitPauseIntentForContext(value) {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "当前不能将暂停作为停止执行。请明确是否要停止该任务。"}
		}
		if app.ConversationRouteHasExplicitNewIntentForContext(value) {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "这条消息同时包含任务操作和新任务，请分别说明目标和要执行的工作。"}
		}
		if action, ok := conversationRouteExplicitControlFallbackAction(value.Content); ok &&
			!app.ConversationRouteHasHighRiskModificationIntent(value.Content) {
			if candidate, found := conversationRouteExplicitControlFallbackTarget(value); found {
				return app.ConversationRouteDecision{Action: action, TaskID: candidate.TaskID, ExpectedRevision: candidate.Revision}
			}
		}
		if app.ConversationRouteHasHighRiskModificationIntent(value.Content) {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "这条请求包含可能改变任务计划、目标或权限的内容。我还不能安全确认具体改动，请把要修改的部分说明清楚。"}
		}
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "我无法安全确认这条任务操作的目标和范围，因此没有执行操作。请明确要操作的任务，或确认是否要停止全部活动任务。"}
	}
	if (len(value.Candidates) != 0 || value.ReferenceTaskID != "") &&
		app.ConversationRouteHasHighRiskModificationIntent(value.Content) {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "这条请求包含可能改变任务计划、目标或权限的内容。我还不能安全确认具体改动，请把要修改的部分说明清楚。"}
	}
	if decision, ok := app.ConversationRouteLongMessageFallback(value); ok {
		return decision
	}
	if value.ResultReview == nil && !value.PreviousMissing && !value.PreviousUnresolved && !value.ReferenceUnavailable &&
		!value.CandidateSetIncomplete && !value.TaskCandidateSetIncomplete && !value.RequestCandidateSetIncomplete &&
		!value.ContentTruncated && len(value.ContentSources) == 0 && !app.ConversationRouteHasTaskDependencyIntent(value.Content) &&
		app.ConversationRouteHasExplicitNewIntentForContext(value) {
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}
	}
	if app.ConversationRouteIndependentFallbackEligible(value) &&
		len(value.Candidates) == 0 && app.ConversationRouteHasExplicitSearchIntent(value.Content) {
		return app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	}
	if app.ConversationRouteIndependentFallbackEligible(value) {
		if intent, explicit := app.RoutedFileDeliveryDirective(value.Content, false); explicit && intent == task.FileDeliveryRequired {
			return app.ConversationRouteDecision{Action: app.ConversationRouteNew}
		}
		if app.ConversationRouteHasExplicitTaskWorkIntent(value) {
			return app.ConversationRouteDecision{Action: app.ConversationRouteNew}
		}
	}
	target := value.ReferenceTaskID
	if target == "" {
		target = value.PreviousTaskID
	}
	if target != "" && app.ValidConversationRouteSteerTarget(target, value) &&
		conversationRouteLooksLikeContinuation(value.Content) {
		for _, candidate := range value.Candidates {
			if candidate.TaskID == target {
				return app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: target,
					ExpectedRevision: candidate.Revision}
			}
		}
	}
	// When the previous message itself is missing, do not offer to continue
	// it: ask the user to supply the missing context instead. This reuses
	// the wording the safety check uses for the same state. A predecessor
	// that merely ended unresolved (for example waiting_user after a
	// clarification was delivered) is known state, not missing context,
	// so it falls through to the continue-or-new question below instead
	// of demanding the context be resupplied.
	if value.PreviousMissing {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。"}
	}
	if value.PreviousUnresolved {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "上一条请求还没有完成；当前没有执行任务操作。你希望把这条消息作为该任务的补充，还是开始一项独立工作？"}
	}
	if len(value.Candidates) == 0 {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "我还不能确定你希望我直接回答，还是开始一项需要执行的工作；当前没有执行任务操作。请补充具体目标。"}
	}
	return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
		Question: "我还不能确定这条请求是要接续哪项任务，还是作为独立工作开始；当前没有执行任务操作。请告诉我目标任务，或补充独立工作的目标。"}
}

func conversationRouteExplicitControlFallbackAction(content string) (app.ConversationRouteAction, bool) {
	var normalized strings.Builder
	for _, char := range strings.ToLower(content) {
		if unicode.IsSpace(char) || unicode.IsPunct(char) || unicode.IsSymbol(char) {
			continue
		}
		normalized.WriteRune(char)
	}
	value := normalized.String()
	preambles := []string{"麻烦你", "请帮我", "请帮忙", "帮我", "麻烦", "请你", "请", "你能不能", "你能否", "能不能", "能否", "现在", "然后", "please", "couldyou"}
	for {
		stripped := false
		for _, preamble := range preambles {
			if strings.HasPrefix(value, preamble) {
				value = strings.TrimPrefix(value, preamble)
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	if value == "" {
		return "", false
	}
	var actions []app.ConversationRouteAction
	stopVerbs := []string{"停止", "停掉", "取消", "撤销", "终止", "stop", "cancel", "terminate", "abort"}
	continueVerbs := []string{"继续", "恢复", "接着", "续接", "continue", "resume"}
	statusQueries := []string{"查看", "查询", "查下", "查一下", "看下", "看一下", "检查", "告诉", "汇报", "show", "check", "get", "whatisthe"}
	if hasRoutePrefix(value, continueVerbs) {
		actions = append(actions, app.ConversationRouteUpdate)
	}
	if hasRoutePrefix(value, statusQueries) && containsRouteIntent(value, []string{"任务", "进度", "状态", "运行到哪", "task", "progress", "status"}) {
		actions = append(actions, app.ConversationRouteStatus)
	}
	if len(actions) != 1 {
		return "", false
	}
	for _, verb := range append(append(stopVerbs, continueVerbs...), statusQueries...) {
		if strings.HasPrefix(value, verb) && app.ConversationRouteHasExplicitControlIntent(strings.TrimPrefix(value, verb)) {
			return "", false
		}
	}
	return actions[0], true
}

func conversationRouteExplicitControlFallbackTarget(value app.ConversationRouteContext) (app.ConversationRouteCandidate, bool) {
	if value.ContentTruncated || value.ResultReview != nil || value.PreviousMissing || value.ReferenceUnavailable ||
		value.CurrentMessageHasAttachments ||
		(value.CandidateSetIncomplete || value.TaskCandidateSetIncomplete || value.RequestCandidateSetIncomplete) && value.ReferenceTaskID == "" {
		return app.ConversationRouteCandidate{}, false
	}
	active := make([]app.ConversationRouteCandidate, 0, len(value.Candidates))
	for _, candidate := range value.Candidates {
		if candidate.InitiatorID == value.ActorID && candidate.Revision >= 0 && activeConversationRouteCandidate(candidate.Status) {
			active = append(active, candidate)
		}
	}
	mentionedIDs := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(value.Content), func(char rune) bool {
		return !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9')
	}) {
		if domain.ValidTaskID(word) {
			mentionedIDs[word] = true
		}
	}
	var exactMatches []app.ConversationRouteCandidate
	for _, candidate := range active {
		if mentionedIDs[candidate.TaskID] || !candidate.ObjectiveTruncated && candidate.Objective != "" && strings.Contains(value.Content, candidate.Objective) {
			exactMatches = append(exactMatches, candidate)
			delete(mentionedIDs, candidate.TaskID)
		}
	}
	if len(mentionedIDs) != 0 || len(exactMatches) > 1 {
		return app.ConversationRouteCandidate{}, false
	}
	if len(exactMatches) == 1 && value.ReferenceTaskID != "" && value.ReferenceTaskID != exactMatches[0].TaskID {
		return app.ConversationRouteCandidate{}, false
	}
	if len(exactMatches) == 1 {
		return exactMatches[0], true
	}
	if !conversationRouteReferencesCurrentTask(value.Content) {
		return app.ConversationRouteCandidate{}, false
	}
	targetID := value.ReferenceTaskID
	if targetID == "" {
		targetID = value.PreviousTaskID
	}
	if targetID != "" {
		var target app.ConversationRouteCandidate
		count := 0
		for _, candidate := range active {
			if candidate.TaskID == targetID {
				target = candidate
				count++
			}
		}
		return target, count == 1
	}
	if !value.TaskCandidateSetIncomplete && !value.CandidateSetIncomplete && len(active) == 1 {
		return active[0], true
	}
	return app.ConversationRouteCandidate{}, false
}

func conversationRouteReferencesCurrentTask(content string) bool {
	var normalized strings.Builder
	for _, char := range strings.ToLower(content) {
		if !unicode.IsSpace(char) && !unicode.IsPunct(char) && !unicode.IsSymbol(char) {
			normalized.WriteRune(char)
		}
	}
	value := normalized.String()
	if containsRouteIntent(value, []string{"当前任务", "这个任务", "这项任务", "该任务", "此任务", "上个任务", "上一个任务", "现有任务", "原任务", "停止它", "继续它", "currenttask", "thistask", "thattask", "previoustask"}) {
		return true
	}
	for _, prefix := range []string{"请帮我", "麻烦你", "请你", "帮我", "麻烦", "请", "please"} {
		value = strings.TrimPrefix(value, prefix)
	}
	switch value {
	case "停止", "停止任务", "取消", "取消任务", "暂停", "暂停任务", "继续", "继续任务", "恢复", "恢复任务", "查看任务状态", "查询任务状态", "查看任务进度", "查询任务进度", "stop", "stopthetask", "cancel", "continue", "resume":
		return true
	default:
		return false
	}
}

func hasRoutePrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func containsRouteIntent(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func activeConversationRouteCandidate(status task.Status) bool {
	switch status {
	case task.StatusQueued, task.StatusRunning, task.StatusWaitingUser, task.StatusPausing, task.StatusPaused, task.StatusCancelling:
		return true
	default:
		return false
	}
}

// conversationRouteLooksLikeContinuation reports whether the message text
// reads as an addition or correction to the previous task rather than an
// independent request. Only narrow, high-precision signals count: a bare
// acknowledgement, or a leading addition/correction marker. Anything else
// is treated as uncertain and ends in a clarification question.
func conversationRouteLooksLikeContinuation(content string) bool {
	if app.ConversationRouteHasHighRiskModificationIntent(content) {
		return false
	}
	var normalized strings.Builder
	for _, char := range strings.ToLower(content) {
		if unicode.IsSpace(char) || unicode.IsPunct(char) || unicode.IsSymbol(char) {
			continue
		}
		normalized.WriteRune(char)
	}
	value := normalized.String()
	if value == "" {
		return false
	}
	switch value {
	case "好", "好的", "可以", "行", "嗯", "对", "就这样", "没问题", "ok", "yes", "接着":
		return true
	}
	for _, marker := range []string{"补充", "追加", "再加", "加上", "改成", "换成", "调整一下", "修改一下"} {
		if strings.HasPrefix(value, marker) {
			return true
		}
	}
	return false
}
