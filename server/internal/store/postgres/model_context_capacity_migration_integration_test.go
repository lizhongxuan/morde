package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestModelContextCapacityMigrationPreservesLegacyValuesAndGuardsDowngrade(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; model context capacity migration integration test was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_context_capacity_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	config := baseConfig.Copy()
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop model context migration database: %v", err)
		}
		adminPool.Close()
	})

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 207); err != nil {
		closeProvider()
		t.Fatalf("prepare schema before model context migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,context_window_tokens)
VALUES
 ('p1234567',NULL,'legacy model','https://models.example/v1','legacy-model',decode(repeat('01',29),'hex'),32768),
 ('p1234568',NULL,'legacy preset','https://api.openai.com/v1','gpt-6-luna',decode(repeat('02',29),'hex'),500000)`); err != nil {
		closeProvider()
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 208); err != nil {
		closeProvider()
		t.Fatalf("apply model context migration: %v", err)
	}
	closeProvider()
	var effective, override int
	var source string
	if err := pool.QueryRow(ctx, `SELECT context_window_tokens,context_window_override_tokens,context_window_source
FROM model_connections WHERE id='p1234567'`).Scan(&effective, &override, &source); err != nil || effective != 32768 || override != 32768 || source != "legacy_manual" {
		t.Fatalf("legacy capacity migration = effective:%d override:%d source:%q err=%v", effective, override, source, err)
	}
	var catalogVersion string
	if err := pool.QueryRow(ctx, `SELECT context_window_tokens,COALESCE(context_window_override_tokens,0),context_window_source,context_window_catalog_version
FROM model_connections WHERE id='p1234568'`).Scan(&effective, &override, &source, &catalogVersion); err != nil || effective != 1050000 || override != 0 || source != "model_catalog" || catalogVersion != "2026-11-18" {
		t.Fatalf("known preset migration = effective:%d override:%d source:%q catalog:%q err=%v", effective, override, source, catalogVersion, err)
	}
	provider, closeProvider, err = newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET context_window_tokens=1050000,context_window_override_tokens=NULL,
context_window_source='model_catalog',context_window_resolved_at=clock_timestamp(),context_window_catalog_version='2026-11-18'
WHERE id='p1234567'`); err != nil {
		closeProvider()
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 207); err == nil {
		closeProvider()
		t.Fatal("downgrade discarded a context capacity that the old schema cannot represent")
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET context_window_tokens=32768,context_window_override_tokens=32768,
context_window_source='legacy_manual',context_window_resolved_at=NULL,context_window_catalog_version=NULL WHERE id='p1234567'`); err != nil {
		closeProvider()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE model_connections SET context_window_tokens=500000,context_window_override_tokens=500000,
context_window_source='legacy_manual',context_window_resolved_at=NULL,context_window_catalog_version=NULL WHERE id='p1234568'`); err != nil {
		closeProvider()
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 207); err != nil {
		closeProvider()
		t.Fatalf("downgrade while retaining representable capacity: %v", err)
	}
	var legacyValue int
	if err := pool.QueryRow(ctx, `SELECT context_window_tokens FROM model_connections WHERE id='p1234567'`).Scan(&legacyValue); err != nil || legacyValue != 32768 {
		closeProvider()
		t.Fatalf("legacy capacity lost on downgrade: value=%d err=%v", legacyValue, err)
	}
	if _, err := provider.UpTo(ctx, 208); err != nil {
		closeProvider()
		t.Fatalf("reapply model context migration: %v", err)
	}
	closeProvider()
	if err := pool.QueryRow(ctx, `SELECT context_window_tokens,context_window_override_tokens,context_window_source
FROM model_connections WHERE id='p1234567'`).Scan(&effective, &override, &source); err != nil || effective != 32768 || override != 32768 || source != "legacy_manual" {
		t.Fatalf("reapplied legacy capacity = effective:%d override:%d source:%q err=%v", effective, override, source, err)
	}
	if err := pool.QueryRow(ctx, `SELECT context_window_tokens,COALESCE(context_window_override_tokens,0),context_window_source,context_window_catalog_version
FROM model_connections WHERE id='p1234568'`).Scan(&effective, &override, &source, &catalogVersion); err != nil || effective != 1050000 || override != 0 || source != "model_catalog" || catalogVersion != "2026-11-18" {
		t.Fatalf("reapplied known preset = effective:%d override:%d source:%q catalog:%q err=%v", effective, override, source, catalogVersion, err)
	}
}
