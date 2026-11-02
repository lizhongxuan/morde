package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) SetRecordAdminGrant(ctx context.Context, principal identity.Principal, ownerID, adminID string, capability record.AdminCapability, granted bool) (record.AdminGrant, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidUserID(adminID) || ownerID == adminID || !capability.Valid() {
		return record.AdminGrant{}, domain.InvalidArgument("record_grant", "record grant fields are invalid")
	}
	var value record.AdminGrant
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := recheckRecordAdminInTx(ctx, tx, principal, true); err != nil {
			return err
		}
		if err := requireRecordOwnerExistsTx(ctx, tx, ownerID); err != nil {
			return err
		}
		var status, role string
		if err := tx.QueryRow(ctx, `SELECT status,role FROM users WHERE id=$1 FOR SHARE`, adminID).Scan(&status, &role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record administrator was not found")
			}
			return fmt.Errorf("read record grant administrator: %w", err)
		}
		if granted && (status != string(identity.StatusApproved) || role != string(identity.RoleAdmin)) {
			return domain.NewError(domain.CodeInvalidArgument, "record grants can only be assigned to active administrators")
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO record_admin_grants
  (owner_id,admin_id,capability,granted,granted_by,version)
VALUES ($1,$2,$3,$4,$5,1) ON CONFLICT (owner_id,admin_id,capability) DO NOTHING`,
			ownerID, adminID, string(capability), granted, principal.UserID)
		if err != nil {
			return fmt.Errorf("initialize record grant: %w", err)
		}
		if inserted.RowsAffected() == 1 {
			if err := tx.QueryRow(ctx, `SELECT owner_id,admin_id,capability,granted,granted_by,version,updated_at
FROM record_admin_grants WHERE owner_id=$1 AND admin_id=$2 AND capability=$3`,
				ownerID, adminID, string(capability)).Scan(
				&value.OwnerID, &value.AdminID, &value.Capability, &value.Granted, &value.GrantedBy, &value.Version, &value.UpdatedAt); err != nil {
				return fmt.Errorf("read initialized record grant: %w", err)
			}
			if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordGrantUpdate, recordAuditTarget(ownerID, ""), map[string]any{
				"owner_id": ownerID, "admin_id": adminID, "capability": string(capability), "granted": granted,
			}); err != nil {
				return fmt.Errorf("audit record grant update: %w", err)
			}
			return nil
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT granted FROM record_admin_grants
WHERE owner_id=$1 AND admin_id=$2 AND capability=$3 FOR UPDATE`, ownerID, adminID, string(capability)).Scan(&current); err != nil {
			return fmt.Errorf("lock record grant: %w", err)
		}
		if current != granted {
			if err := tx.QueryRow(ctx, `UPDATE record_admin_grants SET granted=$4,granted_by=$5,
  version=version+1,updated_at=clock_timestamp()
WHERE owner_id=$1 AND admin_id=$2 AND capability=$3
RETURNING owner_id,admin_id,capability,granted,granted_by,version,updated_at`,
				ownerID, adminID, string(capability), granted, principal.UserID).Scan(
				&value.OwnerID, &value.AdminID, &value.Capability, &value.Granted, &value.GrantedBy, &value.Version, &value.UpdatedAt); err != nil {
				return fmt.Errorf("update record grant: %w", err)
			}
			if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordGrantUpdate, recordAuditTarget(ownerID, ""), map[string]any{
				"owner_id": ownerID, "admin_id": adminID, "capability": string(capability), "granted": granted,
			}); err != nil {
				return fmt.Errorf("audit record grant update: %w", err)
			}
			return nil
		}
		return tx.QueryRow(ctx, `SELECT owner_id,admin_id,capability,granted,granted_by,version,updated_at
FROM record_admin_grants WHERE owner_id=$1 AND admin_id=$2 AND capability=$3`,
			ownerID, adminID, string(capability)).Scan(
			&value.OwnerID, &value.AdminID, &value.Capability, &value.Granted, &value.GrantedBy, &value.Version, &value.UpdatedAt)
	})
	if err != nil {
		return record.AdminGrant{}, err
	}
	return value, nil
}

