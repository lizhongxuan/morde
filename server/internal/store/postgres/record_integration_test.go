package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/record"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

func recordTestPrincipal(t *testing.T, store *Store, role identity.Role) identity.Principal {
	t.Helper()
	user := createTestUser(t, store, 1, 2000, 4096)
	if role != identity.RoleUser {
		if _, err := store.pool.Exec(context.Background(), `UPDATE users SET role=$2 WHERE id=$1`, user.ID, string(role)); err != nil {
			t.Fatalf("set record test role: %v", err)
		}
	}
	tokenID := testSessionTokenID(user.ID)
	secretHash := sha256.Sum256([]byte("record-test-session:" + tokenID))
	if _, err := store.CreateToken(context.Background(), identity.Token{
		ID: tokenID, OwnerID: user.ID, Kind: identity.KindSession, SecretHash: secretHash[:],
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create record test session: %v", err)
	}
	return identity.Principal{UserID: user.ID, Role: role, Kind: identity.KindSession, TokenID: tokenID}
}

func TestRecordConsentVersionsAndQueueReservation(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)

	consents, err := store.ListRecordConsents(ctx, owner)
	if err != nil || len(consents) != 2 || consents[0].Enabled || consents[1].Enabled || consents[0].Version != 0 || consents[1].Version != 0 {
		t.Fatalf("default record consents: %#v, %v", consents, err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn,
		Payload: []byte("synthetic record body"), Attachments: []record.AttachmentInput{{
			FileName: "sample.txt", MediaType: "text/plain", Content: []byte("synthetic attachment"),
		}}}
	if item, err := store.EnqueueRecordCapture(ctx, owner, input); err != nil || item.ID != "" {
		t.Fatalf("capture while consent is off: %#v, %v", item, err)
	}

	first, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || !first.Enabled || first.Version != 1 {
		t.Fatalf("first full-record consent: %#v, %v", first, err)
	}
	unchanged, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || unchanged.Version != first.Version {
		t.Fatalf("unchanged consent advanced version: %#v, %v", unchanged, err)
	}
	item, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil || item.State != record.QueueQueued || item.ConsentVersion != first.Version {
		t.Fatalf("enqueue optional capture: %#v, %v", item, err)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.LimitBytes != 1<<30 || quota.RetentionDays != 30 ||
		quota.ReservedBytes != int64(len(input.Payload)+len(input.Attachments[0].Content)) {
		t.Fatalf("default record quota reservation: %#v, %v", quota, err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "records-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != item.ID || claimed[0].LeaseVersion != 1 {
		t.Fatalf("claim record queue item: %#v, %v", claimed, err)
	}

	withdrawn, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false)
	if err != nil || withdrawn.Enabled || withdrawn.Version != 2 {
		t.Fatalf("withdraw full-record consent: %#v, %v", withdrawn, err)
	}
	quota, err = store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.ReservedBytes != 0 {
		t.Fatalf("withdrawal did not release reservation: %#v, %v", quota, err)
	}
	var queueState string
	var queuedPayload []byte
	var queuedAttachments int
	if err := pool.QueryRow(ctx, `SELECT state,payload FROM record_capture_queue WHERE id=$1`, item.ID).Scan(&queueState, &queuedPayload); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_capture_queue_attachments WHERE queue_item_id=$1`, item.ID).Scan(&queuedAttachments); err != nil {
		t.Fatal(err)
	}
	if queueState != string(record.QueueRejected) || len(queuedPayload) != 0 || queuedAttachments != 0 {
		t.Fatalf("withdrawn queue retained content: state=%q payload=%v attachments=%d", queueState, queuedPayload, queuedAttachments)
	}
	if _, err := store.CommitRecordCapture(ctx, item.ID, "records-test", claimed[0].LeaseVersion); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("withdrawn queue commit error = %v", err)
	}
	third, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil || third.Version != 3 {
		t.Fatalf("re-enabled consent was not monotonic: %#v, %v", third, err)
	}

	superAdmin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordQuota(ctx, superAdmin, owner.UserID, 1, 2, 20); err != nil {
		t.Fatalf("set record quota: %v", err)
	}
	quota, err = store.GetRecordQuota(ctx, superAdmin, owner.UserID)
	if err != nil || quota.RetentionDays != 2 || quota.LimitBytes != 20 {
		t.Fatalf("updated record quota: %#v, %v", quota, err)
	}
	if _, err := store.SetRecordQuota(ctx, superAdmin, owner.UserID, 1, 3, 30); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale record quota version: %v", err)
	}
	if _, err := store.EnqueueRecordCapture(ctx, owner, input); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota optional capture error = %v", err)
	}
	var consentReads, consentUpdates, quotaReads, quotaUpdates int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='record.consent.read'),
count(*) FILTER (WHERE action='record.consent.update'),
	count(*) FILTER (WHERE action='record.quota.read')
FROM audit_logs WHERE actor_id=$1`, owner.UserID).Scan(&consentReads, &consentUpdates, &quotaReads); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_id=$1 AND action='record.quota.update'`, superAdmin.UserID).Scan(&quotaUpdates); err != nil {
		t.Fatal(err)
	}
	if consentReads < 1 || consentUpdates != 4 || quotaReads < 2 || quotaUpdates != 1 {
		t.Fatalf("record consent/quota audit rows: reads=%d updates=%d quota_reads=%d quota_updates=%d", consentReads, consentUpdates, quotaReads, quotaUpdates)
	}
}

func TestRecordCaptureTaskIdempotencyAndOwnerStatuses(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	otherOwner := recordTestPrincipal(t, store, identity.RoleUser)
	admin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, TaskID: taskID, Payload: []byte("safe result")}
	start := make(chan struct{})
	type enqueueResult struct {
		item record.QueueItem
		err  error
	}
	results := make(chan enqueueResult, 2)
	for range 2 {
		go func() {
			<-start
			item, err := store.EnqueueRecordCapture(ctx, owner, input)
			results <- enqueueResult{item: item, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.item.ID == "" || first.item.ID != second.item.ID ||
		first.item.State != record.QueueQueued || second.item.State != record.QueueQueued {
		t.Fatalf("same-task concurrent enqueue: first=%#v second=%#v", first, second)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.ReservedBytes != int64(len(input.Payload)) {
		t.Fatalf("same-task enqueue reserved twice: quota=%#v err=%v", quota, err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-task-idempotency-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.item.ID {
		t.Fatalf("claim idempotent task capture: %#v, %v", claimed, err)
	}
	if _, err := store.CommitRecordCapture(ctx, first.item.ID, "record-task-idempotency-test", claimed[0].LeaseVersion); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListRecordCaptureStatuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.item.ID || page.Items[0].TaskID != taskID ||
		page.Items[0].State != record.QueueStored || page.Items[0].FailureCode != "" || page.NextBefore != "" {
		t.Fatalf("stored task capture status: %#v, %v", page, err)
	}
	if page, err := store.ListRecordCaptureStatuses(ctx, otherOwner, record.CaptureStatusQuery{Limit: 10}); err != nil || len(page.Items) != 0 {
		t.Fatalf("another owner read capture status: %#v, %v", page, err)
	}
	consent, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false)
	if err != nil || consent.Enabled {
		t.Fatalf("withdraw consent after stored capture: %#v, %v", consent, err)
	}
	if page, err := store.ListRecordCaptureStatuses(ctx, owner, record.CaptureStatusQuery{Limit: 10}); err != nil || len(page.Items) != 1 || page.Items[0].State != record.QueueStored {
		t.Fatalf("owner could not read status metadata after consent withdrawal: %#v, %v", page, err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	quota, err = store.GetRecordQuota(ctx, admin, owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecordQuota(ctx, admin, owner.UserID, quota.Version, quota.RetentionDays, 1); err != nil {
		t.Fatal(err)
	}
	failedTaskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	input.TaskID = failedTaskID
	failed, err := store.EnqueueRecordCapture(ctx, owner, input)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded || failed.State != record.QueueFailed || failed.TaskID != failedTaskID || failed.FailureCode != "quota_exceeded" || failed.ReservedBytes != 0 {
		t.Fatalf("quota-rejected capture status: %#v, %v", failed, err)
	}
	var payloadBytes, reservedBytes int64
	var failureCode string
	if err := pool.QueryRow(ctx, `SELECT octet_length(payload),reserved_bytes,failure_code FROM record_capture_queue WHERE id=$1`, failed.ID).
		Scan(&payloadBytes, &reservedBytes, &failureCode); err != nil {
		t.Fatal(err)
	}
	if payloadBytes != 0 || reservedBytes != 0 || failureCode != "quota_exceeded" {
		t.Fatalf("quota-rejected status retained content: payload=%d reserved=%d failure=%q", payloadBytes, reservedBytes, failureCode)
	}
	page, err = store.ListRecordCaptureStatuses(ctx, owner, record.CaptureStatusQuery{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != failed.ID || page.Items[0].State != record.QueueFailed || page.NextBefore != failed.ID {
		t.Fatalf("first capture status page: %#v, %v", page, err)
	}
	page, err = store.ListRecordCaptureStatuses(ctx, owner, record.CaptureStatusQuery{Limit: 1, BeforeID: page.NextBefore})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.item.ID || page.NextBefore != "" {
		t.Fatalf("second capture status page: %#v, %v", page, err)
	}
	if replay, err := store.EnqueueRecordCapture(ctx, owner, input); err != nil || replay.ID != failed.ID || replay.State != record.QueueFailed {
		t.Fatalf("failed task status was not idempotent: %#v, %v", replay, err)
	}
}

func TestCloudDSHCompletedTaskCapturePersistsAndReportsStatus(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	ownerUser := createPreRetirementTestUser(t, store, 1, 2000, 4096)
	owner := identity.Principal{
		UserID: ownerUser.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(ownerUser.ID),
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	access, err := app.NewRecordAccessService(store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithRecordCapture(store, &app.RuntimeToolExecutor{}, nil, nil, capture)
	if err != nil {
		t.Fatal(err)
	}
	completeTask := func(key, objective, result string, captureOnCompletion bool) task.Task {
		t.Helper()
		submission := newTestTaskSubmission(t, owner.UserID, "", key, objective)
		submission.Task.ExecutionLocation = "server"
		created, err := store.SubmitTask(ctx, submission)
		if err != nil {
			t.Fatalf("submit Cloud DSH Task: %v", err)
		}
		claimed, found, err := store.ClaimNextTask(ctx, "record-capture-"+key, time.Minute)
		if err != nil || !found || claimed.ID != created.Task.ID {
			t.Fatalf("claim Cloud DSH Task: task=%#v found=%t err=%v", claimed, found, err)
		}
		sessionID, instanceID := "session_"+key, "agent_"+key
		digest := bytes.Repeat([]byte{7}, 32)
		if _, err := pool.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=1,agent_session_id=$3,
checkpoint_through_seq=2,checkpoint_digest=$4 WHERE id=$1`, claimed.ID, instanceID, sessionID, digest); err != nil {
			t.Fatalf("set Cloud DSH completion fence: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest) VALUES ($1,$2,$3,2,$4)`,
			claimed.ID, owner.UserID, sessionID, digest); err != nil {
			t.Fatalf("persist Cloud DSH root session: %v", err)
		}
		details, err := store.GetTask(ctx, claimed.ID, owner.UserID)
		if err != nil {
			t.Fatalf("load claimed Cloud DSH Task: %v", err)
		}
		current := details.Task
		completionBroker := broker
		if !captureOnCompletion {
			completionBroker, err = app.NewCloudDSHToolBroker(store, &app.RuntimeToolExecutor{}, nil, nil)
			if err != nil {
				t.Fatalf("create Cloud DSH broker without capture hook: %v", err)
			}
		}
		if err := completionBroker.Complete(ctx, current, app.CloudDSHTaskCompletion{
			AgentInstanceID: current.AgentInstanceID, AgentGeneration: current.AgentGeneration,
			AuthorizationVersion: current.Authorization.Version, AgentSessionID: current.AgentSessionID,
			CheckpointThroughSequence: current.CheckpointThroughSequence, CheckpointDigest: current.CheckpointDigest,
			Result: result,
		}); err != nil {
			t.Fatalf("complete Cloud DSH Task: %v", err)
		}
		completed, err := store.GetTask(ctx, current.ID, owner.UserID)
		if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != result {
			t.Fatalf("Cloud DSH Task after record capture: task=%#v err=%v", completed.Task, err)
		}
		return completed.Task
	}

	first := completeTask("record-e2e-success", "Summarize a safe result.", "The local integration succeeded.", true)
	queued, err := capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(queued.Items) != 1 || queued.Items[0].TaskID != first.ID || queued.Items[0].State != record.QueueQueued {
		t.Fatalf("capture status after Task completion: %#v err=%v", queued, err)
	}
	lostTask := completeTask("record-e2e-crash-gap", "Recover a safe result after a process crash.", "The reconciler recovered this result.", false)
	queued, err = capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(queued.Items) != 1 || queued.Items[0].TaskID != first.ID {
		t.Fatalf("crash-gap task was captured before reconciliation: %#v err=%v", queued, err)
	}
	if err := capture.RecoverCloudDSHCaptures(ctx, 10); err != nil {
		t.Fatalf("recover crash-gap capture: %v", err)
	}
	queued, err = capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(queued.Items) != 2 {
		t.Fatalf("capture status after crash-gap recovery: %#v err=%v", queued, err)
	}
	for _, item := range queued.Items {
		if item.TaskID == lostTask.ID && item.State != record.QueueQueued {
			t.Fatalf("recovered capture state = %q, want queued", item.State)
		}
	}
	worker, err := worker.NewRecordCaptureWorker(worker.RecordCaptureWorkerOptions{
		Capture: capture, Access: access, WorkerID: "record-capture-e2e", PollInterval: 5 * time.Millisecond, BatchSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	workerStopped := false
	defer func() {
		cancelWorker()
		if !workerStopped {
			if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("record worker stopped with %v", err)
			}
		}
	}()
	stored := make(map[string]record.CaptureStatus)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page, err := capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
		if err != nil {
			t.Fatalf("read capture status while worker runs: %v", err)
		}
		for _, item := range page.Items {
			if item.State == record.QueueStored && (item.TaskID == first.ID || item.TaskID == lostTask.ID) {
				stored[item.TaskID] = item
			}
		}
		if len(stored) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelWorker()
	if err := <-workerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("record worker stopped with %v, want context cancellation", err)
	}
	workerStopped = true
	if len(stored) != 2 {
		t.Fatal("record worker did not persist both completed Task captures")
	}
	for _, completed := range []task.Task{first, lostTask} {
		var recordID string
		if err := pool.QueryRow(ctx, `SELECT record_id FROM record_capture_queue WHERE owner_id=$1 AND source_task_id=$2`, owner.UserID, completed.ID).Scan(&recordID); err != nil {
			t.Fatalf("read stored record id: %v", err)
		}
		content, err := access.Read(ctx, owner, owner.UserID, recordID)
		if err != nil || !bytes.Contains(content.Payload, []byte(completed.Objective)) || !bytes.Contains(content.Payload, []byte(completed.Result)) {
			t.Fatalf("read captured objective and result for %s: content=%q err=%v", completed.ID, content.Payload, err)
		}
	}
	staleConsentTask := completeTask("record-e2e-stale-consent", "Do not backfill after renewed consent.", "Consent changed after this task.", false)
	candidates, err := store.ListRecoverableCloudDSHCaptures(ctx, 10)
	if err != nil {
		t.Fatalf("list recoverable captures before consent race: %v", err)
	}
	var staleCandidate *app.CloudDSHCaptureCandidate
	for index := range candidates {
		if candidates[index].TaskID == staleConsentTask.ID {
			staleCandidate = &candidates[index]
			break
		}
	}
	if staleCandidate == nil {
		t.Fatal("completed Cloud DSH task was not eligible for recovery under its original consent")
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	if item, err := store.EnqueueRecordCaptureAtConsentVersion(ctx, staleCandidate.Principal, record.CaptureInput{
		Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, TaskID: staleCandidate.TaskID,
		Payload: []byte(`{"objective":"old","result":"must not be captured"}`),
	}, staleCandidate.ConsentVersion); err != nil || item.ID != "" {
		t.Fatalf("stale consent version was enqueued: %#v err=%v", item, err)
	}
	if err := capture.RecoverCloudDSHCaptures(ctx, 10); err != nil {
		t.Fatalf("recover after consent changed: %v", err)
	}
	statuses, err := capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range statuses.Items {
		if item.TaskID == staleConsentTask.ID {
			t.Fatalf("recovery backfilled a task after consent changed: %#v", item)
		}
	}
	rejectedTask := completeTask("record-e2e-rejected-secret", "Summarize a local result safely.", "password=local-test-value", false)
	if err := capture.RecoverCloudDSHCaptures(ctx, 10); err == nil {
		t.Fatal("recovery accepted secret-like content")
	}
	statuses, err = capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var rejected record.CaptureStatus
	for _, item := range statuses.Items {
		if item.TaskID == rejectedTask.ID {
			rejected = item
			break
		}
	}
	if rejected.ID == "" || rejected.State != record.QueueFailed || rejected.FailureCode != "content_rejected" {
		t.Fatalf("rejected recovery status = %#v", rejected)
	}
	var rejectedPayloadBytes int
	if err := pool.QueryRow(ctx, `SELECT octet_length(payload) FROM record_capture_queue WHERE id=$1`, rejected.ID).Scan(&rejectedPayloadBytes); err != nil || rejectedPayloadBytes != 0 {
		t.Fatalf("rejected recovery retained payload: bytes=%d err=%v", rejectedPayloadBytes, err)
	}

	admin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	quota, err := store.GetRecordQuota(ctx, admin, owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecordQuota(ctx, admin, owner.UserID, quota.Version, 30, 1); err != nil {
		t.Fatalf("reduce record quota for failure injection: %v", err)
	}
	second := completeTask("record-e2e-quota", "Summarize a result exceeding the tiny quota.", "Quota failure remains isolated.", true)
	if second.Status != task.StatusCompleted {
		t.Fatalf("record quota failure changed Task status: %#v", second)
	}
	statuses, err = capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var failed record.CaptureStatus
	for _, item := range statuses.Items {
		if item.TaskID == second.ID {
			failed = item
			break
		}
	}
	if failed.ID == "" || failed.State != record.QueueFailed || failed.FailureCode != "quota_exceeded" {
		t.Fatalf("quota failure status = %#v", failed)
	}
	var failedPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM record_capture_queue WHERE owner_id=$1 AND source_task_id=$2`, owner.UserID, second.ID).Scan(&failedPayload); err != nil || len(failedPayload) != 0 {
		t.Fatalf("failed capture retained payload: bytes=%d err=%v", len(failedPayload), err)
	}
}

type recordCloudDSHE2ESupervisor struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	key []byte
}

func (s *recordCloudDSHE2ESupervisor) Start(_ context.Context, launch worker.CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := exec.Command(launch.NodePath, launch.ScriptPath)
	cmd.Env = append([]string(nil), launch.Environment...)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	s.cmd = cmd
	return in, out, nil
}

func (*recordCloudDSHE2ESupervisor) Renew(context.Context, app.AgentInstanceFence, time.Time) error {
	return nil
}

func (s *recordCloudDSHE2ESupervisor) Stop(_ context.Context, fence app.AgentInstanceFence, nonce []byte) (app.AgentStopReceipt, error) {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	receipt := app.AgentStopReceipt{Fence: fence, SupervisorNonce: append([]byte(nil), nonce...), TreeStopped: true}
	payload, err := app.AgentStopReceiptSigningPayload(receipt)
	if err != nil {
		return receipt, err
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	receipt.Signature = mac.Sum(nil)
	return receipt, nil
}

type recordCloudDSHE2EModel struct{ calls int }

func (m *recordCloudDSHE2EModel) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Source != app.ModelCallTask || call.Task == nil {
		return app.ModelResponse{}, domain.NewError(domain.CodeInvalidState, "runtime model call was not task scoped")
	}
	m.calls++
	return app.ModelResponse{Text: "The real local DSH runtime completed.", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 6}}, nil
}

func TestCloudDSHRuntimeCompletesAndCapturesThroughPostgres(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	user := createPreRetirementTestUser(t, store, 1, 2000, 4096)
	owner := identity.Principal{UserID: user.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(user.ID)}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{9}, 32)
	authenticator, err := app.NewAgentStopAuthenticator("record-e2e-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithRecordCapture(store, &app.RuntimeToolExecutor{}, nil, nil, capture)
	if err != nil {
		t.Fatal(err)
	}
	model := &recordCloudDSHE2EModel{}
	supervisor := &recordCloudDSHE2ESupervisor{key: key}
	executor, err := worker.NewCloudDSHExecutor(worker.CloudDSHExecutorOptions{
		NodePath: node, ScriptPath: script, SupervisorID: "record-e2e-supervisor", Supervisor: supervisor,
		Instances: instances, Checkpoints: checkpoints, Models: model, Completion: broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.UserID, "", "record-runtime-e2e", "Summarize the local runtime integration.")
	submission.Task.ExecutionLocation = "server"
	submission.Task.ModelID = "p1234567"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Cloud DSH task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "record-runtime-e2e-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Cloud DSH task: task=%#v found=%t err=%v", claimed, found, err)
	}
	if err := executor.ExecuteTask(ctx, claimed); err != nil {
		t.Fatalf("run local Cloud DSH runtime: %v", err)
	}
	completed, err := store.GetTask(ctx, claimed.ID, owner.UserID)
	if err != nil || completed.Task.Status != task.StatusCompleted || completed.Task.Result != "The real local DSH runtime completed." || model.calls != 1 {
		t.Fatalf("Cloud DSH completion: task=%#v model_calls=%d err=%v", completed.Task, model.calls, err)
	}
	queued, err := capture.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(queued.Items) != 1 || queued.Items[0].TaskID != claimed.ID || queued.Items[0].State != record.QueueQueued {
		t.Fatalf("Cloud DSH runtime record capture: %#v err=%v", queued, err)
	}
	var storedResult string
	if err := pool.QueryRow(ctx, `SELECT result FROM tasks WHERE id=$1`, claimed.ID).Scan(&storedResult); err != nil || storedResult != completed.Task.Result {
		t.Fatalf("persisted runtime result=%q err=%v", storedResult, err)
	}
}

func TestRecordCaptureRejectedContentLeavesOnlyFailureStatus(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Capture(ctx, owner, record.CaptureInput{
		Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, TaskID: taskID,
		Payload: []byte(`{"api_key":"synthetic-secret-value"}`),
	})
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("sensitive capture error = %v", err)
	}
	page, err := service.Statuses(ctx, owner, record.CaptureStatusQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TaskID != taskID || page.Items[0].State != record.QueueFailed ||
		page.Items[0].FailureCode != "content_rejected" {
		t.Fatalf("sensitive capture status: %#v, %v", page, err)
	}
	var payloadBytes, reservedBytes int64
	var failureCode string
	if err := pool.QueryRow(ctx, `SELECT octet_length(payload),reserved_bytes,failure_code FROM record_capture_queue WHERE id=$1`, page.Items[0].ID).
		Scan(&payloadBytes, &reservedBytes, &failureCode); err != nil {
		t.Fatal(err)
	}
	if payloadBytes != 0 || reservedBytes != 0 || failureCode != "content_rejected" {
		t.Fatalf("rejected content retained data: payload=%d reserved=%d failure=%q", payloadBytes, reservedBytes, failureCode)
	}
}

func TestRecordQuotaReductionPreservesExistingAndReservedRecords(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	admin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	existingInput := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("existing record")}
	existingQueue, err := store.EnqueueRecordCapture(ctx, owner, existingInput)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-quota-reduction-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != existingQueue.ID {
		t.Fatalf("claim existing record: %#v, %v", claimed, err)
	}
	existingRecord, err := store.CommitRecordCapture(ctx, existingQueue.ID, "record-quota-reduction-test", claimed[0].LeaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	pendingInput := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("accepted reservation")}
	pendingQueue, err := store.EnqueueRecordCapture(ctx, owner, pendingInput)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.UsedBytes != int64(len(existingInput.Payload)) || quota.ReservedBytes != int64(len(pendingInput.Payload)) {
		t.Fatalf("usage before quota reduction: %#v, %v", quota, err)
	}
	lowered, err := store.SetRecordQuota(ctx, admin, owner.UserID, quota.Version, quota.RetentionDays, 1)
	if err != nil || lowered.LimitBytes != 1 || lowered.UsedBytes != quota.UsedBytes || lowered.ReservedBytes != quota.ReservedBytes {
		t.Fatalf("lower quota below current usage: %#v, %v", lowered, err)
	}
	claimed, err = store.ClaimRecordCaptures(ctx, "record-quota-reduction-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != pendingQueue.ID {
		t.Fatalf("claim existing reservation: %#v, %v", claimed, err)
	}
	pendingRecord, err := store.CommitRecordCapture(ctx, pendingQueue.ID, "record-quota-reduction-test", claimed[0].LeaseVersion)
	if err != nil || pendingRecord.State != "active" {
		t.Fatalf("commit reservation accepted before quota reduction: %#v, %v", pendingRecord, err)
	}
	quota, err = store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.LimitBytes != 1 || quota.UsedBytes != int64(len(existingInput.Payload)+len(pendingInput.Payload)) || quota.ReservedBytes != 0 {
		t.Fatalf("quota after reserved commit: %#v, %v", quota, err)
	}
	if content, err := store.ReadRecord(ctx, owner, owner.UserID, existingRecord.ID); err != nil || string(content.Payload) != string(existingInput.Payload) {
		t.Fatalf("quota reduction removed an existing record: %#v, %v", content, err)
	}
	if _, err := store.EnqueueRecordCapture(ctx, owner, record.CaptureInput{
		Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("new"),
	}); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("new capture below existing usage error = %v", err)
	}
}

func TestRecordCapturePendingLimitCountsLeasedAndSerializesEnqueues(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("x")}
	for index := int64(0); index < maxPendingRecordCapturesPerOwner-1; index++ {
		if _, err := store.EnqueueRecordCapture(ctx, owner, input); err != nil {
			t.Fatalf("enqueue pending item %d: %v", index, err)
		}
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-pending-limit-test", time.Now().UTC(), time.Minute, 60)
	if err != nil || len(claimed) != 60 {
		t.Fatalf("claim pending items: got %d, err %v", len(claimed), err)
	}
	type enqueueResult struct {
		item record.QueueItem
		err  error
	}
	const concurrentEnqueues = 8
	start := make(chan struct{})
	results := make(chan enqueueResult, concurrentEnqueues)
	for index := 0; index < concurrentEnqueues; index++ {
		go func() {
			<-start
			item, err := store.EnqueueRecordCapture(ctx, owner, input)
			results <- enqueueResult{item: item, err: err}
		}()
	}
	close(start)
	successes, quotaFailures := 0, 0
	for index := 0; index < concurrentEnqueues; index++ {
		result := <-results
		if result.err == nil && result.item.State == record.QueueQueued {
			successes++
		} else if domain.CodeOf(result.err) == domain.CodeQuotaExceeded {
			quotaFailures++
		} else {
			t.Errorf("concurrent enqueue result: item=%#v err=%v", result.item, result.err)
		}
	}
	if successes != 1 || quotaFailures != concurrentEnqueues-1 {
		t.Fatalf("pending queue limit admitted %d and rejected %d; want 1 admitted and %d rejected", successes, quotaFailures, concurrentEnqueues-1)
	}
	var queued, leased int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state='leased')
FROM record_capture_queue WHERE owner_id=$1`, owner.UserID).Scan(&queued, &leased); err != nil {
		t.Fatal(err)
	}
	if queued+leased != maxPendingRecordCapturesPerOwner || leased != 60 {
		t.Fatalf("pending queue states: queued=%d leased=%d", queued, leased)
	}
	if _, err := store.EnqueueRecordCapture(ctx, owner, input); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("enqueue beyond pending limit error = %v", err)
	}
	fullTaskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	input.TaskID = fullTaskID
	failed, err := store.EnqueueRecordCapture(ctx, owner, input)
	if domain.CodeOf(err) != domain.CodeQuotaExceeded || failed.State != record.QueueFailed || failed.FailureCode != "queue_full" || failed.TaskID != fullTaskID {
		t.Fatalf("queue-full task status: %#v, %v", failed, err)
	}
}

