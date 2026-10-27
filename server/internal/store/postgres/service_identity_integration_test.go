package postgres

import (
	"context"
	"testing"
)

func TestServiceInstanceIDStableAcrossReopenAndMigrationRollback(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	first, err := store.ServiceInstanceID(ctx)
	if err != nil || first == "" {
		t.Fatalf("read service instance id: id=%q err=%v", first, err)
	}
	reopened, err := New(pool).ServiceInstanceID(ctx)
	if err != nil || reopened != first {
		t.Fatalf("reopen changed service instance id: got=%q want=%q err=%v", reopened, first, err)
	}
	otherStore, _ := newTemporaryStore(t)
	other, err := otherStore.ServiceInstanceID(ctx)
	if err != nil || other == "" || other == first {
		t.Fatalf("different database did not get a distinct service instance id: got=%q err=%v", other, err)
	}
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 54); err != nil {
		t.Fatalf("roll back service identity migration: %v", err)
	}
	if _, err := provider.UpTo(ctx, 55); err != nil {
		t.Fatalf("reapply service identity migration: %v", err)
	}
	after, err := store.ServiceInstanceID(ctx)
	if err != nil || after != first {
		t.Fatalf("migration rollback changed service instance id: got=%q want=%q err=%v", after, first, err)
	}
}

func TestServiceInstanceIDFailsClosedIfIdentityMissing(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM service_identity`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ServiceInstanceID(ctx); err == nil {
		t.Fatal("missing service identity unexpectedly succeeded")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_identity`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("identity read recreated a row: count=%d err=%v", count, err)
	}
}
