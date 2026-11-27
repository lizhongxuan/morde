package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"morde/server/internal/app"
)

type TaskTerminalDeliveryModelGateway interface {
	ResolveTaskTerminalDeliveryModel(context.Context, app.TaskTerminalDeliveryModelFence) (string, error)
	CompleteTaskTerminalDeliveryDSHModel(context.Context, app.TaskTerminalDeliveryModelFence, string,
		json.RawMessage) (app.ModelResponse, app.TaskTerminalDeliveryModelCallDisposition, error)
	RecoverTaskTerminalDeliveryDSHModelResponse(context.Context, app.TaskTerminalDeliveryModelFence,
		json.RawMessage) (app.ModelResponse, bool, error)
}

type TaskTerminalDeliveryWorkerOptions struct {
	Store         app.TaskTerminalDeliveryStore
	Models        TaskTerminalDeliveryModelGateway
	ReplySealer   *app.Sealer
	WorkerID      string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	Logger        *zap.Logger
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
		if err != nil && ctx.Err() == nil {
			w.options.Logger.Warn("Task terminal delivery iteration failed", zap.String("job", "task_terminal_delivery"))
		}
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
func (w *TaskTerminalDeliveryWorker) RunOnce(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, errors.New("task terminal delivery worker context is required")
	}
	ready, err := w.options.Store.ListTaskTerminalDeliveryReplyReady(ctx, 1)
	if err != nil || len(ready) != 0 {
		if err != nil {
			return false, err
		}
		return true, w.publishReady(ctx, ready[0])
	}
	claims, err := w.options.Store.ClaimTaskTerminalDeliverySources(ctx, w.options.WorkerID, w.options.LeaseDuration, 1)
	if err != nil || len(claims) == 0 {
		return len(claims) != 0, err
	}
	return true, w.deliverClaim(ctx, claims[0])
}

func (w *TaskTerminalDeliveryWorker) deliverClaim(ctx context.Context, claim app.TaskTerminalDeliveryClaim) error {
	if !app.ValidTaskTerminalDeliveryClaim(claim) || claim.State != app.TaskTerminalDeliveryStateClaimed {
		return errors.New("task terminal delivery claim is invalid")
	}
	workCtx, cancel := context.WithDeadline(ctx, claim.LeaseExpiresAt.Add(-time.Second))
	defer cancel()
	if err := workCtx.Err(); err != nil {
		return err
	}
	facts, found, err := w.options.Store.ReadTaskTerminalDeliverySourceFacts(workCtx, claim)
	if err != nil || !found {
		return err
	}
	if !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
		return errors.New("verified Task terminal delivery source facts are invalid")
	}
	// Recovery uses the Task's persisted model ID so a settled call can be
	// reconstructed even if current model configuration has changed.
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
	_, recovered, err := w.options.Models.RecoverTaskTerminalDeliveryDSHModelResponse(workCtx, fence, recoveryRequest)
	if err != nil {
		return err
	}
	if recovered {
		ready, found, err := w.options.Store.GetTaskTerminalDeliveryReplyReady(workCtx, claim.DeliveryID)
		if err != nil || !found {
			if err != nil {
				return err
			}
			return errors.New("settled Task terminal delivery response has no reply-ready source")
		}
		return w.publishReady(workCtx, ready)
	}
	modelID, err := w.options.Models.ResolveTaskTerminalDeliveryModel(workCtx, fence)
	if err != nil && workCtx.Err() != nil {
		return workCtx.Err()
	}
	if err != nil {
		// Complete asks Gateway to classify the persisted call before the
		// worker chooses fallback. This preserves unknown if an earlier
		// provider dispatch was ambiguous.
		modelID = facts.ModelID
	}
	// Resolve first, then bind the no-tools request to that exact model ID.
	// If the resolver failed, the persisted ID is used only to let Gateway
	// classify whether a prior stable call is unknown or definitely absent.
	raw, err := app.BuildTaskTerminalDeliveryDSHModelRequest(modelID, facts)
	if err != nil {
		return err
	}
	response, disposition, callErr := w.options.Models.CompleteTaskTerminalDeliveryDSHModel(workCtx, fence, modelID, raw)
	switch disposition {
	case app.TaskTerminalDeliveryModelReplyReady:
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
		return w.publishReady(workCtx, ready)
	case app.TaskTerminalDeliveryModelUnknown:
		// The explanation call has no tools or external side effects. If its
		// reply cannot be recovered now, persist one deterministic explanation
		// against the same outbox source instead of leaving the conversation
		// without a response. A late model result cannot replace a published
		// fallback or create a second assistant message.
		return w.persistAndPublishSystemState(workCtx, claim, facts)
	case app.TaskTerminalDeliveryModelStale:
		_, invalidateErr := w.options.Store.InvalidateTaskTerminalDeliveryClaim(context.WithoutCancel(ctx), claim)
		return errors.Join(callErr, invalidateErr)
	case app.TaskTerminalDeliveryModelNotDispatched, app.TaskTerminalDeliveryModelFallbackSafe:
		if workCtx.Err() != nil {
			return workCtx.Err()
		}
		return w.persistAndPublishSystemState(workCtx, claim, facts)
	default:
		// Never make another model call for an unrecognized outcome. The
		// deterministic reply is tied to this source and can only be published
		// once, so it remains safe even if the model call later settles.
		return w.persistAndPublishSystemState(workCtx, claim, facts)
	}
}

func (w *TaskTerminalDeliveryWorker) publishReady(ctx context.Context, claim app.TaskTerminalDeliveryClaim) error {
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
	fallback, fallbackErr := app.OpenTaskTerminalDeliveryReply(w.options.ReplySealer, claim)
	if fallbackErr == nil && fallback.Kind == app.TaskTerminalDeliveryReplySystemState {
		content = fallback.Text
	} else {
		raw, err := app.BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
		if err != nil {
			return err
		}
		fence, err := claim.ModelFence()
		if err != nil {
			return err
		}
		response, recovered, err := w.options.Models.RecoverTaskTerminalDeliveryDSHModelResponse(ctx, fence, raw)
		if err != nil {
			return err
		}
		if recovered {
			if !validTerminalDeliveryModelResponse(response) {
				return errors.New("recovered Task terminal delivery model response is invalid")
			}
			content = strings.TrimSpace(response.Text)
		} else if fallbackErr == nil && fallback.Kind == app.TaskTerminalDeliveryReplySystemState {
			content = fallback.Text
		} else {
			return errors.New("reply-ready Task terminal delivery has no recoverable response")
		}
	}
	_, _, err = w.options.Store.PublishTaskTerminalDeliveryReply(ctx, claim, content)
	return err
}

func (w *TaskTerminalDeliveryWorker) persistAndPublishSystemState(ctx context.Context,
	claim app.TaskTerminalDeliveryClaim, facts app.TaskTerminalDeliverySourceFacts) error {
	reply := app.TaskTerminalDeliveryReply{Kind: app.TaskTerminalDeliveryReplySystemState,
		Text: terminalDeliverySystemStateReply(facts)}
	ciphertext, err := app.SealTaskTerminalDeliveryReply(w.options.ReplySealer, claim, reply)
	if err != nil {
		return err
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ready, changed, err := w.options.Store.PersistTaskTerminalDeliveryReplyReady(persistCtx, claim, ciphertext)
	if err != nil || !changed {
		return err
	}
	return w.publishReady(persistCtx, ready)
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
