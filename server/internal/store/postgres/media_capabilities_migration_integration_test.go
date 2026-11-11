package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestModelMediaCapabilitiesMigrationPersistsAndGuardsRollback(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()

	if _, err := provider.DownTo(ctx, 130); err != nil {
		t.Fatalf("roll schema back before media capabilities migration: %v", err)
	}
	if _, err := provider.UpTo(ctx, 131); err != nil {
		t.Fatalf("apply media capabilities migration: %v", err)
	}

	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,name,base_url,model_name,api_key_cipher)
VALUES ('p1234567','test','https://api.example.com/v1','example',decode(repeat('00',29),'hex'))`); err != nil {
		t.Fatal(err)
	}
	var imageCapability bool
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id='p1234567'`).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if imageCapability {
		t.Fatal("new media capability was not defaulted to false")
	}

	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET capability_media_image_generation=true, checked_at=clock_timestamp() WHERE id='p1234567'`); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id='p1234567'`).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if !imageCapability {
		t.Fatal("media capability did not persist")
	}

	if _, err := provider.DownTo(ctx, 130); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back checked model media capabilities") {
		t.Fatalf("rollback with a checked media capability = %v, want guarded refusal", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET capability_media_image_generation=false, checked_at=NULL WHERE id='p1234567'`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 130); err != nil {
		t.Fatalf("rollback without checked media capabilities: %v", err)
	}
	if _, err := provider.UpTo(ctx, 131); err != nil {
		t.Fatalf("reapply media capabilities migration: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id='p1234567'`).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if imageCapability {
		t.Fatal("reapplied media capability default was not false")
	}
}