func (s *Store) GetAdminRecordSettings(ctx context.Context, principal identity.Principal, ownerID string) (app.RecordAdminSettings, error) {
	if !domain.ValidUserID(ownerID) {
		return app.RecordAdminSettings{}, domain.InvalidArgument("owner_id", "record owner is invalid")
	}
	settings := app.RecordAdminSettings{
		Consents: []record.Consent{
			{OwnerID: ownerID, Purpose: record.PurposeFullRecord},
			{OwnerID: ownerID, Purpose: record.PurposeTrainingContribution},
		},
		Grants: []record.AdminGrant{},
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := recheckRecordAdminInTx(ctx, tx, principal, true); err != nil {
			return err
		}
		if err := requireRecordOwnerExistsTx(ctx, tx, ownerID); err != nil {
			return err
		}
		purposeIndexes := map[record.Purpose]int{
			record.PurposeFullRecord:           0,
			record.PurposeTrainingContribution: 1,
		}
		rows, err := tx.Query(ctx, `SELECT purpose,enabled,version,updated_at FROM record_consents
WHERE owner_id=$1 ORDER BY purpose FOR SHARE`, ownerID)
		if err != nil {
			return fmt.Errorf("list admin record consents: %w", err)
		}
		for rows.Next() {
			var purpose string
			var value record.Consent
			if err := rows.Scan(&purpose, &value.Enabled, &value.Version, &value.UpdatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan admin record consent: %w", err)
			}
			index, ok := purposeIndexes[record.Purpose(purpose)]
			if !ok {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "stored record purpose is invalid")
			}
			settings.Consents[index].Enabled = value.Enabled
			settings.Consents[index].Version = value.Version
			settings.Consents[index].UpdatedAt = value.UpdatedAt
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read admin record consent rows: %w", err)
		}
		rows.Close()
		if err := ensureRecordQuotaTx(ctx, tx, ownerID); err != nil {
			return err
		}
		if err := scanRecordQuota(tx.QueryRow(ctx, `SELECT owner_id,retention_days,limit_bytes,used_bytes,
reserved_bytes,version,updated_at FROM record_quotas WHERE owner_id=$1 FOR SHARE`, ownerID), &settings.Quota); err != nil {
			return err
		}
		grants, err := tx.Query(ctx, `SELECT owner_id,admin_id,capability,granted,granted_by,version,updated_at
FROM record_admin_grants WHERE owner_id=$1 ORDER BY admin_id,capability FOR SHARE`, ownerID)
		if err != nil {
			return fmt.Errorf("list admin record grants: %w", err)
		}
		defer grants.Close()
		for grants.Next() {
			var value record.AdminGrant
			if err := grants.Scan(&value.OwnerID, &value.AdminID, &value.Capability, &value.Granted,
				&value.GrantedBy, &value.Version, &value.UpdatedAt); err != nil {
				return fmt.Errorf("scan admin record grant: %w", err)
			}
			settings.Grants = append(settings.Grants, value)
		}
		if err := grants.Err(); err != nil {
			return fmt.Errorf("read admin record grant rows: %w", err)
		}
		grants.Close()
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordSettingsRead, recordAuditTarget(ownerID, ""), map[string]any{
			"owner_id": ownerID, "capability": "settings",
		}); err != nil {
			return fmt.Errorf("audit admin record settings read: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.RecordAdminSettings{}, err
	}
	return settings, nil
}

