package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func TestPostgresPersonalSnapshotAndChangesUseFixedWatermarks(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	insertTestPersonalObject(t, store, owner.ID, false)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_change_log
(service_instance_id, owner_id, change_sequence, object_type, object_id, revision, operation, mutation_id)
VALUES ($1::uuid,$2,1,'profile','default',4,'upsert','mutation-123')`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	addPersonalSyncTestChange(t, store, owner.ID, "conversation", "c1234567", "", "")
	addPersonalSyncTestChange(t, store, owner.ID, personal.ObjectTypeProfile, personal.ProfileObjectID, `{"name":"Muse 2","avatar":"dragon"}`, "mutation-234")
	addPersonalSyncTestChange(t, store, owner.ID, "message", "m1234567", "", "")
	addPersonalSyncTestChange(t, store, owner.ID, personal.ObjectTypeProfile, personal.ProfileObjectID, `{"name":"Muse 3","avatar":"dragon"}`, "mutation-345")

	snapshot, err := store.GetPersonalSnapshotBatch(ctx, principal, nil, 1)
	if err != nil || snapshot.Watermark != 5 || len(snapshot.Items) != 1 || snapshot.Items[0].Revision != 6 ||
		!personalSyncProfileBodyMatches(snapshot.Items[0].Body, "Muse 3") || snapshot.HasMore {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}

	first, err := store.GetPersonalChanges(ctx, principal, 0, nil, 1)
	if err != nil || first.ThroughSequence != 5 || len(first.Items) != 1 || first.Items[0].ChangeSequence != 1 ||
		first.Items[0].MutationID != "mutation-123" || !first.HasMore || first.NextAfterSequence != 1 {
		t.Fatalf("first change page = %+v, %v", first, err)
	}

	// A new profile mutation after the initial read must be outside the fixed K=5.
	addPersonalSyncTestChange(t, store, owner.ID, personal.ObjectTypeProfile, personal.ProfileObjectID, `{"name":"Muse 4","avatar":"dragon"}`, "mutation-456")
	through := first.ThroughSequence
	second, err := store.GetPersonalChanges(ctx, principal, first.NextAfterSequence, &through, 1)
	if err != nil || second.ThroughSequence != 5 || len(second.Items) != 1 || second.Items[0].ChangeSequence != 3 ||
		!second.HasMore || second.NextAfterSequence != 3 {
		t.Fatalf("second change page = %+v, %v", second, err)
	}
	third, err := store.GetPersonalChanges(ctx, principal, second.NextAfterSequence, &through, 1)
	if err != nil || third.ThroughSequence != 5 || len(third.Items) != 1 || third.Items[0].ChangeSequence != 5 ||
		third.HasMore || third.NextAfterSequence != 5 {
		t.Fatalf("terminal change page = %+v, %v", third, err)
	}
	if _, err := store.GetPersonalChanges(ctx, principal, 5, &through, 1); err != nil {
		t.Fatalf("empty fixed range page failed: %v", err)
	}
	nextRound, err := store.GetPersonalChanges(ctx, principal, 5, nil, 1)
	if err != nil || nextRound.ThroughSequence != 6 || len(nextRound.Items) != 1 || nextRound.Items[0].ChangeSequence != 6 {
		t.Fatalf("next-round changes = %+v, %v", nextRound, err)
	}

	pagedSnapshotCursor := &personal.SnapshotCursor{
		Version: personal.SnapshotCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
		ObjectType: personal.SnapshotScopeObjectType, ObjectID: personal.SnapshotScopeObjectID, Watermark: 5,
		AfterObjectType: personal.ObjectTypeProfile, AfterObjectID: personal.ProfileObjectID,
	}
	secondSnapshotPage, err := store.GetPersonalSnapshotBatch(ctx, principal, pagedSnapshotCursor, 1)
	if err != nil || secondSnapshotPage.Watermark != 5 || len(secondSnapshotPage.Items) != 0 || secondSnapshotPage.HasMore {
		t.Fatalf("second snapshot page = %+v, %v", secondSnapshotPage, err)
	}

	badThrough := int64(7)
	if _, err := store.GetPersonalChanges(ctx, principal, 5, &badThrough, 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("future through sequence error = %v, want invalid_argument", err)
	}
	wrongOwner := *pagedSnapshotCursor
	wrongOwner.OwnerID = "u7654321"
	if _, err := store.GetPersonalSnapshotBatch(ctx, principal, &wrongOwner, 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-account snapshot cursor error = %v, want invalid_argument", err)
	}

	if err := store.RevokeToken(ctx, principal.TokenID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPersonalChanges(ctx, principal, 0, nil, 1); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked session error = %v, want unauthenticated", err)
	}
}

func TestPostgresPersonalSyncDoesNotLoseWritesDuringSnapshotPaging(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	objects := []personal.Object{
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"global"}`)},
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000001", Body: json.RawMessage(`{"content":"first"}`)},
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000002", Body: json.RawMessage(`{"content":"second"}`)},
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000003", Body: json.RawMessage(`{"content":"third"}`)},
	}
	for i, object := range objects {
		mutation := personalContentMutationForTest(t, owner.ID, 700+i, object, 0)
		if _, err := store.ApplyPersonalMutation(ctx, principal, mutation); err != nil {
			t.Fatalf("create %s: %v", object.ObjectID, err)
		}
	}
	addNonPersonalAccountChange(t, store, owner.ID, "conversation", "c1234567")
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	firstSnapshot, err := store.GetPersonalSnapshotBatch(ctx, principal, nil, 1)
	if err != nil || firstSnapshot.Watermark != 5 || len(firstSnapshot.Items) != 1 ||
		firstSnapshot.Items[0].ObjectID != "global" || !firstSnapshot.HasMore {
		t.Fatalf("first snapshot = %+v, %v", firstSnapshot, err)
	}

	updated := personal.Object{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000001", Body: json.RawMessage(`{"content":"updated first"}`)}
	updateMutation := personalContentMutationForTest(t, owner.ID, 710, updated, 1)
	if receipt, err := store.ApplyPersonalMutation(ctx, principal, updateMutation); err != nil || receipt.ResultRevision != 2 || receipt.AccountChangeSequence != 6 {
		t.Fatalf("update during snapshot = %+v, %v", receipt, err)
	}
	addNonPersonalAccountChange(t, store, owner.ID, "conversation", "c2345678")
	deleted := personalContentMutationForTest(t, owner.ID, 711, objects[2], 1)
	deleted.Delete, deleted.Body = true, nil
	deleted.RequestHash, err = personal.HashMutationContent(deleted)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := store.ApplyPersonalMutation(ctx, principal, deleted); err != nil || receipt.ResultRevision != 2 || receipt.AccountChangeSequence != 8 {
		t.Fatalf("delete during snapshot = %+v, %v", receipt, err)
	}
	created := personal.Object{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000004", Body: json.RawMessage(`{"content":"created during snapshot"}`)}
	if receipt, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 712, created, 0)); err != nil || receipt.ResultRevision != 1 || receipt.AccountChangeSequence != 9 {
		t.Fatalf("create during snapshot = %+v, %v", receipt, err)
	}
	firstItem := firstSnapshot.Items[0]
	cursor := &personal.SnapshotCursor{
		Version: personal.SnapshotCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
		ObjectType: personal.SnapshotScopeObjectType, ObjectID: personal.SnapshotScopeObjectID,
		Watermark: firstSnapshot.Watermark, AfterObjectType: firstItem.ObjectType, AfterObjectID: firstItem.ObjectID,
	}
	seen := map[string]int64{firstItem.ObjectID: firstItem.Revision}
	for pageIndex := 0; ; pageIndex++ {
		batch, err := store.GetPersonalSnapshotBatch(ctx, principal, cursor, 1)
		if err != nil || batch.Watermark != firstSnapshot.Watermark {
			t.Fatalf("snapshot page %d watermark=%d want=%d err=%v", pageIndex, batch.Watermark, firstSnapshot.Watermark, err)
		}
		for _, item := range batch.Items {
			if _, duplicate := seen[item.ObjectID]; duplicate {
				t.Fatalf("snapshot returned duplicate object %s", item.ObjectID)
			}
			seen[item.ObjectID] = item.Revision
		}
		if !batch.HasMore {
			break
		}
		if len(batch.Items) == 0 {
			t.Fatal("snapshot cursor advanced without returning objects")
		}
		last := batch.Items[len(batch.Items)-1]
		cursor = &personal.SnapshotCursor{
			Version: personal.SnapshotCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
			ObjectType: personal.SnapshotScopeObjectType, ObjectID: personal.SnapshotScopeObjectID,
			Watermark: firstSnapshot.Watermark, AfterObjectType: last.ObjectType, AfterObjectID: last.ObjectID,
		}
	}
	wantSnapshot := map[string]int64{"global": 1, "m_00000001": 2, "m_00000003": 1, "m_00000004": 1}
	if len(seen) != len(wantSnapshot) {
		t.Fatalf("snapshot after interleaved writes = %v, want %v", seen, wantSnapshot)
	}
	for objectID, revision := range wantSnapshot {
		if seen[objectID] != revision {
			t.Errorf("snapshot revision for %s = %d, want %d (all: %v)", objectID, seen[objectID], revision, seen)
		}
	}

	// Fix K only after all snapshot pages; each page may already contain a post-H revision.
	firstChanges, err := store.GetPersonalChanges(ctx, principal, firstSnapshot.Watermark, nil, 1)
	if err != nil || firstChanges.ThroughSequence != 9 || len(firstChanges.Items) != 1 ||
		firstChanges.Items[0].ChangeSequence != 6 || firstChanges.Items[0].Revision != 2 || !firstChanges.HasMore {
		t.Fatalf("first fixed changes page = %+v, %v", firstChanges, err)
	}
	through := firstChanges.ThroughSequence
	after := firstChanges.NextAfterSequence
	createdAfterK := personal.Object{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_00000005", Body: json.RawMessage(`{"content":"created after K"}`)}
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 713, createdAfterK, 0)); err != nil {
		t.Fatalf("create after fixed K: %v", err)
	}
	addNonPersonalAccountChange(t, store, owner.ID, "message", "m1234567")

	changes := append([]personal.Change(nil), firstChanges.Items...)
	for pageIndex := 0; ; pageIndex++ {
		page, err := store.GetPersonalChanges(ctx, principal, after, &through, 1)
		if err != nil || page.ThroughSequence != through {
			t.Fatalf("fixed changes page %d after=%d = %+v, %v", pageIndex, after, page, err)
		}
		changes = append(changes, page.Items...)
		if !page.HasMore {
			if page.NextAfterSequence != through {
				t.Fatalf("terminal cursor=%d want fixed K=%d", page.NextAfterSequence, through)
			}
			break
		}
		if len(page.Items) == 0 || page.NextAfterSequence <= after {
			t.Fatalf("fixed changes cursor did not advance: %+v", page)
		}
		after = page.NextAfterSequence
	}
	if len(changes) != 3 || changes[0].ChangeSequence != 6 || changes[0].Revision != 2 ||
		changes[0].ObjectID != updated.ObjectID || changes[1].ChangeSequence != 8 ||
		changes[1].Operation != "delete" || changes[1].Revision != 2 || changes[1].ObjectID != deleted.ObjectID ||
		changes[2].ChangeSequence != 9 || changes[2].ObjectID != created.ObjectID || changes[2].Operation != "upsert" {
		t.Fatalf("changes within fixed K = %+v", changes)
	}

	nextRound, err := store.GetPersonalChanges(ctx, principal, through, nil, 1)
	if err != nil || nextRound.ThroughSequence != 11 || len(nextRound.Items) != 1 ||
		nextRound.Items[0].ChangeSequence != 10 || nextRound.Items[0].ObjectID != createdAfterK.ObjectID ||
		nextRound.HasMore || nextRound.NextAfterSequence != 11 {
		t.Fatalf("next fixed-watermark round = %+v, %v", nextRound, err)
	}
}

func personalSyncProfileBodyMatches(body []byte, name string) bool {
	var profile struct {
		Name   string `json:"name"`
		Avatar string `json:"avatar"`
	}
	return json.Unmarshal(body, &profile) == nil && profile.Name == name && profile.Avatar == "dragon"
}

func addPersonalSyncTestChange(t *testing.T, store *Store, ownerID, objectType, objectID, profileBody, mutationID string) {
	t.Helper()
	ctx := context.Background()
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if objectType == personal.ObjectTypeProfile && profileBody != "" {
			if _, err := tx.Exec(ctx, `UPDATE personal_objects SET body=$3::jsonb, updated_at=clock_timestamp()
WHERE owner_id=$1 AND object_type=$2 AND object_id=$4`, ownerID, objectType, profileBody, objectID); err != nil {
				return err
			}
		}
		return recordAccountObjectChanges(ctx, tx, accountObjectChange{
			OwnerID: ownerID, ObjectType: objectType, ObjectID: objectID, Operation: "upsert", MutationID: mutationID,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
