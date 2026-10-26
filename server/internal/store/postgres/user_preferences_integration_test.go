package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
)

func TestUserPreferencesRoundTripAndDefaultHostLock(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 8000, 16384)

	initial, err := store.GetUserPreferences(ctx, owner.ID)
	if err != nil || initial.OwnerID != owner.ID || initial.AssistantName != "" || !initial.UpdatedAt.IsZero() {
		t.Fatalf("GetUserPreferences() default = %#v, %v", initial, err)
	}
	saved, err := store.SaveUserPreferences(ctx, owner.ID, "Morde", "p1234567")
	if err != nil || saved.AssistantName != "Morde" || saved.SelectedModelID != "p1234567" || saved.DefaultHostID != "" {
		t.Fatalf("SaveUserPreferences() = %#v, %v", saved, err)
	}
	selected, err := store.SetDefaultHost(ctx, owner.ID, "h1234567", "")
	if err != nil || selected.DefaultHostID != "h1234567" || selected.AssistantName != "Morde" || selected.SelectedModelID != "p1234567" {
		t.Fatalf("SetDefaultHost() = %#v, %v", selected, err)
	}
	capacity, err := store.SetDefaultHost(ctx, owner.ID, "h1234567", "capacity_unavailable")
	if err != nil || capacity.LastHostIssue != "capacity_unavailable" {
		t.Fatalf("SetDefaultHost(capacity) = %#v, %v", capacity, err)
	}

	firstRelease, err := store.AcquireDefaultHostLock(ctx, owner.ID)
	if err != nil {
		t.Fatalf("AcquireDefaultHostLock(first) error = %v", err)
	}
	secondAcquired := make(chan func() error, 1)
	secondErr := make(chan error, 1)
	go func() {
		release, err := store.AcquireDefaultHostLock(ctx, owner.ID)
		if err != nil {
			secondErr <- err
			return
		}
		secondAcquired <- release
	}()
	select {
	case release := <-secondAcquired:
		_ = release()
		t.Fatal("second default host lock was acquired while the first was held")
	case err := <-secondErr:
		t.Fatalf("AcquireDefaultHostLock(second) error = %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := firstRelease(); err != nil {
		t.Fatalf("release first default host lock: %v", err)
	}
	select {
	case release := <-secondAcquired:
		if err := release(); err != nil {
			t.Fatalf("release second default host lock: %v", err)
		}
	case err := <-secondErr:
		t.Fatalf("AcquireDefaultHostLock(second) error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second default host lock did not acquire after first release")
	}
	if err := firstRelease(); err != nil {
		t.Fatalf("idempotent first lock release: %v", err)
	}
	if _, err := store.GetUserPreferences(ctx, "invalid"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("GetUserPreferences(invalid owner) error = %v", err)
	}
}
