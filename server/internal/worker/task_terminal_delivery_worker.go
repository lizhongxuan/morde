package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		return nil, errors.New("Task terminal delivery worker dependencies and bounds are required")
	}
	return &TaskTerminalDeliveryWorker{options: options}, nil
}

func (w *TaskTerminalDeliveryWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Task terminal delivery worker context is required")
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
		return false, errors.New("Task terminal delivery worker context is required")
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
		return errors.New("Task terminal delivery claim is invalid")
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
			return errors.New("Task terminal delivery model response is invalid")
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
		changed, err := w.options.Store.MarkTaskTerminalDeliveryUnknownClaim(context.WithoutCancel(ctx), claim)
		if err != nil {
			return errors.Join(callErr, err)
		}
		if !changed && callErr == nil {
			return errors.New("unknown Task terminal delivery call lost its active claim")
		}
		return callErr
	case app.TaskTerminalDeliveryModelStale:
		_, invalidateErr := w.options.Store.InvalidateTaskTerminalDeliveryClaim(context.WithoutCancel(ctx), claim)
		return errors.Join(callErr, invalidateErr)
	case app.TaskTerminalDeliveryModelNotDispatched, app.TaskTerminalDeliveryModelFallbackSafe:
		if workCtx.Err() != nil {
			return workCtx.Err()
		}
		return w.persistAndPublishSystemState(workCtx, claim, facts)
	default:
		// Unknown dispositions are treated as ambiguous. Preserve the stable
		// call identity and never synthesize a response or retry another call.
		changed, markErr := w.options.Store.MarkTaskTerminalDeliveryUnknownClaim(context.WithoutCancel(ctx), claim)
		if markErr != nil {
			return errors.Join(callErr, markErr)
		}
		if !changed && callErr == nil {
			return errors.New("unrecognized Task terminal delivery disposition lost its active claim")
		}
		return callErr
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
	var content string
	if recovered {
		if !validTerminalDeliveryModelResponse(response) {
			return errors.New("recovered Task terminal delivery model response is invalid")
		}
		content = strings.TrimSpace(response.Text)
	} else {
		fallback, err := app.OpenTaskTerminalDeliveryReply(w.options.ReplySealer, claim)
		if err != nil {
			return err
		}
		if fallback.Kind != app.TaskTerminalDeliveryReplySystemState {
			return errors.New("reply-ready Task terminal delivery has no recoverable model response")
		}
		content = fallback.Text
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
	progress := fmt.Sprintf("持久步骤记录共 %d 步：已完成 %d 步、失败 %d 步、取消 %d 步、未确认或未完成 %d 步；详细步骤和执行记录请查看任务记录。下一步：%s",
		facts.StepCounts.Total, facts.StepCounts.Succeeded, facts.StepCounts.Failed, facts.StepCounts.Cancelled,
		facts.StepCounts.Unconfirmed, app.TaskTerminalDeliverySafeNextStep(facts.Identity.SourceKind))
	switch facts.Identity.SourceKind {
	case app.TaskTerminalDeliverySourceFailure:
		return fmt.Sprintf("系统状态说明：任务执行状态为失败。原因：%s %s", app.TaskTerminalDeliveryFailureExplanation(facts.FailureCode), progress)
	case app.TaskTerminalDeliverySourceCancellation:
		return "系统状态说明：任务已确认取消。" + progress
	case app.TaskTerminalDeliverySourceWaiting:
		return fmt.Sprintf("系统状态说明：任务当前处于等待处理状态。原因：%s %s", app.TaskTerminalDeliveryWaitingExplanation(facts.WaitingReason), progress)
	case app.TaskTerminalDeliverySourceValidation:
		var checks []string
		for _, finding := range facts.ValidationFindings {
			if finding.Outcome == app.TaskValidationCheckFail || finding.Outcome == app.TaskValidationCheckPending {
				checks = append(checks, string(finding.ID))
			}
		}
		if len(checks) == 0 {
			return fmt.Sprintf("系统状态说明：任务验证结论为 %s。%s", facts.ValidationState, progress)
		}
		return fmt.Sprintf("系统状态说明：任务验证结论为 %s；未通过或尚未确认的检查：%s。%s",
			facts.ValidationState, strings.Join(checks, "、"), progress)
	default:
		return "系统状态说明：任务状态已记录。" + progress
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