func (s *Store) GetRecordAdminAccess(ctx context.Context, principal identity.Principal, ownerID string) (app.RecordAdminAccess, error) {
	if !domain.ValidUserID(ownerID) {
		return app.RecordAdminAccess{}, domain.InvalidArgument("owner_id", "record owner is invalid")
	}
	value := app.RecordAdminAccess{
		Consents: []record.Consent{
			{OwnerID: ownerID, Purpose: record.PurposeFullRecord},
			{OwnerID: ownerID, Purpose: record.PurposeTrainingContribution},
		},
		Grants: []record.AdminGrant{},
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := recheckRecordAdminInTx(ctx, tx, principal, false); err != nil {
			return err
		}
		if err := requireRecordOwnerExistsTx(ctx, tx, ownerID); err != nil {
			return err
		}
		purposeIndexes := map[record.Purpose]int{
			record.PurposeFullRecord: 0, record.PurposeTrainingContribution: 1,
		}
		rows, err := tx.Query(ctx, `SELECT purpose,enabled,version,updated_at FROM record_consents
WHERE owner_id=$1 ORDER BY purpose FOR SHARE`, ownerID)
		if err != nil {
			return fmt.Errorf("list record access consents: %w", err)
		}
		for rows.Next() {
			var purpose string
			var consent record.Consent
			if err := rows.Scan(&purpose, &consent.Enabled, &consent.Version, &consent.UpdatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan record access consent: %w", err)
			}
			index, ok := purposeIndexes[record.Purpose(purpose)]
			if !ok {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "stored record purpose is invalid")
			}
			consent.OwnerID = ownerID
			consent.Purpose = record.Purpose(purpose)
			value.Consents[index] = consent
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read record access consents: %w", err)
		}
		rows.Close()
		for _, capability := range []record.AdminCapability{
			record.CapabilityView, record.CapabilityAttachment, record.CapabilityExport, record.CapabilityDelete,
		} {
			var granted bool
			err := tx.QueryRow(ctx, `SELECT granted FROM record_admin_grants
WHERE owner_id=$1 AND admin_id=$2 AND capability=$3`, ownerID, principal.UserID, string(capability)).Scan(&granted)
			if errors.Is(err, pgx.ErrNoRows) {
				granted = false
			} else if err != nil {
				return fmt.Errorf("read current administrator record grant: %w", err)
			}
			value.Grants = append(value.Grants, record.AdminGrant{
				OwnerID: ownerID, AdminID: principal.UserID, Capability: capability, Granted: granted,
			})
		}
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordAccessRead, recordAuditTarget(ownerID, ""), map[string]any{
			"owner_id": ownerID, "capability": "self_grants",
		}); err != nil {
			return fmt.Errorf("audit record access settings read: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.RecordAdminAccess{}, err
	}
	return value, nil
}

func requireRecordOwnerExistsTx(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, ownerID).Scan(&exists); err != nil {
		return fmt.Errorf("check record owner: %w", err)
	}
	if !exists {
		return domain.NewError(domain.CodeNotFound, "record owner was not found")
	}
	return nil
}

