package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresNotificationUpdatesBaselinePaginationReconnectExpiryAndRevocation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	member := createTestUser(t, store, 8, 20_000, 40_000)
	outsider := createTestUser(t, store, 8, 20_000, 40_000)

	createAndConsumeNotificationSourceFixture(t, ctx, store, pool, member.ID, notification.SourceGoal, "goal-m10-before-baseline", "", notification.KindGoalCompleted, time.Now().UTC())
	baseline, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 10})
	if err != nil || len(baseline.Items) != 0 || baseline.Cursor == "" || baseline.HasMore {
		t.Fatalf("first notification update call = %#v, err=%v; want empty no-backfill baseline", baseline, err)
	}
	if _, err := store.ListNotificationUpdates(ctx, outsider.ID, app.PageRequest{Limit: 10, Cursor: baseline.Cursor}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-account notification cursor error=%v; want invalid argument", err)
	}

	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Incremental notices", "M10 access checks", "m10-notice-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"m10-notice-invite-01", "m10-notice-accept-01"); err != nil {
		t.Fatalf("invite notification recipient: %v", err)
	}
	itemID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	item := project.WorkItem{
		ID: itemID, ProjectID: projectValue.ID, Title: "M10 notice fixture", AssigneeID: owner.ID,
		State: project.WorkItemOpen, Version: 1,
	}
	if _, replay, err := store.CreateProjectWorkItem(ctx, owner.ID, item, "m10-notice-item-01"); err != nil || replay {
		t.Fatalf("create work item for revoked notice = replay:%t err:%v", replay, err)
	}
	createAndConsumeNotificationSourceFixture(t, ctx, store, pool, member.ID, notification.SourceProjectWorkItem, item.ID, projectValue.ID, notification.KindProjectItemBlocked, time.Now().UTC())
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "m10-notice-remove-01"); err != nil {
		t.Fatalf("revoke notification recipient: %v", err)
	}
	createAndConsumeNotificationSourceFixture(t, ctx, store, pool, member.ID, notification.SourceGoal, "goal-m10-after-revoke", "", notification.KindGoalCompleted, time.Now().UTC())

	first, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1, Cursor: baseline.Cursor})
	if err != nil || len(first.Items) != 0 || !first.HasMore || first.Cursor == baseline.Cursor {
		t.Fatalf("revoked notice scan = %#v err=%v; want cursor advancement over inaccessible notice", first, err)
	}
	// The client disconnects here and resumes from the last durable cursor after another notice commits.
	createAndConsumeNotificationSourceFixture(t, ctx, store, pool, member.ID, notification.SourceGoal, "goal-m10-reconnected", "", notification.KindGoalCompleted, time.Now().UTC())
	resumed, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1, Cursor: first.Cursor})
	if err != nil || len(resumed.Items) != 1 || resumed.Items[0].SourceID != "goal-m10-after-revoke" || !resumed.HasMore {
		t.Fatalf("resumed notification page = %#v err=%v", resumed, err)
	}
	lastPage, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1, Cursor: resumed.Cursor})
	if err != nil || len(lastPage.Items) != 1 || lastPage.Items[0].SourceID != "goal-m10-reconnected" || lastPage.HasMore {
		t.Fatalf("next notification page = %#v err=%v", lastPage, err)
	}

	var secret []byte
	var lastSequence int64
	if err := pool.QueryRow(ctx, `SELECT cursor_secret,last_sequence FROM notification_account_sequences WHERE owner_id=$1`, member.ID).Scan(&secret, &lastSequence); err != nil {
		t.Fatal(err)
	}
	expired := encodeNotificationUpdateCursor(member.ID, lastSequence, secret, time.Now().UTC().Add(-notificationUpdateCursorTTL-time.Minute))
	if _, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1, Cursor: expired}); domain.CodeOf(err) != domain.CodeEventCursorExpired {
		t.Fatalf("expired notification cursor error=%v; want cursor expired", err)
	}
	rebaseline, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1})
	if err != nil || len(rebaseline.Items) != 0 || rebaseline.Cursor == "" {
		t.Fatalf("expired cursor recovery baseline = %#v err=%v", rebaseline, err)
	}
	createAndConsumeNotificationSourceFixture(t, ctx, store, pool, member.ID, notification.SourceGoal, "goal-m10-after-rebaseline", "", notification.KindGoalCompleted, time.Now().UTC())
	recovered, err := store.ListNotificationUpdates(ctx, member.ID, app.PageRequest{Limit: 1, Cursor: rebaseline.Cursor})
	if err != nil || len(recovered.Items) != 1 || recovered.Items[0].SourceID != "goal-m10-after-rebaseline" {
		t.Fatalf("notification updates after rebaseline = %#v err=%v", recovered, err)
	}
}

