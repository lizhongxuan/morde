package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresCloudDSHSkillReadIsIdempotentAndFenced(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "skill-read-task", "compare options")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "skill-read-worker", 20_000_000_000)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", ok, claimed, err)
	}
	initialEventSequence := claimed.EventSequence
	claimed.AgentSessionID = "session_skill_read_1"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_session_id=$2 WHERE id=$1`, claimed.ID, claimed.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	progress := cloudDSHSkillReadProgress(t, claimed, "call_skill_read_1", "session_skill_read_1", "1", hex.EncodeToString(sha256Bytes("skill body")), hex.EncodeToString(sha256Bytes("skill output")))
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	events := make(chan task.Event, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			event, err := store.CommitTaskProgress(ctx, progress)
			errors <- err
			events <- event
		}()
	}
	wg.Wait()
	close(errors)
	close(events)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent skill read commit: %v", err)
		}
	}
	var first task.Event
	for event := range events {
		if first.Sequence == 0 {
			first = event
		} else if event.Sequence != first.Sequence {
			t.Fatalf("concurrent duplicate returned different event sequence: %d vs %d", first.Sequence, event.Sequence)
		}
	}
	if first.Sequence != initialEventSequence+1 || first.Type != "task.step" {
		t.Fatalf("skill event = %+v; want one task.step after sequence %d", first, initialEventSequence)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Usage.ToolCalls != 1 || details.Task.EventSequence != initialEventSequence+1 || len(details.Steps) != 1 {
		t.Fatalf("skill budget/event was charged more than once: details=%+v err=%v", details.Task, err)
	}

	wrongSession := progress
	wrongSession.CloudDSHSkillRead = copyCloudDSHSkillRead(progress.CloudDSHSkillRead)
	wrongSession.CloudDSHSkillRead.AgentSessionID = "session_skill_read_other"
	wrongSession.CloudDSHSkillRead.FingerprintSHA256 = postgresCloudDSHSkillReadFingerprint(*wrongSession.CloudDSHSkillRead)
	wrongSession.Event.Payload = postgresCloudDSHSkillReadPayload(*wrongSession.CloudDSHSkillRead)
	wrongOutput := progress
	wrongOutput.CloudDSHSkillRead = copyCloudDSHSkillRead(progress.CloudDSHSkillRead)
	wrongOutput.CloudDSHSkillRead.OutputSHA256 = hex.EncodeToString(sha256Bytes("different output"))
	wrongOutput.CloudDSHSkillRead.FingerprintSHA256 = postgresCloudDSHSkillReadFingerprint(*wrongOutput.CloudDSHSkillRead)
	wrongOutput.Event.Payload = postgresCloudDSHSkillReadPayload(*wrongOutput.CloudDSHSkillRead)
	if _, err := store.CommitTaskProgress(ctx, wrongOutput); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed output fingerprint was not rejected: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_session_id=$2 WHERE id=$1`, claimed.ID, "session_skill_read_other"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, wrongSession); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("same call ID in another session was not fenced: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET auth_version=2 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, wrongSession); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed authorization version was not fenced: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET auth_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	expiredClaim := claimed
	expiredClaim.AgentSessionID = "session_skill_read_other"
	expiredClaim.Authorization.Version = 2
	expired := cloudDSHSkillReadProgress(t, expiredClaim, "call_skill_read_expired", "session_skill_read_other", "1",
		hex.EncodeToString(sha256Bytes("skill body")), hex.EncodeToString(sha256Bytes("skill output")))
	if _, err := store.CommitTaskProgress(ctx, expired); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("expired authorization was not rejected inside the commit transaction: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Usage.ToolCalls != 1 || details.Task.EventSequence != initialEventSequence+1 {
		t.Fatalf("rejected replays changed Task budget or sequence: details=%+v err=%v", details.Task, err)
	}
}

func cloudDSHSkillReadProgress(t *testing.T, claimed task.Task, callID, sessionID, outputVersion, contentHash, outputHash string) app.TaskProgress {
	t.Helper()
	read := app.CloudDSHSkillReadCommit{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, AgentSessionID: sessionID,
		AuthorizationVersion: claimed.Authorization.Version,
		CallID:               callID, SkillID: "compare-options", SkillVersion: outputVersion, BundleVersion: "nanomuse-builtin-v1",
		SourceRef:     "project/nanoMuse/nanomuse/skills/builtin/compare-options/SKILL.md",
		ContentSHA256: contentHash, OutputSHA256: outputHash,
	}
	read.FingerprintSHA256 = postgresCloudDSHSkillReadFingerprint(read)
	stepID := mustID(t, domain.NewStepID)
	step := task.Step{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Load nanoMuse skill: compare-options", Summary: "Loaded the fixed skill bundle."}
	usage := claimed.Usage
	usage.ToolCalls++
	return app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Result: claimed.Result, Usage: usage,
		Steps: []task.Step{step}, Event: task.Event{TaskID: claimed.ID, Type: "task.step", Payload: postgresCloudDSHSkillReadPayload(read)},
		CloudDSHSkillRead: &read,
	}
}

func postgresCloudDSHSkillReadPayload(value app.CloudDSHSkillReadCommit) []byte {
	detail := map[string]any{
		"tool": "nanomuse_skill_read", "skill_call_id": value.CallID,
		"agent_session_id": value.AgentSessionID, "authorization_version": value.AuthorizationVersion,
		"skill_id": value.SkillID, "skill_version": value.SkillVersion,
		"bundle_version": value.BundleVersion, "source_ref": value.SourceRef,
		"content_sha256": value.ContentSHA256, "output_sha256": value.OutputSHA256,
		"fingerprint_sha256": value.FingerprintSHA256,
	}
	encoded, _ := json.Marshal(map[string]any{"detail": detail})
	return encoded
}

func postgresCloudDSHSkillReadFingerprint(value app.CloudDSHSkillReadCommit) string {
	value.FingerprintSHA256 = ""
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func copyCloudDSHSkillRead(value *app.CloudDSHSkillReadCommit) *app.CloudDSHSkillReadCommit {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sha256Bytes(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}