func (s *Store) ListRecordMetadata(ctx context.Context, principal identity.Principal, ownerID string, query record.Query) (record.Page, error) {
	if !domain.ValidUserID(ownerID) || (query.Purpose != "" && !query.Purpose.Valid()) ||
		(query.BeforeID != "" && !record.ValidRecordID(query.BeforeID)) || query.Limit < 1 || query.Limit > 100 {
		return record.Page{}, domain.InvalidArgument("record_query", "record query is invalid")
	}
	page := record.Page{Items: make([]record.RecordMetadata, 0, query.Limit)}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		isOwner := principal.UserID == ownerID
		if isOwner {
			if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
				return err
			}
			if query.Purpose.Valid() {
				if err := requireRecordConsentTx(ctx, tx, ownerID, query.Purpose); err != nil {
					return err
				}
			}
		} else {
			if err := recheckRecordAdminInTx(ctx, tx, principal, false); err != nil {
				return err
			}
			if err := requireRecordAdminGrantTx(ctx, tx, ownerID, principal.UserID, record.CapabilityView); err != nil {
				return err
			}
		}
		limit := query.Limit + 1
		var rows pgx.Rows
		var err error
		if isOwner {
			rows, err = tx.Query(ctx, `SELECT e.id,e.owner_id,e.purpose,e.item_kind,e.consent_version,e.created_at,
e.expires_at,e.payload_bytes,e.attachment_bytes,e.attachment_count,e.state
FROM record_entries e
JOIN record_consents c ON c.owner_id=e.owner_id AND c.purpose=e.purpose AND c.enabled
WHERE e.owner_id=$1 AND e.state='active' AND e.expires_at > clock_timestamp()
  AND ($2='' OR e.purpose=$2)
  AND ($3='' OR EXISTS (SELECT 1 FROM record_entries cursor WHERE cursor.id=$3 AND cursor.owner_id=$1
    AND (e.created_at,e.id) < (cursor.created_at,cursor.id)))
ORDER BY e.created_at DESC,e.id DESC LIMIT $4 FOR SHARE OF e,c`, ownerID, string(query.Purpose), query.BeforeID, limit)
		} else {
			rows, err = tx.Query(ctx, `SELECT e.id,e.owner_id,e.purpose,e.item_kind,e.consent_version,e.created_at,
e.expires_at,e.payload_bytes,e.attachment_bytes,e.attachment_count,e.state
FROM record_entries e
JOIN record_consents c ON c.owner_id=e.owner_id AND c.purpose=e.purpose AND c.enabled
JOIN record_admin_grants g ON g.owner_id=e.owner_id AND g.admin_id=$2 AND g.capability='view' AND g.granted
WHERE e.owner_id=$1 AND e.state='active' AND e.expires_at > clock_timestamp()
  AND ($3='' OR e.purpose=$3)
  AND ($4='' OR EXISTS (SELECT 1 FROM record_entries cursor WHERE cursor.id=$4 AND cursor.owner_id=$1
    AND (e.created_at,e.id) < (cursor.created_at,cursor.id)))
ORDER BY e.created_at DESC,e.id DESC LIMIT $5 FOR SHARE OF e,c,g`, ownerID, principal.UserID,
				string(query.Purpose), query.BeforeID, limit)
		}
		if err != nil {
			return fmt.Errorf("list record metadata: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value record.RecordMetadata
			if err := rows.Scan(&value.ID, &value.OwnerID, &value.Purpose, &value.Kind, &value.ConsentVersion,
				&value.CreatedAt, &value.ExpiresAt, &value.PayloadBytes, &value.AttachmentBytes,
				&value.AttachmentCount, &value.State); err != nil {
				return fmt.Errorf("scan record metadata: %w", err)
			}
			page.Items = append(page.Items, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read record metadata: %w", err)
		}
		rows.Close()
		if len(page.Items) > int(query.Limit) {
			page.Items = page.Items[:query.Limit]
			page.NextBefore = page.Items[len(page.Items)-1].ID
		}
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordList, recordAuditTarget(ownerID, ""), map[string]any{
			"owner_id": ownerID, "purpose": string(query.Purpose), "count": len(page.Items), "limit": query.Limit,
		}); err != nil {
			return fmt.Errorf("audit record metadata read: %w", err)
		}
		return nil
	})
	if err != nil {
		return record.Page{}, err
	}
	return page, nil
}

func (s *Store) ReadRecord(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.Content, error) {
	return s.readRecordWithGrant(ctx, principal, ownerID, recordID, record.CapabilityView)
}

func (s *Store) ExportRecord(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.Content, error) {
	return s.readRecordWithGrant(ctx, principal, ownerID, recordID, record.CapabilityExport)
}

func (s *Store) readRecordWithGrant(ctx context.Context, principal identity.Principal, ownerID, recordID string, capability record.AdminCapability) (record.Content, error) {
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return record.Content{}, domain.InvalidArgument("record", "record identity is invalid")
	}
	var value record.Content
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		metadata, err := lockRecordForAccessTx(ctx, tx, principal, ownerID, recordID, []record.AdminCapability{capability})
		if err != nil {
			return err
		}
		value.Metadata = metadata
		query := `SELECT body FROM record_turn_bodies WHERE record_id=$1`
		if metadata.Kind == record.ItemSample {
			query = `SELECT body FROM record_sample_bodies WHERE record_id=$1`
		}
		if err := tx.QueryRow(ctx, query, recordID).Scan(&value.Payload); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record was not found")
			}
			return fmt.Errorf("read record body: %w", err)
		}
		action := audit.ActionRecordRead
		if capability == record.CapabilityExport {
			action = audit.ActionRecordExport
		}
		if err := writeRecordAudit(ctx, tx, principal, action, recordAuditTarget(ownerID, recordID), map[string]any{
			"owner_id": ownerID, "record_id": recordID, "purpose": string(metadata.Purpose), "capability": string(capability),
		}); err != nil {
			return fmt.Errorf("audit record body read: %w", err)
		}
		return nil
	})
	if err != nil {
		return record.Content{}, err
	}
	return value, nil
}

