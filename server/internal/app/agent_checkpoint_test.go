package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain"
)

type checkpointStoreStub struct {
	value StoredAgentCheckpoint
	page  StoredAgentCheckpoint
	calls int
}

func (s *checkpointStoreStub) SaveAgentCheckpoint(_ context.Context, value StoredAgentCheckpoint) (bool, error) {
	s.value = value
	s.calls++
	return false, nil
}

func (s *checkpointStoreStub) ReadAgentCheckpointPage(_ context.Context, _ AgentCheckpointPageRequest) (StoredAgentCheckpoint, error) {
	return s.page, nil
}

func checkpointTestCommand(payload string) AgentCheckpointCommand {
	digest := sha256.Sum256([]byte(payload))
	return AgentCheckpointCommand{
		Fence: AgentCheckpointFence{
			TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent_1", AgentGeneration: 1,
			AgentSessionID: "session_1", AuthorizationVersion: 1, LeaseOwner: "worker_1", LeaseVersion: 1,
			EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
		},
		From: 0, Through: 1, Digest: digest[:], Payload: []byte(payload),
	}
}

func TestAgentCheckpointEncryptedAndBoundToTask(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"text":"private content"},{"seq":1}]}`)
	if replayed, err := service.Save(context.Background(), command); err != nil || replayed {
		t.Fatalf("save checkpoint: replayed=%v err=%v", replayed, err)
	}
	if bytes.Contains(store.value.Ciphertext, []byte("private content")) {
		t.Fatal("checkpoint contains plaintext")
	}
	plain, err := service.Decode(store.value)
	if err != nil || !bytes.Equal(plain, command.Payload) {
		t.Fatalf("decode saved checkpoint failed: %v", err)
	}
	other := store.value
	other.Fence.TaskID = "q7654321"
	if _, err := service.Decode(other); err == nil {
		t.Fatal("checkpoint ciphertext accepted under another task")
	}
}

func TestAgentCheckpointRejectsBadDigestAndEventSequence(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":2}]}`)
	if _, err := service.Save(context.Background(), command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("non-contiguous events accepted: %v", err)
	}
	command = checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":1}]}`)
	command.Digest[0] ^= 1
	if _, err := service.Save(context.Background(), command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("invalid checkpoints reached store %d times", store.calls)
	}
}

func TestAgentCheckpointReadPageDecodesAndPinsTail(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":1}]}`)
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	store.page = store.value
	throughDigest := append([]byte(nil), command.Digest...)
	request := AgentCheckpointPageRequest{Fence: AgentInstanceFence{
		TaskID: command.Fence.TaskID, OwnerID: command.Fence.OwnerID, AgentInstanceID: "agent_2",
		AgentGeneration: 2, AgentSessionID: command.Fence.AgentSessionID, SupervisorID: "supervisor_1",
		LeaseOwner: "worker_2", LeaseVersion: 2, AuthorizationVersion: command.Fence.AuthorizationVersion,
		EngineVersion: command.Fence.EngineVersion, ProfileVersion: command.Fence.ProfileVersion,
	}, From: command.From, Through: command.Through, ThroughDigest: throughDigest}
	page, err := service.ReadPage(context.Background(), request)
	if err != nil || page.From != command.From || page.Through != command.Through ||
		!bytes.Equal(page.Payload, command.Payload) || !bytes.Equal(page.Digest, command.Digest) {
		t.Fatalf("read checkpoint page = %#v, %v", page, err)
	}
	request.ThroughDigest[0] ^= 1
	if _, err := service.ReadPage(context.Background(), request); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed checkpoint tail accepted: %v", err)
	}
}
