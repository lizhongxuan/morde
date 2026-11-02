package postgres

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func TestPostgresPersonalContentObjectsAreOwnerScopedVersionedAndSyncable(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	otherOwner := createTestUser(t, store, 3, 4000, 8192)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, otherOwner.ID)
	objects := []personal.Object{
		{ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`)},
		{ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"A selected memory.","category":"fact"}`)},
		{ObjectType: personal.ObjectTypeSoul, ObjectID: "default", Body: json.RawMessage(`{"name":"Muse","style":"warm","lang":"en","content":"Be clear."}`)},
		{ObjectType: personal.ObjectTypeDiary, ObjectID: "2026-11-02", Body: json.RawMessage(`{"date":"2026-11-02","content":"<!-- 2026-11-02 12:00:00 -->\nA selected entry."}`)},
		{ObjectType: personal.ObjectTypeFeedPost, ObjectID: "feedpost_2026-11-02-01", Body: json.RawMessage(`{"title":"A note","type":"reflection","created_at":"2026-11-02T12:00:00Z","liked":true,"body":"A selected post."}`)},
		{ObjectType: personal.ObjectTypeFeedPreferences, ObjectID: "default", Body: json.RawMessage(`{"content":"Prefer concise suggestions."}`)},
		{ObjectType: personal.ObjectTypeIdeaMark, ObjectID: "train-watch", Body: json.RawMessage(`{"state":"saved"}`)},
		{ObjectType: personal.ObjectTypeLibraryItem, ObjectID: "library-item_1", Body: json.RawMessage(`{"name":"notes.md","kind":"artifact","mime":"text/markdown","size_bytes":4}`)},
		{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", Body: json.RawMessage(`{"title":"Learn Go","status":"active","steps":[{"id":"s_1","title":"Read the tour","status":"pending"}],"related_material_ids":["library-item_1"]}`)},
		{ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_weather", Body: json.RawMessage(`{"enabled":false}`)},
		{ObjectType: personal.ObjectTypeConnectorSetting, ObjectID: "catalogue_calendar", Body: json.RawMessage(`{"auth":"oauth","enabled":true}`)},
	}
	if len(objects) != len(personal.PersonalSyncObjectTypes()) {
		t.Fatalf("test covers %d of %d syncable types", len(objects), len(personal.PersonalSyncObjectTypes()))
	}

	mutations := make([]personal.Mutation, len(objects))
	var watermark int64
	for i, object := range objects {
		mutation := personalContentMutationForTest(t, owner.ID, i, object, 0)
		mutations[i] = mutation
		receipt, err := store.ApplyPersonalMutation(ctx, principal, mutation)
		if err != nil || receipt.ResultRevision != 1 || receipt.AccountChangeSequence <= watermark || receipt.Replay {
			t.Fatalf("upsert %s/%s = %+v, %v", object.ObjectType, object.ObjectID, receipt, err)
		}
		watermark = receipt.AccountChangeSequence
		got, err := store.GetPersonalObject(ctx, principal, object.ObjectType, object.ObjectID)
		if err != nil || got.Revision != 1 || !personalJSONEqual(got.Body, object.Body) {
			t.Fatalf("read %s/%s = %+v, %v", object.ObjectType, object.ObjectID, got, err)
		}
		if i == 1 {
			addNonPersonalAccountChange(t, store, owner.ID, "conversation", "c1234567")
			watermark++
		}
	}

	replay, err := store.ApplyPersonalMutation(ctx, principal, mutations[1])
	if err != nil || !replay.Replay || replay.ResultRevision != 1 || replay.AccountChangeSequence != 2 {
		t.Fatalf("memory mutation replay = %+v, %v", replay, err)
	}
	for i, object := range objects {
		if _, err := store.GetPersonalObject(ctx, otherPrincipal, object.ObjectType, object.ObjectID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Errorf("cross-account %s/%s read error = %v, want not_found", object.ObjectType, object.ObjectID, err)
		}
		stale := personalContentMutationForTest(t, owner.ID, 100+i, personal.Object{
			ObjectType: object.ObjectType, ObjectID: object.ObjectID, Body: updatedPersonalContentBody(object),
		}, 0)
		if _, err := store.ApplyPersonalMutation(ctx, principal, stale); domain.CodeOf(err) != domain.CodeConflict {
			t.Errorf("stale %s/%s update error = %v, want conflict", object.ObjectType, object.ObjectID, err)
		}
	}

	var cursor *personal.SnapshotCursor
	seen := make(map[string]bool, len(objects))
	for {
		batch, err := store.GetPersonalSnapshotBatch(ctx, principal, cursor, 3)
		if err != nil || batch.Watermark != watermark {
			t.Fatalf("snapshot batch watermark=%d want=%d err=%v", batch.Watermark, watermark, err)
		}
		for _, object := range batch.Items {
			key := object.ObjectType + "/" + object.ObjectID
			if seen[key] || object.Revision != 1 || !personalJSONEqual(object.Body, findPersonalObject(t, objects, key).Body) {
				t.Fatalf("invalid or duplicate snapshot object %+v", object)
			}
			seen[key] = true
		}
		if !batch.HasMore {
			break
		}
		if len(batch.Items) == 0 {
			t.Fatal("snapshot cursor advanced without returning objects")
		}
		last := batch.Items[len(batch.Items)-1]
		cursor = &personal.SnapshotCursor{
			Version: personal.SnapshotCursorVersion, OwnerID: owner.ID, ServiceInstanceID: batch.ServiceInstanceID,
			ObjectType: personal.SnapshotScopeObjectType, ObjectID: personal.SnapshotScopeObjectID,
			Watermark: batch.Watermark, AfterObjectType: last.ObjectType, AfterObjectID: last.ObjectID,
		}
	}
	if len(seen) != len(objects) {
		t.Fatalf("snapshot returned %d objects, want %d: %v", len(seen), len(objects), seen)
	}
	otherSnapshot, err := store.GetPersonalSnapshotBatch(ctx, otherPrincipal, nil, 10)
	if err != nil || len(otherSnapshot.Items) != 0 {
		t.Fatalf("cross-account snapshot = %+v, %v", otherSnapshot, err)
	}

	through := watermark
	after := int64(0)
	changes := make([]personal.Change, 0, len(objects))
	for {
		page, err := store.GetPersonalChanges(ctx, principal, after, &through, 3)
		if err != nil || page.ThroughSequence != through {
			t.Fatalf("change page after=%d = %+v, %v", after, page, err)
		}
		for _, change := range page.Items {
			if change.ObjectType == "conversation" || !personal.IsPersonalSyncObjectType(change.ObjectType) {
				t.Fatalf("non-personal change leaked into content sync: %+v", change)
			}
			changes = append(changes, change)
		}
		if !page.HasMore {
			if page.NextAfterSequence != through {
				t.Fatalf("terminal change cursor=%d want fixed watermark %d", page.NextAfterSequence, through)
			}
			break
		}
		if page.NextAfterSequence <= after {
			t.Fatalf("change cursor did not advance: after=%d next=%d", after, page.NextAfterSequence)
		}
		after = page.NextAfterSequence
	}
	if len(changes) != len(objects) {
		t.Fatalf("change feed returned %d personal objects, want %d", len(changes), len(objects))
	}

	sequence := watermark
	for i, object := range objects {
		updatedObject := personal.Object{ObjectType: object.ObjectType, ObjectID: object.ObjectID, Body: updatedPersonalContentBody(object)}
		update := personalContentMutationForTest(t, owner.ID, 200+i, updatedObject, 1)
		updated, err := store.ApplyPersonalMutation(ctx, principal, update)
		sequence++
		if err != nil || updated.ResultRevision != 2 || updated.AccountChangeSequence != sequence {
			t.Fatalf("update %s/%s = %+v, %v", object.ObjectType, object.ObjectID, updated, err)
		}
		deletion := personalContentMutationForTest(t, owner.ID, 300+i, updatedObject, 2)
		deletion.Delete, deletion.Body = true, nil
		deletion.RequestHash, err = personal.HashMutationContent(deletion)
		if err != nil {
			t.Fatal(err)
		}
		deleted, err := store.ApplyPersonalMutation(ctx, principal, deletion)
		sequence++
		if err != nil || deleted.ResultRevision != 3 || deleted.AccountChangeSequence != sequence {
			t.Fatalf("delete %s/%s = %+v, %v", object.ObjectType, object.ObjectID, deleted, err)
		}
		if _, err := store.GetPersonalObject(ctx, principal, object.ObjectType, object.ObjectID); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("deleted %s/%s read error = %v, want not_found", object.ObjectType, object.ObjectID, err)
		}
	}
	postSnapshotChanges, err := store.GetPersonalChanges(ctx, principal, watermark, nil, 100)
	if err != nil || len(postSnapshotChanges.Items) != len(objects)*2 {
		t.Fatalf("content update/delete change page has %d items, want %d: %v", len(postSnapshotChanges.Items), len(objects)*2, err)
	}
	for i, object := range objects {
		update, deletion := postSnapshotChanges.Items[i*2], postSnapshotChanges.Items[i*2+1]
		if update.ObjectType != object.ObjectType || update.ObjectID != object.ObjectID || update.Operation != "upsert" || update.Revision != 2 ||
			deletion.ObjectType != object.ObjectType || deletion.ObjectID != object.ObjectID || deletion.Operation != "delete" || deletion.Revision != 3 {
			t.Errorf("change pair for %s/%s = %+v, %+v", object.ObjectType, object.ObjectID, update, deletion)
		}
	}
}

