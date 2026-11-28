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

func TestModelMaxOutputCapacityMigrationUsesExactCatalogAndLeavesUnknownRoutesUnknown(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; model max output capacity migration integration test was not run")
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
	databaseName := "morde_max_output_capacity_" + hex.EncodeToString(random[:])
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
			t.Errorf("drop model max output migration database: %v", err)
		}
		adminPool.Close()
	})

	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 263); err != nil {
		closeProvider()
		t.Fatalf("prepare schema before model max output migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher)
VALUES
 ('p1234567',NULL,'OpenAI Luna','https://api.openai.com/v1','gpt-6-luna',decode(repeat('01',29),'hex')),
 ('p1234568',NULL,'OpenAI Sol','https://api.openai.com/v1','gpt-6.1-sol',decode(repeat('02',29),'hex')),
 ('p1234569',NULL,'OpenAI Astra','https://api.openai.com/v1','gpt-6-astra',decode(repeat('03',29),'hex')),
 ('p1234570',NULL,'DeepSeek Flash','https://api.deepseek.com','deepseek-flash',decode(repeat('04',29),'hex')),
 ('p1234571',NULL,'Unknown GLM','https://api.z.ai/api/paas/v4','glm-5.1',decode(repeat('05',29),'hex')),
 ('p1234572',NULL,'OpenAI route mismatch','https://api.openai.com/v2','gpt-6-luna',decode(repeat('06',29),'hex'))`); err != nil {
		closeProvider()
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 264); err != nil {
		closeProvider()
		t.Fatalf("apply model max output migration: %v", err)
	}
	closeProvider()

	expected := map[string]int{"p1234567": 128_000, "p1234568": 128_000, "p1234569": 128_000, "p1234570": 393_216}
	for id, want := range expected {
		var effective, override int
		var source, version, sourceURL string
		var resolvedAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT max_output_tokens,COALESCE(max_output_override_tokens,0),max_output_source,
max_output_catalog_version,max_output_source_url,max_output_resolved_at FROM model_connections WHERE id=$1`, id).
			Scan(&effective, &override, &source, &version, &sourceURL, &resolvedAt); err != nil || effective != want || override != 0 ||
			source != "model_catalog" || version != "2026-11-27" || !strings.HasPrefix(sourceURL, "https://") || resolvedAt == nil {
			t.Errorf("catalog output metadata for %s = effective:%d override:%d source:%q version:%q url:%q resolved:%v err=%v",
				id, effective, override, source, version, sourceURL, resolvedAt, err)
		}
	}
	for _, id := range []string{"p1234571", "p1234572"} {
		var effective, override *int
		var source string
		var resolvedAt *time.Time
		var version, sourceURL *string
		if err := pool.QueryRow(ctx, `SELECT max_output_tokens,max_output_override_tokens,max_output_source,max_output_resolved_at,
max_output_catalog_version,max_output_source_url FROM model_connections WHERE id=$1`, id).
			Scan(&effective, &override, &source, &resolvedAt, &version, &sourceURL); err != nil || effective != nil || override != nil ||
			source != "unknown" || resolvedAt != nil || version != nil || sourceURL != nil {
			t.Errorf("unverified output metadata for %s = effective:%v override:%v source:%q resolved:%v version:%v url:%v err=%v",
				id, effective, override, source, resolvedAt, version, sourceURL, err)
		}
	}
}
