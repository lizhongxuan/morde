package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestOutboxConversationCreationChecksInstanceBeforeWritingAndReplays(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 1, 2000, 4096)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrongInstance := "11111111-1111-4111-8111-111111111111"
	if wrongInstance == instanceID {
		wrongInstance = "22222222-2222-4222-8222-222222222222"
	}
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Outbox scope", "fixture", "scoped-create-project")
	for _, projectID := range []string{"", projectValue.ID} {
		id, err := domain.NewConversationID()
		if err != nil {
			t.Fatal(err)
		}
		value := conversation.Conversation{ID: id, OwnerID: owner.ID, ProjectID: projectID, Title: "queued"}
		operation, key := "conversation.create", "scoped-personal-create"
		if projectID != "" {
			operation, key = "project_conversation.create", "scoped-project-create"
		}
		record := testIdempotency(owner.ID, operation, key)
		create := func(instance string) (conversation.Conversation, bool, error) {
			if projectID != "" {
				return store.CreateProjectConversationForServiceInstance(ctx, owner.ID, value, record, instance)
			}
			return store.CreateConversationForServiceInstance(ctx, value, record, instance)
		}
		if _, _, err := create(wrongInstance); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("wrong instance project=%q: %v", projectID, err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("wrong instance wrote a conversation: count=%d err=%v", count, err)
		}
		created, replay, err := create(instanceID)
		if err != nil || replay || created.ID != id {
			t.Fatalf("create project=%q: %+v replay=%v err=%v", projectID, created, replay, err)
		}
		repeated, replay, err := create(instanceID)
		if err != nil || !replay || repeated.ID != id {
			t.Fatalf("replay project=%q: %+v replay=%v err=%v", projectID, repeated, replay, err)
		}
	}
}