func (s *Store) ReadRecordAttachment(ctx context.Context, principal identity.Principal, ownerID, recordID, attachmentID string) (record.AttachmentContent, error) {
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) || !record.ValidAttachmentID(attachmentID) {
		return record.AttachmentContent{}, domain.InvalidArgument("record_attachment", "record attachment identity is invalid")
	}
	var value record.AttachmentContent
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRecordForAccessTx(ctx, tx, principal, ownerID, recordID,
			[]record.AdminCapability{record.CapabilityView, record.CapabilityAttachment}); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id,record_id,file_name,media_type,size_bytes,sha256,content
FROM record_attachments WHERE id=$1 AND record_id=$2 AND owner_id=$3`, attachmentID, recordID, ownerID).Scan(
			&value.Metadata.ID, &value.Metadata.RecordID, &value.Metadata.FileName, &value.Metadata.MediaType,
			&value.Metadata.SizeBytes, &value.Metadata.SHA256, &value.Content)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "record attachment was not found")
		}
		if err != nil {
			return fmt.Errorf("read record attachment: %w", err)
		}
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordAttachmentRead, recordAuditTarget(ownerID, recordID), map[string]any{
			"owner_id": ownerID, "record_id": recordID, "attachment_id": attachmentID, "capability": "attachment",
		}); err != nil {
			return fmt.Errorf("audit record attachment read: %w", err)
		}
		return nil
	})
	if err != nil {
		return record.AttachmentContent{}, err
	}
	return value, nil
}

func (s *Store) ListRecordAttachments(ctx context.Context, principal identity.Principal, ownerID, recordID string) ([]record.AttachmentMetadata, error) {
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return nil, domain.InvalidArgument("record_attachment", "record attachment identity is invalid")
	}
	values := make([]record.AttachmentMetadata, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockRecordForAccessTx(ctx, tx, principal, ownerID, recordID,
			[]record.AdminCapability{record.CapabilityView, record.CapabilityAttachment}); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,record_id,file_name,media_type,size_bytes,sha256
FROM record_attachments WHERE record_id=$1 AND owner_id=$2 ORDER BY id FOR SHARE`, recordID, ownerID)
		if err != nil {
			return fmt.Errorf("list record attachments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value record.AttachmentMetadata
			if err := rows.Scan(&value.ID, &value.RecordID, &value.FileName, &value.MediaType, &value.SizeBytes, &value.SHA256); err != nil {
				return fmt.Errorf("scan record attachment metadata: %w", err)
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read record attachment metadata: %w", err)
		}
		rows.Close()
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordAttachmentsRead, recordAuditTarget(ownerID, recordID), map[string]any{
			"owner_id": ownerID, "record_id": recordID, "capability": "attachment",
		}); err != nil {
			return fmt.Errorf("audit record attachment metadata read: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (s *Store) RequestRecordDelete(ctx context.Context, principal identity.Principal, ownerID, recordID string) (record.DeleteTombstone, error) {
	if !domain.ValidUserID(ownerID) || !record.ValidRecordID(recordID) {
		return record.DeleteTombstone{}, domain.InvalidArgument("record", "record identity is invalid")
	}
	tombstoneID, err := record.NewTombstoneID()
	if err != nil {
		return record.DeleteTombstone{}, err
	}
	var value record.DeleteTombstone
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		isOwner := principal.UserID == ownerID
		if isOwner {
			if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
				return err
			}
		} else if err := recheckRecordAdminInTx(ctx, tx, principal, false); err != nil {
			return err
		}
		var purpose string
		if err := tx.QueryRow(ctx, `SELECT purpose FROM record_entries WHERE id=$1 AND owner_id=$2`, recordID, ownerID).Scan(&purpose); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record was not found")
			}
			return fmt.Errorf("read record before delete request: %w", err)
		}
		if !isOwner {
			if err := requireRecordAdminGrantTx(ctx, tx, ownerID, principal.UserID, record.CapabilityDelete); err != nil {
				return err
			}
			if err := requireRecordConsentTx(ctx, tx, ownerID, record.Purpose(purpose)); err != nil {
				return err
			}
		}
		var currentPurpose, state string
		if err := tx.QueryRow(ctx, `SELECT purpose,state FROM record_entries
WHERE id=$1 AND owner_id=$2 FOR UPDATE`, recordID, ownerID).Scan(&currentPurpose, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "record was not found")
			}
			return fmt.Errorf("lock record for delete request: %w", err)
		}
		if currentPurpose != purpose {
			return domain.NewError(domain.CodeConflict, "record purpose changed")
		}
		if err := scanDeleteTombstone(tx.QueryRow(ctx, `SELECT id,owner_id,record_id,requested_by,state,attempts,
failure_code,requested_at,completed_at,updated_at FROM record_delete_tombstones WHERE record_id=$1`, recordID), &value); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if state == "active" {
			if _, err := tx.Exec(ctx, `UPDATE record_entries SET state='deleting' WHERE id=$1 AND owner_id=$2`, recordID, ownerID); err != nil {
				return fmt.Errorf("mark record for deletion: %w", err)
			}
		}
		requestedBy := principal.UserID
		if _, err := tx.Exec(ctx, `INSERT INTO record_delete_tombstones
  (id,owner_id,record_id,requested_by,state) VALUES ($1,$2,$3,$4,'pending')`, tombstoneID, ownerID, recordID, requestedBy); err != nil {
			return fmt.Errorf("create record deletion tombstone: %w", err)
		}
		if err := writeRecordAudit(ctx, tx, principal, audit.ActionRecordDelete, recordAuditTarget(ownerID, recordID), map[string]any{
			"owner_id": ownerID, "record_id": recordID, "purpose": purpose,
		}); err != nil {
			return fmt.Errorf("audit record delete request: %w", err)
		}
		return scanDeleteTombstone(tx.QueryRow(ctx, `SELECT id,owner_id,record_id,requested_by,state,attempts,
failure_code,requested_at,completed_at,updated_at FROM record_delete_tombstones WHERE id=$1`, tombstoneID), &value)
	})
	if err != nil {
		return record.DeleteTombstone{}, err
	}
	return value, nil
}

