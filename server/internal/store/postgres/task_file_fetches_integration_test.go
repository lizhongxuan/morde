package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
)

func TestTaskFileFetchOwnerSessionRequiresActiveDesktopInstallation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	desktopID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	iosID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Synthetic iOS','ios',clock_timestamp()+interval '1 day')`, iosID, owner.ID); err != nil {
		t.Fatalf("insert iOS installation: %v", err)
	}
	for _, test := range []struct {
		name           string
		installationID string
		want           bool
	}{
		{name: "desktop", installationID: desktopID, want: true},
		{name: "ios", installationID: iosID},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := store.IsDesktopClientWebSession(ctx, owner.ID, test.installationID)
			if err != nil || allowed != test.want {
				t.Fatalf("IsDesktopClientWebSession(%s)=%t err=%v; want %t", test.name, allowed, err, test.want)
			}
		})
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, desktopID); err != nil {
		t.Fatalf("revoke desktop installation: %v", err)
	}
	if allowed, err := store.IsDesktopClientWebSession(ctx, owner.ID, desktopID); err != nil || allowed {
		t.Fatalf("revoked desktop installation allowed=%t err=%v; want denied", allowed, err)
	}
}

func TestPostgresTaskFileFetchPersistsTaskFenceTicketHashAndOfflineWait(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Synthetic PC','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert target installation: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "file-fetch-fence", "read one local report")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "file-fetch-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: found=%t task=%+v err=%v", found, claimed, err)
	}
	revision := int64(1)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=$3,desired_revision=$3,status='running'
