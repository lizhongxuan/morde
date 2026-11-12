package worker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
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
}

type ConversationRouteWorker struct {
	options ConversationRouteWorkerOptions
}

func NewConversationRouteWorker(options ConversationRouteWorkerOptions) (*ConversationRouteWorker, error) {
	if options.Queue == nil || options.Actions == nil || options.Models == nil || options.WorkerID == "" || strings.TrimSpace(options.WorkerID) != options.WorkerID ||
		len(options.WorkerID) > 128 || options.LeaseDuration < time.Minute || options.LeaseDuration > 5*time.Minute ||
		options.LeaseDuration%time.Second != 0 || options.PollInterval <= 0 || options.MaxConcurrent < 1 || options.MaxConcurrent > 16 ||
		options.Logger == nil || (options.SummaryStore == nil) != (options.SummaryInputs == nil) ||
		(options.SummaryStore != nil && options.SummarySealer == nil) {
		return nil, errors.New("conversation routing dependencies and bounded worker options are required")
	}
	return &ConversationRouteWorker{options: options}, nil
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
// context conflicts are durable; an absent predecessor is deferred without guessing.
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
	if len(snapshot.Context.Content) > app.MaxConversationAgentInputBytes {
		return true, w.failRoute(ctx, claim, "message_too_long", errors.New("message exceeds the model input budget"))
	}
	if snapshot.PreviousCycle {
		return true, w.options.Actions.ApplyConversationRoute(routeCtx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteClarify, Question: "消息顺序存在循环，请重新发送。",
		})
	}
	if snapshot.PreviousSequence > claim.Message.Sequence && snapshot.Context.PreviousUnresolved && snapshot.PreviousRoutingState != "waiting_adjustment" {
		return true, w.options.Actions.DeferConversationRoute(routeCtx, claim, 2*time.Second)
	}
	modelID, err := w.options.Models.ResolveConversationRouteModel(routeCtx, claim)
	if err != nil || !domain.ValidModelConnectionID(modelID) {
		if err == nil {
			err = errors.New("resolved conversation model identity is invalid")
		}
		return true, w.failRoute(ctx, claim, "model_unavailable", err)
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
	classifyContext.ModelID = modelID
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteNew}
	if classifyContext.Content != "" || len(classifyContext.Candidates) != 0 || classifyContext.ReferenceTaskID != "" ||
		classifyContext.PreviousUnresolved || classifyContext.SummaryVersion > 0 {
		if _, err := app.ConversationRouteModelRequest(classifyContext); domain.CodeOf(err) == domain.CodeResultTooLarge && classifyContext.SummaryVersion > 0 {
			classifyContext = snapshot.Context
			classifyContext.ModelID = modelID
			classifyContext.SummaryVersion = 0
			classifyContext.SummarySHA256, classifyContext.SummarySourcesSHA256 = nil, nil
			classifyContext.SummaryText = ""
		}
		if _, err := app.ConversationRouteModelRequest(classifyContext); domain.CodeOf(err) == domain.CodeResultTooLarge {
			return true, w.failRoute(ctx, claim, "message_too_long", err)
		}
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
			if !snapshot.PreviousMissing && domain.CodeOf(err) == domain.CodeConflict && ctx.Err() == nil {
				// Surface stale context to the existing retry action instead of leaving the accepted message pending forever.
				return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
			}
			return true, w.failRoute(ctx, claim, "model_unavailable", err)
		}
	}
	decision = safeConversationRouteDecision(snapshot.Context, decision)
	if snapshot.Context.ResultReview != nil && decision.Action != app.ConversationRouteReply {
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", errors.New("task result review did not produce a reply"))
	}
	if snapshot.PreviousMissing && decision.Action != app.ConversationRouteNew {
		decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "前一条消息尚未确认，请先处理或重试。"}
	}
	if err := w.options.Actions.ApplyConversationRoute(routeCtx, claim, snapshot, decision); err != nil {
		if domain.CodeOf(err) == domain.CodeConflict && (claim.Input.ResultReviewID != "" || routeCtx.Err() == nil) {
			return true, w.failRoute(ctx, claim, "routing_context_unavailable", err)
		}
		if domain.CodeOf(err) == domain.CodeConflict || routeCtx.Err() != nil {
			return true, err
		}
		return true, w.failRoute(ctx, claim, "routing_action_unavailable", err)
	}
	return true, nil
}

func (w *ConversationRouteWorker) failRoute(ctx context.Context, claim app.ConversationRouteClaim, reason string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
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
			return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "前一条消息尚未确认，请先处理或重试。",
				ModelOutput: decision.ModelOutput}
		}
	}
	if value.PreviousMissing && (decision.Action != app.ConversationRouteNew || strings.TrimSpace(value.Content) == "") {
		return app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "前一条消息尚未确认，请先处理或重试。",
			ModelOutput: decision.ModelOutput}
	}
	switch decision.Action {
	case app.ConversationRouteNew:
		if decision.TaskID == "" && decision.ExpectedRevision == 0 && decision.Question == "" &&
			app.ValidConversationRouteDependencyIDs(decision.DependencyTaskIDs, value.Candidates) {
			return decision
		}
	case app.ConversationRouteReply:
		if decision.TaskID == "" && decision.ExpectedRevision == 0 && decision.Question == "" &&
			len(decision.DependencyTaskIDs) == 0 && app.ValidConversationRouteReply(decision.Reply) {
			return decision
		}
	case app.ConversationRouteClarify:
		if decision.TaskID == "" && decision.ExpectedRevision == 0 && strings.TrimSpace(decision.Question) != "" && utf8.ValidString(decision.Question) &&
			!strings.ContainsRune(decision.Question, '\x00') && len([]rune(decision.Question)) <= 200 {
			return decision
		}
	case app.ConversationRouteUpdate, app.ConversationRouteStatus, app.ConversationRouteStop:
		if decision.Question != "" {
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
