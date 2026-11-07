package postgres

import (
	"context"
	"testing"
	"time"
)

func TestPostgresTaskInitiatorReadDoesNotHoldTaskRowLock(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 4, 10_000, 20_000)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "task-initiator-lock", "exercise Task authorization locking"))
	if err != nil {
		t.Fatal(err)
	}

	reader, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(context.Background()) }()
	if _, err := lockTaskInitiatorInTx(ctx, reader, created.Task.ID, owner.ID, owner.ID); err != nil {
		t.Fatalf("read Task initiator: %v", err)
	}

	writer, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := lockTaskInitiatorInTx(ctx, writer, created.Task.ID, owner.ID, owner.ID); err != nil {
		t.Fatalf("read Task initiator in concurrent transaction: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Exec(ctx, `UPDATE tasks SET updated_at=clock_timestamp() WHERE id=$1`, created.Task.ID)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("concurrent Task writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = reader.Rollback(context.Background())
		if err := <-writeDone; err != nil {
			t.Fatalf("concurrent Task writer after releasing the reader: %v", err)
		}
		t.Fatal("Task initiator read retained a row lock and blocked a concurrent Task writer")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent Task writer: %v", err)
	}
}
