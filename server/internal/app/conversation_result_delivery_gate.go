package app

import "morde/server/internal/domain"

// ValidateConversationResultDelivery keeps every source paired with its
// current validation state. A failed review is delivered as a failure, not
// as an accepted result; it must not hide the other results in the batch.
func ValidateConversationResultDelivery(sources []ConversationTaskResultSource, states []TaskValidationState) error {
	if len(sources) != len(states) {
		return domain.InvalidArgument("result_review", "task result validation states do not match sources")
	}
	return nil
}