func (s *Store) ProcessRecordDeletes(ctx context.Context, limit int32) (int32, error) {
	if limit < 1 || limit > 100 {
		return 0, domain.InvalidArgument("limit", "record deletion batch limit is invalid")
	}
	if err := s.queueExpiredRecordDeletes(ctx, limit); err != nil {
		return 0, err
	}
	var processed int32
	for processed < limit {
		done, tombstoneID, err := s.processOneRecordDelete(ctx)
		if err != nil {
			if tombstoneID != "" {
				if markErr := s.markRecordDeleteFailure(ctx, tombstoneID); markErr != nil {
					return processed, fmt.Errorf("record delete failed: %w; preserve tombstone: %w", err, markErr)
				}
			}
			return processed, err
		}
		if !done {
			break
		}
		processed++
	}
	return processed, nil
}

func (s *Store) queueExpiredRecordDeletes(ctx context.Context, limit int32) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.id,e.owner_id FROM record_entries e
WHERE e.state='active' AND e.expires_at <= clock_timestamp()
  AND NOT EXISTS (SELECT 1 FROM record_delete_tombstones d WHERE d.record_id=e.id)
ORDER BY e.expires_at,e.id FOR UPDATE OF e SKIP LOCKED LIMIT $1`, limit)
		if err != nil {
			return fmt.Errorf("select expired records: %w", err)
		}
		type expired struct{ recordID, ownerID string }
		items := make([]expired, 0, limit)
		for rows.Next() {
			var item expired
			if err := rows.Scan(&item.recordID, &item.ownerID); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired record: %w", err)
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read expired records: %w", err)
		}
		rows.Close()
		for _, item := range items {
			tombstoneID, err := record.NewTombstoneID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE record_entries SET state='deleting' WHERE id=$1`, item.recordID); err != nil {
				return fmt.Errorf("mark expired record for deletion: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO record_delete_tombstones
  (id,owner_id,record_id,requested_by,state) VALUES ($1,$2,$3,'system','pending')`, tombstoneID, item.ownerID, item.recordID); err != nil {
				return fmt.Errorf("create expired record tombstone: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) processOneRecordDelete(ctx context.Context) (bool, string, error) {
	var tombstoneID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, recordID string
		var state string
		err := tx.QueryRow(ctx, `SELECT id,owner_id,record_id,state FROM record_delete_tombstones
WHERE state IN ('pending','failed') ORDER BY requested_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(
			&tombstoneID, &ownerID, &recordID, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			tombstoneID = ""
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim record deletion tombstone: %w", err)
		}
		var usedBytes int64
		if err := tx.QueryRow(ctx, `SELECT payload_bytes+attachment_bytes FROM record_entries
WHERE id=$1 AND owner_id=$2 FOR UPDATE`, recordID, ownerID).Scan(&usedBytes); err != nil {
			return fmt.Errorf("lock record for deletion cleanup: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM record_turn_bodies WHERE record_id=$1`, recordID); err != nil {
			return fmt.Errorf("delete record turn body: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM record_sample_bodies WHERE record_id=$1`, recordID); err != nil {
			return fmt.Errorf("delete record sample body: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM record_attachments WHERE record_id=$1 AND owner_id=$2`, recordID, ownerID); err != nil {
			return fmt.Errorf("delete dedicated record attachments: %w", err)
		}
		if usedBytes > 0 {
			result, err := tx.Exec(ctx, `UPDATE record_quotas SET used_bytes=used_bytes-$2,
  updated_at=clock_timestamp() WHERE owner_id=$1 AND used_bytes >= $2`, ownerID, usedBytes)
			if err != nil {
				return fmt.Errorf("release deleted record quota: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeInternal, "record quota usage is inconsistent")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE record_entries SET state='deleted',payload_bytes=0,
attachment_bytes=0,attachment_count=0 WHERE id=$1 AND owner_id=$2`, recordID, ownerID); err != nil {
			return fmt.Errorf("mark record content deleted: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE record_delete_tombstones SET state='completed',failure_code='',
completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, tombstoneID); err != nil {
			return fmt.Errorf("complete record deletion tombstone: %w", err)
		}
		if err := writeRecordAuditEntry(ctx, tx, audit.Entry{
			ActorID: "system", ActorKind: audit.ActorSystem, Action: audit.ActionRecordDeleteComplete,
			Target: recordAuditTarget(ownerID, recordID), Detail: map[string]any{
				"owner_id": ownerID, "record_id": recordID, "tombstone_id": tombstoneID,
			},
		}); err != nil {
			return fmt.Errorf("audit record delete completion: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, tombstoneID, err
	}
	return tombstoneID != "", tombstoneID, nil
}

func (s *Store) markRecordDeleteFailure(ctx context.Context, tombstoneID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE record_delete_tombstones SET state='failed',attempts=attempts+1,
failure_code='cleanup_failed',updated_at=clock_timestamp() WHERE id=$1 AND state IN ('pending','failed')`, tombstoneID)
		if err != nil {
			return fmt.Errorf("record tombstone failure state: %w", err)
		}
		return nil
	})
}

func lockRecordForAccessTx(ctx context.Context, tx pgx.Tx, principal identity.Principal, ownerID, recordID string, capabilities []record.AdminCapability) (record.RecordMetadata, error) {
	if principal.UserID == ownerID {
		if err := authorizeRecordUserInTx(ctx, tx, principal); err != nil {
			return record.RecordMetadata{}, err
		}
		var value record.RecordMetadata
		if err := scanRecordMetadata(tx.QueryRow(ctx, `SELECT id,owner_id,purpose,item_kind,consent_version,created_at,
expires_at,payload_bytes,attachment_bytes,attachment_count,state FROM record_entries
WHERE id=$1 AND owner_id=$2 AND state='active' AND expires_at > clock_timestamp() FOR SHARE`, recordID, ownerID), &value); err != nil {
			return record.RecordMetadata{}, err
		}
		if err := requireRecordConsentTx(ctx, tx, ownerID, value.Purpose); err != nil {
			return record.RecordMetadata{}, err
		}
		return value, nil
	}
	if err := recheckRecordAdminInTx(ctx, tx, principal, false); err != nil {
		return record.RecordMetadata{}, err
	}
	var purpose string
	if err := tx.QueryRow(ctx, `SELECT purpose FROM record_entries WHERE id=$1 AND owner_id=$2`, recordID, ownerID).Scan(&purpose); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return record.RecordMetadata{}, domain.NewError(domain.CodeNotFound, "record was not found")
		}
		return record.RecordMetadata{}, fmt.Errorf("read record authorization identity: %w", err)
	}
	for _, capability := range capabilities {
		if err := requireRecordAdminGrantTx(ctx, tx, ownerID, principal.UserID, capability); err != nil {
			return record.RecordMetadata{}, err
		}
	}
	if err := requireRecordConsentTx(ctx, tx, ownerID, record.Purpose(purpose)); err != nil {
		return record.RecordMetadata{}, err
	}
	var value record.RecordMetadata
	if err := scanRecordMetadata(tx.QueryRow(ctx, `SELECT id,owner_id,purpose,item_kind,consent_version,created_at,
expires_at,payload_bytes,attachment_bytes,attachment_count,state FROM record_entries
WHERE id=$1 AND owner_id=$2 AND state='active' AND expires_at > clock_timestamp() FOR SHARE`, recordID, ownerID), &value); err != nil {
		return record.RecordMetadata{}, err
	}
	return value, nil
}

func requireRecordAdminGrantTx(ctx context.Context, tx pgx.Tx, ownerID, adminID string, capability record.AdminCapability) error {
	var granted bool
	err := tx.QueryRow(ctx, `SELECT granted FROM record_admin_grants
WHERE owner_id=$1 AND admin_id=$2 AND capability=$3 FOR SHARE`, ownerID, adminID, string(capability)).Scan(&granted)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !granted) {
		return domain.NewError(domain.CodeForbidden, "record administrator grant is required")
	}
	if err != nil {
		return fmt.Errorf("check record administrator grant: %w", err)
	}
	return nil
}

func requireRecordConsentTx(ctx context.Context, tx pgx.Tx, ownerID string, purpose record.Purpose) error {
	var enabled bool
	err := tx.QueryRow(ctx, `SELECT enabled FROM record_consents
WHERE owner_id=$1 AND purpose=$2 FOR SHARE`, ownerID, string(purpose)).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
		return domain.NewError(domain.CodeForbidden, "current record consent is required")
	}
	if err != nil {
		return fmt.Errorf("check current record consent: %w", err)
	}
	return nil
}

