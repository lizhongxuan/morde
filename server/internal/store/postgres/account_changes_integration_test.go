package postgres

import (
	"context"
	"testing"
	"time"
)

type expectedAccountChange struct {
	objectType string
	objectID   string
	revision   int64
}

func assertAccountChangeLog(t *testing.T, ctx context.Context, store *Store, ownerID string, expected ...expectedAccountChange) {
	t.Helper()
	rows, err := store.pool.Query(ctx, `SELECT change_sequence, object_type, object_id, revision, operation
FROM account_change_log WHERE owner_id=$1 ORDER BY change_sequence`, ownerID)
	if err != nil {
		t.Fatalf("read account change log: %v", err)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var sequence, revision int64
		var objectType, objectID, operation string
		if err := rows.Scan(&sequence, &objectType, &objectID, &revision, &operation); err != nil {
			t.Fatalf("scan account change: %v", err)
		}
		if index >= len(expected) || sequence != int64(index+1) || objectType != expected[index].objectType ||
			objectID != expected[index].objectID || revision != expected[index].revision || operation != "upsert" {
			t.Fatalf("account change[%d] = (%d,%s,%s,%d,%s), expected %#v", index, sequence, objectType, objectID, revision, operation, expected)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate account changes: %v", err)
	}
	if index != len(expected) {
		t.Fatalf("account change count = %d, want %d", index, len(expected))
	}
	var lastSequence int64
	if err := store.pool.QueryRow(ctx, `SELECT last_sequence FROM account_change_counters WHERE owner_id=$1`, ownerID).Scan(&lastSequence); err != nil || lastSequence != int64(len(expected)) {
		t.Fatalf("account change counter = %d, want %d, err=%v", lastSequence, len(expected), err)
	}
}

func TestPostgresAccountChangeCountersSerializePerOwner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 4000, 8192)
	otherOwner := createTestUser(t, store, 3, 4000, 8192)
	firstTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstTx.Rollback(context.Background()) }()
	if err := recordAccountObjectChanges(ctx, firstTx, accountObjectChange{
		OwnerID: owner.ID, ObjectType: "conversation", ObjectID: "c-first-01", Operation: "upsert",
	}); err != nil {
		t.Fatal(err)
	}

	secondTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondTx.Rollback(context.Background()) }()
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondResult <- recordAccountObjectChanges(ctx, secondTx, accountObjectChange{
			OwnerID: owner.ID, ObjectType: "conversation", ObjectID: "c-second-02", Operation: "upsert",
		})
	}()
	<-secondStarted
	select {
	case err := <-secondResult:
		t.Fatalf("same-account writer passed a held counter lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	otherTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordAccountObjectChanges(ctx, otherTx, accountObjectChange{
		OwnerID: otherOwner.ID, ObjectType: "conversation", ObjectID: "c-other-03", Operation: "upsert",
	}); err != nil {
		_ = otherTx.Rollback(ctx)
		t.Fatalf("different-account writer was blocked: %v", err)
	}
	if err := otherTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := firstTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second account change: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("same-account writer did not resume after the first transaction committed")
	}
	if err := secondTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assertAccountChangeLog(t, ctx, store, owner.ID,
		expectedAccountChange{objectType: "conversation", objectID: "c-first-01", revision: 1},
		expectedAccountChange{objectType: "conversation", objectID: "c-second-02", revision: 1},
	)
	assertAccountChangeLog(t, ctx, store, otherOwner.ID,
		expectedAccountChange{objectType: "conversation", objectID: "c-other-03", revision: 1},
	)
}
