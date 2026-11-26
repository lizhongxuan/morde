package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type taskPCDeliveryObservationStore interface {
	RecordPCDeliveryObservation(context.Context, string, string, string, string, string, int64) error
}

// ObservePCDelivery records only that an authenticated desktop installation
// displayed a completed Task reply. It never accepts or stores message text.
func (s *TaskService) ObservePCDelivery(ctx context.Context, principal identity.Principal, taskID, messageID string, taskRevision int64) error {
	if s == nil || s.store == nil || principal.Kind != identity.KindSession ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return domain.NewError(domain.CodeUnauthenticated, "authenticated desktop session required")
	}
	if !domain.ValidTaskID(taskID) || !domain.ValidMessageID(messageID) || taskRevision < 0 {
		return domain.InvalidArgument("delivery_observation", "Task, message and revision are invalid")
	}
	store, ok := s.store.(taskPCDeliveryObservationStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "PC delivery observation is unavailable")
	}
	return store.RecordPCDeliveryObservation(ctx, principal.UserID, principal.TokenID,
		principal.ClientInstallationID, taskID, messageID, taskRevision)
}
