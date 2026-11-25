package app

import (
	"fmt"

	"morde/server/internal/domain"
)

// ValidateConversationResultDelivery is the delivery-authenticity fence for
// result batches (E11). It refuses the whole batch when any source failed
// independent verification: a result that did not pass verification must not
// reach the user as a delivered receipt.
//
// A pending source does not block: the receipt labels its state and the
// header states the summary does not represent independent acceptance, so a
// pending delivery never claims a verification it does not have. Blocking
// pending would also strand the normal flow, because the batch is enqueued
// as soon as its tasks complete while the independent verifier still runs,
// and there is no re-trigger that would re-publish the batch once the review
// lands, so the delivery would wait for a manual retry. PASS and PASS+NOTES
// publish unchanged.
//
// The checkpoint-digest fence lives one layer below: the review loader
// re-verifies revision, result bytes, turn, authorization and dependencies
// at publish time and supersedes stale sources, so a digest mismatch never
// reaches this gate.
//
// The refusal is fail-closed and atomic: it happens before the receipt is
// built and before any publish write, so the route fails, the batch is
// marked failed, and an authorized retry can re-run the publish once the
// review passes. The stage detail keeps the refusal locatable from the
// route-failure log alone.
func ValidateConversationResultDelivery(sources []ConversationTaskResultSource, states []TaskValidationState) error {
	if len(sources) != len(states) {
		return domain.InvalidArgument("result_review", "task result validation states do not match sources")
	}
	for index, state := range states {
		if state != TaskValidationFail {
			continue
		}
		source := sources[index]
		err := domain.NewError(domain.CodeInvalidState,
			fmt.Sprintf("task result validation failed for %s r%d; result will not be published", source.TaskID, source.TaskRevision))
		err.Details["stage"] = "result_validation_gate"
		return err
	}
	return nil
}
