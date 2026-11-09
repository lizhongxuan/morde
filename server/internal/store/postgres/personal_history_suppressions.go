package postgres

import (
	"context"
	"fmt"

	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListSuppressedPersonalHistoryMessageIDs(ctx context.Context, ownerID string, messageIDs []string) (map[string]struct{}, error) {
	if !domain.ValidUserID(ownerID) || len(messageIDs) == 0 || len(messageIDs) > 100 {
		return nil, domain.InvalidArgument("message_ids", "personal history message ids are invalid")
	}
	uniqueIDs := make([]string, 0, len(messageIDs))
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if !domain.ValidMessageID(id) {
			return nil, domain.InvalidArgument("message_ids", "personal history message ids are invalid")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}

	suppressed := make(map[string]struct{})
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := personalServiceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT message_id FROM personal_history_suppressions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND message_id=ANY($3::text[])`,
			serviceInstanceID, ownerID, uniqueIDs)
		if err != nil {
			return fmt.Errorf("list suppressed personal history messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var messageID string
			if err := rows.Scan(&messageID); err != nil {
				return fmt.Errorf("scan suppressed personal history message: %w", err)
			}
			suppressed[messageID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate suppressed personal history messages: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return suppressed, nil
}
