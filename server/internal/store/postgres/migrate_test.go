package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestCheckSchemaVersion(t *testing.T) {
	ctx := context.Background()
	getVersions := func(current, latest int64, err error) func(context.Context) (int64, int64, error) {
		return func(context.Context) (int64, int64, error) { return current, latest, err }
	}
	if err := checkSchemaVersion(ctx, getVersions(1, 1, nil)); err != nil {
		t.Fatalf("matching schema version: %v", err)
	}
	if err := checkSchemaVersion(ctx, getVersions(0, 1, nil)); err == nil || !strings.Contains(err.Error(), "portal migrate up") {
		t.Fatalf("mismatched schema error = %v", err)
	}
	queryErr := errors.New("query failed")
	if err := checkSchemaVersion(ctx, getVersions(0, 0, queryErr)); !errors.Is(err, queryErr) {
		t.Fatalf("version lookup error was not wrapped: %v", err)
	}
}

func TestApplyMigrations(t *testing.T) {
	ctx := context.Background()
	if err := applyMigrations(ctx, func(context.Context) ([]*goose.MigrationResult, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("successful migration: %v", err)
	}
	migrationErr := errors.New("up failed")
	if err := applyMigrations(ctx, func(context.Context) ([]*goose.MigrationResult, error) {
		return nil, migrationErr
	}); !errors.Is(err, migrationErr) {
		t.Fatalf("migration error was not wrapped: %v", err)
	}
}
