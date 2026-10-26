package postgres

import (
	"context"
	"fmt"

	"morde/server/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		return err
	}
	defer closeProvider()
	return checkSchemaVersion(ctx, provider.GetVersions)
}

func checkSchemaVersion(ctx context.Context, getVersions func(context.Context) (int64, int64, error)) error {
	current, latest, err := getVersions(ctx)
	if err != nil {
		return fmt.Errorf("check database schema version: %w", err)
	}
	if current != latest {
		return fmt.Errorf("database schema version %d does not match latest migration version %d; run portal migrate up", current, latest)
	}
	return nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		return err
	}
	defer closeProvider()
	return applyMigrations(ctx, provider.Up)
}

func applyMigrations(ctx context.Context, up func(context.Context) ([]*goose.MigrationResult, error)) error {
	if _, err := up(ctx); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	return nil
}

func newMigrationProvider(pool *pgxpool.Pool) (*goose.Provider, func(), error) {
	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations.FS,
		goose.WithLogger(goose.NopLogger()),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("initialize goose migration provider: %w", err)
	}
	return provider, func() { _ = provider.Close() }, nil
}