func scanRecordMetadata(row rowScanner, value *record.RecordMetadata) error {
	err := row.Scan(&value.ID, &value.OwnerID, &value.Purpose, &value.Kind, &value.ConsentVersion,
		&value.CreatedAt, &value.ExpiresAt, &value.PayloadBytes, &value.AttachmentBytes,
		&value.AttachmentCount, &value.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "record was not found")
	}
	if err != nil {
		return fmt.Errorf("scan record metadata: %w", err)
	}
	return nil
}

func scanDeleteTombstone(row rowScanner, value *record.DeleteTombstone) error {
	var completedAt *time.Time
	err := row.Scan(&value.ID, &value.OwnerID, &value.RecordID, &value.RequestedBy, &value.State,
		&value.Attempts, &value.FailureCode, &value.RequestedAt, &completedAt, &value.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgx.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("scan record deletion tombstone: %w", err)
	}
	value.CompletedAt = completedAt
	return nil
}

func writeRecordAudit(ctx context.Context, tx pgx.Tx, principal identity.Principal, action audit.Action, target string, detail map[string]any) error {
	actorKind := audit.ActorSession
	if principal.Kind == identity.KindClient {
		actorKind = audit.ActorClient
	} else if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "record audit actor is invalid")
	}
	return writeRecordAuditEntry(ctx, tx, audit.Entry{
		ActorID: principal.UserID, ActorKind: actorKind, Action: action, Target: target, Detail: detail,
	})
}

func writeRecordAuditEntry(ctx context.Context, tx pgx.Tx, entry audit.Entry) error {
	if !entry.Action.Valid() {
		return domain.InvalidArgument("audit", "record audit action is invalid")
	}
	params, err := auditParams(entry)
	if err != nil {
		return err
	}
	if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, params); err != nil {
		return fmt.Errorf("write record audit log: %w", err)
	}
	return nil
}

func recordAuditTarget(ownerID, recordID string) string {
	if recordID == "" {
		return "record-owner:" + ownerID
	}
	return "record-owner:" + ownerID + "/record:" + recordID
}
