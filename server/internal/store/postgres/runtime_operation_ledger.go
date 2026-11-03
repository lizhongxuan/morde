package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	maxRuntimeOperationEvents    = 65536
	maxRuntimeOperationEventData = 8 << 20
	maxRuntimeOperationLedger    = 128 << 20
)

type runtimeOperationHashInput struct {
	Version      int             `json:"version"`
	Sequence     uint64          `json:"sequence"`
	Kind         string          `json:"kind"`
	Data         json.RawMessage `json:"data"`
	PreviousHash string          `json:"previous_hash"`
}

func (s *Store) ListRuntimeOperationEvents(ctx context.Context, principal identity.Principal, namespace string, after uint64, limit int32) (app.RuntimeOperationLedgerPage, error) {
	if err := validateRuntimeLedgerPrincipal(principal, namespace); err != nil {
		return app.RuntimeOperationLedgerPage{}, err
	}
	if after > maxRuntimeOperationEvents || limit < 1 || limit > app.RuntimeOperationLedgerPageLimit {
		return app.RuntimeOperationLedgerPage{}, domain.InvalidArgument("cursor", "runtime operation ledger cursor or page size is invalid")
	}
	if err := s.verifyRuntimeLedgerHost(ctx, s.pool, principal); err != nil {
		return app.RuntimeOperationLedgerPage{}, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT sequence, kind, data_canonical, previous_hash, event_hash
FROM host_runtime_operation_ledger_events
WHERE host_id=$1 AND runtime_generation=$2 AND namespace=$3 AND sequence>$4
ORDER BY sequence ASC LIMIT $5`, principal.HostScope, principal.HostGeneration, namespace, int64(after), limit+1)
	if err != nil {
		return app.RuntimeOperationLedgerPage{}, fmt.Errorf("list Host Runtime operation ledger events: %w", err)
	}
	defer rows.Close()
	page := app.RuntimeOperationLedgerPage{Items: make([]app.RuntimeOperationLedgerEvent, 0, limit)}
	for rows.Next() {
		var event app.RuntimeOperationLedgerEvent
		var data []byte
		if err := rows.Scan(&event.Sequence, &event.Kind, &data, &event.PreviousHash, &event.Hash); err != nil {
			return app.RuntimeOperationLedgerPage{}, fmt.Errorf("scan Host Runtime operation ledger event: %w", err)
		}
		if int32(len(page.Items)) == limit {
			page.HasMore = true
			break
		}
		event.Version = 1
		event.Data = append(json.RawMessage(nil), data...)
		page.Items = append(page.Items, event)
	}
	if err := rows.Err(); err != nil {
		return app.RuntimeOperationLedgerPage{}, fmt.Errorf("read Host Runtime operation ledger events: %w", err)
	}
	return page, nil
}

func (s *Store) AppendRuntimeOperationEvent(ctx context.Context, principal identity.Principal, namespace string, event app.RuntimeOperationLedgerEvent) (app.RuntimeOperationLedgerEvent, error) {
	if err := validateRuntimeLedgerPrincipal(principal, namespace); err != nil {
		return app.RuntimeOperationLedgerEvent{}, err
	}
	if err := validateRuntimeOperationEvent(namespace, event); err != nil {
		return app.RuntimeOperationLedgerEvent{}, domain.InvalidArgument("event", "runtime operation ledger event is invalid")
	}
	var acknowledged app.RuntimeOperationLedgerEvent
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := s.verifyRuntimeLedgerHost(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO host_runtime_operation_ledger_heads (host_id, runtime_generation, namespace)
VALUES ($1, $2, $3) ON CONFLICT (host_id, runtime_generation, namespace) DO NOTHING`,
			principal.HostScope, principal.HostGeneration, namespace); err != nil {
			return fmt.Errorf("initialize Host Runtime operation ledger head: %w", err)
		}
		var eventCount int64
		var headHash string
		var totalBytes int64
		if err := tx.QueryRow(ctx, `
SELECT event_count, head_hash, total_bytes
FROM host_runtime_operation_ledger_heads
WHERE host_id=$1 AND runtime_generation=$2 AND namespace=$3
FOR UPDATE`, principal.HostScope, principal.HostGeneration, namespace).
			Scan(&eventCount, &headHash, &totalBytes); err != nil {
			return fmt.Errorf("lock Host Runtime operation ledger head: %w", err)
		}
		if int64(event.Sequence) <= eventCount {
			var stored app.RuntimeOperationLedgerEvent
			var data []byte
			err := tx.QueryRow(ctx, `
SELECT sequence, kind, data_canonical, previous_hash, event_hash
FROM host_runtime_operation_ledger_events
WHERE host_id=$1 AND runtime_generation=$2 AND namespace=$3 AND sequence=$4`,
				principal.HostScope, principal.HostGeneration, namespace, int64(event.Sequence)).
				Scan(&stored.Sequence, &stored.Kind, &data, &stored.PreviousHash, &stored.Hash)
			if err != nil {
				return fmt.Errorf("load Host Runtime operation ledger replay: %w", err)
			}
			stored.Version = 1
			stored.Data = append(json.RawMessage(nil), data...)
			if !sameRuntimeOperationEvent(stored, event) {
				return domain.NewError(domain.CodeConflict, "runtime operation ledger sequence was reused with different event data")
			}
			acknowledged = stored
			return nil
		}
		if event.Sequence != uint64(eventCount+1) || event.Sequence > maxRuntimeOperationEvents || event.PreviousHash != headHash {
			return domain.NewError(domain.CodeConflict, "runtime operation ledger sequence or hash predecessor is out of date")
		}
		if totalBytes+int64(len(event.Data)) > maxRuntimeOperationLedger {
			return domain.NewError(domain.CodeResultTooLarge, "runtime operation ledger capacity is exhausted")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO host_runtime_operation_ledger_events
(host_id, runtime_generation, namespace, sequence, kind, data, data_canonical, previous_hash, event_hash)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)`,
			principal.HostScope, principal.HostGeneration, namespace, int64(event.Sequence), event.Kind,
			string(event.Data), []byte(event.Data), event.PreviousHash, event.Hash); err != nil {
			return fmt.Errorf("append Host Runtime operation ledger event: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE host_runtime_operation_ledger_heads
SET event_count=$4, head_hash=$5, total_bytes=$6, updated_at=clock_timestamp()
WHERE host_id=$1 AND runtime_generation=$2 AND namespace=$3`,
			principal.HostScope, principal.HostGeneration, namespace, event.Sequence, event.Hash, totalBytes+int64(len(event.Data))); err != nil {
			return fmt.Errorf("advance Host Runtime operation ledger head: %w", err)
		}
		acknowledged = cloneRuntimeOperationEvent(event)
		return nil
	})
	if err != nil {
		return app.RuntimeOperationLedgerEvent{}, err
	}
	return acknowledged, nil
}

