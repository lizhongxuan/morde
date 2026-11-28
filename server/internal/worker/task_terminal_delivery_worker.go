package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type TaskTerminalDeliveryModelGateway interface {
	ResolveTaskTerminalDeliveryModel(context.Context, app.TaskTerminalDeliveryModelFence) (string, error)
	CompleteTaskTerminalDeliveryDSHModel(context.Context, app.TaskTerminalDeliveryModelFence, string,
		json.RawMessage) (app.ModelResponse, app.TaskTerminalDeliveryModelCallDisposition, error)
	RecoverTaskTerminalDeliveryDSHModelResponse(context.Context, app.TaskTerminalDeliveryModelFence,
		json.RawMessage) (app.ModelResponse, bool, error)
}

type TaskTerminalDeliveryWorkerOptions struct {
	Store                app.TaskTerminalDeliveryStore
	ObservabilityContext app.TaskObservabilityContextReader
	TracerProvider       trace.TracerProvider
	Models               TaskTerminalDeliveryModelGateway
	ReplySealer          *app.Sealer
	WorkerID             string
	LeaseDuration        time.Duration
	PollInterval         time.Duration
	Logger               *zap.Logger
}

type TaskTerminalDeliveryWorker struct {
	options TaskTerminalDeliveryWorkerOptions
}

func NewTaskTerminalDeliveryWorker(options TaskTerminalDeliveryWorkerOptions) (*TaskTerminalDeliveryWorker, error) {
	if options.Store == nil || options.Models == nil || options.ReplySealer == nil ||
		strings.TrimSpace(options.WorkerID) != options.WorkerID || options.WorkerID == "" || len(options.WorkerID) > 128 ||
		options.LeaseDuration < time.Second || options.LeaseDuration > 5*time.Minute || options.LeaseDuration%time.Second != 0 ||
		options.PollInterval <= 0 || options.Logger == nil {
		return nil, errors.New("task terminal delivery worker dependencies and bounds are required")
	}
	return &TaskTerminalDeliveryWorker{options: options}, nil
}

func (w *TaskTerminalDeliveryWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("task terminal delivery worker context is required")
	}
	for ctx.Err() == nil {
		found, err := w.RunOnce(ctx)
		if found && err == nil {
			continue
		}
		timer := time.NewTimer(w.options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// RunOnce first publishes durable replies left by a previous process. It only
// claims another source when no reply-ready candidate needs recovery.
func (w *TaskTerminalDeliveryWorker) RunOnce(ctx context.Context) (worked bool, resultErr error) {
	if ctx == nil {
		return false, errors.New("task terminal delivery worker context is required")
	}
	scope := w.startPollLogScope(ctx)
	defer func() { scope.finish(resultErr) }()
	ready, err := w.options.Store.ListTaskTerminalDeliveryReplyReady(ctx, 1)
	if err != nil {
		scope.logger.Error("task terminal delivery reply lookup failed", zap.String("stage", "list_reply_ready"),
			zap.String("error_type", typeName(err)))
		return false, err
	}
	if len(ready) != 0 {
		scope.logger.Info("task terminal delivery reply is ready for publication", zap.String("stage", "publish_ready"),
			zap.String("status", "ready"))
		return true, w.publishReady(ctx, ready[0])
	}
	claims, err := w.options.Store.ClaimTaskTerminalDeliverySources(ctx, w.options.WorkerID, w.options.LeaseDuration, 1)
	if err != nil {
		scope.logger.Error("task terminal delivery source claim failed", zap.String("stage", "claim_source"),
			zap.String("error_type", typeName(err)))
		return false, err
	}
	if len(claims) == 0 {
		return false, nil
	}
	return true, w.deliverClaim(ctx, claims[0])
}

func (w *TaskTerminalDeliveryWorker) deliverClaim(ctx context.Context, claim app.TaskTerminalDeliveryClaim) (resultErr error) {
	scope := w.startDeliveryLogScope(ctx, claim, "deliver")
	defer func() { scope.finish(resultErr) }()
	logger := scope.logger
	stage := "validate_claim"
	defer func() {
		if resultErr != nil {
			logger.Error("task terminal delivery processing failed", zap.String("stage", stage),
				zap.String("failure_code", string(domain.CodeOf(resultErr))),
				zap.String("error_type", typeName(resultErr)))
		}
	}()
	if !app.ValidTaskTerminalDeliveryClaim(claim) || claim.State != app.TaskTerminalDeliveryStateClaimed {
		return errors.New("task terminal delivery claim is invalid")
	}
	logger.Info("task terminal delivery processing started", zap.String("stage", stage), zap.String("status", claim.State))
	workCtx, cancel := context.WithDeadline(scope.ctx, claim.LeaseExpiresAt.Add(-time.Second))
	defer cancel()
	if err := workCtx.Err(); err != nil {
		return err
	}
	stage = "read_source_facts"
	facts, found, err := w.options.Store.ReadTaskTerminalDeliverySourceFacts(workCtx, claim)
	if err != nil || !found {
		if err == nil {
			logger.Warn("task terminal delivery source facts are missing", zap.String("stage", stage), zap.String("status", "missing"))
		}
		return err
	}
	if !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
		return errors.New("verified Task terminal delivery source facts are invalid")
	}
	logger.Info("task terminal delivery source facts loaded", zap.String("stage", stage), zap.String("status", "verified"))
	// Recovery uses the Task's persisted model ID so a settled call can be
	// reconstructed even if current model configuration has changed.
	stage = "build_model_request"
	recoveryRequest, err := app.BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
	if err != nil {
		return err
	}
	fence, err := claim.ModelDispatchFence()
	if err != nil {
		return err
	}
	// A stable call can already have settled before this process reclaimed an
	// unknown lease. Recover first; no provider call or fallback is made here.
	stage = "recover_model_response"
	logger.Info("task terminal explanation recovery started", zap.String("stage", stage), zap.String("status", "started"))
	recoveryStarted := time.Now()
	_, recovered, err := w.options.Models.RecoverTaskTerminalDeliveryDSHModelResponse(workCtx, fence, recoveryRequest)
	if err != nil {
		logger.Warn("task terminal explanation recovery failed", zap.String("stage", stage),
			zap.Int64("duration_ms", time.Since(recoveryStarted).Milliseconds()), zap.String("error_type", typeName(err)))
	} else {
		status := "not_found"
		if recovered {
			status = "recovered"
		}
		logger.Info("task terminal explanation recovery completed", zap.String("stage", stage),
			zap.String("status", status), zap.Int64("duration_ms", time.Since(recoveryStarted).Milliseconds()))
	}
	if err != nil {
		return err
	}
	if recovered {
		stage = "load_recovered_reply"
		ready, found, err := w.options.Store.GetTaskTerminalDeliveryReplyReady(workCtx, claim.DeliveryID)
		if err != nil || !found {
			if err != nil {
				return err
			}
			return errors.New("settled Task terminal delivery response has no reply-ready source")
		}
		return w.publishReadyWithLogger(workCtx, ready, logger)
	}
	stage = "resolve_model"
	modelID, err := w.options.Models.ResolveTaskTerminalDeliveryModel(workCtx, fence)
	if err != nil && workCtx.Err() != nil {
		return workCtx.Err()
	}
	if err != nil {
		// Complete asks Gateway to classify the persisted call before the
		// worker chooses fallback. This preserves unknown if an earlier
		// provider dispatch was ambiguous.
		modelID = facts.ModelID
		logger.Warn("task terminal explanation model resolution failed; persisted model binding will be checked",
			zap.String("stage", stage), zap.String("status", "fallback_to_persisted_model"), zap.String("error_type", typeName(err)))
	} else {
		logger.Info("task terminal explanation model resolved", zap.String("stage", stage), zap.String("status", "resolved"))
	}
	// Resolve first, then bind the no-tools request to that exact model ID.
	// If the resolver failed, the persisted ID is used only to let Gateway
	// classify whether a prior stable call is unknown or definitely absent.
	stage = "build_model_request"
	raw, err := app.BuildTaskTerminalDeliveryDSHModelRequest(modelID, facts)
	if err != nil {
		return err
	}
	stage = "model_request"
	logger.Info("task terminal explanation request started", zap.String("stage", stage), zap.String("status", "started"))
	modelStarted := time.Now()
	response, disposition, callErr := w.options.Models.CompleteTaskTerminalDeliveryDSHModel(workCtx, fence, modelID, raw)
	responseFields := []zap.Field{zap.String("stage", "model_response"), zap.String("status", string(disposition)),
		zap.Int64("duration_ms", time.Since(modelStarted).Milliseconds())}
	if callErr != nil {
		responseFields = append(responseFields, zap.String("error_type", typeName(callErr)))
		logger.Warn("task terminal explanation request completed with an error", responseFields...)
	} else {
		logger.Info("task terminal explanation request completed", responseFields...)
	}
	switch disposition {
	case app.TaskTerminalDeliveryModelReplyReady:
		stage = "validate_model_response"
		if callErr != nil {
			return callErr
		}
		if !validTerminalDeliveryModelResponse(response) {
			return errors.New("task terminal delivery model response is invalid")
		}
		ready, found, err := w.options.Store.GetTaskTerminalDeliveryReplyReady(workCtx, claim.DeliveryID)
		if err != nil || !found {
			if err != nil {
				return err
			}
			return errors.New("settled Task terminal delivery response has no reply-ready source")
		}
		return w.publishReadyWithLogger(workCtx, ready, logger)
	case app.TaskTerminalDeliveryModelUnknown:
		// The explanation call has no tools or external side effects. If its
		// reply cannot be recovered now, persist one deterministic explanation
		// against the same outbox source instead of leaving the conversation
		// without a response. A late model result cannot replace a published
		// fallback or create a second assistant message.
		stage = "persist_unknown_fallback"
		logger.Warn("task terminal explanation outcome is unknown; publishing a deterministic fallback",
			zap.String("stage", stage), zap.String("status", "unknown"))
		return w.persistAndPublishSystemState(workCtx, claim, facts, logger)
	case app.TaskTerminalDeliveryModelStale:
		stage = "invalidate_stale_claim"
		invalidated, invalidateErr := w.options.Store.InvalidateTaskTerminalDeliveryClaim(context.WithoutCancel(ctx), claim)
		invalidateStatus := "unchanged"
		if invalidated {
			invalidateStatus = "invalidated"
		}
		invalidateFields := []zap.Field{zap.String("stage", stage), zap.String("status", invalidateStatus)}
		if invalidateErr != nil {
			invalidateFields = append(invalidateFields, zap.String("error_type", typeName(invalidateErr)))
		}
		logger.Warn("stale task terminal delivery claim processed", invalidateFields...)
		return errors.Join(callErr, invalidateErr)
	case app.TaskTerminalDeliveryModelNotDispatched, app.TaskTerminalDeliveryModelFallbackSafe:
		if workCtx.Err() != nil {
			return workCtx.Err()
		}
		stage = "persist_safe_fallback"
		logger.Info("task terminal explanation was not dispatched; publishing a deterministic fallback",
			zap.String("stage", stage), zap.String("status", string(disposition)))
		return w.persistAndPublishSystemState(workCtx, claim, facts, logger)
	default:
		// Never make another model call for an unrecognized outcome. The
		// deterministic reply is tied to this source and can only be published
		// once, so it remains safe even if the model call later settles.
		stage = "persist_unrecognized_fallback"
		logger.Warn("task terminal explanation returned an unrecognized disposition; publishing a deterministic fallback",
			zap.String("stage", stage), zap.String("status", "unrecognized"))
		return w.persistAndPublishSystemState(workCtx, claim, facts, logger)
	}
}

func (w *TaskTerminalDeliveryWorker) publishReady(ctx context.Context, claim app.TaskTerminalDeliveryClaim) (resultErr error) {
	scope := w.startDeliveryLogScope(ctx, claim, "publish")
	defer func() { scope.finish(resultErr) }()
	return w.publishReadyWithLogger(scope.ctx, claim, scope.logger)
}

func (w *TaskTerminalDeliveryWorker) publishReadyWithLogger(ctx context.Context, claim app.TaskTerminalDeliveryClaim,
	logger *zap.Logger) (resultErr error) {
	stage := "validate_reply_ready"
	defer func() {
		if resultErr != nil {
			logger.Error("task terminal delivery publication failed", zap.String("stage", stage),
				zap.String("error_type", typeName(resultErr)))
		}
	}()
	if !app.ValidTaskTerminalDeliveryClaim(claim) || claim.State != app.TaskTerminalDeliveryStateReplyReady {
		return errors.New("reply-ready Task terminal delivery claim is invalid")
	}
	facts, found, err := w.options.Store.ReadTaskTerminalDeliverySourceFacts(ctx, claim)
	if err != nil || !found {
		return err
	}
	if !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
		return errors.New("verified reply-ready Task terminal delivery facts are invalid")
	}
	var content string
	stage = "open_persisted_reply"
	fallback, fallbackErr := app.OpenTaskTerminalDeliveryReply(w.options.ReplySealer, claim)
	if fallbackErr == nil && fallback.Kind == app.TaskTerminalDeliveryReplySystemState {
		content = fallback.Text
		logger.Info("deterministic task terminal response recovered", zap.String("stage", stage), zap.String("status", "system_state"))
	} else {
		if fallbackErr != nil {
			logger.Warn("persisted task terminal response requires model recovery", zap.String("stage", stage),
				zap.String("status", "model_response"), zap.String("error_type", typeName(fallbackErr)))
		}
		raw, err := app.BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
		if err != nil {
			return err
		}
		fence, err := claim.ModelFence()
		if err != nil {
			return err
		}
		stage = "recover_model_response"
		logger.Info("task terminal model response recovery started", zap.String("stage", stage), zap.String("status", "started"))
		recoveryStarted := time.Now()
		response, recovered, err := w.options.Models.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, fence, raw)
		if err != nil {
			return err
		}
		if recovered {
			if !validTerminalDeliveryModelResponse(response) {
				return errors.New("recovered Task terminal delivery model response is invalid")
			}
			content = strings.TrimSpace(response.Text)
			logger.Info("task terminal model response recovered", zap.String("stage", stage), zap.String("status", "recovered"),
				zap.Int64("duration_ms", time.Since(recoveryStarted).Milliseconds()))
		} else if fallbackErr == nil && fallback.Kind == app.TaskTerminalDeliveryReplySystemState {
			content = fallback.Text
		} else {
			return errors.New("reply-ready Task terminal delivery has no recoverable response")
		}
	}
	stage = "publish_assistant_response"
	published, messageID, err := w.options.Store.PublishTaskTerminalDeliveryReply(ctx, claim, content)
	if err == nil {
		status := "already_published"
		if published {
			status = "published"
		}
		logger.Info("task terminal assistant response publication completed", zap.String("stage", stage),
			zap.String("status", status), zap.String("message_id", messageID))
	}
	return err
}