func personalContentMutationForTest(t *testing.T, ownerID string, sequence int, object personal.Object, revision int64) personal.Mutation {
	t.Helper()
	mutation := personal.Mutation{
		OwnerID: ownerID, MutationID: "content-write-" + strconv.Itoa(sequence),
		ObjectType: object.ObjectType, ObjectID: object.ObjectID, BaseRevision: revision,
		Body: append(json.RawMessage(nil), object.Body...),
	}
	hash, err := personal.HashMutationContent(mutation)
	if err != nil {
		t.Fatalf("hash %s/%s: %v", object.ObjectType, object.ObjectID, err)
	}
	mutation.RequestHash = hash
	return mutation
}

func addNonPersonalAccountChange(t *testing.T, store *Store, ownerID, objectType, objectID string) {
	t.Helper()
	err := store.WithTx(context.Background(), func(tx pgx.Tx) error {
		return recordAccountObjectChanges(context.Background(), tx, accountObjectChange{
			OwnerID: ownerID, ObjectType: objectType, ObjectID: objectID, Operation: "upsert",
		})
	})
	if err != nil {
		t.Fatalf("add non-personal account change: %v", err)
	}
}

func personalJSONEqual(left, right []byte) bool {
	var leftValue, rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil &&
		reflect.DeepEqual(leftValue, rightValue)
}

