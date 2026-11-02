package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetPersonalObject(ctx context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.Object{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return personal.Object{}, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if err := personal.ValidateObjectIdentifier(objectType, objectID); err != nil {
		return personal.Object{}, err
	}

	var result personal.Object
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		var revision int64
		var deleted bool
		var body []byte
		err = tx.QueryRow(ctx, `SELECT v.revision, v.is_deleted, p.body
FROM account_object_versions v
LEFT JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id=$4
FOR SHARE OF v`, instanceID, principal.UserID, objectType, objectID).Scan(&revision, &deleted, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "personal object not found")
		}
		if err != nil {
			return fmt.Errorf("read personal object: %w", err)
		}
		if deleted || len(body) == 0 {
			return domain.NewError(domain.CodeNotFound, "personal object not found")
		}
		if revision < 1 || !json.Valid(body) || personal.ValidateObjectBody(objectType, objectID, body) != nil {
			return domain.NewError(domain.CodeInternal, "personal object is invalid")
		}
		result = personal.Object{
			ObjectType: objectType, ObjectID: objectID, Revision: revision,
			Body: append(json.RawMessage(nil), body...),
		}
		return nil
	})
	if err != nil {
		return personal.Object{}, err
	}
	return result, nil
}
