package worker

import (
	"bytes"
	"context"
	"errors"
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
	if len(snapshot.Context.Content) > app.MaxConversationAgentInputBytes {
		return true, w.failRoute(ctx, claim, "message_too_long", errors.New("message exceeds the model input budget"))
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
			w.options.Logger.Warn("conversation summary compaction skipped", zap.String("job", "conversation_route"))
		} else if compacted {
			snapshot, err = w.options.Actions.LoadConversationRouteSnapshot(routeCtx, claim)
			if err != nil {
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
			}
		}
	}
	snapshot.Context.ModelID = modelID
	classifyContext := snapshot.Context
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
	copyConversationRouteCandidates(&classifyContext)
	if err := fitConversationRouteContext(&classifyContext, contextWindow); err != nil {
		return true, w.failRoute(ctx, claim, "message_too_long", err)
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
				return true, w.failRoute(ctx, claim, "message_too_long", err)
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
			return true, w.failRoute(ctx, claim, "model_unavailable", err)
		}
	}
	if decision.InvalidOutput {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("route model output is invalid"))
	}
	decision = safeConversationRouteDecision(classifyContext, decision)
	if decision.InvalidOutput {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("route model output is invalid for the current context"))
	}
	if snapshot.Context.ResultReview != nil && decision.Action != app.ConversationRouteReply {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("task result review did not produce a reply"))
	}
	if decision.Action == app.ConversationRouteNew {
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

func fitConversationRouteContext(value *app.ConversationRouteContext, contextWindow int) error {
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
		if value.SummaryVersion > 0 {
			value.SummaryVersion = 0
			value.SummarySHA256, value.SummarySourcesSHA256, value.SummaryText = nil, nil, ""
			value.HistoryIncomplete = true
			continue
		}
		trimmedHistory := false
		for index := range value.History {
			if value.History[index].Content != "" {
				value.History[index].Content = ""
				value.History[index].Truncated = true
				value.HistoryIncomplete = true
				trimmedHistory = true
				break
			}
		}
		if trimmedHistory {
			continue
		}
		if err != nil {
			return err
		}
		return domain.NewError(domain.CodeResultTooLarge, "conversation message exceeds the selected model context budget")
	}
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
	case app.ConversationRouteClarify:
		action = "clarify"
	case app.ConversationRouteReply:
		action = "reply"
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

func trimConversationRouteCandidates(value *app.ConversationRouteContext) bool {
	for index := len(value.RequestCandidates) - 1; index >= 0; index-- {
		if value.RequestCandidates[index].MessageID == value.PreviousMessageID {
			continue
		}
		value.RequestCandidates = append(value.RequestCandidates[:index], value.RequestCandidates[index+1:]...)
		value.CandidateSetIncomplete = true
		return true
	}
	for index := len(value.Candidates) - 1; index >= 0; index-- {
		taskID := value.Candidates[index].TaskID
		if taskID == value.ReferenceTaskID || taskID == value.PreviousTaskID {
			continue
		}
		value.Candidates = append(value.Candidates[:index], value.Candidates[index+1:]...)
		value.CandidateSetIncomplete = true
		return true
	}
	if len(value.AggregationCandidates) > 0 {
		value.AggregationCandidates = value.AggregationCandidates[:len(value.AggregationCandidates)-1]
		value.CandidateSetIncomplete = true
		return true
	}
	return false
}

func (w *ConversationRouteWorker) failRoute(ctx context.Context, claim app.ConversationRouteClaim, reason string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.options.Logger.Warn("conversation route failed",
		zap.String("job", "conversation_route"), zap.String("message_id", claim.Message.ID),
		zap.String("failure_reason", reason), zap.String("failure_code", string(domain.CodeOf(cause))),
		zap.String("failure_category", cloudDSHModelErrorCategory(cause)))
	if err := w.options.Actions.FailConversationRoute(ctx, claim, reason); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func safeConversationRouteDecision(value app.ConversationRouteContext, decision app.ConversationRouteDecision) app.ConversationRouteDecision {
	clarify := app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "请说明要修改哪个任务，或是否开始一个新任务。"}
	clarify.ModelOutput = decision.ModelOutput
	if !app.ValidConversationRouteModelOutput(decision.ModelOutput) ||
		decision.Action != app.ConversationRouteReply && decision.Reply != "" {
		return clarify
	}
	if (decision.Action == app.ConversationRouteNew || decision.Action == app.ConversationRouteReply) &&
		app.ConversationRouteHasExplicitControlIntent(value.Content) {
		if value.ReferenceUnavailable {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: app.UnavailableRouteReferenceMessage,
				ModelOutput: decision.ModelOutput}
		}
		if value.PreviousMissing {
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "我无法确认你要操作哪项任务。请指定任务，或重新描述操作。",
				ModelOutput: decision.ModelOutput}
		}
	}
	if value.PreviousMissing && decision.Action != app.ConversationRouteClarify &&
		((decision.Action != app.ConversationRouteNew && decision.Action != app.ConversationRouteReply) || strings.TrimSpace(value.Content) == "") {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "我无法确认这条消息依赖的上下文。请补充相关内容，或把请求完整重发。",
			ModelOutput: decision.ModelOutput}
	}
	if value.CandidateSetIncomplete && app.ConversationRouteHasExplicitControlIntent(value.Content) &&
		(decision.Action == app.ConversationRouteNew || decision.Action == app.ConversationRouteReply) {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
			Question: "当前未能确认你要操作的任务。请指出任务，或重新描述一项独立工作。", ModelOutput: decision.ModelOutput}
	}
	switch decision.Action {
	case app.ConversationRouteNew:
		if decision.TaskID == "" && decision.TargetMessageID == "" && (!decision.ExplicitNew || app.ConversationRouteHasExplicitNewIntent(value.Content)) &&
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
