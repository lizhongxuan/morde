package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

const (
	personalSyncDefaultLimit int32 = 100
	personalSyncMaxLimit     int32 = 100
)

type PersonalSnapshotPage struct {
	Watermark  int64             `json:"watermark"`
	Items      []personal.Object `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

type personalSyncStore interface {
	GetPersonalSnapshotBatch(context.Context, identity.Principal, *personal.SnapshotCursor, int32) (personal.SnapshotBatch, error)
	GetPersonalChanges(context.Context, identity.Principal, int64, *int64, int32) (personal.ChangePage, error)
}

type PersonalSyncService struct {
	store   personalSyncStore
	dataKey []byte
}

func NewPersonalSyncService(store personalSyncStore, dataKey []byte) (*PersonalSyncService, error) {
	if store == nil {
		return nil, errors.New("personal sync store is required")
	}
	if len(dataKey) != 32 {
		return nil, errors.New("personal sync data key must be 32 bytes")
	}
	return &PersonalSyncService{store: store, dataKey: append([]byte(nil), dataKey...)}, nil
}

func (s *PersonalSyncService) GetSnapshot(ctx context.Context, principal identity.Principal, cursor string, limit int32) (PersonalSnapshotPage, error) {
	if err := validatePersonalSyncPrincipal(principal); err != nil {
		return PersonalSnapshotPage{}, err
	}
	limit, err := personalSyncLimit(limit)
	if err != nil {
		return PersonalSnapshotPage{}, err
	}
	var decoded *personal.SnapshotCursor
	if cursor != "" {
		value, err := personal.DecodeSnapshotCursor(cursor, s.dataKey)
		if err != nil || value.OwnerID != principal.UserID {
			return PersonalSnapshotPage{}, domain.InvalidArgument("cursor", "snapshot cursor is invalid for this account")
		}
		decoded = &value
	}
	batch, err := s.store.GetPersonalSnapshotBatch(ctx, principal, decoded, limit)
	if err != nil {
		return PersonalSnapshotPage{}, err
	}
	if batch.Watermark < 0 || batch.ServiceInstanceID == "" || int32(len(batch.Items)) > limit {
		return PersonalSnapshotPage{}, domain.NewError(domain.CodeInternal, "personal snapshot page is invalid")
	}
	if decoded != nil && (batch.Watermark != decoded.Watermark || batch.ServiceInstanceID != decoded.ServiceInstanceID) {
		return PersonalSnapshotPage{}, domain.InvalidArgument("cursor", "snapshot cursor does not match the current service instance")
	}
	for _, item := range batch.Items {
		if !personal.IsPersonalSyncObjectType(item.ObjectType) ||
			personal.ValidateObjectIdentifier(item.ObjectType, item.ObjectID) != nil || item.Revision < 1 ||
			len(item.Body) == 0 || !json.Valid(item.Body) ||
			personal.ValidateObjectBody(item.ObjectType, item.ObjectID, item.Body) != nil {
			return PersonalSnapshotPage{}, domain.NewError(domain.CodeInternal, "personal snapshot object is invalid")
		}
	}
	page := PersonalSnapshotPage{Watermark: batch.Watermark, Items: batch.Items}
	if page.Items == nil {
		page.Items = make([]personal.Object, 0)
	}
	if batch.HasMore {
		if len(batch.Items) == 0 || batch.AfterObjectType != batch.Items[len(batch.Items)-1].ObjectType ||
			batch.AfterObjectID != batch.Items[len(batch.Items)-1].ObjectID {
			return PersonalSnapshotPage{}, domain.NewError(domain.CodeInternal, "personal snapshot cursor position is invalid")
		}
		value := personal.SnapshotCursor{
			Version: personal.SnapshotCursorVersion, OwnerID: principal.UserID, ServiceInstanceID: batch.ServiceInstanceID,
			ObjectType: personal.SnapshotScopeObjectType, ObjectID: personal.SnapshotScopeObjectID,
			Watermark: batch.Watermark, AfterObjectType: batch.AfterObjectType, AfterObjectID: batch.AfterObjectID,
		}
		page.NextCursor, err = personal.EncodeSnapshotCursor(value, s.dataKey)
		if err != nil {
			return PersonalSnapshotPage{}, fmt.Errorf("encode personal snapshot cursor: %w", err)
		}
	}
	return page, nil
}

func (s *PersonalSyncService) GetChanges(ctx context.Context, principal identity.Principal, afterSequence int64, throughSequence *int64, limit int32) (personal.ChangePage, error) {
	if err := validatePersonalSyncPrincipal(principal); err != nil {
		return personal.ChangePage{}, err
	}
	if afterSequence < 0 || (throughSequence != nil && *throughSequence < afterSequence) {
		return personal.ChangePage{}, domain.InvalidArgument("after_sequence", "personal change cursor is invalid")
	}
	limit, err := personalSyncLimit(limit)
	if err != nil {
		return personal.ChangePage{}, err
	}
	return s.store.GetPersonalChanges(ctx, principal, afterSequence, throughSequence, limit)
}

func validatePersonalSyncPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return validatePrincipal(principal)
}

func personalSyncLimit(limit int32) (int32, error) {
	if limit == 0 {
		return personalSyncDefaultLimit, nil
	}
	if limit < 1 || limit > personalSyncMaxLimit {
		return 0, domain.InvalidArgument("limit", "personal sync page limit is out of range")
	}
	return limit, nil
}
