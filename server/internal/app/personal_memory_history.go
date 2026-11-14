package app

import (
	"context"
	"encoding/json"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type PersonalMemoryHistoryPage struct {
	Items      []personal.MemoryRevision `json:"items"`
	NextCursor string                    `json:"next_cursor"`
}

type personalMemoryHistoryStore interface {
	GetPersonalMemoryHistoryBatch(context.Context, identity.Principal, string, *personal.MemoryHistoryCursor, int32) (personal.MemoryHistoryBatch, error)
}

func (s *PersonalSyncService) GetMemoryHistory(ctx context.Context, principal identity.Principal, objectType, objectID, cursor string, limit int32) (PersonalMemoryHistoryPage, error) {
	if (principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if objectType != personal.ObjectTypeMemory {
		return PersonalMemoryHistoryPage{}, domain.InvalidArgument("type", "history is supported only for memory objects")
	}
	if err := personal.ValidateObjectIdentifier(objectType, objectID); err != nil {
		return PersonalMemoryHistoryPage{}, err
	}
	limit, err := personalSyncLimit(limit)
	if err != nil {
		return PersonalMemoryHistoryPage{}, err
	}
	var decoded *personal.MemoryHistoryCursor
	if cursor != "" {
		value, err := personal.DecodeMemoryHistoryCursor(cursor, s.dataKey)
		if err != nil || value.OwnerID != principal.UserID || value.ObjectType != objectType || value.ObjectID != objectID {
			return PersonalMemoryHistoryPage{}, domain.InvalidArgument("cursor", "memory history cursor is invalid for this account and object")
		}
		decoded = &value
	}
	store, ok := s.store.(personalMemoryHistoryStore)
	if !ok {
		return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history is unavailable")
	}
	batch, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, objectID, decoded, limit)
	if err != nil {
		return PersonalMemoryHistoryPage{}, err
	}
	if batch.ServiceInstanceID == "" || int32(len(batch.Items)) > limit {
		return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history page is invalid")
	}
	if decoded != nil && decoded.ServiceInstanceID != batch.ServiceInstanceID {
		return PersonalMemoryHistoryPage{}, domain.InvalidArgument("cursor", "memory history cursor does not match the current service instance")
	}
	previousRevision := int64(^uint64(0) >> 1)
	for _, revision := range batch.Items {
		if revision.Revision < 1 || revision.Revision >= previousRevision || revision.ChangedAt.IsZero() ||
			!validMemoryHistorySourceStatus(revision.SourceStatus) || revision.Sources == nil {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history revision is invalid")
		}
		if decoded != nil && revision.Revision >= decoded.AfterRevision {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history page violates its cursor")
		}
		if len(revision.Body) == 0 {
			if revision.Restorable {
				return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "memory history without a body cannot be restorable")
			}
		} else if !json.Valid(revision.Body) || personal.ValidateObjectBody(personal.ObjectTypeMemory, objectID, revision.Body) != nil || !revision.Restorable {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history body is invalid")
		}
		if revision.SourceStatus == personal.MemoryHistorySourceUnavailable && (len(revision.Body) != 0 || revision.Restorable || len(revision.Sources) != 0) {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "unavailable memory history provenance must be withheld")
		}
		if revision.SourceStatus == personal.MemoryHistorySourceUnrecorded && len(revision.Sources) != 0 {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "unrecorded memory history cannot contain sources")
		}
		if revision.SourceStatus == personal.MemoryHistorySourceAvailable && len(revision.Sources) == 0 {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "available memory history provenance has no source references")
		}
		for _, source := range revision.Sources {
			if (source.Kind != "conversation_message" && source.Kind != "mutation_receipt") || source.ID == "" ||
				(source.Kind == "conversation_message" && source.ConversationID == "") ||
				(source.Kind == "mutation_receipt" && source.ConversationID != "") {
				return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history source is invalid")
			}
		}
		previousRevision = revision.Revision
	}
	page := PersonalMemoryHistoryPage{Items: batch.Items}
	if page.Items == nil {
		page.Items = make([]personal.MemoryRevision, 0)
	}
	if batch.HasMore {
		if len(batch.Items) == 0 {
			return PersonalMemoryHistoryPage{}, domain.NewError(domain.CodeInternal, "personal memory history cursor position is invalid")
		}
		page.NextCursor, err = personal.EncodeMemoryHistoryCursor(personal.MemoryHistoryCursor{
			Version: personal.MemoryHistoryCursorVersion, OwnerID: principal.UserID, ServiceInstanceID: batch.ServiceInstanceID,
			ObjectType: personal.ObjectTypeMemory, ObjectID: objectID, AfterRevision: batch.Items[len(batch.Items)-1].Revision,
		}, s.dataKey)
		if err != nil {
			return PersonalMemoryHistoryPage{}, fmt.Errorf("encode personal memory history cursor: %w", err)
		}
	}
	return page, nil
}

func validMemoryHistorySourceStatus(status string) bool {
	return status == personal.MemoryHistorySourceAvailable || status == personal.MemoryHistorySourceUnrecorded ||
		status == personal.MemoryHistorySourceUnavailable
}
