package app

import (
	"strings"

	"morde/server/internal/domain"
)

// ValidateConversationResultDelivery enforces the delivery authenticity rule
// (E11): a result batch is refused as a whole when any source's current
// validation verdict is FAIL. The refusal is fail-closed and atomic — the
// caller runs it inside the publish transaction before any publish write,
// so a refused batch leaves no message, no published source and no
// published batch behind. The route worker then fails the route with
// `routing_action_unavailable`, which marks the whole batch failed; an
// authorized retry may re-run the route after a re-review passes, and the
// gate re-checks the current verdict at that point.
//
// pending does not block delivery. That is a decided rule, not an
// oversight: the receipt states pending verdicts as pending, and there is
// no mechanism that re-publishes a batch once its verdict lands — blocking
// pending would strand normal deliveries in manual retries. Whether pending
// should also block is still awaiting the user's decision; do not change
// this without it.
//
// The checkpoint-digest fence is not re-implemented here: the publish path
// already refuses to deliver when the result bytes, the result digest, or
// the result event digest disagree (the load and enqueue checks), and this
// gate sits behind them. The receipt wording rule is enforced
// structurally, not by matching model text: the receipt is the model
// summary (what was fixed) plus the delivery facts (which operations and
// files) plus the per-source validation states (how each result was
// verified), and this gate is what guarantees a FAIL verdict never reaches
// a receipt.
func ValidateConversationResultDelivery(sources []ConversationTaskResultSource, states []TaskValidationState) error {
	if len(sources) != len(states) {
		return domain.InvalidArgument("result_review", "task result delivery validation states do not match their sources")
	}
	var failed []string
	for index, state := range states {
		if !ValidTaskValidationState(state) {
			return domain.InvalidArgument("result_review", "task result delivery validation state is invalid")
		}
		if state == TaskValidationFail {
			failed = append(failed, sources[index].TaskID)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	err := domain.NewError(domain.CodeInvalidState,
		"task result delivery refused: result validation failed for task "+strings.Join(failed, ", "))
	err.Details["stage"] = "result_validation_gate"
	err.Details["failed_task_ids"] = strings.Join(failed, ",")
	err.Details["failed_count"] = len(failed)
	return err
}
