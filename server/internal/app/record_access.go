package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type RecordAccessStore interface {
	SetRecordAdminGrant(context.Context, identity.Principal, string, string, record.AdminCapability, bool) (record.AdminGrant, error)
	GetAdminRecordSettings(context.Context, identity.Principal, string) (RecordAdminSettings, error)
	GetRecordAdminAccess(context.Context, identity.Principal, string) (RecordAdminAccess, error)
	ListRecordMetadata(context.Context, identity.Principal, string, record.Query) (record.Page, error)
	ReadRecord(context.Context, identity.Principal, string, string) (record.Content, error)
	ExportRecord(context.Context, identity.Principal, string, string) (record.Content, error)
	ListRecordAttachments(context.Context, identity.Principal, string, string) ([]record.AttachmentMetadata, error)
	ReadRecordAttachment(context.Context, identity.Principal, string, string, string) (record.AttachmentContent, error)
	RequestRecordDelete(context.Context, identity.Principal, string, string) (record.DeleteTombstone, error)
	ProcessRecordDeletes(context.Context, int32) (int32, error)
}

type RecordAdminSettings struct {
	Consents []record.Consent
	Quota    record.Quota
	Grants   []record.AdminGrant
}

type RecordAdminAccess struct {
	Consents []record.Consent
	Grants   []record.AdminGrant
}

type RecordAccessService struct {
	store RecordAccessStore
}

func NewRecordAccessService(store RecordAccessStore) (*RecordAccessService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("record_access", "record access store is required")
	}
	return &RecordAccessService{store: store}, nil
}

func (s *RecordAccessService) SetGrant(ctx context.Context, principal identity.Principal, ownerID, adminID string, capability record.AdminCapability, granted bool) (record.AdminGrant, error) {
	if principal.Kind != identity.KindSession || principal.Role != identity.RoleSuperAdmin ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return record.AdminGrant{}, domain.NewError(domain.CodeForbidden, "record grant management is denied")
	}
	if !domain.ValidUserID(ownerID) || !domain.ValidUserID(adminID) || ownerID == adminID || !capability.Valid() {
		return record.AdminGrant{}, domain.InvalidArgument("record_grant", "record grant fields are invalid")
	}
	return s.store.SetRecordAdminGrant(ctx, principal, ownerID, adminID, capability, granted)
}

func (s *RecordAccessService) AdminSettings(ctx context.Context, principal identity.Principal, ownerID string) (RecordAdminSettings, error) {
	if principal.Kind != identity.KindSession || principal.Role != identity.RoleSuperAdmin ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return RecordAdminSettings{}, domain.NewError(domain.CodeForbidden, "record administrator settings access is denied")
	}
	if !domain.ValidUserID(ownerID) {
		return RecordAdminSettings{}, domain.InvalidArgument("owner_id", "record owner is invalid")
	}
	return s.store.GetAdminRecordSettings(ctx, principal, ownerID)
}

func (s *RecordAccessService) AccessSettings(ctx context.Context, principal identity.Principal, ownerID string) (RecordAdminAccess, error) {
	if principal.Kind != identity.KindSession || !principal.Role.IsAdmin() ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return RecordAdminAccess{}, domain.NewError(domain.CodeForbidden, "record administrator access is denied")
	}
	if !domain.ValidUserID(ownerID) {
		return RecordAdminAccess{}, domain.InvalidArgument("owner_id", "record owner is invalid")
	}
	return s.store.GetRecordAdminAccess(ctx, principal, ownerID)
}

func (s *RecordAccessService) List(ctx context.Context, principal identity.Principal, ownerID string, query record.Query) (record.Page, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.Page{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || (query.Purpose != "" && !query.Purpose.Valid()) ||
		(query.BeforeID != "" && !record.ValidRecordID(query.BeforeID)) {
		return record.Page{}, domain.InvalidArgument("record_query", "record query is invalid")
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 100 {
		return record.Page{}, domain.InvalidArgument("limit", "record page limit must be between 1 and 100")
	}
	return s.store.ListRecordMetadata(ctx, principal, ownerID, query)
}

func (s *RecordAccessService) Read(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.Content, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.Content{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return record.Content{}, domain.InvalidArgument("record", "record identity is invalid")
	}
	return s.store.ReadRecord(ctx, principal, ownerID, recordID)
}

func (s *RecordAccessService) Export(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.Content, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.Content{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return record.Content{}, domain.InvalidArgument("record", "record identity is invalid")
	}
	return s.store.ExportRecord(ctx, principal, ownerID, recordID)
}

func (s *RecordAccessService) ReadAttachment(ctx context.Context, principal identity.Principal, ownerID, recordID, attachmentID string) (record.AttachmentContent, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.AttachmentContent{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) || !record.ValidAttachmentID(attachmentID) {
		return record.AttachmentContent{}, domain.InvalidArgument("record_attachment", "record attachment identity is invalid")
	}
	return s.store.ReadRecordAttachment(ctx, principal, ownerID, recordID, attachmentID)
}

func (s *RecordAccessService) ListAttachments(ctx context.Context, principal identity.Principal, ownerID, recordID string) ([]record.AttachmentMetadata, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return nil, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return nil, domain.InvalidArgument("record_attachment", "record attachment identity is invalid")
	}
	return s.store.ListRecordAttachments(ctx, principal, ownerID, recordID)
}

func (s *RecordAccessService) Delete(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.DeleteTombstone, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.DeleteTombstone{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return record.DeleteTombstone{}, domain.InvalidArgument("record", "record identity is invalid")
	}
	return s.store.RequestRecordDelete(ctx, principal, ownerID, recordID)
}

func (s *RecordAccessService) ProcessDeletes(ctx context.Context, limit int32) (int32, error) {
	if limit < 1 || limit > 100 {
		return 0, domain.InvalidArgument("limit", "record deletion batch limit must be between 1 and 100")
	}
	return s.store.ProcessRecordDeletes(ctx, limit)
}