func TestRecordCaptureRetentionIsFixedAndExpiredRecordsAreCleaned(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	admin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := store.SetRecordQuota(ctx, admin, owner.UserID, quota.Version, 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("retention record")}
	item, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := item.CapturedAt.AddDate(0, 0, 2)
	if !item.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("capture expiry = %s, want capture time plus 2 days %s", item.ExpiresAt, wantExpiry)
	}
	if _, err := store.SetRecordQuota(ctx, admin, owner.UserID, configured.Version, 1, 1024); err != nil {
		t.Fatalf("change retention for future captures: %v", err)
	}
	var queuedExpiry time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM record_capture_queue WHERE id=$1`, item.ID).Scan(&queuedExpiry); err != nil {
		t.Fatal(err)
	}
	if !queuedExpiry.Truncate(time.Microsecond).Equal(item.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("quota change rewrote queued expiry: got %s, want %s", queuedExpiry, item.ExpiresAt)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-retention-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != item.ID {
		t.Fatalf("claim retained record: %#v, %v", claimed, err)
	}
	metadata, err := store.CommitRecordCapture(ctx, item.ID, "record-retention-test", claimed[0].LeaseVersion)
	if err != nil || !metadata.ExpiresAt.Equal(queuedExpiry) {
		t.Fatalf("stored record expiry: %s, queue expiry %s, error %v", metadata.ExpiresAt, queuedExpiry, err)
	}
	next, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	if !next.ExpiresAt.Equal(next.CapturedAt.AddDate(0, 0, 1)) {
		t.Fatalf("new capture did not use updated retention: expiry=%s capture=%s", next.ExpiresAt, next.CapturedAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE record_entries SET captured_at=clock_timestamp()-interval '2 seconds', expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, metadata.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := store.ProcessRecordDeletes(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("process expired record: processed=%d err=%v", processed, err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM record_entries WHERE id=$1`, metadata.ID).Scan(&state); err != nil || state != "deleted" {
		t.Fatalf("expired record state=%q err=%v", state, err)
	}
	var tombstoneState string
	if err := pool.QueryRow(ctx, `SELECT state FROM record_delete_tombstones WHERE record_id=$1`, metadata.ID).Scan(&tombstoneState); err != nil || tombstoneState != string(record.DeleteCompleted) {
		t.Fatalf("expired record tombstone state=%q err=%v", tombstoneState, err)
	}
	var bodyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_turn_bodies WHERE record_id=$1`, metadata.ID).Scan(&bodyCount); err != nil || bodyCount != 0 {
		t.Fatalf("expired record body count=%d err=%v", bodyCount, err)
	}
	quota, err = store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.UsedBytes != 0 || quota.ReservedBytes != int64(len(input.Payload)) {
		t.Fatalf("expired cleanup quota accounting: %#v, %v", quota, err)
	}
}

func TestRecordAdminGrantsExportAttachmentAndIdempotentDelete(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	admin := recordTestPrincipal(t, store, identity.RoleAdmin)
	superAdmin := recordTestPrincipal(t, store, identity.RoleSuperAdmin)
	if !requirePersonalAttachmentSchema(t, ctx, pool) {
		return
	}
	createPersonalLibraryItemForAttachmentTest(t, store, owner, "library-item_1")
	ordinaryContent := []byte("ordinary personal attachment")
	ordinaryUpload := newPersonalAttachmentUploadForTest(t, owner.UserID, "library-item_1", 1, ordinaryContent)
	ordinaryCreated, err := store.CreatePersonalAttachmentUpload(ctx, owner, ordinaryUpload, "record-delete-ordinary-create")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryReservation, err := store.ReservePersonalAttachment(ctx, owner, ordinaryCreated.UploadID, int64(len(ordinaryContent)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, owner, ordinaryCreated.UploadID, ordinaryReservation); err != nil {
		t.Fatal(err)
	}
	ordinaryDigest := sha256.Sum256(ordinaryContent)
	ordinaryChunk := personal.AttachmentChunk{
		UploadID: ordinaryCreated.UploadID, Sequence: 0, SizeBytes: int64(len(ordinaryContent)), SHA256: hex.EncodeToString(ordinaryDigest[:]),
	}
	if _, err := store.PutPersonalAttachmentChunk(ctx, owner, ordinaryChunk, bytes.NewReader(ordinaryContent)); err != nil {
		t.Fatal(err)
	}
	ordinaryReceipt, err := store.CommitPersonalAttachment(ctx, owner, ordinaryCreated.UploadID, "record-delete-ordinary-commit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true); err != nil {
		t.Fatal(err)
	}
	input := record.CaptureInput{Purpose: record.PurposeFullRecord, Kind: record.ItemTurn,
		Payload: []byte("private synthetic body"), Attachments: []record.AttachmentInput{{
			FileName: "sample.txt", MediaType: "text/plain", Content: []byte("private synthetic attachment"),
		}}}
	queueItem, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-access-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim record for access test: %#v, %v", claimed, err)
	}
	metadata, err := store.CommitRecordCapture(ctx, queueItem.ID, "record-access-test", claimed[0].LeaseVersion)
	if err != nil || metadata.State != "active" || metadata.PayloadBytes != int64(len(input.Payload)) ||
		metadata.AttachmentBytes != int64(len(input.Attachments[0].Content)) || metadata.AttachmentCount != 1 {
		t.Fatalf("commit record capture: %#v, %v", metadata, err)
	}
	usedQuota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || usedQuota.UsedBytes != int64(len(input.Payload)+len(input.Attachments[0].Content)) {
		t.Fatalf("attachment bytes were not included in used quota: %#v, %v", usedQuota, err)
	}
	if _, err := store.ListRecordMetadata(ctx, admin, owner.UserID, record.Query{Limit: 10}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("metadata was readable without view grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, admin, owner.UserID, admin.UserID, record.CapabilityView, true); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin grant change was allowed: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityView, true); err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetAdminRecordSettings(ctx, superAdmin, owner.UserID)
	if err != nil || len(settings.Consents) != 2 || !settings.Consents[0].Enabled ||
		settings.Quota.LimitBytes != record.DefaultQuotaBytes || len(settings.Grants) != 1 || !settings.Grants[0].Granted {
		t.Fatalf("superadmin record settings: %#v, %v", settings, err)
	}
	if _, err := store.GetAdminRecordSettings(ctx, admin, owner.UserID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin read of record settings was allowed: %v", err)
	}
	adminAccess, err := store.GetRecordAdminAccess(ctx, admin, owner.UserID)
	if err != nil || len(adminAccess.Consents) != 2 || len(adminAccess.Grants) != 4 || !adminAccess.Grants[0].Granted && !adminAccess.Grants[1].Granted {
		t.Fatalf("administrator's own record access: %#v, %v", adminAccess, err)
	}
	for _, grant := range adminAccess.Grants {
		if grant.AdminID != admin.UserID {
			t.Fatalf("record access exposed another administrator's grant: %#v", grant)
		}
	}
	page, err := store.ListRecordMetadata(ctx, admin, owner.UserID, record.Query{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != metadata.ID {
		t.Fatalf("metadata with view grant: %#v, %v", page, err)
	}
	content, err := store.ReadRecord(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || string(content.Payload) != string(input.Payload) {
		t.Fatalf("detail with view grant: %#v, %v", content, err)
	}
	if _, err := store.ExportRecord(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("export was allowed without export grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityExport, true); err != nil {
		t.Fatal(err)
	}
	if exported, err := store.ExportRecord(ctx, admin, owner.UserID, metadata.ID); err != nil || string(exported.Payload) != string(input.Payload) {
		t.Fatalf("explicit export grant: %#v, %v", exported, err)
	}
	if _, err := store.ListRecordAttachments(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("attachment metadata was allowed without attachment grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityAttachment, true); err != nil {
		t.Fatal(err)
	}
	attachments, err := store.ListRecordAttachments(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("list record attachments: %#v, %v", attachments, err)
	}
	attachment, err := store.ReadRecordAttachment(ctx, admin, owner.UserID, metadata.ID, attachments[0].ID)
	if err != nil || string(attachment.Content) != string(input.Attachments[0].Content) {
		t.Fatalf("read record attachment: %#v, %v", attachment, err)
	}
	if _, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("record deletion was allowed without delete grant: %v", err)
	}
	if _, err := store.SetRecordAdminGrant(ctx, superAdmin, owner.UserID, admin.UserID, record.CapabilityDelete, true); err != nil {
		t.Fatal(err)
	}
	firstDelete, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayedDelete, err := store.RequestRecordDelete(ctx, admin, owner.UserID, metadata.ID)
	if err != nil || firstDelete.ID != replayedDelete.ID {
		t.Fatalf("delete request was not idempotent: %#v %#v %v", firstDelete, replayedDelete, err)
	}
	processed, err := store.ProcessRecordDeletes(ctx, 10)
	if err != nil || processed != 1 {
		t.Fatalf("process record deletion: processed=%d err=%v", processed, err)
	}
	var deleteState string
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT state,completed_at FROM record_delete_tombstones WHERE id=$1`, firstDelete.ID).Scan(&deleteState, &completedAt); err != nil {
		t.Fatal(err)
	}
	if deleteState != string(record.DeleteCompleted) || completedAt == nil {
		t.Fatalf("delete tombstone was not retained as complete: %s %v", deleteState, completedAt)
	}
	var turnBodies, attachmentsLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_turn_bodies WHERE record_id=$1`, metadata.ID).Scan(&turnBodies); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_attachments WHERE record_id=$1`, metadata.ID).Scan(&attachmentsLeft); err != nil {
		t.Fatal(err)
	}
	if turnBodies != 0 || attachmentsLeft != 0 {
		t.Fatalf("record deletion left content: bodies=%d attachments=%d", turnBodies, attachmentsLeft)
	}
	var ordinaryAttachments, queuedAttachments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_attachment_uploads`).Scan(&ordinaryAttachments); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_capture_queue_attachments WHERE queue_item_id=$1`, queueItem.ID).Scan(&queuedAttachments); err != nil {
		t.Fatal(err)
	}
	if ordinaryAttachments != 1 || queuedAttachments != 0 {
		t.Fatalf("record cleanup crossed attachment stores: personal=%d queue=%d", ordinaryAttachments, queuedAttachments)
	}
	reader, ordinaryMetadata, err := store.OpenPersonalAttachment(ctx, owner, ordinaryReceipt.AttachmentID)
	if err != nil {
		t.Fatalf("record deletion made ordinary attachment unavailable: %v", err)
	}
	ordinaryRead, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(ordinaryRead) != string(ordinaryContent) || ordinaryMetadata.AttachmentID != ordinaryReceipt.AttachmentID {
		t.Fatalf("ordinary attachment after record deletion: data=%q metadata=%#v err=%v", ordinaryRead, ordinaryMetadata, err)
	}
	quota, err := store.GetRecordQuota(ctx, owner, owner.UserID)
	if err != nil || quota.UsedBytes != 0 || quota.ReservedBytes != 0 {
		t.Fatalf("record deletion did not release quota: %#v, %v", quota, err)
	}

	secondQueue, err := store.EnqueueRecordCapture(ctx, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimRecordCaptures(ctx, "record-access-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != secondQueue.ID {
		t.Fatalf("claim second record: %#v, %v", claimed, err)
	}
	second, err := store.CommitRecordCapture(ctx, secondQueue.ID, "record-access-test", claimed[0].LeaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	ownerAttachments, err := store.ListRecordAttachments(ctx, owner, owner.UserID, second.ID)
	if err != nil || len(ownerAttachments) != 1 {
		t.Fatalf("list owner's record attachments before withdrawal: %#v, %v", ownerAttachments, err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRecordMetadata(ctx, owner, owner.UserID, record.Query{Purpose: record.PurposeFullRecord, Limit: 10}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("owner listed records after consent withdrawal: %v", err)
	}
	if page, err := store.ListRecordMetadata(ctx, owner, owner.UserID, record.Query{Limit: 10}); err != nil || len(page.Items) != 0 {
		t.Fatalf("owner saw withdrawn records in an unfiltered list: %#v, %v", page, err)
	}
	if _, err := store.ReadRecord(ctx, owner, owner.UserID, second.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("owner read record after consent withdrawal: %v", err)
	}
	if _, err := store.ExportRecord(ctx, owner, owner.UserID, second.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("owner exported record after consent withdrawal: %v", err)
	}
	if _, err := store.ListRecordAttachments(ctx, owner, owner.UserID, second.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("owner listed record attachments after consent withdrawal: %v", err)
	}
	if _, err := store.ReadRecordAttachment(ctx, owner, owner.UserID, second.ID, ownerAttachments[0].ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("owner read record attachment after consent withdrawal: %v", err)
	}
	if _, err := store.RequestRecordDelete(ctx, admin, owner.UserID, second.ID); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("administrator deleted after consent withdrawal: %v", err)
	}
	ownerDelete, err := store.RequestRecordDelete(ctx, owner, owner.UserID, second.ID)
	if err != nil {
		t.Fatalf("owner could not delete after withdrawing consent: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE record_quotas SET used_bytes=0 WHERE owner_id=$1`, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if processed, err := store.ProcessRecordDeletes(ctx, 10); domain.CodeOf(err) != domain.CodeInternal || processed != 0 {
		t.Fatalf("failed cleanup result: processed=%d err=%v", processed, err)
	}
	var failureState string
	if err := pool.QueryRow(ctx, `SELECT state FROM record_delete_tombstones WHERE id=$1`, ownerDelete.ID).Scan(&failureState); err != nil || failureState != string(record.DeleteFailed) {
		t.Fatalf("failed cleanup lost its tombstone: state=%q err=%v", failureState, err)
	}
	var retainedBody int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_turn_bodies WHERE record_id=$1`, second.ID).Scan(&retainedBody); err != nil || retainedBody != 1 {
		t.Fatalf("failed cleanup partially removed content: rows=%d err=%v", retainedBody, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE record_quotas SET used_bytes=$2 WHERE owner_id=$1`, owner.UserID,
		int64(len(input.Payload)+len(input.Attachments[0].Content))); err != nil {
		t.Fatal(err)
	}
	if processed, err := store.ProcessRecordDeletes(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("retry owner deletion: processed=%d err=%v", processed, err)
	}
	wantActions := map[string]bool{
		"record.grant.update": false, "record.settings.read": false, "record.access.read": false,
		"record.list": false, "record.read": false, "record.export": false,
		"record.attachments.read": false, "record.attachment.read": false,
		"record.delete": false, "record.delete.complete": false,
	}
	rows, err := pool.Query(ctx, `SELECT action,detail::text FROM audit_logs WHERE action LIKE 'record.%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if _, exists := wantActions[action]; exists {
			wantActions[action] = true
		}
		if strings.Contains(detail, string(input.Payload)) || strings.Contains(detail, string(input.Attachments[0].Content)) {
			rows.Close()
			t.Fatalf("audit detail exposed record contents: %q", detail)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for action, found := range wantActions {
		if !found {
			t.Errorf("missing audited record operation %q", action)
		}
	}
}

func TestRecordCaptureCommitRechecksConsentVersion(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeTrainingContribution, true); err != nil {
		t.Fatal(err)
	}
	item, err := store.EnqueueRecordCapture(ctx, owner, record.CaptureInput{
		Purpose: record.PurposeTrainingContribution, Kind: record.ItemSample, Payload: []byte("synthetic sample"),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimRecordCaptures(ctx, "record-version-test", time.Now().UTC(), time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim training sample: %#v, %v", claimed, err)
	}
	if _, err := store.SetRecordConsent(ctx, owner, record.PurposeTrainingContribution, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRecordCapture(ctx, item.ID, "record-version-test", claimed[0].LeaseVersion); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale consent capture commit error = %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM record_entries WHERE owner_id=$1`, owner.UserID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale capture was committed: records=%d", count)
	}
}

func TestRecordCaptureWithdrawalAndReconsentRaceIsLinearizable(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := recordTestPrincipal(t, store, identity.RoleUser)
	consent, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 8; iteration++ {
		item, err := store.EnqueueRecordCapture(ctx, owner, record.CaptureInput{
			Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, Payload: []byte("race synthetic record"),
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimRecordCaptures(ctx, "record-race-test", time.Now().UTC(), time.Minute, 10)
		if err != nil || len(claimed) != 1 || claimed[0].ID != item.ID {
			t.Fatalf("claim race capture: %#v, %v", claimed, err)
		}
		start := make(chan struct{})
		commitResult := make(chan error, 1)
		withdrawResult := make(chan error, 1)
		go func() {
			<-start
			_, err := store.CommitRecordCapture(ctx, item.ID, "record-race-test", claimed[0].LeaseVersion)
			commitResult <- err
		}()
		go func() {
			<-start
			if _, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, false); err != nil {
				withdrawResult <- err
				return
			}
			_, err := store.SetRecordConsent(ctx, owner, record.PurposeFullRecord, true)
			withdrawResult <- err
		}()
		close(start)
		commitErr := <-commitResult
		if err := <-withdrawResult; err != nil {
			t.Fatalf("withdraw and re-enable consent: %v", err)
		}
		var state, recordID string
		if err := pool.QueryRow(ctx, `SELECT state,COALESCE(record_id,'') FROM record_capture_queue WHERE id=$1`, item.ID).Scan(&state, &recordID); err != nil {
			t.Fatal(err)
		}
		switch state {
		case string(record.QueueStored):
			if commitErr != nil || recordID == "" {
				t.Fatalf("committed queue result: state=%s id=%q err=%v", state, recordID, commitErr)
			}
			var capturedVersion int64
			if err := pool.QueryRow(ctx, `SELECT consent_version FROM record_entries WHERE id=$1`, recordID).Scan(&capturedVersion); err != nil || capturedVersion != consent.Version {
				t.Fatalf("stored consent version=%d want=%d err=%v", capturedVersion, consent.Version, err)
			}
		case string(record.QueueRejected):
			if domain.CodeOf(commitErr) != domain.CodeInvalidState || recordID != "" {
				t.Fatalf("rejected queue result: id=%q err=%v", recordID, commitErr)
			}
		default:
			t.Fatalf("queue did not reach a terminal state after consent race: %s", state)
		}
		var reserved int64
		if err := pool.QueryRow(ctx, `SELECT reserved_bytes FROM record_quotas WHERE owner_id=$1`, owner.UserID).Scan(&reserved); err != nil || reserved != 0 {
			t.Fatalf("race left reserved quota: %d err=%v", reserved, err)
		}
		consent.Version += 2
	}
}