func updatedPersonalContentBody(object personal.Object) json.RawMessage {
	switch object.ObjectType {
	case personal.ObjectTypeProfile:
		return json.RawMessage(`{"name":"Updated Muse","avatar":"dragon"}`)
	case personal.ObjectTypeMemory:
		return json.RawMessage(`{"content":"Updated selected memory.","category":"fact"}`)
	case personal.ObjectTypeSoul:
		return json.RawMessage(`{"name":"Muse","style":"warm","lang":"en","content":"Be concise."}`)
	case personal.ObjectTypeDiary:
		return json.RawMessage(`{"date":"2026-11-02","content":"Updated selected entry."}`)
	case personal.ObjectTypeFeedPost:
		return json.RawMessage(`{"title":"Updated note","type":"reflection","created_at":"2026-11-02T12:00:00Z","liked":true,"body":"Updated selected post."}`)
	case personal.ObjectTypeFeedPreferences:
		return json.RawMessage(`{"content":"Prefer shorter suggestions."}`)
	case personal.ObjectTypeIdeaMark:
		return json.RawMessage(`{"state":"dismissed"}`)
	case personal.ObjectTypeLibraryItem:
		return json.RawMessage(`{"name":"updated-notes.md","kind":"artifact","mime":"text/markdown","size_bytes":12}`)
	case personal.ObjectTypeGoal:
		return json.RawMessage(`{"title":"Learn Go","status":"active","steps":[{"id":"s_1","title":"Read the tour","status":"done"}],"related_material_ids":["library-item_1"]}`)
	case personal.ObjectTypeSkillSetting:
		return json.RawMessage(`{"enabled":true}`)
	case personal.ObjectTypeConnectorSetting:
		return json.RawMessage(`{"auth":"oauth","enabled":false}`)
	default:
		return nil
	}
}

func findPersonalObject(t *testing.T, objects []personal.Object, key string) personal.Object {
	t.Helper()
	for _, object := range objects {
		if object.ObjectType+"/"+object.ObjectID == key {
			return object
		}
	}
	t.Fatalf("test object %q is missing", key)
	return personal.Object{}
}
