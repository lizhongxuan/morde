package postgres

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/codex"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) DiscoverCodexSession(ctx context.Context, principal identity.Principal, request codex.Discover) (codex.Session, error) {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidClientInstallationID(principal.ClientInstallationID) ||
		principal.Kind != identity.KindClient || !domain.ValidTokenID(principal.TokenID) {
		return codex.Session{}, domain.NewError(domain.CodeForbidden, "active Codex client installation is required")
	}
	id, err := domain.NewCodexSessionID()
	if err != nil {
		return codex.Session{}, err
	}
	scopes := codexScopeStrings(request.Scopes)
	var value codex.Session
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkCodexInstallation(ctx, tx, principal.UserID, principal.ClientInstallationID); err != nil {
			return err
		}
		var providerUpdatedAt pgtype.Timestamptz
		if request.UpdatedAt != nil {
			providerUpdatedAt = pgtype.Timestamptz{Time: request.UpdatedAt.UTC(), Valid: true}
		}
		var createdAt, updatedAt, storedProviderUpdatedAt pgtype.Timestamptz
		var storedStatus string
		if err := tx.QueryRow(ctx, `INSERT INTO codex_sessions (
id,owner_id,installation_id,profile_key,thread_id,title,cwd_label,provider_updated_at,status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (owner_id,installation_id,profile_key,thread_id) DO UPDATE SET
  title=EXCLUDED.title, cwd_label=EXCLUDED.cwd_label,
  provider_updated_at=COALESCE(EXCLUDED.provider_updated_at,codex_sessions.provider_updated_at),
  status=EXCLUDED.status, updated_at=statement_timestamp()
		RETURNING id,owner_id,installation_id,profile_key,thread_id,title,cwd_label,provider_updated_at,status,
          last_sequence,created_at,updated_at`, id, principal.UserID, principal.ClientInstallationID,
			request.ProfileKey, request.ThreadID, request.Title, request.CWDLabel, providerUpdatedAt, string(request.Status)).Scan(
			&value.ID, &value.OwnerID, &value.InstallationID, &value.ProfileKey, &value.ThreadID, &value.Title, &value.CWDLabel,
			&storedProviderUpdatedAt, &storedStatus, &value.LastSequence, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("upsert Codex session mapping: %w", err)
		}
		value.CreatedAt, value.UpdatedAt, value.Status = createdAt.Time, updatedAt.Time, codex.SessionStatus(storedStatus)
		if storedProviderUpdatedAt.Valid {
			storedAt := storedProviderUpdatedAt.Time
			value.ProviderUpdatedAt = &storedAt
		}
		var previousScopes []string
		var previousGrantVersion int64
		var previousRevokedAt pgtype.Timestamptz
		previousGrantFound := true
		err := tx.QueryRow(ctx, `SELECT scopes,grant_version,revoked_at FROM codex_session_grants
WHERE session_id=$1 AND owner_id=$2 AND installation_id=$3 FOR UPDATE`, value.ID, value.OwnerID, value.InstallationID).
			Scan(&previousScopes, &previousGrantVersion, &previousRevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			previousGrantFound = false
		} else if err != nil {
			return fmt.Errorf("read current Codex grant: %w", err)
		}
		revoking := len(scopes) == 0
		grantChanged := !previousGrantFound || !sameCodexScopes(previousScopes, scopes) || previousRevokedAt.Valid != revoking
		var grantVersion int64
		var storedScopes []string
		if err := tx.QueryRow(ctx, `INSERT INTO codex_session_grants (session_id,owner_id,installation_id,scopes,revoked_at)
VALUES ($1,$2,$3,$4,CASE WHEN cardinality($4::text[])=0 THEN statement_timestamp() ELSE NULL END)
ON CONFLICT (session_id) DO UPDATE SET
  grant_version=codex_session_grants.grant_version + CASE
    WHEN codex_session_grants.scopes IS DISTINCT FROM EXCLUDED.scopes
      OR (codex_session_grants.revoked_at IS NOT NULL AND cardinality(EXCLUDED.scopes)>0)
      OR (codex_session_grants.revoked_at IS NULL AND cardinality(EXCLUDED.scopes)=0)
    THEN 1 ELSE 0 END,
  scopes=EXCLUDED.scopes,
  revoked_at=CASE WHEN cardinality(EXCLUDED.scopes)=0 THEN statement_timestamp() ELSE NULL END,
  updated_at=statement_timestamp()
			RETURNING scopes,grant_version`, value.ID, value.OwnerID, value.InstallationID, scopes).Scan(&storedScopes, &grantVersion); err != nil {
			return fmt.Errorf("upsert Codex session grant: %w", err)
		}
		value.GrantVersion, value.Scopes = grantVersion, codexScopes(storedScopes)
		rows, err := tx.Query(ctx, `UPDATE codex_commands SET
status=CASE WHEN status='waiting_pc' THEN 'failed' ELSE 'unknown' END,
result_code='grant_changed',status_version=status_version+1,updated_at=statement_timestamp()
WHERE session_id=$1 AND owner_id=$2 AND installation_id=$3
  AND status IN ('waiting_pc','claimed','forwarded','running','waiting_approval') AND grant_version<>$4
RETURNING id,type,COALESCE(payload->>'decision',''),status,status_version`, value.ID, value.OwnerID, value.InstallationID, grantVersion)
		if err != nil {
			return fmt.Errorf("invalidate stale Codex commands: %w", err)
		}
		type invalidatedCommand struct {
			id, commandType, decision, status string
			statusVersion                     int64
		}
		var invalidated []invalidatedCommand
		for rows.Next() {
			var command invalidatedCommand
			if err := rows.Scan(&command.id, &command.commandType, &command.decision, &command.status, &command.statusVersion); err != nil {
				rows.Close()
				return fmt.Errorf("read invalidated Codex command: %w", err)
			}
			invalidated = append(invalidated, command)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read invalidated Codex commands: %w", err)
		}
		rows.Close()
		for _, command := range invalidated {
			if err := insertCodexCommandStatusEvent(ctx, tx, value.ID, value.OwnerID, value.InstallationID,
				command.id, codex.CommandType(command.commandType), codex.CommandStatus(command.status), command.statusVersion, "grant_changed"); err != nil {
				return err
			}
			if err := insertCodexAudit(ctx, tx, audit.Entry{
				ActorID: principal.UserID, ActorKind: audit.ActorClient, Action: audit.ActionCodexCommandResult, Target: value.ID,
				Detail: map[string]any{"installation_id": value.InstallationID, "command_id": command.id,
					"status": command.status},
			}); err != nil {
				return err
			}
			if command.commandType == string(codex.CommandApproval) {
				if err := insertCodexAudit(ctx, tx, audit.Entry{
					ActorID: principal.UserID, ActorKind: audit.ActorClient, Action: audit.ActionCodexApprovalResult, Target: value.ID,
					Detail: map[string]any{"installation_id": value.InstallationID, "command_id": command.id,
						"decision": command.decision, "status": command.status},
				}); err != nil {
					return err
				}
			}
		}
		if grantChanged {
			grantStatus := "active"
			if revoking {
				grantStatus = "revoked"
			}
			if err := insertCodexAudit(ctx, tx, audit.Entry{
				ActorID: principal.UserID, ActorKind: audit.ActorClient, Action: audit.ActionCodexSessionGrant, Target: value.ID,
				Detail: map[string]any{"installation_id": value.InstallationID, "scopes": storedScopes, "status": grantStatus},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return value, err
}

func (s *Store) ListCodexSessions(ctx context.Context, ownerID, installationID, cursor string, limit int32) (codex.SessionPage, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidClientInstallationID(installationID) {
		return codex.SessionPage{}, domain.NewError(domain.CodeForbidden, "Codex installation access is not authorized")
	}
	var active bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM client_installations WHERE id=$1 AND owner_id=$2 AND revoked_at IS NULL
    AND platform='desktop' AND (expires_at IS NULL OR expires_at>statement_timestamp())
)`, installationID, ownerID).Scan(&active); err != nil {
		return codex.SessionPage{}, fmt.Errorf("check Codex installation: %w", err)
	}
	if !active {
		return codex.SessionPage{}, domain.NewError(domain.CodeForbidden, "Codex installation access is not authorized")
	}
	rows, err := s.q.CodexListSessions(ctx, sqlcgen.CodexListSessionsParams{
		OwnerID: ownerID, InstallationID: installationID, ID: cursor, Limit: limit + 1,
	})
	if err != nil {
		return codex.SessionPage{}, fmt.Errorf("list Codex sessions: %w", err)
	}
	page := codex.SessionPage{Items: make([]codex.Session, 0, limit)}
	if int32(len(rows)) > limit {
		cursorValue := rows[limit-1].ID
		page.NextCursor = &cursorValue
		rows = rows[:limit]
	}
	for _, row := range rows {
		page.Items = append(page.Items, codexSessionFromQuery(row))
	}
	return page, nil
}

func (s *Store) GetCodexEvents(ctx context.Context, ownerID, sessionID string, requiredScopes []codex.Scope, afterSequence int64, limit int32) (codex.EventPage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return codex.EventPage{}, fmt.Errorf("begin Codex event page snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := sqlcgen.New(tx)
	row, err := queries.CodexGetSessionForRead(ctx, sqlcgen.CodexGetSessionForReadParams{
		OwnerID: ownerID, ID: sessionID, Column3: codexScopeStrings(requiredScopes),
	})
	if err != nil {
		return codex.EventPage{}, storeError("get Codex session", "Codex session", err)
	}
	if afterSequence > row.LastSequence {
		return codex.EventPage{}, domain.InvalidArgument("after_sequence", "Codex event cursor is ahead of the session")
	}
	items, err := queries.CodexListEvents(ctx, sqlcgen.CodexListEventsParams{
		OwnerID: ownerID, SessionID: sessionID, Column3: codexScopeStrings(requiredScopes), Sequence: afterSequence, Limit: limit + 1,
	})
	if err != nil {
		return codex.EventPage{}, fmt.Errorf("list Codex events: %w", err)
	}
	page := codex.EventPage{Items: make([]codex.Event, 0, limit), CurrentSequence: row.LastSequence}
	if int32(len(items)) > limit {
		cursorValue := fmt.Sprint(items[limit-1].Sequence)
		page.NextCursor = &cursorValue
		items = items[:limit]
	}
	expectedSequence := afterSequence + 1
	for _, item := range items {
		if item.Sequence != expectedSequence {
			return codex.EventPage{}, domain.NewError(domain.CodeEventCursorExpired, "Codex event history has a sequence gap; refresh the session snapshot")
		}
		expectedSequence++
	}
	if len(items) == 0 && afterSequence < row.LastSequence {
		return codex.EventPage{}, domain.NewError(domain.CodeEventCursorExpired, "Codex event history has a sequence gap; refresh the session snapshot")
	}
	if err := tx.Commit(ctx); err != nil {
		return codex.EventPage{}, fmt.Errorf("finish Codex event page snapshot: %w", err)
	}
	for _, item := range items {
		var payload map[string]any
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return codex.EventPage{}, fmt.Errorf("decode stored Codex event payload: %w", err)
		}
		page.Items = append(page.Items, codex.Event{
			SessionID: item.SessionID, Sequence: item.Sequence, ProviderEventID: item.ProviderEventID,
			Kind: codex.EventKind(item.Kind), OccurredAt: item.OccurredAt.Time, TurnID: item.TurnID,
			ItemID: item.ItemID, ParentItemID: item.ParentItemID, Status: item.Status, Payload: payload,
			StoredAt: item.StoredAt.Time,
		})
	}
	return page, nil
}

func (s *Store) CreateCodexCommand(ctx context.Context, ownerID, installationID, sessionID, idempotencyKey string,
	input codex.CommandInput, requestHash []byte) (codex.Command, error) {
	if !domain.ValidUserID(ownerID) || installationID != "" && !domain.ValidClientInstallationID(installationID) ||
		!domain.ValidCodexSessionID(sessionID) || len(requestHash) != 32 {
		return codex.Command{}, domain.InvalidArgument("command", "Codex command request is invalid")
	}
	commandID, err := domain.NewCodexCommandID()
	if err != nil {
		return codex.Command{}, err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return codex.Command{}, fmt.Errorf("encode Codex command: %w", err)
	}
	var result codex.Command
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ownerID+"|"+sessionID+"|"+idempotencyKey); err != nil {
			return fmt.Errorf("serialize Codex command idempotency key: %w", err)
		}
		if installationID != "" {
			if err := checkCodexReaderInstallation(ctx, tx, ownerID, installationID); err != nil {
				return err
			}
		}
		session, scopes, err := getCodexSessionForWrite(ctx, tx, ownerID, "", sessionID, false)
		if err != nil {
			return err
		}
		if !containsCodexScope(scopes, codex.ScopeRead) || !containsCodexScope(scopes, input.RequiredScope()) {
			return domain.NewError(domain.CodeForbidden, "Codex command scope is not granted")
		}
		var existingHash, existingPayload []byte
		var existingType, existingStatus string
		var existingCreatedAt, existingUpdatedAt pgtype.Timestamptz
		var existingClaimedAt pgtype.Timestamptz
		err = tx.QueryRow(ctx, `SELECT id,request_hash,type,status,payload,grant_version,result_code,provider_request_id,created_at,claimed_at,updated_at
FROM codex_commands WHERE owner_id=$1 AND session_id=$2 AND idempotency_key=$3 FOR UPDATE`, ownerID, sessionID, idempotencyKey).Scan(
			&result.ID, &existingHash, &existingType, &existingStatus, &existingPayload, &result.GrantVersion, &result.ResultCode,
			&result.ProviderRequestID, &existingCreatedAt, &existingClaimedAt, &existingUpdatedAt)
		if err == nil {
			if subtle.ConstantTimeCompare(existingHash, requestHash) != 1 {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Codex command")
			}
			result.SessionID, result.InstallationID = session.ID, session.InstallationID
			result.Input = input
			result.Type, result.Status = codex.CommandType(existingType), codex.CommandStatus(existingStatus)
			result.CreatedAt, result.UpdatedAt = existingCreatedAt.Time, existingUpdatedAt.Time
			if existingClaimedAt.Valid {
				result.ClaimedAt = &existingClaimedAt.Time
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read Codex command idempotency record: %w", err)
		}
		var createdAt, updatedAt pgtype.Timestamptz
		var claimedAt pgtype.Timestamptz
		if err := tx.QueryRow(ctx, `INSERT INTO codex_commands (
id,session_id,owner_id,installation_id,idempotency_key,request_hash,type,status,payload,grant_version
) VALUES ($1,$2,$3,$4,$5,$6,$7,'waiting_pc',$8::jsonb,$9)
RETURNING created_at,claimed_at,updated_at`, commandID, sessionID, ownerID, session.InstallationID,
			idempotencyKey, requestHash, string(input.Type), payload, session.GrantVersion).Scan(&createdAt, &claimedAt, &updatedAt); err != nil {
			return fmt.Errorf("create Codex command: %w", err)
		}
		result = codex.Command{ID: commandID, SessionID: sessionID, InstallationID: session.InstallationID,
			Type: input.Type, Status: codex.CommandWaitingPC,
			Input: input, GrantVersion: session.GrantVersion, CreatedAt: createdAt.Time, UpdatedAt: updatedAt.Time}
		if err := insertCodexCommandStatusEvent(ctx, tx, sessionID, ownerID, session.InstallationID,
			commandID, input.Type, result.Status, 1, result.ResultCode); err != nil {
			return err
		}
		commandActorKind := audit.ActorSession
		if installationID != "" {
			commandActorKind = audit.ActorClient
		}
		auditDetail := map[string]any{"installation_id": session.InstallationID, "command_id": commandID,
			"status": string(codex.CommandWaitingPC)}
		if installationID != "" {
			auditDetail["actor_installation_id"] = installationID
		}
		if err := insertCodexAudit(ctx, tx, audit.Entry{
			ActorID: ownerID, ActorKind: commandActorKind, Action: audit.ActionCodexCommandCreate, Target: sessionID,
			Detail: auditDetail,
		}); err != nil {
			return err
		}
		if input.Type == codex.CommandApproval {
			if err := insertCodexAudit(ctx, tx, audit.Entry{
				ActorID: ownerID, ActorKind: commandActorKind, Action: audit.ActionCodexApprovalDecision, Target: sessionID,
				Detail: auditDetailWithDecision(auditDetail, input.Decision),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) ClaimCodexCommand(ctx context.Context, ownerID, installationID string) ([]codex.Command, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidClientInstallationID(installationID) {
		return nil, domain.NewError(domain.CodeForbidden, "active Codex client installation is required")
	}
	var result []codex.Command
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkCodexInstallation(ctx, tx, ownerID, installationID); err != nil {
			return err
		}
		if err := expireStaleCodexCommandClaims(ctx, tx, ownerID, installationID); err != nil {
			return err
		}
		var value codex.Command
		var commandType, commandStatus string
		var input []byte
		var createdAt, claimedAt, updatedAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT c.id,c.session_id,c.installation_id,s.profile_key,s.thread_id,c.grant_version,c.type,c.status,
       c.payload,c.result_code,c.provider_request_id,c.created_at,c.claimed_at,c.updated_at
FROM codex_commands c
JOIN codex_sessions s ON s.id=c.session_id AND s.owner_id=c.owner_id AND s.installation_id=c.installation_id
JOIN codex_session_grants g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations i ON i.id=c.installation_id AND i.owner_id=c.owner_id
WHERE c.owner_id=$1 AND c.installation_id=$2 AND c.status='waiting_pc'
  AND g.revoked_at IS NULL AND c.grant_version=g.grant_version
  AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp())
  AND 'read'=ANY(g.scopes)
  AND CASE c.type WHEN 'send' THEN 'send'=ANY(g.scopes)
                  WHEN 'answer_question' THEN 'send'=ANY(g.scopes)
                  WHEN 'interrupt' THEN 'interrupt'=ANY(g.scopes)
                  WHEN 'approval' THEN 'approval'=ANY(g.scopes) ELSE false END
ORDER BY c.created_at,c.id
LIMIT 1 FOR UPDATE OF c SKIP LOCKED`, ownerID, installationID).Scan(
			&value.ID, &value.SessionID, &value.InstallationID, &value.ProfileKey, &value.ThreadID, &value.GrantVersion,
			&commandType, &commandStatus, &input, &value.ResultCode, &value.ProviderRequestID, &createdAt, &claimedAt, &updatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			result = []codex.Command{}
			return nil
		}
		if err != nil {
			return fmt.Errorf("find claimable Codex command: %w", err)
		}
		var claimedTime pgtype.Timestamptz
		var statusVersion int64
		if err := tx.QueryRow(ctx, `UPDATE codex_commands SET status='claimed',status_version=status_version+1,
claimed_at=statement_timestamp(),updated_at=statement_timestamp()
WHERE id=$1 AND status='waiting_pc' RETURNING claimed_at,updated_at,status_version`, value.ID).Scan(&claimedTime, &updatedAt, &statusVersion); err != nil {
			return fmt.Errorf("claim Codex command: %w", err)
		}
		if err := json.Unmarshal(input, &value.Input); err != nil {
			return fmt.Errorf("decode Codex command payload: %w", err)
		}
		value.Type, value.Status = codex.CommandType(commandType), codex.CommandClaimed
		value.CreatedAt, value.ClaimedAt, value.UpdatedAt = createdAt.Time, &claimedTime.Time, updatedAt.Time
		if err := insertCodexCommandStatusEvent(ctx, tx, value.SessionID, ownerID, value.InstallationID,
			value.ID, value.Type, value.Status, statusVersion, value.ResultCode); err != nil {
			return err
		}
		if err := insertCodexAudit(ctx, tx, audit.Entry{
			ActorID: ownerID, ActorKind: audit.ActorClient, Action: audit.ActionCodexCommandClaim, Target: value.SessionID,
			Detail: map[string]any{"installation_id": installationID, "command_id": value.ID, "status": string(value.Status)},
		}); err != nil {
			return err
		}
		result = []codex.Command{value}
		return nil
	})
	return result, err
}