func TestPostgresNotificationUpdatesSerializeAcrossConnectionsAtCommit(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	baseline, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 10})
	if err != nil || baseline.Cursor == "" {
		t.Fatalf("notification baseline = %#v err=%v", baseline, err)
	}
	now := time.Now().UTC()
	secondEvent := insertNotificationSourceEventFixture(t, ctx, pool, owner.ID, notification.SourceGoal, "goal-m10-order-second", "", notification.KindGoalCompleted, now.Add(-time.Hour))
	firstEvent := insertNotificationSourceEventFixture(t, ctx, pool, owner.ID, notification.SourceGoal, "goal-m10-order-first", "", notification.KindGoalCompleted, now.Add(time.Hour))

	secondConfig := pool.Config().Copy()
	secondConfig.MaxConns = 1
	secondConfig.MinConns = 0
	secondPool, err := pgxpool.NewWithConfig(ctx, secondConfig)
	if err != nil {
		t.Fatalf("create second PostgreSQL pool: %v", err)
	}
	t.Cleanup(secondPool.Close)

	firstTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstTx.Rollback(context.Background()) }()
	var firstPID int32
	if err := firstTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	if err := insertNotificationDeliveryFixture(ctx, firstTx, firstEvent); err != nil {
		t.Fatalf("insert first uncommitted notice: %v", err)
	}

	type assignedSequence struct {
		sequence int64
		pid      int32
		err      error
	}
	assigned := make(chan assignedSequence, 1)
	started := make(chan int32, 1)
	commitResult := make(chan error, 1)
	releaseSecond := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSecond) }) }
	defer release()
	go func() {
		tx, err := secondPool.Begin(ctx)
		if err != nil {
			assigned <- assignedSequence{err: err}
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			assigned <- assignedSequence{err: err}
			return
		}
		started <- pid
		sequence, err := nextNotificationDeliverySequence(ctx, tx, secondEvent.RecipientID)
		if err != nil {
			assigned <- assignedSequence{pid: pid, err: err}
			return
		}
		id, err := notification.NewID()
		if err != nil {
			assigned <- assignedSequence{pid: pid, err: err}
			return
		}
		value, err := notification.NewSourceEventNotification(secondEvent, id)
		if err == nil {
			tag, insertErr := tx.Exec(ctx, `INSERT INTO notifications
(id,owner_id,source_event_id,source_kind,source_id,source_version,project_id,kind,summary,delivery_sequence,created_at)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11)`, value.ID, value.OwnerID, value.SourceEventID,
				value.SourceKind, value.SourceID, value.SourceVersion, value.ProjectID, string(value.Kind), value.Summary, sequence, value.CreatedAt)
			err = insertErr
			if err == nil && tag.RowsAffected() != 1 {
				err = errors.New("second notification insert was not applied")
			}
		}
		if err == nil {
			err = commitNotificationDeliverySequence(ctx, tx, secondEvent.RecipientID, sequence)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE notification_source_events SET processed_at=clock_timestamp() WHERE id=$1`, secondEvent.ID)
		}
		if err != nil {
			assigned <- assignedSequence{pid: pid, err: err}
			return
		}
		assigned <- assignedSequence{sequence: sequence, pid: pid}
		<-releaseSecond
		commitResult <- tx.Commit(ctx)
	}()

	secondPID := <-started
	if secondPID == firstPID {
		t.Fatalf("notification sequencing did not use two distinct PostgreSQL connections: pid=%d", firstPID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, secondPID).Scan(&waiting); err != nil {
			t.Fatalf("read second connection wait state: %v", err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second PostgreSQL connection did not block on the account sequence lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	whileBlocked, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 10, Cursor: baseline.Cursor})
	if err != nil || len(whileBlocked.Items) != 0 || whileBlocked.Cursor == "" {
		t.Fatalf("uncommitted notification became visible: %#v err=%v", whileBlocked, err)
	}
	var cursorSecret []byte
	if err := pool.QueryRow(ctx, `SELECT cursor_secret FROM notification_account_sequences WHERE owner_id=$1`, owner.ID).Scan(&cursorSecret); err != nil {
		t.Fatal(err)
	}
	blockedPosition, err := decodeNotificationUpdateCursor(whileBlocked.Cursor, owner.ID, cursorSecret, time.Now().UTC())
	if err != nil || blockedPosition != 0 {
		t.Fatalf("cursor advanced across an uncommitted notice: position=%d err=%v", blockedPosition, err)
	}

	if err := firstTx.Commit(ctx); err != nil {
		t.Fatalf("commit first account sequence transaction: %v", err)
	}
	secondAssigned := <-assigned
	if secondAssigned.err != nil || secondAssigned.sequence != 2 {
		t.Fatalf("second notification assignment after first commit = %#v", secondAssigned)
	}
	firstCommitted, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 1, Cursor: baseline.Cursor})
	if err != nil || len(firstCommitted.Items) != 1 || firstCommitted.Items[0].SourceID != firstEvent.SourceID || firstCommitted.HasMore {
		t.Fatalf("committed first notification page = %#v err=%v", firstCommitted, err)
	}
	if !firstCommitted.Items[0].CreatedAt.After(secondEvent.CreatedAt) {
		t.Fatalf("fixture timestamps did not reverse stream order: first=%s second=%s", firstCommitted.Items[0].CreatedAt, secondEvent.CreatedAt)
	}
	release()
	if err := <-commitResult; err != nil {
		t.Fatalf("commit second account sequence transaction: %v", err)
	}
	secondCommitted, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 1, Cursor: firstCommitted.Cursor})
	if err != nil || len(secondCommitted.Items) != 1 || secondCommitted.Items[0].SourceID != secondEvent.SourceID || secondCommitted.HasMore {
		t.Fatalf("second committed notification page = %#v err=%v", secondCommitted, err)
	}
	if !secondCommitted.Items[0].CreatedAt.Before(firstCommitted.Items[0].CreatedAt) {
		t.Fatalf("incremental stream followed timestamp order instead of commit sequence: first=%s second=%s", firstCommitted.Items[0].CreatedAt, secondCommitted.Items[0].CreatedAt)
	}
}

func TestPostgresTaskCompletionNotificationUsesIncrementalSequence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	baseline, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("task notification baseline = %#v err=%v", baseline, err)
	}
	taskID := commitNotificationTerminalTask(t, store, ctx, owner.ID, hostValue.ID, "m10-task-completion", task.StatusCompleted)
	service, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ProcessPending(ctx, 10); err != nil || processed < 1 {
		t.Fatalf("process task completion notice = %d, %v", processed, err)
	}
	updates, err := store.ListNotificationUpdates(ctx, owner.ID, app.PageRequest{Limit: 10, Cursor: baseline.Cursor})
	if err != nil || len(updates.Items) != 1 || updates.Items[0].TaskID != taskID || updates.Items[0].Kind != notification.KindCompleted {
		t.Fatalf("task completion incremental page = %#v err=%v", updates, err)
	}
}

func createAndConsumeNotificationSourceFixture(t *testing.T, ctx context.Context, store *Store, pool *pgxpool.Pool, recipientID, sourceKind, sourceID, projectID string, kind notification.Kind, createdAt time.Time) {
	t.Helper()
	event := insertNotificationSourceEventFixture(t, ctx, pool, recipientID, sourceKind, sourceID, projectID, kind, createdAt)
	id, err := notification.NewID()
	if err != nil {
		t.Fatal(err)
	}
	value, err := notification.NewSourceEventNotification(event, id)
	if err != nil {
		t.Fatal(err)
	}
	if consumed, err := store.ConsumeNotificationSourceEvent(ctx, event, value); err != nil || !consumed {
		t.Fatalf("consume source notice %s = consumed:%t err:%v", sourceID, consumed, err)
	}
}

func insertNotificationSourceEventFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, recipientID, sourceKind, sourceID, projectID string, kind notification.Kind, createdAt time.Time) notification.SourceEvent {
	t.Helper()
	var event notification.SourceEvent
	event.RecipientID, event.SourceKind, event.SourceID, event.ProjectID = recipientID, sourceKind, sourceID, projectID
	event.SourceVersion, event.Kind = 1, kind
	err := pool.QueryRow(ctx, `INSERT INTO notification_source_events
(source_kind,source_id,source_version,event_kind,recipient_id,project_id,created_at)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7) RETURNING id,created_at`,
		sourceKind, sourceID, event.SourceVersion, string(kind), recipientID, projectID, createdAt).Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		t.Fatalf("insert source event fixture %s: %v", sourceID, err)
	}
	if err := notification.ValidateSourceEvent(event); err != nil {
		t.Fatalf("source event fixture %s is invalid: %v", sourceID, err)
	}
	return event
}

func insertNotificationDeliveryFixture(ctx context.Context, tx pgx.Tx, event notification.SourceEvent) error {
	sequence, err := nextNotificationDeliverySequence(ctx, tx, event.RecipientID)
	if err != nil {
		return err
	}
	id, err := notification.NewID()
	if err != nil {
		return err
	}
	value, err := notification.NewSourceEventNotification(event, id)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO notifications
(id,owner_id,source_event_id,source_kind,source_id,source_version,project_id,kind,summary,delivery_sequence,created_at)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11)`, value.ID, value.OwnerID, value.SourceEventID,
		value.SourceKind, value.SourceID, value.SourceVersion, value.ProjectID, string(value.Kind), value.Summary, sequence, value.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("first notification insert was not applied")
	}
	if err := commitNotificationDeliverySequence(ctx, tx, event.RecipientID, sequence); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE notification_source_events SET processed_at=clock_timestamp() WHERE id=$1`, event.ID)
	return err
}
