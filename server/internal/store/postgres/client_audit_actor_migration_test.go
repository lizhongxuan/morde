package postgres

import (
	"context"
	"testing"
)

func TestClientAuditActorMigrationProtectsRecordsOnRollback(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := pool.Exec(ctx, `INSERT INTO audit_logs (actor_kind, action) VALUES ('client', 'client.revoke')`); err != nil {
		t.Fatalf("insert client audit record: %v", err)
	}
	if _, err := provider.DownTo(ctx, 55); err == nil {
		t.Fatal("rollback accepted an existing client audit record")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_kind = 'client'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("client audit record was not preserved: count=%d err=%v", count, err)
	}
}
