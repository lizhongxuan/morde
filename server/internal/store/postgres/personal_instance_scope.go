package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
)

// The shared lock keeps the identity stable until the personal-data write,
// upload chunk, or quota reservation transaction commits.
func personalServiceInstanceIDInTx(ctx context.Context, tx pgx.Tx) (string, error) {
	if expected := app.ExpectedServiceInstance(ctx); expected != "" {
		if err := requireConversationServiceInstance(ctx, tx, expected); err != nil {
			return "", err
		}
		return expected, nil
	}
	return serviceInstanceIDInTx(ctx, tx)
}
