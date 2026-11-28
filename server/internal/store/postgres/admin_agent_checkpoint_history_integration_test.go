package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestAdminAgentCheckpointHistoryReadsStoppedSessionWithoutLease(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, replayed, err := f.service.Start(ctx, f.request)
	if err != nil || replayed {
		t.Fatalf("start agent: replayed=%v err=%v", replayed, err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"header":{"id":"session-1"},"from_seq":0,"events":[{"seq":0,"type":"assistant/message","text":"private thought"}]}`
	digest := sha256.Sum256([]byte(payload))
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{
		TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID, InitiatorID: lease.Fence.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion,
	}, From: 0, Through: 0, Digest: digest[:], Payload: []byte(payload)}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	if _, err := f.service.ConfirmStopped(ctx, signAgentLifecycleReceipt(t, f.key, lease.Fence, f.request.SupervisorNonce)); err != nil {
		t.Fatalf("stop agent: %v", err)
	}
	page, err := checkpoints.ReadAdminPage(ctx, lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, 0)
	if err != nil || page.Coverage != app.AgentCheckpointCoverageComplete || page.Watermark != 0 ||
		page.From != 0 || page.Through != 0 || page.NextFrom != 1 || !strings.Contains(string(page.Payload), "private thought") {
		t.Fatalf("read checkpoint without active lease: page=%+v err=%v", page, err)
	}
	sessions, err := checkpoints.ListAdminSessions(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != lease.Fence.AgentSessionID ||
		sessions[0].RootSessionID != lease.Fence.AgentSessionID || sessions[0].CheckpointThrough != 0 {
		t.Fatalf("list agent session tree: sessions=%+v err=%v", sessions, err)
	}
}

func TestAdminAgentCheckpointHistoryRejectsInconsistentRootHead(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"header":{"id":"session-1"},"from_seq":0,"events":[{"seq":0}]}`)
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{
		TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID, InitiatorID: lease.Fence.InitiatorID,
		AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
		AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
		LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion,
	}, From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	wrongDigest := sha256.Sum256([]byte("changed"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_digest=$3 WHERE id=$1 AND owner_id=$2`,
		lease.Fence.TaskID, lease.Fence.OwnerID, wrongDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.ReadAdminPage(ctx, lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, 0); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("inconsistent root head returned %v", err)
	}
}

func TestAdminAgentCheckpointHistoryReportsGapsAndRejectsCorruption(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		lease, _, err := f.service.Start(ctx, f.request)
		if err != nil {
			t.Fatal(err)
		}
		sealer, err := app.NewSealer(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
		if err != nil {
			t.Fatal(err)
		}
		fence := app.AgentCheckpointFence{TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID, InitiatorID: lease.Fence.InitiatorID,
			AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
			AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
		for sequence := int64(0); sequence <= 1; sequence++ {
			payload := []byte(fmt.Sprintf(`{"header":{"id":"session-1"},"from_seq":%d,"events":[{"seq":%d,"text":"event-%d"}]}`,
				sequence, sequence, sequence))
			digest := sha256.Sum256(payload)
			if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: sequence, Through: sequence, Digest: digest[:], Payload: payload}); err != nil {
				t.Fatalf("save sequence %d: %v", sequence, err)
			}
		}
		if _, err := f.store.pool.Exec(ctx, `DELETE FROM task_agent_checkpoints WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND from_seq=1`,
			lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID); err != nil {
			t.Fatal(err)
		}
		page, err := checkpoints.ReadAdminPage(ctx, lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, 0)
		if err != nil || page.Coverage != app.AgentCheckpointCoveragePartial || page.Reason != "checkpoint_gap" ||
			page.GapAt == nil || *page.GapAt != 1 || page.Through != 0 || page.Watermark != 1 {
			t.Fatalf("gap page=%+v err=%v", page, err)
		}
		missing, err := checkpoints.ReadAdminPage(ctx, lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, 1)
		if err != nil || missing.Coverage != app.AgentCheckpointCoveragePartial || missing.Reason != "checkpoint_gap" ||
			missing.GapAt == nil || *missing.GapAt != 1 || missing.Payload != nil {
			t.Fatalf("missing page=%+v err=%v", missing, err)
		}
	})

	t.Run("corrupt digest", func(t *testing.T) {
		f := newAgentLifecycleFixture(t)
		ctx := context.Background()
		lease, _, err := f.service.Start(ctx, f.request)
		if err != nil {
			t.Fatal(err)
		}
		sealer, err := app.NewSealer(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		checkpoints, err := app.NewAgentCheckpointService(f.store, sealer)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(`{"header":{"id":"session-1"},"from_seq":0,"events":[{"seq":0}]}`)
		digest := sha256.Sum256(payload)
		fence := app.AgentCheckpointFence{TaskID: lease.Fence.TaskID, OwnerID: lease.Fence.OwnerID, InitiatorID: lease.Fence.InitiatorID,
			AgentInstanceID: lease.Fence.AgentInstanceID, AgentGeneration: lease.Fence.AgentGeneration,
			AgentSessionID: lease.Fence.AgentSessionID, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
			EngineVersion: lease.Fence.EngineVersion, ProfileVersion: lease.Fence.ProfileVersion}
		if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
			t.Fatalf("save checkpoint: %v", err)
		}
		wrongDigest := sha256.Sum256([]byte("changed"))
		if _, err := f.store.pool.Exec(ctx, `UPDATE task_agent_checkpoints SET digest=$4 WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND from_seq=0`,
			lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, wrongDigest[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := checkpoints.ReadAdminPage(ctx, lease.Fence.TaskID, lease.Fence.OwnerID, lease.Fence.AgentSessionID, 0); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("corrupt digest returned %v", err)
		}
	})
}
