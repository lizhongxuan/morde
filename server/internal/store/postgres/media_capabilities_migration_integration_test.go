package postgres

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/store/postgres/sqlcgen"
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

	connection, err := store.CreateModelConnection(ctx, app.ModelConnectionRecord{
		ID: "p1234567", Name: "test", BaseURL: "https://api.example.com/v1", ModelName: "example",
		APIKeyCipher: make([]byte, 29),
	})
	if err != nil {
		t.Fatal(err)
	}
	var imageCapability bool
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id=$1`, connection.ID).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if imageCapability {
		t.Fatal("new media capability was not defaulted to false")
	}

	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET capability_media_image_generation=true, checked_at=clock_timestamp() WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}
	connection, err = store.GetModelConnection(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !connection.Capabilities.MediaGeneration.ImageGeneration {
		t.Fatal("model connection adapter did not read the media capability")
	}
	_, err = store.q.UpdateModelConnection(ctx, sqlcgen.UpdateModelConnectionParams{
		ID: connection.ID, Name: connection.Name, BaseUrl: connection.BaseURL, ModelName: connection.ModelName,
		ApiKeyCipher: connection.APIKeyCipher, CheckedAt: nullableTimestamp(connection.CheckedAt),
		ExpectedUpdatedAt: timestamp(connection.UpdatedAt),
	})
	if err != nil {
		t.Fatalf("legacy model connection update without media flags: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id=$1`, connection.ID).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if !imageCapability {
		t.Fatal("model connection update without media flags erased a stored capability")
	}
	connection, err = store.GetModelConnection(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection.Name = "updated"
	if _, err := store.UpdateModelConnection(ctx, connection); err != nil {
		t.Fatalf("update connection with media flags: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id=$1`, connection.ID).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if !imageCapability {
		t.Fatal("connection update erased an existing media capability")
	}

	if _, err := provider.DownTo(ctx, 130); err == nil || !strings.Contains(strings.ToLower(err.Error()), "cannot roll back checked model media capabilities") {
		t.Fatalf("rollback with a checked media capability = %v, want guarded refusal", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET capability_media_image_generation=false, checked_at=NULL WHERE id=$1`, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 130); err != nil {
		t.Fatalf("rollback without checked media capabilities: %v", err)
	}
	if _, err := provider.UpTo(ctx, 131); err != nil {
		t.Fatalf("reapply media capabilities migration: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT capability_media_image_generation FROM model_connections WHERE id=$1`, connection.ID).Scan(&imageCapability); err != nil {
		t.Fatal(err)
	}
	if imageCapability {
		t.Fatal("reapplied media capability default was not false")
	}
}
