package postgres

import (
	"context"
	"fmt"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/personal"
)

func TestPostgresListPersonalGoalsIsOwnerScopedExcludesDeletedAndPaginates(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	ownerPrincipal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	otherOwner := createTestUser(t, store, 3, 4000, 8192)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, otherOwner.ID)

	ownerGoalIDs := []string{"g_owner_a", "g_owner_b", "g_owner_c"}
	for index, id := range ownerGoalIDs {
		body := fmt.Sprintf(`{"title":"Goal %s","status":"active","steps":[]}`, id)
		mutationID := fmt.Sprintf("goal-create-%d", index)
		mutation := personalObjectMutationForTest(t, owner.ID, mutationID, personal.ObjectTypeGoal, id, 0, body, false)
		if _, err := store.ApplyPersonalMutation(ctx, ownerPrincipal, mutation); err != nil {
			t.Fatalf("create owner goal %s: %v", id, err)
		}
	}
	if _, err := store.ApplyPersonalMutation(ctx, ownerPrincipal,
		personalObjectMutationForTest(t, owner.ID, "goal-delete-b", personal.ObjectTypeGoal, ownerGoalIDs[1], 1, "", true)); err != nil {
		t.Fatalf("delete owner goal: %v", err)
	}
	otherGoal := personalObjectMutationForTest(t, otherOwner.ID, "goal-other-1", personal.ObjectTypeGoal, "g_other_1", 0,
		`{"title":"Other account","status":"active","steps":[]}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, otherPrincipal, otherGoal); err != nil {
		t.Fatalf("create other account goal: %v", err)
	}

	var ownerIDs []string
	cursor := ""
	for {
		page, err := store.ListPersonalGoals(ctx, ownerPrincipal, app.PageRequest{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("list owner goals cursor=%q: %v", cursor, err)
		}
		if page.Items == nil {
			t.Fatal("empty goal page items must be an empty slice")
		}
		for _, item := range page.Items {
			if item.ObjectType != personal.ObjectTypeGoal || item.Revision < 1 || len(item.Body) == 0 {
				t.Fatalf("invalid listed goal: %+v", item)
			}
			ownerIDs = append(ownerIDs, item.ObjectID)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatalf("cursor did not advance: %q", cursor)
		}
		cursor = page.NextCursor
	}
	if len(ownerIDs) != 2 || ownerIDs[0] == ownerIDs[1] || ownerIDs[0] == ownerGoalIDs[1] || ownerIDs[1] == ownerGoalIDs[1] {
		t.Fatalf("owner listed IDs = %v; want the two live owner goals only", ownerIDs)
	}

	otherPage, err := store.ListPersonalGoals(ctx, otherPrincipal, app.PageRequest{Limit: 10})
	if err != nil || len(otherPage.Items) != 1 || otherPage.Items[0].ObjectID != "g_other_1" {
		t.Fatalf("other owner goal page = %+v, %v", otherPage, err)
	}
}