func (s *Store) verifyRuntimeLedgerHost(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, principal identity.Principal) error {
	var generation int64
	var state string
	var ownerID string
	var deletedAt pgtype.Timestamptz
	query := `SELECT runtime_generation, state, owner_id, deleted_at FROM hosts WHERE id=$1`
	if _, ok := queryer.(pgx.Tx); ok {
		query += " FOR SHARE"
	}
	if err := queryer.QueryRow(ctx, query, principal.HostScope).Scan(&generation, &state, &ownerID, &deletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
		}
		return fmt.Errorf("verify Host Runtime operation ledger scope: %w", err)
	}
	if generation != principal.HostGeneration || ownerID != principal.UserID || deletedAt.Valid || (state != "creating" && state != "running") {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	return nil
}

func validateRuntimeLedgerPrincipal(principal identity.Principal, namespace string) error {
	if principal.Kind != identity.KindHostReplica || !domain.ValidHostID(principal.HostScope) || !domain.ValidUserID(principal.UserID) || principal.HostGeneration < 1 {
		return domain.NewError(domain.CodeUnauthenticated, "host replica authentication required")
	}
	if namespace != app.RuntimeOperationLedgerCoreNamespace && namespace != app.RuntimeOperationLedgerBrowserNamespace {
		return domain.InvalidArgument("namespace", "runtime operation ledger namespace is invalid")
	}
	return nil
}

func validateRuntimeOperationEvent(namespace string, event app.RuntimeOperationLedgerEvent) error {
	if event.Version != 1 || event.Sequence < 1 || event.Sequence > maxRuntimeOperationEvents || event.Kind == "" || len(event.Kind) > 128 ||
		len(event.Data) == 0 || len(event.Data) > maxRuntimeOperationEventData || !json.Valid(event.Data) ||
		!validOperationHash(event.Hash) || (event.PreviousHash != "" && !validOperationHash(event.PreviousHash)) {
		return errors.New("event fields are invalid")
	}
	if namespace == app.RuntimeOperationLedgerCoreNamespace {
		if event.Kind != "runtime_state" && event.Kind != "operation" {
			return errors.New("core namespace event kind is invalid")
		}
	} else {
		switch event.Kind {
		case "browser.operation", "browser.operation_binding", "browser.operation_completed", "browser.task_context", "browser.target_owned", "browser.context_closed":
		default:
			return errors.New("browser namespace event kind is invalid")
		}
	}
	encoded, err := json.Marshal(runtimeOperationHashInput{
		Version: event.Version, Sequence: event.Sequence, Kind: event.Kind, Data: event.Data, PreviousHash: event.PreviousHash,
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	if event.Hash != hex.EncodeToString(digest[:]) {
		return errors.New("event hash is invalid")
	}
	return nil
}

func validOperationHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sameRuntimeOperationEvent(left, right app.RuntimeOperationLedgerEvent) bool {
	return left.Version == right.Version && left.Sequence == right.Sequence && left.Kind == right.Kind && bytes.Equal(left.Data, right.Data) &&
		left.PreviousHash == right.PreviousHash && left.Hash == right.Hash
}

func cloneRuntimeOperationEvent(event app.RuntimeOperationLedgerEvent) app.RuntimeOperationLedgerEvent {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}

var _ app.RuntimeOperationLedgerService = (*Store)(nil)