WHERE id=$1 AND owner_id=$2`, claimed.ID, owner.ID, revision); err != nil {
		t.Fatalf("set current Task revision for test: %v", err)
	}
	claimed.CurrentRevision, claimed.DesiredRevision, claimed.Status = &revision, &revision, task.StatusRunning

	newOperation := func(key string, taskRevision int64) app.TaskHubOperation {
		return app.TaskHubOperation{
			OwnerID: owner.ID, TaskID: claimed.ID, Key: key, ToolName: "morde_hub_file_fetch_" + targetID + "_v5",
			TargetInstallationID: targetID, Action: hub.ActionFileFetch,
			TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 5,
			TaskRevision: taskRevision, LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("file.fetch " + key),
			Status: task.OperationPrepared,
		}
	}
	operation := newOperation("agent:file-fetch-online", revision)
	prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare file.fetch operation: replayed=%t err=%v", replayed, err)
	}
	fetchID := mustID(t, domain.NewOperationID)
	fetch := app.TaskFileFetch{
		ID: fetchID, OwnerID: owner.ID, TaskID: claimed.ID, OperationKey: prepared.Key,
		SourceInstallationID: targetID, TargetGrantVersion: prepared.TargetGrantVersion,
		TaskAuthorizationVersion: prepared.TaskAuthorizationVersion, TaskRevision: revision,
		RelativePath: "reports/summary.pdf", Status: task.FileFetchQueued,
	}
	createdFetch, replayed, err := store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: fetch}, claimed.Lease.Owner)
	if err != nil || replayed || createdFetch.ID != fetchID || createdFetch.Status != task.FileFetchQueued {
		t.Fatalf("create file.fetch record: fetch=%+v replayed=%t err=%v", createdFetch, replayed, err)
	}
	changedID := mustID(t, domain.NewOperationID)
	fetch.ID = changedID
	replayedFetch, replayed, err := store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: fetch}, claimed.Lease.Owner)
	if err != nil || !replayed || replayedFetch.ID != fetchID {
		t.Fatalf("same file.fetch operation did not reuse its record: fetch=%+v replayed=%t err=%v", replayedFetch, replayed, err)
	}
	if _, _, err := store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: app.TaskFileFetch{
		ID: changedID, OwnerID: owner.ID, TaskID: claimed.ID, OperationKey: prepared.Key,
		SourceInstallationID: targetID, TargetGrantVersion: prepared.TargetGrantVersion,
		TaskAuthorizationVersion: prepared.TaskAuthorizationVersion, TaskRevision: revision,
		RelativePath: "reports/other.pdf", Status: task.FileFetchQueued,
	}}, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed path for same file.fetch operation = %v; want idempotency conflict", err)
	}

	plaintextTicket := "synthetic-only-ticket-material"
	ticketHash := sha256.Sum256([]byte(plaintextTicket))
	ticketExpiry := time.Now().UTC().Add(app.TaskFileFetchTicketLifetime)
	dispatched, err := store.DispatchTaskFileFetch(ctx, prepared, fetchID, ticketHash[:], &ticketExpiry, true, claimed.Lease.Owner)
	if err != nil || !dispatched {
		t.Fatalf("dispatch authorized fetch: dispatched=%t err=%v", dispatched, err)
	}
	var storedHash []byte
	var storedExpiry time.Time
	if err := store.pool.QueryRow(ctx, `SELECT ticket_hash,ticket_expires_at FROM task_file_fetches WHERE fetch_id=$1`, fetchID).
		Scan(&storedHash, &storedExpiry); err != nil {
		t.Fatalf("read stored ticket digest: %v", err)
	}
	if len(storedHash) != sha256.Size || string(storedHash) == plaintextTicket || string(storedHash) != string(ticketHash[:]) ||
		storedExpiry.IsZero() {
		t.Fatalf("stored ticket data was not hash-only: hash=%x expires=%v", storedHash, storedExpiry)
	}
	status, err := store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, fetchID)
	if err != nil || status.Status != task.FileFetchQueued || status.TaskRevision != revision || status.SourceDeviceName != "Synthetic PC" {
		t.Fatalf("read online file.fetch status: %+v err=%v", status, err)
	}

	offlineOperation := newOperation("agent:file-fetch-offline", revision)
	offlinePrepared, replayed, err := store.PrepareTaskHubOperation(ctx, offlineOperation, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare offline file.fetch operation: replayed=%t err=%v", replayed, err)
	}
	offlineFetchID := mustID(t, domain.NewOperationID)
	offlineFetch := app.TaskFileFetch{
		ID: offlineFetchID, OwnerID: owner.ID, TaskID: claimed.ID, OperationKey: offlinePrepared.Key,
		SourceInstallationID: targetID, TargetGrantVersion: offlinePrepared.TargetGrantVersion,
		TaskAuthorizationVersion: offlinePrepared.TaskAuthorizationVersion, TaskRevision: revision,
		RelativePath: "reports/offline.pdf", Status: task.FileFetchQueued,
	}
	if _, _, err := store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: offlinePrepared, Fetch: offlineFetch}, claimed.Lease.Owner); err != nil {
		t.Fatalf("create offline file.fetch record: %v", err)
	}
	dispatched, err = store.DispatchTaskFileFetch(ctx, offlinePrepared, offlineFetchID, nil, nil, false, claimed.Lease.Owner)
	if err != nil || !dispatched {
		t.Fatalf("persist offline file.fetch wait: dispatched=%t err=%v", dispatched, err)
	}
	status, err = store.GetTaskFileFetch(ctx, owner.ID, claimed.ID, offlineFetchID)
	if err != nil || status.Status != task.FileFetchWaitingDevice || status.WaitingReason != "device_offline" {
		t.Fatalf("offline file.fetch status: %+v err=%v", status, err)
	}

	stale := newOperation("agent:file-fetch-stale", revision+1)
	if _, _, err := store.PrepareTaskHubOperation(ctx, stale, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("prepare stale Task revision file.fetch = %v; want conflict", err)
	}
	if _, err := store.GetTaskFileFetch(ctx, "u7654321", claimed.ID, fetchID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner file.fetch status = %v; want not_found", err)
	}
}
