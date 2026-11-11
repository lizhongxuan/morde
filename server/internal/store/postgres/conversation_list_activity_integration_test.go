package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestListConversationsOrdersAndPaginatesByLastActivity(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	oldestCreated := createMainConversationForTest(t, store, owner.ID)
	newestCreated := createMainConversationForTest(t, store, owner.ID)
	middleCreated := createMainConversationForTest(t, store, owner.ID)
	base := time.Date(2026, time.October, 22, 8, 0, 0, 0, time.UTC)
	for _, entry := range []struct {
		id        string
		createdAt time.Time
		updatedAt time.Time
	}{
		{oldestCreated.ID, base, base.Add(2 * time.Hour)},
		{newestCreated.ID, base.Add(2 * time.Hour), base.Add(time.Hour)},
		{middleCreated.ID, base.Add(time.Hour), base},
	} {
		if _, err := store.pool.Exec(ctx, `UPDATE conversations SET created_at=$2,updated_at=$3 WHERE id=$1`, entry.id, entry.createdAt, entry.updatedAt); err != nil {
			t.Fatal(err)
		}
	}

	first, err := store.ListConversations(ctx, owner.ID, nil, app.PageRequest{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" ||
		first.Items[0].ID != oldestCreated.ID || first.Items[1].ID != newestCreated.ID {
		t.Fatalf("first activity page = %#v, err=%v", first, err)
	}
	second, err := store.ListConversations(ctx, owner.ID, nil, app.PageRequest{Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" || second.Items[0].ID != middleCreated.ID {
		t.Fatalf("second activity page = %#v, err=%v", second, err)
	}
}