func (w *TaskTerminalDeliveryWorker) persistAndPublishSystemState(ctx context.Context,
	claim app.TaskTerminalDeliveryClaim, facts app.TaskTerminalDeliverySourceFacts, logger *zap.Logger) error {
	logger.Info("deterministic task terminal response persistence started", zap.String("stage", "persist_fallback"),
		zap.String("status", "started"))
	reply := app.TaskTerminalDeliveryReply{Kind: app.TaskTerminalDeliveryReplySystemState,
		Text: terminalDeliverySystemStateReply(facts)}
	ciphertext, err := app.SealTaskTerminalDeliveryReply(w.options.ReplySealer, claim, reply)
	if err != nil {
		return err
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ready, changed, err := w.options.Store.PersistTaskTerminalDeliveryReplyReady(persistCtx, claim, ciphertext)
	if err != nil {
		return err
	}
	if !changed {
		logger.Info("deterministic task terminal response was not persisted because the claim changed",
			zap.String("stage", "persist_fallback"), zap.String("status", "claim_changed"))
		return nil
	}
	logger.Info("deterministic task terminal response persisted", zap.String("stage", "persist_fallback"),
		zap.String("status", "reply_ready"))
	return w.publishReadyWithLogger(persistCtx, ready, logger)
}

func terminalDeliverySystemStateReply(facts app.TaskTerminalDeliverySourceFacts) string {
	switch facts.Identity.SourceKind {
	case app.TaskTerminalDeliverySourceFailure:
		cause := app.TaskTerminalDeliveryFailureExplanation(facts.FailureCode)
		if cause == "" || facts.FailureCode == "unknown" {
			return "这次没有拿到已核实的结果，任务已中断。原因暂未查明；可以查看任务详情。"
		}
		return "这次没有拿到已核实的结果，任务已中断。" + cause + "可以查看任务详情。"
	case app.TaskTerminalDeliverySourceCancellation:
		return "任务已停止；未完成的部分没有交付。可以查看任务详情。"
	case app.TaskTerminalDeliverySourceWaiting:
		diagnosis := facts.RecoveryDiagnosis
		if diagnosis == "" {
			diagnosis = app.TaskTerminalDeliveryWaitingExplanation(facts.WaitingReason)
		}
		return "这次任务没有形成可核实的答复。" + diagnosis +
			"执行进度保留在任务详情中。"
	case app.TaskTerminalDeliverySourceValidation:
		if facts.ValidationState == app.TaskValidationPass || facts.ValidationState == app.TaskValidationPassNotes {
			return "任务结果已通过验证。验证依据可在任务详情查看。"
		}
		return "任务结果仍有检查未通过或待确认。验证依据可在任务详情查看。"
	default:
		return "任务状态已保存；当前没有可确认的结果。可以查看任务详情。"
	}
}

func validTerminalDeliveryModelResponse(response app.ModelResponse) bool {
	return len(response.ToolCalls) == 0 && response.StopReason == "stop" && strings.TrimSpace(response.Text) != "" &&
		utf8.ValidString(response.Text) && !strings.ContainsRune(response.Text, '\x00') &&
		len(response.Text) <= app.MaxTaskTerminalDeliveryModelResponseTextBytes && response.Usage.Known &&
		response.Usage.InputTokens >= 0 && response.Usage.OutputTokens >= 0 &&
		response.Usage.InputTokens <= 1<<53-1 && response.Usage.OutputTokens <= 1<<53-1 &&
		app.ValidProviderReportedModel(response.ProviderReportedModel)
}