// A claim with no result after 60 seconds is unknown and is never requeued.
func expireStaleCodexCommandClaims(ctx context.Context, tx pgx.Tx, ownerID, installationID string) error {
	rows, err := tx.Query(ctx, `UPDATE codex_commands SET status='unknown',result_code='claim_timeout',
status_version=status_version+1,updated_at=statement_timestamp()
WHERE owner_id=$1 AND installation_id=$2 AND status='claimed'
  AND claimed_at<statement_timestamp()-interval '60 seconds'
RETURNING id,session_id,type,status_version,COALESCE(payload->>'decision','')`, ownerID, installationID)
	if err != nil {
		return fmt.Errorf("expire stale Codex command claims: %w", err)
	}
	type staleClaim struct {
		id, sessionID, commandType, decision string
		statusVersion                        int64
	}
	var stale []staleClaim
	for rows.Next() {
		var command staleClaim
		if err := rows.Scan(&command.id, &command.sessionID, &command.commandType, &command.statusVersion, &command.decision); err != nil {
			rows.Close()
			return fmt.Errorf("read stale Codex command claim: %w", err)
		}
		stale = append(stale, command)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read stale Codex command claims: %w", err)
	}
	rows.Close()
	for _, command := range stale {
		if err := insertCodexCommandStatusEvent(ctx, tx, command.sessionID, ownerID, installationID,
			command.id, codex.CommandType(command.commandType), codex.CommandUnknown, command.statusVersion, "claim_timeout"); err != nil {
			return err
		}
		if err := insertCodexAudit(ctx, tx, audit.Entry{
			ActorID: ownerID, ActorKind: audit.ActorClient, Action: audit.ActionCodexCommandResult, Target: command.sessionID,
			Detail: map[string]any{"installation_id": installationID, "command_id": command.id,
				"status": string(codex.CommandUnknown), "result_code": "claim_timeout"},
		}); err != nil {
			return err
		}
		if command.commandType == string(codex.CommandApproval) {
			if err := insertCodexAudit(ctx, tx, audit.Entry{
				ActorID: ownerID, ActorKind: audit.ActorClient, Action: audit.ActionCodexApprovalResult, Target: command.sessionID,
				Detail: map[string]any{"installation_id": installationID, "command_id": command.id,
					"decision": command.decision, "status": string(codex.CommandUnknown), "result_code": "claim_timeout"},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ApplyCodexCommandResult(ctx context.Context, ownerID, installationID, commandID string, result codex.CommandResult) (codex.Command, error) {
	var value codex.Command
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkCodexInstallation(ctx, tx, ownerID, installationID); err != nil {
			return err
		}
		var sessionID string
		var commandType string
		if err := tx.QueryRow(ctx, `SELECT session_id,type FROM codex_commands
WHERE id=$1 AND owner_id=$2 AND installation_id=$3`, commandID, ownerID, installationID).Scan(&sessionID, &commandType); err != nil {
			return storeError("find Codex command for result", "Codex command", err)
		}
		var grantVersion int64
		var scopes []string
		if err := tx.QueryRow(ctx, `SELECT s.id,g.grant_version,g.scopes
FROM codex_sessions s
JOIN codex_session_grants g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations i ON i.id=s.installation_id AND i.owner_id=s.owner_id
WHERE s.id=$1 AND s.owner_id=$2 AND s.installation_id=$3 AND g.revoked_at IS NULL
  AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp())
FOR UPDATE OF s,g`, sessionID, ownerID, installationID).Scan(new(string), &grantVersion, &scopes); err != nil {
			return storeError("authorize Codex command result", "Codex command", err)
		}
		requiredScope := codex.ScopeRead
		switch codex.CommandType(commandType) {
		case codex.CommandSend:
			requiredScope = codex.ScopeSend
		case codex.CommandAnswerQuestion:
			requiredScope = codex.ScopeSend
		case codex.CommandInterrupt:
			requiredScope = codex.ScopeInterrupt
		case codex.CommandApproval:
			requiredScope = codex.ScopeApproval
		default:
			return domain.NewError(domain.CodeInvalidState, "Codex command type is invalid")
		}
		if !containsCodexScope(scopes, codex.ScopeRead) || !containsCodexScope(scopes, requiredScope) {
			return domain.NewError(domain.CodeForbidden, "Codex command scope is not granted")
		}
		var rawPayload []byte
		var claimedAt pgtype.Timestamptz
		var createdAt, updatedAt pgtype.Timestamptz
		var currentStatus string
		err := tx.QueryRow(ctx, `SELECT c.id,c.session_id,c.installation_id,c.grant_version,c.type,c.status,c.payload,
       c.result_code,c.provider_request_id,c.created_at,c.claimed_at,c.updated_at
FROM codex_commands c
WHERE c.id=$1 AND c.owner_id=$2 AND c.installation_id=$3 AND c.session_id=$4
FOR UPDATE`, commandID, ownerID, installationID, sessionID).Scan(
			&value.ID, &value.SessionID, &value.InstallationID, &value.GrantVersion, &commandType, &currentStatus,
			&rawPayload, &value.ResultCode, &value.ProviderRequestID, &createdAt, &claimedAt, &updatedAt)
		if err != nil {
			return storeError("find Codex command for result", "Codex command", err)
		}
		if value.GrantVersion != grantVersion {
			return domain.NewError(domain.CodeNotFound, "Codex command not found")
		}
		value.Type, value.Status = codex.CommandType(commandType), codex.CommandStatus(currentStatus)
		if !codexTransitionAllowed(value.Status, result.Status) {
			return domain.NewError(domain.CodeInvalidState, "Codex command result cannot move to the requested state")
		}
		if value.Status == result.Status {
			if (value.ProviderRequestID != "" && result.ProviderRequestID != "" && value.ProviderRequestID != result.ProviderRequestID) ||
				(value.ResultCode != "" && result.ResultCode != "" && value.ResultCode != result.ResultCode) {
				return domain.NewError(domain.CodeConflict, "Codex command result conflicts with the recorded result")
			}
		}
		if err := json.Unmarshal(rawPayload, &value.Input); err != nil {
			return fmt.Errorf("decode stored Codex command payload: %w", err)
		}
		if value.Status == result.Status && (result.ProviderRequestID == "" || result.ProviderRequestID == value.ProviderRequestID) &&
			(result.ResultCode == "" || result.ResultCode == value.ResultCode) {
			value.CreatedAt, value.UpdatedAt = createdAt.Time, updatedAt.Time
			if claimedAt.Valid {
				value.ClaimedAt = &claimedAt.Time
			}
			return nil
		}
		statusChanged := value.Status != result.Status
		var newClaimedAt pgtype.Timestamptz
		var statusVersion int64
		if claimedAt.Valid {
			newClaimedAt = claimedAt
		}
		var updatedStatus string
		if err := tx.QueryRow(ctx, `UPDATE codex_commands SET status=$2,
  provider_request_id=CASE WHEN $3='' THEN provider_request_id ELSE $3 END,
  result_code=CASE WHEN $4='' THEN result_code ELSE $4 END,
  status_version=status_version+CASE WHEN status<>$2 THEN 1 ELSE 0 END,
  updated_at=statement_timestamp()
WHERE id=$1 RETURNING status,result_code,provider_request_id,claimed_at,updated_at,status_version`, commandID,
			string(result.Status), result.ProviderRequestID, result.ResultCode).Scan(
			&updatedStatus, &value.ResultCode, &value.ProviderRequestID, &newClaimedAt, &updatedAt, &statusVersion); err != nil {
			return fmt.Errorf("update Codex command result: %w", err)
		}
		value.Status = codex.CommandStatus(updatedStatus)
		value.CreatedAt, value.UpdatedAt = createdAt.Time, updatedAt.Time
		if newClaimedAt.Valid {
			value.ClaimedAt = &newClaimedAt.Time
		}
		if statusChanged {
			if err := insertCodexCommandStatusEvent(ctx, tx, value.SessionID, ownerID, installationID,
				commandID, value.Type, value.Status, statusVersion, value.ResultCode); err != nil {
				return err
			}
		}
		if err := insertCodexAudit(ctx, tx, audit.Entry{
			ActorID: ownerID, ActorKind: audit.ActorClient, Action: audit.ActionCodexCommandResult, Target: value.SessionID,
			Detail: map[string]any{"installation_id": installationID, "command_id": commandID, "status": string(value.Status)},
		}); err != nil {
			return err
		}
		if value.Type == codex.CommandApproval {
			if err := insertCodexAudit(ctx, tx, audit.Entry{
				ActorID: ownerID, ActorKind: audit.ActorClient, Action: audit.ActionCodexApprovalResult, Target: value.SessionID,
				Detail: map[string]any{"installation_id": installationID, "command_id": commandID,
					"decision": value.Input.Decision, "status": string(value.Status)},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return value, err
}

func (s *Store) AppendCodexEvents(ctx context.Context, ownerID, installationID, sessionID string, batch codex.EventBatch) (codex.EventBatchAck, error) {
	var result codex.EventBatchAck
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		session, scopes, err := getCodexSessionForWrite(ctx, tx, ownerID, installationID, sessionID, true)
		if err != nil {
			return err
		}
		for _, event := range batch.Events {
			requiredScope := event.RequiredScope()
			if requiredScope == "" || !containsCodexScope(scopes, requiredScope) {
				return domain.NewError(domain.CodeForbidden, "Codex event source scope is not granted")
			}
		}
		result.Items = make([]codex.EventAck, 0, len(batch.Events))
		result.LastSequence = session.LastSequence
		for _, event := range batch.Events {
			var sequence int64
			err := tx.QueryRow(ctx, `SELECT sequence FROM codex_events WHERE session_id=$1 AND provider_event_id=$2`, sessionID, event.ProviderEventID).Scan(&sequence)
			if err == nil {
				result.Items = append(result.Items, codex.EventAck{ProviderEventID: event.ProviderEventID, Sequence: sequence})
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check duplicate Codex provider event: %w", err)
			}
			if err := tx.QueryRow(ctx, `UPDATE codex_sessions SET last_sequence=last_sequence+1,updated_at=statement_timestamp()
WHERE id=$1 AND owner_id=$2 AND installation_id=$3 RETURNING last_sequence`, sessionID, ownerID, installationID).Scan(&sequence); err != nil {
				return fmt.Errorf("advance Codex event sequence: %w", err)
			}
			payload, err := json.Marshal(event.Payload)
			if err != nil {
				return domain.InvalidArgument("events", "Codex event payload is invalid")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO codex_events (
session_id,owner_id,installation_id,sequence,provider_event_id,kind,occurred_at,turn_id,item_id,parent_item_id,status,payload
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`, sessionID, ownerID, installationID, sequence,
				event.ProviderEventID, string(event.Kind), event.OccurredAt.UTC(), event.TurnID, event.ItemID,
				event.ParentItemID, event.Status, payload); err != nil {
				return fmt.Errorf("append Codex event: %w", err)
			}
			result.Items = append(result.Items, codex.EventAck{ProviderEventID: event.ProviderEventID, Sequence: sequence})
			result.LastSequence = sequence
		}
		return nil
	})
	return result, err
}

func insertCodexCommandStatusEvent(ctx context.Context, tx pgx.Tx, sessionID, ownerID, installationID,
	commandID string, commandType codex.CommandType, status codex.CommandStatus, statusVersion int64, resultCode string) error {
	providerEventID := fmt.Sprintf("morde:command:%s:%d:%s", commandID, statusVersion, status)
	var existingSequence int64
	err := tx.QueryRow(ctx, `SELECT sequence FROM codex_events WHERE session_id=$1 AND provider_event_id=$2`,
		sessionID, providerEventID).Scan(&existingSequence)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check duplicate Codex command status event: %w", err)
	}
	event := codex.EventInput{
		ProviderEventID: providerEventID, Kind: codex.EventCommandStatus, OccurredAt: time.Now().UTC(),
		Status: string(status), Payload: map[string]any{
			"command_id": commandID, "type": string(commandType), "status": string(status), "result_code": resultCode,
		},
	}
	if err := event.Validate(); err != nil {
		return domain.InvalidArgument("command_status", "Codex command status event is invalid")
	}
	var sequence int64
	if err := tx.QueryRow(ctx, `UPDATE codex_sessions SET last_sequence=last_sequence+1,updated_at=statement_timestamp()
WHERE id=$1 AND owner_id=$2 AND installation_id=$3 RETURNING last_sequence`, sessionID, ownerID, installationID).Scan(&sequence); err != nil {
		return fmt.Errorf("advance Codex command status sequence: %w", err)
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encode Codex command status payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO codex_events (
session_id,owner_id,installation_id,sequence,provider_event_id,kind,occurred_at,turn_id,item_id,parent_item_id,status,payload
) VALUES ($1,$2,$3,$4,$5,$6,$7,'','','',$8,$9::jsonb)`, sessionID, ownerID, installationID, sequence,
		providerEventID, string(codex.EventCommandStatus), event.OccurredAt, event.Status, payload); err != nil {
		return fmt.Errorf("append Codex command status event: %w", err)
	}
	return nil
}

func (s *Store) DeleteCodexSession(ctx context.Context, ownerID, sessionID string) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var installationID string
		if err := tx.QueryRow(ctx, `SELECT installation_id FROM codex_sessions WHERE owner_id=$1 AND id=$2 FOR UPDATE`, ownerID, sessionID).Scan(&installationID); err != nil {
			return storeError("find Codex session for deletion", "Codex session", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM codex_sessions WHERE owner_id=$1 AND id=$2`, ownerID, sessionID); err != nil {
			return fmt.Errorf("delete Codex mirror session: %w", err)
		}
		return insertCodexAudit(ctx, tx, audit.Entry{
			ActorID: ownerID, ActorKind: audit.ActorSession, Action: audit.ActionCodexSessionDelete, Target: sessionID,
			Detail: map[string]any{"installation_id": installationID, "status": "deleted"},
		})
	})
}

type codexWriteSession struct {
	ID             string
	InstallationID string
	ProfileKey     string
	ThreadID       string
	GrantVersion   int64
	LastSequence   int64
}

func getCodexSessionForWrite(ctx context.Context, tx pgx.Tx, ownerID, installationID, sessionID string, pcOnly bool) (codexWriteSession, []string, error) {
	var value codexWriteSession
	var scopes []string
	query := `SELECT s.id,s.installation_id,s.profile_key,s.thread_id,s.last_sequence,g.grant_version,g.scopes
FROM codex_sessions s
JOIN codex_session_grants g ON g.session_id=s.id AND g.owner_id=s.owner_id AND g.installation_id=s.installation_id
JOIN client_installations i ON i.id=s.installation_id AND i.owner_id=s.owner_id
WHERE s.owner_id=$1 AND s.id=$2 AND g.revoked_at IS NULL AND i.platform='desktop' AND i.revoked_at IS NULL
  AND (i.expires_at IS NULL OR i.expires_at>statement_timestamp())`
	args := []any{ownerID, sessionID}
	if pcOnly {
		query += ` AND s.installation_id=$3`
		args = append(args, installationID)
	}
	query += ` FOR UPDATE OF s,g,i`
	if err := tx.QueryRow(ctx, query, args...).Scan(&value.ID, &value.InstallationID, &value.ProfileKey, &value.ThreadID,
		&value.LastSequence, &value.GrantVersion, &scopes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return codexWriteSession{}, nil, domain.NewError(domain.CodeNotFound, "Codex session not found")
		}
		return codexWriteSession{}, nil, fmt.Errorf("authorize Codex session: %w", err)
	}
	return value, scopes, nil
}

func checkCodexInstallation(ctx context.Context, tx pgx.Tx, ownerID, installationID string) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND platform='desktop'
  AND (expires_at IS NULL OR expires_at>statement_timestamp())
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID, ownerID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return domain.NewError(domain.CodeForbidden, "active Codex client installation is required")
	}
	if err != nil {
		return fmt.Errorf("verify Codex client installation: %w", err)
	}
	return nil
}

func checkCodexReaderInstallation(ctx context.Context, tx pgx.Tx, ownerID, installationID string) error {
	var active bool
	err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND (expires_at IS NULL OR expires_at>statement_timestamp())
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID, ownerID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return domain.NewError(domain.CodeForbidden, "active owner client installation is required")
	}
	if err != nil {
		return fmt.Errorf("verify Codex reader installation: %w", err)
	}
	return nil
}

func auditDetailWithDecision(detail map[string]any, decision string) map[string]any {
	withDecision := make(map[string]any, len(detail)+1)
	for key, value := range detail {
		withDecision[key] = value
	}
	withDecision["decision"] = decision
	return withDecision
}

func codexSessionFromQuery(row sqlcgen.CodexListSessionsRow) codex.Session {
	value := codex.Session{ID: row.ID, OwnerID: row.OwnerID, InstallationID: row.InstallationID, ProfileKey: row.ProfileKey,
		ThreadID: row.ThreadID, Title: row.Title, CWDLabel: row.CwdLabel, Status: codex.SessionStatus(row.Status),
		Scopes: codexScopes(row.Scopes), GrantVersion: row.GrantVersion, LastSequence: row.LastSequence,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if row.ProviderUpdatedAt.Valid {
		updatedAt := row.ProviderUpdatedAt.Time
		value.ProviderUpdatedAt = &updatedAt
	}
	return value
}

func codexScopes(values []string) []codex.Scope {
	result := make([]codex.Scope, 0, len(values))
	for _, value := range values {
		result = append(result, codex.Scope(value))
	}
	return result
}

func codexScopeStrings(values []codex.Scope) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	sort.Strings(result)
	return result
}

func containsCodexScope(values []string, required codex.Scope) bool {
	for _, value := range values {
		if value == string(required) {
			return true
		}
	}
	return false
}

func sameCodexScopes(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func insertCodexAudit(ctx context.Context, tx pgx.Tx, entry audit.Entry) error {
	if !entry.Action.Valid() {
		return domain.InvalidArgument("audit", "Codex audit action is invalid")
	}
	params, err := auditParams(entry)
	if err != nil {
		return fmt.Errorf("encode Codex audit record: %w", err)
	}
	if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, params); err != nil {
		return fmt.Errorf("write Codex lifecycle audit: %w", err)
	}
	return nil
}

func codexTransitionAllowed(current, next codex.CommandStatus) bool {
	if current == next {
		return true
	}
	switch current {
	case codex.CommandClaimed:
		return next == codex.CommandForwarded || next == codex.CommandRunning || next == codex.CommandWaitingApproval ||
			next == codex.CommandCompleted || next == codex.CommandFailed || next == codex.CommandUnknown
	case codex.CommandForwarded:
		return next == codex.CommandRunning || next == codex.CommandWaitingApproval || next == codex.CommandCompleted ||
			next == codex.CommandFailed || next == codex.CommandUnknown
	case codex.CommandRunning:
		return next == codex.CommandWaitingApproval || next == codex.CommandCompleted || next == codex.CommandFailed || next == codex.CommandUnknown
	case codex.CommandWaitingApproval:
		return next == codex.CommandRunning || next == codex.CommandCompleted || next == codex.CommandFailed || next == codex.CommandUnknown
	default:
		return false
	}
}

var _ app.CodexStore = (*Store)(nil)
