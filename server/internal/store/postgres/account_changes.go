package postgres

import (
	"context"
	"fmt"
	"math"

	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5"
)

type accountObjectChange struct {
	OwnerID    string
	ObjectType string
	ObjectID   string
	Operation  string
	MutationID string
}

// Callers record changes after their business writes; this row lock stays held through commit.
func recordAccountObjectChanges(ctx context.Context, tx pgx.Tx, changes ...accountObjectChange) error {
	if len(changes) == 0 {
		return nil
	}
	ownerID := changes[0].OwnerID
	if !domain.ValidUserID(ownerID) {
		return domain.InvalidArgument("owner_id", "account change owner is invalid")
	}
	for _, change := range changes {
		if change.OwnerID != ownerID || change.ObjectType == "" || len(change.ObjectType) > 64 ||
			change.ObjectID == "" || len(change.ObjectID) > 128 || (change.Operation != "upsert" && change.Operation != "delete") {
			return domain.InvalidArgument("change", "account change fields are invalid")
		}
	}
	instanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	lastSequence, err := lockAccountChangeCounter(ctx, tx, instanceID, ownerID)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if lastSequence == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "account change sequence is exhausted")
		}
		var revision int64
		err := tx.QueryRow(ctx, `INSERT INTO account_object_versions
(service_instance_id, owner_id, object_type, object_id, revision, is_deleted)
VALUES ($1::uuid,$2,$3,$4,1,$5)
ON CONFLICT (service_instance_id, owner_id, object_type, object_id) DO UPDATE
SET revision=account_object_versions.revision+1, is_deleted=EXCLUDED.is_deleted, updated_at=clock_timestamp()
RETURNING revision`, instanceID, ownerID, change.ObjectType, change.ObjectID, change.Operation == "delete").Scan(&revision)
		if err != nil {
			return fmt.Errorf("advance account object version: %w", err)
		}
		lastSequence++
		var mutationID any
		if change.MutationID != "" {
			mutationID = change.MutationID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_change_log
(service_instance_id, owner_id, change_sequence, object_type, object_id, revision, operation, mutation_id)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`,
			instanceID, ownerID, lastSequence, change.ObjectType, change.ObjectID, revision, change.Operation, mutationID); err != nil {
			return fmt.Errorf("append account change: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE account_change_counters SET last_sequence=$3, updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2`, instanceID, ownerID, lastSequence); err != nil {
		return fmt.Errorf("advance account change counter: %w", err)
	}
	return nil
}

func lockAccountChangeCounter(ctx context.Context, tx pgx.Tx, instanceID, ownerID string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO account_change_counters (service_instance_id, owner_id, last_sequence)
VALUES ($1::uuid,$2,0) ON CONFLICT (service_instance_id, owner_id) DO NOTHING`, instanceID, ownerID); err != nil {
		return 0, fmt.Errorf("create account change counter: %w", err)
	}
	var lastSequence int64
	if err := tx.QueryRow(ctx, `SELECT last_sequence FROM account_change_counters
WHERE service_instance_id=$1::uuid AND owner_id=$2 FOR UPDATE`, instanceID, ownerID).Scan(&lastSequence); err != nil {
		return 0, fmt.Errorf("lock account change counter: %w", err)
	}
	return lastSequence, nil
}
